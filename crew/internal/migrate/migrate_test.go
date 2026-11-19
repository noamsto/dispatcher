package migrate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/git"
	"github.com/noamsto/dispatcher/crew/internal/gitguard"
	"github.com/noamsto/dispatcher/crew/internal/migrate/release"
	"github.com/noamsto/dispatcher/crew/internal/proc"
	"github.com/noamsto/dispatcher/crew/internal/proc/proctest"
	"github.com/noamsto/dispatcher/crew/internal/sender"
	"github.com/noamsto/dispatcher/crew/internal/tmux/tmuxtest"
	"github.com/noamsto/dispatcher/crew/internal/wtgit"
)

const (
	mb      = "feat/m"
	panes   = "list-panes -a -F #{window_id}\t#{pane_id}\t#{pane_current_command}\t#{pane_current_path}"
	windows = "list-windows -a -F #{window_id} #{pane_current_path} #{@worktree}"
	nowUnix = int64(1_800_000_000)
)

// fx is one migrate scenario: a real repo whose main branch gitignores the
// root scaffold, a linked worktree on mb with one commit of its own and a
// dispatcher record, a bus, and fake tmux and processes.
type fx struct {
	t              *testing.T
	root, main, wt string
	paths          bus.Paths
	env            map[string]string
	tmux           map[string]string
	procs          *proctest.Table
	stubborn       bool // the fake engine survives SIGTERM
	now            time.Time
	sleeps         int
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
	f := &fx{
		t: t, root: root, main: filepath.Join(root, "main"), wt: filepath.Join(root, "wt"),
		env: map[string]string{}, tmux: map[string]string{}, procs: &proctest.Table{Procs: map[int]proctest.Proc{}},
		now: time.Unix(nowUnix, 0),
	}
	if err := os.Mkdir(f.main, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, f.main, "init", "-q", "-b", "main")
	write(t, f.main+"/.gitignore", "SPEC.md\nPLAN.md\nDECOMPOSITION.md\nREVIEW_NOTES.md\nPLAN_ROUND*.md\nWORKER_TASK.md\n")
	runGit(t, f.main, "add", ".gitignore")
	runGit(t, f.main, "commit", "-q", "-m", "init")
	runGit(t, f.main, "worktree", "add", "-q", "-b", mb, f.wt)
	write(t, f.wt+"/a.txt", "one\n")
	runGit(t, f.wt, "add", "a.txt")
	runGit(t, f.wt, "commit", "-q", "-m", "a")
	common := filepath.Join(f.main, ".git")
	var stderr bytes.Buffer
	if err := gitguard.Init(common, &stderr); err != nil {
		t.Fatalf("gitguard.Init: %v\n%s", err, stderr.String())
	}
	f.paths = bus.Paths{Common: common, Dir: common + "/crew", Log: common + "/crew/events.jsonl"}
	if err := os.MkdirAll(f.paths.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	admin, err := wtgit.AdminDir(common, f.wt)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := wtgit.AnchorPath(f.wt)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(rec), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, rec, f.wt+"\n"+f.paths.Dir+"\n"+mb+"\n"+admin+"\n")
	t.Chdir(f.main)
	return f
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// dyingTable is the fake process table: Kill takes the process away unless
// the scenario's engine is stubborn.
type dyingTable struct{ f *fx }

func (d dyingTable) Alive(pid int) bool                  { return d.f.procs.Alive(pid) }
func (d dyingTable) Elapsed(pid int) (int64, bool)       { return d.f.procs.Elapsed(pid) }
func (d dyingTable) Start(pid int) (string, bool)        { return d.f.procs.Start(pid) }
func (d dyingTable) Parent(pid int) (int, bool)          { return d.f.procs.Parent(pid) }
func (d dyingTable) Comm(pid int) (string, bool)         { return d.f.procs.Comm(pid) }
func (d dyingTable) Args(pid int) string                 { return d.f.procs.Args(pid) }
func (d dyingTable) Cwds(ctx context.Context) []proc.Cwd { return d.f.procs.Cwds(ctx) }
func (d dyingTable) Kill(pid int) {
	d.f.procs.Kill(pid)
	if !d.f.stubborn {
		delete(d.f.procs.Procs, pid)
	}
}

func (f *fx) opts() Options {
	return Options{
		Tmux: &tmuxtest.Fake{Func: func(args []string) (string, error) {
			return f.tmux[strings.Join(args, " ")], nil
		}},
		Proc:   dyingTable{f},
		Git:    git.Exec{},
		Sender: sender.Probes{Getenv: func(k string) string { return f.env[k] }},
		Now:    func() time.Time { return f.now },
		Sleep: func(d time.Duration) {
			f.now = f.now.Add(d)
			f.sleeps++
		},
		Append: bus.Append,
	}
}

func (f *fx) reset() {
	f.stdout.Reset()
	f.stderr.Reset()
}

func (f *fx) snapshot(cwd string) int {
	f.reset()
	return Snapshot(context.Background(), Request{Crew: "c1", Branch: mb}, f.paths, cwd, &f.stdout, &f.stderr, f.opts())
}

func (f *fx) mark(req MarkRequest) int {
	f.reset()
	return Mark(context.Background(), req, f.paths, f.main, &f.stdout, &f.stderr, f.opts())
}

func (f *fx) stop(timeout int) int {
	f.reset()
	return Stop(context.Background(), StopRequest{Request: Request{Crew: "c1", Branch: mb}, Timeout: timeout}, f.paths, f.main, &f.stdout, &f.stderr, f.opts())
}

type snapOut struct {
	Dir, Patch, Tree, Tip string
}

func (f *fx) snapOut() snapOut {
	f.t.Helper()
	line := f.stdout.String()
	if strings.Count(line, "\n") != 1 || !strings.HasSuffix(line, "\n") {
		f.t.Fatalf("stdout is not one line: %q", line)
	}
	var keys map[string]string
	if err := json.Unmarshal([]byte(line), &keys); err != nil || len(keys) != 4 {
		f.t.Fatalf("stdout %q: %v", line, err)
	}
	return snapOut{Dir: keys["dir"], Patch: keys["patch"], Tree: keys["tree"], Tip: keys["tip"]}
}

func (f *fx) migrateDir() string {
	return f.paths.Dir + "/migrate/" + mb + "/" + time.Unix(nowUnix, 0).UTC().Format("20060102T150405Z")
}

func (f *fx) noArtifacts() {
	f.t.Helper()
	for _, d := range []string{"/artifacts", "/migrate"} {
		if _, err := os.Lstat(f.paths.Dir + d); !os.IsNotExist(err) {
			f.t.Fatalf("%s written: %v", d, err)
		}
	}
	if m, _ := filepath.Glob(f.paths.Dir + "/migrate.*"); len(m) != 0 {
		f.t.Fatalf("scratch dirs left: %q", m)
	}
}

// applied checks out the tip in a fresh worktree, applies the patch to it with
// its index, and returns the fresh worktree and its index's tree.
func (f *fx) applied(out snapOut) (string, string) {
	f.t.Helper()
	fresh := filepath.Join(f.root, "fresh")
	runGit(f.t, f.main, "worktree", "add", "-q", "--detach", fresh, out.Tip)
	runGit(f.t, fresh, "apply", "--check", out.Patch)
	runGit(f.t, fresh, "apply", "--index", out.Patch)
	return fresh, runGit(f.t, fresh, "write-tree")
}

func TestSnapshotCarriesTrackedStagedUntrackedAndIgnoredScaffold(t *testing.T) {
	f := newFx(t)
	write(t, f.wt+"/a.txt", "one\ntwo\n")
	write(t, f.wt+"/staged.txt", "staged\n")
	runGit(t, f.wt, "add", "staged.txt")
	write(t, f.wt+"/untracked.bin", "\x00\x01x")
	for _, name := range []string{"SPEC.md", "PLAN.md", "DECOMPOSITION.md", "REVIEW_NOTES.md", "PLAN_ROUND2.md", "WORKER_TASK.md"} {
		write(t, f.wt+"/"+name, name+"\n")
	}

	if rc := f.snapshot(f.main); rc != 0 {
		t.Fatalf("rc=%d stderr=%q", rc, f.stderr.String())
	}
	out := f.snapOut()
	dir := f.migrateDir()
	if out.Dir != dir || out.Patch != dir+"/patch" {
		t.Fatalf("dir=%q patch=%q, want %q", out.Dir, out.Patch, dir)
	}
	f.freshBundleDir(dir)
	if out.Tip != runGit(t, f.main, "rev-parse", "refs/heads/"+mb) {
		t.Fatalf("tip=%q", out.Tip)
	}
	fresh, tree := f.applied(out)
	if tree != out.Tree {
		t.Fatalf("applied tree %s, snapshot tree %s", tree, out.Tree)
	}
	for _, name := range []string{"SPEC.md", "PLAN.md", "DECOMPOSITION.md", "REVIEW_NOTES.md", "PLAN_ROUND2.md"} {
		if got, _ := os.ReadFile(fresh + "/" + name); string(got) != name+"\n" {
			t.Errorf("%s = %q", name, got)
		}
	}
	if _, err := os.Lstat(fresh + "/WORKER_TASK.md"); !os.IsNotExist(err) {
		t.Errorf("WORKER_TASK.md travelled: %v", err)
	}
	// The worker's own index and files are untouched.
	if st := runGit(t, f.wt, "status", "--porcelain"); st != "M a.txt\nA  staged.txt\n?? untracked.bin" {
		t.Errorf("worktree status changed: %q", st)
	}
}

func TestSnapshotWithOnlySpecPresent(t *testing.T) {
	f := newFx(t)
	write(t, f.wt+"/SPEC.md", "spec\n")
	if rc := f.snapshot(f.main); rc != 0 {
		t.Fatalf("rc=%d stderr=%q", rc, f.stderr.String())
	}
	out := f.snapOut()
	fresh, tree := f.applied(out)
	if tree != out.Tree {
		t.Fatalf("applied tree %s, snapshot tree %s", tree, out.Tree)
	}
	if got, _ := os.ReadFile(fresh + "/SPEC.md"); string(got) != "spec\n" {
		t.Fatalf("SPEC.md = %q", got)
	}
}

// freshBundleDir checks dir is a real 0700 directory snapshot made, outside
// the worker-writable artifacts tree.
func (f *fx) freshBundleDir(dir string) {
	f.t.Helper()
	fi, err := os.Lstat(dir)
	if err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		f.t.Fatalf("bundle dir %s: %v %v", dir, fi, err)
	}
	if _, err := os.Lstat(f.paths.Dir + "/artifacts"); !os.IsNotExist(err) {
		f.t.Fatalf("artifacts written: %v", err)
	}
}

