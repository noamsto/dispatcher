// Package proc is the one seam to the process table.
package proc

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"

	"github.com/noamsto/dispatcher/crew/internal/ascii"
)

// Table is the process table. A read ps cannot answer is (zero, false).
type Table interface {
	Alive(pid int) bool             // a process with this pid exists, under any uid
	Elapsed(pid int) (int64, bool)  // the process's age in seconds
	Parent(pid int) (int, bool)     // the pid's parent
	Comm(pid int) (string, bool)    // the pid's command name
	Args(pid int) string            // the pid's command line, "" when unknown
	Cwds(ctx context.Context) []Cwd // every enumerable process with its working directory
	Kill(pid int)                   // SIGTERM, error ignored; a non-positive pid is never signalled
}

// Cwd is one process and the directory it runs in.
type Cwd struct {
	PID int
	Dir string
}

// OS is the real process table.
type OS struct{}

// IsAncestor is _is_ancestor_pid: is <pid> one of this process's ancestors?
// The walk is bounded (`while [ "$depth" -lt 32 ]`) so a parent chain that
// cycles cannot spin it, as is OwnerPID's.
func IsAncestor(t Table, pid int) bool {
	cur := os.Getpid()
	for depth := 0; depth < 32; depth++ {
		parent, ok := t.Parent(cur)
		if !ok {
			return false
		}
		if parent == pid {
			return true
		}
		cur = parent
	}
	return false
}

// Alive is _pid_alive under any uid: a failed signal is not proof of
// death — another uid's process answers EPERM, and the signal merely being
// refused proves it exists. `ps -p` is the fallback for any other error,
// exit status only, as the bash arm discards its output.
func (OS) Alive(pid int) bool {
	err := syscall.Kill(pid, 0)
	if err == nil || errors.Is(err, syscall.EPERM) {
		return true
	}
	return exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "pid=").Run() == nil
}

// Elapsed is _ps_elapsed_s: `etimes` is exact where the platform has it;
// `etime` parses the [[dd-]hh:]mm:ss macOS prints. Whitespace is stripped
// whole, like the arm's `tr -d '[:space:]'`.
func (OS) Elapsed(pid int) (int64, bool) {
	s := psField("-o", "etimes=", "-p", strconv.Itoa(pid))
	if ascii.IsDigits(s) {
		n, err := strconv.ParseInt(s, 10, 64)
		return n, err == nil
	}
	return parseEtime(psField("-o", "etime=", "-p", strconv.Itoa(pid)))
}

// parseEtime reads the [[dd-]hh:]mm:ss of ps' etime.
func parseEtime(s string) (int64, bool) {
	if s == "" || strings.ContainsFunc(s, func(c rune) bool {
		return c != '-' && c != ':' && (c < '0' || c > '9')
	}) {
		return 0, false
	}
	days, clock, found := strings.Cut(s, "-")
	if !found {
		days, clock = "0", s
	}
	parts := strings.Split(clock, ":")
	if len(parts) > 3 {
		return 0, false
	}
	var secs int64
	for _, part := range parts {
		n, err := strconv.ParseInt(part, 10, 64)
		if err != nil {
			return 0, false
		}
		secs = secs*60 + n
	}
	d := int64(0)
	if days != "" {
		var err error
		if d, err = strconv.ParseInt(days, 10, 64); err != nil {
			return 0, false
		}
	}
	return d*86400 + secs, true
}

// psField runs ps and returns the whitespace-stripped stdout, "" on failure.
func psField(args ...string) string {
	out, err := exec.Command("ps", args...).Output()
	if err != nil {
		return ""
	}
	return strings.Map(func(r rune) rune {
		if r == ' ' || (r >= '\t' && r <= '\r') {
			return -1
		}
		return r
	}, string(out))
}

