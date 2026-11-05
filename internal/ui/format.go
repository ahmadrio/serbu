package ui

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

func trimZeros(s string) string {
	if !strings.Contains(s, ".") {
		return s
	}
	s = strings.TrimRight(s, "0")
	return strings.TrimSuffix(s, ".")
}

// Duration memformat milidetik seperti k6: 812µs, 6.18ms, 1.02s, 1m3.5s.
func Duration(ms float64) string {
	if ms == 0 || math.IsNaN(ms) {
		return "0s"
	}
	sign := ""
	if ms < 0 {
		sign, ms = "-", -ms
	}
	var s string
	switch {
	case ms < 0.001:
		s = fmt.Sprintf("%.0fns", ms*1e6)
	case ms < 1:
		s = trimZeros(fmt.Sprintf("%.2f", ms*1000)) + "µs"
	case ms < 1000:
		s = trimZeros(fmt.Sprintf("%.2f", ms)) + "ms"
	case ms < 60_000:
		s = trimZeros(fmt.Sprintf("%.2f", ms/1000)) + "s"
	default:
		total := ms / 1000
		h := int(total / 3600)
		m := int(total/60) % 60
		sec := total - float64(h*3600+m*60)
		if h > 0 {
			s = fmt.Sprintf("%dh%dm%ss", h, m, trimZeros(fmt.Sprintf("%.1f", sec)))
		} else {
			s = fmt.Sprintf("%dm%ss", m, trimZeros(fmt.Sprintf("%.1f", sec)))
		}
	}
	return sign + s
}

// Bytes memformat ukuran data dengan satuan SI (kB = 1000 B) seperti k6.
func Bytes(b float64) string {
	units := []string{"B", "kB", "MB", "GB", "TB"}
	i := 0
	for b >= 1000 && i < len(units)-1 {
		b /= 1000
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%.0f B", b)
	}
	if b < 10 {
		return fmt.Sprintf("%.1f %s", b, units[i])
	}
	return fmt.Sprintf("%.0f %s", b, units[i])
}

// Num memformat angka: bulat tanpa desimal, selain itu maks. 2 desimal.
func Num(f float64) string {
	if f == math.Trunc(f) && math.Abs(f) < 1e15 {
		return strconv.FormatInt(int64(f), 10)
	}
	return trimZeros(fmt.Sprintf("%.2f", f))
}

// Percent memformat rasio 0..1 menjadi "99.66%".
func Percent(r float64) string {
	return fmt.Sprintf("%.2f%%", r*100)
}

// PerSec memformat laju per detik.
func PerSec(f float64) string {
	return trimZeros(fmt.Sprintf("%.2f", f)) + "/s"
}

// Clock memformat durasi untuk progress: 0m12.3s.
func Clock(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	total := d.Seconds()
	m := int(total / 60)
	s := total - float64(m*60)
	return fmt.Sprintf("%dm%04.1fs", m, s)
}

// Value memformat nilai sesuai jenis isinya (time/data/default).
func Value(v float64, contains string) string {
	switch contains {
	case "time":
		return Duration(v)
	case "data":
		return Bytes(v)
	}
	return Num(v)
}
