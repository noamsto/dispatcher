// Package roster folds a crew bus into one row per branch. It is a literal
// port of the `roster)` arm in adapters/core/crew.sh: every step mirrors one
// jq operation or shell step, including the points where jq raises a type
// error and the points where bash's word splitting and `$(...)` change data.
package roster

import (
	"bytes"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strings"

	"github.com/noamsto/dispatcher/crew/internal/identity"
	"github.com/noamsto/dispatcher/crew/internal/jsonv"
)

// EngineCommands are the pane commands `_is_engine_cmd` accepts once a leading
// "." and a trailing "-wrapped" are stripped.
var EngineCommands = []string{"claude", "codex", "cursor-agent", "node", "pi"}

// ExitError is a failed external command: its exit status and stderr.
type ExitError struct {
	Code   int
	Stderr string
}

func (e *ExitError) Error() string { return fmt.Sprintf("exit status %d: %s", e.Code, e.Stderr) }

// Probes are the two external reads the arm makes, injected so a fold is a
// pure function of the bus.
type Probes struct {
	// Panes is the stdout of `tmux list-panes -a -F '#{pane_current_command}
	// #{pane_current_path}'`, or "" when tmux fails.
	Panes func() string
	// Worktrees is the stdout of `git worktree list --porcelain`. A failure
	// is an *ExitError: the arm dies with git's status.
	Worktrees func() (string, error)
}

// detailRunes is the codepoint cap on `detail`.
const detailRunes = 120

// idSuffix is jq's `capture("(?:[a-z]+/)?(?<id>[A-Za-z]+-[0-9]+|[0-9]+)")`.
var idSuffix = regexp.MustCompile(`(?:[a-z]+/)?([A-Za-z]+-[0-9]+|[0-9]+)`)

// Fold is `crew roster <crew>` once the log exists: the rows as jq would
// print them, with age_s computed against nowSec (jq's `now`). A jq runtime
// error is returned as a *jsonv.TypeError, a failing worktree probe as its
// *ExitError.
func Fold(events []jsonv.Value, crew string, nowSec float64, p Probes) (jsonv.Value, error) {
	rows, err := base(events, crew, nowSec)
	if err != nil {
		return jsonv.Value{}, err
	}
	if rows, err = reread(rows); err != nil {
		return jsonv.Value{}, err
	}
	// $(...) drops trailing newlines, so the here-doc over $live is exactly
	// these lines.
	live := strings.Split(strings.TrimRight(p.Panes(), "\n"), "\n")
	for i := range rows {
		if err := resolveExited(&rows[i], live, p.Worktrees); err != nil {
			return jsonv.Value{}, err
		}
	}
	return jsonv.Array(withIdentity(events, rows)...), nil
}

func field(name string) func(jsonv.Value) (jsonv.Value, error) {
	return func(v jsonv.Value) (jsonv.Value, error) { return v.Index(name) }
}

// at is jq's `.a.b.c`.
func at(v jsonv.Value, keys ...string) (jsonv.Value, error) {
	for _, k := range keys {
		var err error
		if v, err = v.Index(k); err != nil {
			return jsonv.Value{}, err
		}
	}
	return v, nil
}

// withDefault is jq's `(.k // null)` over a member of v.
func withDefault(v jsonv.Value, key string) (jsonv.Value, error) {
	x, err := v.Index(key)
	return x.Or(jsonv.Null()), err
}

// inCrew is `map(select(.crew_id==$crew and match))`.
func inCrew(events []jsonv.Value, crew string, match func(ev jsonv.Value) (bool, error)) ([]jsonv.Value, error) {
	want := jsonv.Str(crew)
	var out []jsonv.Value
	for _, ev := range events {
		id, err := ev.Index("crew_id")
		if err != nil {
			return nil, err
		}
		if !id.Equal(want) {
			continue
		}
		ok, err := match(ev)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, ev)
		}
	}
	return out, nil
}

func kindIs(kinds ...string) func(jsonv.Value) (bool, error) {
	return func(ev jsonv.Value) (bool, error) {
		k, err := ev.Index("kind")
		if err != nil {
			return false, err
		}
		return slices.ContainsFunc(kinds, func(s string) bool { return k.Equal(jsonv.Str(s)) }), nil
	}
}

// objectKey is what `from_entries` accepts as a key in jq 1.8.2: strings only.
func objectKey(v jsonv.Value) (string, error) {
	s, ok := v.AsString()
	if !ok {
		return "", jsonv.TypeErrorf("Cannot use %s as object key", v.Kind())
	}
	return s, nil
}

