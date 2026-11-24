package bus

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestIndexRepos(t *testing.T) {
	state := filepath.Join(t.TempDir(), "crew")
	for _, c := range []string{"/z/.git", "/a/.git"} {
		if _, err := IndexAdd(state, c, "s", "c1", time.UnixMilli(1)); err != nil {
			t.Fatal(err)
		}
	}
	dir := filepath.Dir(IndexPath(state, "/a/.git"))
	writeStateFile(t, filepath.Join(dir, "misnamed"), `{"v":1,"common":"/m/.git","slug":""}`+"\n")
	writeStateFile(t, filepath.Join(dir, ".index.tmp"), `{"v":1,"common":"/t/.git","slug":""}`+"\n")
	want := []IndexHeader{{V: 1, Common: "/a/.git", Slug: "s"}, {V: 1, Common: "/z/.git", Slug: "s"}}
	if got := IndexRepos(state); !reflect.DeepEqual(got, want) {
		t.Fatalf("IndexRepos = %v, want %v", got, want)
	}
	if got := IndexRepos(filepath.Join(t.TempDir(), "none")); got != nil {
		t.Fatalf("no index dir: %v", got)
	}
}

type repoFx struct {
	t     *testing.T
	state string
	paths Paths
	tag   string
}

func newRepoFx(t *testing.T) *repoFx {
	t.Helper()
	root := t.TempDir()
	common := filepath.Join(root, "r", ".git")
	if err := os.MkdirAll(common, 0o755); err != nil {
		t.Fatal(err)
	}
	tag, err := RepoTag(common)
	if err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(root, "state")
	return &repoFx{t: t, state: state, paths: PathsFor(common).WithState(state), tag: tag}
}

// crew creates a live indexed layout-2 crew with the given log rows.
func (f *repoFx) crew(id string, rows ...string) {
	f.t.Helper()
	if err := CreateMeta(f.state, Meta{ID: id}); err != nil {
		f.t.Fatal(err)
	}
	if _, err := IndexAdd(f.state, f.tag, "", id, time.UnixMilli(1)); err != nil {
		f.t.Fatal(err)
	}
	writeStateFile(f.t, StoreDir(f.state, id)+"/events.jsonl", strings.Join(rows, "\n")+"\n")
}

func (f *repoFx) rows() RepoRows {
	f.t.Helper()
	got, err := RepoCrewRows(f.paths)
	if err != nil {
		f.t.Fatal(err)
	}
	return got
}

func (f *repoFx) dispatch(id, branch, repo string, ts int) string {
	return `{"ts":` + strconv.Itoa(ts) + `,"crew_id":"` + id + `","kind":"dispatch","branch":"` + branch + `","repo":"` + repo + `"}`
}

func rowIDs(evs []Event) []string {
	var out []string
	for _, e := range evs {
		out = append(out, e.CrewID+"/"+string(e.Kind)+"/"+rowBranch(e))
	}
	return out
}

