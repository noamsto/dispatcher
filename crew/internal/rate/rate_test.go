package rate

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/git/gittest"
)

// rowA is one swept, settled run of acme/widgets; rowB is another repo's row,
// invisible when scoped. Together they exercise the fold's input, the repo
// scope, and the pooled footer. The expected table text below is jq's own
// output for this fixture (the bash-vs-Go diff in the PR pins the same
// equality on the bats suite's worked example).
const (
	rowA = `{"repo":"acme/widgets","run_id":"r1","engine":"claude","model":"opus","tier":"deep","effort":"high","outcome":"merged","reached_pr":true,"time_to_pr_ms":100,"pr_state":"MERGED","time_to_merge_ms":200,"rework_count":0,"review_high":null,"review_mode":"high","review_rounds":0,"blocked_count":0,"watchdog_blocked_count":0,"first_ci_green":true,"unresolved_notes":0,"reverted":false,"cost_proxy":100,"swept_at":1}`
	rowB = `{"repo":"other/repo","run_id":"r2","engine":"pi","model":"flash","tier":"trivial","outcome":"done","reached_pr":false,"pr_state":null,"blocked_count":0,"watchdog_blocked_count":0,"swept_at":1}`

	// headerScoped is padded to the widths the scoped single-row table
	// produces; headerBare is the header-alone render of an empty store,
	// where only the header names set the widths.
	headerScoped = "engine  model  tier   n  inc  run  pend   pr%  merge%  ttpr  ttmerge  rework  high  rounds  blocked   ci1  notes  rev  cost"
	headerBare   = "engine  model  tier  n  inc  run  pend  pr%  merge%  ttpr  ttmerge  rework  high  rounds  blocked  ci1  notes  rev  cost"
)

// fakeGit answers the two probes the arm makes; an empty return stands in for
// git's empty output when the key or the repo is missing.
func fakeGit(origin, toplevel string) *gittest.Fake {
	return &gittest.Fake{Out: map[string]string{
		"-C /repo config --get remote.origin.url": origin,
		"-C /repo rev-parse --show-toplevel":      toplevel,
	}}
}

// run points Run at a fresh store path; a nil store leaves the file unwritten,
// so the missing-store case never reads the caller's real XDG store.
func run(t *testing.T, store *string, origin, toplevel string, req Request) (string, string, int) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ratings.jsonl")
	if store != nil {
		if err := os.WriteFile(path, []byte(*store), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var out, errb bytes.Buffer
	code := Run(req, bus.Paths{Log: filepath.Join(t.TempDir(), "events.jsonl")}, "/repo",
		&out, &errb, Options{StorePath: path, Git: fakeGit(origin, toplevel)})
	return out.String(), errb.String(), code
}

func ptr(s string) *string { return &s }

// TestModeRefusals pins the lines an agent reads when it hands `crew rate` a
// combination of modes it cannot run.
func TestModeRefusals(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  Request
		want string
	}{
		{"json alone", Request{JSON: true}, Usage},
		{"pooled alone", Request{Pooled: true}, Usage},
		{"sweep-all report", Request{SweepAll: true, Report: true}, Usage},
		{"sweep-all json", Request{SweepAll: true, JSON: true}, Usage},
		{"sweep-all pooled", Request{SweepAll: true, Pooled: true}, Usage},
		{"root without sweep-all", Request{Report: true, Roots: []string{"/x"}}, "crew: rate --root needs --sweep-all"},
	} {
		_, errb, code := run(t, ptr(""), "https://github.com/acme/widgets.git", "", tc.req)
		if code != 1 {
			t.Fatalf("%s: exit %d, want 1 (stderr %q)", tc.name, code, errb)
		}
		if strings.Count(errb, "\n") != 1 || strings.TrimSuffix(errb, "\n") != tc.want {
			t.Errorf("%s: stderr = %q, want %q", tc.name, errb, tc.want)
		}
	}
}

// TestModesAccepted checks the combinations the refusals above must not cover:
// --json and --pooled belong to --report, and --root belongs to --sweep-all.
func TestModesAccepted(t *testing.T) {
	for _, req := range []Request{
		{Report: true},
		{Report: true, JSON: true},
		{Report: true, Pooled: true},
		{Report: true, JSON: true, Pooled: true},
		{},
		{SweepAll: true},
		{SweepAll: true, Roots: []string{"/x", "/y"}},
	} {
		if why := req.refusal(); why != "" {
			t.Errorf("request %+v: refused %q", req, why)
		}
	}
}

