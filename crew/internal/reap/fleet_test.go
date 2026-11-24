package reap

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
)

// fleetFixture lays a machine of repo candidates out the way --fleet finds
// them: every common dir in the registry, and an empty sweep root so the walk
// adds nothing. The reap behind the seam is scripted per repo, so what a test
// reads is the loop's own lines and exit status rather than tmux, gh or a lock.
type fleetFixture struct {
	t    *testing.T
	root string
	data string // XDG_DATA_HOME
	home string
	rc   map[string]int
	seen []string
}

func newFleetFixture(t *testing.T) *fleetFixture {
	t.Helper()
	f := &fleetFixture{
		t:    t,
		root: t.TempDir(),
		data: t.TempDir(),
		home: t.TempDir(),
		rc:   map[string]int{},
	}
	t.Setenv("XDG_DATA_HOME", f.data)
	t.Setenv("HOME", f.home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(f.home, ".local", "state"))
	// An empty sweep root: only the registry is read, so the fixture's repos are
	// exactly the ones it registered.
	t.Setenv("CREW_SWEEP_ROOTS", t.TempDir())
	return f
}

// repo creates <root>/<name>/.git, with a bus only when withBus is set, and
// registers its common dir. It returns the common dir, which is what the loop
// prints for a skipped or failed repo.
func (f *fleetFixture) repo(name string, withBus bool) string {
	f.t.Helper()
	common := filepath.Join(f.root, name, ".git")
	if err := os.MkdirAll(common, 0o755); err != nil {
		f.t.Fatal(err)
	}
	if withBus {
		dir := filepath.Join(common, "crew")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			f.t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), []byte("{}\n"), 0o644); err != nil {
			f.t.Fatal(err)
		}
	}
	f.registry(common)
	return common
}

