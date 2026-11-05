package js

import (
	"math"
	"math/rand"
	"strings"
	"time"

	"github.com/dop251/goja"

	"serbu/internal/metrics"
)

// modCore = modul "k6": check, sleep, group, fail, randomSeed.
func (vu *VU) modCore() goja.Value {
	o := vu.rt.NewObject()
	_ = o.Set("check", vu.check)
	_ = o.Set("sleep", vu.sleep)
	_ = o.Set("group", vu.groupFn)
	_ = o.Set("fail", func(call goja.FunctionCall) goja.Value {
		msg := call.Argument(0)
		text := "fail() dipanggil"
		if !goja.IsUndefined(msg) {
			text = msg.String()
		}
		errCtor := vu.rt.Get("Error")
		obj, _ := vu.rt.New(errCtor, vu.rt.ToValue(text))
		panic(obj)
	})
	_ = o.Set("randomSeed", func(call goja.FunctionCall) goja.Value {
		rand.Seed(call.Argument(0).ToInteger()) //nolint:staticcheck
		return goja.Undefined()
	})
	return o
}

func (vu *VU) check(call goja.FunctionCall) goja.Value {
	if vu.inInit {
		vu.throw("check() tidak bisa dipakai di init context; panggil di dalam function")
	}
	val := call.Argument(0)
	sets := call.Argument(1)
	if goja.IsUndefined(sets) || goja.IsNull(sets) {
		return vu.rt.ToValue(true)
	}
	extra := vu.exportTags(call.Argument(2))
	obj := sets.ToObject(vu.rt)
	all := true
	now := time.Now()
	for _, name := range obj.Keys() {
		v := obj.Get(name)
		ok := false
		if fn, isFn := goja.AssertFunction(v); isFn {
			res, err := fn(goja.Undefined(), val)
			if err != nil {
				panic(err)
			}
			ok = res.ToBoolean()
		} else {
			ok = v.ToBoolean()
		}
		val01 := 0.0
		if ok {
			val01 = 1
		} else {
			all = false
		}
		vu.emit(metrics.Sample{Metric: vu.sh.B.Checks, Value: val01, Tags: vu.tags(extra, map[string]string{"check": name}), Time: now})
	}
	return vu.rt.ToValue(all)
}

func (vu *VU) sleep(call goja.FunctionCall) goja.Value {
	secs := call.Argument(0).ToFloat()
	if secs <= 0 || math.IsNaN(secs) {
		return goja.Undefined()
	}
	t := time.NewTimer(time.Duration(secs * float64(time.Second)))
	defer t.Stop()
	select {
	case <-t.C:
	case <-vu.ctx.Done():
	}
	return goja.Undefined()
}

