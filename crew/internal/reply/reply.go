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
// The row itself is a plain write through bus.FitLine/bus.Append, and unlike
// `hold`'s it needs no byte-exact construction: `body` is the caller's text, not
// embedded JSON, so a row's key order is not part of any value a reader compares
// (docs/crew-go-port.md's output contract).
package reply

import (
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
	"github.com/noamsto/dispatcher/crew/internal/jsonv"
	"github.com/noamsto/dispatcher/crew/internal/sessions"
)

const (
	exitFailure = 1
	exitType    = 5 // jq's status for a fold that fails on a row it cannot use
)

// Options is everything Run reads beyond the bus: how to resolve the crew the
// arm defaults to (`_crew_id`), the clock the row stamps (`RealMS` is jq's
// `now*1000|floor`; the arm built `ts` with jq's `now`, so even a `CREW_CLOCK`
// run stamps real milliseconds), and the pid probes the cross-crew lookup reads
// through crews.PidAlive.
type Options struct {
	CrewID func() string
	Clock  clock.Clock
	Probes crews.Probes
}

// say writes to a stream whose failure is reported elsewhere (the exit status,
// or nowhere, for stderr) — main.go's helper, same reason.
func say(w io.Writer, format string, args ...any) { _, _ = fmt.Fprintf(w, format, args...) }

