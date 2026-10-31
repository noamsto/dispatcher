package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/clock"
	"github.com/noamsto/dispatcher/crew/internal/jsonval"
	"github.com/noamsto/dispatcher/crew/internal/proc/proctest"
	"github.com/noamsto/dispatcher/crew/internal/remote"
	"github.com/noamsto/dispatcher/crew/internal/roster"
	"github.com/noamsto/dispatcher/crew/internal/testjson"
	"github.com/noamsto/dispatcher/crew/internal/tmux/tmuxtest"
)

// rosterArm runs `roster` against a bus with the fold's clock frozen, so age_s
// is exact. Fold behaviour is pinned by value in internal/roster; this file is
// the arm's own half: how it prints and what it exits.
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

// gitRun is a git fixture call, so it carries its own identity: the test repo
// has no committer configured and must not borrow this checkout's.
func gitRun(t *testing.T, repo string, args ...string) {
	t.Helper()
	full := append([]string{"-C", repo, "-c", "user.email=t@e", "-c", "user.name=t", "-c", "commit.gpgsign=false"}, args...)
	if out, err := exec.Command("git", full...).CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// TestRosterCLIResolvesAnExitedRowThroughTheRealWorktreeList covers the wiring
// the fold's tests stub out: internal/roster hands `resolveExited` a scripted
// porcelain list, while the arm reads a real `git worktree list --porcelain`
// from the repo it resolved, plus a pane list read by one exact tmux argv. A
// wrong cwd or a changed `-F` format silently leaves every false `exited` real.
func TestRosterCLIResolvesAnExitedRowThroughTheRealWorktreeList(t *testing.T) {
	rows := `{"ts":1700000001000,"crew_id":"c1","kind":"status","from":"worker:feat/roster-live#s1-1","to":"dispatcher:c1","body":{"state":"working"}}
{"ts":1700000002000,"crew_id":"c1","kind":"status","from":"worker:feat/roster-live#s1-1","to":"dispatcher:c1","body":{"state":"exited"}}
`
	repo, _ := repoWithLog(t, &rows, 0o644)
	gitRun(t, repo, "commit", "--allow-empty", "-qm", "init")
	gitRun(t, repo, "branch", "feat/roster-live")
	wt := filepath.Join(repo, ".wt", "roster-live")
	gitRun(t, repo, "worktree", "add", "-q", wt, "feat/roster-live")
	// A real tmux reports a kernel cwd; git resolves symlinks the same way, and
	// the pane's path must equal the worktree's exactly.
	wt, err := filepath.EvalSymlinks(wt)
	if err != nil {
		t.Fatal(err)
	}

	const panesArgv = "list-panes -a -F #{pane_current_command} #{pane_current_path}"
	tx := &tmuxtest.Fake{Out: map[string]string{panesArgv: ".claude-wrapped " + wt + "\n"}}
	e := env{
		getwd: func() (string, error) { return repo, nil },
		clock: clock.Clock{Now: func() time.Time { return time.Unix(1700000100, 0) }},
		probes: func(ctx context.Context, cwd string) roster.Probes {
			p := probes(ctx, cwd)
			p.Tmux = tx // scripted: no tmux server is this test's
			return p
		},
		proc: proctest.New(nil, nil),
	}
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"roster", "c1"}, &stdout, &stderr, e); code != 0 {
		t.Fatalf("code %d, stderr %q", code, stderr.String())
	}
	var outRows []map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &outRows); err != nil || len(outRows) != 1 {
		t.Fatalf("rows: %v\n%s", err, stdout.String())
	}
	if outRows[0]["state"] != "working" || outRows[0]["exit_suspect"] != true {
		t.Errorf("row = state %v exit_suspect %v, want working/true: %s", outRows[0]["state"], outRows[0]["exit_suspect"], stdout.String())
	}
	if calls := tx.Calls(); len(calls) != 1 || calls[0] != panesArgv {
		t.Errorf("tmux calls = %q, want the one pane list read by %q", calls, panesArgv)
	}
}

// TestRosterCLIStdoutIsThePrettyForm is `crew roster`'s stdout framing: the fold
// printed in jq's pretty form, not compact like `crew sessions`. The bats case
// ran `cmp "$f" <(jq . "$f")`; the same fixed point holds here — stdout is byte
// for byte the indented encoding of its own value, and its rows match by value.
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
	// two-space indent, and ends the file with one newline. Re-encoding the value
	// the same way must give those bytes back.
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

// TestRosterCLIHelpIsTheRosterScreen pins the whole first line, where
// TestRunHelpRouting pins only the routing and its prefix. No repo: --help is
// routed before the bus is opened.
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

