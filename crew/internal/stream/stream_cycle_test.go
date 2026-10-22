// The ported `stream:` cycle cases: the heartbeat's quiet counter, the inner
// watch's error suppression, and the matured-hold announcements. Each one drives
// the loop park by park — the stub `watch` child does not return until the test
// releases it, and `lane.park` waits for the iteration that follows — so "one
// line per cause, not one per park" is a count.
package stream

import (
	"fmt"
	"strings"
	"testing"
)

// holdRowJSON is what `crew hold due --json` answers with: the matured rows the
// stream embeds verbatim and keys its suppression on.
func holdRowJSON(ids ...string) string {
	var b strings.Builder
	b.WriteString("[")
	for i, id := range ids {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"id":%q,"ref":"FOO-1","branch":"feat/x","tier":"standard","title":"hold %s"}`, id, id)
	}
	b.WriteString("]")
	return b.String()
}

// Three parks' worth of quiet is one heartbeat, at --park 1 --heartbeat 3: the
// count is of parks, not of seconds.
func TestStreamHeartbeatAfterQuietOfParks(t *testing.T) {
	p := streamBus(t)
	s := entryStub(t, map[string]string{"watch": parkedWatch("0")})
	l := startLane(t, p, s, Request{Heartbeat: "3"}, laneOpts{})

	l.park()
	l.park()
	// Two parks is below --heartbeat, and quiet only ever counts a completed
	// park, so there is nothing for the lane to have printed yet.
	if got := l.raw(); got != "" {
		t.Fatalf("the lane printed %q after two parks, want nothing", got)
	}

	l.park()
	// The heartbeat is emitted before the next iteration's tick, and park waits
	// for that tick, so the lane is parked again with its count settled: one
	// heartbeat for three parks, not one per park.
	lines := l.lines()
	if len(lines) != 1 {
		t.Fatalf("lines = %d (%s), want one heartbeat", len(lines), l.raw())
	}
	line := lines[0]
	if line["stream"] != "heartbeat" || line["crew"] != "c1" {
		t.Errorf("line = %v, want a c1 heartbeat", line)
	}
	if quiet, ok := line["quiet_s"].(float64); !ok || int64(quiet) != 3 {
		t.Errorf("quiet_s = %v, want the 3 parks it counted", line["quiet_s"])
	}
}

// The inner watch failing is one line per cause: the same failure on the next
// park is silence, and a different cause earns its own line. The two faults are a
// live holder of the crew's watch lock, then a swapped-in failure with a
// different rc and detail.
func TestStreamInnerFailureIsOneLinePerCause(t *testing.T) {
	p := streamBus(t)
	s := entryStub(t, map[string]string{"watch": parkedWatchFailing(`
  if [ -f "$d/fault2" ]; then echo "crew: fault injected" >&2; exit 3; fi
  echo "crew: another watch is already running for this crew (c1)" >&2
  exit 1`)})
	l := startLane(t, p, s, Request{Retry: "1"}, laneOpts{})

	l.park()
	l.waitKind("error", 1)
	first := l.ofKind("error")[0]
	if code, ok := first["rc"].(float64); !ok || int64(code) != 1 {
		t.Errorf("first error = %v, want rc 1", first)
	}
	if d, _ := first["detail"].(string); !strings.Contains(d, "another watch is already running") {
		t.Errorf("first error detail = %q", d)
	}
	if errb := l.errb.str(); !strings.Contains(errb, "stream: inner watch failed (rc 1)") {
		t.Errorf("stderr = %q, want the operator-facing note", errb)
	}

	// One more park of the same failure: the loop turned (park returned and the
	// next one started) and said nothing.
	l.park()
	if got := len(l.ofKind("error")); got != 1 {
		t.Fatalf("error lines = %d (%s), want the repeated cause suppressed", got, l.raw())
	}

	// A different cause IS re-emitted, on the very next park.
	s.write("fault2", "x")
	l.park()
	l.waitKind("error", 2)
	errs := l.ofKind("error")
	second := errs[1]
	if code, ok := second["rc"].(float64); !ok || int64(code) != 3 {
		t.Errorf("second error = %v, want rc 3", second)
	}
	if d, _ := second["detail"].(string); d != "crew: fault injected" {
		t.Errorf("second error detail = %q", d)
	}
	if len(l.lines()) != 2 {
		t.Errorf("lines = %s, want exactly the two error lines", l.raw())
	}
}

// A matured hold announces once, however many parks it stays matured across, and
// the line carries the matured set.
func TestStreamMaturedHoldAnnouncesOnce(t *testing.T) {
	p := streamBus(t)
	s := entryStub(t, map[string]string{
		"watch":    parkedWatch("0"),
		"hold due": `printf '%s\n' '` + holdRowJSON("h1") + `'; exit 0`,
	})
	l := startLane(t, p, s, Request{}, laneOpts{})

	l.waitKind("hold_due", 1)
	l.park()
	l.park()
	l.park()

	dues := l.ofKind("hold_due")
	if len(dues) != 1 {
		t.Fatalf("hold_due lines = %d (%s), want one", len(dues), l.raw())
	}
	line := dues[0]
	if line["stream"] != "hold_due" || line["crew"] != "c1" {
		t.Errorf("line = %v", line)
	}
	if got := holdIDsOf(t, line["holds"]); got != "h1" {
		t.Errorf("holds = %v, want the one matured id", line["holds"])
	}
}

// An unmatured hold — `hold due`'s own "nothing matured", exit 1 with no rows —
// is silence: no hold_due line, and no batch either.
func TestStreamUnmaturedHoldStaysQuiet(t *testing.T) {
	p := streamBus(t)
	s := entryStub(t, map[string]string{"watch": parkedWatch("0")})
	l := startLane(t, p, s, Request{Heartbeat: "3600"}, laneOpts{})

	waitFor(t, "the first park", func() bool { return s.calls("watch") >= 1 })
	l.park()
	l.park()
	l.park()

	if got := l.raw(); got != "" {
		t.Errorf("the lane printed %q, want nothing for an unmatured hold", got)
	}
}

// Releasing one of two matured holds changes the matured set, and the set — not
// the count — is the suppression key: the survivor re-announces on the next park
// with no restart.
func TestStreamHoldSetChangeReannounces(t *testing.T) {
	p := streamBus(t)
	s := entryStub(t, map[string]string{
		"watch": parkedWatch("0"),
		"hold due": `if [ -f "$d/release" ]; then printf '%s\n' '` + holdRowJSON("h2") + `';
    else printf '%s\n' '` + holdRowJSON("h1", "h2") + `'; fi
    exit 0`,
	})
	l := startLane(t, p, s, Request{}, laneOpts{})

	l.waitKind("hold_due", 1)
	if got := holdIDsOf(t, l.ofKind("hold_due")[0]["holds"]); got != "h1,h2" {
		t.Errorf("first announcement = %q, want both matured ids", got)
	}

	s.write("release", "h1")
	l.park()
	l.waitKind("hold_due", 2)
	if got := holdIDsOf(t, l.ofKind("hold_due")[1]["holds"]); got != "h2" {
		t.Errorf("re-announcement = %q, want the surviving id alone", got)
	}

	// The set holding pat — and --heartbeat nowhere near — adds no third line.
	l.park()
	l.park()
	if got := len(l.ofKind("hold_due")); got != 2 {
		t.Errorf("hold_due lines = %d (%s), want the unchanged set silent", got, l.raw())
	}
}

// A hold_due line is news, but it is not *quiet* news: the heartbeat counter
// keeps counting parks beside it.
func TestStreamHoldDueDoesNotResetQuiet(t *testing.T) {
	p := streamBus(t)
	s := entryStub(t, map[string]string{
		"watch":    parkedWatch("0"),
		"hold due": `printf '%s\n' '` + holdRowJSON("h1") + `'; exit 0`,
	})
	l := startLane(t, p, s, Request{Heartbeat: "3"}, laneOpts{})

	l.waitKind("hold_due", 1)
	l.park()
	l.park()
	l.park()
	l.waitKind("heartbeat", 1)

	hb := l.ofKind("heartbeat")[0]
	if quiet, ok := hb["quiet_s"].(float64); !ok || int64(quiet) != 3 {
		t.Errorf("heartbeat = %v, want quiet_s 3 with hold_due firing alongside", hb)
	}
	if got := len(l.ofKind("hold_due")); got != 1 {
		t.Errorf("hold_due lines = %d, want the one announcement", got)
	}
}

// `hold due` exit 1 is "nothing matured"; anything past it is a failure the
// stream may not fold into that silence, because a hold exists precisely when
// nobody is watching the lane.
func TestStreamHoldDueFailureIsAnError(t *testing.T) {
	p := streamBus(t)
	s := entryStub(t, map[string]string{
		"watch":    parkedWatch("0"),
		"hold due": `echo "crew: hold fault injected" >&2; exit 3`,
	})
	l := startLane(t, p, s, Request{}, laneOpts{})

	l.waitKind("error", 1)
	line := l.ofKind("error")[0]
	if line["stream"] != "error" || line["crew"] != "c1" {
		t.Errorf("line = %v", line)
	}
	if code, ok := line["rc"].(float64); !ok || int64(code) != 3 {
		t.Errorf("rc = %v, want the failing hold due's own status", line["rc"])
	}
	if d, _ := line["detail"].(string); d != "hold due: crew: hold fault injected" {
		t.Errorf("detail = %q, want it attributed to hold due", d)
	}

	// Suppressed like the inner-watch path, with the loop still turning.
	l.park()
	l.park()
	if got := len(l.ofKind("error")); got != 1 {
		t.Errorf("error lines = %d (%s), want the same cause suppressed", got, l.raw())
	}
	if got := len(l.ofKind("hold_due")); got != 0 {
		t.Errorf("a failed hold due must not fold into an empty announcement: %s", l.raw())
	}
}

// A crew with no holds at all streams batches and heartbeats exactly as it did
// before the hold pre-check existed.
func TestStreamNoHoldsStreamsBatchesAndHeartbeats(t *testing.T) {
	p := streamBus(t)
	s := entryStub(t, map[string]string{"watch": printsOnceThenPark(doneBatch)})
	l := startLane(t, p, s, Request{Heartbeat: "3"}, laneOpts{})

	// The batch reaches stdout when its park returns, so the first park is what
	// delivers it; the heartbeat then needs three quiet parks after it.
	l.park()
	l.waitLines(1)
	l.park()
	l.park()
	l.park()
	l.waitLines(2)

	lines := l.lines()
	if len(lines) != 2 {
		t.Fatalf("lines = %s, want the batch and one heartbeat", l.raw())
	}
	if _, tagged := lines[0]["stream"]; tagged {
		t.Errorf("first line = %v, want the pass-through batch first", lines[0])
	}
	events, _ := lines[0]["events"].([]any)
	if len(events) != 1 {
		t.Errorf("batch = %v", lines[0])
	}
	if lines[1]["stream"] != "heartbeat" {
		t.Errorf("second line = %v, want the heartbeat", lines[1])
	}
	if quiet, ok := lines[1]["quiet_s"].(float64); !ok || int64(quiet) != 3 {
		t.Errorf("quiet_s = %v, want 3", lines[1]["quiet_s"])
	}
	if got := len(l.ofKind("hold_due")); got != 0 {
		t.Errorf("hold_due lines = %d, want none for a crew with no holds", got)
	}
}
