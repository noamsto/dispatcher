// Package reply is `crew reply`: the dispatcher's in-band answer, sugar over a
// `msg` whose `from` is `dispatcher:<crew>` so the dispatcher never rebuilds its
// own id.
//
// Its one piece of real logic is that a branch-only `worker:<branch>` target
// resolves, at send time, to the newest session on that branch — which is what
// makes a directive uninheritable: a stopped session's successor has a different
// id, so a message written for the former is never addressed to the latter (#17).
// With no crew named the lookup widens across every crew on the bus and keeps
// the one whose registered dispatcher pid is not known-dead (#327, #302).
//
// The row itself is a plain write through bus.FitLine/bus.Append: `body` is the
// caller's text rather than embedded JSON, so a row's key order is not part of
// any value a reader compares (docs/crew-go-port.md's output contract).
package reply

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/clock"
	"github.com/noamsto/dispatcher/crew/internal/crews"
	"github.com/noamsto/dispatcher/crew/internal/remote"
	"github.com/noamsto/dispatcher/crew/internal/sender"
	"github.com/noamsto/dispatcher/crew/internal/sessions"
)

const (
	exitFailure = 1
	exitType    = 5 // jq's status for a fold that fails on a row it cannot use
)

// Options is everything Run reads beyond the bus: how to resolve the crew the
// arm defaults to (`_crew_id`), the clock the row stamps — `RealMS`, jq's
// `now*1000|floor`, which stays real time under `CREW_CLOCK` — and the pid probes
// the cross-crew lookup reads through crews.PidAlive.
type Options struct {
	CrewID func() string
	Clock  clock.Clock
	Probes crews.Probes
	Sender sender.Probes // the sender check's reads; zero checks nothing
	// Remote runs argv on host as crew and returns its status. It is how a
	// reply to a worker dispatched there reaches that host's bus; nil routes
	// nothing.
	Remote func(host, crew string, argv []string) int
}

// say writes to a stream whose failure is reported elsewhere (the exit status,
// or nowhere, for stderr) — main.go's helper, same reason.
func say(w io.Writer, format string, args ...any) { _, _ = fmt.Fprintf(w, format, args...) }

// Request is `crew reply [--crew ID] <to> <body>`; an empty Crew is the caller's own.
type Request struct{ Crew, To, Body string }

// Run is the arm: the target, then the append. Nothing prints on success, so
// there is no stdout to write.
func Run(req Request, paths bus.Paths, stderr io.Writer, o Options) int {
	to, body := req.To, req.Body
	crew := req.Crew
	if crew == "" && o.CrewID != nil {
		crew = o.CrewID()
	}

	// The arm's `mkdir -p "$dir"` runs before it resolves anything: a refusal
	// still leaves the bus dir, and the append needs it on a repo with no bus yet.
	if err := os.MkdirAll(paths.Dir, 0o755); err != nil {
		say(stderr, "crew: reply: %v\n", err)
		return exitFailure
	}

	// Refused before any bus read, so a worker's reply is refused rather than resolved.
	if reason := sender.Check("dispatcher:"+crew, crew, o.Sender); reason != "" {
		say(stderr, "crew: reply: refusing sender 'dispatcher:%s' — %s\n", crew, reason)
		return exitFailure
	}

	// The remote writes the row to its own bus, where the worker reads it. The
	// `--` keeps a body that starts with a dash from reading there as a flag.
	host, as, err := routeHost(paths, crew, to, o)
	if err != nil {
		say(stderr, "crew: reply: CREW_ID not set and %v\n", err)
		return exitFailure
	}
	if host != "" {
		return o.Remote(host, as, []string{"crew", "reply", "--crew", as, "--", to, body})
	}

	if strings.HasPrefix(to, "worker:") && !bus.IsSessionID(to) {
		branch := strings.TrimPrefix(to, "worker:")
		events, code := read(paths, stderr)
		if code != 0 {
			return code
		}
		newest, resolved, code := resolve(branch, crew, events, paths, stderr, o)
		if code != 0 {
			return code
		}
		crew = resolved
		to = text(newest, "worker_id")
	}
	if crew == "" {
		say(stderr, "crew: CREW_ID not set and no WORKER_TASK.md crew_id — pass --crew <id>\n")
		return exitFailure
	}

	// Computed once, before the builder: FitLine runs it again for every shrink
	// pass, and a stamp taken inside would move with the loop.
	ts := o.Clock.RealMS()
	line := bus.FitLine(func(b string) string {
		return compact(msgRow{
			TS: ts, CrewID: crew, From: "dispatcher:" + crew, To: to, Kind: "msg", Body: b,
		})
	}, body)
	if err := bus.Append(paths.Log, line); err != nil {
		say(stderr, "crew: reply: %v\n", err)
		return exitFailure
	}
	return 0
}

