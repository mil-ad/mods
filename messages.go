package main

import (
	"strings"

	"github.com/mil-ad/mods/internal/proto"
)

func firstPrompt(messages []proto.Message) string {
	for _, msg := range messages {
		if msg.Role == proto.RoleUser && msg.Content != "" {
			return msg.Content
		}
	}
	return ""
}

func lastPrompt(messages []proto.Message) string {
	var result string
	for _, msg := range messages {
		if msg.Role != proto.RoleUser {
			continue
		}
		if msg.Content == "" {
			continue
		}
		result = msg.Content
	}
	return result
}

// turnCount returns the number of user prompts in a conversation.
func turnCount(messages []proto.Message) int {
	n := 0
	for _, msg := range messages {
		if msg.Role == proto.RoleUser && msg.Content != "" {
			n++
		}
	}
	return n
}

func firstLine(s string) string {
	first, _, _ := strings.Cut(s, "\n")
	return first
}
