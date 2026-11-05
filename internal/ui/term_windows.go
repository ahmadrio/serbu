//go:build windows

package ui

import (
	"os"

	"golang.org/x/sys/windows"
)

// TermWidth mengembalikan lebar konsol (default 80).
func TermWidth() int {
	var info windows.ConsoleScreenBufferInfo
	if err := windows.GetConsoleScreenBufferInfo(windows.Handle(os.Stderr.Fd()), &info); err != nil {
		return 80
	}
	w := int(info.Window.Right-info.Window.Left) + 1
	if w <= 0 {
		return 80
	}
	return w
}
