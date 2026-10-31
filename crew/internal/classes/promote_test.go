package classes

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/drift"
	"github.com/noamsto/dispatcher/crew/internal/lock"
)

const (
	hourMS          = 3_600_000.0
	catalogWithHead = `{"codex":{"models":[{"id":"gpt-5.6-luna"},{"id":"gpt-5.6-terra"},{"id":"gpt-5.7-terra"},{"id":"gpt-5.8-terra"},{"id":"gpt-5.6-sol"},{"id":"gpt-5.7-sol"}]}}`
)

var fixedNow = time.UnixMilli(1793429446454)

type promoteFixture struct {
	t    *testing.T
	vars map[string]string
	env  Env
}

// newPromoteFixture builds an Env over the shipped resolver and defaults, a
// private XDG_DATA_HOME holding catalog, and a temp bus. patch edits the
// settings document before it is served.
func newPromoteFixture(t *testing.T, catalog string, patch func(map[string]any)) *promoteFixture {
	t.Helper()
	if _, err := os.Stat(resolveScript); err != nil {
		drift.Missing(t, resolveScript, err)
	}
	settings := readDefaults(t)
	if patch != nil {
		var doc map[string]any
		if err := json.Unmarshal(settings, &doc); err != nil {
			t.Fatal(err)
		}
		patch(doc)
		var err error
		if settings, err = json.Marshal(doc); err != nil {
			t.Fatal(err)
		}
	}
	f := &promoteFixture{t: t, vars: map[string]string{"XDG_DATA_HOME": t.TempDir()}}
	getenv := func(k string) string { return f.vars[k] }
	if err := os.MkdirAll(DataDir(getenv), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(CatalogPath(getenv), []byte(catalog), 0o644); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	f.env = Env{
		Getenv:   getenv,
		Settings: func() ([]byte, error) { return settings, nil },
		Bash:     "bash",
		Script:   resolveScript,
		Now:      func() time.Time { return fixedNow },
		CrewID:   "c1",
		Paths:    bus.Paths{Common: root + "/.git", Dir: root + "/.git/crew", Log: root + "/.git/crew/events.jsonl"},
	}
	return f
}

func (f *promoteFixture) statePath() string { return StatePath(f.env.Getenv) }

func (f *promoteFixture) writeState(text string) {
	f.t.Helper()
	if err := os.WriteFile(f.statePath(), []byte(text), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *promoteFixture) state() State {
	f.t.Helper()
	st, usable, warnings := Load(f.statePath())
	if !usable || len(warnings) > 0 {
		f.t.Fatalf("state not usable (%v): %v", usable, warnings)
	}
	return st
}

// stateBytes is the raw file, or "" when absent, for unchanged-state checks.
func (f *promoteFixture) stateBytes() string {
	f.t.Helper()
	data, err := os.ReadFile(f.statePath())
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		f.t.Fatal(err)
	}
	return string(data)
}

// busRows decodes the bus log; an absent log has no rows.
func (f *promoteFixture) busRows() []map[string]any {
	f.t.Helper()
	file, err := os.Open(f.env.Paths.Log)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	var out []map[string]any
	sc := bufio.NewScanner(file)
	for sc.Scan() {
		var row map[string]any
		if err := json.Unmarshal(sc.Bytes(), &row); err != nil {
			f.t.Fatalf("bus row %q: %v", sc.Text(), err)
		}
		out = append(out, row)
	}
	return out
}

func rowsOfKind(rows []map[string]any, kind string) []map[string]any {
	var out []map[string]any
	for _, r := range rows {
		if r["kind"] == kind {
			out = append(out, r)
		}
	}
	return out
}

// runs builds n owned settled codex rows for model on tier; the first merged of
// them merged.
func runs(n int, model, tier string, merged int, rework, costMS float64) []any {
	out := make([]any, 0, n)
	for i := range n {
		state := "CLOSED"
		if i < merged {
			state = "MERGED"
		}
		out = append(out, map[string]any{
			"engine": "codex", "model": model, "tier": tier,
			"owns_pr": true, "pr_url": "https://github.com/o/r/pull/1", "pr_state": state,
			"rework_count": rework, "cost_proxy": costMS,
		})
	}
	return out
}

// incumbentRows is the incumbent the fixtures share: 6 runs, 5 merged, rework 0.5, 1h each.
func incumbentRows() []any { return runs(6, "gpt-5.6-terra", "standard", 5, 0.5, hourMS) }

// eligibleRows add a perfect 6-run trial to incumbentRows.
func eligibleRows() []any {
	return append(runs(6, "gpt-5.7-terra", "standard", 6, 0, hourMS), incumbentRows()...)
}

func findVerdict(t *testing.T, vs []Verdict, engine, class string) Verdict {
	t.Helper()
	for _, v := range vs {
		if v.Engine == engine && v.Class == class {
			return v
		}
	}
	t.Fatalf("no verdict for %s %s in %+v", engine, class, vs)
	return Verdict{}
}

func findCandidate(t *testing.T, v Verdict, model string) Candidate {
	t.Helper()
	for _, c := range v.Candidates {
		if c.Model == model {
			return c
		}
	}
	t.Fatalf("no candidate %s in %+v", model, v.Candidates)
	return Candidate{}
}

func hasReason(c Candidate, sub string) bool {
	return slices.ContainsFunc(c.Reasons, func(r string) bool { return strings.Contains(r, sub) })
}

func TestCheckPromotesOnce(t *testing.T) {
	f := newPromoteFixture(t, catalogJSON, nil)
	rows := eligibleRows()
	var stderr bytes.Buffer
	if got := Check(rows, f.env, &stderr); got != 1 {
		t.Fatalf("Check = %d, want 1; stderr %q", got, stderr.String())
	}
	e := f.state()["codex"]["standard"]
	if e.Default != "gpt-5.7-terra" {
		t.Errorf("default = %q, want gpt-5.7-terra", e.Default)
	}
	if len(e.History) != 1 || e.History[0].From != "gpt-5.6-terra" || e.History[0].To != "gpt-5.7-terra" || e.History[0].By != "auto" {
		t.Errorf("history = %+v, want one auto gpt-5.6-terra → gpt-5.7-terra", e.History)
	}
	logged := f.busRows()
	msgs, decisions := rowsOfKind(logged, "msg"), rowsOfKind(logged, "decision")
	if len(msgs) != 1 || len(decisions) != 1 {
		t.Fatalf("bus = %v, want one msg and one decision", logged)
	}
	m := msgs[0]
	if m["from"] != "crew:class" || m["to"] != "dispatcher:c1" || m["crew_id"] != "c1" {
		t.Errorf("msg = %v", m)
	}
	body, _ := m["body"].(string)
	want := "class codex standard: gpt-5.6-terra → gpt-5.7-terra (auto): runs 6 vs 6, merge 100% ≥ 83%, rework 0.0 ≤ 0.5, burn/merged 1.00h ≤ 1.25×1.20h; undo: crew class revert codex standard"
	if body != want {
		t.Errorf("msg body = %q\nwant        %q", body, want)
	}
	if decisions[0]["body"] != body || decisions[0]["from"] != "dispatcher:c1" {
		t.Errorf("decision = %v, want the msg body from dispatcher:c1", decisions[0])
	}

	stderr.Reset()
	if got := Check(rows, f.env, &stderr); got != 0 {
		t.Errorf("second Check = %d, want 0; stderr %q", got, stderr.String())
	}
	if n := len(f.busRows()); n != 2 {
		t.Errorf("bus rows after the second Check = %d, want 2", n)
	}
}

// costedOnly drops cost_proxy from every row after the first keep, as the
// ledger does for a model the burn table does not price.
func costedOnly(rows []any, keep int) []any {
	for i, r := range rows {
		if i >= keep {
			r.(map[string]any)["cost_proxy"] = nil
		}
	}
	return rows
}

func TestCheckShortfalls(t *testing.T) {
	tests := []struct {
		name   string
		trial  []any
		inc    []any // nil → incumbentRows
		reason string
	}{
		{"runs", runs(4, "gpt-5.7-terra", "standard", 4, 0, hourMS), nil, "runs 4 < 5"},
		{"merge", runs(6, "gpt-5.7-terra", "standard", 4, 0, hourMS), nil, "merge 67% < incumbent 83%"},
		{"rework", runs(6, "gpt-5.7-terra", "standard", 6, 1, hourMS), nil, "rework 1.0 > incumbent 0.5"},
		{"burn", runs(6, "gpt-5.7-terra", "standard", 6, 0, 2*hourMS), nil, "burn/merged 2.00h > 1.25 × incumbent 1.20h"},
		{"uncosted", costedOnly(runs(6, "gpt-5.7-terra", "standard", 6, 0, hourMS), 0), nil, "burn unmeasured (costed 0/6)"},
		{"partly costed", costedOnly(runs(6, "gpt-5.7-terra", "standard", 6, 0, hourMS), 1), nil, "burn unmeasured (costed 1/6)"},
		{"incumbent uncosted", runs(6, "gpt-5.7-terra", "standard", 6, 0, hourMS), costedOnly(incumbentRows(), 2), "incumbent burn unmeasured (costed 2/6)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newPromoteFixture(t, catalogJSON, nil)
			inc := tc.inc
			if inc == nil {
				inc = incumbentRows()
			}
			rows := append(tc.trial, inc...)
			vs, _, err := Evaluate(rows, f.env)
			if err != nil {
				t.Fatal(err)
			}
			v := findVerdict(t, vs, "codex", "standard")
			if v.To != "" {
				t.Errorf("verdict promotes %s", v.To)
			}
			if c := findCandidate(t, v, "gpt-5.7-terra"); !hasReason(c, tc.reason) {
				t.Errorf("reasons = %q, want one containing %q", c.Reasons, tc.reason)
			}
			var stderr bytes.Buffer
			if got := Check(rows, f.env, &stderr); got != 0 {
				t.Errorf("Check = %d, want 0", got)
			}
			if s := f.stateBytes(); s != "" {
				t.Errorf("state written: %s", s)
			}
			if rows := f.busRows(); len(rows) != 0 {
				t.Errorf("bus rows: %v", rows)
			}
		})
	}
}

