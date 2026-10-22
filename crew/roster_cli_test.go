package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/clock"
	"github.com/noamsto/dispatcher/crew/internal/jsonval"
	"github.com/noamsto/dispatcher/crew/internal/proc/proctest"
	"github.com/noamsto/dispatcher/crew/internal/testjson"
	"github.com/noamsto/dispatcher/crew/internal/tmux/tmuxtest"
)

// rosterArm runs `roster` against a bus with the fold's clock frozen, so age_s
// is exact. The fold itself is pinned by value in internal/roster; what lives
// here is the arm's own half of the contract: how it prints and what it exits.
func rosterArm(t *testing.T, rows string, args ...string) (string, string, int) {
	t.Helper()
	repo, _ := repoWithLog(t, &rows, 0o644)
	e := env{
		getwd:  func() (string, error) { return repo, nil },
		clock:  clock.Clock{Now: func() time.Time { return time.Unix(1700000100, 0) }},
		probes: tmuxProbes(&tmuxtest.Fake{}),
		proc:   proctest.New(nil, nil),
	}
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), append([]string{"roster"}, args...), &stdout, &stderr, e)
	return stdout.String(), stderr.String(), code
}

// TestRosterCLIStdoutIsThePrettyForm is `crew roster`'s stdout framing: the fold
// printed in jq's pretty form, not compact like `crew sessions`. The bats case
// ran `cmp "$f" <(jq . "$f")`, so the Go version pins the same fixed point —
// stdout is byte for byte the indented encoding of its own value (#1044 asks
// for value-equality over jq's exact spacing, so the rows are also compared by
// value against the fold).
func TestRosterCLIStdoutIsThePrettyForm(t *testing.T) {
	const rows = `{"ts":1700000001000,"crew_id":"c1","kind":"status","from":"worker:feat/x#s1-1","to":"dispatcher:c1","body":{"state":"working"}}
{"ts":1700000002000,"crew_id":"c1","kind":"status","from":"worker:feat/x#s1-1","to":"dispatcher:c1","body":{"state":"done"}}
{"ts":1700000003000,"crew_id":"c1","kind":"status","from":"worker:feat/y#s1-1","to":"dispatcher:c1","body":{"state":"blocked","detail":"which approach?"}}
`
	const want = `[{"from":"worker:feat/x#s1-1","branch":"feat/x","session":"s1-1","state":"done","detail":null,"source":null,"ts":1700000002000,"pr_url":null,"age_s":98,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":"s1-1","state":"done","age_s":98}],"name":"lagoon","color":"darkcyan","tmux":"colour31"},` +
		`{"from":"worker:feat/y#s1-1","branch":"feat/y","session":"s1-1","state":"blocked","detail":"which approach?","source":null,"ts":1700000003000,"pr_url":null,"age_s":97,"title":null,"engine":null,"model":null,"tier":null,"engine_session":null,"sessions":[{"session":"s1-1","state":"blocked","age_s":97}],"name":"rose","color":"pink","tmux":"colour162"}]`

	out, errOut, code := rosterArm(t, rows, "c1")
	if code != 0 || errOut != "" {
		t.Fatalf("code %d, stderr %q", code, errOut)
	}
	if got := testjson.Compact(testjson.MustParse(t, out)); got != testjson.Compact(testjson.MustParse(t, want)) {
		t.Errorf("stdout by value:\n got %s\nwant %s", got, testjson.Compact(testjson.MustParse(t, want)))
	}

	// The framing: jq pretty-prints an array of objects over many lines with a
	// two-space indent and ends the file with one newline. Re-encoding the value
	// the same way must give the bytes back — a compact arm, an extra blank line
	// or a re-indented one all fail here.
	dec := json.NewDecoder(strings.NewReader(out))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, out)
	}
	var pretty bytes.Buffer
	if err := jsonval.Encode(&pretty, v, true); err != nil {
		t.Fatal(err)
	}
	if pretty.String() != out {
		t.Errorf("stdout is not the indented encoding of its own value:\n got %q\nwant %q", out, pretty.String())
	}
	if !strings.HasPrefix(out, "[\n  {\n    ") || !strings.HasSuffix(out, "\n  }\n]\n") || strings.HasSuffix(out, "\n\n") {
		t.Errorf("stdout is not jq's multi-line pretty shape:\n%s", out)
	}
}

// TestRosterCLIHelpIsTheRosterScreen pins the whole first line of the screen.
// TestRunHelpRouting pins the routing and its prefix; the bats case asserted the
// usage line exactly, and it is the line every caller of `crew roster --help`
// reads. No repo: --help is routed before the bus is opened.
func TestRosterCLIHelpIsTheRosterScreen(t *testing.T) {
	noCwd := env{getwd: func() (string, error) { return "", errors.New("getwd: no such file") }}
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"roster", "--help"}, &stdout, &stderr, noCwd)
	if code != 0 {
		t.Fatalf("code %d, want 0 (stderr %q)", code, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr %q, want none", stderr.String())
	}
	first, _, _ := strings.Cut(stdout.String(), "\n")
	if first != "usage: crew roster [crew]" {
		t.Errorf("first line %q, want %q", first, "usage: crew roster [crew]")
	}
}
