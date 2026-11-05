//go:build darwin || linux || freebsd || netbsd || openbsd || dragonfly

package ui

import (
	"os"

	"golang.org/x/sys/unix"
)

// TermWidth mengembalikan lebar terminal stderr (default 80).
func TermWidth() int {
	ws, err := unix.IoctlGetWinsize(int(os.Stderr.Fd()), unix.TIOCGWINSZ)
	if err != nil || ws.Col == 0 {
		return 80
	}
	return int(ws.Col)
}
