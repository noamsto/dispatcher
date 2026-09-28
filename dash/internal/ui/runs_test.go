package ui

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/noamsto/dispatcher/dash/internal/data"
)

func loadFullSnapshot(t *testing.T) data.Snapshot {
	t.Helper()
	raw, err := os.ReadFile("testdata/snapshot-full.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var snap data.Snapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}
	return snap
}

func newTestRunsView(t *testing.T) runsView {
	t.Helper()
	return newRunsView(loadFullSnapshot(t))
}

func TestRunsDefaultSortIsTierEngineModel(t *testing.T) {
	v := newTestRunsView(t)
	if got := len(v.ratings); got != 3 {
		t.Fatalf("len(ratings) = %d, want 3", got)
	}
	want := []string{"deep", "standard", "trivial"}
	for i, w := range want {
		if v.ratings[i].Tier != w {
			t.Fatalf("ratings[%d].Tier = %q, want %q (full order: %+v)", i, v.ratings[i].Tier, w, v.ratings)
		}
	}
	if sortColumns[v.sortCol] != "tier" || v.desc {
		t.Fatalf("default sort = %q desc=%v, want tier asc", sortColumns[v.sortCol], v.desc)
	}
}

func TestRunsSortCyclingAndOrder(t *testing.T) {
	v := newTestRunsView(t)
	// s cycles tier -> engine -> model -> n -> pr% -> success -> burn.
	wantCycle := []string{"engine", "model", "n", "pr%", "success", "burn", "tier"}
	for _, want := range wantCycle {
		nv, _ := v.Update(keyRune('s'))
		v = nv.(runsView)
		if got := sortColumns[v.sortCol]; got != want {
			t.Fatalf("after s: sort column = %q, want %q", got, want)
		}
	}

	// Land on "n" and check ascending order: B(2) < C(4) < A(8).
	for sortColumns[v.sortCol] != "n" {
		nv, _ := v.Update(keyRune('s'))
		v = nv.(runsView)
	}
	wantAsc := []string{"standard", "trivial", "deep"} // n = 2, 4, 8
	for i, w := range wantAsc {
		if v.ratings[i].Tier != w {
			t.Fatalf("n asc[%d].Tier = %q, want %q", i, v.ratings[i].Tier, w)
		}
	}

	nv, _ := v.Update(keyRune('o'))
	v = nv.(runsView)
	if !v.desc {
		t.Fatalf("o did not flip to descending")
	}
	wantDesc := []string{"deep", "trivial", "standard"}
	for i, w := range wantDesc {
		if v.ratings[i].Tier != w {
			t.Fatalf("n desc[%d].Tier = %q, want %q", i, v.ratings[i].Tier, w)
		}
	}
}

func TestRunsRatingsMarkers(t *testing.T) {
	v := newTestRunsView(t)
	byTier := map[string]ratingGroup{}
	for _, g := range v.ratings {
		byTier[g.Tier] = g
	}

	deep := byTier["deep"]
	if got := renderAgg(deep.MergePct.K, deep.MergePct.N, optNumber(deep.MergePct.Value)); got != "87.5(7)" {
		t.Errorf("deep merge_pct marker = %q, want 87.5(7)", got)
	}
	if got := renderAgg(deep.N.K, deep.N.N, optNumber(deep.N.Value)); got != "8" {
		t.Errorf("deep n marker = %q, want 8 (no marker)", got)
	}

	standard := byTier["standard"]
	if got := renderAgg(standard.PrPct.K, standard.PrPct.N, optNumber(standard.PrPct.Value)); got != "100!" {
		t.Errorf("standard pr_pct marker = %q, want 100!", got)
	}
	if got := renderAgg(standard.MergePct.K, standard.MergePct.N, optNumber(standard.MergePct.Value)); got != "—" {
		t.Errorf("standard merge_pct marker = %q, want —", got)
	}

	trivial := byTier["trivial"]
	if got := renderAgg(trivial.BurnMedian.K, trivial.BurnMedian.N, optFmt1(trivial.BurnMedian.Value)); got != "0.8(3)!" {
		t.Errorf("trivial burn_median marker = %q, want 0.8(3)!", got)
	}
}

func TestRunsFocusSwitch(t *testing.T) {
	v := newTestRunsView(t)
	if v.focus != 0 {
		t.Fatalf("initial focus = %d, want 0 (ratings)", v.focus)
	}
	nv, _ := v.Update(keyRune('f'))
	v = nv.(runsView)
	if v.focus != 1 {
		t.Fatalf("after f: focus = %d, want 1 (runs list)", v.focus)
	}
	nv, _ = v.Update(keyRune('f'))
	v = nv.(runsView)
	if v.focus != 0 {
		t.Fatalf("after f f: focus = %d, want 0", v.focus)
	}
}

func TestRunsListNewestFirst(t *testing.T) {
	v := newTestRunsView(t)
	wantT0 := []int64{1789999500000, 1789998000000, 1789992000000, 1789990000000}
	if len(v.rows) != len(wantT0) {
		t.Fatalf("len(rows) = %d, want %d", len(v.rows), len(wantT0))
	}
	for i, want := range wantT0 {
		if v.rows[i].T0 != want {
			t.Fatalf("rows[%d].T0 = %d, want %d", i, v.rows[i].T0, want)
		}
	}
}

func TestRunsSelectionOnlyWhenListFocused(t *testing.T) {
	v := newTestRunsView(t)
	if sel := v.Selection(); sel != nil {
		t.Fatalf("Selection() with table focused = %+v, want nil", sel)
	}
	nv, _ := v.Update(keyRune('f'))
	v = nv.(runsView)
	sel, ok := v.Selection().(data.Run)
	if !ok {
		t.Fatalf("Selection() with list focused = %T, want data.Run", v.Selection())
	}
	if sel.Branch != "feat/584-crew-dash" || sel.T0 != 1789999500000 {
		t.Fatalf("Selection() = %+v, want the newest run row", sel)
	}
}

func TestRunsEnterOpensDetailAndEscReturns(t *testing.T) {
	v := newTestRunsView(t)
	nv, _ := v.Update(keyRune('f')) // focus the list
	v = nv.(runsView)

	nv, _ = v.Update(keyType(tea.KeyEnter))
	v = nv.(runsView)
	if !v.detail {
		t.Fatalf("enter did not open the detail pane")
	}
	out := v.View(80, 24)
	if !strings.Contains(out, "feat/584-crew-dash") {
		t.Fatalf("detail view missing branch header:\n%s", out)
	}
	if !strings.Contains(out, "deep/claude/opus") {
		t.Fatalf("detail view missing tier/engine/model:\n%s", out)
	}
	if !strings.Contains(out, "review · gate_thrash") {
		t.Fatalf("detail view missing a note's seam · tag line:\n%s", out)
	}

	nv, _ = v.Update(keyType(tea.KeyEsc))
	v = nv.(runsView)
	if v.detail {
		t.Fatalf("esc did not close the detail pane")
	}
}

func TestRunsDetailWrapsLongDetailWithinWidth(t *testing.T) {
	v := newTestRunsView(t)
	nv, _ := v.Update(keyRune('f'))
	v = nv.(runsView)
	nv, _ = v.Update(keyType(tea.KeyEnter))
	v = nv.(runsView)

	out := v.View(40, 24)
	for _, line := range strings.Split(out, "\n") {
		if w := ansi.StringWidth(line); w > 40 {
			t.Fatalf("detail line exceeds width 40 (%d): %q", w, line)
		}
	}
	if !strings.Contains(out, "eighty") {
		t.Fatalf("detail at width 40 should still contain a fragment of the long note (wrapped, not dropped):\n%s", out)
	}
}

func TestRunsEmptyStates(t *testing.T) {
	snap := loadFullSnapshot(t)
	snap.Runs.Retro.Rows = nil
	snap.Runs.Ratings = nil
	v := newRunsView(snap)
	out := v.View(80, 24)
	if !strings.Contains(out, "no retro notes yet") {
		t.Errorf("empty retro should read \"no retro notes yet\":\n%s", out)
	}
	if !strings.Contains(out, "no runs swept for this repo yet") {
		t.Errorf("empty ratings should read \"no runs swept for this repo yet\":\n%s", out)
	}
}

func TestRunsSourceErrors(t *testing.T) {
	snap := loadFullSnapshot(t)
	retroErr := "exit 1"
	ratingsErr := "exit 2"
	snap.Runs.RetroError = &retroErr
	snap.Runs.RatingsError = &ratingsErr
	v := newRunsView(snap)
	out := v.View(80, 24)
	if !strings.Contains(out, "unavailable: exit 1") {
		t.Errorf("retro error should read unavailable: exit 1:\n%s", out)
	}
	if !strings.Contains(out, "unavailable: exit 2") {
		t.Errorf("ratings error should read unavailable: exit 2:\n%s", out)
	}
}
