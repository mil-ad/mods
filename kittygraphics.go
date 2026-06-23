package main

import (
	"bufio"
	_ "embed"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
)

// This file implements the kitty terminal graphics protocol using Unicode
// placeholders, which let images flow with text and survive a TUI like
// bubbletea. An image is transmitted once as a virtual placement (a=T,U=1) and
// then "drawn" by emitting a grid of placeholder cells whose diacritics encode
// row/column and whose foreground color encodes the image id.
//
// Reference: https://sw.kovidgoyal.net/kitty/graphics-protocol/#unicode-placeholders

// placeholderRune is the kitty Unicode placeholder character. Each cell of this
// rune, decorated with row/column combining diacritics, references one cell of a
// previously transmitted image.
const placeholderRune = '\U0010EEEE'

// transmitChunkSize is the max base64 payload per graphics escape, per spec.
const transmitChunkSize = 4096

//go:embed rowcolumn-diacritics.txt
var diacriticsRaw string

// isKittyTerminal reports whether the terminal supports the kitty graphics
// protocol (kitty itself or compatible emulators such as ghostty). It is
// detected once and cached.
//
// Detection queries the terminal's name via XTGETTCAP (the "TN" capability),
// which — unlike $TERM or $KITTY_WINDOW_ID — is reported by the real terminal
// even across SSH and tmux. The environment is used as a fallback when the
// query is unsupported or unavailable (e.g. non-unix platforms).
var isKittyTerminal = sync.OnceValue(func() bool {
	if name, ok := queryTerminalName(); ok {
		if isGraphicsTerminalName(name) {
			return true
		}
	}
	return envIndicatesGraphicsTerminal()
})

// isGraphicsTerminalName reports whether a terminal name denotes kitty/ghostty.
func isGraphicsTerminalName(name string) bool {
	n := strings.ToLower(name)
	return strings.Contains(n, "kitty") || strings.Contains(n, "ghostty")
}

// envIndicatesGraphicsTerminal checks environment variables as a fallback signal.
func envIndicatesGraphicsTerminal() bool {
	if os.Getenv("KITTY_WINDOW_ID") != "" {
		return true
	}
	return isGraphicsTerminalName(os.Getenv("TERM")) ||
		isGraphicsTerminalName(os.Getenv("TERM_PROGRAM"))
}

// parseDiacritics returns the ordered combining runes used to encode row/column
// indices, parsed from kitty's rowcolumn-diacritics.txt (the rune at index N
// encodes index N).
func parseDiacritics() []rune {
	var out []rune
	sc := bufio.NewScanner(strings.NewReader(diacriticsRaw))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		hexv, _, _ := strings.Cut(line, ";")
		if v, err := strconv.ParseInt(strings.TrimSpace(hexv), 16, 32); err == nil {
			out = append(out, rune(v))
		}
	}
	return out
}

// transmitImage sends a PNG to kitty as a virtual placement (a=T,U=1): invisible
// until referenced by placeholder cells, so it never disturbs the screen. The
// data is base64-encoded and chunked. It is written straight to the terminal
// (not through a TUI renderer, which would strip the graphics escapes).
func transmitImage(out io.Writer, id, cols, rows int, png []byte) {
	b64 := base64.StdEncoding.EncodeToString(png)
	first := true
	for len(b64) > 0 {
		n := min(transmitChunkSize, len(b64))
		piece := b64[:n]
		b64 = b64[n:]
		more := 0
		if len(b64) > 0 {
			more = 1
		}
		if first {
			_, _ = fmt.Fprintf(out, "\x1b_Ga=T,U=1,i=%d,f=100,c=%d,r=%d,q=2,m=%d;%s\x1b\\",
				id, cols, rows, more, piece)
			first = false
		} else {
			_, _ = fmt.Fprintf(out, "\x1b_Gm=%d;%s\x1b\\", more, piece)
		}
	}
}

// placeholderGrid builds the rows×cols block of placeholder cells that displays
// a transmitted image. Each cell carries its row and column diacritic; the
// image id's low 24 bits are encoded in the SGR foreground color. id must be
// <2^24 (the most-significant byte would need a third diacritic, which we omit).
func placeholderGrid(id, cols, rows int, diac []rune) string {
	r, g, b := byte((id>>16)&0xff), byte((id>>8)&0xff), byte(id&0xff)
	var sb strings.Builder
	for row := range rows {
		fmt.Fprintf(&sb, "\x1b[38;2;%d;%d;%dm", r, g, b)
		for col := range cols {
			sb.WriteRune(placeholderRune)
			sb.WriteRune(diac[row])
			sb.WriteRune(diac[col])
		}
		sb.WriteString("\x1b[39m\n")
	}
	return sb.String()
}
