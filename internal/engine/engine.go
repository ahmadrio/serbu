package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"serbu/internal/js"
	"serbu/internal/metrics"
	"serbu/internal/ui"
)

// Exit code mengikuti k6 agar mudah dipakai di CI.
const (
	ExitOK              = 0
	ExitThresholds      = 99
	ExitSetupTimeout    = 100
	ExitTeardownTimeout = 101
	ExitConfig          = 104
	ExitSignal          = 105
	ExitScript          = 107
	ExitAborted         = 108
)

type Config struct {
	Script        string
	Version       string
	Env           map[string]string
	CLI           CLIExec
	SummaryExport string
	JSONOut       string
	HTTPDebug     string
	Quiet         bool
	NoColor       bool
	NoThresholds  bool
	NoSummary     bool
	TrendStats    []string
	InsecureTLS   bool
	UserAgent     *string
}

type Engine struct {
	cfg        Config
	con        *ui.Console
	reg        *metrics.Registry
	b          *metrics.Builtin
	sh         *js.Shared
	out        chan []metrics.Sample
	coll       *collector
	opts       *Options
	optsJSON   string
	plans      []*Plan
	runs       []*scenarioRun
	env        map[string]string
	globalTags map[string]string
	trendStats []string

	runCtx      context.Context
	cancelRun   context.CancelCauseFunc
	runStart    time.Time
	nextVUID    atomic.Int64
	vusInit     atomic.Int64
	completed   atomic.Int64
	interrupted atomic.Int64

	stopMu     sync.Mutex
	stopCode   int
	stopReason string
	fatalErr   error
}

// Run menjalankan satu tes dan mengembalikan exit code.
func Run(cfg Config) int {
	e := &Engine{cfg: cfg, con: ui.NewConsole(cfg.NoColor)}
	return e.run()
}

func indent(s, pad string) string {
	return pad + strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", "\n"+pad)
}

