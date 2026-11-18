package reap

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/filekind"
	"github.com/noamsto/dispatcher/crew/internal/proc/proctest"
)

// discardFx is a done worker on rb whose PR is MERGED, with one commit of its
// own so the patch has a tip to apply to.
func discardFx(t *testing.T) *fx {
	t.Helper()
	f := newFx(t)
	if err := os.WriteFile(f.wt+"/a.txt", []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, f.wt, "add", "a.txt")
	runGit(t, f.wt, "commit", "-q", "-m", "a")
	f.bus(status("done", prURL, 0))
	f.gh["pr view "+prURL+" --json state --jq .state"] = "MERGED"
	return f
}

func (f *fx) patchPath() string {
	return f.paths.Dir + "/artifacts/" + rb + "/discarded-" + time.Unix(nowUnix, 0).UTC().Format("20060102T150405Z") + ".patch"
}

func (f *fx) noScratch() {
	f.t.Helper()
	if m, _ := filepath.Glob(f.paths.Dir + "/discard.*"); len(m) != 0 {
		f.t.Fatalf("scratch dirs left: %q", m)
	}
}

func TestDiscardSavesApplicablePatch(t *testing.T) {
	f := discardFx(t)
	if err := os.WriteFile(f.wt+"/a.txt", []byte("one\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.wt+"/new.bin", []byte{0, 1, 2, 'x'}, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.wt+"/PLAN.md", []byte("plan\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.wt+"/staged.txt", []byte("staged\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, f.wt, "add", "staged.txt")
	patch := f.patchPath()

	if rc := f.reap(f.main, Request{Discard: rb, DryRun: true}); rc != 0 {
		t.Fatalf("dry rc=%d stderr=%q", rc, f.stderr.String())
	}
	f.wantOut("crew reap: would save uncommitted state to "+patch, "crew reap: would reap feat/r (MERGED) @ "+f.wt)

	if rc := f.reap(f.main, Request{Discard: rb}); rc != 0 {
		t.Fatalf("rc=%d stderr=%q", rc, f.stderr.String())
	}
	f.wantOut("crew reap: saved uncommitted state to "+patch, "crew reap: reaped feat/r (MERGED, discarded)")
	f.wantErr("")
	f.noScratch()
	if _, err := os.Lstat(f.wt); !os.IsNotExist(err) {
		t.Fatalf("worktree still there: %v", err)
	}
	reap := f.rows("reap")
	if len(reap) != 1 {
		t.Fatalf("appended %q", f.appended)
	}
	wantJSON(t, reap[0], fmt.Sprintf(`{"ts":%d,"kind":"reap","branch":"feat/r","pr":%q,"pr_state":"MERGED","worktree":%q,"discarded":%q}`, nowUnix*1000, prURL, f.wt, patch))
	// --discard keeps the branch: the patch applies to its tip.
	fresh := filepath.Join(f.root, "fresh")
	runGit(t, f.main, "worktree", "add", "-q", fresh, rb)
	runGit(t, fresh, "apply", "--check", patch)
	runGit(t, fresh, "apply", patch)
	if got, _ := os.ReadFile(fresh + "/a.txt"); string(got) != "one\ntwo\n" {
		t.Fatalf("a.txt = %q", got)
	}
	for name, want := range map[string]string{"PLAN.md": "plan\n", "staged.txt": "staged\n"} {
		if got, _ := os.ReadFile(fresh + "/" + name); string(got) != want {
			t.Fatalf("%s = %q", name, got)
		}
	}
	if got, _ := os.ReadFile(fresh + "/new.bin"); string(got) != "\x00\x01\x02x" {
		t.Fatalf("new.bin = %q", got)
	}
	if fi, err := os.Lstat(patch); err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("patch %v %v", fi, err)
	}
}

func TestDiscardCleanTree(t *testing.T) {
	f := discardFx(t)
	if rc := f.reap(f.main, Request{Discard: rb}); rc != 0 {
		t.Fatalf("rc=%d stderr=%q", rc, f.stderr.String())
	}
	f.wantOut("crew reap: no uncommitted changes to save", "crew reap: reaped feat/r (MERGED, discarded)")
	wantJSON(t, f.rows("reap")[0], fmt.Sprintf(`{"ts":%d,"kind":"reap","branch":"feat/r","pr":%q,"pr_state":"MERGED","worktree":%q,"discarded":""}`, nowUnix*1000, prURL, f.wt))
	f.noScratch()
	if _, err := os.Lstat(f.paths.Dir + "/artifacts"); !os.IsNotExist(err) {
		t.Fatalf("artifacts made for an empty patch: %v", err)
	}
}

func TestDiscardRefusals(t *testing.T) {
	f := discardFx(t)
	if err := os.WriteFile(f.wt+"/a.txt", []byte("dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	refused := func(why, branch string) {
		t.Helper()
		if rc := f.reap(f.main, Request{Discard: branch}); rc != 1 {
			t.Fatalf("%q: rc=%d stdout=%q", branch, rc, f.stdout.String())
		}
		f.wantOut()
		f.wantErr("crew reap: refusing --discard " + branch + " — " + why + "\n")
		if !filekind.IsDir(f.wt) {
			t.Fatal("worktree removed")
		}
		portFNoPatch(f)
	}
	refused("not a valid branch name", "feat/..bad")
	refused("not a valid branch name", "@{-1}")
	refused("no worktree for it", "feat/none")

	f.gh["pr view "+prURL+" --json state --jq .state"] = "OPEN"
	refused("PR OPEN", rb)
	f.gh["pr view "+prURL+" --json state --jq .state"] = ""
	refused("could not read PR state ("+prURL+")", rb)
	f.gh["pr view "+prURL+" --json state --jq .state"] = "MERGED"

	f.tmux[panesKey] = "@5\t%5\tclaude\t" + f.wt
	refused("an engine is running there", rb)
	f.tmux[panesKey] = ""

	f.bus(status("working", prURL, 0))
	refused("latest status is working", rb)
	f.bus(status("done", prURL, 0))

	if rc := f.reap(f.wt, Request{Discard: rb}); rc != 1 {
		t.Fatalf("rc=%d", rc)
	}
	f.wantErr("crew reap: refusing --discard feat/r — it is the current worktree\n")

	f.noGh = true
	refused("needs gh", rb)
	f.noGh = false

	// A worker-planted symlink on the artifacts chain redirects nothing.
	if err := os.MkdirAll(f.paths.Dir+"/artifacts", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(f.root, f.paths.Dir+"/artifacts/feat"); err != nil {
		t.Fatal(err)
	}
	refused(f.paths.Dir+"/artifacts/feat is a symlink or not a directory", rb)
	f.noScratch()
	if err := os.Remove(f.paths.Dir + "/artifacts/feat"); err != nil {
		t.Fatal(err)
	}

	patch := f.patchPath()
	if err := os.MkdirAll(filepath.Dir(patch), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(patch, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	refused(patch+" already exists", rb)
	if got, _ := os.ReadFile(patch); string(got) != "old" {
		t.Fatalf("existing patch clobbered: %q", got)
	}
	f.noScratch()
}

// add -A would stage an untracked nested repo as a bare gitlink.
func TestDiscardRefusesEmbeddedRepo(t *testing.T) {
	f := discardFx(t)
	nested := f.wt + "/nested"
	if err := os.Mkdir(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, nested, "init", "-q")
	runGit(t, nested, "commit", "-q", "--allow-empty", "-m", "n")
	if rc := f.reap(f.main, Request{Discard: rb}); rc != 1 {
		t.Fatalf("rc=%d stdout=%q", rc, f.stdout.String())
	}
	f.wantErr("crew reap: refusing --discard feat/r — it holds an embedded git repository\n")
	f.noScratch()
	portFNoPatch(f)
	if !filekind.IsDir(nested + "/.git") {
		t.Fatal("nested repo removed")
	}
}

// A TERM mid-save runs the cleaner through notify's signal wiring: the child
// registers a scratch dir, signals itself and must exit 128+SIGTERM with the
// dir gone.
func TestDiscardScratchRemovedOnSignal(t *testing.T) {
	if dir := os.Getenv("CREW_REAP_SIGNAL_CHILD"); dir != "" {
		c := &cleaner{}
		stop := c.notify()
		defer stop()
		c.setTmp(dir)
		_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
		time.Sleep(10 * time.Second)
		os.Exit(1)
	}
	tmp, err := os.MkdirTemp(t.TempDir(), "discard.")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tmp+"/index", []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestDiscardScratchRemovedOnSignal$")
	cmd.Env = append(os.Environ(), "CREW_REAP_SIGNAL_CHILD="+tmp)
	err = cmd.Run()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 128+int(syscall.SIGTERM) {
		t.Fatalf("child exit = %v, want 143", err)
	}
	if _, err := os.Lstat(tmp); !os.IsNotExist(err) {
		t.Fatalf("scratch dir left: %v", err)
	}
}

// The removal re-check refuses a tree that changed after the save.
func TestDiscardRecheck(t *testing.T) {
	f := discardFx(t)
	if err := os.WriteFile(f.wt+"/a.txt", []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := f.run()
	r.discard = rb
	c := f.candidate()
	st := " M a.txt"
	r.discardSt = st
	r.discardTree = "0000000000000000000000000000000000000000"
	if why := r.discardRecheck(t.Context(), c, st); why != "changed while saving" {
		t.Fatalf("why %q", why)
	}
	if why := r.discardRecheck(t.Context(), c, "?? b.txt"); why != "changed while saving" {
		t.Fatalf("why %q", why)
	}
	tree, err := ScratchTree(t.Context(), c.admin, c.wtpath, "refs/heads/"+rb, filepath.Join(t.TempDir(), "index"), false, &f.stderr)
	if err != nil {
		t.Fatal(err)
	}
	r.discardTree = tree
	if why := r.discardRecheck(t.Context(), c, st); why != "" {
		t.Fatalf("why %q", why)
	}
	f.noScratch()
	// The worker's own index is untouched by the scratch builds.
	if out, err := exec.Command("git", "-C", f.wt, "diff", "--cached", "--name-only").Output(); err != nil || strings.TrimSpace(string(out)) != "" {
		t.Fatalf("worker index changed: %q %v", out, err)
	}
}

// ignoredScaffoldFx is discardFx with the scaffold names gitignored and only
// SPEC.md and WORKER_TASK.md written.
func ignoredScaffoldFx(t *testing.T) *fx {
	t.Helper()
	f := discardFx(t)
	exclude, err := os.OpenFile(f.main+"/.git/info/exclude", os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := exclude.WriteString("WORKER_TASK.md\nSPEC.md\nPLAN.md\nDECOMPOSITION.md\nREVIEW_NOTES.md\nPLAN_ROUND*.md\n"); err != nil {
		t.Fatal(err)
	}
	_ = exclude.Close()
	portFWrite(t, f.wt+"/SPEC.md", "spec\n")
	portFWrite(t, f.wt+"/WORKER_TASK.md", "task\n")
	return f
}

func treeFiles(t *testing.T, dir, tree string) []string {
	t.Helper()
	return strings.Fields(gitOut(t, dir, "ls-tree", "-r", "--name-only", tree))
}

// The forced add takes the ignored scaffold that exists; a missing name is
// no pathspec, and WORKER_TASK.md (the remote writes its own) never rides.
func TestScratchTreeIgnoredScaffold(t *testing.T) {
	f := ignoredScaffoldFx(t)
	c := f.candidate()
	for _, tc := range []struct {
		ignored bool
		want    []string
	}{{false, []string{"a.txt"}}, {true, []string{"SPEC.md", "a.txt"}}} {
		idx := filepath.Join(t.TempDir(), "index")
		tree, err := ScratchTree(t.Context(), c.admin, c.wtpath, "refs/heads/"+rb, idx, tc.ignored, &f.stderr)
		if err != nil {
			t.Fatalf("ignored=%v: %v stderr=%q", tc.ignored, err, f.stderr.String())
		}
		if got := treeFiles(t, f.main, tree); !slices.Equal(got, tc.want) {
			t.Fatalf("ignored=%v: tree holds %q, want %q", tc.ignored, got, tc.want)
		}
	}
	f.wantErr("")
}

func TestSnapshotIgnoredScaffold(t *testing.T) {
	f := ignoredScaffoldFx(t)
	portFWrite(t, f.wt+"/a.txt", "one\ntwo\n")
	c := f.candidate()
	tip := gitOut(t, f.main, "rev-parse", "refs/heads/"+rb)
	req := SnapshotRequest{
		Admin: c.admin, WtPath: c.wtpath, Branch: rb, Tmp: t.TempDir(),
		Root: f.paths.Dir + "/artifacts", Dir: rb + "/migrate-x", Name: "patch",
		IgnoredScaffold: true, Stderr: &f.stderr,
	}
	res, err := Snapshot(t.Context(), req)
	if err != nil {
		t.Fatalf("%v stderr=%q", err, f.stderr.String())
	}
	if want := req.Root + "/" + req.Dir + "/patch"; res.Patch != want || res.Tip != tip {
		t.Fatalf("result %+v, want patch %q tip %s", res, want, tip)
	}
	if got := treeFiles(t, f.main, res.Tree); !slices.Equal(got, []string{"SPEC.md", "a.txt"}) {
		t.Fatalf("tree holds %q", got)
	}
	// The patch onto a fresh checkout of the tip reproduces the scratch tree.
	fresh := filepath.Join(f.root, "fresh")
	runGit(t, f.main, "worktree", "add", "-q", "--detach", fresh, tip)
	runGit(t, fresh, "apply", "--index", res.Patch)
	if got := gitOut(t, fresh, "write-tree"); got != res.Tree {
		t.Fatalf("applied tree %s, want %s", got, res.Tree)
	}
	if _, err := os.Lstat(fresh + "/WORKER_TASK.md"); !os.IsNotExist(err) {
		t.Fatalf("WORKER_TASK.md travelled: %v", err)
	}
}

func TestSnapshotCleanTree(t *testing.T) {
	f := discardFx(t)
	c := f.candidate()
	res, err := Snapshot(t.Context(), SnapshotRequest{
		Admin: c.admin, WtPath: c.wtpath, Branch: rb, Tmp: t.TempDir(),
		Root: f.paths.Dir + "/artifacts", Dir: rb, Name: "patch", IgnoredScaffold: true, Stderr: &f.stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Patch != "" || res.Tree != gitOut(t, f.main, "rev-parse", rb+"^{tree}") {
		t.Fatalf("result %+v", res)
	}
	if _, err := os.Lstat(f.paths.Dir + "/artifacts"); !os.IsNotExist(err) {
		t.Fatalf("artifacts made for an empty patch: %v", err)
	}
}

// KillWorkerWindows kills the windows stamped or sitting at wt and the
// processes still rooted in it.
func TestKillWorkerWindows(t *testing.T) {
	f := newFx(t)
	f.tmux["list-windows -a -F #{window_id} #{pane_current_path} #{@worktree}"] = strings.Join([]string{
		"@1 " + f.wt + " ", "@2 /elsewhere " + f.wt, "@3 " + f.wt + "x /x",
	}, "\n")
	f.procs.Procs = map[int]proctest.Proc{101: {Cwd: f.wt + "/sub"}, 102: {Cwd: f.wt + "x"}}
	p := f.opts().Probes
	var said []string
	KillWorkerWindows(t.Context(), p.Tmux, p.Proc, f.wt, func(s string) { said = append(said, s) })
	var kills []string
	for _, c := range f.calls {
		if strings.HasPrefix(c, "tmux kill-window") {
			kills = append(kills, c)
		}
	}
	if want := []string{"tmux kill-window -t @1", "tmux kill-window -t @2"}; !slices.Equal(kills, want) {
		t.Fatalf("kills %q, want %q", kills, want)
	}
	if k := f.procs.Killed(); !slices.Equal(k, []int{101}) {
		t.Fatalf("killed %v", k)
	}
	if want := []string{"killed pid 101 (cwd " + f.wt + "/sub)"}; !slices.Equal(said, want) {
		t.Fatalf("said %q", said)
	}
}
