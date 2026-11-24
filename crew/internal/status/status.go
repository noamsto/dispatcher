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
// A pr_open or done is also refused while a dispatcher msg to the session is
// still undelivered (undeliveredGate); the refusal prints and marks it.
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
	"path/filepath"
	"regexp"
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

// begin is the arms' shared preamble: the crew, then the directory of its log
// the append needs on a repo with no bus yet.
func begin(sub string, paths bus.Paths, stderr io.Writer, o Options) (bus.Crew, bool) {
	id := o.CrewID()
	if id == "" {
		say(stderr, "crew: CREW_ID unset and no WORKER_TASK.md crew_id\n")
		return bus.Crew{}, false
	}
	c, err := paths.Crew(id)
	if err != nil {
		say(stderr, "crew: %s: invalid crew id %s\n", sub, id)
		return bus.Crew{}, false
	}
	if err := os.MkdirAll(filepath.Dir(c.Log), 0o755); err != nil {
		say(stderr, "crew: %s: %v\n", sub, err)
		return bus.Crew{}, false
	}
	return c, true
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
	// BodyFile names the spilled full body of a msg too long for one line.
	BodyFile string `json:"body_file,omitempty"`
}

type statusBody struct {
	State   string `json:"state"`
	Detail  string `json:"detail,omitempty"`
	PRURL   string `json:"pr_url,omitempty"`
	Restamp bool   `json:"restamp,omitempty"`
	ToHost  string `json:"to_host,omitempty"`
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
func post(sub, log string, stderr io.Writer, build func(text string) string, full string) int {
	if err := bus.Append(log, bus.FitLine(build, full)); err != nil {
		say(stderr, "crew: %s: %v\n", sub, err)
		return exitFailure
	}
	return 0
}

// StatusRequest is `crew status <from> <state> [detail] [pr_url] [--restamp] [--to-host <h>]`.
type StatusRequest struct {
	From, State, Detail, PR, ToHost string
	Restamp                         bool
}

var hostName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// RunStatus posts a worker's state.
func RunStatus(ctx context.Context, req StatusRequest, paths bus.Paths, stderr io.Writer, o Options) int {
	o = o.withDefaults()
	c, ok := begin("status", paths, stderr, o)
	if !ok {
		return exitFailure
	}
	crew := c.ID

	from, state, detail, pr, restamp, toHost := req.From, req.State, req.Detail, req.PR, req.Restamp, req.ToHost
	if refuseSender(ctx, "status", from, crew, o, stderr) {
		return exitFailure
	}
	if strings.HasPrefix(from, "overseer:") {
		say(stderr, "crew: status: refusing sender '%s' — the overseer posts no status rows; it messages a dispatcher through crew overseer msg\n", from)
		return exitFailure
	}

	switch state {
	case "working", "blocked", "pr_open", "done", "failed", "exited", "reaped", "migrated":
	default:
		say(stderr, "crew: status state must be one of working|blocked|pr_open|done|failed|exited|reaped|migrated (got '%s')\n", state)
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
	switch {
	case state != "migrated" && toHost != "":
		say(stderr, "crew: status: --to-host is only valid with migrated\n")
		return exitFailure
	case state == "migrated" && toHost == "":
		say(stderr, "crew: status: migrated requires --to-host <host>\n")
		return exitFailure
	case state == "migrated" && !hostName.MatchString(toHost):
		say(stderr, "crew: status: invalid --to-host '%s'\n", toHost)
		return exitFailure
	case state == "migrated" && strings.HasPrefix(from, "worker:") && !migrateDirected(c.Log, crew, from, toHost):
		say(stderr, "crew: status: migrated needs a dispatcher migrate directive naming --to-host %s\n", toHost)
		return exitFailure
	}

	// Terminal states are posted once: a re-announced pr_open/done wakes `watch`
	// twice with an identical batch.
	switch state {
	case "pr_open", "done", "failed", "exited", "reaped", "migrated":
		if lastPosted(c.Log, crew, from) == state+"\t"+pr+"\t"+toHost {
			clearRelaunch(ctx, o, from, state)
			return 0
		}
	}

	if undeliveredGate(paths, crew, from, state, stderr) {
		return exitFailure
	}
	g := gate{crew: crew, from: from, state: state, detail: detail, log: c.Log, o: o}
	if line := g.check(ctx); line != "" {
		say(stderr, "%s\n", line)
		return exitFailure
	}

	ts := o.Clock.RealMS()
	code := post("status", c.Log, stderr, func(d string) string {
		return encodeLine(row[statusBody]{
			TS: ts, CrewID: crew, From: from, To: "dispatcher:" + crew, Kind: bus.KindStatus,
			Body: statusBody{State: state, Detail: d, PRURL: pr, Restamp: restamp, ToHost: toHost},
		})
	}, detail)
	if code != 0 {
		return code
	}
	publishPane(ctx, o, state, detail)
	clearRelaunch(ctx, o, from, state)
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

// clearRelaunch drops the tmux-remux relaunch stamp from a finished worker's
// own lead pane so a server restore never resurrects it. The @crew_branch and
// @crew_role check proves the caller sits in that worker's pane: a dispatcher
// process post (another TMUX_PANE) and role panes clear nothing. Watchdog
// terminal rows are written by stall-watch directly and never reach RunStatus,
// so they never clear. pr_open keeps the stamp, since the worker still goes on
// to done.
// Best effort: it must never fail the write that precedes it.
func clearRelaunch(ctx context.Context, o Options, from, state string) {
	if state != "done" && state != "failed" && state != "migrated" {
		return
	}
	pane := o.Sender.Getenv("TMUX_PANE")
	if o.Sender.Getenv("CREW_ROLE_ID") != "" || pane == "" || o.Tmux == nil {
		return
	}
	branch, ok := strings.CutPrefix(branchSession(from), "worker:")
	if !ok || branch == "" {
		return
	}
	out, err := o.Tmux.Run(ctx, "display-message", "-p", "-t", pane, "#{@crew_branch}\x1f#{@crew_role}")
	if err != nil {
		return
	}
	paneBranch, role, _ := strings.Cut(out, "\x1f")
	if paneBranch != branch || (role != "" && role != "lead") {
		return
	}
	for _, option := range []string{"@remux_relaunch", "@remux_relaunch_pinned"} {
		_, _ = o.Tmux.Run(ctx, "set-option", "-pu", "-t", pane, option)
	}
}

// migrateDirected reports whether the newest migrate or migrate-cancel
// directive dispatcher:<crew> sent to exactly from is a migrate naming host.
// Exactly from: crew reply resolves a branch to its newest session, so a
// successor session never inherits a directive.
func migrateDirected(log, crew, from, host string) bool {
	events, _ := bus.ReadEventsTolerant(log)
	directed := false
	for _, ev := range events {
		m, ok := ev.Doc.(map[string]any)
		if !ok || ev.CrewID != crew || ev.Kind != bus.KindMsg || m["from"] != "dispatcher:"+crew || m["to"] != from {
			continue
		}
		body, _ := m["body"].(string)
		v, err := jqrun.FromJSON(body)
		if err != nil {
			continue
		}
		d, _ := v.(map[string]any)
		switch d["directive"] {
		case "migrate":
			directed = d["to_host"] == host
		case "migrate-cancel":
			directed = false
		}
	}
	return directed
}

// lastPosted is the terminal dedupe key, "<state>\t<pr_url>\t<to_host>", of the
// last status row this crew's from posted; "" when there is none. The well-formed
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
		var state, pr, toHost string
		switch body := m["body"].(type) {
		case nil:
			state = "null"
		case map[string]any:
			state = text(body["state"])
			if v := body["pr_url"]; v != nil && v != false {
				pr = text(v)
			}
			if v := body["to_host"]; v != nil && v != false {
				toHost = text(v)
			}
		default:
			continue
		}
		last = state + "\t" + pr + "\t" + toHost
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
