package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// watchCLI runs argv through the same run() the binary does, against a repo
// whose bus and WORKER_TASK.md are the ones the test wrote. `crew watch` reads
// its crew through bus.CrewID, so `--crew` resolution is this level's, not the
// watch package's: there the resolver is a stub.
type watchCLI struct {
	t    *testing.T
	repo string
	log  string
}

func newWatchCLI(t *testing.T, rows string) *watchCLI {
	t.Helper()
	repo, logPath := repoWithLog(t, &rows, 0o644)
	return &watchCLI{t: t, repo: repo, log: logPath}
}

func (w *watchCLI) taskDoc(text string) {
	w.t.Helper()
	if err := os.WriteFile(filepath.Join(w.repo, "WORKER_TASK.md"), []byte(text), 0o644); err != nil {
		w.t.Fatal(err)
	}
}

// watch runs one park and returns its stdout, stderr and status. Every row
// these tests seed qualifies on the first fold, so no park sleeps.
func (w *watchCLI) watch(args ...string) (string, string, int) {
	w.t.Helper()
	e := env{getwd: func() (string, error) { return w.repo, nil }}
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), append([]string{"watch"}, args...), &stdout, &stderr, e)
	return stdout.String(), stderr.String(), code
}

func (w *watchCLI) events(t *testing.T, out string) []map[string]any {
	t.Helper()
	var batch struct {
		Cursor int64            `json:"cursor"`
		Events []map[string]any `json:"events"`
	}
	if err := json.Unmarshal([]byte(out), &batch); err != nil {
		t.Fatalf("batch %q: %v", out, err)
	}
	return batch.Events
}

// `--crew` is the whole crew answer when nothing else names one: CREW_ID unset
// (repoWithLog clears it) and no WORKER_TASK.md at the repo top.
func TestWatchCLIResolvesCrewFromFlagAlone(t *testing.T) {
	w := newWatchCLI(t, `{"ts":1785951264000,"crew_id":"c1","from":"worker:feat/x","to":"dispatcher:c1","kind":"status","body":{"state":"done"}}`+"\n")

	out, stderr, code := w.watch("--crew", "c1", "--timeout", "1", "--interval", "1")
	if code != 0 || stderr != "" {
		t.Fatalf("code %d stderr %q", code, stderr)
	}
	events := w.events(t, out)
	if len(events) != 1 || events[0]["crew_id"] != "c1" {
		t.Errorf("events = %v, want the one c1 row", events)
	}
}

// An explicit `--crew` beats the checkout's own crew, and dropping it hands the
// WORKER_TASK.md crew back: each call delivers only its own crew's rows.
func TestWatchCLICrewFlagBeatsTaskDoc(t *testing.T) {
	const (
		c1Row = `{"ts":1785951264000,"crew_id":"c1","from":"worker:feat/x","to":"dispatcher:c1","kind":"status","body":{"state":"done"}}`
		c9Row = `{"ts":1785951264001,"crew_id":"c9","from":"worker:feat/y","to":"dispatcher:c9","kind":"status","body":{"state":"done"}}`
	)
	w := newWatchCLI(t, c1Row+"\n"+c9Row+"\n")
	w.taskDoc("crew_id: c9\n")

	out, stderr, code := w.watch("--crew", "c1", "--timeout", "1", "--interval", "1")
	if code != 0 || stderr != "" {
		t.Fatalf("--crew c1: code %d stderr %q", code, stderr)
	}
	if events := w.events(t, out); len(events) != 1 || events[0]["crew_id"] != "c1" {
		t.Errorf("--crew c1 events = %v, want only the c1 row", events)
	}

	out, stderr, code = w.watch("--timeout", "1", "--interval", "1")
	if code != 0 || stderr != "" {
		t.Fatalf("no flag: code %d stderr %q", code, stderr)
	}
	if events := w.events(t, out); len(events) != 1 || events[0]["crew_id"] != "c9" {
		t.Errorf("no flag events = %v, want only the c9 row the task doc names", events)
	}
}
