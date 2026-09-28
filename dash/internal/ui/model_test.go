package ui

import (
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
	for want, digit := range []rune{'1', '2', '3', '4'} {
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
	m.active = 3
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
	other := snap
	other.Now = snap.Now + 999
	calls := 0
	collect := func() data.Snapshot {
		calls++
		return other
	}
	m := sized(NewModel(snap, collect, fixedNow(snap), rosterDeps{}), 80, 24)

	nm, cmd := m.Update(keyRune('r'))
	m = nm.(Model)
	if !m.refreshing {
		t.Fatalf("r did not set refreshing")
	}
	if cmd == nil {
		t.Fatalf("r did not return a Cmd")
	}
	msg := cmd()
	sm, ok := msg.(snapshotMsg)
	if !ok {
		t.Fatalf("Cmd's msg = %T, want snapshotMsg", msg)
	}
	if data.Snapshot(sm).Now != other.Now {
		t.Fatalf("snapshotMsg.Now = %d, want %d", data.Snapshot(sm).Now, other.Now)
	}
	if calls != 1 {
		t.Fatalf("collect called %d times, want 1", calls)
	}

	nm, _ = m.Update(sm)
	m = nm.(Model)
	if m.refreshing {
		t.Fatalf("refreshing still true after snapshotMsg")
	}
	bv := m.views[1].(budgetView)
	if bv.now != other.Now {
		t.Fatalf("budget view did not pick up refreshed snapshot: now = %d, want %d", bv.now, other.Now)
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
	m := sized(newTestModel(t), 80, 24) // settings is the active (0) view
	nm, _ := m.Update(keyRune('/'))
	m = nm.(Model)
	if !m.views[0].Capturing() {
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
	if m.active != 0 {
		t.Fatalf("digit switched tab while capturing: active = %d", m.active)
	}

	// esc leaves capturing mode again, and q now quits as normal.
	nm, _ = m.Update(keyType(tea.KeyEsc))
	m = nm.(Model)
	if m.views[0].Capturing() {
		t.Fatalf("esc did not leave capturing mode")
	}
}

func TestResizeAllViewsExactFrame(t *testing.T) {
	sizes := [][2]int{{80, 24}, {60, 20}, {120, 40}, {20, 5}}
	for view := 0; view < 4; view++ {
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

// TestResizeRunsRosterWithContentExactFrame is TestResizeAllViewsExactFrame
// but over the G3 fixture (populated ratings/retro rows, live roster,
// holds) — the mostly-empty G1/G2 snapshot never exercises the widest
// content (long detail/pr cells, the ratings table, the detail panes) at a
// degenerate width like 20x5, which is exactly where a fixed-width column
// budget (roster.go) or a wrap/window computation (runs.go detail) is most
// likely to break.
func TestResizeRunsRosterWithContentExactFrame(t *testing.T) {
	snap := loadFullSnapshot(t)
	sizes := [][2]int{{80, 24}, {60, 20}, {120, 40}, {20, 5}}
	for view := 2; view <= 3; view++ {
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
