package stack

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// alignStack is the bats `_align_fixture`: a bare origin with main ← feat/a ←
// feat/b ← feat/c (one empty commit each), the dispatch rows recording each
// child's cut, and the gh stub serving one PR per number and per head.
func (f *fixture) alignStack(t *testing.T) (string, string) {
	t.Helper()
	a := f.branch("origin/main", "feat/a", "a-1")
	b := f.branch("origin/feat/a", "feat/b", "b-1")
	f.branch("origin/feat/b", "feat/c", "c-1")
	f.addPR(1, "feat/a", "main", "OPEN")
	f.addPR(2, "feat/b", "feat/a", "OPEN")
	f.addPR(3, "feat/c", "feat/b", "OPEN")
	f.bus(dispatchRow(1, "feat/b", "feat/a", a), dispatchRow(2, "feat/c", "feat/b", b))
	return a, b
}

// squashMergeA is the bats `_align_squash_a`: main gains a commit, feat/a is
// deleted on origin and its PR reads MERGED.
func (f *fixture) squashMergeA() string {
	f.t.Helper()
	f.git(f.main, "checkout", "-q", "main")
	f.git(f.main, "commit", "-q", "--allow-empty", "-m", "squash-a")
	f.push(f.main, "main")
	f.git(f.main, "push", "-q", "origin", "--delete", "feat/a")
	f.addPR(1, "feat/a", "main", "MERGED")
	return f.head(f.main, "main")
}

// dropRows is the bats `grep -v '"branch":"feat/b"' "$SLOG"`: forget one
// branch's records.
func (f *fixture) dropRows(branch string) {
	f.t.Helper()
	data, err := os.ReadFile(f.log)
	if err != nil {
		f.t.Fatal(err)
	}
	var keep []string
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		if line != "" && !strings.Contains(line, `"branch":"`+branch+`"`) {
			keep = append(keep, line)
		}
	}
	f.bus(keep...)
}

func TestAlignNothingToDo(t *testing.T) {
	f := newFixture(t)
	aOID, bOID := f.alignStack(t)
	code, out, errOut := f.stackRun(t, Request{Top: "3"})
	if code != 0 {
		t.Fatalf("align 3 = %d (%s)", code, errOut)
	}
	got := rows(t, out)
	if len(got) != 3 {
		t.Fatalf("rows = %d, want 3:\n%s", len(got), out)
	}
	if order := strings.Join(mapBranches(got), ","); order != "feat/a,feat/b,feat/c" {
		t.Errorf("layer order = %s, want bottom to top", order)
	}
	wantRow(t, out, "feat/a", map[string]string{
		"pr": "1", "branch": `"feat/a"`, "parent": `"main"`, "mode": `"none"`,
		"reason": `"bottom layer, based on main"`,
	})
	wantRow(t, out, "feat/b", map[string]string{
		"pr": "2", "branch": `"feat/b"`, "parent": `"feat/a"`, "cut": `"` + aOID + `"`,
		"mode": `"none"`, "reason": `"already based on origin/feat/a"`,
	})
	wantRow(t, out, "feat/c", map[string]string{
		"pr": "3", "branch": `"feat/c"`, "parent": `"feat/b"`, "cut": `"` + bOID + `"`,
		"mode": `"none"`, "reason": `"already based on origin/feat/b"`,
	})
	if n := countReady(out); n != 0 {
		t.Errorf("ready rows = %d, want 0", n)
	}
}

