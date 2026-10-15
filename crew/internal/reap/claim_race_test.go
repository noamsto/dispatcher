package reap

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/wtgit"
)

// claim is a dispatch's pre-window claim row, ageSec before the fake now
// (negative: dated in the future).
func claim(session string, ageSec int64) string {
	return fmt.Sprintf(`{"ts":%d,"crew_id":"c1","from":"worker:%s","kind":"claim"}`, (nowUnix-ageSec)*1000, session)
}

func (f *fx) killed() bool {
	return slices.ContainsFunc(f.calls, func(c string) bool { return strings.HasPrefix(c, "tmux kill-window") })
}

// stubOccupied makes the rb worktree's window @23 occupied by a pane running cmd.
func (f *fx) stubOccupied(cmd string) {
	f.tmux["list-windows -a -F #{window_id}\t#{@crew_name}\t#{pane_current_path}"] = "@23\tsage\t" + f.wt
	f.tmux["list-panes -a -F #{window_id}\t#{pane_id}\t#{pane_current_command}"] = "@23\t%33\t" + cmd
}

// wantOccupied guards the release tests below: an empty occupancy would make a
// "not released" assertion vacuous.
func (f *fx) wantOccupied() {
	f.t.Helper()
	if occ := Occupants(f.t.Context(), f.opts().Probes, f.wt, ""); len(occ) == 0 {
		f.t.Fatal("fixture window is not an occupant of the worktree")
	}
}

func (f *fx) mergedPR() {
	f.gh["pr view "+prURL+" --json state --jq .state"] = "MERGED"
}

func (f *fx) wantReaped(t *testing.T) {
	t.Helper()
	anchor, _ := wtgit.AnchorPath(f.wt)
	f.wantOut("crew reap: would reap feat/r (MERGED) @ "+f.wt, "crew reap: would prune record "+anchor)
}

// A fresh claim from a re-dispatched session outranks the branch's older done
// session in the per-branch fold, so the old done is not released.
func TestClaimMasksOlderDoneFromRelease(t *testing.T) {
	f := newFx(t)
	f.stubOccupied("fish")
	f.wantOccupied()
	f.bus(status("done", "", 100), claim(rb+"#s2", 50))
	if rc := f.reap(f.main, Request{Idle: "0"}); rc != 0 {
		t.Fatalf("rc=%d stderr=%q", rc, f.stderr.String())
	}
	if f.killed() {
		t.Fatalf("released a window under a fresh claim: %q", f.calls)
	}
}

// A claim superseded by the same session's own later done does not hide it.
func TestSupersededClaimDoesNotBlockRelease(t *testing.T) {
	f := newFx(t)
	f.stubOccupied("fish")
	f.wantOccupied()
	f.bus(claim(rb+"#s1", 100), status("done", "", 50))
	if rc := f.reap(f.main, Request{Idle: "0", Quiet: true}); rc != 0 {
		t.Fatalf("rc=%d stderr=%q", rc, f.stderr.String())
	}
	if !strings.Contains(f.stdout.String(), "released @23") || !slices.Contains(f.calls, "tmux kill-window -t @23") {
		t.Fatalf("stdout %q calls %q", f.stdout.String(), f.calls)
	}
}

// A fresh claim on the branch masks a prior done session, so its merged PR's
// worktree is not offered for removal.
func TestFreshClaimBlocksWorktreeRemoval(t *testing.T) {
	f := newFx(t)
	f.mergedPR()
	f.bus(status("done", prURL, 100), claim(rb+"#s2", 0))
	if rc := f.reap(f.main, Request{DryRun: true}); rc != 0 {
		t.Fatalf("rc=%d stderr=%q", rc, f.stderr.String())
	}
	f.wantOut("crew reap: nothing done to reap")
}

// A claim dated in the future never outranks a status, or it would mask the
// merged branch forever.
func TestFutureClaimDoesNotMask(t *testing.T) {
	f := newFx(t)
	f.mergedPR()
	f.bus(status("done", prURL, 100), claim(rb+"#s2", -1_000_000_000))
	if rc := f.reap(f.main, Request{DryRun: true}); rc != 0 {
		t.Fatalf("rc=%d stderr=%q", rc, f.stderr.String())
	}
	f.wantReaped(t)
}

// The claim is newer than the done status but past its mask TTL (1h), so only
// the TTL guard, not recency, keeps it from masking the done.
func TestExpiredClaimDoesNotMask(t *testing.T) {
	f := newFx(t)
	f.mergedPR()
	f.bus(status("done", prURL, 7200), claim(rb+"#s2", 7100))
	if rc := f.reap(f.main, Request{DryRun: true}); rc != 0 {
		t.Fatalf("rc=%d stderr=%q", rc, f.stderr.String())
	}
	f.wantReaped(t)
}

// A fresh claim on another branch has no bearing on this branch's reap.
func TestUnrelatedClaimDoesNotMask(t *testing.T) {
	f := newFx(t)
	f.mergedPR()
	f.bus(status("done", prURL, 100), claim("other-branch#s9", 0))
	if rc := f.reap(f.main, Request{DryRun: true}); rc != 0 {
		t.Fatalf("rc=%d stderr=%q", rc, f.stderr.String())
	}
	f.wantReaped(t)
}
