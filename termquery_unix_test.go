//go:build unix

package main

import (
	"encoding/hex"
	"testing"
)

func TestParseTerminalName(t *testing.T) {
	reply := func(name string) []byte {
		return []byte("\x1bP1+r544e=" + hex.EncodeToString([]byte(name)) + "\x1b\\")
	}

	tests := []struct {
		name   string
		buf    []byte
		want   string
		wantOK bool
	}{
		{"kitty", reply("xterm-kitty"), "xterm-kitty", true},
		{"ghostty", reply("xterm-ghostty"), "xterm-ghostty", true},
		{"reply amid DA1", append(reply("xterm-kitty"), "\x1b[?62;c"...), "xterm-kitty", true},
		{"unsupported (DCS 0)", []byte("\x1bP0+r544e\x1b\\"), "", false},
		{"other cap only", []byte("\x1bP1+r626365=31\x1b\\"), "", false}, // "bce"
		{"no reply", []byte("\x1b[?62;c"), "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseTerminalName(tt.buf)
			if ok != tt.wantOK || got != tt.want {
				t.Errorf("parseTerminalName = (%q, %v), want (%q, %v)", got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestIsGraphicsTerminalName(t *testing.T) {
	yes := []string{"xterm-kitty", "xterm-ghostty", "KITTY", "ghostty"}
	no := []string{"xterm-256color", "screen", "tmux-256color", ""}
	for _, n := range yes {
		if !isGraphicsTerminalName(n) {
			t.Errorf("isGraphicsTerminalName(%q) = false, want true", n)
		}
	}
	for _, n := range no {
		if isGraphicsTerminalName(n) {
			t.Errorf("isGraphicsTerminalName(%q) = true, want false", n)
		}
	}
}