// registry appends one common dir to the registry dispatch writes.
func (f *fleetFixture) registry(common string) {
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

// options scripts the per-repo reap: it records the bus it was handed and
// returns the code the fixture set for that repo's checkout.
func (f *fleetFixture) options() Options {
	return Options{
		FleetReap: func(_ context.Context, _ Request, paths bus.Paths, cwd string, _, _ io.Writer) int {
			f.seen = append(f.seen, paths.Common)
			if want := filepath.Join(cwd, ".git"); paths.Common != want {
				f.t.Errorf("reap of %s got the bus %s, want %s", cwd, paths.Common, want)
			}
			return f.rc[cwd]
		},
	}
}

// run drives a request through RunFleet. The streams are the caller's own.
func (f *fleetFixture) run(o Options) (string, string, int) {
	f.t.Helper()
	return f.runReq(Request{}, o)
}

// runReq is run with the request the caller wants (e.g. Quiet) set.
func (f *fleetFixture) runReq(req Request, o Options) (string, string, int) {
	f.t.Helper()
	var out, errb bytes.Buffer
	code := RunFleet(context.Background(), req, &out, &errb, o, FleetOptions{Getenv: os.Getenv})
	return out.String(), errb.String(), code
}

func TestFleetReapsEveryRepoAndReportsFailures(t *testing.T) {
	f := newFleetFixture(t)
	repoA := f.repo("alpha", true)
	repoB := f.repo("bravo", true)
	repoC := f.repo("charlie", false) // no crew/events.jsonl => no bus
	f.rc[filepath.Dir(repoA)] = 7
	f.rc[filepath.Dir(repoB)] = 0

	out, errb, code := f.run(f.options())
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	// Discovery byte-sorts, so alpha then bravo; charlie has no bus and is never
	// handed to the reap.
	want := []string{repoA, repoB}
	if strings.Join(f.seen, "\n") != strings.Join(want, "\n") {
		t.Errorf("visited = %q, want %q", f.seen, want)
	}
	if line := "crew reap --fleet: " + repoC + ": skipped (no bus)"; !strings.Contains(out, line) {
		t.Errorf("stdout = %q, want a line %q", out, line)
	}
	if line := "crew reap --fleet: " + repoA + ": failed (rc=7)"; !strings.Contains(errb, line) {
		t.Errorf("stderr = %q, want a line %q", errb, line)
	}
}

func TestFleetAllGreenExitsZero(t *testing.T) {
	f := newFleetFixture(t)
	repoA := f.repo("alpha", true)
	repoB := f.repo("bravo", true)
	f.rc[filepath.Dir(repoA)] = 0
	f.rc[filepath.Dir(repoB)] = 0

	out, errb, code := f.run(f.options())
	if code != 0 {
		t.Errorf("exit = %d, want 0", code)
	}
	if len(f.seen) != 2 {
		t.Errorf("visited = %q, want both repos", f.seen)
	}
	if strings.Contains(errb, "failed") {
		t.Errorf("stderr = %q, want no failed lines", errb)
	}
	if strings.Contains(out, "failed") {
		t.Errorf("stdout = %q, want no failed lines", out)
	}
}

// TestFleetAbsolutizesRelativeCandidates covers a registry line (or sweep
// root) that is relative: the loop must hand reap an absolute checkout, or
// wtgit.TrustedCwd never terminates on the ancestor walk.
func TestFleetAbsolutizesRelativeCandidates(t *testing.T) {
	f := newFleetFixture(t)
	repoA := f.repo("alpha", true)

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(cwd, repoA)
	if err != nil {
		t.Fatal(err)
	}
	// Overwrite the registered absolute line with a relative one.
	if err := os.WriteFile(filepath.Join(f.data, "crew", "repos"), []byte(rel+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	f.rc[filepath.Dir(repoA)] = 0
	_, _, code := f.run(f.options())
	if code != 0 {
		t.Errorf("exit = %d, want 0", code)
	}
	if len(f.seen) != 1 {
		t.Fatalf("visited = %q, want exactly the one repo", f.seen)
	}
	got := f.seen[0]
	if !filepath.IsAbs(got) {
		t.Errorf("visited common = %q, want an absolute path", got)
	}
	if filepath.Clean(got) != filepath.Clean(repoA) {
		t.Errorf("visited common = %q, want %q", got, repoA)
	}
}

// TestFleetQuietSuppressesSkips: the timer always passes --quiet, so a stale
// registry entry must not write a skip line to the journal every 10 minutes.
func TestFleetQuietSuppressesSkips(t *testing.T) {
	f := newFleetFixture(t)
	_ = f.repo("alpha", false) // no crew/events.jsonl => no bus

	out, _, code := f.runReq(Request{Quiet: true}, f.options())
	if code != 0 {
		t.Errorf("exit = %d, want 0", code)
	}
	if strings.Contains(out, "skipped") {
		t.Errorf("stdout = %q, want no skipped lines under --quiet", out)
	}
}

// index lists common in the fleet fixture's state index, the way dispatch's
// IndexAdd does for a layout-2 crew; it returns the state root.
func (f *fleetFixture) index(common string) string {
	f.t.Helper()
	state := filepath.Join(f.home, ".local", "state", "crew")
	if _, err := bus.IndexAdd(state, common, "o/r", "c2", time.Now()); err != nil {
		f.t.Fatal(err)
	}
	return state
}

// stateSeen records the state root each stubbed reap was handed.
func (f *fleetFixture) stateOptions(states *[]string) Options {
	o := f.options()
	inner := o.FleetReap
	o.FleetReap = func(ctx context.Context, req Request, paths bus.Paths, cwd string, stdout, stderr io.Writer) int {
		*states = append(*states, paths.State)
		return inner(ctx, req, paths, cwd, stdout, stderr)
	}
	return o
}

// A repo with no repo log that the index lists is reaped, with the state root
// set, instead of skipped as `no bus`.
func TestFleetIndexListedNoBusRepoIsReaped(t *testing.T) {
	f := newFleetFixture(t)
	repoA := f.repo("alpha", false)
	state := f.index(repoA)
	var states []string
	out, errb, code := f.run(f.stateOptions(&states))
	if code != 0 {
		t.Fatalf("exit = %d, stderr %q", code, errb)
	}
	if strings.Join(f.seen, "\n") != repoA || strings.Join(states, "\n") != state {
		t.Fatalf("visited %q with states %q, want %s with %s", f.seen, states, repoA, state)
	}
	if strings.Contains(out, "skipped") {
		t.Fatalf("stdout %q, want no skip", out)
	}
}

// A repo both Discover and the index find is reaped once, Discover repos keep
// their order, and index-only repos follow sorted.
func TestFleetDedupesAndOrders(t *testing.T) {
	f := newFleetFixture(t)
	repoB := f.repo("bravo", true)
	repoA := f.repo("alpha", true)
	f.index(repoB)
	only := []string{filepath.Join(f.root, "zulu", ".git"), filepath.Join(f.root, "yankee", ".git")}
	for _, c := range only {
		if err := os.MkdirAll(c, 0o755); err != nil {
			t.Fatal(err)
		}
		f.index(c)
	}
	_, errb, code := f.run(f.options())
	if code != 0 {
		t.Fatalf("exit = %d, stderr %q", code, errb)
	}
	want := []string{repoA, repoB, only[1], only[0]}
	if strings.Join(f.seen, "\n") != strings.Join(want, "\n") {
		t.Fatalf("visited %q, want %q", f.seen, want)
	}
}

// An index header that is not a repo's own .git dir is skipped with why; the
// run goes on.
func TestFleetIndexOnlyHeaderValidated(t *testing.T) {
	f := newFleetFixture(t)
	repoA := f.repo("alpha", true)
	missing := filepath.Join(f.root, "gone", ".git")
	notGit := filepath.Join(f.root, "plain")
	real := filepath.Join(f.root, "real", ".git")
	link := filepath.Join(f.root, "link", ".git")
	for _, d := range []string{notGit, real, filepath.Dir(link)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{missing, notGit, link} {
		f.index(c)
	}
	out, errb, code := f.run(f.options())
	if code != 0 {
		t.Fatalf("exit = %d, stderr %q", code, errb)
	}
	if strings.Join(f.seen, "\n") != repoA {
		t.Fatalf("visited %q, want only %s", f.seen, repoA)
	}
	for _, line := range []string{
		"crew reap --fleet: " + missing + ": skipped (not a directory)",
		"crew reap --fleet: " + notGit + ": skipped (not a .git dir)",
		"crew reap --fleet: " + link + ": skipped (not its own realpath)",
	} {
		if !strings.Contains(out, line) {
			t.Errorf("stdout %q, want a line %q", out, line)
		}
	}
	out, _, _ = f.runReq(Request{Quiet: true}, f.options())
	if strings.Contains(out, "skipped") {
		t.Errorf("stdout = %q, want no skipped lines under --quiet", out)
	}
}

// A nil Getenv reads the process environment: the index-only repo is still
// seen by RunFleet and FleetBuses.
func TestFleetNilGetenvSeesIndexOnlyRepo(t *testing.T) {
	f := newFleetFixture(t)
	only := filepath.Join(f.root, "solo", ".git")
	if err := os.MkdirAll(only, 0o755); err != nil {
		t.Fatal(err)
	}
	xdg := t.TempDir()
	t.Setenv("XDG_STATE_HOME", xdg)
	state := filepath.Join(xdg, "crew")
	if _, err := bus.IndexAdd(state, only, "o/r", "c2", time.Now()); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	var states []string
	if code := RunFleet(context.Background(), Request{}, &out, &errb, f.stateOptions(&states), FleetOptions{}); code != 0 {
		t.Fatalf("exit = %d, stderr %q", code, errb.String())
	}
	if strings.Join(f.seen, "\n") != only || strings.Join(states, "\n") != state {
		t.Fatalf("visited %q with states %q, want %s with %s", f.seen, states, only, state)
	}
	buses := FleetBuses(io.Discard, FleetOptions{})
	if len(buses) != 1 || buses[0].Common != only || buses[0].State != state {
		t.Fatalf("FleetBuses = %+v", buses)
	}
}
