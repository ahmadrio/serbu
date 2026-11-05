package js

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dop251/goja"

	"serbu/internal/metrics"
	"serbu/internal/ui"
)

var (
	// ErrInterrupted dipakai saat iterasi dihentikan (durasi habis, ramp-down, Ctrl+C).
	ErrInterrupted = errors.New("iterasi dihentikan")
	// ErrAborted dipakai saat script memanggil test.abort().
	ErrAborted = errors.New("tes dihentikan oleh script")
)

// IsInterrupted true bila error berasal dari penghentian iterasi, bukan bug script.
func IsInterrupted(err error) bool {
	var ie *goja.InterruptedError
	return errors.As(err, &ie) || errors.Is(err, ErrInterrupted) || errors.Is(err, ErrAborted)
}

// Options adalah opsi tes yang memengaruhi perilaku HTTP dan VU.
type Options struct {
	UserAgent             string
	InsecureSkipTLSVerify bool
	MaxRedirects          int
	NoCookiesReset        bool
	NoConnectionReuse     bool
	NoVUConnectionReuse   bool
	DiscardResponseBodies bool
	Batch, BatchPerHost   int
	Hosts                 map[string]string
	HTTPDebug             string
	Tags                  map[string]string
}

// InstanceInfo dipakai modul k6/execution.
type InstanceInfo struct {
	IterationsCompleted, IterationsInterrupted int64
	VUsActive, VUsInitialized                  int64
	Elapsed                                    time.Duration
}

// Host adalah layanan dari engine yang dibutuhkan modul JS.
type Host interface {
	Abort(reason string)
	Instance() InstanceInfo
	OptionsJSON() string
	TextSummary(dataJSON string, color bool, indent string) (string, error)
}

// ErrorLog menghitung error yang sama supaya terminal tidak banjir pesan.
type ErrorLog struct {
	mu     sync.Mutex
	counts map[string]int64
	order  []string
}

type ErrorCount struct {
	Key   string
	Count int64
}

func NewErrorLog() *ErrorLog { return &ErrorLog{counts: map[string]int64{}} }

func (l *ErrorLog) Add(key string) int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.counts[key]; !ok {
		l.order = append(l.order, key)
	}
	l.counts[key]++
	return l.counts[key]
}

// Top mengembalikan error terbanyak.
func (l *ErrorLog) Top(n int) []ErrorCount {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]ErrorCount, 0, len(l.order))
	for _, k := range l.order {
		out = append(out, ErrorCount{k, l.counts[k]})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Count > out[j].Count })
	if n > 0 && len(out) > n {
		out = out[:n]
	}
	return out
}

// Shared berisi data yang dipakai bersama oleh semua VU dalam satu tes.
type Shared struct {
	Program   *goja.Program
	ScriptDir string
	Registry  *metrics.Registry
	B         *metrics.Builtin
	Out       chan<- []metrics.Sample
	Console   *ui.Console
	Host      Host
	Opts      Options

	SetupJSON  string
	HasSetup   bool
	ScriptErrs *ErrorLog
	HTTPErrs   *ErrorLog

	files  sync.Map
	shared sync.Map
}

// ScenarioInfo dibagi oleh semua VU dalam satu skenario.
type ScenarioInfo struct {
	Name, Executor, Exec string
	Tags                 map[string]string
	StartNano            atomic.Int64
	Progress             func() float64
}

// VU adalah satu virtual user dengan runtime JavaScript sendiri.
type VU struct {
	ID      int64
	rt      *goja.Runtime
	sh      *Shared
	exports *goja.Object
	modules map[string]goja.Value
	inInit  bool
	env     map[string]string

	ctx        context.Context
	scen       *ScenarioInfo
	iter       int64
	scenIter   int64
	iterInTest int64
	group      string
	baseTags   metrics.Tags

	client    *http.Client
	transport *http.Transport
	jar       *cookiejar.Jar
	sent      atomic.Int64
	recv      atomic.Int64
	respCb    *statusCallback
	respCbSet bool

	setupVal    goja.Value
	setupParsed bool
}

