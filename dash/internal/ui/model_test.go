package ui

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/noamsto/dispatcher/dash/internal/data"
)

func newTestModel(t *testing.T) Model {
	t.Helper()
	return testModel(loadSnapshot(t))
}

func TestViewEmptyBeforeWindowSize(t *testing.T) {
	m := newTestModel(t)
	if got := m.View(); got != "" {
		t.Fatalf("View() before WindowSizeMsg = %q, want \"\"", got)
	}
}

func TestTabSwitchingByDigits(t *testing.T) {
	m := sized(newTestModel(t), 80, 24)
	for want, digit := range []rune{'1', '2', '3', '4', '5'} {
		nm, _ := m.Update(keyRune(digit))
		m = nm.(Model)
		if m.active != want {
			t.Fatalf("after %q: active = %d, want %d", digit, m.active, want)
		}
	}
}

func TestTabSwitchingByTab(t *testing.T) {
	m := sized(newTestModel(t), 80, 24)
	nm, _ := m.Update(keyType(tea.KeyTab))
	m = nm.(Model)
	if m.active != 1 {
		t.Fatalf("after tab: active = %d, want 1", m.active)
	}
	nm, _ = m.Update(keyType(tea.KeyTab))
	m = nm.(Model)
	if m.active != 2 {
		t.Fatalf("after tab tab: active = %d, want 2", m.active)
	}
	nm, _ = m.Update(keyType(tea.KeyShiftTab))
	m = nm.(Model)
	if m.active != 1 {
		t.Fatalf("after shift+tab: active = %d, want 1", m.active)
	}
	// wraps around
	m.active = mapTab
	nm, _ = m.Update(keyType(tea.KeyTab))
	m = nm.(Model)
	if m.active != 0 {
		t.Fatalf("tab wrap: active = %d, want 0", m.active)
	}
}

func TestHelpToggle(t *testing.T) {
	m := sized(newTestModel(t), 80, 24)
	if m.help.ShowAll {
		t.Fatalf("help starts expanded")
	}
	nm, _ := m.Update(keyRune('?'))
	m = nm.(Model)
	if !m.help.ShowAll {
		t.Fatalf("? did not expand help")
	}
	nm, _ = m.Update(keyRune('?'))
	m = nm.(Model)
	if m.help.ShowAll {
		t.Fatalf("? did not collapse help")
	}
}

func TestRefreshReplacesSnapshot(t *testing.T) {
	snap := loadSnapshot(t)
	other := snap.Budget
	calls := 0
	deps := Deps{Now: snap.Now, Load: Loaders{
		Budget: func(context.Context) data.BudgetSection {
			calls++
			return other
		},
	}}
	m := sized(loadedModel(snap, deps), 80, 24)

	nm, cmd := m.Update(keyRune('r'))
	m = nm.(Model)
	if cmd == nil {
		t.Fatalf("r did not return a Cmd")
	}
	if got := m.loadStatus(); got != "loading settings, budget, retro, ratings, roster…" {
		t.Fatalf("status after r = %q, want every source loading", got)
	}
	var sm sourceMsg
	for _, msg := range flattenCmd(cmd) {
		if s, ok := msg.(sourceMsg); ok && s.src == srcBudget {
			sm = s
		}
	}
	if sm.apply == nil {
		t.Fatalf("r did not reload the budget source")
	}
	if calls != 1 {
		t.Fatalf("budget loader called %d times, want 1", calls)
	}

	nm, _ = m.Update(sm)
	m = nm.(Model)
	if m.load[srcBudget].inFlight {
		t.Fatalf("budget still in flight after its sourceMsg")
	}
	if m.load[srcSettings].inFlight != true {
		t.Fatalf("settings landed before its own sourceMsg")
	}
}

func TestFooterLoadStatus(t *testing.T) {
	snap := loadSnapshot(t)
	m := sized(loadingModel(Deps{Now: snap.Now}), 120, 24)
	if got := m.loadStatus(); got != "loading settings, budget, retro, ratings, roster…" {
		t.Fatalf("fresh status = %q", got)
	}
	m = sized(testModel(snap), 120, 24)
	if got := m.loadStatus(); got != "refreshed "+fixedNow(snap)().Format("15:04:05") {
		t.Fatalf("loaded status = %q", got)
	}
	m.load[srcRetro].inFlight = true
	m.load[srcRoster].inFlight = true
	if got := m.loadStatus(); got != "loading retro, roster…" {
		t.Fatalf("partial status = %q", got)
	}
	var idle Model
	if got := idle.loadStatus(); got != "loading…" {
		t.Fatalf("nothing loaded status = %q", got)
	}
}

