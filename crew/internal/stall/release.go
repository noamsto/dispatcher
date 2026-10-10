package stall

import (
	"strconv"
	"strings"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/frame"
)

// finishedRelease is `_finished_release`: the worker posted done/failed, so
// wait out --release, then release its window and exit. nil means the worker
// re-opened the session (a later non-terminal status) and the main loop
// resumes watching; everything else exits, so a failed sample or --max-life
// never loops here.
func (w *watch) finishedRelease() error {
	relSession := "-"
	if bus.IsSessionID(w.cfg.fromID) {
		relSession = w.cfg.fromID[strings.LastIndex(w.cfg.fromID, "#")+1:]
	}
	prevChange := w.o.Clock.Seconds()
	prevText, haveText := "", false
	idleTicks := 0

	// release ends the watch on any verdict but 3. A helper that failed to run
	// released nothing, so it is retried like 3: nil keeps watching.
	release := func() error {
		_, rc, ran, err := w.shWrite("release", w.cfg.branch, relSession, w.bus.state,
			strconv.FormatInt(w.bus.ts, 10), strconv.FormatInt(w.cfg.release, 10))
		if err != nil {
			return err
		}
		if ran && rc != 3 {
			return exitCode(0)
		}
		idleTicks = 0
		prevChange = w.o.Clock.Seconds()
		return nil
	}

	for {
		if err := w.refresh(); err != nil {
			return err
		}
		switch w.bus.state {
		case "done", "failed":
		case "":
			if err := w.sleep(w.cfg.interval); err != nil {
				return err
			}
			continue
		default:
			return nil
		}
		t := w.o.Clock.Seconds()
		if t-w.start >= w.cfg.maxLife {
			return exitCode(0)
		}
		// A watchdog-posted `failed` marks a hung pane: keep it as evidence.
		if w.bus.source == "watchdog" {
			return exitCode(0)
		}
		plain, alive := w.p.Sample(w.ctx)
		if err := w.cancelled(); err != nil {
			return err
		}
		if !alive {
			return exitCode(0)
		}
		if !haveText || plain != prevText {
			prevText, haveText = plain, true
			prevChange = t
		}
		if t-w.bus.ts/1000 >= w.cfg.release {
			if w.cfg.engine == "claude" {
				colored := w.p.SampleColored(w.ctx)
				if err := w.cancelled(); err != nil {
					return err
				}
				if _, idle := frame.PaneIdleReason(plain, colored, false); idle {
					idleTicks++
				} else {
					idleTicks = 0
				}
				if idleTicks >= 2 {
					if err := release(); err != nil {
						return err
					}
				}
			} else {
				quietS := t - prevChange
				if frame.IsPrompt(w.cfg.engine, plain) || frame.IsPermissionPrompt(w.cfg.engine, plain) {
					quietS = 0
				}
				if quietS >= w.cfg.release {
					if err := release(); err != nil {
						return err
					}
				}
			}
		}
		if err := w.sleep(w.cfg.interval); err != nil {
			return err
		}
	}
}
