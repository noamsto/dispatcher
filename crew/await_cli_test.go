package main

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
)

// The straggler fold (`crew inbox --since`) is how a worker takes a reply that
// missed a timed-out await, and taking it counts as delivering it: the marks
// inbox raises are the ones await reads, so the next await hands out nothing.
func TestRunAwaitAfterTheInboxFold(t *testing.T) {
	const me = "worker:feat/x#s1-1"
	const spec = "role:feat/x:spec-critic"
	verdict := `{"ts":1000,"crew_id":"c1","kind":"msg","from":"` + spec + `","to":"` + me + `","body":"spec verdict"}`
	repo, logPath := repoWithLog(t, &verdict, 0o644)
	t.Setenv("CREW_ID", "c1")
	unsetenv(t, "CREW_CLOCK")
	e := env{getwd: func() (string, error) { return repo, nil }}
	cli := func(argv ...string) (string, string, int) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		code := run(context.Background(), argv, &stdout, &stderr, e)
		return stdout.String(), stderr.String(), code
	}

	out, stderr, code := cli("inbox", me, "c1", "--since", "0")
	if code != 0 || !sameRow(t, out, verdict) {
		t.Fatalf("inbox: code %d stdout %q stderr %q", code, out, stderr)
	}

	// await reads that mark: exit 0, empty stdout, only the timeout note.
	out, stderr, code = cli("await", me, "--timeout", "0")
	if code != 0 || out != "" {
		t.Fatalf("await after the fold: code %d stdout %q", code, out)
	}
	if want := "no reply to " + me + " yet\n"; !strings.HasSuffix(stderr, want) {
		t.Errorf("stderr = %q, want the %q note", stderr, want)
	}

	// A later question to that sender does not re-open the verdict it took.
	ask := `{"ts":2000,"crew_id":"c1","kind":"msg","from":"` + me + `","to":"` + spec + `","body":"review again"}`
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(ask + "\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	out, _, code = cli("await", me, "--timeout", "0")
	if code != 0 || out != "" {
		t.Fatalf("await after a later question: code %d stdout %q", code, out)
	}
}
