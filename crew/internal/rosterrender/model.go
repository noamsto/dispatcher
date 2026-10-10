package rosterrender

import (
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/hold"
	"github.com/noamsto/dispatcher/crew/internal/identity"
	"github.com/noamsto/dispatcher/crew/internal/jqrun"
	"github.com/noamsto/dispatcher/crew/internal/jsonv"
	"github.com/noamsto/dispatcher/crew/internal/roster"
)

//go:embed pending.jq
var pendingProg string

//go:embed model.jq
var modelProg string

//go:embed d2.jq
var d2Prog string

//go:embed live.jq
var liveProg string

//go:embed hex.json
var hexJSON []byte

// rolePaneFormat is `_rr_role_panes`' `-F`: the six dispatcher-set window stamps
// the diagram is keyed on. Never discovery — a role pane with no stamps is not
// this crew's.
const rolePaneFormat = "#{@crew_dir}\t#{@crew_id}\t#{@crew_branch}\t#{@crew_role}\t#{@crew_state}\t#{@crew_exited}"

// roleRe is the arm's `^[A-Za-z0-9._-]{1,32}$` gate on a tmux-sourced role name.
var roleRe = regexp.MustCompile(`\A[A-Za-z0-9._-]{1,32}\z`)

// engines is the `case` that keeps a roles.json engine out of the diagram when
// it names something other than a worker engine.
var engines = []string{"claude", "codex", "cursor", "pi"}

// emptyModel is the arm's `{"rows":[],"holds":[],"roles":[]}`: what a repo with
// no bus yet renders as, with no jq run at all.
func emptyModel() jsonv.Value {
	return jsonv.Object(
		jsonv.Member{Key: "rows", Val: jsonv.Array()},
		jsonv.Member{Key: "holds", Val: jsonv.Array()},
		jsonv.Member{Key: "roles", Val: jsonv.Array()},
	)
}

// model is `_rr_model`: the renderer's whole input, {rows, holds, roles}. Every
// fallible step returns an error before the caller writes anything, so a failed
// model can never replace a good diagram.
//
// The rows come from `roster.Fold` in process — the arm re-entered the script for
// `crew roster`, and this is the same fold, on the same wall clock. `nowSec` is
// therefore real time and never CREW_CLOCK, exactly as that child read `jq now`.
func model(paths bus.Paths, crew string, nowSec float64, roleRows []string, rp roster.Probes) (jsonv.Value, error) {
	events, err := bus.ReadEvents(paths.Log)
	if errors.Is(err, bus.ErrNoLog) {
		return emptyModel(), nil
	}
	if err != nil {
		return jsonv.Value{}, err
	}
	raws := make([]jsonv.Value, len(events))
	for i, ev := range events {
		raws[i] = ev.Raw
	}

	rows, err := roster.Fold(raws, crew, nowSec, rp)
	if err != nil {
		return jsonv.Value{}, err
	}
	pending, err := jqrun.Run(pendingProg, raws, nowSec, map[string]jsonv.Value{
		"crew": jsonv.Str(crew),
	})
	if err != nil {
		return jsonv.Value{}, err
	}
	holds, err := hold.Outstanding(raws, crew)
	if err != nil {
		return jsonv.Value{}, err
	}
	out, err := jqrun.Run(modelProg,
		[]jsonv.Value{rows, pending, identityIDs(raws, pending), holds, roleList(roleRows, paths.Dir)},
		nowSec, nil)
	if err != nil {
		return jsonv.Value{}, err
	}
	return out, nil
}

// identityIDs is the arm's `ids` map: one entry per pending branch, the identity
// its dispatch recorded, else the pool one. The arm read the branches one line at
// a time, so a branch holding a space still keys itself here (unlike in `roster`,
// whose unquoted `$(…)` word-splits).
func identityIDs(events []jsonv.Value, pending jsonv.Value) jsonv.Value {
	recorded := identity.RecordedAll(events)
	seen := map[string]bool{}
	var members []jsonv.Member
	for _, p := range pending.Elems() {
		pend, _ := p.Get("pending")
		if !pend.Truthy() {
			continue
		}
		br, _ := p.Get("branch")
		branch, isStr := br.AsString()
		if !isStr || branch == "" || seen[branch] {
			continue
		}
		seen[branch] = true
		members = append(members, jsonv.Member{Key: branch, Val: identity.For(recorded, branch)})
	}
	return jsonv.Object(members...)
}

