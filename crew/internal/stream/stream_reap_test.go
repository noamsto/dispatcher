// The ported `stream:` reap cases. The loop's side of the contract is what
// these pin: it spawns `crew reap` as a background child it neither awaits nor
// signals, turns that child's stdout into one `reap` line and its stderr plus
// non-zero status into one rate-limited `error` line, and keeps each child's
// output in a pair of files of its own. Which workers a real `crew reap` would
// reclaim is the subject of the reap suite's own tests.
package stream

import (
	"strings"
	"testing"
	"time"
)

// reapLineText is one `reap` line's `lines` array joined, for the "reaped feat/x"
// assertions the bats suite made with a grep.
func reapLineText(t *testing.T, line map[string]any) string {
	t.Helper()
	rows, _ := line["lines"].([]any)
	var out []string
	for _, r := range rows {
		if s, ok := r.(string); ok {
			out = append(out, s)
		}
	}
	return strings.Join(out, "\n")
}

// A cadence reap reports what it reclaimed as a `reap` line of the lane's own
// crew, and never as a batch: a batch is the bus, and nothing about a reap is an
// event anyone's cursor should move past.
func TestStreamCadenceReapIsItsOwnLine(t *testing.T) {
	p := streamBus(t)
	clk := &fakeClock{}
	s := entryStub(t, map[string]string{
		"watch": parkedWatch("0"),
		"reap":  `printf 'crew reap: reaped feat/x\n'`,
	})
	l := startLane(t, p, s, Request{ReapEvery: "1"}, laneOpts{Clock: clockNow(clk)})

	l.park()
	if got := s.calls("reap"); got != 0 {
		t.Fatalf("the cadence fired after %d parks, want --reap-every 1 to need a second", got)
	}
	clk.advance(2 * time.Second)
	l.parkUntil("the reap child", func() bool { return s.calls("reap") >= 1 })
	l.parkUntil("the reap line", func() bool { return len(l.ofKind("reap")) >= 1 })

	line := l.ofKind("reap")[0]
	if line["stream"] != "reap" || line["crew"] != "c1" {
		t.Errorf("line = %v, want a reap line for this lane's crew", line)
	}
	// The arm's `sub("^crew reap: ";"")`, once per line.
	if got := reapLineText(t, line); got != "reaped feat/x" {
		t.Errorf("lines = %q, want the `crew reap: ` prefix stripped", got)
	}
	if got := l.batches(); len(got) != 0 {
		t.Errorf("the reap arrived as %v, want no batch line", got)
	}
}

// A terminal batch is worth a reap now, whatever the cadence says: the lane does
// not sit until the next --reap-every with a finished worker on its hands.
func TestStreamTerminalBatchTriggersReap(t *testing.T) {
	p := streamBus(t)
	s := entryStub(t, map[string]string{
		"watch": printsOnceThenPark(doneBatch),
		"reap":  `printf 'reaped feat/x\n'`,
	})
	l := startLane(t, p, s, Request{ReapEvery: "3600"}, laneOpts{})

	l.park()
	l.parkUntil("the reap line", func() bool { return len(l.ofKind("reap")) >= 1 })

	if got := s.calls("reap"); got != 1 {
		t.Errorf("the lane ran %d reaps, want the terminal batch to have triggered exactly one", got)
	}
	line := l.ofKind("reap")[0]
	if got := reapLineText(t, line); got != "reaped feat/x" {
		t.Errorf("lines = %q", got)
	}
}

// --reap-every 0 is how the cadence reap — and the terminal-batch trigger that
// shares its gate — is switched off.
func TestStreamReapEveryZeroReapsNothing(t *testing.T) {
	p := streamBus(t)
	s := entryStub(t, map[string]string{
		"watch": printsOnceThenPark(doneBatch),
		"reap":  `printf 'reaped feat/x\n'`,
	})
	l := startLane(t, p, s, Request{ReapEvery: "0"}, laneOpts{})

	l.park()
	l.park()
	l.park()

	if got := s.calls("reap"); got != 0 {
		t.Errorf("the lane ran %d reaps with --reap-every 0", got)
	}
	if got := l.ofKind("reap"); len(got) != 0 {
		t.Errorf("reap lines = %v, want none", got)
	}
	if got := len(l.batches()); got != 1 {
		t.Errorf("batches = %d, want the terminal batch itself delivered", got)
	}
}

