package reap

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/clock"
	"github.com/noamsto/dispatcher/crew/internal/gh/ghtest"
	"github.com/noamsto/dispatcher/crew/internal/git"
	"github.com/noamsto/dispatcher/crew/internal/gitguard"
	"github.com/noamsto/dispatcher/crew/internal/lock"
	"github.com/noamsto/dispatcher/crew/internal/proc/proctest"
	"github.com/noamsto/dispatcher/crew/internal/tmux/tmuxtest"
	"github.com/noamsto/dispatcher/crew/internal/wtgit"
)

const (
	rb      = "feat/r"
	prURL   = "https://github.com/o/r/pull/7"
	nowUnix = int64(1_800_000_000)
)

// fx is one reap scenario: a real repo with an origin on github.com, a
// baseline, a linked worktree on rb and a bus, with fake tmux and gh.
type fx struct {
	t              *testing.T
	root, main, wt string
	paths          bus.Paths
	env            map[string]string
	gh             map[string]string // `gh <args>` → stdout
	ghFail         map[string]bool
	tmux           map[string]string // `tmux <args>` → stdout
	noGh           bool
	appendErr      error
	// appendFail, when set, fails the append of the lines it returns an error for.
	appendFail     func(line string) error
	procs          *proctest.Table
	mu             sync.Mutex
	calls          []string
	ghCwd          []string
	appended       []string
	sleeps         int
	rateCwd        []string
	stdout, stderr bytes.Buffer
}

func newFx(t *testing.T) *fx {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", root)
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(root, "claude"))
	t.Setenv("TMUX_PANE", "")
	f := &fx{
		t: t, root: root, main: filepath.Join(root, "main"), wt: filepath.Join(root, "wt"),
		env:    map[string]string{"CREW_RATE_AUTOSWEEP": "0", "XDG_DATA_HOME": filepath.Join(root, "data")},
		gh:     map[string]string{},
		ghFail: map[string]bool{},
		tmux:   map[string]string{},
		procs:  &proctest.Table{},
	}
	if err := os.Mkdir(f.main, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, f.main, "init", "-q", "-b", "main")
	runGit(t, f.main, "commit", "-q", "--allow-empty", "-m", "init")
	runGit(t, f.main, "remote", "add", "origin", "https://github.com/o/r.git")
	runGit(t, f.main, "worktree", "add", "-q", "-b", rb, f.wt)
	common := filepath.Join(f.main, ".git")
	var stderr bytes.Buffer
	if err := gitguard.Init(common, &stderr); err != nil {
		t.Fatalf("gitguard.Init: %v\n%s", err, stderr.String())
	}
	f.paths = bus.Paths{Common: common, Dir: common + "/crew", Log: common + "/crew/events.jsonl"}
	if err := os.MkdirAll(f.paths.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(f.main)
	return f
}

func (f *fx) record(s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, s)
}

func (f *fx) ghCall(args []string) (string, error) {
	key := strings.Join(args, " ")
	if key == "--version" {
		if f.noGh {
			return "", exec.ErrNotFound
		}
		return "", nil
	}
	cwd, _ := os.Getwd()
	f.mu.Lock()
	f.calls = append(f.calls, "gh "+key)
	f.ghCwd = append(f.ghCwd, cwd)
	f.mu.Unlock()
	if f.ghFail[key] {
		return f.gh[key], errors.New("exit status 1")
	}
	return f.gh[key], nil
}

