// The #1294 regression: a TERM landing after `watch` had published its lock
// record but before its handlers were armed took Go's default action, the
// deferred release never ran, and watch.lock.d outlived its holder — refusing
// every later park of the crew. The seam parks the child exactly there, so the
// signal lands in the window for sure instead of under load's luck.
package watch

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
)

const (
	signalHoldEnv = "CREW_WATCH_TEST_HOLD"
	signalBusEnv  = "CREW_WATCH_TEST_BUS"
)

// TestHelperSignalWindow is the child: the real Run over the bus the test
// names, held by the seam at the gate dir between the acquire and the loop.
func TestHelperSignalWindow(t *testing.T) {
	dir := os.Getenv(signalHoldEnv)
	if dir == "" {
		t.Skip("helper process for the watch signal-window test")
	}
	afterLockAcquire = func() {
		_ = os.WriteFile(filepath.Join(dir, "held"), nil, 0o644)
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(filepath.Join(dir, "go")); err == nil {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	common := os.Getenv(signalBusEnv)
	if common == "" {
		t.Fatal("the helper was named no bus")
	}
	paths := bus.Paths{Common: common, Dir: common + "/crew", Log: common + "/crew/events.jsonl"}
	os.Exit(Run(Request{Crew: "c1", Timeout: "30", Interval: "1"}, paths, os.Stdout, os.Stderr, Options{}))
}

// TestWatchTermInLockWindowReleasesTheLock signals a real watch child while it
// holds watch.lock.d with the loop not yet reached, and requires the lock to be
// gone when it exits: a holder's release must be armed before the lock exists.
func TestWatchTermInLockWindowReleasesTheLock(t *testing.T) {
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		dir := t.TempDir()
		busk := t.TempDir()
		paths := bus.Paths{Common: busk, Dir: busk + "/crew", Log: busk + "/crew/events.jsonl"}

		cmd := exec.Command(bin, "-test.run=^TestHelperSignalWindow$", "-test.count=1")
		cmd.Env = append(os.Environ(), signalHoldEnv+"="+dir, signalBusEnv+"="+busk)
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		waitForFile(t, filepath.Join(dir, "held"))
		if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
		// Open the gate: a fixed child folds the signal into its exit path and
		// releases on the way out; a leaking one is already dead.
		if err := os.WriteFile(filepath.Join(dir, "go"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := cmd.Wait(); err != nil {
			if rc := childStatus(err); rc != 128+int(syscall.SIGTERM) {
				t.Errorf("iteration %d: exit = %d, want 143:\n%s", i, rc, out.String())
			}
		} else {
			t.Errorf("iteration %d: the watch exited clean on TERM, want 143:\n%s", i, out.String())
		}
		if _, err := os.Stat(filepath.Join(paths.CrewDir("c1"), "watch.lock.d")); err == nil {
			t.Errorf("iteration %d: watch.lock.d survived a TERM landed in the lock window:\n%s", i, out.String())
		}
	}
}

// waitForFile is the gate's poll: the marker proves the child is inside the
// seam, which is after the record was published and before the loop began.
func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no %s after 15s — the child never reached the lock window", filepath.Base(path))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

const watchParkBusEnv = "CREW_WATCH_TEST_PARK_BUS"

// TestHelperWatchPark is the child the park test signals: the real Run over the
// bus it names, parked on its real sleep.
func TestHelperWatchPark(t *testing.T) {
	common := os.Getenv(watchParkBusEnv)
	if common == "" {
		t.Skip("helper process for the watch park signal test")
	}
	paths := bus.Paths{Common: common, Dir: common + "/crew", Log: common + "/crew/events.jsonl"}
	os.Exit(Run(Request{Crew: "c1", Timeout: "60", Interval: "1"}, paths, os.Stdout, os.Stderr, Options{}))
}

// A TERM to a parked watch exits 143 and leaves the lock and the cursor alone,
// and the watch after it starts at once: a leaked watch.lock.d refuses every
// later park of the crew.
func TestWatchTermMidParkReleasesTheLock(t *testing.T) {
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	p := bus.Paths{Common: dir, Dir: dir + "/crew", Log: dir + "/crew/events.jsonl"}
	lockd := filepath.Join(p.CrewDir("c1"), "watch.lock.d")

	cmd := exec.Command(bin, "-test.run=^TestHelperWatchPark$", "-test.count=1")
	cmd.Env = append(os.Environ(), watchParkBusEnv+"="+dir)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitForFile(t, filepath.Join(lockd, "pid"))
	pid, err := os.ReadFile(filepath.Join(lockd, "pid"))
	if err != nil || strings.TrimSpace(string(pid)) != strconv.Itoa(cmd.Process.Pid) {
		t.Fatalf("watch.lock.d names %q (%v), want the parked watch's pid %d", pid, err, cmd.Process.Pid)
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); childStatus(err) != 128+int(syscall.SIGTERM) {
		t.Fatalf("exit = %v, want 143:\n%s", err, out.String())
	}
	if _, err := os.Stat(lockd); !os.IsNotExist(err) {
		t.Errorf("watch.lock.d survived a TERM mid-park (%v)", err)
	}
	if _, err := os.Stat(filepath.Join(p.CrewDir("c1"), "cursor")); !os.IsNotExist(err) {
		t.Errorf("the cursor moved without a batch (%v)", err)
	}

	row := status(1785951264000, "worker:feat/x", "done", "", "")
	writeLog(t, p, row)
	stdout, stderr, code, _ := run(t, p, Request{Since: since("0"), Timeout: "1", Interval: "1"})
	if code != 0 || stderr != "" || stdout != want(t, 1785951264000, row) {
		t.Errorf("the next watch = (%d, %q, %q), want its batch at once", code, stderr, stdout)
	}
}
