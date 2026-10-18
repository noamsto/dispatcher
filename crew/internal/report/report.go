// Package report is `crew report`: the per-run dispatch table — engine, model,
// tier, shape, outcome, duration — joining each `dispatch` row of a crew to the
// `status` rows of the session it dispatched.
package report

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/jsonval"
)

// header is the arm's printf, matching the fold's column order.
const header = "engine\tmodel\ttier\tshape\toutcome\tduration_s"

// The status for a fold that fails on a row it cannot use. A bus it cannot
// read maps through bus.JQFailure (2 unreadable, 5 corrupt) before the fold.
const exitType = 5

const none = "—"

// tsvEscape is @tsv's escaping of one column.
var tsvEscape = strings.NewReplacer(`\`, `\\`, "\t", `\t`, "\n", `\n`, "\r", `\r`)

// Options is everything Run reads beyond the bus: whether $JQ_COLORS was
// invalid (jq warns once, at startup, even for the raw-output fold).
type Options struct {
	JQColorsInvalid bool
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

	rows, err := fold(events, crew)
	if err != nil {
		say(stderr, "crew: report: %s: %v\n", paths.Log, err)
		return exitType
	}
	for _, r := range rows {
		say(stdout, "%s\n", r)
	}
	return 0
}

// fold renders one table row per dispatch of the crew. A dispatch joins every
// status row whose sender, minus the "worker:" prefix and "#session" suffix, is
// its branch.
func fold(events []bus.Event, crew string) ([]string, error) {
	var dispatches, statuses []bus.Event
	for _, ev := range events {
		if _, ok := ev.Doc.(map[string]any); !ok && ev.Doc != nil {
			return nil, fmt.Errorf("cannot index %s with \"crew_id\"", typeName(ev.Doc))
		}
		if ev.CrewID != crew {
			continue
		}
		if ev.Kind == bus.KindDispatch {
			dispatches = append(dispatches, ev)
		}
		if ev.Kind == bus.KindStatus {
			statuses = append(statuses, ev)
		}
	}

	var rows []string
	for _, d := range dispatches {
		var mine []bus.Event
		for _, st := range statuses {
			if d.Branch != "" && sessionBranch(st.From) == d.Branch {
				mine = append(mine, st)
			}
		}
		// Stable, so file order breaks ties; a row with no ts sorts first.
		sort.SliceStable(mine, func(i, j int) bool { return ts(mine[i]) < ts(mine[j]) })

		var outcome any = none
		start, last := math.Inf(-1), math.Inf(-1)
		for i, st := range mine {
			state, err := stateOf(st)
			if err != nil {
				return nil, err
			}
			if state == "working" && math.IsInf(start, -1) {
				start = ts(st)
			}
			if i == len(mine)-1 {
				last = ts(st)
				if truthy(state) {
					outcome = state
				}
			}
		}
		duration := none
		if !math.IsInf(start, -1) && !math.IsInf(last, -1) {
			duration = strconv.FormatFloat(math.Floor((last-start)/1000), 'f', -1, 64)
		}
		shape := doc(d)["shape"]
		if !truthy(shape) {
			shape = none
		}

		var cells []string
		for _, c := range []any{doc(d)["engine"], doc(d)["model"], doc(d)["tier"], shape, outcome, duration} {
			cell := ""
			if c != nil {
				cell = jsonval.RawText(c)
			}
			cells = append(cells, tsvEscape.Replace(cell))
		}
		rows = append(rows, strings.Join(cells, "\t"))
	}
	return rows, nil
}

// sessionBranch is a sender's branch.
func sessionBranch(from string) string {
	from = strings.TrimPrefix(from, "worker:")
	if i := strings.LastIndex(from, "#"); i >= 0 {
		return from[:i]
	}
	return from
}

func doc(e bus.Event) map[string]any {
	m, _ := e.Doc.(map[string]any)
	return m
}

// stateOf is a status body's state; an absent or null body has none.
func stateOf(e bus.Event) (any, error) {
	switch b := doc(e)["body"].(type) {
	case nil:
		return nil, nil
	case map[string]any:
		return b["state"], nil
	default:
		return nil, fmt.Errorf("cannot index %s with \"state\"", typeName(b))
	}
}

// ts is an event's timestamp; a row without a numeric one sorts first.
func ts(e bus.Event) float64 {
	if f, ok := e.TS.AsFloat(); ok {
		return f
	}
	return math.Inf(-1)
}

func truthy(v any) bool { return v != nil && v != false }

// typeName is jq's name for a decoded value's type.
func typeName(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case json.Number, float64:
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	}
	return "object"
}
