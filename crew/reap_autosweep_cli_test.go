package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestMain lets reap's detached ratings sweep run: the async path execs
// os.Executable() with `reap-autosweep`, which under `go test` is this binary.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "reap-autosweep" {
		os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr, env{getwd: os.Getwd}))
	}
	os.Exit(m.Run())
}

const (
	widgetsPR  = "https://github.com/acme/widgets/pull/"
	asyncLimit = 10 * time.Second
)

// autosweepCLI is a host repo whose origin is acme/widgets (the reconcile gate
// spends gh credentials on the dispatching repo only, so every pr_url fixture
// targets it) with a one-run bus, under the sweepAllCLI machine.
type autosweepCLI struct {
	*sweepAllCLI
	repoDir string
}

func newAutosweepCLI(t *testing.T) *autosweepCLI {
	t.Helper()
	c := newSweepAllCLI(t)
	unsetenv(t, "TMUX", "TMUX_PANE", "PR_OPEN_BRANCH", "TMUX_WINDOWS", "CREW_RATE_AUTOSWEEP")
	a := &autosweepCLI{sweepAllCLI: c}
	a.repoDir = c.repo(filepath.Join(filepath.Dir(c.root), "widgets"), "acme/widgets")
	return a
}

func (a *autosweepCLI) reap(args ...string) (string, string, int) {
	a.t.Helper()
	return a.runIn(a.repoDir, append([]string{"reap"}, args...)...)
}

// reapOK is reap with the given CREW_RATE_AUTOSWEEP mode; the exit must be 0.
// It returns stdout and stderr together, as a user sees them.
func (a *autosweepCLI) reapOK(mode string, args ...string) string {
	a.t.Helper()
	a.t.Setenv("CREW_RATE_AUTOSWEEP", mode)
	out, errb, code := a.reap(args...)
	if code != 0 {
		a.t.Fatalf("reap %v (%s): code %d\nstdout %q\nstderr %q", args, mode, code, out, errb)
	}
	return out + errb
}

func (a *autosweepCLI) dir() string          { return filepath.Join(a.data, "crew") }
func (a *autosweepCLI) lockDir() string      { return filepath.Join(a.dir(), "ratings.sweep.lock.d") }
func (a *autosweepCLI) autosweepLog() string { return filepath.Join(a.dir(), "autosweep.log") }

