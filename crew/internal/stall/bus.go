package stall

import (
	_ "embed"
	"strconv"

	"github.com/noamsto/dispatcher/crew/internal/jqrun"
	"github.com/noamsto/dispatcher/crew/internal/jsonv"
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
	out, err := jqrun.Run(refreshProgram, rows, 0, map[string]jsonv.Value{
		"c":  jsonv.Str(w.cfg.crew),
		"m":  jsonv.Str(w.cfg.me),
		"f":  jsonv.Str(w.cfg.fromID),
		"e0": jsonv.Str(w.cfg.ownEpoch),
		"t0": jsonv.Num(float64(w.cfg.runStartMS)),
	})
	// A jq that cannot run at all is the arm's `2>/dev/null || true`: no rows.
	if err != nil {
		return nil
	}
	matched := out.Elems()
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

func field(row jsonv.Value, i int) string {
	v, _ := row.At(i)
	s, _ := v.AsString()
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
