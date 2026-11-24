package bus

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
)

func TestCreateMetaRoundTrip(t *testing.T) {
	state := filepath.Join(t.TempDir(), "crew")
	repo := "/r/.git"
	m := Meta{ID: "c1", Created: 1700000000000, HomeHost: "h", HomeRepo: &repo, HomeSlug: "o/r", CreatedBy: "register"}
	if err := CreateMeta(state, m); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(StoreDir(state, "c1"), "meta.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"layout":2,"id":"c1","created":1700000000000,"home_host":"h","home_repo":"/r/.git","home_slug":"o/r","visiting":false,"created_by":"register"}` + "\n"
	if string(data) != want {
		t.Fatalf("meta.json = %s, want %s", data, want)
	}
	got, err := ReadMeta(state, "c1")
	m.Layout = 2
	if err != nil || !reflect.DeepEqual(got, m) {
		t.Fatalf("ReadMeta = %+v, %v; want %+v", got, err, m)
	}
	entries, err := os.ReadDir(StoreDir(state, "c1"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("crew dir holds %v (%v), want only meta.json", entries, err)
	}
}

func TestCreateMetaNullHomeRepo(t *testing.T) {
	state := filepath.Join(t.TempDir(), "crew")
	if err := CreateMeta(state, Meta{ID: "c1", HomeHost: "h", CreatedBy: "register"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(StoreDir(state, "c1"), "meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"layout":2,"id":"c1","created":0,"home_host":"h","home_repo":null,"visiting":false,"created_by":"register"}` + "\n"; string(data) != want {
		t.Fatalf("meta.json = %s", data)
	}
}

func TestCreateMetaNeverRewrites(t *testing.T) {
	state := filepath.Join(t.TempDir(), "crew")
	if err := CreateMeta(state, Meta{ID: "c1", HomeHost: "first"}); err != nil {
		t.Fatal(err)
	}
	if err := CreateMeta(state, Meta{ID: "c1", HomeHost: "second"}); !errors.Is(err, ErrMetaExists) {
		t.Fatalf("second CreateMeta = %v, want ErrMetaExists", err)
	}
	if m, err := ReadMeta(state, "c1"); err != nil || m.HomeHost != "first" {
		t.Fatalf("meta rewritten: %+v, %v", m, err)
	}
	if entries, _ := os.ReadDir(StoreDir(state, "c1")); len(entries) != 1 {
		t.Fatalf("temp file left behind: %v", entries)
	}
}

func TestCreateMetaConcurrentExactlyOneWins(t *testing.T) {
	state := filepath.Join(t.TempDir(), "crew")
	const n = 16
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() { errs[i] = CreateMeta(state, Meta{ID: "c1", Created: int64(i)}) })
	}
	wg.Wait()
	wins := 0
	for _, err := range errs {
		switch {
		case err == nil:
			wins++
		case !errors.Is(err, ErrMetaExists):
			t.Fatalf("unexpected error %v", err)
		}
	}
	if wins != 1 {
		t.Fatalf("%d creators won, want exactly 1", wins)
	}
	if entries, _ := os.ReadDir(StoreDir(state, "c1")); len(entries) != 1 {
		t.Fatalf("temp files left behind: %v", entries)
	}
}

func TestCreateMetaInvalidIDNeverJoined(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "s", "crew")
	for _, id := range []string{"../x", "-x", "", ".", "..", "a/b"} {
		var inv *InvalidCrewIDError
		if err := CreateMeta(state, Meta{ID: id}); !errors.As(err, &inv) || inv.ID != id {
			t.Fatalf("CreateMeta(%q) = %v, want InvalidCrewIDError", id, err)
		}
		if _, err := ReadMeta(state, id); !errors.As(err, &inv) {
			t.Fatalf("ReadMeta(%q) = %v, want InvalidCrewIDError", id, err)
		}
	}
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Fatalf("invalid ids created %v", entries)
	}
}

func TestReadMetaRejects(t *testing.T) {
	state := filepath.Join(t.TempDir(), "crew")
	cases := map[string]string{
		"mismatch": `{"layout":2,"id":"other"}`,
		"layout1":  `{"layout":1,"id":"layout1"}`,
		"corrupt":  `{"layout":2,`,
	}
	for id, body := range cases {
		writeStateFile(t, filepath.Join(StoreDir(state, id), "meta.json"), body)
		if _, err := ReadMeta(state, id); err == nil {
			t.Errorf("ReadMeta(%q) accepted %s", id, body)
		}
	}
	if _, err := ReadMeta(state, "missing"); err == nil {
		t.Error("ReadMeta of a missing meta accepted")
	}
}

func TestLiveMetas(t *testing.T) {
	state := filepath.Join(t.TempDir(), "crew")
	if got := LiveMetas(state); got != nil {
		t.Fatalf("no crews dir: %v", got)
	}
	for _, id := range []string{"c", "B", "a"} {
		if err := CreateMeta(state, Meta{ID: id}); err != nil {
			t.Fatal(err)
		}
	}
	writeStateFile(t, filepath.Join(StoreDir(state, "bad"), "meta.json"), `{"layout":2,"id":"nope"}`)
	if err := os.MkdirAll(StoreDir(state, "nometa"), 0o755); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, m := range LiveMetas(state) {
		ids = append(ids, m.ID)
	}
	if want := []string{"B", "a", "c"}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("LiveMetas ids = %v, want %v", ids, want)
	}
}

func TestReadMetaAt(t *testing.T) {
	state := filepath.Join(t.TempDir(), "crew")
	if err := CreateMeta(state, Meta{ID: "c1", HomeHost: "halo"}); err != nil {
		t.Fatal(err)
	}
	m, err := ReadMetaAt(StoreDir(state, "c1"), "c1")
	if err != nil || m.HomeHost != "halo" {
		t.Fatalf("ReadMetaAt = %+v, %v", m, err)
	}
	writeStateFile(t, filepath.Join(state, "d", "meta.json"), `{"layout":2,"id":"other"}`)
	writeStateFile(t, filepath.Join(state, "e", "meta.json"), `{"layout":1,"id":"e"}`)
	for dir, id := range map[string]string{"d": "d", "e": "e"} {
		if _, err := ReadMetaAt(filepath.Join(state, dir), id); err == nil {
			t.Errorf("ReadMetaAt(%q) accepted a bad meta", dir)
		}
	}
	var inv *InvalidCrewIDError
	if _, err := ReadMetaAt(StoreDir(state, "c1"), "../c1"); !errors.As(err, &inv) {
		t.Errorf("ReadMetaAt invalid id = %v, want InvalidCrewIDError", err)
	}
}
