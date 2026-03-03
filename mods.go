package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/atotto/clipboard"
	"github.com/caarlos0/go-shellwords"
	timeago "github.com/caarlos0/timea.go"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/mods/internal/anthropic"
	"github.com/charmbracelet/mods/internal/cache"
	"github.com/charmbracelet/mods/internal/cohere"
	"github.com/charmbracelet/mods/internal/google"
	"github.com/charmbracelet/mods/internal/ollama"
	"github.com/charmbracelet/mods/internal/openai"
	"github.com/charmbracelet/mods/internal/proto"
	"github.com/charmbracelet/mods/internal/stream"
	"github.com/charmbracelet/x/exp/ordered"
	rw "github.com/mattn/go-runewidth"
	"github.com/rivo/uniseg"
	"golang.org/x/term"
)

type state int

const (
	startState state = iota
	configLoadedState
	inputState
	requestState
	responseState
	doneState
	errorState
)

// Mods is the Bubble Tea model that manages reading stdin and querying the
// OpenAI API.
type Mods struct {
	Output        string
	Input         string
	Styles        styles
	Error         *modsError
	state         state
	retries       int
	renderer      *lipgloss.Renderer
	glam          *glamour.TermRenderer
	glamStyle     string // resolved glamour style ("dark"/"light"); detected once at startup
	glamViewport  viewport.Model
	glamOutput    string
	glamHeight    int
	messages      []proto.Message
	cancelRequest []context.CancelFunc
	anim            tea.Model
	responseSpinner spinner.Model
	width           int
	height          int

	db     *convoDB
	cache  *cache.Conversations
	Config *Config

	content      []string
	contentMutex *sync.Mutex

	ctx context.Context

	// Interactive mode fields
	textarea            textarea.Model
	interactive         bool
	browseMode          bool
	conversationContent string
	messageOffsets      []int
	rawMessages         []string
	currentMsgIdx       int
	yankFlashIdx        int  // user message index whose response is flashing (-1 = none)
	browsePendingG      bool // true after pressing 'g' in browse mode, waiting for second key
	cachedTextareaHeight int    // cached result of interactiveTextareaHeight(); updated by syncTextareaHeight()
	cachedVLC            int    // cached textareaVisualLineCount result
	cachedVLCWidth       int    // textarea width used for cachedVLC
	cachedVLCValue       string // textarea value used for cachedVLC

	// History mode fields (Ctrl+R conversation picker)
	historyMode          bool
	historyConversations []Conversation
	historySelectedIdx   int
}

// resolveGlamourStyle determines the glamour style name once. It respects the
// GLAMOUR_STYLE env var; otherwise it uses the lipgloss renderer's cached
// background color detection. This must be called before bubbletea takes over
// stdin, since the initial detection sends an OSC escape sequence.
func resolveGlamourStyle() string {
	if s := os.Getenv("GLAMOUR_STYLE"); s != "" && s != "auto" {
		return s
	}
	if lipgloss.HasDarkBackground() {
		return "dark"
	}
	return "light"
}

func newMods(
	ctx context.Context,
	r *lipgloss.Renderer,
	cfg *Config,
	db *convoDB,
	cache *cache.Conversations,
) *Mods {
	wordWrap := cfg.WordWrap
	width, height := 0, 0
	if cfg.DynamicWidth {
		if w, h, err := term.GetSize(int(os.Stdout.Fd())); err == nil && w > 0 {
			wordWrap = w
			width, height = w, h
		}
	}
	// Detect glamour style once at startup (before bubbletea takes over the
	// terminal). This calls termenv.HasDarkBackground() which sends an OSC
	// query — safe here, but must not be repeated during resize.
	glamStyle := resolveGlamourStyle()
	gr, _ := glamour.NewTermRenderer(
		glamour.WithStandardStyle(glamStyle),
		glamour.WithWordWrap(wordWrap),
	)
	vp := viewport.New(width, height)
	vp.GotoBottom()
	m := &Mods{
		Styles:          makeStyles(r),
		glam:            gr,
		glamStyle:       glamStyle,
		state:           startState,
		renderer:        r,
		glamViewport:    vp,
		responseSpinner: newResponseSpinner(r),
		width:           width,
		height:          height,
		contentMutex:    &sync.Mutex{},
		db:              db,
		cache:           cache,
		Config:          cfg,
		ctx:             ctx,
		interactive:     cfg.Interactive,
		currentMsgIdx:   -1,
		yankFlashIdx:    -1,
	}
	if cfg.Interactive {
		m.textarea = newInteractiveTextarea()
		if width > 0 {
			m.textarea.SetWidth(width - 6) //nolint:mnd
		}
		m.syncTextareaHeight()
	}
	return m
}

// updateGlamRenderer recreates the glamour renderer with a new word wrap width.
// Uses the style resolved at startup to avoid expensive terminal queries.
func (m *Mods) updateGlamRenderer(width int) {
	gr, _ := glamour.NewTermRenderer(
		glamour.WithStandardStyle(m.glamStyle),
		glamour.WithWordWrap(width),
	)
	m.glam = gr
}

// reRenderOutput re-renders the current output with the current glamour settings.
func (m *Mods) reRenderOutput() {
	if !isOutputTTY() || m.Config.Raw || m.Output == "" {
		return
	}

	wasAtBottom := m.glamViewport.ScrollPercent() == 1.0
	m.glamOutput, _ = m.glam.Render(m.Output)
	m.glamOutput = strings.TrimRightFunc(m.glamOutput, unicode.IsSpace)
	m.glamOutput = strings.ReplaceAll(m.glamOutput, "\t", strings.Repeat(" ", tabWidth))
	m.glamHeight = lipgloss.Height(m.glamOutput)
	m.glamOutput += "\n"
	truncatedGlamOutput := m.renderer.NewStyle().
		MaxWidth(m.width).
		Render(m.glamOutput)
	m.glamViewport.SetContent(truncatedGlamOutput)
	if wasAtBottom {
		m.glamViewport.GotoBottom()
	}
}

// clearYankFlashMsg signals that the yank flash highlight should be cleared.
type clearYankFlashMsg struct{}

// completionInput is a tea.Msg that wraps the content read from stdin.
type completionInput struct {
	content string
}

// completionOutput a tea.Msg that wraps the content returned from openai.
type completionOutput struct {
	content string
	stream  stream.Stream
	errh    func(error) tea.Msg
}

// Init implements tea.Model.
func (m *Mods) Init() tea.Cmd {
	return m.findCacheOpsDetails()
}