func TestRepoCrewRows(t *testing.T) {
	f := newRepoFx(t)
	f.crew("c1",
		f.dispatch("c1", "feat/a", f.tag, 10),
		`{"ts":11,"crew_id":"c1","kind":"status","from":"worker:feat/a#s1234","body":{"state":"working"}}`,
		`{"ts":12,"crew_id":"c1","kind":"msg","from":"dispatcher","to":"role:feat/a:reviewer"}`,
		`{"ts":13,"crew_id":"c1","kind":"status","body":{"state":"working"}}`,
		f.dispatch("c1", "feat/else", "/other/.git", 14),
		`{"ts":15,"crew_id":"c1","kind":"status","branch":"feat/else"}`,
	)
	f.crew("c2", f.dispatch("c2", "feat/b", "/other/.git", 10), `{"ts":11,"crew_id":"c2","kind":"status","branch":"feat/b"}`)
	if _, err := IndexAdd(f.state, f.tag, "", "ghost", time.UnixMilli(1)); err != nil {
		t.Fatal(err)
	}
	if err := CreateMeta(f.state, Meta{ID: "arch"}); err != nil {
		t.Fatal(err)
	}
	if _, err := IndexAdd(f.state, f.tag, "", "arch", time.UnixMilli(1)); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(f.state+"/archive", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(StoreDir(f.state, "arch"), ArchiveDir(f.state, "arch")); err != nil {
		t.Fatal(err)
	}

	got := f.rows()
	want := []string{"c1/dispatch/feat/a", "c1/status/feat/a", "c1/msg/feat/a"}
	if !reflect.DeepEqual(rowIDs(got.Events), want) {
		t.Fatalf("Events = %v, want %v", rowIDs(got.Events), want)
	}
	if !reflect.DeepEqual(got.Crews, map[string]string{"feat/a": "c1"}) {
		t.Fatalf("Crews = %v", got.Crews)
	}
}

func TestRepoCrewRowsZeroAndMismatch(t *testing.T) {
	f := newRepoFx(t)
	f.crew("c1", f.dispatch("c1", "feat/a", f.tag, 10))
	for name, p := range map[string]Paths{
		"no state":  PathsFor(f.paths.Common),
		"no common": StatePaths(f.state),
		"bad tag":   PathsFor(filepath.Join(t.TempDir(), "missing")).WithState(f.state),
	} {
		if got, err := RepoCrewRows(p); err != nil || got.Events != nil || got.Crews != nil {
			t.Fatalf("%s: %v", name, got)
		}
	}
	writeStateFile(t, IndexPath(f.state, f.tag), `{"v":1,"common":"/else/.git","slug":""}`+"\n"+`{"crew":"c1","ts":1}`+"\n")
	if got := f.rows(); got.Events != nil || got.Crews != nil {
		t.Fatalf("header mismatch: %v", got)
	}
}

func TestRepoCrewRowsUnreadableLogErrors(t *testing.T) {
	f := newRepoFx(t)
	f.crew("c1", f.dispatch("c1", "feat/a", f.tag, 10), "torn{")
	f.crew("c2", f.dispatch("c2", "feat/b", f.tag, 10))
	var de *DecodeError
	_, err := RepoCrewRows(f.paths)
	if !errors.As(err, &de) || !strings.Contains(err.Error(), "crew c1") {
		t.Fatalf("err = %v, want a decode error naming c1", err)
	}
	if _, err := ReadRepoEvents(f.paths); !errors.As(err, &de) {
		t.Fatalf("ReadRepoEvents err = %v", err)
	}

	// A crew with no log yet is not an error.
	if err := os.Remove(StoreDir(f.state, "c1") + "/events.jsonl"); err != nil {
		t.Fatal(err)
	}
	if got := f.rows(); !reflect.DeepEqual(got.Crews, map[string]string{"feat/b": "c2"}) {
		t.Fatalf("Crews = %v", got.Crews)
	}
}

func TestRepoCrewRowsNewestDispatchWins(t *testing.T) {
	f := newRepoFx(t)
	f.crew("c1", f.dispatch("c1", "feat/a", f.tag, 10), `{"ts":11,"crew_id":"c1","kind":"status","branch":"feat/a"}`)
	f.crew("c2", f.dispatch("c2", "feat/a", f.tag, 20), `{"ts":21,"crew_id":"c2","kind":"status","branch":"feat/a"}`)
	got := f.rows()
	if !reflect.DeepEqual(got.Crews, map[string]string{"feat/a": "c2"}) {
		t.Fatalf("Crews = %v", got.Crews)
	}
	if want := []string{"c2/dispatch/feat/a", "c2/status/feat/a"}; !reflect.DeepEqual(rowIDs(got.Events), want) {
		t.Fatalf("Events = %v, want %v", rowIDs(got.Events), want)
	}
}

func TestReadRepoEvents(t *testing.T) {
	f := newRepoFx(t)
	f.crew("c1", f.dispatch("c1", "feat/a", f.tag, 10), `{"ts":12,"crew_id":"c1","kind":"status","branch":"feat/a"}`)
	got, err := ReadRepoEvents(f.paths)
	if err != nil {
		t.Fatalf("missing repo log: %v", err)
	}
	if want := []string{"c1/dispatch/feat/a", "c1/status/feat/a"}; !reflect.DeepEqual(rowIDs(got), want) {
		t.Fatalf("crew rows only = %v", rowIDs(got))
	}

	writeStateFile(t, f.paths.Log,
		`{"ts":10,"kind":"msg","crew_id":"r","branch":"x"}`+"\n"+`{"ts":13,"kind":"msg","crew_id":"r","branch":"y"}`+"\n")
	got, err = ReadRepoEvents(f.paths)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"r/msg/x", "c1/dispatch/feat/a", "c1/status/feat/a", "r/msg/y"}
	if !reflect.DeepEqual(rowIDs(got), want) {
		t.Fatalf("merged = %v, want %v", rowIDs(got), want)
	}

	writeStateFile(t, f.paths.Log, "torn{\n")
	var de *DecodeError
	if _, err := ReadRepoEvents(f.paths); !errors.As(err, &de) {
		t.Fatalf("decode error not returned: %v", err)
	}
}

