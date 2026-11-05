// Package engine mengatur jalannya tes: opsi, skenario, eksekutor, metrik, dan hasil.
package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Duration menerima "30s", "1m30s", "2h", "1d", atau angka (milidetik).
type Duration time.Duration

var dayRe = regexp.MustCompile(`^(\d+(?:\.\d+)?)d(.*)$`)

func ParseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("durasi kosong")
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return time.Duration(f * float64(time.Second)), nil
	}
	var extra time.Duration
	if m := dayRe.FindStringSubmatch(s); m != nil {
		days, _ := strconv.ParseFloat(m[1], 64)
		extra = time.Duration(days * 24 * float64(time.Hour))
		s = m[2]
		if s == "" {
			return extra, nil
		}
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("durasi %q tidak valid (contoh: 30s, 1m30s, 2h)", s)
	}
	return extra + d, nil
}

func (d *Duration) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		v, err := ParseDuration(s)
		if err != nil {
			return err
		}
		*d = Duration(v)
		return nil
	}
	var f float64
	if err := json.Unmarshal(b, &f); err != nil {
		return fmt.Errorf("durasi harus string seperti \"30s\" atau angka milidetik")
	}
	*d = Duration(f * float64(time.Millisecond))
	return nil
}

func (d *Duration) Get(def time.Duration) time.Duration {
	if d == nil {
		return def
	}
	return time.Duration(*d)
}

type Stage struct {
	Duration Duration `json:"duration"`
	Target   *float64 `json:"target"`
}

// ThresholdDef = string "p(95)<500" atau objek {threshold, abortOnFail, delayAbortEval}.
type ThresholdDef struct {
	Threshold      string
	AbortOnFail    bool
	DelayAbortEval time.Duration
}

func (t *ThresholdDef) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		return json.Unmarshal(b, &t.Threshold)
	}
	var o struct {
		Threshold      string    `json:"threshold"`
		AbortOnFail    bool      `json:"abortOnFail"`
		DelayAbortEval *Duration `json:"delayAbortEval"`
	}
	if err := json.Unmarshal(b, &o); err != nil {
		return fmt.Errorf("threshold harus string atau objek {threshold, abortOnFail}")
	}
	t.Threshold, t.AbortOnFail, t.DelayAbortEval = o.Threshold, o.AbortOnFail, o.DelayAbortEval.Get(0)
	return nil
}

type ThresholdGroup struct {
	Metric string
	Defs   []ThresholdDef
}

type Scenario struct {
	Executor         string            `json:"executor"`
	Exec             string            `json:"exec"`
	StartTime        *Duration         `json:"startTime"`
	GracefulStop     *Duration         `json:"gracefulStop"`
	Env              map[string]string `json:"env"`
	Tags             map[string]string `json:"tags"`
	VUs              *int64            `json:"vus"`
	Duration         *Duration         `json:"duration"`
	Iterations       *int64            `json:"iterations"`
	MaxDuration      *Duration         `json:"maxDuration"`
	StartVUs         *int64            `json:"startVUs"`
	Stages           []Stage           `json:"stages"`
	GracefulRampDown *Duration         `json:"gracefulRampDown"`
	Rate             *float64          `json:"rate"`
	StartRate        *float64          `json:"startRate"`
	TimeUnit         *Duration         `json:"timeUnit"`
	PreAllocatedVUs  *int64            `json:"preAllocatedVUs"`
	MaxVUs           *int64            `json:"maxVUs"`
}

type NamedScenario struct {
	Name string
	*Scenario
}

// Options adalah isi `export const options` (subset opsi k6).
type Options struct {
	VUs                   *int64            `json:"vus"`
	Duration              *Duration         `json:"duration"`
	Iterations            *int64            `json:"iterations"`
	Stages                []Stage           `json:"stages"`
	ScenariosRaw          json.RawMessage   `json:"scenarios"`
	ThresholdsRaw         json.RawMessage   `json:"thresholds"`
	UserAgent             *string           `json:"userAgent"`
	InsecureSkipTLSVerify *bool             `json:"insecureSkipTLSVerify"`
	MaxRedirects          *int              `json:"maxRedirects"`
	NoCookiesReset        *bool             `json:"noCookiesReset"`
	NoConnectionReuse     *bool             `json:"noConnectionReuse"`
	NoVUConnectionReuse   *bool             `json:"noVUConnectionReuse"`
	DiscardResponseBodies *bool             `json:"discardResponseBodies"`
	Batch                 *int              `json:"batch"`
	BatchPerHost          *int              `json:"batchPerHost"`
	Hosts                 map[string]string `json:"hosts"`
	SetupTimeout          *Duration         `json:"setupTimeout"`
	TeardownTimeout       *Duration         `json:"teardownTimeout"`
	SummaryTrendStats     []string          `json:"summaryTrendStats"`
	Tags                  map[string]string `json:"tags"`

	Scenarios  []NamedScenario  `json:"-"`
	Thresholds []ThresholdGroup `json:"-"`
}

