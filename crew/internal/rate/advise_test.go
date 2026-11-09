package rate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/clock"
)

var adviseNow = time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)

// advRow is a settled claude/standard row for repo, started daysAgo before adviseNow.
func advRow(repo, runID, model string, daysAgo int) string {
	row := mergedRow(repo, runID, "claude", model, "standard", "medium")
	t0 := adviseNow.AddDate(0, 0, -daysAgo).UnixMilli()
	return strings.Replace(row, "{", fmt.Sprintf(`{"t0_ms":%d,`, t0), 1)
}

func advRows(repo, model string, n int) []string {
	rows := make([]string, n)
	for i := range rows {
		rows[i] = advRow(repo, fmt.Sprintf("%s-%s-%d", repo, model, i), model, 5)
	}
	return rows
}

func runAdvise(t *testing.T, store *string, req Request) (string, string, int) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ratings.jsonl")
	if store != nil {
		if err := os.WriteFile(path, []byte(*store), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var out, errb bytes.Buffer
	code := Run(req, bus.Paths{}, "/repo", &out, &errb, Options{
		StorePath: path,
		Git:       fakeGit(widgetsOrigin, "/repo"),
		Clock:     clock.Clock{Now: func() time.Time { return adviseNow }},
	})
	return out.String(), errb.String(), code
}

func adviseFixture() *string {
	var rows []string
	rows = append(rows, advRows("acme/widgets", "opus", 6)...)
	rows = append(rows, advRows("acme/widgets", "sonnet", 2)...)
	rows = append(rows, advRows("other/repo", "sonnet", 4)...)
	rows = append(rows, advRows("other/repo", "haiku", 2)...)
	rows = append(rows, advRow("acme/widgets", "old-1", "opus", 90), advRow("acme/widgets", "old-2", "ancient", 90))
	return storeOf(rows...)
}

func TestAdviseRanksMeasuredAboveAnecdoteAndMarksIt(t *testing.T) {
	out, errb, code := runAdvise(t, adviseFixture(), Request{Advise: true, Args: []string{"claude", "standard"}})
	if code != 0 || errb != "" {
		t.Fatalf("exit %d, stderr %q", code, errb)
	}
	for _, want := range []string{
		"Model advice for claude standard — last 60 days, since 2026-09-09 — repo acme/widgets",
		"repo n=6 merge 100% (6 of 6 settled)",
		"repo n=2 merge 100% (2 of 2 settled)!",
		"| pooled n=6 merge 100% (6 of 6 settled) (repo n=2, pooled measured)",
		"Anecdotes (<5 runs) — not evidence, never ranked above measured",
		"repo n=0  | pooled n=2 merge 100% (2 of 2 settled)! (anecdote, n=2)",
		"Advice only: informs the choice, never overrides dispatch gates, tier rules, budget or the owner's model map.",
		"Models with no claude standard runs in the last 60 days are not listed; '!' marks a figure measured over fewer than 5 runs.",
		"Rows fold an alias with the full id of the model it names, so a folded row lists the ids it absorbed; '(gated: …)' names a requirement the dispatcher checks; the trailing section lists ids the shipped model map does not name.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	opus, sonnet, anecdotes, haiku := strings.Index(out, "\n  opus"), strings.Index(out, "\n  sonnet"),
		strings.Index(out, "Anecdotes"), strings.Index(out, "\n  haiku")
	if opus < 0 || opus >= sonnet || sonnet >= anecdotes || anecdotes >= haiku {
		t.Errorf("want opus, sonnet, anecdote header, haiku in order:\n%s", out)
	}
	if strings.Contains(out, "ancient") || strings.Contains(out, "repo n=7") {
		t.Errorf("a row older than the window leaked in:\n%s", out)
	}
}

func TestAdviseAbsentStoreOrNoRowsInWindow(t *testing.T) {
	out, _, code := runAdvise(t, nil, Request{Advise: true, Args: []string{"claude", "standard"}})
	if code != 0 || !strings.HasPrefix(out, "no ratings yet for claude standard (store: ") || !strings.HasSuffix(out, ") — pick by task fit\n") {
		t.Errorf("absent store: code %d, out %q", code, out)
	}
	out, _, code = runAdvise(t, adviseFixture(), Request{Advise: true, Args: []string{"pi", "deep"}})
	if code != 0 || !strings.HasPrefix(out, "no ratings yet for pi deep") {
		t.Errorf("no rows: code %d, out %q", code, out)
	}
}

func TestAdviseJSONShape(t *testing.T) {
	out, errb, code := runAdvise(t, adviseFixture(), Request{Advise: true, JSON: true, Args: []string{"claude", "standard"}})
	if code != 0 || errb != "" {
		t.Fatalf("exit %d, stderr %q", code, errb)
	}
	var got struct {
		Engine, Tier, Repo, Since string
		WindowDays                int `json:"window_days"`
		Models                    []struct {
			Model, Class string
			Repo, Pooled map[string]any
		}
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("%v in %q", err, out)
	}
	if got.Engine != "claude" || got.Tier != "standard" || got.Repo != "acme/widgets" || got.WindowDays != 60 || got.Since != "2026-09-09" {
		t.Errorf("header fields: %+v", got)
	}
	var order []string
	for _, m := range got.Models {
		order = append(order, m.Model+":"+m.Class)
	}
	if want := "opus:measured sonnet:repo-thin haiku:anecdote"; strings.Join(order, " ") != want {
		t.Fatalf("models = %v, want %s", order, want)
	}
	if got.Models[2].Repo != nil || got.Models[0].Repo == nil || got.Models[0].Pooled == nil {
		t.Errorf("repo/pooled presence wrong: %+v", got.Models)
	}
}

func TestAdviseRefusals(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  Request
	}{
		{"no positionals", Request{Advise: true}},
		{"one positional", Request{Advise: true, Args: []string{"claude"}}},
		{"three positionals", Request{Advise: true, Args: []string{"a", "b", "c"}}},
		{"space token", Request{Advise: true, Args: []string{"claude code", "standard"}}},
		{"empty token", Request{Advise: true, Args: []string{"claude", ""}}},
		{"with report", Request{Advise: true, Report: true, Args: []string{"claude", "standard"}}},
		{"with pooled", Request{Advise: true, Pooled: true, Args: []string{"claude", "standard"}}},
		{"with sweep-all", Request{Advise: true, SweepAll: true, Args: []string{"claude", "standard"}}},
	} {
		out, errb, code := runAdvise(t, nil, tc.req)
		if code != 1 || out != "" || errb != Usage+"\n" {
			t.Errorf("%s: code %d, stdout %q, stderr %q", tc.name, code, out, errb)
		}
	}
}

// advUnsettled is a run with no settled PR: an empty state is a failed run that
// never opened one, "OPEN" a PR still awaiting its verdict.
func advUnsettled(repo, runID, model, state string) string {
	row := advRow(repo, runID, model, 5)
	outcome, reached, prState := "failed", "false", "null"
	if state != "" {
		outcome, reached, prState = "pr_open", "true", `"`+state+`"`
	}
	row = strings.Replace(row, `"outcome":"merged","reached_pr":true`, `"outcome":"`+outcome+`","reached_pr":`+reached, 1)
	return strings.Replace(row, `"pr_state":"MERGED"`, `"pr_state":`+prState, 1)
}

func TestAdviseRanksOnEndToEndMergeRate(t *testing.T) {
	var rows []string
	for i := range 45 {
		rows = append(rows, advUnsettled("acme/widgets", fmt.Sprintf("flaky-x%d", i), "flaky", ""))
		rows = append(rows, advRow("acme/widgets", fmt.Sprintf("solid-m%d", i), "solid", 5))
	}
	rows = append(rows, advRows("acme/widgets", "flaky", 5)...)
	for i := range 5 {
		rows = append(rows, advUnsettled("acme/widgets", fmt.Sprintf("solid-c%d", i), "solid", "CLOSED"))
	}
	out, _, code := runAdvise(t, storeOf(rows...), Request{Advise: true, Args: []string{"claude", "standard"}})
	flaky, solid := strings.Index(out, "\n  flaky"), strings.Index(out, "\n  solid")
	if code != 0 || flaky < 0 || solid < 0 || solid > flaky {
		t.Errorf("want solid (45 of 50 merged) ahead of flaky (5 of 50):\n%s", out)
	}
}

func TestAdviseThinMergeFigureSortsBelowWellMeasured(t *testing.T) {
	for _, tc := range []struct {
		name, state string
		n           int
	}{
		{"no-PR failures", "", 6},
		{"open PRs", "OPEN", 10},
	} {
		var rows []string
		for i := range tc.n - 1 {
			rows = append(rows, advUnsettled("acme/widgets", fmt.Sprintf("lucky-%d", i), "lucky", tc.state))
		}
		rows = append(rows, advRows("acme/widgets", "lucky", 1)...)
		rows = append(rows, advRows("acme/widgets", "solid", tc.n-1)...)
		rows = append(rows, advUnsettled("acme/widgets", "solid-x", "solid", tc.state))
		out, _, code := runAdvise(t, storeOf(rows...), Request{Advise: true, Args: []string{"claude", "standard"}})
		lucky, solid := strings.Index(out, "\n  lucky"), strings.Index(out, "\n  solid")
		if code != 0 || lucky < 0 || solid < 0 || solid > lucky {
			t.Errorf("%s: want solid ahead of lucky:\n%s", tc.name, out)
		}
		if !strings.Contains(out, fmt.Sprintf("(1 of %d settled)!", tc.n)) {
			t.Errorf("%s: lucky's thin figure not marked:\n%s", tc.name, out)
		}
	}
}

// advRowAt is a settled row for engine/tier at model, started daysAgo before
// adviseNow; advRowsAt is n of them, one run each.
func advRowAt(repo, runID, engine, tier, model string, daysAgo int) string {
	row := mergedRow(repo, runID, engine, model, tier, "medium")
	t0 := adviseNow.AddDate(0, 0, -daysAgo).UnixMilli()
	return strings.Replace(row, "{", fmt.Sprintf(`{"t0_ms":%d,`, t0), 1)
}

func advRowsAt(repo, engine, tier, model string, n int) []string {
	rows := make([]string, n)
	for i := range rows {
		rows[i] = advRowAt(repo, fmt.Sprintf("%s-%s-%d", engine, model, i), engine, tier, model, 5)
	}
	return rows
}

// adviseRowLine is the first rendered line whose model column starts with name.
func adviseRowLine(out, name string) string {
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "  "+name+" ") {
			return line
		}
	}
	return ""
}