func TestCheckNeverPromotesCrossClass(t *testing.T) {
	// The standard tier admits cheap models too, so a cheaper trial is as
	// foreign to the class as a pricier one.
	for _, model := range []string{"gpt-5.7-sol", "gpt-5.6-luna"} {
		t.Run(model, func(t *testing.T) {
			f := newPromoteFixture(t, catalogJSON, nil)
			rows := append(runs(6, model, "standard", 6, 0, hourMS), incumbentRows()...)
			vs, _, err := Evaluate(rows, f.env)
			if err != nil {
				t.Fatal(err)
			}
			v := findVerdict(t, vs, "codex", "standard")
			c := findCandidate(t, v, model)
			if v.To != "" || len(c.Reasons) == 0 || c.Reasons[0] != "not class standard" {
				t.Errorf("To = %q, reasons = %q; want no promotion, first reason 'not class standard'", v.To, c.Reasons)
			}
			if got := Check(rows, f.env, &bytes.Buffer{}); got != 0 || f.stateBytes() != "" {
				t.Errorf("Check = %d, state %q; want 0 and no state", got, f.stateBytes())
			}
			if rows := f.busRows(); len(rows) != 0 {
				t.Errorf("bus rows: %v", rows)
			}
		})
	}
}

func TestCheckExactlyOnceUnderTheLock(t *testing.T) {
	f := newPromoteFixture(t, catalogJSON, nil)
	promoted := State{"codex": {}}
	var e Entry
	e.Default = "gpt-5.7-terra"
	e.Push(Change{From: "gpt-5.6-terra", To: "gpt-5.7-terra", TS: fixedNow.UnixMilli(), By: "auto", Why: "another sweep"})
	promoted["codex"]["standard"] = e
	calls := 0
	beforeApply = func() {
		calls++
		if err := Save(f.statePath(), promoted); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { beforeApply = nil })
	var stderr bytes.Buffer
	if got := Check(eligibleRows(), f.env, &stderr); got != 0 {
		t.Errorf("Check = %d, want 0: another sweep promoted first; stderr %q", got, stderr.String())
	}
	if calls != 1 {
		t.Errorf("beforeApply ran %d times, want 1", calls)
	}
	if got := f.state()["codex"]["standard"]; !reflect.DeepEqual(got, e) {
		t.Errorf("entry = %+v, want the other sweep's %+v", got, e)
	}
	if rows := f.busRows(); len(rows) != 0 {
		t.Errorf("bus rows: %v", rows)
	}
}

