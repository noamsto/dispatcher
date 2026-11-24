package register

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/crews"
)

// statePaths is testPaths with a state root, so layout-2 crews resolve.
func statePaths(t *testing.T) bus.Paths {
	t.Helper()
	return testPaths(t).WithState(t.TempDir())
}

func layout2World(id string) *world {
	w := newWorld(id)
	w.env["CREW_LAYOUT"] = "2"
	return w
}

func metaOf(t *testing.T, state, id string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(readFile(t, bus.StoreDir(state, id)+"/meta.json")), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func absent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Errorf("%s exists (%v)", path, err)
	}
}

func TestRegisterLayout2InARepo(t *testing.T) {
	p := statePaths(t)
	w := layout2World("c1")
	w.env["TMUX_PANE"] = "%3"
	if code, stderr := run("register", "888", p, w); code != 0 || stderr != "" {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	dir := bus.StoreDir(p.State, "c1")
	tag, err := bus.RepoTag(p.Common)
	if err != nil {
		t.Fatal(err)
	}
	m := metaOf(t, p.State, "c1")
	want := map[string]any{
		"layout": float64(2), "id": "c1", "created": float64(1791594123000), "home_host": "box",
		"home_repo": tag, "home_slug": "acme/widgets", "visiting": false, "created_by": "register",
	}
	if len(m) != len(want) {
		t.Errorf("meta = %v, want %v", m, want)
	}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("meta[%s] = %v, want %v", k, m[k], v)
		}
	}
	if got := readFile(t, dir+"/pid"); got != "888\n" {
		t.Errorf("pid = %q", got)
	}
	if got := readFile(t, dir+"/pane"); got != "%3\n" {
		t.Errorf("pane = %q", got)
	}
	if got := readFile(t, dir+"/pidfile.log"); !strings.Contains(got, " register ok crew=c1 old=- new=888 ") {
		t.Errorf("pidfile.log = %q", got)
	}
	// Nothing under the repo bus: no crew dir, no home record, no pidfile.log.
	absent(t, p.Dir)
}

