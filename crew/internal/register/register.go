// Package register is `crew register` and `crew deregister`: a dispatcher
// claiming, and releasing, its crew directory — on the repo bus (layout 1) or
// under the state root (layout 2, which register alone creates).
//
// The pid and pane files and `<state>/pidfile.log` are shared with the readers
// in crews, adopt and dispatch, so their text is a contract: the pid is written
// verbatim, whatever the caller passed.
package register

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/clock"
	"github.com/noamsto/dispatcher/crew/internal/crews"
	"github.com/noamsto/dispatcher/crew/internal/filekind"
	"github.com/noamsto/dispatcher/crew/internal/proc"
)

const exitFailure = 1

// Options is everything Run reads beyond the bus.
type Options struct {
	// CrewID is `_crew_id`: the cwd worktree's WORKER_TASK.md crew_id, then
	// $CREW_ID. "" when neither names one.
	CrewID func() string
	// Proc answers liveness, the owner-pid walk and the parent's command line.
	Proc proc.Table
	// Clock.Now is the clock the pidfile.log line stamps and the recycle check reads.
	Clock clock.Clock
	// PID and PPID are `$$` and `$PPID`. PPID is the bash caller's parent only
	// because crew.sh's delegation arm `exec`s this binary, so the process
	// that ran crew.sh is the one whose parent this is.
	PID, PPID int
	// Pwd is `$PWD`.
	Pwd string
	// Getenv reads TMUX_PANE and CREW_LAYOUT.
	Getenv   func(string) string
	Hostname func() (string, error)
	// Slug is the owner/repo of the repo with the given common dir, "" when its
	// origin names none.
	Slug func(common string) string
}

// errCreate marks a failure of register's own layout-2 resolution.
type errCreate struct{ err error }

func (e *errCreate) Error() string { return e.err.Error() }

