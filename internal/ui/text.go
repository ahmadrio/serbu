package ui

import "strings"

// Truncate memotong teks agar lebarnya (tanpa menghitung kode warna ANSI) tidak lebih dari `width`.
func Truncate(s string, width int) string {
	if width <= 0 {
		return ""
	}
	var b strings.Builder
	visible := 0
	colored := false
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		if r == '\x1b' {
			j := i
			for j < len(rs) && rs[j] != 'm' {
				j++
			}
			b.WriteString(string(rs[i:min(j+1, len(rs))]))
			colored = true
			i = j
			continue
		}
		if visible >= width {
			if colored {
				b.WriteString("\x1b[0m")
			}
			return b.String()
		}
		b.WriteRune(r)
		visible++
	}
	return b.String()
}

// Bar menggambar progress bar sepanjang `width`.
func Bar(frac float64, width int, st Style) string {
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	full := int(frac * float64(width))
	return st.Cyan(strings.Repeat("█", full)) + st.Gray(strings.Repeat("░", width-full))
}
