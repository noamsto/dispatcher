package merge_test

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/clock"
	"github.com/noamsto/dispatcher/crew/internal/gh/ghtest"
	"github.com/noamsto/dispatcher/crew/internal/git/gittest"
	"github.com/noamsto/dispatcher/crew/internal/marks"
	"github.com/noamsto/dispatcher/crew/internal/merge"
	"github.com/noamsto/dispatcher/crew/internal/policy"
	"github.com/noamsto/dispatcher/crew/internal/reply"
	"github.com/noamsto/dispatcher/crew/internal/sender"
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
		{name: "corrupt line before the pr_open row", view: view("OPEN", "false", "MERGEABLE", okRoll), runs: okRuns, log: "{not json\n" + okRow, req: merge.Request{DryRun: true}, stdout: "ledger: worker:feat/x#s1-1 validates\n  unread: none for worker:feat/x#s1-1\n", noMerge: true},
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

func TestSenderRefusedBeforeMerge(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"worker", map[string]string{"CREW_WORKER_ID": "worker:feat/x#s1-1"}, "worker or role session"},
		{"role", map[string]string{"CREW_ROLE_ID": "role:feat/x:reviewer"}, "worker or role session"},
		{"overseer", map[string]string{"CREW_OVERSEER": "host1"}, "overseer session"},
		{"dispatcher", nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			paths := bus.Paths{Common: dir, Dir: dir, Log: filepath.Join(dir, "events.jsonl")}
			if err := os.WriteFile(paths.Log, []byte(okRow+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			g := &ghtest.Fake{Out: map[string]string{viewIn: view("OPEN", "false", "MERGEABLE", okRoll), runsIn: okRuns, mergeG: "merged"}}
			gt := &gittest.Fake{Func: func(string, []string) (string, error) { return "", nil }}
			rep := reply.Options{Clock: clock.FromEnv(), Sender: sender.Probes{Getenv: func(k string) string { return tt.env[k] }}}
			var stdout, stderr bytes.Buffer
			code := merge.Run(context.Background(), merge.Request{PR: "7", Crew: "c1"}, paths, &stdout, &stderr, merge.Options{GH: g, Git: gt, Dir: dir, Reply: rep})
			if tt.want == "" {
				if code != 0 {
					t.Fatalf("code = %d, want 0 (stderr %q)", code, stderr.String())
				}
				return
			}
			if code != 1 || !strings.Contains(stderr.String(), "refusing sender 'dispatcher:c1' — ") || !strings.Contains(stderr.String(), tt.want) {
				t.Errorf("code %d stderr %q, want the sender refusal containing %q", code, stderr.String(), tt.want)
			}
			if calls := g.Calls(); len(calls) != 0 {
				t.Errorf("gh calls = %v, want none", calls)
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
	if os.Geteuid() == 0 {
		t.Skip("a read-only log still takes root's append")
	}
	dir := t.TempDir()
	paths := bus.Paths{Common: dir, Dir: dir, Log: filepath.Join(dir, "events.jsonl")}
	if err := os.WriteFile(paths.Log, []byte(okRow+"\n"), 0o444); err != nil {
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

func TestUnreadDirectives(t *testing.T) {
	const (
		sess     = "worker:feat/x#s1-1"
		succ     = "worker:feat/x#s9-9"
		msgToOld = `{"ts":3,"crew_id":"c1","from":"dispatcher:c1","to":"` + sess + `","kind":"msg","body":"fix the flaky\ntest too"}`
		msgToNew = `{"ts":6,"crew_id":"c1","from":"dispatcher:c1","to":"` + succ + `","kind":"msg","body":"rebase please"}`
		working  = `{"ts":5,"crew_id":"c1","from":"` + succ + `","to":"dispatcher:c1","kind":"status","body":{"state":"working"}}`
		exited   = `{"ts":2,"crew_id":"c1","from":"` + sess + `","to":"dispatcher:c1","kind":"status","body":{"state":"exited"}}`
		// bare is a re-dispatch whose session id was never recorded.
		bare = `{"kind":"dispatch","branch":"feat/x","session":null,"ts":5,"crew_id":"c1"}`
		// The re-dispatch ran under crew c2, so its directives come from dispatcher:c2.
		workingC2 = `{"ts":4,"crew_id":"c2","from":"` + succ + `","to":"dispatcher:c2","kind":"status","body":{"state":"working"}}`
		// dispatchC2 is the row that ties the successor to crew c2.
		dispatchC2 = `{"ts":3,"crew_id":"c2","kind":"dispatch","branch":"feat/x","session":"s9-9","worker_id":"` + succ + `"}`
		// forgedZZ is a status a worker posted under a crew with no directives.
		forgedZZ = `{"ts":7,"crew_id":"zz","from":"` + sess + `","to":"dispatcher:zz","kind":"status","body":{"state":"working"}}`
		// escSess is a session whose text before the last # carries an ESC; only the
		// text after the last # is checked as a session id.
		escSess = `worker:feat/x#\u001b[2J#s9-9`
		escDisp = `{"ts":3,"crew_id":"c1","kind":"dispatch","branch":"feat/x","session":"\u001b[2J#s9-9","worker_id":"` + escSess + `"}`
		escMsg  = `{"ts":6,"crew_id":"c1","from":"dispatcher:c1","to":"` + escSess + `","kind":"msg","body":"x"}`
		escBare = `{"kind":"dispatch","branch":"feat/x","session":"\u009b31m\u202e","ts":5,"crew_id":"c1"}`
		msgC2   = `{"ts":3,"crew_id":"c2","from":"dispatcher:c2","to":"` + succ + `","kind":"msg","body":"rebase please"}`
		// badTS is a status whose string ts makes the sessions fold fail.
		badTS  = `{"ts":"\u202e","crew_id":"c1","from":"` + sess + `","to":"dispatcher:c1","kind":"status","body":{"state":"working"}}`
		forged = `{"ts":"%s red","crew_id":"c1","from":"dispatcher:c1","to":"` + sess + `","kind":"msg","body":"x"}`
	)
	tests := []struct {
		name   string
		log    []string
		req    merge.Request
		marked string // a session whose msg ts 3 is already delivered
		// markedCrew is the crew that mark is recorded under; "" is c1.
		markedCrew string
		code       int
		stderr     []string
		notStderr  []string
		stdout     string
		merged     bool
	}{
		{name: "undelivered refuses", log: []string{okRow, msgToOld}, code: 1, stderr: []string{sess, "ts 3", `"fix the flaky test too"`, "crew nudge feat/x"}},
		{name: "delivered merges", log: []string{okRow, msgToOld}, marked: sess, merged: true},
		{name: "dry run lists the check", log: []string{okRow}, req: merge.Request{DryRun: true}, stdout: "  unread: none for " + sess + "\n"},
		{name: "successor undelivered refuses", log: []string{okRow, working, msgToNew}, code: 1, stderr: []string{succ, "ts 6", "rebase please"}},
		{name: "predecessor msg ignored once a successor has none", log: []string{okRow, msgToOld, working}, merged: true},
		{name: "no-ledger still checks", log: []string{okRow, msgToOld}, req: merge.Request{NoLedger: true}, code: 1, stderr: []string{sess}},
		{name: "no-ledger without a pr_open row skips", log: []string{msgToOld}, req: merge.Request{NoLedger: true, DryRun: true}, stdout: "unread: skipped (no worker pr_open for this PR)"},
		{name: "no-ledger without a pr_open row merges", log: []string{msgToOld}, req: merge.Request{NoLedger: true}, merged: true},
		{name: "torn line before the directive refuses", log: []string{okRow, `{torn`, msgToOld}, code: 1, stderr: []string{sess, "ts 3"}},
		{name: "torn line before the directive refuses on dry run", log: []string{okRow, `{torn`, msgToOld}, req: merge.Request{DryRun: true}, code: 1, stderr: []string{sess, "ts 3"}},
		{name: "torn line before the pr_open row, directive after", log: []string{`{torn`, okRow, msgToOld}, code: 1, stderr: []string{sess, "ts 3"}},
		{name: "forged ts is escaped", log: []string{okRow, `{"ts":"\u001b[31mred","crew_id":"c1","from":"dispatcher:c1","to":"` + sess + `","kind":"msg","body":"x"}`}, code: 1, stderr: []string{`ts "\u001b[31mred"`}},
		{name: "terminal session refuses naming re-dispatch", log: []string{okRow, exited, msgToOld}, code: 1, stderr: []string{sess, "exited", "ts 3", "dispatch --pr 7"}, notStderr: []string{"crew nudge"}},
		{name: "session-less newest session refuses", log: []string{okRow, bare, msgToOld}, code: 1, stderr: []string{"no session id", "dispatch --pr 7"}},
		{name: "successor under another crew refuses", log: []string{okRow, dispatchC2, workingC2, msgC2}, code: 1, stderr: []string{succ, "ts 3", "rebase please"}},
		{name: "successor under another crew, delivered, merges", log: []string{okRow, dispatchC2, workingC2, msgC2}, marked: succ, markedCrew: "c2", merged: true},
		{name: "directives under the pr_open crew and the dispatch crew are both checked", log: []string{okRow, dispatchC2, workingC2, msgC2, msgToNew}, marked: succ, markedCrew: "c2", code: 1, stderr: []string{succ, "ts 6", "rebase please"}},
		{name: "fold error is escaped", log: []string{okRow, badTS}, code: 1, stderr: []string{"could not fold", `\u{202E}`}, notStderr: []string{"\u202e"}},
		{name: "forged status under another crew does not hide a directive", log: []string{okRow, msgToOld, forgedZZ}, code: 1, stderr: []string{sess, "ts 3", "fix the flaky"}},
		{name: "session-less session id is escaped", log: []string{okRow, escBare}, code: 1, stderr: []string{"no session id", `\u{009B}`, `\u{202E}`}, notStderr: []string{"\u009b", "\u202e"}},
		{name: "session id with ESC is escaped on the not-delivered path", log: []string{okRow, escDisp, escMsg}, code: 1, stderr: []string{`\u{001B}`, "not yet delivered"}, notStderr: []string{"\x1b"}},
		{name: "session id with ESC is escaped on the pass line", log: []string{okRow, escDisp}, req: merge.Request{DryRun: true}, stdout: `unread: none for worker:feat/x#\u{001B}[2J#s9-9`},
		{name: "invisible code points in the undelivered excerpt are escaped", log: []string{okRow, `{"ts":3,"crew_id":"c1","from":"dispatcher:c1","to":"` + sess + `","kind":"msg","body":"a` + string([]rune{0xFE0F, 0xE0100, 0x034F}) + `b"}`}, code: 1, stderr: []string{`\u{FE0F}`, `\u{E0100}`, `\u{034F}`}, notStderr: []string{string(rune(0xFE0F)), string(rune(0xE0100)), string(rune(0x034F))}},
		{name: "successor lookup ignores --crew", log: []string{okRow, dispatchC2, workingC2, msgC2}, req: merge.Request{Crew: "c1"}, code: 1, stderr: []string{succ, "ts 3"}},
		{name: "forged ts with bidi is escaped", log: []string{okRow, fmt.Sprintf(forged, "\u202e")}, code: 1, stderr: []string{`\u{202E}`}, notStderr: []string{"\u202e"}},
		{name: "forged ts with C1 is escaped", log: []string{okRow, fmt.Sprintf(forged, "\u009b")}, code: 1, stderr: []string{`\u{009B}`}, notStderr: []string{"\u009b"}},
		{name: "empty crew uses the row's crew", log: []string{okRow, msgToOld}, req: merge.Request{Crew: ""}, code: 1, stderr: []string{"dispatcher msg(s) to " + sess}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			paths := bus.Paths{Common: dir, Dir: dir, Log: filepath.Join(dir, "events.jsonl")}
			if err := os.WriteFile(paths.Log, []byte(strings.Join(tt.log, "\n")+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if tt.marked != "" {
				crew := cmp.Or(tt.markedCrew, "c1")
				if !marks.RecordHeld(dir, crew, tt.marked, []any{map[string]any{"from": "dispatcher:" + crew, "ts": 3}}) {
					t.Fatal("could not record the delivered mark")
				}
			}
			g := &ghtest.Fake{Out: map[string]string{viewIn: view("OPEN", "false", "MERGEABLE", okRoll), runsIn: okRuns, mergeG: "merged"}}
			gt := &gittest.Fake{Func: func(string, []string) (string, error) { return "", nil }}
			req := tt.req
			req.PR = "7"
			var stdout, stderr bytes.Buffer
			code := merge.Run(context.Background(), req, paths, &stdout, &stderr, merge.Options{GH: g, Git: gt, Dir: dir, Reply: reply.Options{Clock: clock.FromEnv()}})
			if code != tt.code {
				t.Errorf("code = %d, want %d (stderr %q)", code, tt.code, stderr.String())
			}
			for _, w := range tt.stderr {
				if !strings.Contains(stderr.String(), w) {
					t.Errorf("stderr = %q, want it to contain %q", stderr.String(), w)
				}
			}
			for _, w := range tt.notStderr {
				if strings.Contains(stderr.String(), w) {
					t.Errorf("stderr = %q, want it free of %q", stderr.String(), w)
				}
			}
			if !strings.Contains(stdout.String(), tt.stdout) {
				t.Errorf("stdout = %q, want %q", stdout.String(), tt.stdout)
			}
			var merged bool
			for _, c := range g.Calls() {
				merged = merged || strings.HasPrefix(c, "pr merge")
			}
			if merged != tt.merged {
				t.Errorf("merged = %v, want %v (calls %v)", merged, tt.merged, g.Calls())
			}
		})
	}
}

func TestPolicy(t *testing.T) {
	rec := policy.Record{Name: "night", Text: "merge when green", By: "owner", Host: "h1", At: "2026-11-19T01:02:03Z"}
	const prov = `'night' set by owner@h1 at 2026-11-19T01:02:03Z: "merge when green"`
	load := func(rec policy.Record, err error) func(string) (policy.Record, error) {
		return func(string) (policy.Record, error) { return rec, err }
	}
	tests := []struct {
		name                string
		policy              func(string) (policy.Record, error)
		dryRun              bool
		noLedger, allowNoCI bool
		code                int
		stderr              string
		stdout              string
		noCalls             bool
		release             string
	}{
		{name: "no-ledger waiver", policy: load(rec, nil), noLedger: true, code: 1, noCalls: true,
			stderr: "crew: merge: refusing PR 7 — --policy never waives a gate: drop --no-ledger/--allow-no-ci, or merge on the human's own approval\n"},
		{name: "allow-no-ci waiver", policy: load(rec, nil), allowNoCI: true, code: 1, noCalls: true,
			stderr: "crew: merge: refusing PR 7 — --policy never waives a gate: drop --no-ledger/--allow-no-ci, or merge on the human's own approval\n"},
		{name: "missing", policy: load(policy.Record{}, fmt.Errorf("%w night in this repo", policy.ErrMissing)), code: 1, noCalls: true,
			stderr: "crew: merge: refusing PR 7 — standing policy 'night' is not in force: no standing policy 'night' in this repo\n"},
		{name: "refused", policy: load(policy.Record{}, errors.New("the file f is owned by uid 1, not this uid 2")), code: 1, noCalls: true,
			stderr: "standing policy 'night' is not in force: the file f is owned by uid 1, not this uid 2"},
		{name: "no loader", code: 1, noCalls: true, stderr: "standing policy 'night' is not in force"},
		{name: "dry run", policy: load(rec, nil), dryRun: true, stdout: "  policy: " + prov + "\n"},
		{name: "merge", policy: load(rec, nil), stdout: "merged under standing policy " + prov + "\n", release: "approved — PR #7 is merged under standing policy 'night'; post done"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			paths := bus.Paths{Common: dir, Dir: dir, Log: filepath.Join(dir, "events.jsonl")}
			if err := os.WriteFile(paths.Log, []byte(okRow+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			g := &ghtest.Fake{Out: map[string]string{viewIn: view("OPEN", "false", "MERGEABLE", okRoll), runsIn: okRuns, mergeG: "merged"}}
			gt := &gittest.Fake{Func: func(string, []string) (string, error) { return "", nil }}
			var stdout, stderr bytes.Buffer
			code := merge.Run(context.Background(), merge.Request{PR: "7", Crew: "c1", Policy: "night", DryRun: tt.dryRun, NoLedger: tt.noLedger, AllowNoCI: tt.allowNoCI}, paths, &stdout, &stderr,
				merge.Options{GH: g, Git: gt, Dir: dir, Reply: reply.Options{Clock: clock.FromEnv()}, Policy: tt.policy})
			if code != tt.code {
				t.Fatalf("code = %d, want %d (stderr %q)", code, tt.code, stderr.String())
			}
			if !strings.Contains(stderr.String(), tt.stderr) || (tt.stderr == "" && stderr.Len() != 0) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), tt.stderr)
			}
			if !strings.Contains(stdout.String(), tt.stdout) {
				t.Errorf("stdout = %q, want it to contain %q", stdout.String(), tt.stdout)
			}
			if tt.noCalls && len(g.Calls()) != 0 {
				t.Errorf("gh calls = %v, want none", g.Calls())
			}
			data, err := os.ReadFile(paths.Log)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Contains(string(data), tt.release); tt.release != "" && !got {
				t.Errorf("bus lacks the release %q:\n%s", tt.release, data)
			}
		})
	}
}

func TestReleaseBodyWithoutPolicyIsUnchanged(t *testing.T) {
	dir := t.TempDir()
	paths := bus.Paths{Common: dir, Dir: dir, Log: filepath.Join(dir, "events.jsonl")}
	if err := os.WriteFile(paths.Log, []byte(okRow+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g := &ghtest.Fake{Out: map[string]string{viewIn: view("OPEN", "false", "MERGEABLE", okRoll), runsIn: okRuns, mergeG: "merged"}}
	gt := &gittest.Fake{Func: func(string, []string) (string, error) { return "", nil }}
	var stdout, stderr bytes.Buffer
	if code := merge.Run(context.Background(), merge.Request{PR: "7", Crew: "c1"}, paths, &stdout, &stderr, merge.Options{GH: g, Git: gt, Dir: dir, Reply: reply.Options{Clock: clock.FromEnv()}}); code != 0 {
		t.Fatalf("code %d stderr %q", code, stderr.String())
	}
	data, err := os.ReadFile(paths.Log)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "approved — PR #7 is merged; post done") || strings.Contains(stdout.String(), "standing policy") {
		t.Errorf("stdout %q, bus %s", stdout.String(), data)
	}
}
