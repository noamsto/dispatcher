// wire.go is the real side effects: tmux, git and the `unread` helper as subprocesses,
// the bus through bus, and the pane captures through the stall probe package so the
// sampler seams keep answering tests.

package nudge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/clock"
	"github.com/noamsto/dispatcher/crew/internal/jsonv"
	"github.com/noamsto/dispatcher/crew/internal/stall/probe"
)

// PaneDeps is the real Deps: every pane, log and clock touch as a subprocess or a file
// read. `internal/stall` builds its D6 nudge from this, which is why the capture seams
// (`CREW_STALL_SAMPLE_CMD`/`CREW_STALL_COLOR_CMD`) are `_pane_capture`'s here: a test
// that scripts a frame scripts one capture, for the watchdog and the arm alike.
func PaneDeps(paths bus.Paths, stderr io.Writer, clk clock.Clock) Deps {
	probes := func(pane string) probe.Probes {
		return probe.Default(probe.Env{
			Getenv: os.Getenv, Pane: pane, CrewSH: os.Getenv("CREW_SH"), Dir: paths.Common, Stderr: stderr,
		})
	}
	return Deps{
		Capture: func(ctx context.Context, pane string, colored bool) (string, bool) {
			p := probes(pane)
			if colored {
				t := p.SampleColored(ctx)
				return t, t != ""
			}
			return p.Sample(ctx)
		},
		// tmux stays a subprocess with the bash arm's argv: the bats rows stub it on
		// PATH and count captures per call.
		Tmux: func(ctx context.Context, args ...string) (string, bool) {
			out, err := exec.CommandContext(ctx, "tmux", args...).Output()
			return trimNewlines(string(out)), err == nil
		},
		Events: func() ([]jsonv.Value, error) {
			events, err := bus.ReadEvents(paths.Log)
			if errors.Is(err, bus.ErrNoLog) {
				// `_sessions` answers `[]` when there is no log at all.
				return nil, nil
			}
			if err != nil {
				return nil, err
			}
			rows := make([]jsonv.Value, len(events))
			for i, ev := range events {
				rows[i] = ev.Raw
			}
			return rows, nil
		},
		Append: func(row jsonv.Value) error {
			return bus.Append(paths.Log, string(jsonv.Append(nil, row, jsonv.Options{})))
		},
		Sleep:  func(ctx context.Context, seconds string) error { return clk.SleepCtx(ctx, seconds, stderr) },
		Getenv: os.Getenv,
		Clock:  clk,
		Warn:   func(msg string) { _, _ = fmt.Fprintln(stderr, msg) },
	}
}

// Default wires the whole arm. The `nudge)` delegation arm is the one piece bash keeps
// (docs/crew-go-port.md) because Unread reaches `_unread_scan` through the documented
// `--sh` entry: the arm exports CREW_SH and execs here.
func Default(paths bus.Paths, stderr io.Writer, pid int, crewID func() string) Options {
	clk := clock.Clock{Now: time.Now, CrewClock: os.Getenv("CREW_CLOCK")}
	return Options{
		Deps: PaneDeps(paths, stderr, clk),
		Unread: func(ctx context.Context, crew, branch, sid string) (string, bool) {
			p := probe.Default(probe.Env{
				Getenv: os.Getenv, CrewSH: os.Getenv("CREW_SH"), Dir: paths.Common, Stderr: stderr,
			})
			out, rc := p.Sh(ctx, "unread", crew, branch, "worker:"+branch, sid, "0", "dispatcher")
			_, ok := probe.ShVerdict(rc)
			return trimNewlines(out), ok
		},
		Rows: func(maxLines int) ([]jsonv.Value, bool) { return probe.BusRows(paths.Log, 0, maxLines) },
		Toplevel: func(ctx context.Context) string {
			out, err := exec.CommandContext(ctx, "git", "rev-parse", "--show-toplevel").Output()
			if err != nil {
				return ""
			}
			return strings.TrimSpace(string(out))
		},
		Exists: func(path string) bool {
			st, err := os.Stat(path)
			return err == nil && st.Mode().IsRegular()
		},
		CrewID: crewID,
		Getenv: os.Getenv,
		PID:    pid,
	}
}

// trimNewlines is a command substitution's stdout: trailing newlines gone, nothing else.
func trimNewlines(out string) string { return strings.TrimRight(out, "\n") }
