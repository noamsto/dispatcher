package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	vm "github.com/noamsto/dispatcher/dash/internal/view"
	"github.com/noamsto/dispatcher/dash/internal/view/viewtest"
)

func sessionRow(session string) vm.FleetCrewRow {
	return vm.FleetCrewRow{Dispatcher: &vm.FleetCrewDispatcher{State: "live", Session: session}}
}

func TestCrewSessionLabel(t *testing.T) {
	sub := sessionRow(vm.SessionNeedsYou)
	sub.Sub = true
	for _, tc := range []struct {
		name string
		row  vm.FleetCrewRow
		want string
	}{
		{"needs you", sessionRow(vm.SessionNeedsYou), "needs you"},
		{"standby", sessionRow(vm.SessionStandby), "standby"},
		{"idle", sessionRow(vm.SessionIdle), "awaiting you"},
		{"working", sessionRow(vm.SessionWorking), ""},
		{"unknown", sessionRow(vm.SessionUnknown), ""},
		{"unset", sessionRow(""), ""},
		{"sub row", sub, ""},
		{"peer row", vm.FleetCrewRow{}, ""},
	} {
		if got := crewSession(tc.row); got != tc.want {
			t.Errorf("%s: label = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestDispatcherGlyphNeedsYouIsBlocked(t *testing.T) {
	if got := dispatcherGlyphState(sessionRow(vm.SessionNeedsYou)); got != "blocked" {
		t.Errorf("needs_you glyph state = %q", got)
	}
	if got := dispatcherGlyphState(sessionRow(vm.SessionStandby)); got != "working" {
		t.Errorf("standby glyph state = %q", got)
	}
}

func TestFleetSessionColumnFits(t *testing.T) {
	asciiProfile(t)
	in := viewtest.FleetInputs()
	in.Fleet.Status.Repos[0].Crews[0].Dispatcher.PaneState = new("waiting")
	fv := vm.BuildFleet(in)
	for _, sz := range [][2]int{{80, 24}, {100, 30}, {120, 40}} {
		out := fleetFixtureView(fv).View(sz[0], sz[1])
		if !strings.Contains(out, "needs you") && sz[0] >= 100 {
			t.Errorf("%dx%d: no needs you label", sz[0], sz[1])
		}
		lines := strings.Split(out, "\n")
		if len(lines) != sz[1] {
			t.Errorf("%dx%d: %d lines", sz[0], sz[1], len(lines))
		}
		for _, l := range lines {
			if w := ansi.StringWidth(l); w > sz[0] {
				t.Errorf("%dx%d: line width %d: %q", sz[0], sz[1], w, l)
			}
		}
	}
}