func (e *Engine) run() int {
	con := e.con
	prg, dir, err := js.Compile(e.cfg.Script)
	if err != nil {
		con.Error("Gagal memuat script %s:\n%s", e.cfg.Script, indent(err.Error(), "  "))
		return ExitScript
	}
	e.reg = metrics.NewRegistry()
	e.b = metrics.RegisterBuiltin(e.reg)
	e.coll = newCollector(e.b, 8192)
	e.out = e.coll.in
	e.env = map[string]string{}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			e.env[k] = v
		}
	}
	for k, v := range e.cfg.Env {
		e.env[k] = v
	}
	e.sh = &js.Shared{
		Program: prg, ScriptDir: dir, Registry: e.reg, B: e.b, Out: e.out, Console: con, Host: e,
		ScriptErrs: js.NewErrorLog(), HTTPErrs: js.NewErrorLog(),
	}
	vu0, err := js.NewVU(e.sh, 0, e.env, nil)
	if err != nil {
		con.Error("%s", err)
		return ExitScript
	}

	if code := e.configure(vu0); code != ExitOK {
		return code
	}
	e.printBanner()

	e.runCtx, e.cancelRun = context.WithCancelCause(context.Background())
	e.handleSignals()
	go e.coll.run()
	finishMetrics := func() {
		close(e.out)
		<-e.coll.done
	}

	if err := e.initVUs(); err != nil {
		con.Error("Gagal menyiapkan VU:\n%s", indent(err.Error(), "  "))
		finishMetrics()
		return ExitScript
	}

	if e.runCtx.Err() == nil {
		sctx, cancel := context.WithTimeout(e.runCtx, e.opts.SetupTimeout.Get(time.Minute))
		data, has, err := vu0.RunSetup(sctx)
		timedOut := errors.Is(sctx.Err(), context.DeadlineExceeded)
		cancel()
		if err != nil {
			finishMetrics()
			switch {
			case timedOut:
				con.Error("setup() melebihi batas waktu %s (ubah dengan options.setupTimeout)", e.opts.SetupTimeout.Get(time.Minute))
				return ExitSetupTimeout
			case e.runCtx.Err() != nil:
				con.Warn("Tes dihentikan saat setup().")
				return ExitSignal
			}
			con.Error("setup() gagal:\n%s", indent(js.FormatError(err, dir), "  "))
			return ExitScript
		}
		e.sh.SetupJSON, e.sh.HasSetup = data, has
	}

	e.runStart = time.Now()
	e.coll.runStart.Store(e.runStart.UnixNano())
	stopGauge := e.startVUsGauge()
	stopProgress := e.startProgress()
	var wg sync.WaitGroup
	for _, r := range e.runs {
		wg.Add(1)
		go func(r *scenarioRun) {
			defer wg.Done()
			if r.plan.StartTime > 0 {
				t := time.NewTimer(r.plan.StartTime)
				defer t.Stop()
				select {
				case <-t.C:
				case <-e.runCtx.Done():
					return
				}
			}
			r.run(e.runCtx)
		}(r)
	}
	wg.Wait()
	elapsed := time.Since(e.runStart)
	stopGauge()
	stopProgress(elapsed)

	tctx, cancel := context.WithTimeout(context.Background(), e.opts.TeardownTimeout.Get(time.Minute))
	hasTeardown, tdErr := vu0.RunTeardown(tctx)
	tdTimeout := errors.Is(tctx.Err(), context.DeadlineExceeded)
	cancel()
	if tdErr != nil {
		if tdTimeout {
			con.Error("teardown() melebihi batas waktu %s", e.opts.TeardownTimeout.Get(time.Minute))
		} else {
			con.Error("teardown() gagal:\n%s", indent(js.FormatError(tdErr, dir), "  "))
		}
	}
	finishMetrics()

	ok := true
	for _, m := range e.reg.All() {
		for _, mm := range append([]*metrics.Metric{m}, m.Submetrics...) {
			for _, th := range mm.Thresholds {
				if !th.Evaluate(mm, elapsed) {
					ok = false
				}
			}
		}
	}
	data := e.buildSummary(elapsed, ok)
	e.writeResults(vu0, data)

	if e.fatalErr != nil {
		con.Error("%s", e.fatalErr)
		return ExitScript
	}
	switch {
	case !ok:
		return ExitThresholds
	case e.stopCode == ExitSignal || e.stopCode == ExitAborted:
		return e.stopCode
	case tdErr != nil && hasTeardown && tdTimeout:
		return ExitTeardownTimeout
	case tdErr != nil:
		return ExitScript
	}
	return ExitOK
}

