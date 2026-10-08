// Package sessions folds a crew bus into per-session rows. It is a literal
// port of crew.sh's `_sessions` jq program: every step mirrors one jq
// operation, including the points where jq raises a type error.
package sessions

import (
	"math"
	"regexp"
	"slices"
	"strings"

	"github.com/noamsto/dispatcher/crew/internal/jsonv"
)

var terminalStates = jsonv.Array(jsonv.Str("done"), jsonv.Str("failed"), jsonv.Str("exited"))

// sessionSuffix is jq's `capture("#(?<s>s[0-9]+-[0-9]+)$")`. Oniguruma's `$`
// also matches before one trailing newline, so the newline is allowed here.
var sessionSuffix = regexp.MustCompile(`#(s[0-9]+-[0-9]+)\n?\z`)

// Fold is `_sessions <branch> <crew>`: one row per session of branch, oldest
// first, with age_s computed against nowSec (jq's `now`). An empty crew keeps
// every crew. A jq runtime error is returned as a *jsonv.TypeError.
func Fold(events []jsonv.Value, branch, crew string, nowSec float64) (jsonv.Value, error) {
	b := jsonv.Str(branch)
	bs, _ := b.AsString()

	evs, err := inCrew(events, jsonv.Str(crew))
	if err != nil {
		return jsonv.Value{}, err
	}
	disp, err := starts(evs, b)
	if err != nil {
		return jsonv.Value{}, err
	}
	raw, err := workerStatuses(evs, bs)
	if err != nil {
		return jsonv.Value{}, err
	}
	sessionStarts, err := firstTS(slices.Concat(disp, raw))
	if err != nil {
		return jsonv.Value{}, err
	}
	sessioned := sessionedRows(raw)

	st := make([]jsonv.Value, 0, len(raw))
	for _, r := range raw {
		if !get(r, "session").IsNull() {
			st = append(st, r)
			continue
		}
		adopted, err := adopt(r, sessionStarts, sessioned)
		if err != nil {
			return jsonv.Value{}, err
		}
		st = append(st, adopted)
	}

	ids := jsonv.Unique(column(slices.Concat(disp, st), "session"))
	rows := make([]jsonv.Value, 0, len(ids))
	for _, s := range ids {
		row, err := sessionRow(s, bs, st, disp)
		if err != nil {
			return jsonv.Value{}, err
		}
		rows = append(rows, row)
	}
	rows, err = jsonv.SortBy(rows, field("ts"))
	if err != nil {
		return jsonv.Value{}, err
	}
	for i, row := range rows {
		rows[i], err = withAge(row, nowSec)
		if err != nil {
			return jsonv.Value{}, err
		}
	}
	return jsonv.Array(rows...), nil
}

// inCrew is `map(select($crew=="" or .crew_id==$crew))`.
func inCrew(events []jsonv.Value, crew jsonv.Value) ([]jsonv.Value, error) {
	if s, _ := crew.AsString(); s == "" {
		return events, nil
	}
	var out []jsonv.Value
	for _, e := range events {
		id, err := e.Index("crew_id")
		if err != nil {
			return nil, err
		}
		if id.Equal(crew) {
			out = append(out, e)
		}
	}
	return out, nil
}

// starts is $disp: the `{session, ts}` of every dispatch or resume event for
// the branch.
func starts(events []jsonv.Value, branch jsonv.Value) ([]jsonv.Value, error) {
	var out []jsonv.Value
	for _, e := range events {
		kind, err := e.Index("kind")
		if err != nil {
			return nil, err
		}
		if !kind.Equal(jsonv.Str("dispatch")) && !kind.Equal(jsonv.Str("resume")) {
			continue
		}
		br, err := e.Index("branch")
		if err != nil {
			return nil, err
		}
		if !br.Equal(branch) {
			continue
		}
		session, err := e.Index("session")
		if err != nil {
			return nil, err
		}
		ts, err := e.Index("ts")
		if err != nil {
			return nil, err
		}
		out = append(out, jsonv.Object(
			jsonv.Member{Key: "session", Val: session.Or(jsonv.Null())},
			jsonv.Member{Key: "ts", Val: ts},
		))
	}
	return out, nil
}