func TestUnusableStateRefusesWrites(t *testing.T) {
	f := newPromoteFixture(t, catalogJSON, nil)
	const garbled = `{"codex":{"standard":{"default":"gpt-5.6-terra","vetoed":["gpt-5.7-terra"]}}`
	f.writeState(garbled)
	var stderr bytes.Buffer
	if got := Check(eligibleRows(), f.env, &stderr); got != 0 {
		t.Errorf("Check = %d, want 0", got)
	}
	refusal := "class-state is unusable ("
	tail := "); fix or remove " + f.statePath() + "\n"
	if n := strings.Count(stderr.String(), refusal); n != 1 || !strings.Contains(stderr.String(), tail) {
		t.Errorf("Check stderr = %q, want the refusal once, ending %q", stderr.String(), tail)
	}
	if s := f.stateBytes(); s != garbled {
		t.Errorf("state rewritten to %q", s)
	}
	stderr.Reset()
	if got := Promote("codex", "standard", "gpt-5.7-terra", f.env, &stderr); got != 1 {
		t.Errorf("Promote = %d, want 1", got)
	}
	if !strings.HasPrefix(stderr.String(), "crew: class: "+refusal) || !strings.HasSuffix(stderr.String(), tail) {
		t.Errorf("Promote stderr = %q, want the refusal", stderr.String())
	}
	if s := f.stateBytes(); s != garbled {
		t.Errorf("state rewritten to %q", s)
	}
	if rows := f.busRows(); len(rows) != 0 {
		t.Errorf("bus rows: %v", rows)
	}
	lines := ReportLines(eligibleRows(), f.env)
	reportLine(t, lines)
	if !slices.ContainsFunc(lines, func(l string) bool { return strings.HasPrefix(l, "  warning: class-state: ") }) {
		t.Errorf("report = %q, want the class-state warning", lines)
	}
}

