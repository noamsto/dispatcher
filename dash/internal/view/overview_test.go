package view_test

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/dash/internal/data"
	"github.com/noamsto/dispatcher/dash/internal/view"
	"github.com/noamsto/dispatcher/dash/internal/view/viewtest"
)

var update = flag.Bool("update", false, "rewrite golden JSON files")

func worker(t *testing.T, v view.OverviewView, branch string) view.OverviewWorker {
	t.Helper()
	for _, w := range v.Workers {
		if w.Branch == branch {
			return w
		}
	}
	t.Fatalf("no worker %q", branch)
	return view.OverviewWorker{}
}

func walk(v any, fn func(map[string]any)) {
	switch x := v.(type) {
	case map[string]any:
		fn(x)
		for _, c := range x {
			walk(c, fn)
		}
	case []any:
		for _, c := range x {
			walk(c, fn)
		}
	}
}

func TestPeerRowsHaveNoActions(t *testing.T) {
	raw, err := json.Marshal(view.BuildOverview(viewtest.Inputs()))
	if err != nil {
		t.Fatal(err)
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	peers := 0
	walk(doc, func(m map[string]any) {
		if m["origin"] != "peer" {
			return
		}
		peers++
		if acts, ok := m["actions"]; ok {
			if l, isList := acts.([]any); !isList || len(l) != 0 {
				t.Errorf("peer row actions = %v", acts)
			}
		}
		if act, ok := m["action"]; ok && act != nil {
			t.Errorf("peer attention action = %v", act)
		}
	})
	if peers < 3 {
		t.Errorf("only %d peer objects (worker, agent, attention expected)", peers)
	}
}

func TestRemoteUnknownFields(t *testing.T) {
	v := view.BuildOverview(viewtest.Inputs())
	w := worker(t, v, "feat/r")
	if w.Origin != view.OriginRemote || w.Phase != nil || w.Acceptance != nil {
		t.Errorf("remote worker = %+v", w)
	}
	for _, a := range v.Agents {
		if a.Branch == "feat/r" && a.Roles != nil {
			t.Error("remote roles not null")
		}
	}
	for _, wd := range v.Monitors.Watchdogs {
		if wd.Branch == "feat/r" && wd.Activity != nil {
			t.Error("remote activity not null")
		}
	}
	item(t, v, "blocked:halo/c1:feat/r")
}

func TestPhase(t *testing.T) {
	v := view.BuildOverview(viewtest.Inputs())
	got := map[string]string{}
	for _, s := range worker(t, v, "feat/a").Phase.Steps {
		got[s.Name] = s.Status
	}
	want := map[string]string{"spec": "done", "plan": "done", "execute": "current", "gate": "pending", "review": "pending", "ship": "pending"}
	for k, s := range want {
		if got[k] != s {
			t.Errorf("feat/a %s = %s, want %s", k, got[k], s)
		}
	}
	for _, s := range worker(t, v, "feat/b").Phase.Steps {
		if s.Status != "done" {
			t.Errorf("pr_open step %s = %s", s.Name, s.Status)
		}
	}
	for _, s := range worker(t, v, "feat/j").Phase.Steps {
		if s.Name == "plan" && s.Status != "skipped" {
			t.Errorf("never-entered plan = %s", s.Status)
		}
	}
}

func TestAcceptance(t *testing.T) {
	v := view.BuildOverview(viewtest.Inputs())
	if a := worker(t, v, "feat/b").Acceptance; a == nil || a.Done != 5 || a.Total != 5 {
		t.Errorf("feat/b acceptance = %+v", a)
	}
	if a := worker(t, v, "feat/a").Acceptance; a != nil {
		t.Errorf("feat/a acceptance = %+v, want nil", a)
	}
}

func TestBudgetPace(t *testing.T) {
	in := viewtest.Inputs()
	pts := func(n int64) *int64 { return &n }
	in.Snap.Budget.Report = &data.BudgetReport{Engines: map[string]*data.EngineBudget{
		"e": {Windows: []data.Window{
			{Key: "a", AheadPts: pts(5)}, {Key: "b", AheadPts: pts(-5)},
			{Key: "c", AheadPts: pts(0)}, {Key: "d"},
		}},
	}}
	got := map[string]string{}
	for _, b := range view.BuildOverview(in).Monitors.Budgets {
		got[b.Label] = b.Pace
	}
	want := map[string]string{"e a": "ahead", "e b": "behind", "e c": "on_pace", "e d": "n/a"}
	for k, p := range want {
		if got[k] != p {
			t.Errorf("pace %s = %q, want %q (%v)", k, got[k], p, got)
		}
	}
}

func TestBudgetRowsSkipNullEngine(t *testing.T) {
	in := viewtest.Inputs()
	in.Snap.Budget.Report = &data.BudgetReport{Engines: map[string]*data.EngineBudget{
		"e":  {Windows: []data.Window{{Key: "a"}}},
		"pi": nil,
	}}
	for _, b := range view.BuildOverview(in).Monitors.Budgets {
		if b.Engine == "pi" {
			t.Errorf("null engine produced budget row %q", b.Label)
		}
	}
}

func TestActivityNonEmpty(t *testing.T) {
	v := view.BuildOverview(viewtest.Inputs())
	for _, wd := range v.Monitors.Watchdogs {
		if wd.Branch != "feat/a" {
			continue
		}
		sum := 0
		for _, n := range wd.Activity {
			sum += n
		}
		if len(wd.Activity) != 30 || sum == 0 {
			t.Errorf("feat/a activity = %v", wd.Activity)
		}
		return
	}
	t.Error("no feat/a watchdog row")
}

func TestGoldenViews(t *testing.T) {
	for _, name := range view.Names {
		t.Run(name, func(t *testing.T) {
			v, err := view.Build(name, viewtest.Inputs())
			if name == "fleet" {
				if err == nil || !strings.Contains(err.Error(), "BuildFleet") {
					t.Errorf("Build(fleet) err = %v, want a pointer to BuildFleet", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var buf bytes.Buffer
			enc := json.NewEncoder(&buf)
			enc.SetEscapeHTML(false)
			enc.SetIndent("", "  ")
			if err := enc.Encode(v); err != nil {
				t.Fatal(err)
			}
			got := buf.Bytes()

			var probe struct {
				SchemaVersion int    `json:"schema_version"`
				View          string `json:"view"`
			}
			if err := json.Unmarshal(got, &probe); err != nil {
				t.Fatal(err)
			}
			if probe.SchemaVersion != 1 || probe.View != name {
				t.Errorf("schema_version/view = %d/%q", probe.SchemaVersion, probe.View)
			}

			path := filepath.Join("testdata", "golden", name+".json")
			if *update {
				if err := os.WriteFile(path, got, 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(want) != string(got) {
				t.Errorf("%s differs from golden; run go test ./internal/view -update and review the diff", name)
			}
		})
	}
}

func TestPeerErrorRowsAreUnknownAndListed(t *testing.T) {
	v := view.BuildOverview(viewtest.Inputs())
	labels := map[string]string{}
	for _, w := range v.Monitors.Watchdogs {
		if w.Origin == view.OriginPeer {
			labels[w.Host+"/"+w.Branch] = w.Label
		}
	}
	if labels["nuc/feat/q"] != "?" || labels["halo/"] != "?" {
		t.Errorf("peer error labels = %v, want ?", labels)
	}
	for _, want := range []string{"peer nuc: sync stale: last ok 900s ago", "peer halo: no snapshot yet"} {
		if !slices.Contains(v.Errors, want) {
			t.Errorf("errors = %v, missing %q", v.Errors, want)
		}
	}
}

func TestPeerErrorsDedupe(t *testing.T) {
	in := viewtest.Inputs()
	in.Snap.Roster.Peers = append(in.Snap.Roster.Peers, in.Snap.Roster.Peers[1])
	n := 0
	for _, e := range view.BuildOverview(in).Errors {
		if e == "peer nuc: sync stale: last ok 900s ago" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("error listed %d times", n)
	}
}

func TestPeerRowNeverShadowsLocalRow(t *testing.T) {
	in := viewtest.Inputs()
	in.Snap.Roster.Peers = []map[string]any{
		{"crew_id": "c1", "branch": "feat/b", "host": "mbp", "peer": true, "state": "blocked"},
	}
	for _, w := range view.BuildOverview(in).Workers {
		if w.Crew == "c1" && w.Branch == "feat/b" && w.Origin == view.OriginPeer {
			t.Errorf("peer row shadows %+v", w)
		}
	}
}

func TestPeerRowDispatcherIsNull(t *testing.T) {
	in := viewtest.Inputs()
	in.Snap.Roster.Crews = []data.RosterCrew{
		{ID: "c1", Workers: []map[string]any{}, Holds: []map[string]any{}, Dispatcher: &data.DispatcherLoc{Where: "here"}},
	}
	in.Snap.Roster.Peers = []map[string]any{
		{"crew_id": "c1", "branch": "feat/peer", "host": "mbp", "peer": true, "state": "blocked"},
	}
	var n int
	for _, w := range view.BuildOverview(in).Workers {
		if w.Origin != view.OriginPeer {
			continue
		}
		n++
		if w.Dispatcher != nil {
			t.Errorf("peer row dispatcher = %+v, want nil", w.Dispatcher)
		}
	}
	if n == 0 {
		t.Fatal("no peer row in overview")
	}
}

func TestOverviewListsForeignError(t *testing.T) {
	in := viewtest.Inputs()
	msg := "foreign crews: boom"
	in.Snap.Roster.ForeignError = &msg
	if !slices.Contains(view.BuildOverview(in).Errors, msg) {
		t.Errorf("Errors = %v, want %q", view.BuildOverview(in).Errors, msg)
	}
}

func TestOverviewInputsDir(t *testing.T) {
	check := func(v view.OverviewView, want *string) {
		t.Helper()
		var acts []view.Action
		for _, w := range v.Workers {
			acts = append(acts, w.Actions...)
		}
		for _, a := range v.Attention {
			if a.Action != nil {
				acts = append(acts, *a.Action)
			}
		}
		if len(acts) == 0 {
			t.Fatal("no actions")
		}
		for _, a := range acts {
			if want == nil {
				if a.Dir != nil || strings.HasPrefix(a.Command, "cd ") {
					t.Errorf("%s: dir %v command %q, want none", a.Verb, a.Dir, a.Command)
				}
				continue
			}
			if a.Dir == nil || *a.Dir != *want || !strings.HasPrefix(a.Command, "cd /r/x && ") {
				t.Errorf("%s: dir %v command %q", a.Verb, a.Dir, a.Command)
			}
		}
	}
	dir := "/r/x"
	in := viewtest.Inputs()
	check(view.BuildOverview(in), nil)
	in.Dir = &dir
	check(view.BuildOverview(in), &dir)
}

func TestAgentExecute(t *testing.T) {
	run := func(id, state, session string, start, end int64) data.SubagentRun {
		return data.SubagentRun{ID: id, Label: "step " + id, Agent: "general-purpose", Engine: "claude",
			Model: "sonnet", Effort: "low", State: state, Session: session, StartTS: start, EndTS: end}
	}
	agentOf := func(v view.OverviewView, branch string) view.AgentRow {
		t.Helper()
		for _, a := range v.Agents {
			if a.Branch == branch {
				return a
			}
		}
		t.Fatalf("no agent row %q", branch)
		return view.AgentRow{}
	}
	in := viewtest.Inputs()
	now := in.NowMS
	a := data.WorkerKey{Crew: "c1", Branch: "feat/a"}
	in.Bus.Workers[a].Session = "s1"
	in.Bus.Subagents = map[data.WorkerKey][]data.SubagentRun{
		a: {
			run("r", "running", "s1", now-30000, 0),
			run("d", "done", "s1", now-90000, now-60000),
			run("f", "failed", "s1", now-90000, now-10000),
			run("old", "running", "s0", now-30000, 0),
		},
		{Crew: "c1", Branch: "feat/g"}: {run("x", "running", "", now-30000, 0)},
		{Crew: "c1", Branch: "feat/p"}: {run("p", "running", "", now-30000, 0)},
	}
	noSubs := viewtest.Inputs()
	noSubs.Bus.Subagents = nil
	base := view.BuildOverview(noSubs)
	v := view.BuildOverview(in)

	got := agentOf(v, "feat/a").Execute
	wantStates := []string{"running", "done", "failed", "unknown"}
	wantAge := []int64{30, 60, 10, 30}
	if len(got) != len(wantStates) {
		t.Fatalf("execute = %+v", got)
	}
	for i, n := range got {
		if n.Kind != "subagent" || n.State != wantStates[i] || *n.AgeSec != wantAge[i] {
			t.Errorf("node %d = %+v age %d", i, n, *n.AgeSec)
		}
	}
	first := got[0]
	if *first.Label != "step r" || *first.Role != "general-purpose" || *first.Engine != "claude" ||
		*first.Model != "sonnet" || *first.Effort != "low" {
		t.Errorf("first = %+v", first)
	}
	if n := agentOf(v, "feat/a").Execute[0]; n.Detail != nil || n.Tier != nil {
		t.Errorf("unexpected detail/tier: %+v", n)
	}
	if st := agentOf(v, "feat/g").Execute[0].State; st != "unknown" {
		t.Errorf("terminal lead: state = %s", st)
	}
	if ex := agentOf(v, "feat/p").Execute; ex == nil || len(ex) != 0 {
		t.Errorf("peer execute = %#v", ex)
	}
	if want := base.Summary.Agents + 1; v.Summary.Agents != want {
		t.Errorf("Summary.Agents = %d, want %d (one running node)", v.Summary.Agents, want)
	}
}

func TestAgentNodeOmitsEmptyLabelAndEffort(t *testing.T) {
	b, err := json.Marshal(view.AgentNode{Kind: "role"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "label") || strings.Contains(string(b), "effort") {
		t.Errorf("json = %s", b)
	}
}

func TestOverviewDispatcherLocation(t *testing.T) {
	row := func(branch string) map[string]any {
		return map[string]any{"branch": branch, "name": branch, "state": "blocked", "detail": "needs input", "tier": "standard", "engine": "claude", "live": true, "age_s": float64(30)}
	}
	repo, common := "a", "/r/a/.git"
	in := viewtest.Inputs()
	in.Snap.Roster.Peers = nil
	in.Snap.Roster.Crews = []data.RosterCrew{
		{ID: "home", Workers: []map[string]any{row("feat/h")}, Holds: []map[string]any{}, Dispatcher: &data.DispatcherLoc{Where: "here"}},
		{ID: "away", Workers: []map[string]any{row("feat/h")}, Holds: []map[string]any{}, Dispatcher: &data.DispatcherLoc{Where: "elsewhere", Repo: &repo, Common: &common}},
		{ID: "bare", Workers: []map[string]any{row("feat/h")}, Holds: []map[string]any{}},
	}
	v := view.BuildOverview(in)
	by := map[string]view.OverviewWorker{}
	for _, w := range v.Workers {
		by[w.Crew] = w
	}
	if len(by) != 3 {
		t.Fatalf("workers = %+v", v.Workers)
	}
	h, a := by["home"], by["away"]
	if h.State != a.State || deref(h.Detail) != deref(a.Detail) || deref(h.Detail) == "" || h.AgeSec == nil || a.AgeSec == nil || *h.AgeSec != *a.AgeSec {
		t.Errorf("home %+v and away %+v fold differently", h, a)
	}
	if h.Dispatcher == nil || h.Dispatcher.Where != "here" || h.Dispatcher.Repo != nil {
		t.Errorf("home dispatcher = %+v", h.Dispatcher)
	}
	if d := a.Dispatcher; d == nil || d.Where != "elsewhere" || *d.Repo != "a" || *d.Common != "/r/a/.git" {
		t.Errorf("away dispatcher = %+v", a.Dispatcher)
	}
	if by["bare"].Dispatcher != nil {
		t.Errorf("bare dispatcher = %+v, want nil", by["bare"].Dispatcher)
	}
	rv := view.BuildRoster(in)
	if rv.Crews[1].Dispatcher == nil || rv.Crews[1].Dispatcher.Where != "elsewhere" || rv.Crews[2].Dispatcher != nil {
		t.Errorf("roster crew dispatchers = %+v %+v", rv.Crews[1].Dispatcher, rv.Crews[2].Dispatcher)
	}
}

func TestRosterViewCarriesForeignError(t *testing.T) {
	in := viewtest.Inputs()
	if got := view.BuildRoster(in).ForeignError; got != nil {
		t.Fatalf("ForeignError = %q, want nil", *got)
	}
	msg := "foreign crews: boom"
	in.Snap.Roster.ForeignError = &msg
	if got := view.BuildRoster(in).ForeignError; got == nil || *got != msg {
		t.Errorf("ForeignError = %v, want %q", got, msg)
	}
}

func TestAge(t *testing.T) {
	cases := []struct {
		sec  int64
		want string
	}{
		{0, "now"}, {4, "now"}, {5, "now"}, {59, "now"}, {-3, "now"},
		{60, "1m"}, {119, "1m"}, {3599, "59m"},
		{3600, "1h"}, {7200, "2h"},
	}
	for _, c := range cases {
		if got := view.Age(c.sec); got != c.want {
			t.Errorf("Age(%d) = %q, want %q", c.sec, got, c.want)
		}
	}
}

func TestOverviewScopeErrorOnlyWhenProbeFailed(t *testing.T) {
	in := viewtest.Inputs()
	b, err := json.Marshal(view.BuildOverview(in).Scope)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"error"`) {
		t.Errorf("clean scope carries error: %s", b)
	}
	in.Snap.Roster.Scope = data.Scope{Source: "none", Err: "tmux list-panes: boom"}
	b, err = json.Marshal(view.BuildOverview(in).Scope)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"error":"tmux list-panes: boom"`) {
		t.Errorf("scope lacks the probe error: %s", b)
	}
}

func TestBudgetRowsLabelPiBudgets(t *testing.T) {
	spend, target, keyLimit, keyLeft := 34.59, 30.0, 100.0, 88.0
	src := "config"
	credit := &data.AnthropicCredit{BalanceUSD: 50, SpentUSD: 30, RemainingUSD: 20, UsedPct: 60}
	in := viewtest.Inputs()
	in.Snap.Budget.Report = &data.BudgetReport{
		Engines: map[string]*data.EngineBudget{
			"claude": {Windows: []data.Window{{Key: "7d", UsedPct: 62}}},
			"pi":     {SpendUSD: &spend, TargetUSD: &target, TargetSource: &src, KeyLimitUSD: &keyLimit, KeyLimitRemainingUSD: &keyLeft, Windows: []data.Window{{Key: "month", UsedPct: 115.3}}},
		},
		AnthropicCredit: credit,
		GeminiBudget:    &data.GeminiBudget{TargetUSD: 40, SpentUSD: 10, RemainingUSD: 30, UsedPct: 25},
	}
	rows := map[string]view.BudgetRow{}
	for _, b := range view.BuildOverview(in).Monitors.Budgets {
		rows[b.Label] = b
	}
	if len(rows) != 4 {
		t.Fatalf("rows = %v, want claude subscription 7d, openrouter, claude api-key, google (gemini)", rows)
	}
	if got := rows["openrouter"].Text; got != "$34.59/$30 · key $88.00 left" {
		t.Errorf("openrouter text = %q", got)
	}
	if got := rows["claude api-key"]; got.Text != "$30.00/$50.00 · $20.00 left" || got.UsedPct != 60 || got.Critical {
		t.Errorf("claude api-key = %+v", got)
	}
	if got := rows["claude subscription 7d"]; got.Text != "" || got.Critical {
		t.Errorf("claude subscription 7d = %+v, want unchanged", got)
	}

	credit.Expired = true
	for _, b := range view.BuildOverview(in).Monitors.Budgets {
		if b.Label == "claude api-key" && (!b.Critical || b.Text != "$30.00/$50.00 · $20.00 left expired") {
			t.Errorf("expired row = %+v, want critical with expired text", b)
		}
	}

	in.Snap.Budget.Report.AnthropicCredit = nil
	for _, b := range view.BuildOverview(in).Monitors.Budgets {
		if b.Label == "claude api-key" {
			t.Errorf("anthropic row without a credit: %+v", b)
		}
	}
}
