// Package probe is the side-effecting half of `crew stall-watch`: the pane,
// process, load and budget reads the bash arm made through `_pane_capture`,
// `_pane_cmd`, `_load_read`, `_top_consumers` and `_budget_refresh_maybe`, plus
// the runner for the bash libs it still calls into (budget-gate.sh, local-models.sh).
//
// The CREW_STALL_*_CMD and CREW_BUDGET_REFRESH_CMD seams keep their bash
// meaning: the text is `eval`ed there and runs under `bash -c` here, stderr is
// discarded, and stdout loses every trailing newline as `$(...)` does. Every
// child runs in its own process group and the group is killed on cancel, so a
// `sleep` under a `bash -c` seam dies with its parent.
package probe

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/proc"
	"github.com/noamsto/dispatcher/crew/internal/roster"
	"github.com/noamsto/dispatcher/crew/internal/tmux"
)

// waitDelay bounds how long Wait lingers on a pipe a grandchild still holds
// after the group kill.
const waitDelay = 2 * time.Second

// Probes are the loop's side effects. A nil-free struct so tests substitute
// any field; the pane reads and writes go through Tmux.
type Probes struct {
	Env
	Tmux          tmux.Client
	Load          func(ctx context.Context) string // "<load1> <nproc>"
	Top           func(ctx context.Context) string
	Procs         func(ctx context.Context) string // process table, one `pid ppid comm` per line
	RefreshBudget func(ctx context.Context)
	Sh            func(ctx context.Context, op string, args ...string) (out string, rc int)
	BusRows       func(path string, sinceMS int64, maxLines int) ([]any, bool) // log tail, oldest first; ok=false if not a regular file
}