func TestRegisterLayout2OutsideARepo(t *testing.T) {
	p := bus.StatePaths(t.TempDir())
	if code, stderr := run("register", "888", p, layout2World("c1")); code != 0 || stderr != "" {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	m := metaOf(t, p.State, "c1")
	if v, ok := m["home_repo"]; !ok || v != nil {
		t.Errorf("home_repo = %v (present %v), want null", v, ok)
	}
	if _, ok := m["home_slug"]; ok {
		t.Errorf("home_slug written outside a repo: %v", m)
	}
	if got := readFile(t, bus.StoreDir(p.State, "c1")+"/pid"); got != "888\n" {
		t.Errorf("pid = %q", got)
	}
}

func TestRegisterLayout1OutsideARepoIsNotARepo(t *testing.T) {
	p := bus.StatePaths(t.TempDir())
	code, stderr := run("register", "888", p, newWorld("c1"))
	if code != 1 || stderr != "crew: not in a git repo\n" {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	absent(t, p.State+"/crews")
}

// An existing meta.json is reused whatever CREW_LAYOUT says, and never
// rewritten.
func TestRegisterLayout2PinIsSticky(t *testing.T) {
	p := statePaths(t)
	if code, stderr := run("register", "888", p, layout2World("c1")); code != 0 {
		t.Fatalf("first register: code=%d stderr=%q", code, stderr)
	}
	before := readFile(t, bus.StoreDir(p.State, "c1")+"/meta.json")
	for _, layout := range []string{"", "1", "2"} {
		w := newWorld("c1")
		w.env["CREW_LAYOUT"] = layout
		if code, stderr := run("register", "999", p, w); code != 0 || stderr != "" {
			t.Fatalf("CREW_LAYOUT=%q: code=%d stderr=%q", layout, code, stderr)
		}
		if got := readFile(t, bus.StoreDir(p.State, "c1")+"/pid"); got != "999\n" {
			t.Errorf("CREW_LAYOUT=%q: pid = %q", layout, got)
		}
		if got := readFile(t, bus.StoreDir(p.State, "c1")+"/meta.json"); got != before {
			t.Errorf("CREW_LAYOUT=%q rewrote meta.json: %q", layout, got)
		}
	}
	absent(t, p.Dir)
}

// A legacy crew dir in this repo keeps the crew layout 1, with no message.
func TestRegisterLayout2KeepsALegacyCrewLayout1(t *testing.T) {
	p := statePaths(t)
	seed(t, p, "c1", "777")
	if code, stderr := run("register", "888", p, layout2World("c1")); code != 0 || stderr != "" {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	if got := readFile(t, p.CrewDir("c1")+"/pid"); got != "888\n" {
		t.Errorf("legacy pid = %q", got)
	}
	if got := readFile(t, crews.HomePath(p, "c1")); got != p.Common+"\n" {
		t.Errorf("legacy home = %q", got)
	}
	absent(t, p.State+"/crews")
}

func TestRegisterLayout2Refusals(t *testing.T) {
	t.Run("archived", func(t *testing.T) {
		p := statePaths(t)
		if err := os.MkdirAll(bus.ArchiveDir(p.State, "c1"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(bus.ArchiveDir(p.State, "c1")+"/meta.json", []byte(`{"layout":2,"id":"c1"}`), 0o644); err != nil {
			t.Fatal(err)
		}
		for _, layout := range []string{"", "2"} {
			w := newWorld("c1")
			w.env["CREW_LAYOUT"] = layout
			code, stderr := run("register", "888", p, w)
			if code != 1 || stderr != "crew: crew c1 is archived\n" {
				t.Errorf("CREW_LAYOUT=%q: code=%d stderr=%q", layout, code, stderr)
			}
		}
		absent(t, p.State+"/crews")
		absent(t, p.Dir)
	})
	t.Run("no state root", func(t *testing.T) {
		p := testPaths(t)
		code, stderr := run("register", "888", p, layout2World("c1"))
		if code != 1 || stderr != "crew: register: CREW_LAYOUT=2 needs XDG_STATE_HOME or HOME\n" {
			t.Errorf("code=%d stderr=%q", code, stderr)
		}
		absent(t, p.Dir)
	})
	t.Run("invalid value", func(t *testing.T) {
		for _, v := range []string{"3", "02", " 2", "two"} {
			p := statePaths(t)
			w := newWorld("c1")
			w.env["CREW_LAYOUT"] = v
			code, stderr := run("register", "888", p, w)
			if want := "crew: register: CREW_LAYOUT must be 1 or 2 (got '" + v + "')\n"; code != 1 || stderr != want {
				t.Errorf("%q: code=%d stderr=%q", v, code, stderr)
			}
			absent(t, p.Dir)
			absent(t, p.State+"/crews")
		}
	})
}

// deregister of a layout-2 crew drops the registration files only: the dir,
// meta.json, log and pidfile.log are the crew and stay.
func TestDeregisterLayout2KeepsTheCrew(t *testing.T) {
	p := statePaths(t)
	w := layout2World("c1")
	w.env["TMUX_PANE"] = "%3"
	w.start = map[int]string{888: "tok"}
	w.alive[888] = false
	if code, stderr := run("register", "888", p, w); code != 0 {
		t.Fatalf("register: code=%d stderr=%q", code, stderr)
	}
	dir := bus.StoreDir(p.State, "c1")
	if got := readFile(t, dir+"/pid.start"); got != "888 tok\n" {
		t.Fatalf("pid.start = %q", got)
	}
	for _, name := range []string{"launch", "relaunch", "events.jsonl", "keep"} {
		if err := os.WriteFile(dir+"/"+name, []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if code, stderr := run("deregister", "", p, newWorld("c1")); code != 0 || stderr != "" {
		t.Fatalf("deregister: code=%d stderr=%q", code, stderr)
	}
	for _, name := range []string{"pid", "pid.start", "pane", "launch", "relaunch"} {
		absent(t, dir+"/"+name)
	}
	for _, name := range []string{"meta.json", "events.jsonl", "keep"} {
		readFile(t, dir+"/"+name)
	}
	if got := readFile(t, dir+"/pidfile.log"); !strings.Contains(got, " deregister removed crew=c1 old=888 ") {
		t.Errorf("pidfile.log = %q", got)
	}
	// A second deregister finds nothing to remove and still succeeds.
	if code, stderr := run("deregister", "", p, newWorld("c1")); code != 0 || stderr != "" {
		t.Fatalf("second deregister: code=%d stderr=%q", code, stderr)
	}
}

// Layout-1 deregister removes the crew dir, so a running legacy crew between
// register cycles is known only by its home record and repo-log rows: either
// keeps it layout 1.
func TestRegisterLayout2KeepsADeregisteredLegacyCrewLayout1(t *testing.T) {
	for _, keepHome := range []bool{true, false} {
		p := statePaths(t)
		if code, stderr := run("register", "888", p, newWorld("c1")); code != 0 {
			t.Fatalf("register: code=%d stderr=%q", code, stderr)
		}
		if err := bus.Append(p.Log, `{"ts":1,"crew_id":"c1","event":"status","state":"working"}`); err != nil {
			t.Fatal(err)
		}
		if code, stderr := run("deregister", "", p, newWorld("c1")); code != 0 {
			t.Fatalf("deregister: code=%d stderr=%q", code, stderr)
		}
		if !keepHome {
			if err := os.Remove(crews.HomePath(p, "c1")); err != nil {
				t.Fatal(err)
			}
		}
		if code, stderr := run("register", "999", p, layout2World("c1")); code != 0 || stderr != "" {
			t.Fatalf("home=%v: code=%d stderr=%q", keepHome, code, stderr)
		}
		absent(t, bus.StoreDir(p.State, "c1"))
		if got := readFile(t, p.CrewDir("c1")+"/pid"); got != "999\n" {
			t.Errorf("home=%v: legacy pid = %q", keepHome, got)
		}
		bc, err := p.Crew("c1")
		if err != nil || bc.Layout == 2 || bc.Log != p.Log {
			t.Errorf("home=%v: crew = %+v, %v; want the repo log", keepHome, bc, err)
		}
	}
}