// orderedObject membaca objek JSON dengan urutan kunci tetap terjaga.
func orderedObject(raw json.RawMessage) ([]string, map[string]json.RawMessage, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return nil, nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, nil, errors.New("harus berupa objek")
	}
	var keys []string
	vals := map[string]json.RawMessage{}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, nil, err
		}
		k := kt.(string)
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, nil, err
		}
		if _, dup := vals[k]; !dup {
			keys = append(keys, k)
		}
		vals[k] = v
	}
	return keys, vals, nil
}

func ParseOptions(js string) (*Options, error) {
	o := &Options{}
	if strings.TrimSpace(js) == "" {
		return o, nil
	}
	if err := json.Unmarshal([]byte(js), o); err != nil {
		return nil, fmt.Errorf("options tidak valid: %v", err)
	}
	keys, vals, err := orderedObject(o.ScenariosRaw)
	if err != nil {
		return nil, fmt.Errorf("options.scenarios: %v", err)
	}
	for _, k := range keys {
		sc := &Scenario{}
		if err := json.Unmarshal(vals[k], sc); err != nil {
			return nil, fmt.Errorf("options.scenarios.%s: %v", k, err)
		}
		o.Scenarios = append(o.Scenarios, NamedScenario{Name: k, Scenario: sc})
	}
	keys, vals, err = orderedObject(o.ThresholdsRaw)
	if err != nil {
		return nil, fmt.Errorf("options.thresholds: %v", err)
	}
	for _, k := range keys {
		var defs []ThresholdDef
		raw := bytes.TrimSpace(vals[k])
		if len(raw) > 0 && raw[0] != '[' {
			raw = append(append([]byte{'['}, raw...), ']')
		}
		if err := json.Unmarshal(raw, &defs); err != nil {
			return nil, fmt.Errorf("options.thresholds[%q]: %v", k, err)
		}
		o.Thresholds = append(o.Thresholds, ThresholdGroup{Metric: k, Defs: defs})
	}
	return o, nil
}

// StagePlan adalah satu tahap (VU atau laju) yang sudah dinormalisasi.
type StagePlan struct {
	Duration time.Duration
	Target   float64
}

// Plan adalah konfigurasi final satu skenario.
type Plan struct {
	Name, Executor, Exec    string
	StartTime, GracefulStop time.Duration
	Env, Tags               map[string]string

	VUs              int64
	Duration         time.Duration
	Iterations       int64
	MaxDuration      time.Duration
	StartVUs         int64
	Stages           []StagePlan
	GracefulRampDown time.Duration
	Rate, StartRate  float64
	TimeUnit         time.Duration
	PreAllocatedVUs  int64
	MaxVUs           int64
}

// CLIExec berisi opsi eksekusi dari flag CLI (menimpa script).
type CLIExec struct {
	VUs        *int64
	Duration   *time.Duration
	Iterations *int64
	Stages     []StagePlan
}

func (c CLIExec) any() bool {
	return c.VUs != nil || c.Duration != nil || c.Iterations != nil || len(c.Stages) > 0
}

func toStagePlans(st []Stage, what string) ([]StagePlan, error) {
	out := make([]StagePlan, 0, len(st))
	for i, s := range st {
		if s.Target == nil {
			return nil, fmt.Errorf("%s ke-%d: 'target' wajib diisi", what, i+1)
		}
		if *s.Target < 0 {
			return nil, fmt.Errorf("%s ke-%d: target tidak boleh negatif", what, i+1)
		}
		if s.Duration < 0 {
			return nil, fmt.Errorf("%s ke-%d: durasi tidak boleh negatif", what, i+1)
		}
		out = append(out, StagePlan{Duration: time.Duration(s.Duration), Target: *s.Target})
	}
	return out, nil
}

