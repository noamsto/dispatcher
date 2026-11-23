package view_test

import (
	"fmt"
	"slices"
	"testing"

	"github.com/noamsto/dispatcher/dash/internal/data"
	"github.com/noamsto/dispatcher/dash/internal/view"
	"github.com/noamsto/dispatcher/dash/internal/view/viewtest"
)

func recentEv(tsSec int64, crew, detail string) data.RecentEvent {
	return data.RecentEvent{TS: tsSec * 1000, From: "dispatcher:" + crew, Kind: "status", State: "working", Detail: detail, Crew: crew}
}

func eventDetails(rows []view.EventRow) []string {
	var out []string
	for _, e := range rows {
		out = append(out, e.Crew+"/"+e.Text)
	}
	return out
}

// twoCrewInputs is the overview fixture with a digest holding events of crews c1 and c2:
// c1 has one inside the hour and one older, c2 one inside.
func twoCrewInputs() view.Inputs {
	in := viewtest.Inputs()
	now := in.NowMS / 1000
	c1 := []data.RecentEvent{recentEv(now-7200, "c1", "old"), recentEv(now-60, "c1", "new")}
	c2 := []data.RecentEvent{recentEv(now-120, "c2", "mid")}
	in.Bus.Recent = []data.RecentEvent{c1[0], c2[0], c1[1]}
	in.Bus.RecentByCrew = map[string][]data.RecentEvent{"c1": c1, "c2": c2}
	return in
}

func TestOverviewScopedEvents(t *testing.T) {
	in := twoCrewInputs()
	in.Snap.Roster.Scope = data.Scope{Crew: "c1", Source: "flag", Known: true, Crews: 1}
	v := view.BuildOverview(in)
	if got, want := eventDetails(v.Events), []string{"c1/working new", "c1/working old"}; !slices.Equal(got, want) {
		t.Errorf("scoped events = %v, want %v", got, want)
	}
	if s := v.Scope; s.Crew == nil || *s.Crew != "c1" || s.Source != "flag" || !s.Known || s.Crews != 1 {
		t.Errorf("scope = %+v", s)
	}
}

func TestOverviewUnscopedEvents(t *testing.T) {
	v := view.BuildOverview(twoCrewInputs())
	if got, want := eventDetails(v.Events), []string{"c1/working new", "c2/working mid"}; !slices.Equal(got, want) {
		t.Errorf("unscoped events = %v, want %v", got, want)
	}
	if s := v.Scope; s.Crew != nil || s.Source != "none" {
		t.Errorf("scope = %+v", s)
	}
}

func TestBuildFleetEvents(t *testing.T) {
	v := view.BuildFleet(viewtest.FleetInputs())
	if len(v.Events) == 0 {
		t.Fatal("no fleet events")
	}
	seen := map[string]bool{}
	for i, e := range v.Events {
		if e.Repo == "" || e.Crew == "" {
			t.Errorf("event %d lacks repo/crew: %+v", i, e)
		}
		if i > 0 && e.TS > v.Events[i-1].TS {
			t.Errorf("event %d newer than %d", i, i-1)
		}
		seen[e.Repo+"/"+e.Crew] = true
	}
	if !seen["a/c1"] {
		t.Errorf("events cover %v, want a/c1", seen)
	}
	if len(v.Crews) == 0 {
		t.Error("crews missing")
	}
}

func TestBuildFleetEventsCapAndOrder(t *testing.T) {
	in := viewtest.FleetInputs()
	now := in.NowMS / 1000
	d := in.Digests["/r/a/.git"]
	var ring []data.RecentEvent
	for i := 0; i < 40; i++ {
		ring = append(ring, recentEv(now-3000+int64(i), "c1", fmt.Sprintf("e%d", i)))
	}
	ring = slices.Insert(ring, 0, recentEv(now-7200, "c1", "ancient"))
	d.RecentByCrew = map[string][]data.RecentEvent{"c1": ring}
	in.Digests["/r/a/.git"] = d
	delete(in.Digests, "/r/b/.git")
	v := view.BuildFleet(in)
	if len(v.Events) != 30 {
		t.Fatalf("events = %d, want 30", len(v.Events))
	}
	if v.Events[0].Text != "working e39" || v.Events[29].Text != "working e10" {
		t.Errorf("order/cap: first %q last %q", v.Events[0].Text, v.Events[29].Text)
	}
}

func TestCrewInputsOverviewEvents(t *testing.T) {
	in := viewtest.FleetInputs()
	now := in.NowMS / 1000
	d := in.Digests["/r/a/.git"]
	d.RecentByCrew = map[string][]data.RecentEvent{
		"c1": {recentEv(now-7200, "c1", "old"), recentEv(now-30, "c1", "new")},
		"c3": {recentEv(now-30, "c3", "other")},
	}
	in.Digests["/r/a/.git"] = d
	ci, ok := view.CrewInputs(in, keyA1)
	if !ok {
		t.Fatal("no crew")
	}
	v := view.BuildOverview(ci)
	if got, want := eventDetails(v.Events), []string{"c1/working new", "c1/working old"}; !slices.Equal(got, want) {
		t.Errorf("events = %v, want %v", got, want)
	}
	if s := v.Scope; s.Crew == nil || *s.Crew != "c1" || s.Source != "fleet" || !s.Known || s.Crews != 1 {
		t.Errorf("scope = %+v", s)
	}
}
