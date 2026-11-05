package engine

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"serbu/internal/js"
	"serbu/internal/metrics"
)

// scenarioRun adalah satu skenario yang sedang berjalan beserta VU-nya.
type scenarioRun struct {
	plan *Plan
	e    *Engine
	info *js.ScenarioInfo
	vus  []*js.VU

	iterInTest  atomic.Int64
	active      atomic.Int64
	completed   atomic.Int64
	interrupted atomic.Int64
	dropped     atomic.Int64
	initialized atomic.Int64
	started     atomic.Bool
	finished    atomic.Bool
	startTime   time.Time
}

func (s *scenarioRun) progress() float64 {
	if !s.started.Load() {
		return 0
	}
	if s.finished.Load() {
		return 1
	}
	p := s.plan
	timeFrac := float64(time.Since(s.startTime)) / float64(p.PlannedDuration())
	var f float64
	switch p.Executor {
	case "shared-iterations":
		f = float64(s.completed.Load()+s.interrupted.Load()) / float64(p.Iterations)
	case "per-vu-iterations":
		f = float64(s.completed.Load()+s.interrupted.Load()) / float64(p.Iterations*p.VUs)
	default:
		f = timeFrac
	}
	if f > 1 {
		f = 1
	}
	return f
}

// iterate menjalankan satu iterasi lalu mencatat metrik iterations/iteration_duration.
func (s *scenarioRun) iterate(ctx context.Context, vu *js.VU) {
	n := s.iterInTest.Add(1) - 1
	start := time.Now()
	err := vu.RunIteration(ctx, s.plan.Exec, n)
	if js.IsInterrupted(err) {
		s.interrupted.Add(1)
		s.e.interrupted.Add(1)
		return
	}
	el := time.Since(start)
	tags := metrics.Tags{"scenario": s.plan.Name, "group": ""}
	for k, v := range s.e.globalTags {
		tags[k] = v
	}
	for k, v := range s.plan.Tags {
		tags[k] = v
	}
	now := time.Now()
	s.e.out <- []metrics.Sample{
		{Metric: s.e.b.Iterations, Value: 1, Tags: tags, Time: now},
		{Metric: s.e.b.IterationDuration, Value: float64(el) / float64(time.Millisecond), Tags: tags, Time: now},
	}
	s.completed.Add(1)
	s.e.completed.Add(1)
	if err != nil {
		s.e.scriptError(vu, err)
	}
}

func (s *scenarioRun) run(ctx context.Context) {
	s.startTime = time.Now()
	s.info.StartNano.Store(s.startTime.UnixNano())
	s.started.Store(true)
	defer s.finished.Store(true)
	switch s.plan.Executor {
	case "constant-vus":
		s.runLooping(ctx, s.plan.Duration, func(*js.VU) bool { return true })
	case "shared-iterations":
		var counter atomic.Int64
		s.runLooping(ctx, s.plan.MaxDuration, func(*js.VU) bool { return counter.Add(1) <= s.plan.Iterations })
	case "per-vu-iterations":
		counts := make(map[*js.VU]*int64, len(s.vus))
		for _, vu := range s.vus {
			counts[vu] = new(int64)
		}
		s.runLooping(ctx, s.plan.MaxDuration, func(vu *js.VU) bool {
			c := counts[vu]
			*c++
			return *c <= s.plan.Iterations
		})
	case "ramping-vus":
		s.runRampingVUs(ctx)
	case "constant-arrival-rate", "ramping-arrival-rate":
		s.runArrivalRate(ctx)
	}
}

// runLooping: setiap VU mengulang iterasi sampai `next` false atau waktu habis.
func (s *scenarioRun) runLooping(ctx context.Context, limit time.Duration, next func(*js.VU) bool) {
	soft, cancelSoft := context.WithDeadline(ctx, s.startTime.Add(limit))
	hard, cancelHard := context.WithDeadline(ctx, s.startTime.Add(limit+s.plan.GracefulStop))
	defer cancelSoft()
	defer cancelHard()
	var wg sync.WaitGroup
	for _, vu := range s.vus[:s.plan.VUs] {
		wg.Add(1)
		s.active.Add(1)
		go func(vu *js.VU) {
			defer wg.Done()
			defer s.active.Add(-1)
			for soft.Err() == nil && next(vu) {
				s.iterate(hard, vu)
			}
		}(vu)
	}
	wg.Wait()
}

type broadcast struct {
	mu sync.Mutex
	ch chan struct{}
}

func newBroadcast() *broadcast { return &broadcast{ch: make(chan struct{})} }

func (b *broadcast) wait() <-chan struct{} {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.ch
}

func (b *broadcast) notify() {
	b.mu.Lock()
	close(b.ch)
	b.ch = make(chan struct{})
	b.mu.Unlock()
}

// vuSlot memungkinkan controller menghentikan iterasi VU saat ramp-down.
type vuSlot struct {
	mu      sync.Mutex
	running bool
	seq     uint64
	cancel  context.CancelFunc
}

