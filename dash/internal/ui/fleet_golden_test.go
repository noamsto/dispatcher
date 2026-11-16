package ui

import (
	"fmt"
	"testing"

	vm "github.com/noamsto/dispatcher/dash/internal/view"
	"github.com/noamsto/dispatcher/dash/internal/view/viewtest"
)

func fleetFixtureView(fv vm.FleetView) fleetView {
	var loaded [numSources]bool
	loaded[srcFleet] = true
	return newFleetView(snapshotMsg{fleet: fv, loaded: loaded})
}

func TestFleetGolden(t *testing.T) {
	asciiProfile(t)
	fv := vm.BuildFleet(viewtest.FleetInputs())
	for _, sz := range [][2]int{{100, 30}, {120, 40}, {200, 60}} {
		w, h := sz[0], sz[1]
		checkGolden(t, fmt.Sprintf("fleet_%dx%d.golden", w, h), fleetFixtureView(fv).View(w, h), w, h)
	}

	narrow := fleetFixtureView(fv)
	narrow.st.focus = fpCrews
	checkGolden(t, "fleet_narrow_80x24.golden", narrow.View(80, 24), 80, 24)

	clear := fv
	clear.Attention = nil
	clear.Summary.Attention = 0
	v := fleetFixtureView(clear)
	r := fleetLayout(120, 40, v.fv, v.st)
	if !r.allClear || r.panels[fpAttention].shown {
		t.Fatalf("all-clear layout has a strip panel: %+v", r)
	}
	checkGolden(t, "fleet_allclear_120x40.golden", v.View(120, 40), 120, 40)

	// No tailscale binary: the Hosts panel still renders, reach from ssh.
	noTS := fleetFixtureView(vm.BuildFleet(viewtest.FleetInputsNoTailscale()))
	checkGolden(t, "fleet_notailscale_120x40.golden", noTS.View(120, 40), 120, 40)
}
