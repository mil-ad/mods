//go:build unix

package main

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// cellPixels returns the pixel width/height of a single terminal cell. It
// measures against /dev/tty so it works even when stdout is redirected, falling
// back to stdout. unix.IoctlGetWinsize uses the platform-correct TIOCGWINSZ.
func cellPixels() (w, h int, err error) {
	fd := int(os.Stdout.Fd())
	if tty, terr := os.Open("/dev/tty"); terr == nil {
		defer func() { _ = tty.Close() }()
		fd = int(tty.Fd())
	}
	ws, err := unix.IoctlGetWinsize(fd, unix.TIOCGWINSZ)
	if err != nil {
		return 0, 0, fmt.Errorf("query window size: %w", err)
	}
	if ws.Col == 0 || ws.Row == 0 || ws.Xpixel == 0 || ws.Ypixel == 0 {
		return 0, 0, errors.New("terminal did not report pixel size")
	}
	return int(ws.Xpixel) / int(ws.Col), int(ws.Ypixel) / int(ws.Row), nil
}
