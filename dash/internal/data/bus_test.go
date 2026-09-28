package data

import (
	"context"
	"testing"
)

func TestEventsPath(t *testing.T) {
	r := &FakeRunner{Responses: map[string]FakeResponse{
		"git rev-parse --path-format=absolute --git-common-dir": {Stdout: []byte("/repo/.git\n")},
	}}
	path, err := EventsPath(context.Background(), r, "git")
	if err != nil {
		t.Fatal(err)
	}
	if path != "/repo/.git/crew/events.jsonl" {
		t.Errorf("EventsPath = %q, want /repo/.git/crew/events.jsonl", path)
	}
}

// TestRecentEventsFiltersExactAndPrefix guards the plan's exact contract:
// RecentEvents(path, "feat/x", 20) returns only events whose from is exactly
// "worker:feat/x" or starts with "worker:feat/x#" — "worker:feat/x-y#s1"
// must NOT match — oldest-first, tolerating a torn last line.
func TestRecentEventsFiltersExactAndPrefix(t *testing.T) {
	events, err := RecentEvents("testdata/events.jsonl", "feat/x", 20)
	if err != nil {
		t.Fatal(err)
	}
	// events.jsonl: line 1 has from=null (dispatch row), line 2 from
	// worker:feat/x#s1 (match), line 3 from worker:feat/x-y#s1 (must not
	// match), line 4 from worker:feat/x#s1 (match), line 5 from
	// worker:feat/x#s1 (match), line 6 is a torn line (tolerated, dropped).
	if len(events) != 3 {
		t.Fatalf("expected 3 matching events, got %d: %+v", len(events), events)
	}
	wantTs := []float64{1100, 1300, 1400}
	for i, want := range wantTs {
		ts, _ := events[i]["ts"].(float64)
		if ts != want {
			t.Errorf("event %d ts = %v, want %v (order must be oldest-first)", i, ts, want)
		}
	}
}

func TestRecentEventsCapsToN(t *testing.T) {
	events, err := RecentEvents("testdata/events.jsonl", "feat/x", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events (capped), got %d", len(events))
	}
	// The last 2 of the 3 matches, still oldest-first: 1300 then 1400.
	ts0, _ := events[0]["ts"].(float64)
	ts1, _ := events[1]["ts"].(float64)
	if ts0 != 1300 || ts1 != 1400 {
		t.Errorf("events = %+v, want ts 1300 then 1400", events)
	}
}
