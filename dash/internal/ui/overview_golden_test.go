package ui

import (
	"fmt"
	"testing"
	"time"

	vm "github.com/noamsto/dispatcher/dash/internal/view"
	"github.com/noamsto/dispatcher/dash/internal/view/viewtest"
)

func overviewNow() time.Time { return time.UnixMilli(viewtest.NowMS) }

// allClearInputs is the fixture with every attention source removed.
func allClearInputs() vm.Inputs {
	in := viewtest.Inputs()
	crews := in.Snap.Roster.Crews
	for i := range crews {
		for _, w := range crews[i].Workers {
			w["state"] = "working"
		}
		crews[i].Holds = nil
		crews[i].Lane.State = "alive"
	}
	in.Snap.Roster.Peers = nil
	in.Bus.Roles = nil
	return in
}

func TestOverviewGolden(t *testing.T) {
	asciiProfile(t)
	in := viewtest.Inputs()
	for _, sz := range [][2]int{{80, 24}, {100, 30}, {150, 40}, {200, 60}} {
		w, h := sz[0], sz[1]
		v := newOverviewView(in, overviewNow)
		checkGolden(t, fmt.Sprintf("overview_%dx%d.golden", w, h), v.View(w, h), w, h)
	}
}

func TestOverviewGoldenAllClear(t *testing.T) {
	asciiProfile(t)
	v := newOverviewView(allClearInputs(), overviewNow)
	if n := len(v.model.Attention); n != 0 {
		t.Fatalf("fixture still has %d attention items", n)
	}
	r := overviewLayout(100, 30, v.model, v.st)
	if !r.allClear || r.panels[panelAttention].shown {
		t.Fatalf("all-clear layout has a strip panel: %+v", r)
	}
	checkGolden(t, "overview_allclear_100x30.golden", v.View(100, 30), 100, 30)
}
