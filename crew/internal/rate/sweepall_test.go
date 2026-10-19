package rate

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/gh/ghtest"
	"github.com/noamsto/dispatcher/crew/internal/git/gittest"
)

// dispatchRow is the one bus event a repo needs to be discoverable: the walk
// finds `<common>/crew/events.jsonl`, and the loop never reads it (the sweep
// behind the seam does).
const dispatchRow = `{"ts":1000,"crew_id":"c1","kind":"dispatch","branch":"feat/x","engine":"claude","model":"sonnet","tier":"standard","effort":"medium","title":"t"}` + "\n"

// sweepAllFixture lays repos out the way --sweep-all finds them: a bus under
// <root>/<name>/.git/crew/events.jsonl for the walk, and a registry line for a
// repo outside the roots. Origins and per-repo sweep results are scripted, so
// what a test reads is the loop's own lines and exit status rather than a real
// store, gh or lock. The sweep behind the seam is sweep_test.go's; the loop with
// its real sweep is TestSweepAllRunsTheRepoSweep.
type sweepAllFixture struct {
	t       *testing.T
	root    string
	data    string // XDG_DATA_HOME
	store   string
	origins map[string]string
	rc      map[string]int
	swept   []string
}

func newSweepAllFixture(t *testing.T) *sweepAllFixture {
	t.Helper()
	f := &sweepAllFixture{
		t:       t,
		root:    t.TempDir(),
		data:    t.TempDir(),
		origins: map[string]string{},
		rc:      map[string]int{},
	}
	f.store = filepath.Join(f.data, "crew", "ratings.jsonl")
	return f
}

// repo creates <root>/<name> with a bus, and an origin only when given one. It
// returns the git common dir, which is what the loop prints for a skipped repo.
func (f *sweepAllFixture) repo(name, origin string) string {
	f.t.Helper()
	common := filepath.Join(f.root, name, ".git")
	if err := os.MkdirAll(filepath.Join(common, "crew"), 0o755); err != nil {
		f.t.Fatal(err)
	}
	f.bus(filepath.Join(common, "crew", "events.jsonl"), dispatchRow)
	if origin != "" {
		f.origins[filepath.Dir(common)] = origin
	}
	return common
}

