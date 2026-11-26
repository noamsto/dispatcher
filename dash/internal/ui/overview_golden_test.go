package ui

import (
	"strings"

	"fmt"
	"github.com/noamsto/dispatcher/dash/internal/data"
	"testing"
	"time"

	vm "github.com/noamsto/dispatcher/dash/internal/view"
	"github.com/noamsto/dispatcher/dash/internal/view/viewtest"
)

func overviewNow() time.Time { return time.UnixMilli(viewtest.NowMS) }

// allClearInputs is the fixture with every attention source removed.
func allClearInputs() vm.Inputs {
	in := viewtest.Inputs()
	crews := in.Snap.Roster.Crews
	for i := range crews {
		for _, w := range crews[i].Workers {
			w["state"] = "working"
		}
		crews[i].Holds = nil
		crews[i].Lane.State = "alive"
	}
	in.Snap.Roster.Peers = nil
	in.Bus.Roles = nil
	return in
}

func TestOverviewGolden(t *testing.T) {
	asciiProfile(t)
	in := viewtest.Inputs()
	for _, sz := range [][2]int{{80, 24}, {100, 30}, {150, 40}, {200, 60}} {
		w, h := sz[0], sz[1]
		v := newOverviewView(in, overviewNow)
		checkGolden(t, fmt.Sprintf("overview_%dx%d.golden", w, h), v.View(w, h), w, h)
	}
}

func TestOverviewGoldenAllClear(t *testing.T) {
	asciiProfile(t)
	v := newOverviewView(allClearInputs(), overviewNow)
	if n := len(v.model.Attention); n != 0 {
		t.Fatalf("fixture still has %d attention items", n)
	}
	r := overviewLayout(100, 30, v.model, v.st)
	if !r.allClear || r.panels[panelAttention].shown {
		t.Fatalf("all-clear layout has a strip panel: %+v", r)
	}
	checkGolden(t, "overview_allclear_100x30.golden", v.View(100, 30), 100, 30)
}

// emptyCrewInputs is a crew with no workers, holds, peers or bus rows.
func emptyCrewInputs() vm.Inputs {
	in := viewtest.Inputs()
	crews := in.Snap.Roster.Crews
	for i := range crews {
		crews[i].Workers = nil
		crews[i].Holds = nil
	}
	in.Snap.Roster.Peers = nil
	in.Snap.Budget = data.BudgetSection{}
	in.Bus = data.BusDigest{
		Workers: map[data.WorkerKey]*data.WorkerDigest{},
		Roles:   map[data.RoleKey]*data.RoleDigest{},
		PRs:     map[int]*data.PRWatch{},
		Recent:  []data.RecentEvent{},
	}
	return in
}

func TestOverviewGoldenEmptyCrew(t *testing.T) {
	asciiProfile(t)
	v := newOverviewView(emptyCrewInputs(), overviewNow)
	out := v.View(100, 30)
	for _, want := range []string{"no live workers in this crew", "no live agents", "nothing watched", "no bus events yet"} {
		if !strings.Contains(out, want) {
			t.Errorf("empty crew render lacks %q", want)
		}
	}
	checkGolden(t, "overview_empty_100x30.golden", out, 100, 30)
}

// The detail overlay's "To clear:" line wraps to the overlay width with
// continuation lines under the text; feat/h has the long reap fallback.
func TestOverviewDetailClearsGolden(t *testing.T) {
	asciiProfile(t)
	for _, sz := range [][2]int{{80, 24}, {150, 40}} {
		w, h := sz[0], sz[1]
		v := newOverviewView(viewtest.Inputs(), overviewNow)
		v.View(w, h)
		selectBranch(t, &v, "feat/h")
		v, _ = ovKeys(v, "enter")
		if v.mode != ovDetail {
			t.Fatalf("mode %v, want detail", v.mode)
		}
		got := stripped(v.View(w, h))
		if !strings.Contains(got, "To clear: reaped once") || !strings.Contains(got, "or re-dispatch") {
			t.Errorf("%dx%d: clears text truncated:\n%s", w, h, got)
		}
		checkGolden(t, fmt.Sprintf("overview_detail_clears_%dx%d.golden", w, h), got, w, h)
	}
}

func TestOverviewNotesDispatcherElsewhere(t *testing.T) {
	asciiProfile(t)
	in := viewtest.Inputs()
	in.Snap.Roster.Peers = nil
	if got := stripped(newOverviewView(in, overviewNow).View(150, 40)); strings.Contains(got, "dispatcher in") {
		t.Fatalf("no dispatcher location, yet the note renders:\n%s", got)
	}
	repo := "other-repo"
	in.Snap.Roster.Crews[0].Dispatcher = &data.DispatcherLoc{Where: "elsewhere", Repo: &repo}
	if got := stripped(newOverviewView(in, overviewNow).View(150, 40)); !strings.Contains(got, "dispatcher in other-repo") {
		t.Errorf("elsewhere crew has no dispatcher note:\n%s", got)
	}
	in.Snap.Roster.Crews[0].Dispatcher = &data.DispatcherLoc{Where: "here"}
	if got := stripped(newOverviewView(in, overviewNow).View(150, 40)); strings.Contains(got, "dispatcher in") {
		t.Errorf("here crew renders the note:\n%s", got)
	}
}

// providerBudgetInputs is the fixture with pi's OpenRouter month window (local
// target and key limit), the Anthropic credit, Gemini and both per-model
// subscription windows.
func providerBudgetInputs() vm.Inputs {
	spend, target, keyLimit, keyLeft := 34.59, 30.0, 100.0, 88.0
	src := "config"
	in := viewtest.Inputs()
	in.Snap.Budget.Report = &data.BudgetReport{
		Engines: map[string]*data.EngineBudget{
			"claude": {Windows: []data.Window{
				{Key: "5h", UsedPct: 10}, {Key: "7d", UsedPct: 62},
				{Key: "7d_opus", UsedPct: 40}, {Key: "7d_sonnet", UsedPct: 25},
			}},
			"pi": {
				SpendUSD: &spend, TargetUSD: &target, TargetSource: &src, KeyLimitUSD: &keyLimit, KeyLimitRemainingUSD: &keyLeft,
				Windows: []data.Window{{Key: "month", UsedPct: 115.3}},
			},
		},
		AnthropicCredit: &data.AnthropicCredit{BalanceUSD: 50, SpentUSD: 30, RemainingUSD: 20, UsedPct: 60},
		GeminiBudget:    &data.GeminiBudget{TargetUSD: 40, SpentUSD: 10, RemainingUSD: 30, UsedPct: 25},
	}
	return in
}

func TestOverviewGoldenProviderBudgets(t *testing.T) {
	asciiProfile(t)
	for _, sz := range [][2]int{{100, 30}, {200, 60}} {
		w, h := sz[0], sz[1]
		v := newOverviewView(providerBudgetInputs(), overviewNow)
		checkGolden(t, fmt.Sprintf("overview_budgets_%dx%d.golden", w, h), v.View(w, h), w, h)
	}
}
