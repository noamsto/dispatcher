package ui

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/noamsto/dispatcher/dash/internal/data"
)

func newTestModel(t *testing.T) Model {
	t.Helper()
	return testModel(loadSnapshot(t))
}

func TestStatusLineStripsControlSequences(t *testing.T) {
	m := sized(newTestModel(t), 80, 24)
	nm, _ := m.Update(actionMsg{text: "dispatch-config: " + injectedErr, err: true})
	out := nm.(Model).View()
	requireNoControl(t, out)
	if !strings.Contains(out, "dispatch-config: boom") {
		t.Fatalf("status text missing from view:\n%s", out)
	}
}

func TestViewEmptyBeforeWindowSize(t *testing.T) {
	m := newTestModel(t)
	if got := m.View(); got != "" {
		t.Fatalf("View() before WindowSizeMsg = %q, want \"\"", got)
	}
}

func TestTabSwitchingByDigits(t *testing.T) {
	m := sized(newTestModel(t), 80, 24)
	for want, digit := range []rune{'1', '2', '3', '4', '5', '6', '7', '8'} {
		m.active = liveTab // the Overview owns digits, so every press starts from Live
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
	if m.active != boardTab {
		t.Fatalf("after tab: active = %d, want %d", m.active, boardTab)
	}
	nm, _ = m.Update(keyType(tea.KeyTab))
	m = nm.(Model)
	if m.active != budgetTab {
		t.Fatalf("after tab tab: active = %d, want %d", m.active, budgetTab)
	}
	nm, _ = m.Update(keyType(tea.KeyShiftTab))
	m = nm.(Model)
	if m.active != boardTab {
		t.Fatalf("after shift+tab: active = %d, want %d", m.active, boardTab)
	}
	// wraps around; Fleet and Overview own tab, so ] wraps
	m.active = fleetTab
	nm, _ = m.Update(keyRune(']'))
	m = nm.(Model)
	if m.active != overviewTab {
		t.Fatalf("tab wrap: active = %d, want %d", m.active, overviewTab)
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
	if got := m.loadStatus(); got != "loading settings, budget, retro, ratings, roster, bus…" {
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
	if got := m.loadStatus(); got != "loading settings, budget, retro, ratings, roster, bus…" {
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

func TestNumberInputSwallowsGlobalKeys(t *testing.T) {
	m := sized(newTestModel(t), 80, 24)
	m.active = settingsTab                   // settings
	nm, _ := m.Update(keyType(tea.KeyEnter)) // cursor on fleetBudget opens the number input
	m = nm.(Model)
	if !m.views[settingsTab].Capturing() {
		t.Fatalf("enter did not open the number input")
	}

	nm, cmd := m.Update(keyRune('q'))
	m = nm.(Model)
	if cmd != nil {
		if _, ok := cmd().(tea.QuitMsg); ok {
			t.Fatalf("q quit while capturing")
		}
	}

	nm, _ = m.Update(keyRune('3'))
	m = nm.(Model)
	if m.active != settingsTab {
		t.Fatalf("digit switched tab while capturing: active = %d", m.active)
	}

	// esc leaves capturing mode again, and q now quits as normal.
	nm, _ = m.Update(keyType(tea.KeyEsc))
	m = nm.(Model)
	if m.views[settingsTab].Capturing() {
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
	for _, view := range []int{liveTab, budgetTab} {
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

// hookView is a view whose enter and leave hooks record the order they run in.
type hookView struct {
	name string
	log  *[]string
}

func (v hookView) Update(tea.Msg) (view, tea.Cmd) { return v, nil }
func (hookView) View(int, int) string             { return "" }
func (hookView) Keys() []key.Binding              { return nil }
func (hookView) Selection() data.Selection        { return nil }
func (hookView) Capturing() bool                  { return false }

func (v hookView) enter(data.Selection) (view, tea.Cmd) {
	return v, func() tea.Msg { *v.log = append(*v.log, v.name+" enter"); return nil }
}

func (v hookView) leave() (view, tea.Cmd) {
	return v, func() tea.Msg { *v.log = append(*v.log, v.name+" leave"); return nil }
}

// runCmd runs cmd, descending into tea.Sequence and tea.Batch messages in
// order.
func runCmd(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	msg := cmd()
	v := reflect.ValueOf(msg)
	if v.Kind() != reflect.Slice {
		return
	}
	for i := range v.Len() {
		if c, ok := v.Index(i).Interface().(tea.Cmd); ok {
			runCmd(c)
		}
	}
}

func TestSwitchTabLeaveBeforeEnter(t *testing.T) {
	var log []string
	m := newTestModel(t) // not sized: syncMapBox stays idle with fake views
	m.active = overviewTab
	m.views[overviewTab] = hookView{name: "overview", log: &log}
	m.views[mapTab] = hookView{name: "map", log: &log}

	cmd := m.switchTab(mapTab)
	if cmd == nil {
		t.Fatal("switchTab returned no Cmd")
	}
	// A Batch would run the leave and enter concurrently; only a sequence
	// guarantees the leave lands first.
	msg := cmd()
	if got := fmt.Sprintf("%T", msg); got != "tea.sequenceMsg" {
		t.Fatalf("switchTab Cmd yields %s, want tea.sequenceMsg", got)
	}
	seq := reflect.ValueOf(msg)
	if seq.Len() == 0 {
		t.Fatal("empty sequence")
	}
	first, _ := seq.Index(0).Interface().(tea.Cmd)
	runCmd(first)
	if !slices.Equal(log, []string{"overview leave"}) {
		t.Fatalf("first sequence step ran %v, want the leave alone", log)
	}
	for i := 1; i < seq.Len(); i++ {
		c, _ := seq.Index(i).Interface().(tea.Cmd)
		runCmd(c)
	}

	want := []string{"overview leave", "map enter"}
	if !slices.Equal(log, want) {
		t.Fatalf("hook order = %v, want %v", log, want)
	}
}

func TestRenderTabsKeepsActiveVisible(t *testing.T) {
	asciiProfile(t)
	for w := 60; w < 80; w++ {
		for i, title := range tabTitles {
			m := Model{width: w, active: i}
			line := stripped(m.renderTabs())
			if lipgloss.Width(line) > w || !strings.Contains(line, "▸ "+title) {
				t.Errorf("width %d active %s: %q", w, title, line)
			}
		}
	}
}
