// Package roster folds a crew bus into one row per branch. The jq halves of
// the arm are the original programs from adapters/core/crew.sh, run through
// gojq (roster_base.jq, roster_final.jq); the external reads — the tmux pane
// list, `git worktree list` and the identity pool — stay Go, as in the arm's
// loop between the two jq steps.
package roster

import (
	"context"
	_ "embed"
	"fmt"
	"slices"
	"strings"

	"github.com/noamsto/dispatcher/crew/internal/identity"
	"github.com/noamsto/dispatcher/crew/internal/jqrun"
	"github.com/noamsto/dispatcher/crew/internal/jsonval"
	"github.com/noamsto/dispatcher/crew/internal/tmux"
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

// Probes are the external reads the arm makes, injected so a fold is a pure
// function of the bus and its scripted tmux.
type Probes struct {
	// Tmux answers the pane list (`list-panes`) and, with Live, the window
	// list. A tmux failure reads as no output.
	Tmux tmux.Client
	// Worktrees is the stdout of `git worktree list --porcelain`. A failure
	// is an *ExitError: the arm dies with git's status.
	Worktrees func() (string, error)
	// Live (`crew roster --live`) gives each row `live`: whether its branch
	// has a tmux window on this host. Unset leaves the rows as the bus alone
	// makes them.
	Live bool
}

// Windows is the set of branches stamped on a tmux window, empty when tmux
// fails.
func Windows(ctx context.Context, c tmux.Client) map[string]bool {
	out := tmux.Output(ctx, c, "list-windows", "-a", "-F", "#{@crew_branch}")
	return WindowBranches(out)
}

// CrewWindowBranches parses the stdout of
// `tmux list-windows -a -F '#{@crew_branch}\t#{@crew_id}'` into the branches
// this crew has a window for: a window stamped for another crew is not its.
// An unset @crew_id counts as any crew's, as it did before windows carried one.
func CrewWindowBranches(out, crew string) map[string]bool {
	branches := map[string]bool{}
	for line := range strings.SplitSeq(out, "\n") {
		branch, id, _ := strings.Cut(line, "\t")
		if branch != "" && (id == "" || id == crew) {
			branches[branch] = true
		}
	}
	return branches
}

// Fold is `crew roster <crew>` once the log exists: one row per branch, with
// age_s computed against nowSec (jq's `now`). A jq runtime error is returned
// as an error (main maps it to the jq-failure exit status), a failing
// worktree probe as its *ExitError.
func Fold(ctx context.Context, events []any, crew string, nowSec float64, p Probes) ([]map[string]any, error) {
	base, err := jqrun.RunAny(baseProgram, events, nowSec, map[string]any{"crew": jsonval.ValidUTF8(crew)})
	if err != nil {
		return nil, err
	}
	rows, _ := base.([]any)
	// $(...) drops trailing newlines, so the here-doc over $live is exactly
	// these lines.
	panes := tmux.Output(ctx, p.Tmux, "list-panes", "-a", "-F", "#{pane_current_command} #{pane_current_path}")
	live := strings.Split(strings.TrimRight(panes, "\n"), "\n")
	for _, row := range rows {
		if err := resolveExited(row, live, p.Worktrees); err != nil {
			return nil, err
		}
	}
	out, err := jqrun.RunAny(finalProgram, rows, nowSec, map[string]any{
		"m": identityMap(events, rows),
	})
	if err != nil {
		return nil, err
	}
	outRows, _ := out.([]any)
	folded := make([]map[string]any, 0, len(outRows))
	for _, r := range outRows {
		if row, ok := r.(map[string]any); ok {
			folded = append(folded, row)
		}
	}
	if p.Live {
		out := tmux.Output(ctx, p.Tmux, "list-windows", "-a", "-F", "#{@crew_branch}\t#{@crew_id}")
		windows := CrewWindowBranches(out, crew)
		for _, row := range folded {
			name, _ := row["branch"].(string)
			row["live"] = windows[name]
		}
	}
	return folded, nil
}

// identityMap is the arm's `for br in $(jq -r '.[].branch'); do ... done`
// loop: one entry per resolved row's branch, recorded identity when a
// dispatch left one, else the pool identity. The unquoted `$(...)`
// word-splits, so a branch holding a space, tab or newline never keys
// itself (and the empty word is dropped): such rows get no entry. Words
// holding glob characters are not mirrored — git refuses those names.
func identityMap(events, rows []any) map[string]any {
	recorded := identity.RecordedAll(events)
	m := map[string]any{}
	for _, r := range rows {
		row, _ := r.(map[string]any)
		branch, isStr := row["branch"].(string)
		if !isStr || branch == "" || strings.ContainsAny(branch, " \t\n") {
			continue
		}
		if _, seen := m[branch]; seen {
			continue
		}
		m[branch] = identity.For(recorded, branch).Value()
	}
	return m
}

// resolveExited turns a false `exited` back into the row's previous state when
// an engine pane sits in the branch's worktree. It reads `.state` and
// `.branch` through `$(jq -r ...)`, which drops trailing newlines. A row that
// is not an object is left alone.
func resolveExited(r any, live []string, worktrees func() (string, error)) error {
	row, ok := r.(map[string]any)
	if !ok {
		return nil
	}
	st, _ := row["state"].(string)
	branch, _ := row["branch"].(string)
	branch = strings.TrimRight(branch, "\n")
	if strings.TrimRight(st, "\n") != "exited" || branch == "" {
		return nil
	}
	porcelain, err := worktrees()
	if err != nil {
		return err
	}
	wt := WorktreePath(porcelain, branch)
	if wt == "" || !slices.ContainsFunc(live, func(pane string) bool { return paneIsEngineAt(pane, wt) }) {
		return nil
	}
	if prev := row["prev_state"]; prev != nil && prev != false {
		row["state"] = prev
	} else {
		row["state"] = "working"
	}
	row["exit_suspect"] = true
	return nil
}

// WorktreePath mirrors the awk program
//
//	/^worktree /{p=$2} $0=="branch "b{print p}
//
// whose output `$(...)` joins and trims: several worktrees on one branch give
// a multi-line value that never equals a pane path.
func WorktreePath(porcelain, branch string) string {
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
	return path == worktree && IsEngineCmd(cmd)
}

// IsEngineCmd is `_is_engine_cmd`: an engine command, with the nix wrapper's
// leading dot and `-wrapped` suffix stripped.
func IsEngineCmd(cmd string) bool {
	c := strings.TrimPrefix(cmd, ".")
	c = strings.TrimSuffix(c, "-wrapped")
	return slices.Contains(EngineCommands, c)
}
