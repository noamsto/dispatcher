package reap

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/tmux/tmuxtest"
)

const portEListKey = "pr list --head feat/r --state all --json url,state,headRefOid,isCrossRepository"

func portEPRKey(url, field string) string {
	if field == "state" {
		return "pr view " + url + " --json state --jq .state"
	}
	return "pr view " + url + " --json headRefOid --jq .headRefOid"
}

// A branch that posts between the first idle sample and the kill-time resample
// keeps the window and the worktree.
func TestPortEBranchPostsBetweenSamplesKept(t *testing.T) {
	idle := idleFrame(t)
	f := newFx(t)
	f.bus(status("done", prURL, 0))
	f.gh[portEPRKey(prURL, "state")] = "MERGED"
	f.tmux[panesKey] = "@5\t%5\tclaude\t" + f.wt
	f.tmux["capture-pane -p -t %5"] = idle
	f.tmux["capture-pane -e -p -t %5"] = idle
	captures := 0
	rc := portCReap(f, f.main, Request{}, func(p *Probes) {
		p.Tmux = &tmuxtest.Fake{Func: func(args []string) (string, error) {
			key := strings.Join(args, " ")
			f.record("tmux " + key)
			if key != "capture-pane -p -t %5" {
				return f.tmux[key], nil
			}
			captures++
			if captures == 2 {
				row := fmt.Sprintf(`{"ts":%d,"crew_id":"c1","from":"worker:%s#s2","to":"dispatcher:c1","kind":"msg","body":"back"}`,
					nowUnix*1000+1000, rb)
				fh, err := os.OpenFile(f.paths.Log, os.O_APPEND|os.O_WRONLY, 0o644)
				if err != nil {
					t.Fatal(err)
				}
				_, err = fh.WriteString(row + "\n")
				if cerr := fh.Close(); err == nil {
					err = cerr
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			return f.tmux[key], nil
		}}
	})
	if rc != 0 {
		t.Fatalf("rc=%d stderr=%q", rc, f.stderr.String())
	}
	if captures < 2 {
		t.Fatalf("no resample: %d captures", captures)
	}
	f.wantOut("crew reap: keeping feat/r — the branch posted since", "crew reap: nothing reclaimed")
	f.portBWantNoCall("kill-window")
	if !portCExists(f.wt) {
		t.Fatal("worktree removed")
	}
}

// A done worker's reap tail removes the worktree: via an earlier session's
// PR, a reap row's PR, or a PR found only through `gh pr list`.
func TestPortEPRSources(t *testing.T) {
	const reapURL = "https://github.com/o/r/pull/6"
	cases := []struct {
		name    string
		bus     func() []string
		url     string
		list    func(tip string) string
		wantDel bool
	}{
		{"earlier session MERGED PR, latest failed without one", func() []string {
			return []string{statusFor(rb, "s1", "done", prURL, 10), statusFor(rb, "s2", "failed", "", 0)}
		}, prURL, func(string) string { return "[]" }, true},
		{"kind:reap row PR", func() []string {
			return []string{statusFor(rb, "s1", "done", "", 0),
				fmt.Sprintf(`{"ts":%d,"kind":"reap","branch":%q,"pr":%q,"pr_state":"MERGED","worktree":"/old/path"}`, nowUnix*1000-1000, rb, reapURL)}
		}, reapURL, func(string) string { return "[]" }, true},
		{"gh pr list only", func() []string {
			return []string{statusFor(rb, "s1", "done", "", 0)}
		}, prURL, func(tip string) string {
			return fmt.Sprintf(`[{"url":%q,"state":"MERGED","headRefOid":%q,"isCrossRepository":false}]`, prURL, tip)
		}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFx(t)
			tip := gitOut(t, f.main, "rev-parse", "refs/heads/"+rb)
			f.bus(c.bus()...)
			f.gh[portEListKey] = c.list(tip)
			f.gh[portEPRKey(c.url, "state")] = "MERGED"
			f.gh[portEPRKey(c.url, "head")] = tip
			if rc := f.reap(f.main, Request{Quiet: true}); rc != 0 {
				t.Fatalf("rc=%d stderr=%q", rc, f.stderr.String())
			}
			f.wantOut("crew reap: reaped feat/r (MERGED)")
			if portCExists(f.wt) {
				t.Fatal("worktree kept")
			}
			if f.portBBranchExists() == c.wantDel {
				t.Fatalf("branch exists=%v", !c.wantDel)
			}
		})
	}
}

// A pr_url that is not this repo's is ignored: it is never `pr view`ed, and
// the GitHub lookup decides.
func TestPortECrossRepoPRURL(t *testing.T) {
	const evil = "https://github.com/evil/other/pull/7"
	t.Run("ignored keeps the worktree, dry run and real", func(t *testing.T) {
		f := newFx(t)
		f.bus(statusFor(rb, "s1", "done", evil, 0))
		f.gh[portEListKey] = "[]"
		f.gh[portEPRKey(evil, "state")] = "MERGED"
		want := []string{"crew reap: keeping feat/r — done but no PR on the bus or GitHub (ignoring cross-repo pr_url " + evil + "); no claim-issue row"}
		for _, req := range []Request{{DryRun: true}, {}} {
			if !req.DryRun {
				want = append(want, "crew reap: nothing reclaimed")
			}
			if rc := f.reap(f.main, req); rc != 0 {
				t.Fatalf("rc=%d stderr=%q", rc, f.stderr.String())
			}
			f.wantOut(want...)
			if !portCExists(f.wt) {
				t.Fatal("worktree removed")
			}
			f.portBWantNoCall("remove-label")
			f.portBWantNoCall("pr view " + evil)
		}
	})
	t.Run("falls through to a same-repo merged PR from gh pr list", func(t *testing.T) {
		f := newFx(t)
		tip := gitOut(t, f.main, "rev-parse", "refs/heads/"+rb)
		f.bus(statusFor(rb, "s1", "done", evil, 0))
		f.gh[portEListKey] = fmt.Sprintf(`[{"url":%q,"state":"MERGED","headRefOid":%q,"isCrossRepository":false}]`, prURL, tip)
		f.gh[portEPRKey(prURL, "state")] = "MERGED"
		f.gh[portEPRKey(prURL, "head")] = tip
		if rc := f.reap(f.main, Request{Quiet: true}); rc != 0 {
			t.Fatalf("rc=%d stderr=%q", rc, f.stderr.String())
		}
		f.wantOut("crew reap: reaped feat/r (MERGED)")
		if portCExists(f.wt) {
			t.Fatal("worktree kept")
		}
		f.portBWantNoCall("pr view " + evil)
	})
	t.Run("github.com pr_url ignored when origin is not github.com", func(t *testing.T) {
		f := newFx(t)
		f.setOriginURL("https://ghe.example.com/o/r.git")
		f.bus(statusFor(rb, "s1", "done", prURL, 0))
		f.gh[portEListKey] = "[]"
		f.gh[portEPRKey(prURL, "state")] = "MERGED"
		if rc := f.reap(f.main, Request{DryRun: true}); rc != 0 {
			t.Fatalf("rc=%d stderr=%q", rc, f.stderr.String())
		}
		f.wantOut("crew reap: keeping feat/r — done but no PR on the bus or GitHub (ignoring cross-repo pr_url " + prURL + "); no claim-issue row")
		if !portCExists(f.wt) {
			t.Fatal("worktree removed")
		}
		f.portBWantNoCall("pr view " + prURL)
	})
}

// A latest session with an empty pr_url does not inherit an earlier session's
// PR as its own: the tip's commits past that PR keep the worker.
func TestPortEEmptyPRURLDoesNotPromoteEarlierPR(t *testing.T) {
	f := newFx(t)
	old := gitOut(t, f.main, "rev-parse", "refs/heads/"+rb)
	runGit(t, f.wt, "commit", "-q", "--allow-empty", "-m", "more")
	f.bus(statusFor(rb, "s1", "done", prURL, 1000), statusFor(rb, "s2", "done", "", 0))
	f.gh[portEListKey] = "[]"
	f.gh[portEPRKey(prURL, "state")] = "MERGED"
	f.gh[portEPRKey(prURL, "head")] = old
	if rc := f.reap(f.main, Request{DryRun: true}); rc != 0 {
		t.Fatalf("rc=%d stderr=%q", rc, f.stderr.String())
	}
	f.wantOut("crew reap: keeping feat/r — its tip has commits past " + prURL)
	if !portCExists(f.wt) {
		t.Fatal("worktree removed")
	}
}

// portEIssue is a done worker with one commit in its worktree, no PR anywhere,
// the claimed issue in state, and (with claim) a claim-issue row for #77.
func portEIssue(f *fx, state string, claim bool) {
	f.t.Helper()
	if err := os.WriteFile(f.wt+"/work.txt", []byte("x\n"), 0o644); err != nil {
		f.t.Fatal(err)
	}
	runGit(f.t, f.wt, "add", "work.txt")
	runGit(f.t, f.wt, "commit", "-q", "-m", "work")
	rows := []string{statusFor(rb, "s1", "done", "", 0)}
	if claim {
		rows = append(rows, fmt.Sprintf(`{"ts":%d,"crew_id":"c1","kind":"claim-issue","issue":"77","branch":%q,"pid":1}`, nowUnix*1000, rb))
	}
	f.bus(rows...)
	f.gh[portEListKey] = "[]"
	f.gh["issue view 77 --json state --jq .state"] = state
}

func TestPortEIssueModeKeeps(t *testing.T) {
	cases := []struct {
		name  string
		state string
		claim bool
		want  string
	}{
		{"closed issue with commits on no remote", "CLOSED", true,
			"crew reap: keeping feat/r — done but no PR on the bus or GitHub; 1 commit(s) not on any remote and not patch-equivalent to origin/main"},
		{"no claim-issue row", "CLOSED", false,
			"crew reap: keeping feat/r — done but no PR on the bus or GitHub; no claim-issue row"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFx(t)
			runGit(t, f.main, "update-ref", "refs/remotes/origin/main", "main")
			portEIssue(f, c.state, c.claim)
			if rc := f.reap(f.main, Request{}); rc != 0 {
				t.Fatalf("rc=%d stderr=%q", rc, f.stderr.String())
			}
			f.wantOut(c.want, "crew reap: nothing reclaimed")
			if !portCExists(f.wt) {
				t.Fatal("worktree removed")
			}
		})
	}
}

