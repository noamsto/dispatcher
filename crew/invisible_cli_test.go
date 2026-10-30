package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/clock"
	"github.com/noamsto/dispatcher/crew/internal/proc/proctest"
	"github.com/noamsto/dispatcher/crew/internal/tmux/tmuxtest"
)

const (
	tagA      = "\U000E0041"
	zwsp      = "\u200B"
	vs1       = "\uFE00"
	rawText   = "hi" + tagA + "x" + zwsp + "y a" + vs1
	escText   = `hi\u{E0041}x\u{200B}y a\u{FE00}`
	invWorker = "worker:feat/x#s1-1"
)

// invisibleBus posts a msg to a worker, a msg to the dispatcher and a blocked
// status through the CLI, so the stored rows are whatever the write path made.
func invisibleBus(t *testing.T) (repo, logPath string) {
	t.Helper()
	repo, logPath = unanchoredBus(t)
	for _, argv := range [][]string{
		{"msg", "dispatcher:c1", invWorker, rawText},
		{"msg", invWorker, "dispatcher:c1", rawText},
		{"status", invWorker, "blocked", rawText},
	} {
		if _, stderr, code := runCrew(t, repo, argv...); code != 0 {
			t.Fatalf("%v: code %d, stderr %q", argv, code, stderr)
		}
	}
	return repo, logPath
}

func readLog(t *testing.T, logPath string) string {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func requireStoredRaw(t *testing.T, logPath, before string) {
	t.Helper()
	after := readLog(t, logPath)
	if after != before {
		t.Errorf("the read changed the bus log:\nbefore %q\nafter  %q", before, after)
	}
	if !strings.Contains(after, tagA) || !strings.Contains(after, zwsp) || !strings.Contains(after, vs1) {
		t.Errorf("the stored rows lost the raw code points: %q", after)
	}
}

func requireEscaped(t *testing.T, stdout string, got any) {
	t.Helper()
	if got != escText {
		t.Errorf("value = %q, want %q", got, escText)
	}
	if strings.Contains(stdout, tagA) || strings.Contains(stdout, zwsp) || strings.Contains(stdout, vs1) {
		t.Errorf("stdout carries a raw invisible code point: %q", stdout)
	}
}

func TestInvisibleInboxEscapesBody(t *testing.T) {
	repo, logPath := invisibleBus(t)
	before := readLog(t, logPath)
	stdout, stderr, code := runCrew(t, repo, "inbox", invWorker, "c1")
	if code != 0 || stderr != "" {
		t.Fatalf("code %d, stderr %q", code, stderr)
	}
	var row map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &row); err != nil {
		t.Fatalf("stdout %q: %v", stdout, err)
	}
	requireEscaped(t, stdout, row["body"])
	requireStoredRaw(t, logPath, before)
}

func TestInvisibleRosterEscapesDetail(t *testing.T) {
	repo, logPath := invisibleBus(t)
	before := readLog(t, logPath)
	e := env{
		getwd:  func() (string, error) { return repo, nil },
		clock:  clock.Clock{Now: func() time.Time { return time.Unix(1700000100, 0) }},
		probes: tmuxProbes(&tmuxtest.Fake{}),
		proc:   proctest.New(nil, nil),
	}
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"roster", "c1"}, &stdout, &stderr, e); code != 0 {
		t.Fatalf("code %d, stderr %q", code, stderr.String())
	}
	var rows []map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &rows); err != nil || len(rows) != 1 {
		t.Fatalf("stdout %q: %v", stdout.String(), err)
	}
	requireEscaped(t, stdout.String(), rows[0]["detail"])
	requireStoredRaw(t, logPath, before)
}

func TestInvisibleWatchEscapesEvents(t *testing.T) {
	repo, logPath := invisibleBus(t)
	before := readLog(t, logPath)
	w := &watchCLI{t: t, repo: repo, log: logPath}
	out, stderr, code := w.watch("--crew", "c1", "--timeout", "1", "--interval", "1")
	if code != 0 || stderr != "" {
		t.Fatalf("code %d stderr %q", code, stderr)
	}
	var sawMsg, sawStatus bool
	for _, ev := range w.events(t, out) {
		switch ev["kind"] {
		case "msg":
			sawMsg = true
			requireEscaped(t, out, ev["body"])
		case "status":
			sawStatus = true
			body, _ := ev["body"].(map[string]any)
			requireEscaped(t, out, body["detail"])
		}
	}
	if !sawMsg || !sawStatus {
		t.Errorf("batch %q: want a msg and a status event", out)
	}
	requireStoredRaw(t, logPath, before)
}

