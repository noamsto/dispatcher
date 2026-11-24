package nudge

import (
	"context"
	"io"
	"os"
	"path/filepath"
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

// A visiting crew's call is state-only (Dir ""): its window carries the remote
// clone's crew dir, so only @crew_id picks it, and the wait lock stays under the
// store dir rather than a path relative to the cwd.
func TestVisitingCrewDirLessWindowMatchAndWaitLock(t *testing.T) {
	const foreign = "/some/clone/crew"
	for _, tc := range []struct {
		name    string
		windows string
		found   bool
	}{
		{"crew id matches", "@7\tfeat/x\t" + foreign + "\tc1\tlead", true},
		{"other crew", "@7\tfeat/x\t" + foreign + "\tother\tlead", false},
		{"no crew id", "@7\tfeat/x\t" + foreign + "\t\tlead", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newArm(t)
			var store string
			f.paths, store = pinCrew(t, f.paths, "c1")
			f.paths.Dir = ""
			f.pane.windows = tc.windows
			if rc := f.run(Request{Target: featX}); (rc == 0) != tc.found {
				t.Fatalf("rc = %d, stderr %q, want window found %t", rc, f.errOut(), tc.found)
			}
			if !tc.found {
				return
			}
			cwd := t.TempDir()
			t.Chdir(cwd)
			f.holdLock = strconv.Itoa(os.Getpid())
			if rc := f.run(Request{Wait: "0", Target: idleReq}); rc != 2 {
				t.Fatalf("wait rc = %d, stderr %q", rc, f.errOut())
			}
			if !strings.HasPrefix(f.lockDir(), store+"/nudge-wait/") {
				t.Errorf("lock dir %s not under the store dir %s", f.lockDir(), store)
			}
			if _, err := os.Stat(filepath.Join(cwd, "nudge-wait")); err == nil {
				t.Error("nudge-wait created relative to the cwd")
			}
		})
	}
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
