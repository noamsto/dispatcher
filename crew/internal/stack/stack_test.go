package stack

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
	"github.com/noamsto/dispatcher/crew/internal/gitguard"
	"github.com/noamsto/dispatcher/crew/internal/testjson"
	"github.com/noamsto/dispatcher/crew/internal/wtgit"
)

// fixture is the bats `_stack_fixture`/`_align_fixture` in Go: a main checkout
// with a bare origin, the branches the test asks for, a bus, and a gh stub that
// serves per-PR JSON the way the bats stub serves it from files.
type fixture struct {
	t               *testing.T
	root, main, log string
	stdout, stderr  *bytes.Buffer
	prs             map[string]pr
	defaultBranch   string
	ghCalls         []string
	origWd          string
	now             time.Time
	appended        []string
}

// pr is one entry of the gh stub: the JSON `gh pr view` prints for a number and
// for a head branch.
type pr struct {
	number int
	head   string
	base   string
	state  string
	cross  bool
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// The suite isolates the same way: no user config, and no dispatcher anchor
	// record for a path under this temp dir.
	t.Setenv("HOME", root)
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("CREW_ID", "c1")

	f := &fixture{
		t: t, root: root, main: filepath.Join(root, "main"),
		stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{},
		prs: map[string]pr{}, defaultBranch: "main",
		now: time.Unix(1_800_000_000, 0),
	}
	f.git(root, "init", "-q", "--bare", "-b", "main", filepath.Join(root, "origin.git"))
	f.git(root, "init", "-q", "-b", "main", f.main)
	f.git(f.main, "remote", "add", "origin", filepath.Join(root, "origin.git"))
	f.commit(f.main, "main", "base")
	f.push(f.main, "main")
	f.log = filepath.Join(f.main, ".git", "crew", "events.jsonl")
	f.seedBaseline()

	// Run from the main checkout, as the command's own trusted cwd; restore it
	// so a later test starts where it expects.
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	f.origWd = wd
	t.Cleanup(func() { _ = os.Chdir(wd) })
	if err := os.Chdir(f.main); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) seedBaseline() {
	f.t.Helper()
	_ = os.Remove(gitguard.BaselinePath(f.common()))
	var stderr bytes.Buffer
	if err := gitguard.Init(f.common(), &stderr); err != nil {
		f.t.Fatalf("gitguard.Init: %v\n%s", err, stderr.String())
	}
}

func (f *fixture) common() string { return filepath.Join(f.main, ".git") }

func (f *fixture) git(dir string, args ...string) string {
	f.t.Helper()
	full := append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t", "-c", "init.defaultBranch=main"}, args...)
	cmd := exec.Command("git", full...)
	var errOut bytes.Buffer
	cmd.Stderr = &errOut
	out, err := cmd.Output()
	if err != nil {
		f.t.Fatalf("git %v: %v\n%s", args, err, errOut.String())
	}
	return strings.TrimRight(string(out), "\n")
}

// commit puts one empty commit on branch (checked out first).
func (f *fixture) commit(dir, branch, msg string) {
	f.t.Helper()
	f.git(dir, "checkout", "-q", "-B", branch)
	f.git(dir, "commit", "-q", "--allow-empty", "-m", msg)
}

func (f *fixture) head(dir, branch string) string {
	f.t.Helper()
	return f.git(dir, "rev-parse", branch)
}

func (f *fixture) push(dir, branch string) {
	f.t.Helper()
	f.git(dir, "push", "-q", "-f", "origin", branch)
}

// branch creates branch on top of from, commits one empty commit on it and
// pushes it, returning its oid.
func (f *fixture) branch(from, name, msg string) string {
	f.t.Helper()
	f.git(f.main, "checkout", "-q", "--no-track", "-B", name, from)
	f.git(f.main, "commit", "-q", "--allow-empty", "-m", msg)
	f.push(f.main, name)
	return f.head(f.main, name)
}

// resetHard moves branch to a new commit that does not contain its old tip, as
// a rewrite does, and force-pushes it.
func (f *fixture) rewrite(name, msg string) string {
	f.t.Helper()
	f.git(f.main, "checkout", "-q", "-B", name, "origin/main")
	f.git(f.main, "commit", "-q", "--allow-empty", "-m", msg)
	f.push(f.main, name)
	return f.head(f.main, name)
}