// Update implements tea.Model.
func (m *Mods) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd
	switch msg := msg.(type) {
	case cacheDetailsMsg:
		m.Config.cacheWriteToID = msg.WriteID
		m.Config.cacheWriteToTitle = msg.Title
		m.Config.cacheReadFromID = msg.ReadID
		m.Config.API = msg.API
		m.Config.Model = msg.Model

		if !m.Config.Quiet {
			m.anim = newAnim(m.Config.Fanciness, m.Config.StatusText, m.renderer, m.Styles)
			cmds = append(cmds, m.anim.Init())
		}
		m.state = configLoadedState

		if m.interactive {
			// In interactive mode, load any continued conversation first
			if m.Config.cacheReadFromID != "" {
				m.loadConversationHistory()
			}
			// If there's initial input (stdin or args), process it as first turn
			if !isInputTTY() || m.Config.Prefix != "" {
				cmds = append(cmds, m.readStdinCmd)
			} else {
				// No initial input: go straight to input state for user to type
				m.state = inputState
				cmds = append(cmds, m.textarea.Focus())
			}
		} else {
			cmds = append(cmds, m.readStdinCmd)
		}

	case completionInput:
		if msg.content != "" {
			m.Input = removeWhitespace(msg.content)
		}
		if m.Input == "" && m.Config.Prefix == "" && m.Config.Show == "" && !m.Config.ShowLast {
			if m.interactive {
				m.state = inputState
				cmds = append(cmds, m.textarea.Focus())
				return m, tea.Batch(cmds...)
			}
			return m, m.quit
		}
		if m.Config.Dirs ||
			len(m.Config.Delete) > 0 ||
			m.Config.DeleteOlderThan != 0 ||
			m.Config.ShowHelp ||
			m.Config.List ||
			m.Config.ListRoles ||
			m.Config.Settings ||
			m.Config.ResetSettings {
			return m, m.quit
		}

		if m.Config.IncludePromptArgs {
			m.appendToOutput(m.Config.Prefix + "\n\n")
		}

		if m.Config.IncludePrompt > 0 {
			parts := strings.Split(m.Input, "\n")
			if len(parts) > m.Config.IncludePrompt {
				parts = parts[0:m.Config.IncludePrompt]
			}
			m.appendToOutput(strings.Join(parts, "\n") + "\n")
		}

		if m.interactive {
			// Add user message to conversation display
			userContent := m.Config.Prefix
			if msg.content != "" {
				if userContent != "" {
					userContent = userContent + "\n\n" + msg.content
				} else {
					userContent = msg.content
				}
			}
			m.appendUserMessageToConversation(strings.TrimSpace(userContent))
		}

		m.state = requestState
		cmds = append(cmds, m.startCompletionCmd(msg.content))

	case textareaSubmitMsg:
		if strings.TrimSpace(msg.content) == "" {
			return m, nil
		}
		m.textarea.Reset()
		m.syncTextareaHeight()
		m.browseMode = false

		// Add user message to conversation display
		m.appendUserMessageToConversation(msg.content)

		// Set up for the next request: use the textarea content as the input
		m.Config.Prefix = msg.content
		m.Input = msg.content
		m.Output = ""
		m.state = requestState

		if !m.Config.Quiet {
			m.anim = newAnim(m.Config.Fanciness, m.Config.StatusText, m.renderer, m.Styles)
			cmds = append(cmds, m.anim.Init())
		}
		cmds = append(cmds, m.startCompletionCmd(""))

	case completionOutput:
		if msg.stream == nil {
			if m.interactive {
				// Clear streaming output BEFORE appending to conversation
				// to avoid doubling the response in the viewport.
				m.glamOutput = ""
				m.appendResponseToConversation()
				m.interactiveSave()
				m.Output = ""
				m.state = inputState
				cmds = append(cmds, m.textarea.Focus())
				return m, tea.Batch(cmds...)
			}
			m.state = doneState
			return m, m.quit
		}
		if msg.content != "" {
			m.appendToOutput(msg.content)
			if m.interactive {
				m.updateInteractiveViewport()
			}
			if m.state != responseState {
				m.state = responseState
				cmds = append(cmds, m.responseSpinner.Tick)
			}
		}
		cmds = append(cmds, m.receiveCompletionStreamCmd(completionOutput{
			stream: msg.stream,
			errh:   msg.errh,
		}))
	case modsError:
		m.Error = &msg
		m.state = errorState
		return m, m.quit
	case clearYankFlashMsg:
		m.yankFlashIdx = -1
		m.reRenderConversation()
		return m, nil
	case tea.WindowSizeMsg:
		oldWidth, oldHeight := m.width, m.height
		m.width, m.height = msg.Width, msg.Height
		if m.width == oldWidth && m.height == oldHeight {
			return m, nil
		}
		if m.interactive {
			if m.width != oldWidth {
				m.textarea.SetWidth(m.width - 6) //nolint:mnd
			}
			m.syncTextareaHeight()
			m.glamViewport.Width = m.width
			if m.historyMode {
				m.glamViewport.Height = m.height
				m.glamViewport.SetContent(m.renderHistoryList())
				m.scrollHistoryIntoView()
			} else if m.width != oldWidth && m.width > 0 {
				m.updateGlamRenderer(m.width)
				m.reRenderConversation()
				m.reRenderStreamingOutput()
			}
		} else {
			m.glamViewport.Width = m.width
			m.glamViewport.Height = m.height
			if m.Config.DynamicWidth && m.width != oldWidth && m.width > 0 {
				m.updateGlamRenderer(m.width)
				m.reRenderOutput()
			}
		}
		return m, nil
	case tea.MouseMsg:
		if m.interactive && m.historyMode {
			return m.handleHistoryModeMouse(msg)
		}
	case tea.KeyMsg:
		if m.interactive {
			return m.handleInteractiveKey(msg)
		}
		switch msg.String() {
		case "q", "ctrl+c":
			m.state = doneState
			return m, m.quit
		}
	}
	if !m.Config.Quiet && (m.state == configLoadedState || m.state == requestState) {
		var cmd tea.Cmd
		m.anim, cmd = m.anim.Update(msg)
		cmds = append(cmds, cmd)
	}
	if m.interactive {
		// In interactive mode, always forward events to viewport for scrolling
		// (mouse wheel, etc.). Key events are handled by handleInteractiveKey.
		var cmd tea.Cmd
		m.glamViewport, cmd = m.glamViewport.Update(msg)
		cmds = append(cmds, cmd)
	} else if m.viewportNeeded() {
		// Only respond to keypresses when the viewport (i.e. the content) is
		// taller than the window.
		var cmd tea.Cmd
		m.glamViewport, cmd = m.glamViewport.Update(msg)
		cmds = append(cmds, cmd)
	}
	if m.state == responseState {
		var cmd tea.Cmd
		m.responseSpinner, cmd = m.responseSpinner.Update(msg)
		cmds = append(cmds, cmd)
	}
	return m, tea.Batch(cmds...)
}

