package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/bus"
)

// #1533: a msg body over the bus line is spilled to a file under the crew dir
// instead of cut, and `crew await` / `crew inbox` hand it back whole.

const (
	spillMe   = "worker:feat/x#s1-1"
	spillFrom = "role:feat/x:reviewer"
)

// spilledBody is a body that cannot fit one bus line: multi-line, non-ASCII, and
// JSON-escaped on the wire.
func spilledBody() string {
	return strings.Repeat("verdict line — ✓ \"quoted\" \\ tab\there\n", 300)
}

// deliveredRow is the one row `crew await` prints, decoded.
func deliveredRow(t *testing.T, out string) map[string]any {
	t.Helper()
	var row map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &row); err != nil {
		t.Fatalf("await output is not one JSON row: %v\n%q", err, out)
	}
	return row
}

func TestMsgSpillsALongBodyAndAwaitReturnsItWhole(t *testing.T) {
	repo, logPath := unanchoredBus(t)
	body := spilledBody()
	if len(body) < 10*1024 {
		t.Fatalf("fixture is %d bytes, want over 10 KB", len(body))
	}
	if _, stderr, code := runCrew(t, repo, "msg", spillFrom, spillMe, body); code != 0 || stderr != "" {
		t.Fatalf("msg: code %d, stderr %q", code, stderr)
	}
	lines := busLines(t, logPath)
	if len(lines) != 1 || len(lines[0]) > bus.LineMax {
		t.Fatalf("want one row within %d bytes, got %d rows (first %d bytes)", bus.LineMax, len(lines), len(lines[0]))
	}
	var onBus struct {
		Body     string `json:"body"`
		BodyFile string `json:"body_file"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &onBus); err != nil {
		t.Fatal(err)
	}
	if onBus.BodyFile == "" || len(onBus.Body) >= len(body) {
		t.Fatalf("row = body %d bytes, body_file %q; want a head and a file", len(onBus.Body), onBus.BodyFile)
	}
	crewDir := filepath.Join(filepath.Dir(logPath), "crews", "c1")
	fi, err := os.Stat(filepath.Join(crewDir, onBus.BodyFile))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("spill file: %v, mode %v; want a 0600 file", err, fi)
	}

	out, stderr, code := runCrew(t, repo, "await", spillMe, "--timeout", "0")
	if code != 0 {
		t.Fatalf("await: code %d, stderr %q", code, stderr)
	}
	row := deliveredRow(t, out)
	if row["body"] != body {
		t.Errorf("await body is %d bytes, want the original %d byte-for-byte", len(row["body"].(string)), len(body))
	}
	if _, kept := row["body_file"]; kept {
		t.Errorf("await row still carries body_file: %v", row)
	}
}

func TestInboxReturnsASpilledBodyWhole(t *testing.T) {
	repo, _ := unanchoredBus(t)
	body := spilledBody()
	if _, _, code := runCrew(t, repo, "msg", spillFrom, spillMe, body); code != 0 {
		t.Fatalf("msg: code %d", code)
	}
	out, _, code := runCrew(t, repo, "inbox", spillMe)
	if code != 0 {
		t.Fatalf("inbox: code %d", code)
	}
	if row := deliveredRow(t, out); row["body"] != body {
		t.Errorf("inbox body is %d bytes, want the original %d", len(row["body"].(string)), len(body))
	}
}

// body_file is worker-writable: a name outside the msg-bodies directory is never
// read, and the head is shown with a note.
func TestAwaitRefusesAnUnsafeBodyFile(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(secret, []byte("TOP SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, file := range map[string]string{
		"absolute":  secret,
		"dotdot":    "msg-bodies/../../../../" + secret,
		"escape":    "../events.jsonl",
		"wrong dir": "pid",
		"nested":    "msg-bodies/a/b.txt",
	} {
		t.Run(name, func(t *testing.T) {
			repo, logPath := unanchoredBus(t)
			rowJSON, _ := json.Marshal(map[string]any{
				"ts": 1000, "crew_id": "c1", "kind": "msg", "from": spillFrom, "to": spillMe,
				"body": "the head", "body_file": file,
			})
			if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
				t.Fatal(err)
			}
			f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.Write(append(rowJSON, '\n')); err != nil {
				t.Fatal(err)
			}
			_ = f.Close()

			out, _, code := runCrew(t, repo, "await", spillMe, "--timeout", "0")
			if code != 0 {
				t.Fatalf("code %d, want 0", code)
			}
			if strings.Contains(out, "TOP SECRET") {
				t.Fatalf("the file outside msg-bodies was read: %s", out)
			}
			row := deliveredRow(t, out)
			if row["body"] != "the head" || row["body_note"] == nil || row["body_file"] != file {
				t.Errorf("row = %v, want the head, a body_note and the untouched body_file", row)
			}
		})
	}
}

// A missing file degrades to the head and a note, exit 0.
func TestAwaitDegradesWhenTheBodyFileIsMissing(t *testing.T) {
	repo, logPath := unanchoredBus(t)
	if _, _, code := runCrew(t, repo, "msg", spillFrom, spillMe, spilledBody()); code != 0 {
		t.Fatalf("msg: code %d", code)
	}
	spilled, err := filepath.Glob(filepath.Join(filepath.Dir(logPath), "crews", "c1", "msg-bodies", "*"))
	if err != nil || len(spilled) != 1 {
		t.Fatalf("spill files = %v (%v), want one", spilled, err)
	}
	if err := os.Remove(spilled[0]); err != nil {
		t.Fatal(err)
	}
	out, _, code := runCrew(t, repo, "await", spillMe, "--timeout", "0")
	if code != 0 {
		t.Fatalf("code %d, want 0", code)
	}
	row := deliveredRow(t, out)
	if note, _ := row["body_note"].(string); !strings.Contains(note, "missing") {
		t.Errorf("body_note = %q, want it to say the file is missing", note)
	}
	if body, _ := row["body"].(string); body == "" || len(body) >= len(spilledBody()) {
		t.Errorf("body is %d bytes, want the head", len(body))
	}
}
