package merge_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/clock"
	"github.com/noamsto/dispatcher/crew/internal/gh/ghtest"
	"github.com/noamsto/dispatcher/crew/internal/git/gittest"
	"github.com/noamsto/dispatcher/crew/internal/merge"
	"github.com/noamsto/dispatcher/crew/internal/reply"
)

const (
	prURL     = "https://github.com/o/r/pull/7"
	viewIn    = "pr view 7 --json state,isDraft,mergeable,headRefOid,baseRefName,url,statusCheckRollup"
	runsIn    = "api --paginate repos/{owner}/{repo}/commits/main/check-runs?per_page=100 --jq .check_runs[] | [.name, .status, .conclusion, .details_url] | @tsv"
	mergeG    = "pr merge 7 --squash --match-head-commit abc123"
	okRoll    = `[{"__typename":"CheckRun","name":"check","status":"COMPLETED","conclusion":"SUCCESS"}]`
	okRuns    = "check\tcompleted\tsuccess\thttps://github.com/o/r/actions/runs/9/job/1\ndeploy\tin_progress\t\t"
	laterDone = `{"ts":2,"crew_id":"c1","from":"worker:feat/x#s1-1","to":"dispatcher:c1","kind":"status","body":{"state":"done"}}`
	okRow     = `{"ts":1,"crew_id":"c1","from":"worker:feat/x#s1-1","to":"dispatcher:c1","kind":"status","body":{"state":"pr_open","detail":"AC1 pass(bats)","pr_url":"` + prURL + `"}}`
)

// jobIn is the argv of the Actions jobs lookup a failing base check run costs.
func jobIn(id string) string {
	return "api repos/{owner}/{repo}/actions/jobs/" + id + " --jq [.runner_name, (.steps | length)] | @tsv"
}

const (
	jobURL    = "https://github.com/o/r/actions/runs/9/job/"
	runFailed = "deploy\tcompleted\tfailure\t" + jobURL
	// startedJob ran on a runner and failed; neverJob never got a runner.
	startedJob = "GitHub Actions 1000025092\t3"
	neverJob   = "\t0"
)

func view(state, draft, mergeable, roll string) string {
	return fmt.Sprintf(`{"state":%q,"isDraft":%s,"mergeable":%q,"headRefOid":"abc123","baseRefName":"main","url":%q,"statusCheckRollup":%s}`, state, draft, mergeable, prURL, roll)
}

