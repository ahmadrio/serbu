package ui

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"serbu/internal/metrics"
)

// SummaryData adalah hasil akhir tes. Formatnya mengikuti data handleSummary k6,
// ditambah info serbu (status HTTP, daftar error).
type SummaryData struct {
	Tool         string                    `json:"tool"`
	Version      string                    `json:"version"`
	Script       string                    `json:"script"`
	OK           bool                      `json:"ok"`
	State        SummaryState              `json:"state"`
	Options      SummaryOptions            `json:"options"`
	Metrics      map[string]*MetricSummary `json:"metrics"`
	RootGroup    *GroupSummary             `json:"root_group"`
	Scenarios    []ScenarioSummary         `json:"scenarios,omitempty"`
	HTTPStatus   map[string]int64          `json:"http_statuses,omitempty"`
	HTTPErrors   []ErrorSummary            `json:"http_errors,omitempty"`
	ScriptErrors []ErrorSummary            `json:"script_errors,omitempty"`
}

type SummaryState struct {
	TestRunDurationMs float64 `json:"testRunDurationMs"`
	IsStdOutTTY       bool    `json:"isStdOutTTY"`
	IsStdErrTTY       bool    `json:"isStdErrTTY"`
	Interrupted       bool    `json:"interrupted,omitempty"`
	StopReason        string  `json:"stopReason,omitempty"`
}

type SummaryOptions struct {
	SummaryTrendStats []string `json:"summaryTrendStats"`
}

type MetricSummary struct {
	Type       string                     `json:"type"`
	Contains   string                     `json:"contains"`
	Values     map[string]float64         `json:"values"`
	Thresholds map[string]ThresholdResult `json:"thresholds,omitempty"`
}

type ThresholdResult struct {
	OK bool `json:"ok"`
}

type GroupSummary struct {
	Name   string          `json:"name"`
	Path   string          `json:"path"`
	ID     string          `json:"id"`
	Groups []*GroupSummary `json:"groups"`
	Checks []*CheckSummary `json:"checks"`
}

type CheckSummary struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	ID     string `json:"id"`
	Passes int64  `json:"passes"`
	Fails  int64  `json:"fails"`
}

type ScenarioSummary struct {
	Name        string `json:"name"`
	Executor    string `json:"executor"`
	Completed   int64  `json:"iterationsCompleted"`
	Interrupted int64  `json:"iterationsInterrupted"`
	Dropped     int64  `json:"iterationsDropped"`
}

type ErrorSummary struct {
	Code    int    `json:"code,omitempty"`
	Message string `json:"message"`
	Count   int64  `json:"count"`
}

func md5hex(s string) string {
	h := md5.Sum([]byte(s))
	return hex.EncodeToString(h[:])
}

// CheckTree menyusun hasil check per group, sesuai urutan pertama kali muncul.
type CheckTree struct {
	Root   *GroupSummary
	groups map[string]*GroupSummary
	checks map[string]*CheckSummary
}

func NewCheckTree() *CheckTree {
	root := &GroupSummary{ID: md5hex(""), Groups: []*GroupSummary{}, Checks: []*CheckSummary{}}
	return &CheckTree{Root: root, groups: map[string]*GroupSummary{"": root}, checks: map[string]*CheckSummary{}}
}

func (t *CheckTree) group(path string) *GroupSummary {
	if g, ok := t.groups[path]; ok {
		return g
	}
	i := strings.LastIndex(path, "::")
	if i < 0 {
		return t.Root
	}
	parent := t.group(path[:i])
	g := &GroupSummary{Name: path[i+2:], Path: path, ID: md5hex(path), Groups: []*GroupSummary{}, Checks: []*CheckSummary{}}
	parent.Groups = append(parent.Groups, g)
	t.groups[path] = g
	return g
}

func (t *CheckTree) Add(groupPath, name string, pass bool) {
	key := groupPath + "\x00" + name
	c, ok := t.checks[key]
	if !ok {
		g := t.group(groupPath)
		c = &CheckSummary{Name: name, Path: groupPath + "::" + name, ID: md5hex(groupPath + "::" + name)}
		g.Checks = append(g.Checks, c)
		t.checks[key] = c
	}
	if pass {
		c.Passes++
	} else {
		c.Fails++
	}
}

// RenderOpts mengatur tampilan ringkasan.
type RenderOpts struct {
	Style  Style
	Indent string
}

type line struct{ name, value string }