func (f *fixture) bus(rows ...string) {
	f.t.Helper()
	if err := os.MkdirAll(filepath.Dir(f.log), 0o755); err != nil {
		f.t.Fatal(err)
	}
	body := ""
	for _, r := range rows {
		body += r + "\n"
	}
	if err := os.WriteFile(f.log, []byte(body), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

// busRows is the bus as rows, for asserting what a run wrote.
func (f *fixture) busRows(t *testing.T, kind string) []map[string]any {
	f.t.Helper()
	data, err := os.ReadFile(f.log)
	if err != nil {
		return nil
	}
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		if line == "" {
			continue
		}
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			f.t.Fatalf("bus row is not JSON: %q", line)
		}
		if row["kind"] == kind {
			out = append(out, row)
		}
	}
	return out
}

// addPR registers one PR with the stub, under both its number and its head.
func (f *fixture) addPR(number int, head, base, state string) {
	f.t.Helper()
	f.prs[numberFor(number)] = pr{number: number, head: head, base: base, state: state}
	f.prs[head] = pr{number: number, head: head, base: base, state: state}
}

func (f *fixture) addCrossPR(number int, head, base string) {
	f.t.Helper()
	f.prs[numberFor(number)] = pr{number: number, head: head, base: base, state: "OPEN", cross: true}
	f.prs[head] = pr{number: number, head: head, base: base, state: "OPEN", cross: true}
}

func numberFor(n int) string { return fmt.Sprintf("%d", n) }

// stackRun runs the command and reports its exit code and both streams.
func (f *fixture) stackRun(t *testing.T, args ...string) (int, string, string) {
	f.t.Helper()
	f.stdout.Reset()
	f.stderr.Reset()
	f.appended = nil
	p := Probes{
		Gh:     f.gh,
		Now:    func() time.Time { return f.now },
		Append: func(path, line string) error { f.appended = append(f.appended, line); return bus.Append(path, line) },
	}
	paths := bus.Paths{Common: f.common(), Dir: f.common() + "/crew", Log: f.log}
	code := Run(context.Background(), args, paths, f.main, f.stdout, f.stderr, Options{Probes: p})
	_ = os.Chdir(f.main)
	return code, f.stdout.String(), f.stderr.String()
}

// gh is the bats stub: `repo view` answers with the default branch, `pr view`
// with the registered JSON, and anything else fails.
func (f *fixture) gh(_ context.Context, args ...string) (string, error) {
	f.ghCalls = append(f.ghCalls, strings.Join(args, " "))
	switch {
	case strings.Join(args, " ") == "repo view --json defaultBranchRef --jq .defaultBranchRef.name":
		return f.defaultBranch, nil
	case len(args) == 5 && args[0] == "pr" && args[1] == "view" && args[3] == "--json" && args[4] == "headRefName,isCrossRepository":
		e, ok := f.prs[args[2]]
		if !ok {
			return "", fmt.Errorf("gh: no PR for %s", args[2])
		}
		return fmt.Sprintf(`{"headRefName":%q,"isCrossRepository":%v}`, e.head, e.cross), nil
	case len(args) == 5 && args[0] == "pr" && args[1] == "view" && args[3] == "--json" &&
		args[4] == "number,headRefName,headRefOid,baseRefName,state,mergedAt,isCrossRepository":
		e, ok := f.prs[args[2]]
		if !ok {
			return "", fmt.Errorf("gh: no PR for %s", args[2])
		}
		return fmt.Sprintf(`{"number":%d,"headRefName":%q,"headRefOid":"deadbeef","baseRefName":%q,"state":%q,"mergedAt":null,"isCrossRepository":%v}`,
			e.number, e.head, e.base, e.state, e.cross), nil
	}
	return "", fmt.Errorf("gh: unexpected call: %v", args)
}