func TestAlignFastForwardedParent(t *testing.T) {
	f := newFixture(t)
	aOID, bOID := f.alignStack(t)
	f.advance("feat/a", "a-2")

	code, out, errOut := f.stackRun(t, Request{Top: "feat/c"})
	if code != 0 {
		t.Fatalf("align feat/c = %d (%s)", code, errOut)
	}
	if n := len(rows(t, out)); n != 3 {
		t.Fatalf("rows = %d, want 3:\n%s", n, out)
	}
	wantRow(t, out, "feat/b", map[string]string{
		"pr": "2", "branch": `"feat/b"`, "parent": `"feat/a"`, "parent_pr": "null", "cut": `"` + aOID + `"`,
		"mode":      `"ff"`,
		"ready":     "true",
		"directive": `"new base feat/a; recorded old head ` + aOID + `; run: git rebase origin/feat/a"`,
		"reason":    `"origin/feat/a advanced"`,
	})
	wantRow(t, out, "feat/c", map[string]string{
		"pr": "3", "branch": `"feat/c"`, "parent": `"feat/b"`, "cut": `"` + bOID + `"`,
		"mode": `"onto"`, "ready": "false",
		"directive": `"new base feat/b; recorded old head ` + bOID + `; run: git rebase --onto origin/feat/b ` + bOID + `"`,
		"reason":    `"waits for feat/b push"`,
	})
	if n := countReady(out); n != 1 {
		t.Errorf("ready rows = %d, want exactly 1", n)
	}
	// The bottom layer is never rebased, however its parent moved.
	wantRow(t, out, "feat/a", map[string]string{"mode": `"none"`, "reason": `"bottom layer, based on main"`})
}

// A parent and its child both advanced: the child of the parent is the one layer
// that may act, and the layer above it still waits for that push even though its
// own parent moved out from under it.
func TestAlignParentAndChildBothAdvanced(t *testing.T) {
	f := newFixture(t)
	aOID, bOID := f.alignStack(t)
	f.advance("feat/a", "a-2")
	f.advance("feat/b", "b-2")

	code, out, errOut := f.stackRun(t, Request{Top: "3"})
	if code != 0 {
		t.Fatalf("align 3 = %d (%s)", code, errOut)
	}
	wantRow(t, out, "feat/b", map[string]string{
		"mode": `"ff"`, "ready": "true", "cut": `"` + aOID + `"`,
		"reason": `"origin/feat/a advanced"`,
	})
	wantRow(t, out, "feat/c", map[string]string{
		"mode": `"onto"`, "ready": "false", "cut": `"` + bOID + `"`,
		"directive": `"new base feat/b; recorded old head ` + bOID + `; run: git rebase --onto origin/feat/b ` + bOID + `"`,
		"reason":    `"waits for feat/b push"`,
	})
	if n := countReady(out); n != 1 {
		t.Errorf("ready rows = %d, want exactly 1", n)
	}
}

// Only the middle layer advanced: it is already based on its parent, so it is
// left alone and the top of the stack is the layer that gets the plan.
func TestAlignOnlyTheMiddleLayerAdvanced(t *testing.T) {
	f := newFixture(t)
	_, bOID := f.alignStack(t)
	f.advance("feat/b", "b-2")

	code, out, errOut := f.stackRun(t, Request{Top: "3"})
	if code != 0 {
		t.Fatalf("align 3 = %d (%s)", code, errOut)
	}
	wantRow(t, out, "feat/b", map[string]string{
		"mode": `"none"`, "ready": "false", "directive": "null",
		"reason": `"already based on origin/feat/a"`,
	})
	wantRow(t, out, "feat/c", map[string]string{
		"mode": `"ff"`, "ready": "true", "cut": `"` + bOID + `"`,
		"directive": `"new base feat/b; recorded old head ` + bOID + `; run: git rebase origin/feat/b"`,
		"reason":    `"origin/feat/b advanced"`,
	})
	if n := countReady(out); n != 1 {
		t.Errorf("ready rows = %d, want exactly 1", n)
	}
}

func TestAlignRewrittenParent(t *testing.T) {
	f := newFixture(t)
	aOID, _ := f.alignStack(t)
	f.rewrite("feat/a", "a-rewritten")

	code, out, errOut := f.stackRun(t, Request{Top: "3"})
	if code != 0 {
		t.Fatalf("align 3 = %d (%s)", code, errOut)
	}
	wantRow(t, out, "feat/b", map[string]string{
		"mode": `"onto"`, "ready": "true", "cut": `"` + aOID + `"`,
		"directive": `"new base feat/a; recorded old head ` + aOID + `; run: git rebase --onto origin/feat/a ` + aOID + `"`,
		"reason":    `"origin/feat/a was rewritten"`,
	})
	if n := countReady(out); n != 1 {
		t.Errorf("ready rows = %d, want exactly 1", n)
	}
}

