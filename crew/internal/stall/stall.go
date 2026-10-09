// Package stall is `crew stall-watch`, the per-worker liveness watchdog that
// `dispatch` spawns next to each pane, ported from the arm of the same name in
// crew.sh. That arm's header comment still documents the detectors, the
// escalation rules and the test seams; the arm itself now only delegates here
// and answers the `--sh` call-outs into the bash helpers that stay.
//
// One loop samples the pane every --interval and runs the detectors in the
// arm's order. Every bus write goes through post, which re-reads the bus first
// (INV-W1) and stops the watch once the worker has finished; postBlocked and
// postClear keep one watchdog episode per prefix on the bus (INV-W3, INV-W2).
// Each `exit` of the arm is an exitCode error returned up to Run.
package stall

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/clock"
	"github.com/noamsto/dispatcher/crew/internal/frame"
	"github.com/noamsto/dispatcher/crew/internal/stall/probe"
)

// Options is everything Run reads beyond its argv and the bus.
type Options struct {
	CrewID     func() string
	Clock      clock.Clock
	NewProbes  func(pane string) probe.Probes // called once, after --pane parses
	BudgetFile string                         // ${XDG_DATA_HOME:-$HOME/.local/share}/crew/engine-budget.json
	PID        int                            // lock owner ($$)
}

// exitCode ends the watch with that status: every `exit` in the arm is one.
type exitCode int

func (c exitCode) Error() string { return fmt.Sprintf("exit %d", int(c)) }

// busView is the worker's latest own status row in this run.
type busView struct {
	ts                    int64
	state, source, detail string
}

type watch struct {
	ctx    context.Context
	cfg    config
	p      probe.Probes
	o      Options
	paths  bus.Paths
	stderr io.Writer

	now, start, lastChange, quietFor int64 // set by the loop before the detectors run
	tick                             int
	text                             string
	suppressed                       bool // D8 may set it mid-tick; later detectors see it
	bgwait, engineSeen, busStale     bool // engineSeen is shared by D5 and role EOL
	bus                              busView

	fd frameDet
	pd probeDet
}

// Run is the arm: it returns the status the arm exited with.
func Run(ctx context.Context, argv []string, paths bus.Paths, stderr io.Writer, o Options) int {
	cfg, err := parseArgs(argv, o.CrewID)
	if err != nil {
		if msg := err.Error(); msg != "" {
			_, _ = fmt.Fprintln(stderr, msg)
		}
		return 1
	}
	p := o.NewProbes(cfg.pane)
	cfg.budgetOn = decideBudget(ctx, cfg, p)
	// A non-claude role watch has only D8 and its end-of-life exit live; with
	// D8 off nothing is left, and the pane's @crew_state belongs to dispatch's
	// --role-watch.
	if cfg.roleMode && !cfg.budgetOn && cfg.engine != "claude" {
		return 0
	}
	// One second of slack: the clock truncates to seconds while rows carry ms,
	// so a status the launcher posted just before this start still counts.
	cfg.runStartMS = (o.Clock.Seconds() - 1) * 1000
	w := &watch{ctx: ctx, cfg: cfg, p: p, o: o, paths: paths, stderr: stderr}
	var code exitCode
	if err := w.run(); !errors.As(err, &code) {
		_, _ = fmt.Fprintf(stderr, "crew: stall-watch: %v\n", err)
		return 1
	}
	return int(code)
}

// decideBudget is D8's on/off switch, decided once: the engine and the pane's
// model are fixed for the watch. A pi pane on a local model spends no metered
// quota (the launch gate skips it too); any failure to tell keeps D8 on.
func decideBudget(ctx context.Context, cfg config, p probe.Probes) bool {
	if cfg.noBudget {
		return false
	}
	switch cfg.engine {
	case "claude", "codex", "cursor":
		return true
	case "pi":
		model := p.PaneModel(ctx)
		if model == "" {
			return true
		}
		out, rc := p.Sh(ctx, "local-model", model)
		_, ok := shVerdict(rc)
		return !ok || out == ""
	}
	return false
}

// run is the arm's main loop. It only ever returns an exitCode, or the error
// a detector step failed with.
func (w *watch) run() error {
	w.start = w.o.Clock.Seconds()
	if err := w.sleep(w.cfg.grace); err != nil {
		return err
	}
	fails := 0
	lastText, haveText := "", false
	w.lastChange = w.o.Clock.Seconds()
	if err := w.refresh(); err != nil {
		return err
	}
	for {
		w.now = w.o.Clock.Seconds()
		// The watchdog is nohup-detached: without a hard cap a bug or an
		// orphaned pane leaves it polling forever.
		if w.now-w.start >= w.cfg.maxLife {
			return exitCode(0)
		}
		// pr_open and working are not terminal: a pr_open worker is still
		// watching its CI.
		switch w.bus.state {
		case "done", "failed":
			if w.cfg.roleMode || w.cfg.release == 0 {
				return exitCode(0)
			}
			if err := w.finishedRelease(); err != nil {
				return err
			}
			haveText = false
			w.lastChange = w.o.Clock.Seconds()
			continue
		case "exited":
			return exitCode(0)
		}

		text, alive := w.p.Sample(w.ctx)
		if err := w.cancelled(); err != nil {
			return err
		}
		if !alive {
			// A quorum, not a single failure: one tmux hiccup must not disarm
			// a 12h watch, and the `exited` backstop owns the real case.
			fails++
			if fails >= 3 {
				return exitCode(0)
			}
			if err := w.sleep(w.cfg.interval); err != nil {
				return err
			}
			w.tick++
			if w.tick%4 == 0 {
				w.busStale = true
			}
			continue
		}
		fails = 0
		w.text = text
		if !haveText || text != lastText {
			lastText, haveText = text, true
			w.lastChange = w.now
		}

		// A worker's own `blocked` means it is in `crew await`: the dispatcher
		// already knows, and a held await leaves a static pane by design.
		w.suppressed = w.bus.state == "blocked" && w.bus.source != "watchdog"
		w.quietFor = w.now - w.lastChange
		w.bgwait = w.cfg.sigBgwait && w.quietFor < w.cfg.bgWait && frame.IsBgWait(text)

		for _, step := range []func() error{w.probePre, w.frameDetect, w.d6, w.escalate} {
			if err := step(); err != nil {
				return err
			}
		}

		if err := w.sleep(w.cfg.interval); err != nil {
			return err
		}
		w.tick++
		// A whole-log read every tick for 12h is thousands of reads per
		// worker; every 4th tick is ≤60s of latency against 1800s thresholds.
		// post's pre-write read is exempt.
		if w.tick%4 == 0 {
			if err := w.refresh(); err != nil {
				return err
			}
			w.busStale = false
		}
	}
}

// sleep is `_clock_sleep` under the arm's `set -e`: an interval sleep rejects
// ended the arm with sleep's status 1, its message already on stderr. A signal
// ends it with the shell's 128+signo.
func (w *watch) sleep(interval string) error {
	if err := w.o.Clock.SleepCtx(w.ctx, interval, w.stderr); err != nil {
		if err := w.cancelled(); err != nil {
			return err
		}
		return exitCode(1)
	}
	return nil
}

// cancelled is the arm dying on the signal. A probe that returns into a
// cancelled context was cut short, and its failure value ("", not alive) is
// no evidence: acting on it would post for a live worker or end the watch
// with 0. Every probe whose answer decides what happens next is followed by
// this check.
func (w *watch) cancelled() error {
	if w.ctx.Err() != nil {
		return exitCode(exitCodeFor(w.ctx))
	}
	return nil
}