// handleInteractiveKey handles key events in interactive mode.
func (m *Mods) handleInteractiveKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch m.state {
	case inputState:
		if m.historyMode {
			return m.handleHistoryModeKey(msg)
		}
		if m.browseMode {
			return m.handleBrowseModeKey(msg)
		}
		switch msg.String() {
		case "ctrl+c", "ctrl+d":
			m.state = doneState
			return m, m.quit
		case "ctrl+r":
			return m.enterHistoryMode()
		case "esc":
			// No conversation yet: quit the app
			if len(m.messageOffsets) == 0 {
				m.state = doneState
				return m, m.quit
			}
			m.browseMode = true
			m.textarea.Blur()
			// Start at the last user message and scroll to it
			m.currentMsgIdx = len(m.messageOffsets) - 1
			m.reRenderConversation()
			m.glamViewport.SetYOffset(m.messageOffsets[m.currentMsgIdx])
			return m, nil
		case "ctrl+v", "alt+v":
			// Paste from clipboard directly so we can sync textarea height
			// after insertion. The textarea's built-in Paste is async and
			// the height sync gets lost.
			if str, err := clipboard.ReadAll(); err == nil && str != "" {
				m.textarea.InsertString(str)
				m.syncTextareaHeight()
			}
			return m, nil
		case "ctrl+j":
			// Kitty (and other terminals) send \n (0x0A) for shift+enter,
			// which Bubble Tea maps to ctrl+j. Insert a newline.
			m.textarea.InsertString("\n")
			m.syncTextareaHeight()
			return m, nil
		case "enter":
			content := m.textarea.Value()
			if strings.TrimSpace(content) == "" {
				return m, nil
			}
			return m, func() tea.Msg {
				return textareaSubmitMsg{content: content}
			}
		}
		// Filter terminal escape sequence noise before passing to textarea
		if isTerminalNoise(msg) {
			return m, nil
		}
		// Expand textarea height before Update so that repositionView()
		// inside Update doesn't scroll content off-screen. We set the
		// correct height afterward in syncTextareaHeight.
		m.textarea.SetHeight(m.height)
		var cmd tea.Cmd
		m.textarea, cmd = m.textarea.Update(msg)
		cmds = append(cmds, cmd)
		m.syncTextareaHeight()
		return m, tea.Batch(cmds...)

	case requestState, responseState:
		switch msg.String() {
		case "ctrl+c":
			m.state = doneState
			return m, m.quit
		}
		// Allow viewport scrolling during response
		var cmd tea.Cmd
		m.glamViewport, cmd = m.glamViewport.Update(msg)
		cmds = append(cmds, cmd)
		if m.state == responseState {
			var cmd2 tea.Cmd
			m.responseSpinner, cmd2 = m.responseSpinner.Update(msg)
			cmds = append(cmds, cmd2)
		}
		return m, tea.Batch(cmds...)

	default:
		switch msg.String() {
		case "q", "ctrl+c":
			m.state = doneState
			return m, m.quit
		}
	}
	return m, nil
}

// handleBrowseModeKey handles key events in browse mode.
func (m *Mods) handleBrowseModeKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c", "q":
		m.state = doneState
		return m, m.quit
	case "ctrl+r":
		m.browseMode = false
		m.currentMsgIdx = -1
		return m.enterHistoryMode()
	case "esc", "i", "enter":
		m.browseMode = false
		m.currentMsgIdx = -1
		m.reRenderConversation()
		m.glamViewport.GotoBottom()
		return m, m.textarea.Focus()
	case "g":
		if m.browsePendingG {
			// gg — go to first message
			m.browsePendingG = false
			if len(m.messageOffsets) == 0 {
				return m, nil
			}
			m.currentMsgIdx = 0
			m.reRenderConversation()
			m.glamViewport.GotoTop()
			return m, nil
		}
		m.browsePendingG = true
		return m, nil
	case "e":
		if m.browsePendingG {
			// ge — go to last message
			m.browsePendingG = false
			if len(m.messageOffsets) == 0 {
				return m, nil
			}
			m.currentMsgIdx = len(m.messageOffsets) - 1
			m.reRenderConversation()
			m.glamViewport.SetYOffset(m.messageOffsets[m.currentMsgIdx])
			return m, nil
		}
		m.browsePendingG = false
		return m, nil
	case "n", "p":
		m.browsePendingG = false
		if len(m.messageOffsets) == 0 {
			return m, nil
		}
		m.currentMsgIdx++
		if m.currentMsgIdx >= len(m.messageOffsets) {
			m.currentMsgIdx = 0
		}
		m.reRenderConversation()
		if m.currentMsgIdx == 0 {
			m.glamViewport.GotoTop()
		} else if m.currentMsgIdx < len(m.messageOffsets) {
			m.glamViewport.SetYOffset(m.messageOffsets[m.currentMsgIdx])
		}
		return m, nil
	case "N", "P":
		m.browsePendingG = false
		if len(m.messageOffsets) == 0 {
			return m, nil
		}
		m.currentMsgIdx--
		if m.currentMsgIdx < 0 {
			m.currentMsgIdx = len(m.messageOffsets) - 1
		}
		m.reRenderConversation()
		if m.currentMsgIdx == 0 {
			m.glamViewport.GotoTop()
		} else if m.currentMsgIdx < len(m.messageOffsets) {
			m.glamViewport.SetYOffset(m.messageOffsets[m.currentMsgIdx])
		}
		return m, nil
	case "y":
		m.browsePendingG = false
		if raw := m.responseForUserMessage(m.currentMsgIdx); raw != "" {
			m.yankFlashIdx = m.currentMsgIdx
			m.reRenderConversation()
			return m, tea.Batch(
				copyToClipboard(raw, false),
				tea.Tick(125*time.Millisecond, func(time.Time) tea.Msg { //nolint:mnd
					return clearYankFlashMsg{}
				}),
			)
		}
		return m, nil
	case "Y":
		m.browsePendingG = false
		if raw := m.responseForUserMessage(m.currentMsgIdx); raw != "" {
			m.yankFlashIdx = m.currentMsgIdx
			m.reRenderConversation()
			return m, tea.Batch(
				copyToClipboard(raw, true),
				tea.Tick(125*time.Millisecond, func(time.Time) tea.Msg { //nolint:mnd
					return clearYankFlashMsg{}
				}),
			)
		}
		return m, nil
	default:
		m.browsePendingG = false
	}
	// Allow viewport scrolling in browse mode
	var cmd tea.Cmd
	m.glamViewport, cmd = m.glamViewport.Update(msg)
	return m, cmd
}

// enterHistoryMode fetches conversations and switches to the history picker.
func (m *Mods) enterHistoryMode() (tea.Model, tea.Cmd) {
	conversations, err := m.db.List()
	if err != nil {
		return m, nil
	}
	// Ensure titles reflect the first user message.
	for i := range conversations {
		var msgs []proto.Message
		if err := m.cache.Read(conversations[i].ID, &msgs); err == nil {
			if t := firstLine(firstPrompt(msgs)); t != "" {
				conversations[i].Title = t
			}
		}
	}
	m.historyMode = true
	m.historyConversations = conversations
	m.historySelectedIdx = 0
	m.textarea.Blur()
	m.glamViewport.Height = m.height
	m.glamViewport.SetContent(m.renderHistoryList())
	m.glamViewport.GotoTop()
	return m, nil
}

