package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// errWriter is a stdout that has nowhere to go: the CLI stand-in for the arm's
// `>/dev/full`, a full device or a reader that left the pipe.
type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, errors.New("no space left on device") }

// The arm printed with `printf` under `set -e`, so a write that failed ended it
// before `_await_record`. At the CLI that ordering is main.go's: inbox.Run gets
// the bufio writer and its own Flush, and the exit 1 must reach the caller with
// no marks file left behind, or the msgs nobody received are skipped for good.
func TestRunInboxAFailedPrintExitsOneAndWritesNoMarks(t *testing.T) {
	const me = "worker:feat/x#s1-1"
	log := `{"ts":1785951264000,"crew_id":"c1","kind":"msg","from":"dispatcher:c1","to":"` + me + `","body":"answer"}` + "\n"
	repo, _ := repoWithLog(t, &log, 0o644)
	t.Setenv("CREW_ID", "c1")
	e := env{getwd: func() (string, error) { return repo, nil }}

	var stderr bytes.Buffer
	if code := run(context.Background(), []string{"inbox", me, "c1"}, errWriter{}, &stderr, e); code != exitFailure {
		t.Fatalf("code %d, want %d", code, exitFailure)
	}
	if want := "crew: no space left on device\n"; stderr.String() != want {
		t.Fatalf("stderr = %q, want the flush failure %q", stderr.String(), want)
	}
	// The msgs stay undelivered: the next read hands them out again.
	if _, err := os.Stat(filepath.Join(repo, ".git", "crew", "await")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("await dir = %v, want it never created", err)
	}
}