// A clean tree has no patch, but its bundle dir is still made.
func TestSnapshotCleanTree(t *testing.T) {
	f := newFx(t)
	if rc := f.snapshot(f.main); rc != 0 {
		t.Fatalf("rc=%d stderr=%q", rc, f.stderr.String())
	}
	out := f.snapOut()
	if out.Patch != "" || out.Dir != f.migrateDir() {
		t.Fatalf("got %+v", out)
	}
	if out.Tree != runGit(t, f.main, "rev-parse", out.Tip+"^{tree}") {
		t.Fatalf("tree %s is not the tip's", out.Tree)
	}
	f.freshBundleDir(out.Dir)
	if ents, _ := os.ReadDir(out.Dir); len(ents) != 0 {
		t.Fatalf("clean bundle dir holds %v", ents)
	}
}

// The bundle dir is always snapshot's own: one already there (a second
// snapshot in the same second, or one planted ahead) is refused, and so is a
// symlink on the path to it.
func TestSnapshotRefusesAnExistingOrLinkedBundleDir(t *testing.T) {
	for _, tc := range []struct {
		name  string
		plant func(f *fx)
		why   string
	}{
		{"clean tree, dir exists", func(f *fx) {
			if err := os.MkdirAll(f.migrateDir(), 0o755); err != nil {
				f.t.Fatal(err)
			}
		}, "bundle dir already exists: "},
		{"dirty tree, dir exists", func(f *fx) {
			write(f.t, f.wt+"/a.txt", "dirty\n")
			if err := os.MkdirAll(f.migrateDir(), 0o755); err != nil {
				f.t.Fatal(err)
			}
		}, "bundle dir already exists: "},
		{"leaf is a symlink", func(f *fx) {
			if err := os.MkdirAll(filepath.Dir(f.migrateDir()), 0o755); err != nil {
				f.t.Fatal(err)
			}
			if err := os.Symlink(f.root, f.migrateDir()); err != nil {
				f.t.Fatal(err)
			}
		}, " is a symlink or not a directory"},
		{"parent is a symlink", func(f *fx) {
			write(f.t, f.wt+"/a.txt", "dirty\n")
			if err := os.MkdirAll(f.paths.Dir+"/migrate", 0o755); err != nil {
				f.t.Fatal(err)
			}
			if err := os.Symlink(f.root, f.paths.Dir+"/migrate/feat"); err != nil {
				f.t.Fatal(err)
			}
		}, " is a symlink or not a directory"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFx(t)
			tc.plant(f)
			if rc := f.snapshot(f.main); rc != 1 || f.stdout.String() != "" || !strings.Contains(f.stderr.String(), tc.why) {
				t.Fatalf("rc=%d stdout=%q stderr=%q, want %q", rc, f.stdout.String(), f.stderr.String(), tc.why)
			}
			if _, err := os.Lstat(f.migrateDir() + "/patch"); !os.IsNotExist(err) {
				t.Fatalf("patch written: %v", err)
			}
			if _, err := os.Lstat(f.root + "/m"); !os.IsNotExist(err) {
				t.Fatalf("wrote through the symlink: %v", err)
			}
		})
	}
}