func TestAlignSquashMergedParent(t *testing.T) {
	f := newFixture(t)
	aOID, bOID := f.alignStack(t)
	f.squashMergeA()

	code, out, errOut := f.stackRun(t, Request{Top: "3"})
	if code != 0 {
		t.Fatalf("align 3 = %d (%s)", code, errOut)
	}
	if len(rows(t, out)) != 2 {
		t.Fatalf("rows = %d, want the two above the merged parent:\n%s", len(rows(t, out)), out)
	}
	wantRow(t, out, "feat/b", map[string]string{
		"pr": "2", "parent": `"feat/a"`, "parent_pr": "1", "cut": `"` + aOID + `"`,
		"mode": `"onto"`, "ready": "true",
		"directive": `"new base main; recorded old head ` + aOID +
			`; parent PR #1 squash-merged; run: git rebase --onto origin/main ` + aOID + `"`,
		"reason": `"parent PR #1 squash-merged"`,
	})
	wantRow(t, out, "feat/c", map[string]string{
		"pr": "3", "parent": `"feat/b"`, "parent_pr": "null", "cut": `"` + bOID + `"`,
		"mode": `"onto"`, "ready": "false",
		"directive": `"new base feat/b; recorded old head ` + bOID + `; run: git rebase --onto origin/feat/b ` + bOID + `"`,
		"reason":    `"waits for feat/b push"`,
	})
}

func TestAlignRetargetedOntoDefaultBranch(t *testing.T) {
	f := newFixture(t)
	aOID, _ := f.alignStack(t)
	f.squashMergeA()
	// The child's PR was retargeted onto main after the parent merged: the same
	// squash-merge plan, still naming PR #1.
	f.addPR(2, "feat/b", "main", "OPEN")

	code, out, errOut := f.stackRun(t, Request{Top: "3"})
	if code != 0 {
		t.Fatalf("align 3 = %d (%s)", code, errOut)
	}
	if n := len(rows(t, out)); n != 2 {
		t.Fatalf("rows = %d, want the two above the merged parent:\n%s", n, out)
	}
	wantRow(t, out, "feat/b", map[string]string{
		// The walk keeps the recorded parent as the layer's parent and names its
		// merged PR, even though the PR itself is retargeted onto main.
		"parent": `"feat/a"`, "parent_pr": "1", "mode": `"onto"`, "ready": "true",
		"directive": `"new base main; recorded old head ` + aOID +
			`; parent PR #1 squash-merged; run: git rebase --onto origin/main ` + aOID + `"`,
	})
	// A newer `--pr` re-dispatch row that recorded main does not hide the retarget.
	f.appendBus(`{"ts":9,"crew_id":"c1","kind":"dispatch","branch":"feat/b","base":"main","base_oid":null}`)
	code, out, errOut = f.stackRun(t, Request{Top: "3"})
	if code != 0 {
		t.Fatalf("after the attach row: align 3 = %d (%s)", code, errOut)
	}
	if n := len(rows(t, out)); n != 2 {
		t.Fatalf("after the attach row: rows = %d:\n%s", n, out)
	}
	wantRow(t, out, "feat/b", map[string]string{
		"parent_pr": "1", "mode": `"onto"`, "ready": "true",
		"directive": `"new base main; recorded old head ` + aOID +
			`; parent PR #1 squash-merged; run: git rebase --onto origin/main ` + aOID + `"`,
	})
}