// workerStatuses is $raw: the `{session, state, ts}` of every worker status
// event whose `from` folds onto the branch.
func workerStatuses(events []jsonv.Value, branch string) ([]jsonv.Value, error) {
	var out []jsonv.Value
	for _, e := range events {
		kind, err := e.Index("kind")
		if err != nil {
			return nil, err
		}
		if !kind.Equal(jsonv.Str("status")) {
			continue
		}
		from, err := e.Index("from")
		if err != nil {
			return nil, err
		}
		fs, ok := from.Or(jsonv.Str("")).AsString()
		if !ok {
			return nil, jsonv.TypeErrorf("startswith() requires string inputs")
		}
		if !strings.HasPrefix(fs, "worker:") {
			continue
		}
		wbranch, session := splitWID(fs)
		if wbranch != branch {
			continue
		}
		body, err := e.Index("body")
		if err != nil {
			return nil, err
		}
		state, err := body.Index("state")
		if err != nil {
			return nil, err
		}
		ts, err := e.Index("ts")
		if err != nil {
			return nil, err
		}
		out = append(out, row(session, state, ts))
	}
	return out, nil
}

// splitWID is jq's split_wid on a `worker:`-prefixed id. A trailing newline
// after the session survives in the branch, as rtrimstr does not match it.
func splitWID(from string) (branch string, session jsonv.Value) {
	rest := strings.TrimPrefix(from, "worker:")
	m := sessionSuffix.FindStringSubmatch(rest)
	if m == nil {
		return rest, jsonv.Null()
	}
	return strings.TrimSuffix(rest, "#"+m[1]), jsonv.Str(m[1])
}

// firstTS is $starts: each named session with the earliest ts it has been seen at.
func firstTS(rows []jsonv.Value) ([]jsonv.Value, error) {
	groups, err := jsonv.GroupBy(sessionedRows(rows), field("session"))
	if err != nil {
		return nil, err
	}
	out := make([]jsonv.Value, 0, len(groups))
	for _, g := range groups {
		first, err := jsonv.MinBy(column(g, "ts"), identity)
		if err != nil {
			return nil, err
		}
		out = append(out, jsonv.Object(
			jsonv.Member{Key: "session", Val: get(g[0], "session")},
			jsonv.Member{Key: "start", Val: first},
		))
	}
	return out, nil
}

// sessionedRows is `map(select(.session != null))`.
func sessionedRows(rows []jsonv.Value) []jsonv.Value {
	return slices.DeleteFunc(slices.Clone(rows), func(r jsonv.Value) bool {
		return get(r, "session").IsNull()
	})
}

// adopt gives a session-less status row to the latest session started by its
// ts, unless that session's own last word was terminal.
func adopt(r jsonv.Value, sessionStarts, sessioned []jsonv.Value) (jsonv.Value, error) {
	t := get(r, "ts")
	started := slices.DeleteFunc(slices.Clone(sessionStarts), func(s jsonv.Value) bool {
		return jsonv.Compare(get(s, "start"), t) > 0
	})
	latestStart, err := jsonv.MaxBy(started, field("start"))
	if err != nil {
		return jsonv.Value{}, err
	}
	s := get(latestStart, "session")

	own := slices.DeleteFunc(slices.Clone(sessioned), func(x jsonv.Value) bool {
		return !get(x, "session").Equal(s) || jsonv.Compare(get(x, "ts"), t) > 0
	})
	last, err := jsonv.MaxBy(own, field("ts"))
	if err != nil {
		return jsonv.Value{}, err
	}
	if isTerminal(get(last, "state")) {
		return r, nil
	}
	return row(s, get(r, "state"), t), nil
}

