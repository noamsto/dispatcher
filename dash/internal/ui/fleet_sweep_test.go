package ui

import (
	"context"
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/noamsto/dispatcher/dash/internal/data"
	vm "github.com/noamsto/dispatcher/dash/internal/view"
	"github.com/noamsto/dispatcher/dash/internal/view/viewtest"
)

// fleetRootModel is a root Model on the loaded Fleet tab over in, with a fake
// runner so no action runs for real.
func fleetRootModel(t *testing.T, in vm.FleetInputs) Model {
	t.Helper()
	deps := Deps{
		Now:    viewtest.NowMS / 1000,
		Runner: &data.FakeRunner{Responses: map[string]data.FakeResponse{}},
		Crew:   []string{"crew"}, InTmux: true,
		Load: Loaders{Fleet: func(context.Context, func(vm.FleetInputs)) vm.FleetInputs { return in }},
	}
	m := NewModel(deps, fixedNow(loadSnapshot(t)), liveDeps{})
	m.active = liveTab
	nm, _ := m.Update(keyRune('8'))
	nm, _ = nm.Update(sourceMsg{src: srcFleet, apply: func(*data.Snapshot) {}, fleet: &in})
	return nm.(Model)
}

// fleetSelectCrew pins the Crews panel on the crew row key.
func fleetSelectCrew(t *testing.T, m Model, key string) Model {
	t.Helper()
	fl := m.views[fleetTab].(fleetView)
	selectCrew(t, &fl, key)
	m.views[fleetTab] = fl
	return m
}

func TestFleetSizeSweep(t *testing.T) {
	asciiProfile(t)
	typed := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(strings.Repeat("x", 300))}
	withErrors := func() vm.FleetInputs {
		in := viewtest.FleetInputs()
		e1, e2 := "exit 2", "unreadable"
		in.Fleet.QueueError, in.Fleet.TailscaleError = &e1, &e2
		return in
	}
	drilled := func(m Model) Model {
		return sweepKeys(fleetSelectCrew(t, m, keyB5), keyType(tea.KeyEnter))
	}
	states := []struct {
		name   string
		title  string // the overlay's top-border title; empty for none
		inputs func() vm.FleetInputs
		open   func(Model) Model
	}{
		{"normal", "", viewtest.FleetInputs, func(m Model) Model { return m }},
		{"help", "HELP", viewtest.FleetInputs, func(m Model) Model { return sweepKeys(m, keyRune('?')) }},
		{"filter", "FILTER", viewtest.FleetInputs, func(m Model) Model { return sweepKeys(m, keyRune('/'), typed) }},
		{"relaunch", "CONFIRM", viewtest.FleetInputs, func(m Model) Model { return sweepKeys(fleetSelectCrew(t, m, keyA3), keyRune('L')) }},
		{"drilled", "", viewtest.FleetInputs, drilled},
		{"drilled help", "HELP", viewtest.FleetInputs, func(m Model) Model { return sweepKeys(drilled(m), keyRune('?')) }},
		{"errors", "", withErrors, func(m Model) Model { return m }},
		{"errors help", "HELP", withErrors, func(m Model) Model { return sweepKeys(m, keyRune('?')) }},
	}
	sizes := [][2]int{{80, 24}}
	for w := 100; w <= 200; w += 10 {
		for h := 30; h <= 60; h += 5 {
			sizes = append(sizes, [2]int{w, h})
		}
	}
	for _, st := range states {
		base := fleetRootModel(t, st.inputs())
		for _, sz := range sizes {
			w, h := sz[0], sz[1]
			name := fmt.Sprintf("%dx%d %s", w, h, st.name)
			frame := st.open(sized(base, w, h)).View()
			if n := len(strings.Split(frame, "\n")); n != h {
				t.Errorf("%s: frame has %d lines, want %d", name, n, h)
				continue
			}
			assertWidth(t, frame, w, h)
			if st.title != "" && !hasOverlayTop(frame, st.title) {
				t.Errorf("%s: no overlay top border titled %s", name, st.title)
			}
			if strings.HasPrefix(st.name, "errors") && st.title == "" && !strings.Contains(stripped(frame), "source error") {
				t.Errorf("%s: errors line not shown", name)
			}
		}
	}
}