// base is the first jq program: one row per branch, already collapsed over
// its sessions.
func base(events []jsonv.Value, crew string, nowSec float64) ([]jsonv.Value, error) {
	dispatch, err := dispatchTable(events, crew)
	if err != nil {
		return nil, err
	}
	esess, err := engineSessions(events, crew)
	if err != nil {
		return nil, err
	}
	statuses, err := inCrew(events, crew, func(ev jsonv.Value) (bool, error) {
		if ok, err := kindIs("status")(ev); !ok || err != nil {
			return false, err
		}
		from, err := withDefault(ev, "from")
		if err != nil {
			return false, err
		}
		s, ok := from.Or(jsonv.Str("")).AsString()
		if !ok {
			return false, jsonv.TypeErrorf("startswith() requires string inputs")
		}
		return strings.HasPrefix(s, "worker:"), nil
	})
	if err != nil {
		return nil, err
	}
	groups, err := jsonv.GroupBy(statuses, field("from"))
	if err != nil {
		return nil, err
	}
	sessions := make([]jsonv.Value, len(groups))
	for i, g := range groups {
		if sessions[i], err = sessionRow(g, nowSec); err != nil {
			return nil, err
		}
	}
	return collapse(sessions, dispatch, esess)
}

// dispatchTable is $dispatch: title, engine, model and tier per branch, the
// last dispatch event winning.
func dispatchTable(events []jsonv.Value, crew string) (jsonv.Value, error) {
	evs, err := inCrew(events, crew, kindIs("dispatch"))
	if err != nil {
		return jsonv.Value{}, err
	}
	table := jsonv.Object()
	for _, ev := range evs {
		br, err := ev.Index("branch")
		if err != nil {
			return jsonv.Value{}, err
		}
		entry := jsonv.Object()
		for _, k := range []string{"title", "engine", "model", "tier"} {
			v, err := withDefault(ev, k)
			if err != nil {
				return jsonv.Value{}, err
			}
			entry.Set(k, v)
		}
		key, err := objectKey(br)
		if err != nil {
			return jsonv.Value{}, err
		}
		table.Set(key, entry)
	}
	return table, nil
}

// engineSessions is $esess: the newest dispatch or resume engine_session per branch.
func engineSessions(events []jsonv.Value, crew string) (jsonv.Value, error) {
	evs, err := inCrew(events, crew, kindIs("dispatch", "resume"))
	if err != nil {
		return jsonv.Value{}, err
	}
	groups, err := jsonv.GroupBy(evs, field("branch"))
	if err != nil {
		return jsonv.Value{}, err
	}
	table := jsonv.Object()
	for _, g := range groups {
		br, err := g[0].Index("branch")
		if err != nil {
			return jsonv.Value{}, err
		}
		sorted, err := jsonv.SortBy(g, field("ts"))
		if err != nil {
			return jsonv.Value{}, err
		}
		es, err := withDefault(sorted[len(sorted)-1], "engine_session")
		if err != nil {
			return jsonv.Value{}, err
		}
		key, err := objectKey(br)
		if err != nil {
			return jsonv.Value{}, err
		}
		table.Set(key, es)
	}
	return table, nil
}

// sessionRow is the per-`from` object of the first group_by.
func sessionRow(g []jsonv.Value, nowSec float64) (jsonv.Value, error) {
	latest, err := jsonv.MaxBy(g, field("ts"))
	if err != nil {
		return jsonv.Value{}, err
	}
	from, err := latest.Index("from")
	if err != nil {
		return jsonv.Value{}, err
	}
	id, _ := from.AsString()
	branch, session := splitWorkerID(id)

	state, err := at(latest, "body", "state")
	if err != nil {
		return jsonv.Value{}, err
	}
	detail, err := detailOf(latest)
	if err != nil {
		return jsonv.Value{}, err
	}
	source, err := at(latest, "body", "source")
	if err != nil {
		return jsonv.Value{}, err
	}
	ts, err := latest.Index("ts")
	if err != nil {
		return jsonv.Value{}, err
	}
	prURL, err := lastPRURL(g)
	if err != nil {
		return jsonv.Value{}, err
	}
	prevState, err := prevStateOf(g)
	if err != nil {
		return jsonv.Value{}, err
	}
	age, err := ageSeconds(ts, nowSec)
	if err != nil {
		return jsonv.Value{}, err
	}
	return jsonv.Object(
		jsonv.Member{Key: "from", Val: from},
		jsonv.Member{Key: "branch", Val: jsonv.Str(branch)},
		jsonv.Member{Key: "session", Val: session},
		jsonv.Member{Key: "state", Val: state},
		jsonv.Member{Key: "detail", Val: detail},
		jsonv.Member{Key: "source", Val: source.Or(jsonv.Null())},
		jsonv.Member{Key: "ts", Val: ts},
		jsonv.Member{Key: "pr_url", Val: prURL},
		jsonv.Member{Key: "prev_state", Val: prevState},
		jsonv.Member{Key: "age_s", Val: age},
	), nil
}

