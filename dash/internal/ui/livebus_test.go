package ui

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A poll glob outside EventsPath (a layout-2 crew log) must wake the live
// Overview watcher.
func TestLiveBusWatcherPollsExtraPaths(t *testing.T) {
	if w := liveBusWatcher("", func() []string { return nil }, time.Second); w != nil {
		t.Fatalf("watcher = %v, want nil without a bus log", w)
	}
	dir := t.TempDir()
	events := filepath.Join(dir, "events.jsonl")
	if err := os.WriteFile(events, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(dir, "crews", "c1", "events.jsonl")
	if err := os.MkdirAll(filepath.Dir(log), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(log, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	w := liveBusWatcher(events, func() []string { return []string{filepath.Join(dir, "crews", "*", "events.jsonl")} }, 20*time.Millisecond)
	t.Cleanup(func() { _ = w.Close() })
	time.Sleep(100 * time.Millisecond)
	f, err := os.OpenFile(log, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{\"a\":1}\n"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	select {
	case <-w.Events():
	case <-time.After(3 * time.Second):
		t.Fatal("no change event after a layout-2 append")
	}
}
