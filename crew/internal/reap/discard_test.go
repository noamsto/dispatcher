package reap

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/filetest"
)

// discardFx is a done worker on rb whose PR is MERGED, with one commit of its
// own so the patch has a tip to apply to.
func discardFx(t *testing.T) *fx {
	t.Helper()
	f := newFx(t)
	if err := os.WriteFile(f.wt+"/a.txt", []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, f.wt, "add", "a.txt")
	git(t, f.wt, "commit", "-q", "-m", "a")
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
	git(t, f.main, "worktree", "add", "-q", fresh, rb)
	git(t, fresh, "apply", "--check", patch)
	git(t, fresh, "apply", patch)
	if got, _ := os.ReadFile(fresh + "/a.txt"); string(got) != "one\ntwo\n" {
		t.Fatalf("a.txt = %q", got)
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
		if !filetest.IsDir(f.wt) {
			t.Fatal("worktree removed")
		}
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
	git(t, nested, "init", "-q")
	git(t, nested, "commit", "-q", "--allow-empty", "-m", "n")
	if rc := f.reap(f.main, Request{Discard: rb}); rc != 1 {
		t.Fatalf("rc=%d stdout=%q", rc, f.stdout.String())
	}
	f.wantErr("crew reap: refusing --discard feat/r — it holds an embedded git repository\n")
	f.noScratch()
	if !filetest.IsDir(nested + "/.git") {
		t.Fatal("nested repo removed")
	}
}

// A fatal signal mid-save runs the cleaner, which removes the registered
// scratch dir the way discardBranch registers it.
func TestDiscardScratchRemovedOnSignal(t *testing.T) {
	f := newFx(t)
	r := f.run()
	tmp, err := os.MkdirTemp(f.paths.Dir, "discard.")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tmp+"/index", []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.clean.setTmp(tmp)
	r.clean.cleanup()
	f.noScratch()
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
	idx := filepath.Join(t.TempDir(), "index")
	if !r.scratchTree(t.Context(), c, idx) {
		t.Fatal("scratchTree failed")
	}
	tree, ok := r.wtIndexOutput(t.Context(), c, idx, &f.stderr, "write-tree")
	if !ok {
		t.Fatal("write-tree failed")
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
