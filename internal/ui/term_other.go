//go:build !(darwin || linux || freebsd || netbsd || openbsd || dragonfly || windows)

package ui

func TermWidth() int { return 80 }