// A reap with nothing to reclaim says nothing, and leaves no pair of files
// behind it beyond the one still in flight.
func TestStreamNoOpReapIsSilent(t *testing.T) {
	p := streamBus(t)
	clk := &fakeClock{}
	s := entryStub(t, map[string]string{"watch": parkedWatch("0")})
	l := startLane(t, p, s, Request{ReapEvery: "1"}, laneOpts{Clock: clockNow(clk)})

	clk.advance(2 * time.Second)
	l.parkUntil("the reap child", func() bool { return s.calls("reap") >= 1 })
	l.park()
	l.park()

	if got := l.ofKind("reap"); len(got) != 0 {
		t.Errorf("reap lines = %v, want silence from a no-op reap", got)
	}
	if got := l.ofKind("error"); len(got) != 0 {
		t.Errorf("error lines = %v", got)
	}
	// A no-op reap still creates its per-child pair; with the tracked child gone
	// the flush drops it, so at most the in-flight pair may remain (#602 review).
	if got := reapPairs(p); len(got) > 1 {
		t.Errorf("reap pairs left = %v, want at most the in-flight one", got)
	}
}

// A reap that fails is one `error` line, attributed to the reap and rate-limited
// like every other cause: one broken anchor is not a line per cadence tick.
func TestStreamFailingReapIsOneErrorLine(t *testing.T) {
	p := streamBus(t)
	clk := &fakeClock{}
	s := entryStub(t, map[string]string{
		"watch": parkedWatch("0"),
		"reap": `echo "crew reap: /elsewhere/crew is not this repo's bus — possible tampering" >&2
    exit 1`,
	})
	l := startLane(t, p, s, Request{ReapEvery: "1", Heartbeat: "3600"}, laneOpts{Clock: clockNow(clk)})

	clk.advance(2 * time.Second)
	l.parkUntil("the failing reap", func() bool { return len(l.ofKind("error")) >= 1 })
	// Two more cadence ticks of the same failure.
	clk.advance(2 * time.Second)
	l.park()
	clk.advance(2 * time.Second)
	l.park()
	l.park()

	if got := s.calls("reap"); got < 3 {
		t.Fatalf("the lane ran %d reaps, want the same failure to have been retried", got)
	}
	if got := l.ofKind("reap"); len(got) != 0 {
		t.Errorf("reap lines = %v, want none for a reap that said nothing on stdout", got)
	}
	errs := l.ofKind("error")
	if len(errs) != 1 {
		t.Fatalf("error lines = %s, want one for one cause", l.raw())
	}
	line := errs[0]
	if line["stream"] != "error" || line["crew"] != "c1" {
		t.Errorf("line = %v", line)
	}
	if code, ok := line["rc"].(float64); !ok || int64(code) == 0 {
		t.Errorf("rc = %v, want the reap child's own status", line["rc"])
	}
	d, _ := line["detail"].(string)
	if !strings.HasPrefix(d, "reap: ") || !strings.Contains(d, "possible tampering") {
		t.Errorf("detail = %q, want the child's stderr attributed to the reap", d)
	}
}

// #602: a reap child the lane spawned may outlive it, and its late write must
// not be read as the next lane's reap output. Lane A's child blocks until after A
// is gone; lane B reaps on its own; A's line must never surface in B.
func TestStreamOutlivedReapChildCannotClobberTheNextLane(t *testing.T) {
	p := streamBus(t)
	clk := &fakeClock{}

	// A's reap child blocks on a release file, so it is still running when A is
	// stopped. Releasing it is a cleanup, never a leftover process.
	releaseA := t.TempDir() + "/release"
	t.Cleanup(func() { writeFile(t, releaseA, "x") })
	sa := entryStub(t, map[string]string{
		"watch": parkedWatch("0"),
		"reap": `while [ ! -e '` + releaseA + `' ]; do sleep 0.02; done
    printf 'reaped feat/a\n'
    : > "$d/late"`,
	})
	a := startLane(t, p, sa, Request{ReapEvery: "1"}, laneOpts{Clock: clockNow(clk)})
	clk.advance(2 * time.Second)
	a.parkUntil("A's reap child", func() bool { return sa.calls("reap") >= 1 })
	a.stop()
	// A's cleanup took its reap pair with it, which is the half of #602 that
	// keeps a lane from ever reading a file it did not create.
	if got := reapPairs(p); len(got) != 0 {
		t.Errorf("A left %v behind, want its reap pair swept", got)
	}

	// B reaps promptly and flushes its own line.
	sb := entryStub(t, map[string]string{
		"watch": parkedWatch("0"),
		"reap":  `printf 'reaped feat/b\n'`,
	})
	b := startLane(t, p, sb, Request{ReapEvery: "1"}, laneOpts{Clock: clockNow(clk)})
	clk.advance(2 * time.Second)
	b.parkUntil("B's reap line", func() bool { return len(b.ofKind("reap")) >= 1 })

	// Release A's outlived child and wait for it to have written at all — after
	// its stream was gone, and while B is turning.
	writeFile(t, releaseA, "x")
	waitFor(t, "A's late reap write", func() bool { return sa.has("late") })
	b.park()
	b.park()

	if got := b.raw(); !strings.Contains(got, "reaped feat/b") {
		t.Errorf("B printed %q, want its own reap", got)
	}
	if got := b.raw(); strings.Contains(got, "reaped feat/a") {
		t.Errorf("B surfaced A's outlived reap: %s", got)
	}
}