func (f *fx) opts() Options {
	return Options{
		Probes: Probes{
			Git: git.Exec{},
			Tmux: &tmuxtest.Fake{Func: func(args []string) (string, error) {
				key := strings.Join(args, " ")
				f.record("tmux " + key)
				return f.tmux[key], nil
			}},
			Gh: &ghtest.Fake{Func: f.ghCall},
			Gtrash: func(_ context.Context, p string) {
				f.record("gtrash put " + p)
				_ = os.RemoveAll(p)
			},
			Proc: f.procs,
			Sleep: func(time.Duration) {
				f.mu.Lock()
				defer f.mu.Unlock()
				f.sleeps++
			},
			Clock: clock.Clock{Now: func() time.Time { return time.Unix(nowUnix, 0) }},
			Append: func(path, line string) error {
				f.mu.Lock()
				defer f.mu.Unlock()
				if f.appendErr != nil {
					return f.appendErr
				}
				if f.appendFail != nil {
					if err := f.appendFail(line); err != nil {
						return err
					}
				}
				f.appended = append(f.appended, line)
				return nil
			},
			Getenv: func(k string) string { return f.env[k] },
		},
		PID:  os.Getpid(),
		Self: "/nonexistent/crew-go",
		RateRun: func(_ []string, _ bus.Paths, cwd string, _, _ io.Writer) int {
			f.record("rate")
			f.rateCwd = append(f.rateCwd, cwd)
			return 0
		},
	}
}

// reap runs `crew reap` from cwd with req.
func (f *fx) reap(cwd string, req Request) int {
	f.t.Helper()
	f.stdout.Reset()
	f.stderr.Reset()
	return Run(context.Background(), req, f.paths, cwd, &f.stdout, &f.stderr, f.opts())
}

