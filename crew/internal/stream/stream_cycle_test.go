// The ported `stream:` cycle cases: the heartbeat's quiet counter, the inner
// watch's error suppression, and the matured-hold announcements. Each one drives
// the loop park by park — the stub `watch` child does not return until the test
// releases it, and `lane.park` waits for the iteration that follows — so "one
// line per cause, not one per park" is a count.
package stream

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// holdDueJSON is what `crew hold due --json` answers with: the matured rows and
// the fleet-parked ones, which the stream embeds verbatim and keys its
// suppression on.
func holdDueJSON(due, parked string) string {
	return `{"due":` + due + `,"parked":` + parked + `}`
}

// holdRowJSON is an array of matured hold rows, one per id.
func holdRowJSON(ids ...string) string { return holdRows("", ids) }

// parkedRowJSON is an array of rows the fleet queue parked, each with its reason.
func parkedRowJSON(reason string, ids ...string) string {
	return holdRows(fmt.Sprintf(`,"reason":%q`, reason), ids)
}

func holdRows(extra string, ids []string) string {
	var b strings.Builder
	b.WriteString("[")
	for i, id := range ids {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"id":%q,"ref":"FOO-1","branch":"feat/x","tier":"standard","title":"hold %s"%s}`, id, id, extra)
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
		"hold due": `printf '%s\n' '` + holdDueJSON(holdRowJSON("h1"), "[]") + `'; exit 0`,
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
	if line["playbook"] != "dispatcher:budget-holds" {
		t.Errorf("playbook = %v, want dispatcher:budget-holds", line["playbook"])
	}
	if parked, has := line["parked"]; has {
		t.Errorf("parked = %v, want no key for a line with nothing parked", parked)
	}
}

// parkedOf is a hold_due line's parked rows, as "id=reason" pairs.
func parkedOf(t *testing.T, v any) string {
	t.Helper()
	rows, ok := v.([]any)
	if !ok {
		t.Fatalf("parked = %#v, want an array", v)
	}
	pairs := make([]string, 0, len(rows))
	for _, row := range rows {
		m, ok := row.(map[string]any)
		if !ok {
			t.Fatalf("parked row = %#v", row)
		}
		id, _ := m["id"].(string)
		reason, _ := m["reason"].(string)
		pairs = append(pairs, id+"="+reason)
	}
	sort.Strings(pairs)
	return strings.Join(pairs, ",")
}

// A crew whose only holds are parked by the fleet queue has nothing due — `hold
// due` exits 1 — and still announces: the owner learns of a parked hold even with
// no other watcher. holds stays the due array, empty here.
func TestStreamParkedOnlyHoldAnnounces(t *testing.T) {
	p := streamBus(t)
	s := entryStub(t, map[string]string{
		"watch":    parkedWatch("0"),
		"hold due": `printf '%s\n' '` + holdDueJSON("[]", parkedRowJSON("demoted", "p1")) + `'; exit 1`,
	})
	l := startLane(t, p, s, Request{}, laneOpts{})

	l.waitKind("hold_due", 1)
	l.park()
	l.park()

	dues := l.ofKind("hold_due")
	if len(dues) != 1 {
		t.Fatalf("hold_due lines = %d (%s), want one", len(dues), l.raw())
	}
	line := dues[0]
	if got := holdIDsOf(t, line["holds"]); got != "" {
		t.Errorf("holds = %v, want the empty due array", line["holds"])
	}
	if got := parkedOf(t, line["parked"]); got != "p1=demoted" {
		t.Errorf("parked = %v, want the one parked hold with its reason", line["parked"])
	}
}

// Due and parked holds travel on one line, each in its own field.
func TestStreamDueAndParkedAnnounceTogether(t *testing.T) {
	p := streamBus(t)
	s := entryStub(t, map[string]string{
		"watch":    parkedWatch("0"),
		"hold due": `printf '%s\n' '` + holdDueJSON(holdRowJSON("h1"), parkedRowJSON("oversize", "p1")) + `'; exit 0`,
	})
	l := startLane(t, p, s, Request{}, laneOpts{})

	l.waitKind("hold_due", 1)
	line := l.ofKind("hold_due")[0]
	if got := holdIDsOf(t, line["holds"]); got != "h1" {
		t.Errorf("holds = %v, want the due id", line["holds"])
	}
	if got := parkedOf(t, line["parked"]); got != "p1=oversize" {
		t.Errorf("parked = %v, want the parked hold with its reason", line["parked"])
	}
}

// The suppression key covers both sets: a change in the parked set alone
// re-announces, and the unchanged pair then stays silent.
func TestStreamParkedSetChangeReannounces(t *testing.T) {
	p := streamBus(t)
	s := entryStub(t, map[string]string{
		"watch": parkedWatch("0"),
		"hold due": `if [ -f "$d/release" ]; then printf '%s\n' '` + holdDueJSON(holdRowJSON("h1"), parkedRowJSON("demoted", "p2")) + `';
    else printf '%s\n' '` + holdDueJSON(holdRowJSON("h1"), parkedRowJSON("demoted", "p1", "p2")) + `'; fi
    exit 0`,
	})
	l := startLane(t, p, s, Request{}, laneOpts{})

	l.waitKind("hold_due", 1)
	if got := parkedOf(t, l.ofKind("hold_due")[0]["parked"]); got != "p1=demoted,p2=demoted" {
		t.Errorf("first announcement parked = %q, want both parked holds", got)
	}

	s.write("release", "p1")
	l.park()
	l.waitKind("hold_due", 2)
	second := l.ofKind("hold_due")[1]
	if got := holdIDsOf(t, second["holds"]); got != "h1" {
		t.Errorf("re-announcement holds = %q, want the unchanged due id", got)
	}
	if got := parkedOf(t, second["parked"]); got != "p2=demoted" {
		t.Errorf("re-announcement parked = %q, want the surviving parked id alone", got)
	}

	l.park()
	l.park()
	if got := len(l.ofKind("hold_due")); got != 2 {
		t.Errorf("hold_due lines = %d (%s), want the unchanged pair silent", got, l.raw())
	}
}

// Anything but the object — the bare array an older `hold due` printed, or an
// object whose either key is not an array — is an undecodable answer, which the
// stream leaves silent.
func TestDecodeHoldDue(t *testing.T) {
	for _, bad := range []string{
		`[]`,
		`[{"id":"h1"}]`,
		`null`,
		`{}`,
		`{"due":[]}`,
		`{"parked":[]}`,
		`{"due":null,"parked":[]}`,
		`{"due":{},"parked":[]}`,
		`{"due":[],"parked":"p1"}`,
		`{"due":[]} {"parked":[]}`,
	} {
		if _, _, ok := decodeHoldDue(bad); ok {
			t.Errorf("decodeHoldDue(%s) accepted, want it refused", bad)
		}
	}
	due, parked, ok := decodeHoldDue(holdDueJSON(holdRowJSON("h1", "h2"), parkedRowJSON("demoted", "p1")))
	if !ok || due.n != 2 || parked.n != 1 {
		t.Errorf("decodeHoldDue = (%d, %d, %t), want (2, 1, true)", due.n, parked.n, ok)
	}
	if got := holdIDs(due.raw); got != "h1,h2" {
		t.Errorf("due ids = %q", got)
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
		"hold due": `if [ -f "$d/release" ]; then printf '%s\n' '` + holdDueJSON(holdRowJSON("h2"), "[]") + `';
    else printf '%s\n' '` + holdDueJSON(holdRowJSON("h1", "h2"), "[]") + `'; fi
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
//
// The lane runs on a stepped clock: --heartbeat is the quiet threshold in parks
// and, at once, the window a still-matured hold re-announces on in seconds, and
// three parks take however long the machine takes. A clock the case steps keeps
// that window shut, so the count is about the matured set and not the load (#1295).
func TestStreamHoldDueDoesNotResetQuiet(t *testing.T) {
	p := streamBus(t)
	s := entryStub(t, map[string]string{
		"watch":    parkedWatch("0"),
		"hold due": `printf '%s\n' '` + holdDueJSON(holdRowJSON("h1"), "[]") + `'; exit 0`,
	})
	const heartbeatSecs = 3 // parks to a heartbeat, and the re-announce window in seconds
	clk := newStepClock()
	l := startLane(t, p, s, Request{Heartbeat: strconv.Itoa(heartbeatSecs)}, laneOpts{Clock: clockStep(clk)})

	l.waitKind("hold_due", 1)
	l.park()
	l.park()
	l.park()
	l.waitKind("heartbeat", 1)

	// The window held open is heartbeatSecs of lane time: 3000 reads of a clock
	// that moves a millisecond per read, against the twenty-odd the case spends.
	// If that margin ever goes, the count below turns load-sensitive and this
	// fails first, naming the window.
	if spent := clk.laneTime(); spent >= heartbeatSecs*time.Second {
		t.Fatalf("the case spent %v of lane time, the --heartbeat window it holds open", spent)
	}

	hb := l.ofKind("heartbeat")[0]
	if quiet, ok := hb["quiet_s"].(float64); !ok || int64(quiet) != heartbeatSecs {
		t.Errorf("heartbeat = %v, want quiet_s %d with hold_due firing alongside", hb, heartbeatSecs)
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
