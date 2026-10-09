// Package roster folds a crew bus into one row per branch. The jq halves of
// the arm are the original programs from adapters/core/crew.sh, run through
// gojq (roster_base.jq, roster_final.jq); the external reads — the tmux pane
// list, `git worktree list` and the identity pool — stay Go, as in the arm's
// loop between the two jq steps.
package roster

import (
	_ "embed"
	"fmt"
	"slices"
	"strings"

	"github.com/noamsto/dispatcher/crew/internal/identity"
	"github.com/noamsto/dispatcher/crew/internal/jqrun"
	"github.com/noamsto/dispatcher/crew/internal/jsonv"
)

//go:embed roster_base.jq
var baseProgram string

//go:embed roster_final.jq
var finalProgram string

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

// Fold is `crew roster <crew>` once the log exists: one row per branch, with
// age_s computed against nowSec (jq's `now`). A jq runtime error is returned
// as an error (main maps it to the jq-failure exit status), a failing
// worktree probe as its *ExitError.
func Fold(events []jsonv.Value, crew string, nowSec float64, p Probes) (jsonv.Value, error) {
	base, err := jqrun.Run(baseProgram, events, nowSec, map[string]jsonv.Value{
		"crew": jsonv.Str(crew),
	})
	if err != nil {
		return jsonv.Value{}, err
	}
	rows := base.Elems()
	// $(...) drops trailing newlines, so the here-doc over $live is exactly
	// these lines.
	live := strings.Split(strings.TrimRight(p.Panes(), "\n"), "\n")
	for i := range rows {
		if err := resolveExited(&rows[i], live, p.Worktrees); err != nil {
			return jsonv.Value{}, err
		}
	}
	out, err := jqrun.Run(finalProgram, rows, nowSec, map[string]jsonv.Value{
		"m": identityMap(events, rows),
	})
	if err != nil {
		return jsonv.Value{}, err
	}
	return out, nil
}

// identityMap is the arm's `for br in $(jq -r '.[].branch'); do ... done`
// loop: one entry per resolved row's branch, recorded identity when a
// dispatch left one, else the pool identity. The unquoted `$(...)`
// word-splits, so a branch holding a space, tab or newline never keys
// itself (and the empty word is dropped): such rows get no entry. Words
// holding glob characters are not mirrored — git refuses those names.
//
// The recorded half is one pass over the bus (RecordedAll), not the arm's
// per-branch scan: a fold over a 100k-branch bus would otherwise be
// quadratic (#821).
func identityMap(events, rows []jsonv.Value) jsonv.Value {
	recorded := identity.RecordedAll(events)
	var members []jsonv.Member
	seen := map[string]bool{}
	for _, row := range rows {
		br, _ := row.Get("branch")
		branch, isStr := br.AsString()
		if !isStr || branch == "" || strings.ContainsAny(branch, " \t\n") || seen[branch] {
			continue
		}
		seen[branch] = true
		members = append(members, jsonv.Member{Key: branch, Val: identity.For(recorded, branch)})
	}
	return jsonv.Object(members...)
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
