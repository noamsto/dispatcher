// Package probe is the side-effecting half of `crew stall-watch`: the pane,
// process, load and budget reads the bash arm made through `_pane_capture`,
// `_pane_cmd`, `_load_read`, `_top_consumers` and `_budget_refresh_maybe`, plus
// the runner for the bash helpers that stay in crew.sh (`stall-watch --sh`).
//
// The CREW_STALL_*_CMD and CREW_BUDGET_REFRESH_CMD seams keep their bash
// meaning: the text is `eval`ed there and runs under `bash -c` here, stderr is
// discarded, and stdout loses every trailing newline as `$(...)` does. Every
// child runs in its own process group and the group is killed on cancel, so a
// `sleep` under a `bash -c` seam dies with its parent.
package probe

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/jsonv"
)

// waitDelay bounds how long Wait lingers on a pipe a grandchild still holds
// after the group kill.
const waitDelay = 2 * time.Second

// Probes are the loop's side effects. A nil-free struct so tests substitute
// any field.
type Probes struct {
	Sample        func(ctx context.Context) (text string, alive bool) // alive = rc 0
	SampleColored func(ctx context.Context) string                    // failure = ""
	PaneCmd       func(ctx context.Context) string
	PaneModel     func(ctx context.Context) string
	Load          func(ctx context.Context) string // "<load1> <nproc>"
	Top           func(ctx context.Context) string
	RefreshBudget func(ctx context.Context)
	SetPaneOption func(ctx context.Context, name, value string)
	Sh            func(ctx context.Context, op string, args ...string) (out string, rc int)
	BusRows       func(path string, sinceMS int64, maxLines int) ([]jsonv.Value, bool) // log tail, oldest first; ok=false if not a regular file
}

// Env is what the probes read from their surroundings.
type Env struct {
	Getenv func(string) string
	Pane   string
	CrewSH string // $CREW_SH
	Dir    string // the `--sh` children's cwd: the repo's git common dir
	Stderr io.Writer
}

// run executes name with args in its own process group and returns stdout with
// its trailing newlines stripped and the exit status (-1 when the child was
// signalled or never started). stderr nil means /dev/null.
func run(ctx context.Context, stderr io.Writer, name string, args ...string) (string, int) {
	return runIn(ctx, "", stderr, name, args...)
}

// runIn is run from dir; "" is this process's cwd.
func runIn(ctx context.Context, dir string, stderr io.Writer, name string, args ...string) (string, int) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = waitDelay
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = stderr
	err := cmd.Run()
	text := strings.TrimRight(out.String(), "\n")
	if err == nil {
		return text, 0
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return text, exit.ExitCode()
	}
	return text, -1
}

func runSeam(ctx context.Context, cmd string) (string, int) {
	return run(ctx, nil, "bash", "-c", cmd)
}

