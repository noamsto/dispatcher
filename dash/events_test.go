package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/noamsto/dispatcher/dash/internal/view"
)

func TestParseArgsEvents(t *testing.T) {
	o, err := parseArgs([]string{"--json", "--view", "events", "--since", "1:2", "--limit", "5"})
	if err != nil || o.since != "1:2" || o.limit != 5 {
		t.Fatalf("o=%+v err=%v", o, err)
	}
	for _, args := range [][]string{
		{"--json", "--view", "overview", "--since", "1:2"},
		{"--json", "--view", "events", "--watch", "--limit", "3"},
		{"--json", "--view", "events", "--limit", "0"},
	} {
		if _, err := parseArgs(args); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
}

func TestEventsCollectorWatchSkipsEmpty(t *testing.T) {
	p := filepath.Join(t.TempDir(), "events.jsonl")
	if err := os.WriteFile(p, []byte(`{"ts":1,"kind":"a"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	collect := eventsCollector(p, nil, opts{watch: true})
	v, err := collect(context.Background())
	if err != nil || v.(view.EventsView).Cursor == "" || len(v.(view.EventsView).Events) != 1 {
		t.Fatalf("first = %+v, %v", v, err)
	}
	if v, _ := collect(context.Background()); v != nil {
		t.Errorf("empty watch page emitted: %+v", v)
	}
	f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o644)
	_, _ = f.WriteString(`{"ts":2,"kind":"b"}` + "\n")
	_ = f.Close()
	v, _ = collect(context.Background())
	if ev := v.(view.EventsView).Events; len(ev) != 1 || ev[0].Kind != "b" {
		t.Errorf("second = %+v", v)
	}
}
