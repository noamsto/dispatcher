package nudge

import (
	"context"
	"io"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/marks"
)

func pinCrew(t *testing.T, p bus.Paths, id string) (bus.Paths, string) {
	t.Helper()
	state := t.TempDir()
	if err := bus.CreateMeta(state, bus.Meta{ID: id, CreatedBy: "register"}); err != nil {
		t.Fatal(err)
	}
	return p.WithState(state), bus.StoreDir(state, id)
}

func TestWaitLockUnderCrewStateRoot(t *testing.T) {
	t.Run("pinned", func(t *testing.T) {
		f := newArm(t)
		var store string
		f.paths, store = pinCrew(t, f.paths, "c1")
		f.holdLock = strconv.Itoa(os.Getpid())
		f.rows = dispatchRows(t, "claude")
		if rc := f.run(Request{Wait: "0", Target: idleReq}); rc != 2 {
			t.Fatalf("rc = %d, stderr = %q", rc, f.errOut())
		}
		if !strings.HasPrefix(f.lockDir(), store+"/nudge-wait/") {
			t.Errorf("lock dir %s not under the store dir %s", f.lockDir(), store)
		}
		if _, err := os.Stat(f.paths.Dir + "/nudge-wait"); err == nil {
			t.Error("nudge-wait created under paths.Dir")
		}
	})

	t.Run("unpinned", func(t *testing.T) {
		f := newArm(t)
		f.paths = f.paths.WithState(t.TempDir())
		f.holdLock = strconv.Itoa(os.Getpid())
		f.rows = dispatchRows(t, "claude")
		if rc := f.run(Request{Wait: "0", Target: idleReq}); rc != 2 {
			t.Fatalf("rc = %d, stderr = %q", rc, f.errOut())
		}
		if !strings.HasPrefix(f.lockDir(), f.paths.Dir+"/nudge-wait/") {
			t.Errorf("lock dir %s not under paths.Dir", f.lockDir())
		}
	})
}

func TestDefaultUnreadMarkUnderCrewStateRoot(t *testing.T) {
	const (
		crew = "c1"
		br   = "feat/x"
		side = "worker:feat/x#s100-1"
		disp = `{"ts":1600000,"crew_id":"c1","from":"dispatcher:c1","to":"` + side + `","kind":"msg","body":"go"}`
	)
	deliver := func(t *testing.T, dir string) {
		t.Helper()
		var v any = map[string]any{"ts": float64(1600000), "from": "dispatcher:c1"}
		if !marks.Record(dir, crew, side, []any{v}) {
			t.Fatal("marks.Record failed")
		}
	}
	scan := func(t *testing.T, p bus.Paths, log string) string {
		t.Helper()
		if err := os.WriteFile(log, []byte(disp+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return Default(p, io.Discard, 1, func() string { return crew }).Unread(context.Background(), crew, br, side)
	}

	t.Run("pinned", func(t *testing.T) {
		common := t.TempDir()
		p, store := pinCrew(t, bus.Paths{Common: common, Dir: common + "/crew", Log: common + "/crew/events.jsonl"}, crew)
		if got := scan(t, p, store+"/events.jsonl"); got == "" {
			t.Fatal("directive read as delivered before any mark")
		}
		deliver(t, p.Dir)
		if got := scan(t, p, store+"/events.jsonl"); got == "" {
			t.Error("a mark under paths.Dir was read for a pinned crew")
		}
		deliver(t, store)
		if got := scan(t, p, store+"/events.jsonl"); got != "" {
			t.Errorf("scan = %q, want the store-dir mark honored", got)
		}
	})

	t.Run("unpinned", func(t *testing.T) {
		common := t.TempDir()
		p := bus.Paths{Common: common, Dir: common + "/crew", Log: common + "/crew/events.jsonl"}.WithState(t.TempDir())
		if err := os.MkdirAll(p.Dir, 0o755); err != nil {
			t.Fatal(err)
		}
		deliver(t, p.Dir)
		if got := scan(t, p, p.Log); got != "" {
			t.Errorf("scan = %q, want the paths.Dir mark honored", got)
		}
	})
}