func TestSnapshotRefusals(t *testing.T) {
	f := newFx(t)
	write(t, f.wt+"/a.txt", "dirty\n")
	refused := func(why string, cwd string) {
		t.Helper()
		if rc := f.snapshot(cwd); rc != 1 {
			t.Fatalf("%q: rc=%d stdout=%q", why, rc, f.stdout.String())
		}
		if f.stdout.String() != "" || !strings.Contains(f.stderr.String(), why) {
			t.Fatalf("want %q, stdout=%q stderr=%q", why, f.stdout.String(), f.stderr.String())
		}
		f.noArtifacts()
	}

	f.tmux[panes] = "@5\t%5\tclaude\t" + f.wt
	refused("crew migrate: refusing snapshot feat/m — an engine is running there\n", f.main)
	f.tmux[panes] = ""

	refused("crew migrate: refusing snapshot feat/m — it is the current worktree\n", f.wt)

	f.env["CREW_WORKER_ID"] = "worker:feat/m#s1-1"
	refused("only the dispatcher posts as dispatcher:<crew>", f.main)
	delete(f.env, "CREW_WORKER_ID")

	admin, _ := wtgit.AdminDir(f.paths.Common, f.wt)
	write(t, admin+"/config.worktree", "")
	refused("config.worktree exists\n", f.main)
	if err := os.Remove(admin + "/config.worktree"); err != nil {
		t.Fatal(err)
	}

	rec, _ := wtgit.AnchorPath(f.wt)
	write(t, rec, f.wt+"\n"+f.paths.Dir+"\nfeat/other\n"+admin+"\n")
	refused("crew migrate: refusing snapshot feat/m — its dispatcher record does not match (branch)\n", f.main)
	if err := os.Remove(rec); err != nil {
		t.Fatal(err)
	}
	refused("crew migrate: refusing snapshot feat/m — no dispatcher record for "+f.wt+"\n", f.main)
	write(t, rec, f.wt+"\n"+f.paths.Dir+"\n"+mb+"\n"+admin+"\n")

	nested := f.wt + "/nested"
	if err := os.Mkdir(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, nested, "init", "-q")
	runGit(t, nested, "commit", "-q", "--allow-empty", "-m", "n")
	refused("crew migrate: refusing snapshot feat/m — it holds an embedded git repository\n", f.main)
	if err := os.RemoveAll(nested); err != nil {
		t.Fatal(err)
	}

	runGit(t, f.main, "config", "core.fsmonitor", "/evil")
	refused("core.fsmonitor", f.main)
}

