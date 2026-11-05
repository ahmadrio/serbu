package engine

import (
	"fmt"
	"time"

	"serbu/internal/ui"
)

var spinner = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// startProgress menampilkan progress realtime (terminal) atau baris periodik (CI/log).
func (e *Engine) startProgress() func(time.Duration) {
	stop := make(chan struct{})
	done := make(chan struct{})
	tty := e.con.ErrTTY
	if e.cfg.Quiet {
		close(done)
	} else {
		go func() {
			defer close(done)
			interval := 10 * time.Second
			if tty {
				interval = 200 * time.Millisecond
			}
			t := time.NewTicker(interval)
			defer t.Stop()
			frame := 0
			for {
				select {
				case <-stop:
					if tty {
						e.con.ClearProgress()
					}
					return
				case <-t.C:
					frame++
					if tty {
						e.con.DrawProgress(e.progressLines(frame))
					} else {
						e.con.Log(e.statusLine(""))
					}
				}
			}
		}()
	}
	return func(elapsed time.Duration) {
		close(stop)
		<-done
		st := e.con.Err
		msg := fmt.Sprintf("selesai dalam %s — %d iterasi selesai, %d terputus", ui.Duration(ms(elapsed)), e.completed.Load(), e.interrupted.Load())
		if e.stopCode != 0 {
			msg = fmt.Sprintf("berhenti setelah %s — %d iterasi selesai, %d terputus (%s)", ui.Duration(ms(elapsed)), e.completed.Load(), e.interrupted.Load(), e.stopReason)
			e.con.Log("  " + st.Yellow("⚠ "+msg))
			return
		}
		e.con.Log("  " + st.Green("✓ ") + msg)
	}
}

func (e *Engine) statusLine(prefix string) string {
	el := time.Since(e.runStart)
	reqs, failed := e.coll.httpReqs.Load(), e.coll.httpFailed.Load()
	rps := 0.0
	if s := el.Seconds(); s > 0 {
		rps = float64(reqs) / s
	}
	failPct := 0.0
	if reqs > 0 {
		failPct = float64(failed) / float64(reqs)
	}
	st := e.con.Err
	failS := ui.Percent(failPct)
	if failed > 0 {
		failS = st.Red(failS)
	}
	return fmt.Sprintf("  %s%s  %s %d/%d  %s %d selesai, %d terputus  %s %d (%s)  %s %s",
		prefix, ui.Clock(el),
		st.Gray("VU"), e.activeVUs(), e.vusInit.Load(),
		st.Gray("iterasi"), e.completed.Load(), e.interrupted.Load(),
		st.Gray("req"), reqs, ui.PerSec(rps),
		st.Gray("gagal"), failS)
}

func (e *Engine) progressLines(frame int) []string {
	st := e.con.Err
	width := ui.TermWidth() - 1
	lines := []string{ui.Truncate(e.statusLine(st.Cyan(spinner[frame%len(spinner)])+" "), width)}
	nameW := 0
	for _, r := range e.runs {
		if n := len([]rune(r.plan.Name)); n > nameW {
			nameW = n
		}
	}
	for _, r := range e.runs {
		var status string
		switch {
		case !r.started.Load():
			status = st.Gray("menunggu, mulai " + fmtDur(r.plan.StartTime))
		case r.finished.Load():
			status = st.Green("✓ selesai")
		default:
			status = fmt.Sprintf("%3d%%", int(r.progress()*100))
		}
		l := fmt.Sprintf("  %-*s %s %s  %s", nameW, r.plan.Name, ui.Bar(r.progress(), 24, st), status, st.Gray(r.plan.Describe()))
		lines = append(lines, ui.Truncate(l, width))
	}
	return lines
}
