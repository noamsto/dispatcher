package view_test

import (
	"testing"

	"github.com/noamsto/dispatcher/dash/internal/view"
	"github.com/noamsto/dispatcher/dash/internal/view/viewtest"
)

func TestDispatcherSession(t *testing.T) {
	for _, tc := range []struct {
		name       string
		state      string
		pane       *string
		unfinished int
		want       string
	}{
		{"waiting", "live", strp("waiting"), 2, view.SessionNeedsYou},
		{"waiting without workers", "live", strp("waiting"), 0, view.SessionNeedsYou},
		{"idle with workers", "live", strp("idle"), 1, view.SessionStandby},
		{"done with workers", "live", strp("done"), 1, view.SessionStandby},
		{"idle without workers", "live", strp("idle"), 0, view.SessionIdle},
		{"done without workers", "live", strp("done"), 0, view.SessionIdle},
		{"processing", "live", strp("processing"), 1, view.SessionWorking},
		{"compacting", "live", strp("compacting"), 0, view.SessionWorking},
		{"no pane state", "live", nil, 1, view.SessionUnknown},
		{"dead dispatcher", "dead", strp("waiting"), 1, view.SessionUnknown},
		{"closed dispatcher", "closed", strp("idle"), 1, view.SessionUnknown},
		{"denied", "live", strp("denied"), 1, view.SessionUnknown},
		{"error", "live", strp("error"), 1, view.SessionUnknown},
		{"interrupted", "live", strp("interrupted"), 1, view.SessionUnknown},
		{"unrecognised", "live", strp("sulking"), 1, view.SessionUnknown},
	} {
		if got := view.DispatcherSession(tc.state, tc.pane, tc.unfinished); got != tc.want {
			t.Errorf("%s: session = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func waitingItems(v view.FleetView) []view.AttentionItem {
	var out []view.AttentionItem
	for _, a := range v.Attention {
		if a.Kind == view.KindDispatcherWaiting {
			out = append(out, a)
		}
	}
	return out
}

func TestFleetDispatcherWaitingItem(t *testing.T) {
	in := viewtest.FleetInputs()
	crewOf(in, "/r/a/.git", "c1").Dispatcher.PaneState = strp("waiting")
	v := view.BuildFleet(in)
	items := waitingItems(v)
	if len(items) != 1 {
		t.Fatalf("crew c1 spans two repos, want one dispatcher_waiting item: %+v", items)
	}
	it := items[0]
	if it.Severity != view.SeverityWarning || it.Action != nil || it.Crew != "c1" ||
		it.Message != "dispatcher waiting on you" || it.Clears != "answer the dispatcher in its pane" ||
		it.CrewKey == nil || *it.CrewKey != keyA1 {
		t.Errorf("dispatcher_waiting = %+v", it)
	}
	for _, key := range []string{keyA1, keyB1} {
		if s := fleetCrew(t, v, key).Dispatcher.Session; s != view.SessionNeedsYou {
			t.Errorf("%s session = %q, want needs_you", key, s)
		}
	}
}

func TestFleetDispatcherNoWaitingItem(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state string
		pane  *string
		want  string
	}{
		{"standby", "live", strp("idle"), view.SessionStandby},
		{"working", "live", strp("processing"), view.SessionWorking},
		{"unknown", "live", nil, view.SessionUnknown},
		{"denied", "live", strp("denied"), view.SessionUnknown},
		{"dead", "dead", strp("waiting"), view.SessionUnknown},
	} {
		in := viewtest.FleetInputs()
		c := crewOf(in, "/r/a/.git", "c1")
		c.Dispatcher.State, c.Dispatcher.PaneState = tc.state, tc.pane
		v := view.BuildFleet(in)
		if items := waitingItems(v); len(items) != 0 {
			t.Errorf("%s: dispatcher_waiting raised: %+v", tc.name, items)
		}
		if s := fleetCrew(t, v, keyA1).Dispatcher.Session; s != tc.want {
			t.Errorf("%s: session = %q, want %q", tc.name, s, tc.want)
		}
	}
}

func TestFleetDispatcherIdleNoItem(t *testing.T) {
	in := viewtest.FleetInputs()
	a := crewOf(in, "/r/a/.git", "c1")
	a.Dispatcher.PaneState = strp("idle")
	a.Workers = nil
	crewOf(in, "/r/b/.git", "c1").Workers = nil
	v := view.BuildFleet(in)
	if items := waitingItems(v); len(items) != 0 {
		t.Errorf("idle dispatcher raised %+v", items)
	}
	if s := fleetCrew(t, v, keyA1).Dispatcher.Session; s != view.SessionIdle {
		t.Errorf("session = %q, want idle", s)
	}
}

func TestFleetStandbyCountsWorkersInOtherRepos(t *testing.T) {
	in := viewtest.FleetInputs()
	a := crewOf(in, "/r/a/.git", "c1")
	a.Dispatcher.PaneState = strp("idle")
	a.Workers = nil
	v := view.BuildFleet(in)
	if s := fleetCrew(t, v, keyA1).Dispatcher.Session; s != view.SessionStandby {
		t.Errorf("home session = %q, want standby: repo b still has feat/b2 open", s)
	}
}
