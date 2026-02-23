package main

import (
	"strings"

	"github.com/atotto/clipboard"
	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/mods/internal/proto"
)

// textareaSubmitMsg is sent when the user submits text from the textarea.
type textareaSubmitMsg struct {
	content string
}

// newInteractiveTextarea creates and configures the textarea for interactive mode.
func newInteractiveTextarea() textarea.Model {
	ta := textarea.New()
	ta.Placeholder = ""
	ta.Prompt = ""
	ta.CharLimit = 0
	ta.ShowLineNumbers = false
	ta.SetHeight(1)

	// Clear the CursorLine background so there's no grey highlight bar.
	ta.FocusedStyle.CursorLine = lipgloss.NewStyle()
	ta.BlurredStyle.CursorLine = lipgloss.NewStyle()
	ta.FocusedStyle.EndOfBuffer = lipgloss.NewStyle()
	ta.BlurredStyle.EndOfBuffer = lipgloss.NewStyle()

	// Disable the InsertNewline binding entirely — Enter is intercepted
	// by Update() for submit. Users can use terminal-level bindings
	// (e.g. kitty's send \n) if they need multi-line input.
	ta.KeyMap.InsertNewline = key.NewBinding(key.WithDisabled())

	// Disable built-in Paste — we handle ctrl+v/alt+v in Update()
	// so we can sync textarea height after insertion.
	ta.KeyMap.Paste = key.NewBinding(key.WithDisabled())

	ta.Focus()
	return ta
}

// renderUserMessage wraps user text in a styled bordered box.
func renderUserMessage(content string, style lipgloss.Style, width int) string {
	if width > 4 { //nolint:mnd
		style = style.Width(width - 4) //nolint:mnd
	}
	return style.Render(strings.TrimSpace(content))
}

// renderConversation renders the full conversation history with styled user
// messages and glamour-rendered AI responses. Returns the rendered content,
// line offsets for each user message, and the raw text of each user message.
// highlightIdx is the index of the user message to highlight (-1 for none).
// yankFlashIdx is the user message index whose assistant response should flash (-1 for none).
func renderConversation(
	messages []proto.Message,
	glam *glamour.TermRenderer,
	userStyle lipgloss.Style,
	userStyleFocused lipgloss.Style,
	width int,
	highlightIdx int,
	yankFlashIdx int,
) (content string, offsets []int, rawMessages []string) {
	var sb strings.Builder
	// markers tracks the byte position in the builder where each user message starts.
	var markers []int
	userIdx := 0
	// lastUserIdx tracks the user message index of the most recently seen user message,
	// so we can match assistant responses to the preceding user message.
	lastUserIdx := -1

	for _, msg := range messages {
		if msg.Content == "" {
			continue
		}

		switch msg.Role {
		case proto.RoleUser:
			markers = append(markers, sb.Len())
			rawMessages = append(rawMessages, msg.Content)

			style := userStyle
			if userIdx == highlightIdx {
				style = userStyleFocused
			}
			rendered := renderUserMessage(msg.Content, style, width)
			sb.WriteString(rendered)
			sb.WriteString("\n")
			lastUserIdx = userIdx
			userIdx++

		case proto.RoleAssistant:
			flashing := lastUserIdx == yankFlashIdx && yankFlashIdx >= 0
			if glam != nil {
				glamRendered, err := glam.Render(msg.Content)
				if err == nil {
					glamRendered = strings.TrimFunc(glamRendered, func(r rune) bool {
						return r == '\n' || r == '\r' || r == ' ' || r == '\t'
					})
					glamRendered = strings.ReplaceAll(glamRendered, "\t", strings.Repeat(" ", tabWidth))
					if flashing {
						glamRendered = flashLines(glamRendered, width)
					}
					sb.WriteString(glamRendered)
					sb.WriteString("\n\n")
					continue
				}
			}
			// Fallback: render as plain text
			txt := msg.Content
			if flashing {
				txt = flashLines(txt, width)
			}
			sb.WriteString(txt)
			sb.WriteString("\n\n")

		case proto.RoleSystem:
			// Skip system messages in the display
			continue
		}
	}

	content = sb.String()

	// Compute line offsets from the actual content string to avoid any drift.
	for _, pos := range markers {
		offsets = append(offsets, strings.Count(content[:pos], "\n"))
	}

	return content, offsets, rawMessages
}

// flashLines applies a background highlight to text that contains ANSI codes
// (e.g. glamour output) by injecting the background color after every ANSI
// reset and padding each line to fill the terminal width.
func flashLines(text string, width int) string {
	// Background color for the flash: a visible purple (#463770).
	const bgCode = "\x1b[48;2;70;55;112m"
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		// Re-inject background after any full ANSI reset so the
		// background persists through glamour's styling.
		line = strings.ReplaceAll(line, "\x1b[0m", "\x1b[0m"+bgCode)
		pad := width - lipgloss.Width(line)
		if pad < 0 {
			pad = 0
		}
		lines[i] = bgCode + line + strings.Repeat(" ", pad) + "\x1b[0m"
	}
	return strings.Join(lines, "\n")
}

// copyToClipboard copies text to the clipboard. When joinLines is true,
// single newlines are replaced with spaces (preserving paragraph breaks).
func copyToClipboard(text string, joinLines bool) tea.Cmd {
	return func() tea.Msg {
		t := text
		if joinLines {
			// Replace single newlines with spaces, preserve double newlines
			t = strings.ReplaceAll(t, "\n\n", "\x00")
			t = strings.ReplaceAll(t, "\n", " ")
			t = strings.ReplaceAll(t, "\x00", "\n\n")
		}
		_ = clipboard.WriteAll(t)
		return nil
	}
}

// isTerminalNoise returns true if the key message looks like a terminal
// escape sequence fragment (OSC responses, mouse SGR sequences, etc.)
// that leaked through as keyboard input.
func isTerminalNoise(msg tea.KeyMsg) bool {
	if msg.Paste {
		return false
	}
	s := msg.String()
	if len(s) <= 1 {
		return false
	}
	// OSC or CSI fragments contain semicolons, brackets, backslashes
	for _, c := range s {
		if c == ';' || c == '[' || c == ']' || c == '\\' || c == '<' || c < 0x20 {
			return true
		}
	}
	return false
}