func section(name string) string {
	switch {
	case name == "checks":
		return ""
	case strings.HasPrefix(name, "http_"):
		return "HTTP"
	case name == "data_sent" || name == "data_received":
		return "JARINGAN"
	case metrics.IsBuiltin(name):
		return "EKSEKUSI"
	}
	return "CUSTOM (dari script)"
}

func baseName(n string) string {
	if i := strings.IndexByte(n, '{'); i >= 0 {
		return n[:i]
	}
	return n
}

func (d *SummaryData) trendStats() []string {
	if len(d.Options.SummaryTrendStats) > 0 {
		return d.Options.SummaryTrendStats
	}
	return []string{"avg", "min", "med", "max", "p(90)", "p(95)"}
}

func fmtMetric(m *MetricSummary, stats []string) string {
	v := m.Values
	switch m.Type {
	case "trend":
		parts := make([]string, 0, len(stats))
		for _, s := range stats {
			if s == "count" {
				parts = append(parts, s+"="+Num(v[s]))
				continue
			}
			parts = append(parts, s+"="+Value(v[s], m.Contains))
		}
		return strings.Join(parts, " ")
	case "counter":
		if m.Contains == "data" {
			return fmt.Sprintf("%-9s %s/s", Bytes(v["count"]), Bytes(v["rate"]))
		}
		return fmt.Sprintf("%-9s %s", Num(v["count"]), PerSec(v["rate"]))
	case "rate":
		total := v["passes"] + v["fails"]
		return fmt.Sprintf("%-9s %s dari %s", Percent(v["rate"]), Num(v["passes"]), Num(total))
	case "gauge":
		return fmt.Sprintf("%-9s min=%s max=%s", Value(v["value"], m.Contains), Value(v["min"], m.Contains), Value(v["max"], m.Contains))
	}
	return ""
}

func thresholdStatValue(m *MetricSummary, src string) string {
	th, err := metrics.ParseThreshold(src)
	if err != nil {
		return ""
	}
	val, ok := m.Values[th.Stat]
	if !ok {
		return ""
	}
	switch {
	case m.Type == "rate" || (m.Type == "counter" && th.Stat == "rate" && m.Contains != "data"):
		if m.Type == "rate" {
			return th.Stat + "=" + Percent(val)
		}
		return th.Stat + "=" + PerSec(val)
	}
	return th.Stat + "=" + Value(val, m.Contains)
}

func sortedMetricNames(ms map[string]*MetricSummary) []string {
	names := make([]string, 0, len(ms))
	for n := range ms {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool {
		bi, bj := baseName(names[i]), baseName(names[j])
		if bi != bj {
			return bi < bj
		}
		return len(names[i]) < len(names[j]) || (len(names[i]) == len(names[j]) && names[i] < names[j])
	})
	return names
}