func TestPartlyDroppedStateRefusesWrites(t *testing.T) {
	f := newPromoteFixture(t, catalogJSON, nil)
	const pruned = `{"codex":{"standard":{"default":null,"vetoed":["gpt-5.7-terra"],"history":[{"from":"a","to":"b"}]}}}`
	f.writeState(pruned)
	var stderr bytes.Buffer
	if got := Check(eligibleRows(), f.env, &stderr); got != 0 {
		t.Errorf("Check = %d, want 0", got)
	}
	refusal := "class-state is unusable ("
	tail := "); fix or remove " + f.statePath() + "\n"
	if n := strings.Count(stderr.String(), refusal); n != 1 || !strings.Contains(stderr.String(), tail) {
		t.Errorf("Check stderr = %q, want the refusal once, ending %q", stderr.String(), tail)
	}
	stderr.Reset()
	if got := Promote("codex", "standard", "gpt-5.7-terra", f.env, &stderr); got != 1 {
		t.Errorf("Promote = %d, want 1", got)
	}
	if !strings.HasPrefix(stderr.String(), "crew: class: "+refusal) {
		t.Errorf("Promote stderr = %q, want the refusal", stderr.String())
	}
	if s := f.stateBytes(); s != pruned {
		t.Errorf("state rewritten to %q", s)
	}
	if rows := f.busRows(); len(rows) != 0 {
		t.Errorf("bus rows: %v", rows)
	}
}

func TestUnusableStateAutoOffProposesNothing(t *testing.T) {
	f := newPromoteFixture(t, catalogJSON, func(doc map[string]any) {
		doc["classes"].(map[string]any)["codex"].(map[string]any)["autoPromote"] = false
	})
	f.writeState(`{"codex":{"standard":{"default":"gpt-5.6-terra","vetoed":["gpt-5.7-terra"]}}`)
	var stderr bytes.Buffer
	if got := Check(eligibleRows(), f.env, &stderr); got != 0 {
		t.Errorf("Check = %d, want 0", got)
	}
	if strings.Contains(stderr.String(), "auto-promotion is off") {
		t.Errorf("stderr = %q, want no propose line", stderr.String())
	}
	if n := strings.Count(stderr.String(), "crew rate: class check skipped: class-state is unusable ("); n != 1 {
		t.Errorf("stderr = %q, want the skip line once", stderr.String())
	}
}

