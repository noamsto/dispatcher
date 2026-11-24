package bus

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/tmux/tmuxtest"
)

func TestPathsFor(t *testing.T) {
	p := PathsFor("/r/.git")
	if p.Common != "/r/.git" || p.Dir != "/r/.git/crew" || p.Log != "/r/.git/crew/events.jsonl" {
		t.Fatalf("%+v", p)
	}
	if got := p.CrewsDir(); got != "/r/.git/crew/crews" {
		t.Fatalf("CrewsDir = %q", got)
	}
	if got := p.CrewDir("c1"); got != "/r/.git/crew/crews/c1" {
		t.Fatalf("CrewDir = %q", got)
	}
}

func TestPathsCrew(t *testing.T) {
	p := PathsFor("/r/.git")
	got, err := p.Crew("c1")
	want := Crew{ID: "c1", Log: p.Log, Dir: "/r/.git/crew/crews/c1", RepoDir: "/r/.git/crew", State: "/r/.git/crew", Layout: 1}
	if err != nil || got != want {
		t.Fatalf("Crew = %+v, %v; want %+v", got, err, want)
	}
	for _, id := range []string{"", ".", "..", "-x", "a/b", "a b"} {
		_, err := p.Crew(id)
		var inv *InvalidCrewIDError
		if !errors.As(err, &inv) || inv.ID != id {
			t.Fatalf("Crew(%q) err = %v", id, err)
		}
		if want := `invalid crew id "` + id + `"`; err.Error() != want {
			t.Fatalf("Error() = %q, want %q", err.Error(), want)
		}
	}
}

func TestWithStateAndStatePaths(t *testing.T) {
	p := PathsFor("/r/.git").WithState("/s/crew")
	if p.State != "/s/crew" || p.Common != "/r/.git" || p.Log != "/r/.git/crew/events.jsonl" {
		t.Fatalf("WithState = %+v", p)
	}
	if PathsFor("/r/.git").State != "" {
		t.Fatal("PathsFor must leave State empty")
	}
	if got := StatePaths("/s/crew"); got != (Paths{State: "/s/crew"}) {
		t.Fatalf("StatePaths = %+v", got)
	}
}

// classifyFixture is a state root holding a layout-2 crew l2, an archived crew
// arch, an id gone that only an index lists, and an id both that has a live
// and an archived meta; legacy ids have nothing under it.
func classifyFixture(t *testing.T) string {
	t.Helper()
	state := filepath.Join(t.TempDir(), "crew")
	for _, id := range []string{"l2", "both"} {
		if err := CreateMeta(state, Meta{ID: id, HomeHost: "h", CreatedBy: "register"}); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"arch", "both", "archidx"} {
		writeStateFile(t, filepath.Join(ArchiveDir(state, id), "meta.json"), `{"layout":2,"id":"`+id+`"}`+"\n")
	}
	for _, id := range []string{"gone", "archidx", "l2"} {
		if _, err := IndexAdd(state, "/r/.git", "o/r", id, time.UnixMilli(1)); err != nil {
			t.Fatal(err)
		}
	}
	return state
}