// splitWorkerID is wid_branch and wid_session on a `worker:` id: the branch
// drops everything from the last `#`, the session is what follows it.
func splitWorkerID(id string) (branch string, session jsonv.Value) {
	id = strings.TrimPrefix(id, "worker:")
	i := strings.LastIndex(id, "#")
	if i < 0 {
		return id, jsonv.Null()
	}
	return id[:i], jsonv.Str(id[i+1:])
}

// detailOf is `($latest.body.detail // null) | if . == null then null else .[0:120] end`.
func detailOf(latest jsonv.Value) (jsonv.Value, error) {
	d, err := at(latest, "body", "detail")
	if err != nil {
		return jsonv.Value{}, err
	}
	d = d.Or(jsonv.Null())
	if d.IsNull() {
		return d, nil
	}
	return d.SliceString(0, detailRunes)
}

// lastPRURL is `map(.body.pr_url) | map(select(. != null)) | last`, so a
// false pr_url counts.
func lastPRURL(g []jsonv.Value) (jsonv.Value, error) {
	last := jsonv.Null()
	for _, ev := range g {
		u, err := at(ev, "body", "pr_url")
		if err != nil {
			return jsonv.Value{}, err
		}
		if !u.IsNull() {
			last = u
		}
	}
	return last, nil
}

// prevStateOf is `map(select(.body.state != "exited")) | max_by(.ts) | .body.state`.
func prevStateOf(g []jsonv.Value) (jsonv.Value, error) {
	var kept []jsonv.Value
	for _, ev := range g {
		st, err := at(ev, "body", "state")
		if err != nil {
			return jsonv.Value{}, err
		}
		if !st.Equal(jsonv.Str("exited")) {
			kept = append(kept, ev)
		}
	}
	best, err := jsonv.MaxBy(kept, field("ts"))
	if err != nil {
		return jsonv.Value{}, err
	}
	return at(best, "body", "state")
}

// ageSeconds is `(now - ($latest.ts/1000)) | floor`.
func ageSeconds(ts jsonv.Value, nowSec float64) (jsonv.Value, error) {
	f, ok := ts.AsFloat()
	if !ok {
		return jsonv.Value{}, jsonv.TypeErrorf("%s and number cannot be divided", ts.Kind())
	}
	return jsonv.Num(math.Floor(nowSec - f/1000)), nil
}

// reread is the arm's `jq -c '.[]'` over $base: every row is printed and parsed
// again, so a number jq computed (age_s, or an infinity it clamps to
// 1.7976931348623157e+308) comes back as a literal and prints in decNumber's
// form, 1.7976931348623157E+308.
func reread(rows []jsonv.Value) ([]jsonv.Value, error) {
	out := make([]jsonv.Value, len(rows))
	for i, row := range rows {
		vs, err := jsonv.DecodeStream(bytes.NewReader(jsonv.Append(nil, row, jsonv.Options{})))
		if err != nil || len(vs) != 1 {
			return nil, fmt.Errorf("roster: row does not reparse: %v", err)
		}
		out[i] = vs[0]
	}
	return out, nil
}

// collapse is the second group_by: the newest session's row per branch plus
// the dispatch joins and a `sessions` list.
func collapse(rows []jsonv.Value, dispatch, esess jsonv.Value) ([]jsonv.Value, error) {
	groups, err := jsonv.GroupBy(rows, field("branch"))
	if err != nil {
		return nil, err
	}
	out := make([]jsonv.Value, len(groups))
	for i, g := range groups {
		sorted, err := jsonv.SortBy(g, field("ts"))
		if err != nil {
			return nil, err
		}
		members := slices.Clone(sorted[len(sorted)-1].Members())
		br, _ := g[0].Get("branch")
		key, _ := br.AsString()
		entry, _ := dispatch.Get(key)
		for _, k := range []string{"title", "engine", "model", "tier"} {
			v, _ := entry.Get(k)
			members = append(members, jsonv.Member{Key: k, Val: v})
		}
		es, _ := esess.Get(key)
		members = append(members, jsonv.Member{Key: "engine_session", Val: es})

		list := make([]jsonv.Value, len(sorted))
		for j, r := range sorted {
			session, _ := r.Get("session")
			state, _ := r.Get("state")
			age, _ := r.Get("age_s")
			list[j] = jsonv.Object(
				jsonv.Member{Key: "session", Val: session},
				jsonv.Member{Key: "state", Val: state},
				jsonv.Member{Key: "age_s", Val: age},
			)
		}
		members = append(members, jsonv.Member{Key: "sessions", Val: jsonv.Array(list...)})
		out[i] = jsonv.Object(members...)
	}
	return out, nil
}