func (a *autosweepCLI) appendBus(rows ...string) {
	a.t.Helper()
	f, err := os.OpenFile(filepath.Join(a.repoDir, ".git", "crew", "events.jsonl"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		a.t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(strings.Join(rows, "\n") + "\n"); err != nil {
		a.t.Fatal(err)
	}
}

// dispatchWithPR seeds one more run, the way tests/rate fixtures did.
func (a *autosweepCLI) dispatchWithPR(branch string, ts int, n int) {
	a.t.Helper()
	a.appendBus(
		fmt.Sprintf(`{"ts":%d,"crew_id":"c1","kind":"dispatch","branch":%q,"engine":"claude","model":"sonnet","tier":"standard","effort":"medium","title":"t"}`, ts, branch),
		fmt.Sprintf(`{"ts":%d,"crew_id":"c1","from":"worker:%s","to":"dispatcher:c1","kind":"status","body":{"state":"pr_open","pr_url":"%s%d"}}`, ts+500, branch, widgetsPR, n),
	)
}

func (a *autosweepCLI) storeLines() int {
	a.t.Helper()
	data, err := os.ReadFile(a.storePath())
	if err != nil {
		a.t.Fatal(err)
	}
	return strings.Count(string(data), "\n")
}

func (a *autosweepCLI) requireNoStore() {
	a.t.Helper()
	if _, err := os.Stat(a.storePath()); !os.IsNotExist(err) {
		a.t.Errorf("ratings store exists (stat err %v), want none", err)
	}
}

// holdLock plants a sweep lock owned by a live foreign process.
func (a *autosweepCLI) holdLock() int {
	a.t.Helper()
	pid := livePid(a.t)
	if err := os.MkdirAll(a.lockDir(), 0o755); err != nil {
		a.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.lockDir(), "pid"), []byte(strconv.Itoa(pid)+"\n"), 0o644); err != nil {
		a.t.Fatal(err)
	}
	return pid
}

// awaitLog waits until the async sweep log has want matches of re. The log
// does not depend on the store, so it also works for a sweep that fails or is
// skipped before writing one; and "done rc=" is printed after the lock is
// released, so a match means the child has finished its sweep.
func (a *autosweepCLI) awaitLog(re string, want int) {
	a.t.Helper()
	rx := regexp.MustCompile(re)
	deadline := time.Now().Add(asyncLimit)
	for {
		data, _ := os.ReadFile(a.autosweepLog())
		if len(rx.FindAll(data, -1)) >= want {
			return
		}
		if time.Now().After(deadline) {
			a.t.Fatalf("autosweep.log never had %d match(es) of %q:\n%s", want, re, data)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestAutosweepSyncReapFillsTheStoreOneRowPerRun(t *testing.T) {
	a := newAutosweepCLI(t)
	a.reapOK("sync")
	if n := a.storeLines(); n != 1 {
		t.Errorf("store has %d rows, want 1", n)
	}
}

func TestAutosweepSecondReapLeavesTheStoreByteIdentical(t *testing.T) {
	a := newAutosweepCLI(t)
	a.reapOK("sync")
	before, err := os.ReadFile(a.storePath())
	if err != nil {
		t.Fatal(err)
	}
	a.reapOK("sync")
	after, err := os.ReadFile(a.storePath())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) || bytes.Count(after, []byte("\n")) != 1 {
		t.Errorf("store changed or grew:\nbefore %q\nafter  %q", before, after)
	}
}

func TestAutosweepDryRunRunsNoSweepAndSaysSo(t *testing.T) {
	a := newAutosweepCLI(t)
	out := a.reapOK("sync", "--dry-run")
	a.requireNoStore()
	wantContains(t, "output", out, "dry run")
}

func TestAutosweepDisabledRunsNoSweep(t *testing.T) {
	a := newAutosweepCLI(t)
	a.reapOK("0")
	a.requireNoStore()
}

func TestAutosweepFailingSweepDoesNotChangeReapsExitStatus(t *testing.T) {
	a := newAutosweepCLI(t)
	// The store path is a directory, so the sweep cannot append its row.
	if err := os.MkdirAll(a.storePath(), 0o755); err != nil {
		t.Fatal(err)
	}
	a.reapOK("sync")
	if info, err := os.Stat(a.storePath()); err != nil || !info.IsDir() {
		t.Errorf("store path is no longer the directory we made it (err %v)", err)
	}
	// Holds whether the sweep failed or never ran, so pin that it ran: reaching
	// the gh stub means it got to the reconcile before the append failed.
	if info, err := os.Stat(a.ghLog); err != nil || info.Size() == 0 {
		t.Errorf("gh stub never called (stat err %v): the sweep did not run", err)
	}
}

func TestAutosweepHookFiresWhenTheStoreDirDoesNotExist(t *testing.T) {
	a := newAutosweepCLI(t)
	if _, err := os.Stat(a.dir()); !os.IsNotExist(err) {
		t.Fatalf("store dir exists before the reap (stat err %v)", err)
	}
	a.reapOK("sync")
	if n := a.storeLines(); n != 1 {
		t.Errorf("store has %d rows, want 1", n)
	}
}

// pr_open is not one of reap's terminal states (done/failed/exited), so there
// are no candidates and reap takes its "nothing reclaimed" exit, which sits
// below the hook.
func TestAutosweepNoReapCandidatesAndTheSweepStillRuns(t *testing.T) {
	a := newAutosweepCLI(t)
	out := a.reapOK("sync")
	wantContains(t, "output", out, "nothing reclaimed")
	if n := a.storeLines(); n != 1 {
		t.Errorf("store has %d rows, want 1", n)
	}
}

func TestAutosweepFirstSweepIngestsEveryHistoricalDispatch(t *testing.T) {
	a := newAutosweepCLI(t)
	// The fixture repo already carries feat/x (pull/1); add four more.
	for i := 2; i <= 5; i++ {
		a.dispatchWithPR(fmt.Sprintf("feat/h%d", i), i*1000, i)
	}
	a.reapOK("sync")
	if n := a.storeLines(); n != 5 {
		t.Errorf("store has %d rows, want 5", n)
	}
}

func TestAutosweepTwoOverlappingSweepsCollapseToOne(t *testing.T) {
	a := newAutosweepCLI(t)
	pid := a.holdLock()
	out := a.reapOK("sync")
	got, err := os.ReadFile(filepath.Join(a.lockDir(), "pid"))
	if err != nil || strings.TrimSpace(string(got)) != strconv.Itoa(pid) {
		t.Errorf("lock pid = %q (err %v), want the holder's %d", got, err, pid)
	}
	a.requireNoStore()
	// Without this the test also passes when the hook never ran at all.
	wantContains(t, "output", out, "a ratings sweep is already running — skipped")
}

func TestAutosweepAsyncModeLeavesStdoutAndExitStatusUnchanged(t *testing.T) {
	a := newAutosweepCLI(t)
	disabled := a.reapOK("0")
	async := a.reapOK("1")
	if disabled != async {
		t.Errorf("async output differs:\ndisabled %q\nasync    %q", disabled, async)
	}
	a.awaitLog(`done rc=`, 1)
}

func TestAutosweepDefaultIsOnWhenTheVariableIsUnset(t *testing.T) {
	a := newAutosweepCLI(t)
	out, errb, code := a.reap("--dry-run")
	if code != 0 {
		t.Fatalf("code %d, stderr %q", code, errb)
	}
	wantContains(t, "output", out+errb, "dry run — skipping ratings sweep")
}

func TestAutosweepFlagAndTheForkCompose(t *testing.T) {
	a := newAutosweepCLI(t)
	a.reapOK("1")
	a.awaitLog(`done rc=0`, 1)
	if n := a.storeLines(); n != 1 {
		t.Errorf("store has %d rows, want 1", n)
	}
	if _, err := os.Stat(a.lockDir()); !os.IsNotExist(err) {
		t.Errorf("sweep lock still held (stat err %v)", err)
	}
}

// Call-site guard, like the bus-append guards in tests/adapters.bats. No
// behavioural test can see this: a loop of per-row appends fails identically to
// the single-write form, so reverting it would leave the suite green. Hence a
// grep on the call site's shape.
func TestGuardRatingsStoreAppendIsOneAtomicOAppendWrite(t *testing.T) {
	// reap changes the process's cwd, so locate the source from this file.
	_, self, _, _ := runtime.Caller(0)
	src, err := os.ReadFile(filepath.Join(filepath.Dir(self), "internal", "rate", "sweep.go"))
	if err != nil {
		t.Fatal(err)
	}
	open := regexp.MustCompile(`os\.OpenFile\(store, os\.O_APPEND\|os\.O_CREATE\|os\.O_WRONLY`)
	if n := len(open.FindAll(src, -1)); n != 1 {
		t.Errorf("store append sites = %d, want exactly 1", n)
	}
	// one Write of the whole batch, not a per-row loop
	write := regexp.MustCompile(`\bf\.Write\(batch\)`)
	if n := len(write.FindAll(src, -1)); n != 1 {
		t.Errorf("f.Write(batch) occurrences = %d, want exactly 1", n)
	}
}

func TestAutosweepUnrecognisedValueWarnsOnStderrAndStillSweeps(t *testing.T) {
	a := newAutosweepCLI(t)
	// `false` is the trap: the natural boolean spelling for "off".
	a.t.Setenv("CREW_RATE_AUTOSWEEP", "false")
	out, errb, code := a.reap("--dry-run")
	if code != 0 {
		t.Fatalf("code %d, stderr %q", code, errb)
	}
	wantContains(t, "stderr", errb, "CREW_RATE_AUTOSWEEP='false' is not 0, 1 or sync")
	// Took the default rather than disabling; --dry-run keeps this fork-free.
	wantContains(t, "output", out+errb, "dry run — skipping ratings sweep")
}

func TestAutosweepTheThreeDocumentedValuesProduceNoWarning(t *testing.T) {
	a := newAutosweepCLI(t)
	for _, v := range []string{"0", "1", "sync", ""} {
		if v == "" {
			unsetenv(t, "CREW_RATE_AUTOSWEEP")
		} else {
			t.Setenv("CREW_RATE_AUTOSWEEP", v)
		}
		_, errb, code := a.reap("--dry-run")
		if code != 0 || errb != "" {
			t.Errorf("CREW_RATE_AUTOSWEEP=%q: code %d, stderr %q, want a silent success", v, code, errb)
		}
	}
}

func TestAutosweepTwoAsyncSweepsEachComplete(t *testing.T) {
	a := newAutosweepCLI(t)
	a.reapOK("1")
	a.awaitLog(`done rc=`, 1)
	if n := a.storeLines(); n != 1 {
		t.Fatalf("store has %d rows after the first sweep, want 1", n)
	}
	// The second wait is the point: the store now exists, so waiting on it alone
	// would return having waited for nothing.
	a.reapOK("1")
	a.awaitLog(`done rc=`, 2)
	// Still one row: the sweep is idempotent, so this pass appends nothing.
	if n := a.storeLines(); n != 1 {
		t.Errorf("store has %d rows after the second sweep, want 1", n)
	}
	if _, err := os.Stat(a.lockDir()); !os.IsNotExist(err) {
		t.Errorf("sweep lock still held (stat err %v)", err)
	}
}

func TestAutosweepFailingAsyncSweepIsLoggedWithItsExitCode(t *testing.T) {
	a := newAutosweepCLI(t)
	if err := os.MkdirAll(a.storePath(), 0o755); err != nil {
		t.Fatal(err)
	}
	a.reapOK("1")
	a.awaitLog(`done rc=`, 1)
	a.awaitLog(`start pid=[0-9]+ repo=`, 1)
	data, err := os.ReadFile(a.autosweepLog())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "done rc=0") {
		t.Errorf("a failing sweep logged rc=0:\n%s", data)
	}
}

func TestAutosweepAsyncLockSkipIsLoggedEvenUnderQuiet(t *testing.T) {
	a := newAutosweepCLI(t)
	a.holdLock()
	a.reapOK("1", "--quiet")
	a.awaitLog(`already running`, 1)
	a.requireNoStore()
}

func TestAutosweepSyncLockSkipStaysSilentUnderQuiet(t *testing.T) {
	a := newAutosweepCLI(t)
	a.holdLock()
	out := a.reapOK("sync", "--quiet")
	if strings.Contains(out, "already running") {
		t.Errorf("output = %q, want the skip silent", out)
	}
	if _, err := os.Stat(a.autosweepLog()); !os.IsNotExist(err) {
		t.Errorf("autosweep.log exists (stat err %v), want none for a sync sweep", err)
	}
}

func TestAutosweepAsyncLogIsTrimmedToItsLast200Lines(t *testing.T) {
	a := newAutosweepCLI(t)
	if err := os.MkdirAll(a.dir(), 0o755); err != nil {
		t.Fatal(err)
	}
	var seed strings.Builder
	for i := 1; i <= 500; i++ {
		fmt.Fprintln(&seed, i)
	}
	if err := os.WriteFile(a.autosweepLog(), []byte(seed.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	a.reapOK("1")
	a.awaitLog(`done rc=0`, 1)
	// The trim is the child's last write; give it a beat to land.
	deadline := time.Now().Add(asyncLimit)
	var lines []string
	for {
		data, _ := os.ReadFile(a.autosweepLog())
		lines = strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
		if len(lines) <= 200 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(lines) != 200 {
		t.Fatalf("log has %d lines, want 200", len(lines))
	}
	if !strings.Contains(strings.Join(lines, "\n"), "done rc=0") {
		t.Error("the trimmed log lost the done line")
	}
	if lines[0] == "1" || strings.Contains("\n"+strings.Join(lines, "\n")+"\n", "\n1\n") {
		t.Error("the oldest seed line survived the trim")
	}
}

// The lock holder's own handle on the log is opened O_APPEND before the skipped
// sweep trims and writes its `done` line after: rewriting the file in place
// keeps that line, replacing the inode would strand it.
func TestAutosweepTrimmingTheLogKeepsAConcurrentSweepsLaterLines(t *testing.T) {
	a := newAutosweepCLI(t)
	a.holdLock()
	holder, err := os.OpenFile(a.autosweepLog(), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Close() }()
	a.reapOK("1")
	a.awaitLog(`already running — skipped`, 1)
	// The skip trims the log as its last write; give it a beat to land before
	// the holder writes.
	time.Sleep(time.Second)
	if _, err := holder.WriteString("HOLDER-LATE-LINE\n"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(a.autosweepLog())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "HOLDER-LATE-LINE") {
		t.Errorf("the holder's late line is gone from the log:\n%s", data)
	}
}
