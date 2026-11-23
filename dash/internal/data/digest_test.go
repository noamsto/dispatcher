package data

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

const digestBase int64 = 1794800000000

func digestFixture(t *testing.T) (string, *BusScanner) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "events.jsonl")
	if err := os.WriteFile(path, readTestdata(t, "digest_events.jsonl"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path, NewBusScanner(path)
}

func scanFixture(t *testing.T) BusDigest {
	t.Helper()
	_, s := digestFixture(t)
	d, err := s.Scan(digestBase + 10000)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func stageNames(w *WorkerDigest) []string {
	names := []string{}
	for _, m := range w.Stages {
		names = append(names, m.Name)
	}
	return names
}

func TestScanStages(t *testing.T) {
	d := scanFixture(t)

	a := d.Workers[WorkerKey{"c1", "feat/a"}]
	if a == nil {
		t.Fatal("no digest for feat/a")
	}
	if got, want := stageNames(a), []string{"spec", "plan", "execute"}; !reflect.DeepEqual(got, want) {
		t.Errorf("feat/a stages = %v, want %v", got, want)
	}
	if a.Stages[0].TS != digestBase {
		t.Errorf("spec ts = %d, want first entry %d", a.Stages[0].TS, digestBase)
	}

	b := d.Workers[WorkerKey{"c1", "feat/b"}]
	if got, want := stageNames(b), []string{"spec"}; !reflect.DeepEqual(got, want) {
		t.Errorf("feat/b stages after new run = %v, want %v", got, want)
	}
	if b.Stages[0].TS != digestBase+6000 {
		t.Errorf("feat/b new-run spec ts = %d, want %d", b.Stages[0].TS, digestBase+6000)
	}
	if b.Ledger != "" || b.LedgerTS != 0 {
		t.Errorf("feat/b ledger after new run = %q @%d, want cleared", b.Ledger, b.LedgerTS)
	}
}

func TestScanLedger(t *testing.T) {
	d := scanFixture(t)
	e := d.Workers[WorkerKey{"c1", "feat/e"}]
	want := "AC1 pass(go test)"
	if e.Ledger != want || e.LedgerTS != digestBase+8600 {
		t.Errorf("ledger = %q @%d, want %q @%d", e.Ledger, e.LedgerTS, want, digestBase+8600)
	}
}

func TestScanRoles(t *testing.T) {
	d := scanFixture(t)

	pc := d.Roles[RoleKey{"c1", "feat/a", "plan-critic"}]
	if pc == nil || pc.Verdict != "accept" || pc.VerdictTS != digestBase+2500 {
		t.Errorf("plan-critic = %+v, want accept @%d", pc, digestBase+2500)
	}
	rv := d.Roles[RoleKey{"c1", "feat/a", "reviewer"}]
	if rv == nil || rv.State != "blocked" || rv.Detail != "role reviewer engine exited (pane %9)" || rv.TS != digestBase+4000 {
		t.Errorf("reviewer = %+v", rv)
	}
	sc := d.Roles[RoleKey{"c1", "feat/c", "spec-critic"}]
	if sc == nil || sc.State != "failed" || sc.Detail != "no assignment" {
		t.Errorf("spec-critic = %+v", sc)
	}
}

func TestScanPRWatch(t *testing.T) {
	d := scanFixture(t)
	pr := d.PRs[12]
	if pr == nil {
		t.Fatal("no pr-watch digest for 12")
	}
	want := PRWatch{PR: 12, URL: "https://github.com/o/r/pull/12", State: "OPEN", Checks: "SUCCESS", Mergeable: "MERGEABLE", TS: digestBase + 8000}
	if *pr != want {
		t.Errorf("pr 12 = %+v, want %+v", *pr, want)
	}
}

func TestScanActivity(t *testing.T) {
	d := scanFixture(t)
	if got := len(d.Workers[WorkerKey{"c1", "feat/a"}].Activity); got != 5 {
		t.Errorf("feat/a activity = %d, want 5 (msg row counted, watchdog row excluded)", got)
	}
	if got := len(d.Workers[WorkerKey{"c1", "feat/d"}].Activity); got != 0 {
		t.Errorf("feat/d activity = %d, want 0 (older than 60 min)", got)
	}
}

func TestScanRecent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	var sb strings.Builder
	for i := range 60 {
		fmt.Fprintf(&sb, `{"ts":%d,"crew_id":"c1","from":"worker:feat/a#s1","kind":"status","body":{"state":"working","detail":"d%d"}}`+"\n", digestBase+int64(i), i)
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	d, err := NewBusScanner(path).Scan(digestBase + 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Recent) != 50 {
		t.Fatalf("recent = %d, want 50", len(d.Recent))
	}
	first, last := d.Recent[0], d.Recent[49]
	if first.Detail != "d10" || last.Detail != "d59" || last.TS != digestBase+59 || last.From != "worker:feat/a#s1" || last.Kind != "status" || last.State != "working" {
		t.Errorf("recent window wrong: first=%+v last=%+v", first, last)
	}
}

func TestScannerIncremental(t *testing.T) {
	path, s := digestFixture(t)
	now := digestBase + 10000

	d, err := s.Scan(now)
	if err != nil {
		t.Fatal(err)
	}
	baseRecent := len(d.Recent)
	if got := stageNames(d.Workers[WorkerKey{"c1", "feat/a"}]); len(got) != 3 {
		t.Fatalf("torn last line must not count yet: stages = %v", got)
	}

	// Mutating the returned copy must not leak into the scanner.
	d.Workers[WorkerKey{"c1", "feat/a"}].Stages = nil
	delete(d.PRs, 12)
	d2, _ := s.Scan(now)
	if len(d2.Workers[WorkerKey{"c1", "feat/a"}].Stages) != 3 || d2.PRs[12] == nil {
		t.Fatal("Scan result aliases scanner state")
	}

	// Complete the torn line and append another: only new lines are read.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	tail := `"}}` + "\n" +
		`{"ts":1794800009500,"crew_id":"c1","from":"worker:feat/a#s1","to":"dispatcher:c1","kind":"status","body":{"state":"working","detail":"review"}}` + "\n"
	if _, err := f.WriteString(tail); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	d3, err := s.Scan(now)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := stageNames(d3.Workers[WorkerKey{"c1", "feat/a"}]), []string{"spec", "plan", "execute", "gate", "review"}; !reflect.DeepEqual(got, want) {
		t.Errorf("stages after append = %v, want %v", got, want)
	}
	if len(d3.Recent) != baseRecent+2 {
		t.Errorf("recent = %d, want %d (torn line counted once)", len(d3.Recent), baseRecent+2)
	}

	// Truncation restarts from zero with a fresh digest.
	short := `{"ts":1794800000000,"crew_id":"c9","from":"worker:feat/z#s1","kind":"status","body":{"state":"working","detail":"spec"}}` + "\n"
	if err := os.WriteFile(path, []byte(short), 0o600); err != nil {
		t.Fatal(err)
	}
	d4, err := s.Scan(now)
	if err != nil {
		t.Fatal(err)
	}
	if len(d4.Workers) != 1 || d4.Workers[WorkerKey{"c9", "feat/z"}] == nil || len(d4.Recent) != 1 {
		t.Errorf("after truncate: workers=%v recent=%d", d4.Workers, len(d4.Recent))
	}
}

func TestNewRunClearsPriorRoles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	lines := []string{
		`{"ts":1794800000000,"crew_id":"c1","from":"worker:feat/x#s1","to":"dispatcher:c1","kind":"status","body":{"state":"working","detail":"spec"}}`,
		`{"ts":1794800001000,"crew_id":"c1","from":"role:feat/x:reviewer","to":"dispatcher:c1","kind":"status","body":{"state":"failed","detail":"boom"}}`,
		`{"ts":1794800002000,"crew_id":"c1","from":"worker:feat/x#s1","to":"dispatcher:c1","kind":"status","body":{"state":"failed","detail":"x"}}`,
		`{"ts":1794800003000,"crew_id":"c1","from":"worker:feat/x#s2","to":"dispatcher:c1","kind":"status","body":{"state":"working","detail":"spec"}}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	d, err := NewBusScanner(path).Scan(digestBase + 10000)
	if err != nil {
		t.Fatal(err)
	}
	if r := d.Roles[RoleKey{"c1", "feat/x", "reviewer"}]; r != nil {
		t.Errorf("old run's role survived a new run: %+v", r)
	}
}

func sinkRow(ts int64, from, to, body string) string {
	b, _ := json.Marshal(body)
	return fmt.Sprintf(`{"ts":%d,"crew_id":"c1","from":%q,"to":%q,"kind":"msg","body":%s}`, ts, from, to, b)
}

func statusRow(ts int64, from, state, detail string) string {
	return fmt.Sprintf(`{"ts":%d,"crew_id":"c1","from":%q,"to":"dispatcher:c1","kind":"status","body":{"state":%q,"detail":%q}}`, ts, from, state, detail)
}

func scanRows(t *testing.T, rows ...string) BusDigest {
	t.Helper()
	path := filepath.Join(t.TempDir(), "events.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(rows, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	d, err := NewBusScanner(path).Scan(digestBase + 100000)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestScanSubagents(t *testing.T) {
	const w, sink = "worker:feat/x#s1", "subagent:c1"
	key := WorkerKey{"c1", "feat/x"}
	at := func(n int64) int64 { return digestBase + n*1000 }
	tests := []struct {
		name string
		rows []string
		want []SubagentRun
	}{
		{"spawn runs", []string{
			sinkRow(at(1), w, sink, `{"subagent":"spawn","id":"t1","engine":"claude","label":"look","agent":"Explore","model":"m","effort":"high"}`),
		}, []SubagentRun{{ID: "t1", Session: "s1", Engine: "claude", Label: "look", Agent: "Explore", Model: "m", Effort: "high", State: "running", StartTS: at(1)}}},
		{"claude foreground", []string{
			sinkRow(at(1), w, sink, `{"subagent":"spawn","id":"t1","label":"look"}`),
			sinkRow(at(2), w, sink, `{"subagent":"finish","agent_id":"a1","outcome":"ok"}`),
			sinkRow(at(3), w, sink, `{"subagent":"finish","id":"t1","agent_id":"a1","outcome":"ok","model":"opus"}`),
		}, []SubagentRun{{ID: "t1", AgentID: "a1", Session: "s1", Label: "look", Model: "opus", State: "done", StartTS: at(1), EndTS: at(3)}}},
		{"background", []string{
			sinkRow(at(1), w, sink, `{"subagent":"spawn","id":"t1","label":"look"}`),
			sinkRow(at(2), w, sink, `{"subagent":"spawn","id":"t1","agent_id":"a1"}`),
			sinkRow(at(3), w, sink, `{"subagent":"finish","agent_id":"a1","outcome":"ok"}`),
		}, []SubagentRun{{ID: "t1", AgentID: "a1", Session: "s1", Label: "look", State: "done", StartTS: at(1), EndTS: at(3)}}},
		{"unknown id falls back to agent_id", []string{
			sinkRow(at(1), w, sink, `{"subagent":"spawn","id":"t1","agent_id":"a1"}`),
			sinkRow(at(2), w, sink, `{"subagent":"finish","id":"zz","agent_id":"a1"}`),
		}, []SubagentRun{{ID: "t1", AgentID: "a1", Session: "s1", State: "done", StartTS: at(1), EndTS: at(2)}}},
		{"error is failed", []string{
			sinkRow(at(1), w, sink, `{"subagent":"spawn","id":"t1"}`),
			sinkRow(at(2), w, sink, `{"subagent":"finish","id":"t1","outcome":"error"}`),
		}, []SubagentRun{{ID: "t1", Session: "s1", State: "failed", StartTS: at(1), EndTS: at(2)}}},
		{"aborted is failed", []string{
			sinkRow(at(1), w, sink, `{"subagent":"spawn","id":"t1"}`),
			sinkRow(at(2), w, sink, `{"subagent":"finish","id":"t1","outcome":"aborted"}`),
		}, []SubagentRun{{ID: "t1", Session: "s1", State: "failed", StartTS: at(1), EndTS: at(2)}}},
		{"duplicate finish ignored", []string{
			sinkRow(at(1), w, sink, `{"subagent":"spawn","id":"t1"}`),
			sinkRow(at(2), w, sink, `{"subagent":"finish","id":"t1","outcome":"ok"}`),
			sinkRow(at(3), w, sink, `{"subagent":"finish","id":"t1","outcome":"error","model":"late"}`),
		}, []SubagentRun{{ID: "t1", Session: "s1", State: "done", StartTS: at(1), EndTS: at(2)}}},
		{"finish before spawn by id", []string{
			sinkRow(at(1), w, sink, `{"subagent":"finish","id":"t1","agent_id":"a1","outcome":"error","model":"m"}`),
			sinkRow(at(2), w, sink, `{"subagent":"spawn","id":"t1","label":"look"}`),
		}, []SubagentRun{{ID: "t1", AgentID: "a1", Session: "s1", Label: "look", Model: "m", State: "failed", StartTS: at(1), EndTS: at(1)}}},
		{"late spawn keeps the finished model", []string{
			sinkRow(at(1), w, sink, `{"subagent":"finish","id":"t1","outcome":"ok","model":"B"}`),
			sinkRow(at(2), w, sink, `{"subagent":"spawn","id":"t1","model":"A","label":"look"}`),
		}, []SubagentRun{{ID: "t1", Session: "s1", Label: "look", Model: "B", State: "done", StartTS: at(1), EndTS: at(1)}}},
		{"spawn then finish records the finished model", []string{
			sinkRow(at(1), w, sink, `{"subagent":"spawn","id":"t1","model":"A"}`),
			sinkRow(at(2), w, sink, `{"subagent":"finish","id":"t1","outcome":"ok","model":"B"}`),
		}, []SubagentRun{{ID: "t1", Session: "s1", Model: "B", State: "done", StartTS: at(1), EndTS: at(2)}}},
		{"unknown agent_id-only finish dropped", []string{
			sinkRow(at(1), w, sink, `{"subagent":"finish","agent_id":"a1"}`),
		}, nil},
		{"spawn without id dropped", []string{
			sinkRow(at(1), w, sink, `{"subagent":"spawn","label":"x"}`),
		}, nil},
		{"unknown phase dropped", []string{
			sinkRow(at(1), w, sink, `{"subagent":"pause","id":"t1"}`),
		}, nil},
		{"role sender ignored", []string{
			sinkRow(at(1), "role:feat/x:reviewer", sink, `{"subagent":"spawn","id":"t1"}`),
		}, nil},
		{"wrong crew ignored", []string{
			sinkRow(at(1), w, "subagent:c2", `{"subagent":"spawn","id":"t1"}`),
		}, nil},
		{"other recipient ignored", []string{
			sinkRow(at(1), w, "dispatcher:c1", `{"subagent":"spawn","id":"t1"}`),
		}, nil},
		{"malformed body ignored", []string{
			sinkRow(at(1), w, sink, `not json`),
			sinkRow(at(2), w, sink, `{"subagent":7,"id":"t1"}`),
		}, nil},
		{"label clipped to 80 runes, others to 64", []string{
			sinkRow(at(1), w, sink, fmt.Sprintf(`{"subagent":"spawn","id":"t1","label":%q,"agent":%q}`, strings.Repeat("é", 100), strings.Repeat("a", 100))),
		}, []SubagentRun{{ID: "t1", Session: "s1", Label: strings.Repeat("é", 80), Agent: strings.Repeat("a", 64), State: "running", StartTS: at(1)}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := scanRows(t, tc.rows...).Subagents[key]
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("runs = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestScanSubagentsCap(t *testing.T) {
	const w, sink = "worker:feat/x#s1", "subagent:c1"
	key := WorkerKey{"c1", "feat/x"}
	ids := func(runs []SubagentRun) []string {
		var out []string
		for _, r := range runs {
			out = append(out, r.ID)
		}
		return out
	}
	spawn := func(rows []string, i int) []string {
		return append(rows, sinkRow(digestBase+int64(i), w, sink, fmt.Sprintf(`{"subagent":"spawn","id":"r%d"}`, i)))
	}

	t.Run("evicts oldest finished before running", func(t *testing.T) {
		var rows []string
		for i := range 16 {
			rows = spawn(rows, i)
		}
		rows = append(rows, sinkRow(digestBase+100, w, sink, `{"subagent":"finish","id":"r3"}`))
		rows = spawn(rows, 16)
		got := ids(scanRows(t, rows...).Subagents[key])
		if len(got) != 16 || slices.Contains(got, "r3") || got[0] != "r0" || got[15] != "r16" {
			t.Errorf("ids = %v, want r3 evicted, r0 kept, r16 last", got)
		}
	})
	t.Run("all running evicts oldest", func(t *testing.T) {
		var rows []string
		for i := range 17 {
			rows = spawn(rows, i)
		}
		got := ids(scanRows(t, rows...).Subagents[key])
		if len(got) != 16 || got[0] != "r1" || got[15] != "r16" {
			t.Errorf("ids = %v, want r1..r16", got)
		}
	})
}

func TestScanSubagentsSinkRows(t *testing.T) {
	d := scanRows(t,
		statusRow(digestBase+1000, "worker:feat/x#s1", "working", "execute"),
		sinkRow(digestBase+2000, "worker:feat/x#s1", "subagent:c1", `{"subagent":"spawn","id":"t1"}`),
	)
	if len(d.Recent) != 1 || d.Recent[0].Kind != "status" {
		t.Errorf("recent = %+v, want only the status row", d.Recent)
	}
	if got := len(d.Workers[WorkerKey{"c1", "feat/x"}].Activity); got != 2 {
		t.Errorf("activity = %d, want 2 (sink row counted)", got)
	}
}

func TestScanSubagentsSession(t *testing.T) {
	key := WorkerKey{"c1", "feat/x"}
	d := scanRows(t,
		statusRow(digestBase+1000, "worker:feat/x#s1", "working", "execute"),
		sinkRow(digestBase+2000, "worker:feat/x#s1", "subagent:c1", `{"subagent":"spawn","id":"t1"}`),
		statusRow(digestBase+3000, "worker:feat/x#s2", "working", "execute"),
		sinkRow(digestBase+4000, "worker:feat/x#s2", "subagent:c1", `{"subagent":"spawn","id":"t2"}`),
		`{"ts":1794800005000,"crew_id":"c1","from":"worker:feat/x#s9","kind":"status","body":{"state":"working","source":"watchdog"}}`,
	)
	if got := d.Workers[key].Session; got != "s2" {
		t.Errorf("worker session = %q, want s2 (watchdog and msg rows excluded)", got)
	}
	runs := d.Subagents[key]
	if len(runs) != 2 || runs[0].Session != "s1" || runs[1].Session != "s2" {
		t.Errorf("run sessions = %+v, want s1, s2", runs)
	}
	bare := scanRows(t, sinkRow(digestBase, "worker:feat/y", "subagent:c1", `{"subagent":"spawn","id":"t"}`))
	if got := bare.Workers[WorkerKey{"c1", "feat/y"}].Session; got != "" {
		t.Errorf("session without suffix or status = %q, want empty", got)
	}
}

func TestNewRunClearsPriorSubagents(t *testing.T) {
	d := scanRows(t,
		statusRow(digestBase, "worker:feat/x#s1", "working", "execute"),
		sinkRow(digestBase+1000, "worker:feat/x#s1", "subagent:c1", `{"subagent":"spawn","id":"t1"}`),
		statusRow(digestBase+2000, "worker:feat/x#s1", "failed", "x"),
		statusRow(digestBase+3000, "worker:feat/x#s2", "working", "spec"),
	)
	if runs := d.Subagents[WorkerKey{"c1", "feat/x"}]; len(runs) != 0 {
		t.Errorf("old run's subagents survived a new run: %+v", runs)
	}
}

func TestScanSubagentsCloneIsolation(t *testing.T) {
	key := WorkerKey{"c1", "feat/x"}
	path := filepath.Join(t.TempDir(), "events.jsonl")
	row := sinkRow(digestBase, "worker:feat/x#s1", "subagent:c1", `{"subagent":"spawn","id":"t1"}`) + "\n"
	if err := os.WriteFile(path, []byte(row), 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewBusScanner(path)
	d, err := s.Scan(digestBase + 1000)
	if err != nil {
		t.Fatal(err)
	}
	d.Subagents[key][0].State = "mutated"
	d2, err := s.Scan(digestBase + 1000)
	if err != nil {
		t.Fatal(err)
	}
	if got := d2.Subagents[key][0].State; got != "running" {
		t.Errorf("state = %q after mutating a returned digest, want running", got)
	}
}

func TestScanSubagentsIncremental(t *testing.T) {
	key := WorkerKey{"c1", "feat/x"}
	path := filepath.Join(t.TempDir(), "events.jsonl")
	write := func(flag int, rows ...string) {
		t.Helper()
		f, err := os.OpenFile(path, flag, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = f.Close() }()
		if _, err := f.WriteString(strings.Join(rows, "\n") + "\n"); err != nil {
			t.Fatal(err)
		}
	}
	s := NewBusScanner(path)
	write(os.O_CREATE|os.O_WRONLY, sinkRow(digestBase, "worker:feat/x#s1", "subagent:c1", `{"subagent":"spawn","id":"t1"}`))
	d, err := s.Scan(digestBase + 1000)
	if err != nil || d.Subagents[key][0].State != "running" {
		t.Fatalf("first scan = %+v, %v", d.Subagents[key], err)
	}
	write(os.O_APPEND|os.O_WRONLY, sinkRow(digestBase+500, "worker:feat/x#s1", "subagent:c1", `{"subagent":"finish","id":"t1"}`))
	d, err = s.Scan(digestBase + 1000)
	if err != nil || d.Subagents[key][0].State != "done" || d.Subagents[key][0].EndTS != digestBase+500 {
		t.Fatalf("second scan = %+v, %v", d.Subagents[key], err)
	}
}

func rawRow(ts int64, crew, from, branch, kind string) string {
	m := map[string]any{"ts": ts, "from": from, "kind": kind, "body": map[string]any{"state": "working"}}
	if crew != "" {
		m["crew_id"] = crew
	}
	if branch != "" {
		m["branch"] = branch
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func TestRecentCrewAttribution(t *testing.T) {
	d := scanRows(t,
		rawRow(digestBase, "c1", "worker:feat/a#s1", "", "status"),
		rawRow(digestBase+1, "", "dispatcher:c2", "", "msg"),
		rawRow(digestBase+2, "", "crew", "feat/a", "reap"),
		rawRow(digestBase+3, "", "worker:feat/a#s1", "", "status"),
		rawRow(digestBase+4, "", "crew", "feat/none", "reap"),
		rawRow(digestBase+5, "", "someone", "", "msg"),
	)
	want := []string{"c1", "c2", "c1", "c1", "", ""}
	if len(d.Recent) != len(want) {
		t.Fatalf("recent = %d rows, want %d", len(d.Recent), len(want))
	}
	for i, w := range want {
		if d.Recent[i].Crew != w {
			t.Errorf("row %d crew = %q, want %q", i, d.Recent[i].Crew, w)
		}
	}
	if got := len(d.RecentByCrew["c1"]); got != 3 {
		t.Errorf("c1 ring = %d, want 3", got)
	}
	if got := len(d.RecentByCrew["c2"]); got != 1 {
		t.Errorf("c2 ring = %d, want 1", got)
	}
	if _, ok := d.RecentByCrew[""]; ok {
		t.Error("unattributed rows must not get a ring")
	}
}

func TestRecentByCrewBusyDoesNotEvictQuiet(t *testing.T) {
	rows := []string{}
	for i := range 3 {
		rows = append(rows, rawRow(digestBase+int64(i), "quiet", "worker:feat/q#s1", "", "status"))
	}
	for i := range 200 {
		rows = append(rows, rawRow(digestBase+100+int64(i), "busy", "worker:feat/b#s1", "", "status"))
	}
	d := scanRows(t, rows...)
	if got := len(d.RecentByCrew["quiet"]); got != 3 {
		t.Errorf("quiet ring = %d, want 3", got)
	}
	if got := len(d.RecentByCrew["busy"]); got != recentCap {
		t.Errorf("busy ring = %d, want %d", got, recentCap)
	}
	if got := d.RecentByCrew["busy"][0].TS; got != digestBase+100+200-recentCap {
		t.Errorf("busy ring keeps oldest row ts=%d", got)
	}
}

func TestRecentByCrewEvictsStalest(t *testing.T) {
	rows := []string{}
	// c10 is the stalest: its only row has the smallest TS, though it is written last.
	for i := range maxCrewRings {
		rows = append(rows, rawRow(digestBase+100+int64(i), fmt.Sprintf("c%d", i), "dispatcher:x", "", "msg"))
	}
	rows[10] = rawRow(digestBase, "c10", "dispatcher:x", "", "msg")
	rows = append(rows, rawRow(digestBase+500, "new", "dispatcher:x", "", "msg"))
	d := scanRows(t, rows...)
	if len(d.RecentByCrew) != maxCrewRings {
		t.Fatalf("rings = %d, want %d", len(d.RecentByCrew), maxCrewRings)
	}
	if _, ok := d.RecentByCrew["c10"]; ok {
		t.Error("stalest ring survived")
	}
	if _, ok := d.RecentByCrew["new"]; !ok {
		t.Error("new crew has no ring")
	}
}

func TestRecentCrewBranchMapIncremental(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	write := func(flag int, rows ...string) {
		t.Helper()
		f, err := os.OpenFile(path, flag, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = f.Close() }()
		if _, err := f.WriteString(strings.Join(rows, "\n") + "\n"); err != nil {
			t.Fatal(err)
		}
	}
	s := NewBusScanner(path)
	write(os.O_CREATE|os.O_WRONLY, rawRow(digestBase, "c1", "worker:feat/a#s1", "", "status"))
	if _, err := s.Scan(digestBase); err != nil {
		t.Fatal(err)
	}
	write(os.O_APPEND|os.O_WRONLY, rawRow(digestBase+1, "", "crew", "feat/a", "reap"))
	d, err := s.Scan(digestBase)
	if err != nil {
		t.Fatal(err)
	}
	if got := d.Recent[len(d.Recent)-1].Crew; got != "c1" {
		t.Errorf("reap crew after incremental scan = %q, want c1", got)
	}

	// Truncation drops the branch map along with the rest of the digest.
	write(os.O_TRUNC|os.O_WRONLY, rawRow(digestBase, "", "crew", "feat/a", "reap"))
	d, err = s.Scan(digestBase)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Recent) != 1 || d.Recent[0].Crew != "" || len(d.RecentByCrew) != 0 {
		t.Errorf("after truncate: recent=%+v byCrew=%v", d.Recent, d.RecentByCrew)
	}
}

func TestRecentByCrewCloneIsolation(t *testing.T) {
	_, s := digestFixture(t)
	d, _ := s.Scan(digestBase + 10000)
	for k := range d.RecentByCrew {
		d.RecentByCrew[k][0].Detail = "mutated"
	}
	d2, _ := s.Scan(digestBase + 10000)
	for k, v := range d2.RecentByCrew {
		if v[0].Detail == "mutated" {
			t.Errorf("ring %q aliases scanner state", k)
		}
	}
	if len(d2.RecentByCrew) == 0 {
		t.Fatal("fixture produced no rings")
	}
}