func TestCandidatesCapped(t *testing.T) {
	f := newPromoteFixture(t, catalogJSON, nil)
	long := strings.Repeat("j", 250)
	// The wrapper logs any resolver run whose state file names the long id.
	hits := filepath.Join(t.TempDir(), "hits")
	wrapper := filepath.Join(t.TempDir(), "bash")
	script := "#!/bin/sh\nif grep -q " + long[:50] + " \"$5\" 2>/dev/null; then echo hit >>'" + hits + "'; fi\nexec bash \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	f.env.Bash = wrapper
	rows := eligibleRows()
	rows = append(rows, runs(8, long, "standard", 0, 0, hourMS)...)
	for i := range 14 {
		rows = append(rows, runs(7, "junk-"+strconv.Itoa(10+i), "standard", 0, 0, hourMS)...)
	}
	vs, _, err := Evaluate(rows, f.env)
	if err != nil {
		t.Fatal(err)
	}
	v := findVerdict(t, vs, "codex", "standard")
	perTier := map[string]int{}
	for _, c := range v.Candidates {
		perTier[c.Tier]++
	}
	for tier, n := range perTier {
		if n > 10 {
			t.Errorf("tier %s has %d candidates, want at most 10", tier, n)
		}
	}
	if v.Omitted != 6 {
		t.Errorf("Omitted = %d, want 6", v.Omitted)
	}
	findCandidate(t, v, "gpt-5.7-terra")
	if c := findCandidate(t, v, long); len(c.Reasons) == 0 || c.Reasons[0] != "not class standard" {
		t.Errorf("long id reasons = %q, want first 'not class standard'", c.Reasons)
	}
	if _, err := os.Stat(hits); err == nil {
		t.Error("the resolver ran for an id over 200 bytes")
	}
	if !slices.Contains(ReportLines(rows, f.env), "  codex standard: 6 more rated models not shown") {
		t.Errorf("report lacks the omitted line: %q", ReportLines(rows, f.env))
	}
}

func TestEvaluateNotTheHead(t *testing.T) {
	f := newPromoteFixture(t, catalogWithHead, nil)
	vs, _, err := Evaluate(eligibleRows(), f.env)
	if err != nil {
		t.Fatal(err)
	}
	v := findVerdict(t, vs, "codex", "standard")
	c := findCandidate(t, v, "gpt-5.7-terra")
	if v.To != "" {
		t.Errorf("verdict promotes %s", v.To)
	}
	if !c.HeadOnly || !reflect.DeepEqual(c.Reasons, []string{"not the trial head (gpt-5.8-terra)"}) {
		t.Errorf("HeadOnly = %v, reasons = %q; want only the trial-head reason", c.HeadOnly, c.Reasons)
	}
}

func TestEvaluateIncumbentWithoutRuns(t *testing.T) {
	f := newPromoteFixture(t, catalogJSON, nil)
	vs, _, err := Evaluate(runs(6, "gpt-5.7-terra", "standard", 6, 0, hourMS), f.env)
	if err != nil {
		t.Fatal(err)
	}
	v := findVerdict(t, vs, "codex", "standard")
	c := findCandidate(t, v, "gpt-5.7-terra")
	if v.To != "" || !reflect.DeepEqual(c.Reasons, []string{"incumbent has no rated runs"}) {
		t.Errorf("To = %q, reasons = %q; want only 'incumbent has no rated runs'", v.To, c.Reasons)
	}
}

