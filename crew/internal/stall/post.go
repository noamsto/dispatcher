package stall

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/jsonv"
	"github.com/noamsto/dispatcher/crew/internal/panestate"
	"github.com/noamsto/dispatcher/crew/internal/roster"
)

// post is INV-W1: the watchdog is a subordinate writer, so it re-reads the bus
// immediately before every append — the hazard lives in the gap between
// sampling the pane and writing — and stops once the worker has finished.
// Exempt from the 4-tick read cadence on purpose: appends are rare, and a
// stale view here would be a lie on the bus.
func (w *watch) post(state, detail string) error {
	err := w.postRow(state, detail)
	var we writeError
	if errors.As(err, &we) {
		return w.fail(we.err)
	}
	return err
}

// writeError is a bus write that failed. A bare `_post` died on it under the
// arm's `set -e`; `_post_blocked` ran only in `if` context, where it meant not
// posted.
type writeError struct{ err error }

func (e writeError) Error() string { return e.err.Error() }

// postRow is post with a failed write returned as a writeError. A signal ends
// the watch before anything is written: the arm died on it posting nothing.
func (w *watch) postRow(state, detail string) error {
	if err := w.cancelled(); err != nil {
		return err
	}
	if err := w.refresh(); err != nil {
		return err
	}
	switch w.bus.state {
	case "done", "failed", "exited":
		return exitCode(0)
	}
	if err := os.MkdirAll(w.paths.Dir, 0o755); err != nil {
		return writeError{err}
	}
	row := jsonv.Object(
		jsonv.Member{Key: "ts", Val: jsonv.Num(float64(w.o.Clock.RealMS()))},
		jsonv.Member{Key: "crew_id", Val: jsonv.Str(w.cfg.crew)},
		jsonv.Member{Key: "from", Val: jsonv.Str(w.cfg.fromID)},
		jsonv.Member{Key: "to", Val: jsonv.Str("dispatcher:" + w.cfg.crew)},
		jsonv.Member{Key: "kind", Val: jsonv.Str("status")},
		jsonv.Member{Key: "body", Val: jsonv.Object(
			jsonv.Member{Key: "state", Val: jsonv.Str(state)},
			jsonv.Member{Key: "detail", Val: jsonv.Str(detail)},
			jsonv.Member{Key: "source", Val: jsonv.Str("watchdog")},
		)},
	)
	if err := bus.Append(w.paths.Log, string(jsonv.Append(nil, row, jsonv.Options{}))); err != nil {
		return writeError{err}
	}
	// --role-watch owns @crew_state for a role pane. Only `blocked` carries
	// the watchdog marker, so a clearance never renders `working (watchdog)`.
	if !w.cfg.roleMode {
		source := ""
		if state == "blocked" {
			source = "watchdog"
		}
		w.publishPaneState(state, detail, source)
	}
	return nil
}

// fail is a write the arm's `set -e` died on: exit 1 with the reason.
func (w *watch) fail(err error) error {
	_, _ = fmt.Fprintf(w.stderr, "crew: stall-watch: %v\n", err)
	return exitCode(1)
}

// postBlocked is INV-W3: one open watchdog episode per branch per prefix,
// checked on the bus, the only thing two watchdogs on one branch share. Same
// prefix rather than any prefix, so a dead pane can still raise `quiet:` over
// an open `stalled:` (which never escalates). An open `prompt:` or `quota:` is
// sticky against every prefix: a frozen prompt satisfies `quiet:` by
// construction, and overwriting either would give it the escalation path it is
// denied. false means suppressed or not written; either way the detector's
// episode stays unopened and the next tick tries again.
func (w *watch) postBlocked(prefix, detail string) (bool, error) {
	if err := w.refresh(); err != nil {
		return false, err
	}
	if w.bus.state == "blocked" && w.bus.source == "watchdog" {
		d := w.bus.detail
		if strings.HasPrefix(d, prefix) || strings.HasPrefix(d, "prompt:") || strings.HasPrefix(d, "quota:") {
			return false, nil
		}
	}
	err := w.postRow("blocked", detail)
	var we writeError
	if errors.As(err, &we) {
		_, _ = fmt.Fprintf(w.stderr, "crew: stall-watch: %v\n", we.err)
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// postClear is INV-W2: a clearance never overwrites a later worker statement,
// and clears only its own prefix — another detector's episode may be the live
// one, and resetting the roster to `working` would leave its later `failed`
// with no visible `blocked` to explain it.
func (w *watch) postClear(prefix string) error {
	if err := w.refresh(); err != nil {
		return err
	}
	if w.bus.state == "blocked" && w.bus.source == "watchdog" && strings.HasPrefix(w.bus.detail, prefix) {
		return w.post("working", prefix+" cleared")
	}
	return nil
}

// publishPaneState mirrors the status onto the pane's border options.
func (w *watch) publishPaneState(state, detail, source string) {
	if w.cfg.pane == "" {
		return
	}
	panestate.Publish(func(option, value string) { w.p.SetPaneOption(w.ctx, option, value) }, state, detail, source)
}

// engineAlive is `_pane_engine_alive`, quiet:'s corroborating evidence for
// dead:. A turn that hangs with its engine still resident never reaches dead:
// through quiet: — only a vanished process does.
func (w *watch) engineAlive() (bool, error) {
	cmd := w.p.PaneCmd(w.ctx)
	if err := w.cancelled(); err != nil {
		return false, err
	}
	return cmd != "" && roster.IsEngineCmd(cmd), nil
}
