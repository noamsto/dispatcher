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
		if want := "(pid " + f.holdLock + ")"; !strings.Contains(f.errOut(), want) {
			t.Errorf("wait never saw the held lock under %s: stdout %q, stderr %q", store, f.out(), f.errOut())
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

// A visiting crew's call is state-only (Dir ""): there is no checkout to compare a
// window's dir with, and any process on the tmux server can stamp @crew_id, so a
// window counts only when the crew's own dispatch, resume or window row for its
// branch names its tmux window id (else, for a row without one, its dir).
// The wait lock stays under the store dir rather than a path relative to the cwd.
func TestVisitingCrewWindowIsBoundToItsDispatchRow(t *testing.T) {
	const clone = "/some/clone"
	// srv is the live tmux server's identity (pid:start_time), the sixth field the
	// arm's list-windows prints.
	const srv = "111:222"
	row := `{"ts":1,"crew_id":"c1","kind":"dispatch","branch":"feat/x","repo":"` + clone + `"}`
	winRow := `{"ts":2,"crew_id":"c1","kind":"window","branch":"feat/x","repo":"` + clone + `","window_id":"@7","tmux_server":"` + srv + `"}`
	resumeRow := `{"ts":3,"crew_id":"c1","kind":"resume","branch":"feat/x","repo":"` + clone + `","window_id":"@8","tmux_server":"` + srv + `"}`
	real := "@7\tfeat/x\t" + clone + "/crew\tc1\tlead\t" + srv
	// A worker of another crew stamps its own window with this crew's id and branch.
	forged := "@9\tfeat/x\t/other/clone/crew\tc1\tevil\t" + srv
	copied := "@9\tfeat/x\t" + clone + "/crew\tc1\tevil\t" + srv
	for _, tc := range []struct {
		name    string
		rows    []string
		windows string
		found   bool
		err     string
	}{
		{"id and row dir match", []string{row}, real, true, ""},
		{"other crew", []string{row}, "@7\tfeat/x\t" + clone + "/crew\tother\tlead", false, "no live pane"},
		{"no crew id", []string{row}, "@7\tfeat/x\t" + clone + "/crew\t\tlead", false, "no live pane"},
		{"forged id, its own dir", []string{row}, forged, false, "no live pane"},
		{"forged id listed before the real window", []string{row}, forged + "\n" + real, true, ""},
		{"forged id, no dispatch row", nil, real, false, "no worker matches"},
		{"forged id, row of another crew", []string{strings.Replace(row, `"c1"`, `"c2"`, 1)}, real, false, "no worker matches"},
		{"forged id, row of another branch", []string{strings.Replace(row, "feat/x", "feat/y", 1)}, real, false, "no worker matches"},
		// The real window closed and a worker copied every stamp of it, dir included.
		{"recorded window id", []string{row, winRow}, real, true, ""},
		{"copied stamps after the real window closed", []string{row, winRow}, copied, false, "no live pane"},
		{"copied stamps beside the real window, recorded id", []string{row, winRow}, copied + "\n" + real, true, ""},
		{"a dispatch row whose window is not open yet binds no window", []string{strings.Replace(row, `"kind":"dispatch"`, `"kind":"dispatch","window_id":null`, 1)}, copied, false, "no live pane"},
		{"a resume row's id replaces the window row's", []string{row, winRow, resumeRow}, real, false, "no live pane"},
		{"a legacy resume row keeps the dir binding", []string{row, winRow, strings.Replace(resumeRow, `,"window_id":"@8"`, "", 1)}, real, true, ""},
		// A row from before the tmux server restarted names an id this server handed
		// to somebody else's window.
		{"a row of another tmux server does not bind", []string{row, strings.Replace(winRow, srv, "999:888", 1)}, real, false, "no live pane"},
		{"a window on the server the row recorded binds", []string{row, strings.Replace(winRow, srv, "999:888", 1)}, "@7\tfeat/x\t" + clone + "/crew\tc1\tlead\t999:888", true, ""},
		// The rows after a torn line could move the branch to another clone.
		{"torn log line before a newer row", []string{row, "{\"ts\":", strings.Replace(row, clone, "/moved", 1)}, real, false, "cannot read the crew log"},
		{"copied stamps beside the real window", []string{row}, copied + "\n" + real, false, "ambiguous branch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newArm(t)
			var store string
			f.paths, store = pinCrew(t, f.paths, "c1")
			f.paths.Dir = ""
			if len(tc.rows) > 0 {
				if err := os.WriteFile(filepath.Join(store, "events.jsonl"), []byte(strings.Join(tc.rows, "\n")+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			f.pane.windows = tc.windows
			rc := f.run(Request{Target: featX})
			if tc.found && rc != 0 {
				t.Fatalf("rc = %d, stderr %q, want the window found", rc, f.errOut())
			}
			if !tc.found && (rc == 0 || !strings.Contains(f.errOut(), tc.err)) {
				t.Fatalf("rc = %d, stderr %q, want a refusal naming %q", rc, f.errOut(), tc.err)
			}
		})
	}
	t.Run("wait lock", func(t *testing.T) {
		f := newArm(t)
		var store string
		f.paths, store = pinCrew(t, f.paths, "c1")
		f.paths.Dir = ""
		if err := os.WriteFile(filepath.Join(store, "events.jsonl"), []byte(row+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		f.pane.windows = real
		f.holdLock = strconv.Itoa(os.Getpid())
		cwd := t.TempDir()
		t.Chdir(cwd)
		if rc := f.run(Request{Wait: "0", Target: idleReq}); rc != 2 {
			t.Fatalf("wait rc = %d, stderr %q", rc, f.errOut())
		}
		if want := "(pid " + f.holdLock + ")"; !strings.Contains(f.errOut(), want) {
			t.Errorf("wait never saw the held lock under %s: stdout %q, stderr %q", store, f.out(), f.errOut())
		}
		if _, err := os.Stat(filepath.Join(cwd, "nudge-wait")); err == nil {
			t.Error("nudge-wait created relative to the cwd")
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