func TestCheckForgedIncumbentWarnsOnce(t *testing.T) {
	f := newPromoteFixture(t, catalogJSON, nil)
	f.writeState(`{"codex":{"cheap":{"default":"gpt-5.6-sol"}}}`)
	// The eligible rows make Check re-evaluate under the lock, whose warnings must
	// not print again.
	rows := append(runs(6, "gpt-5.6-luna", "trivial", 6, 0, hourMS), eligibleRows()...)
	vs, warnings, err := Evaluate(rows, f.env)
	if err != nil {
		t.Fatal(err)
	}
	if v := findVerdict(t, vs, "codex", "cheap"); v.From != "gpt-5.6-luna" || v.To != "" {
		t.Errorf("cheap verdict From = %q, To = %q; want the seed gpt-5.6-luna and no promotion", v.From, v.To)
	}
	for _, v := range vs {
		for _, c := range v.Candidates {
			if c.Incumbent == "gpt-5.6-sol" && c.Tier == "trivial" {
				t.Errorf("candidate %+v treats gpt-5.6-sol as the trivial incumbent", c)
			}
		}
	}
	if !containsWarning(warnings, "is not valid for the class") {
		t.Errorf("warnings = %q, want the resolver's invalid-default line", warnings)
	}
	var stderr bytes.Buffer
	if got := Check(rows, f.env, &stderr); got != 1 {
		t.Fatalf("Check = %d, want 1 (the standard trial); stderr %q", got, stderr.String())
	}
	if n := strings.Count(stderr.String(), "is not valid for the class"); n != 1 {
		t.Errorf("warning printed %d times, want once; stderr %q", n, stderr.String())
	}
	if d := f.state()["codex"]["cheap"].Default; d != "gpt-5.6-sol" {
		t.Errorf("cheap default rewritten to %q; the writer keeps the recorded value", d)
	}
}

func TestRevert(t *testing.T) {
	f := newPromoteFixture(t, catalogJSON, nil)
	rows := eligibleRows()
	if got := Check(rows, f.env, &bytes.Buffer{}); got != 1 {
		t.Fatalf("Check = %d, want 1", got)
	}
	var stderr bytes.Buffer
	if got := Revert("codex", "standard", f.env, &stderr); got != 0 {
		t.Fatalf("Revert = %d, want 0; stderr %q", got, stderr.String())
	}
	e := f.state()["codex"]["standard"]
	if e.Default != "gpt-5.6-terra" || len(e.History) != 0 || !reflect.DeepEqual(e.Vetoed, []string{"gpt-5.7-terra"}) {
		t.Errorf("entry = %+v, want default gpt-5.6-terra, no history, gpt-5.7-terra vetoed", e)
	}
	logged := f.busRows()
	msgs, decisions := rowsOfKind(logged, "msg"), rowsOfKind(logged, "decision")
	if len(msgs) != 2 || len(decisions) != 2 {
		t.Fatalf("bus = %v, want two msgs and two decisions", logged)
	}
	want := "class codex standard: gpt-5.7-terra → gpt-5.6-terra (revert); gpt-5.7-terra vetoed for auto-promotion"
	if msgs[1]["body"] != want || decisions[1]["body"] != want {
		t.Errorf("revert bodies = %q / %q, want %q", msgs[1]["body"], decisions[1]["body"], want)
	}

	if got := Check(rows, f.env, &bytes.Buffer{}); got != 0 {
		t.Errorf("Check after revert = %d, want 0", got)
	}
	vs, _, err := Evaluate(rows, f.env)
	if err != nil {
		t.Fatal(err)
	}
	if c := findCandidate(t, findVerdict(t, vs, "codex", "standard"), "gpt-5.7-terra"); !hasReason(c, "vetoed") {
		t.Errorf("reasons = %q, want vetoed", c.Reasons)
	}

	stderr.Reset()
	if got := Revert("codex", "standard", f.env, &stderr); got != 1 || !strings.Contains(stderr.String(), "no previous default") {
		t.Errorf("second Revert = %d, stderr %q; want 1 and 'no previous default'", got, stderr.String())
	}
}

