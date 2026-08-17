package main

import (
	"bytes"
	"os"
	"os/exec"
	"runtime"
)

// clipboardImageMediaType is the media type readClipboardImage returns data
// for: every tool below is asked for PNG specifically.
const clipboardImageMediaType = "image/png"

// readClipboardImage attempts to read a PNG image from the OS clipboard. It
// returns ok=false (not an error) when the clipboard doesn't currently hold
// image data, or when no supported clipboard tool is available, so callers
// can fall back to normal text-paste handling. This shells out to the same
// kind of platform tools atotto/clipboard (already a dependency, used for
// text paste) uses internally, rather than adding a new dependency.
func readClipboardImage() (data []byte, ok bool) {
	var candidates []*exec.Cmd
	switch runtime.GOOS {
	case "linux":
		if os.Getenv("WAYLAND_DISPLAY") != "" {
			candidates = append(candidates, exec.Command("wl-paste", "--type", "image/png", "--no-newline"))
		}
		candidates = append(candidates, exec.Command("xclip", "-selection", "clipboard", "-t", "image/png", "-o"))
	case "darwin":
		candidates = append(candidates, exec.Command("pngpaste", "-"))
	}

	for _, cmd := range candidates {
		// cmd.Run() fails cleanly (without executing anything) if the binary
		// isn't on PATH, so there's no need to check for it separately.
		var out bytes.Buffer
		cmd.Stdout = &out
		if err := cmd.Run(); err == nil && out.Len() > 0 {
			return out.Bytes(), true
		}
	}
	return nil, false
}
