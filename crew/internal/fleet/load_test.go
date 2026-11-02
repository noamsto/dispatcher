package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/clock"
	"github.com/noamsto/dispatcher/crew/internal/git"
	"github.com/noamsto/dispatcher/crew/internal/tmux/tmuxtest"
)

const loadNow = 1700000000

type loadFixture struct {
	t     *testing.T
	root  string
	panes []string
	env   map[string]string
	cmds  map[string]string
	opts  LoadOptions
}

func newLoadFixture(t *testing.T) *loadFixture {
	t.Helper()
	f := &loadFixture{t: t, root: t.TempDir(), env: map[string]string{}, cmds: map[string]string{"nproc": "4"}}
	f.opts = LoadOptions{
		Getenv: func(k string) string { return f.env[k] },
		Tmux: &tmuxtest.Fake{Func: func(args []string) (string, error) {
			if len(args) > 1 && args[0] == "list-panes" && args[1] == "-a" {
				return strings.Join(f.panes, "\n"), nil
			}
			return "", tmuxtest.ErrNoServer
		}},
		Git:        git.Exec{},
		Clock:      clock.Clock{Now: func() time.Time { return time.Unix(loadNow, 0) }},
		BudgetFile: filepath.Join(f.root, "engine-budget.json"),
		Cmd: func(_ context.Context, name string, args ...string) (string, error) {
			if out, ok := f.cmds[strings.Join(append([]string{name}, args...), " ")]; ok {
				return out, nil
			}
			return "", errors.New("no such command")
		},
	}
	f.env["DISPATCH_LOADAVG_FILE"] = filepath.Join(f.root, "loadavg")
	f.loadavg("0.00 0.00 0.00 1/1 1\n")
	return f
}

