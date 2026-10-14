package stack

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// cutStack is the bats `_stack_fixture`: a bare origin with feat/parent (on
// main) and feat/child (on the parent), plus the dispatch row that recorded the
// child's cut point.
func (f *fixture) cutStack(t *testing.T) string {
	t.Helper()
	parent := f.branch("origin/main", "feat/parent", "parent1")
	f.branch("origin/feat/parent", "feat/child", "child1")
	f.bus(dispatchRow(1, "feat/child", "feat/parent", parent))
	return parent
}

// advance puts a new commit on branch and publishes it.
func (f *fixture) advance(branch, msg string) string {
	f.t.Helper()
	f.git(f.main, "checkout", "-q", branch)
	f.git(f.main, "commit", "-q", "--allow-empty", "-m", msg)
	f.push(f.main, branch)
	return f.head(f.main, branch)
}

// rebaseOnto rebases branch onto upstream and force-pushes it, as a worker who
// followed the align directive does.
func (f *fixture) rebaseOnto(branch, upstream string) {
	f.t.Helper()
	f.git(f.main, "checkout", "-q", branch)
	f.git(f.main, "rebase", "-q", upstream)
	f.push(f.main, branch)
}

func (f *fixture) appendBus(rows ...string) {
	f.t.Helper()
	data, err := os.ReadFile(f.log)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(f.log, append(data, []byte(strings.Join(rows, "\n")+"\n")...), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func TestCutPrintsRecordedCut(t *testing.T) {
	f := newFixture(t)
	parent := f.cutStack(t)
	code, out, errOut := f.stackRun(t, Request{Child: "feat/child"})
	if code != 0 || out != parent+"\n" {
		t.Fatalf("cut feat/child = %d, %q (%s); want the recorded oid", code, out, errOut)
	}
}

func TestCutIgnoresWorkerRows(t *testing.T) {
	f := newFixture(t)
	parent := f.cutStack(t)
	const other = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	f.appendBus(
		fmt.Sprintf(`{"ts":5,"crew_id":"c1","kind":"status","from":"worker:feat/child#s1","branch":"feat/child","body":{"state":"working","cut":%q,"base_oid":%q}}`, other, other),
		fmt.Sprintf(`{"ts":6,"crew_id":"c1","kind":"msg","from":"worker:feat/child#s1","to":"dispatcher:c1","branch":"feat/child","body":{"kind":"stack_cut","cut":%q,"base_oid":%q}}`, other, other),
	)
	code, out, errOut := f.stackRun(t, Request{Child: "feat/child"})
	if code != 0 || out != parent+"\n" {
		t.Fatalf("cut with forged rows = %d, %q (%s); want %s", code, out, errOut, parent)
	}
}

func TestCutOnlyForgedRowsMeansNoCut(t *testing.T) {
	f := newFixture(t)
	const other = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	f.bus(fmt.Sprintf(`{"ts":5,"crew_id":"c1","kind":"status","from":"worker:feat/child#s1","branch":"feat/child","body":{"cut":%q,"base_oid":%q}}`, other, other))
	code, out, errOut := f.stackRun(t, Request{Child: "feat/child"})
	if code != 1 || out != "" || !strings.Contains(errOut, "ask the child") {
		t.Fatalf("cut with only forged rows = %d, %q, %q", code, out, errOut)
	}
	if !strings.Contains(errOut, "no recorded cut for feat/child in crew c1") {
		t.Errorf("refusal lost the arm's wording: %s", errOut)
	}
}

func TestCutPRNumberResolvesHeadBranch(t *testing.T) {
	f := newFixture(t)
	parent := f.cutStack(t)
	f.addPR(7, "feat/child", "feat/parent", "OPEN")
	f.addCrossPR(8, "feat/fork", "feat/parent")

	code, out, errOut := f.stackRun(t, Request{Child: "#7"})
	if code != 0 || out != parent+"\n" {
		t.Fatalf("cut '#7' = %d, %q (%s)", code, out, errOut)
	}
	// A bare number is a PR number too: the arm strips one leading `#`.
	if code, out, _ := f.stackRun(t, Request{Child: "7"}); code != 0 || out != parent+"\n" {
		t.Fatalf("cut 7 = %d, %q", code, out)
	}
	code, out, errOut = f.stackRun(t, Request{Child: "8"})
	if code != 1 || out != "" || !strings.Contains(errOut, "PR #8 is from a fork") {
		t.Fatalf("cut 8 = %d, %q, %q", code, out, errOut)
	}
	// A PR gh cannot read is the arm's refusal, not a fallthrough to the branch.
	if code, _, errOut := f.stackRun(t, Request{Child: "#99"}); code != 1 || errOut != "crew: cannot read PR #99\n" {
		t.Fatalf("cut #99 = %d, %q", code, errOut)
	}
}

func TestCutRefusesAnUnsafeBranchName(t *testing.T) {
	f := newFixture(t)
	f.cutStack(t)
	// `git check-ref-format --branch` accepts a shell metacharacter, so these are
	// the names the guards refuse: a colon (a refspec), a leading `+` (a force-update
	// refspec git itself calls valid) and git's own refusals.
	for _, bad := range []string{"+refs/heads/x", "bad name", "a:b", "feat/x?y", "feat/x*", "feat/x/"} {
		code, out, errOut := f.stackRun(t, Request{Child: bad})
		if code != 1 || out != "" || !strings.Contains(errOut, "is not a plain branch name") {
			t.Fatalf("cut %q = %d, %q, %q", bad, code, out, errOut)
		}
	}
}

func TestCutRecordWritesParentTipWhenRebased(t *testing.T) {
	f := newFixture(t)
	f.cutStack(t)
	newTip := f.advance("feat/parent", "parent2")
	f.rebaseOnto("feat/child", "feat/parent")

	code, out, errOut := f.stackRun(t, Request{Record: true, Child: "feat/child"})
	if code != 0 || out != newTip+"\n" {
		t.Fatalf("cut --record = %d, %q (%s); want %s", code, out, errOut, newTip)
	}
	rows := f.busRows(t, "stack_cut")
	if len(rows) != 1 {
		t.Fatalf("bus rows: %d, want one stack_cut row", len(rows))
	}
	if rows[0]["cut"] != newTip || rows[0]["parent"] != "feat/parent" || rows[0]["branch"] != "feat/child" ||
		rows[0]["crew_id"] != "c1" {
		t.Fatalf("row = %v", rows[0])
	}
	if ts, ok := rows[0]["ts"].(float64); !ok || int64(ts) != f.now.UnixMilli() {
		t.Fatalf("row ts = %v, want the clock's %d", rows[0]["ts"], f.now.UnixMilli())
	}
	// The recorded cut is what a later plain cut prints.
	if _, out, _ := f.stackRun(t, Request{Child: "feat/child"}); out != newTip+"\n" {
		t.Fatalf("cut after --record printed %q, want %q", out, newTip)
	}
}

func TestCutRecordRefusesWhenChildIsNotRebased(t *testing.T) {
	f := newFixture(t)
	f.cutStack(t)
	newTip := f.advance("feat/parent", "parent2")
	_ = newTip

	code, out, errOut := f.stackRun(t, Request{Record: true, Child: "feat/child"})
	if code != 1 || out != "" || !strings.Contains(errOut, "is not an ancestor of origin/feat/child") {
		t.Fatalf("cut --record = %d, %q, %q", code, out, errOut)
	}
	if !strings.Contains(errOut, "crew stack cut --record feat/child <oid>") {
		t.Errorf("the refusal lost the arm's next step: %s", errOut)
	}
	if n := len(f.busRows(t, "stack_cut")); n != 0 {
		t.Fatalf("a refused record wrote %d rows", n)
	}
}

func TestCutRecordOidTheChildReports(t *testing.T) {
	f := newFixture(t)
	old := f.cutStack(t)
	newTip := f.advance("feat/parent", "parent2")

	// The old parent tip is an ancestor of the child, so it is recordable even
	// though the parent has moved.
	code, out, errOut := f.stackRun(t, Request{Record: true, Child: "feat/child", Oid: old})
	if code != 0 || out != old+"\n" {
		t.Fatalf("record the child's oid = %d, %q (%s)", code, out, errOut)
	}
	rows := f.busRows(t, "stack_cut")
	if len(rows) != 1 || rows[0]["cut"] != old || rows[0]["parent"] != "feat/parent" {
		t.Fatalf("rows = %v", rows)
	}

	// The parent's new tip is not in the child: refused, nothing written.
	f.bus(dispatchRow(1, "feat/child", "feat/parent", old))
	code, out, errOut = f.stackRun(t, Request{Record: true, Child: "feat/child", Oid: newTip})
	if code != 1 || out != "" || !strings.Contains(errOut, "is not an ancestor of origin/feat/child") {
		t.Fatalf("record a non-ancestor = %d, %q, %q", code, out, errOut)
	}
	if n := len(f.busRows(t, "stack_cut")); n != 0 {
		t.Fatalf("a refused record wrote %d rows", n)
	}

	// The child's own head is never a cut point.
	childHead := f.head(f.main, "feat/child")
	code, _, errOut = f.stackRun(t, Request{Record: true, Child: "feat/child", Oid: childHead})
	if code != 1 || !strings.Contains(errOut, "own head, not a cut") {
		t.Fatalf("record the child's head = %d, %q", code, errOut)
	}
}

func TestCutRecordIgnoresAttachRow(t *testing.T) {
	f := newFixture(t)
	f.cutStack(t)
	f.rebaseOnto("feat/child", "feat/parent")
	// A newer `--pr` attach row names a base the worker could retarget: it must
	// not become the parent.
	f.appendBus(`{"ts":2,"crew_id":"c1","kind":"dispatch","branch":"feat/child","base":"main","base_oid":null}`)
	code, out, errOut := f.stackRun(t, Request{Record: true, Child: "feat/child"})
	if code != 0 {
		t.Fatalf("cut --record = %d, %q (%s)", code, out, errOut)
	}
	rows := f.busRows(t, "stack_cut")
	if len(rows) != 1 || rows[0]["parent"] != "feat/parent" {
		t.Fatalf("rows = %v, want the recorded parent only", rows)
	}
	if n := len(f.busRows(t, "stack_cut")); n != 1 {
		t.Fatalf("rows = %d", n)
	}
}

func TestCutRecordNeedsARecordedParent(t *testing.T) {
	f := newFixture(t)
	f.cutStack(t)
	f.bus(`{"ts":2,"crew_id":"c1","kind":"dispatch","branch":"feat/child","base":"main","base_oid":null}`)
	code, out, errOut := f.stackRun(t, Request{Record: true, Child: "feat/child"})
	if code != 1 || out != "" || !strings.Contains(errOut, "no recorded parent for feat/child") {
		t.Fatalf("cut --record with only an attach row = %d, %q, %q", code, out, errOut)
	}
	if n := len(f.busRows(t, "stack_cut")); n != 0 {
		t.Fatalf("a refused record wrote %d rows", n)
	}
}

func TestCutUnreadableBus(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode 000 file")
	}
	f := newFixture(t)
	f.cutStack(t)
	if err := os.Chmod(f.log, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(f.log, 0o600) })
	code, out, errOut := f.stackRun(t, Request{Child: "feat/child"})
	if code != 1 || out != "" || errOut != "crew: cannot read the bus\n" {
		t.Fatalf("cut over an unreadable bus = %d, %q, %q", code, out, errOut)
	}
}

func TestCutNoCrewID(t *testing.T) {
	f := newFixture(t)
	f.cutStack(t)
	t.Setenv("CREW_ID", "")
	code, out, errOut := f.stackRun(t, Request{Child: "feat/child"})
	if code != 1 || out != "" || !strings.HasPrefix(errOut, "crew: no crew id — CREW_ID unset") {
		t.Fatalf("cut with no crew id = %d, %q, %q", code, out, errOut)
	}
}