// Parent is `ps -o ppid= -p <pid>`, the one parent-of spelling identical on
// BSD and GNU; any failure or guard value reads as "no parent".
func (OS) Parent(pid int) (int, bool) {
	n, err := strconv.Atoi(psField("-o", "ppid=", "-p", strconv.Itoa(pid)))
	return n, err == nil && n > 0
}

// Cwds is `_reap_procs`: CREW_REAP_PROC_CMD under `bash -c` with stderr
// dropped, else the /proc/[0-9]*/cwd readlinks. A cwd holding a tab or newline
// is skipped: a worker can create one to forge a pid for reap to SIGTERM.
func (OS) Cwds(ctx context.Context) []Cwd {
	if c := os.Getenv("CREW_REAP_PROC_CMD"); c != "" {
		out, _ := exec.CommandContext(ctx, "bash", "-c", c).Output()
		return parseCwds(string(out))
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var cwds []Cwd
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		dir, err := os.Readlink("/proc/" + e.Name() + "/cwd")
		if err != nil || !representable(dir) {
			continue
		}
		cwds = append(cwds, Cwd{pid, dir})
	}
	return cwds
}

// representable is whether a cwd survives a "pid\tcwd\n" row: not empty, and
// without a tab or newline.
func representable(dir string) bool {
	return dir != "" && !strings.ContainsAny(dir, "\t\n")
}

// parseCwds reads "pid\tcwd" rows, skipping blank ones and those with a
// non-numeric pid.
func parseCwds(out string) []Cwd {
	var cwds []Cwd
	for line := range strings.SplitSeq(out, "\n") {
		spid, dir, _ := strings.Cut(strings.Trim(line, "\t"), "\t")
		pid, err := strconv.Atoi(spid)
		if err != nil {
			continue
		}
		cwds = append(cwds, Cwd{pid, strings.Trim(dir, "\t")})
	}
	return cwds
}

func (OS) Args(pid int) string {
	out, _ := exec.Command("ps", "-o", "args=", "-p", strconv.Itoa(pid)).Output()
	return strings.TrimRight(string(out), "\n")
}

func (OS) Kill(pid int) {
	if pid > 0 {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
}

// Comm is `ps -o comm= -p <pid>`, the command name the owner walk matches
// against the shell list. Whitespace goes whole, and nothing left over means ps
// named no process: the walk breaks there and records $PPID.
func (OS) Comm(pid int) (string, bool) {
	name := psField("-o", "comm=", "-p", strconv.Itoa(pid))
	return name, name != ""
}

// shells are the command names _owner_pid climbs past. `dispatch` execs `crew
// new` through a shell, and a shell is a step on the way to the owner rather
// than the owner itself.
var shells = map[string]bool{"bash": true, "sh": true, "zsh": true, "fish": true, "dash": true, "ksh": true}

// OwnerPID is _owner_pid: the pid to record when the caller does not name one.
// The caller's parent is that shell, so the walk climbs past the shells to the
// first ancestor that is not one — the process that actually wants to own the
// crew. A pid ps cannot name, a parent of 0 or 1, or 32 steps ends the walk,
// and the direct parent is the answer it falls back to.
func OwnerPID(t Table, ppid int) int {
	for depth, pid := 0, ppid; depth < 32 && pid > 1; depth++ {
		comm, ok := t.Comm(pid)
		if !ok {
			break
		}
		if name := shellName(comm); !shells[name] {
			return pid
		}
		next, ok := t.Parent(pid)
		if !ok {
			break
		}
		pid = next
	}
	return ppid
}

// shellName is the arm's `${c##*/}` and `${c#-}`: ps prints the leading path for
// a process exec'd by absolute path, and a login shell prefixes itself with '-'.
// A name with a space in it, or one that starts with '-', is beyond `ps -o comm=`.
func shellName(comm string) string {
	n := comm
	if i := strings.LastIndex(n, "/"); i >= 0 {
		n = n[i+1:]
	}
	return strings.TrimPrefix(n, "-")
}