func (f *fx) bus(rows ...string) {
	f.t.Helper()
	if err := os.WriteFile(f.paths.Log, []byte(strings.Join(rows, "\n")+"\n"), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

// status is a worker status row for rb, ageSec seconds before the fake now.
func status(state, pr string, ageSec int64) string {
	body := `{"state":"` + state + `"`
	if pr != "" {
		body += `,"pr_url":"` + pr + `"`
	}
	return fmt.Sprintf(`{"ts":%d,"crew_id":"c1","from":"worker:%s#s1","to":"dispatcher:c1","kind":"status","body":%s}}`,
		(nowUnix-ageSec)*1000, rb, body)
}

func (f *fx) wantOut(want ...string) {
	f.t.Helper()
	var lines []string
	if s := strings.TrimSuffix(f.stdout.String(), "\n"); s != "" {
		lines = strings.Split(s, "\n")
	}
	if !slices.Equal(lines, want) {
		f.t.Fatalf("stdout:\n%s\nwant:\n%s\nstderr:\n%s", f.stdout.String(), strings.Join(want, "\n"), f.stderr.String())
	}
}

func (f *fx) wantErr(want string) {
	f.t.Helper()
	if got := f.stderr.String(); got != want {
		f.t.Fatalf("stderr %q, want %q", got, want)
	}
}

func TestMissingLog(t *testing.T) {
	f := newFx(t)
	if rc := f.reap(f.main, Request{}); rc != 0 {
		t.Fatalf("rc=%d, want 0", rc)
	}
	f.wantOut()
	f.wantErr("")
	if rc := f.reap(f.main, Request{Discard: rb}); rc != 1 {
		t.Fatalf("--discard rc=%d, want 1", rc)
	}
	f.wantErr("crew reap: refusing --discard feat/r — latest status is none\n")
}

// The cwd-discovered calls run from the trusted dir, not the caller's worker
// worktree (#633), while the caller's cwd still decides the current worktree.
func TestTrustedCwdChdir(t *testing.T) {
	f := newFx(t)
	rec, err := wtgit.AnchorPath(f.wt)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(rec), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rec, []byte(rb+"\n"+f.paths.Dir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.bus(status("done", prURL, 0))
	f.gh["pr view "+prURL+" --json state --jq .state"] = "OPEN"
	if rc := f.reap(f.wt, Request{}); rc != 0 {
		t.Fatalf("rc=%d stderr=%q", rc, f.stderr.String())
	}
	f.wantOut("crew reap: keeping feat/r — PR OPEN", "crew reap: nothing reclaimed")
	if len(f.ghCwd) != 1 || f.ghCwd[0] != f.main {
		t.Fatalf("gh ran from %q, want %q", f.ghCwd, f.main)
	}
	if cwd, _ := os.Getwd(); cwd != f.main {
		t.Fatalf("process cwd %q, want %q", cwd, f.main)
	}
}

func TestTrustedCwdRefusalExits1(t *testing.T) {
	f := newFx(t)
	rec, _ := wtgit.AnchorPath(f.wt)
	_ = os.MkdirAll(filepath.Dir(rec), 0o755)
	if err := os.WriteFile(rec, []byte(rb+"\n/elsewhere/crew\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.bus(status("done", prURL, 0))
	if rc := f.reap(f.wt, Request{}); rc != 1 {
		t.Fatalf("rc=%d, want 1", rc)
	}
	if !strings.Contains(f.stderr.String(), "possible tampering") || len(f.calls) != 0 {
		t.Fatalf("stderr %q calls %q", f.stderr.String(), f.calls)
	}
}

func TestLockBusy(t *testing.T) {
	f := newFx(t)
	f.bus()
	ld := f.paths.Dir + "/reap.lock.d"
	if !lock.Acquire(ld, "1") {
		t.Fatal("could not plant the reap lock")
	}
	t.Cleanup(func() { lock.Release(ld) })

	if rc := f.reap(f.main, Request{NoWait: true}); rc != 0 {
		t.Fatalf("rc=%d", rc)
	}
	f.wantOut("crew reap: another reap is running — skipped")
	if rc := f.reap(f.main, Request{NoWait: true, Quiet: true}); rc != 0 {
		t.Fatalf("rc=%d", rc)
	}
	f.wantOut()
	if rc := f.reap(f.main, Request{NoWait: true, Discard: rb}); rc != 1 {
		t.Fatalf("--discard rc=%d", rc)
	}
	f.wantErr("crew reap: refusing --discard feat/r — another reap is running\n")

	f.sleeps = 0
	if rc := f.reap(f.main, Request{}); rc != 0 {
		t.Fatalf("rc=%d", rc)
	}
	f.wantOut("crew reap: another reap is still running after 120s — skipped")
	if f.sleeps != 120 {
		t.Fatalf("slept %d times, want 120", f.sleeps)
	}
}

func TestReleasesLockOnReturn(t *testing.T) {
	f := newFx(t)
	f.bus()
	if rc := f.reap(f.main, Request{}); rc != 0 {
		t.Fatalf("rc=%d", rc)
	}
	f.wantOut("crew reap: nothing done to reap")
	ld := f.paths.Dir + "/reap.lock.d"
	if !lock.Acquire(ld, "1") {
		t.Fatal("reap lock still held after Run returned")
	}
	lock.Release(ld)
}

func TestCandidateFoldOnCorruptBus(t *testing.T) {
	f := newFx(t)
	if err := os.WriteFile(f.paths.Log, []byte(status("done", prURL, 0)+"\n{\"ts\":\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if rc := f.reap(f.main, Request{}); rc != 5 {
		t.Fatalf("rc=%d, want 5; stderr %q", rc, f.stderr.String())
	}
	f.wantOut()
	errs := strings.Split(strings.TrimSuffix(f.stderr.String(), "\n"), "\n")
	if len(errs) != 1 || !strings.HasPrefix(errs[0], "crew: reap: "+f.paths.Log+": ") {
		t.Fatalf("stderr %q, want one crew: reap: <log>: line", f.stderr.String())
	}
}

func TestCandidateFoldOnUnreadableBus(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-000 file")
	}
	f := newFx(t)
	f.bus(status("done", prURL, 0))
	if err := os.Chmod(f.paths.Log, 0); err != nil {
		t.Fatal(err)
	}
	if rc := f.reap(f.main, Request{}); rc != 2 {
		t.Fatalf("rc=%d, want 2; stderr %q", rc, f.stderr.String())
	}
	if !strings.HasPrefix(f.stderr.String(), "crew: reap: "+f.paths.Log+": ") {
		t.Fatalf("stderr %q", f.stderr.String())
	}
}

func TestNeedsGh(t *testing.T) {
	f := newFx(t)
	f.bus(status("done", prURL, 0))
	f.noGh = true
	if rc := f.reap(f.main, Request{}); rc != 0 {
		t.Fatalf("rc=%d", rc)
	}
	f.wantOut("crew reap: needs gh")
}

func TestOrphanWindows(t *testing.T) {
	f := newFx(t)
	f.bus()
	f.tmux["list-windows -a -F #{window_id}\t#{@crew_branch}\t#{@crew_dir}"] = strings.Join([]string{
		"@9\tfeat/gone\t" + f.paths.Dir,
		"@8\t" + rb + "\t" + f.paths.Dir,
		"@7\tfeat/gone\t/other/crew",
		"@6\t\t" + f.paths.Dir,
	}, "\n")
	if rc := f.reap(f.main, Request{}); rc != 0 {
		t.Fatalf("rc=%d", rc)
	}
	f.wantOut(
		"crew reap: window @9 is stamped feat/gone but its worktree is gone — not killed",
		"crew reap: nothing done to reap",
	)
}

func TestCrossRepoPRIgnoredThenGitHubPR(t *testing.T) {
	f := newFx(t)
	f.bus(status("done", `https://github.com/x/y/pull/1\u0007`, 0))
	f.gh["pr list --head feat/r --state all --json url,state,headRefOid,isCrossRepository"] =
		`[{"url":"https://github.com/z/fork/pull/3","isCrossRepository":true},{"url":"https://github.com/o/r/pull/9","state":"OPEN","headRefOid":"abc","isCrossRepository":false}]`
	f.gh["pr view https://github.com/o/r/pull/9 --json state --jq .state"] = "OPEN"
	if rc := f.reap(f.main, Request{}); rc != 0 {
		t.Fatalf("rc=%d", rc)
	}
	f.wantOut(
		"crew reap: keeping feat/r — PR OPEN (ignoring cross-repo pr_url https://github.com/x/y/pull/1?)",
		"crew reap: nothing reclaimed",
	)
}

func TestFindPRFailures(t *testing.T) {
	f := newFx(t)
	f.bus(status("done", "", 0))
	list := "pr list --head feat/r --state all --json url,state,headRefOid,isCrossRepository"
	f.ghFail[list] = true
	f.reap(f.main, Request{})
	f.wantOut("crew reap: keeping feat/r — could not list PRs for feat/r", "crew reap: nothing reclaimed")

	f.ghFail[list] = false
	f.gh[list] = `[{"url":"https://example.com/nope","isCrossRepository":false}]`
	f.reap(f.main, Request{})
	f.wantOut("crew reap: keeping feat/r — could not read PRs for feat/r", "crew reap: nothing reclaimed")
}

func TestIssueMode(t *testing.T) {
	f := newFx(t)
	claim := func(issue string) string {
		return `{"ts":1,"crew_id":"c1","kind":"claim-issue","branch":"` + rb + `","issue":` + issue + `}`
	}
	f.bus(status("done", "", 0), claim("5"), claim(`"6"`), claim("5"))
	f.gh["pr list --head feat/r --state all --json url,state,headRefOid,isCrossRepository"] = "[]"
	f.gh["issue view 5 --json state --jq .state"] = "CLOSED"
	f.gh["issue view 6 --json state --jq .state"] = "OPEN"
	f.reap(f.main, Request{})
	f.wantOut("crew reap: keeping feat/r — done but no PR on the bus or GitHub; issue #6 is OPEN", "crew reap: nothing reclaimed")

	// Both closed: no origin default branch ref in this repo keeps it.
	f.gh["issue view 6 --json state --jq .state"] = "CLOSED"
	f.reap(f.main, Request{})
	f.wantOut("crew reap: keeping feat/r — done but no PR on the bus or GitHub; no origin default branch ref", "crew reap: nothing reclaimed")

	f.bus(status("exited", "", 0))
	f.reap(f.main, Request{})
	f.wantOut("crew reap: keeping feat/r — exited but no PR on the bus or GitHub; only done/failed reclaim on a closed issue", "crew reap: nothing reclaimed")
}

func TestIssueLabel(t *testing.T) {
	f := newFx(t)
	r := &run{paths: f.paths, p: f.opts().Probes}
	f.bus(`{"ts":1,"kind":"claim-issue","branch":"`+rb+`","issue":12}`, `{"ts":2,"kind":"claim-issue","branch":"`+rb+`","issue":3}`)
	f.gh["issue view 12 --json state --jq .state"] = "CLOSED"
	f.gh["issue view 3 --json state --jq .state"] = "CLOSED"
	c := &candidate{latestRow: latestRow{branch: rb, state: "failed"}}
	if !r.issueClosed(context.Background(), c) {
		t.Fatalf("issueClosed: %s", c.why)
	}
	if c.label != "issue #12, #3 CLOSED" || c.prState != "ISSUE_CLOSED" || c.pr != "" || !slices.Equal(c.issues, []string{"12", "3"}) {
		t.Fatalf("candidate %+v", c)
	}
}

func TestMergedPRReachesRemoval(t *testing.T) {
	f := newFx(t)
	f.bus(status("done", prURL, 0))
	f.gh["pr view "+prURL+" --json state --jq .state"] = "MERGED"
	src := filepath.Join(f.wt, "src.go")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.reap(f.main, Request{})
	f.wantOut("crew reap: keeping feat/r — uncommitted changes", "crew reap: nothing reclaimed")

	// The caller standing in the worktree keeps it.
	f.reap(f.wt, Request{})
	f.wantOut("crew reap: keeping feat/r — it is the current worktree", "crew reap: nothing reclaimed")

	// Scaffold is not dirt: it goes to the trash and the tree is reaped.
	if err := os.Remove(src); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.wt, "PLAN.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if rc := f.reap(f.main, Request{}); rc != 0 {
		t.Fatalf("rc=%d stderr=%q", rc, f.stderr.String())
	}
	f.wantOut("crew reap: reaped feat/r (MERGED)", "crew reap: kept local branch feat/r — could not verify the merged PR head")
	f.wantErr("")
	if !slices.Contains(f.calls, "gtrash put "+f.wt+"/PLAN.md") {
		t.Fatalf("calls %q", f.calls)
	}
}

func TestEarlierPRHeadCheck(t *testing.T) {
	f := newFx(t)
	earlier := fmt.Sprintf(`{"ts":%d,"crew_id":"c1","from":"worker:%s#s0","kind":"status","body":{"state":"done","pr_url":"%s"}}`,
		(nowUnix-10)*1000, rb, prURL)
	f.bus(earlier, status("done", "", 0))
	f.gh["pr view "+prURL+" --json state --jq .state"] = "MERGED"
	f.gh["pr view "+prURL+" --json headRefOid --jq .headRefOid"] = "0123456789012345678901234567890123456789"
	f.reap(f.main, Request{})
	f.wantOut("crew reap: keeping feat/r — could not compare with "+prURL+" head (not fetched?)", "crew reap: nothing reclaimed")
}

func TestIdleEngineWindow(t *testing.T) {
	idle := idleFrame(t)
	f := newFx(t)
	f.bus(status("done", prURL, 0))
	f.gh["pr view "+prURL+" --json state --jq .state"] = "MERGED"
	f.tmux["list-panes -a -F #{window_id}\t#{pane_id}\t#{pane_current_command}\t#{pane_current_path}"] =
		"@5\t%5\tclaude\t" + f.wt + "\n@5\t%6\t.claude-wrapped\t" + f.wt + "\n@4\t%4\tclaude\t" + f.wt + "x"
	for _, p := range []string{"%5", "%6"} {
		f.tmux["capture-pane -p -t "+p] = idle
		f.tmux["capture-pane -e -p -t "+p] = idle
	}
	if rc := f.reap(f.main, Request{DryRun: true}); rc != 0 {
		t.Fatalf("rc=%d stderr=%q", rc, f.stderr.String())
	}
	anchor, _ := wtgit.AnchorPath(f.wt)
	f.wantOut("crew reap: would kill window @5 (feat/r idle engine)",
		"crew reap: would reap feat/r (MERGED) @ "+f.wt, "crew reap: would prune record "+anchor)

	// A branch that posted after the status keeps everything.
	f.bus(status("done", prURL, 5), fmt.Sprintf(`{"ts":%d,"from":"worker:%s#s2","kind":"msg"}`, nowUnix*1000, rb))
	f.reap(f.main, Request{})
	f.wantOut("crew reap: keeping feat/r — an engine is still running there (session posted after its done)", "crew reap: nothing reclaimed")

	f.bus(status("done", prURL, 0))
	f.reap(f.main, Request{})
	f.wantOut("crew reap: killed window @5 (feat/r idle engine)", "crew reap: reaped feat/r (MERGED)",
		"crew reap: kept local branch feat/r — could not verify the merged PR head")
	if !slices.Contains(f.calls, "tmux kill-window -t @5") {
		t.Fatalf("calls %q", f.calls)
	}
}

func TestIdlePiEngineWindow(t *testing.T) {
	read := func(name string) string {
		b, err := os.ReadFile("../frame/testdata/" + name)
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimRight(string(b), "\n")
	}
	idle, working, draft, truncated := read("frames/nudge_pi_idle.txt"), read("pi/midturn_working_rule.txt"), read("frames/nudge_pi_draft.txt"), read("frames/pi_truncated_idle.txt")
	f := newFx(t)
	f.bus(status("done", prURL, 0))
	f.gh["pr view "+prURL+" --json state --jq .state"] = "MERGED"
	f.tmux["list-panes -a -F #{window_id}\t#{pane_id}\t#{pane_current_command}\t#{pane_current_path}"] = "@5\t%5\tpi\t" + f.wt

	f.tmux["capture-pane -p -t %5"] = idle
	f.reap(f.main, Request{DryRun: true})
	anchor, _ := wtgit.AnchorPath(f.wt)
	f.wantOut("crew reap: would kill window @5 (feat/r idle engine)",
		"crew reap: would reap feat/r (MERGED) @ "+f.wt, "crew reap: would prune record "+anchor)

	f.tmux["capture-pane -p -t %5"] = working
	f.reap(f.main, Request{DryRun: true})
	f.wantOut("crew reap: keeping feat/r — an engine is still running there (pi frame: live turn or no idle input box)")

	f.tmux["capture-pane -p -t %5"] = draft
	f.reap(f.main, Request{DryRun: true})
	f.wantOut("crew reap: keeping feat/r — an engine is still running there (pi frame: unsent input)")

	f.tmux["capture-pane -p -t %5"] = truncated
	f.reap(f.main, Request{DryRun: true})
	f.wantOut("crew reap: keeping feat/r — an engine is still running there (pi frame: stopped on truncated)")
}

func TestIdleEngineKeepReasons(t *testing.T) {
	f := newFx(t)
	f.bus(status("done", prURL, 0))
	f.gh["pr view "+prURL+" --json state --jq .state"] = "MERGED"
	panes := "list-panes -a -F #{window_id}\t#{pane_id}\t#{pane_current_command}\t#{pane_current_path}"

	f.tmux[panes] = "@5\t%5\tcodex\t" + f.wt
	f.reap(f.main, Request{})
	f.wantOut("crew reap: keeping feat/r — an engine is still running there (codex has no idle signature)", "crew reap: nothing reclaimed")

	f.tmux[panes] = "@5\t%5\tclaude\t" + f.wt
	f.reap(f.main, Request{})
	f.wantOut("crew reap: keeping feat/r — an engine is still running there (empty capture of %5)", "crew reap: nothing reclaimed")

	f.bus(status("failed", prURL, 0))
	f.reap(f.main, Request{})
	f.wantOut("crew reap: keeping feat/r — an engine is still running there (failed session)", "crew reap: nothing reclaimed")
}

func TestIdleReleaseWritesRowAndAbortsOnAppendError(t *testing.T) {
	f := newFx(t)
	f.bus(status("done", "", 600))
	f.ghFail["pr list --head feat/r --state all --json url,state,headRefOid,isCrossRepository"] = true
	f.tmux["list-windows -a -F #{window_id}\t#{@crew_name}\t#{pane_current_path}"] = "@5\tworker\t" + f.wt
	f.tmux["list-panes -a -F #{window_id}\t#{pane_id}\t#{pane_current_command}"] = "@5\t%5\tzsh"
	if rc := f.reap(f.main, Request{Quiet: true}); rc != 0 {
		t.Fatalf("rc=%d stderr=%q", rc, f.stderr.String())
	}
	f.wantOut("crew reap: released @5 at " + f.wt + " (feat/r done)")
	if len(f.appended) != 1 || !strings.Contains(f.appended[0], `"kind":"release"`) || !strings.Contains(f.appended[0], `"session":"s1"`) {
		t.Fatalf("appended %q", f.appended)
	}

	f.appendErr = errors.New("disk full")
	if rc := f.reap(f.main, Request{Quiet: true}); rc != 1 {
		t.Fatalf("rc=%d, want 1", rc)
	}
	f.wantErr("crew: reap: disk full\n")
}

func TestIdleFoldFailureIsSilent(t *testing.T) {
	f := newFx(t)
	// A string ts fails the idle fold's arithmetic; the candidate fold
	// never does arithmetic on a status ts, so the reap carries on.
	f.bus(`{"ts":"x","from":"worker:feat/r#s1","kind":"status","body":{"state":"done"}}`)
	f.noGh = true
	if rc := f.reap(f.main, Request{}); rc != 0 {
		t.Fatalf("rc=%d stderr=%q", rc, f.stderr.String())
	}
	f.wantOut("crew reap: needs gh")
	f.wantErr("")
}

func TestAutosweep(t *testing.T) {
	f := newFx(t)
	f.bus()
	f.env["CREW_RATE_AUTOSWEEP"] = "yes"
	f.reap(f.main, Request{DryRun: true})
	f.wantErr("crew reap: CREW_RATE_AUTOSWEEP='yes' is not 0, 1 or sync — sweeping anyway\n")
	f.wantOut("crew reap: dry run — skipping ratings sweep", "crew reap: nothing done to reap")

	f.env["CREW_RATE_AUTOSWEEP"] = "sync"
	f.reap(f.main, Request{Discard: rb})
	if slices.Contains(f.calls, "rate") {
		t.Fatal("--discard swept")
	}

	f.reap(f.main, Request{})
	if !slices.Equal(f.rateCwd, []string{f.main}) {
		t.Fatalf("rate ran from %q", f.rateCwd)
	}
	ld := f.root + "/data/crew/ratings.sweep.lock.d"
	if !lock.Acquire(ld, "1") {
		t.Fatal("sweep lock still held")
	}
	f.rateCwd = nil
	f.reap(f.main, Request{})
	lock.Release(ld)
	f.wantOut("crew reap: a ratings sweep is already running — skipped", "crew reap: nothing done to reap")
	if f.rateCwd != nil {
		t.Fatal("swept under a held lock")
	}
}

func TestTailLines(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"a\nb\nc\n", "b\nc\n"},
		{"a\nb\nc", "b\nc"},
		{"c\n", "c\n"},
		{"\n\n\n", "\n\n"},
	}
	for _, c := range cases {
		if got := string(tailLines([]byte(c.in), 2)); got != c.want {
			t.Errorf("tailLines(%q, 2) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestTrimLogKeepsInode(t *testing.T) {
	p := filepath.Join(t.TempDir(), "autosweep.log")
	var b strings.Builder
	for i := range 250 {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	if err := os.WriteFile(p, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(p)
	trimLog(p, autosweepKeep)
	after, _ := os.Stat(p)
	data, _ := os.ReadFile(p)
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) != 200 || lines[0] != "line 50" || !os.SameFile(before, after) {
		t.Fatalf("trimmed to %d lines starting %q, same file %v", len(lines), lines[0], os.SameFile(before, after))
	}
}

func TestCntrlToQ(t *testing.T) {
	if got := cntrlToQ("a\x1b[31m\u0085b\xffc"); got != "a?[31m?b\xffc" {
		t.Fatalf("cntrlToQ = %q", got)
	}
}