// handleHistoryModeKey handles key events in the history picker.
func (m *Mods) handleHistoryModeKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	totalItems := len(m.historyConversations) + 1 // +1 for "New conversation"
	switch msg.String() {
	case "ctrl+c":
		m.state = doneState
		return m, m.quit
	case "esc":
		m.historyMode = false
		m.glamViewport.Height = m.interactiveViewportHeight()
		m.reRenderConversation()
		m.glamViewport.GotoBottom()
		return m, m.textarea.Focus()
	case "j", "down":
		m.historySelectedIdx++
		if m.historySelectedIdx >= totalItems {
			m.historySelectedIdx = 0
		}
		m.glamViewport.SetContent(m.renderHistoryList())
		m.scrollHistoryIntoView()
		return m, nil
	case "k", "up":
		m.historySelectedIdx--
		if m.historySelectedIdx < 0 {
			m.historySelectedIdx = totalItems - 1
		}
		m.glamViewport.SetContent(m.renderHistoryList())
		m.scrollHistoryIntoView()
		return m, nil
	case "enter":
		return m.switchConversation()
	}
	// Allow viewport scrolling for long lists
	var cmd tea.Cmd
	m.glamViewport, cmd = m.glamViewport.Update(msg)
	return m, cmd
}

// handleHistoryModeMouse handles mouse events in the history picker.
func (m *Mods) handleHistoryModeMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	totalItems := len(m.historyConversations) + 1
	switch {
	case msg.Button == tea.MouseButtonLeft && msg.Action == tea.MouseActionPress:
		// Map click Y coordinate to a list index, accounting for viewport scroll.
		idx := msg.Y + m.glamViewport.YOffset
		if idx >= 0 && idx < totalItems {
			m.historySelectedIdx = idx
			m.glamViewport.SetContent(m.renderHistoryList())
		}
		return m, nil
	case msg.Button == tea.MouseButtonWheelUp:
		if m.glamViewport.YOffset > 0 {
			m.glamViewport.SetYOffset(m.glamViewport.YOffset - 1)
		}
		return m, nil
	case msg.Button == tea.MouseButtonWheelDown:
		m.glamViewport.SetYOffset(m.glamViewport.YOffset + 1)
		return m, nil
	}
	return m, nil
}

// scrollHistoryIntoView ensures the selected history item is visible in the viewport.
func (m *Mods) scrollHistoryIntoView() {
	// Each item is 1 line; selected index maps directly to a line offset.
	line := m.historySelectedIdx
	vpH := m.glamViewport.Height
	yOff := m.glamViewport.YOffset
	if line < yOff {
		m.glamViewport.SetYOffset(line)
	} else if line >= yOff+vpH {
		m.glamViewport.SetYOffset(line - vpH + 1)
	}
}

// switchConversation switches to the selected conversation from the history picker.
func (m *Mods) switchConversation() (tea.Model, tea.Cmd) {
	m.historyMode = false
	m.glamViewport.Height = m.interactiveViewportHeight()

	if m.historySelectedIdx == 0 {
		// "New conversation" — start fresh
		id := newConversationID()
		m.Config.cacheWriteToID = id
		m.Config.cacheReadFromID = ""
		m.Config.cacheWriteToTitle = ""
		m.messages = nil
		m.conversationContent = ""
		m.messageOffsets = nil
		m.rawMessages = nil
		m.glamOutput = ""
		m.Output = ""
		m.updateInteractiveViewport()
		m.glamViewport.GotoBottom()
		return m, m.textarea.Focus()
	}

	// Existing conversation
	convo := m.historyConversations[m.historySelectedIdx-1]
	m.Config.cacheReadFromID = convo.ID
	m.Config.cacheWriteToID = convo.ID
	m.Config.cacheWriteToTitle = convo.Title
	m.messages = nil
	m.conversationContent = ""
	m.messageOffsets = nil
	m.rawMessages = nil
	m.glamOutput = ""
	m.Output = ""
	m.loadConversationHistory()
	m.glamViewport.GotoBottom()
	return m, m.textarea.Focus()
}

// renderHistoryList renders the conversation history as a styled list.
func (m *Mods) renderHistoryList() string {
	var sb strings.Builder

	w := m.width - 2 //nolint:mnd // account for padding
	if w < 20 {       //nolint:mnd
		w = 20
	}
	innerW := w - 2 //nolint:mnd // padding inside HistoryItem/HistorySelected

	// Fixed-width timestamp column covers all timeago values (e.g. "12 months ago").
	const timeCol = 16

	// "New conversation" entry
	label := "+ New conversation"
	if m.historySelectedIdx == 0 {
		sb.WriteString(m.Styles.HistorySelected.Width(w).Render(label))
	} else {
		sb.WriteString(m.Styles.HistoryItem.Width(w).Render(label))
	}
	sb.WriteString("\n")

	for i, c := range m.historyConversations {
		timea := timeago.Of(c.UpdatedAt)
		// Pad/truncate timestamp to fixed width
		if rw.StringWidth(timea) < timeCol {
			timea += strings.Repeat(" ", timeCol-rw.StringWidth(timea))
		} else {
			timea = rw.Truncate(timea, timeCol, "")
		}

		model := ""
		if c.Model != nil {
			model = strings.TrimSpace(*c.Model)
		}
		modelW := rw.StringWidth(model)

		// Sanitize title — strip whitespace so width math is reliable.
		title := strings.TrimSpace(c.Title)
		title = strings.ReplaceAll(title, "\t", " ")

		// Budget: afterTime = space after timestamp. Title gets up to 80%
		// of that, but is further reduced to guarantee title+gap+model fits
		// in a single line.
		afterTime := innerW - timeCol
		titleMax := afterTime * 4 / 5 //nolint:mnd
		if model != "" {
			fitMax := afterTime - modelW - 2 //nolint:mnd // 2 = min gap
			if fitMax < titleMax {
				titleMax = fitMax
			}
		}
		if titleMax < 1 {
			titleMax = 1
		}

		if rw.StringWidth(title) > titleMax {
			title = rw.Truncate(title, titleMax-1, "") + "…"
		}
		titleW := rw.StringWidth(title)

		// Right-align model: fill remaining space with gap, but never
		// let the total exceed innerW.
		gap := afterTime - titleW - modelW
		if gap < 2 { //nolint:mnd
			// Not enough room — shrink model to fit.
			modelMax := afterTime - titleW - 2 //nolint:mnd
			if modelMax < 0 {
				modelMax = 0
			}
			if modelW > modelMax {
				model = rw.Truncate(model, modelMax, "")
				modelW = rw.StringWidth(model)
			}
			gap = afterTime - titleW - modelW
			if gap < 1 {
				gap = 1
			}
		}

		if m.historySelectedIdx == i+1 {
			line := timea + title
			if model != "" {
				line += strings.Repeat(" ", gap) + model
			}
			sb.WriteString(m.Styles.HistorySelected.Width(w).Render(line))
		} else {
			line := m.Styles.Timeago.Render(timea) + title
			if model != "" {
				line += strings.Repeat(" ", gap) + m.Styles.Comment.Render(model)
			}
			sb.WriteString(m.Styles.HistoryItem.Width(w).Render(line))
		}
		sb.WriteString("\n")
	}

	if len(m.historyConversations) == 0 {
		sb.WriteString(m.Styles.Comment.Render("  No saved conversations"))
		sb.WriteString("\n")
	}

	return sb.String()
}