func TestRun(t *testing.T) {
	tests := []struct {
		name      string
		view      string
		runs      string
		jobs      map[string]string
		log       string
		req       merge.Request
		code      int
		stderr    string
		stdout    string
		merged    bool
		noMerge   bool
		noJobCall bool
		msgs      int
	}{
		{name: "success", view: view("OPEN", "false", "MERGEABLE", okRoll), runs: okRuns, log: okRow, stdout: "released worker:feat/x#s1-1", merged: true, msgs: 1},
		{name: "draft", view: view("OPEN", "true", "MERGEABLE", okRoll), runs: okRuns, log: okRow, code: 1, stderr: "draft"},
		{name: "closed", view: view("CLOSED", "false", "MERGEABLE", okRoll), runs: okRuns, log: okRow, code: 1, stderr: "closed, not OPEN"},
		{name: "conflicting", view: view("OPEN", "false", "CONFLICTING", okRoll), runs: okRuns, log: okRow, code: 1, stderr: "conflicting"},
		{name: "unknown mergeable", view: view("OPEN", "false", "UNKNOWN", okRoll), runs: okRuns, log: okRow, code: 1, stderr: "still computing mergeability; retry"},
		{name: "pending head check", view: view("OPEN", "false", "MERGEABLE", `[{"__typename":"CheckRun","name":"check","status":"IN_PROGRESS","conclusion":""}]`), runs: okRuns, log: okRow, code: 1, stderr: "checks still running"},
		{name: "failed head check", view: view("OPEN", "false", "MERGEABLE", `[{"__typename":"CheckRun","name":"lint","status":"COMPLETED","conclusion":"FAILURE"}]`), runs: okRuns, log: okRow, code: 1, stderr: "head check lint is failure"},
		{name: "pending status context", view: view("OPEN", "false", "MERGEABLE", `[{"__typename":"StatusContext","context":"ci/x","state":"PENDING"}]`), runs: okRuns, log: okRow, code: 1, stderr: "checks still running"},
		{name: "skipped and neutral pass", view: view("OPEN", "false", "MERGEABLE", `[{"name":"a","status":"COMPLETED","conclusion":"SKIPPED"},{"name":"b","status":"COMPLETED","conclusion":"NEUTRAL"}]`), runs: okRuns, log: okRow, merged: true, stdout: "merged"},
		{name: "zero checks", view: view("OPEN", "false", "MERGEABLE", `[]`), runs: okRuns, log: okRow, code: 1, stderr: "--allow-no-ci"},
		{name: "zero checks allowed", view: view("OPEN", "false", "MERGEABLE", `[]`), runs: okRuns, log: okRow, req: merge.Request{AllowNoCI: true}, merged: true, stdout: "merged"},
		{name: "red base", view: view("OPEN", "false", "MERGEABLE", okRoll), runs: runFailed + jobURL + "7", jobs: map[string]string{jobIn("7"): startedJob}, log: okRow, code: 1, stderr: "base main is red (deploy: failure)"},
		{name: "never-started failure ignored", view: view("OPEN", "false", "MERGEABLE", okRoll), runs: runFailed + jobURL + "2", jobs: map[string]string{jobIn("2"): neverJob}, log: okRow, req: merge.Request{DryRun: true}, stdout: "base main: not red (1 check run never started — ignored)\n", noMerge: true},
		{name: "never-started failure still merges", view: view("OPEN", "false", "MERGEABLE", okRoll), runs: runFailed + jobURL + "2", jobs: map[string]string{jobIn("2"): neverJob}, log: okRow, merged: true},
		{name: "two never-started failures ignored", view: view("OPEN", "false", "MERGEABLE", okRoll), runs: "a\tcompleted\tfailure\t" + jobURL + "4\nb\tcompleted\tfailure\t" + jobURL + "5", jobs: map[string]string{jobIn("4"): neverJob, jobIn("5"): neverJob}, log: okRow, req: merge.Request{DryRun: true}, stdout: "base main: not red (2 check runs never started — ignored)\n", noMerge: true},
		{name: "never-started failure does not mask a red run", view: view("OPEN", "false", "MERGEABLE", okRoll), runs: "a\tcompleted\tfailure\t" + jobURL + "2\nb\tcompleted\tfailure\t" + jobURL + "3", jobs: map[string]string{jobIn("2"): neverJob, jobIn("3"): startedJob}, log: okRow, code: 1, stderr: "base main is red (b: failure)"},
		{name: "failed job with no answer stays red", view: view("OPEN", "false", "MERGEABLE", okRoll), runs: runFailed + jobURL + "2", log: okRow, code: 1, stderr: "base main is red (deploy: failure)"},
		{name: "startup_failure stays red without a job lookup", view: view("OPEN", "false", "MERGEABLE", okRoll), runs: "deploy\tcompleted\tstartup_failure\t" + jobURL + "6", log: okRow, code: 1, stderr: "base main is red (deploy: startup_failure)", noJobCall: true},
		{name: "external check run failure stays red", view: view("OPEN", "false", "MERGEABLE", okRoll), runs: "snyk\tcompleted\tfailure\thttps://app.example.com/checks/12", log: okRow, code: 1, stderr: "base main is red (snyk: failure)", noJobCall: true},
		{name: "missing pr_open row", view: view("OPEN", "false", "MERGEABLE", okRoll), runs: okRuns, code: 1, stderr: "no pr_open ledger"},
		{name: "missing row with --no-ledger", view: view("OPEN", "false", "MERGEABLE", okRoll), runs: okRuns, req: merge.Request{NoLedger: true}, merged: true, stdout: "merged"},
		{name: "row of another crew", view: view("OPEN", "false", "MERGEABLE", okRoll), runs: okRuns, log: okRow, req: merge.Request{Crew: "c2"}, code: 1, stderr: "no pr_open ledger"},
		{name: "invalid ledger", view: view("OPEN", "false", "MERGEABLE", okRoll), runs: okRuns, log: strings.Replace(okRow, "AC1 pass(bats)", "AC1 pending", 1), code: 1, stderr: "no longer validates"},
		{name: "newest row wins", view: view("OPEN", "false", "MERGEABLE", okRoll), runs: okRuns, log: strings.Replace(okRow, "AC1 pass(bats)", "AC1 pending", 1) + "\n" + okRow, merged: true, stdout: "merged"},
		{name: "dry run", view: view("OPEN", "false", "MERGEABLE", okRoll), runs: okRuns, log: okRow, req: merge.Request{DryRun: true}, stdout: "ok: all checks passed\n  pr: open, ready, mergeable\n  head check check: success\n  base main: not red\n  ledger: worker:feat/x#s1-1 validates\n", noMerge: true},
		{name: "worker moved on to done", view: view("OPEN", "false", "MERGEABLE", okRoll), runs: okRuns, log: okRow + "\n" + laterDone, merged: true, stdout: "merged"},
		{name: "bad pr", req: merge.Request{PR: "x"}, code: 1, stderr: "usage: crew merge"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			paths := bus.Paths{Common: dir, Dir: dir, Log: filepath.Join(dir, "events.jsonl")}
			if tt.log != "" {
				if err := os.WriteFile(paths.Log, []byte(tt.log+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			g := &ghtest.Fake{Out: map[string]string{viewIn: tt.view, runsIn: tt.runs, mergeG: "merged"}}
			for k, v := range tt.jobs {
				g.Out[k] = v
			}
			git := &gittest.Fake{Func: func(string, []string) (string, error) { return "", nil }}
			req := tt.req
			if req.PR == "" {
				req.PR = "7"
			}
			var stdout, stderr bytes.Buffer
			code := merge.Run(context.Background(), req, paths, &stdout, &stderr, merge.Options{GH: g, Git: git, Dir: dir, Reply: reply.Options{Clock: clock.FromEnv()}})
			if code != tt.code {
				t.Errorf("code = %d, want %d (stderr %q)", code, tt.code, stderr.String())
			}
			if !strings.Contains(stderr.String(), tt.stderr) || (tt.stderr == "" && stderr.Len() != 0) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), tt.stderr)
			}
			if tt.stdout != "" && !strings.Contains(stdout.String(), tt.stdout) {
				t.Errorf("stdout = %q, want %q", stdout.String(), tt.stdout)
			}
			var merged bool
			for _, c := range g.Calls() {
				if strings.HasPrefix(c, "pr merge") {
					merged = true
					if c != mergeG {
						t.Errorf("merge argv = %q, want %q", c, mergeG)
					}
				}
			}
			if merged != tt.merged {
				t.Errorf("merged = %v, want %v (calls %v)", merged, tt.merged, g.Calls())
			}
			if tt.noJobCall {
				for _, c := range g.Calls() {
					if strings.Contains(c, "actions/jobs/") {
						t.Errorf("unexpected jobs lookup %q", c)
					}
				}
			}
		})
	}
}