// configure membaca options, menyusun skenario, dan mendaftarkan threshold.
func (e *Engine) configure(vu0 *js.VU) int {
	con := e.con
	optsJSON, err := vu0.OptionsJSON()
	if err != nil {
		con.Error("options tidak bisa dibaca: %v", err)
		return ExitConfig
	}
	opts, err := ParseOptions(optsJSON)
	if err != nil {
		con.Error("%v", err)
		return ExitConfig
	}
	e.opts, e.optsJSON = opts, optsJSON
	plans, warns, err := BuildPlan(opts, e.cfg.CLI)
	if err != nil {
		con.Error("Konfigurasi eksekusi tidak valid: %v", err)
		return ExitConfig
	}
	fns := vu0.FuncExports()
	for _, p := range plans {
		if fns[p.Exec] {
			continue
		}
		if p.Exec == "default" {
			con.Error("Script belum punya `export default function () { ... }` — function inilah yang dijalankan setiap iterasi.")
		} else {
			con.Error("Skenario %q memakai exec %q, tapi function itu tidak diekspor script.", p.Name, p.Exec)
		}
		return ExitConfig
	}
	e.plans = plans
	for _, w := range warns {
		con.Warn("%s", w)
	}

	e.sh.Opts = e.jsOptions(opts)
	e.globalTags = opts.Tags

	e.trendStats = []string{"avg", "min", "med", "max", "p(90)", "p(95)"}
	if len(opts.SummaryTrendStats) > 0 {
		e.trendStats = opts.SummaryTrendStats
	}
	if len(e.cfg.TrendStats) > 0 {
		e.trendStats = e.cfg.TrendStats
	}
	for _, s := range e.trendStats {
		if err := metrics.ValidTrendStat(s); err != nil {
			con.Error("%v", err)
			return ExitConfig
		}
	}

	if !e.cfg.NoThresholds {
		seen := map[*metrics.Metric]bool{}
		for _, g := range opts.Thresholds {
			m, err := e.reg.Submetric(g.Metric)
			if err != nil {
				con.Error("Threshold %q: %v. Pastikan nama metric benar; metric custom harus dibuat di init context.", g.Metric, err)
				return ExitConfig
			}
			for _, def := range g.Defs {
				th, err := metrics.ParseThreshold(def.Threshold)
				if err == nil {
					err = th.ValidFor(m)
				}
				if err != nil {
					con.Error("%v", err)
					return ExitConfig
				}
				th.AbortOnFail, th.DelayAbortEval = def.AbortOnFail, def.DelayAbortEval
				m.Thresholds = append(m.Thresholds, th)
				if th.AbortOnFail && !seen[m] {
					seen[m] = true
					e.coll.abortable = append(e.coll.abortable, m)
				}
			}
		}
	}
	e.coll.onAbort = func(name string, th *metrics.Threshold) {
		e.stop(ExitThresholds, fmt.Sprintf("threshold %s '%s' gagal (abortOnFail)", name, th.Source))
	}
	if e.cfg.JSONOut != "" {
		jw, err := newJSONWriter(e.cfg.JSONOut)
		if err != nil {
			con.Error("Tidak bisa membuat file output %s: %v", e.cfg.JSONOut, err)
			return ExitConfig
		}
		e.coll.json = jw
	}
	return ExitOK
}

func (e *Engine) jsOptions(o *Options) js.Options {
	r := js.Options{
		UserAgent:    "serbu/" + e.cfg.Version,
		MaxRedirects: 10,
		Batch:        20,
		BatchPerHost: 6,
		Hosts:        o.Hosts,
		HTTPDebug:    e.cfg.HTTPDebug,
		Tags:         o.Tags,
	}
	if o.UserAgent != nil {
		r.UserAgent = *o.UserAgent
	}
	if e.cfg.UserAgent != nil {
		r.UserAgent = *e.cfg.UserAgent
	}
	if o.MaxRedirects != nil {
		r.MaxRedirects = *o.MaxRedirects
	}
	if o.Batch != nil {
		r.Batch = *o.Batch
	}
	if o.BatchPerHost != nil {
		r.BatchPerHost = *o.BatchPerHost
	}
	b := func(p *bool) bool { return p != nil && *p }
	r.InsecureSkipTLSVerify = b(o.InsecureSkipTLSVerify) || e.cfg.InsecureTLS
	r.NoCookiesReset = b(o.NoCookiesReset)
	r.NoConnectionReuse = b(o.NoConnectionReuse)
	r.NoVUConnectionReuse = b(o.NoVUConnectionReuse)
	r.DiscardResponseBodies = b(o.DiscardResponseBodies)
	return r
}

