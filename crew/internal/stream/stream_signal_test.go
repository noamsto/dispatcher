// The ported `stream:` lock and signal cases: who may hold the lane's lock,
// what a caught signal leaves behind, and what the lane never writes to its own
// stderr. The signals go to this process, which is where `Run` catches them.
package stream

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/lock"
)

// An empty stdout, exit 1 and the incumbent's pid: a second stream for one crew
// is refused by the tool rather than maintained by hand, and the refusal leaves
// the incumbent's lock and process exactly as they were.
func TestStreamSecondStreamForOneCrewIsRefused(t *testing.T) {
	p := streamBus(t)
	lockd := filepath.Join(p.CrewDir("c1"), "stream.lock.d")
	if err := os.MkdirAll(filepath.Dir(lockd), 0o755); err != nil {
		t.Fatal(err)
	}
	holder := startForceChild(t, "hold", lockd, t.TempDir())
	if got := holder.next(t); got != "held" {
		t.Fatalf("the holder said %q", got)
	}

	var out, errb bytes.Buffer
	rc := Run(Request{Crew: "c1", Park: "1", Interval: "1"}, p, &out, &errb,
		Options{Self: entryStub(t, nil).Script})

	if rc != 1 {
		t.Errorf("exit = %d, want 1 (a refused start is not a usage error)", rc)
	}
	want := fmt.Sprintf("another stream is already running for this crew (c1) — pid %d", holder.pid())
	if got := errb.String(); !strings.Contains(got, want) || !strings.Contains(got, "--force") {
		t.Errorf("stderr = %q, want it to name %q and offer --force", got, want)
	}
	if out.Len() != 0 {
		t.Errorf("stdout = %q, want nothing", out.String())
	}
	if !holder.running() {
		t.Error("the refusal signalled the incumbent")
	}
	if got := lock.Holder(lockd); got != strconv.Itoa(holder.pid()) {
		t.Errorf("the lock reads %q after the refusal, want the incumbent untouched", got)
	}
}

// 0 as a signal target hits this whole process group, and internal/lock's
// old-format liveness check reads it as live — so --force refuses before it ever
// sends a TERM, and leaves the lock it refused to reclaim.
func TestStreamForceRefusesAZeroPidHolder(t *testing.T) {
	p := streamBus(t)
	lockd := writeLock(t, p, "0\n")

	var out, errb bytes.Buffer
	rc := Run(Request{Crew: "c1", Park: "1", Interval: "1", Force: true}, p, &out, &errb,
		Options{Self: entryStub(t, nil).Script})

	if rc != 1 {
		t.Errorf("exit = %d, want 1", rc)
	}
	if got, want := errb.String(),
		"crew: --force found no valid holder pid for crew (c1) stream lock (got '0') — refusing to signal\n"; got != want {
		t.Errorf("stderr = %q, want %q", got, want)
	}
	if out.Len() != 0 {
		t.Errorf("stdout = %q, want nothing", out.String())
	}
	pid, err := os.ReadFile(filepath.Join(lockd, "pid"))
	if err != nil || string(pid) != "0\n" {
		t.Errorf("the lock was reclaimed (%q, %v), want it refused", pid, err)
	}
}

// A TERM while the inner watch is parked stops the child and waits for it, so
// both locks are gone by the time the lane exits — and the cursor, which only a
// live watch folds, never moves.
func TestStreamTermMidParkReleasesTheWatchAndLeavesTheCursor(t *testing.T) {
	p := streamBus(t)
	s := realWatch(t, p)
	l := startLane(t, p, s, Request{Park: "10", Interval: "1"}, laneOpts{Timer: time.After})

	waitFor(t, "the inner watch's lock", func() bool { return existsIn(p, "watch.lock.d") })
	// The pid the watch wrote is its own — the stub execs it, so it kept it — and
	// it is the one the lock names.
	pid := watchPID(t, s)
	if got := lock.Holder(cdirFile(p, "watch.lock.d")); got != strconv.Itoa(pid) {
		t.Fatalf("watch.lock.d names %q, want the parked watch's pid %d", got, pid)
	}

	l.signal(syscall.SIGTERM)
	if rc := l.waitRC(); rc != 0 {
		t.Errorf("exit = %d, want 0 (a caught signal is a deliberate stop)", rc)
	}
	// Bounded by nothing: the cleanup waits for the child, so by the time Run has
	// returned it is gone rather than about to be.
	if err := syscall.Kill(pid, 0); err == nil {
		t.Errorf("the inner watch pid %d outlived its stream", pid)
	}
	for _, lockd := range []string{"watch.lock.d", "stream.lock.d"} {
		if existsIn(p, lockd) {
			t.Errorf("%s survived the cleanup", lockd)
		}
	}
	if existsIn(p, "cursor") {
		t.Error("the cursor advanced without a qualifying batch")
	}

	// An orphan would print a batch to a stdout nobody reads and still mv its
	// cursor into place, and a watch can advance the cursor and release the lock
	// on the same exit.
	postRow(t, p, statusRow(time.Now().UnixMilli(), "c1", "worker:feat/x", "done"))
	if !stayedFalse(1500*time.Millisecond, func() bool { return existsIn(p, "cursor") }) {
		t.Error("an orphaned watch advanced the cursor after its stream had stopped")
	}
}