// Run is the arm: the `--crew` flag loop, the target, then the append. Nothing
// prints on success, so there is no stdout to write.
func Run(args []string, paths bus.Paths, stderr io.Writer, o Options) int {
	rc, pos, msg := parse(args)
	if msg != "" {
		say(stderr, "%s\n", msg)
		return exitFailure
	}
	to, body := "", ""
	if len(pos) > 0 {
		to = pos[0]
	}
	if len(pos) > 1 {
		body = pos[1]
	}
	crew := rc
	if crew == "" && o.CrewID != nil {
		crew = o.CrewID()
	}

	// The arm's `mkdir -p "$dir"` runs before it resolves anything: a refusal
	// still leaves the bus dir, and the append needs it on a repo with no bus yet.
	if err := os.MkdirAll(paths.Dir, 0o755); err != nil {
		say(stderr, "crew: reply: %v\n", err)
		return exitFailure
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

	// Computed once, before the builder: FitLine calls the builder again for every
	// shrink pass, and jq re-read `now` on each would stamp the row at however
	// long the loop took.
	ts := o.Clock.RealMS()
	line := bus.FitLine(func(b string) string {
		return compact(jsonv.Object(
			jsonv.Member{Key: "ts", Val: tsValue(ts)},
			jsonv.Member{Key: "crew_id", Val: jsonv.Str(crew)},
			jsonv.Member{Key: "from", Val: jsonv.Str("dispatcher:" + crew)},
			jsonv.Member{Key: "to", Val: jsonv.Str(to)},
			jsonv.Member{Key: "kind", Val: jsonv.Str("msg")},
			jsonv.Member{Key: "body", Val: jsonv.Str(b)},
		))
	}, body)
	if err := bus.Append(paths.Log, line); err != nil {
		say(stderr, "crew: reply: %v\n", err)
		return exitFailure
	}
	return 0
}

// parse is the arm's flag loop: `--crew` is taken wherever it appears (the last
// one wins, as each assignment overwrote the last) and everything else is
// positional, in order. msg is the arm's line for a `--crew` with no value —
// including an empty one, which `[ -n "${2:-}" ]` refuses too.
func parse(args []string) (crew string, pos []string, msg string) {
	for len(args) > 0 {
		if args[0] == "--crew" {
			if len(args) < 2 || args[1] == "" {
				return "", nil, "crew: --crew needs a value"
			}
			crew, args = args[1], args[2:]
			continue
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
	return crew, pos, ""
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
	row   jsonv.Value
	crew  string
	alive *bool
}

// resolve is the target resolution: the crew's own newest session when a crew is
// named, else the single crew with one that is not known-dead. It returns the
// session row to address and the crew to post as — the second only the cross-crew
// path can supply — with a non-zero code when it printed a refusal or a fold
// failure.
func resolve(branch, crew string, events []bus.Event, paths bus.Paths, stderr io.Writer, o Options) (jsonv.Value, string, int) {
	if crew != "" {
		newest, code := newestSession(events, branch, crew, paths.Log, stderr, o)
		if code != 0 {
			return jsonv.Value{}, "", code
		}
		if ok, code := refuse(newest, branch, stderr); !ok {
			return jsonv.Value{}, crew, code
		}
		return newest, crew, 0
	}

	// Every crew the bus carries, in the byte order `sort -u` produced (the
	// documented locale-free choice, as `crews` made it).
	var cands []candidate
	for _, c := range crewIDs(events) {
		newest, code := newestSession(events, branch, c, paths.Log, stderr, o)
		if code != 0 {
			return jsonv.Value{}, "", code
		}
		// `last // empty | select(.terminal | not)`: a crew whose newest session
		// on the branch is terminal is no candidate at all.
		if newest.IsNull() || text(newest, "terminal") == "true" {
			continue
		}
		pidfile := paths.CrewDir(c) + "/pid"
		pid, _ := crews.PidFileText(pidfile)
		cands = append(cands, candidate{
			row: newest, crew: c,
			alive: crews.PidAlive(o.Probes, o.Clock.Now(), pidfile, pid),
		})
	}
	// Drop known-dead crews only while a not-known-dead candidate remains. If
	// every candidate's crew is dead, keep them all — one still delivers, several
	// still refuse naming both, exactly as before #327.
	if slices.ContainsFunc(cands, func(c candidate) bool { return c.alive == nil || *c.alive }) {
		cands = slices.DeleteFunc(slices.Clone(cands), func(c candidate) bool {
			return c.alive != nil && !*c.alive
		})
	}
	switch len(cands) {
	case 0:
		say(stderr, "crew: CREW_ID not set and no live session on %s in any crew — pass --crew <id> (or dispatch a worker before replying to one)\n", branch)
		return jsonv.Value{}, "", exitFailure
	case 1:
		if ok, code := refuse(cands[0].row, branch, stderr); !ok {
			return jsonv.Value{}, cands[0].crew, code
		}
		return cands[0].row, cands[0].crew, 0
	default:
		names := make([]string, len(cands))
		for i, c := range cands {
			names[i] = c.crew
		}
		say(stderr, "crew: CREW_ID not set and %s has live sessions in crews: %s — pass --crew <id>\n", branch, strings.Join(names, ", "))
		return jsonv.Value{}, "", exitFailure
	}
}

// newestSession is `_sessions <branch> <crew> | jq -c 'last'`: the newest session
// row, or null when the branch has none for that crew. A fold that fails on this
// bus is the arm's jq failure — one line naming the log, and jq's status.
func newestSession(events []bus.Event, branch, crew, log string, stderr io.Writer, o Options) (jsonv.Value, int) {
	raws := make([]jsonv.Value, len(events))
	for i, ev := range events {
		raws[i] = ev.Raw
	}
	// age_s is the one field of the fold this arm never reads, so the fold's `now`
	// is whatever the clock says rather than a value any output carries.
	out, err := sessions.Fold(raws, branch, crew, float64(o.Clock.RealMS())/1000)
	if err != nil {
		say(stderr, "crew: reply: %s: %v\n", log, err)
		return jsonv.Value{}, exitType
	}
	if out.Kind() != jsonv.KindArray || out.Len() == 0 {
		return jsonv.Null(), 0
	}
	return out.Elems()[out.Len()-1], 0
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
func refuse(newest jsonv.Value, branch string, stderr io.Writer) (bool, int) {
	if newest.IsNull() {
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

// text is `jq -r .<key>` on the row: a string verbatim, a number as jq prints it,
// a boolean as `true`/`false` (which is how the arm compares `.terminal`), and
// null or a missing key as the four letters the arm compared. (An array or object
// here would have cost jq a type error; the fold only ever puts the first three.)
func text(v jsonv.Value, key string) string {
	x, _ := v.Get(key)
	switch x.Kind() {
	case jsonv.KindString:
		s, _ := x.AsString()
		return s
	case jsonv.KindNumber:
		return x.NumberText()
	case jsonv.KindTrue:
		return "true"
	case jsonv.KindFalse:
		return "false"
	case jsonv.KindNull, jsonv.KindArray, jsonv.KindObject:
	}
	return "null"
}

// tsValue is jq's `now*1000|floor` as the number the arm's jq wrote: the digits
// of a whole-millisecond stamp, never an exponent.
func tsValue(ms int64) jsonv.Value {
	v, ok := jsonv.ParseNumber(strconv.FormatInt(ms, 10))
	if !ok {
		return jsonv.Num(float64(ms))
	}
	return v
}

func compact(v jsonv.Value) string { return string(jsonv.Append(nil, v, jsonv.Options{})) }