func (f *loadFixture) loadavg(s string) {
	f.t.Helper()
	if err := os.WriteFile(f.env["DISPATCH_LOADAVG_FILE"], []byte(s), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

// pane adds one list-panes row.
func (f *loadFixture) pane(cmd, name, branch, role, path string) {
	f.panes = append(f.panes, strings.Join([]string{cmd, name, branch, role, path}, "\x1f"))
}

// worktree makes a directory whose WORKER_TASK.md carries tier ("" writes none).
func (f *loadFixture) worktree(name, tier string) string {
	f.t.Helper()
	dir := filepath.Join(f.root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		f.t.Fatal(err)
	}
	if tier != "" {
		if err := os.WriteFile(filepath.Join(dir, "WORKER_TASK.md"), []byte("tier: "+tier+"\nkind: implement\n"), 0o644); err != nil {
			f.t.Fatal(err)
		}
	}
	return dir
}

// repo makes a git repo with a bus of lines and returns its checkout.
func (f *loadFixture) repo(name, tier string, lines ...string) string {
	f.t.Helper()
	dir := f.worktree(name, tier)
	if out, err := exec.Command("git", "init", "-q", dir).CombinedOutput(); err != nil {
		f.t.Skipf("git init: %v\n%s", err, out)
	}
	bus := filepath.Join(dir, ".git", "crew")
	if err := os.MkdirAll(bus, 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bus, "events.jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		f.t.Fatal(err)
	}
	return dir
}

func (f *loadFixture) budgetCache(body string) {
	f.t.Helper()
	if err := os.WriteFile(f.opts.BudgetFile, []byte(body), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *loadFixture) load(req LoadRequest) (LoadDoc, int, string, string) {
	f.t.Helper()
	req.JSON = true
	var stdout, stderr bytes.Buffer
	code := Load(context.Background(), req, &stdout, &stderr, f.opts)
	var doc LoadDoc
	if stdout.Len() > 0 {
		if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil {
			f.t.Fatalf("stdout is not a LoadDoc: %v\n%s", err, stdout.String())
		}
	}
	return doc, code, stdout.String(), stderr.String()
}

func TestLoadTierWeights(t *testing.T) {
	f := newLoadFixture(t)
	for _, tc := range []struct{ name, tier string }{
		{"amber", "trivial"}, {"birch", "standard"}, {"cedar", "deep"}, {"dune", ""}, {"elm", "wat"},
	} {
		f.pane("claude", tc.name, "feat/"+tc.name, "", f.worktree("wt-"+tc.name, tc.tier))
	}
	doc, code, _, _ := f.load(LoadRequest{Budget: new(20)})
	if code != ExitFits {
		t.Fatalf("exit %d", code)
	}
	weights := map[string]int{}
	tiers := map[string]string{}
	for _, l := range doc.Leads {
		weights[l.Name], tiers[l.Name] = l.Weight, l.Tier
	}
	want := map[string]int{"amber": 1, "birch": 2, "cedar": 3, "dune": 2, "elm": 2}
	for n, w := range want {
		if weights[n] != w {
			t.Errorf("%s weighs %d, want %d (%v)", n, weights[n], w, weights)
		}
	}
	if tiers["dune"] != "standard" || tiers["amber"] != "trivial" || tiers["cedar"] != "deep" {
		t.Errorf("tiers %v", tiers)
	}
	if doc.Weight != 10 || doc.Free == nil || *doc.Free != 10 || doc.Budget != 20 || doc.Pending != 0 {
		t.Errorf("doc %+v", doc)
	}
	if doc.Leads[0].Name != "cedar" {
		t.Errorf("heaviest first, got %v", doc.Leads)
	}
}

func TestLoadSkipsWhatIsNotALiveLead(t *testing.T) {
	f := newLoadFixture(t)
	live := f.worktree("wt-live", "deep")
	f.pane("claude", "amber", "feat/1-a", "", live)
	f.pane("claude", "amber", "feat/1-a", "reviewer", live)
	f.pane("claude", "amber", "feat/1-a", "plan-critic", live)
	f.pane("claude", "birch", "feat/2-b", "lead", f.worktree("wt-lead", "trivial"))
	f.pane("fish", "cedar", "feat/3-c", "", live)
	f.pane("claude", "", "", "", live)
	f.pane("claude", "dune", "feat/4-d", "", filepath.Join(f.root, "gone"))
	f.pane("claude", "elm", "feat/5-e", "", live+" (deleted)")
	f.pane("pi", "fern", "feat/6-f", "", f.worktree("wt-pi", "standard"))
	f.pane(".claude-wrapped", "gale", "feat/7-g", "", f.worktree("wt-wrapped", "trivial"))

	doc, _, _, _ := f.load(LoadRequest{Budget: new(20)})
	var names []string
	for _, l := range doc.Leads {
		names = append(names, l.Name)
	}
	if got := strings.Join(names, ","); got != "amber,fern,birch,gale" {
		t.Errorf("leads %s, want amber,fern,birch,gale", got)
	}
	if doc.Weight != 3+2+1+1 {
		t.Errorf("weight %d", doc.Weight)
	}
}

func TestLoadFinishedLeadsCountZero(t *testing.T) {
	f := newLoadFixture(t)
	wt := f.repo("repo", "deep",
		`{"ts":1,"kind":"status","from":"worker:feat/amber-x#s1","body":{"state":"working"}}`,
		`{"ts":2,"kind":"status","from":"worker:feat/amber-x#s1","body":{"state":"done"}}`,
		`{"ts":3,"kind":"status","from":"worker:feat/birch-x#s2","body":{"state":"pr_open"}}`,
		`{"ts":4,"kind":"msg","from":"worker:feat/birch-x#s2","body":{"state":"done"}}`,
		`{"ts":5,"kind":"status","from":"worker:feat/cedar-x#s3","body":"junk"}`,
		`{"ts":6,"kind":"status","from":"worker:feat/cedar-x#s3","body":{"state":"failed"}}`,
		`{"ts":7,"kind":"status","from":"worker:feat/dune-x#s4","body":{"state":"exited"}}`,
		`{"ts":8,"kind":"status","from":"worker:feat/elm-x#s5","body":{"state":"reaped"}}`,
		`{"ts":9,"kind":"status","from":"worker:feat/fern-x#s6","body":{"state":"done"}}`,
		`{"ts":10,"kind":"status","from":"worker:feat/fern-x#s7","body":"junk"}`,
		`{"ts":11,"kind":"status","from":"dispatcher:c1","body":{"state":"done"}}`,
	)
	for _, n := range []string{"amber", "birch", "cedar", "dune", "elm", "fern", "gale"} {
		f.pane("claude", n, "feat/"+n+"-x", "", wt)
	}
	doc, _, _, _ := f.load(LoadRequest{Budget: new(20)})
	var names []string
	for _, l := range doc.Leads {
		names = append(names, l.Name)
	}
	// fern's newest row has no state, which replaces its earlier done.
	if got := strings.Join(names, ","); got != "birch,fern,gale" {
		t.Errorf("live leads %s, want birch,fern,gale", got)
	}
	if doc.Leads[0].Repo != "repo" {
		t.Errorf("repo %q", doc.Leads[0].Repo)
	}
}

func TestLoadPaneInWorktreeSubdirectoryWeighsItsTier(t *testing.T) {
	f := newLoadFixture(t)
	wt := f.repo("repo", "deep")
	sub := filepath.Join(wt, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	f.pane("claude", "amber", "feat/a", "", sub)
	doc, _, _, _ := f.load(LoadRequest{Budget: new(20)})
	if doc.Weight != 3 {
		t.Errorf("weight %d, want 3", doc.Weight)
	}
}

func TestLoadAverageAgainstCores(t *testing.T) {
	f := newLoadFixture(t)
	for _, tc := range []struct {
		avg  string
		over bool
		why  string
	}{
		{"4.50 3.00 2.00 1/1 1\n", true, "1-minute load average 4.50 is above the 4 cores"},
		{"4.00 3.00 2.00 1/1 1\n", false, ""},
		{"9.00 3.00 2.00 1/1 1\n", true, "1-minute load average 9.00 is above the 4 cores"},
	} {
		f.loadavg(tc.avg)
		doc, code, _, stderr := f.load(LoadRequest{Budget: new(8), Add: 1})
		if doc.LoadOver != tc.over || doc.Cores == nil || *doc.Cores != 4 {
			t.Errorf("%q: %+v", tc.avg, doc)
		}
		if tc.over != (code == ExitRefused) || strings.TrimSpace(stderr) != tc.why {
			t.Errorf("%q: exit %d stderr %q", tc.avg, code, stderr)
		}
	}

	f.loadavg("garbage\n")
	doc, code, _, _ := f.load(LoadRequest{Budget: new(8), Add: 1})
	if code != ExitFits || doc.Load1 != nil || doc.Cores != nil || doc.LoadOver {
		t.Errorf("garbage load average: exit %d %+v", code, doc)
	}

	f.loadavg("4.50 3.00 2.00 1/1 1\n")
	delete(f.cmds, "nproc")
	doc, code, _, _ = f.load(LoadRequest{Budget: new(8), Add: 1})
	if code != ExitFits || doc.Cores != nil {
		t.Errorf("no core count must skip the check: exit %d %+v", code, doc)
	}
	f.cmds["sysctl -n hw.ncpu"] = "2"
	doc, code, _, _ = f.load(LoadRequest{Budget: new(8), Add: 1})
	if code != ExitRefused || doc.Cores == nil || *doc.Cores != 2 {
		t.Errorf("hw.ncpu fallback: exit %d %+v", code, doc)
	}
}

func TestLoadAverageSources(t *testing.T) {
	f := newLoadFixture(t)
	f.env["DISPATCH_LOADAVG_FILE"] = filepath.Join(f.root, "missing")
	f.cmds["sysctl -n vm.loadavg"] = "{ 7.25 1.00 1.00 }"
	doc, _, _, _ := f.load(LoadRequest{Budget: new(8)})
	if doc.Load1 == nil || *doc.Load1 != 7.25 || !doc.LoadOver {
		t.Errorf("sysctl fallback: %+v", doc)
	}
}

func TestLoadRefusalNamesTheHeaviestThree(t *testing.T) {
	f := newLoadFixture(t)
	for _, tc := range []struct{ name, tier string }{
		{"amber", "deep"}, {"birch", "deep"}, {"cedar", "trivial"}, {"dune", "trivial"},
	} {
		f.pane("claude", tc.name, "feat/"+tc.name, "", f.worktree("wt-"+tc.name, tc.tier))
	}
	_, code, stdout, stderr := f.load(LoadRequest{Budget: new(8), Add: 2})
	if code != ExitRefused {
		t.Fatalf("exit %d", code)
	}
	want := "live lead weight 8 + this standard dispatch (2) exceeds fleetBudget 8 — heaviest: amber (?, deep), birch (?, deep), cedar (?, trivial)"
	if strings.TrimSpace(stderr) != want {
		t.Errorf("why %q, want %q", stderr, want)
	}
	if !strings.HasPrefix(stdout, "{") {
		t.Errorf("--json keeps the document on stdout, got %q", stdout)
	}

	// Without --json the reason is stdout, and a trivial dispatch still fits.
	var out, errb bytes.Buffer
	code = Load(context.Background(), LoadRequest{Budget: new(10), Add: 3}, &out, &errb, f.opts)
	if code != ExitRefused || strings.TrimSpace(out.String()) != strings.Replace(want, "weight 8 + this standard dispatch (2) exceeds fleetBudget 8", "weight 8 + this deep dispatch (3) exceeds fleetBudget 10", 1) || errb.Len() != 0 {
		t.Errorf("exit %d stdout %q stderr %q", code, out.String(), errb.String())
	}
	code = Load(context.Background(), LoadRequest{Budget: new(10), Add: 1}, &out, &errb, f.opts)
	if code != ExitFits {
		t.Errorf("trivial dispatch at weight 8 of 10 should fit, exit %d", code)
	}
}

func TestLoadRefusalWithNoLeadsNamesNobody(t *testing.T) {
	f := newLoadFixture(t)
	_, code, _, stderr := f.load(LoadRequest{Budget: new(2), Add: 3})
	if code != ExitRefused || strings.TrimSpace(stderr) != "live lead weight 0 + this deep dispatch (3) exceeds fleetBudget 2" {
		t.Errorf("exit %d %q", code, stderr)
	}
}

func TestLoadBudgetZeroDisablesBothChecks(t *testing.T) {
	f := newLoadFixture(t)
	for _, n := range []string{"amber", "birch", "cedar", "dune"} {
		f.pane("claude", n, "feat/"+n, "", f.worktree("wt-"+n, "deep"))
	}
	f.loadavg("9.00 9.00 9.00 1/1 1\n")
	doc, code, _, stderr := f.load(LoadRequest{Budget: new(0), Add: 3})
	if code != ExitFits || stderr != "" {
		t.Errorf("exit %d %q", code, stderr)
	}
	if doc.Budget != 0 || doc.Free != nil || doc.Weight != 12 || !doc.LoadOver {
		t.Errorf("doc %+v", doc)
	}
}

func TestLoadOwnedBranchIsNoNewLead(t *testing.T) {
	f := newLoadFixture(t)
	for _, n := range []string{"amber", "birch", "cedar"} {
		f.pane("claude", n, "feat/"+n, "", f.worktree("wt-"+n, "deep"))
	}
	// A pane whose engine has died still owns its branch.
	f.pane("fish", "dune", "feat/dune", "", f.worktree("wt-dune", "deep"))
	// A role pane does not.
	f.pane("claude", "elm", "feat/elm", "reviewer", f.worktree("wt-elm", "deep"))
	for branch, want := range map[string]int{"feat/amber": ExitFits, "feat/dune": ExitFits, "feat/elm": ExitRefused, "feat/new": ExitRefused} {
		_, code, _, _ := f.load(LoadRequest{Budget: new(8), Add: 2, Own: []string{"feat/other", branch}})
		if code != want {
			t.Errorf("own %s: exit %d, want %d", branch, code, want)
		}
	}
	_, code, _, _ := f.load(LoadRequest{Budget: new(8), Add: 2})
	if code != ExitRefused {
		t.Errorf("no --own: exit %d", code)
	}
}

func TestLoadWithoutAddNeverRefuses(t *testing.T) {
	f := newLoadFixture(t)
	f.pane("claude", "amber", "feat/a", "", f.worktree("wt-a", "deep"))
	f.loadavg("9.00 9.00 9.00 1/1 1\n")
	_, code, _, _ := f.load(LoadRequest{Budget: new(1)})
	if code != ExitFits {
		t.Errorf("exit %d", code)
	}
}

func TestLoadBudgetSources(t *testing.T) {
	f := newLoadFixture(t)
	f.opts.Budget = func(context.Context) (int, error) { return 5, nil }
	if doc, _, _, _ := f.load(LoadRequest{}); doc.Budget != 5 {
		t.Errorf("settings budget %d", doc.Budget)
	}
	if doc, _, _, _ := f.load(LoadRequest{Budget: new(3)}); doc.Budget != 3 {
		t.Errorf("--budget %d", doc.Budget)
	}
	f.opts.Budget = func(context.Context) (int, error) { return 0, errors.New("boom") }
	_, code, _, stderr := f.load(LoadRequest{})
	if code != ExitError || !strings.Contains(stderr, "crew: fleet load: boom") {
		t.Errorf("exit %d %q", code, stderr)
	}
	// --budget never asks the settings.
	if _, code, _, _ := f.load(LoadRequest{Budget: new(3)}); code != ExitFits {
		t.Errorf("exit %d", code)
	}
}

func TestResolveBudget(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		settings string
		want     int
	}{
		{`{}`, 8}, {`{"fleetBudget":0}`, 0}, {`{"fleetBudget":1e1}`, 10}, {`{"fleetBudget":2.9}`, 2}, {`{"fleetBudget":99999999}`, 1000000},
	} {
		bin := filepath.Join(dir, "dispatch-config")
		if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf '%s\\n' '"+tc.settings+"'\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		got, err := ResolveBudget(context.Background(), bin, &bytes.Buffer{})
		if err != nil || got != tc.want {
			t.Errorf("%s: %d, %v; want %d", tc.settings, got, err, tc.want)
		}
	}
	if _, err := ResolveBudget(context.Background(), filepath.Join(dir, "absent"), &bytes.Buffer{}); err == nil {
		t.Error("a missing dispatch-config must error")
	}
}

func budgetJSON(fetchedAgo int, engines string) string {
	return fmt.Sprintf(`{"fetched_epoch":%d,"engines":%s}`, loadNow-fetchedAgo, engines)
}

func TestLoadBudgetCacheSummary(t *testing.T) {
	f := newLoadFixture(t)
	f.budgetCache(budgetJSON(60, fmt.Sprintf(`{
	  "claude": {"windows": {"7d": {"used_pct": 96.5, "resets_at": null}, "5h": {"used_pct": 99, "resets_at": %d}, "old": {"used_pct": 99, "resets_at": %d}, "low": {"used_pct": 40, "resets_at": null}}, "limit_reached": null},
	  "codex": {"windows": null, "limit_reached": {"rate_limit_reached_type": "primary"}},
	  "cursor": {"windows": {"month": {"used_pct": 10, "resets_at": 1}}, "limit_reached": {"reason": "cap", "resets_at": %d}},
	  "pi": {"anthropic": {"credit": 1}}
	}`, loadNow+100, loadNow-100, loadNow+100)))
	doc, _, _, _ := f.load(LoadRequest{Budget: new(8)})

	claude := doc.Engines["claude"]
	if claude.InBudget || strings.Join(claude.HotWindows, ",") != "5h,7d" || claude.LimitReached {
		t.Errorf("claude %+v (a window already reset is not hot)", claude)
	}
	if codex := doc.Engines["codex"]; codex.InBudget || !codex.LimitReached || len(codex.HotWindows) != 0 {
		t.Errorf("codex %+v", codex)
	}
	if cursor := doc.Engines["cursor"]; cursor.InBudget || !cursor.LimitReached {
		t.Errorf("cursor %+v", cursor)
	}
	// pi holds only the credit estimate: no quota window, so no reading.
	if pi := doc.Engines["pi"]; !pi.InBudget || pi.LimitReached || pi.HotWindows == nil {
		t.Errorf("pi %+v", pi)
	}
	if doc.BudgetAgeS == nil || *doc.BudgetAgeS != 60 {
		t.Errorf("age %v", doc.BudgetAgeS)
	}
}

func TestLoadBudgetCacheCursorLimitPastItsResetIsClear(t *testing.T) {
	f := newLoadFixture(t)
	f.budgetCache(budgetJSON(0, fmt.Sprintf(`{"cursor": {"limit_reached": {"reason": "cap", "resets_at": %d}}, "pi": {"limit_reached": {"reason": "credit"}}}`, loadNow-1)))
	doc, _, _, _ := f.load(LoadRequest{Budget: new(8)})
	if c := doc.Engines["cursor"]; !c.InBudget {
		t.Errorf("cursor %+v", c)
	}
	if p := doc.Engines["pi"]; p.InBudget || !p.LimitReached {
		t.Errorf("pi %+v", p)
	}
}

func TestLoadBudgetCacheUnknownFailsOpen(t *testing.T) {
	hot := `{"claude": {"windows": {"7d": {"used_pct": 99, "resets_at": null}}}}`
	for _, tc := range []struct {
		name  string
		cache string
		age   *int
	}{
		{"missing", "", nil},
		{"unparsable", "{not json", nil},
		{"no epoch", `{"engines": ` + hot + `}`, nil},
		{"epoch not a number", `{"fetched_epoch":"x","engines": ` + hot + `}`, nil},
		{"over 2h old", budgetJSON(7201, hot), new(7201)},
		{"over 300s ahead", budgetJSON(-301, hot), new(0)},
		{"null entry", budgetJSON(0, `{"claude": null}`), new(0)},
	} {
		f := newLoadFixture(t)
		if tc.cache != "" {
			f.budgetCache(tc.cache)
		}
		doc, code, _, _ := f.load(LoadRequest{Budget: new(8), Add: 1})
		if code != ExitFits {
			t.Errorf("%s: a blind cache must not refuse, exit %d", tc.name, code)
		}
		for _, name := range budgetEngines {
			if e := doc.Engines[name]; !e.InBudget || e.LimitReached || len(e.HotWindows) != 0 || e.HotWindows == nil {
				t.Errorf("%s: %s %+v, want in budget", tc.name, name, e)
			}
		}
		switch {
		case tc.age == nil && doc.BudgetAgeS != nil:
			t.Errorf("%s: age %d, want null", tc.name, *doc.BudgetAgeS)
		case tc.age != nil && (doc.BudgetAgeS == nil || *doc.BudgetAgeS != *tc.age):
			t.Errorf("%s: age %v, want %d", tc.name, doc.BudgetAgeS, *tc.age)
		}
	}
}

func TestLoadBudgetCacheAtTheEdgeOfTrust(t *testing.T) {
	hot := `{"claude": {"windows": {"7d": {"used_pct": 95, "resets_at": null}}}}`
	for _, ago := range []int{7200, -300} {
		f := newLoadFixture(t)
		f.budgetCache(budgetJSON(ago, hot))
		doc, _, _, _ := f.load(LoadRequest{Budget: new(8)})
		if doc.Engines["claude"].InBudget {
			t.Errorf("a cache %d s old is still trusted at its limit", ago)
		}
	}
}

func TestLoadJSONShape(t *testing.T) {
	f := newLoadFixture(t)
	var stdout bytes.Buffer
	Load(context.Background(), LoadRequest{JSON: true, Budget: new(8)}, &stdout, &bytes.Buffer{}, f.opts)
	var raw map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"budget", "weight", "pending", "free", "leads", "load1", "cores", "load_over", "engines", "budget_age_s"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("missing key %s in %s", k, stdout.String())
		}
	}
	if leads, ok := raw["leads"].([]any); !ok || len(leads) != 0 {
		t.Errorf("leads must be an empty array, got %v", raw["leads"])
	}
}

func TestLoadTextOutput(t *testing.T) {
	f := newLoadFixture(t)
	f.pane("claude", "amber", "feat/a", "", f.worktree("wt-a", "deep"))
	var out bytes.Buffer
	if code := Load(context.Background(), LoadRequest{Budget: new(8)}, &out, &bytes.Buffer{}, f.opts); code != ExitFits {
		t.Fatalf("exit %d", code)
	}
	for _, want := range []string{"fleetBudget 8  live lead weight 3  pending 0  free 5", "load 0.00 on 4 cores", "amber", "feat/a"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in\n%s", want, out.String())
		}
	}
}