// BuildPlan menyusun daftar skenario final dari options script + flag CLI.
func BuildPlan(o *Options, cli CLIExec) ([]*Plan, []string, error) {
	var warns []string
	vus := o.VUs
	var dur *time.Duration
	if o.Duration != nil {
		d := time.Duration(*o.Duration)
		dur = &d
	}
	iters := o.Iterations
	stages, err := toStagePlans(o.Stages, "stage")
	if err != nil {
		return nil, nil, err
	}
	if cli.any() {
		if cli.Duration != nil || cli.Iterations != nil || len(cli.Stages) > 0 {
			dur, iters, stages = cli.Duration, cli.Iterations, cli.Stages
		}
		if cli.VUs != nil {
			vus = cli.VUs
		}
		if len(o.Scenarios) > 0 {
			warns = append(warns, "flag --vus/--duration/--iterations/--stage dipakai, jadi options.scenarios di script diabaikan")
		}
	} else if len(o.Scenarios) > 0 {
		if vus != nil || dur != nil || iters != nil || len(stages) > 0 {
			warns = append(warns, "options.vus/duration/iterations/stages diabaikan karena options.scenarios dipakai")
		}
		plans := make([]*Plan, 0, len(o.Scenarios))
		for _, ns := range o.Scenarios {
			p, err := scenarioPlan(ns.Name, ns.Scenario)
			if err != nil {
				return nil, nil, fmt.Errorf("skenario %q: %v", ns.Name, err)
			}
			if w := p.normalize(); w != "" {
				warns = append(warns, fmt.Sprintf("skenario %q: %s", ns.Name, w))
			}
			plans = append(plans, p)
		}
		return plans, warns, nil
	}

	p := &Plan{Name: "default", Exec: "default", GracefulStop: 30 * time.Second, MaxDuration: 10 * time.Minute}
	v := int64(1)
	if vus != nil {
		v = *vus
	}
	if v < 0 {
		return nil, nil, errors.New("vus tidak boleh negatif")
	}
	switch {
	case len(stages) > 0:
		if dur != nil || iters != nil {
			warns = append(warns, "duration/iterations diabaikan karena stages dipakai")
		}
		p.Executor, p.StartVUs, p.Stages, p.GracefulRampDown = "ramping-vus", v, stages, 30*time.Second
		if vus == nil {
			p.StartVUs = 1
		}
	case iters != nil:
		p.Executor, p.Iterations, p.VUs = "shared-iterations", *iters, v
		if dur != nil {
			p.MaxDuration = *dur
		}
	case dur != nil:
		p.Executor, p.Duration, p.VUs = "constant-vus", *dur, v
	default:
		p.Executor, p.Iterations, p.VUs = "per-vu-iterations", 1, v
	}
	if err := p.validate(); err != nil {
		return nil, nil, err
	}
	if w := p.normalize(); w != "" {
		warns = append(warns, w)
	}
	return []*Plan{p}, warns, nil
}

func i64(p *int64, def int64) int64 {
	if p == nil {
		return def
	}
	return *p
}

func f64(p *float64, def float64) float64 {
	if p == nil {
		return def
	}
	return *p
}

func scenarioPlan(name string, s *Scenario) (*Plan, error) {
	p := &Plan{
		Name:         name,
		Executor:     s.Executor,
		Exec:         s.Exec,
		StartTime:    s.StartTime.Get(0),
		GracefulStop: s.GracefulStop.Get(30 * time.Second),
		Env:          s.Env,
		Tags:         s.Tags,
	}
	if p.Exec == "" {
		p.Exec = "default"
	}
	switch s.Executor {
	case "constant-vus":
		p.VUs, p.Duration = i64(s.VUs, 1), s.Duration.Get(0)
	case "ramping-vus":
		st, err := toStagePlans(s.Stages, "stage")
		if err != nil {
			return nil, err
		}
		p.StartVUs, p.Stages, p.GracefulRampDown = i64(s.StartVUs, 1), st, s.GracefulRampDown.Get(30*time.Second)
	case "shared-iterations", "per-vu-iterations":
		p.VUs, p.Iterations, p.MaxDuration = i64(s.VUs, 1), i64(s.Iterations, 1), s.MaxDuration.Get(10*time.Minute)
	case "constant-arrival-rate", "ramping-arrival-rate":
		if s.PreAllocatedVUs == nil {
			return nil, fmt.Errorf("preAllocatedVUs wajib diisi untuk %s (jumlah VU yang disiapkan di awal, mis. 10)", s.Executor)
		}
		p.PreAllocatedVUs = *s.PreAllocatedVUs
		p.MaxVUs = i64(s.MaxVUs, p.PreAllocatedVUs)
		p.TimeUnit = s.TimeUnit.Get(time.Second)
		if s.Executor == "constant-arrival-rate" {
			if s.Rate == nil {
				return nil, errors.New("rate wajib diisi (jumlah iterasi per timeUnit)")
			}
			p.Rate, p.Duration = *s.Rate, s.Duration.Get(0)
		} else {
			st, err := toStagePlans(s.Stages, "stage")
			if err != nil {
				return nil, err
			}
			p.StartRate, p.Stages = f64(s.StartRate, 0), st
		}
	case "":
		return nil, errors.New("executor wajib diisi (constant-vus, ramping-vus, shared-iterations, per-vu-iterations, constant-arrival-rate, ramping-arrival-rate)")
	case "externally-controlled":
		return nil, errors.New("executor externally-controlled belum didukung")
	default:
		return nil, fmt.Errorf("executor %q tidak dikenal", s.Executor)
	}
	return p, p.validate()
}

