package await

import (
	"os"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/marks"
)

func TestMarkUnderCrewStateRoot(t *testing.T) {
	row := msg(1785951264000, disp, me, "answer")

	t.Run("pinned", func(t *testing.T) {
		p := fixture(t)
		state := t.TempDir()
		p = p.WithState(state)
		if err := bus.CreateMeta(state, bus.Meta{ID: "c1", CreatedBy: "register"}); err != nil {
			t.Fatal(err)
		}
		store := bus.StoreDir(state, "c1")
		if err := os.WriteFile(store+"/events.jsonl", []byte(row+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, stderr, code := run(t, p, Request{Me: me, Timeout: "0"}); code != 0 || stderr != "" {
			t.Fatalf("code %d stderr %q", code, stderr)
		}
		if _, err := os.Stat(marks.Path(store, "c1", me)); err != nil {
			t.Errorf("mark not under the store dir: %v", err)
		}
		if _, err := os.Stat(marks.Path(p.Dir, "c1", me)); err == nil {
			t.Error("mark leaked under paths.Dir")
		}
	})

	t.Run("unpinned", func(t *testing.T) {
		p := fixture(t).WithState(t.TempDir())
		writeLog(t, p, row)
		if _, stderr, code := run(t, p, Request{Me: me, Timeout: "0"}); code != 0 || stderr != "" {
			t.Fatalf("code %d stderr %q", code, stderr)
		}
		if _, err := os.Stat(marks.Path(p.Dir, "c1", me)); err != nil {
			t.Errorf("mark not under paths.Dir: %v", err)
		}
	})
}
