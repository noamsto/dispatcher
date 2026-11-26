package ui

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	vm "github.com/noamsto/dispatcher/dash/internal/view"
	"github.com/noamsto/dispatcher/dash/internal/view/viewtest"
)

// narrowInputs is the shared fixture, which already carries attention items,
// agents, budget rows and bus events; the test pins that so a fixture change
// cannot silently thin the goldens.
func narrowInputs(t *testing.T) vm.Inputs {
	t.Helper()
	in := viewtest.Inputs()
	ov := vm.BuildOverview(in)
	if len(ov.Attention) < 3 || len(ov.Agents) < 3 || len(ov.Events) == 0 || len(ov.Monitors.Budgets) == 0 {
		t.Fatalf("fixture too thin: %d attention, %d agents, %d events, %d budget rows",
			len(ov.Attention), len(ov.Agents), len(ov.Events), len(ov.Monitors.Budgets))
	}
	return in
}

func TestOverviewNarrowGolden(t *testing.T) {
	asciiProfile(t)
	for _, sz := range [][2]int{{50, 30}, {64, 30}, {80, 30}, {64, 80}} {
		w, h := sz[0], sz[1]
		v := newOverviewView(narrowInputs(t), overviewNow)
		if !overviewLayout(w, h, v.model, v.st).narrow {
			t.Fatalf("%dx%d is not narrow", w, h)
		}
		checkGolden(t, fmt.Sprintf("overview_narrow_%dx%d.golden", w, h), v.View(w, h), w, h)
	}
}

func TestEveryTabRendersAt50(t *testing.T) {
	asciiProfile(t)
	for tab := range numTabs {
		m := sized(overviewModel(sweepInputs()), 50, 24)
		m.active = tab
		frame := m.View()
		assertWidth(t, frame, 50, 24)
	}
}

func TestNarrowForcedStacksAtWidth120(t *testing.T) {
	in := narrowInputs(t)
	forced := newOverviewView(in, overviewNow)
	forced.st.narrow = true
	r := overviewLayout(120, 40, forced.model, forced.st)
	if !r.narrow {
		t.Fatalf("forced narrow at 120 is not narrow")
	}
	for _, p := range []ovPanel{panelAgents, panelChecklists, panelMonitors} {
		if !r.panels[p].shown {
			t.Errorf("panel %v not shown", p)
		}
	}
	for i := 1; i < len(r.stack); i++ {
		a, b := r.panels[r.stack[i-1]].rect, r.panels[r.stack[i]].rect
		if a.x != 0 || b.x != 0 || b.y != a.y+a.h {
			t.Errorf("panels %v and %v are not stacked: %+v %+v", r.stack[i-1], r.stack[i], a, b)
		}
	}
	if overviewLayout(120, 40, forced.model, newOverviewView(in, overviewNow).st).narrow {
		t.Errorf("default at 120 is narrow")
	}
}

func TestNarrowDepsAndResize(t *testing.T) {
	in := narrowInputs(t)
	mk := func(narrow bool) Model {
		m := overviewModel(in)
		if narrow {
			ov := m.views[overviewTab].(overviewView)
			ov.st.narrow = true
			m.views[overviewTab] = ov
		}
		return m
	}
	layout := func(m Model) overviewRects {
		ov := m.views[overviewTab].(overviewView)
		return ov.layout()
	}
	if !layout(sized(mk(true), 120, 40)).narrow {
		t.Errorf("forced model at 120 is not narrow")
	}
	if layout(sized(mk(false), 120, 40)).narrow {
		t.Errorf("default model at 120 is narrow")
	}
	m := sized(mk(false), 120, 40)
	m = sized(m, 60, 30)
	if !layout(m).narrow {
		t.Errorf("resize 120 -> 60 did not go narrow")
	}
	nm := NewModel(Deps{Narrow: true}, overviewNow, liveDeps{})
	if !nm.views[overviewTab].(overviewView).st.narrow {
		t.Errorf("Deps.Narrow did not reach the Overview")
	}
	if NewModel(Deps{}, overviewNow, liveDeps{}).views[overviewTab].(overviewView).st.narrow {
		t.Errorf("default Deps forces narrow")
	}
}

