package entry

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/bus"
)

// TestID is the arm's whole contract: the task document wins over a disagreeing
// environment, the environment answers when there is no document, and neither
// prints the refusal. bus.CrewID is the resolver the arm had, so the fixture is
// a real repo rather than a stub of it.
func TestID(t *testing.T) {
	repo := gitRepo(t)
	run := func() (string, string, int) {
		var stdout, stderr bytes.Buffer
		code := RunID(&stdout, &stderr, IDOptions{
			CrewID: func() string { return bus.CrewID(context.Background(), repo) },
		})
		return stdout.String(), stderr.String(), code
	}

	t.Run("the task document wins over the environment", func(t *testing.T) {
		write(t, filepath.Join(repo, "WORKER_TASK.md"), "tier: standard\ncrew_id: c-from-task\n")
		t.Setenv("CREW_ID", "c-from-env")
		stdout, stderr, code := run()
		if code != 0 || stdout != "c-from-task\n" || stderr != "" {
			t.Fatalf("id = %d, %q, stderr %q", code, stdout, stderr)
		}
	})

	t.Run("a task document with no crew_id falls back to the environment", func(t *testing.T) {
		write(t, filepath.Join(repo, "WORKER_TASK.md"), "tier: standard\n")
		t.Setenv("CREW_ID", "c-from-env")
		stdout, _, code := run()
		if code != 0 || stdout != "c-from-env\n" {
			t.Fatalf("id = %d, %q", code, stdout)
		}
	})

	t.Run("the environment answers with no task document", func(t *testing.T) {
		if err := os.Remove(filepath.Join(repo, "WORKER_TASK.md")); err != nil {
			t.Fatal(err)
		}
		t.Setenv("CREW_ID", "c-from-env")
		stdout, _, code := run()
		if code != 0 || stdout != "c-from-env\n" {
			t.Fatalf("id = %d, %q", code, stdout)
		}
	})

	t.Run("nothing names an id", func(t *testing.T) {
		t.Setenv("CREW_ID", "")
		stdout, stderr, code := run()
		if code != 1 {
			t.Fatalf("id exit %d, want 1", code)
		}
		if stdout != "" {
			t.Errorf("id wrote %q to stdout", stdout)
		}
		if stderr != noCrewID {
			t.Errorf("id refusal = %q, want the arm's line", stderr)
		}
	})
}

// TestIDRefusalIsTheArmsLine pins the whole sentence, recovery hints included:
// it is the text a session reads and acts on, so it is ported verbatim.
func TestIDRefusalIsTheArmsLine(t *testing.T) {
	const want = "crew: no crew id — CREW_ID unset and no WORKER_TASK.md crew_id; " +
		"run 'crew crews' to find this repo's crews, 'crew adopt <id>' to re-attach, " +
		"or 'crew new' to start one\n"
	if noCrewID != want {
		t.Fatalf("noCrewID = %q", noCrewID)
	}
	var stdout, stderr bytes.Buffer
	if code := RunID(&stdout, &stderr, IDOptions{CrewID: func() string { return "" }}); code != 1 {
		t.Fatalf("code %d, want 1", code)
	}
	if stdout.Len() != 0 || stderr.String() != want {
		t.Fatalf("stdout %q, stderr %q", stdout.String(), stderr.String())
	}
}

// TestIDPrintsOneLine: the arm captured the id and printed it, which is what
// made `crew id` pipeable — one line, no padding, no trailing space.
func TestIDPrintsOneLine(t *testing.T) {
	var stdout bytes.Buffer
	if code := RunID(&stdout, &bytes.Buffer{}, IDOptions{CrewID: func() string { return "1720800000-12345" }}); code != 0 {
		t.Fatalf("code %d, want 0", code)
	}
	if stdout.String() != "1720800000-12345\n" {
		t.Fatalf("stdout %q, want the id and one newline", stdout.String())
	}
}
