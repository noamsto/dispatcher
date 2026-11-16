package fleet

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/proc/proctest"
)

func writeRelayFile(t *testing.T, common, crew, name, text string, mtime time.Time) {
	t.Helper()
	path := filepath.Join(common, "crew", "crews", crew, "relay", name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func TestCrewRelays(t *testing.T) {
	common := t.TempDir()
	hm, mm := time.UnixMilli(1700000010000), time.UnixMilli(1700000020000)
	writeRelayFile(t, common, "c1", "halo.lock.d/pid", "4242\n", hm)
	writeRelayFile(t, common, "c1", "halo.cursor", "17\n", hm)
	writeRelayFile(t, common, "c1", "mbp.cursor", "9\n", mm)
	hts, mts := hm.UnixMilli(), mm.UnixMilli()

	live := proctest.New(map[int]bool{4242: true}, nil)
	want := []RelayStatus{{Host: "halo", Live: true, CursorTS: &hts}, {Host: "mbp", CursorTS: &mts}}
	if got := crewRelays(common, "c1", live); !reflect.DeepEqual(got, want) {
		t.Errorf("live holder: got %+v, want %+v", got, want)
	}

	want[0].Live = false
	if got := crewRelays(common, "c1", proctest.New(nil, nil)); !reflect.DeepEqual(got, want) {
		t.Errorf("dead holder: got %+v, want %+v", got, want)
	}
}

func TestCrewRelaysLockOnly(t *testing.T) {
	common := t.TempDir()
	writeRelayFile(t, common, "c1", "halo.lock.d/pid", "4242\n", time.Now())
	got := crewRelays(common, "c1", proctest.New(map[int]bool{4242: true}, nil))
	if want := []RelayStatus{{Host: "halo", Live: true}}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestCrewRelaysNoDir(t *testing.T) {
	got := crewRelays(t.TempDir(), "c1", proctest.New(nil, nil))
	if got == nil || len(got) != 0 {
		t.Errorf("got %#v, want empty non-nil", got)
	}
}