func (e *Engine) printBanner() {
	st := e.con.Out
	var maxVUs int64
	var maxDur time.Duration
	for _, p := range e.plans {
		maxVUs += p.MaxVUsTotal()
		if d := p.StartTime + p.PlannedDuration() + p.GracefulStop; d > maxDur {
			maxDur = d
		}
	}
	outputs := []string{"ringkasan di terminal"}
	if e.cfg.SummaryExport != "" {
		outputs = append(outputs, "ringkasan JSON → "+e.cfg.SummaryExport)
	}
	if e.cfg.JSONOut != "" {
		outputs = append(outputs, "metrik mentah → "+e.cfg.JSONOut)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\n  %s %s\n\n", st.Bold(st.Cyan("serbu")), st.Gray("v"+e.cfg.Version+" · uji beban ala k6"))
	fmt.Fprintf(&b, "    %s %s\n", st.Gray("script  :"), e.cfg.Script)
	fmt.Fprintf(&b, "    %s %s\n", st.Gray("output  :"), strings.Join(outputs, ", "))
	fmt.Fprintf(&b, "    %s %d skenario, maks. %d VU, durasi maks. %s (termasuk graceful stop)\n", st.Gray("skenario:"), len(e.plans), maxVUs, fmtDur(maxDur))
	for _, p := range e.plans {
		extra := "gracefulStop " + fmtDur(p.GracefulStop)
		if p.StartTime > 0 {
			extra = "mulai " + fmtDur(p.StartTime) + ", " + extra
		}
		if p.Exec != "default" {
			extra = "exec " + p.Exec + ", " + extra
		}
		fmt.Fprintf(&b, "              • %s: %s %s\n", st.Bold(p.Name), p.Describe(), st.Gray("("+extra+")"))
	}
	b.WriteString("\n")
	e.con.Print(b.String())
}

func (e *Engine) handleSignals() {
	ch := make(chan os.Signal, 2)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ch
		e.con.Warn("Menghentikan tes... (tekan Ctrl+C sekali lagi untuk keluar paksa)")
		e.stop(ExitSignal, "dihentikan dengan Ctrl+C")
		<-ch
		e.con.Error("Keluar paksa.")
		os.Exit(ExitSignal)
	}()
}

func (e *Engine) stop(code int, reason string) {
	e.stopMu.Lock()
	if e.stopCode == 0 {
		e.stopCode, e.stopReason = code, reason
	}
	e.stopMu.Unlock()
	if e.cancelRun != nil {
		e.cancelRun(errors.New(reason))
	}
}

func (e *Engine) fatal(err error) {
	e.stopMu.Lock()
	if e.fatalErr == nil {
		e.fatalErr = err
	}
	e.stopMu.Unlock()
	e.stop(ExitScript, "error saat menyiapkan VU tambahan")
}

func (e *Engine) newVU(r *scenarioRun) (*js.VU, error) {
	id := e.nextVUID.Add(1)
	env := e.env
	if len(r.plan.Env) > 0 {
		env = make(map[string]string, len(e.env)+len(r.plan.Env))
		for k, v := range e.env {
			env[k] = v
		}
		for k, v := range r.plan.Env {
			env[k] = v
		}
	}
	vu, err := js.NewVU(e.sh, id, env, r.info)
	if err != nil {
		return nil, err
	}
	e.vusInit.Add(1)
	return vu, nil
}

// initVUs menyiapkan semua VU awal secara paralel.
func (e *Engine) initVUs() error {
	var total int64
	e.runs = make([]*scenarioRun, len(e.plans))
	for i, p := range e.plans {
		info := &js.ScenarioInfo{Name: p.Name, Executor: p.Executor, Exec: p.Exec, Tags: p.Tags}
		r := &scenarioRun{plan: p, e: e, info: info}
		info.Progress = r.progress
		r.vus = make([]*js.VU, p.InitVUs())
		e.runs[i] = r
		total += p.InitVUs()
	}
	type job struct {
		r *scenarioRun
		i int
	}
	jobs := make(chan job)
	var mu sync.Mutex
	var firstErr error
	var done atomic.Int64
	workers := min(max(runtime.NumCPU(), 1), 8)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				vu, err := e.newVU(j.r)
				if err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					mu.Unlock()
					continue
				}
				j.r.vus[j.i] = vu
				done.Add(1)
			}
		}()
	}
	stop := make(chan struct{})
	var pw sync.WaitGroup
	if total >= 50 && e.con.ErrTTY && !e.cfg.Quiet {
		pw.Add(1)
		go func() {
			defer pw.Done()
			t := time.NewTicker(150 * time.Millisecond)
			defer t.Stop()
			for {
				select {
				case <-stop:
					e.con.ClearProgress()
					return
				case <-t.C:
					e.con.DrawProgress([]string{fmt.Sprintf("  menyiapkan VU %d/%d ...", done.Load(), total)})
				}
			}
		}()
	}
