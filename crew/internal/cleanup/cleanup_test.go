package cleanup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/clock"
	"github.com/noamsto/dispatcher/crew/internal/gh/ghtest"
	"github.com/noamsto/dispatcher/crew/internal/git"
	"github.com/noamsto/dispatcher/crew/internal/gitguard"
	"github.com/noamsto/dispatcher/crew/internal/proc/proctest"
	"github.com/noamsto/dispatcher/crew/internal/reap"
	"github.com/noamsto/dispatcher/crew/internal/snooze"
	"github.com/noamsto/dispatcher/crew/internal/tmux/tmuxtest"
)

const (
	branch  = "feat/r"
	nowUnix = int64(1_800_000_000)
)

// fx is one cleanup scenario: a real repo with a github origin name, a
// baseline, a linked worktree on `branch` and a bus, with fake tmux and gh.
type fx struct {
	t              *testing.T
	root, main, wt string
	paths          bus.Paths
	env            map[string]string
	gh             map[string]string
	now            time.Time
	tty            bool
	input          string
	paged          []string
	stdout, stderr bytes.Buffer
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
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
	t.Setenv("TMUX_PANE", "")
	f := &fx{
		t: t, root: root, main: filepath.Join(root, "main"), wt: filepath.Join(root, "wt"),
		env: map[string]string{"CREW_RATE_AUTOSWEEP": "0", "XDG_DATA_HOME": filepath.Join(root, "data"), "HOME": root},
		gh:  map[string]string{"issue view 5 --json state --jq .state": "CLOSED"},
		now: time.Unix(nowUnix, 0), tty: true,
	}
	if err := os.Mkdir(f.main, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, f.main, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(f.main, "tracked.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, f.main, "add", "tracked.txt")
	runGit(t, f.main, "commit", "-q", "-m", "init")
	// init is on origin's default branch, so only later commits are unpushed.
	runGit(t, f.main, "update-ref", "refs/remotes/origin/main", "HEAD")
	runGit(t, f.main, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
	runGit(t, f.main, "remote", "add", "origin", "https://github.com/o/r.git")
	runGit(t, f.main, "worktree", "add", "-q", "-b", branch, f.wt)
	common := filepath.Join(f.main, ".git")
	var stderr bytes.Buffer
	if err := gitguard.Init(common, &stderr); err != nil {
		t.Fatalf("gitguard.Init: %v\n%s", err, stderr.String())
	}
	f.paths = bus.Paths{Common: common, Dir: common + "/crew", Log: common + "/crew/events.jsonl"}
	if err := os.MkdirAll(f.paths.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	f.bus(3 * 24 * 3600)
	t.Chdir(f.main)
	return f
}

// bus writes a failed worker's status ageSec old and a claim on closed issue 5.
func (f *fx) bus(ageSec int64) {
	f.t.Helper()
	rows := []string{
		fmt.Sprintf(`{"ts":%d,"crew_id":"c1","from":"worker:%s#s1","to":"dispatcher:c1","kind":"status","body":{"state":"failed"}}`, (nowUnix-ageSec)*1000, branch),
		fmt.Sprintf(`{"ts":1,"crew_id":"c1","kind":"claim-issue","branch":"%s","issue":5}`, branch),
	}
	if err := os.WriteFile(f.paths.Log, []byte(strings.Join(rows, "\n")+"\n"), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

// dirty leaves an uncommitted file and, when commit, one unpushed commit.
func (f *fx) dirty(commit bool) {
	f.t.Helper()
	if commit {
		if err := os.WriteFile(filepath.Join(f.wt, "done.txt"), []byte("committed work\n"), 0o644); err != nil {
			f.t.Fatal(err)
		}
		runGit(f.t, f.wt, "add", "done.txt")
		runGit(f.t, f.wt, "commit", "-q", "-m", "work")
	}
	if err := os.WriteFile(filepath.Join(f.wt, "notes.txt"), []byte("scratch\n"), 0o644); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.wt, "tracked.txt"), []byte("base\nedited\n"), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fx) opts() Options {
	return Options{
		Reap: reap.Options{
			Probes: reap.Probes{
				Git:  git.Exec{},
				Tmux: &tmuxtest.Fake{Func: func([]string) (string, error) { return "", nil }},
				Gh: &ghtest.Fake{Func: func(args []string) (string, error) {
					if len(args) == 1 && args[0] == "--version" {
						return "", nil
					}
					return f.gh[strings.Join(args, " ")], nil
				}},
				Gtrash: func(context.Context, string) {},
				Proc:   &proctest.Table{},
				Sleep:  func(time.Duration) {},
				Clock:  clock.Clock{Now: func() time.Time { return f.now }},
				Append: func(string, string) error { return nil },
				Getenv: func(k string) string { return f.env[k] },
			},
			PID:     os.Getpid(),
			RateRun: func([]string, bus.Paths, string, io.Writer, io.Writer) int { return 0 },
		},
		Getenv:      func(k string) string { return f.env[k] },
		Stdin:       strings.NewReader(f.input),
		Interactive: func() bool { return f.tty },
		Pager:       func(_ context.Context, text string) error { f.paged = append(f.paged, text); return nil },
	}
}

func (f *fx) run(req Request) int {
	f.t.Helper()
	f.stdout.Reset()
	f.stderr.Reset()
	return Run(context.Background(), req, f.main, &f.stdout, &f.stderr, f.opts())
}

func (f *fx) items() []Item {
	f.t.Helper()
	if rc := f.run(Request{List: true, JSON: true}); rc != 0 {
		f.t.Fatalf("rc=%d %s", rc, f.stderr.String())
	}
	var doc struct{ Items []Item }
	if err := json.Unmarshal(f.stdout.Bytes(), &doc); err != nil {
		f.t.Fatalf("%v: %s", err, f.stdout.String())
	}
	return doc.Items
}

func (f *fx) archiveDirs() []string {
	dirs, _ := filepath.Glob(filepath.Join(f.env["XDG_DATA_HOME"], "crew", "cleanup-archive", "*", "*"))
	return dirs
}

func TestListJSONShowsStaleWorktree(t *testing.T) {
	f := newFx(t)
	f.dirty(true)
	items := f.items()
	if len(items) != 1 {
		t.Fatalf("items %+v", items)
	}
	it := items[0]
	if it.Branch != branch || !it.Stale || it.Worktree != f.wt || it.Reason != "uncommitted changes; issue closed" {
		t.Fatalf("item %+v", it)
	}
	if it.AgeS < 3*24*3600 || len(it.Issues) != 1 || it.Issues[0].State != "CLOSED" {
		t.Fatalf("age/issues %+v", it)
	}
	if !strings.Contains(strings.Join(it.Status, "\n"), "?? notes.txt") || len(it.Unpushed) != 1 || !strings.Contains(it.Unpushed[0], "work") ||
		!strings.Contains(it.Diffstat, "tracked.txt") {
		t.Fatalf("git context %+v", it)
	}
}

func TestListTextAndNothingToReview(t *testing.T) {
	f := newFx(t)
	f.dirty(false)
	if rc := f.run(Request{List: true}); rc != 0 {
		t.Fatalf("rc=%d", rc)
	}
	for _, want := range []string{"reason:   uncommitted changes; issue closed", "issue #5 CLOSED", "?? notes.txt", "1 worktree(s) to review"} {
		if !strings.Contains(f.stdout.String(), want) {
			t.Errorf("missing %q in %s", want, f.stdout.String())
		}
	}
	runGit(t, f.wt, "checkout", "--", "tracked.txt")
	if err := os.Remove(filepath.Join(f.wt, "notes.txt")); err != nil {
		t.Fatal(err)
	}
	if got := f.items(); len(got) != 0 {
		t.Fatalf("clean worktree listed: %+v", got)
	}
}

func TestRemoveArchivesThenRemoves(t *testing.T) {
	f := newFx(t)
	f.dirty(true)
	tip := runGit(t, f.wt, "rev-parse", "HEAD")
	f.input = "r\n"
	if rc := f.run(Request{}); rc != 0 {
		t.Fatalf("rc=%d stderr=%s", rc, f.stderr.String())
	}
	dirs := f.archiveDirs()
	if len(dirs) != 1 || !strings.HasPrefix(filepath.Base(dirs[0]), "feat-r-20") {
		t.Fatalf("archive dirs %v", dirs)
	}
	if !strings.Contains(f.stdout.String(), dirs[0]) || !strings.Contains(f.stdout.String(), "removed 1, kept 0, skipped 0") {
		t.Fatalf("stdout %s", f.stdout.String())
	}
	patch, err := os.ReadFile(filepath.Join(dirs[0], "uncommitted.patch"))
	if err != nil || !strings.Contains(string(patch), "notes.txt") || !strings.Contains(string(patch), "scratch") {
		t.Fatalf("patch %q %v", patch, err)
	}
	var meta info
	b, err := os.ReadFile(filepath.Join(dirs[0], "info.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &meta); err != nil {
		t.Fatal(err)
	}
	if meta.Branch != branch || meta.Tip != tip || meta.Reason != "uncommitted changes; issue closed" ||
		len(meta.Issues) != 1 || meta.Issues[0].Ref != "#5" || !meta.Uncommit || !meta.Commits {
		t.Fatalf("info %+v", meta)
	}
	if _, err := os.Stat(f.wt); !os.IsNotExist(err) {
		t.Fatalf("worktree survived: %v", err)
	}
	if got := runGit(t, f.main, "branch", "--list", branch); got != "" {
		t.Fatalf("branch survived: %q", got)
	}

	// The bundle restores the commits into a fresh repo.
	bundle := filepath.Join(dirs[0], "commits.bundle")
	fresh := filepath.Join(f.root, "fresh")
	if err := os.Mkdir(fresh, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, fresh, "init", "-q")
	// A real restore happens in a clone, which already holds the remote's
	// commits the bundle builds on.
	runGit(t, fresh, "fetch", "-q", f.main, "main")
	runGit(t, fresh, "bundle", "verify", bundle)
	runGit(t, fresh, "fetch", "-q", bundle, "refs/heads/"+branch+":refs/heads/restored")
	if got := runGit(t, fresh, "rev-parse", "restored"); got != tip {
		t.Fatalf("restored %s, want %s", got, tip)
	}
}

func TestRemoveWithoutCommitsWritesNoBundle(t *testing.T) {
	f := newFx(t)
	f.dirty(false)
	f.input = "r\n"
	if rc := f.run(Request{}); rc != 0 {
		t.Fatalf("rc=%d %s", rc, f.stderr.String())
	}
	dirs := f.archiveDirs()
	if len(dirs) != 1 {
		t.Fatalf("archive dirs %v", dirs)
	}
	if _, err := os.Stat(filepath.Join(dirs[0], "commits.bundle")); !os.IsNotExist(err) {
		t.Fatalf("empty bundle written: %v", err)
	}
}

func TestKeepSnoozesForBothCleanupAndReap(t *testing.T) {
	f := newFx(t)
	f.dirty(false)
	f.input = "k\n"
	if rc := f.run(Request{}); rc != 0 {
		t.Fatalf("rc=%d %s", rc, f.stderr.String())
	}
	if !strings.Contains(f.stdout.String(), "kept 1") {
		t.Fatalf("stdout %s", f.stdout.String())
	}
	if _, err := os.Stat(f.wt); err != nil {
		t.Fatalf("keep touched the worktree: %v", err)
	}
	if !snooze.Active(f.paths.Dir, branch, f.now.Add(snooze.Default-time.Second)) || snooze.Active(f.paths.Dir, branch, f.now.Add(snooze.Default)) {
		t.Fatal("snooze is not exactly 14 days")
	}

	reapOut := func() string {
		var out bytes.Buffer
		if rc := reap.Run(context.Background(), reap.Request{DryRun: true}, f.paths, f.main, &out, io.Discard, f.opts().Reap); rc != 0 {
			t.Fatalf("reap rc=%d", rc)
		}
		return out.String()
	}
	if got := f.items(); len(got) != 0 {
		t.Fatalf("snoozed item listed by cleanup: %+v", got)
	}
	if out := reapOut(); strings.Contains(out, "stale: needs human cleanup") {
		t.Fatalf("snoozed item reported stale by reap: %s", out)
	}

	f.now = f.now.Add(snooze.Default + time.Hour)
	f.bus(3*24*3600 + int64(snooze.Default/time.Second))
	if got := f.items(); len(got) != 1 {
		t.Fatalf("lapsed snooze not raised again: %+v", got)
	}
	if out := reapOut(); !strings.Contains(out, "stale: needs human cleanup") {
		t.Fatalf("reap does not report it after the snooze: %s", out)
	}
}

func TestSkipQuitAndDiff(t *testing.T) {
	f := newFx(t)
	f.dirty(false)
	f.input = "x\nd\ns\n"
	if rc := f.run(Request{}); rc != 0 {
		t.Fatalf("rc=%d %s", rc, f.stderr.String())
	}
	if len(f.paged) != 1 || !strings.Contains(f.paged[0], "notes.txt") || !strings.Contains(f.paged[0], "+scratch") {
		t.Fatalf("pager got %q", f.paged)
	}
	if !strings.Contains(f.stdout.String(), "removed 0, kept 0, skipped 1") {
		t.Fatalf("stdout %s", f.stdout.String())
	}

	f.input = "q\n"
	f.run(Request{})
	if !strings.Contains(f.stdout.String(), "1 left unreviewed") {
		t.Fatalf("stdout %s", f.stdout.String())
	}
	f.input = ""
	f.run(Request{})
	if !strings.Contains(f.stdout.String(), "1 left unreviewed") {
		t.Fatalf("EOF is not quit: %s", f.stdout.String())
	}
	if _, err := os.Stat(f.wt); err != nil {
		t.Fatalf("worktree touched: %v", err)
	}
	if len(f.archiveDirs()) != 0 {
		t.Fatal("archive written without a remove")
	}
}

func TestRefusals(t *testing.T) {
	f := newFx(t)
	f.dirty(false)
	f.input = "r\n"

	f.env["CREW_WORKER_ID"] = "worker:x#s1"
	if rc := f.run(Request{}); rc != 1 || !strings.Contains(f.stderr.String(), "CREW_WORKER_ID is set") {
		t.Fatalf("worker: rc=%d %q", rc, f.stderr.String())
	}
	if rc := f.run(Request{PruneArchive: true}); rc != 1 || !strings.Contains(f.stderr.String(), "CREW_WORKER_ID") {
		t.Fatalf("worker prune: rc=%d %q", rc, f.stderr.String())
	}
	for _, req := range []Request{{List: true}, {JSON: true}} {
		if rc := f.run(req); rc != 0 {
			t.Fatalf("%+v under CREW_WORKER_ID: rc=%d %s", req, rc, f.stderr.String())
		}
	}
	delete(f.env, "CREW_WORKER_ID")

	f.env["CREW_ROLE_ID"] = "role:x:plan-critic"
	if rc := f.run(Request{}); rc != 1 || !strings.Contains(f.stderr.String(), "CREW_ROLE_ID is set") {
		t.Fatalf("role: rc=%d %q", rc, f.stderr.String())
	}
	delete(f.env, "CREW_ROLE_ID")

	f.tty = false
	if rc := f.run(Request{}); rc != 1 || !strings.Contains(f.stderr.String(), "must both be a terminal") {
		t.Fatalf("non-tty: rc=%d %q", rc, f.stderr.String())
	}
	if rc := f.run(Request{List: true}); rc != 0 {
		t.Fatalf("non-tty --list: rc=%d", rc)
	}
	if _, err := os.Stat(f.wt); err != nil || len(f.archiveDirs()) != 0 {
		t.Fatalf("a refusal changed something: %v %v", err, f.archiveDirs())
	}
}

func TestDriftedConfigIsRefusedNotRemoved(t *testing.T) {
	f := newFx(t)
	f.dirty(false)
	buses, cwds, _ := f.opts().repos(context.Background(), Request{}, f.main, io.Discard)
	items, code := f.opts().gather(context.Background(), buses, cwds, io.Discard)
	if code != 0 || len(items) != 1 {
		t.Fatalf("items %+v rc=%d", items, code)
	}
	runGit(t, f.main, "config", "core.fsmonitor", "/bin/true")
	var stderr bytes.Buffer
	dir, err := f.opts().remove(context.Background(), items[0], &stderr)
	if !errors.Is(err, errDrift) || err.Error() != "git config drift (run `crew git-baseline`)" {
		t.Fatalf("err %v", err)
	}
	if dir != "" || len(f.archiveDirs()) != 0 {
		t.Fatalf("archived under drift: %q %v", dir, f.archiveDirs())
	}
	if _, err := os.Stat(filepath.Join(f.wt, "notes.txt")); err != nil {
		t.Fatalf("worktree touched: %v", err)
	}
	if runGit(t, f.main, "branch", "--list", branch) == "" {
		t.Fatal("branch deleted under drift")
	}
	if !strings.Contains(stderr.String(), "refusing git") {
		t.Fatalf("guard reason not shown: %q", stderr.String())
	}
}

func TestPruneArchive(t *testing.T) {
	f := newFx(t)
	root := f.opts().archiveRoot()
	mk := func(repo, name string) string {
		p := filepath.Join(root, repo, name)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	old := mk("r", "feat-a-"+f.now.Add(-31*24*time.Hour).UTC().Format("20060102T150405Z"))
	fresh := mk("r", "feat-b-"+f.now.Add(-29*24*time.Hour).UTC().Format("20060102T150405Z"))
	onlyOld := mk("other", "feat-c-"+f.now.Add(-90*24*time.Hour).UTC().Format("20060102T150405Z"))
	if rc := f.run(Request{PruneArchive: true}); rc != 0 {
		t.Fatalf("rc=%d %s", rc, f.stderr.String())
	}
	for _, p := range []string{old, onlyOld} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s survived", p)
		}
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("recent archive pruned: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(onlyOld)); !os.IsNotExist(err) {
		t.Error("empty repo dir left behind")
	}
	if rc := f.run(Request{PruneArchive: true, OlderThan: "7d"}); rc != 0 {
		t.Fatalf("rc=%d", rc)
	}
	if _, err := os.Stat(fresh); !os.IsNotExist(err) {
		t.Error("--older-than 7d kept it")
	}
	if rc := f.run(Request{PruneArchive: true, OlderThan: "soon"}); rc != 1 {
		t.Fatalf("bad age: rc=%d", rc)
	}
}

func TestParseAge(t *testing.T) {
	for in, want := range map[string]time.Duration{"30d": 30 * 24 * time.Hour, "12h": 12 * time.Hour, "2w": 14 * 24 * time.Hour, "0d": 0} {
		if got, err := ParseAge(in); err != nil || got != want {
			t.Errorf("%s = %v, %v", in, got, err)
		}
	}
	for _, in := range []string{"", "d", "30", "30m", "-1d", "xd", "99999999999999999d"} {
		if _, err := ParseAge(in); err == nil {
			t.Errorf("%q accepted", in)
		}
	}
}
