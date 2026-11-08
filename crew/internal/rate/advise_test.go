package rate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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

func TestProtocolNamesTheAdviseCommand(t *testing.T) {
	data, err := os.ReadFile("../../../adapters/core/protocols/DISPATCHER_PROTOCOL.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "crew rate --advise <engine> <tier>") {
		t.Error("DISPATCHER_PROTOCOL.md does not tell the dispatcher to run crew rate --advise <engine> <tier>")
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
