package metrics

import (
	"math"
	"math/rand"
	"sort"
	"testing"
	"time"
)

func TestTrendExactMatchesK6(t *testing.T) {
	s := &TrendSink{}
	for _, v := range []float64{10, 20, 30, 40, 50} {
		s.Add(v)
	}
	cases := map[string]float64{"avg": 30, "min": 10, "max": 50, "med": 30, "p(90)": 46, "p(95)": 48, "count": 5}
	for stat, want := range cases {
		if got := s.Stat(stat); math.Abs(got-want) > 1e-9 {
			t.Errorf("%s = %v, mau %v", stat, got, want)
		}
	}
}

func TestTrendHistogramAccuracy(t *testing.T) {
	s := &TrendSink{}
	r := rand.New(rand.NewSource(1))
	vals := make([]float64, 0, 300_000)
	for i := 0; i < 300_000; i++ {
		v := math.Exp(r.NormFloat64()*0.8 + 4) // lognormal ~ 55ms
		vals = append(vals, v)
		s.Add(v)
	}
	if s.hist == nil {
		t.Fatal("seharusnya sudah beralih ke histogram")
	}
	sort.Float64s(vals)
	for _, p := range []float64{0.5, 0.9, 0.95, 0.99} {
		want := vals[int(p*float64(len(vals)-1))]
		got := s.P(p)
		if rel := math.Abs(got-want) / want; rel > 0.002 {
			t.Errorf("p(%v) = %v, mau ≈%v (galat %.4f)", p*100, got, want, rel)
		}
	}
}

func TestThresholds(t *testing.T) {
	r := NewRegistry()
	b := RegisterBuiltin(r)
	for _, v := range []float64{100, 200, 300, 400, 1000} {
		b.HTTPReqDuration.Sink.Add(v)
	}
	b.HTTPReqFailed.Sink.Add(1)
	for i := 0; i < 99; i++ {
		b.HTTPReqFailed.Sink.Add(0)
	}
	check := func(metric *Metric, src string, wantPass bool) {
		th, err := ParseThreshold(src)
		if err != nil {
			t.Fatal(err)
		}
		if err := th.ValidFor(metric); err != nil {
			t.Fatal(err)
		}
		if got := th.Evaluate(metric, time.Second); got != wantPass {
			t.Errorf("%s pada %s: lulus=%v, mau %v (nilai %v)", src, metric.Name, got, wantPass, th.Value)
		}
	}
	check(b.HTTPReqDuration, "p(95)<900", true) // p95 = 400+(1000-400)*0.8 = 880
	check(b.HTTPReqDuration, "p(95) < 500", false)
	check(b.HTTPReqDuration, "avg<=400", true)
	check(b.HTTPReqDuration, "med==300", true)
	check(b.HTTPReqFailed, "rate<0.02", true)
	check(b.HTTPReqFailed, "rate<0.01", false)

	if _, err := ParseThreshold("p95<500"); err == nil {
		t.Error("format salah seharusnya ditolak")
	}
	th, _ := ParseThreshold("rate<0.1")
	if err := th.ValidFor(b.HTTPReqDuration); err == nil {
		t.Error("rate pada trend seharusnya ditolak")
	}
}

func TestSubmetric(t *testing.T) {
	r := NewRegistry()
	RegisterBuiltin(r)
	sub, err := r.Submetric("http_req_duration{ name: login , method:POST}")
	if err != nil {
		t.Fatal(err)
	}
	if sub.Name != "http_req_duration{method:POST,name:login}" {
		t.Errorf("nama submetric: %s", sub.Name)
	}
	again, _ := r.Submetric("http_req_duration{method:POST,name:login}")
	if again != sub {
		t.Error("submetric yang sama harus dipakai ulang")
	}
	g, _ := r.Submetric("http_req_duration{group:::auth}")
	if g.Filter["group"] != "::auth" {
		t.Errorf("filter group: %q", g.Filter["group"])
	}
	if _, err := r.Submetric("tidak_ada{a:b}"); err == nil {
		t.Error("metric tak dikenal harus error")
	}
}
