package reap

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/filekind"
	"github.com/noamsto/dispatcher/crew/internal/gitguard"
	"github.com/noamsto/dispatcher/crew/internal/identity"
	"github.com/noamsto/dispatcher/crew/internal/marks"
	"github.com/noamsto/dispatcher/crew/internal/proc/proctest"
	"github.com/noamsto/dispatcher/crew/internal/wtgit"
)

const panesKey = "list-panes -a -F #{window_id}\t#{pane_id}\t#{pane_current_command}\t#{pane_current_path}"

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

// statusFor is a worker status row for branch's session, ageSec before now.
func statusFor(branch, session, state, pr string, ageSec int64) string {
	return fmt.Sprintf(`{"ts":%d,"crew_id":"c1","from":"worker:%s#%s","to":"dispatcher:c1","kind":"status","body":{"state":%q,"pr_url":%q}}`,
		(nowUnix-ageSec)*1000, branch, session, state, pr)
}

// addWorktree adds a linked worktree on a new branch.
func (f *fx) addWorktree(branch string) string {
	f.t.Helper()
	wt := filepath.Join(f.root, strings.ReplaceAll(branch, "/", "-"))
	runGit(f.t, f.main, "worktree", "add", "-q", "-b", branch, wt)
	return wt
}

// wantJSON asserts got and want are equal JSON values.
func wantJSON(t *testing.T, got, want string) {
	t.Helper()
	sameJSON(t, got, want)
}

func (f *fx) rows(kind string) []string {
	var out []string
	for _, line := range f.appended {
		if strings.Contains(line, `"kind":"`+kind+`"`) {
			out = append(out, line)
		}
	}
	return out
}

