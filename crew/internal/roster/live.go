package roster

import (
	"slices"
	"strings"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/jsonv"
)

// LiveStates are the non-terminal states a row can hold: what `crew fleet`
// shows without --all, and what a live diagram keeps while the row still has
// evidence of life.
var LiveStates = []string{"working", "blocked", "pr_open"}

// WaitingStates are the finished states a live view keeps while the branch's
// worktree still stands: the worker said done, failed or exited, and only
// `crew reap` has anything left to take away.
var WaitingStates = []string{"done", "failed", "exited"}

// StaleAfter is how old a non-terminal row may be and still count as live when
// only its worktree vouches for it.
const StaleAfter = 48 * time.Hour

// Liveness carries the two outside reads the rules make, injected so a test
// needs no tmux server and no git. Both are required, and each is read at most
// once by the caller.
type Liveness struct {
	// Windows is the set of branches stamped on a tmux window.
	Windows func() map[string]bool
	// Worktrees is the stdout of `git worktree list --porcelain` for one repo.
	Worktrees func() (string, error)
}

// WindowBranches parses the stdout of
// `tmux list-windows -a -F '#{@crew_branch}'` into a set. A window with no
// stamp and a tmux failure both print an empty field, so neither names a
// branch.
func WindowBranches(out string) map[string]bool {
	branches := map[string]bool{}
	for b := range strings.SplitSeq(out, "\n") {
		if b != "" {
			branches[b] = true
		}
	}
	return branches
}

// Stale reports a non-terminal row with no evidence of life: no tmux window
// stamped with its branch, and no worktree for it with a row younger than
// StaleAfter.
func (l Liveness) Stale(row jsonv.Value) bool {
	if l.Windows()[rowBranch(row)] {
		return false
	}
	v, _ := row.Get("age_s")
	ageS, ok := v.AsFloat()
	if !ok || time.Duration(ageS*float64(time.Second)) >= StaleAfter {
		return true
	}
	return l.worktreeGone(row)
}

// Drawn reports whether a row belongs in a live view: a finished row only while
// its worktree still stands for `crew reap` to take, and any other row only while
// it is not stale. A `reaped` row is already history, and a row with no branch or
// no state is not a worker row at all. Neither is drawn.
//
// "Any other row" is a running state or a state this build does not know: the
// states are `crew status`'s, and a diagram that dropped a worker the moment
// `crew status` learned a new one would hide the one row its operator needs.
// Staleness still governs an unknown state, so this cannot resurrect a crew's
// history.
func (l Liveness) Drawn(row jsonv.Value) bool {
	state, branch := rowState(row), rowBranch(row)
	switch {
	case branch == "" || state == "":
		return false
	case state == "reaped":
		return false
	case slices.Contains(WaitingStates, state):
		return !l.worktreeGone(row)
	default:
		return !l.Stale(row)
	}
}

// worktreeGone reports the one thing the rules read from the worktree list with
// certainty: a probe that ran, and did not list a worktree for the row's
// branch. A probe that failed says nothing, so it hides nothing — the same
// stance `crew fleet` takes, where a diagram that grew back the finished rows
// beats one that emptied on a transient git error.
func (l Liveness) worktreeGone(row jsonv.Value) bool {
	porcelain, err := l.Worktrees()
	return err == nil && WorktreePath(porcelain, rowBranch(row)) == ""
}

// rowBranch is a row's branch as the fold wrote it, and "" when the row has no
// string there. `crew fleet`'s table prints the same field through a control-rune
// sanitizer, which cannot matter for a git ref, and neither "-" nor "" names a
// window or a worktree.
func rowBranch(row jsonv.Value) string {
	v, _ := row.Get("branch")
	s, _ := v.AsString()
	return s
}

// rowState is a row's state as the fold wrote it, and "" when it has no string
// there — which is no state, and so no row.
func rowState(row jsonv.Value) string {
	v, _ := row.Get("state")
	s, _ := v.AsString()
	return s
}
