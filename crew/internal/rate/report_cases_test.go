package rate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const widgetsOrigin = "https://github.com/acme/widgets.git"

// mergedRow is one swept, settled, merged run; effort and the repo vary so a
// test can place it in a group and a scope.
func mergedRow(repo, runID, engine, model, tier, effort string) string {
	eff := ""
	if effort != "" {
		eff = fmt.Sprintf(`"effort":%q,`, effort)
	}
	repoField := ""
	if repo != "" {
		repoField = fmt.Sprintf(`"repo":%q,`, repo)
	}
	return fmt.Sprintf(`{%s"run_id":%q,"engine":%q,"model":%q,"tier":%q,%s"outcome":"merged","reached_pr":true,"time_to_pr_ms":100,"pr_state":"MERGED","time_to_merge_ms":200,"rework_count":0,"review_high":null,"review_mode":null,"review_rounds":0,"blocked_count":0,"watchdog_blocked_count":0,"first_ci_green":true,"unresolved_notes":0,"reverted":false,"cost_proxy":100,"swept_at":1}`,
		repoField, runID, engine, model, tier, eff)
}

func storeOf(rows ...string) *string {
	s := strings.Join(rows, "\n") + "\n"
	return &s
}

func reportOut(t *testing.T, store *string, req Request) string {
	t.Helper()
	req.Report = true
	out, errb, code := run(t, store, widgetsOrigin, "/repo", req)
	if code != 0 || errb != "" {
		t.Fatalf("exit %d, stderr %q", code, errb)
	}
	return strings.TrimRight(out, "\n")
}

func lastLines(out string, n int) string {
	lines := strings.Split(out, "\n")
	return strings.Join(lines[len(lines)-n:], "\n")
}

func lineWithPrefix(t *testing.T, out, prefix string) string {
	t.Helper()
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, prefix) {
			return l
		}
	}
	t.Fatalf("no line starting %q in:\n%s", prefix, out)
	return ""
}

