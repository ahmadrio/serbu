package engine

import (
	"bufio"
	"encoding/json"
	"os"
	"sync/atomic"
	"time"

	"serbu/internal/metrics"
	"serbu/internal/ui"
)

// collector adalah satu-satunya goroutine yang mengisi sink metrik.
type collector struct {
	in       chan []metrics.Sample
	b        *metrics.Builtin
	checks   *ui.CheckTree
	statuses map[string]int64
	json     *jsonWriter
	done     chan struct{}

	httpReqs, httpFailed atomic.Int64

	abortable []*metrics.Metric
	runStart  atomic.Int64
	onAbort   func(metric string, th *metrics.Threshold)
	aborted   bool
}

func newCollector(b *metrics.Builtin, bufSize int) *collector {
	return &collector{
		in:       make(chan []metrics.Sample, bufSize),
		b:        b,
		checks:   ui.NewCheckTree(),
		statuses: map[string]int64{},
		done:     make(chan struct{}),
	}
}

func (c *collector) run() {
	defer close(c.done)
	var tick <-chan time.Time
	if len(c.abortable) > 0 {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		tick = t.C
	}
	for {
		select {
		case batch, ok := <-c.in:
			if !ok {
				if c.json != nil {
					c.json.close()
				}
				return
			}
			for i := range batch {
				c.add(&batch[i])
			}
		case <-tick:
			c.evalAbort()
		}
	}
}

func (c *collector) add(s *metrics.Sample) {
	m := s.Metric
	m.Observed = true
	m.Sink.Add(s.Value)
	for _, sub := range m.Submetrics {
		if sub.Filter.Matches(s.Tags) {
			sub.Observed = true
			sub.Sink.Add(s.Value)
		}
	}
	switch m {
	case c.b.Checks:
		c.checks.Add(s.Tags["group"], s.Tags["check"], s.Value != 0)
	case c.b.HTTPReqs:
		c.statuses[s.Tags["status"]]++
		c.httpReqs.Add(1)
	case c.b.HTTPReqFailed:
		if s.Value != 0 {
			c.httpFailed.Add(1)
		}
	}
	if c.json != nil {
		c.json.write(s)
	}
}

// evalAbort memeriksa threshold abortOnFail selama tes berjalan.
func (c *collector) evalAbort() {
	start := c.runStart.Load()
	if c.aborted || start == 0 {
		return
	}
	el := time.Since(time.Unix(0, start))
	for _, m := range c.abortable {
		for _, th := range m.Thresholds {
			if !th.AbortOnFail || el < th.DelayAbortEval {
				continue
			}
			if !th.Evaluate(m, el) {
				c.aborted = true
				c.onAbort(m.Name, th)
				return
			}
		}
	}
}

// jsonWriter menulis setiap titik metrik sebagai NDJSON (format sama dengan `k6 --out json`).
type jsonWriter struct {
	f    *os.File
	w    *bufio.Writer
	enc  *json.Encoder
	seen map[*metrics.Metric]bool
}

func newJSONWriter(path string) (*jsonWriter, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	w := bufio.NewWriterSize(f, 256<<10)
	return &jsonWriter{f: f, w: w, enc: json.NewEncoder(w), seen: map[*metrics.Metric]bool{}}, nil
}

type jsonMetric struct {
	Type   string         `json:"type"`
	Data   jsonMetricData `json:"data"`
	Metric string         `json:"metric"`
}

type jsonMetricData struct {
	Name       string   `json:"name"`
	Type       string   `json:"type"`
	Contains   string   `json:"contains"`
	Thresholds []string `json:"thresholds"`
}

type jsonPoint struct {
	Type   string        `json:"type"`
	Data   jsonPointData `json:"data"`
	Metric string        `json:"metric"`
}

type jsonPointData struct {
	Time  string            `json:"time"`
	Value float64           `json:"value"`
	Tags  map[string]string `json:"tags"`
}

func (j *jsonWriter) write(s *metrics.Sample) {
	m := s.Metric
	if !j.seen[m] {
		j.seen[m] = true
		ths := []string{}
		for _, t := range m.Thresholds {
			ths = append(ths, t.Source)
		}
		_ = j.enc.Encode(jsonMetric{Type: "Metric", Metric: m.Name, Data: jsonMetricData{Name: m.Name, Type: m.Type.String(), Contains: m.Contains.String(), Thresholds: ths}})
	}
	_ = j.enc.Encode(jsonPoint{Type: "Point", Metric: m.Name, Data: jsonPointData{Time: s.Time.Format(time.RFC3339Nano), Value: s.Value, Tags: s.Tags}})
}

func (j *jsonWriter) close() {
	_ = j.w.Flush()
	_ = j.f.Close()
}