// responseForUserMessage returns the assistant response that follows the Nth
// user message (0-indexed). Returns empty string if not found.
func (m *Mods) responseForUserMessage(userIdx int) string {
	if userIdx < 0 {
		return ""
	}
	currentUser := 0
	for i, msg := range m.messages {
		if msg.Role == proto.RoleUser && msg.Content != "" {
			if currentUser == userIdx {
				// Found the target user message; return the next assistant response.
				for j := i + 1; j < len(m.messages); j++ {
					if m.messages[j].Role == proto.RoleAssistant && m.messages[j].Content != "" {
						return m.messages[j].Content
					}
				}
				return ""
			}
			currentUser++
		}
	}
	return ""
}

func (m Mods) viewportNeeded() bool {
	if m.interactive {
		return m.glamHeight > m.interactiveViewportHeight()
	}
	return m.glamHeight > m.height
}

func newResponseSpinner(r *lipgloss.Renderer) spinner.Model {
	sp := spinner.New(spinner.WithSpinner(spinner.Dot))
	sp.Style = r.NewStyle().Foreground(lipgloss.Color("#6C50FF"))
	return sp
}

func (m *Mods) placeSpinnerTopRight(view string) string {
	if m.width <= 0 {
		return view
	}
	spinnerStr := m.responseSpinner.View()
	spinnerWidth := lipgloss.Width(spinnerStr)
	lines := strings.Split(view, "\n")
	if len(lines) == 0 {
		return view
	}
	firstLineWidth := lipgloss.Width(lines[0])
	availableWidth := m.width - spinnerWidth
	if firstLineWidth <= availableWidth {
		lines[0] = lines[0] + strings.Repeat(" ", availableWidth-firstLineWidth) + spinnerStr
	} else {
		lines[0] = m.renderer.NewStyle().MaxWidth(availableWidth).Render(lines[0]) + spinnerStr
	}
	return strings.Join(lines, "\n")
}

// View implements tea.Model.
func (m *Mods) View() string {
	if m.interactive {
		return m.interactiveView()
	}

	//nolint:exhaustive
	switch m.state {
	case errorState:
		return ""
	case requestState:
		if !m.Config.Quiet {
			return m.anim.View()
		}
	case responseState:
		if !m.Config.Raw && isOutputTTY() {
			if m.height > 0 {
				return m.placeSpinnerTopRight(m.glamViewport.View())
			}
			return m.glamOutput
		}

		if isOutputTTY() && !m.Config.Raw {
			return m.Output
		}

		m.contentMutex.Lock()
		for _, c := range m.content {
			fmt.Print(c)
		}
		m.content = []string{}
		m.contentMutex.Unlock()
	case doneState:
		if !isOutputTTY() {
			fmt.Printf("\n")
		}
		return ""
	}
	return ""
}

// interactiveView renders the interactive mode layout.
func (m *Mods) interactiveView() string {
	//nolint:exhaustive
	switch m.state {
	case errorState:
		return ""
	case doneState:
		return ""
	case inputState:
		if m.historyMode {
			return m.padToTermHeight(trimViewportBottom(m.glamViewport.View()))
		}

		// Use focused style when typing, dimmer when in browse mode
		boxStyle := m.Styles.InputBoxFocused
		if m.browseMode {
			boxStyle = m.Styles.InputBoxBlurred
		}

		var sb strings.Builder
		// Trim viewport padding so the textarea sits directly below
		// conversation content instead of being pinned to the bottom.
		if vp := trimViewportBottom(m.glamViewport.View()); vp != "" {
			sb.WriteString(vp)
			sb.WriteString("\n\n") // blank separator line
		}
		sb.WriteString(boxStyle.Width(m.width - 4).Render(m.textarea.View())) //nolint:mnd

		return m.padToTermHeight(sb.String())

	case configLoadedState, requestState:
		var sb strings.Builder
		if vp := trimViewportBottom(m.glamViewport.View()); vp != "" {
			sb.WriteString(vp)
			sb.WriteString("\n")
		}
		if !m.Config.Quiet {
			sb.WriteString(m.anim.View())
		}
		return m.padToTermHeight(sb.String())

	case responseState:
		return m.padToTermHeight(m.placeSpinnerTopRight(trimViewportBottom(m.glamViewport.View())))
	}
	return ""
}

// padToTermHeight pads the view with empty lines to fill the full terminal
// height. This ensures every row in the altscreen is overwritten on each
// frame, preventing ghost artifacts when the terminal shrinks and old wider
// content wraps into extra visual rows that the renderer doesn't clear.
func (m *Mods) padToTermHeight(view string) string {
	if m.height <= 0 {
		return view
	}
	n := strings.Count(view, "\n") + 1
	if n < m.height {
		view += strings.Repeat("\n", m.height-n)
	}
	return view
}

// trimViewportBottom removes trailing empty lines from a viewport's rendered
// output so that elements placed after it sit directly below the content.
func trimViewportBottom(view string) string {
	lines := strings.Split(view, "\n")
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n")
}

// textareaVisualLineCount returns the number of visual lines the textarea
// content occupies, accounting for word wrapping. It uses the same wrapping
// algorithm as the bubbles textarea (see textareaWrap) so that our height
// calculation matches the textarea's actual rendering exactly.
// Results are cached and only recalculated when textarea content or width changes.
func (m *Mods) textareaVisualLineCount() int {
	w := m.textarea.Width()
	if w <= 0 {
		return m.textarea.LineCount()
	}
	val := m.textarea.Value()
	if w == m.cachedVLCWidth && val == m.cachedVLCValue && m.cachedVLC > 0 {
		return m.cachedVLC
	}
	total := 0
	for _, line := range strings.Split(val, "\n") {
		total += len(textareaWrap([]rune(line), w))
	}
	if total < 1 {
		total = 1
	}
	m.cachedVLC = total
	m.cachedVLCWidth = w
	m.cachedVLCValue = val
	return total
}

// textareaWrap is a direct copy of the unexported wrap() function from
// github.com/charmbracelet/bubbles/textarea. We replicate it here so that
// textareaVisualLineCount produces results that exactly match the textarea's
// internal line wrapping, rather than using an approximation.
func textareaWrap(runes []rune, width int) [][]rune {
	var (
		lines  = [][]rune{{}}
		word   = []rune{}
		row    int
		spaces int
	)

	for _, r := range runes {
		if unicode.IsSpace(r) {
			spaces++
		} else {
			word = append(word, r)
		}

		if spaces > 0 { //nolint:nestif
			if uniseg.StringWidth(string(lines[row]))+uniseg.StringWidth(string(word))+spaces > width {
				row++
				lines = append(lines, []rune{})
				lines[row] = append(lines[row], word...)
				lines[row] = append(lines[row], repeatSpaces(spaces)...)
				spaces = 0
				word = nil
			} else {
				lines[row] = append(lines[row], word...)
				lines[row] = append(lines[row], repeatSpaces(spaces)...)
				spaces = 0
				word = nil
			}
		} else {
			lastCharLen := rw.RuneWidth(word[len(word)-1])
			if uniseg.StringWidth(string(word))+lastCharLen > width {
				if len(lines[row]) > 0 {
					row++
					lines = append(lines, []rune{})
				}
				lines[row] = append(lines[row], word...)
				word = nil
			}
		}
	}

	if uniseg.StringWidth(string(lines[row]))+uniseg.StringWidth(string(word))+spaces >= width {
		lines = append(lines, []rune{})
		lines[row+1] = append(lines[row+1], word...)
		spaces++
		lines[row+1] = append(lines[row+1], repeatSpaces(spaces)...)
	} else {
		lines[row] = append(lines[row], word...)
		spaces++
		lines[row] = append(lines[row], repeatSpaces(spaces)...)
	}

	return lines
}

