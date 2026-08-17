package main

import (
	"fmt"
	"strings"

	"github.com/mil-ad/mods/internal/proto"
)

func (m *Mods) setupStreamContext(content string, mod Model) error {
	cfg := m.Config
	m.messages = []proto.Message{}
	if txt := cfg.FormatText[cfg.FormatAs]; cfg.Format && txt != "" {
		m.messages = append(m.messages, proto.Message{
			Role:    proto.RoleSystem,
			Content: txt,
		})
	}

	if cfg.Role != "" {
		roleSetup, ok := cfg.Roles[cfg.Role]
		if !ok {
			return modsError{
				err:    fmt.Errorf("role %q does not exist", cfg.Role),
				reason: "Could not use role",
			}
		}
		for _, msg := range roleSetup {
			content, err := loadMsg(msg)
			if err != nil {
				return modsError{
					err:    err,
					reason: "Could not use role",
				}
			}
			m.messages = append(m.messages, proto.Message{
				Role:    proto.RoleSystem,
				Content: content,
			})
		}
	}

	if prefix := cfg.Prefix; prefix != "" {
		content = strings.TrimSpace(prefix + "\n\n" + content)
	}

	if !cfg.NoLimit && int64(len(content)) > mod.MaxChars {
		content = content[:mod.MaxChars]
	}

	if !cfg.NoCache && cfg.cacheReadFromID != "" {
		if err := m.cache.Read(cfg.cacheReadFromID, &m.messages); err != nil {
			return modsError{
				err: err,
				reason: fmt.Sprintf(
					"There was a problem reading the cache. Use %s / %s to disable it.",
					m.Styles.InlineCode.Render("--no-cache"),
					m.Styles.InlineCode.Render("NO_CACHE"),
				),
			}
		}
	}

	m.messages = append(m.messages, proto.Message{
		Role:        proto.RoleUser,
		Content:     content,
		Attachments: cfg.Attachments,
	})

	return nil
}

// apiSupportsAttachments reports whether the given API can accept image
// attachments. Gated at the provider level rather than per-model: simpler,
// and avoids maintaining a model capability list.
func apiSupportsAttachments(api string) bool {
	switch api {
	case "google", "cohere", "ollama":
		return false
	default: // openai (default case), azure, azure-ad, anthropic
		return true
	}
}
