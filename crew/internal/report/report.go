// Package report is `crew report`: the per-run dispatch table — engine, model,
// tier, shape, outcome, duration — joining each `dispatch` row of a crew to the
// `status` rows of the session it dispatched. The fold is the original jq
// program from adapters/core/crew.sh (report.jq), run through gojq, so the
// program stays one shared source with the arm it replaces.
package report

import (
	_ "embed"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/jqrun"
	"github.com/noamsto/dispatcher/crew/internal/jsonv"
)

//go:embed report.jq
var program string

// header is the arm's printf, matching the fold's column order.
const header = "engine\tmodel\ttier\tshape\toutcome\tduration_s"

// jq's own status for a fold it cannot run: a bus it cannot read (2) or a row
// whose fields the program cannot index or subtract (5) come from bus.JQFailure;
// a gojq type error is jq's 5.
const exitType = 5

// Options is everything Run reads beyond the bus: whether $JQ_COLORS was
// invalid (jq warns once, at startup, even for the raw-output fold), and the
// clock the fold would freeze `now` to (it does not read `now`; the field
// keeps the package deterministic in tests, as crews.Options does).
type Options struct {
	JQColorsInvalid bool
	Now             func() time.Time
}

// say writes to a stream whose failure is reported elsewhere (the exit status,
// or nowhere, for stderr) — main.go's helper, same reason.
func say(w io.Writer, format string, args ...any) { _, _ = fmt.Fprintf(w, format, args...) }

// Run is the arm: the crew defaults to the caller's, a log that is not a
// regular file prints nothing at all, and every other outcome prints the header
// first — the arm prints it before jq starts, so a fold that then fails keeps
// it.
func Run(crew string, paths bus.Paths, stdout, stderr io.Writer, o Options) int {
	// `jq -s` has no torn-tail tolerance: a break anywhere costs every row, so
	// this is the strict read.
	events, err := bus.ReadEvents(paths.Log)
	if errors.Is(err, bus.ErrNoLog) {
		return 0
	}
	say(stdout, "%s\n", header)
	if o.JQColorsInvalid {
		say(stderr, "Failed to set $JQ_COLORS\n")
	}
	if err != nil {
		msg, code := bus.JQFailure(err)
		say(stderr, "crew: report: %s: %s\n", paths.Log, msg)
		return code
	}

	raws := make([]jsonv.Value, len(events))
	for i, ev := range events {
		raws[i] = ev.Raw
	}
	now := o.Now()
	nowSec := float64(now.Unix()) + float64(now.Nanosecond()/1000)/1e6
	out, err := jqrun.Run(program, raws, nowSec, map[string]jsonv.Value{"crew": jsonv.Str(crew)})
	if err != nil {
		say(stderr, "crew: report: %s: %v\n", paths.Log, err)
		return exitType
	}
	if s, _ := out.AsString(); s != "" {
		say(stdout, "%s\n", s)
	}
	return 0
}