func repeatSpaces(n int) []rune {
	return []rune(strings.Repeat(string(' '), n))
}

// interactiveTextareaHeight returns the total lines consumed by the textarea
// area (border + content lines).
func (m *Mods) interactiveTextareaHeight() int {
	const borderLines = 2
	lines := m.textareaVisualLineCount()
	// Cap so total view (viewport + separator + textarea) fits terminal height.
	maxContent := m.height - borderLines - 2 //nolint:mnd // 2 = separator + min 1 viewport line
	if maxContent < 1 {
		maxContent = 1
	}
	if lines > maxContent {
		lines = maxContent
	}
	return lines + borderLines
}

// syncTextareaHeight adjusts the textarea's internal height to match its
// content, then recalculates the viewport height. The nil Update triggers
// repositionView inside the textarea — needed after InsertString (paste,
// newline) which modifies content without calling repositionView.
func (m *Mods) syncTextareaHeight() {
	h := m.interactiveTextareaHeight()
	m.cachedTextareaHeight = h
	m.textarea.SetHeight(h - 2) //nolint:mnd // subtract border
	m.textarea, _ = m.textarea.Update(nil)
	m.glamViewport.Height = m.interactiveViewportHeight()
}

// interactiveViewportHeight calculates the viewport height for interactive mode,
// reserving space for the textarea and the blank separator line between them.
func (m *Mods) interactiveViewportHeight() int {
	taHeight := m.cachedTextareaHeight
	if taHeight == 0 {
		taHeight = m.interactiveTextareaHeight() // fallback before first sync
	}
	const separatorLines = 1 // the "\n\n" between viewport and textarea adds 1 visible blank line
	vpHeight := m.height - taHeight - separatorLines
	if vpHeight < 1 {
		vpHeight = 1
	}
	return vpHeight
}

// appendUserMessageToConversation adds a rendered user message to the conversation.
func (m *Mods) appendUserMessageToConversation(content string) {
	rendered := renderUserMessage(content, m.Styles.UserMessage, m.width)
	m.messageOffsets = append(m.messageOffsets, strings.Count(m.conversationContent, "\n"))
	m.rawMessages = append(m.rawMessages, content)
	m.conversationContent += rendered + "\n"
	m.updateInteractiveViewport()
}

// appendResponseToConversation adds the completed AI response to the conversation.
func (m *Mods) appendResponseToConversation() {
	if m.glam != nil {
		glamRendered, err := m.glam.Render(m.Output)
		if err == nil {
			glamRendered = strings.TrimFunc(glamRendered, unicode.IsSpace)
			glamRendered = strings.ReplaceAll(glamRendered, "\t", strings.Repeat(" ", tabWidth))
			m.conversationContent += glamRendered + "\n\n"
			m.updateInteractiveViewport()
			return
		}
	}
	m.conversationContent += m.Output + "\n\n"
	m.updateInteractiveViewport()
}

// updateInteractiveViewport updates the viewport content with conversation + current streaming.
func (m *Mods) updateInteractiveViewport() {
	wasAtBottom := m.glamViewport.ScrollPercent() >= 1.0 ||
		m.glamViewport.TotalLineCount() <= m.glamViewport.VisibleLineCount()
	content := m.conversationContent
	if m.glamOutput != "" {
		content += m.glamOutput
	}
	m.glamViewport.SetContent(content)
	m.glamHeight = lipgloss.Height(content)
	if wasAtBottom {
		m.glamViewport.GotoBottom()
	}
}

// reRenderStreamingOutput re-renders the in-progress streaming output with the
// current glamour renderer (e.g. after a terminal resize changes word wrap width).
func (m *Mods) reRenderStreamingOutput() {
	if m.Output == "" {
		return
	}
	m.glamOutput, _ = m.glam.Render(m.Output)
	m.glamOutput = strings.TrimRightFunc(m.glamOutput, unicode.IsSpace)
	m.glamOutput = strings.ReplaceAll(m.glamOutput, "\t", strings.Repeat(" ", tabWidth))
	m.glamHeight = lipgloss.Height(m.glamOutput)
	m.glamOutput += "\n"
}

// reRenderConversation re-renders the entire conversation after a terminal resize or highlight change.
func (m *Mods) reRenderConversation() {
	content, offsets, rawMessages := renderConversation(
		m.messages, m.glam,
		m.Styles.UserMessage, m.Styles.UserMessageFocused,
		m.width, m.currentMsgIdx, m.yankFlashIdx,
	)
	m.conversationContent = content
	m.messageOffsets = offsets
	m.rawMessages = rawMessages
	m.updateInteractiveViewport()
}


// loadConversationHistory loads and renders existing conversation from cache.
func (m *Mods) loadConversationHistory() {
	if m.Config.cacheReadFromID == "" {
		return
	}
	var messages []proto.Message
	if err := m.cache.Read(m.Config.cacheReadFromID, &messages); err != nil {
		return
	}
	m.messages = messages
	content, offsets, rawMessages := renderConversation(
		messages, m.glam,
		m.Styles.UserMessage, m.Styles.UserMessageFocused,
		m.width, -1, -1,
	)
	m.conversationContent = content
	m.messageOffsets = offsets
	m.rawMessages = rawMessages
	m.updateInteractiveViewport()
}

// interactiveSave saves the conversation in interactive mode after each exchange.
func (m *Mods) interactiveSave() {
	if m.Config.NoCache {
		return
	}
	id := m.Config.cacheWriteToID
	title := firstLine(firstPrompt(m.messages))
	if title == "" {
		title = strings.TrimSpace(m.Config.cacheWriteToTitle)
	}
	if title == "" {
		title = "interactive conversation"
	}
	// Truncate title
	const maxTitleLen = 50
	if len(title) > maxTitleLen {
		title = title[:maxTitleLen]
	}
	if err := m.cache.Write(id, &m.messages); err != nil {
		return
	}
	_ = m.db.Save(id, title, m.Config.API, m.Config.Model)
	// Ensure subsequent turns can read from this conversation's cache
	m.Config.cacheReadFromID = id
}

func (m *Mods) quit() tea.Msg {
	for _, cancel := range m.cancelRequest {
		cancel()
	}
	return tea.Quit()
}

func (m *Mods) retry(content string, err modsError) tea.Msg {
	m.retries++
	if m.retries >= m.Config.MaxRetries {
		return err
	}
	wait := time.Millisecond * 100 * time.Duration(math.Pow(2, float64(m.retries))) //nolint:mnd
	time.Sleep(wait)
	return completionInput{content}
}

