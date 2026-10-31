package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/proc/proctest"
)

// `crew msg` at the argv boundary: the words the caller passed, the sender gate
// (#908), the recipient shapes, and the clip that keeps a bus line one atomic
// write.

// runCrew drives one argv through the same run() the binary does.
func runCrew(t *testing.T, repo string, argv ...string) (stdout, stderr string, code int) {
	t.Helper()
	e := env{getwd: func() (string, error) { return repo, nil }, proc: proctest.New(nil, nil)}
	var out, errBuf bytes.Buffer
	code = run(context.Background(), argv, &out, &errBuf, e)
	return out.String(), errBuf.String(), code
}

// unanchoredBus is a repo bus with no ambient session id and no tmux to anchor
// the sender check, so a caller posts as whatever it names.
func unanchoredBus(t *testing.T) (repo, logPath string) {
	t.Helper()
	repo, logPath = repoWithLog(t, nil, 0)
	t.Setenv("CREW_ID", "c1")
	unsetenv(t, "CREW_WORKER_ID", "CREW_ROLE_ID")
	stubTmux(t, "", "")
	return repo, logPath
}

// logHasNoRows reports a log that holds no row at all, its absence included.
func logHasNoRows(t *testing.T, path string) bool {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return true
	}
	return len(data) == 0
}

// A caller may address a bare name, a worker id with or without its session, the
// dispatcher, the two sink ids and the broadcast.
func TestMsgAcceptsEveryValidRecipientShape(t *testing.T) {
	repo, logPath := unanchoredBus(t)
	wants := []string{"bob", "worker:feat/1", "worker:feat/1#s1-1", "dispatcher:c1", "retro:c1", "metrics:c1", "*"}
	for _, to := range wants {
		if stdout, stderr, code := runCrew(t, repo, "msg", "worker", to, "hi"); code != 0 || stdout != "" || stderr != "" {
			t.Fatalf("msg worker %s hi: code %d, stdout %q stderr %q", to, code, stdout, stderr)
		}
	}
	lines := busLines(t, logPath)
	if len(lines) != len(wants) {
		t.Fatalf("%d rows, want one per recipient", len(lines))
	}
	for i, line := range lines {
		row := decodeMsgRow(t, line)
		if row.CrewID != "c1" || row.From != "worker" || row.To != wants[i] || row.Kind != "msg" || row.Body != "hi" {
			t.Errorf("row %d = %+v, want c1's msg from worker to %s", i, row, wants[i])
		}
	}
}

// #46/F04: a shell that expanded an unset $CREW_ID into `dispatcher:$CREW_ID`
// must fail loudly, not log a row nobody reads.
func TestMsgRejectsRecipientPrefixWithEmptyID(t *testing.T) {
	repo, logPath := unanchoredBus(t)
	for _, tc := range []struct{ name, from, to string }{
		{"dispatcher:", "worker", "dispatcher:"},
		{"worker:", "dispatcher:c1", "worker:"},
		{"retro:", "worker", "retro:"},
	} {
		_, stderr, code := runCrew(t, repo, "msg", tc.from, tc.to, "hi")
		want := "crew: msg recipient '" + tc.to + "' is missing an id after the colon\n"
		if code != 1 || stderr != want {
			t.Errorf("%s: code %d, stderr %q, want 1 and %q", tc.name, code, stderr, want)
		}
	}
	if !logHasNoRows(t, logPath) {
		t.Errorf("a refused recipient logged a row: %q", busLines(t, logPath))
	}
}

// #1044: a retro body over the cap loses its detail, not its shape — the line
// stays one atomic write and the body still parses with every key.
func TestMsgOversizedJSONBodyStaysParseable(t *testing.T) {
	repo, logPath := unanchoredBus(t)
	detail := strings.Repeat("x", 6000)
	body := `{"seam":"execute","tag":"gate_thrash","detail":"` + detail + `"}`
	if _, stderr, code := runCrew(t, repo, "msg", "worker:feat/1", "retro:c1", body); code != 0 || stderr != "" {
		t.Fatalf("code %d, stderr %q", code, stderr)
	}
	lines := busLines(t, logPath)
	if len(lines) != 1 {
		t.Fatalf("%d rows, want one", len(lines))
	}
	if len(lines[0]) > bus.LineMax {
		t.Fatalf("the line is %d bytes, over the %d cap", len(lines[0]), bus.LineMax)
	}
	var clipped struct {
		Seam   string `json:"seam"`
		Tag    string `json:"tag"`
		Detail string `json:"detail"`
	}
	row := decodeMsgRow(t, lines[0])
	if err := json.Unmarshal([]byte(row.Body), &clipped); err != nil {
		t.Fatalf("the clipped body no longer parses: %v\n%s", err, row.Body)
	}
	if clipped.Seam != "execute" || clipped.Tag != "gate_thrash" {
		t.Errorf("seam/tag = %q/%q, want the keys the body carried", clipped.Seam, clipped.Tag)
	}
	if !strings.HasPrefix(clipped.Detail, "xxx") || !strings.HasSuffix(clipped.Detail, "[elided]") {
		t.Errorf("detail = %.40q…, want a cut of the original ending in the elided marker", clipped.Detail)
	}
	if len(clipped.Detail) >= len(detail) {
		t.Errorf("detail kept %d chars of %d: nothing was clipped", len(clipped.Detail), len(detail))
	}
}