func TestSnapshotBranchRefusals(t *testing.T) {
	f := newFx(t)
	for branch, why := range map[string]string{
		"feat/..bad": "not a valid branch name",
		"@{-1}":      "not a valid branch name",
		"feat/none":  "no worktree for it",
	} {
		f.reset()
		rc := Snapshot(context.Background(), Request{Crew: "c1", Branch: branch}, f.paths, f.main, &f.stdout, &f.stderr, f.opts())
		if want := "crew migrate: refusing snapshot " + branch + " — " + why + "\n"; rc != 1 || f.stderr.String() != want {
			t.Errorf("%s: rc=%d stderr=%q, want %q", branch, rc, f.stderr.String(), want)
		}
	}
	f.reset()
	if rc := Snapshot(context.Background(), Request{Branch: mb}, f.paths, f.main, &f.stdout, &f.stderr, f.opts()); rc != 1 ||
		!strings.Contains(f.stderr.String(), "no crew id") {
		t.Errorf("no crew: rc=%d stderr=%q", rc, f.stderr.String())
	}
	f.noArtifacts()
}

func (f *fx) bus(rows ...string) {
	f.t.Helper()
	write(f.t, f.paths.Log, strings.Join(rows, "\n")+"\n")
}

func dispatchRow(session string) string {
	return fmt.Sprintf(`{"ts":1,"crew_id":"c1","kind":"dispatch","branch":%q,"session":%q,"worker_id":"worker:%s#%s"}`, mb, session, mb, session)
}