feed:
	for _, r := range e.runs {
		for i := range r.vus {
			mu.Lock()
			failed := firstErr != nil
			mu.Unlock()
			if failed || e.runCtx.Err() != nil {
				break feed
			}
			jobs <- job{r, i}
		}
	}
	close(jobs)
	wg.Wait()
	close(stop)
	pw.Wait()
	if firstErr == nil && e.runCtx.Err() != nil {
		return errors.New("dihentikan sebelum tes dimulai")
	}
	return firstErr
}

func (e *Engine) activeVUs() int64 {
	var n int64
	for _, r := range e.runs {
		n += r.active.Load()
	}
	return n
}

// startVUsGauge mencatat metrik vus & vus_max (nilai puncak tiap detik).
func (e *Engine) startVUsGauge() func() {
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		sample := time.NewTicker(50 * time.Millisecond)
		emitT := time.NewTicker(time.Second)
		defer sample.Stop()
		defer emitT.Stop()
		var peak int64
		emitted := false
		emit := func() {
			now := time.Now()
			e.out <- []metrics.Sample{
				{Metric: e.b.VUs, Value: float64(peak), Tags: metrics.Tags{}, Time: now},
				{Metric: e.b.VUsMax, Value: float64(e.vusInit.Load()), Tags: metrics.Tags{}, Time: now},
			}
			emitted = true
			peak = 0
		}
		for {
			select {
			case <-stop:
				if peak > 0 || !emitted {
					emit()
				}
				return
			case <-sample.C:
				if a := e.activeVUs(); a > peak {
					peak = a
				}
			case <-emitT.C:
				emit()
			}
		}
	}()
	return func() { close(stop); <-done }
}

// Instance dipakai modul k6/execution.
func (e *Engine) Instance() js.InstanceInfo {
	el := time.Duration(0)
	if !e.runStart.IsZero() {
		el = time.Since(e.runStart)
	}
	return js.InstanceInfo{
		IterationsCompleted:   e.completed.Load(),
		IterationsInterrupted: e.interrupted.Load(),
		VUsActive:             e.activeVUs(),
		VUsInitialized:        e.vusInit.Load(),
		Elapsed:               el,
	}
}

func (e *Engine) Abort(reason string) { e.stop(ExitAborted, "test.abort(): "+reason) }

func (e *Engine) OptionsJSON() string {
	if e.optsJSON == "" {
		return "{}"
	}
	return e.optsJSON
}

func (e *Engine) TextSummary(dataJSON string, color bool, indent string) (string, error) {
	var d ui.SummaryData
	if err := json.Unmarshal([]byte(dataJSON), &d); err != nil {
		return "", fmt.Errorf("textSummary: data ringkasan tidak valid: %v", err)
	}
	return ui.RenderText(&d, ui.RenderOpts{Style: ui.Style{On: color}, Indent: indent}), nil
}

