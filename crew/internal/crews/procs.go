package crews

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Probes are the process reads the bash arm makes through `kill -0`, `ps`
// and `stat`, injected so a test controls liveness and the ancestor walk.
type Probes struct {
	// Alive is _pid_alive: a process with this pid exists, under any uid.
	Alive func(pid int) bool
	// Elapsed is _ps_elapsed_s: the process's age in seconds, false when ps
	// cannot say.
	Elapsed func(pid int) (int64, bool)
	// Mtime is _file_mtime_s: the file's mtime in epoch seconds, false when
	// there is no readable regular file there.
	Mtime func(path string) (int64, bool)
	// Parent is `ps -o ppid= -p`: the pid's parent, false when ps cannot say
	// or names none (the walk's `'' | *[!0-9]* | 0` guard).
	Parent func(pid int) (int, bool)
}

// DefaultProbes are the real reads.
func DefaultProbes() Probes {
	return Probes{Alive: pidAlive, Elapsed: psElapsedS, Mtime: fileMtimeS, Parent: psParent}
}

// RecordedLive is _recorded_pid_live: <pid> can still be the dispatcher
// <pidfile> records — live and not a later-recycled pid.
func (p Probes) RecordedLive(now time.Time, pid int, pidfile string) bool {
	return p.Alive(pid) && !p.recycled(now, pid, pidfile)
}

// recycled is _pid_recycled: the process now holding <pid> started after
// <pidfile> was last written (plus 2s of slack for ps' second granularity),
// so it cannot be the dispatcher the file records. An unreadable timestamp
// says not-recycled, so a live process is never judged dead.
func (p Probes) recycled(now time.Time, pid int, pidfile string) bool {
	elapsed, ok1 := p.Elapsed(pid)
	mtime, ok2 := p.Mtime(pidfile)
	if !ok1 || !ok2 {
		return false
	}
	return now.Unix()-elapsed > mtime+2
}

// isAncestor is _is_ancestor_pid: is <pid> one of this process's ancestors?
// Bounded walk, like the bash one.
func (p Probes) isAncestor(pid int) bool {
	cur := os.Getpid()
	for depth := 0; depth < 32; depth++ {
		parent, ok := p.Parent(cur)
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

// pidAlive is _pid_alive under any uid: a failed signal is not proof of
// death — another uid's process answers EPERM, and the signal merely being
// refused proves it exists. `ps -p` is the fallback for any other error,
// exit status only, as the bash arm discards its output.
func pidAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	if err == nil || errors.Is(err, syscall.EPERM) {
		return true
	}
	return exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "pid=").Run() == nil
}

// psElapsedS is _ps_elapsed_s: `etimes` is exact where the platform has it;
// `etime` parses the [[dd-]hh:]mm:ss macOS prints. Whitespace is stripped
// whole, like the arm's `tr -d '[:space:]'`.
func psElapsedS(pid int) (int64, bool) {
	s := psField("-o", "etimes=", "-p", strconv.Itoa(pid))
	if isDigits(s) {
		n, err := strconv.ParseInt(s, 10, 64)
		return n, err == nil
	}
	s = psField("-o", "etime=", "-p", strconv.Itoa(pid))
	if s == "" || strings.ContainsFunc(s, func(c rune) bool {
		return c != '-' && c != ':' && (c < '0' || c > '9')
	}) {
		return 0, false
	}
	days := int64(0)
	if i := strings.IndexByte(s, '-'); i >= 0 {
		if ds := s[:i]; ds != "" {
			n, err := strconv.ParseInt(ds, 10, 64)
			if err != nil {
				return 0, false
			}
			days = n
		}
		s = s[i+1:]
	}
	var h, m, sec string
	switch head, rest, found := strings.Cut(s, ":"); {
	case strings.Contains(rest, ":"):
		h = head
		m, sec, _ = strings.Cut(rest, ":")
	case found:
		h, m, sec = "0", head, rest
	default:
		h, m, sec = "0", "0", s
	}
	hn, err1 := strconv.ParseInt(h, 10, 64)
	mn, err2 := strconv.ParseInt(m, 10, 64)
	sn, err3 := strconv.ParseInt(sec, 10, 64)
	if err1 != nil || err2 != nil || err3 != nil {
		return 0, false
	}
	return days*86400 + hn*3600 + mn*60 + sn, true
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

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// fileMtimeS is _file_mtime_s behind _pid_recycled's `[ -f ]`: a missing or
// non-regular file has no readable mtime.
func fileMtimeS(path string) (int64, bool) {
	st, err := os.Stat(path)
	if err != nil || !st.Mode().IsRegular() {
		return 0, false
	}
	return st.ModTime().Unix(), true
}

// psParent is `ps -o ppid= -p <pid>`: the one parent-of spelling identical
// on BSD and GNU. Any failure or guard value reads as "no parent".
func psParent(pid int) (int, bool) {
	out, err := exec.Command("ps", "-o", "ppid=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}