func TestScopedTableScopesToTheOriginSlug(t *testing.T) {
	store := rowA + "\n" + rowB + "\n"
	out, errb, code := run(t, ptr(store), "https://github.com/acme/widgets.git", "/home/dev/widgets", Request{Report: true})
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, errb)
	}
	if first, _, _ := strings.Cut(out, "\n"); first != headerScoped {
		t.Errorf("header =\n%s\nwant\n%s", first, headerScoped)
	}
	if !strings.Contains(out, "claude  opus   deep") {
		t.Errorf("row missing from:\n%s", out)
	}
	if strings.Contains(out, "pi      flash") || strings.Contains(out, "flash") {
		t.Errorf("scoped table leaked another repo's row:\n%s", out)
	}
	if !strings.Contains(out, "Scoped to acme/widgets: 1 runs.") {
		t.Errorf("footer:\n%s", out)
	}
	// The cross-tab is part of the table the bats suite pins byte-for-byte.
	if !strings.HasSuffix(out, "* = tier-typical effort rung\n") {
		t.Errorf("table does not end with the cross-tab legend:\n%s", out)
	}
}

func TestPooledFooterNamesEveryRepo(t *testing.T) {
	store := rowA + "\n" + rowB + "\n"
	out, errb, code := run(t, ptr(store), "https://github.com/acme/widgets.git", "", Request{Report: true, Pooled: true})
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, errb)
	}
	if !strings.Contains(out, "Pooled over 2 runs from 2 repos: acme/widgets, other/repo.") {
		t.Errorf("pooled footer:\n%s", out)
	}
	if !strings.Contains(out, "flash") {
		t.Errorf("pooled table dropped the other repo's row:\n%s", out)
	}
}

func TestScopingFallsBackToToplevelThenUnknownRepo(t *testing.T) {
	// The store row is labeled "widgets" so the toplevel-basename scope
	// actually matches it; the unknown-repo case renders the empty state.
	store := strings.ReplaceAll(rowA, `"repo":"acme/widgets"`, `"repo":"widgets"`) + "\n"
	out, _, _ := run(t, ptr(store), "", "/home/dev/widgets", Request{Report: true})
	if !strings.Contains(out, "Scoped to widgets: 1 runs.") {
		t.Errorf("toplevel fallback:\n%s", out)
	}
	out, _, _ = run(t, ptr(store), "", "", Request{Report: true})
	if out != "unknown-repo: no runs swept for this repo yet\n" {
		t.Errorf("unknown-repo fallback = %q", out)
	}
}

func TestJSONAggregatesCarryValueKN(t *testing.T) {
	store := rowA + "\n"
	out, errb, code := run(t, ptr(store), "https://github.com/acme/widgets.git", "", Request{Report: true, JSON: true})
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, errb)
	}
	if !strings.HasPrefix(out, "[\n  {\n") {
		t.Errorf("--json is not jq-pretty-printed:\n%s", out)
	}
	var groups []map[string]any
	if err := json.Unmarshal([]byte(out), &groups); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if len(groups) != 1 {
		t.Fatalf("groups = %d", len(groups))
	}
	if got := groups[0]["merge_pct"]; !reflect.DeepEqual(got, map[string]any{"value": float64(100), "k": float64(1), "n": float64(1)}) {
		t.Errorf("merge_pct = %v", got)
	}
	if got := groups[0]["engine"]; got != "claude" {
		t.Errorf("engine = %v", got)
	}
}