func (f *sweepAllFixture) bus(path, content string) {
	f.t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

// registry adds a candidate the walk cannot find: dispatch appended its common
// dir once, which is the only reason the loop still knows about it.
func (f *sweepAllFixture) registry(common string) {
	f.t.Helper()
	dir := filepath.Join(f.data, "crew")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		f.t.Fatal(err)
	}
	fl, err := os.OpenFile(filepath.Join(dir, "repos"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = fl.Close() }()
	if _, err := fmt.Fprintln(fl, common); err != nil {
		f.t.Fatal(err)
	}
}

// storeRow is one ratings row, reduced to the two fields the loop's count reads.
func storeRow(repo, runID string) string {
	return fmt.Sprintf(`{"repo":%q,"run_id":%q,"swept_at":1}`, repo, runID)
}

func (f *sweepAllFixture) storeRows(rows ...string) {
	f.t.Helper()
	if err := os.MkdirAll(filepath.Dir(f.store), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(f.store, []byte(strings.Join(rows, "\n")+"\n"), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *sweepAllFixture) options() Options {
	return Options{
		StorePath: f.store,
		// Registry and store under a temp XDG root: a test never reads or writes
		// the developer's own.
		LookupEnv: func(key string) string {
			if key == "XDG_DATA_HOME" {
				return f.data
			}
			return ""
		},
		Gh: &ghtest.Fake{},
		Git: &gittest.Fake{Func: func(_ string, args []string) (string, error) {
			return f.origins[args[1]], nil
		}},
		SweepRepo: func(paths bus.Paths, workdir string, _ io.Writer) int {
			f.swept = append(f.swept, workdir)
			if want := filepath.Join(workdir, ".git"); paths.Common != want {
				f.t.Errorf("sweep of %s got the bus %s, want %s", workdir, paths.Common, want)
			}
			return f.rc[workdir]
		},
	}
}

// run drives a request through Run. The paths and cwd are the zero values main.go hands --sweep-all.
func (f *sweepAllFixture) run(o Options, req Request) (string, string, int) {
	f.t.Helper()
	var out, errb bytes.Buffer
	code := Run(req, bus.Paths{}, "", &out, &errb, o)
	return out.String(), errb.String(), code
}

func TestSweepAllLinesAndExitStatus(t *testing.T) {
	f := newSweepAllFixture(t)
	f.repo("alpha", "https://github.com/acme/alpha.git")
	f.repo("noremote", "")
	f.repo("broken", "git@github.com:acme/broken.git")
	f.registry(filepath.Join(f.root, "vanished", ".git"))
	f.rc[filepath.Join(f.root, "broken")] = 5
	// Two distinct run ids for acme/alpha, one of them stored twice, beside rows
	// of other repos the count must not see.
	f.storeRows(
		storeRow("acme/alpha", "r1"),
		storeRow("acme/alpha", "r1"),
		storeRow("acme/alpha", "r2"),
		storeRow("acme/broken", "r3"),
		storeRow("other/repo", "r4"),
	)

	out, errb, code := f.run(f.options(), Request{SweepAll: true, Roots: []string{f.root}})
	// A repo with an origin reports the slug its store rows are keyed by; a
	// skipped one reports the path discovery found, which is the only name it has.
	want := strings.Join([]string{
		"acme/alpha: swept, 2 runs in store",
		"acme/broken: failed (rc=5)",
		filepath.Join(f.root, "noremote") + ": skipped (no origin remote)",
		filepath.Join(f.root, "vanished", ".git") + ": skipped (gone)",
	}, "\n") + "\n"
	if code != 1 {
		t.Errorf("exit %d, want 1 (one repo failed)", code)
	}
	if out != want {
		t.Errorf("stdout =\n%s\nwant\n%s", out, want)
	}
	if errb != "" {
		t.Errorf("stderr = %q, want clean", errb)
	}
	if len(f.swept) != 2 {
		t.Errorf("swept %v, want the two repos with an origin", f.swept)
	}
}

func TestSweepAllExitsZeroWhenNoRepoFails(t *testing.T) {
	f := newSweepAllFixture(t)
	f.repo("alpha", "https://github.com/acme/alpha.git")
	f.repo("noremote", "")

	out, errb, code := f.run(f.options(), Request{SweepAll: true, Roots: []string{f.root}})
	want := strings.Join([]string{
		"acme/alpha: swept, 0 runs in store",
		filepath.Join(f.root, "noremote") + ": skipped (no origin remote)",
	}, "\n") + "\n"
	if code != 0 {
		t.Errorf("exit %d, want 0 (stderr %q)", code, errb)
	}
	if out != want {
		t.Errorf("stdout =\n%s\nwant\n%s", out, want)
	}
}

// TestSweepAllRoots covers the loop's roots: --root is repeatable, a root that
// is not a directory is a note and not a failure, and with no --root at all the
// walk falls back to CREW_SWEEP_ROOTS, then $HOME.
func TestSweepAllRoots(t *testing.T) {
	f := newSweepAllFixture(t)
	f.repo("alpha", "https://github.com/acme/alpha.git")
	typo := filepath.Join(t.TempDir(), "typo")

	out, errb, code := f.run(f.options(), Request{SweepAll: true, Roots: []string{f.root, typo}})
	if code != 0 || !strings.Contains(out, "alpha: swept") {
		t.Fatalf("exit %d stdout %q stderr %q", code, out, errb)
	}
	if !strings.Contains(errb, typo+" is not a directory") {
		t.Errorf("stderr = %q, want the note about the bad root", errb)
	}

	for _, tc := range []struct{ name, env, home string }{
		{name: "CREW_SWEEP_ROOTS", env: f.root},
		{name: "HOME", home: f.root},
	} {
		o := f.options()
		o.LookupEnv = func(key string) string {
			switch key {
			case "XDG_DATA_HOME":
				return t.TempDir()
			case "CREW_SWEEP_ROOTS":
				return tc.env
			case "HOME":
				return tc.home
			}
			return ""
		}
		out, errb, code := f.run(o, Request{SweepAll: true})
		if code != 0 || !strings.Contains(out, "alpha: swept") {
			t.Errorf("%s: exit %d stdout %q stderr %q", tc.name, code, out, errb)
		}
	}
}

// TestSweepAllNoReposFound is the empty-machine case: nothing to sweep is a note
// on stderr, not a failure.
func TestSweepAllNoReposFound(t *testing.T) {
	f := newSweepAllFixture(t)
	out, errb, code := f.run(f.options(), Request{SweepAll: true, Roots: []string{f.root}})
	if code != 0 || out != "" {
		t.Fatalf("exit %d stdout %q, want 0 and nothing", code, out)
	}
	if !strings.Contains(errb, "no repo with a crew bus found") {
		t.Errorf("stderr = %q, want the note", errb)
	}
}

// TestSweepAllRunsTheRepoSweep drives the loop with its real per-repo sweep —
// the call that replaced the child `crew rate`. A bus that cannot be folded
// fails with the sweep's own status; a bus with no events sweeps clean.
func TestSweepAllRunsTheRepoSweep(t *testing.T) {
	f := newSweepAllFixture(t)
	empty := f.repo("empty", "https://github.com/acme/empty.git")
	f.bus(filepath.Join(empty, "crew", "events.jsonl"), "")
	bad := f.repo("bad", "https://github.com/acme/bad.git")
	f.bus(filepath.Join(bad, "crew", "events.jsonl"), "not json\n")

	o := f.options()
	o.SweepRepo = nil
	o.Run = func(args ...string) (string, error) {
		if len(args) > 0 && args[0] == "dispatch-config" {
			return `{"burnClasses":[]}`, nil
		}
		return "", nil
	}
	out, errb, code := f.run(o, Request{SweepAll: true, Roots: []string{f.root}})
	want := strings.Join([]string{
		"acme/bad: failed (rc=5)",
		"acme/empty: swept, 0 runs in store",
	}, "\n") + "\n"
	if code != 1 {
		t.Errorf("exit %d, want 1 (stderr %q)", code, errb)
	}
	if out != want {
		t.Errorf("stdout =\n%s\nwant\n%s", out, want)
	}
	if !strings.Contains(errb, "events.jsonl") {
		t.Errorf("stderr = %q, want the sweep's own note about the bus", errb)
	}
}

func TestRunsInStore(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name    string
		content *string
		want    int
	}{
		{name: "missing store", content: nil, want: 0},
		{name: "empty store", content: ptr(""), want: 0},
		{name: "one run stored twice", content: ptr(storeRow("acme/alpha", "r1") + "\n" + storeRow("acme/alpha", "r1")), want: 1},
		{name: "two runs", content: ptr(storeRow("acme/alpha", "r1") + "\n" + storeRow("acme/alpha", "r2")), want: 2},
		{name: "another repo only", content: ptr(storeRow("acme/beta", "r1")), want: 0},
		{name: "a torn line is jq's error", content: ptr(storeRow("acme/alpha", "r1") + "\nnot json"), want: 0},
		{name: "a row .repo cannot index is jq's error", content: ptr(storeRow("acme/alpha", "r1") + "\n5"), want: 0},
		{name: "a null row is not", content: ptr(storeRow("acme/alpha", "r1") + "\nnull"), want: 1},
		{name: "a row with no run_id counts once", content: ptr(`{"repo":"acme/alpha","swept_at":1}`), want: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := filepath.Join(dir, strings.ReplaceAll(tc.name, " ", "-")+".jsonl")
			if tc.content != nil {
				if err := os.WriteFile(store, []byte(*tc.content+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if got := runsInStore(store, "acme/alpha"); got != tc.want {
				t.Errorf("runsInStore = %d, want %d", got, tc.want)
			}
		})
	}
}