// TestFleetShortWideFocusDrawn pins that at wide, short sizes the focused
// panel is always drawn, whichever right-column panel holds focus.
func TestFleetShortWideFocusDrawn(t *testing.T) {
	asciiProfile(t)
	titles := map[fleetPanel]string{
		fpAttention: "ATTENTION", fpCrews: "CREWS", fpCapacity: "CAPACITY", fpHolds: "HOLDS", fpHosts: "HOSTS", fpEvents: "RECENT EVENTS",
	}
	for _, h := range []int{10, 12, 14} {
		m := sized(fleetRootModel(t, viewtest.FleetInputs()), 120, h)
		seen := map[fleetPanel]bool{}
		for range int(numFleetPanels) {
			m = sweepKeys(m, keyType(tea.KeyTab))
			f := m.views[fleetTab].(fleetView).st.focus
			seen[f] = true
			frame := m.View()
			assertWidth(t, frame, 120, h)
			ok := false
			for _, l := range strings.Split(stripped(frame), "\n") {
				ok = ok || strings.Contains(l, "╭") && strings.Contains(l, titles[f])
			}
			if !ok {
				t.Errorf("120x%d focus %v: panel %s not drawn", h, f, titles[f])
			}
		}
		if len(seen) != int(numFleetPanels) {
			t.Errorf("120x%d: tab visited %d focus values, want %d", h, len(seen), numFleetPanels)
		}
	}
}

func TestSplitHeightsFocusFirst(t *testing.T) {
	got := splitHeights(7, []int{5, 5, 5}, 2)
	if got[2] < fleetPanelMinH || got[0]+got[1]+got[2] != 7 {
		t.Errorf("splitHeights focus last = %v, want the third panel >= %d", got, fleetPanelMinH)
	}
}

// TestFleetPanelsDrawnAtLayoutRects pins that every shown panel's top border
// is drawn on its layout rect's row and column, so hit-testing matches pixels.
func TestFleetPanelsDrawnAtLayoutRects(t *testing.T) {
	asciiProfile(t)
	titles := map[fleetPanel]string{
		fpAttention: "ATTENTION", fpCrews: "CREWS", fpCapacity: "CAPACITY", fpHolds: "HOLDS", fpHosts: "HOSTS", fpEvents: "RECENT EVENTS",
	}
	withAttn := vm.BuildFleet(viewtest.FleetInputs())
	allClear := withAttn
	allClear.Attention = nil
	allClear.Summary.Attention = 0
	for _, c := range []struct {
		name string
		fv   vm.FleetView
	}{{"attention", withAttn}, {"allclear", allClear}} {
		fv := c.fv
		heights := []int{}
		for h := 3; h <= 60; h++ {
			heights = append(heights, h)
		}
		for w := 100; w <= 200; w += 10 {
			for _, h := range heights {
				for focus := range numFleetPanels {
					st := fleetState{focus: focus}
					r := fleetLayout(w, h, fv, st)
					lines := strings.Split(stripped(renderFleet(w, h, fv, st, pillRound)), "\n")
					for p, pl := range r.panels {
						if !pl.shown {
							continue
						}
						title := titles[fleetPanel(p)]
						if panelTopHas(lines, pl.rect, title) {
							continue
						}
						{
							row := -1
							for i, l := range lines {
								if strings.Contains(l, "╭") && strings.Contains(l, title) {
									row = i
								}
							}
							t.Errorf("%s %dx%d focus %d: %s rect (%d,%d) but title drawn on row %d", c.name, w, h, focus, title, pl.rect.x, pl.rect.y, row)
						}
					}
				}
			}
		}
	}
}

// panelTopHas reports whether the frame row at r.y has a top-left corner at
// column r.x and the title within the panel's width.
func panelTopHas(lines []string, r rect, title string) bool {
	if r.y >= len(lines) {
		return false
	}
	rs := []rune(lines[r.y])
	if r.x >= len(rs) || rs[r.x] != '╭' {
		return false
	}
	return strings.Contains(string(rs[r.x:min(r.x+r.w, len(rs))]), title)
}
