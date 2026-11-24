package adopt

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
)

// refusedPaths is a bus with a state root holding an archived crew c-arch and
// an index-only crew c-gone (listed in a repo index, no crew dir).
func refusedPaths(t *testing.T) bus.Paths {
	t.Helper()
	p := testPaths(t).WithState(t.TempDir())
	if err := os.MkdirAll(bus.ArchiveDir(p.State, "c-arch"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bus.ArchiveDir(p.State, "c-arch")+"/meta.json", []byte(`{"layout":2,"id":"c-arch"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := bus.IndexAdd(p.State, "/elsewhere/.git", "", "c-gone", time.Unix(1700000000, 0)); err != nil {
		t.Fatal(err)
	}
	return p
}

func nothingCreated(t *testing.T, p bus.Paths) {
	t.Helper()
	for _, path := range []string{p.Dir, p.State + "/crews"} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Errorf("%s created (%v)", path, err)
		}
	}
}

func TestAdoptPropagatesLayout2Refusals(t *testing.T) {
	for id, want := range map[string]string{
		"c-arch": "crew: crew c-arch is archived\n",
		"c-gone": "crew: crew c-gone is listed in the repo index but has no crew dir — run 'crew index rebuild'; live crews: none\n",
	} {
		p := refusedPaths(t)
		for _, req := range []Request{{ID: id, PID: "123"}, {ID: id, PID: "123", Force: true}} {
			got := run(t, &stub{now: time.Unix(1700000000, 0)}, p, req)
			if got.code != 1 || got.stdout != "" || got.stderr != want {
				t.Errorf("%+v: code %d stdout %q stderr %q, want 1 and %q", req, got.code, got.stdout, got.stderr, want)
			}
		}
		got := run(t, reattachWorld(), p, reattachReq(id))
		if got.code != 1 || got.stderr != want {
			t.Errorf("reattach %s: code %d stderr %q, want 1 and %q", id, got.code, got.stderr, want)
		}
		nothingCreated(t, p)
	}
}

// adopt of a layout-2 crew records into the crew's own dir and pidfile.log and
// writes no home record.
func TestAdoptLayout2Crew(t *testing.T) {
	p := testPaths(t).WithState(t.TempDir())
	if err := bus.CreateMeta(p.State, bus.Meta{ID: "c2", HomeHost: "h", CreatedBy: "register", HomeRepo: repoTag(t, p)}); err != nil {
		t.Fatal(err)
	}
	got := run(t, &stub{now: time.Unix(1700000000, 0)}, p, Request{ID: "c2", PID: "123"})
	if got.code != 0 {
		t.Fatalf("code %d stderr %q", got.code, got.stderr)
	}
	dir := bus.StoreDir(p.State, "c2")
	if data, _ := os.ReadFile(dir + "/pid"); string(data) != "123\n" {
		t.Errorf("pid = %q", data)
	}
	if data, _ := os.ReadFile(dir + "/pidfile.log"); !strings.Contains(string(data), " adopt ok crew=c2 ") {
		t.Errorf("pidfile.log = %q", data)
	}
	if _, err := os.Lstat(p.Dir); !os.IsNotExist(err) {
		t.Errorf("repo bus touched for a layout-2 adopt (%v)", err)
	}
}

func layout2Meta(t *testing.T, p bus.Paths, m bus.Meta) {
	t.Helper()
	m.ID, m.HomeHost, m.CreatedBy = "c2", "h", "register"
	if err := bus.CreateMeta(p.State, m); err != nil {
		t.Fatal(err)
	}
}

func repoTag(t *testing.T, p bus.Paths) *string {
	t.Helper()
	tag, err := bus.RepoTag(p.Common)
	if err != nil {
		t.Fatal(err)
	}
	return &tag
}

func TestAdoptLayout2RefusesNotHome(t *testing.T) {
	elsewhere := "/elsewhere/.git"
	cases := []struct {
		name string
		meta func(t *testing.T, p bus.Paths) bus.Meta
		want func(p bus.Paths) string
	}{
		{
			name: "visiting",
			meta: func(t *testing.T, p bus.Paths) bus.Meta { return bus.Meta{Visiting: true, HomeRepo: repoTag(t, p)} },
			want: func(bus.Paths) string { return "crew c2 is visiting this host (home h) — adopt it there\n" },
		},
		{
			name: "homed elsewhere",
			meta: func(*testing.T, bus.Paths) bus.Meta { return bus.Meta{HomeRepo: &elsewhere} },
			want: func(bus.Paths) string { return "crew c2 is homed in /elsewhere/.git, not here\n" },
		},
		{
			name: "homed nowhere",
			meta: func(*testing.T, bus.Paths) bus.Meta { return bus.Meta{} },
			want: func(bus.Paths) string { return "crew c2 is homed in no repo, not here\n" },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := testPaths(t).WithState(t.TempDir())
			layout2Meta(t, p, tc.meta(t, p))
			want := "crew: " + tc.want(p)
			for _, req := range []Request{{ID: "c2", PID: "123"}, {ID: "c2", PID: "123", Force: true}} {
				got := run(t, &stub{now: time.Unix(1700000000, 0)}, p, req)
				if got.code != 1 || got.stdout != "" || got.stderr != want {
					t.Errorf("%+v: code %d stdout %q stderr %q, want 1 and %q", req, got.code, got.stdout, got.stderr, want)
				}
			}
			if _, err := os.Lstat(p.Dir); !os.IsNotExist(err) {
				t.Errorf("repo bus touched (%v)", err)
			}
			for _, name := range []string{"pid", "pane", "pidfile.log"} {
				if _, err := os.Lstat(bus.StoreDir(p.State, "c2") + "/" + name); !os.IsNotExist(err) {
					t.Errorf("%s written (%v)", name, err)
				}
			}
		})
	}
}

func TestReattachLayout2NotHomeIsSilent(t *testing.T) {
	elsewhere := "/elsewhere/.git"
	for name, m := range map[string]bus.Meta{
		"visiting":        {Visiting: true},
		"homed elsewhere": {HomeRepo: &elsewhere},
	} {
		t.Run(name, func(t *testing.T) {
			p := testPaths(t).WithState(t.TempDir())
			layout2Meta(t, p, m)
			got := run(t, reattachWorld(), p, reattachReq("c2"))
			if got.code != 0 || got.stdout != "" || got.stderr != "" {
				t.Fatalf("code %d stdout %q stderr %q, want a silent 0", got.code, got.stdout, got.stderr)
			}
			for _, name := range []string{"pid", "pane", "pidfile.log"} {
				if _, err := os.Lstat(bus.StoreDir(p.State, "c2") + "/" + name); !os.IsNotExist(err) {
					t.Errorf("%s written (%v)", name, err)
				}
			}
		})
	}
}

func TestReattachLayout2HomedHereReregisters(t *testing.T) {
	p := testPaths(t).WithState(t.TempDir())
	layout2Meta(t, p, bus.Meta{HomeRepo: repoTag(t, p)})
	s := reattachWorld()
	s.start = map[int]string{400: "tok-E"}
	if got := run(t, s, p, reattachReq("c2")); got.code != 0 {
		t.Fatalf("code %d stderr %q", got.code, got.stderr)
	}
	if data, _ := os.ReadFile(bus.StoreDir(p.State, "c2") + "/pid"); string(data) != "400\n" {
		t.Errorf("pid = %q, want 400", data)
	}
}