// NewVU membuat runtime baru lalu menjalankan init context script (kode di luar function).
func NewVU(sh *Shared, id int64, env map[string]string, scen *ScenarioInfo) (*VU, error) {
	rt := goja.New()
	rt.SetFieldNameMapper(goja.TagFieldNameMapper("js", true))
	vu := &VU{ID: id, rt: rt, sh: sh, inInit: true, env: env, scen: scen, modules: map[string]goja.Value{}, ctx: context.Background()}
	vu.baseTags = metrics.Tags{}
	for k, v := range sh.Opts.Tags {
		vu.baseTags[k] = v
	}
	if scen != nil {
		for k, v := range scen.Tags {
			vu.baseTags[k] = v
		}
		vu.baseTags["scenario"] = scen.Name
	}

	module := rt.NewObject()
	exports := rt.NewObject()
	_ = module.Set("exports", exports)
	_ = rt.Set("module", module)
	_ = rt.Set("exports", exports)
	_ = rt.Set("require", vu.require)
	_ = rt.Set("__VU", id)
	_ = rt.Set("__ITER", 0)
	envObj := rt.NewObject()
	for k, v := range env {
		_ = envObj.Set(k, v)
	}
	_ = rt.Set("__ENV", envObj)
	_ = rt.Set("open", vu.open)
	console := rt.NewObject()
	for _, lvl := range []string{"log", "info", "warn", "error", "debug"} {
		_ = console.Set(lvl, vu.consoleFn(lvl))
	}
	_ = rt.Set("console", console)

	if _, err := rt.RunProgram(sh.Program); err != nil {
		return nil, fmt.Errorf("error di init context script:\n%s", FormatError(err, sh.ScriptDir))
	}
	exp := module.Get("exports")
	if exp == nil || goja.IsUndefined(exp) || goja.IsNull(exp) {
		return nil, errors.New("script tidak mengekspor apa pun")
	}
	vu.exports = exp.ToObject(rt)
	vu.inInit = false
	return vu, nil
}

// Exports mengembalikan nama ekspor yang berupa function.
func (vu *VU) FuncExports() map[string]bool {
	out := map[string]bool{}
	for _, k := range vu.exports.Keys() {
		if _, ok := goja.AssertFunction(vu.exports.Get(k)); ok {
			out[k] = true
		}
	}
	return out
}

// OptionsJSON mengembalikan `export const options` sebagai JSON.
func (vu *VU) OptionsJSON() (string, error) {
	v := vu.exports.Get("options")
	if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
		return "{}", nil
	}
	return vu.stringify(v)
}

func (vu *VU) stringify(v goja.Value) (string, error) {
	stringify, _ := goja.AssertFunction(vu.rt.Get("JSON").ToObject(vu.rt).Get("stringify"))
	res, err := stringify(goja.Undefined(), v)
	if err != nil {
		return "", err
	}
	if goja.IsUndefined(res) {
		return "", nil
	}
	return res.String(), nil
}

func (vu *VU) parseJSON(s string) (goja.Value, error) {
	parse, _ := goja.AssertFunction(vu.rt.Get("JSON").ToObject(vu.rt).Get("parse"))
	return parse(goja.Undefined(), vu.rt.ToValue(s))
}

func (vu *VU) fn(name string) (goja.Callable, bool) {
	return goja.AssertFunction(vu.exports.Get(name))
}

// call menjalankan function JS; bila ctx selesai, eksekusi JS diinterupsi.
func (vu *VU) call(ctx context.Context, f goja.Callable, args ...goja.Value) (goja.Value, error) {
	done := make(chan struct{})
	watcher := make(chan struct{})
	go func() {
		defer close(watcher)
		select {
		case <-ctx.Done():
			vu.rt.Interrupt(ErrInterrupted)
		case <-done:
		}
	}()
	ret, err := f(goja.Undefined(), args...)
	close(done)
	<-watcher
	vu.rt.ClearInterrupt()
	if err == nil && ret != nil {
		if p, ok := ret.Export().(*goja.Promise); ok {
			switch p.State() {
			case goja.PromiseStateRejected:
				err = vu.promiseError(p.Result())
			case goja.PromiseStatePending:
				err = errors.New("function async belum selesai (promise masih pending)")
			}
		}
	}
	return ret, err
}

type jsError struct{ msg string }

func (e *jsError) Error() string { return e.msg }

func (vu *VU) promiseError(v goja.Value) error {
	if obj, ok := v.(*goja.Object); ok {
		if st := obj.Get("stack"); st != nil && !goja.IsUndefined(st) {
			return &jsError{msg: st.String()}
		}
	}
	return &jsError{msg: "Uncaught (in promise) " + v.String()}
}

// RunIteration menjalankan satu iterasi function `fnName`.
func (vu *VU) RunIteration(ctx context.Context, fnName string, iterInTest int64) error {
	f, ok := vu.fn(fnName)
	if !ok {
		return fmt.Errorf("function %q tidak diekspor oleh script", fnName)
	}
	vu.ctx = ctx
	vu.iterInTest = iterInTest
	vu.group = ""
	_ = vu.rt.Set("__ITER", vu.iter)
	defer func() { vu.iter++; vu.scenIter++ }()
	if !vu.sh.Opts.NoCookiesReset && vu.jar != nil {
		vu.jar, _ = cookiejar.New(nil)
		vu.client.Jar = vu.jar
	}
	if vu.sh.Opts.NoVUConnectionReuse && vu.transport != nil {
		vu.transport.CloseIdleConnections()
	}
	_, err := vu.call(ctx, f, vu.setupData())
	return err
}

