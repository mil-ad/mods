//go:build !unix

package main

import "errors"

// cellPixels is unsupported on non-unix platforms; callers fall back to a
// default cell size. (Math rendering is gated on kitty/ghostty, which do not
// run here, so this path is effectively unreachable in practice.)
func cellPixels() (w, h int, err error) {
	return 0, 0, errors.New("terminal pixel size unavailable on this platform")
}