// Default is the production probe set for one pane.
func Default(e Env) Probes {
	var (
		cores     string
		coresOnce sync.Once
	)
	tmux := func(ctx context.Context, args ...string) (string, int) {
		return run(ctx, nil, "tmux", args...)
	}
	// capture is `_pane_capture`: a failure yields "" with rc != 0.
	capture := func(ctx context.Context, colored bool) (string, int) {
		if colored {
			if c := e.Getenv("CREW_STALL_COLOR_CMD"); c != "" {
				return runSeam(ctx, c)
			}
		}
		if c := e.Getenv("CREW_STALL_SAMPLE_CMD"); c != "" {
			return runSeam(ctx, c)
		}
		if colored {
			return tmux(ctx, "capture-pane", "-e", "-p", "-t", e.Pane)
		}
		return tmux(ctx, "capture-pane", "-p", "-t", e.Pane)
	}

	return Probes{
		Sample: func(ctx context.Context) (string, bool) {
			text, rc := capture(ctx, false)
			return text, rc == 0
		},
		SampleColored: func(ctx context.Context) string {
			text, rc := capture(ctx, true)
			if rc != 0 {
				return ""
			}
			return text
		},
		PaneCmd: func(ctx context.Context) string {
			if c := e.Getenv("CREW_STALL_PROC_CMD"); c != "" {
				text, _ := runSeam(ctx, c)
				return text
			}
			rows, rc := tmux(ctx, "list-panes", "-a", "-F", "#{pane_id}\t#{pane_current_command}")
			if rc != 0 {
				return ""
			}
			for row := range strings.SplitSeq(rows, "\n") {
				if id, cmd, _ := strings.Cut(row, "\t"); id == e.Pane {
					return cmd
				}
			}
			return ""
		},
		PaneModel: func(ctx context.Context) string {
			text, rc := tmux(ctx, "show-options", "-pqv", "-t", e.Pane, "@crew_model")
			if rc != 0 {
				return ""
			}
			return text
		},
		Load: func(ctx context.Context) string {
			if c := e.Getenv("CREW_STALL_LOAD_CMD"); c != "" {
				text, _ := runSeam(ctx, c)
				return text
			}
			avg, err := os.ReadFile("/proc/loadavg")
			if err != nil {
				return ""
			}
			load1, _, _ := strings.Cut(string(avg), " ")
			coresOnce.Do(func() {
				cores = "1"
				if n, rc := run(ctx, nil, "nproc"); rc == 0 {
					cores = n
				}
			})
			return load1 + " " + cores
		},
		Top: func(ctx context.Context) string {
			if c := e.Getenv("CREW_STALL_TOP_CMD"); c != "" {
				text, _ := runSeam(ctx, c)
				return text
			}
			out, rc := run(ctx, nil, "ps", "-eo", "cwd=,comm=,pid=,pcpu=", "--sort=-pcpu")
			if rc != 0 {
				return ""
			}
			lines := strings.SplitN(out, "\n", 3)
			lines = lines[:min(len(lines), 2)]
			for i, l := range lines {
				lines[i] = collapseCwd(l)
			}
			return strings.TrimRight(strings.Join(lines, "\n"), "\n")
		},
		RefreshBudget: func(ctx context.Context) {
			if c := e.Getenv("CREW_BUDGET_REFRESH_CMD"); c != "" {
				runSeam(ctx, c)
				return
			}
			if _, err := exec.LookPath("refresh-budget"); err != nil {
				return
			}
			run(ctx, nil, "env", "-u", "CREW_WORKER_ID", "-u", "CREW_ID", "timeout", "120", "refresh-budget")
		},
		SetPaneOption: func(ctx context.Context, name, value string) {
			tmux(ctx, "set-option", "-p", "-t", e.Pane, name, value)
		},
		// The common dir, not this process's cwd: the worktree it started in
		// may be reaped while the watchdog lives, and crew.sh's preamble then
		// refuses with exit 1.
		Sh: func(ctx context.Context, op string, args ...string) (string, int) {
			argv := append([]string{"stall-watch", "--sh", op}, args...)
			if isExecutable(e.CrewSH) {
				return runIn(ctx, e.Dir, e.Stderr, e.CrewSH, argv...)
			}
			return runIn(ctx, e.Dir, e.Stderr, "bash", append([]string{"-euo", "pipefail", e.CrewSH}, argv...)...)
		},
		BusRows: BusRows,
	}
}

// isExecutable tells the Nix-built crew, which carries its pinned bash and
// `set -euo pipefail`, from a raw crew.sh source that needs both supplied.
func isExecutable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir() && info.Mode()&0o111 != 0
}

// cwdTail is `_top_consumers`' sed -E 's#.*/([^/]+/[^/]+) #\1 #'. sed picks the
// leftmost-longest match, which is not Go's default leftmost-first.
var cwdTail = func() *regexp.Regexp {
	re := regexp.MustCompile(`.*/([^/]+/[^/]+) `)
	re.Longest()
	return re
}()

func collapseCwd(line string) string {
	m := cwdTail.FindStringSubmatchIndex(line)
	if m == nil {
		return line
	}
	return line[:m[0]] + line[m[2]:m[3]] + " " + line[m[1]:]
}