// A reap row that cannot be written does not stop the removal tail or the
// sweep.
func TestReapRowWriteFailureContinues(t *testing.T) {
	f := newFx(t)
	const sb, prS = "feat/s", "https://github.com/o/r/pull/8"
	wtS := f.addWorktree(sb)
	f.bus(statusFor(rb, "s1", "done", prURL, 0), statusFor(sb, "s2", "done", prS, 0))
	for _, pr := range []string{prURL, prS} {
		f.gh["pr view "+pr+" --json state --jq .state"] = "MERGED"
	}
	f.gh["pr view "+prURL+" --json headRefOid --jq .headRefOid"] = gitOut(t, f.main, "rev-parse", "refs/heads/"+rb)
	f.gh["pr view "+prS+" --json headRefOid --jq .headRefOid"] = "0123456789012345678901234567890123456789"
	f.gh["pr view "+prURL+" --json closingIssuesReferences --jq .closingIssuesReferences[].number"] = "12\n13"
	f.ghFail["issue edit 13 --remove-label dispatched"] = true
	f.appendFail = func(line string) error {
		if strings.Contains(line, `"kind":"reap"`) {
			return errors.New("disk full")
		}
		return nil
	}

	if rc := f.reap(f.main, Request{}); rc != 0 {
		t.Fatalf("rc=%d stderr=%q", rc, f.stderr.String())
	}
	f.wantOut(
		"crew reap: reaped feat/r (MERGED)",
		"crew reap: could not remove the dispatched label from #13 (feat/r)",
		"crew reap: reaped feat/s (MERGED)",
		"crew reap: kept local branch feat/s — tip diverges from the merged PR head",
	)
	f.wantErr("crew reap: could not record the reap row for feat/r: disk full\n" +
		"crew reap: could not record the reap row for feat/s: disk full\n")
	for _, wt := range []string{f.wt, wtS} {
		if _, err := os.Lstat(wt); !os.IsNotExist(err) {
			t.Fatalf("%s still there: %v", wt, err)
		}
	}
	st := f.rows("status")
	if len(st) != 2 || len(f.rows("reap")) != 0 {
		t.Fatalf("appended %q", f.appended)
	}
	wantJSON(t, st[0], fmt.Sprintf(`{"ts":%d,"crew_id":"c1","from":"worker:feat/r#s1","to":"dispatcher:c1","kind":"status","body":{"state":"reaped","detail":"PR #7 MERGED","source":"reap"}}`, nowUnix*1000))
	wantJSON(t, st[1], fmt.Sprintf(`{"ts":%d,"crew_id":"c1","from":"worker:feat/s#s2","to":"dispatcher:c1","kind":"status","body":{"state":"reaped","detail":"PR #8 MERGED","source":"reap"}}`, nowUnix*1000))
	// The tip equal to the merged head is deleted; the diverged one stays.
	if exec.Command("git", "-C", f.main, "show-ref", "--verify", "--quiet", "refs/heads/"+rb).Run() == nil {
		t.Fatal("feat/r was not deleted")
	}
	gitOut(t, f.main, "show-ref", "--verify", "refs/heads/"+sb)
	for _, call := range []string{"gh issue edit 12 --remove-label dispatched", "gh issue edit 13 --remove-label dispatched"} {
		if !slices.Contains(f.calls, call) {
			t.Fatalf("calls %q missing %q", f.calls, call)
		}
	}
	// Only the label that actually came off is released in the claim index.
	idx, err := os.ReadFile(filepath.Join(f.env["XDG_DATA_HOME"], "crew", "claims", "o", "r.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(idx), `"kind":"release","issue":"12"`) || strings.Contains(string(idx), `"issue":"13"`) {
		t.Fatalf("claim index = %s", idx)
	}
}

func TestReapRowAndAnchor(t *testing.T) {
	f := newFx(t)
	anchor, err := wtgit.AnchorPath(f.wt)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(anchor), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(anchor, []byte(rb+"\n"+f.paths.Dir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.bus(status("done", prURL, 0))
	f.gh["pr view "+prURL+" --json state --jq .state"] = "CLOSED"
	f.ghFail["pr view "+prURL+" --json closingIssuesReferences --jq .closingIssuesReferences[].number"] = true
	f.gh["pr view "+prURL+" --json closingIssuesReferences --jq .closingIssuesReferences[].number"] = "GraphQL: boom"
	f.tmux["list-windows -a -F #{window_id} #{pane_current_path} #{@worktree}"] = strings.Join([]string{
		"@1 " + f.wt + " ", "@2 /elsewhere " + f.wt, "@3 " + f.wt + "x /x",
	}, "\n")

	if rc := f.reap(f.main, Request{}); rc != 0 {
		t.Fatalf("rc=%d stderr=%q", rc, f.stderr.String())
	}
	f.wantOut(
		"crew reap: reaped feat/r (CLOSED)",
		"crew reap: could not resolve closing issues for PR "+prURL+" (feat/r): GraphQL: boom",
	)
	if _, err := os.Lstat(anchor); !os.IsNotExist(err) {
		t.Fatalf("anchor still there: %v", err)
	}
	reap := f.rows("reap")
	if len(reap) != 1 {
		t.Fatalf("appended %q", f.appended)
	}
	wantJSON(t, reap[0], fmt.Sprintf(`{"ts":%d,"kind":"reap","branch":"feat/r","pr":%q,"pr_state":"CLOSED","worktree":%q}`, nowUnix*1000, prURL, f.wt))
	for _, w := range []string{"@1", "@2"} {
		if !slices.Contains(f.calls, "tmux kill-window -t "+w) {
			t.Fatalf("calls %q missing kill of %s", f.calls, w)
		}
	}
	if slices.Contains(f.calls, "tmux kill-window -t @3") {
		t.Fatal("killed a window of another path")
	}
	// A CLOSED PR's branch is kept.
	gitOut(t, f.main, "show-ref", "--verify", "refs/heads/"+rb)
}

// rm -f never removes a directory: an anchor planted as one survives a reap
// that still completes.
func TestReapKeepsDirectoryAnchor(t *testing.T) {
	f := newFx(t)
	anchor, err := wtgit.AnchorPath(f.wt)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(anchor, 0o755); err != nil {
		t.Fatal(err)
	}
	f.bus(status("done", prURL, 0))
	f.gh["pr view "+prURL+" --json state --jq .state"] = "CLOSED"
	f.gh["pr view "+prURL+" --json closingIssuesReferences --jq .closingIssuesReferences[].number"] = ""

	if rc := f.reap(f.main, Request{}); rc != 0 {
		t.Fatalf("rc=%d stderr=%q", rc, f.stderr.String())
	}
	if fi, err := os.Lstat(anchor); err != nil || !fi.IsDir() {
		t.Fatalf("anchor directory gone: %v", err)
	}
	if len(f.rows("reap")) != 1 {
		t.Fatalf("appended %q", f.appended)
	}
}

func TestIssueModeReap(t *testing.T) {
	f := newFx(t)
	runGit(t, f.main, "update-ref", "refs/remotes/origin/main", "HEAD")
	f.bus(status("done", "", 0), `{"ts":1,"crew_id":"c1","kind":"claim-issue","branch":"`+rb+`","issue":5}`)
	f.gh["pr list --head feat/r --state all --json url,state,headRefOid,isCrossRepository"] = "[]"
	f.gh["issue view 5 --json state --jq .state"] = "CLOSED"
	if rc := f.reap(f.main, Request{}); rc != 0 {
		t.Fatalf("rc=%d stderr=%q", rc, f.stderr.String())
	}
	f.wantOut("crew reap: reaped feat/r (issue #5 CLOSED)")
	wantJSON(t, f.rows("reap")[0], fmt.Sprintf(`{"ts":%d,"kind":"reap","branch":"feat/r","pr":"","pr_state":"ISSUE_CLOSED","worktree":%q,"issues":["5"]}`, nowUnix*1000, f.wt))
	wantJSON(t, f.rows("status")[0], fmt.Sprintf(`{"ts":%d,"crew_id":"c1","from":"worker:feat/r#s1","to":"dispatcher:c1","kind":"status","body":{"state":"reaped","detail":"issue #5 CLOSED","source":"reap"}}`, nowUnix*1000))
	if !slices.Contains(f.calls, "gh issue edit 5 --remove-label dispatched") {
		t.Fatalf("calls %q", f.calls)
	}
}

// setOriginURL repoints origin and records the guard baseline again.
func (f *fx) setOriginURL(url string) {
	f.t.Helper()
	runGit(f.t, f.main, "remote", "set-url", "origin", url)
	if err := os.Remove(gitguard.BaselinePath(f.paths.Common)); err != nil {
		f.t.Fatal(err)
	}
	var stderr strings.Builder
	if err := gitguard.Init(f.paths.Common, &stderr); err != nil {
		f.t.Fatalf("gitguard.Init: %v\n%s", err, stderr.String())
	}
}

// bareOrigin points origin at a fresh bare repo holding rb at HEAD and
// returns it. remote.*.url is a guarded redirect key, so the baseline is
// recorded again after the change.
func (f *fx) bareOrigin() string {
	f.t.Helper()
	bare := filepath.Join(f.root, "origin.git")
	runGit(f.t, f.root, "init", "-q", "--bare", bare)
	f.setOriginURL(bare)
	runGit(f.t, f.main, "push", "-q", "origin", "HEAD:refs/heads/"+rb)
	return bare
}

// issueOriginFx is an issue-mode scenario whose dispatch row recorded the
// push of rb to a bare origin, plus that origin.
func issueOriginFx(t *testing.T, dispatchRow func(oid string) string) (*fx, string) {
	t.Helper()
	f := newFx(t)
	runGit(t, f.main, "update-ref", "refs/remotes/origin/main", "HEAD")
	bare := f.bareOrigin()
	f.bus(status("done", "", 0), `{"ts":1,"crew_id":"c1","kind":"claim-issue","branch":"`+rb+`","issue":5}`,
		dispatchRow(gitOut(t, f.main, "rev-parse", "HEAD")))
	f.gh["pr list --head feat/r --state all --json url,state,headRefOid,isCrossRepository"] = "[]"
	f.gh["issue view 5 --json state --jq .state"] = "CLOSED"
	return f, bare
}

func pushedRow(oid string) string {
	return `{"ts":2,"crew_id":"c1","kind":"dispatch","branch":"` + rb + `","pushed_oid":"` + oid + `"}`
}

func originHas(bare string) bool {
	return exec.Command("git", "-C", bare, "show-ref", "--verify", "--quiet", "refs/heads/"+rb).Run() == nil
}

func TestIssueModeReapDeletesUntouchedOriginBranch(t *testing.T) {
	f, bare := issueOriginFx(t, pushedRow)
	if rc := f.reap(f.main, Request{}); rc != 0 {
		t.Fatalf("rc=%d stderr=%q", rc, f.stderr.String())
	}
	f.wantOut("crew reap: reaped feat/r (issue #5 CLOSED)", "crew reap: deleted origin branch feat/r (still at its dispatch base)")
	if originHas(bare) {
		t.Fatal("origin feat/r still there")
	}
}

func TestIssueModeReapKeepsMovedOriginBranch(t *testing.T) {
	f, bare := issueOriginFx(t, pushedRow)
	runGit(t, f.main, "commit", "-q", "--allow-empty", "-m", "worker")
	runGit(t, f.main, "push", "-q", "origin", "HEAD:refs/heads/"+rb)
	moved := gitOut(t, bare, "rev-parse", "refs/heads/"+rb)
	if rc := f.reap(f.main, Request{}); rc != 0 {
		t.Fatalf("rc=%d stderr=%q", rc, f.stderr.String())
	}
	f.wantOut("crew reap: reaped feat/r (issue #5 CLOSED)", "crew reap: kept origin branch feat/r — origin moved past its dispatch base")
	if got := gitOut(t, bare, "rev-parse", "refs/heads/"+rb); got != moved {
		t.Fatalf("origin feat/r = %s, want %s", got, moved)
	}
}

func TestIssueModeReapDeletesAfterResume(t *testing.T) {
	f, bare := issueOriginFx(t, pushedRow)
	f.bus(append(strings.Split(strings.TrimSpace(readFile(t, f.paths.Log)), "\n"),
		`{"ts":3,"crew_id":"c1","kind":"dispatch","branch":"`+rb+`","resume":true}`)...)
	if rc := f.reap(f.main, Request{}); rc != 0 {
		t.Fatalf("rc=%d stderr=%q", rc, f.stderr.String())
	}
	f.wantOut("crew reap: reaped feat/r (issue #5 CLOSED)", "crew reap: deleted origin branch feat/r (still at its dispatch base)")
	if originHas(bare) {
		t.Fatal("origin feat/r still there")
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestIssueModeReapNoPushedOID(t *testing.T) {
	f, bare := issueOriginFx(t, func(string) string {
		return `{"ts":2,"crew_id":"c1","kind":"dispatch","branch":"` + rb + `"}`
	})
	if rc := f.reap(f.main, Request{}); rc != 0 {
		t.Fatalf("rc=%d stderr=%q", rc, f.stderr.String())
	}
	f.wantOut("crew reap: reaped feat/r (issue #5 CLOSED)")
	if !originHas(bare) {
		t.Fatal("origin feat/r was deleted")
	}
}

func TestIssueModeReapIgnoresMalformedPushedOID(t *testing.T) {
	f, bare := issueOriginFx(t, func(string) string { return pushedRow("--delete") })
	if rc := f.reap(f.main, Request{}); rc != 0 {
		t.Fatalf("rc=%d stderr=%q", rc, f.stderr.String())
	}
	f.wantOut("crew reap: reaped feat/r (issue #5 CLOSED)")
	if !originHas(bare) {
		t.Fatal("origin feat/r was deleted")
	}
}

func TestIssueModeReapNewestCreateRowWins(t *testing.T) {
	f, bare := issueOriginFx(t, pushedRow)
	f.bus(append(strings.Split(strings.TrimSpace(readFile(t, f.paths.Log)), "\n"),
		`{"ts":3,"crew_id":"c1","kind":"dispatch","branch":"`+rb+`"}`)...)
	if rc := f.reap(f.main, Request{}); rc != 0 {
		t.Fatalf("rc=%d stderr=%q", rc, f.stderr.String())
	}
	f.wantOut("crew reap: reaped feat/r (issue #5 CLOSED)")
	if !originHas(bare) {
		t.Fatal("origin feat/r was deleted")
	}
}

func TestIssueModeReapKeepsForgedUniqueWork(t *testing.T) {
	f, bare := issueOriginFx(t, pushedRow)
	runGit(t, f.main, "commit", "-q", "--allow-empty", "-m", "unique")
	runGit(t, f.main, "push", "-q", "origin", "HEAD:refs/heads/"+rb)
	tip := gitOut(t, bare, "rev-parse", "refs/heads/"+rb)
	runGit(t, f.main, "update-ref", "refs/remotes/origin/"+rb, tip)
	f.bus(status("done", "", 0), `{"ts":1,"crew_id":"c1","kind":"claim-issue","branch":"`+rb+`","issue":5}`, pushedRow(tip))
	if rc := f.reap(f.main, Request{}); rc != 0 {
		t.Fatalf("rc=%d stderr=%q", rc, f.stderr.String())
	}
	f.wantOut("crew reap: reaped feat/r (issue #5 CLOSED)", "crew reap: kept origin branch feat/r — its tip holds commits no other remote branch has")
	if got := gitOut(t, bare, "rev-parse", "refs/heads/"+rb); got != tip {
		t.Fatalf("origin feat/r = %s, want %s", got, tip)
	}
}

func TestDeleteOriginBranchSkipsOriginDefault(t *testing.T) {
	f, bare := issueOriginFx(t, pushedRow)
	runGit(t, f.main, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/"+rb)
	c := f.candidate()
	c.mode = "issue"
	f.run().deleteOriginBranch(context.Background(), c)
	if f.stdout.Len() != 0 || !originHas(bare) {
		t.Fatalf("stdout %q, origin has branch %v", f.stdout.String(), originHas(bare))
	}
}

func TestDeleteOriginBranchSkipsNonFeatBranch(t *testing.T) {
	f, bare := issueOriginFx(t, pushedRow)
	runGit(t, f.main, "push", "-q", "origin", "HEAD:refs/heads/fix/r")
	f.bus(pushedRow(gitOut(t, f.main, "rev-parse", "HEAD")), strings.Replace(pushedRow(gitOut(t, f.main, "rev-parse", "HEAD")), rb, "fix/r", 1))
	c := f.candidate()
	c.mode = "issue"
	c.branch = "fix/r"
	f.run().deleteOriginBranch(context.Background(), c)
	if exec.Command("git", "-C", bare, "show-ref", "--verify", "--quiet", "refs/heads/fix/r").Run() != nil {
		t.Fatal("origin fix/r was deleted")
	}
}

func TestIssueModeReapUnreachableOriginSaysSoUnderQuiet(t *testing.T) {
	f, _ := issueOriginFx(t, pushedRow)
	f.setOriginURL(filepath.Join(f.root, "missing.git"))
	if rc := f.reap(f.main, Request{Quiet: true}); rc != 0 {
		t.Fatalf("rc=%d stderr=%q", rc, f.stderr.String())
	}
	f.wantOut("crew reap: reaped feat/r (issue #5 CLOSED)", "crew reap: could not delete origin branch feat/r (still at its dispatch base?) — delete it by hand once origin is reachable")
}

func TestMergedPRReapLeavesOriginBranch(t *testing.T) {
	f := newFx(t)
	bare := f.bareOrigin()
	f.bus(status("done", prURL, 0), pushedRow(gitOut(t, f.main, "rev-parse", "HEAD")))
	if removed, code := f.run().remove(context.Background(), f.candidate()); !removed || code != 0 {
		t.Fatalf("removed=%v code=%d stderr=%q", removed, code, f.stderr.String())
	}
	if strings.Contains(f.stdout.String(), "origin branch") {
		t.Fatalf("stdout %q", f.stdout.String())
	}
	if !originHas(bare) {
		t.Fatal("origin feat/r was deleted")
	}
}

func TestLeftoverProcesses(t *testing.T) {
	f := newFx(t)
	f.bus(status("done", prURL, 0))
	f.gh["pr view "+prURL+" --json state --jq .state"] = "MERGED"
	f.procs.Procs = map[int]proctest.Proc{
		101:         {Cwd: f.wt},
		102:         {Cwd: f.wt + "/sub (deleted)"},
		103:         {Cwd: f.wt + "x"},
		104:         {Cwd: f.wt},
		os.Getpid(): {Parent: 104, Cwd: f.wt},
	}
	anchor, _ := wtgit.AnchorPath(f.wt)
	f.reap(f.main, Request{DryRun: true})
	f.wantOut(
		"crew reap: would kill pid 101 (cwd "+f.wt+")",
		"crew reap: would kill pid 102 (cwd "+f.wt+"/sub (deleted))",
		"crew reap: would reap feat/r (MERGED) @ "+f.wt,
		"crew reap: would prune record "+anchor,
	)
	if k := f.procs.Killed(); len(k) != 0 {
		t.Fatalf("dry run signalled: %v", k)
	}

	f.reap(f.main, Request{})
	if k := f.procs.Killed(); !slices.Equal(k, []int{101, 102}) {
		t.Fatalf("killed %v, want 101 and 102", k)
	}
	if !strings.HasPrefix(f.stdout.String(), "crew reap: killed pid 101 (cwd "+f.wt+")\ncrew reap: killed pid 102 ") {
		t.Fatalf("stdout %q", f.stdout.String())
	}
}

// r is a bare run for calling remove directly, as the candidate loop would.
func (f *fx) run() *run {
	return &run{paths: f.paths, p: f.opts().Probes, stdout: &f.stdout, stderr: &f.stderr, trusted: f.main, reapCwd: f.main, clean: &cleaner{}}
}

func (f *fx) candidate() *candidate {
	f.t.Helper()
	admin, err := wtgit.AdminDir(f.paths.Common, f.wt)
	if err != nil {
		f.t.Fatal(err)
	}
	return &candidate{latestRow: latestRow{branch: rb}, wtpath: f.wt, admin: admin, mode: "pr", pr: prURL, prState: "MERGED", label: "MERGED"}
}

func TestRemoveKeepsTamperedGitlink(t *testing.T) {
	f := newFx(t)
	c := f.candidate()
	if err := os.WriteFile(f.wt+"/.git", []byte("gitdir: /elsewhere\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if removed, code := f.run().remove(context.Background(), c); removed || code != 0 {
		t.Fatalf("removed=%v code=%d", removed, code)
	}
	f.wantOut("crew reap: keeping feat/r — " + tampered)
	if !filekind.IsDir(f.wt) {
		t.Fatal("worktree removed")
	}
}

func TestRemoveKeepsDirt(t *testing.T) {
	f := newFx(t)
	c := f.candidate()
	if err := os.WriteFile(f.wt+"/late.txt", []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.run().remove(context.Background(), c)
	f.wantOut("crew reap: keeping feat/r — uncommitted changes")
	if len(f.appended) != 0 {
		t.Fatalf("appended %q", f.appended)
	}
}

func TestForget(t *testing.T) {
	f := newFx(t)
	dir := f.paths.Dir
	f.bus(
		status("done", "", 0),
		`{"ts":2,"crew_id":"c1","kind":"dispatch","branch":"`+rb+`","session":"s2"}`,
		`{"ts":3,"crew_id":"c1","from":"role:`+rb+`:lead","kind":"msg"}`,
		`{"ts":4,"crew_id":"c1","from":"role:`+rb+`:lead:x","kind":"msg"}`,
		`{"ts":5,"crew_id":"c1","from":"worker:feat/other#s9","kind":"status","body":{"state":"done"}}`,
	)
	write := func(p, data string) string {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cks := func(id string) string { return strconv.FormatUint(uint64(identity.CKsum([]byte(id))), 10) }
	var gone, kept []string
	gone = append(gone, write(dir+"/grants/"+rb, "g"), write(dir+"/protocol-dirs/"+rb, "p"))
	kept = append(kept, write(dir+"/grants/feat/other", "g"), write(dir+"/artifacts/"+rb+"/discarded-x.patch", "p"))
	// A symlinked category dir is skipped, its target untouched.
	elsewhere := filepath.Join(f.root, "elsewhere")
	kept = append(kept, write(elsewhere+"/feat/r", "l"))
	if err := os.Symlink(elsewhere, dir+"/leads"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"worker:feat/r#s1", "worker:feat/r#s2", "role:feat/r:lead"} {
		gone = append(gone,
			write(marks.Path(dir, "c1", id), "{}"),
			write(marks.LockPath(dir, "c1", id), ""),
			write(dir+"/phase-status/"+sanitize(id)+"."+cks(id), "x"),
			write(dir+"/directive-delivery/"+sanitize(id), "d\n"+id+"\n"),
		)
	}
	other := "worker:feat/other#s9"
	kept = append(kept,
		write(marks.Path(dir, "c1", other), "{}"),
		write(marks.Path(dir, "c1", "role:feat/r:lead:x"), "{}"),
	)
	// A directive-delivery file whose id line names another session stays.
	id := "worker:feat/r#s1"
	if err := os.Remove(dir + "/directive-delivery/" + sanitize(id)); err != nil {
		t.Fatal(err)
	}
	gone = slices.DeleteFunc(gone, func(p string) bool { return p == dir+"/directive-delivery/"+sanitize(id) })
	kept = append(kept, write(dir+"/directive-delivery/"+sanitize(id), "d\nworker:feat/r#s1x\n"))

	f.run().forget(rb)
	for _, p := range gone {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Errorf("%s still there", p)
		}
	}
	for _, p := range kept {
		if _, err := os.Lstat(p); err != nil {
			t.Errorf("%s removed: %v", p, err)
		}
	}
}

func TestChainBad(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(root+"/a/b", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(root+"/f", nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(root+"/a", root+"/l"); err != nil {
		t.Fatal(err)
	}
	cases := []struct{ root, rel, want string }{
		{root, "a/b/c", ""},
		{root, "", ""},
		{root, "a//b", ""},
		{root, "f/x", root + "/f"},
		{root, "l/b", root + "/l"},
		{root + "/l", "b", root + "/l"},
		{root + "/missing", "x/y", ""},
	}
	for _, c := range cases {
		if got := chainBad(c.root, c.rel); got != c.want {
			t.Errorf("chainBad(%q, %q) = %q, want %q", c.root, c.rel, got, c.want)
		}
	}
}

func TestSanitizeAndLine2(t *testing.T) {
	if got := sanitize("worker:feat/r#s1 é"); got != "worker_feat_r_s1___" {
		t.Fatalf("sanitize = %q", got)
	}
	for in, want := range map[string]string{"a\nb\n": "b", "a\nb": "b", "a\n": "", "a": "", "a\nb\nc": "b"} {
		if got := line2(in); got != want {
			t.Errorf("line2(%q) = %q, want %q", in, got, want)
		}
	}
}
