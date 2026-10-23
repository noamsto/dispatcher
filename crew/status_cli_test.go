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

// A repo-keyed subcommand carries its full argv into the arm, but the repo
// check runs first: from outside a git repo `status worker working` refuses
// with the one line, exit 1, stdout empty.
func TestStatusRepoKeyedRefusalOutsideRepo(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GIT_CEILING_DIRECTORIES", dir)
	t.Setenv("CREW_ID", "c1")
	e := env{getwd: func() (string, error) { return dir, nil }}
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"status", "worker", "working"}, &stdout, &stderr, e)
	if code != exitFailure || stdout.Len() != 0 || stderr.String() != "crew: not in a git repo\n" {
		t.Fatalf("code %d stdout %q stderr %q", code, stdout.String(), stderr.String())
	}
}

// With CREW_ID unset the status arm resolves the crew through bus.CrewID, so
// the row it appends carries the WORKER_TASK.md crew_id, not an empty one.
func TestStatusRowCrewIDFromTaskDoc(t *testing.T) {
	for _, k := range []string{"CREW_WORKER_ID", "CREW_ROLE_ID", "CREW_CLOCK"} {
		t.Setenv(k, "")
		if err := os.Unsetenv(k); err != nil {
			t.Fatal(err)
		}
	}
	repo, logPath := repoWithLog(t, nil, 0) // CREW_ID unset, as after `CREW_ID=`
	if err := os.WriteFile(filepath.Join(repo, "WORKER_TASK.md"), []byte("crew_id: c-from-file\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e := env{getwd: func() (string, error) { return repo, nil }}
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"status", "worker", "working"}, &stdout, &stderr, e); code != 0 {
		t.Fatalf("code %d, stderr %q", code, stderr.String())
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("%d rows, want 1:\n%s", len(lines), data)
	}
	var row struct {
		CrewID string `json:"crew_id"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &row); err != nil {
		t.Fatalf("row does not parse: %v\n%s", err, lines[0])
	}
	if row.CrewID != "c-from-file" {
		t.Errorf("crew_id = %q, want c-from-file", row.CrewID)
	}
}
