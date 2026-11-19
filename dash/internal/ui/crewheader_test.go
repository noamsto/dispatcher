package ui

import (
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/dash/internal/data"
)

func TestCrewHeaderOnSingleCrewTabs(t *testing.T) {
	asciiProfile(t)
	m := sized(newTestModel(t), 100, 30)
	m.repo = "myrepo"
	m.snap.Roster.Crews = []data.RosterCrew{{ID: "c1", Lane: &data.LaneStatus{State: "alive"}}}
	for _, tab := range []int{overviewTab, settingsTab} {
		m.active = tab
		out := stripped(m.View())
		if !strings.Contains(out, "crew › myrepo c1 · live") {
			t.Errorf("%s tab lacks the crew header:\n%s", tabTitles[tab], out)
		}
	}
	m.active = fleetTab
	if out := stripped(m.View()); strings.Contains(out, "crew ›") {
		t.Errorf("Fleet tab shows the crew header:\n%s", out)
	}
}

func TestCrewHeaderDeadLaneAndNoCrew(t *testing.T) {
	asciiProfile(t)
	m := sized(newTestModel(t), 100, 30)
	m.active = overviewTab
	m.snap.Roster.Crews = []data.RosterCrew{{ID: "c2", Lane: &data.LaneStatus{State: "dead"}}}
	if out := stripped(m.View()); !strings.Contains(out, "crew › c2 · dead") {
		t.Errorf("dead crew header missing:\n%s", out)
	}
	m.snap.Roster.Crews = nil
	if out := stripped(m.View()); strings.Contains(out, "crew ›") {
		t.Errorf("header shown without a crew:\n%s", out)
	}
}

func TestCrewHeaderNamesEveryCrew(t *testing.T) {
	asciiProfile(t)
	m := sized(newTestModel(t), 100, 30)
	m.active = overviewTab
	m.snap.Roster.Crews = []data.RosterCrew{
		{ID: "c1", Lane: &data.LaneStatus{State: "alive"}}, {ID: "c2"},
	}
	if out := stripped(m.View()); !strings.Contains(out, "crew › c1, c2\n") {
		t.Errorf("multi-crew header missing or carries a state:\n%s", out)
	}
}

func TestCrewHeaderStateIgnoresForeignCrews(t *testing.T) {
	asciiProfile(t)
	m := sized(newTestModel(t), 100, 30)
	m.active = overviewTab
	m.snap.Roster.Crews = []data.RosterCrew{
		{ID: "c1", Lane: &data.LaneStatus{State: "alive"}, Dispatcher: &data.DispatcherLoc{Where: "here"}},
		{ID: "f1", Dispatcher: &data.DispatcherLoc{Where: "elsewhere"}},
	}
	if out := stripped(m.View()); !strings.Contains(out, "crew › c1, f1 · live") {
		t.Errorf("home crew state missing:\n%s", out)
	}
}

func TestRepoOfEvents(t *testing.T) {
	for in, want := range map[string]string{
		"/h/dispatcher/.git/crew/events.jsonl": "dispatcher",
		"/h/dispatcher.git/crew/events.jsonl":  "dispatcher.git",
		"":                                     "",
	} {
		if got := repoOfEvents(in); got != want {
			t.Errorf("repoOfEvents(%q) = %q, want %q", in, got, want)
		}
	}
}
