// Package metrics berisi tipe metrik, registry, sink agregasi, dan threshold.
package metrics

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

type MetricType int

const (
	Counter MetricType = iota
	Gauge
	Rate
	Trend
)

func (t MetricType) String() string {
	switch t {
	case Counter:
		return "counter"
	case Gauge:
		return "gauge"
	case Rate:
		return "rate"
	case Trend:
		return "trend"
	}
	return "unknown"
}

// ValueType menentukan cara nilai ditampilkan (angka biasa, durasi, atau byte).
type ValueType int

const (
	Default ValueType = iota
	Time
	Data
)

func (v ValueType) String() string {
	switch v {
	case Time:
		return "time"
	case Data:
		return "data"
	}
	return "default"
}

// Tags adalah label pada setiap sampel (method, status, name, group, scenario, ...).
type Tags map[string]string

// Matches true jika semua pasangan di f ada di t.
func (f Tags) Matches(t Tags) bool {
	for k, v := range f {
		if t[k] != v {
			return false
		}
	}
	return true
}

// Sample adalah satu titik data metrik.
type Sample struct {
	Metric *Metric
	Value  float64
	Tags   Tags
	Time   time.Time
}

// Metric bisa berupa metrik utama atau submetric (Parent != nil, difilter Filter).
type Metric struct {
	Name       string
	Type       MetricType
	Contains   ValueType
	Sink       Sink
	Thresholds []*Threshold
	Submetrics []*Metric
	Observed   bool

	Parent *Metric
	Filter Tags
}

var nameRe = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]{0,127}$`)

type Registry struct {
	mu      sync.Mutex
	metrics map[string]*Metric
	order   []*Metric
}

func NewRegistry() *Registry {
	return &Registry{metrics: map[string]*Metric{}}
}

// New membuat metrik baru, atau mengembalikan yang sudah ada bila tipenya sama.
func (r *Registry) New(name string, t MetricType, vt ValueType) (*Metric, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if m, ok := r.metrics[name]; ok {
		if m.Type != t {
			return nil, fmt.Errorf("metric %q sudah terdaftar sebagai %s, tidak bisa dibuat ulang sebagai %s", name, m.Type, t)
		}
		return m, nil
	}
	if !nameRe.MatchString(name) {
		return nil, fmt.Errorf("nama metric %q tidak valid: gunakan huruf, angka, dan underscore (maks. 128 karakter, tidak diawali angka)", name)
	}
	m := &Metric{Name: name, Type: t, Contains: vt, Sink: NewSink(t)}
	r.metrics[name] = m
	r.order = append(r.order, m)
	return m, nil
}

func (r *Registry) Get(name string) *Metric {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.metrics[name]
}

// All mengembalikan semua metrik utama sesuai urutan pendaftaran.
func (r *Registry) All() []*Metric {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*Metric, len(r.order))
	copy(out, r.order)
	return out
}

// Submetric mengembalikan metrik untuk ekspresi seperti `http_req_duration{name:login}`.
// Tanpa kurung kurawal, metrik utama yang dikembalikan.
func (r *Registry) Submetric(expr string) (*Metric, error) {
	name, filter, err := ParseMetricExpr(expr)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	parent := r.metrics[name]
	if parent == nil {
		return nil, fmt.Errorf("metric %q tidak ditemukan", name)
	}
	if len(filter) == 0 {
		return parent, nil
	}
	for _, sub := range parent.Submetrics {
		if sameTags(sub.Filter, filter) {
			return sub, nil
		}
	}
	sub := &Metric{
		Name:     name + "{" + filter.String() + "}",
		Type:     parent.Type,
		Contains: parent.Contains,
		Sink:     NewSink(parent.Type),
		Parent:   parent,
		Filter:   filter,
	}
	parent.Submetrics = append(parent.Submetrics, sub)
	return sub, nil
}

// String menulis tag terurut: "method:POST,name:login".
func (f Tags) String() string {
	keys := make([]string, 0, len(f))
	for k := range f {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + ":" + f[k]
	}
	return strings.Join(parts, ",")
}

func sameTags(a, b Tags) bool {
	if len(a) != len(b) {
		return false
	}
	return a.Matches(b)
}

// ParseMetricExpr memecah `nama{tag:nilai,tag2:nilai2}`.
func ParseMetricExpr(expr string) (string, Tags, error) {
	expr = strings.TrimSpace(expr)
	i := strings.IndexByte(expr, '{')
	if i < 0 {
		return expr, nil, nil
	}
	if !strings.HasSuffix(expr, "}") {
		return "", nil, fmt.Errorf("format submetric salah: %q (contoh benar: http_req_duration{name:login})", expr)
	}
	name := strings.TrimSpace(expr[:i])
	body := expr[i+1 : len(expr)-1]
	tags := Tags{}
	for _, part := range strings.Split(body, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		k, v, ok := strings.Cut(part, ":")
		if !ok {
			return "", nil, fmt.Errorf("format tag salah pada %q: %q harus berbentuk tag:nilai", expr, part)
		}
		k = strings.TrimSpace(k)
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		tags[k] = v
	}
	return name, tags, nil
}

// Builtin menyimpan pointer ke metrik bawaan (nama sama dengan k6).
type Builtin struct {
	VUs, VUsMax                                      *Metric
	Iterations, IterationDuration, DroppedIterations *Metric
	DataSent, DataReceived                           *Metric
	HTTPReqs, HTTPReqFailed, HTTPReqDuration         *Metric
	HTTPReqBlocked, HTTPReqConnecting, HTTPReqTLS    *Metric
	HTTPReqSending, HTTPReqWaiting, HTTPReqReceiving *Metric
	Checks, GroupDuration                            *Metric
}

func RegisterBuiltin(r *Registry) *Builtin {
	must := func(name string, t MetricType, vt ValueType) *Metric {
		m, err := r.New(name, t, vt)
		if err != nil {
			panic(err)
		}
		return m
	}
	return &Builtin{
		VUs:               must("vus", Gauge, Default),
		VUsMax:            must("vus_max", Gauge, Default),
		Iterations:        must("iterations", Counter, Default),
		IterationDuration: must("iteration_duration", Trend, Time),
		DroppedIterations: must("dropped_iterations", Counter, Default),
		DataSent:          must("data_sent", Counter, Data),
		DataReceived:      must("data_received", Counter, Data),
		HTTPReqs:          must("http_reqs", Counter, Default),
		HTTPReqFailed:     must("http_req_failed", Rate, Default),
		HTTPReqDuration:   must("http_req_duration", Trend, Time),
		HTTPReqBlocked:    must("http_req_blocked", Trend, Time),
		HTTPReqConnecting: must("http_req_connecting", Trend, Time),
		HTTPReqTLS:        must("http_req_tls_handshaking", Trend, Time),
		HTTPReqSending:    must("http_req_sending", Trend, Time),
		HTTPReqWaiting:    must("http_req_waiting", Trend, Time),
		HTTPReqReceiving:  must("http_req_receiving", Trend, Time),
		Checks:            must("checks", Rate, Default),
		GroupDuration:     must("group_duration", Trend, Time),
	}
}

// IsBuiltin true untuk nama metrik bawaan.
func IsBuiltin(name string) bool {
	switch name {
	case "vus", "vus_max", "iterations", "iteration_duration", "dropped_iterations",
		"data_sent", "data_received", "http_reqs", "http_req_failed", "http_req_duration",
		"http_req_blocked", "http_req_connecting", "http_req_tls_handshaking", "http_req_sending",
		"http_req_waiting", "http_req_receiving", "checks", "group_duration":
		return true
	}
	return false
}