func readTestdata(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// The spec's worked example, built from a hand-authored store: --report folds
// the global store last-wins by run_id and never touches the bus or the
// network, so the store is the input under test. 19 rows across 3 groups reach
// every marker the table renders: "value", "value(k)", "!" on an aggregate and
// on n itself, and "—" for an unmeasured aggregate.
func TestReportWorkedExampleByteForByte(t *testing.T) {
	store := readTestdata(t, "worked_example.jsonl")
	got := reportOut(t, &store, Request{})
	want := strings.TrimRight(readTestdata(t, "worked_example.txt"), "\n")
	if got != want {
		t.Errorf("report =\n%s\nwant\n%s", got, want)
	}
}

// Covers the `unknown` paths the worked example doesn't: an effort outside the
// known rungs, and real rungs across two tiers, to pin the tier-typical `*`
// marker on the right cell.
func TestReportCrossTabBucketsRealMissingAndUnrecognizedEffort(t *testing.T) {
	got := reportOut(t, storeOf(
		mergedRow("acme/widgets", "e1", "claude", "opus", "deep", "high"),
		mergedRow("acme/widgets", "e2", "claude", "opus", "deep", "low"),
		mergedRow("acme/widgets", "e3", "claude", "sonnet", "standard", "medium"),
		mergedRow("acme/widgets", "e4", "claude", "sonnet", "standard", "ludicrous"),
	), Request{})
	want := `tier      low  medium  high  xhigh  max  ultra  unknown
deep        1       0    1*      0    0      0        0
standard    0      1*     0      0    0      0        1
* = tier-typical effort rung`
	if lastLines(got, 4) != want {
		t.Errorf("cross-tab =\n%s\nwant\n%s", lastLines(got, 4), want)
	}
}

func TestReportLocalAndOpenrouterPiModelsAtOneTierAreSeparateGroups(t *testing.T) {
	out := reportOut(t, storeOf(
		mergedRow("acme/widgets", "l1", "pi", "lemonade/Qwen3.8-Flash-Next-MTP", "standard", ""),
		mergedRow("acme/widgets", "l2", "pi", "openrouter/deepseek/deepseek-v4.1-flash", "standard", ""),
	), Request{})
	for _, model := range []string{"lemonade/Qwen3.8-Flash-Next-MTP", "openrouter/deepseek/deepseek-v4.1-flash"} {
		if !strings.Contains(out, model) {
			t.Errorf("table lacks a %s group:\n%s", model, out)
		}
	}
	if got := strings.Count(out, "\npi "); got != 2 {
		t.Errorf("%d pi groups, want 2:\n%s", got, out)
	}
}

func TestReportPooledAggregatesEveryRepoAndTheFooterNamesThem(t *testing.T) {
	out := reportOut(t, storeOf(
		mergedRow("acme/widgets", "p1", "claude", "opus", "deep", ""),
		mergedRow("acme/gadgets", "p2", "claude", "opus", "deep", ""),
		mergedRow("", "p3", "claude", "opus", "deep", ""),
	), Request{Pooled: true})
	if got := lineWithPrefix(t, out, "Pooled over"); got != "Pooled over 3 runs from 3 repos: acme/gadgets, acme/widgets, unknown." {
		t.Errorf("footer = %q", got)
	}
}

func TestReportDefaultScopesToTheCurrentRepoExcludingOthers(t *testing.T) {
	store := storeOf(
		mergedRow("acme/widgets", "s1", "claude", "opus", "deep", ""),
		mergedRow("acme/widgets", "s2", "claude", "opus", "deep", ""),
		mergedRow("acme/gadgets", "s3", "claude", "opus", "deep", ""),
	)
	groups := decodeGroups(t, reportOut(t, store, Request{JSON: true}))
	if len(groups) != 1 || valueOf(groups[0]["n"]) != 2 {
		t.Errorf("groups = %v, want one group of n=2", groups)
	}
	if got := lineWithPrefix(t, reportOut(t, store, Request{}), "Scoped to"); got != "Scoped to acme/widgets: 2 runs." {
		t.Errorf("footer = %q", got)
	}
}

func TestReportCurrentRepoWithNoRowsInANonEmptyStore(t *testing.T) {
	got := reportOut(t, storeOf(mergedRow("acme/gadgets", "z1", "claude", "opus", "deep", "")), Request{})
	if got != "acme/widgets: no runs swept for this repo yet" {
		t.Errorf("output = %q", got)
	}
}

func TestReportCrossTabHonoursTheScopedPopulation(t *testing.T) {
	store := storeOf(
		mergedRow("acme/widgets", "x1", "claude", "opus", "deep", "high"),
		mergedRow("acme/gadgets", "x2", "claude", "sonnet", "standard", "medium"),
	)
	scoped := `tier  low  medium  high  xhigh  max  ultra  unknown
deep    0       0    1*      0    0      0        0
* = tier-typical effort rung`
	if got := lastLines(reportOut(t, store, Request{}), 3); got != scoped {
		t.Errorf("scoped cross-tab =\n%s\nwant\n%s", got, scoped)
	}
	pooled := `tier      low  medium  high  xhigh  max  ultra  unknown
deep        0       0    1*      0    0      0        0
standard    0      1*     0      0    0      0        0
* = tier-typical effort rung`
	if got := lastLines(reportOut(t, store, Request{Pooled: true}), 4); got != pooled {
		t.Errorf("pooled cross-tab =\n%s\nwant\n%s", got, pooled)
	}
}

// 3 widgets rows and 3 gadgets rows: the `!` (own sample < 5) follows the
// population the table was scoped to, not the pooled one.
func TestReportSmallSampleMarkerFollowsTheScopedPopulation(t *testing.T) {
	var rows []string
	for i, repo := range []string{"acme/widgets", "acme/widgets", "acme/widgets", "acme/gadgets", "acme/gadgets", "acme/gadgets"} {
		rows = append(rows, mergedRow(repo, fmt.Sprintf("m%d", i+1), "claude", "opus", "deep", ""))
	}
	store := storeOf(rows...)
	pooled := lineWithPrefix(t, reportOut(t, store, Request{Pooled: true}), "claude  ")
	if !strings.Contains(pooled, " 6 ") || strings.Contains(pooled, "!") {
		t.Errorf("pooled row = %q, want n=6 unmarked", pooled)
	}
	scoped := lineWithPrefix(t, reportOut(t, store, Request{}), "claude  ")
	if !strings.Contains(scoped, " 3! ") {
		t.Errorf("scoped row = %q, want n=3 marked", scoped)
	}
}

func TestReportJSONAggregatesCarryValueKNAndRawCountsStayNumbers(t *testing.T) {
	pending := strings.Replace(mergedRow("acme/widgets", "g2", "claude", "opus", "deep", ""),
		`"outcome":"merged"`, `"outcome":"pr_open"`, 1)
	pending = strings.Replace(pending, `"pr_state":"MERGED"`, `"pr_state":"OPEN"`, 1)
	pending = strings.Replace(pending, `"time_to_merge_ms":200`, `"time_to_merge_ms":null`, 1)
	groups := decodeGroups(t, reportOut(t, storeOf(mergedRow("acme/widgets", "g1", "claude", "opus", "deep", ""), pending), Request{JSON: true}))
	mp := groups[0]["merge_pct"].(map[string]any)
	if mp["k"] != float64(1) || mp["n"] != float64(2) {
		t.Errorf("merge_pct = %v, want k=1 n=2", mp)
	}
	if _, ok := groups[0]["inc"].(float64); !ok {
		t.Errorf("inc = %#v, want a plain number", groups[0]["inc"])
	}
}

func TestReportBurnMedianIsTheGroupMedianInHoursAndNullWhenEveryRowIsNull(t *testing.T) {
	withCost := func(runID, model, tier string, cost any) string {
		row := mergedRow("acme/widgets", runID, "claude", model, tier, "")
		return strings.Replace(row, `"cost_proxy":100`, fmt.Sprintf(`"cost_proxy":%v`, cost), 1)
	}
	groups := decodeGroups(t, reportOut(t, storeOf(
		withCost("b1", "opus", "deep", 3600000),
		withCost("b2", "opus", "deep", 7200000),
		withCost("b3", "opus", "deep", 36000000),
		withCost("b4", "sonnet", "standard", "null"),
		withCost("b5", "sonnet", "standard", "null"),
	), Request{JSON: true}))
	if len(groups) != 2 || groups[0]["model"] != "opus" || groups[1]["model"] != "sonnet" {
		t.Fatalf("groups = %v", groups)
	}
	opus := groups[0]["burn_median"].(map[string]any)
	if opus["value"] != float64(2) || opus["k"] != float64(3) || opus["n"] != float64(3) {
		t.Errorf("opus burn_median = %v, want {value:2 k:3 n:3}", opus)
	}
	sonnet := groups[1]["burn_median"].(map[string]any)
	if sonnet["k"] != float64(0) || sonnet["value"] != nil {
		t.Errorf("sonnet burn_median = %v, want k=0 value=null", sonnet)
	}
}

func valueOf(agg any) float64 {
	v, _ := agg.(map[string]any)["value"].(float64)
	return v
}

func decodeGroups(t *testing.T, out string) []map[string]any {
	t.Helper()
	var groups []map[string]any
	if err := json.Unmarshal([]byte(out), &groups); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	return groups
}
