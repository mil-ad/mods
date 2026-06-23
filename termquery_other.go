//go:build !unix

package main

// queryTerminalName is only implemented on unix (it talks to /dev/tty); on other
// platforms detection falls back to environment variables.
func queryTerminalName() (string, bool) {
	return "", false
}
