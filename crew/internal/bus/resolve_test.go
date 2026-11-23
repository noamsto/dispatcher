package bus

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

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
	want := Crew{ID: "c1", Log: p.Log, Dir: "/r/.git/crew/crews/c1", RepoDir: "/r/.git/crew"}
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
