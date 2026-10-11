// Package reap is `crew reap` and the release of a finished worker's tmux
// windows (`_release_windows`), which reap's idle-release pass and
// stall-watch's finished-worker release share.
//
// Every side effect the bash arm made — tmux, gh, gtrash, /proc, signals, the
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
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/stall/probe"
)

// ReleaseGrace is how long, in seconds, a finished worker's window may stay
// open before it is released. The session is terminal (`reply` refuses it,
// nothing types into it), so the window only buys a human a glance at the final
// summary and a late dispatcher directive time to re-open the session.
const ReleaseGrace = 300

// Probes are the reap path's side effects. A nil-free struct so tests
// substitute any field.
type Probes struct {
	// Tmux is `$(tmux … 2>/dev/null || true)`: stderr dropped, a failure is "",
	// trailing newlines stripped.
	Tmux func(ctx context.Context, args ...string) string
	// TmuxRun is `tmux … 2>/dev/null || true`, output discarded.
	TmuxRun func(ctx context.Context, args ...string)
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
	// Procs lists "pid\tcwd\n" for every enumerable process: CREW_REAP_PROC_CMD
	// under `bash -c` with stderr dropped, else the /proc/[0-9]*/cwd readlinks.
	Procs func(ctx context.Context) string
	// Ancestors is this process and its PPid chain.
	Ancestors func() map[int]bool
	// Kill sends SIGTERM and ignores the error. A non-positive pid is never
	// signalled: it would reach the caller's own process group or every process.
	Kill func(pid int)
	// Capture is `_pane_capture`: the pane's text, or its ANSI-colored capture,
	// through the CREW_STALL_SAMPLE_CMD / CREW_STALL_COLOR_CMD seams.
	Capture func(ctx context.Context, pane string, colored bool) string
	Sleep   func(d time.Duration)
	Now     func() time.Time
	// Append is `_bus_append`.
	Append func(path, line string) error
}

// DefaultProbes is the production probe set.
func DefaultProbes() Probes {
	return Probes{
		Tmux: func(ctx context.Context, args ...string) string {
			out, _ := output(ctx, "tmux", args, false)
			return out
		},
		TmuxRun: func(ctx context.Context, args ...string) {
			_ = exec.CommandContext(ctx, "tmux", args...).Run()
		},
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
		Procs:     procs,
		Ancestors: ancestors,
		Kill: func(pid int) {
			if pid > 0 {
				_ = syscall.Kill(pid, syscall.SIGTERM)
			}
		},
		Capture: func(ctx context.Context, pane string, colored bool) string {
			p := probe.Default(probe.Env{Getenv: os.Getenv, Pane: pane})
			if colored {
				return p.SampleColored(ctx)
			}
			text, _ := p.Sample(ctx)
			return text
		},
		Sleep:  time.Sleep,
		Now:    time.Now,
		Append: bus.Append,
	}
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

// procs is `_reap_procs`.
func procs(ctx context.Context) string {
	if c := os.Getenv("CREW_REAP_PROC_CMD"); c != "" {
		out, _ := output(ctx, "bash", []string{"-c", c}, false)
		if out == "" {
			return ""
		}
		return out + "\n"
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return ""
	}
	var b strings.Builder
	for _, e := range entries {
		pid := e.Name()
		if _, err := strconv.Atoi(pid); err != nil {
			continue
		}
		cwd, err := os.Readlink("/proc/" + pid + "/cwd")
		if err != nil {
			continue
		}
		b.WriteString(procLine(pid, cwd))
	}
	return b.String()
}

// procLine is one "pid\tcwd\n" row of procs, or "" for an empty cwd or one
// holding a tab or newline: a worker can create such a directory, and the row
// would forge a pid of its choosing for reap to SIGTERM.
func procLine(pid, cwd string) string {
	if cwd == "" || strings.ContainsAny(cwd, "\t\n") {
		return ""
	}
	return pid + "\t" + cwd + "\n"
}

// ancestors is the arm's `ancestors="$$ $BASHPID"` plus the PPid walk, which
// stops at pid 1 or a status it cannot read.
func ancestors() map[int]bool {
	p := os.Getpid()
	set := map[int]bool{p: true}
	for p > 1 {
		data, err := os.ReadFile("/proc/" + strconv.Itoa(p) + "/status")
		if err != nil {
			break
		}
		pp := 0
		for line := range strings.SplitSeq(string(data), "\n") {
			if v, ok := strings.CutPrefix(line, "PPid:"); ok {
				pp, _ = strconv.Atoi(strings.TrimSpace(v))
				break
			}
		}
		if pp == 0 {
			break
		}
		set[pp] = true
		p = pp
	}
	return set
}