const peerRosterLog = `{"ts":1700000001000,"crew_id":"c1","kind":"status","from":"worker:feat/x#s1-1","to":"dispatcher:c1","body":{"state":"working"}}
`

// writePeerFiles gives the repo's bus a halo link file and roster file.
func writePeerFiles(t *testing.T, repo, link string) {
	t.Helper()
	dir := filepath.Join(repo, ".git", "crew", "peers")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	const snap = `{"synced":1700000099000,"rows":[{"crew_id":"c1","branch":"feat/p","name":"otter","state":"working","tier":"standard","engine":"claude","ts":1700000090000}]}`
	for name, body := range map[string]string{"halo.link": link, "halo.roster": snap} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func peerRosterEnv(repo string, hosts func() (map[string]remote.Host, error)) env {
	return env{
		getwd:     func() (string, error) { return repo, nil },
		clock:     clock.Clock{Now: func() time.Time { return time.Unix(1700000100, 0) }},
		probes:    tmuxProbes(&tmuxtest.Fake{}),
		proc:      proctest.New(nil, nil),
		peerHosts: hosts,
	}
}

func haloHosts() (map[string]remote.Host, error) {
	return map[string]remote.Host{"halo": {Peer: true}}, nil
}

func rosterJSON(t *testing.T, e env, args ...string) ([]map[string]any, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), append([]string{"roster"}, args...), &stdout, &stderr, e); code != 0 {
		t.Fatalf("code %d, stderr %q", code, stderr.String())
	}
	var rows []map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &rows); err != nil {
		t.Fatalf("stdout: %v\n%s", err, stdout.String())
	}
	return rows, stderr.String()
}

func TestRosterIncludesPeerRows(t *testing.T) {
	log := peerRosterLog
	repo, _ := repoWithLog(t, &log, 0o644)
	want, _ := rosterJSON(t, peerRosterEnv(repo, haloHosts), "c1")
	writePeerFiles(t, repo, `{"ok":1700000098000,"err":0,"detail":""}`)

	rows, errOut := rosterJSON(t, peerRosterEnv(repo, haloHosts), "c1")
	if errOut != "" {
		t.Errorf("stderr %q", errOut)
	}
	if len(rows) != len(want)+1 {
		t.Fatalf("%d rows, want %d: %v", len(rows), len(want)+1, rows)
	}
	if got, w := testjson.Compact(rows[:len(want)]), testjson.Compact(want); got != w {
		t.Errorf("local rows changed:\n got %s\nwant %s", got, w)
	}
	p := rows[len(rows)-1]
	if p["host"] != "halo" || p["peer"] != true || p["live"] != nil || p["name"] != "otter" || p["state"] != "working" || p["crew_id"] != "c1" {
		t.Errorf("peer row = %v", p)
	}
}

func TestRosterPeerError(t *testing.T) {
	log := peerRosterLog
	repo, _ := repoWithLog(t, &log, 0o644)
	writePeerFiles(t, repo, `{"ok":1700000000000,"err":1700000098000,"detail":"ssh: refused"}`)

	rows, _ := rosterJSON(t, peerRosterEnv(repo, haloHosts), "c1")
	p := rows[len(rows)-1]
	if p["peer"] != true || p["state"] != nil || p["peer_error"] != "last sync failed: ssh: refused" {
		t.Errorf("unhealthy peer row = %v", p)
	}

	boom := func() (map[string]remote.Host, error) { return nil, errors.New("boom") }
	rows, errOut := rosterJSON(t, peerRosterEnv(repo, boom), "c1")
	if errOut != "crew: roster: peer settings: boom\n" {
		t.Errorf("stderr %q", errOut)
	}
	if len(rows) != 1 || rows[0]["peer"] != nil {
		t.Errorf("settings error should leave only local rows: %v", rows)
	}
}

func TestRosterLiveOmitsPeerRows(t *testing.T) {
	log := peerRosterLog
	repo, _ := repoWithLog(t, &log, 0o644)
	writePeerFiles(t, repo, `{"ok":1700000098000,"err":0,"detail":""}`)

	called := false
	hosts := func() (map[string]remote.Host, error) { called = true; return haloHosts() }
	rows, _ := rosterJSON(t, peerRosterEnv(repo, hosts), "--live", "c1")
	for _, r := range rows {
		if r["peer"] == true {
			t.Errorf("--live has a peer row: %v", r)
		}
	}
	if called {
		t.Error("--live loaded peer settings")
	}
}
