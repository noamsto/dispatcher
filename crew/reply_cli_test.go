package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/proc/proctest"
)

// msgRow is a bus msg read back from the log or from `crew inbox`. `ts` stays the
// number the clock stamped: the row's byte-exact construction is pinned in
// internal/reply.
type msgRow struct {
	TS     json.Number `json:"ts"`
	CrewID string      `json:"crew_id"`
	From   string      `json:"from"`
	To     string      `json:"to"`
	Kind   string      `json:"kind"`
	Body   string      `json:"body"`
}

// decodeMsgRow is one bus line, value-parsed.
func decodeMsgRow(t *testing.T, line string) msgRow {
	t.Helper()
	var r msgRow
	if err := json.Unmarshal([]byte(line), &r); err != nil {
		t.Fatalf("row %q: %v", line, err)
	}
	return r
}

// busLines is the log's lines, oldest first.
func busLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimRight(string(data), "\n"), "\n")
}

// The `--crew` anchor at the CLI: with CREW_ID unset it names the crew the reply
// posts as, and the branch resolves inside that crew rather than across every
// crew the bus carries (#302).
func TestReplyCrewFlagWithNoAmbientCrew(t *testing.T) {
	log := `{"ts":1785951000000,"crew_id":"c1","kind":"status","from":"worker:feat/x#s1-1","body":{"state":"working"}}` + "\n" +
		`{"ts":1785951100000,"crew_id":"c2","kind":"status","from":"worker:feat/x#s2-2","body":{"state":"working"}}` + "\n"
	repo, logPath := repoWithLog(t, &log, 0o644)
	unsetenv(t, "CREW_ID", "CREW_WORKER_ID", "CREW_ROLE_ID")
	e := env{getwd: func() (string, error) { return repo, nil }, proc: proctest.New(nil, nil)}

	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"reply", "worker:feat/x", "go", "--crew", "c2"}, &stdout, &stderr, e); code != 0 {
		t.Fatalf("code %d, stdout %q stderr %q", code, stdout.String(), stderr.String())
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("a reply that landed printed stdout %q stderr %q", stdout.String(), stderr.String())
	}

	lines := busLines(t, logPath)
	if len(lines) != 3 {
		t.Fatalf("%d bus lines, want the two seeded ones and the reply", len(lines))
	}
	got := decodeMsgRow(t, lines[2])
	if got.CrewID != "c2" || got.From != "dispatcher:c2" || got.To != "worker:feat/x#s2-2" || got.Kind != "msg" || got.Body != "go" {
		t.Fatalf("row = %+v, want c2's reply to session 2", got)
	}
	if _, err := got.TS.Float64(); err != nil {
		t.Fatalf("ts %q is not the number the clock stamped", got.TS)
	}
}

// A branch-only reply is addressed to the session that was live when it was
// sent, and only that session reads it: the successor's inbox stays empty, which
// is what makes a directive uninheritable (#17).
func TestReplyDeliversOnlyToTheSessionItResolved(t *testing.T) {
	repo, _ := repoWithLog(t, nil, 0)
	t.Setenv("CREW_ID", "c1")
	unsetenv(t, "CREW_WORKER_ID", "CREW_ROLE_ID")
	e := env{getwd: func() (string, error) { return repo, nil }, proc: proctest.New(nil, nil)}

	must := func(argv ...string) string {
		t.Helper()
		var stdout, stderr bytes.Buffer
		if code := run(context.Background(), argv, &stdout, &stderr, e); code != 0 {
			t.Fatalf("%v: code %d, stdout %q stderr %q", argv, code, stdout.String(), stderr.String())
		}
		return stdout.String()
	}

	must("status", "worker:feat/x#s1-1", "working")
	must("reply", "worker:feat/x", "STOP - do not push")
	must("status", "worker:feat/x#s1-1", "done")
	must("status", "worker:feat/x#s2-2", "working")

	if out := must("inbox", "worker:feat/x#s2-2", "c1"); out != "" {
		t.Fatalf("session 2's inbox holds %q, want nothing", out)
	}
	out := must("inbox", "worker:feat/x#s1-1", "c1")
	if !strings.HasSuffix(out, "\n") || strings.Count(out, "\n") != 1 {
		t.Fatalf("session 1's inbox printed %q, want one row", out)
	}
	got := decodeMsgRow(t, out)
	if got.From != "dispatcher:c1" || got.To != "worker:feat/x#s1-1" || got.Kind != "msg" || got.Body != "STOP - do not push" {
		t.Fatalf("row = %+v, want the directive addressed to session 1", got)
	}
}