// adviseJSONModel is one --json model object, the three added fields included.
type adviseJSONModel struct {
	Model        string
	Class        string
	Admitted     bool
	Requires     *string
	Ids          []string
	Repo, Pooled map[string]any
}

func adviseJSONModels(t *testing.T, out string) []adviseJSONModel {
	t.Helper()
	var got struct {
		Models []adviseJSONModel
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("%v in %q", err, out)
	}
	return got.Models
}

func adviseJSONIndex(models []adviseJSONModel, name string) int {
	for i, m := range models {
		if m.Model == name {
			return i
		}
	}
	return -1
}

func TestAdviseFoldsAliasAndCurrentFullID(t *testing.T) {
	var rows []string
	rows = append(rows, advRowsAt("acme/widgets", "claude", "deep", "fable", 3)...)
	rows = append(rows, advRowsAt("acme/widgets", "claude", "deep", "claude-fable-5-1", 3)...)
	out, errb, code := runAdvise(t, storeOf(rows...), Request{Advise: true, Args: []string{"claude", "deep"}})
	if code != 0 || errb != "" {
		t.Fatalf("exit %d, stderr %q", code, errb)
	}
	line := adviseRowLine(out, "fable")
	if line == "" {
		t.Fatalf("no fable row:\n%s", out)
	}
	for _, want := range []string{"repo n=6", "| pooled n=6", " (folds claude-fable-5-1)"} {
		if !strings.Contains(line, want) {
			t.Errorf("fable row lacks %q: %q", want, line)
		}
	}
	if adviseRowLine(out, "claude-fable-5-1") != "" {
		t.Errorf("claude-fable-5-1 kept its own row:\n%s", out)
	}
}