func resumeRow(prev, next string) string {
	return fmt.Sprintf(`{"ts":2,"crew_id":"c1","kind":"resume","branch":%q,"prev_worker_id":"worker:%s#%s","worker_id":"worker:%s#%s"}`, mb, mb, prev, mb, next)
}

func statusRow(session, body string) string {
	return fmt.Sprintf(`{"ts":3,"crew_id":"c1","from":"worker:%s#%s","to":"dispatcher:c1","kind":"status","body":%s}`, mb, session, body)
}

const (
	tip40  = "0123456789abcdef0123456789abcdef01234567"
	tree40 = "89abcdef0123456789abcdef0123456789abcdef"
)

// bundleDir makes branch's bundle dir as snapshot does and returns it.
func (f *fx) bundleDir(branch, ts string) string {
	f.t.Helper()
	dir := f.paths.Dir + "/migrate/" + branch + "/" + ts
	if err := os.MkdirAll(dir, 0o700); err != nil {
		f.t.Fatal(err)
	}
	return dir
}

const markTS = "20270115T080000Z"

func TestMarkAppendsTheReleasingRow(t *testing.T) {
	f := newFx(t)
	f.bus(dispatchRow("s1-1"), resumeRow("s1-1", "s2-1"), statusRow("s2-1", `{"state":"working"}`))
	dir := f.bundleDir(mb, markTS)
	patch := dir + "/patch"
	write(t, patch, "diff\n")
	req := MarkRequest{Request: Request{Crew: "c1", Branch: mb}, ToHost: "mbp", Mode: "now", Tip: tip40, Tree: tree40, Dir: dir, Patch: patch}
	if rc := f.mark(req); rc != 0 {
		t.Fatalf("rc=%d stderr=%q", rc, f.stderr.String())
	}
	evs, err := bus.ReadEvents(f.paths.Log)
	if err != nil || len(evs) != 4 {
		t.Fatalf("events %d: %v", len(evs), err)
	}
	row := evs[3].Doc.(map[string]any)
	got, _ := json.Marshal(row)
	want := fmt.Sprintf(`{"body":{"detail":"moved to mbp","dir":%q,"mode":"now","patch":%q,"source":"migrate","state":"migrated","tip":%q,"to_host":"mbp","tree":%q},"crew_id":"c1","from":"worker:feat/m#s2-1","kind":"status","to":"dispatcher:c1","ts":%d}`,
		dir, patch, tip40, tree40, nowUnix*1000)
	if string(got) != want {
		t.Fatalf("row\n got %s\nwant %s", got, want)
	}
	if !release.Released(evs, mb, "mbp") {
		t.Fatal("not released after the mark")
	}
	rel, ok := release.Active(evs, mb)
	if !ok || rel != (release.Release{ToHost: "mbp", Mode: "now", Tip: tip40, Tree: tree40, Dir: dir, Patch: patch}) {
		t.Fatalf("release %+v %v", rel, ok)
	}

	// Without a patch the row records "", and still names the bundle dir.
	req.Patch, req.Mode = "", "coop"
	if rc := f.mark(req); rc != 0 {
		t.Fatalf("rc=%d stderr=%q", rc, f.stderr.String())
	}
	evs, _ = bus.ReadEvents(f.paths.Log)
	if rel, ok := release.Active(evs, mb); !ok || rel.Patch != "" || rel.Mode != "coop" || rel.Dir != dir {
		t.Fatalf("release %+v %v", rel, ok)
	}
}