func (m *Mods) startCompletionCmd(content string) tea.Cmd {
	if m.Config.Show != "" || m.Config.ShowLast {
		return m.readFromCache()
	}

	return func() tea.Msg {
		var mod Model
		var api API
		var ccfg openai.Config
		var accfg anthropic.Config
		var cccfg cohere.Config
		var occfg ollama.Config
		var gccfg google.Config

		cfg := m.Config
		api, mod, err := m.resolveModel(cfg)
		cfg.API = mod.API
		if err != nil {
			return err
		}
		if api.Name == "" {
			eps := make([]string, 0)
			for _, a := range cfg.APIs {
				eps = append(eps, m.Styles.InlineCode.Render(a.Name))
			}
			return modsError{
				err: newUserErrorf(
					"Your configured API endpoints are: %s",
					eps,
				),
				reason: fmt.Sprintf(
					"The API endpoint %s is not configured.",
					m.Styles.InlineCode.Render(cfg.API),
				),
			}
		}

		switch mod.API {
		case "ollama":
			occfg = ollama.DefaultConfig()
			if api.BaseURL != "" {
				occfg.BaseURL = api.BaseURL
			}
		case "anthropic":
			key, err := m.ensureKey(api, "ANTHROPIC_API_KEY", "https://console.anthropic.com/settings/keys")
			if err != nil {
				return modsError{err, "Anthropic authentication failed"}
			}
			accfg = anthropic.DefaultConfig(key)
			if api.BaseURL != "" {
				accfg.BaseURL = api.BaseURL
			}
		case "google":
			key, err := m.ensureKey(api, "GOOGLE_API_KEY", "https://aistudio.google.com/app/apikey")
			if err != nil {
				return modsError{err, "Google authentication failed"}
			}
			gccfg = google.DefaultConfig(mod.Name, key)
			gccfg.ThinkingBudget = mod.ThinkingBudget
		case "cohere":
			key, err := m.ensureKey(api, "COHERE_API_KEY", "https://dashboard.cohere.com/api-keys")
			if err != nil {
				return modsError{err, "Cohere authentication failed"}
			}
			cccfg = cohere.DefaultConfig(key)
			if api.BaseURL != "" {
				ccfg.BaseURL = api.BaseURL
			}
		case "azure", "azure-ad": //nolint:goconst
			key, err := m.ensureKey(api, "AZURE_OPENAI_KEY", "https://aka.ms/oai/access")
			if err != nil {
				return modsError{err, "Azure authentication failed"}
			}
			ccfg = openai.Config{
				AuthToken: key,
				BaseURL:   api.BaseURL,
			}
			if mod.API == "azure-ad" {
				ccfg.APIType = "azure-ad"
			}
			if api.User != "" {
				cfg.User = api.User
			}
		default:
			key, err := m.ensureKey(api, "OPENAI_API_KEY", "https://platform.openai.com/account/api-keys")
			if err != nil {
				return modsError{err, "OpenAI authentication failed"}
			}
			ccfg = openai.Config{
				AuthToken: key,
				BaseURL:   api.BaseURL,
			}
		}

		if cfg.HTTPProxy != "" {
			proxyURL, err := url.Parse(cfg.HTTPProxy)
			if err != nil {
				return modsError{err, "There was an error parsing your proxy URL."}
			}
			httpClient := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}
			ccfg.HTTPClient = httpClient
			accfg.HTTPClient = httpClient
			cccfg.HTTPClient = httpClient
			occfg.HTTPClient = httpClient
		}

		if mod.MaxChars == 0 {
			mod.MaxChars = cfg.MaxInputChars
		}

		// Check if the model is an o1 model and unset the max_tokens parameter
		// accordingly, as it's unsupported by o1.
		// We do set max_completion_tokens instead, which is supported.
		// Release won't have a prefix with a dash, so just putting o1 for match.
		if strings.HasPrefix(mod.Name, "o1") {
			cfg.MaxTokens = 0
		}

		ctx, cancel := context.WithTimeout(m.ctx, config.MCPTimeout)
		m.cancelRequest = append(m.cancelRequest, cancel)

		tools, err := mcpTools(ctx)
		if err != nil {
			return err
		}

		if err := m.setupStreamContext(content, mod); err != nil {
			return err
		}

		request := proto.Request{
			Messages:    m.messages,
			API:         mod.API,
			Model:       mod.Name,
			User:        cfg.User,
			Temperature: ptrOrNil(cfg.Temperature),
			TopP:        ptrOrNil(cfg.TopP),
			TopK:        ptrOrNil(cfg.TopK),
			Stop:        cfg.Stop,
			Tools:       tools,
			ToolCaller: func(name string, data []byte) (string, error) {
				ctx, cancel := context.WithTimeout(m.ctx, config.MCPTimeout)
				m.cancelRequest = append(m.cancelRequest, cancel)
				return toolCall(ctx, name, data)
			},
		}
		if cfg.MaxTokens > 0 {
			request.MaxTokens = &cfg.MaxTokens
		}

		var client stream.Client
		switch mod.API {
		case "anthropic":
			client = anthropic.New(accfg)
		case "google":
			client = google.New(gccfg)
		case "cohere":
			client = cohere.New(cccfg)
		case "ollama":
			client, err = ollama.New(occfg)
		default:
			client = openai.New(ccfg)
			if cfg.Format && config.FormatAs == "json" {
				request.ResponseFormat = &config.FormatAs
			}
		}
		if err != nil {
			return modsError{err, "Could not setup client"}
		}

		stream := client.Request(m.ctx, request)
		return m.receiveCompletionStreamCmd(completionOutput{
			stream: stream,
			errh: func(err error) tea.Msg {
				return m.handleRequestError(err, mod, m.Input)
			},
		})()
	}
}

func (m Mods) ensureKey(api API, defaultEnv, docsURL string) (string, error) {
	key := api.APIKey
	if key == "" && api.APIKeyEnv != "" && api.APIKeyCmd == "" {
		key = os.Getenv(api.APIKeyEnv)
	}
	if key == "" && api.APIKeyCmd != "" {
		args, err := shellwords.Parse(api.APIKeyCmd)
		if err != nil {
			return "", modsError{err, "Failed to parse api-key-cmd"}
		}
		out, err := exec.Command(args[0], args[1:]...).CombinedOutput() //nolint:gosec
		if err != nil {
			return "", modsError{err, "Cannot exec api-key-cmd"}
		}
		key = strings.TrimSpace(string(out))
	}
	if key == "" {
		key = os.Getenv(defaultEnv)
	}
	if key != "" {
		return key, nil
	}
	return "", modsError{
		reason: fmt.Sprintf(
			"%[1]s required; set the environment variable %[1]s or update %[2]s through %[3]s.",
			m.Styles.InlineCode.Render(defaultEnv),
			m.Styles.InlineCode.Render("mods.yaml"),
			m.Styles.InlineCode.Render("mods --settings"),
		),
		err: newUserErrorf(
			"You can grab one at %s",
			m.Styles.Link.Render(docsURL),
		),
	}
}

