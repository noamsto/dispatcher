package ui

import (
	"os"
	"path/filepath"
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

func TestCrewHeaderUnscopedProbeFailed(t *testing.T) {
	asciiProfile(t)
	m := headerModel(t, data.Scope{Source: "none", Crews: 2, Err: "tmux display-message: no server"},
		data.RosterCrew{ID: "c1"}, data.RosterCrew{ID: "c2"})
	if out := stripped(m.View()); !strings.Contains(out, "crew › myrepo · unscoped (2 crews) · scope probe failed\n") {
		t.Errorf("probe failure not flagged:\n%s", out)
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
		if got := RepoOfEvents(in); got != want {
			t.Errorf("RepoOfEvents(%q) = %q, want %q", in, got, want)
		}
	}
}

func labelledModel(t *testing.T, rows string) Model {
	t.Helper()
	log := filepath.Join(t.TempDir(), "events.jsonl")
	if err := os.WriteFile(log, []byte(rows), 0o644); err != nil {
		t.Fatal(err)
	}
	m := sized(loadedModel(loadSnapshot(t), Deps{Repo: data.RepoFromLog(log, "c2")}), 100, 30)
	m.active = overviewTab
	m.snap.Roster.Scope = data.Scope{Crew: "c2", Source: "flag", Known: true, Crews: 1}
	m.snap.Roster.Crews = []data.RosterCrew{{ID: "c2", Lane: &data.LaneStatus{State: "alive"}}}
	return m
}

func TestCrewHeaderRepoFromDeps(t *testing.T) {
	asciiProfile(t)
	m := labelledModel(t, `{"kind":"dispatch","crew_id":"c2","repo":"/x/myrepo/.git"}`+"\n")
	if out := stripped(m.View()); !strings.Contains(out, "crew › myrepo c2 · live") {
		t.Errorf("header lacks repo from Deps.Repo:\n%s", out)
	}
}

func TestCrewHeaderNoRepoWithoutDispatchRows(t *testing.T) {
	asciiProfile(t)
	m := labelledModel(t, "")
	out := stripped(m.View())
	if !strings.Contains(out, "crew › c2 · live") {
		t.Errorf("header should name the crew without a repo:\n%s", out)
	}
}

func TestCrewHeaderRepoAppearsAfterLateDispatch(t *testing.T) {
	asciiProfile(t)
	m := labelledModel(t, "")
	if out := stripped(m.View()); strings.Contains(out, "myrepo") {
		t.Fatalf("label before any dispatch row:\n%s", out)
	}
	nm, _ := m.Update(sourceMsg{src: srcBus, apply: func(*data.Snapshot) {}, bus: &data.BusDigest{Repo: "myrepo"}})
	if out := stripped(nm.(Model).View()); !strings.Contains(out, "crew › myrepo c2 · live") {
		t.Errorf("header after dispatch row:\n%s", out)
	}
}
