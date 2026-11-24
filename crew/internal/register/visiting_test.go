package register

import (
	"bytes"
	"os"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
)

func runVisiting(origin string, p bus.Paths, w *world) (int, string) {
	var errb bytes.Buffer
	code := RunVisiting(origin, p, &errb, w.options())
	return code, errb.String()
}

func TestRunVisitingCreatesTheMeta(t *testing.T) {
	p := statePaths(t)
	if code, stderr := runVisiting("halo", p, newWorld("c1")); code != 0 || stderr != "" {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	m := metaOf(t, p.State, "c1")
	want := map[string]any{
		"layout": float64(2), "id": "c1", "created": float64(1791594123000), "home_host": "halo",
		"home_repo": nil, "visiting": true, "created_by": "remote-dispatch",
	}
	if len(m) != len(want) {
		t.Errorf("meta = %v, want %v", m, want)
	}
	for k, v := range want {
		if got, ok := m[k]; !ok || got != v {
			t.Errorf("meta[%s] = %v (present %v), want %v", k, got, ok, v)
		}
	}
	entries, err := os.ReadDir(bus.StoreDir(p.State, "c1"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "meta.json" {
		t.Errorf("store holds %v, want only meta.json", entries)
	}
	absent(t, p.Dir)
}

func TestRunVisitingRerunIsANoOp(t *testing.T) {
	p := statePaths(t)
	if code, _ := runVisiting("halo", p, newWorld("c1")); code != 0 {
		t.Fatal("first run failed")
	}
	path := bus.StoreDir(p.State, "c1") + "/meta.json"
	before := readFile(t, path)
	w := newWorld("c1")
	o := w.options()
	o.Clock.Now = func() time.Time { return time.Unix(1, 0) }
	var errb bytes.Buffer
	if code := RunVisiting("halo", p, &errb, o); code != 0 || errb.Len() != 0 {
		t.Fatalf("code=%d stderr=%q", code, errb.String())
	}
	if got := readFile(t, path); got != before {
		t.Errorf("meta rewritten: %q", got)
	}
}

func TestRunVisitingRefusals(t *testing.T) {
	const archived = "crew: crew c1 is archived\n"
	cases := []struct {
		name   string
		setup  func(t *testing.T, p bus.Paths)
		origin string
		want   string
	}{
		{"own crew", func(t *testing.T, p bus.Paths) {
			mustMeta(t, p, bus.Meta{ID: "c1", HomeHost: "halo"})
		}, "halo", "crew: register: crew c1 is this host's own crew (home_host halo), not a visiting one\n"},
		{"other origin", func(t *testing.T, p bus.Paths) {
			mustMeta(t, p, bus.Meta{ID: "c1", HomeHost: "other", Visiting: true})
		}, "halo", "crew: register: crew c1 on this host is visiting from other, not halo\n"},
		{"archived", func(t *testing.T, p bus.Paths) {
			if err := os.MkdirAll(bus.ArchiveDir(p.State, "c1"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(bus.ArchiveDir(p.State, "c1")+"/meta.json", []byte(`{"layout":2,"id":"c1"}`), 0o644); err != nil {
				t.Fatal(err)
			}
		}, "halo", archived},
		{"index only", func(t *testing.T, p bus.Paths) {
			if _, err := bus.IndexAdd(p.State, "repo-tag", "", "c1", time.Unix(1, 0)); err != nil {
				t.Fatal(err)
			}
		}, "halo", "crew: " + (&bus.IndexOnlyError{ID: "c1"}).Error() + "\n"},
		{"legacy crew dir", func(t *testing.T, p bus.Paths) { seed(t, p, "c1", "777") },
			"halo", "crew: register: crew c1 already runs at layout 1 in this clone — finish its workers before dispatching it here at layout 2\n"},
		{"legacy log row", func(t *testing.T, p bus.Paths) {
			if err := os.MkdirAll(p.Dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := bus.Append(p.Log, `{"ts":1,"crew_id":"c1","event":"status","state":"working"}`); err != nil {
				t.Fatal(err)
			}
		}, "halo", "crew: register: crew c1 already runs at layout 1 in this clone — finish its workers before dispatching it here at layout 2\n"},
		{"dotted origin", nil, "a.b", "crew: register: invalid origin host \"a.b\"\n"},
		{"dash origin", nil, "-x", "crew: register: invalid origin host \"-x\"\n"},
		{"empty origin", nil, "", "crew: register: invalid origin host \"\"\n"},
		{"newline origin", nil, "halo\n", "crew: register: invalid origin host \"halo\\n\"\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := statePaths(t)
			if tc.setup != nil {
				tc.setup(t, p)
			}
			code, stderr := runVisiting(tc.origin, p, newWorld("c1"))
			if code != 1 || stderr != tc.want {
				t.Errorf("code=%d stderr=%q, want %q", code, stderr, tc.want)
			}
			if tc.name != "own crew" && tc.name != "other origin" {
				absent(t, bus.StoreDir(p.State, "c1")+"/meta.json")
			}
		})
	}
}

func TestRunVisitingNoStateRoot(t *testing.T) {
	p := testPaths(t)
	code, stderr := runVisiting("halo", p, newWorld("c1"))
	if code != 1 || stderr != "crew: register: needs XDG_STATE_HOME or HOME\n" {
		t.Errorf("code=%d stderr=%q", code, stderr)
	}
}

func TestRunVisitingCrewID(t *testing.T) {
	p := statePaths(t)
	if code, stderr := runVisiting("halo", p, newWorld("")); code != 1 || stderr == "" {
		t.Errorf("unset: code=%d stderr=%q", code, stderr)
	}
	code, stderr := runVisiting("halo", p, newWorld("../x"))
	if code != 1 || stderr != "crew: invalid crew id — expected only letters, digits, '.', '_' and '-'\n" {
		t.Errorf("invalid: code=%d stderr=%q", code, stderr)
	}
}

func TestRunVisitingOutsideARepo(t *testing.T) {
	p := bus.StatePaths(t.TempDir())
	if code, stderr := runVisiting("halo", p, newWorld("c1")); code != 0 || stderr != "" {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	if m := metaOf(t, p.State, "c1"); m["visiting"] != true {
		t.Errorf("meta = %v", m)
	}
}

// A creator that lost the race re-reads the winner's meta and applies the
// existing-meta rule.
func TestCreateVisitingLostRace(t *testing.T) {
	o := newWorld("c1").options()
	t.Run("same origin", func(t *testing.T) {
		p := statePaths(t)
		mustMeta(t, p, bus.Meta{ID: "c1", HomeHost: "halo", Visiting: true})
		if err := createVisiting(p.State, "c1", "halo", o); err != nil {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("other origin", func(t *testing.T) {
		p := statePaths(t)
		mustMeta(t, p, bus.Meta{ID: "c1", HomeHost: "other", Visiting: true})
		err := createVisiting(p.State, "c1", "halo", o)
		if err == nil || err.Error() != "crew c1 on this host is visiting from other, not halo" {
			t.Errorf("err = %v", err)
		}
	})
}

func mustMeta(t *testing.T, p bus.Paths, m bus.Meta) {
	t.Helper()
	if err := bus.CreateMeta(p.State, m); err != nil {
		t.Fatal(err)
	}
}
