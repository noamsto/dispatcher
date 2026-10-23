package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
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

// The blocked→await loop's own commands, in the order it runs them at the CLI:
// a cycle ends on `crew await`'s timeout marker, the straggler fold takes a
// reply that missed its window, and the budget exhaustion ends the loop.
// `--timeout 0` is the instant timeout: the due read comes before the deadline
// check, so a seeded reply is delivered and an empty bus times out at once.

// blockedCycleBus is a repo bus with no ambient session id and no tmux to anchor
// to, so the worker posts as whatever id the test names.
func blockedCycleBus(t *testing.T) (repo, logPath string) {
	t.Helper()
	repo, logPath = unanchoredBus(t)
	unsetenv(t, "CREW_CLOCK")
	return repo, logPath
}

// mustStatus runs `crew status` and requires it wrote its row.
func mustStatus(t *testing.T, repo string, argv ...string) {
	t.Helper()
	if out, errOut, code := runCrew(t, repo, append([]string{"status"}, argv...)...); code != 0 {
		t.Fatalf("status %v: code %d, stdout %q, stderr %q", argv, code, out, errOut)
	}
}

// seedReply appends the dispatcher's answer straight to the bus, as a row no
// `crew msg` wrote.
func seedReply(t *testing.T, logPath, to string) string {
	t.Helper()
	row := `{"ts":1785951264000,"crew_id":"c1","kind":"msg","from":"dispatcher:c1","to":"` + to + `","body":"answer"}`
	(&busSeed{t: t, log: logPath}).raw(row)
	return row
}

// postedStatus is one status row as the bus holds it.
type postedStatus struct{ state, detail string }

// postedStatuses reads back the bus's status rows in log order.
func postedStatuses(t *testing.T, logPath string) []postedStatus {
	t.Helper()
	var got []postedStatus
	for _, line := range busLines(t, logPath) {
		var row struct {
			Kind string          `json:"kind"`
			Body json.RawMessage `json:"body"`
		}
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("row %q: %v", line, err)
		}
		if row.Kind != "status" {
			continue
		}
		var body struct {
			State  string `json:"state"`
			Detail string `json:"detail"`
		}
		if err := json.Unmarshal(row.Body, &body); err != nil {
			t.Fatalf("status body in %q: %v", line, err)
		}
		got = append(got, postedStatus{body.State, body.Detail})
	}
	return got
}

// detailsOf is the detail of every row in that state, in log order.
func detailsOf(rows []postedStatus, state string) []string {
	var got []string
	for _, r := range rows {
		if r.state == state {
			got = append(got, r.detail)
		}
	}
	return got
}

func TestBlockedCycleReplyInALaterCycleIsDeliveredInBand(t *testing.T) {
	const me = "worker:feat/x#s1-1"
	repo, logPath := blockedCycleBus(t)
	mustStatus(t, repo, me, "working")
	mustStatus(t, repo, me, "blocked", "why?")

	// Cycle 1: nothing is owed yet, so the window closes on its marker. That is
	// the marker, not a failure: exit 0, empty stdout, the note on stderr.
	out, errOut, code := runCrew(t, repo, "await", me, "--timeout", "0")
	if code != 0 || out != "" {
		t.Fatalf("cycle 1: code %d, stdout %q", code, out)
	}
	if want := "crew: await ended after 0s — no reply to " + me + " yet\n"; errOut != want {
		t.Errorf("cycle 1 stderr = %q, want %q", errOut, want)
	}
	mustStatus(t, repo, me, "blocked", "why? (cycle 1 of 24)")

	// Cycle 2: the answer is unread and the timed-out cycle marked nothing, so
	// this window hands it out on its first read.
	reply := seedReply(t, logPath, me)
	out, errOut, code = runCrew(t, repo, "await", me, "--timeout", "0")
	if code != 0 || !sameRow(t, out, reply) {
		t.Fatalf("cycle 2: code %d, stdout %q, stderr %q", code, out, errOut)
	}
	mustStatus(t, repo, me, "working", "resumed")

	// A cycle that timed out is not an ending: the loop never posted one.
	if failed := detailsOf(postedStatuses(t, logPath), "failed"); len(failed) != 0 {
		t.Errorf("failed rows = %v, want none", failed)
	}
}

func TestBlockedCycleStragglerFoldTakesAReplyThatMissedTheAwait(t *testing.T) {
	const me = "worker:feat/x#s1-1"
	repo, logPath := blockedCycleBus(t)
	mustStatus(t, repo, me, "working")
	mustStatus(t, repo, me, "blocked", "why?")
	if out, _, code := runCrew(t, repo, "await", me, "--timeout", "0"); code != 0 || out != "" {
		t.Fatalf("cycle 1: code %d, stdout %q", code, out)
	}

	// The answer lands after that window closed, and it is still owed: the fold —
	// `crew inbox --since <seen>` — prints it.
	reply := seedReply(t, logPath, me)
	out, errOut, code := runCrew(t, repo, "inbox", me, "c1", "--since", "0")
	if code != 0 || !sameRow(t, out, reply) {
		t.Fatalf("fold: code %d, stdout %q, stderr %q", code, out, errOut)
	}
	mustStatus(t, repo, me, "working", "resumed")

	if failed := detailsOf(postedStatuses(t, logPath), "failed"); len(failed) != 0 {
		t.Errorf("failed rows = %v, want none", failed)
	}
}

func TestBlockedCycleBudgetExhaustionFailsExactlyOnce(t *testing.T) {
	const me = "worker:feat/x#s1-1"
	repo, logPath := blockedCycleBus(t)
	mustStatus(t, repo, me, "working")
	mustStatus(t, repo, me, "blocked", "why?")

	// A 3-cycle budget on a bus no dispatcher ever wrote. The 24-cycle budget is
	// the loop's prose, not the arm's: every cycle still ends on the marker, and
	// every one re-stamps blocked for the liveness signal.
	for i := 1; i <= 3; i++ {
		if out, _, code := runCrew(t, repo, "await", me, "--timeout", "0"); code != 0 || out != "" {
			t.Fatalf("cycle %d: code %d, stdout %q", i, code, out)
		}
		mustStatus(t, repo, me, "blocked", fmt.Sprintf("why? (cycle %d of 24)", i))
	}
	mustStatus(t, repo, me, "failed", "blocked, no dispatcher reply")

	rows := postedStatuses(t, logPath)
	// Every cycle re-stamped blocked for the liveness signal, and the exhaustion
	// posted exactly one failed at the end: never one per cycle, never zero.
	want := []string{"why?", "why? (cycle 1 of 24)", "why? (cycle 2 of 24)", "why? (cycle 3 of 24)"}
	if got := detailsOf(rows, "blocked"); !slices.Equal(got, want) {
		t.Errorf("blocked rows = %v, want %v", got, want)
	}
	if failed := detailsOf(rows, "failed"); !slices.Equal(failed, []string{"blocked, no dispatcher reply"}) {
		t.Errorf("failed rows = %v, want exactly one with the canonical detail", failed)
	}
}