// routeHost is the host a reply to a worker goes to, the one its branch was
// dispatched on, and the crew it posts there as; host is "" for a local worker.
// With no crew named, the crew is the one the remote dispatch row names. The
// bus is read tolerantly, so a bus the local path would refuse routes nothing
// and the local path refuses it.
func routeHost(paths bus.Paths, crew, to string, o Options) (host, as string, err error) {
	branch, ok := strings.CutPrefix(to, "worker:")
	if o.Remote == nil || !ok {
		return "", "", nil
	}
	// The local path's own reading of a session id: a branch named like one,
	// with no '#', is still a branch.
	if bus.IsSessionID(to) {
		branch = branch[:strings.LastIndex(branch, "#")]
	}
	events, _ := bus.ReadEventsTolerant(paths.Log)
	if crew == "" {
		return remote.RouteAny(events, branch)
	}
	return remote.Route(events, branch, crew), crew, nil
}

// msgRow is the msg the arm appends; body stays last, where FitLine's shrink marker lands.
type msgRow struct {
	TS     int64  `json:"ts"`
	CrewID string `json:"crew_id"`
	From   string `json:"from"`
	To     string `json:"to"`
	Kind   string `json:"kind"`
	Body   string `json:"body"`
}

// read is the log `_sessions` reads: `jq -s`, so no torn-tail tolerance, and
// where the arm's `[ -f "$log" ]` misses, no rows at all — the fold then answers
// `[]` and the caller refuses for want of a session.
func read(paths bus.Paths, stderr io.Writer) ([]bus.Event, int) {
	events, err := bus.ReadEvents(paths.Log)
	if errors.Is(err, bus.ErrNoLog) {
		return nil, 0
	}
	if err != nil {
		msg, code := bus.JQFailure(err)
		say(stderr, "crew: reply: %s: %s\n", paths.Log, msg)
		return nil, code
	}
	return events, 0
}

// candidate is one crew's newest non-terminal session on the branch, with the
// three-state liveness of that crew's registered pid.
type candidate struct {
	row   any
	crew  string
	alive *bool
}

// resolve is the target resolution: the crew's own newest session when a crew is
// named, else the single crew with one that is not known-dead. It returns the
// session row to address and the crew to post as — the second only the cross-crew
// path can supply — with a non-zero code when it printed a refusal or a fold
// failure.
func resolve(branch, crew string, events []bus.Event, paths bus.Paths, stderr io.Writer, o Options) (any, string, int) {
	if crew != "" {
		newest, code := newestSession(events, branch, crew, paths.Log, stderr, o)
		if code != 0 {
			return nil, "", code
		}
		if ok, code := refuse(newest, branch, stderr); !ok {
			return nil, crew, code
		}
		return newest, crew, 0
	}

	// Every crew the bus carries, in the byte order `sort -u` produced (the
	// documented locale-free choice, as `crews` made it).
	var cands []candidate
	for _, c := range crewIDs(events) {
		newest, code := newestSession(events, branch, c, paths.Log, stderr, o)
		if code != 0 {
			return nil, "", code
		}
		// `last // empty | select(.terminal | not)`: a crew whose newest session
		// on the branch is terminal is no candidate at all.
		if newest == nil || text(newest, "terminal") == "true" {
			continue
		}
		pidfile := paths.CrewDir(c) + "/pid"
		pid, _ := crews.PidFileText(pidfile)
		cands = append(cands, candidate{
			row: newest, crew: c,
			alive: crews.PidAlive(o.Probes, o.Clock.Now(), pidfile, pid),
		})
	}
	// Drop known-dead crews only while a not-known-dead candidate remains. If every
	// candidate's crew is dead, keep them all: one still delivers, several still
	// refuse naming both.
	if slices.ContainsFunc(cands, func(c candidate) bool { return c.alive == nil || *c.alive }) {
		cands = slices.DeleteFunc(cands, func(c candidate) bool {
			return c.alive != nil && !*c.alive
		})
	}
	switch len(cands) {
	case 0:
		say(stderr, "crew: CREW_ID not set and no live session on %s in any crew — pass --crew <id> (or dispatch a worker before replying to one)\n", branch)
		return nil, "", exitFailure
	case 1:
		if ok, code := refuse(cands[0].row, branch, stderr); !ok {
			return nil, cands[0].crew, code
		}
		return cands[0].row, cands[0].crew, 0
	default:
		names := make([]string, len(cands))
		for i, c := range cands {
			names[i] = c.crew
		}
		say(stderr, "crew: CREW_ID not set and %s has live sessions in crews: %s — pass --crew <id>\n", branch, strings.Join(names, ", "))
		return nil, "", exitFailure
	}
}