// rows reads back the command's stdout as one JSON value per line.
func rows(t *testing.T, out string) []string {
	t.Helper()
	out = strings.TrimSuffix(out, "\n")
	if out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

// wantRow asserts one layer's row. Each entry of `want` is a field name and the
// JSON text the arm emits for it; every one of the arm's nine keys must be
// present, and the fields a test does not name are left to the sibling rows.
func wantRow(t *testing.T, out, branch string, want map[string]string) {
	t.Helper()
	for _, line := range rows(t, out) {
		if !strings.Contains(line, `"branch":"`+branch+`"`) {
			continue
		}
		var got map[string]any
		if err := json.Unmarshal([]byte(line), &got); err != nil {
			t.Fatalf("row is not JSON: %v\n%s", err, line)
		}
		for _, k := range armRowKeys {
			if _, ok := got[k]; !ok {
				t.Errorf("row for %s has no %q key: %s", branch, k, line)
			}
		}
		if len(got) != len(armRowKeys) {
			t.Errorf("row for %s has %d keys, want the arm's %d: %s", branch, len(got), len(armRowKeys), line)
		}
		for k, w := range want {
			v, ok := got[k]
			if !ok {
				continue
			}
			text, err := json.Marshal(v)
			if err != nil {
				t.Fatal(err)
			}
			if g, e := testjson.Compact(testjson.MustParse(t, string(text))), testjson.Compact(testjson.MustParse(t, w)); g != e {
				t.Errorf("row for %s, field %s:\n got %s\nwant %s", branch, k, g, e)
			}
		}
		return
	}
	t.Fatalf("no row for %s in:\n%s", branch, out)
}

// armRowKeys is the arm's `jq -nc` row, in its order.
var armRowKeys = []string{"pr", "branch", "parent", "parent_pr", "cut", "mode", "ready", "directive", "reason"}

// dispatchRow is the dispatcher's `--base` create row: the only dispatch row a
// cut point may come from.
func dispatchRow(ts int, branch, base, baseOID string) string {
	return fmt.Sprintf(`{"ts":%d,"crew_id":"c1","kind":"dispatch","branch":%q,"base":%q,"base_oid":%s}`,
		ts, branch, base, jsonNull(baseOID))
}

func stackCutRow(ts int, branch, parent, cut string) string {
	return fmt.Sprintf(`{"ts":%d,"crew_id":"c1","kind":"stack_cut","branch":%q,"parent":%q,"cut":%q}`,
		ts, branch, parent, cut)
}

func jsonNull(s string) string {
	if s == "" {
		return "null"
	}
	return fmt.Sprintf("%q", s)
}

// cmd is the parsed command with no argv state, for the fold tests.
func (f *fixture) cmd() *stackCmd {
	f.t.Helper()
	return &stackCmd{
		paths:   bus.Paths{Common: f.common(), Dir: f.common() + "/crew", Log: f.log},
		stdout:  f.stdout,
		stderr:  f.stderr,
		o:       wtgit.Opts{Stderr: io.Discard},
		p:       Probes{Gh: f.gh, Now: func() time.Time { return f.now }, Append: bus.Append},
		crew:    "c1",
		fetched: map[string]string{},
	}
}

func TestUsageAndOid(t *testing.T) {
	f := newFixture(t)
	f.bus()
	for _, args := range [][]string{
		{}, {"cut"}, {"cut", "feat/child", "extra"}, {"cut", "--record"}, {"cut", "--record", "a", "b", "c"},
		{"align"}, {"align", "3", "extra"}, {"nonsense"}, {"cut", "--record", "feat/child", "extra", "oid"},
	} {
		code, out, errOut := f.stackRun(t, args...)
		if code != 1 {
			t.Errorf("%v: exit %d, want 1 (%s)", args, code, errOut)
		}
		if out != "" {
			t.Errorf("%v: stdout %q, want empty", args, out)
		}
		if !strings.HasPrefix(errOut, "usage: crew stack cut [--record] <child-branch|PR#> [<oid>]\n       crew stack align <top-PR#|branch>\n") {
			t.Errorf("%v: stderr %q, want the arm's two usage lines", args, errOut)
		}
	}
}

func TestOidFormat(t *testing.T) {
	f := newFixture(t)
	f.bus()
	const oid = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	for _, bad := range []string{"HEAD", oid[:12], strings.ToUpper(oid), oid + "z", " " + oid} {
		code, out, errOut := f.stackRun(t, "cut", "--record", "feat/child", bad)
		if code != 1 || out != "" || errOut != "crew: '"+bad+"' is not a full lowercase hex commit oid\n" {
			t.Errorf("oid %q: exit %d, stdout %q, stderr %q", bad, code, out, errOut)
		}
	}
	// A sha256 oid is a full oid, so it reaches the next check (the recorded
	// parent, not the oid format).
	if _, _, errOut := f.stackRun(t, "cut", "--record", "feat/child", strings.Repeat("a", 64)); !strings.Contains(errOut, "no recorded parent") {
		t.Errorf("a 64-hex oid was refused as an oid: %s", errOut)
	}
	// An empty second argument is no oid at all, exactly as bash's `[ -n … ]`
	// read it: the format check is skipped and the parent tip path runs.
	if _, _, errOut := f.stackRun(t, "cut", "--record", "feat/child", ""); !strings.Contains(errOut, "no recorded parent") {
		t.Errorf("an empty oid argument: %s", errOut)
	}
}

// TestFoldRecord is `_stack_newest` and `_stack_align_record` over a planted
// bus: which kinds count, which are forged, and what a torn or undecodable line
// does to the fold.
func TestFoldRecord(t *testing.T) {
	f := newFixture(t)
	const (
		oidA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		oidB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		oidC = "cccccccccccccccccccccccccccccccccccccccc"
	)
	f.bus(
		`not json at all`,
		``,
		`{"ts":1,"crew_id":"other","kind":"stack_cut","branch":"feat/child","parent":"feat/other","cut":"`+oidC+`"}`,
		`{"ts":2,"crew_id":"c1","kind":"status","from":"worker:feat/child#s1","branch":"feat/child","body":{"state":"working","cut":"`+oidC+`","base_oid":"`+oidC+`"}}`,
		`{"ts":3,"crew_id":"c1","kind":"msg","from":"worker:feat/child#s1","to":"dispatcher:c1","branch":"feat/child","body":{"kind":"stack_cut","cut":"`+oidC+`","base_oid":"`+oidC+`"}}`,
		dispatchRow(4, "feat/child", "main", ""), // a --pr attach row: base_oid is null
		stackCutRow(5, "feat/child", "feat/parent", oidB),
		`{"ts":6,"crew_id":"c1","kind":"stack_cut","branch":"feat/child","parent":"feat/evil","cut":"\ud83d"}`,
		`{"ts":7,"crew_id":"c1","kind":"stack_cut","branch":"feat/child","parent":"feat/torn"`,
	)
	s := f.cmd()

	if cut, err := s.recordedCut("feat/child"); err != nil || cut != oidB {
		t.Fatalf("recordedCut = %q, %v; want %s", cut, err, oidB)
	}
	if parent, err := s.recordedParent("feat/child"); err != nil || parent != "feat/parent" {
		t.Fatalf("recordedParent = %q, %v; want feat/parent", parent, err)
	}
	parent, cut, err := s.record("feat/child")
	if err != nil || parent != "feat/parent" || cut != oidB {
		t.Fatalf("record = %q, %q, %v", parent, cut, err)
	}
	// A branch with no rows of its own is no record, and no error.
	if cut, err := s.recordedCut("feat/none"); err != nil || cut != "" {
		t.Fatalf("recordedCut(feat/none) = %q, %v", cut, err)
	}
	if parent, cut, err := s.record("feat/none"); err != nil || parent != "" || cut != "" {
		t.Fatalf("record(feat/none) = %q, %q, %v", parent, cut, err)
	}
	// The newest complete row wins: an incomplete dispatch row never pairs its
	// base with an older row's cut.
	f.bus(
		stackCutRow(1, "feat/child", "feat/old", oidA),
		dispatchRow(2, "feat/child", "feat/new", oidB),
	)
	s = f.cmd()
	if parent, cut, err := s.record("feat/child"); err != nil || parent != "feat/new" || cut != oidB {
		t.Fatalf("newest row: %q, %q, %v", parent, cut, err)
	}
	// A recorded cut that is not a full oid is no cut.
	f.bus(`{"ts":1,"crew_id":"c1","kind":"stack_cut","branch":"feat/child","parent":"feat/parent","cut":"deadbeef"}`)
	if parent, cut, err := f.cmd().record("feat/child"); err != nil || parent != "feat/parent" || cut != "" {
		t.Fatalf("short cut: %q, %q, %v", parent, cut, err)
	}
	// A parent no directive can carry is a refusal, printed once.
	f.bus(`{"ts":1,"crew_id":"c1","kind":"stack_cut","branch":"feat/child","parent":"bad name","cut":"` + oidA + `"}`)
	if _, _, err := f.cmd().record("feat/child"); err == nil || !strings.Contains(f.stderr.String(), "unsafe to put in a directive") {
		t.Fatalf("unsafe recorded parent: err %v, stderr %q", err, f.stderr.String())
	}
}

func TestFoldUnreadableBus(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode 000 file")
	}
	f := newFixture(t)
	f.bus(`{"ts":1,"crew_id":"c1","kind":"stack_cut","branch":"feat/child","parent":"feat/parent","cut":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`)
	if err := os.Chmod(f.log, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(f.log, 0o600) })
	s := f.cmd()
	if _, err := s.recordedCut("feat/child"); !errors.Is(err, errBus) {
		t.Fatalf("recordedCut over an unreadable bus = %v, want the bus error", err)
	}
	if _, _, err := s.record("feat/child"); !errors.Is(err, errBus) {
		t.Fatalf("record over an unreadable bus = %v, want the bus error", err)
	}
	// No log file at all is the arm's /dev/null: no rows, no error.
	if err := os.Remove(f.log); err != nil {
		t.Fatal(err)
	}
	if cut, err := f.cmd().recordedCut("feat/child"); err != nil || cut != "" {
		t.Fatalf("no bus: %q, %v", cut, err)
	}
}

// replacePR points the stub's number key and one branch key at one PR, the way
// the bats `_align_pr` helper writes both pr/<number>.json and pr/<head>.json.
func (f *fixture) replacePR(number int, under, head, base, state string) {
	f.t.Helper()
	p := pr{number: number, head: head, base: base, state: state}
	f.prs[numberFor(number)] = p
	f.prs[under] = p
}
