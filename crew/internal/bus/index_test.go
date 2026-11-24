package bus

import (
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"
)

func TestIndexPath(t *testing.T) {
	sum := sha256.Sum256([]byte("/r/.git"))
	if got, want := IndexPath("/s/crew", "/r/.git"), "/s/crew/index/repos/"+hex.EncodeToString(sum[:]); got != want {
		t.Fatalf("IndexPath = %q, want %q", got, want)
	}
}

func TestIndexAddHeaderAndDedupe(t *testing.T) {
	state := filepath.Join(t.TempDir(), "crew")
	add := func(id string, ms int64) bool {
		t.Helper()
		added, err := IndexAdd(state, "/r/.git", "o/r", id, time.UnixMilli(ms))
		if err != nil {
			t.Fatal(err)
		}
		return added
	}
	if !add("c1", 10) || !add("c2", 20) {
		t.Fatal("first adds reported not added")
	}
	if add("c1", 30) {
		t.Fatal("re-adding c1 reported added")
	}
	data, err := os.ReadFile(IndexPath(state, "/r/.git"))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"v":1,"common":"/r/.git","slug":"o/r"}` + "\n" + `{"crew":"c1","ts":10}` + "\n" + `{"crew":"c2","ts":20}` + "\n"
	if string(data) != want {
		t.Fatalf("index = %q, want %q", data, want)
	}
	if got := IndexCrews(state, "/r/.git"); !reflect.DeepEqual(got, []string{"c1", "c2"}) {
		t.Fatalf("IndexCrews = %v", got)
	}
	if got := IndexCrews(state, "/other/.git"); got != nil {
		t.Fatalf("IndexCrews of an unindexed repo = %v", got)
	}
	entries, _ := os.ReadDir(filepath.Dir(IndexPath(state, "/r/.git")))
	if len(entries) != 1 {
		t.Fatalf("index dir holds %v, want one file", entries)
	}
	var inv *InvalidCrewIDError
	if _, err := IndexAdd(state, "/r/.git", "o/r", "../x", time.UnixMilli(1)); !errors.As(err, &inv) {
		t.Fatalf("invalid id: %v", err)
	}
}

func TestIndexReadersTolerateJunk(t *testing.T) {
	state := filepath.Join(t.TempDir(), "crew")
	path := IndexPath(state, "/r/.git")
	writeStateFile(t, path, `{"v":1,"common":"/r/.git","slug":""}`+"\n"+
		`{"crew":"a","ts":1}`+"\n"+"torn{\n"+`{"crew":"../x","ts":2}`+"\n"+`{"crew":"b","ts":3}`+"\n"+`{"crew":"a","ts":4}`)
	if got := IndexCrews(state, "/r/.git"); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("IndexCrews = %v", got)
	}
	writeStateFile(t, path, `{"crew":"a","ts":1}`+"\n")
	if got := IndexCrews(state, "/r/.git"); got != nil {
		t.Fatalf("headerless file listed %v", got)
	}
}

func TestIndexListed(t *testing.T) {
	state := filepath.Join(t.TempDir(), "crew")
	if IndexListed(state, "a") {
		t.Fatal("no index dir: listed")
	}
	for _, c := range []struct{ repo, id string }{{"/r1/.git", "a"}, {"/r2/.git", "b"}} {
		if _, err := IndexAdd(state, c.repo, "", c.id, time.UnixMilli(1)); err != nil {
			t.Fatal(err)
		}
	}
	writeStateFile(t, filepath.Join(state, "index", "repos", ".index.tmp"), `{"v":1,"common":"/x","slug":""}`+"\n"+`{"crew":"t","ts":1}`+"\n")
	for id, want := range map[string]bool{"a": true, "b": true, "c": false, "t": false} {
		if got := IndexListed(state, id); got != want {
			t.Errorf("IndexListed(%q) = %v, want %v", id, got, want)
		}
	}
}

func dispatchRow(id, repo string, ts int) string {
	return `{"ts":` + strconv.Itoa(ts) + `,"crew_id":"` + id + `","kind":"dispatch","repo":"` + repo + `","branch":"b"}`
}

func writeGz(t *testing.T, path, content string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := gzip.NewWriter(f)
	if _, err := zw.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// rebuildFixture: live crew a dispatched in r1 and r2, archived crew b (gzipped
// log) in r1, live crew c never dispatched; a's log also holds another crew's
// row and a status row naming r3, which list nothing.
func rebuildFixture(t *testing.T) string {
	t.Helper()
	state := filepath.Join(t.TempDir(), "crew")
	for _, id := range []string{"a", "c"} {
		if err := CreateMeta(state, Meta{ID: id}); err != nil {
			t.Fatal(err)
		}
	}
	writeStateFile(t, filepath.Join(StoreDir(state, "a"), "events.jsonl"),
		dispatchRow("a", "/r2/.git", 50)+"\n"+dispatchRow("a", "/r1/.git", 30)+"\n"+dispatchRow("a", "/r1/.git", 70)+"\n"+
			dispatchRow("z", "/r3/.git", 5)+"\n"+`{"ts":6,"crew_id":"a","kind":"status","repo":"/r3/.git"}`+"\n"+"torn{\n")
	writeStateFile(t, filepath.Join(ArchiveDir(state, "b"), "meta.json"), `{"layout":2,"id":"b"}`+"\n")
	writeGz(t, filepath.Join(ArchiveDir(state, "b"), "events.jsonl.gz"), dispatchRow("b", "/r1/.git", 10)+"\n")
	return state
}

func slugOf(common string) string { return "slug:" + common }

func TestIndexRebuildFromLogs(t *testing.T) {
	state := rebuildFixture(t)
	// Adds in dispatch order, as dispatch makes them.
	for _, c := range []struct{ repo, id string }{{"/r1/.git", "b"}, {"/r1/.git", "a"}, {"/r2/.git", "a"}} {
		if _, err := IndexAdd(state, c.repo, "old", c.id, time.UnixMilli(1)); err != nil {
			t.Fatal(err)
		}
	}
	added := map[string][]string{"/r1/.git": IndexCrews(state, "/r1/.git"), "/r2/.git": IndexCrews(state, "/r2/.git")}

	if err := os.RemoveAll(filepath.Join(state, "index")); err != nil {
		t.Fatal(err)
	}
	if err := IndexRebuild(state, slugOf); err != nil {
		t.Fatal(err)
	}
	for repo, want := range added {
		if got := IndexCrews(state, repo); !reflect.DeepEqual(got, want) {
			t.Errorf("rebuilt IndexCrews(%s) = %v, want %v", repo, got, want)
		}
	}
	data, err := os.ReadFile(IndexPath(state, "/r1/.git"))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"v":1,"common":"/r1/.git","slug":"slug:/r1/.git"}` + "\n" + `{"crew":"b","ts":10}` + "\n" + `{"crew":"a","ts":30}` + "\n"
	if string(data) != want {
		t.Fatalf("rebuilt r1 index = %q, want %q", data, want)
	}
	entries, err := os.ReadDir(filepath.Join(state, "index", "repos"))
	if err != nil || len(entries) != 2 {
		t.Fatalf("index files = %v (%v), want r1 and r2 only", entries, err)
	}
	if _, err := os.Lstat(filepath.Join(state, "gc.lock.d")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("gc.lock.d left behind: %v", err)
	}
}

func TestIndexRebuildDropsStaleLines(t *testing.T) {
	state := rebuildFixture(t)
	for _, c := range []struct{ repo, id string }{{"/r1/.git", "c"}, {"/r1/.git", "gone"}, {"/r3/.git", "a"}} {
		if _, err := IndexAdd(state, c.repo, "", c.id, time.UnixMilli(1)); err != nil {
			t.Fatal(err)
		}
	}
	if err := IndexRebuild(state, slugOf); err != nil {
		t.Fatal(err)
	}
	if got := IndexCrews(state, "/r1/.git"); !reflect.DeepEqual(got, []string{"b", "a"}) {
		t.Fatalf("r1 = %v, want [b a]", got)
	}
	if _, err := os.Stat(IndexPath(state, "/r3/.git")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("r3 index kept with no backing row: %v", err)
	}
	if IndexListed(state, "gone") || IndexListed(state, "c") {
		t.Fatal("stale ids still listed")
	}
}

func TestIndexRebuildEmptyStateCreatesNoIndex(t *testing.T) {
	state := filepath.Join(t.TempDir(), "crew")
	if err := IndexRebuild(state, slugOf); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(state, "index")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("index dir created: %v", err)
	}
}