func TestAdviseKeepsOlderGenerationApart(t *testing.T) {
	var rows []string
	rows = append(rows, advRowsAt("acme/widgets", "claude", "standard", "opus", 3)...)
	rows = append(rows, advRowsAt("acme/widgets", "claude", "standard", "claude-opus-5-5", 3)...)
	rows = append(rows, advRowsAt("acme/widgets", "claude", "standard", "claude-opus-5", 3)...)
	out, errb, code := runAdvise(t, storeOf(rows...), Request{Advise: true, Args: []string{"claude", "standard"}})
	if code != 0 || errb != "" {
		t.Fatalf("exit %d, stderr %q", code, errb)
	}
	opus := adviseRowLine(out, "opus")
	if opus == "" || !strings.Contains(opus, " (folds claude-opus-5-5)") {
		t.Errorf("opus row does not fold the newest full id: %q\n%s", opus, out)
	}
	if strings.Contains(opus, "claude-opus-5,") {
		t.Errorf("opus row folded the older generation: %q", opus)
	}
	if adviseRowLine(out, "claude-opus-5-5") != "" {
		t.Errorf("claude-opus-5-5 kept its own row:\n%s", out)
	}
	older := adviseRowLine(out, "claude-opus-5")
	if older == "" {
		t.Fatalf("claude-opus-5 lost its row:\n%s", out)
	}
	if strings.Contains(older, "folds") {
		t.Errorf("older generation folded into another row: %q", older)
	}
}