func TestAlignRetargetedWhileParentUnmerged(t *testing.T) {
	f := newFixture(t)
	aOID, _ := f.alignStack(t)
	f.addPR(2, "feat/b", "main", "OPEN")
	f.advance("feat/a", "a-2")
	for _, state := range []string{"OPEN", "CLOSED"} {
		f.addPR(1, "feat/a", "main", state)
		code, out, errOut := f.stackRun(t, Request{Top: "3"})
		if code != 0 {
			t.Fatalf("state %s: align 3 = %d (%s)", state, code, errOut)
		}
		if n := len(rows(t, out)); n != 2 {
			t.Fatalf("state %s: rows = %d, want the two above the retargeted layer:\n%s", state, n, out)
		}
		wantRow(t, out, "feat/b", map[string]string{
			"parent": `"main"`, "parent_pr": "null", "mode": `"unknown"`, "ready": "false",
			"directive": "null",
			"reason":    `"PR base main differs from the recorded parent feat/a — ask the child; never guess"`,
		})
		// The layer above an unknown one still waits: its own cut is sound, so it
		// keeps the recomputed onto plan against the rewritten parent.
		wantRow(t, out, "feat/c", map[string]string{"mode": `"onto"`, "ready": "false",
			"reason": `"waits for feat/b push"`})
		if n := countReady(out); n != 0 {
			t.Errorf("state %s: ready rows = %d, want 0 (an unknown lowest layer blocks the stack)", state, n)
		}
	}
	_ = aOID
}

func TestAlignStaleCutIsUnknown(t *testing.T) {
	f := newFixture(t)
	aOID, _ := f.alignStack(t)
	f.squashMergeA()
	// The child dropped the parent's commits: its recorded cut is gone.
	f.git(f.main, "checkout", "-q", "feat/b")
	f.git(f.main, "rebase", "-q", "--onto", "origin/main", aOID)
	f.push(f.main, "feat/b")
	f.advance("main", "main-2")

	code, out, errOut := f.stackRun(t, Request{Top: "3"})
	if code != 0 {
		t.Fatalf("align 3 = %d (%s)", code, errOut)
	}
	wantRow(t, out, "feat/b", map[string]string{
		"mode": `"unknown"`, "ready": "false", "directive": "null",
		"reason": fmt.Sprintf(`"recorded cut %s is not in origin/feat/b — ask the child; never guess"`, aOID[:12]),
	})
	if n := countReady(out); n != 0 {
		t.Errorf("ready rows = %d, want 0", n)
	}
}

// The parent was rewritten and the child rebased onto it without a new cut
// recorded; once the parent squash-merges, the stale cut says unknown, and the
// plan never assumes the child is up to date.
func TestAlignRebasedOntoRewrittenParentStaysUnknownAfterSquashMerge(t *testing.T) {
	f := newFixture(t)
	aOID, _ := f.alignStack(t)
	f.rewrite("feat/a", "a-prime")
	f.git(f.main, "checkout", "-q", "feat/b")
	f.git(f.main, "rebase", "-q", "--onto", "feat/a", aOID)
	f.push(f.main, "feat/b")
	f.squashMergeA()
	f.advance("main", "main-2")

	code, out, errOut := f.stackRun(t, Request{Top: "3"})
	if code != 0 {
		t.Fatalf("align 3 = %d (%s)", code, errOut)
	}
	wantRow(t, out, "feat/b", map[string]string{
		"mode": `"unknown"`, "ready": "false", "directive": "null",
		"reason": fmt.Sprintf(`"recorded cut %s is not in origin/feat/b — ask the child; never guess"`, aOID[:12]),
	})
	if n := countReady(out); n != 0 {
		t.Errorf("ready rows = %d, want 0", n)
	}
}

// Merging the default branch into the child does not make a stale cut
// trustworthy: the layer stays unknown.
func TestAlignMergingDefaultIntoChildDoesNotProveTheCut(t *testing.T) {
	f := newFixture(t)
	aOID, _ := f.alignStack(t)
	f.rewrite("feat/a", "a-prime")
	f.git(f.main, "checkout", "-q", "feat/b")
	f.git(f.main, "rebase", "-q", "--onto", "feat/a", aOID)
	f.push(f.main, "feat/b")
	f.squashMergeA()
	f.advance("main", "main-2")
	f.git(f.main, "checkout", "-q", "feat/b")
	f.git(f.main, "merge", "-q", "--no-ff", "-m", "merge-main", "origin/main")
	f.push(f.main, "feat/b")

	code, out, errOut := f.stackRun(t, Request{Top: "3"})
	if code != 0 {
		t.Fatalf("align 3 = %d (%s)", code, errOut)
	}
	wantRow(t, out, "feat/b", map[string]string{
		"mode": `"unknown"`, "ready": "false", "directive": "null",
		"reason": fmt.Sprintf(`"recorded cut %s is not in origin/feat/b — ask the child; never guess"`, aOID[:12]),
	})
	if n := countReady(out); n != 0 {
		t.Errorf("ready rows = %d, want 0", n)
	}
}