func TestDuplicateRunIDsFoldLastWins(t *testing.T) {
	fresh := `{"repo":"acme/widgets","run_id":"r1","engine":"claude","model":"opus","tier":"deep","outcome":"merged","reached_pr":true,"pr_state":"MERGED","blocked_count":0,"watchdog_blocked_count":0,"swept_at":2}`
	stale := `{"repo":"acme/widgets","run_id":"r1","engine":"claude","model":"opus","tier":"deep","outcome":"incomplete","reached_pr":false,"pr_state":null,"blocked_count":0,"watchdog_blocked_count":0,"swept_at":1}`
	store := stale + "\n" + fresh + "\n"
	out, errb, code := run(t, ptr(store), "https://github.com/acme/widgets.git", "", Request{Report: true, JSON: true})
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, errb)
	}
	var groups []map[string]any
	if err := json.Unmarshal([]byte(out), &groups); err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 {
		t.Fatalf("groups = %d, want the fold to collapse the run_id", len(groups))
	}
	if n := groups[0]["n"]; !reflect.DeepEqual(n, map[string]any{"value": float64(1), "k": float64(1), "n": float64(1)}) {
		t.Errorf("n = %v", n)
	}
	if pct := groups[0]["merge_pct"].(map[string]any)["value"]; pct != float64(100) {
		t.Errorf("merge_pct = %v, want the fresh row's MERGED state to win", pct)
	}
}

// Every store failure mode — missing, empty, unparseable, and parseable but
// unfodable (a bare number, where `.run_id` fails) — folds to [] in silence
// and renders the empty-state line, exit 0.
func TestStoreFailuresFoldToEmpty(t *testing.T) {
	cases := map[string]*string{
		"missing":     nil,
		"empty":       ptr(""),
		"unparseable": ptr("not json\n"),
		"bare number": ptr("5\n"),
	}
	for name, store := range cases {
		out, errb, code := run(t, store, "https://github.com/acme/widgets.git", "", Request{Report: true})
		if code != 0 || errb != "" {
			t.Errorf("%s: exit %d, stderr %q", name, code, errb)
		}
		if out != "acme/widgets: no runs swept for this repo yet\n" {
			t.Errorf("%s: scoped output = %q", name, out)
		}
		out, errb, code = run(t, store, "https://github.com/acme/widgets.git", "", Request{Report: true, Pooled: true})
		if code != 0 || errb != "" {
			t.Errorf("%s pooled: exit %d, stderr %q", name, code, errb)
		}
		if out != headerBare+"\n" {
			t.Errorf("%s pooled: output = %q, want the header alone", name, out)
		}
	}
}

// sweepOne runs a sweep over one branch's events and returns its only row.
func sweepOne(t *testing.T, seed func(f *fixture)) map[string]any {
	t.Helper()
	f := newFixture(t)
	f.seedSettings()
	seed(f)
	if _, code := f.runQuiet(); code != 0 {
		t.Fatalf("exit %d", code)
	}
	rows := f.rows()
	if len(rows) != 1 {
		t.Fatalf("store has %d rows: %v", len(rows), rows)
	}
	return rows[0]
}

func (f *fixture) reaped(ts int64) {
	f.event(`{"ts":` + strconv.FormatInt(ts, 10) + `,"crew_id":"c1","from":"worker:feat/x#s1","to":"dispatcher:c1","kind":"status","body":{"state":"reaped","detail":"PR #8 MERGED","source":"reap"}}`)
}

func TestReapedRowIsNotTheTerminalState(t *testing.T) {
	bare := sweepOne(t, func(f *fixture) { f.seedPRRow() })
	reaped := sweepOne(t, func(f *fixture) {
		f.seedPRRow()
		f.reaped(1700)
	})
	if reaped["terminal_state"] != "done" || reaped["outcome"] != "pr_open" {
		t.Errorf("terminal_state/outcome = %v/%v, want done/pr_open", reaped["terminal_state"], reaped["outcome"])
	}
	if reaped["wall_clock_ms"] == nil || reaped["wall_clock_ms"] != bare["wall_clock_ms"] {
		t.Errorf("wall_clock_ms = %v, want %v (as without the reaped row)", reaped["wall_clock_ms"], bare["wall_clock_ms"])
	}

	failed := sweepOne(t, func(f *fixture) {
		f.dispatch("feat/x", 1000, "sonnet", "medium")
		f.status("worker:feat/x#s1", 1500, "failed")
		f.reaped(1700)
	})
	if failed["terminal_state"] != "failed" || failed["outcome"] != "failed" {
		t.Errorf("terminal_state/outcome = %v/%v, want failed/failed", failed["terminal_state"], failed["outcome"])
	}
}