// #908: the pane's stamp, not the caller's argv, names the id a stamped role pane
// posts as — `crew msg` and `crew status` refuse the others alike.
func TestMsgStampedRolePanePostsOnlyAsItsRoleID(t *testing.T) {
	repo, logPath := repoWithLog(t, nil, 0)
	t.Setenv("CREW_ID", "c1")
	unsetenv(t, "CREW_WORKER_ID", "CREW_ROLE_ID")
	// The caller runs under a pane stamped as feat/x's reviewer.
	stubTmux(t, "", fmt.Sprintf("%d\x1ffeat/x\x1freviewer\x1fc1\x1f@1\x1f%%1\n", os.Getpid()))

	const self, lead = "role:feat/x:reviewer", "worker:feat/x#s1-1"
	const reason = "this pane is stamped as role:feat/x:reviewer, the only id it posts as"
	for _, from := range []string{lead, "role:feat/x:plan-critic", "role:feat/y:reviewer"} {
		_, stderr, code := runCrew(t, repo, "msg", from, "dispatcher:c1", "hi")
		want := "crew: msg: refusing sender '" + from + "' — " + reason + "\n"
		if code != 1 || stderr != want {
			t.Errorf("msg %s: code %d, stderr %q, want 1 and %q", from, code, stderr, want)
		}
	}
	// `crew status` is refused the same way, and writes no row of its own.
	if _, stderr, code := runCrew(t, repo, "status", lead, "failed", "boom"); code != 1 ||
		stderr != "crew: status: refusing sender '"+lead+"' — "+reason+"\n" {
		t.Errorf("status %s: code %d, stderr %q", lead, code, stderr)
	}
	if !logHasNoRows(t, logPath) {
		t.Fatalf("a refused sender logged a row: %q", busLines(t, logPath))
	}

	if _, stderr, code := runCrew(t, repo, "msg", self, "dispatcher:c1", "hi"); code != 0 || stderr != "" {
		t.Fatalf("the role posting as itself: code %d, stderr %q", code, stderr)
	}
	lines := busLines(t, logPath)
	if len(lines) != 1 {
		t.Fatalf("%d rows, want the one the role posted", len(lines))
	}
	if row := decodeMsgRow(t, lines[0]); row.From != self || row.To != "dispatcher:c1" || row.Body != "hi" {
		t.Errorf("row = %+v, want %s to dispatcher:c1", row, self)
	}
}

// #908: the dispatcher posts as dispatcher:<crew> however it names its crew, and
// an anchored dispatcher pane that is stamped as nothing still posts.
func TestDispatcherStillPostsAsItsCrew(t *testing.T) {
	repo, logPath := repoWithLog(t, nil, 0)
	const to = "worker:feat/x#s1-1"
	t.Setenv("CREW_ID", "c1")
	unsetenv(t, "CREW_WORKER_ID", "CREW_ROLE_ID")

	for _, argv := range [][]string{
		{"msg", "dispatcher:c1", to, "go"},
		{"reply", to, "go"},
	} {
		if _, stderr, code := runCrew(t, repo, argv...); code != 0 || stderr != "" {
			t.Fatalf("%v: code %d, stderr %q", argv, code, stderr)
		}
	}
	unsetenv(t, "CREW_ID")
	if _, stderr, code := runCrew(t, repo, "reply", to, "go", "--crew", "c1"); code != 0 || stderr != "" {
		t.Fatalf("reply --crew: code %d, stderr %q", code, stderr)
	}
	if n := len(busLines(t, logPath)); n != 3 {
		t.Fatalf("%d rows, want 3", n)
	}

	stubTmux(t, "", fmt.Sprintf("%d\x1f\x1f\x1f\x1f@1\x1f%%1\n", os.Getpid()))
	t.Setenv("CREW_ID", "c1")
	for _, argv := range [][]string{
		{"msg", "dispatcher:c1", to, "go"},
		{"reply", to, "go"},
	} {
		if _, stderr, code := runCrew(t, repo, argv...); code != 0 || stderr != "" {
			t.Fatalf("anchored %v: code %d, stderr %q", argv, code, stderr)
		}
	}
	lines := busLines(t, logPath)
	if len(lines) != 5 {
		t.Fatalf("%d rows, want 5", len(lines))
	}
	for _, line := range lines {
		if row := decodeMsgRow(t, line); row.From != "dispatcher:c1" {
			t.Errorf("row from %q, want dispatcher:c1", row.From)
		}
	}
}
