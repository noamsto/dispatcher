package crews

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/bus"
)

// layout2Fixture is fixture with a state root, holding one layout-2 crew of
// each kind: homed here (live and dead), visiting, homed in another repo and
// registered outside any repo. c-legacy is a layout-1 crew of this repo.
func layout2Fixture(t *testing.T) bus.Paths {
	t.Helper()
	p := fixture(t).WithState(t.TempDir())
	here, err := bus.RepoTag(p.Common)
	if err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	for _, m := range []bus.Meta{
		{ID: "c-live", HomeRepo: &here},
		{ID: "c-dead", HomeRepo: &here},
		{ID: "c-visit", HomeRepo: &here, Visiting: true},
		{ID: "c-visit-null", Visiting: true},
		{ID: "c-other", HomeRepo: &other},
		{ID: "c-null"},
	} {
		m.HomeHost, m.CreatedBy = "h", "register"
		if err := bus.CreateMeta(p.State, m); err != nil {
			t.Fatal(err)
		}
	}
	for id, pid := range map[string]string{"c-live": "4321\n", "c-dead": "8888\n", "c-null": "4321\n", "c-other": "4321\n"} {
		if err := os.WriteFile(bus.StoreDir(p.State, id)+"/pid", []byte(pid), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	crewDir(t, p, "c-legacy", "")
	return p
}

func TestCrewsTableListsLayout2CrewsHomedHere(t *testing.T) {
	p := layout2Fixture(t)
	out, errB, code := run(t, p, opts(map[int]bool{4321: true}, nil))
	if code != 0 || errB != "" {
		t.Fatalf("code %d, stderr %q", code, errB)
	}
	want := strings.Join([]string{
		header,
		"c-dead\t—\t—\t0\t8888\tno",
		"c-legacy\t—\t—\t0\t—\t—",
		"c-live\t—\t—\t0\t4321\tyes",
	}, "\n") + "\n"
	if out != want {
		t.Errorf("got:\n%s\nwant:\n%s", out, want)
	}
}

func TestCrewsTableOutsideARepoListsNullHomeCrews(t *testing.T) {
	p := bus.StatePaths(layout2Fixture(t).State)
	out, errB, code := run(t, p, opts(map[int]bool{4321: true}, nil))
	if code != 0 || errB != "" {
		t.Fatalf("code %d, stderr %q", code, errB)
	}
	if want := header + "\nc-null\t—\t—\t0\t4321\tyes\n"; out != want {
		t.Errorf("got:\n%s\nwant:\n%s", out, want)
	}
}

func TestCrewsHomeKeepsLayout2CrewsHomedHere(t *testing.T) {
	p := layout2Fixture(t)
	// c-legacy has no home record, no log line and no pid: foreign.
	if got := homeOnly(t, p, newProcs(nil)); !slices.Equal(got, []string{"c-dead", "c-live"}) {
		t.Errorf("home crews = %v, want [c-dead c-live]", got)
	}
	if got := homeOnly(t, bus.StatePaths(p.State), newProcs(nil)); !slices.Equal(got, []string{"c-null"}) {
		t.Errorf("home crews outside a repo = %v, want [c-null]", got)
	}
}

func TestCrewsMineIncludesLayout2CrewsHomedHere(t *testing.T) {
	p := layout2Fixture(t)
	parents := map[int]int{os.Getpid(): 4321}
	out, _, code := runMine(t, p, opts(map[int]bool{4321: true}, parents))
	if code != 0 || out != "c-live\n" {
		t.Errorf("mine: code %d out %q, want c-live only (c-other shares the pid but is homed elsewhere)", code, out)
	}
	out, _, code = runMine(t, bus.StatePaths(p.State), opts(map[int]bool{4321: true}, parents))
	if code != 0 || out != "c-null\n" {
		t.Errorf("mine outside a repo: code %d out %q, want c-null", code, out)
	}
}

func TestWriteHomeWritesNothingAtLayout2(t *testing.T) {
	p := layout2Fixture(t)
	bc, err := p.Crew("c-live")
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteHome(p, bc); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p.Dir + "/homes"); !os.IsNotExist(err) {
		t.Errorf("homes/ exists after a layout-2 WriteHome: %v", err)
	}
	legacy, err := p.Crew("c-legacy")
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteHome(p, legacy); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(HomePath(p, "c-legacy")); string(data) != p.Common+"\n" {
		t.Errorf("layout-1 home record = %q", data)
	}
}

func TestPidfileLogWritesUnderTheStateRoot(t *testing.T) {
	p := layout2Fixture(t)
	bc, err := p.Crew("c-live")
	if err != nil {
		t.Fatal(err)
	}
	PidfileLog(bc.State, Caller{Now: fakeNow(), PID: 1, PPID: 2, Pwd: "/x", By: "b"}, "register", "ok", "c-live", "", "9")
	data, err := os.ReadFile(bc.Dir + "/pidfile.log")
	if err != nil || !strings.Contains(string(data), " register ok crew=c-live ") {
		t.Errorf("pidfile.log = %q, %v", data, err)
	}
	if _, err := os.Stat(p.Dir + "/pidfile.log"); !os.IsNotExist(err) {
		t.Errorf("repo pidfile.log written for a layout-2 crew: %v", err)
	}
}

// A layout-2 crew's rows are on its own log, so its stats come from there;
// foreign crew_ids on that log stay out.
func TestCrewsTableStatsALayout2CrewFromItsOwnLog(t *testing.T) {
	p := layout2Fixture(t)
	body := strings.Join([]string{
		status(1785951000000, "c-live", "worker:feat/x#s1"),
		status(1785950000000, "c-live", "worker:feat/y#s2"),
		status(1785950500000, "c-legacy", "worker:feat/z#s3"),
	}, "\n") + "\n"
	if err := os.WriteFile(bus.StoreDir(p.State, "c-live")+"/events.jsonl", []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	out, errB, code := run(t, p, opts(map[int]bool{4321: true}, nil))
	if code != 0 || errB != "" {
		t.Fatalf("code %d, stderr %q", code, errB)
	}
	want := strings.Join([]string{
		header,
		"c-live\t264\t1264\t2\t4321\tyes",
		"c-dead\t—\t—\t0\t8888\tno",
		"c-legacy\t—\t—\t0\t—\t—",
	}, "\n") + "\n"
	if out != want {
		t.Errorf("got:\n%s\nwant:\n%s", out, want)
	}
}

// TestCrewsLegacyListsLiveLayout1Crews is the phase 6 gate: --legacy names the
// layout-1 crews of this repo whose recorded pid is still live. The ended
// layout-1 crew, the pid-less fixture crew and every layout-2 crew are omitted.
func TestCrewsLegacyListsLiveLayout1Crews(t *testing.T) {
	p := layout2Fixture(t)
	crewDir(t, p, "c-l1-live", "4321\n")
	crewDir(t, p, "c-l1-dead", "8888\n")
	out, errB, code := runLegacy(t, p, opts(map[int]bool{4321: true}, nil))
	if code != 0 || errB != "" {
		t.Fatalf("code %d, stderr %q", code, errB)
	}
	if want := "c-l1-live\n"; out != want {
		t.Errorf("legacy: got %q, want %q", out, want)
	}
}

// TestCrewsLegacyOmitsALayout2CrewWithALegacyDir: a repo dir beside a
// meta.json is still layout 2, so --legacy must not report it.
func TestCrewsLegacyOmitsALayout2CrewWithALegacyDir(t *testing.T) {
	p := layout2Fixture(t)
	crewDir(t, p, "c-live", "4321\n")
	out, errB, code := runLegacy(t, p, opts(map[int]bool{4321: true}, nil))
	if code != 0 || errB != "" {
		t.Fatalf("code %d, stderr %q", code, errB)
	}
	if out != "" {
		t.Errorf("legacy printed a layout-2 crew: %q", out)
	}
}

// TestCrewsLegacyOutsideARepoIsSilent: no repo means no layout-1 dirs to glob.
func TestCrewsLegacyOutsideARepoIsSilent(t *testing.T) {
	p := bus.StatePaths(layout2Fixture(t).State)
	out, errB, code := runLegacy(t, p, opts(map[int]bool{4321: true}, nil))
	if code != 0 || errB != "" || out != "" {
		t.Errorf("legacy outside a repo: code %d, stdout %q, stderr %q", code, out, errB)
	}
}
