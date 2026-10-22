// The ported `stream:` park cases: what a batch looks like on the way through,
// and what does and does not end a park. These run the real `crew watch` behind
// the entry stub — cursor seeding, the wake fold and the states filter are the
// subject, so standing them in for would assert the stub rather than the code.
package stream

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
)

// passBatch is a compact `crew watch` batch, the shape `crew stream` is meant to
// pass through with its bytes untouched.
const passBatch = `{"cursor":1700000000000,"events":[{"ts":1700000000000,"crew_id":"c1","from":"worker:feat/x","to":"dispatcher:c1","kind":"status","body":{"state":"done"}}]}`

// The stream's whole job on the batch path: the line `watch` printed, once, and
// newline-terminated. The bats case compared it to `jq -c .` of itself to prove
// it was not re-encoded; byte equality with the child's line is the same claim
// with no second implementation in the way.
func TestStreamPassesBatchThroughVerbatim(t *testing.T) {
	p := streamBus(t)
	s := entryStub(t, map[string]string{"watch": printsThenPark(passBatch)})
	l := startLane(t, p, s, Request{}, laneOpts{})

	l.park()
	l.waitLines(1)
	if got := l.raw(); got != passBatch+"\n" {
		t.Errorf("stdout = %q, want the watch's own line, byte for byte, newline-terminated", got)
	}
	line := l.lines()[0]
	events, ok := line["events"].([]any)
	if !ok || len(events) != 1 {
		t.Fatalf("events = %v, want the one event", line["events"])
	}
	event, _ := events[0].(map[string]any)
	body, _ := event["body"].(map[string]any)
	if body["state"] != "done" {
		t.Errorf("event = %v", event)
	}
	if line["cursor"] != event["ts"] {
		t.Errorf("cursor = %v, want the batch's own, equal to its last event's ts", line["cursor"])
	}
}

// A re-stamped blocked — the same question with its cycle counter moved on — is
// not news, so the lane stays quiet through it; a genuinely different question
// wakes it. The suppression itself is `crew watch`'s (watch_test.go's
// TestBlockedRestampSuppression); what this pins is that the lane inherits it
// rather than re-deciding, and that the batch it does deliver is the new one.
func TestStreamRestampedBlockedDoesNotWake(t *testing.T) {
	const from = "worker:feat/x#s1-1"
	p := streamBus(t)
	s := realWatch(t, p)
	now := time.Now().UnixMilli()
	postRow(t, p, statusDetailRow(now-2000, "c1", from, "blocked",
		"need a waiver — awaited 300s, no reply (cycle 7 of 24)"))

	l := startLane(t, p, s, Request{Park: "1", Interval: "1"}, laneOpts{Timer: time.After})
	l.waitLines(1)
	if got := len(l.batches()); got != 1 {
		t.Fatalf("batches = %d (%s)", got, l.raw())
	}

	postRow(t, p, statusDetailRow(now, "c1", from, "blocked",
		"need a waiver — awaited 300s, no reply (cycle 8 of 24)"))

	// Two more parks of the restamp on the log, and the lane has still said
	// nothing: an expired park prints no batch.
	turnParks(t, p, 2)
	if got := len(l.batches()); got != 1 {
		t.Fatalf("batches = %d (%s), want the restamp suppressed", got, l.raw())
	}

	postRow(t, p, statusDetailRow(time.Now().UnixMilli(), "c1", from, "blocked", "different question (cycle 2 of 24)"))
	l.waitLines(2)
	batch := l.batches()[1]
	events, _ := batch["events"].([]any)
	if len(events) != 1 {
		t.Fatalf("waking batch = %v, want only the new question", batch)
	}
	event, _ := events[0].(map[string]any)
	body, _ := event["body"].(map[string]any)
	if body["detail"] != "different question (cycle 2 of 24)" {
		t.Errorf("detail = %v", body["detail"])
	}
}