// RenderText membuat ringkasan teks bergaya k6 dalam bahasa Indonesia.
func RenderText(d *SummaryData, o RenderOpts) string {
	st := o.Style
	ind := o.Indent
	if ind == "" {
		ind = "  "
	}
	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format, args...) }
	names := sortedMetricNames(d.Metrics)
	stats := d.trendStats()

	// THRESHOLD
	total, failed := 0, 0
	var thrBlock strings.Builder
	for _, n := range names {
		m := d.Metrics[n]
		if len(m.Thresholds) == 0 {
			continue
		}
		srcs := make([]string, 0, len(m.Thresholds))
		for s := range m.Thresholds {
			srcs = append(srcs, s)
		}
		sort.Strings(srcs)
		fmt.Fprintf(&thrBlock, "%s%s%s\n", ind, ind, st.Bold(n))
		for _, s := range srcs {
			total++
			mark := st.Green("✓")
			if !m.Thresholds[s].OK {
				mark = st.Red("✗")
				failed++
			}
			fmt.Fprintf(&thrBlock, "%s%s%s '%s' %s\n", ind, ind, mark, s, st.Gray(thresholdStatValue(m, s)))
		}
		thrBlock.WriteString("\n")
	}
	w("\n")
	if total > 0 {
		w("%s%s\n\n%s", ind, st.Bold("█ THRESHOLD"), thrBlock.String())
		w("\n")
	}

	// HASIL TOTAL
	w("%s%s\n\n", ind, st.Bold("█ HASIL TOTAL"))
	in2 := ind + ind
	if d.RootGroup != nil && (len(d.RootGroup.Checks) > 0 || len(d.RootGroup.Groups) > 0) {
		w("%s%s\n", in2, st.Gray("CHECK"))
		renderGroup(&b, d.RootGroup, in2, st)
		w("\n")
	}
	var checkLines []line
	if c, ok := d.Metrics["checks"]; ok {
		pass, fail := c.Values["passes"], c.Values["fails"]
		all := pass + fail
		rate := 0.0
		if d.State.TestRunDurationMs > 0 {
			rate = all / (d.State.TestRunDurationMs / 1000)
		}
		failRate := 0.0
		if all > 0 {
			failRate = fail / all
		}
		checkLines = []line{
			{"checks_total", fmt.Sprintf("%-9s %s", Num(all), PerSec(rate))},
			{"checks_succeeded", fmt.Sprintf("%-9s %s dari %s", Percent(c.Values["rate"]), Num(pass), Num(all))},
			{"checks_failed", fmt.Sprintf("%-9s %s dari %s", Percent(failRate), Num(fail), Num(all))},
		}
	}
	bySection := map[string][]line{}
	var order []string
	for _, n := range names {
		sec := section(baseName(n))
		if sec == "" {
			continue
		}
		label := n
		if i := strings.IndexByte(n, '{'); i >= 0 {
			label = "  " + n[i:]
		}
		if _, ok := bySection[sec]; !ok {
			order = append(order, sec)
		}
		bySection[sec] = append(bySection[sec], line{label, fmtMetric(d.Metrics[n], stats)})
	}
	pad := padWidth(checkLines)
	for _, ls := range bySection {
		if p := padWidth(ls); p > pad {
			pad = p
		}
	}
	if len(checkLines) > 0 {
		writeLines(&b, checkLines, in2, pad, st)
		w("\n")
	}
	sort.SliceStable(order, func(i, j int) bool { return sectionRank(order[i]) < sectionRank(order[j]) })
	for _, sec := range order {
		w("%s%s\n", in2, st.Gray(sec))
		writeLines(&b, bySection[sec], in2, pad, st)
		w("\n")
	}
	for _, sc := range d.Scenarios {
		if len(d.Scenarios) == 1 && sc.Interrupted == 0 && sc.Dropped == 0 {
			break
		}
		extra := ""
		if sc.Dropped > 0 {
			extra = fmt.Sprintf(", %s tidak sempat jalan", st.Yellow(Num(float64(sc.Dropped))))
		}
		w("%s%s %s: %d selesai, %d terputus%s\n", in2, st.Gray("skenario"), sc.Name, sc.Completed, sc.Interrupted, extra)
	}

	// RESPONS
	if len(d.HTTPStatus) > 0 {
		w("\n%s%s\n\n", ind, st.Bold("█ RESPONS HTTP"))
		var reqs int64
		codes := make([]string, 0, len(d.HTTPStatus))
		for c, n := range d.HTTPStatus {
			codes = append(codes, c)
			reqs += n
		}
		sort.Slice(codes, func(i, j int) bool { a, _ := strconv.Atoi(codes[i]); c, _ := strconv.Atoi(codes[j]); return a < c })
		var ls []line
		for _, c := range codes {
			code, _ := strconv.Atoi(c)
			label := c + " " + http.StatusText(code)
			val := fmt.Sprintf("%-9s (%s)", Num(float64(d.HTTPStatus[c])), Percent(float64(d.HTTPStatus[c])/float64(reqs)))
			switch {
			case code == 0:
				label = "tanpa respons"
				val = st.Red(val)
			case code >= 500:
				val = st.Red(val)
			case code >= 400:
				val = st.Yellow(val)
			}
			ls = append(ls, line{strings.TrimSpace(label), val})
		}
		writeLines(&b, ls, in2, padWidth(ls), st)
		if len(d.HTTPErrors) > 0 {
			w("\n%s%s\n", in2, st.Gray("ERROR JARINGAN (status 0)"))
			for _, e := range d.HTTPErrors {
				w("%s%s ×%d  %s\n", in2, st.Red(fmt.Sprintf("[%d]", e.Code)), e.Count, e.Message)
			}
		}
	}
	if len(d.ScriptErrors) > 0 {
		w("\n%s%s\n\n", ind, st.Bold("█ ERROR SCRIPT"))
		for _, e := range d.ScriptErrors {
			msg := strings.SplitN(e.Message, "\n", 2)[0]
			w("%s%s %s\n", in2, st.Red(fmt.Sprintf("×%d", e.Count)), msg)
		}
	}

	// CATATAN
	if notes := hints(d, total); len(notes) > 0 {
		w("\n%s%s\n\n", ind, st.Bold("█ CATATAN"))
		for _, n := range notes {
			w("%s• %s\n", in2, n)
		}
	}

	// KESIMPULAN
	w("\n")
	if d.State.Interrupted {
		reason := d.State.StopReason
		if reason == "" {
			reason = "dihentikan"
		}
		w("%s%s\n", ind, st.Yellow("⚠ Tes berhenti lebih awal: "+reason))
	}
	switch {
	case total == 0:
		w("%s%s\n", ind, st.Gray("ℹ Tidak ada threshold, jadi tidak ada kriteria lulus/gagal."))
	case failed == 0:
		w("%s%s\n", ind, st.Green(st.Bold(fmt.Sprintf("✓ LULUS — semua %d threshold terpenuhi", total))))
	default:
		w("%s%s\n", ind, st.Red(st.Bold(fmt.Sprintf("✗ GAGAL — %d dari %d threshold tidak terpenuhi", failed, total))))
	}
	return b.String()
}