// resolveExited turns a false `exited` back into the row's previous state when
// an engine pane sits in the branch's worktree. It reads `.state` and
// `.branch` through `$(jq -r ...)`, which drops trailing newlines.
func resolveExited(row *jsonv.Value, live []string, worktrees func() (string, error)) error {
	state, _ := row.Get("state")
	st, _ := state.AsString()
	br, _ := row.Get("branch")
	branch, _ := br.AsString()
	branch = strings.TrimRight(branch, "\n")
	if strings.TrimRight(st, "\n") != "exited" || branch == "" {
		return nil
	}
	porcelain, err := worktrees()
	if err != nil {
		return err
	}
	wt := worktreePath(porcelain, branch)
	if wt == "" || !slices.ContainsFunc(live, func(pane string) bool { return paneIsEngineAt(pane, wt) }) {
		return nil
	}
	prev, _ := row.Get("prev_state")
	row.Set("state", prev.Or(jsonv.Str("working")))
	row.Set("exit_suspect", jsonv.Bool(true))
	return nil
}

// worktreePath mirrors the awk program
//
//	/^worktree /{p=$2} $0=="branch "b{print p}
//
// whose output `$(...)` joins and trims: several worktrees on one branch give
// a multi-line value that never equals a pane path.
func worktreePath(porcelain, branch string) string {
	want := "branch refs/heads/" + branch
	var p string
	var hits []string
	for line := range strings.SplitSeq(porcelain, "\n") {
		if strings.HasPrefix(line, "worktree ") {
			p = secondField(line)
		}
		if line == want {
			hits = append(hits, p)
		}
	}
	return strings.TrimRight(strings.Join(hits, "\n"), "\n")
}

// secondField is awk's `$2` under the default FS: blank- and tab-separated.
func secondField(line string) string {
	f := strings.FieldsFunc(line, func(r rune) bool { return r == ' ' || r == '\t' })
	if len(f) < 2 {
		return ""
	}
	return f[1]
}

// paneIsEngineAt is `_pane_is_engine_at`: the pane's path is everything after
// its first space and must equal the worktree exactly. A row with no space is
// its own command and its own path, as `${1#* }` and `${1%% *}` leave it.
func paneIsEngineAt(pane, worktree string) bool {
	cmd, path, found := strings.Cut(pane, " ")
	if !found {
		path = pane
	}
	return path == worktree && isEngineCmd(cmd)
}

func isEngineCmd(cmd string) bool {
	c := strings.TrimPrefix(cmd, ".")
	c = strings.TrimSuffix(c, "-wrapped")
	return slices.Contains(EngineCommands, c)
}

// withIdentity appends the codename identity, suffixes colliding names, and
// drops prev_state unless it explains an exit_suspect row.
//
// The arm builds its identity map from `for br in $(jq -r '.[].branch')`, so
// a branch holding a space, tab or newline splits into words and never keys
// itself: such a row gets no identity.
//
// Not mirrored, since git refuses such branch names: the unquoted `$(...)`
// would also glob-expand a word holding `*`, `?` or `[`, a NUL byte in a
// bus-supplied branch is dropped by `$(...)`, and awk's `-v` processes
// backslash escapes in the branch it is given (see worktreePath).
func withIdentity(events []jsonv.Value, rows []jsonv.Value) []jsonv.Value {
	counts := map[string]int{}
	for i := range rows {
		row := &rows[i]
		br, _ := row.Get("branch")
		branch, _ := br.AsString()
		if branch == "" || strings.ContainsAny(branch, " \t\n") {
			continue
		}
		id := identity.For(events, branch)
		for _, m := range id.Members() {
			row.Set(m.Key, m.Val)
		}
		name, _ := id.Get("name")
		s, _ := name.AsString()
		counts[s]++
	}
	for i := range rows {
		row := &rows[i]
		if name, ok := row.Get("name"); ok {
			n, _ := name.AsString()
			if counts[n] > 1 {
				br, _ := row.Get("branch")
				branch, _ := br.AsString()
				row.Set("name", jsonv.Str(n+"·"+idToken(branch)))
			}
		}
		if suspect, _ := row.Get("exit_suspect"); !suspect.Truthy() {
			row.Delete("prev_state")
		}
	}
	return rows
}

// idToken is `(.branch | capture(...) | .id) // .branch`.
func idToken(branch string) string {
	if m := idSuffix.FindStringSubmatch(branch); m != nil {
		return m[1]
	}
	return branch
}
