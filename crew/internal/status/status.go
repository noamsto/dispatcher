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
//
// `msg` gates one body: a review seam from a branch that has no plan seam is
// refused, so the miss surfaces at the review gate instead of at pr_open. It
// runs `status`'s own check — same task-doc condition, same plan.jq fold.
package status

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/clock"
	"github.com/noamsto/dispatcher/crew/internal/git"
	"github.com/noamsto/dispatcher/crew/internal/jqrun"
	"github.com/noamsto/dispatcher/crew/internal/jsonval"
	"github.com/noamsto/dispatcher/crew/internal/panestate"
	"github.com/noamsto/dispatcher/crew/internal/remote"
	"github.com/noamsto/dispatcher/crew/internal/sender"
	"github.com/noamsto/dispatcher/crew/internal/tmux"
)

//go:embed seam.jq
var seamProg string

//go:embed deslop.jq
var deslopProg string

//go:embed plan.jq
var planProg string

const exitFailure = 1

// Options is everything the arms read beyond the bus. Nil fields take the
// production default (CrewID and Git read the process's working
// directory) except Tmux, where nil publishes nothing to a pane.
type Options struct {
	CrewID func() string                                                       // `_crew_id`
	Git    git.Client                                                          // nil: the git on PATH
	Dir    string                                                              // the directory the toplevel is read from (-C); "" is "."
	Clock  clock.Clock                                                         // RealMS is jq's `now*1000|floor`; real time, never CREW_CLOCK
	Tmux   tmux.Client                                                         // nil: no tmux on PATH
	Fold   func(prog string, lines []string, vars map[string]any) (any, error) // jqrun.RunAny by default; a test seam
	Sender sender.Probes                                                       // the sender check's reads and CREW_ROLE_ID, TMUX_PANE; Getenv defaults to os.Getenv

	PeerHosts func() (map[string]remote.Host, error) // the settings hosts; read only for a peer: recipient
}

func (o Options) withDefaults() Options {
	if o.CrewID == nil {
		o.CrewID = func() string { return bus.CrewID(context.Background(), ".") }
	}
	if o.Git == nil {
		o.Git = git.Exec{}
	}
	if o.Dir == "" {
		o.Dir = "."
	}
	if o.Clock.Now == nil {
		o.Clock.Now = time.Now
	}
	if o.Sender.Getenv == nil {
		o.Sender.Getenv = os.Getenv
	}
	if o.PeerHosts == nil {
		o.PeerHosts = func() (map[string]remote.Host, error) {
			return remote.Load(context.Background(), remote.ConfigBin(), io.Discard)
		}
	}
	if o.Fold == nil {
		o.Fold = func(prog string, lines []string, vars map[string]any) (any, error) {
			in := make([]any, len(lines))
			for i, l := range lines {
				in[i] = l
			}
			return jqrun.RunAny(prog, in, 0, vars, jqrun.WithJQFromJSON())
		}
	}
	return o
}

// say writes to a stream whose failure is reported elsewhere, as main.go's does.
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
func refuseSender(ctx context.Context, sub, from, crew string, o Options, stderr io.Writer) bool {
	reason := sender.Check(ctx, from, crew, o.Sender)
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
// `_bus_append`. Callers take ts once before build: FitLine reruns it per shrink
// pass, and a ts taken inside would move.
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
func RunStatus(ctx context.Context, req StatusRequest, paths bus.Paths, stderr io.Writer, o Options) int {
	o = o.withDefaults()
	crew, ok := begin("status", paths, stderr, o)
	if !ok {
		return exitFailure
	}

	from, state, detail, pr, restamp := req.From, req.State, req.Detail, req.PR, req.Restamp
	if refuseSender(ctx, "status", from, crew, o, stderr) {
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
	if line := g.check(ctx); line != "" {
		say(stderr, "%s\n", line)
		return exitFailure
	}

	ts := o.Clock.RealMS()
	code := post("status", paths, stderr, func(d string) string {
		return encodeLine(row[statusBody]{
			TS: ts, CrewID: crew, From: from, To: "dispatcher:" + crew, Kind: bus.KindStatus,
			Body: statusBody{State: state, Detail: d, PRURL: pr, Restamp: restamp},
		})
	}, detail)
	if code != 0 {
		return code
	}
	publishPane(ctx, o, state, detail)
	pointToPhase(ctx, o, state, detail, stderr)
	return 0
}

// publishPane mirrors a worker's status onto its OWN lead pane, never a role
// pane's (CREW_ROLE_ID set). The @crew_role=lead check also keeps a
// dispatcher-process `crew status` off the dispatcher's pane. Best effort: it
// must never fail the write that precedes it.
func publishPane(ctx context.Context, o Options, state, detail string) {
	pane := o.Sender.Getenv("TMUX_PANE")
	if o.Sender.Getenv("CREW_ROLE_ID") != "" || pane == "" || o.Tmux == nil {
		return
	}
	role, err := o.Tmux.Run(ctx, "display-message", "-p", "-t", pane, "#{@crew_role}")
	if err != nil || role != "lead" {
		return
	}
	panestate.Publish(func(option, value string) {
		_, _ = o.Tmux.Run(ctx, "set-option", "-p", "-t", pane, option, value)
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
