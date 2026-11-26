package view

import (
	"testing"

	"github.com/noamsto/dispatcher/dash/internal/data"
)

func TestBuildEventsRows(t *testing.T) {
	page := data.EventPage{Cursor: "1:10", Events: []data.RawEvent{{
		ID: "abc",
		Ev: data.Event{"ts": float64(5), "kind": "status", "from": "worker:feat/x#s1", "to": "dispatcher:c", "crew_id": "c",
			"body": map[string]any{"state": "working", "detail": "go"}},
	}}}
	v := BuildEvents("box", page, 7)
	if v.View != "events" || v.Host != "box" || v.Cursor != "1:10" || len(v.Events) != 1 {
		t.Fatalf("view = %+v", v)
	}
	e := v.Events[0]
	if e.ID != "abc" || e.Host != "box" || e.TS != 5 || e.Branch != "feat/x" || e.Text != "working go" || e.Crew != "c" {
		t.Errorf("row = %+v", e)
	}
}
