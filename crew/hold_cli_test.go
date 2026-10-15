package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// holdCLI runs argv through the same run() the binary does, against a repo
// whose bus starts absent.
type holdCLI struct {
	t    *testing.T
	repo string
	log  string
}

func newHoldCLI(t *testing.T) *holdCLI {
	t.Helper()
	// A worker or role session may not post as the dispatcher.
	for _, k := range []string{"CREW_WORKER_ID", "CREW_ROLE_ID", "CREW_CLOCK"} {
		t.Setenv(k, "")
		if err := os.Unsetenv(k); err != nil {
			t.Fatal(err)
		}
	}
	repo, log := repoWithLog(t, nil, 0)
	return &holdCLI{t: t, repo: repo, log: log}
}

func (h *holdCLI) run(args ...string) (string, string, int) {
	h.t.Helper()
	e := env{getwd: func() (string, error) { return h.repo, nil }}
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), args, &stdout, &stderr, e)
	return stdout.String(), stderr.String(), code
}

func (h *holdCLI) add(title string) string {
	h.t.Helper()
	out, errOut, code := h.run("hold", "add", "--engine", "claude", "--window", "5h",
		"--resets-at", fmt.Sprint(time.Now().Unix()+100), "--agent", "codex", "--ref", "r1",
		"--branch", "b1", "--tier", "standard", "--model", "sonnet", "--effort", "medium",
		"--crew", "c1", title)
	if code != 0 {
		h.t.Fatalf("hold add: code %d, stderr %q", code, errOut)
	}
	return strings.TrimSpace(out)
}

// A hold is addressed `hold:<crew>`: it wakes no watch and is no one's inbox
// message, but the log still shows it.
func TestHoldIsQuietOnWatchAndInboxButInTheLog(t *testing.T) {
	h := newHoldCLI(t)
	id := h.add("quiet hold")

	out, errOut, code := h.run("watch", "--crew", "c1", "--timeout", "1", "--interval", "1")
	if code != 0 || out != "" {
		t.Errorf("watch: code %d, stdout %q, stderr %q", code, out, errOut)
	}
	out, errOut, code = h.run("inbox", "dispatcher:c1", "c1")
	if code != 0 || out != "" {
		t.Errorf("inbox: code %d, stdout %q, stderr %q", code, out, errOut)
	}
	out, errOut, code = h.run("log", "c1")
	if code != 0 || !strings.Contains(out, id) {
		t.Errorf("log: code %d, stdout %q, stderr %q, want it to name %s", code, out, errOut, id)
	}
}

// `status`, `msg` and the hold arms all append through the one bus writer, so
// the log stays one JSON object per line however they interleave.
func TestHoldRowsInterleaveWithBusWritersIntoAllJSON(t *testing.T) {
	h := newHoldCLI(t)
	t.Setenv("CREW_ID", "c1")

	mustRun := func(args ...string) string {
		t.Helper()
		out, errOut, code := h.run(args...)
		if code != 0 {
			t.Fatalf("%v: code %d, stderr %q", args, code, errOut)
		}
		return out
	}
	mustRun("status", "worker:feat/x", "working", "start")
	mustRun("msg", "dispatcher:c1", "worker:feat/x", "question")
	id := h.add("parked after bus rows")
	mustRun("hold", "release", id, "--crew", "c1")
	mustRun("status", "worker:feat/y", "blocked", "later")
	h.add("parked after its own release")

	data, err := os.ReadFile(h.log)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	for i, line := range lines {
		if !json.Valid([]byte(line)) {
			t.Errorf("line %d is not JSON: %s", i+1, line)
		}
	}
	if len(lines) != 6 {
		t.Fatalf("%d lines, want 6:\n%s", len(lines), data)
	}

	var listed []struct {
		Task struct {
			Title string `json:"title"`
		} `json:"task"`
	}
	if err := json.Unmarshal([]byte(mustRun("hold", "list", "--json", "--crew", "c1")), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].Task.Title != "parked after its own release" {
		t.Errorf("list folded to %+v", listed)
	}
}

// With CREW_CLOCK unset, `await` really waits and the hold arms read the real
// clock.
func TestUnsetCrewClockUsesRealTime(t *testing.T) {
	h := newHoldCLI(t)
	t.Setenv("CREW_ID", "c1")

	start := time.Now()
	if _, errOut, code := h.run("await", "worker:feat/x#s1-1", "--timeout", "1", "--interval", "1"); code != 0 {
		t.Fatalf("await: code %d, stderr %q", code, errOut)
	}
	if elapsed := time.Since(start); elapsed < 900*time.Millisecond {
		t.Errorf("await returned after %v, want about 1s of real time", elapsed)
	}

	h.add("matures in real time")
	if _, _, code := h.run("hold", "due", "--crew", "c1"); code != 1 {
		t.Errorf("due on an unmatured hold: code %d, want 1", code)
	}
	out, _, code := h.run("hold", "park", "1000", "--crew", "c1")
	if !regexp.MustCompile(`^(\d+)\n$`).MatchString(out) || code != 0 {
		t.Fatalf("park: code %d, stdout %q", code, out)
	}
	n, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		t.Fatal(err)
	}
	if n < 1 || n > 100 {
		t.Errorf("park printed %d, want 1..100", n)
	}
}
