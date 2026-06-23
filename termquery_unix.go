//go:build unix

package main

import (
	"encoding/hex"
	"io"
	"os"
	"regexp"
	"strings"
	"time"

	"golang.org/x/term"
)

// termQueryTimeout bounds how long we wait for the terminal to answer the
// XTGETTCAP probe before giving up.
const termQueryTimeout = 300 * time.Millisecond

// da1Re matches a Primary Device Attributes reply (CSI ? … c), used as a sync
// sentinel: every terminal answers DA1, so reading until it appears bounds the
// wait without relying on XTGETTCAP being supported.
var da1Re = regexp.MustCompile("\x1b\\[\\?[0-9;]*c")

// xtgettcapTNRe matches an XTGETTCAP reply for the "TN" (terminal name)
// capability: DCS 1 + r 544e=<hex-name> ST. 544e is hex for "TN".
var xtgettcapTNRe = regexp.MustCompile("\x1bP1\\+r([0-9A-Fa-f]+)=([0-9A-Fa-f]+)")

// parseTerminalName extracts the terminal name from a buffer containing an
// XTGETTCAP "TN" reply, decoding the hex-encoded value.
func parseTerminalName(buf []byte) (string, bool) {
	for _, m := range xtgettcapTNRe.FindAllSubmatch(buf, -1) {
		if !strings.EqualFold(string(m[1]), "544e") { // "TN"
			continue
		}
		name, err := hex.DecodeString(string(m[2]))
		if err != nil {
			continue
		}
		return string(name), true
	}
	return "", false
}

// queryTerminalName asks the controlling terminal for its name via XTGETTCAP
// ("TN") and returns it. It works regardless of stdin (e.g. piped input) by
// talking to /dev/tty, and survives SSH and tmux because the request and reply
// are forwarded to the real terminal. Returns ok=false if there is no terminal
// or it does not answer.
func queryTerminalName() (string, bool) {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return "", false
	}
	defer tty.Close()

	fd := int(tty.Fd())
	old, err := term.MakeRaw(fd)
	if err != nil {
		return "", false
	}
	defer term.Restore(fd, old)

	// XTGETTCAP for "TN" (hex 544e), then Primary DA as a sync sentinel.
	if _, err := io.WriteString(tty, "\x1bP+q544e\x1b\\\x1b[c"); err != nil {
		return "", false
	}

	// Best effort: bound the read so an unresponsive terminal can't hang us.
	// (Not all platforms honour deadlines on a tty; the DA1 sentinel is the
	// primary bound, this is just a backstop.)
	_ = tty.SetReadDeadline(time.Now().Add(termQueryTimeout))

	var buf []byte
	tmp := make([]byte, 512)
	for {
		n, err := tty.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if da1Re.Match(buf) || len(buf) > 8192 {
			break
		}
		if err != nil {
			break
		}
	}
	return parseTerminalName(buf)
}