func TestNarrowKeepsFocusedMonitors(t *testing.T) {
	v := newOverviewView(narrowInputs(t), overviewNow)
	v.st.narrow = true
	v.st.focus = panelMonitors
	r := overviewLayout(64, 14, v.model, v.st)
	if !r.panels[panelMonitors].shown {
		t.Errorf("focused Monitors dropped at 64x14: %+v", r.stack)
	}
}

func TestNarrowClickHitTest(t *testing.T) {
	v := newOverviewView(narrowInputs(t), overviewNow)
	v.View(64, 40)
	r := v.layout()
	if !r.narrow {
		t.Fatalf("64 cols is not narrow")
	}
	for _, p := range []ovPanel{panelAgents, panelMonitors} {
		pl := r.panels[p]
		if !pl.shown || len(pl.items) == 0 {
			t.Fatalf("panel %v not shown with items at 64x40", p)
		}
		it := pl.items[0]
		nv, _ := v.Update(tea.MouseMsg{X: it.rect.x, Y: it.rect.y, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
		if got := nv.(overviewView).st.focus; got != p {
			t.Errorf("click on panel %v row focused %v", p, got)
		}
	}
}

func TestNarrowTabReachesEveryPanel(t *testing.T) {
	m := sized(overviewModel(narrowInputs(t)), 64, 14)
	ov := m.views[overviewTab].(overviewView)
	if !ov.layout().narrow {
		t.Fatalf("64x14 is not narrow")
	}
	seen := map[ovPanel]bool{}
	for range 6 {
		m = sweepKeys(m, keyType(tea.KeyTab))
		seen[m.views[overviewTab].(overviewView).st.focus] = true
	}
	for _, p := range []ovPanel{panelAttention, panelAgents, panelChecklists, panelMonitors} {
		if !seen[p] {
			t.Errorf("tab never focused panel %v at 64x14", p)
		}
	}
}

func TestTabOrderFollowsLayout(t *testing.T) {
	for _, tc := range []struct {
		w, h int
		want []ovPanel
	}{
		{64, 30, []ovPanel{panelAgents, panelChecklists, panelMonitors, panelAttention}},
		{120, 40, []ovPanel{panelChecklists, panelAgents, panelMonitors, panelAttention}},
	} {
		m := sized(overviewModel(narrowInputs(t)), tc.w, tc.h)
		ov := m.views[overviewTab].(overviewView)
		ov.st.focus = panelAttention
		m.views[overviewTab] = ov
		for i, want := range tc.want {
			m = sweepKeys(m, keyType(tea.KeyTab))
			if got := m.views[overviewTab].(overviewView).st.focus; got != want {
				t.Errorf("%dx%d tab %d: focus %v, want %v", tc.w, tc.h, i+1, got, want)
			}
		}
	}
}

func TestFitLabelKeepsWindowKey(t *testing.T) {
	for label, want := range map[string]string{
		"claude subscription 5h": "claude sub 5h",
		"claude subscription 7d": "claude sub 7d",
	} {
		if got := fitLabel(label, 13); got != want {
			t.Errorf("fitLabel(%q, 13) = %q, want %q", label, got, want)
		}
	}
	if a, b := fitLabel("claude subscription 5h", 9), fitLabel("claude subscription 7d", 9); a == b || !strings.HasSuffix(a, "5h") || !strings.HasSuffix(b, "7d") {
		t.Errorf("narrow labels %q, %q lost the window key", a, b)
	}
	if got := fitLabel("claude subscription 5h", 22); got != "claude subscription 5h" {
		t.Errorf("wide label changed: %q", got)
	}
}