func TestMarkRefusals(t *testing.T) {
	f := newFx(t)
	f.bus(dispatchRow("s1-1"))
	before, _ := os.ReadFile(f.paths.Log)
	dir := f.bundleDir(mb, markTS)
	noneDir := f.bundleDir("feat/none", markTS)
	other := f.bundleDir(mb, "20270115T080001Z")
	write(t, other+"/patch", "diff\n")
	good := MarkRequest{Request: Request{Crew: "c1", Branch: mb}, ToHost: "mbp", Mode: "coop", Tip: tip40, Tree: tree40, Dir: dir}
	outside := filepath.Join(f.root, "patch")
	write(t, outside, "diff\n")
	if err := os.Symlink(outside, dir+"/patch"); err != nil {
		t.Fatal(err)
	}
	// Well-shaped bundle dirs that are not real directories snapshot made.
	linked := f.paths.Dir + "/migrate/" + mb + "/20270115T080002Z"
	if err := os.Symlink(f.root, linked); err != nil {
		t.Fatal(err)
	}
	file := f.paths.Dir + "/migrate/" + mb + "/20270115T080003Z"
	write(t, file, "")
	missing := f.paths.Dir + "/migrate/" + mb + "/20270115T080004Z"
	f.bundleDir("feat/real", markTS)
	if err := os.Symlink(f.paths.Dir+"/migrate/feat/real", f.paths.Dir+"/migrate/feat/lnk"); err != nil {
		t.Fatal(err)
	}
	linkedParent := f.paths.Dir + "/migrate/feat/lnk/" + markTS
	artifacts := f.paths.Dir + "/artifacts/" + mb + "/migrate-" + markTS
	if err := os.MkdirAll(artifacts, 0o755); err != nil {
		t.Fatal(err)
	}
	notDir := func(d string) string { return "--dir " + d + " is not a migrate bundle dir of feat/m" }
	notPatch := func(p string) string { return "--patch " + p + " is not a migrate patch of feat/m" }
	for _, c := range []struct {
		why string
		mut func(*MarkRequest)
	}{
		{"invalid --to-host '-x'", func(r *MarkRequest) { r.ToHost = "-x" }},
		{"invalid --to-host 'a b'", func(r *MarkRequest) { r.ToHost = "a b" }},
		{"--mode must be coop or now (got 'later')", func(r *MarkRequest) { r.Mode = "later" }},
		{"--tip must be a full commit id (got 'abc')", func(r *MarkRequest) { r.Tip = "abc" }},
		{"--tip must be a full commit id (got '" + strings.ToUpper(tip40) + "')", func(r *MarkRequest) { r.Tip = strings.ToUpper(tip40) }},
		{"--tree must be a full tree id (got 'x')", func(r *MarkRequest) { r.Tree = "x" }},
		{notDir(""), func(r *MarkRequest) { r.Dir = "" }},
		{notDir(f.root), func(r *MarkRequest) { r.Dir = f.root }},
		{notDir(artifacts), func(r *MarkRequest) { r.Dir = artifacts }},
		{notDir(dir + "/"), func(r *MarkRequest) { r.Dir = dir + "/" }},
		{notDir(f.paths.Dir + "/migrate/" + mb + "/notatime"), func(r *MarkRequest) { r.Dir = f.paths.Dir + "/migrate/" + mb + "/notatime" }},
		{notDir(noneDir), func(r *MarkRequest) { r.Dir = noneDir }},
		{notDir(linked), func(r *MarkRequest) { r.Dir = linked }},
		{notDir(file), func(r *MarkRequest) { r.Dir = file }},
		{notDir(missing), func(r *MarkRequest) { r.Dir = missing }},
		{"--dir " + linkedParent + " is not a migrate bundle dir of feat/lnk", func(r *MarkRequest) { r.Branch, r.Dir = "feat/lnk", linkedParent }},
		{notPatch(outside), func(r *MarkRequest) { r.Patch = outside }},
		{notPatch(other + "/patch"), func(r *MarkRequest) { r.Patch = other + "/patch" }},
		{notPatch(dir + "/patch"), func(r *MarkRequest) { r.Patch = dir + "/patch" }},
		{"no dispatch row for it", func(r *MarkRequest) { r.Branch, r.Dir = "feat/none", noneDir }},
		{"not a valid branch name", func(r *MarkRequest) { r.Branch = "feat/..x" }},
		{"only the dispatcher posts as dispatcher:<crew>", func(*MarkRequest) { f.env["CREW_WORKER_ID"] = "worker:feat/m#s1-1" }},
	} {
		req := good
		c.mut(&req)
		if rc := f.mark(req); rc != 1 || !strings.Contains(f.stderr.String(), c.why) {
			t.Errorf("%s: rc=%d stderr=%q", c.why, rc, f.stderr.String())
		}
		if after, _ := os.ReadFile(f.paths.Log); !bytes.Equal(before, after) {
			t.Fatalf("%s: bus written", c.why)
		}
	}
}