// tree lists every path under root with its size, to prove a call wrote nothing.
func tree(t *testing.T, root string) map[string]int64 {
	t.Helper()
	got := map[string]int64{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		got[path] = info.Size()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestPathsCrewClassification(t *testing.T) {
	state := classifyFixture(t)
	repo := PathsFor("/r/.git").WithState(state)
	legacy := func(p Paths, id string) Crew {
		return Crew{ID: id, Log: p.Log, Dir: p.CrewDir(id), RepoDir: p.Dir, State: p.Dir, Layout: 1}
	}
	layout2 := func(p Paths, id string) Crew {
		dir := StoreDir(state, id)
		return Crew{ID: id, Log: dir + "/events.jsonl", Dir: dir, RepoDir: p.Dir, State: dir, Layout: 2}
	}
	before := tree(t, state)

	cases := []struct {
		name     string
		p        Paths
		id       string
		want     Crew
		wantErr  string
		refusal  bool
		notRepo  bool
		archived bool
	}{
		{name: "layout 2", p: repo, id: "l2", want: layout2(repo, "l2")},
		{name: "crews meta beats archive", p: repo, id: "both", want: layout2(repo, "both")},
		{name: "crews meta beats index", p: repo, id: "l2", want: layout2(repo, "l2")},
		{name: "archived", p: repo, id: "arch", wantErr: "crew arch is archived", refusal: true, archived: true},
		{name: "archive beats index", p: repo, id: "archidx", wantErr: "crew archidx is archived", refusal: true, archived: true},
		{name: "index only", p: repo, id: "gone", refusal: true,
			wantErr: "crew gone is listed in the repo index but has no crew dir — run 'crew index rebuild'; live crews: both, l2"},
		{name: "legacy", p: repo, id: "c1", want: legacy(repo, "c1")},
		{name: "no state root is legacy", p: PathsFor("/r/.git"), id: "l2", want: legacy(PathsFor("/r/.git"), "l2")},
		{name: "no state root skips archive", p: PathsFor("/r/.git"), id: "arch", want: legacy(PathsFor("/r/.git"), "arch")},
		{name: "no state root skips index", p: PathsFor("/r/.git"), id: "gone", want: legacy(PathsFor("/r/.git"), "gone")},
		{name: "layout 2 outside a repo", p: StatePaths(state), id: "l2", want: layout2(StatePaths(state), "l2")},
		{name: "legacy outside a repo", p: StatePaths(state), id: "c1", notRepo: true},
		{name: "refusal outside a repo", p: StatePaths(state), id: "arch", wantErr: "crew arch is archived", refusal: true, archived: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := c.p.Crew(c.id)
			switch {
			case c.notRepo:
				if !errors.Is(err, ErrNotRepo) || IsRefusal(err) {
					t.Fatalf("err = %v, want ErrNotRepo", err)
				}
			case c.wantErr != "":
				if err == nil || err.Error() != c.wantErr || IsRefusal(err) != c.refusal {
					t.Fatalf("err = %v, want %q (refusal %v)", err, c.wantErr, c.refusal)
				}
				var a *ArchivedError
				if errors.As(err, &a) != c.archived || (c.archived && a.ID != c.id) {
					t.Fatalf("ArchivedError = %v, want %v", err, c.archived)
				}
			case err != nil || got != c.want:
				t.Fatalf("Crew = %+v, %v; want %+v", got, err, c.want)
			}
		})
	}
	if _, err := repo.Crew("a/b"); !errors.As(err, new(*InvalidCrewIDError)) {
		t.Fatalf("invalid id: err = %v", err)
	}
	if after := tree(t, state); !maps.Equal(after, before) {
		t.Fatalf("classification wrote under the state root:\nbefore %v\nafter  %v", before, after)
	}
}

func TestPathsCrewMetaMustBeAFile(t *testing.T) {
	state := filepath.Join(t.TempDir(), "crew")
	if err := os.MkdirAll(filepath.Join(StoreDir(state, "c1"), "meta.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := PathsFor("/r/.git").WithState(state)
	if got, err := p.Crew("c1"); err != nil || got.Layout != 1 {
		t.Fatalf("meta.json dir: Crew = %+v, %v; want layout 1", got, err)
	}
}

func TestIndexOnlyErrorNoLiveCrews(t *testing.T) {
	err := &IndexOnlyError{ID: "x"}
	if want := "crew x is listed in the repo index but has no crew dir — run 'crew index rebuild'; live crews: none"; err.Error() != want {
		t.Fatalf("Error() = %q", err.Error())
	}
}

func TestResolveCrewID(t *testing.T) {
	window := func(id string) func(context.Context) string {
		return func(context.Context) string { return id }
	}
	cases := []struct {
		name    string
		task    *string
		env     string
		l       Lookup
		want    string
		wantErr error
		invalid bool
	}{
		{name: "env", env: "e", want: "e"},
		{name: "header", task: ptr("crew_id: h\n"), want: "h"},
		{name: "header beats env", task: ptr("crew_id: h\n"), env: "e", want: "h"},
		{name: "env first beats header", task: ptr("crew_id: h\n"), env: "e", l: Lookup{EnvFirst: true}, want: "e"},
		{name: "env first falls back to header", task: ptr("crew_id: h\n"), l: Lookup{EnvFirst: true}, want: "h"},
		{name: "flag beats all", task: ptr("crew_id: h\n"), env: "e", l: Lookup{Flag: "f", Window: window("w")}, want: "f"},
		{name: "window when nothing else", l: Lookup{Window: window("w")}, want: "w"},
		{name: "window only when empty", env: "e", l: Lookup{Window: window("w")}, want: "e"},
		{name: "nil window", wantErr: ErrNoCrew},
		{name: "empty window", l: Lookup{Window: window("")}, wantErr: ErrNoCrew},
		{name: "invalid flag", l: Lookup{Flag: "a/b"}, want: "a/b", invalid: true},
		{name: "invalid header", task: ptr("crew_id: ../x\n"), want: "../x", invalid: true},
		{name: "invalid env", env: "-x", want: "-x", invalid: true},
		{name: "invalid window", l: Lookup{Window: window("..")}, want: "..", invalid: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			main, _ := newRepo(t)
			if c.task != nil {
				writeFile(t, filepath.Join(main, "WORKER_TASK.md"), *c.task)
			}
			t.Setenv("CREW_ID", c.env)
			got, err := ResolveCrewID(context.Background(), main, c.l)
			if got != c.want {
				t.Fatalf("id = %q, want %q", got, c.want)
			}
			var inv *InvalidCrewIDError
			switch {
			case c.invalid:
				if !errors.As(err, &inv) || inv.ID != c.want {
					t.Fatalf("err = %v, want InvalidCrewIDError", err)
				}
			case c.wantErr != nil:
				if !errors.Is(err, c.wantErr) {
					t.Fatalf("err = %v, want %v", err, c.wantErr)
				}
			case err != nil:
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestWindowCrewID(t *testing.T) {
	pane := func(v string) func(string) string {
		return func(k string) string {
			if k != "TMUX_PANE" {
				t.Fatalf("lookup of %q", k)
			}
			return v
		}
	}
	const call = "display-message -p -t %3 #{@crew_id}"
	cases := []struct {
		name  string
		pane  string
		out   map[string]string
		want  string
		calls []string
	}{
		{"success", "%3", map[string]string{"display-message": "c1\n"}, "c1", []string{call}},
		{"no pane", "", map[string]string{"display-message": "c1\n"}, "", nil},
		{"tmux fails", "%3", nil, "", []string{call}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &tmuxtest.Fake{Out: c.out}
			if got := WindowCrewID(pane(c.pane), f)(context.Background()); got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
			got := f.Calls()
			if len(got) != len(c.calls) || (len(got) == 1 && got[0] != c.calls[0]) {
				t.Fatalf("calls = %v, want %v", got, c.calls)
			}
		})
	}
	if got := WindowCrewID(pane("%3"), nil)(context.Background()); got != "" {
		t.Fatalf("nil client: %q", got)
	}
}

func TestRepoTag(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(root, "real")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if got, err := RepoTag(link); err != nil || got != real {
		t.Fatalf("RepoTag = %q, %v; want %q", got, err, real)
	}
	if _, err := RepoTag(filepath.Join(root, "missing")); err == nil {
		t.Fatal("missing dir: want error")
	}
}
