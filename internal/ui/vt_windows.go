//go:build windows

package ui

import (
	"os"

	"golang.org/x/sys/windows"
)

// enableVT mengaktifkan kode warna ANSI di konsol Windows.
func enableVT() {
	for _, f := range []*os.File{os.Stdout, os.Stderr} {
		h := windows.Handle(f.Fd())
		var mode uint32
		if windows.GetConsoleMode(h, &mode) == nil {
			_ = windows.SetConsoleMode(h, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING)
		}
	}
}
