// Package status is `crew status` and `crew msg`: the two writers of a row onto
// the bus.
//
// `status` is a write behind gates. A worker session posting pr_open or done
// is refused unless its task doc's acceptance ledger conforms (pr_open,
// internal/ledger) and, for a standard or deep implement session, the log holds
// the lead's review seam and deslop seam, plus a plan seam (or a resume of the
// branch) when its task doc says `plan: required`. The seam folds are jq
// programs (the arm's own seam.jq and deslop.jq, plus plan.jq), run through
// jqrun. Refusing less often than the arm is a regression: every gate fails
// closed.
package status

import (
	"context"
	_ "embed"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/clock"
	"github.com/noamsto/dispatcher/crew/internal/jqrun"
	"github.com/noamsto/dispatcher/crew/internal/jsonv"
	"github.com/noamsto/dispatcher/crew/internal/panestate"
)

//go:embed seam.jq
var seamProg string

//go:embed deslop.jq
var deslopProg string

//go:embed plan.jq
var planProg string

const (
	exitFailure = 1

	// jq's statuses for a fold that cannot run: 2 unreadable input, 5 a runtime error.
	jqUnreadable = 2
	jqRuntime    = 5
)

// Options is everything the arms read beyond the bus. Nil fields take the
// production default (CrewID and Toplevel read the process's working
// directory) except Tmux, where nil means no tmux on PATH.
type Options struct {
	CrewID   func() string                                                                           // `_crew_id`
	Toplevel func() string                                                                           // `git rev-parse --show-toplevel`, "" outside a checkout
	NowMS    func() int64                                                                            // jq's `now*1000|floor`; real time, never CREW_CLOCK
	Getenv   func(string) string                                                                     // CREW_ROLE_ID, TMUX_PANE
	Tmux     func(args ...string) (string, error)                                                    // stdout of `tmux args...`
	Fold     func(prog string, rows []jsonv.Value, vars map[string]jsonv.Value) (jsonv.Value, error) // jqrun.Run by default; a test seam
}

func (o Options) withDefaults() Options {
	if o.CrewID == nil {
		o.CrewID = func() string { return bus.CrewID(context.Background(), ".") }
	}
	if o.Toplevel == nil {
		o.Toplevel = func() string { return bus.Toplevel(context.Background(), ".") }
	}
	if o.Getenv == nil {
		o.Getenv = os.Getenv
	}
	if o.Fold == nil {
		o.Fold = func(prog string, rows []jsonv.Value, vars map[string]jsonv.Value) (jsonv.Value, error) {
			return jqrun.Run(prog, rows, 0, vars, jqrun.WithJQFromJSON())
		}
	}
	return o
}

// say writes to a stream whose failure is reported elsewhere (the exit status,
// or nowhere, for stderr) — main.go's helper, same reason.
func say(w io.Writer, format string, args ...any) { _, _ = fmt.Fprintf(w, format, args...) }

// begin is the arms' shared preamble: the crew id, then the bus dir the append
// needs on a repo with no bus yet.
func begin(sub string, paths bus.Paths, stderr io.Writer, o Options) (string, bool) {
	crew := o.CrewID()
	if crew == "" {
		say(stderr, "crew: CREW_ID unset and no WORKER_TASK.md crew_id\n")
		return "", false
	}
	if err := os.MkdirAll(paths.Dir, 0o755); err != nil {
		say(stderr, "crew: %s: %v\n", sub, err)
		return "", false
	}
	return crew, true
}

// stamp is the row's ts, computed once before the builder: FitLine runs it
// again for every shrink pass, and a stamp taken inside would move with the loop.
func stamp(o Options) jsonv.Value {
	if o.NowMS == nil {
		return jsonv.Num(float64(clock.Clock{Now: time.Now}.RealMS()))
	}
	return jsonv.Num(float64(o.NowMS()))
}

func member(key string, v jsonv.Value) jsonv.Member { return jsonv.Member{Key: key, Val: v} }

func compact(v jsonv.Value) string { return string(jsonv.Append(nil, v, jsonv.Options{})) }

// post fits the row to a bus line and appends it, the arm's `_fit_line` and
// `_bus_append`.
func post(sub string, paths bus.Paths, stderr io.Writer, build func(text string) string, full string) int {
	if err := bus.Append(paths.Log, bus.FitLine(build, full)); err != nil {
		say(stderr, "crew: %s: %v\n", sub, err)
		return exitFailure
	}
	return 0
}

