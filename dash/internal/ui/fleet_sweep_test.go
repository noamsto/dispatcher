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
		Load: Loaders{Fleet: func(context.Context) vm.FleetInputs { return in }},
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