func (p *Plan) validate() error {
	if p.StartTime < 0 || p.GracefulStop < 0 {
		return errors.New("startTime dan gracefulStop tidak boleh negatif")
	}
	switch p.Executor {
	case "constant-vus":
		if p.VUs < 1 {
			return errors.New("vus minimal 1")
		}
		if p.Duration <= 0 {
			return errors.New("duration wajib diisi dan harus lebih dari 0 (mis. '30s')")
		}
	case "ramping-vus":
		if len(p.Stages) == 0 {
			return errors.New("stages wajib diisi minimal satu tahap")
		}
		if p.StartVUs < 0 {
			return errors.New("startVUs tidak boleh negatif")
		}
		if p.maxStageVUs() < 1 {
			return errors.New("semua target stages 0: tidak ada VU yang akan berjalan")
		}
		if p.stagesDuration() <= 0 {
			return errors.New("total durasi stages harus lebih dari 0")
		}
	case "shared-iterations", "per-vu-iterations":
		if p.VUs < 1 {
			return errors.New("vus minimal 1")
		}
		if p.Iterations < 1 {
			return errors.New("iterations minimal 1")
		}
		if p.MaxDuration <= 0 {
			return errors.New("maxDuration harus lebih dari 0")
		}
	case "constant-arrival-rate", "ramping-arrival-rate":
		if p.PreAllocatedVUs < 1 {
			return errors.New("preAllocatedVUs minimal 1")
		}
		if p.MaxVUs < p.PreAllocatedVUs {
			return fmt.Errorf("maxVUs (%d) tidak boleh lebih kecil dari preAllocatedVUs (%d)", p.MaxVUs, p.PreAllocatedVUs)
		}
		if p.TimeUnit <= 0 {
			return errors.New("timeUnit harus lebih dari 0")
		}
		if p.Executor == "constant-arrival-rate" {
			if p.Rate <= 0 {
				return errors.New("rate harus lebih dari 0")
			}
			if p.Duration <= 0 {
				return errors.New("duration wajib diisi dan harus lebih dari 0")
			}
		} else {
			if len(p.Stages) == 0 {
				return errors.New("stages wajib diisi minimal satu tahap")
			}
			if p.StartRate < 0 {
				return errors.New("startRate tidak boleh negatif")
			}
			if p.stagesDuration() <= 0 {
				return errors.New("total durasi stages harus lebih dari 0")
			}
		}
	}
	return nil
}

// normalize merapikan nilai yang tidak masuk akal tapi tidak fatal.
func (p *Plan) normalize() string {
	if p.Executor == "shared-iterations" && p.VUs > p.Iterations {
		w := fmt.Sprintf("vus (%d) lebih banyak dari iterations (%d); VU dikurangi menjadi %d", p.VUs, p.Iterations, p.Iterations)
		p.VUs = p.Iterations
		return w
	}
	return ""
}

func (p *Plan) stagesDuration() time.Duration {
	var d time.Duration
	for _, s := range p.Stages {
		d += s.Duration
	}
	return d
}

func (p *Plan) maxStageVUs() int64 {
	m := p.StartVUs
	for _, s := range p.Stages {
		if int64(s.Target) > m {
			m = int64(s.Target)
		}
	}
	return m
}

// InitVUs = jumlah VU yang disiapkan sebelum tes dimulai.
func (p *Plan) InitVUs() int64 {
	switch p.Executor {
	case "ramping-vus":
		return p.maxStageVUs()
	case "constant-arrival-rate", "ramping-arrival-rate":
		return p.PreAllocatedVUs
	}
	return p.VUs
}

// MaxVUsTotal = VU maksimum yang mungkin dipakai skenario ini.
func (p *Plan) MaxVUsTotal() int64 {
	if strings.HasSuffix(p.Executor, "arrival-rate") {
		return p.MaxVUs
	}
	return p.InitVUs()
}