func (f *fx) engine() {
	f.procs.Procs[4242] = proctest.Proc{Comm: "claude", Cwd: f.wt + "/sub"}
	f.tmux[windows] = "@7 " + f.wt + " " + f.wt
}

func TestStopKillsAndWaits(t *testing.T) {
	f := newFx(t)
	f.engine()
	if rc := f.stop(30); rc != 0 {
		t.Fatalf("rc=%d stderr=%q", rc, f.stderr.String())
	}
	if got := f.procs.Killed(); len(got) != 1 || got[0] != 4242 {
		t.Fatalf("killed %v", got)
	}
	if _, alive := f.procs.Procs[4242]; alive {
		t.Fatal("engine still alive")
	}
}

func TestStopFailsNamingTheSurvivor(t *testing.T) {
	f := newFx(t)
	f.engine()
	f.stubborn = true
	if rc := f.stop(3); rc != 1 {
		t.Fatalf("rc=%d", rc)
	}
	if want := "crew migrate: stop feat/m: pid 4242 (claude) still runs in " + f.wt + " after 3s\n"; f.stderr.String() != want {
		t.Fatalf("stderr %q, want %q", f.stderr.String(), want)
	}
	if f.sleeps < 3 {
		t.Fatalf("gave up after %d sleeps", f.sleeps)
	}

	// An engine pane that outlives kill-window is a survivor too.
	f.procs.Procs = map[int]proctest.Proc{}
	f.tmux[panes] = "@5\t%5\tclaude\t" + f.wt
	if rc := f.stop(0); rc != 1 || !strings.Contains(f.stderr.String(), "pane %5 (claude) still runs in "+f.wt) {
		t.Fatalf("rc=%d stderr=%q", rc, f.stderr.String())
	}
}

func TestStopRefusesAWorker(t *testing.T) {
	f := newFx(t)
	f.engine()
	f.env["CREW_ROLE_ID"] = "role:feat/m:critic"
	if rc := f.stop(30); rc != 1 || !strings.Contains(f.stderr.String(), "crew migrate: refusing stop feat/m — ") {
		t.Fatalf("rc=%d stderr=%q", rc, f.stderr.String())
	}
	if len(f.procs.Killed()) != 0 {
		t.Fatal("killed for a worker")
	}
}