// The same missing cut with the parent still open: the layer is unknown rather
// than planned, and nothing above it is offered either.
func TestAlignCutMissingFromAnUnmergedLayer(t *testing.T) {
	f := newFixture(t)
	aOID, _ := f.alignStack(t)
	f.rewrite("feat/b", "b-x")

	code, out, errOut := f.stackRun(t, Request{Top: "3"})
	if code != 0 {
		t.Fatalf("align 3 = %d (%s)", code, errOut)
	}
	wantRow(t, out, "feat/b", map[string]string{
		"mode": `"unknown"`, "ready": "false", "directive": "null",
		"reason": fmt.Sprintf(`"recorded cut %s is not in origin/feat/b — ask the child; never guess"`, aOID[:12]),
	})
	wantRow(t, out, "feat/c", map[string]string{"ready": "false"})
	if n := countReady(out); n != 0 {
		t.Errorf("ready rows = %d, want 0", n)
	}
}

func TestAlignNoRecordedCut(t *testing.T) {
	f := newFixture(t)
	f.alignStack(t)
	f.dropRows("feat/b")
	f.advance("feat/a", "a-2")

	code, out, errOut := f.stackRun(t, Request{Top: "3"})
	if code != 0 {
		t.Fatalf("align 3 = %d (%s)", code, errOut)
	}
	wantRow(t, out, "feat/b", map[string]string{
		"mode": `"unknown"`, "ready": "false", "cut": "null", "directive": "null",
		"reason": `"no recorded cut for feat/b — ask the child for the parent oid it is based on ` +
			`(-> skill dispatcher:stacked); never guess"`,
	})
	wantRow(t, out, "feat/c", map[string]string{"mode": `"onto"`, "ready": "false", "reason": `"waits for feat/b push"`})
	if n := countReady(out); n != 0 {
		t.Errorf("ready rows = %d, want 0: an unknown layer is never ready", n)
	}
}

func TestAlignBaseDiffersFromRecordedParent(t *testing.T) {
	f := newFixture(t)
	aOID, _ := f.alignStack(t)
	f.dropRows("feat/b")
	f.appendBus(fmt.Sprintf(`{"ts":1,"crew_id":"c1","kind":"dispatch","branch":"feat/b","base":"feat/z","base_oid":%q}`, aOID))
	f.advance("feat/a", "a-2")

	code, out, errOut := f.stackRun(t, Request{Top: "3"})
	if code != 0 {
		t.Fatalf("align 3 = %d (%s)", code, errOut)
	}
	wantRow(t, out, "feat/b", map[string]string{
		"mode": `"unknown"`, "ready": "false", "directive": "null",
		"reason": `"PR base feat/a differs from the recorded parent feat/z — ask the child; never guess"`,
	})
	wantRow(t, out, "feat/c", map[string]string{"mode": `"onto"`, "ready": "false", "reason": `"waits for feat/b push"`})
}

func TestAlignWaitingLayerWithADoubtStaysUnknown(t *testing.T) {
	f := newFixture(t)
	_, bOID := f.alignStack(t)
	f.dropRows("feat/c")
	f.appendBus(fmt.Sprintf(`{"ts":2,"crew_id":"c1","kind":"dispatch","branch":"feat/c","base":"feat/z","base_oid":%q}`, bOID))
	f.advance("feat/a", "a-2")

	code, out, errOut := f.stackRun(t, Request{Top: "3"})
	if code != 0 {
		t.Fatalf("align 3 = %d (%s)", code, errOut)
	}
	wantRow(t, out, "feat/b", map[string]string{"mode": `"ff"`, "ready": "true"})
	rows := rows(t, out)
	last := rows[len(rows)-1]
	if !strings.Contains(last, `"mode":"unknown"`) || !strings.Contains(last, `"directive":null`) ||
		!strings.Contains(last, `"reason":"waits for feat/b push; PR base feat/b differs from the recorded parent feat/z`) {
		t.Fatalf("waiting layer: %s", last)
	}
}