// PlannedDuration = lama skenario berjalan (tanpa graceful stop).
func (p *Plan) PlannedDuration() time.Duration {
	switch p.Executor {
	case "constant-vus", "constant-arrival-rate":
		return p.Duration
	case "ramping-vus", "ramping-arrival-rate":
		return p.stagesDuration()
	}
	return p.MaxDuration
}

// RampTarget menghitung jumlah VU aktif pada waktu `el` (ramping-vus).
// Seperti k6: VU baru ditambahkan tepat saat garis linear mencapai bilangan bulat berikutnya.
func (p *Plan) RampTarget(el time.Duration) int64 {
	from := float64(p.StartVUs)
	var acc time.Duration
	for _, st := range p.Stages {
		if el < acc+st.Duration {
			frac := float64(el-acc) / float64(st.Duration)
			v := from + (st.Target-from)*frac
			if st.Target >= from {
				return int64(math.Floor(v + 1e-9))
			}
			return int64(math.Ceil(v - 1e-9))
		}
		acc += st.Duration
		from = st.Target
	}
	return int64(from)
}

// ArrivalTime menghitung kapan iterasi ke-k (mulai 0) harus dimulai (arrival-rate).
// Untuk ramping, jumlah iterasi = luas di bawah kurva laju (linear per tahap).
func (p *Plan) ArrivalTime(k int64) (time.Duration, bool) {
	unit := p.TimeUnit.Seconds()
	if p.Executor == "constant-arrival-rate" {
		at := time.Duration(float64(k) * float64(p.TimeUnit) / p.Rate)
		return at, at < p.Duration
	}
	need := float64(k)
	acc, t0, from := 0.0, 0.0, p.StartRate/unit
	for _, st := range p.Stages {
		d := st.Duration.Seconds()
		to := st.Target / unit
		seg := (from + to) / 2 * d
		if d > 0 && acc+seg >= need {
			r := need - acc
			tau := 0.0
			if r > 0 {
				slope := (to - from) / d
				tau = 2 * r / (from + math.Sqrt(math.Max(0, from*from+2*slope*r)))
			}
			return time.Duration((t0 + tau) * float64(time.Second)), true
		}
		acc += seg
		t0 += d
		from = to
	}
	return 0, false
}

func fmtDur(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	if d < time.Second {
		return d.String()
	}
	d = d.Round(100 * time.Millisecond)
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

func fmtRate(r float64, unit time.Duration) string {
	u := fmtDur(unit)
	if unit == time.Second {
		u = "detik"
	} else if unit == time.Minute {
		u = "menit"
	}
	return fmt.Sprintf("%s iterasi/%s", strconv.FormatFloat(r, 'f', -1, 64), u)
}

// Describe menjelaskan skenario dalam bahasa Indonesia.
func (p *Plan) Describe() string {
	switch p.Executor {
	case "constant-vus":
		return fmt.Sprintf("%d VU berulang selama %s", p.VUs, fmtDur(p.Duration))
	case "ramping-vus":
		parts := []string{strconv.FormatInt(p.StartVUs, 10)}
		for _, s := range p.Stages {
			parts = append(parts, strconv.FormatInt(int64(s.Target), 10))
		}
		return fmt.Sprintf("VU bertahap %s dalam %s (puncak %d VU)", strings.Join(parts, "→"), fmtDur(p.stagesDuration()), p.maxStageVUs())
	case "shared-iterations":
		return fmt.Sprintf("%d iterasi dibagi ke %d VU (maks. %s)", p.Iterations, p.VUs, fmtDur(p.MaxDuration))
	case "per-vu-iterations":
		return fmt.Sprintf("%d iterasi per VU × %d VU (maks. %s)", p.Iterations, p.VUs, fmtDur(p.MaxDuration))
	case "constant-arrival-rate":
		return fmt.Sprintf("%s selama %s (VU %d–%d)", fmtRate(p.Rate, p.TimeUnit), fmtDur(p.Duration), p.PreAllocatedVUs, p.MaxVUs)
	case "ramping-arrival-rate":
		parts := []string{strconv.FormatFloat(p.StartRate, 'f', -1, 64)}
		for _, s := range p.Stages {
			parts = append(parts, strconv.FormatFloat(s.Target, 'f', -1, 64))
		}
		return fmt.Sprintf("laju bertahap %s per %s dalam %s (VU %d–%d)", strings.Join(parts, "→"), fmtDur(p.TimeUnit), fmtDur(p.stagesDuration()), p.PreAllocatedVUs, p.MaxVUs)
	}
	return p.Executor
}