// sessionRow is one element of the `[ $ids[] as $s | ... ]` array.
func sessionRow(s jsonv.Value, branch string, st, disp []jsonv.Value) (jsonv.Value, error) {
	latest, err := lastByTS(st, s)
	if err != nil {
		return jsonv.Value{}, err
	}
	d, err := lastByTS(disp, s)
	if err != nil {
		return jsonv.Value{}, err
	}
	workerID := "worker:" + branch
	if !s.IsNull() {
		ss, ok := s.AsString()
		if !ok {
			return jsonv.Value{}, jsonv.TypeErrorf("string (\"#\") and %s cannot be added", s.Kind())
		}
		workerID += "#" + ss
	}
	state := get(latest, "state")
	return jsonv.Object(
		jsonv.Member{Key: "session", Val: s},
		jsonv.Member{Key: "worker_id", Val: jsonv.Str(workerID)},
		jsonv.Member{Key: "state", Val: state.Or(jsonv.Null())},
		jsonv.Member{Key: "ts", Val: get(latest, "ts").Or(get(d, "ts"))},
		jsonv.Member{Key: "terminal", Val: jsonv.Bool(isTerminal(state))},
	), nil
}

// lastByTS is `map(select(.session == $s)) | sort_by(.ts) | last`.
func lastByTS(rows []jsonv.Value, session jsonv.Value) (jsonv.Value, error) {
	mine := slices.DeleteFunc(slices.Clone(rows), func(r jsonv.Value) bool {
		return !get(r, "session").Equal(session)
	})
	sorted, err := jsonv.SortBy(mine, field("ts"))
	if err != nil || len(sorted) == 0 {
		return jsonv.Null(), err
	}
	return sorted[len(sorted)-1], nil
}

// withAge is `. + {age_s: (((now*1000) - .ts) / 1000 | floor)}`.
func withAge(r jsonv.Value, nowSec float64) (jsonv.Value, error) {
	ts := get(r, "ts")
	f, ok := ts.AsFloat()
	if !ok {
		return jsonv.Value{}, jsonv.TypeErrorf("number and %s cannot be subtracted", ts.Kind())
	}
	// The conversion forbids fusing the multiply into the subtraction.
	ageS := math.Floor((float64(nowSec*1000) - f) / 1000)
	out := jsonv.Object(r.Members()...)
	out.Set("age_s", jsonv.Num(ageS))
	return out, nil
}

// isTerminal is `(["done","failed","exited"] | index($x // "")) != null`, with
// jq's index semantics: a string is a membership test, an array a contiguous
// subarray search (an empty one never matches) and anything else is no match.
func isTerminal(x jsonv.Value) bool {
	needle := x.Or(jsonv.Str(""))
	states := terminalStates.Elems()
	switch needle.Kind() {
	case jsonv.KindString:
		return slices.ContainsFunc(states, needle.Equal)
	case jsonv.KindArray:
		sub := needle.Elems()
		if len(sub) == 0 {
			return false
		}
		for i := range states {
			if slices.EqualFunc(states[i:min(i+len(sub), len(states))], sub, jsonv.Value.Equal) {
				return true
			}
		}
	case jsonv.KindNull, jsonv.KindFalse, jsonv.KindTrue, jsonv.KindNumber, jsonv.KindObject:
	}
	return false
}

func row(session, state, ts jsonv.Value) jsonv.Value {
	return jsonv.Object(
		jsonv.Member{Key: "session", Val: session},
		jsonv.Member{Key: "state", Val: state},
		jsonv.Member{Key: "ts", Val: ts},
	)
}

// get is `.key` on a row this package built, which is always an object.
func get(v jsonv.Value, key string) jsonv.Value {
	x, _ := v.Get(key)
	return x
}

func column(rows []jsonv.Value, key string) []jsonv.Value {
	out := make([]jsonv.Value, len(rows))
	for i, r := range rows {
		out[i] = get(r, key)
	}
	return out
}

func field(key string) func(jsonv.Value) (jsonv.Value, error) {
	return func(v jsonv.Value) (jsonv.Value, error) { return v.Index(key) }
}

func identity(v jsonv.Value) (jsonv.Value, error) { return v, nil }
