package reap

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
)

// stampTmux answers `display-message -p -t <window> #{@crew_id}` per window.
type stampTmux map[string]string

func (s stampTmux) Run(_ context.Context, args ...string) (string, error) {
	return s[args[3]], nil
}

func dispatchRow(branch, id string) string {
	return fmt.Sprintf(`{"ts":1,"kind":"dispatch","branch":%q,"crew_id":%q}`, branch, id)
}

func writeLog(t *testing.T, path string, rows ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	var data string
	for _, r := range rows {
		data += r + "\n"
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

// routeRepo is a fake repo common dir under a temp root, with isolated HOME and
// XDG dirs.
func routeRepo(t *testing.T) (root, common string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", root)
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "xdgstate"))
	common = filepath.Join(root, "repo", ".git")
	if err := os.MkdirAll(common, 0o755); err != nil {
		t.Fatal(err)
	}
	return root, common
}

func TestRowCrewIDResolvesLayout2ThroughIndex(t *testing.T) {
	root, common := routeRepo(t)
	paths := bus.PathsFor(common).WithState(filepath.Join(root, "state"))
	if err := bus.CreateMeta(paths.State, bus.Meta{ID: "c2", CreatedBy: "register"}); err != nil {
		t.Fatal(err)
	}
	if _, err := bus.IndexAdd(paths.State, repoTag(common), "repo", "c2", time.Now()); err != nil {
		t.Fatal(err)
	}
	c, err := paths.Crew("c2")
	if err != nil || c.Layout != 2 {
		t.Fatalf("crew = %+v, %v", c, err)
	}
	writeLog(t, c.Log, dispatchRow("other", "zz"), dispatchRow(rb, "c2"))

	id := rowCrewID(context.Background(), stampTmux{}, []string{"@1"}, filepath.Join(root, "wt"), rb, eventDocs(paths.Log), indexCrewDocs(paths))
	if id != "c2" {
		t.Fatalf("crew id = %q, want c2", id)
	}
	if got := rowLog(paths, id); got != c.Log {
		t.Fatalf("rowLog = %q, want %q", got, c.Log)
	}
}

func TestRowCrewIDIndexIsLastResort(t *testing.T) {
	root, common := routeRepo(t)
	paths := bus.PathsFor(common)
	writeLog(t, paths.Log, dispatchRow(rb, "d1"))
	called := false
	spy := func() []map[string]any { called = true; return nil }
	wt := filepath.Join(root, "wt")

	if id := rowCrewID(context.Background(), stampTmux{}, nil, wt, rb, eventDocs(paths.Log), spy); id != "d1" || called {
		t.Fatalf("crew id = %q, index read = %v; want d1, false", id, called)
	}
	if id := rowCrewID(context.Background(), stampTmux{}, nil, wt, rb, eventDocs(paths.Log), nil); id != "d1" {
		t.Fatalf("nil indexDocs: crew id = %q", id)
	}
	if id := rowCrewID(context.Background(), stampTmux{}, nil, wt, "none", eventDocs(paths.Log), func() []map[string]any { return nil }); id != "" {
		t.Fatalf("empty indexDocs: crew id = %q", id)
	}
}

func TestRowLogLayout1KeepsEveryRowInRepoLog(t *testing.T) {
	for _, withState := range []bool{false, true} {
		for _, c := range crewIDCases {
			t.Run(fmt.Sprintf("state=%v/%s", withState, c.name), func(t *testing.T) {
				root, common := routeRepo(t)
				paths := bus.PathsFor(common)
				if withState {
					paths = paths.WithState(filepath.Join(root, "state"))
				}
				wt := filepath.Join(root, "wt")
				if err := os.Mkdir(wt, 0o755); err != nil {
					t.Fatal(err)
				}
				if c.record != "" {
					writeRecord(t, wt, c.record)
				}
				if c.bus != "" {
					writeLog(t, paths.Log, dispatchRow(rb, c.bus))
				}
				id := rowCrewID(context.Background(), stampTmux{"@1": c.window}, []string{"@1"}, wt, rb, eventDocs(paths.Log), indexCrewDocs(paths))
				if id != c.want {
					t.Fatalf("crew id = %q, want %q", id, c.want)
				}
				if got := rowLog(paths, id); got != paths.Log {
					t.Fatalf("rowLog = %q, want repo log %q", got, paths.Log)
				}
			})
		}
	}
}