// firstWrite takes one write and fails every later one, which is how a dropped
// link ends a relay.
type firstWrite struct{ got bytes.Buffer }

func (f *firstWrite) Write(p []byte) (int, error) {
	if f.got.Len() > 0 {
		return 0, os.ErrClosed
	}
	return f.got.Write(p)
}

func TestInvisibleRelayEscapesEvents(t *testing.T) {
	repo, logPath := invisibleBus(t)
	before := readLog(t, logPath)
	e := env{getwd: func() (string, error) { return repo, nil }}
	out := &firstWrite{}
	var stderr bytes.Buffer
	if code := run(context.Background(), []string{"relay", "--crew", "c1"}, out, &stderr, e); code != 0 {
		t.Fatalf("code %d, stderr %q", code, stderr.String())
	}
	var sawMsg, sawStatus bool
	for _, ev := range (&watchCLI{}).events(t, out.got.String()) {
		switch ev["kind"] {
		case "msg":
			sawMsg = true
			requireEscaped(t, out.got.String(), ev["body"])
		case "status":
			sawStatus = true
			body, _ := ev["body"].(map[string]any)
			requireEscaped(t, out.got.String(), body["detail"])
		}
	}
	if !sawMsg || !sawStatus {
		t.Errorf("batch %q: want a msg and a status event", out.got.String())
	}
	requireStoredRaw(t, logPath, before)
}

func TestInvisibleAwaitEscapesBody(t *testing.T) {
	repo, logPath := invisibleBus(t)
	before := readLog(t, logPath)
	stdout, stderr, code := runCrew(t, repo, "await", invWorker, "--timeout", "1")
	if code != 0 || stderr != "" {
		t.Fatalf("code %d, stderr %q", code, stderr)
	}
	var row map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &row); err != nil {
		t.Fatalf("stdout %q: %v", stdout, err)
	}
	requireEscaped(t, stdout, row["body"])
	requireStoredRaw(t, logPath, before)
}

func TestInvisibleLogEscapesRows(t *testing.T) {
	repo, logPath := invisibleBus(t)
	before := readLog(t, logPath)
	stdout, stderr, code := runCrew(t, repo, "log", "c1")
	if code != 0 || stderr != "" {
		t.Fatalf("code %d, stderr %q", code, stderr)
	}
	var sawMsg, sawStatus bool
	for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("row %q: %v", line, err)
		}
		switch row["kind"] {
		case "msg":
			sawMsg = true
			requireEscaped(t, stdout, row["body"])
		case "status":
			sawStatus = true
			body, _ := row["body"].(map[string]any)
			requireEscaped(t, stdout, body["detail"])
		}
	}
	if !sawMsg || !sawStatus {
		t.Errorf("stdout %q: want a msg and a status row", stdout)
	}
	requireStoredRaw(t, logPath, before)
}

func TestInvisibleSessionsEscapesState(t *testing.T) {
	repo, logPath := invisibleBus(t)
	// The CLI refuses an unknown state, so a hand-written row stands in for
	// whatever a foreign writer could append.
	row, err := json.Marshal(map[string]any{
		"ts": time.Now().Add(time.Hour).UnixMilli(), "crew_id": "c1", "from": invWorker,
		"to": "dispatcher:c1", "kind": "status", "body": map[string]any{"state": rawText},
	})
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(append(row, '\n')); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	before := readLog(t, logPath)
	stdout, stderr, code := runCrew(t, repo, "sessions", "feat/x", "--crew", "c1")
	if code != 0 || stderr != "" {
		t.Fatalf("code %d, stderr %q", code, stderr)
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &rows); err != nil || len(rows) != 1 {
		t.Fatalf("stdout %q: %v", stdout, err)
	}
	requireEscaped(t, stdout, rows[0]["state"])
	requireStoredRaw(t, logPath, before)
}