func TestAlignBottomLayerStaysBottom(t *testing.T) {
	f := newFixture(t)
	aOID, _ := f.alignStack(t)
	// The newest complete record names the default branch, so the layer stays the
	// bottom one even after main moved: it is never rebased.
	f.appendBus(dispatchRow(3, "feat/a", "feat/old", aOID))
	f.appendBus(dispatchRow(4, "feat/a", "main", "1111111111111111111111111111111111111111"))
	f.advance("main", "main-2")

	code, out, errOut := f.stackRun(t, Request{Top: "3"})
	if code != 0 {
		t.Fatalf("align 3 = %d (%s)", code, errOut)
	}
	if len(rows(t, out)) != 3 {
		t.Fatalf("rows = %d:\n%s", len(rows(t, out)), out)
	}
	wantRow(t, out, "feat/a", map[string]string{
		"mode": `"none"`, "parent_pr": "null", "reason": `"bottom layer, based on main"`,
	})
}

func TestAlignRefusesAForkACycleAndABadTop(t *testing.T) {
	f := newFixture(t)
	f.alignStack(t)

	f.addCrossPR(3, "feat/c", "feat/b")
	code, out, errOut := f.stackRun(t, Request{Top: "3"})
	if code != 1 || out != "" || !strings.Contains(errOut, "PR for 3 is from a fork") {
		t.Fatalf("cross-repo top: %d, %q, %q", code, out, errOut)
	}
	f.addPR(3, "feat/c", "feat/b", "OPEN")

	// feat/c ← feat/b ← feat/a ← feat/c.
	f.addPR(1, "feat/a", "feat/c", "OPEN")
	code, out, errOut = f.stackRun(t, Request{Top: "3"})
	if code != 1 || out != "" || !strings.Contains(errOut, "cycle in the stack at feat/c") {
		t.Fatalf("cycle: %d, %q, %q", code, out, errOut)
	}
	f.addPR(1, "feat/a", "main", "OPEN")

	// A closed top PR has no stack to align.
	f.addPR(3, "feat/c", "feat/b", "CLOSED")
	if code, out, errOut := f.stackRun(t, Request{Top: "3"}); code != 1 || out != "" ||
		!strings.Contains(errOut, "PR #3 is CLOSED, not OPEN") {
		t.Fatalf("closed top: %d, %q, %q", code, out, errOut)
	}
	f.addPR(3, "feat/c", "feat/b", "OPEN")

	// A branch ref whose PR has another head is refused, not planned.
	f.prs["feat/other"] = pr{number: 3, head: "feat/c", base: "feat/b", state: "OPEN"}
	if code, out, errOut := f.stackRun(t, Request{Top: "feat/other"}); code != 1 || out != "" ||
		!strings.Contains(errOut, "the PR for feat/other has head feat/c, not feat/other") {
		t.Fatalf("branch ref mismatch: %d, %q, %q", code, out, errOut)
	}
	// A ref that is not a plain branch name never reaches gh, and prints no plan.
	if code, out, errOut := f.stackRun(t, Request{Top: "+refs/heads/x:refs/remotes/origin/main"}); code != 1 || out != "" ||
		!strings.Contains(errOut, "is not a plain branch name") {
		t.Fatalf("unsafe top ref: %d, %q, %q", code, out, errOut)
	}
}

func TestAlignRefusesUnsafeNamesFromGh(t *testing.T) {
	f := newFixture(t)
	f.alignStack(t)
	for _, bad := range []string{"feat/a..b", "bad name"} {
		f.replacePR(2, "feat/b", bad, "feat/a", "OPEN")
		code, out, errOut := f.stackRun(t, Request{Top: "feat/c"})
		if code != 1 || out != "" || !strings.Contains(errOut, "PR #2") {
			t.Fatalf("head %q: %d, %q, %q", bad, code, out, errOut)
		}
	}
	f.addPR(2, "feat/b", "feat/a", "OPEN")
	for _, bad := range []string{"feat/x;id", "a$(id)", "x|sh"} {
		f.replacePR(2, "feat/b", "feat/b", bad, "OPEN")
		code, out, errOut := f.stackRun(t, Request{Top: "3"})
		if code != 1 || out != "" || !strings.Contains(errOut, "unsafe") {
			t.Fatalf("base %q: %d, %q, %q", bad, code, out, errOut)
		}
		if !strings.Contains(errOut, "directive") {
			t.Errorf("base %q: the refusal should say why: %s", bad, errOut)
		}
	}
}

