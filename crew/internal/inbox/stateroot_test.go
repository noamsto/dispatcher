package inbox

import (
	"os"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/marks"
)

func TestMarkUnderCrewStateRoot(t *testing.T) {
	const me = "worker:feat/x#s1-1"

	t.Run("pinned", func(t *testing.T) {
		p := fixture(t)
		state := t.TempDir()
		p = p.WithState(state)
		if err := bus.CreateMeta(state, bus.Meta{ID: "c1", CreatedBy: "register"}); err != nil {
			t.Fatal(err)
		}
		store := bus.StoreDir(state, "c1")
		if err := os.WriteFile(store+"/events.jsonl", []byte(msgRow+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, stderr, code := run(t, p, me, "c1"); code != 0 {
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
		writeLog(t, p, msgRow+"\n")
		if _, stderr, code := run(t, p, me, "c1"); code != 0 {
			t.Fatalf("code %d stderr %q", code, stderr)
		}
		if !strings.Contains(readMarks(t, p, "c1", me), "1785951264000") {
			t.Error("mark not under paths.Dir")
		}
	})
}