func (vu *VU) setupData() goja.Value {
	if !vu.setupParsed {
		vu.setupParsed = true
		vu.setupVal = goja.Undefined()
		if vu.sh.SetupJSON != "" {
			if v, err := vu.parseJSON(vu.sh.SetupJSON); err == nil {
				vu.setupVal = v
			}
		}
	}
	return vu.setupVal
}

// RunSetup menjalankan export function setup(); hasilnya diserialisasi ke JSON.
func (vu *VU) RunSetup(ctx context.Context) (string, bool, error) {
	f, ok := vu.fn("setup")
	if !ok {
		return "", false, nil
	}
	vu.ctx, vu.group = ctx, "::setup"
	ret, err := vu.call(ctx, f)
	if err != nil {
		return "", true, err
	}
	if ret == nil || goja.IsUndefined(ret) {
		return "", true, nil
	}
	s, err := vu.stringify(ret)
	if err != nil {
		return "", true, fmt.Errorf("hasil setup() harus bisa diubah ke JSON: %w", err)
	}
	return s, true, nil
}

// RunTeardown menjalankan export function teardown(data).
func (vu *VU) RunTeardown(ctx context.Context) (bool, error) {
	f, ok := vu.fn("teardown")
	if !ok {
		return false, nil
	}
	vu.ctx, vu.group = ctx, "::teardown"
	vu.setupParsed = false
	_, err := vu.call(ctx, f, vu.setupData())
	return true, err
}

// HandleSummary memanggil export function handleSummary(data) bila ada.
func (vu *VU) HandleSummary(dataJSON string) (map[string]string, bool, error) {
	f, ok := vu.fn("handleSummary")
	if !ok {
		return nil, false, nil
	}
	data, err := vu.parseJSON(dataJSON)
	if err != nil {
		return nil, true, err
	}
	vu.ctx, vu.group = context.Background(), ""
	ret, err := vu.call(context.Background(), f, data)
	if err != nil {
		return nil, true, err
	}
	out := map[string]string{}
	if ret == nil || goja.IsUndefined(ret) || goja.IsNull(ret) {
		return out, true, nil
	}
	obj := ret.ToObject(vu.rt)
	for _, k := range obj.Keys() {
		v := obj.Get(k)
		if ab, ok := v.Export().(goja.ArrayBuffer); ok {
			out[k] = string(ab.Bytes())
			continue
		}
		out[k] = v.String()
	}
	return out, true, nil
}

// throw melempar Error JavaScript biasa (bisa ditangkap try/catch di script).
func (vu *VU) throw(format string, args ...any) {
	obj, err := vu.rt.New(vu.rt.Get("Error"), vu.rt.ToValue(fmt.Sprintf(format, args...)))
	if err != nil {
		panic(vu.rt.NewGoError(fmt.Errorf(format, args...)))
	}
	panic(obj)
}

func (vu *VU) emit(s ...metrics.Sample) {
	if vu.sh.Out != nil {
		vu.sh.Out <- s
	}
}

// tags menggabungkan tag dasar VU, group saat ini, dan tag tambahan.
func (vu *VU) tags(extra ...map[string]string) metrics.Tags {
	t := make(metrics.Tags, len(vu.baseTags)+6)
	for k, v := range vu.baseTags {
		t[k] = v
	}
	t["group"] = vu.group
	for _, e := range extra {
		for k, v := range e {
			t[k] = v
		}
	}
	return t
}

// exportTags mengubah objek JS {a: 'b'} menjadi map string.
func (vu *VU) exportTags(v goja.Value) map[string]string {
	if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
		return nil
	}
	obj := v.ToObject(vu.rt)
	out := map[string]string{}
	for _, k := range obj.Keys() {
		out[k] = obj.Get(k).String()
	}
	return out
}

func (vu *VU) open(call goja.FunctionCall) goja.Value {
	if !vu.inInit {
		vu.throw("open() hanya bisa dipakai di init context (di luar function)")
	}
	name := call.Argument(0).String()
	p := name
	if !filepath.IsAbs(p) {
		p = filepath.Join(vu.sh.ScriptDir, p)
	}
	var data []byte
	if c, ok := vu.sh.files.Load(p); ok {
		data = c.([]byte)
	} else {
		b, err := os.ReadFile(p)
		if err != nil {
			vu.throw("open(%q): file tidak bisa dibaca: %v", name, err)
		}
		vu.sh.files.Store(p, b)
		data = b
	}
	if mode := call.Argument(1); !goja.IsUndefined(mode) && mode.String() == "b" {
		return vu.rt.ToValue(vu.rt.NewArrayBuffer(append([]byte(nil), data...)))
	}
	return vu.rt.ToValue(string(data))
}

