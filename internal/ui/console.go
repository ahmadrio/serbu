// Package ui menangani output terminal: warna, log, progress, dan ringkasan hasil.
package ui

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
)

// Style menambahkan warna ANSI bila aktif.
type Style struct{ On bool }

func (s Style) wrap(code, t string) string {
	if !s.On || t == "" {
		return t
	}
	return "\x1b[" + code + "m" + t + "\x1b[0m"
}

func (s Style) Green(t string) string   { return s.wrap("32", t) }
func (s Style) Red(t string) string     { return s.wrap("31", t) }
func (s Style) Yellow(t string) string  { return s.wrap("33", t) }
func (s Style) Cyan(t string) string    { return s.wrap("36", t) }
func (s Style) Magenta(t string) string { return s.wrap("35", t) }
func (s Style) Gray(t string) string    { return s.wrap("90", t) }
func (s Style) Bold(t string) string    { return s.wrap("1", t) }

// Console menyatukan penulisan ke stdout/stderr supaya log tidak menabrak progress bar.
type Console struct {
	mu        sync.Mutex
	out, err  io.Writer
	OutTTY    bool
	ErrTTY    bool
	Out, Err  Style
	progLines int
}

func NewConsole(noColor bool) *Console {
	enableVT()
	c := &Console{out: os.Stdout, err: os.Stderr, OutTTY: isTTY(os.Stdout), ErrTTY: isTTY(os.Stderr)}
	color := !noColor && os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb"
	c.Out = Style{On: color && c.OutTTY}
	c.Err = Style{On: color && c.ErrTTY}
	return c
}

func isTTY(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// Print menulis ke stdout.
func (c *Console) Print(s string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.clearLocked()
	fmt.Fprint(c.out, s)
}

func (c *Console) Printf(format string, args ...any) { c.Print(fmt.Sprintf(format, args...)) }

// Log menulis satu atau beberapa baris ke stderr.
func (c *Console) Log(s string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.clearLocked()
	if !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	fmt.Fprint(c.err, s)
}

func (c *Console) Logf(format string, args ...any) { c.Log(fmt.Sprintf(format, args...)) }

// Warn dan Error: log dengan label berwarna.
func (c *Console) Warn(format string, args ...any) {
	c.Log(c.Err.Yellow("⚠ ") + fmt.Sprintf(format, args...))
}

func (c *Console) Error(format string, args ...any) {
	c.Log(c.Err.Red("✗ ") + fmt.Sprintf(format, args...))
}

// DrawProgress menggambar ulang beberapa baris progress di stderr (hanya di terminal).
func (c *Console) DrawProgress(lines []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.clearLocked()
	for _, l := range lines {
		fmt.Fprint(c.err, l+"\n")
	}
	c.progLines = len(lines)
}

// ClearProgress menghapus progress yang sedang tampil.
func (c *Console) ClearProgress() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.clearLocked()
}

func (c *Console) clearLocked() {
	if c.progLines == 0 {
		return
	}
	fmt.Fprintf(c.err, "\x1b[%dA", c.progLines)
	for i := 0; i < c.progLines; i++ {
		fmt.Fprint(c.err, "\r\x1b[2K\n")
	}
	fmt.Fprintf(c.err, "\x1b[%dA", c.progLines)
	c.progLines = 0
}
