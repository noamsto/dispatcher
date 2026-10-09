package rate

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
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
func fakeGit(origin, toplevel string) func(string, ...string) string {
	return func(_ string, args ...string) string {
		if len(args) >= 2 && args[0] == "config" {
			return origin
		}
		return toplevel
	}
}

// run points Run at a fresh store path; a nil store leaves the file unwritten,
// so the missing-store case never reads the caller's real XDG store.
func run(t *testing.T, store *string, origin, toplevel string, args ...string) (string, string, int) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ratings.jsonl")
	if store != nil {
		if err := os.WriteFile(path, []byte(*store), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var out, errb bytes.Buffer
	code := Run(args, "/repo", &out, &errb, Options{StorePath: path, Git: fakeGit(origin, toplevel)})
	return out.String(), errb.String(), code
}

func ptr(s string) *string { return &s }

func TestFlagRefusals(t *testing.T) {
	for _, args := range [][]string{{}, {"--json"}, {"--pooled"}, {"--sweep-all"}, {"--report", "--root", "/x"}} {
		_, errb, code := run(t, ptr(""), "https://github.com/acme/widgets.git", "", args...)
		if code != 1 {
			t.Fatalf("args %v: exit %d, want 1", args, code)
		}
		if strings.Count(errb, "\n") != 1 || !strings.HasPrefix(errb, "crew-go: rate takes --report") {
			t.Errorf("args %v: stderr = %q", args, errb)
		}
	}
}

func TestScopedTableScopesToTheOriginSlug(t *testing.T) {
	store := rowA + "\n" + rowB + "\n"
	out, errb, code := run(t, ptr(store), "https://github.com/acme/widgets.git", "/home/dev/widgets", "--report")
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
	out, errb, code := run(t, ptr(store), "https://github.com/acme/widgets.git", "", "--report", "--pooled")
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
	out, _, _ := run(t, ptr(store), "", "/home/dev/widgets", "--report")
	if !strings.Contains(out, "Scoped to widgets: 1 runs.") {
		t.Errorf("toplevel fallback:\n%s", out)
	}
	out, _, _ = run(t, ptr(store), "", "", "--report")
	if out != "unknown-repo: no runs swept for this repo yet\n" {
		t.Errorf("unknown-repo fallback = %q", out)
	}
}

func TestJSONAggregatesCarryValueKN(t *testing.T) {
	store := rowA + "\n"
	out, errb, code := run(t, ptr(store), "https://github.com/acme/widgets.git", "", "--report", "--json")
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
	out, errb, code := run(t, ptr(store), "https://github.com/acme/widgets.git", "", "--report", "--json")
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
		out, errb, code := run(t, store, "https://github.com/acme/widgets.git", "", "--report")
		if code != 0 || errb != "" {
			t.Errorf("%s: exit %d, stderr %q", name, code, errb)
		}
		if out != "acme/widgets: no runs swept for this repo yet\n" {
			t.Errorf("%s: scoped output = %q", name, out)
		}
		out, errb, code = run(t, store, "https://github.com/acme/widgets.git", "", "--report", "--pooled")
		if code != 0 || errb != "" {
			t.Errorf("%s pooled: exit %d, stderr %q", name, code, errb)
		}
		if out != headerBare+"\n" {
			t.Errorf("%s pooled: output = %q, want the header alone", name, out)
		}
	}
}

func TestOriginSlug(t *testing.T) {
	cases := map[string]string{
		"https://github.com/acme/widgets.git":       "acme/widgets",
		"https://github.com/acme/widgets":           "acme/widgets",
		"git@github.com:acme/widgets.git":           "acme/widgets",
		"https://gitlab.example.com/group/proj.git": "group/proj",
		"https://host/x/y":                          "x/y",
		"":                                          "",
		"/srv/git/local.git":                        "/srv/git/local",
		// The sed is unanchored: inside an ssh:// URL it still finds the
		// git@ host and strips it, leaving the scheme behind.
		"ssh://git@github.com/acme/widgets.git": "ssh://acme/widgets",
	}
	for url, want := range cases {
		if got := originSlug(url); got != want {
			t.Errorf("originSlug(%q) = %q, want %q", url, got, want)
		}
	}
}
