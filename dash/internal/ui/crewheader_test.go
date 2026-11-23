package ui

import (
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/dash/internal/data"
)

func headerModel(t *testing.T, sc data.Scope, crews ...data.RosterCrew) Model {
	t.Helper()
	m := sized(newTestModel(t), 100, 30)
	m.repo = "myrepo"
	m.active = overviewTab
	m.snap.Roster.Scope = sc
	m.snap.Roster.Crews = crews
	return m
}

func TestCrewHeaderScopedKnown(t *testing.T) {
	asciiProfile(t)
	m := headerModel(t, data.Scope{Crew: "c1", Source: "flag", Known: true, Crews: 1},
		data.RosterCrew{ID: "c1", Lane: &data.LaneStatus{State: "alive"}})
	for _, tab := range []int{overviewTab, settingsTab} {
		m.active = tab
		if out := stripped(m.View()); !strings.Contains(out, "crew › myrepo c1 · live") {
			t.Errorf("%s tab lacks the crew header:\n%s", tabTitles[tab], out)
		}
	}
	m.active = fleetTab
	if out := stripped(m.View()); strings.Contains(out, "crew ›") {
		t.Errorf("Fleet tab shows the crew header:\n%s", out)
	}
}

func TestCrewHeaderScopedStateIsTheScopedCrews(t *testing.T) {
	asciiProfile(t)
	here := &data.DispatcherLoc{Where: "here"}
	m := headerModel(t, data.Scope{Crew: "c2", Source: "env", Known: true, Crews: 2},
		data.RosterCrew{ID: "c1", Lane: &data.LaneStatus{State: "alive"}, Dispatcher: here},
		data.RosterCrew{ID: "c2", Lane: &data.LaneStatus{State: "dead"}, Dispatcher: here})
	if out := stripped(m.View()); !strings.Contains(out, "crew › myrepo c2 · dead") {
		t.Errorf("scoped crew state missing:\n%s", out)
	}
	m = headerModel(t, data.Scope{Crew: "f1", Source: "env", Known: true, Crews: 1},
		data.RosterCrew{ID: "f1", Lane: &data.LaneStatus{State: "alive"}, Dispatcher: &data.DispatcherLoc{Where: "elsewhere"}})
	if out := stripped(m.View()); !strings.Contains(out, "crew › myrepo f1\n") {
		t.Errorf("foreign crew carries a lane state:\n%s", out)
	}
}

func TestCrewHeaderScopedUnknown(t *testing.T) {
	asciiProfile(t)
	m := headerModel(t, data.Scope{Crew: "zz", Source: "flag", Known: false, Crews: 1},
		data.RosterCrew{ID: "c1"})
	if out := stripped(m.View()); !strings.Contains(out, "crew › myrepo zz · unknown crew") {
		t.Errorf("unknown-crew header missing:\n%s", out)
	}
}

func TestCrewHeaderUnscoped(t *testing.T) {
	asciiProfile(t)
	m := headerModel(t, data.Scope{Source: "none", Crews: 2},
		data.RosterCrew{ID: "c1", Lane: &data.LaneStatus{State: "alive"}}, data.RosterCrew{ID: "c2"})
	out := stripped(m.View())
	if !strings.Contains(out, "crew › myrepo · unscoped (2 crews)\n") {
		t.Errorf("unscoped header missing:\n%s", out)
	}
	if strings.Contains(out, "c1, c2") {
		t.Errorf("unscoped header joins crew ids:\n%s", out)
	}
	m = headerModel(t, data.Scope{Source: "none", Crews: 1}, data.RosterCrew{ID: "c1"})
	if out := stripped(m.View()); !strings.Contains(out, "unscoped (1 crew)") {
		t.Errorf("singular header missing:\n%s", out)
	}
}

func TestCrewHeaderEmpty(t *testing.T) {
	asciiProfile(t)
	m := headerModel(t, data.Scope{Source: "none"})
	if out := stripped(m.View()); strings.Contains(out, "crew ›") {
		t.Errorf("header shown without a crew:\n%s", out)
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
