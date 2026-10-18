// Package status is `crew status` and `crew msg`: the two writers of a row onto
// the bus.
//
// `status` is a write behind gates. A worker session posting pr_open or done
// is refused unless its task doc's acceptance ledger conforms (pr_open,
// internal/ledger) and, for a standard or deep implement session, the log holds
// the lead's review seam and deslop seam, plus a plan seam (or a resume of the
// branch) when its task doc says `plan: required`. The seam folds are Go
// (seam.go). Refusing less often than the arm is a regression: every gate fails
// closed.
package status

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/clock"
	"github.com/noamsto/dispatcher/crew/internal/jsonval"
	"github.com/noamsto/dispatcher/crew/internal/panestate"
	"github.com/noamsto/dispatcher/crew/internal/sender"
)

const exitFailure = 1

// Options is everything the arms read beyond the bus. Nil fields take the
// production default (CrewID and Toplevel read the process's working
// directory) except Tmux, where nil means no tmux on PATH.
type Options struct {
	CrewID   func() string                        // `_crew_id`
	Toplevel func() string                        // `git rev-parse --show-toplevel`, "" outside a checkout
	NowMS    func() int64                         // jq's `now*1000|floor`; real time, never CREW_CLOCK
	Getenv   func(string) string                  // CREW_ROLE_ID, TMUX_PANE
	Tmux     func(args ...string) (string, error) // stdout of `tmux args...`
	Sender   sender.Probes                        // the sender check's reads; Getenv defaults to Options.Getenv
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
	if o.NowMS == nil {
		o.NowMS = clock.Clock{Now: time.Now}.RealMS
	}
	if o.Sender.Getenv == nil {
		o.Sender.Getenv = o.Getenv
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

// refuseSender prints the sender check's refusal; true when the row must not be written.
func refuseSender(sub, from, crew string, o Options, stderr io.Writer) bool {
	reason := sender.Check(from, crew, o.Sender)
	if reason == "" {
		return false
	}
	say(stderr, "crew: %s: refusing sender '%s' — %s\n", sub, from, reason)
	return true
}

// row is a row as written; field order is the key order.
type row[B any] struct {
	TS     int64    `json:"ts"`
	CrewID string   `json:"crew_id"`
	From   string   `json:"from"`
	To     string   `json:"to"`
	Kind   bus.Kind `json:"kind"`
	Body   B        `json:"body"`
}

type statusBody struct {
	State   string `json:"state"`
	Detail  string `json:"detail,omitempty"`
	PRURL   string `json:"pr_url,omitempty"`
	Restamp bool   `json:"restamp,omitempty"`
}

// encodeLine is v as one compact JSON line. Unescaped <>& keep the measured
// length of a line equal to the written one.
func encodeLine(v any) string {
	var b bytes.Buffer
	_ = jsonval.Encode(&b, v, false)
	return strings.TrimSuffix(b.String(), "\n")
}

// post fits the row to a bus line and appends it, the arm's `_fit_line` and
// `_bus_append`. Callers take the row's ts once before build: FitLine reruns it
// for every shrink pass, and a ts taken inside would move with the loop.
func post(sub string, paths bus.Paths, stderr io.Writer, build func(text string) string, full string) int {
	if err := bus.Append(paths.Log, bus.FitLine(build, full)); err != nil {
		say(stderr, "crew: %s: %v\n", sub, err)
		return exitFailure
	}
	return 0
}

// StatusRequest is `crew status <from> <state> [detail] [pr_url] [--restamp]`.
type StatusRequest struct {
	From, State, Detail, PR string
	Restamp                 bool
}

// RunStatus posts a worker's state.
func RunStatus(req StatusRequest, paths bus.Paths, stderr io.Writer, o Options) int {
	o = o.withDefaults()
	crew, ok := begin("status", paths, stderr, o)
	if !ok {
		return exitFailure
	}

	from, state, detail, pr, restamp := req.From, req.State, req.Detail, req.PR, req.Restamp
	if refuseSender("status", from, crew, o, stderr) {
		return exitFailure
	}

	switch state {
	case "working", "blocked", "pr_open", "done", "failed", "exited", "reaped":
	default:
		say(stderr, "crew: status state must be one of working|blocked|pr_open|done|failed|exited|reaped (got '%s')\n", state)
		return exitFailure
	}
	if state == "reaped" && (strings.HasPrefix(from, "worker:") || strings.HasPrefix(from, "role:")) {
		say(stderr, "crew: status: reaped is posted only by crew reap\n")
		return exitFailure
	}
	if restamp && state != "blocked" {
		say(stderr, "crew: --restamp is only valid with blocked\n")
		return exitFailure
	}

	// Terminal states are posted once: a re-announced pr_open/done wakes `watch`
	// twice with an identical batch.
	switch state {
	case "pr_open", "done", "failed", "exited", "reaped":
		if lastPosted(paths.Log, crew, from) == state+"\t"+pr {
			return 0
		}
	}

	g := gate{crew: crew, from: from, state: state, detail: detail, paths: paths, o: o}
	if line := g.check(); line != "" {
		say(stderr, "%s\n", line)
		return exitFailure
	}

	ts := o.NowMS()
	code := post("status", paths, stderr, func(d string) string {
		return encodeLine(row[statusBody]{
			TS: ts, CrewID: crew, From: from, To: "dispatcher:" + crew, Kind: bus.KindStatus,
			Body: statusBody{State: state, Detail: d, PRURL: pr, Restamp: restamp},
		})
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

// lastPosted is the terminal dedupe key, "<state>\t<pr_url>", of the last
// status row this crew's from posted; "" when there is none. The well-formed
// prefix of the log decides (a torn tail is ignored). A row whose from is
// missing or not a string matches no from, and a body that is neither an object
// nor null is skipped. A key holding a newline keeps only its last line.
func lastPosted(log, crew, from string) string {
	events, _ := bus.ReadEventsTolerant(log)
	var last string
	for _, ev := range events {
		m, ok := ev.Doc.(map[string]any)
		if !ok || ev.CrewID != crew || ev.Kind != bus.KindStatus || m["from"] != from {
			continue
		}
		var state, pr string
		switch body := m["body"].(type) {
		case nil:
			state = "null"
		case map[string]any:
			state = text(body["state"])
			if v := body["pr_url"]; v != nil && v != false {
				pr = text(v)
			}
		default:
			continue
		}
		last = state + "\t" + pr
	}
	return last[strings.LastIndex(last, "\n")+1:]
}

// text is a string raw, anything else as compact JSON.
func text(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return encodeLine(v)
}
