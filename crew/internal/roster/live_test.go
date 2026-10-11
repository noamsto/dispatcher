package roster

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/jsonv"
)

// row is a folded row as the diagram sees it: the three fields the liveness rule
// reads, and nothing else.
func row(t *testing.T, state, branch, ageS string) jsonv.Value {
	t.Helper()
	text := fmt.Sprintf(`{"state":%q,"branch":%q}`, state, branch)
	if ageS != "" {
		text = text[:len(text)-1] + fmt.Sprintf(`,"age_s":%s}`, ageS)
	}
	v, err := jsonv.ParseOne(text)
	if err != nil {
		t.Fatalf("row %q: %v", text, err)
	}
	return v
}

// live is the rule with both reads stubbed: `windows` are the branches stamped
// on a tmux window, `worktrees` the branches a `git worktree list --porcelain`
// names, and `probeErr` stands in for a probe that did not run.
func live(windows []string, worktrees []string, probeErr error) Liveness {
	return Liveness{
		Windows: func() map[string]bool { return WindowBranches(strings.Join(windows, "\n")) },
		Worktrees: func() (string, error) {
			if probeErr != nil {
				return "", probeErr
			}
			out := ""
			for _, b := range worktrees {
				out += "worktree /wt/" + b + "\nHEAD 0000000000000000000000000000000000000000\nbranch refs/heads/" + b + "\n\n"
			}
			return out, nil
		},
	}
}

const young = "60"

// TestDrawnIsTheLiveView is #999's rule as one table: what a diagram that stops
// drawing a crew's whole history keeps, and what it drops.
func TestDrawnIsTheLiveView(t *testing.T) {
	const stale = "172801" // one second past StaleAfter

	for _, tc := range []struct {
		name    string
		state   string
		branch  string
		ageS    string
		windows []string
		wt      []string
		want    bool
	}{
		{name: "working row with a window is live", state: "working", branch: "feat/a", ageS: young, windows: []string{"feat/a"}, want: true},
		{name: "working row with a young worktree is live", state: "working", branch: "feat/a", ageS: young, wt: []string{"feat/a"}, want: true},
		{name: "blocked row with a window is live", state: "blocked", branch: "feat/a", ageS: stale, windows: []string{"feat/a"}, want: true},
		{name: "pr_open with a worktree is live", state: "pr_open", branch: "feat/a", ageS: young, wt: []string{"feat/a"}, want: true},
		{name: "stale pr_open is hidden", state: "pr_open", branch: "feat/a", ageS: stale, want: false},
		{name: "pr_open with neither window nor worktree is hidden", state: "pr_open", branch: "feat/a", ageS: young, want: false},
		{name: "done with a worktree waits for reap", state: "done", branch: "feat/a", ageS: stale, wt: []string{"feat/a"}, want: true},
		{name: "done without one is history", state: "done", branch: "feat/a", ageS: young, want: false},
		{name: "failed with a worktree waits for reap", state: "failed", branch: "feat/a", ageS: stale, wt: []string{"feat/a"}, want: true},
		{name: "exited without one is history", state: "exited", branch: "feat/a", ageS: young, want: false},
		{name: "a window does not revive a finished row", state: "done", branch: "feat/a", ageS: young, windows: []string{"feat/a"}, want: false},
		{name: "reaped is never drawn", state: "reaped", branch: "feat/a", ageS: young, windows: []string{"feat/a"}, wt: []string{"feat/a"}, want: false},
		{name: "an unknown state with a window is still a worker", state: "sleeping", branch: "feat/a", ageS: young, windows: []string{"feat/a"}, want: true},
		{name: "an unknown state that is stale is not drawn", state: "sleeping", branch: "feat/a", ageS: stale, want: false},
		{name: "no state is no worker", state: "", branch: "feat/a", ageS: young, windows: []string{"feat/a"}, wt: []string{"feat/a"}, want: false},
		{name: "no branch is no worker", state: "working", branch: "", ageS: young, windows: []string{"feat/a"}, wt: []string{"feat/a"}, want: false},
		{name: "age_s missing on a live row is stale", state: "working", branch: "feat/a", ageS: "", wt: []string{"feat/a"}, want: false},
		{name: "another branch's worktree is not this one's", state: "done", branch: "feat/a", ageS: young, wt: []string{"feat/b"}, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := live(tc.windows, tc.wt, nil)
			if got := l.Drawn(row(t, tc.state, tc.branch, tc.ageS)); got != tc.want {
				t.Errorf("Drawn(%s %s age=%s) = %v, want %v", tc.state, tc.branch, tc.ageS, got, tc.want)
			}
		})
	}
}

// TestProbeFailureHidesNothing is `crew fleet`'s stance, now shared: a worktree
// list that could not be read says nothing, so a diagram never empties on a
// transient git error.
func TestProbeFailureHidesNothing(t *testing.T) {
	l := live(nil, nil, errors.New("git: not a worktree"))
	if !l.Drawn(row(t, "working", "feat/a", young)) {
		t.Error("a failed probe made a live row stale")
	}
	if !l.Drawn(row(t, "done", "feat/a", young)) {
		t.Error("a failed probe hid a finished row the probe never saw")
	}
	if l.Drawn(row(t, "reaped", "feat/a", young)) {
		t.Error("a failed probe drew a reaped row")
	}
	// Age is the one thing the probe cannot vouch for, so it still short-circuits.
	if l.Drawn(row(t, "working", "feat/a", "999999")) {
		t.Error("a failed probe revived a row past StaleAfter")
	}
}

// TestStaleIsTheRuleFleetPrints: the field `crew fleet --json` reports, and the
// count its stderr line hides, are this predicate — so moving it cannot move
// fleet's output.
func TestStaleIsTheRuleFleetPrints(t *testing.T) {
	for _, tc := range []struct {
		name    string
		ageS    string
		windows []string
		wt      []string
		want    bool
	}{
		{name: "windowed row is never stale", ageS: "999999", windows: []string{"feat/a"}},
		{name: "young row with a worktree is not stale", ageS: young, wt: []string{"feat/a"}},
		{name: "young row with neither is stale", ageS: young, want: true},
		{name: "age past the window is stale even with a worktree", ageS: "172800", wt: []string{"feat/a"}, want: true},
		{name: "old row with neither is stale", ageS: "999999", want: true},
		{name: "age_s missing is stale", want: true},
		{name: "age_s not a number is stale", ageS: `"young"`, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := live(tc.windows, tc.wt, nil).Stale(row(t, "working", "feat/a", tc.ageS))
			if got != tc.want {
				t.Errorf("Stale(age=%s) = %v, want %v", tc.ageS, got, tc.want)
			}
		})
	}
}

// TestWindowBranchesIgnoresUnstampedWindows: a window with no stamp prints an
// empty field, and a tmux failure prints nothing at all — neither names a branch.
func TestWindowBranchesIgnoresUnstampedWindows(t *testing.T) {
	got := WindowBranches("feat/a\n\nfeat/b\n")
	if len(got) != 2 || !got["feat/a"] || !got["feat/b"] {
		t.Errorf("WindowBranches = %v", got)
	}
	if len(WindowBranches("")) != 0 {
		t.Error("empty tmux output named a branch")
	}
}