func TestAdviseMarksModelsTheMapNoLongerAdmits(t *testing.T) {
	var rows []string
	rows = append(rows, advRowsAt("acme/widgets", "pi", "standard", "openrouter/deepseek/deepseek-v4.1-flash", 6)...)
	rows = append(rows, advRowsAt("acme/widgets", "pi", "standard", "opencode/minimax-m3", 6)...)
	out, errb, code := runAdvise(t, storeOf(rows...), Request{Advise: true, Args: []string{"pi", "standard"}})
	if code != 0 || errb != "" {
		t.Fatalf("exit %d, stderr %q", code, errb)
	}
	header := "Not in the shipped model map for pi standard — retired, or added by your own settings"
	admittedAt, retiredAt := strings.Index(out, "\n  openrouter/deepseek/deepseek-v4.1-flash "), strings.Index(out, "\n  opencode/minimax-m3 ")
	headerAt := strings.Index(out, "\n"+header)
	if admittedAt < 0 || retiredAt < 0 || headerAt < 0 {
		t.Fatalf("missing admitted row, retired row or section (admitted %d, retired %d, header %d):\n%s", admittedAt, retiredAt, headerAt, out)
	}
	if admittedAt > headerAt || headerAt > retiredAt {
		t.Errorf("want admitted row, then the section header, then the retired row:\n%s", out)
	}
	jout, errb, code := runAdvise(t, storeOf(rows...), Request{Advise: true, JSON: true, Args: []string{"pi", "standard"}})
	if code != 0 || errb != "" {
		t.Fatalf("json: exit %d, stderr %q", code, errb)
	}
	models := adviseJSONModels(t, jout)
	admitted, retiredAt2 := adviseJSONIndex(models, "openrouter/deepseek/deepseek-v4.1-flash"), adviseJSONIndex(models, "opencode/minimax-m3")
	if admitted < 0 || retiredAt2 < 0 {
		t.Fatalf("json models missing a row: %+v", models)
	}
	if !models[admitted].Admitted || models[retiredAt2].Admitted {
		t.Errorf("json admitted flags wrong: %+v vs %+v", models[admitted], models[retiredAt2])
	}
	if retiredAt2 < admitted {
		t.Errorf("retired model ranked above the admitted one: %+v", models)
	}
}