func sectionRank(s string) int {
	switch s {
	case "HTTP":
		return 0
	case "EKSEKUSI":
		return 1
	case "JARINGAN":
		return 2
	}
	return 3
}

func padWidth(ls []line) int {
	p := 0
	for _, l := range ls {
		if n := len([]rune(l.name)); n > p {
			p = n
		}
	}
	return p + 3
}

func writeLines(b *strings.Builder, ls []line, ind string, pad int, st Style) {
	for _, l := range ls {
		n := len([]rune(l.name))
		dots := strings.Repeat(".", max(pad-n, 1))
		fmt.Fprintf(b, "%s%s%s: %s\n", ind, l.name, st.Gray(dots), l.value)
	}
}

func renderGroup(b *strings.Builder, g *GroupSummary, ind string, st Style) {
	for _, c := range g.Checks {
		total := c.Passes + c.Fails
		if c.Fails == 0 {
			fmt.Fprintf(b, "%s%s %s\n", ind, st.Green("✓"), c.Name)
			continue
		}
		pct := 0
		if total > 0 {
			pct = int(float64(c.Passes) * 100 / float64(total))
		}
		fmt.Fprintf(b, "%s%s %s\n", ind, st.Red("✗"), c.Name)
		fmt.Fprintf(b, "%s  %s\n", ind, st.Gray(fmt.Sprintf("↳ %d%% — ✓ %d / ✗ %d", pct, c.Passes, c.Fails)))
	}
	for i, sub := range g.Groups {
		if i > 0 || len(g.Checks) > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(b, "%s%s\n", ind, st.Bold("█ "+sub.Name))
		renderGroup(b, sub, ind+"  ", st)
	}
}

// hints memberi catatan singkat yang bisa ditindaklanjuti.
func hints(d *SummaryData, thresholds int) []string {
	var out []string
	var n5xx, n429, total int64
	for c, n := range d.HTTPStatus {
		code, _ := strconv.Atoi(c)
		total += n
		if code >= 500 {
			n5xx += n
		}
		if code == 429 {
			n429 += n
		}
	}
	if n5xx > 0 {
		out = append(out, fmt.Sprintf("%d respons 5xx: server error saat dibebani. Cek log aplikasi (Laravel: storage/logs/laravel.log) dan log web server.", n5xx))
	}
	if n429 > 0 {
		out = append(out, fmt.Sprintf("%d respons 429: server membatasi laju request (rate limit). Di Laravel cek middleware throttle / RateLimiter.", n429))
	}
	if n := d.HTTPStatus["0"]; n > 0 {
		out = append(out, fmt.Sprintf("%d request gagal tanpa respons (lihat daftar error jaringan di atas).", n))
	}
	if m, ok := d.Metrics["dropped_iterations"]; ok && m.Values["count"] > 0 {
		out = append(out, fmt.Sprintf("%s iterasi tidak sempat dijalankan karena VU kurang. Naikkan preAllocatedVUs/maxVUs.", Num(m.Values["count"])))
	}
	dur, okD := d.Metrics["http_req_duration"]
	wait, okW := d.Metrics["http_req_waiting"]
	if okD && okW && dur.Values["avg"] > 50 && total > 0 && d.HTTPStatus["0"]*10 < total {
		if share := wait.Values["avg"] / dur.Values["avg"]; share >= 0.8 {
			out = append(out, fmt.Sprintf("Rata-rata %d%% durasi request habis menunggu respons pertama dari server (http_req_waiting/TTFB) — umumnya waktu proses aplikasi/database.", int(share*100)))
		}
	}
	if thresholds == 0 && total > 0 {
		out = append(out, "Tambahkan options.thresholds (mis. http_req_duration: ['p(95)<500']) agar ada kriteria lulus/gagal; serbu keluar dengan kode 99 bila gagal, cocok untuk CI.")
	}
	return out
}