func TestAutoPromoteOff(t *testing.T) {
	f := newPromoteFixture(t, catalogJSON, func(doc map[string]any) {
		doc["classes"].(map[string]any)["codex"].(map[string]any)["autoPromote"] = false
	})
	var stderr bytes.Buffer
	if got := Check(eligibleRows(), f.env, &stderr); got != 0 {
		t.Errorf("Check = %d, want 0", got)
	}
	if !strings.Contains(stderr.String(), "crew rate: class codex standard: gpt-5.7-terra is eligible; auto-promotion is off — crew class promote codex standard gpt-5.7-terra") {
		t.Errorf("stderr = %q, want the propose line", stderr.String())
	}
	if s := f.stateBytes(); s != "" {
		t.Errorf("state written: %s", s)
	}
	stderr.Reset()
	if got := Promote("codex", "standard", "gpt-5.7-terra", f.env, &stderr); got != 0 {
		t.Fatalf("Promote = %d, want 0; stderr %q", got, stderr.String())
	}
	e := f.state()["codex"]["standard"]
	if e.Default != "gpt-5.7-terra" || len(e.History) != 1 || e.History[0].By != "promote" || e.History[0].From != "gpt-5.6-terra" {
		t.Errorf("entry = %+v, want a promote from gpt-5.6-terra", e)
	}
	logged := f.busRows()
	if len(rowsOfKind(logged, "msg")) != 1 || len(rowsOfKind(logged, "decision")) != 1 {
		t.Errorf("bus = %v, want one msg and one decision", logged)
	}
	want := "class codex standard: gpt-5.6-terra → gpt-5.7-terra (promote); undo: crew class revert codex standard"
	if got := rowsOfKind(logged, "msg")[0]["body"]; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
}

func TestPromoteRefusals(t *testing.T) {
	tests := []struct {
		name, class, id, want string
	}{
		{"cross class", "standard", "gpt-5.7-sol", "gpt-5.7-sol is not valid for class standard"},
		{"unknown class", "bogus", "gpt-5.7-terra", "engine codex has no class bogus"},
		{"current default", "standard", "gpt-5.6-terra", "gpt-5.6-terra is already the codex standard default"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newPromoteFixture(t, catalogJSON, nil)
			var stderr bytes.Buffer
			if got := Promote("codex", tc.class, tc.id, f.env, &stderr); got != 1 {
				t.Errorf("Promote = %d, want 1", got)
			}
			if want := "crew: class: " + tc.want + "\n"; stderr.String() != want {
				t.Errorf("stderr = %q, want %q", stderr.String(), want)
			}
			if s := f.stateBytes(); s != "" {
				t.Errorf("state written: %s", s)
			}
			if rows := f.busRows(); len(rows) != 0 {
				t.Errorf("bus rows: %v", rows)
			}
		})
	}
}

func TestNoCrewID(t *testing.T) {
	f := newPromoteFixture(t, catalogJSON, nil)
	f.env.CrewID = ""
	var stderr bytes.Buffer
	if got := Check(eligibleRows(), f.env, &stderr); got != 0 {
		t.Errorf("Check = %d, want 0", got)
	}
	if lines := strings.Count(stderr.String(), "\n"); lines != 1 || !strings.Contains(stderr.String(), "no crew id") {
		t.Errorf("Check stderr = %q, want one 'no crew id' line", stderr.String())
	}
	stderr.Reset()
	if got := Promote("codex", "standard", "gpt-5.7-terra", f.env, &stderr); got != 1 {
		t.Errorf("Promote = %d, want 1", got)
	}
	if want := "crew: class: no crew id — run it from the dispatcher pane or set CREW_ID=<crew id>\n"; stderr.String() != want {
		t.Errorf("Promote stderr = %q, want %q", stderr.String(), want)
	}
	if s := f.stateBytes(); s != "" {
		t.Errorf("state written: %s", s)
	}
	if rows := f.busRows(); len(rows) != 0 {
		t.Errorf("bus rows: %v", rows)
	}
}

func TestLockBusy(t *testing.T) {
	f := newPromoteFixture(t, catalogJSON, nil)
	dir := filepath.Join(DataDir(f.env.Getenv), LockName)
	if !lock.Acquire(dir, "test") {
		t.Fatal("could not take the lock")
	}
	defer lock.Release(dir)
	var stderr bytes.Buffer
	if got := Check(eligibleRows(), f.env, &stderr); got != 0 {
		t.Errorf("Check = %d, want 0", got)
	}
	if lines := strings.Count(stderr.String(), "\n"); lines != 1 || !strings.Contains(stderr.String(), "busy") {
		t.Errorf("Check stderr = %q, want one busy line", stderr.String())
	}
	stderr.Reset()
	if got := Promote("codex", "standard", "gpt-5.7-terra", f.env, &stderr); got != 1 {
		t.Errorf("Promote = %d, want 1", got)
	}
	if want := "crew: class: class-state is busy; retry\n"; stderr.String() != want {
		t.Errorf("Promote stderr = %q, want %q", stderr.String(), want)
	}
	if s := f.stateBytes(); s != "" {
		t.Errorf("state written: %s", s)
	}
}