// A closed claimed issue whose commits are patch-equivalent to origin/main
// reclaims the worktree and keeps the branch.
func TestPortEClosedIssueReclaimsKeepingBranch(t *testing.T) {
	f := newFx(t)
	portEIssue(f, "CLOSED", true)
	runGit(t, f.main, "cherry-pick", "refs/heads/"+rb)
	runGit(t, f.main, "update-ref", "refs/remotes/origin/main", "main")
	runGit(t, f.main, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
	if rc := f.reap(f.main, Request{Quiet: true}); rc != 0 {
		t.Fatalf("rc=%d stderr=%q", rc, f.stderr.String())
	}
	f.wantOut("crew reap: reaped feat/r (issue #77 CLOSED)")
	if portCExists(f.wt) {
		t.Fatal("worktree kept")
	}
	if !f.portBBranchExists() {
		t.Fatal("branch deleted")
	}
	if !slices.Contains(f.calls, "gh issue edit 77 --remove-label dispatched") {
		t.Fatalf("calls %q", f.calls)
	}
	rows := f.rows("reap")
	if len(rows) != 1 {
		t.Fatalf("rows %q", rows)
	}
	wantJSON(t, rows[0], fmt.Sprintf(`{"ts":%d,"kind":"reap","branch":"feat/r","pr":"","pr_state":"ISSUE_CLOSED","worktree":%q,"issues":["77"]}`, nowUnix*1000, f.wt))
}
