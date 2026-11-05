package engine

import (
	"testing"
	"time"
)

func mustOpts(t *testing.T, js string) *Options {
	t.Helper()
	o, err := ParseOptions(js)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func TestShortcutsToScenario(t *testing.T) {
	cases := []struct {
		js, executor string
		cli          CLIExec
	}{
		{`{}`, "per-vu-iterations", CLIExec{}},
		{`{"vus":10,"duration":"30s"}`, "constant-vus", CLIExec{}},
		{`{"vus":5,"iterations":100}`, "shared-iterations", CLIExec{}},
		{`{"stages":[{"duration":"10s","target":5}]}`, "ramping-vus", CLIExec{}},
		// flag CLI --duration menimpa stages di script
		{`{"stages":[{"duration":"10s","target":5}]}`, "constant-vus", CLIExec{Duration: ptr(5 * time.Second)}},
	}
	for _, c := range cases {
		plans, _, err := BuildPlan(mustOpts(t, c.js), c.cli)
		if err != nil {
			t.Fatalf("%s: %v", c.js, err)
		}
		if plans[0].Executor != c.executor {
			t.Errorf("%s → %s, mau %s", c.js, plans[0].Executor, c.executor)
		}
	}
}

func ptr[T any](v T) *T { return &v }

func TestScenarioOrderAndValidation(t *testing.T) {
	o := mustOpts(t, `{"scenarios":{"zeta":{"executor":"constant-vus","vus":2,"duration":"5s"},"alfa":{"executor":"constant-arrival-rate","rate":10,"duration":"5s","preAllocatedVUs":2}}}`)
	plans, _, err := BuildPlan(o, CLIExec{})
	if err != nil {
		t.Fatal(err)
	}
	if plans[0].Name != "zeta" || plans[1].Name != "alfa" {
		t.Errorf("urutan skenario harus mengikuti script, dapat %s, %s", plans[0].Name, plans[1].Name)
	}
	if plans[1].MaxVUs != 2 || plans[1].TimeUnit != time.Second {
		t.Errorf("default maxVUs/timeUnit salah: %+v", plans[1])
	}
	bad := []string{
		`{"scenarios":{"x":{"executor":"constant-vus","vus":1}}}`,
		`{"scenarios":{"x":{"executor":"constant-arrival-rate","rate":5,"duration":"5s"}}}`,
		`{"scenarios":{"x":{"executor":"ngawur"}}}`,
		`{"scenarios":{"x":{"executor":"ramping-vus","stages":[{"duration":"5s","target":0}],"startVUs":0}}}`,
		`{"duration":"lima detik"}`,
	}
	for _, js := range bad {
		o, err := ParseOptions(js)
		if err == nil {
			_, _, err = BuildPlan(o, CLIExec{})
		}
		if err == nil {
			t.Errorf("seharusnya error: %s", js)
		}
	}
}

func TestRampTarget(t *testing.T) {
	p := &Plan{Executor: "ramping-vus", StartVUs: 0, Stages: []StagePlan{{10 * time.Second, 10}, {5 * time.Second, 10}, {10 * time.Second, 0}}}
	cases := map[time.Duration]int64{
		0: 0, 999 * time.Millisecond: 0, time.Second: 1, 5500 * time.Millisecond: 5, 10 * time.Second: 10,
		12 * time.Second: 10, 15500 * time.Millisecond: 10, 16 * time.Second: 9, 24500 * time.Millisecond: 1, 30 * time.Second: 0,
	}
	for at, want := range cases {
		if got := p.RampTarget(at); got != want {
			t.Errorf("t=%s: %d VU, mau %d", at, got, want)
		}
	}
}

func TestArrivalTimes(t *testing.T) {
	c := &Plan{Executor: "constant-arrival-rate", Rate: 20, TimeUnit: time.Second, Duration: 30 * time.Second}
	n := 0
	for k := int64(0); ; k++ {
		at, ok := c.ArrivalTime(k)
		if !ok {
			break
		}
		n++
		_ = at
	}
	if n != 600 {
		t.Errorf("constant 20/s × 30s = %d iterasi, mau 600", n)
	}
	// 0→20/s selama 4s lalu 20/s selama 2s: luas = 40 + 40 = 80
	r := &Plan{Executor: "ramping-arrival-rate", StartRate: 0, TimeUnit: time.Second, Stages: []StagePlan{{4 * time.Second, 20}, {2 * time.Second, 20}}}
	total := r.PlannedDuration()
	n = 0
	var prev time.Duration
	for k := int64(0); ; k++ {
		at, ok := r.ArrivalTime(k)
		if !ok || at >= total {
			break
		}
		if at < prev {
			t.Fatalf("waktu mundur pada k=%d", k)
		}
		prev = at
		n++
	}
	if n != 80 {
		t.Errorf("ramping = %d iterasi, mau 80", n)
	}
	// iterasi ke-10 pada kurva naik: 10 = 20/4 * t²/2 → t = 2s
	if at, _ := r.ArrivalTime(10); at < 1990*time.Millisecond || at > 2010*time.Millisecond {
		t.Errorf("iterasi ke-10 pada %s, mau ≈2s", at)
	}
}

func TestDurations(t *testing.T) {
	cases := map[string]time.Duration{"30s": 30 * time.Second, "1m30s": 90 * time.Second, "1d2h": 26 * time.Hour, "1.5": 1500 * time.Millisecond}
	for s, want := range cases {
		got, err := ParseDuration(s)
		if err != nil || got != want {
			t.Errorf("%q → %v (%v), mau %v", s, got, err, want)
		}
	}
	o := mustOpts(t, `{"setupTimeout": 1500}`)
	if o.SetupTimeout.Get(0) != 1500*time.Millisecond {
		t.Errorf("angka di JSON harus dibaca sebagai milidetik")
	}
}