func (sl *vuSlot) begin(parent context.Context) context.Context {
	ctx, cancel := context.WithCancel(parent)
	sl.mu.Lock()
	sl.running, sl.cancel = true, cancel
	sl.seq++
	sl.mu.Unlock()
	return ctx
}

func (sl *vuSlot) end() {
	sl.mu.Lock()
	c := sl.cancel
	sl.running, sl.cancel = false, nil
	sl.mu.Unlock()
	if c != nil {
		c()
	}
}

// rampDown memberi iterasi yang sedang jalan waktu `grace` untuk selesai.
func (sl *vuSlot) rampDown(grace time.Duration, stillOut func() bool) {
	sl.mu.Lock()
	if !sl.running {
		sl.mu.Unlock()
		return
	}
	seq, c := sl.seq, sl.cancel
	sl.mu.Unlock()
	time.AfterFunc(grace, func() {
		sl.mu.Lock()
		same := sl.running && sl.seq == seq
		sl.mu.Unlock()
		if same && stillOut() {
			c()
		}
	})
}

func (s *scenarioRun) runRampingVUs(ctx context.Context) {
	p := s.plan
	total := p.stagesDuration()
	soft, cancelSoft := context.WithDeadline(ctx, s.startTime.Add(total))
	hard, cancelHard := context.WithDeadline(ctx, s.startTime.Add(total+p.GracefulStop))
	defer cancelSoft()
	defer cancelHard()

	var target atomic.Int64
	target.Store(p.RampTarget(0))
	changed := newBroadcast()
	slots := make([]*vuSlot, len(s.vus))
	for i := range slots {
		slots[i] = &vuSlot{}
	}
	go func() {
		tick := time.NewTicker(10 * time.Millisecond)
		defer tick.Stop()
		prev := target.Load()
		for {
			select {
			case <-soft.Done():
				changed.notify()
				return
			case <-tick.C:
			}
			t := p.RampTarget(time.Since(s.startTime))
			if t == prev {
				continue
			}
			target.Store(t)
			if t < prev {
				for i := t; i < prev && int(i) < len(slots); i++ {
					idx := i
					slots[idx].rampDown(p.GracefulRampDown, func() bool { return idx >= target.Load() })
				}
			}
			prev = t
			changed.notify()
		}
	}()

	var wg sync.WaitGroup
	for i, vu := range s.vus {
		wg.Add(1)
		go func(i int64, vu *js.VU, sl *vuSlot) {
			defer wg.Done()
			for soft.Err() == nil {
				if i >= target.Load() {
					ch := changed.wait()
					if i < target.Load() {
						continue
					}
					select {
					case <-ch:
					case <-soft.Done():
						return
					}
					continue
				}
				s.active.Add(1)
				s.iterate(sl.begin(hard), vu)
				sl.end()
				s.active.Add(-1)
			}
		}(int64(i), vu, slots[i])
	}
	wg.Wait()
}

func (s *scenarioRun) runArrivalRate(ctx context.Context) {
	p := s.plan
	total := p.PlannedDuration()
	soft, cancelSoft := context.WithDeadline(ctx, s.startTime.Add(total))
	hard, cancelHard := context.WithDeadline(ctx, s.startTime.Add(total+p.GracefulStop))
	defer cancelSoft()
	defer cancelHard()

	idle := make(chan *js.VU, p.MaxVUs)
	for _, vu := range s.vus {
		idle <- vu
	}
	s.initialized.Store(int64(len(s.vus)))
	var pending atomic.Int64
	initSem := make(chan struct{}, 4)
	warned := false
	var wg sync.WaitGroup
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()

	for k := int64(0); ; k++ {
		at, ok := p.ArrivalTime(k)
		if !ok || at >= total {
			break
		}
		if d := time.Until(s.startTime.Add(at)); d > 0 {
			timer.Reset(d)
			select {
			case <-timer.C:
			case <-soft.Done():
			}
		}
		if soft.Err() != nil {
			break
		}
		select {
		case vu := <-idle:
			wg.Add(1)
			s.active.Add(1)
			go func(vu *js.VU) {
				defer wg.Done()
				s.iterate(hard, vu)
				s.active.Add(-1)
				idle <- vu
			}(vu)
		default:
			s.dropped.Add(1)
			s.e.out <- []metrics.Sample{{Metric: s.e.b.DroppedIterations, Value: 1, Tags: metrics.Tags{"scenario": p.Name}, Time: time.Now()}}
			if s.initialized.Load()+pending.Load() < p.MaxVUs {
				select {
				case initSem <- struct{}{}:
					pending.Add(1)
					go func() {
						defer func() { <-initSem; pending.Add(-1) }()
						vu, err := s.e.newVU(s)
						if err != nil {
							s.e.fatal(err)
							return
						}
						s.initialized.Add(1)
						idle <- vu
					}()
				default:
				}
			} else if !warned {
				warned = true
				s.e.con.Warn("skenario %q: semua %d VU sedang sibuk, sebagian iterasi tidak bisa dijalankan. Naikkan maxVUs bila perlu.", p.Name, p.MaxVUs)
			}
		}
	}
	wg.Wait()
}