func TestRepoCrewRowsBranchFromWorker(t *testing.T) {
	f := newRepoFx(t)
	f.crew("c1",
		f.dispatch("c1", "feat/x", f.tag, 10),
		f.dispatch("c1", "feat/y", f.tag, 11),
		`{"ts":12,"crew_id":"c1","kind":"status","branch":"feat/x","from":"worker:feat/y#s1","body":{"state":"done"}}`,
		`{"ts":13,"crew_id":"c1","kind":"status","from":"worker:feat/y#x","body":{"state":"done"}}`,
		`{"ts":14,"crew_id":"c1","kind":"status","branch":"feat/y","from":"worker:feat/y#s2","body":{"state":"done"}}`,
	)
	got := f.rows()
	want := []string{"c1/dispatch/feat/x", "c1/dispatch/feat/y", "c1/status/feat/y", "c1/status/feat/y"}
	if !reflect.DeepEqual(rowIDs(got.Events), want) {
		t.Fatalf("Events = %v, want %v", rowIDs(got.Events), want)
	}
}

func TestRepoCrewRowsRepoFieldMustMatch(t *testing.T) {
	f := newRepoFx(t)
	f.crew("c1",
		f.dispatch("c1", "feat/a", f.tag, 10),
		`{"ts":11,"crew_id":"c1","kind":"reap","branch":"feat/a","repo":"/other/.git"}`,
		`{"ts":12,"crew_id":"c1","kind":"release","branch":"feat/a","repo":"`+f.tag+`"}`,
	)
	got := f.rows()
	want := []string{"c1/dispatch/feat/a", "c1/release/feat/a"}
	if !reflect.DeepEqual(rowIDs(got.Events), want) {
		t.Fatalf("Events = %v, want %v", rowIDs(got.Events), want)
	}
}

func TestReadRepoEventsKeepsRepoLogOrder(t *testing.T) {
	f := newRepoFx(t)
	writeStateFile(t, f.paths.Log,
		`{"ts":20,"kind":"msg","crew_id":"r","branch":"a"}`+"\n"+
			`{"ts":10,"kind":"msg","crew_id":"r","branch":"b"}`+"\n"+
			`{"kind":"msg","crew_id":"r","branch":"c"}`+"\n")
	got, err := ReadRepoEvents(f.paths)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"r/msg/a", "r/msg/b", "r/msg/c"}; !reflect.DeepEqual(rowIDs(got), want) {
		t.Fatalf("no crew rows = %v, want %v", rowIDs(got), want)
	}

	f.crew("c1", f.dispatch("c1", "feat/a", f.tag, 15), `{"ts":5,"crew_id":"c1","kind":"status","branch":"feat/a"}`)
	got, err = ReadRepoEvents(f.paths)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"c1/status/feat/a", "c1/dispatch/feat/a", "r/msg/a", "r/msg/b", "r/msg/c"}; !reflect.DeepEqual(rowIDs(got), want) {
		t.Fatalf("merged = %v, want %v", rowIDs(got), want)
	}
}

func TestRepoCrewRowsDropsOtherRepoDispatch(t *testing.T) {
	f := newRepoFx(t)
	f.crew("c1",
		f.dispatch("c1", "feat/x", f.tag, 10),
		f.dispatch("c1", "feat/x", "/other/.git", 20),
	)
	got := f.rows()
	if want := []string{"c1/dispatch/feat/x"}; !reflect.DeepEqual(rowIDs(got.Events), want) {
		t.Fatalf("Events = %v, want %v", rowIDs(got.Events), want)
	}
	if got.Crews["feat/x"] != "c1" {
		t.Fatalf("Crews = %v, want feat/x owned by c1", got.Crews)
	}
}