// Env is what the probes read from their surroundings.
type Env struct {
	Getenv func(string) string
	Pane   string
	Bash   string // the interpreter the libs run under; "" is bash on PATH
	// BudgetLib and LocalModelsLib are the paths of the dispatch.sh-shared libs
	// the Sh probe sources; an empty one makes its ops fail to run.
	BudgetLib      string
	LocalModelsLib string
	Dir            string // the libs' cwd: the repo's git common dir
	Stderr         io.Writer
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

// ShOffset is what shScript adds to every op's verdict, so the child's own
// failures — a missing lib or a usage refusal (1), a missing file (127), a
// signal — can never read as one.
const ShOffset = 10

// ShVerdict decodes a Sh exit status: ok=false means the helper failed to run, so
// neither its status nor its output is the op's. 126 and up are the shell's own
// not-executable, not-found and 128+signo statuses.
func ShVerdict(rc int) (int, bool) {
	if rc < ShOffset || rc >= 126 {
		return 0, false
	}
	return rc - ShOffset, true
}

// shScript runs one op against its lib, $1 and $2 being the op and the lib. The
// verdict leaves as status + ShOffset.
const shScript = `op=$1 lib=$2
shift 2
rc=0
case "$op" in
budget)
	. "$lib" || exit 1
	pred=${1:-}
	shift || true
	case "$pred" in
	windows) _budget_windows "$@" || rc=$? ;;
	limit) _budget_limit "$@" || rc=$? ;;
	*)
		echo "crew: stall-watch: unknown --sh budget predicate '$pred'" >&2
		exit 1
		;;
	esac
	;;
local-model)
	. "$lib" || exit 1
	_local_entry "$("${DISPATCH_CONFIG_BIN:-dispatch-config}" 2>/dev/null)" "$1" 2>/dev/null || true
	;;
*)
	echo "crew: stall-watch: unknown --sh op '$op'" >&2
	exit 1
	;;
esac
exit $((rc + 10))`

// Default is the production probe set for one pane.
func Default(e Env) Probes {
	var (
		cores     string
		coresOnce sync.Once
	)

	return Probes{
		Env:  e,
		Tmux: tmux.Exec{},
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
		Procs: func(ctx context.Context) string {
			if c := e.Getenv("CREW_STALL_PS_CMD"); c != "" {
				text, _ := runSeam(ctx, c)
				return text
			}
			out, rc := run(ctx, nil, "ps", "-A", "-o", "pid=,ppid=,comm=")
			if rc != 0 {
				return ""
			}
			return out
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
		// The common dir, not this process's cwd: the worktree it started in
		// may be reaped while the watchdog lives.
		Sh: func(ctx context.Context, op string, args ...string) (string, int) {
			lib := e.BudgetLib
			if op == "local-model" {
				lib = e.LocalModelsLib
			}
			bash := cmp.Or(e.Bash, "bash")
			argv := append([]string{"-euo", "pipefail", "-c", shScript, "_", op, lib}, args...)
			return runIn(ctx, e.Dir, e.Stderr, bash, argv...)
		},
		BusRows: BusRows,
	}
}

// Capture is `_pane_capture`: the color or sample seam when set, else
// capture-pane. A failed colored capture is "" and not ok.
func Capture(ctx context.Context, t tmux.Client, getenv func(string) string, pane string, colored bool) (text string, ok bool) {
	cmd := getenv("CREW_STALL_SAMPLE_CMD")
	if c := getenv("CREW_STALL_COLOR_CMD"); colored && c != "" {
		cmd = c
	}
	if cmd != "" {
		var rc int
		text, rc = runSeam(ctx, cmd)
		ok = rc == 0
	} else {
		args := []string{"capture-pane", "-p", "-t", pane}
		if colored {
			args = slices.Insert(args, 1, "-e")
		}
		var err error
		text, err = t.Run(ctx, args...)
		ok = err == nil
	}
	if colored && !ok {
		text = ""
	}
	return text, ok
}

func (p Probes) Sample(ctx context.Context) (string, bool) {
	return Capture(ctx, p.Tmux, p.Getenv, p.Pane, false)
}

func (p Probes) SampleColored(ctx context.Context) string {
	text, _ := Capture(ctx, p.Tmux, p.Getenv, p.Pane, true)
	return text
}

// PaneCmd is the pane's effective command. tmux-remux restores a pane as
// `<shell> -c <cmd>`; a non-interactive shell has no job control, so the engine
// stays in the shell's process group and #{pane_current_command} names the
// shell. For a shell pane the engine is looked up among its descendants.
func (p Probes) PaneCmd(ctx context.Context) string {
	if c := p.Getenv("CREW_STALL_PROC_CMD"); c != "" {
		text, _ := runSeam(ctx, c)
		return text
	}
	rows := tmux.Output(ctx, p.Tmux, "list-panes", "-a", "-F", "#{pane_id}\t#{pane_current_command}\t#{pane_pid}")
	for row := range strings.SplitSeq(rows, "\n") {
		id, rest, _ := strings.Cut(row, "\t")
		if id != p.Pane {
			continue
		}
		cmd, pidText, _ := strings.Cut(rest, "\t")
		pid, err := strconv.Atoi(pidText)
		if !proc.IsShell(cmd) || err != nil || pid <= 1 || p.Procs == nil {
			return cmd
		}
		if engine := engineBelow(p.Procs(ctx), pid); engine != "" {
			return engine
		}
		return cmd
	}
	return ""
}

const (
	maxVisited = 4096
	maxDepth   = 32
)

// engineBelow is the basename of the first engine among root's descendants in
// a `pid ppid comm` table, breadth first. comm is the rest of the line: darwin
// prints full paths, which may contain spaces.
func engineBelow(table string, root int) string {
	type child struct {
		pid  int
		comm string
	}
	kids := map[int][]child{}
	for line := range strings.SplitSeq(table, "\n") {
		pidText, rest, _ := strings.Cut(strings.TrimSpace(line), " ")
		ppidText, comm, _ := strings.Cut(strings.TrimSpace(rest), " ")
		pid, err1 := strconv.Atoi(pidText)
		ppid, err2 := strconv.Atoi(ppidText)
		if err1 != nil || err2 != nil {
			continue
		}
		kids[ppid] = append(kids[ppid], child{pid, strings.TrimSpace(comm)})
	}
	seen := map[int]bool{root: true}
	level := []int{root}
	for range maxDepth {
		var next []int
		for _, parent := range level {
			for _, k := range kids[parent] {
				if seen[k.pid] {
					continue
				}
				// node as an arbitrary descendant (dev server, MCP/language server)
				// is weak evidence and would mask launch-not-started, dead: and EOL.
				if name := path.Base(k.comm); k.comm != "" && name != "node" && roster.IsEngineCmd(name) {
					return name
				}
				if seen[k.pid] = true; len(seen) >= maxVisited {
					return ""
				}
				next = append(next, k.pid)
			}
		}
		if len(next) == 0 {
			break
		}
		level = next
	}
	return ""
}

func (p Probes) PaneModel(ctx context.Context) string {
	return tmux.Output(ctx, p.Tmux, "show-options", "-pqv", "-t", p.Pane, "@crew_model")
}

func (p Probes) SetPaneOption(ctx context.Context, name, value string) {
	_, _ = p.Tmux.Run(ctx, "set-option", "-p", "-t", p.Pane, name, value)
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
