// Package prwatch is `crew pr-watch`: the thin bus bridge over the standalone
// `pr-watch` binary, which owns the park, the change signals and the per-PR
// cursor — and needs no crew id at all. All this adds is the post: addressed to
// this crew's dispatcher, so an armed `crew watch` wakes. Stdout stays the
// event, so the wrapper still composes.
//
// The park is a child process, and its death is part of the contract: a
// dispatcher that stops a parked watch must not leave an orphaned `pr-watch`
// polling GitHub. The bash arm inherited the signal and died around the child;
// Go forwards it to the child, waits for the child, and exits with that status.
//
// The row is a plain write through bus.Append with the arm's six keys in order.
// It needs no byte-exact body: `body` is the event text as the child printed it,
// so no reader compares key order inside it.
package prwatch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/clock"
	"github.com/noamsto/dispatcher/crew/internal/jsonv"
)

const (
	// binary is the standalone pr-watch, on PATH next to crew itself.
	binary = "pr-watch"

	exitFailure = 1
	// exitCantExec is what `command not found` left in `$?` for the arm, and the
	// status it died with under `set -e`.
	exitCantExec = 127
)

// Child is a started pr-watch: enough to forward one signal to it and to wait
// for it.
type Child interface {
	// Signal forwards one signal to the child.
	Signal(sig os.Signal) error
	// Wait blocks until the child is gone and reports the status `set -e` would
	// have ended the arm with: its own, or 128+n when it died of a signal.
	Wait() int
}

// Start runs `pr-watch` with args verbatim: stdin and stderr inherited, stdout
// captured into stdout. It is the seam tests replace, so no case needs a real
// pr-watch binary.
type Start func(ctx context.Context, args []string, stdout, stderr io.Writer) (Child, error)

// Options is everything Run reads beyond the bus: how to resolve the crew id the
// post is addressed to (`_crew_id`), the clock the row stamps — `RealMS`, jq's
// `now*1000|floor`, which stays real time under `CREW_CLOCK` — the child
// starter, and the signals forwarded to the child (nil: the process's own
// SIGTERM, SIGINT and SIGHUP).
type Options struct {
	CrewID  func() string
	Clock   clock.Clock
	Start   Start
	Signals <-chan os.Signal
}

// Run is the arm: the crew id first (before anything is run), then the park,
// then — only for a non-empty event from a child that exited 0 — the post and
// the print.
func Run(ctx context.Context, args []string, paths bus.Paths, stdout, stderr io.Writer, o Options) int {
	crew := ""
	if o.CrewID != nil {
		crew = o.CrewID()
	}
	if crew == "" {
		say(stderr, "crew: CREW_ID unset and no WORKER_TASK.md crew_id\n")
		return exitFailure
	}

	// The park. Stop signals are caught before the child starts, so one that lands
	// during the exec is forwarded rather than taken by the default action, and
	// the wait then goes on until the child is gone — because the post below is
	// exactly what a stopped watch must not do.
	sigs := o.Signals
	if sigs == nil {
		caught := make(chan os.Signal, 1)
		signal.Notify(caught, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
		defer signal.Stop(caught)
		sigs = caught
	}

	start := o.Start
	if start == nil {
		start = startChild
	}
	var out bytes.Buffer
	child, err := start(ctx, args, &out, stderr)
	if err != nil {
		// The arm had no child to run at all: `command not found`, 127, and
		// nothing posted.
		say(stderr, "crew: pr-watch: %v\n", err)
		return exitCantExec
	}
	done := make(chan int, 1)
	go func() { done <- child.Wait() }()

	var code int
	select {
	case code = <-done:
	case s := <-sigs:
		_ = child.Signal(s)
		// The child's own status, as the arm that died of the same signal would
		// have reported it — and no post, whatever the child printed.
		return <-done
	}
	if code != 0 {
		// `set -e`: the child's status ends the arm, skipping both the post and
		// the print.
		return code
	}

	// `$(…)` strips the trailing newlines; empty stdout is pr-watch's timeout
	// marker, not a failure.
	ev := strings.TrimRight(out.String(), "\n")
	if ev == "" {
		return 0
	}

	// `from` is the arm's `"pr-watch:${1:-}"`: the first argument verbatim, a
	// flag included, and the empty string when there was no argument at all.
	from := ""
	if len(args) > 0 {
		from = args[0]
	}
	if err := os.MkdirAll(paths.Dir, 0o755); err != nil {
		say(stderr, "crew: pr-watch: %v\n", err)
		return exitFailure
	}
	ts := o.Clock.RealMS()
	line := compact(jsonv.Object(
		jsonv.Member{Key: "ts", Val: jsonv.Num(float64(ts))},
		jsonv.Member{Key: "crew_id", Val: jsonv.Str(crew)},
		jsonv.Member{Key: "from", Val: jsonv.Str("pr-watch:" + from)},
		jsonv.Member{Key: "to", Val: jsonv.Str("dispatcher:" + crew)},
		jsonv.Member{Key: "kind", Val: jsonv.Str("msg")},
		jsonv.Member{Key: "body", Val: jsonv.Str(ev)},
	))
	if err := bus.Append(paths.Log, line); err != nil {
		say(stderr, "crew: pr-watch: %v\n", err)
		return exitFailure
	}
	say(stdout, "%s\n", ev)
	return 0
}

// startChild is the default Start: the `pr-watch` binary from PATH.
func startChild(ctx context.Context, args []string, stdout, stderr io.Writer) (Child, error) {
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &process{cmd: cmd}, nil
}

// process is a started child; Wait reports its status the way a shell does.
type process struct{ cmd *exec.Cmd }

func (p *process) Signal(sig os.Signal) error { return p.cmd.Process.Signal(sig) }

func (p *process) Wait() int {
	err := p.cmd.Wait()
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok {
			if ws.Signaled() {
				return 128 + int(ws.Signal())
			}
			return ws.ExitStatus()
		}
		if code := ee.ExitCode(); code >= 0 {
			return code
		}
	}
	return 1
}

// say writes to a stream whose failure is reported elsewhere (the exit status,
// or nowhere, for stderr) — main.go's helper, same reason.
func say(w io.Writer, format string, args ...any) { _, _ = fmt.Fprintf(w, format, args...) }

func compact(v jsonv.Value) string { return string(jsonv.Append(nil, v, jsonv.Options{})) }