func (m *Mods) receiveCompletionStreamCmd(msg completionOutput) tea.Cmd {
	return func() tea.Msg {
		if msg.stream.Next() {
			chunk, err := msg.stream.Current()
			if err != nil && !errors.Is(err, stream.ErrNoContent) {
				_ = msg.stream.Close()
				return msg.errh(err)
			}
			return completionOutput{
				content: chunk.Content,
				stream:  msg.stream,
				errh:    msg.errh,
			}
		}

		// stream is done, check for errors
		if err := msg.stream.Err(); err != nil {
			return msg.errh(err)
		}

		results := msg.stream.CallTools()
		toolMsg := completionOutput{
			stream: msg.stream,
			errh:   msg.errh,
		}
		for _, call := range results {
			toolMsg.content += call.String()
		}
		if len(results) == 0 {
			m.messages = msg.stream.Messages()
			return completionOutput{
				errh: msg.errh,
			}
		}
		return toolMsg
	}
}

type cacheDetailsMsg struct {
	WriteID, Title, ReadID, API, Model string
}

func (m *Mods) findCacheOpsDetails() tea.Cmd {
	return func() tea.Msg {
		continueLast := m.Config.ContinueLast || (m.Config.Continue != "" && m.Config.Title == "")
		readID := ordered.First(m.Config.Continue, m.Config.Show)
		writeID := ordered.First(m.Config.Title, m.Config.Continue)
		title := writeID
		model := m.Config.Model
		api := m.Config.API

		if readID != "" || continueLast || m.Config.ShowLast {
			found, err := m.findReadID(readID)
			if err != nil {
				return modsError{
					err:    err,
					reason: "Could not find the conversation.",
				}
			}
			if found != nil {
				readID = found.ID
				if found.Model != nil && found.API != nil {
					model = *found.Model
					api = *found.API
				}
			}
		}

		// if we are continuing last, update the existing conversation
		if continueLast {
			writeID = readID
		}

		if writeID == "" {
			writeID = newConversationID()
		}

		if !sha1reg.MatchString(writeID) {
			convo, err := m.db.Find(writeID)
			if err != nil {
				// its a new conversation with a title
				writeID = newConversationID()
			} else {
				writeID = convo.ID
			}
		}

		return cacheDetailsMsg{
			WriteID: writeID,
			Title:   title,
			ReadID:  readID,
			API:     api,
			Model:   model,
		}
	}
}

func (m *Mods) findReadID(in string) (*Conversation, error) {
	convo, err := m.db.Find(in)
	if err == nil {
		return convo, nil
	}
	if errors.Is(err, errNoMatches) && m.Config.Show == "" {
		convo, err := m.db.FindHEAD()
		if err != nil {
			return nil, err
		}
		return convo, nil
	}
	return nil, err
}

func (m *Mods) readStdinCmd() tea.Msg {
	if !isInputTTY() {
		reader := bufio.NewReader(os.Stdin)
		stdinBytes, err := io.ReadAll(reader)
		if err != nil {
			return modsError{err, "Unable to read stdin."}
		}

		return completionInput{increaseIndent(string(stdinBytes))}
	}
	return completionInput{""}
}

func (m *Mods) readFromCache() tea.Cmd {
	return func() tea.Msg {
		var messages []proto.Message
		if err := m.cache.Read(m.Config.cacheReadFromID, &messages); err != nil {
			return modsError{err, "There was an error loading the conversation."}
		}

		m.appendToOutput(proto.Conversation(messages).String())
		return completionOutput{
			errh: func(err error) tea.Msg {
				return modsError{err: err}
			},
		}
	}
}

const tabWidth = 4

func (m *Mods) appendToOutput(s string) {
	m.Output += s
	if !isOutputTTY() || m.Config.Raw {
		m.contentMutex.Lock()
		m.content = append(m.content, s)
		m.contentMutex.Unlock()
		return
	}

	// In interactive mode, only update glamOutput for streaming display.
	// The viewport content is managed by updateInteractiveViewport.
	m.glamOutput, _ = m.glam.Render(m.Output)
	m.glamOutput = strings.TrimRightFunc(m.glamOutput, unicode.IsSpace)
	m.glamOutput = strings.ReplaceAll(m.glamOutput, "\t", strings.Repeat(" ", tabWidth))
	m.glamHeight = lipgloss.Height(m.glamOutput)
	m.glamOutput += "\n"

	if m.interactive {
		return
	}

	wasAtBottom := m.glamViewport.ScrollPercent() == 1.0
	oldHeight := m.glamHeight
	truncatedGlamOutput := m.renderer.NewStyle().
		MaxWidth(m.width).
		Render(m.glamOutput)
	m.glamViewport.SetContent(truncatedGlamOutput)
	if oldHeight < m.glamHeight && wasAtBottom {
		// If the viewport's at the bottom and we've received a new
		// line of content, follow the output by auto scrolling to
		// the bottom.
		m.glamViewport.GotoBottom()
	}
}

// if the input is whitespace only, make it empty.
func removeWhitespace(s string) string {
	if strings.TrimSpace(s) == "" {
		return ""
	}
	return s
}

var tokenErrRe = regexp.MustCompile(`This model's maximum context length is (\d+) tokens. However, your messages resulted in (\d+) tokens`)

func cutPrompt(msg, prompt string) string {
	found := tokenErrRe.FindStringSubmatch(msg)
	if len(found) != 3 { //nolint:mnd
		return prompt
	}

	maxt, _ := strconv.Atoi(found[1])
	current, _ := strconv.Atoi(found[2])

	if maxt > current {
		return prompt
	}

	// 1 token =~ 4 chars
	// cut 10 extra chars 'just in case'
	reduceBy := 10 + (current-maxt)*4 //nolint:mnd
	if len(prompt) > reduceBy {
		return prompt[:len(prompt)-reduceBy]
	}

	return prompt
}

func increaseIndent(s string) string {
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = "\t" + lines[i]
	}
	return strings.Join(lines, "\n")
}

func (m *Mods) resolveModel(cfg *Config) (API, Model, error) {
	for _, api := range cfg.APIs {
		if api.Name != cfg.API && cfg.API != "" {
			continue
		}
		for name, mod := range api.Models {
			if name == cfg.Model || slices.Contains(mod.Aliases, cfg.Model) {
				cfg.Model = name
				break
			}
		}
		mod, ok := api.Models[cfg.Model]
		if ok {
			mod.Name = cfg.Model
			mod.API = api.Name
			return api, mod, nil
		}
		if cfg.API != "" {
			return API{}, Model{}, modsError{
				err: newUserErrorf(
					"Available models are: %s",
					strings.Join(slices.Collect(maps.Keys(api.Models)), ", "),
				),
				reason: fmt.Sprintf(
					"The API endpoint %s does not contain the model %s",
					m.Styles.InlineCode.Render(cfg.API),
					m.Styles.InlineCode.Render(cfg.Model),
				),
			}
		}
	}

	return API{}, Model{}, modsError{
		reason: fmt.Sprintf(
			"Model %s is not in the settings file.",
			m.Styles.InlineCode.Render(cfg.Model),
		),
		err: newUserErrorf(
			"Please specify an API endpoint with %s or configure the model in the settings: %s",
			m.Styles.InlineCode.Render("--api"),
			m.Styles.InlineCode.Render("mods --settings"),
		),
	}
}

type number interface{ int64 | float64 }

func ptrOrNil[T number](t T) *T {
	if t < 0 {
		return nil
	}
	return &t
}