// `exited` is one of the states the lane wakes on, and it reaches the lane as an
// ordinary batch.
func TestStreamExitedWakesTheLane(t *testing.T) {
	p := streamBus(t)
	s := realWatch(t, p)
	now := time.Now().UnixMilli()
	postRow(t, p, statusRow(now-2000, "c1", "worker:feat/x#s1-1", "working"))
	postRow(t, p, statusRow(now-1000, "c1", "worker:feat/x#s1-1", "exited"))

	l := startLane(t, p, s, Request{Park: "1", Interval: "1"}, laneOpts{Timer: time.After})
	l.waitLines(1)

	batches := l.batches()
	if len(batches) != 1 {
		t.Fatalf("batches = %s, want one", l.raw())
	}
	events, _ := batches[0]["events"].([]any)
	if len(events) != 1 {
		t.Fatalf("events = %v, want the exited status alone: a `working` row is not a wake", batches[0])
	}
	event, _ := events[0].(map[string]any)
	body, _ := event["body"].(map[string]any)
	if body["state"] != "exited" {
		t.Errorf("state = %v", body["state"])
	}
}

// Events that land while the lane is in its --coalesce sleep arrive as ONE
// batch, and it is the coalesce floor that made them one — not the inner watch's
// own interval, which coalesces a burst for free. So the second batch cannot
// arrive before the sleep it slept through has ended, which is what the elapsed
// bound below measures.
func TestStreamCoalescesBurstIntoOneBatch(t *testing.T) {
	p := streamBus(t)
	s := realWatch(t, p)
	l := startLane(t, p, s, Request{Park: "1", Interval: "1", Coalesce: "2"}, laneOpts{Timer: time.After})

	postRow(t, p, statusRow(time.Now().UnixMilli(), "c1", "worker:feat/a", "done"))
	l.waitLines(1)
	t0 := time.Now()

	// The lane is inside its --coalesce sleep now.
	for _, branch := range []string{"feat/b", "feat/c", "feat/d"} {
		postRow(t, p, statusRow(time.Now().UnixMilli(), "c1", "worker:"+branch, "done"))
	}

	l.waitLines(2)
	elapsed := time.Since(t0)
	if got := len(l.lines()); got != 2 {
		t.Fatalf("lines = %d (%s), want the burst as one batch", got, l.raw())
	}
	batch := l.batches()[1]
	events, _ := batch["events"].([]any)
	if len(events) != 3 {
		t.Errorf("second batch = %v, want the three events the burst posted", batch)
	}
	// Half a second of slack for the poll that observed the first line: a lower
	// bound never flakes upward.
	if elapsed < 1500*time.Millisecond {
		t.Errorf("the second batch arrived %s after the first, want the --coalesce 2 floor", elapsed)
	}
}

// The park is seeded from the crew's own cursor file, never by a --since the
// stream tracks: the cursor a lane starts with is the cursor the crew is at, and
// the batch's cursor is the one the file holds afterwards.
func TestStreamHonoursSeededCursorAndNeverPassesSince(t *testing.T) {
	p := streamBus(t)
	s := realWatch(t, p)
	now := time.Now().UnixMilli()
	postRow(t, p, statusRow(now-2000, "c1", "worker:feat/old", "done"))
	seed := now - 2000
	seedFile(t, p, "cursor", strconv.FormatInt(seed, 10)+"\n")

	l := startLane(t, p, s, Request{Park: "1", Interval: "1"}, laneOpts{Timer: time.After})
	postRow(t, p, statusRow(now, "c1", "worker:feat/new", "done"))
	l.waitLines(1)

	batches := l.batches()
	if len(batches) != 1 {
		t.Fatalf("batches = %s, want the one new event", l.raw())
	}
	events, _ := batches[0]["events"].([]any)
	if len(events) != 1 {
		t.Fatalf("events = %v, want the pre-seeded cursor to have suppressed the old row", batches[0])
	}
	event, _ := events[0].(map[string]any)
	if event["from"] != "worker:feat/new" {
		t.Errorf("from = %v", event["from"])
	}
	// The cursor the batch names is the one the crew's file holds afterwards:
	// the park advanced it, and the lane is the reason a reader can trust it.
	after := strings.TrimSpace(readFile(cdirFile(p, "cursor")))
	if got := fmt.Sprintf("%.0f", batches[0]["cursor"]); got != after {
		t.Errorf("batch cursor = %s, cursor file = %s, want the two to agree", got, after)
	}
	if after == strconv.FormatInt(seed, 10) {
		t.Errorf("the cursor file never advanced past the seeded %d", seed)
	}

	for _, argv := range s.argv() {
		if strings.Contains(argv, "--since") {
			t.Errorf("the lane re-entered watch with a cursor of its own: %q", argv)
		}
	}
}