// RunStatus is `crew status <from> <state> [detail] [pr_url] [--restamp] [--]`.
func RunStatus(args []string, paths bus.Paths, stderr io.Writer, o Options) int {
	o = o.withDefaults()
	crew, ok := begin("status", paths, stderr, o)
	if !ok {
		return exitFailure
	}

	// --restamp may sit anywhere; stripping it keeps the positionals in order.
	restamp := false
	var pos []string
parse:
	for len(args) > 0 {
		switch a := args[0]; {
		case a == "--restamp":
			restamp = true
			args = args[1:]
		case a == "--":
			pos = append(pos, args[1:]...)
			break parse
		case strings.HasPrefix(a, "--"):
			say(stderr, "crew: status: unknown arg '%s' (use -- before a detail that starts with dashes)\n", a)
			return exitFailure
		default:
			pos = append(pos, a)
			args = args[1:]
		}
	}
	at := func(i int) string {
		if i < len(pos) {
			return pos[i]
		}
		return ""
	}
	from, state, detail, pr := at(0), at(1), at(2), at(3)

	switch state {
	case "working", "blocked", "pr_open", "done", "failed", "exited":
	default:
		say(stderr, "crew: status state must be one of working|blocked|pr_open|done|failed|exited (got '%s')\n", state)
		return exitFailure
	}
	if restamp && state != "blocked" {
		say(stderr, "crew: --restamp is only valid with blocked\n")
		return exitFailure
	}

	// Terminal states are posted once: a re-announced pr_open/done wakes `watch`
	// twice with an identical batch.
	switch state {
	case "pr_open", "done", "failed", "exited":
		if lastPosted(paths.Log, crew, from) == state+"\t"+pr {
			return 0
		}
	}

	g := gate{crew: crew, from: from, state: state, detail: detail, paths: paths, o: o}
	if line := g.check(); line != "" {
		say(stderr, "%s\n", line)
		return exitFailure
	}

	ts := stamp(o)
	code := post("status", paths, stderr, func(d string) string {
		body := []jsonv.Member{member("state", jsonv.Str(state))}
		if d != "" {
			body = append(body, member("detail", jsonv.Str(d)))
		}
		if pr != "" {
			body = append(body, member("pr_url", jsonv.Str(pr)))
		}
		if restamp {
			body = append(body, member("restamp", jsonv.Bool(true)))
		}
		return compact(jsonv.Object(
			member("ts", ts),
			member("crew_id", jsonv.Str(crew)),
			member("from", jsonv.Str(from)),
			member("to", jsonv.Str("dispatcher:"+crew)),
			member("kind", jsonv.Str("status")),
			member("body", jsonv.Object(body...)),
		))
	}, detail)
	if code != 0 {
		return code
	}
	publishPane(o, state, detail)
	return 0
}

// publishPane mirrors a worker's status onto its OWN lead pane; a role pane's
// does not (CREW_ROLE_ID set, and its @crew_role is the role). The
// @crew_role=lead check also keeps a dispatcher-process `crew status` from
// painting the dispatcher's own pane. Best effort: it must never fail the
// write that precedes it.
func publishPane(o Options, state, detail string) {
	pane := o.Getenv("TMUX_PANE")
	if o.Getenv("CREW_ROLE_ID") != "" || pane == "" || o.Tmux == nil {
		return
	}
	role, err := o.Tmux("display-message", "-p", "-t", pane, "#{@crew_role}")
	if err != nil || strings.TrimRight(role, "\n") != "lead" {
		return
	}
	panestate.Publish(func(option, value string) {
		_, _ = o.Tmux("set-option", "-p", "-t", pane, option, value)
	}, state, detail, "")
}

// lastPosted is the arm's
//
//	jq -r 'select(.crew_id==$c and .kind=="status" and .from==$m)
//	       | "\(.body.state)\t\(.body.pr_url // "")"' log 2>/dev/null | tail -1
//
// "" when nothing prints. jq stops at the first parse error, so the
// well-formed prefix decides; a row whose evaluation would error (a
// non-object row, a selected row with a non-object body) prints nothing and
// jq moves on.
func lastPosted(log, crew, from string) string {
	events, _ := bus.ReadEventsTolerant(log)
	var out strings.Builder
	for _, ev := range events {
		if ev.Raw.Kind() != jsonv.KindObject || ev.CrewID != crew || ev.Kind != bus.KindStatus {
			continue
		}
		// A missing or non-string from is null to jq, which equals no string.
		if f, _ := ev.Raw.Get("from"); f.Kind() != jsonv.KindString || ev.From != from {
			continue
		}
		state, pr, ok := bodyFields(ev.Body)
		if !ok {
			continue
		}
		out.WriteString(interpolate(state) + "\t" + interpolate(pr.Or(jsonv.Str(""))) + "\n")
	}
	text := strings.TrimSuffix(out.String(), "\n")
	return text[strings.LastIndex(text, "\n")+1:]
}

// bodyFields is `.body.state` and `.body.pr_url`; ok is false where jq's index
// would error, on a body that is neither an object nor null.
func bodyFields(body jsonv.Value) (state, pr jsonv.Value, ok bool) {
	switch body.Kind() {
	case jsonv.KindNull:
		return jsonv.Null(), jsonv.Null(), true
	case jsonv.KindObject:
		state, _ = body.Get("state")
		pr, _ = body.Get("pr_url")
		return state, pr, true
	case jsonv.KindFalse, jsonv.KindTrue, jsonv.KindNumber, jsonv.KindString, jsonv.KindArray:
	}
	return jsonv.Null(), jsonv.Null(), false
}

// interpolate is jq's `"\(v)"`: a string raw, anything else as compact JSON.
func interpolate(v jsonv.Value) string {
	if s, ok := v.AsString(); ok {
		return s
	}
	return compact(v)
}
