// Package register is `crew register` and `crew deregister`: a dispatcher
// claiming, and releasing, its crew directory on the repo bus.
//
// The pid and pane files and `$dir/pidfile.log` are shared with the readers in
// crews, adopt and dispatch, so their text is a contract: the pid is written
// verbatim, whatever the caller passed.
package register

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/crews"
)

const exitFailure = 1

// Options is everything Run reads beyond the bus.
type Options struct {
	// CrewID is `_crew_id`: the cwd worktree's WORKER_TASK.md crew_id, then
	// $CREW_ID. "" when neither names one.
	CrewID func() string
	// Probes are the pid reads: liveness and the owner-pid walk.
	Probes crews.Probes
	// Now is the clock the pidfile.log line stamps and the recycle check reads.
	Now func() time.Time
	// PID and PPID are `$$` and `$PPID`. PPID is the bash caller's parent only
	// because crew.sh's delegation arm `exec`s this binary, so the process
	// that ran crew.sh is the one whose parent this is.
	PID, PPID int
	// Pwd is `$PWD`.
	Pwd string
	// Args is `ps -o args= -p <pid>`.
	Args func(pid int) string
	// Getenv reads TMUX_PANE.
	Getenv func(string) string
}

// Run is the `register | deregister)` arm. sub is "register" or "deregister";
// args are the arm's positional arguments after it.
func Run(sub string, args []string, paths bus.Paths, stderr io.Writer, o Options) int {
	crew := o.CrewID()
	if crew == "" {
		say(stderr, "crew: CREW_ID unset and no WORKER_TASK.md crew_id — run 'crew crews' to find this repo's crews, 'crew adopt <id>' to re-attach, or 'crew new' to start one\n")
		return exitFailure
	}
	// deregister RemoveAlls the crew dir, so an unvalidated `..` removes the bus.
	if !bus.ValidCrewID(crew) {
		say(stderr, "crew: invalid crew id — expected only letters, digits, '.', '_' and '-'\n")
		return exitFailure
	}

	cdir := paths.CrewDir(crew)
	pidfile := cdir + "/pid"
	epid, _ := crews.PidFileText(pidfile)
	live := func() bool {
		a := crews.PidAlive(o.Probes, o.Now(), pidfile, epid)
		return a != nil && *a
	}
	caller := func() crews.Caller {
		return crews.Caller{Now: o.Now(), PID: o.PID, PPID: o.PPID, Pwd: o.Pwd, By: o.Args(o.PPID)}
	}
	logLine := func(action, outcome, newPID string) {
		crews.PidfileLog(paths, caller(), action, outcome, crew, epid, newPID)
	}

	if sub == "register" {
		pid := ""
		if len(args) > 0 {
			pid = args[0]
		}
		if pid == "" {
			pid = strconv.Itoa(o.Probes.OwnerPID(o.PPID))
		}
		if epid != pid && live() {
			logLine("register", "refused", pid)
			say(stderr, "crew: crew '%s' still has a live dispatcher (pid %s) — 'crew new' starts your own crew; if that pid is a stale reuse, recover with 'crew adopt --force %s'\n", crew, epid, crew)
			return exitFailure
		}
		if err := os.MkdirAll(cdir, 0o755); err != nil {
			say(stderr, "crew: register: %v\n", err)
			return exitFailure
		}
		if err := os.WriteFile(pidfile, []byte(pid+"\n"), 0o644); err != nil {
			say(stderr, "crew: register: %v\n", err)
			return exitFailure
		}
		logLine("register", "ok", pid)
		// The pane, not just the pid: a worker reattaching to a live dispatcher
		// has to retarget its `dispatcher_pane:` ping, and the pid alone cannot
		// name a pane. Absent outside tmux, which readers must tolerate.
		if pane := o.Getenv("TMUX_PANE"); pane != "" {
			if err := os.WriteFile(cdir+"/pane", []byte(pane+"\n"), 0o644); err != nil {
				say(stderr, "crew: register: %v\n", err)
				return exitFailure
			}
		}
		return 0
	}

	// Exit 0 on refusal: this is cleanup, and dispatcher.sh runs it from its
	// exit path. The compares are string compares as in bash, and the owner walk
	// only runs when the recorded pid is live and not already the parent.
	if live() && epid != strconv.Itoa(o.PPID) && epid != strconv.Itoa(o.Probes.OwnerPID(o.PPID)) {
		logLine("deregister", "refused", "")
		say(stderr, "crew: crew '%s' still has a live dispatcher (pid %s) that is not this caller's parent or owner — left in place\n", crew, epid)
		return 0
	}
	if isDir(cdir) {
		logLine("deregister", "removed", "")
	}
	if err := os.RemoveAll(cdir); err != nil {
		say(stderr, "crew: deregister: %v\n", err)
		return exitFailure
	}
	return 0
}

func isDir(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}

func say(w io.Writer, format string, args ...any) { _, _ = fmt.Fprintf(w, format, args...) }