// newestSession is `_sessions <branch> <crew> | jq -c 'last'`: the newest session
// row, or null when the branch has none for that crew. A fold that fails on this
// bus is the arm's jq failure — one line naming the log, and jq's status.
func newestSession(events []bus.Event, branch, crew, log string, stderr io.Writer, o Options) (any, int) {
	docs := make([]any, len(events))
	for i, ev := range events {
		docs[i] = ev.Doc
	}
	// age_s is the one field of the fold this arm never reads, so the fold's `now`
	// is whatever the clock says rather than a value any output carries.
	folded, err := sessions.Fold(docs, branch, crew, float64(o.Clock.RealMS())/1000)
	if err != nil {
		say(stderr, "crew: reply: %s: %v\n", log, err)
		return nil, exitType
	}
	out, _ := folded.([]any)
	if len(out) == 0 {
		return nil, 0
	}
	return out[len(out)-1], 0
}

// crewIDs is `jq -r 'select(.crew_id != null) | .crew_id' | sort -u`, minus the
// empty id the loop's `[ -n "$c" ]` skipped.
func crewIDs(events []bus.Event) []string {
	var ids []string
	for _, ev := range events {
		if ev.CrewID != "" {
			ids = append(ids, ev.CrewID)
		}
	}
	slices.Sort(ids)
	return slices.Compact(ids)
}

// refuse is the arm's three shared checks on the resolved row, in its order: no
// session at all, a terminal newest, a newest with no session id to address. ok
// is true when none of them fired.
func refuse(newest any, branch string, stderr io.Writer) (bool, int) {
	if newest == nil {
		say(stderr, "crew: no session on %s — dispatch a worker before replying to one\n", branch)
		return false, exitFailure
	}
	if text(newest, "terminal") == "true" {
		say(stderr, "crew: newest session on %s is %s — a stopped session never reads its inbox; re-dispatch with the context baked in\n", branch, text(newest, "state"))
		return false, exitFailure
	}
	if text(newest, "session") == "null" {
		say(stderr, "crew: %s has no session id on the bus — a branch-only address can never reach a live worker's inbox; re-dispatch\n", branch)
		return false, exitFailure
	}
	return true, 0
}

// text is `jq -r .<key>` on the row: a string bare, a number as jq prints it, a
// boolean as `true`/`false` (how the arm compares `.terminal`), null or a missing
// key as the four letters the arm compared, and an array or object in jq's pretty
// form — `-r` passes only strings through bare, and `.state` is read straight off
// the bus, so a row carrying a container prints the container.
func text(v any, key string) string {
	m, _ := v.(map[string]any)
	switch x := m[key].(type) {
	case string:
		return x
	case json.Number:
		return x.String()
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	case []any, map[string]any:
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		_ = enc.Encode(x)
		return strings.TrimSuffix(buf.String(), "\n")
	}
	return "null"
}

func compact(r msgRow) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(r)
	return strings.TrimSuffix(buf.String(), "\n")
}
