package ui

import (
	"fmt"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/noamsto/dispatcher/dash/internal/data"
	vm "github.com/noamsto/dispatcher/dash/internal/view"
)

// fleetView is the machine-wide Fleet tab.
type fleetView struct {
	fv     vm.FleetView
	in     vm.FleetInputs
	loaded bool
}

func newFleetView(msg snapshotMsg) fleetView {
	return fleetView{fv: msg.fleet, in: msg.fleetIn, loaded: msg.loaded[srcFleet]}
}

func (v fleetView) Update(msg tea.Msg) (view, tea.Cmd) {
	if s, ok := msg.(snapshotMsg); ok {
		return newFleetView(s), nil
	}
	return v, nil
}

func (v fleetView) View(w, _ int) string {
	if !v.loaded {
		return loadingLine(srcFleet, w)
	}
	return truncateLine(fmt.Sprintf("%d crews · %d need action", v.fv.Summary.Crews, v.fv.Summary.Attention), w)
}

func (v fleetView) Keys() []key.Binding { return nil }

func (v fleetView) Selection() data.Selection { return nil }

func (v fleetView) Capturing() bool { return false }
