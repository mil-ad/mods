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
	if width > 2 { //nolint:mnd
		style = style.Width(width - 2) //nolint:mnd
	}
	return style.Render(strings.TrimSpace(content))
}

// renderAssistantMarkdown renders an assistant message to ANSI via glamour,
// swapping block- and inline-math spans for kitty placeholder grids when math is
// non-nil. Returns the rendered string and whether rendering succeeded (false
// falls back to plain text at the call site). Callers apply their own whitespace
// trimming.
//
// glamour has no math support and no extension hook, so math is replaced before
// it runs (block → line sentinels, inline → width-matched filler runs) and the
// placeholders are swapped for images afterwards.
func renderAssistantMarkdown(glam *glamour.TermRenderer, math *mathRenderer, content string) (string, bool) {
	if glam == nil {
		return "", false
	}
	renderContent := content
	var blockGrids map[int]string
	var inlineReps []inlineRep
	if math != nil {
		var blockFormulas []string
		renderContent, blockFormulas = extractBlockMath(renderContent)
		blockGrids = math.render(blockFormulas)
		renderContent, inlineReps = math.prepareInline(renderContent)
	}
	rendered, err := glam.Render(renderContent)
	if err != nil {
		return "", false
	}
	if math != nil {
		rendered = substituteMath(rendered, blockGrids)
		rendered = substituteInline(rendered, inlineReps)
	}
	return rendered, true
}

// renderConversation renders the full conversation history with styled user
// messages and glamour-rendered AI responses. Returns the rendered content,
// line offsets for each message, raw text, and roles.
// highlightIdx is the index of the message to highlight (-1 for none).
// yankFlashIdx is the message index to flash (-1 for none).
func renderConversation(
	messages []proto.Message,
	glam *glamour.TermRenderer,
	math *mathRenderer,
	userStyle lipgloss.Style,
	userStyleFocused lipgloss.Style,
	assistantStyleFocused lipgloss.Style,
	width int,
	highlightIdx int,
	yankFlashIdx int,
) (content string, offsets []int, rawMessages []string, roles []string) {
	var sb strings.Builder
	// markers tracks the byte position in the builder where each message starts.
	var markers []int

	// Collect visible messages so we can peek ahead for spacing decisions.
	type visibleMsg struct {
		msg proto.Message
		idx int // combined message index
	}
	var visible []visibleMsg
	idx := 0
	for _, msg := range messages {
		if msg.Content == "" {
			continue
		}
		if msg.Role == proto.RoleSystem {
			continue
		}
		visible = append(visible, visibleMsg{msg, idx})
		idx++
	}

	for vi, vm := range visible {
		switch vm.msg.Role {
		case proto.RoleUser:
			markers = append(markers, sb.Len())
			rawMessages = append(rawMessages, vm.msg.Content)
			roles = append(roles, proto.RoleUser)

			style := userStyle
			if vm.idx == highlightIdx {
				style = userStyleFocused
			}
			rendered := renderUserMessage(vm.msg.Content, style, width)
			flashing := vm.idx == yankFlashIdx && yankFlashIdx >= 0
			if flashing {
				rendered = flashLines(rendered, width)
			}
			sb.WriteString(rendered)
			// Blank line after user message, unless the next message is
			// a highlighted assistant (its border top replaces the gap).
			nextIsHighlightedAssistant := vi+1 < len(visible) &&
				visible[vi+1].msg.Role == proto.RoleAssistant &&
				visible[vi+1].idx == highlightIdx
			if nextIsHighlightedAssistant {
				sb.WriteString("\n")
			} else {
				sb.WriteString("\n\n")
			}

		case proto.RoleAssistant:
			markers = append(markers, sb.Len())
			rawMessages = append(rawMessages, vm.msg.Content)
			roles = append(roles, proto.RoleAssistant)

			highlighted := vm.idx == highlightIdx
			flashing := vm.idx == yankFlashIdx && yankFlashIdx >= 0
			if glamRendered, ok := renderAssistantMarkdown(glam, math, vm.msg.Content); ok {
				glamRendered = strings.TrimFunc(glamRendered, func(r rune) bool {
					return r == '\n' || r == '\r' || r == ' ' || r == '\t'
				})
				glamRendered = strings.ReplaceAll(glamRendered, "\t", strings.Repeat(" ", tabWidth))
				if highlighted {
					glamRendered = renderAssistantFocused(glamRendered, assistantStyleFocused, width)
				}
				if flashing {
					glamRendered = flashLines(glamRendered, width)
				}
				sb.WriteString(glamRendered)
				// Border bottom replaces blank line when highlighted.
				if highlighted {
					sb.WriteString("\n")
				} else {
					sb.WriteString("\n\n")
				}
				continue
			}
			// Fallback: render as plain text
			txt := vm.msg.Content
			if highlighted {
				txt = renderAssistantFocused(txt, assistantStyleFocused, width)
			}
			if flashing {
				txt = flashLines(txt, width)
			}
			sb.WriteString(txt)
			if highlighted {
				sb.WriteString("\n")
			} else {
				sb.WriteString("\n\n")
			}
		}
	}

	content = sb.String()

	// Compute line offsets from the actual content string to avoid any drift.
	for _, pos := range markers {
		offsets = append(offsets, strings.Count(content[:pos], "\n"))
	}

	return content, offsets, rawMessages, roles
}

// renderAssistantFocused wraps assistant content in a border, reusing the
// leading space glamour adds to each line for the left border character.
func renderAssistantFocused(content string, style lipgloss.Style, width int) string {
	// Strip 1 leading space per line so the left border character
	// consumes it. The right border consumes 1 trailing space via
	// the reduced content width.
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		lines[i] = trimLeadingVisibleSpaces(line, 1)
	}
	content = strings.Join(lines, "\n")
	if width > 2 { //nolint:mnd
		style = style.Width(width - 2) //nolint:mnd
	}
	return style.Render(content)
}

// flashLines applies a background highlight to text that contains ANSI codes
// (e.g. glamour output) by injecting the background color after every ANSI
// reset and padding each line to fill the terminal width.
func flashLines(text string, width int) string {
	// Background color for the flash: a dim yellow/gold (#4A4000).
	const bgCode = "\x1b[48;2;74;64;0m"
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

// trimLeadingVisibleSpaces strips up to n visible space characters from the
// start of a line, skipping over any ANSI escape sequences.
func trimLeadingVisibleSpaces(line string, n int) string {
	b := []byte(line)
	i := 0
	stripped := 0
	for i < len(b) && stripped < n {
		if b[i] == '\x1b' && i+1 < len(b) && b[i+1] == '[' {
			// Skip CSI sequence: ESC [ ... final_byte
			j := i + 2 //nolint:mnd
			for j < len(b) && b[j] != 'm' {
				j++
			}
			if j < len(b) {
				j++ // skip 'm'
			}
			i = j
		} else if b[i] == ' ' {
			b = append(b[:i], b[i+1:]...)
			stripped++
		} else {
			break
		}
	}
	return string(b)
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
	// OSC or CSI fragments contain semicolons, brackets, backslashes,
	// or colons (e.g. OSC 11 color responses like "rgb:0e0e/0e0e/1c1c").
	for _, c := range s {
		if c == ';' || c == '[' || c == ']' || c == '\\' || c == '<' || c == ':' || c < 0x20 {
			return true
		}
	}
	return false
}
