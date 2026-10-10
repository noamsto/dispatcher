package rosterrender

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/jsonv"
	"github.com/noamsto/dispatcher/crew/internal/roster"
)

func modelFixture(t *testing.T, events ...string) (bus.Paths, string) {
	t.Helper()
	dir := t.TempDir()
	p := bus.Paths{Common: dir, Dir: dir + "/crew", Log: dir + "/crew/events.jsonl"}
	cdir := p.CrewDir("c1")
	if err := os.MkdirAll(cdir, 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(p.Log)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if _, err := f.WriteString(e + "\n"); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return p, cdir
}

var emptyRoster = roster.Probes{
	Panes:     func() string { return "" },
	Worktrees: func() (string, error) { return "", nil },
}

// TestModelWithoutABusIsTheArmsEmptyOne: a repo with no bus yet renders an empty
// diagram rather than failing, and the arm said so with a literal.
// compact is a model rendered the way a test reads it back: one line, no colour.
func compact(v jsonv.Value) string {
	var b bytes.Buffer
	if err := jsonv.Encode(&b, v, jsonv.Options{}); err != nil {
		panic(err)
	}
	return b.String()
}

func decodeOne(t *testing.T, text string) jsonv.Value {
	t.Helper()
	vs, err := jsonv.DecodeStream(strings.NewReader(text))
	if err != nil || len(vs) != 1 {
		t.Fatalf("decode %q: %v", text, err)
	}
	return vs[0]
}

func TestModelWithoutABusIsTheArmsEmptyOne(t *testing.T) {
	dir := t.TempDir()
	p := bus.Paths{Common: dir, Dir: dir + "/crew", Log: dir + "/crew/events.jsonl"}
	m, err := model(p, "c1", 1791360000, nil, emptyRoster)
	if err != nil {
		t.Fatal(err)
	}
	if got := compact(m); got != `{"rows":[],"holds":[],"roles":[]}` {
		t.Errorf("model = %s", got)
	}
}

// TestRenderTextEndsWithExactlyOneNewline is the `$(…)` plus `printf '%s\n'` pair
// the arm wrote the file with: every trailing newline of the jq output is dropped
// and one is appended, so `cmp -s` sees the same bytes the next pass produces.
func TestRenderTextEndsWithExactlyOneNewline(t *testing.T) {
	text, err := renderText(decodeOne(t, `{"rows":[],"holds":[],"roles":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(text, "\n") || strings.HasSuffix(text, "\n\n") {
		t.Errorf("text ends with %q", text[len(text)-2:])
	}
}

// TestLiveCountIsTheModelNotItsPrintedForm: the count is a number the daemon
// compares, never a string it re-parses, so a jq float stays a count.
func TestLiveCountIsTheModelNotItsPrintedForm(t *testing.T) {
	for _, tc := range []struct {
		model string
		want  int64
	}{
		{`{"rows":[],"holds":[],"roles":[]}`, 0},
		{`{"rows":[{"state":"working"},{"state":"done"},{"state":"blocked"},{"state":"dispatched"}],"holds":[],"roles":[]}`, 3},
		{`{"rows":[],"holds":[{},{}],"roles":[]}`, 2},
	} {
		got, err := liveCount(decodeOne(t, tc.model))
		if err != nil {
			t.Fatal(err)
		}
		if got != tc.want {
			t.Errorf("liveCount(%s) = %d, want %d", tc.model, got, tc.want)
		}
	}
}

// TestModelCarriesTheDispatchedRowUntilTheBranchSpeaks: `crew roster` folds status
// rows only, so a launched session reads as `dispatched` until it posts, and a
// re-dispatch without --base keeps the base an earlier dispatch named.
func TestModelCarriesTheDispatchedRowUntilTheBranchSpeaks(t *testing.T) {
	p, _ := modelFixture(t,
		`{"ts":1791360000000,"crew_id":"c1","kind":"dispatch","branch":"feat/1-a","session":"s1","worker_id":"worker:feat/1-a#s1","engine":"claude","model":"sonnet","tier":"standard","title":"Alpha","name":"sage","color":"green","tmux":"colour28","base":"main"}`,
		`{"ts":1791360100000,"crew_id":"c1","kind":"dispatch","branch":"feat/2-b","session":"s2","worker_id":"worker:feat/2-b#s2","engine":"codex","model":"gpt-5","tier":"deep","title":"Bravo","name":"atlas","color":"blue","tmux":"colour32"}`,
		`{"ts":1791360200000,"crew_id":"c1","from":"worker:feat/1-a#s1","to":"dispatcher:c1","kind":"status","body":{"state":"working","detail":"execute"}}`,
	)
	m, err := model(p, "c1", 1791360300, nil, emptyRoster)
	if err != nil {
		t.Fatal(err)
	}
	rows, _ := m.Get("rows")
	if rows.Len() != 2 {
		t.Fatalf("rows = %d", rows.Len())
	}
	byBranch := map[string]map[string]any{}
	var out []map[string]any
	if err := json.Unmarshal([]byte(compact(rows)), &out); err != nil {
		t.Fatal(err)
	}
	for _, r := range out {
		byBranch[r["branch"].(string)] = r
	}
	if got := byBranch["feat/1-a"]["state"]; got != "working" {
		t.Errorf("a branch that posted reads as %v", got)
	}
	if got := byBranch["feat/2-b"]["state"]; got != "dispatched" {
		t.Errorf("a silent branch reads as %v", got)
	}
	// The base rides along on both, from the dispatch that named one.
	if got := byBranch["feat/1-a"]["base"]; got != "main" {
		t.Errorf("base = %v", got)
	}
}

// TestRoleEngineIsOnlyAKnownEngine: roles.json is worker-writable, so a symlink is
// refused and any value but a worker engine reads as "?".
func TestRoleEngineIsOnlyAKnownEngine(t *testing.T) {
	dir := t.TempDir()
	branch := "feat/1-a"
	if err := os.MkdirAll(dir+"/artifacts/"+branch, 0o755); err != nil {
		t.Fatal(err)
	}
	rows := []string{branch + "\tspec-critic\tidle"}

	write := func(text string) {
		t.Helper()
		if err := os.WriteFile(dir+"/artifacts/"+branch+"/roles.json", []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	engine := func() string {
		list := roleList(rows, dir)
		v, _ := list.Elems()[0].Get("engine")
		got, _ := v.AsString()
		return got
	}

	write(`{"spec-critic":{"agent":"claude"}}`)
	if got := engine(); got != "claude" {
		t.Errorf("engine = %q", got)
	}
	write(`{"spec-critic":{"agent":"something-else"}}`)
	if got := engine(); got != "?" {
		t.Errorf("an unknown agent read as %q", got)
	}
	write(`{"other-role":{"agent":"pi"}}`)
	if got := engine(); got != "?" {
		t.Errorf("a missing role read as %q", got)
	}
	write(`{not json`)
	if got := engine(); got != "?" {
		t.Errorf("a malformed roles.json read as %q", got)
	}

	// A symlinked roles.json could aim the reader at any file the user can read.
	if err := os.Remove(dir + "/artifacts/" + branch + "/roles.json"); err != nil {
		t.Fatal(err)
	}
	secret := dir + "/secret.json"
	if err := os.WriteFile(secret, []byte(`{"spec-critic":{"agent":"pi"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, dir+"/artifacts/"+branch+"/roles.json"); err != nil {
		t.Fatal(err)
	}
	if got := engine(); got != "?" {
		t.Errorf("a symlinked roles.json was read: %q", got)
	}
}

// TestLoopSigIsTheBusSizeThePaneAndTheRoles: a part of the diagram the bus cannot
// mention — a role pane starting, a dispatcher pane moving — has to change it.
func TestLoopSigIsTheBusSizeThePaneAndTheRoles(t *testing.T) {
	p, cdir := modelFixture(t, `{"ts":1,"crew_id":"c1","kind":"dispatch","branch":"b"}`)
	base := loopSig(busSize(p), cdir, nil)
	if got := loopSig(busSize(p)+1, cdir, nil); got == base {
		t.Error("a bus that grew did not change the signature")
	}
	if err := os.WriteFile(cdir+"/roster-render.pane", []byte("%7\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := loopSig(busSize(p), cdir, nil); got == base {
		t.Error("a new dispatcher pane did not change the signature")
	}
	if got := loopSig(busSize(p), cdir, []string{"b\tspec-critic\tidle"}); got == base {
		t.Error("a role pane did not change the signature")
	}
	// A bus that does not exist yet is a zero, not an error.
	missing := bus.Paths{Common: t.TempDir(), Log: t.TempDir() + "/events.jsonl"}
	if got := busSize(missing); got != 0 {
		t.Errorf("busSize = %d", got)
	}
}

func TestTargetUsesTheRenderersZone(t *testing.T) {
	// The stamp is the renderer's local time, not UTC: the same epoch in two zones
	// is two different names, which is what the arm's `%(…)T` meant.
	epoch := time.Date(2026, 10, 7, 23, 30, 0, 0, time.UTC)
	utc := target("/w/repo/.git", "1791360000-x", "/r", func() time.Time { return epoch })
	if !strings.Contains(utc, "-10-07-0800-") {
		t.Errorf("UTC stamp = %s", utc)
	}
}
