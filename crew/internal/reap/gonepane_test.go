package reap

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/proc/proctest"
	"github.com/noamsto/dispatcher/crew/internal/tmux/tmuxtest"
)

const otherWindow = "@9\tother\t/elsewhere"

// exitedRows are the rows this run appended that mark a session exited.
func (f *fx) exitedRows() []map[string]any {
	f.t.Helper()
	var rows []map[string]any
	for _, line := range f.appended {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			f.t.Fatal(err)
		}
		if body, _ := m["body"].(map[string]any); body["state"] == "exited" {
			rows = append(rows, m)
		}
	}
	return rows
}

func gonePaneFx(t *testing.T, state, prState string) *fx {
	t.Helper()
	f := newFx(t)
	f.bus(status(state, prURL, 0))
	f.tmux[winListKey] = otherWindow
	f.portBMerged("", f.portBTip())
	f.portBGh(prState)
	// The fake append only records; the same-run fold reads the bus file.
	f.appendFail = func(line string) error {
		fh, err := os.OpenFile(f.paths.Log, os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		if _, err := fh.WriteString(line + "\n"); err != nil {
			_ = fh.Close()
			return err
		}
		return fh.Close()
	}
	return f
}

func TestGonePaneSessionExited(t *testing.T) {
	t.Run("merged PR reclaimed", func(t *testing.T) {
		f := gonePaneFx(t, "working", "MERGED")
		f.portBOK(Request{})
		rows := f.exitedRows()
		if len(rows) != 1 {
			t.Fatalf("rows %v", f.appended)
		}
		body := rows[0]["body"].(map[string]any)
		if body["source"] != "reap" || rows[0]["from"] != "worker:feat/r#s1" || rows[0]["crew_id"] != "c1" || rows[0]["kind"] != "status" ||
			body["detail"] != "engine pane and window gone (no window stamped for feat/r, no engine pane in "+f.wt+", stall-watch gone)" {
			t.Fatalf("row %v", rows[0])
		}
		f.portBWantAbsent(f.wt)
		if !strings.Contains(f.stdout.String(), "reaped feat/r") {
			t.Fatalf("stdout %q", f.stdout.String())
		}
	})

	for _, tc := range []struct {
		name  string
		setup func(f *fx)
		req   Request
	}{
		{"stamped window", func(f *fx) { f.tmux[winListKey] = otherWindow + "\n@2\t" + rb + "\t" + f.paths.Dir }, Request{}},
		{"engine pane in worktree", func(f *fx) { f.tmux[panesKey] = "@3\t%7\tclaude\t" + f.wt }, Request{}},
		{"stall-watch alive", func(f *fx) {
			f.procs.Procs = map[int]proctest.Proc{
				41: {Cwd: "/", Args: "crew stall-watch worker:feat/r#s1 --pane %7"},
			}
		}, Request{}},
		{"remote dispatch", func(f *fx) {
			f.bus(`{"ts":1,"crew_id":"c1","kind":"dispatch","branch":"`+rb+`","host":"mini","remote":true}`, status("working", prURL, 0))
		}, Request{}},
		{"dry run", func(*fx) {}, Request{DryRun: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := gonePaneFx(t, "working", "MERGED")
			tc.setup(f)
			f.reap(f.main, tc.req)
			if len(f.exitedRows()) != 0 {
				t.Fatalf("marked exited: %v", f.appended)
			}
			f.portBWantPresent(f.wt)
			if tc.req.DryRun && !strings.Contains(f.stdout.String(), "would mark feat/r exited (engine pane and window gone)") {
				t.Fatalf("stdout %q", f.stdout.String())
			}
		})
	}

	t.Run("role stall-watch does not match", func(t *testing.T) {
		f := gonePaneFx(t, "working", "MERGED")
		f.procs.Procs = map[int]proctest.Proc{41: {Cwd: "/", Args: "crew stall-watch role:feat/r:reviewer --pane %7"}}
		f.portBOK(Request{})
		if len(f.exitedRows()) != 1 {
			t.Fatalf("rows %v", f.appended)
		}
	})

	t.Run("tmux error", func(t *testing.T) {
		f := gonePaneFx(t, "working", "MERGED")
		o := f.opts()
		o.Probes.Tmux = &tmuxtest.Fake{Func: func(args []string) (string, error) {
			if args[0] == "list-panes" {
				return "", errors.New("no server")
			}
			return f.tmux[strings.Join(args, " ")], nil
		}}
		Run(context.Background(), Request{}, f.paths, f.main, &f.stdout, &f.stderr, o)
		if len(f.exitedRows()) != 0 {
			t.Fatalf("marked exited: %v", f.appended)
		}
	})

	t.Run("no windows listed", func(t *testing.T) {
		f := gonePaneFx(t, "working", "MERGED")
		f.tmux[winListKey] = ""
		f.reap(f.main, Request{})
		if len(f.exitedRows()) != 0 {
			t.Fatalf("marked exited: %v", f.appended)
		}
	})

	t.Run("open PR kept, not stale", func(t *testing.T) {
		f := gonePaneFx(t, "working", "OPEN")
		f.portBOK(Request{})
		if len(f.exitedRows()) != 1 {
			t.Fatalf("rows %v", f.appended)
		}
		if !strings.Contains(f.stdout.String(), "keeping feat/r") || !strings.Contains(f.stdout.String(), "PR OPEN") {
			t.Fatalf("stdout %q", f.stdout.String())
		}
		if len(f.staleRows()) != 0 {
			t.Fatalf("stale rows %v", f.appended)
		}
		f.portBWantPresent(f.wt)
	})
}

func TestGonePaneAppendFailureIsReported(t *testing.T) {
	f := gonePaneFx(t, "working", "MERGED")
	f.appendFail = func(string) error { return errors.New("disk full") }
	f.portBOK(Request{})
	if got := f.stderr.String(); !strings.Contains(got, "crew reap: cannot mark feat/r exited: disk full\n") {
		t.Fatalf("stderr %q", got)
	}
	if len(f.exitedRows()) != 0 {
		t.Fatalf("rows %v", f.appended)
	}
}
