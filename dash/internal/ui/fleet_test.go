package ui

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/noamsto/dispatcher/dash/internal/data"
	vm "github.com/noamsto/dispatcher/dash/internal/view"
	"github.com/noamsto/dispatcher/dash/internal/view/viewtest"
)

func fleetModel(t *testing.T, calls *atomic.Int32) Model {
	t.Helper()
	deps := Deps{Now: viewtest.NowMS / 1000, Load: Loaders{
		Fleet: func(context.Context) vm.FleetInputs {
			calls.Add(1)
			return viewtest.FleetInputs()
		},
	}}
	m := NewModel(deps, fixedNow(loadSnapshot(t)), liveDeps{})
	m.active = liveTab
	return sized(m, 120, 40)
}

// fleetLoads counts the fleet loads cmd would start, by running every leaf.
func fleetLoads(cmd tea.Cmd) int {
	n := 0
	for _, msg := range flattenFleet(cmd) {
		if sm, ok := msg.(sourceMsg); ok && sm.src == srcFleet {
			n++
		}
	}
	return n
}

// flattenFleet runs cmd's leaves except the tick timers, which would block.
func flattenFleet(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	var out []tea.Msg
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	select {
	case msg := <-done:
		if batch, ok := msg.(tea.BatchMsg); ok {
			for _, c := range batch {
				out = append(out, flattenFleet(c)...)
			}
			return out
		}
		return append(out, msg)
	case <-time.After(50 * time.Millisecond):
		return nil
	}
}

func TestFleetTabLazyLoad(t *testing.T) {
	var calls atomic.Int32
	m := fleetModel(t, &calls)

	if got := fleetLoads(m.Init()); got != 0 || calls.Load() != 0 {
		t.Fatalf("Init requested fleet: loads=%d calls=%d", got, calls.Load())
	}
	if m.load[srcFleet].inFlight {
		t.Fatal("fleet marked in flight before activation")
	}
	if got := m.loadStatus(); got == "" || strings.Contains(got, "fleet") {
		t.Fatalf("footer lists fleet before activation: %q", got)
	}

	nm, cmd := m.Update(keyRune('8'))
	m = nm.(Model)
	if m.active != fleetTab {
		t.Fatalf("active = %d, want fleetTab", m.active)
	}
	if got := fleetLoads(cmd); got != 1 {
		t.Fatalf("switch to Fleet requested fleet %d times, want 1", got)
	}
	if !m.load[srcFleet].inFlight {
		t.Fatal("fleet not in flight after activation")
	}

	// away and back while the first load is in flight: no second load.
	nm, _ = m.Update(keyRune('2'))
	nm, cmd = nm.(Model).Update(keyRune('8'))
	m = nm.(Model)
	if got := fleetLoads(cmd); got != 0 {
		t.Fatalf("re-entry during flight started %d loads", got)
	}

	// land the result
	in := viewtest.FleetInputs()
	nm, _ = m.Update(sourceMsg{src: srcFleet, apply: func(*data.Snapshot) {}, fleet: &in})
	m = nm.(Model)
	if !m.load[srcFleet].loaded {
		t.Fatal("fleet not loaded after landing")
	}
	if m.views[fleetTab].(fleetView).fv.Summary.Crews == 0 {
		t.Fatal("fleet view not built from the landed inputs")
	}

	// R on Fleet refreshes it again
	nm, cmd = m.Update(keyRune('R'))
	m = nm.(Model)
	if got := fleetLoads(cmd); got != 1 {
		t.Fatalf("R requested fleet %d times, want 1", got)
	}
	if calls.Load() != 2 {
		t.Fatalf("loader calls = %d, want 2", calls.Load())
	}
}

func TestRefreshAllSkipsUnopenedFleet(t *testing.T) {
	var calls atomic.Int32
	m := fleetModel(t, &calls)
	for i := range m.load {
		m.load[i].inFlight = false
	}
	if got := fleetLoads(m.requestAll()); got != 0 {
		t.Fatalf("R before the Fleet tab opened requested fleet %d times", got)
	}
}

func TestFleetTabTick(t *testing.T) {
	var calls atomic.Int32
	m := fleetModel(t, &calls)
	nm, _ := m.Update(keyRune('8'))
	m = nm.(Model)
	gen := m.fleetGen
	m.load[srcFleet].inFlight = false

	nm, cmd := m.Update(fleetTickMsg{gen: gen})
	m = nm.(Model)
	if cmd == nil {
		t.Fatal("tick while Fleet active scheduled nothing")
	}
	if !m.load[srcFleet].inFlight {
		t.Fatal("tick did not request fleet")
	}

	// stale generation: dropped
	if _, cmd := m.Update(fleetTickMsg{gen: gen - 1}); cmd != nil {
		t.Fatal("stale tick rescheduled")
	}

	// another tab active: dropped, nothing requested
	m.active = liveTab
	m.load[srcFleet].inFlight = false
	nm, cmd = m.Update(fleetTickMsg{gen: gen})
	if cmd != nil || nm.(Model).load[srcFleet].inFlight {
		t.Fatal("tick off the Fleet tab scheduled work")
	}
}

func TestDigit8RoutesToFleet(t *testing.T) {
	if tabIndex("8") != fleetTab {
		t.Fatalf("tabIndex(8) = %d, want %d", tabIndex("8"), fleetTab)
	}
	if tabTitles[fleetTab] != "Fleet" {
		t.Fatalf("title = %q", tabTitles[fleetTab])
	}
}
