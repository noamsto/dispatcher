package stall

import (
	_ "embed"
	"encoding/json"
	"strconv"

	"github.com/noamsto/dispatcher/crew/internal/jqrun"
)

//go:embed refresh.jq
var refreshProgram string

// refresh is `_bus_refresh`: w.bus becomes the worker's latest own status in
// this run, or the zero view. Every read is scoped to the run (C-3): the log is
// append-only per repo and re-dispatch onto a branch is routine, so the
// previous run's `failed` would otherwise mute this watchdog for its life.
// A post from a session of this branch no older than ours means a resume took
// the pane over (INV-W0): the watch steps aside with exit 0, so it never
// samples the successor's pane under a dead session id.
func (w *watch) refresh() error {
	w.bus = busView{}
	// Rows older than the run are dropped by the reader and by the jq below: a
	// whole-log read every 4th tick costs memory proportional to the log.
	rows, ok := w.p.BusRows(w.paths.Log, w.cfg.runStartMS, 0)
	if !ok {
		return nil
	}
	out, err := jqrun.RunAny(refreshProgram, rows, 0, map[string]any{
		"c":  w.cfg.crew,
		"m":  w.cfg.me,
		"f":  w.cfg.fromID,
		"e0": w.cfg.ownEpoch,
		"t0": json.Number(strconv.FormatInt(w.cfg.runStartMS, 10)),
	})
	// A jq that cannot run at all is the arm's `2>/dev/null || true`: no rows.
	if err != nil {
		return nil
	}
	matched, _ := out.([]any)
	for _, r := range matched {
		if field(r, 0) == "1" {
			return exitCode(0)
		}
	}
	if len(matched) == 0 {
		return nil
	}
	last := matched[len(matched)-1]
	w.bus = busView{ts: tsMS(field(last, 1)), state: field(last, 2), source: field(last, 3), detail: field(last, 4)}
	return nil
}

func field(row any, i int) string {
	r, _ := row.([]any)
	if i < 0 || i >= len(r) {
		return ""
	}
	s, _ := r[i].(string)
	return s
}

// tsMS reads jq's `.ts|tostring`; a fractional or exponent form truncates.
func tsMS(s string) int64 {
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n
	}
	f, _ := strconv.ParseFloat(s, 64)
	return int64(f)
}