func TestAlignRefusesAParentThatDidNotLandOnDefault(t *testing.T) {
	f := newFixture(t)
	f.alignStack(t)
	f.addPR(2, "feat/b", "feat/a", "MERGED")
	code, out, errOut := f.stackRun(t, Request{Top: "3"})
	if code != 1 || out != "" || !strings.Contains(errOut, "PR #2 for feat/b merged into feat/a, not main") {
		t.Fatalf("merged into another branch: %d, %q, %q", code, out, errOut)
	}
	f.addPR(2, "feat/b", "feat/a", "CLOSED")
	if code, out, errOut := f.stackRun(t, Request{Top: "3"}); code != 1 || out != "" ||
		!strings.Contains(errOut, "PR #2 for feat/b is CLOSED, not merged") {
		t.Fatalf("closed parent: %d, %q, %q", code, out, errOut)
	}

	// Reached the other way round: the top PR was retargeted onto main, so its
	// parent comes from the record, and that merged PR landed on another branch.
	f.addPR(1, "feat/a", "main", "MERGED")
	f.addPR(2, "feat/b", "feat/x", "MERGED")
	f.addPR(3, "feat/c", "main", "OPEN")
	f.appendBus(`{"ts":9,"crew_id":"c1","kind":"dispatch","branch":"feat/c","base":"feat/b","base_oid":null}`)
	code, out, errOut = f.stackRun(t, Request{Top: "3"})
	if code != 1 || out != "" || !strings.Contains(errOut, "PR #2 for feat/b merged into feat/x, not main") {
		t.Fatalf("recorded parent merged elsewhere: %d, %q, %q", code, out, errOut)
	}
}

func TestAlignNeverWritesTheBus(t *testing.T) {
	f := newFixture(t)
	f.alignStack(t)
	f.advance("feat/a", "a-2")
	before, err := os.ReadFile(f.log)
	if err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := f.stackRun(t, Request{Top: "3"}); code != 0 {
		t.Fatalf("align 3 = %d (%s)", code, errOut)
	}
	after, err := os.ReadFile(f.log)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("align wrote the bus:\n%s\n---\n%s", before, after)
	}
	if len(f.appended) != 0 {
		t.Fatalf("align appended %d rows", len(f.appended))
	}
}

func TestAlignUnreadableBus(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode 000 file")
	}
	f := newFixture(t)
	f.alignStack(t)
	if err := os.Chmod(f.log, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(f.log, 0o600) })
	code, out, errOut := f.stackRun(t, Request{Top: "3"})
	if code != 1 || out != "" || errOut != "crew: cannot read the bus\n" {
		t.Fatalf("align over an unreadable bus = %d, %q, %q", code, out, errOut)
	}
}

// mapBranches is each row's branch, in order.
func mapBranches(rows []string) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		_, rest, _ := strings.Cut(r, `"branch":"`)
		out[i], _, _ = strings.Cut(rest, `"`)
	}
	return out
}

// countReady is how many layers the plan says may act now.
func countReady(out string) int {
	n := 0
	for _, r := range strings.Split(out, "\n") {
		if strings.Contains(r, `"ready":true`) {
			n++
		}
	}
	return n
}

func TestLayerRowLeavesHTMLUnescaped(t *testing.T) {
	got := layerRow(7, "a<b>&c", "main", 0, false, "", "none", false, "", "")
	want := `{"pr":7,"branch":"a<b>&c","parent":"main","parent_pr":null,"cut":null,"mode":"none","ready":false,"directive":null,"reason":""}`
	if got != want {
		t.Errorf("layerRow = %s, want %s", got, want)
	}
}