func TestAdviseGatedRowNamesItsRequirement(t *testing.T) {
	rows := advRowsAt("acme/widgets", "pi", "deep", "openrouter/anthropic/claude-opus-5.5", 3)
	out, errb, code := runAdvise(t, storeOf(rows...), Request{Advise: true, Args: []string{"pi", "deep"}})
	if code != 0 || errb != "" {
		t.Fatalf("exit %d, stderr %q", code, errb)
	}
	line := adviseRowLine(out, "openrouter/anthropic/claude-opus-5.5")
	if line == "" || !strings.Contains(line, " (gated: pi_month_target)") {
		t.Errorf("gated row does not name its requirement: %q\n%s", line, out)
	}
	if strings.Contains(out, "Not in the shipped model map") {
		t.Errorf("gated row printed as retired:\n%s", out)
	}
	jout, errb, code := runAdvise(t, storeOf(rows...), Request{Advise: true, JSON: true, Args: []string{"pi", "deep"}})
	if code != 0 || errb != "" {
		t.Fatalf("json: exit %d, stderr %q", code, errb)
	}
	models := adviseJSONModels(t, jout)
	i := adviseJSONIndex(models, "openrouter/anthropic/claude-opus-5.5")
	if i < 0 {
		t.Fatalf("json model missing: %+v", models)
	}
	if m := models[i]; !m.Admitted || m.Requires == nil || *m.Requires != "pi_month_target" {
		t.Errorf("gated json row = %+v", m)
	}
	if ids := models[i].Ids; strings.Join(ids, ",") != "openrouter/anthropic/claude-opus-5.5" {
		t.Errorf("ids = %v", ids)
	}
}

func TestAdvisePathShellsOutToNothing(t *testing.T) {
	// The crew rate --advise path must run no subprocess. Every non-test file in
	// the package is covered, so a file added later is checked automatically;
	// rate.go is the one exception, its git probe is not the advise path.
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || name == "rate.go" {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(token.NewFileSet(), name, src, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, imp := range f.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if path == "os/exec" {
				t.Errorf("%s imports os/exec; crew rate --advise must run no subprocess", name)
			}
		}
		if bytes.Contains(src, []byte("exec.")) {
			t.Errorf("%s references exec.; crew rate --advise must run no subprocess", name)
		}
	}
}

func TestAdviseLocalLaneIdIsNotCalledUnselectable(t *testing.T) {
	rows := advRowsAt("acme/widgets", "pi", "standard", "lemonade/Qwen3.8-Flash-Next-Strata", 3)
	out, errb, code := runAdvise(t, storeOf(rows...), Request{Advise: true, Args: []string{"pi", "standard"}})
	if code != 0 || errb != "" {
		t.Fatalf("exit %d, stderr %q", code, errb)
	}
	header := "Not in the shipped model map for pi standard — retired, or added by your own settings"
	headerAt, rowAt := strings.Index(out, "\n"+header), strings.Index(out, "\n  lemonade/Qwen3.8-Flash-Next-Strata ")
	if headerAt < 0 || rowAt < 0 {
		t.Fatalf("local lane row not under the section (header %d, row %d):\n%s", headerAt, rowAt, out)
	}
	if headerAt > rowAt {
		t.Errorf("section header must precede the row:\n%s", out)
	}
	for _, bad := range []string{"selectable", "no longer"} {
		if strings.Contains(out, bad) {
			t.Errorf("section wording says %q, but crew cannot see the user's settings:\n%s", bad, out)
		}
	}
}
