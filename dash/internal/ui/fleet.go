package ui

import (
	"os"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/noamsto/dispatcher/dash/internal/data"
	vm "github.com/noamsto/dispatcher/dash/internal/view"
)

// fleetView is the machine-wide Fleet tab. fv is cleaned once on arrival:
// peer and tailscale strings are untrusted.
type fleetView struct {
	fv     vm.FleetView
	in     vm.FleetInputs
	loaded bool
	st     fleetState
	pills  pillMode
}

func newFleetView(msg snapshotMsg) fleetView {
	v := fleetView{pills: pillModeOf(os.Getenv, lipgloss.ColorProfile())}
	v.apply(msg)
	return v
}

// apply takes a snapshot's fleet model and keeps the operator's state, which
// the layout resolves against it per frame.
func (v *fleetView) apply(msg snapshotMsg) {
	v.fv, v.in, v.loaded = cleanView(msg.fleet), msg.fleetIn, msg.loaded[srcFleet]
}

func (v fleetView) Update(msg tea.Msg) (view, tea.Cmd) {
	if s, ok := msg.(snapshotMsg); ok {
		v.apply(s)
	}
	return v, nil
}

func (v fleetView) View(w, h int) string {
	if !v.loaded {
		return strings.Join(normalizeFrame(loadingLine(srcFleet, w), h, w), "\n")
	}
	return renderFleet(w, h, v.fv, v.st, v.pills)
}

func (v fleetView) Keys() []key.Binding { return nil }

func (v fleetView) Selection() data.Selection { return nil }

func (v fleetView) Capturing() bool { return false }
