package rosterrender

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/hold"
	"github.com/noamsto/dispatcher/crew/internal/identity"
	"github.com/noamsto/dispatcher/crew/internal/jqrun"
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
func emptyModel() map[string]any {
	return map[string]any{"rows": []any{}, "holds": []any{}, "roles": []any{}}
}

// model is `_rr_model`: the renderer's whole input, {rows, holds, roles}. Every
// fallible step returns an error before the caller writes anything, so a failed
// model can never replace a good diagram.
//
// The rows come from `roster.Fold` in process — the arm re-entered the script for
// `crew roster`, and this is the same fold, on the same wall clock. `nowSec` is
// therefore real time and never CREW_CLOCK, exactly as that child read `jq now`.
//
// The fold is then cut to the rows `live` finds drawn. The cut happens here and
// not in `modelProg` so the `dispatched` rows the merge synthesizes from a
// pending dispatch survive — a session launched and not yet posting is live by
// definition — and so the live count, the legend and the boxes speak of the same
// rows.
func model(paths bus.Paths, crew string, nowSec float64, roleRows []string, rp roster.Probes, live roster.Liveness) (map[string]any, error) {
	events, err := bus.ReadEvents(paths.Log)
	if errors.Is(err, bus.ErrNoLog) {
		return emptyModel(), nil
	}
	if err != nil {
		return nil, err
	}
	docs := make([]any, len(events))
	for i, ev := range events {
		docs[i] = ev.Doc
	}

	rows, err := roster.Fold(docs, crew, nowSec, rp)
	if err != nil {
		return nil, err
	}
	kept := make([]any, 0, len(rows))
	for _, row := range rows {
		if live.Drawn(row) {
			kept = append(kept, row)
		}
	}
	pending, err := jqrun.RunAny(pendingProg, docs, nowSec, map[string]any{"crew": crew})
	if err != nil {
		return nil, err
	}
	holds, err := hold.Outstanding(docs, crew)
	if err != nil {
		return nil, err
	}
	out, err := jqrun.RunAny(modelProg,
		[]any{kept, pending, identityIDs(docs, pending), holds, roleList(roleRows, paths.Dir)},
		nowSec, nil)
	if err != nil {
		return nil, err
	}
	m, ok := out.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("roster-render: model is %T, not an object", out)
	}
	return m, nil
}

// identityIDs is the arm's `ids` map: one entry per pending branch, the identity
// its dispatch recorded, else the pool one. The arm read the branches one line at
// a time, so a branch holding a space still keys itself here (unlike in `roster`,
// whose unquoted `$(…)` word-splits).
func identityIDs(docs []any, pending any) map[string]any {
	recorded := identity.RecordedAll(docs)
	ids := map[string]any{}
	list, _ := pending.([]any)
	for _, p := range list {
		doc, _ := p.(map[string]any)
		if pend := doc["pending"]; pend == nil || pend == false {
			continue
		}
		branch, _ := doc["branch"].(string)
		if _, seen := ids[branch]; branch == "" || seen {
			continue
		}
		ids[branch] = identity.For(recorded, branch).Value()
	}
	return ids
}

// roleList is the arm's roles loop over `_rr_role_panes`' rows: `branch\trole\t
// state` per live role pane, with the engine read from the worker-writable
// roles.json beside it.
func roleList(rows []string, dir string) []any {
	out := make([]any, 0, len(rows))
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
		out = append(out, map[string]any{
			"branch": br,
			"role":   role,
			"state":  state,
			"engine": roleEngine(dir, br, role),
		})
	}
	return out
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
	var doc any
	if json.Unmarshal(data, &doc) != nil {
		return "?"
	}
	roles, _ := doc.(map[string]any)
	entry, _ := roles[role].(map[string]any)
	name, _ := entry["agent"].(string)
	if !slices.Contains(engines, name) {
		return "?"
	}
	return name
}

// renderText is `_rr_d2`: the model to D2 text. The program emits one string per
// output line; the arm read them through `jq -r` into `$(…)`, which strips every
// trailing newline, and wrote `printf '%s\n'` of that — so Go joins with one
// newline, trims any trailing ones, and appends exactly one.
func renderText(m map[string]any) (string, error) {
	var table map[string]string
	if err := json.Unmarshal(hexJSON, &table); err != nil {
		return "", fmt.Errorf("roster-render: hex table: %v", err)
	}
	hex := make(map[string]any, len(table))
	for k, v := range table {
		hex[k] = v
	}
	var palette []any
	for _, c := range identity.Palette() {
		palette = append(palette, c)
	}
	out, err := jqrun.RunAny(d2Prog, []any{m}, 0, map[string]any{"palette": palette, "hex": hex})
	if err != nil {
		return "", err
	}
	var lines []string
	elems, _ := out.([]any)
	for _, e := range elems {
		s, _ := e.(string)
		lines = append(lines, s)
	}
	return strings.TrimRight(strings.Join(lines, "\n"), "\n") + "\n", nil
}

// liveCount is `_rr_pass`' live number: working, blocked and dispatched rows
// plus the outstanding holds. It is the only thing the renderer prints, and the
// only thing its quiet window reads.
func liveCount(m map[string]any) (int64, error) {
	out, err := jqrun.RunAny(liveProg, []any{m}, 0, nil)
	if err != nil {
		return 0, err
	}
	lines, _ := out.([]any)
	if len(lines) != 1 {
		return 0, fmt.Errorf("roster-render: live count: %d values", len(lines))
	}
	var n float64
	switch v := lines[0].(type) {
	case json.Number:
		n, err = strconv.ParseFloat(v.String(), 64)
		if err != nil {
			return 0, errors.New("roster-render: live count is not a number")
		}
	case float64:
		n = v
	default:
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