func TestQuitQuits(t *testing.T) {
	m := sized(newTestModel(t), 80, 24)
	_, cmd := m.Update(keyRune('q'))
	if cmd == nil {
		t.Fatalf("q did not return a Cmd")
	}
	msg := cmd()
	if _, ok := msg.(tea.QuitMsg); !ok {
		t.Fatalf("q's Cmd msg = %T, want tea.QuitMsg", msg)
	}

	_, cmd = m.Update(keyType(tea.KeyCtrlC))
	if cmd == nil {
		t.Fatalf("ctrl+c did not return a Cmd")
	}
	msg = cmd()
	if _, ok := msg.(tea.QuitMsg); !ok {
		t.Fatalf("ctrl+c's Cmd msg = %T, want tea.QuitMsg", msg)
	}
}

func TestFilterModeSwallowsGlobalKeys(t *testing.T) {
	m := sized(newTestModel(t), 80, 24)
	m.active = 3 // settings
	nm, _ := m.Update(keyRune('/'))
	m = nm.(Model)
	if !m.views[3].Capturing() {
		t.Fatalf("/ did not enter capturing mode")
	}

	nm, cmd := m.Update(keyRune('q'))
	m = nm.(Model)
	if cmd != nil {
		if _, ok := cmd().(tea.QuitMsg); ok {
			t.Fatalf("q quit while capturing")
		}
	}

	nm, _ = m.Update(keyRune('2'))
	m = nm.(Model)
	if m.active != 3 {
		t.Fatalf("digit switched tab while capturing: active = %d", m.active)
	}

	// esc leaves capturing mode again, and q now quits as normal.
	nm, _ = m.Update(keyType(tea.KeyEsc))
	m = nm.(Model)
	if m.views[3].Capturing() {
		t.Fatalf("esc did not leave capturing mode")
	}
}

func TestResizeAllViewsExactFrame(t *testing.T) {
	sizes := [][2]int{{80, 24}, {60, 20}, {120, 40}, {20, 5}}
	for view := range newTestModel(t).views {
		for _, sz := range sizes {
			m := sized(newTestModel(t), sz[0], sz[1])
			m.active = view
			out := m.View()
			lines := strings.Split(out, "\n")
			if len(lines) != sz[1] {
				t.Fatalf("view %d at %dx%d: got %d lines, want %d", view, sz[0], sz[1], len(lines), sz[1])
			}
			for i, l := range lines {
				if w := ansi.StringWidth(l); w > sz[0] {
					t.Fatalf("view %d at %dx%d: line %d width %d > %d: %q", view, sz[0], sz[1], i, w, sz[0], l)
				}
			}
		}
	}
}

// TestResizeLiveRunsWithContentExactFrame is TestResizeAllViewsExactFrame
// but over the full fixture (populated ratings/retro rows, live roster,
// holds) — the mostly-empty snapshot fixture never exercises the widest
// content (long detail/pr cells, the ratings table, the detail panes) at a
// degenerate width like 20x5, which is exactly where a column layout
// (live.go) or a wrap/window computation (runs.go detail) is most likely to
// break.
func TestResizeLiveRunsWithContentExactFrame(t *testing.T) {
	snap := loadFullSnapshot(t)
	sizes := [][2]int{{80, 24}, {60, 20}, {120, 40}, {20, 5}}
	for _, view := range []int{0, 2} {
		for _, sz := range sizes {
			m := sized(testModel(snap), sz[0], sz[1])
			m.active = view
			out := m.View()
			lines := strings.Split(out, "\n")
			if len(lines) != sz[1] {
				t.Fatalf("view %d at %dx%d: got %d lines, want %d", view, sz[0], sz[1], len(lines), sz[1])
			}
			for i, l := range lines {
				if w := ansi.StringWidth(l); w > sz[0] {
					t.Fatalf("view %d at %dx%d: line %d width %d > %d: %q", view, sz[0], sz[1], i, w, sz[0], l)
				}
			}
		}
	}
}