// The signal contract, per signal: exit 0, and both lock dirs gone — a leaked
// watch.lock.d refuses every later park of the crew, a leaked stream.lock.d every
// later stream.
func TestStreamSignalsReleaseBothLocksAndTheNextStreamStarts(t *testing.T) {
	for _, sig := range []os.Signal{syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP} {
		t.Run(sig.String(), func(t *testing.T) {
			p := streamBus(t)
			s := realWatch(t, p)
			l := startLane(t, p, s, Request{Park: "10", Interval: "1"}, laneOpts{Timer: time.After})
			waitFor(t, "both locks", func() bool {
				return existsIn(p, "stream.lock.d") && existsIn(p, "watch.lock.d")
			})

			l.signal(sig)
			if rc := l.waitRC(); rc != 0 {
				t.Errorf("exit = %d, want 0", rc)
			}
			for _, lockd := range []string{"stream.lock.d", "watch.lock.d"} {
				if existsIn(p, lockd) {
					t.Errorf("%s survived %s", lockd, sig)
				}
			}

			// And the very next stream starts rather than refusing: it acquires
			// the lock and reaches its first iteration.
			next := startLane(t, p, s, Request{Park: "1", Interval: "1"}, laneOpts{Timer: time.After})
			if got := next.errb.str(); got != "" {
				t.Errorf("the next stream wrote %q to stderr, want a clean start", got)
			}
		})
	}
}

// A TERM that lands while the inner watch has printed a batch the loop never got
// to read must not lose it: the drain is what the cleanup does before it unlinks
// the file, and the batch has to end its line — a reap line printed next is
// otherwise glued onto it.
func TestStreamTermDrainsAnUndrainedBatch(t *testing.T) {
	p := streamBus(t)
	s := entryStub(t, map[string]string{"watch": parkedWatch("0")})
	l := startLane(t, p, s, Request{Park: "10"}, laneOpts{})
	waitFor(t, "the park", func() bool { return s.calls("watch") >= 1 })

	// The natural window for this is microseconds wide, so the undrained batch is
	// planted: the child is parked with nothing written.
	seedFile(t, p, "stream.out", doneBatch+"\n")

	l.signal(syscall.SIGTERM)
	if rc := l.waitRC(); rc != 0 {
		t.Errorf("exit = %d, want 0", rc)
	}
	if got := l.raw(); got != doneBatch+"\n" {
		t.Errorf("stdout = %q, want the undrained batch, line-terminated", got)
	}
	if existsIn(p, "stream.out") {
		t.Error("stream.out survived the drain")
	}
}

// Both printing paths, and the park-expiry line the inner watch writes to *its*
// stderr on every expired park: the lane's own stderr stays empty, or prose would
// sit next to the JSON a dispatcher parses.
func TestStreamStderrStaysEmptyInNormalOperation(t *testing.T) {
	p := streamBus(t)
	s := realWatch(t, p)
	postRow(t, p, statusRow(time.Now().UnixMilli(), "c1", "worker:feat/x", "done"))
	l := startLane(t, p, s, Request{Park: "1", Interval: "1", Heartbeat: "2"}, laneOpts{Timer: time.After})

	l.waitLines(2)
	lines := l.lines()
	if _, tagged := lines[0]["stream"]; tagged {
		t.Errorf("first line = %v, want the batch", lines[0])
	}
	if lines[1]["stream"] != "heartbeat" {
		t.Errorf("second line = %v, want the heartbeat", lines[1])
	}
	if got := l.errb.str(); got != "" {
		t.Errorf("the lane wrote %q to its own stderr, want nothing", got)
	}
}

// watchPID is the pid the real-watch stub recorded for the watch it execs.
func watchPID(t *testing.T, s *stub) int {
	t.Helper()
	text := strings.TrimSpace(s.read("watch.pid"))
	pid, err := strconv.Atoi(text)
	if err != nil {
		t.Fatalf("the watch recorded no pid (%q)", text)
	}
	return pid
}

// stayedFalse waits out the whole window and reports whether cond never held —
// the shape every "and nothing further happened" assertion in these ports needs.
func stayedFalse(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return false
		}
		time.Sleep(25 * time.Millisecond)
	}
	return true
}