func TestLedgerAgainstTaskDoc(t *testing.T) {
	dir := t.TempDir()
	wt := filepath.Join(dir, "wt")
	if err := os.Mkdir(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	// An acceptance list the ledger's CI item must satisfy: AC1 names CI.
	doc := "## Acceptance\n\n- AC1: CI (`check` workflow) green on the PR head\n"
	if err := os.WriteFile(filepath.Join(wt, "WORKER_TASK.md"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	paths := bus.Paths{Common: dir, Dir: dir, Log: filepath.Join(dir, "events.jsonl")}
	if err := os.WriteFile(paths.Log, []byte(okRow+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g := &ghtest.Fake{Out: map[string]string{viewIn: view("OPEN", "false", "MERGEABLE", okRoll), runsIn: okRuns, mergeG: "merged"}}
	gt := &gittest.Fake{Func: func(string, []string) (string, error) {
		return "worktree " + wt + "\nHEAD x\nbranch refs/heads/feat/x\n", nil
	}}
	var stdout, stderr bytes.Buffer
	code := merge.Run(context.Background(), merge.Request{PR: "7"}, paths, &stdout, &stderr, merge.Options{GH: g, Git: gt, Dir: dir, Reply: reply.Options{Clock: clock.FromEnv()}})
	if code != 1 || !strings.Contains(stderr.String(), "CI item") {
		t.Errorf("code %d stderr %q, want the CI-evidence refusal", code, stderr.String())
	}
}

func TestTaskDocEdges(t *testing.T) {
	dir := t.TempDir()
	wt := filepath.Join(dir, "wt")
	if err := os.Mkdir(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	paths := bus.Paths{Common: dir, Dir: dir, Log: filepath.Join(dir, "events.jsonl")}
	if err := os.WriteFile(paths.Log, []byte(strings.Replace(okRow, "AC1 pass(bats)", "AC1 pending", 1)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g := &ghtest.Fake{Out: map[string]string{viewIn: view("OPEN", "false", "MERGEABLE", okRoll), runsIn: okRuns, mergeG: "merged"}}
	wtList := func(string, []string) (string, error) {
		return "worktree " + wt + "\nHEAD x\nbranch refs/heads/feat/x\n", nil
	}
	run := func(gt *gittest.Fake) (int, string) {
		var stdout, stderr bytes.Buffer
		code := merge.Run(context.Background(), merge.Request{PR: "7"}, paths, &stdout, &stderr, merge.Options{GH: g, Git: gt, Dir: dir, Reply: reply.Options{Clock: clock.FromEnv()}})
		return code, stderr.String()
	}

	if code, msg := run(&gittest.Fake{}); code != 1 || !strings.Contains(msg, "could not locate the task doc") {
		t.Errorf("git failure: code %d stderr %q", code, msg)
	}
	if err := os.WriteFile(filepath.Join(wt, "WORKER_TASK.md"), []byte("kind: docs\n## Acceptance\n- AC1: x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, msg := run(&gittest.Fake{Func: wtList}); code != 0 {
		t.Errorf("non-implement kind: code %d stderr %q, want the ledger skipped", code, msg)
	}
	if err := os.Chmod(filepath.Join(wt, "WORKER_TASK.md"), 0); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() != 0 {
		if err := os.WriteFile(paths.Log, []byte(okRow+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if code, msg := run(&gittest.Fake{Func: wtList}); code != 1 || !strings.Contains(msg, "could not read the task doc's acceptance list") {
			t.Errorf("unreadable doc: code %d stderr %q", code, msg)
		}
	}
}

func TestReleaseFailureStillExitsZero(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "file")
	if err := os.WriteFile(blocker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	paths := bus.Paths{Common: dir, Dir: filepath.Join(blocker, "bus"), Log: filepath.Join(dir, "events.jsonl")}
	if err := os.WriteFile(paths.Log, []byte(okRow+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g := &ghtest.Fake{Out: map[string]string{viewIn: view("OPEN", "false", "MERGEABLE", okRoll), runsIn: okRuns, mergeG: "merged"}}
	gt := &gittest.Fake{Func: func(string, []string) (string, error) { return "", nil }}
	var stdout, stderr bytes.Buffer
	code := merge.Run(context.Background(), merge.Request{PR: "7"}, paths, &stdout, &stderr, merge.Options{GH: g, Git: gt, Dir: dir, Reply: reply.Options{Clock: clock.FromEnv()}})
	if code != 0 || !strings.Contains(stderr.String(), "PR merged, but could not release worker:feat/x#s1-1") {
		t.Errorf("code %d stderr %q, want exit 0 with the release warning", code, stderr.String())
	}
}
