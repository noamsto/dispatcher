// wire.go is the real side effects: tmux and git as subprocesses, the bus
// through bus, the pane captures through the stall probe package so the sampler
// seams keep answering tests, and the unread scan in process through
// `internal/unread`.

package nudge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/clock"
	"github.com/noamsto/dispatcher/crew/internal/git"
	"github.com/noamsto/dispatcher/crew/internal/stall/probe"
	"github.com/noamsto/dispatcher/crew/internal/tmux"
	"github.com/noamsto/dispatcher/crew/internal/unread"
)

// PaneDeps is the real Deps: every pane, log and clock touch as a subprocess or a file
// read. `internal/stall` builds its D6 nudge from this, which is why the capture seams
// (`CREW_STALL_SAMPLE_CMD`/`CREW_STALL_COLOR_CMD`) are `_pane_capture`'s here: a test
// that scripts a frame scripts one capture, for the watchdog and the arm alike.
func PaneDeps(paths bus.Paths, stderr io.Writer, clk clock.Clock) Deps {
	return Deps{
		Tmux: tmux.Exec{},
		Events: func() ([]any, error) {
			events, err := bus.ReadEvents(paths.Log)
			if errors.Is(err, bus.ErrNoLog) {
				// `_sessions` answers `[]` when there is no log at all.
				return nil, nil
			}
			if err != nil {
				return nil, err
			}
			rows := make([]any, len(events))
			for i, ev := range events {
				rows[i] = ev.Doc
			}
			return rows, nil
		},
		Append: func(row any) error {
			b, err := json.Marshal(row)
			if err != nil {
				return err
			}
			return bus.Append(paths.Log, string(b))
		},
		Sleep:  func(ctx context.Context, seconds string) error { return clk.SleepCtx(ctx, seconds, stderr) },
		Getenv: os.Getenv,
		Clock:  clk,
		Warn:   func(msg string) { _, _ = fmt.Fprintln(stderr, msg) },
	}
}

// Default wires the whole arm. The `nudge)` delegation arm execs here directly:
// the one helper it called back for, `_unread_scan`, moved in process to
// `internal/unread` with #968.
func Default(paths bus.Paths, stderr io.Writer, pid int, crewID func() string) Options {
	clk := clock.Clock{Now: time.Now, CrewClock: os.Getenv("CREW_CLOCK")}
	return Options{
		Deps: PaneDeps(paths, stderr, clk),
		Unread: func(_ context.Context, crew, branch, sid string) string {
			return unread.Scan(unread.Request{
				Dir: paths.Dir, Log: paths.Log, Crew: crew, Branch: branch,
				Me: "worker:" + branch, FromID: sid,
			}, unread.Dispatcher)
		},
		Rows: func(maxLines int) ([]any, bool) { return probe.BusRows(paths.Log, 0, maxLines) },
		Git:  git.Exec{},
		Exists: func(path string) bool {
			st, err := os.Stat(path)
			return err == nil && st.Mode().IsRegular()
		},
		CrewID: crewID,
		Getenv: os.Getenv,
		PID:    pid,
	}
}
