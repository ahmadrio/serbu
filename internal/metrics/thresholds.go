package metrics

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Threshold adalah kriteria lulus/gagal, mis. "p(95)<500" atau "rate<0.01".
type Threshold struct {
	Source         string
	Stat           string
	Pct            float64
	Op             string
	Target         float64
	AbortOnFail    bool
	DelayAbortEval time.Duration

	Failed bool
	Value  float64
}

var thrRe = regexp.MustCompile(`^\s*(avg|min|max|med|count|rate|value|p\(\s*\d+(?:\.\d+)?\s*\))\s*(<=|>=|===|==|!==|!=|<|>)\s*(-?\d+(?:\.\d+)?(?:[eE][-+]?\d+)?)\s*$`)

func ParseThreshold(src string) (*Threshold, error) {
	m := thrRe.FindStringSubmatch(src)
	if m == nil {
		return nil, fmt.Errorf("threshold %q tidak valid. Contoh yang benar: 'p(95)<500', 'avg<200', 'rate<0.01', 'count>100'", src)
	}
	t := &Threshold{Source: strings.TrimSpace(src), Stat: strings.ReplaceAll(m[1], " ", ""), Op: m[2]}
	if p, ok := ParsePercentile(t.Stat); ok {
		t.Pct = p
	} else if strings.HasPrefix(t.Stat, "p(") {
		return nil, fmt.Errorf("persentil pada threshold %q harus antara 0 dan 100", src)
	}
	t.Target, _ = strconv.ParseFloat(m[3], 64)
	return t, nil
}

// ValidFor memeriksa apakah statistik threshold cocok dengan tipe metrik.
func (t *Threshold) ValidFor(m *Metric) error {
	ok := false
	switch m.Type {
	case Counter:
		ok = t.Stat == "count" || t.Stat == "rate"
	case Gauge:
		ok = t.Stat == "value" || t.Stat == "min" || t.Stat == "max"
	case Rate:
		ok = t.Stat == "rate"
	case Trend:
		ok = t.Stat == "avg" || t.Stat == "min" || t.Stat == "max" || t.Stat == "med" || t.Stat == "count" || strings.HasPrefix(t.Stat, "p(")
	}
	if !ok {
		allowed := map[MetricType]string{
			Counter: "count, rate",
			Gauge:   "value, min, max",
			Rate:    "rate",
			Trend:   "avg, min, max, med, count, p(N)",
		}[m.Type]
		return fmt.Errorf("threshold %q tidak bisa dipakai pada %s (tipe %s). Statistik yang tersedia: %s", t.Source, m.Name, m.Type, allowed)
	}
	return nil
}

// Evaluate menghitung nilai metrik saat ini lalu membandingkan dengan target.
func (t *Threshold) Evaluate(m *Metric, elapsed time.Duration) bool {
	var v float64
	switch s := m.Sink.(type) {
	case *CounterSink:
		if t.Stat == "rate" {
			if secs := elapsed.Seconds(); secs > 0 {
				v = s.Sum / secs
			}
		} else {
			v = s.Sum
		}
	case *GaugeSink:
		switch t.Stat {
		case "min":
			v = s.Min
		case "max":
			v = s.Max
		default:
			v = s.Value
		}
	case *RateSink:
		v = s.Rate()
	case *TrendSink:
		v = s.Stat(t.Stat)
	}
	t.Value = v
	pass := false
	switch t.Op {
	case "<":
		pass = v < t.Target
	case "<=":
		pass = v <= t.Target
	case ">":
		pass = v > t.Target
	case ">=":
		pass = v >= t.Target
	case "==", "===":
		pass = v == t.Target
	case "!=", "!==":
		pass = v != t.Target
	}
	t.Failed = !pass
	return pass
}
