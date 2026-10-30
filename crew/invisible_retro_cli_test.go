package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// retroInvisibleBus seeds one failed run whose worker posted a retro note
// carrying rawText as its detail and, when state is non-empty, a status row
// with that state. The rows are hand-built because `crew status` rejects an
// arbitrary state.
func retroInvisibleBus(t *testing.T, state string) (repo, logPath string) {
	t.Helper()
	repo, logPath = unanchoredBus(t)
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatal(err)
	}
	s := &busSeed{t: t, log: logPath}
	s.dispatch("feat/x", 1000, nil)
	note, err := json.Marshal(map[string]string{"seam": "execute", "tag": "gate_thrash", "detail": rawText})
	if err != nil {
		t.Fatal(err)
	}
	s.msg(invWorker, "retro:c1", 1100, string(note))
	if state != "" {
		s.status(invWorker, 1200, state)
	}
	return repo, logPath
}

func TestInvisibleRetroReportJSONEscapesNotes(t *testing.T) {
	repo, logPath := retroInvisibleBus(t, "failed")
	before := readLog(t, logPath)
	stdout, stderr, code := runCrew(t, repo, "retro", "--report", "--json")
	if code != 0 {
		t.Fatalf("code %d, stderr %q", code, stderr)
	}
	var report retroReport
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("stdout %q: %v", stdout, err)
	}
	if len(report.Rows) != 1 {
		t.Fatalf("rows = %v", report.Rows)
	}
	notes, _ := report.Rows[0]["notes"].([]any)
	if len(notes) != 1 {
		t.Fatalf("notes = %v", notes)
	}
	requireEscaped(t, stdout, notes[0].(map[string]any)["detail"])
	if len(report.Tags) != 1 {
		t.Fatalf("tags = %v", report.Tags)
	}
	details, _ := report.Tags[0]["details"].([]any)
	if len(details) != 1 {
		t.Fatalf("tag details = %v", report.Tags[0])
	}
	requireEscaped(t, stdout, details[0])
	requireStoredRaw(t, logPath, before)
}

func TestInvisibleRetroBareEscapesOutcome(t *testing.T) {
	repo, logPath := retroInvisibleBus(t, rawText)
	before := readLog(t, logPath)
	stdout, stderr, code := runCrew(t, repo, "retro")
	if code != 0 {
		t.Fatalf("code %d, stderr %q", code, stderr)
	}
	lines := strings.Split(strings.TrimSuffix(stdout, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("stdout %q", stdout)
	}
	cols := strings.Split(lines[1], "\t")
	if len(cols) != 6 {
		t.Fatalf("row %q", lines[1])
	}
	requireEscaped(t, stdout, cols[4])
	requireStoredRaw(t, logPath, before)
}

func TestInvisibleRetroDraftEscapesDetail(t *testing.T) {
	repo, logPath := retroInvisibleBus(t, rawText)
	before := readLog(t, logPath)
	e := env{getwd: func() (string, error) { return repo, nil }, probes: noTouchProbes()}
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"retro", "--draft", "--crew", "c1"}, &stdout, &stderr, e); code != 0 {
		t.Fatalf("code %d, stderr %q", code, stderr.String())
	}
	var note struct{ Detail string }
	if err := json.Unmarshal(stdout.Bytes(), &note); err != nil {
		t.Fatalf("stdout %q: %v", stdout.String(), err)
	}
	if !strings.Contains(note.Detail, escText) {
		t.Errorf("detail %q, want %q in it", note.Detail, escText)
	}
	requireEscaped(t, stdout.String(), escText)
	requireStoredRaw(t, logPath, before)
}