func (vu *VU) consoleFn(level string) func(goja.FunctionCall) goja.Value {
	return func(call goja.FunctionCall) goja.Value {
		parts := make([]string, len(call.Arguments))
		for i, a := range call.Arguments {
			parts[i] = vu.formatValue(a)
		}
		msg := strings.Join(parts, " ")
		st := vu.sh.Console.Err
		prefix := st.Gray(fmt.Sprintf("[VU %d]", vu.ID))
		if vu.ID == 0 {
			prefix = st.Gray("[init]")
			if vu.group == "::setup" {
				prefix = st.Gray("[setup]")
			} else if vu.group == "::teardown" {
				prefix = st.Gray("[teardown]")
			}
		}
		switch level {
		case "warn":
			prefix += " " + st.Yellow("WARN")
		case "error":
			prefix += " " + st.Red("ERROR")
		case "debug":
			prefix += " " + st.Gray("DEBUG")
		}
		vu.sh.Console.Log(prefix + " " + msg)
		return goja.Undefined()
	}
}

func (vu *VU) formatValue(v goja.Value) string {
	if v == nil || goja.IsUndefined(v) {
		return "undefined"
	}
	if goja.IsNull(v) {
		return "null"
	}
	if obj, ok := v.(*goja.Object); ok {
		if _, isFn := goja.AssertFunction(v); isFn {
			return "[Function]"
		}
		if obj.ClassName() == "Error" {
			return v.String()
		}
		if s, err := vu.stringify(v); err == nil && s != "" {
			return s
		}
	}
	return v.String()
}

func (vu *VU) require(call goja.FunctionCall) goja.Value {
	name := call.Argument(0).String()
	if m, ok := vu.modules[name]; ok {
		return m
	}
	var m goja.Value
	switch name {
	case "k6", "serbu":
		m = vu.modCore()
	case "k6/http", "serbu/http":
		m = vu.modHTTP()
	case "k6/metrics", "serbu/metrics":
		m = vu.modMetrics()
	case "k6/execution", "serbu/execution":
		m = vu.modExecution()
	case "k6/encoding", "serbu/encoding":
		m = vu.modEncoding()
	case "k6/crypto", "serbu/crypto":
		m = vu.modCrypto()
	case "k6/data", "serbu/data":
		m = vu.modData()
	case "serbu/utils":
		m = vu.modUtils()
	case "serbu/summary":
		m = vu.modSummary()
	case "k6/options":
		m = vu.rt.NewObject()
	default:
		switch {
		case strings.HasPrefix(name, "https://jslib.k6.io/k6-utils/"):
			m = vu.modUtils()
		case strings.HasPrefix(name, "https://jslib.k6.io/k6-summary/"):
			m = vu.modSummary()
		case strings.HasPrefix(name, "http://") || strings.HasPrefix(name, "https://"):
			vu.throw("import dari URL (%s) tidak didukung. Unduh file-nya, simpan di samping script, lalu import secara lokal", name)
		case strings.HasPrefix(name, "k6/"):
			vu.throw("modul %q belum didukung serbu. Modul yang tersedia: k6, k6/http, k6/metrics, k6/execution, k6/encoding, k6/crypto, k6/data", name)
		default:
			vu.throw("modul %q tidak ditemukan", name)
		}
	}
	vu.modules[name] = m
	return m
}

// FormatError merapikan error JS: pesan + lokasi baris di file aslinya (bukan file bundel).
func FormatError(err error, scriptDir string) string {
	var ex *goja.Exception
	if errors.As(err, &ex) {
		msg := "Error"
		if v := ex.Value(); v != nil {
			msg = v.String()
		}
		var lines []string
		for _, fr := range ex.Stack() {
			pos := fr.Position()
			if pos.Filename == "" || pos.Line == 0 {
				continue
			}
			file := pos.Filename
			file = strings.TrimPrefix(file, "file://")
			if rel, rerr := filepath.Rel(scriptDir, file); rerr == nil && !strings.HasPrefix(rel, "..") {
				file = rel
			}
			fn := fr.FuncName()
			switch {
			case fn == "":
				fn = "<anonim>"
			case strings.HasSuffix(fn, "_default"):
				// nama buatan esbuild untuk `export default function () {}`
				fn = "default"
			}
			lines = append(lines, fmt.Sprintf("    di %s (%s:%d:%d)", fn, file, pos.Line, pos.Column))
			if len(lines) == 4 {
				break
			}
		}
		if len(lines) == 0 {
			return msg
		}
		return msg + "\n" + strings.Join(lines, "\n")
	}
	var se *goja.CompilerSyntaxError
	if errors.As(err, &se) {
		return "SyntaxError: " + se.Error()
	}
	return err.Error()
}
