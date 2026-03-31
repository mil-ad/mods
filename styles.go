package main

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

type styles struct {
	AppName,
	CliArgs,
	Comment,
	CyclingChars,
	ErrorHeader,
	ErrorDetails,
	ErrPadding,
	Flag,
	FlagComma,
	FlagDesc,
	InlineCode,
	Link,
	Pipe,
	Quote,
	ConversationList,
	SHA1,
	Timeago,
	HistorySelected,
	HistoryItem,
	UserMessage,
	UserMessageFocused,
	AssistantMessageFocused,
	InputBoxFocused,
	InputBoxBlurred,
	RecordingIndicator lipgloss.Style
}

func makeStyles(r *lipgloss.Renderer) (s styles) {
	const horizontalEdgePadding = 2
	s.AppName = r.NewStyle().Bold(true)
	s.CliArgs = r.NewStyle().Foreground(lipgloss.Color("#585858"))
	s.Comment = r.NewStyle().Foreground(lipgloss.Color("#757575"))
	s.CyclingChars = r.NewStyle().Foreground(lipgloss.Color("#FF87D7"))
	s.ErrorHeader = r.NewStyle().Foreground(lipgloss.Color("#F1F1F1")).Background(lipgloss.Color("#FF5F87")).Bold(true).Padding(0, 1).SetString("ERROR")
	s.ErrorDetails = s.Comment
	s.ErrPadding = r.NewStyle().Padding(0, horizontalEdgePadding)
	s.Flag = r.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#00B594", Dark: "#3EEFCF"}).Bold(true)
	s.FlagComma = r.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#5DD6C0", Dark: "#427C72"}).SetString(",")
	s.FlagDesc = s.Comment
	s.InlineCode = r.NewStyle().Foreground(lipgloss.Color("#FF5F87")).Background(lipgloss.Color("#3A3A3A")).Padding(0, 1)
	s.Link = r.NewStyle().Foreground(lipgloss.Color("#00AF87")).Underline(true)
	s.Quote = r.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#FF71D0", Dark: "#FF78D2"})
	s.Pipe = r.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#8470FF", Dark: "#745CFF"})
	s.ConversationList = r.NewStyle().Padding(0, 1)
	s.SHA1 = s.Flag
	s.Timeago = r.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#999", Dark: "#555"})
	s.HistorySelected = r.NewStyle().
		Foreground(lipgloss.Color("#F1F1F1")).
		Background(lipgloss.Color("#6C50FF")).
		Bold(true).
		Padding(0, 1).
		Width(0) // set dynamically
	s.HistoryItem = r.NewStyle().
		Padding(0, 1).
		Width(0) // set dynamically
	s.UserMessage = r.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("#555555")).
		Padding(0, 1)
	s.UserMessageFocused = r.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("#FFD700")).
		Padding(0, 1)
	s.AssistantMessageFocused = r.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("#FFD700"))
	s.InputBoxFocused = r.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("#6C50FF")).
		Padding(0, 1)
	s.InputBoxBlurred = r.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("#555555")).
		Padding(0, 1)
	s.RecordingIndicator = r.NewStyle().
		Foreground(lipgloss.Color("#FF5F5F")).
		Bold(true)
	return s
}

// action messages

const defaultAction = "WROTE"

var outputHeader = lipgloss.NewStyle().Foreground(lipgloss.Color("#F1F1F1")).Background(lipgloss.Color("#6C50FF")).Bold(true).Padding(0, 1).MarginRight(1)

func printConfirmation(action, content string) {
	if action == "" {
		action = defaultAction
	}
	outputHeader = outputHeader.SetString(strings.ToUpper(action))
	fmt.Println(lipgloss.JoinHorizontal(lipgloss.Center, outputHeader.String(), content))
}
