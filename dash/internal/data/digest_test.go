package data

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
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