func (vu *VU) groupFn(call goja.FunctionCall) goja.Value {
	if vu.inInit {
		vu.throw("group() tidak bisa dipakai di init context")
	}
	name := call.Argument(0).String()
	fn, ok := goja.AssertFunction(call.Argument(1))
	if !ok {
		vu.throw("group(%q, fn): argumen kedua harus function", name)
	}
	if strings.Contains(name, "::") {
		vu.throw("nama group tidak boleh mengandung '::'")
	}
	prev := vu.group
	vu.group = prev + "::" + name
	start := time.Now()
	ret, err := fn(goja.Undefined())
	path := vu.group
	vu.group = prev
	if err != nil {
		panic(err)
	}
	if vu.ctx.Err() == nil {
		el := time.Since(start)
		tags := vu.tags()
		tags["group"] = path
		vu.emit(metrics.Sample{Metric: vu.sh.B.GroupDuration, Value: ms(el), Tags: tags, Time: time.Now()})
	}
	return ret
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

// modMetrics = modul "k6/metrics": Counter, Gauge, Rate, Trend.
func (vu *VU) modMetrics() goja.Value {
	o := vu.rt.NewObject()
	_ = o.Set("Counter", vu.metricCtor(metrics.Counter))
	_ = o.Set("Gauge", vu.metricCtor(metrics.Gauge))
	_ = o.Set("Rate", vu.metricCtor(metrics.Rate))
	_ = o.Set("Trend", vu.metricCtor(metrics.Trend))
	return o
}

func (vu *VU) metricCtor(t metrics.MetricType) func(goja.ConstructorCall) *goja.Object {
	return func(call goja.ConstructorCall) *goja.Object {
		if !vu.inInit {
			vu.throw("metric custom harus dibuat di init context (di luar function)")
		}
		name := call.Argument(0).String()
		vt := metrics.Default
		if t == metrics.Trend && call.Argument(1).ToBoolean() {
			vt = metrics.Time
		}
		m, err := vu.sh.Registry.New(name, t, vt)
		if err != nil {
			vu.throw("%v", err)
		}
		this := call.This
		_ = this.Set("name", name)
		_ = this.Set("add", func(c goja.FunctionCall) goja.Value {
			if vu.inInit {
				vu.throw("%s.add() hanya bisa dipanggil di dalam function", name)
			}
			arg := c.Argument(0)
			var v float64
			if t == metrics.Rate {
				if arg.ToBoolean() {
					v = 1
				}
			} else {
				v = arg.ToFloat()
				if math.IsNaN(v) {
					vu.throw("%s.add(): nilai harus angka, bukan %s", name, arg.String())
				}
			}
			vu.emit(metrics.Sample{Metric: m, Value: v, Tags: vu.tags(vu.exportTags(c.Argument(1))), Time: time.Now()})
			return vu.rt.ToValue(true)
		})
		return nil
	}
}

// modExecution = modul "k6/execution".
func (vu *VU) modExecution() goja.Value {
	rt := vu.rt
	getters := func(obj *goja.Object, props map[string]func() any) {
		for name, fn := range props {
			fn := fn
			_ = obj.DefineAccessorProperty(name, rt.ToValue(func(goja.FunctionCall) goja.Value { return rt.ToValue(fn()) }), nil, goja.FLAG_FALSE, goja.FLAG_TRUE)
		}
	}
	vuObj := rt.NewObject()
	getters(vuObj, map[string]func() any{
		"idInTest":            func() any { return vu.ID },
		"idInInstance":        func() any { return vu.ID },
		"iterationInInstance": func() any { return vu.iter },
		"iterationInScenario": func() any { return vu.scenIter },
	})
	scen := rt.NewObject()
	getters(scen, map[string]func() any{
		"name":     func() any { return vu.scenName() },
		"executor": func() any { return vu.scenField(func(s *ScenarioInfo) string { return s.Executor }) },
		"exec":     func() any { return vu.scenField(func(s *ScenarioInfo) string { return s.Exec }) },
		"startTime": func() any {
			if vu.scen == nil {
				return 0
			}
			return vu.scen.StartNano.Load() / int64(time.Millisecond)
		},
		"progress": func() any {
			if vu.scen == nil || vu.scen.Progress == nil {
				return 0
			}
			return vu.scen.Progress()
		},
		"iterationInInstance": func() any { return vu.iterInTest },
		"iterationInTest":     func() any { return vu.iterInTest },
	})
	inst := rt.NewObject()
	getters(inst, map[string]func() any{
		"iterationsCompleted":    func() any { return vu.sh.Host.Instance().IterationsCompleted },
		"iterationsInterrupted":  func() any { return vu.sh.Host.Instance().IterationsInterrupted },
		"vusActive":              func() any { return vu.sh.Host.Instance().VUsActive },
		"vusInitialized":         func() any { return vu.sh.Host.Instance().VUsInitialized },
		"currentTestRunDuration": func() any { return ms(vu.sh.Host.Instance().Elapsed) },
	})
	test := rt.NewObject()
	_ = test.Set("abort", func(call goja.FunctionCall) goja.Value {
		reason := "test.abort() dipanggil"
		if a := call.Argument(0); !goja.IsUndefined(a) {
			reason = a.String()
		}
		vu.sh.Host.Abort(reason)
		vu.rt.Interrupt(ErrAborted)
		return goja.Undefined()
	})
	_ = test.DefineAccessorProperty("options", rt.ToValue(func(goja.FunctionCall) goja.Value {
		v, err := vu.parseJSON(vu.sh.Host.OptionsJSON())
		if err != nil {
			return goja.Undefined()
		}
		return v
	}), nil, goja.FLAG_FALSE, goja.FLAG_TRUE)

	o := rt.NewObject()
	_ = o.Set("vu", vuObj)
	_ = o.Set("scenario", scen)
	_ = o.Set("instance", inst)
	_ = o.Set("test", test)
	return o
}

func (vu *VU) scenName() string {
	if vu.scen == nil {
		return ""
	}
	return vu.scen.Name
}

func (vu *VU) scenField(f func(*ScenarioInfo) string) string {
	if vu.scen == nil {
		return ""
	}
	return f(vu.scen)
}