// Run is the `register | deregister)` arm. sub is "register" or "deregister";
// pid is register's optional pid argument, written verbatim.
func Run(sub, pid string, paths bus.Paths, stderr io.Writer, o Options) int {
	crew := o.CrewID()
	if crew == "" {
		say(stderr, "crew: CREW_ID unset and no WORKER_TASK.md crew_id — run 'crew crews' to find this repo's crews, 'crew adopt <id>' to re-attach, or 'crew new' to start one\n")
		return exitFailure
	}
	// deregister RemoveAlls the crew dir, so an unvalidated `..` removes the bus.
	var bc bus.Crew
	var err error
	if sub == "register" {
		bc, err = registerCrew(paths, crew, o)
	} else {
		bc, err = paths.Crew(crew)
	}
	var invalid *bus.InvalidCrewIDError
	var create *errCreate
	switch {
	case errors.As(err, &invalid):
		say(stderr, "crew: invalid crew id — expected only letters, digits, '.', '_' and '-'\n")
		return exitFailure
	case errors.As(err, &create):
		say(stderr, "crew: register: %v\n", err)
		return exitFailure
	case errors.Is(err, bus.ErrNotRepo):
		say(stderr, "crew: not in a git repo\n")
		return exitFailure
	case err != nil:
		say(stderr, "crew: %v\n", err)
		return exitFailure
	}

	cdir := bc.Dir
	pidfile := cdir + "/pid"
	epid, _ := crews.PidFileText(pidfile)
	live := func() bool {
		a := crews.PidAlive(o.Proc, o.Clock.Now(), pidfile, epid)
		return a != nil && *a
	}
	caller := func() crews.Caller {
		return crews.Caller{Now: o.Clock.Now(), PID: o.PID, PPID: o.PPID, Pwd: o.Pwd, By: o.Proc.Args(o.PPID)}
	}
	logLine := func(action, outcome, newPID string) {
		crews.PidfileLog(bc.State, caller(), action, outcome, crew, epid, newPID)
	}

	if sub == "register" {
		if pid == "" {
			pid = strconv.Itoa(proc.OwnerPID(o.Proc, o.PPID))
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
		if err := crews.WritePidStart(o.Proc, pidfile, pid); err != nil {
			say(stderr, "crew: register: %v\n", err)
			return exitFailure
		}
		if err := os.WriteFile(pidfile, []byte(pid+"\n"), 0o644); err != nil {
			_ = os.Remove(crews.PidStartFile(pidfile))
			say(stderr, "crew: register: %v\n", err)
			return exitFailure
		}
		if err := crews.WriteHome(paths, bc); err != nil {
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
	if live() && epid != strconv.Itoa(o.PPID) && epid != strconv.Itoa(proc.OwnerPID(o.Proc, o.PPID)) {
		logLine("deregister", "refused", "")
		say(stderr, "crew: crew '%s' still has a live dispatcher (pid %s) that is not this caller's parent or owner — left in place\n", crew, epid)
		return 0
	}
	if filekind.IsDir(cdir) {
		logLine("deregister", "removed", "")
	}
	if bc.Layout == 2 {
		// The dir, meta.json and log outlive the registration: they are the crew.
		for _, name := range []string{"pid", "pid.start", "pane", "launch", "relaunch"} {
			if err := os.Remove(cdir + "/" + name); err != nil && !errors.Is(err, fs.ErrNotExist) {
				say(stderr, "crew: deregister: %v\n", err)
				return exitFailure
			}
		}
		return 0
	}
	if err := os.RemoveAll(cdir); err != nil {
		say(stderr, "crew: deregister: %v\n", err)
		return exitFailure
	}
	return 0
}

// registerCrew resolves crew for register under CREW_LAYOUT. Under 2 it creates
// the layout-2 crew unless the id already resolves to one, is refused, or has a
// legacy trace in this repo, which keeps it layout 1.
func registerCrew(paths bus.Paths, crew string, o Options) (bus.Crew, error) {
	switch v := o.Getenv("CREW_LAYOUT"); v {
	case "", "1":
		return paths.Crew(crew)
	case "2":
	default:
		return bus.Crew{}, &errCreate{fmt.Errorf("CREW_LAYOUT must be 1 or 2 (got '%s')", v)}
	}
	if !bus.ValidCrewID(crew) {
		return bus.Crew{}, &bus.InvalidCrewIDError{ID: crew}
	}
	if paths.State == "" {
		return bus.Crew{}, &errCreate{errors.New("CREW_LAYOUT=2 needs XDG_STATE_HOME or HOME")}
	}
	bc, err := paths.Crew(crew)
	switch {
	case err == nil && (bc.Layout == 2 || legacy(paths, bc)):
		return bc, nil
	case err != nil && !errors.Is(err, bus.ErrNotRepo):
		return bc, err
	}
	m, err := newMeta(paths, crew, o)
	if err != nil {
		return bus.Crew{}, &errCreate{err}
	}
	if err := bus.CreateMeta(paths.State, m); err != nil && !errors.Is(err, bus.ErrMetaExists) {
		return bus.Crew{}, &errCreate{err}
	}
	return paths.Crew(crew)
}

// legacy reports whether layout-1 crew bc left a trace in this repo: its dir,
// its home record, or a row on the repo log. Deregister removes only the dir,
// so a legacy crew between register cycles has just the other two.
func legacy(paths bus.Paths, bc bus.Crew) bool {
	if filekind.IsDir(bc.Dir) || filekind.IsRegular(crews.HomePath(paths, bc.ID)) {
		return true
	}
	return slices.Contains(crews.IDs(paths), bc.ID)
}

func newMeta(paths bus.Paths, crew string, o Options) (bus.Meta, error) {
	host, err := o.Hostname()
	if err != nil {
		return bus.Meta{}, err
	}
	host, _, _ = strings.Cut(host, ".")
	m := bus.Meta{ID: crew, Created: o.Clock.Now().UnixMilli(), HomeHost: host, CreatedBy: "register"}
	if paths.Common != "" {
		tag, err := bus.RepoTag(paths.Common)
		if err != nil {
			return bus.Meta{}, err
		}
		m.HomeRepo = &tag
		m.HomeSlug = o.Slug(paths.Common)
	}
	return m, nil
}

func say(w io.Writer, format string, args ...any) { _, _ = fmt.Fprintf(w, format, args...) }