func TestPromoteWritesOnlyTheChangedEntry(t *testing.T) {
	f := newPromoteFixture(t, catalogJSON, nil)
	if got := Promote("codex", "standard", "gpt-5.7-terra", f.env, &bytes.Buffer{}); got != 0 {
		t.Fatalf("Promote = %d, want 0", got)
	}
	st := f.state()
	if len(st) != 1 || len(st["codex"]) != 1 {
		t.Fatalf("state = %+v, want only codex.standard", st)
	}
	e := st["codex"]["standard"]
	if e.Default != "gpt-5.7-terra" || len(e.History) != 1 || e.History[0].From != "gpt-5.6-terra" || e.History[0].To != "gpt-5.7-terra" {
		t.Errorf("codex standard = %+v, want gpt-5.7-terra from gpt-5.6-terra", e)
	}
}

func TestPromoteLeavesOtherClassesFollowingSettings(t *testing.T) {
	f := newPromoteFixture(t, catalogJSON, nil)
	if got := Promote("codex", "standard", "gpt-5.7-terra", f.env, &bytes.Buffer{}); got != 0 {
		t.Fatalf("Promote = %d, want 0", got)
	}
	var doc map[string]any
	if err := json.Unmarshal(readDefaults(t), &doc); err != nil {
		t.Fatal(err)
	}
	doc["modelMap"].(map[string]any)["codex"].(map[string]any)["deep"].(map[string]any)["default"] = "gpt-5.7-sol"
	edited, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	s, err := ParseSettings(edited)
	if err != nil {
		t.Fatal(err)
	}
	res, err := f.env.resolver(s).Resolve("codex", "deep", f.statePath())
	if err != nil {
		t.Fatal(err)
	}
	if res.Default != "gpt-5.7-sol" || len(res.Warnings) != 0 {
		t.Errorf("deep resolves %q (warnings %v), want the edited seed gpt-5.7-sol", res.Default, res.Warnings)
	}
}

func TestWorkerRefusal(t *testing.T) {
	f := newPromoteFixture(t, catalogJSON, nil)
	if got := WorkerRefusal(f.env.Getenv); got != "" {
		t.Errorf("WorkerRefusal without CREW_WORKER_ID = %q, want empty", got)
	}
	f.vars["CREW_WORKER_ID"] = "worker:x"
	const want = "crew: class: refusing to run with CREW_WORKER_ID set"
	if got := WorkerRefusal(f.env.Getenv); got != want {
		t.Errorf("WorkerRefusal = %q, want %q", got, want)
	}
	var stderr bytes.Buffer
	if got := Check(eligibleRows(), f.env, &stderr); got != 0 || stderr.Len() != 0 {
		t.Errorf("Check = %d, stderr %q; want 0 and silence", got, stderr.String())
	}
	for name, run := range map[string]func() int{
		"promote": func() int { return Promote("codex", "standard", "gpt-5.7-terra", f.env, &stderr) },
		"revert":  func() int { return Revert("codex", "standard", f.env, &stderr) },
	} {
		stderr.Reset()
		if got := run(); got != 1 || stderr.String() != want+"\n" {
			t.Errorf("%s = %d, stderr %q; want 1 and the refusal", name, got, stderr.String())
		}
	}
	if s := f.stateBytes(); s != "" {
		t.Errorf("state written: %s", s)
	}
	if rows := f.busRows(); len(rows) != 0 {
		t.Errorf("bus rows: %v", rows)
	}
}

func TestCleanID(t *testing.T) {
	long := strings.Repeat("é", 70) // 140 bytes
	tests := []struct{ in, want string }{
		{"gpt-5.7-terra", "gpt-5.7-terra"},
		{"a\nb\x1bc", "a?b?c"},
		{"a" + string(rune(0x200b)) + "b", `a\u{200B}b`},
		{long, strings.Repeat("é", 60)},
	}
	for _, tc := range tests {
		if got := cleanID(tc.in); got != tc.want {
			t.Errorf("cleanID(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
