// Package reap is `crew reap` and the release of a finished worker's tmux
// windows (`_release_windows`), which reap's idle-release pass and
// stall-watch's finished-worker release share.
//
// Every side effect the bash arm made — tmux, gh, gtrash, processes, the
// pane captures, sleeps, the clock and the bus append — goes through Probes, so
// a test fakes them without a tmux server or a network. The jq programs the
// arm ran live in the .jq files next to this one, run verbatim through jqrun
// with each patch documented in the file's header.
package reap

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/git"
	"github.com/noamsto/dispatcher/crew/internal/proc"
	"github.com/noamsto/dispatcher/crew/internal/tmux"
)

// ReleaseGrace is how long, in seconds, a finished worker's window may stay
// open before it is released. The session is terminal (`reply` refuses it,
// nothing types into it), so the window only buys a human a glance at the final
// summary and a late dispatcher directive time to re-open the session.
const ReleaseGrace = 300

// Probes are the reap path's side effects. A nil-free struct so tests
// substitute any field.
type Probes struct {
	Tmux tmux.Client
	// Git runs the plain git reads; the guarded wtgit and gitguard calls stay apart.
	Git git.Client
	// Getenv reads the CREW_STALL_*_CMD capture seams.
	Getenv func(string) string
	// Gh is `gh …` with stderr dropped and trailing newlines stripped.
	Gh func(ctx context.Context, args ...string) (string, error)
	// GhAll is `gh … 2>&1`, stdout and stderr combined.
	GhAll func(ctx context.Context, args ...string) (string, error)
	// GhQuiet is `gh … >/dev/null 2>&1`: only the status is read.
	GhQuiet func(ctx context.Context, args ...string) error
	// HaveGh is `command -v gh`.
	HaveGh func() bool
	// Gtrash is `gtrash put <path>` with everything discarded.
	Gtrash func(ctx context.Context, path string)
	// Proc lists the processes by cwd, walks this process's ancestry and
	// signals the leftovers.
	Proc  proc.Table
	Sleep func(d time.Duration)
	Now   func() time.Time
	// Append is `_bus_append`.
	Append func(path, line string) error
}

// DefaultProbes is the production probe set.
func DefaultProbes() Probes {
	return Probes{
		Tmux: tmux.Exec{},
		Git:  git.Exec{},
		Gh: func(ctx context.Context, args ...string) (string, error) {
			return output(ctx, "gh", args, false)
		},
		GhAll: func(ctx context.Context, args ...string) (string, error) {
			return output(ctx, "gh", args, true)
		},
		GhQuiet: func(ctx context.Context, args ...string) error {
			return exec.CommandContext(ctx, "gh", args...).Run()
		},
		HaveGh: func() bool {
			_, err := exec.LookPath("gh")
			return err == nil
		},
		Gtrash: func(ctx context.Context, path string) {
			_ = exec.CommandContext(ctx, "gtrash", "put", path).Run()
		},
		Proc:   proc.OS{},
		Sleep:  time.Sleep,
		Now:    time.Now,
		Append: bus.Append,
		Getenv: os.Getenv,
	}
}

// tmuxOut is `$(tmux … 2>/dev/null || true)`: whatever tmux printed, failed or not.
func (p Probes) tmuxOut(ctx context.Context, args ...string) string {
	out, _ := p.Tmux.Run(ctx, args...)
	return out
}

// output runs name and returns its stdout, trailing newlines stripped, with
// stderr dropped or merged into the result. Output is returned alongside a
// failure: `$(cmd || true)` keeps whatever the command printed.
func output(ctx context.Context, name string, args []string, merge bool) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	if merge {
		cmd.Stderr = &buf
	}
	err := cmd.Run()
	return strings.TrimRight(buf.String(), "\n"), err
}
