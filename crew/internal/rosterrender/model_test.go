package rosterrender

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/jsonval"
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

// live is the liveness rule with both reads stubbed: `windows` are the branches
// stamped on a tmux window, and the worktree list names one per entry of
// `worktrees`. Pair it with a `roster.Probes` whose Worktrees agrees.
func live(windows []string, worktrees ...string) roster.Liveness {
	return roster.Liveness{
		Windows: func() map[string]bool {
			set := map[string]bool{}
			for _, b := range windows {
				set[b] = true
			}
			return set
		},
		Worktrees: porcelain(worktrees...),
	}
}

// porcelain is `git worktree list --porcelain` with one worktree per branch.
func porcelain(branches ...string) func() (string, error) {
	return func() (string, error) {
		out := ""
		for _, b := range branches {
			out += "worktree /wt/" + b + "\nHEAD 0000000000000000000000000000000000000000\nbranch refs/heads/" + b + "\n\n"
		}
		return out, nil
	}
}

// emptyLive is no evidence of life anywhere: no window, no worktree.
var emptyLive = live(nil)

// rowStates is the model's rows as `branch state`, sorted by branch the way the
// model leaves them.
func rowStates(t *testing.T, m map[string]any) []string {
	t.Helper()
	rows, _ := m["rows"].([]any)
	var out []string
	for _, r := range rows {
		row, _ := r.(map[string]any)
		b, _ := row["branch"].(string)
		s, _ := row["state"].(string)
		out = append(out, b+" "+s)
	}
	return out
}

// TestModelWithoutABusIsTheArmsEmptyOne: a repo with no bus yet renders an empty
// diagram rather than failing, and the arm said so with a literal.
// compact is a model rendered the way a test reads it back: one line, no colour.
func compact(v any) string {
	var b bytes.Buffer
	if err := jsonval.Encode(&b, v, false); err != nil {
		panic(err)
	}
	return b.String()
}

func decodeOne(t *testing.T, text string) map[string]any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil || m == nil {
		t.Fatalf("decode %q: %v", text, err)
	}
	return m
}

func TestModelWithoutABusIsTheArmsEmptyOne(t *testing.T) {
	dir := t.TempDir()
	p := bus.Paths{Common: dir, Dir: dir + "/crew", Log: dir + "/crew/events.jsonl"}
	m, err := model(p, "c1", 1791360000, nil, emptyRoster, emptyLive)
	if err != nil {
		t.Fatal(err)
	}
	if want := decodeOne(t, `{"rows":[],"holds":[],"roles":[]}`); !reflect.DeepEqual(m, want) {
		t.Errorf("model = %s", compact(m))
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
	m, err := model(p, "c1", 1791360300, nil, emptyRoster, live([]string{"feat/1-a"}))
	if err != nil {
		t.Fatal(err)
	}
	rows, _ := m["rows"].([]any)
	if len(rows) != 2 {
		t.Fatalf("rows = %d", len(rows))
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

// TestModelDrawsOnlyLiveRows is the diagram's cut: of a bus holding live,
// finished, awaiting-reap, reaped and unanswered-dispatch rows, the model keeps
// the live one, the one whose worktree still waits for the reaper, and the
// dispatch nothing has answered yet.
func TestModelDrawsOnlyLiveRows(t *testing.T) {
	events := func(branch, state string) []string {
		out := []string{
			`{"ts":1791360000000,"crew_id":"c1","kind":"dispatch","branch":"` + branch +
				`","session":"s1","worker_id":"worker:` + branch + `#s1","engine":"claude","model":"sonnet","tier":"standard","title":"` + branch + `","name":"n","color":"green","tmux":"colour28"}`,
		}
		if state != "" {
			out = append(out, `{"ts":1791360600000,"crew_id":"c1","from":"worker:`+branch+`#s1","to":"dispatcher:c1","kind":"status","body":{"state":"`+state+`"}}`)
		}
		return out
	}
	log := append(events("feat/1-live", "working"), events("feat/2-reap", "done")...)
	log = append(log, events("feat/3-history", "done")...)
	log = append(log, events("feat/4-stale", "pr_open")...)
	log = append(log, events("feat/5-dead", "exited")...)
	log = append(log, events("feat/6-reaped", "reaped")...)
	log = append(log, events("feat/7-pending", "")...)

	p, _ := modelFixture(t, log...)
	// feat/1-live has a window; feat/2-reap is finished with its worktree still
	// standing; nothing else has either.
	m, err := model(p, "c1", 1791361200, nil,
		roster.Probes{Panes: func() string { return "" }, Worktrees: porcelain("feat/2-reap")},
		live([]string{"feat/1-live"}, "feat/2-reap"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"feat/1-live working", "feat/2-reap done", "feat/7-pending dispatched"}
	if got := rowStates(t, m); len(got) != len(want) {
		t.Fatalf("rows = %v, want %v", got, want)
	} else {
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("row %d = %q, want %q", i, got[i], want[i])
			}
		}
	}
	// The live count is the diagram's own number, so it counts the rows it holds.
	if n, err := liveCount(m); err != nil || n != 2 {
		t.Errorf("liveCount = %d (%v), want 2", n, err)
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
		got, _ := list[0].(map[string]any)["engine"].(string)
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
