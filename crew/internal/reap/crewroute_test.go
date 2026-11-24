package reap

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/marks"
)

// stampTmux answers `display-message -p -t <window> #{@crew_id}` per window.
type stampTmux map[string]string

func (s stampTmux) Run(_ context.Context, args ...string) (string, error) {
	return s[args[3]], nil
}

func dispatchRow(branch, id string) string {
	return fmt.Sprintf(`{"ts":1,"kind":"dispatch","branch":%q,"crew_id":%q}`, branch, id)
}

func repoDispatchRow(branch, id, repo string, ts int) string {
	return fmt.Sprintf(`{"ts":%d,"kind":"dispatch","branch":%q,"crew_id":%q,"repo":%q}`, ts, branch, id, repo)
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
	tag := repoTag(common)
	writeLog(t, c.Log, repoDispatchRow("other", "zz", tag, 1), repoDispatchRow(rb, "c2", tag, 1), dispatchRow(rb, "nr"), repoDispatchRow(rb, "elsewhere", "/other/.git", 9))

	id := rowCrewID(context.Background(), stampTmux{}, []string{"@1"}, filepath.Join(root, "wt"), rb, eventDocs(paths.Log), indexCrewDocs(paths))
	if id != "c2" {
		t.Fatalf("crew id = %q, want c2", id)
	}
	if got := rowLog(paths, id); got != c.Log {
		t.Fatalf("rowLog = %q, want %q", got, c.Log)
	}
}

// When both the repo log and an index-listed crew dispatched the branch, the
// newer dispatch row names the crew; a tie keeps the repo log's.
func TestRowCrewIDNewestDispatchWins(t *testing.T) {
	root, common := routeRepo(t)
	paths := bus.PathsFor(common)
	wt := filepath.Join(root, "wt")
	tag := repoTag(common)
	rows := func(rows ...string) func() []map[string]any {
		return func() []map[string]any {
			var docs []map[string]any
			for _, r := range rows {
				var doc map[string]any
				if err := json.Unmarshal([]byte(r), &doc); err != nil {
					t.Fatal(err)
				}
				docs = append(docs, doc)
			}
			return docs
		}
	}
	for _, tc := range []struct {
		name      string
		repo, idx func() []map[string]any
		want      string
	}{
		{"index newer", rows(repoDispatchRow(rb, "d1", tag, 5)), rows(repoDispatchRow(rb, "c2", tag, 9)), "c2"},
		{"repo newer", rows(repoDispatchRow(rb, "d1", tag, 9)), rows(repoDispatchRow(rb, "c2", tag, 5)), "d1"},
		{"tie", rows(repoDispatchRow(rb, "d1", tag, 7)), rows(repoDispatchRow(rb, "c2", tag, 7)), "d1"},
		{"repo only", rows(repoDispatchRow(rb, "d1", tag, 7)), rows(), "d1"},
		{"index only", rows(), rows(repoDispatchRow(rb, "c2", tag, 7)), "c2"},
		{"repo only, nil index", rows(repoDispatchRow(rb, "d1", tag, 7)), nil, "d1"},
		{"neither", rows(), rows(), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if id := rowCrewID(context.Background(), stampTmux{}, nil, wt, rb, tc.repo, tc.idx); id != tc.want {
				t.Fatalf("crew id = %q, want %q", id, tc.want)
			}
		})
	}
	writeLog(t, paths.Log, dispatchRow(rb, "d1"))
	if id := rowCrewID(context.Background(), stampTmux{}, nil, wt, rb, eventDocs(paths.Log), nil); id != "d1" {
		t.Fatalf("repo log on disk: crew id = %q", id)
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

// A layout-2 crew's sessions are on its own log and their await marks under its
// store dir: forget finds and drops them even with no repo log.
func TestForgetLayout2CrewMarks(t *testing.T) {
	f := newFx(t)
	f.paths = f.paths.WithState(filepath.Join(f.root, "state"))
	if err := bus.CreateMeta(f.paths.State, bus.Meta{ID: "c2", CreatedBy: "register"}); err != nil {
		t.Fatal(err)
	}
	c, err := f.paths.Crew("c2")
	if err != nil || c.Layout != 2 {
		t.Fatalf("crew = %+v, %v", c, err)
	}
	writeLog(t, c.Log, `{"ts":2,"crew_id":"c2","kind":"dispatch","branch":"`+rb+`","session":"s3"}`)
	id, other := "worker:"+rb+"#s3", "worker:feat/other#s9"
	var gone, kept []string
	for _, p := range []string{marks.Path(c.State, "c2", id), marks.LockPath(c.State, "c2", id), marks.Path(c.State, "c2", other)} {
		writeLog(t, p, "{}")
		if strings.Contains(p, "other") {
			kept = append(kept, p)
		} else {
			gone = append(gone, p)
		}
	}
	f.run().forget(rb, "c2")
	for _, p := range gone {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Errorf("%s still there", p)
		}
	}
	for _, p := range kept {
		if _, err := os.Lstat(p); err != nil {
			t.Errorf("%s removed: %v", p, err)
		}
	}
}