// scriptError mencetak error iterasi; pesan yang sama hanya ditampilkan beberapa kali.
func (e *Engine) scriptError(vu *js.VU, err error) {
	msg := js.FormatError(err, e.sh.ScriptDir)
	n := e.sh.ScriptErrs.Add(msg)
	st := e.con.Err
	switch {
	case n <= 3:
		e.con.Log(st.Red("✗ ") + st.Gray(fmt.Sprintf("[VU %d] ", vu.ID)) + msg)
	case n == 4:
		e.con.Log(st.Gray("  (error yang sama terus berulang; pesan berikutnya disembunyikan, jumlahnya ada di ringkasan)"))
	}
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

func (e *Engine) buildSummary(elapsed time.Duration, ok bool) *ui.SummaryData {
	d := &ui.SummaryData{
		Tool: "serbu", Version: e.cfg.Version, Script: e.cfg.Script, OK: ok,
		Metrics: map[string]*ui.MetricSummary{}, RootGroup: e.coll.checks.Root,
		HTTPStatus: e.coll.statuses,
	}
	d.State = ui.SummaryState{TestRunDurationMs: ms(elapsed), IsStdOutTTY: e.con.OutTTY, IsStdErrTTY: e.con.ErrTTY}
	if e.stopCode != 0 {
		d.State.Interrupted, d.State.StopReason = true, e.stopReason
	}
	d.Options.SummaryTrendStats = e.trendStats
	add := func(m *metrics.Metric) {
		if !m.Observed && len(m.Thresholds) == 0 {
			return
		}
		s := &ui.MetricSummary{Type: m.Type.String(), Contains: m.Contains.String(), Values: metrics.Values(m, elapsed, e.trendStats)}
		if len(m.Thresholds) > 0 {
			s.Thresholds = map[string]ui.ThresholdResult{}
			for _, th := range m.Thresholds {
				s.Thresholds[th.Source] = ui.ThresholdResult{OK: !th.Failed}
				if _, has := s.Values[th.Stat]; !has {
					s.Values[th.Stat] = th.Value
				}
			}
		}
		d.Metrics[m.Name] = s
	}
	for _, m := range e.reg.All() {
		add(m)
		for _, sub := range m.Submetrics {
			add(sub)
		}
	}
	for _, r := range e.runs {
		d.Scenarios = append(d.Scenarios, ui.ScenarioSummary{Name: r.plan.Name, Executor: r.plan.Executor, Completed: r.completed.Load(), Interrupted: r.interrupted.Load(), Dropped: r.dropped.Load()})
	}
	for _, ec := range e.sh.HTTPErrs.Top(8) {
		codeS, msg, _ := strings.Cut(ec.Key, "|")
		code, _ := strconv.Atoi(codeS)
		d.HTTPErrors = append(d.HTTPErrors, ui.ErrorSummary{Code: code, Message: msg, Count: ec.Count})
	}
	for _, ec := range e.sh.ScriptErrs.Top(5) {
		d.ScriptErrors = append(d.ScriptErrors, ui.ErrorSummary{Message: ec.Key, Count: ec.Count})
	}
	if len(d.HTTPStatus) == 0 {
		d.HTTPStatus = nil
	}
	return d
}

func (e *Engine) writeResults(vu0 *js.VU, data *ui.SummaryData) {
	con := e.con
	dataJSON, _ := json.Marshal(data)
	handled := false
	if outs, has, err := vu0.HandleSummary(string(dataJSON)); has {
		if err != nil {
			con.Error("handleSummary() gagal, ringkasan standar ditampilkan:\n%s", indent(js.FormatError(err, e.sh.ScriptDir), "  "))
		} else {
			handled = true
			for name, content := range outs {
				switch name {
				case "stdout":
					con.Print(content)
				case "stderr":
					con.Log(content)
				default:
					if dir := filepath.Dir(name); dir != "." {
						_ = os.MkdirAll(dir, 0o755)
					}
					if err := os.WriteFile(name, []byte(content), 0o644); err != nil {
						con.Error("handleSummary: gagal menulis %s: %v", name, err)
					} else {
						con.Log(con.Err.Green("✓") + " handleSummary → " + name)
					}
				}
			}
		}
	}
	if !handled && !e.cfg.NoSummary {
		con.Print(ui.RenderText(data, ui.RenderOpts{Style: con.Out}) + "\n")
	}
	if p := e.cfg.SummaryExport; p != "" {
		b, _ := json.MarshalIndent(data, "", "  ")
		if err := os.WriteFile(p, b, 0o644); err != nil {
			con.Error("Gagal menyimpan ringkasan JSON ke %s: %v", p, err)
		} else {
			con.Log(con.Err.Green("✓") + " ringkasan JSON disimpan ke " + p)
		}
	}
	if p := e.cfg.JSONOut; p != "" {
		con.Log(con.Err.Green("✓") + " metrik mentah (NDJSON) disimpan ke " + p)
	}
}
