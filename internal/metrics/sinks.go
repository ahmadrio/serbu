package metrics

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Sink mengagregasi nilai satu metrik. Tidak thread-safe: hanya diisi oleh collector.
type Sink interface {
	Add(v float64)
	Count() int64
}

func NewSink(t MetricType) Sink {
	switch t {
	case Counter:
		return &CounterSink{}
	case Gauge:
		return &GaugeSink{}
	case Rate:
		return &RateSink{}
	default:
		return &TrendSink{}
	}
}

type CounterSink struct {
	Sum float64
	N   int64
}

func (s *CounterSink) Add(v float64) { s.Sum += v; s.N++ }
func (s *CounterSink) Count() int64  { return s.N }

type GaugeSink struct {
	Value, Min, Max float64
	N               int64
}

func (s *GaugeSink) Add(v float64) {
	if s.N == 0 || v < s.Min {
		s.Min = v
	}
	if s.N == 0 || v > s.Max {
		s.Max = v
	}
	s.Value = v
	s.N++
}
func (s *GaugeSink) Count() int64 { return s.N }

type RateSink struct {
	Trues, Total int64
}

func (s *RateSink) Add(v float64) {
	if v != 0 {
		s.Trues++
	}
	s.Total++
}
func (s *RateSink) Count() int64 { return s.Total }

func (s *RateSink) Rate() float64 {
	if s.Total == 0 {
		return 0
	}
	return float64(s.Trues) / float64(s.Total)
}

// TrendSink menyimpan nilai persis sampai exactCap sampel (persentil identik k6),
// lalu beralih ke histogram logaritmik dengan galat relatif ±0,1% agar memori tetap kecil
// pada tes panjang.
type TrendSink struct {
	N             int64
	Sum, Min, Max float64

	exact  []float64
	sorted bool

	hist  map[int32]int64 // nilai positif
	neg   map[int32]int64 // nilai negatif (berdasarkan |v|)
	zeros int64
}

const (
	exactCap   = 100_000
	histAlpha  = 0.001
	histMinAbs = 1e-9
)

var (
	histGamma    = (1 + histAlpha) / (1 - histAlpha)
	histLogGamma = math.Log(histGamma)
)

func (s *TrendSink) Add(v float64) {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return
	}
	if s.N == 0 || v < s.Min {
		s.Min = v
	}
	if s.N == 0 || v > s.Max {
		s.Max = v
	}
	s.N++
	s.Sum += v
	if s.hist == nil {
		s.exact = append(s.exact, v)
		s.sorted = false
		if len(s.exact) > exactCap {
			s.toHistogram()
		}
		return
	}
	s.histAdd(v)
}

func (s *TrendSink) Count() int64 { return s.N }

func (s *TrendSink) toHistogram() {
	s.hist = map[int32]int64{}
	s.neg = map[int32]int64{}
	for _, v := range s.exact {
		s.histAdd(v)
	}
	s.exact = nil
}

func histIndex(abs float64) int32 {
	return int32(math.Ceil(math.Log(abs) / histLogGamma))
}

func histValue(i int32) float64 {
	return 2 * math.Pow(histGamma, float64(i)) / (histGamma + 1)
}

func (s *TrendSink) histAdd(v float64) {
	switch {
	case math.Abs(v) < histMinAbs:
		s.zeros++
	case v > 0:
		s.hist[histIndex(v)]++
	default:
		s.neg[histIndex(-v)]++
	}
}

func (s *TrendSink) Avg() float64 {
	if s.N == 0 {
		return 0
	}
	return s.Sum / float64(s.N)
}

// P mengembalikan persentil (pct 0..1) memakai interpolasi linear seperti k6.
func (s *TrendSink) P(pct float64) float64 {
	if s.N == 0 {
		return 0
	}
	if pct <= 0 {
		return s.Min
	}
	if pct >= 1 {
		return s.Max
	}
	if s.hist == nil {
		if !s.sorted {
			sort.Float64s(s.exact)
			s.sorted = true
		}
		if len(s.exact) == 1 {
			return s.exact[0]
		}
		i := pct * float64(len(s.exact)-1)
		lo := s.exact[int(math.Floor(i))]
		hi := s.exact[int(math.Ceil(i))]
		return lo + (hi-lo)*(i-math.Floor(i))
	}
	return s.histQuantile(pct)
}

func (s *TrendSink) histQuantile(pct float64) float64 {
	rank := int64(math.Floor(pct * float64(s.N-1)))
	var seen int64
	clamp := func(v float64) float64 { return math.Max(s.Min, math.Min(s.Max, v)) }
	// negatif: dari |v| terbesar ke terkecil
	if len(s.neg) > 0 {
		keys := sortedKeys(s.neg)
		for i := len(keys) - 1; i >= 0; i-- {
			seen += s.neg[keys[i]]
			if seen > rank {
				return clamp(-histValue(keys[i]))
			}
		}
	}
	seen += s.zeros
	if seen > rank {
		return clamp(0)
	}
	for _, k := range sortedKeys(s.hist) {
		seen += s.hist[k]
		if seen > rank {
			return clamp(histValue(k))
		}
	}
	return s.Max
}

func sortedKeys(m map[int32]int64) []int32 {
	keys := make([]int32, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	return keys
}

// ParsePercentile membaca "p(95)" → 0.95.
func ParsePercentile(stat string) (float64, bool) {
	if !strings.HasPrefix(stat, "p(") || !strings.HasSuffix(stat, ")") {
		return 0, false
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(stat[2:len(stat)-1]), 64)
	if err != nil || f < 0 || f > 100 {
		return 0, false
	}
	return f / 100, true
}

// ValidTrendStat memeriksa nama statistik di summaryTrendStats.
func ValidTrendStat(stat string) error {
	switch stat {
	case "avg", "min", "med", "max", "count":
		return nil
	}
	if _, ok := ParsePercentile(stat); ok {
		return nil
	}
	return fmt.Errorf("statistik trend %q tidak dikenal (pilihan: avg, min, med, max, count, p(N))", stat)
}

// TrendStat menghitung satu statistik trend.
func (s *TrendSink) Stat(stat string) float64 {
	switch stat {
	case "avg":
		return s.Avg()
	case "min":
		if s.N == 0 {
			return 0
		}
		return s.Min
	case "max":
		if s.N == 0 {
			return 0
		}
		return s.Max
	case "med":
		return s.P(0.5)
	case "count":
		return float64(s.N)
	}
	if p, ok := ParsePercentile(stat); ok {
		return s.P(p)
	}
	return 0
}

// Values mengubah isi sink menjadi map nilai (format sama dengan data ringkasan k6).
func Values(m *Metric, elapsed time.Duration, trendStats []string) map[string]float64 {
	secs := elapsed.Seconds()
	switch s := m.Sink.(type) {
	case *CounterSink:
		rate := 0.0
		if secs > 0 {
			rate = s.Sum / secs
		}
		return map[string]float64{"count": s.Sum, "rate": rate}
	case *GaugeSink:
		return map[string]float64{"value": s.Value, "min": s.Min, "max": s.Max}
	case *RateSink:
		return map[string]float64{"rate": s.Rate(), "passes": float64(s.Trues), "fails": float64(s.Total - s.Trues)}
	case *TrendSink:
		out := make(map[string]float64, len(trendStats))
		for _, st := range trendStats {
			out[st] = s.Stat(st)
		}
		return out
	}
	return nil
}