// roleList is the arm's roles loop over `_rr_role_panes`' rows: `branch\trole\t
// state` per live role pane, with the engine read from the worker-writable
// roles.json beside it.
func roleList(rows []string, dir string) jsonv.Value {
	out := make([]jsonv.Value, 0, len(rows))
	for _, line := range rows {
		if line == "" {
			continue
		}
		br, rest, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		role, state, _ := strings.Cut(rest, "\t")
		if !roleRe.MatchString(role) {
			continue
		}
		out = append(out, jsonv.Object(
			jsonv.Member{Key: "branch", Val: jsonv.Str(br)},
			jsonv.Member{Key: "role", Val: jsonv.Str(role)},
			jsonv.Member{Key: "state", Val: jsonv.Str(state)},
			jsonv.Member{Key: "engine", Val: jsonv.Str(roleEngine(dir, br, role))},
		))
	}
	return jsonv.Array(out...)
}

// roleEngine is the arm's engine read: roles.json sits under a worker's own
// artifacts dir, so a symlink could aim it at any JSON the user can read. Only a
// regular file is opened, and only a known engine name survives — anything else,
// including a file that will not parse or has no entry for this role, is "?".
func roleEngine(dir, branch, role string) string {
	f := filepath.Join(dir, "artifacts", branch, "roles.json")
	st, err := os.Lstat(f)
	if err != nil || !st.Mode().IsRegular() {
		return "?"
	}
	data, err := os.ReadFile(f)
	if err != nil {
		return "?"
	}
	vs, err := jsonv.DecodeStream(strings.NewReader(string(data)))
	if err != nil || len(vs) != 1 {
		return "?"
	}
	entry, ok := member(vs[0], role)
	if !ok {
		return "?"
	}
	agent, ok := member(entry, "agent")
	if !ok {
		return "?"
	}
	name, isStr := agent.AsString()
	if !isStr || !slices.Contains(engines, name) {
		return "?"
	}
	return name
}

func member(v jsonv.Value, key string) (jsonv.Value, bool) {
	if v.Kind() != jsonv.KindObject {
		return jsonv.Value{}, false
	}
	got, ok := v.Get(key)
	return got, ok
}

// renderText is `_rr_d2`: the model to D2 text. The program emits one string per
// output line; the arm read them through `jq -r` into `$(…)`, which strips every
// trailing newline, and wrote `printf '%s\n'` of that — so Go joins with one
// newline, trims any trailing ones, and appends exactly one.
func renderText(m jsonv.Value) (string, error) {
	hex, err := jsonv.DecodeStream(strings.NewReader(string(hexJSON)))
	if err != nil || len(hex) != 1 {
		return "", fmt.Errorf("roster-render: hex table: %v", err)
	}
	out, err := jqrun.Run(d2Prog, []jsonv.Value{m}, 0, map[string]jsonv.Value{
		"palette": identity.Palette(),
		"hex":     hex[0],
	})
	if err != nil {
		return "", err
	}
	var lines []string
	for _, e := range out.Elems() {
		s, _ := e.AsString()
		lines = append(lines, s)
	}
	return strings.TrimRight(strings.Join(lines, "\n"), "\n") + "\n", nil
}

// liveCount is `_rr_pass`' live number: working, blocked and dispatched rows
// plus the outstanding holds. It is the only thing the renderer prints, and the
// only thing its quiet window reads.
func liveCount(m jsonv.Value) (int64, error) {
	out, err := jqrun.Run(liveProg, []jsonv.Value{m}, 0, nil)
	if err != nil {
		return 0, err
	}
	lines := out.Elems()
	if len(lines) != 1 {
		return 0, fmt.Errorf("roster-render: live count: %d values", len(lines))
	}
	n, ok := lines[0].AsFloat()
	if !ok {
		return 0, errors.New("roster-render: live count is not a number")
	}
	return int64(n), nil
}

// roleRows is `_rr_role_panes`: this dir's, this crew's live role panes — a
// branch, a role that is not `lead`, no `@crew_exited` — as sorted
// `branch\trole\tstate` lines. The renderer sorts by role again, so the byte-wise
// order only settles the signature the daemon compares.
func roleRows(panes, dir, crew string) []string {
	var rows []string
	for line := range strings.SplitSeq(strings.TrimRight(panes, "\n"), "\n") {
		f := strings.Split(line, "\t")
		if len(f) < 6 {
			continue
		}
		if f[0] != dir || f[1] != crew || f[2] == "" || f[3] == "" || f[3] == "lead" || f[5] != "" {
			continue
		}
		rows = append(rows, f[2]+"\t"+f[3]+"\t"+f[4])
	}
	slices.Sort(rows)
	return rows
}
