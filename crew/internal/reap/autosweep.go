package reap

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/lock"
)

// autosweepKeep is how many lines of autosweep.log the async sweep keeps.
const autosweepKeep = 200

// storeDir is the machine-global ratings store, `${XDG_DATA_HOME:-$HOME/.local/share}/crew`.
func storeDir(getenv func(string) string) string {
	base := getenv("XDG_DATA_HOME")
	if base == "" {
		base = getenv("HOME") + "/.local/share"
	}
	return base + "/crew"
}

// stamp is `date -u +%Y-%m-%dT%H:%M:%SZ`.
func (o Options) stamp() string {
	return o.Probes.Clock.Now().UTC().Format("2006-01-02T15:04:05Z")
}

// autosweep is the arm's ratings-sweep hook. CREW_RATE_AUTOSWEEP: unset/1 is a
// detached async sweep, sync runs it in the foreground (an operator watching
// or diagnosing a skipped sweep), 0 is none at all (test isolation, not a
// supported production switch). An unrecognised value sweeps, loudly: reading
// a typo as "off" would silently disable the sweep. The sweep never changes
// reap's status; only a store dir the sync sweep cannot create aborts, as the
// arm's `set -e` did.
func (r *run) autosweep() int {
	mode := r.o.Getenv("CREW_RATE_AUTOSWEEP")
	if mode == "" {
		mode = "1"
	}
	switch mode {
	case "0", "1", "sync":
	default:
		_, _ = fmt.Fprintf(r.stderr, "crew reap: CREW_RATE_AUTOSWEEP='%s' is not 0, 1 or sync — sweeping anyway\n", mode)
		mode = "1"
	}
	// --discard acts on one branch only.
	if r.discard != "" {
		mode = "0"
	}
	switch {
	case mode == "0":
	case r.dry:
		r.note("dry run — skipping ratings sweep")
	case mode == "sync":
		return r.sweepSync()
	default:
		r.sweepAsync()
	}
	return 0
}

// sweepSync is `_rate_autosweep` in the foreground. The store, and so this
// lock, is machine-global: the skip is cross-repo, and self-healing — the bus
// is append-only, so the next reap for the skipped repo backfills it in full.
func (r *run) sweepSync() int {
	store := storeDir(r.o.Getenv)
	// Without it the lock cannot be taken, and the sweep would never run on
	// exactly the machines that have never run `crew rate` by hand.
	if err := os.MkdirAll(store, 0o777); err != nil {
		_, _ = fmt.Fprintf(r.stderr, "crew reap: %v\n", err)
		return 1
	}
	ld := store + "/ratings.sweep.lock.d"
	if !lock.Acquire(ld, strconv.Itoa(r.o.PID)) {
		r.note("a ratings sweep is already running — skipped")
		return 0
	}
	r.clean.hold(ld)
	r.o.RateRun(nil, r.paths, r.trusted, r.stdout, r.stderr)
	r.clean.release(ld)
	return 0
}

// sweepAsync starts `reap-autosweep` detached from the trusted cwd, stdin
// /dev/null and its output appended to autosweep.log, so a skipped or failed
// sweep leaves a trace instead of vanishing. reap does not wait for it.
func (r *run) sweepAsync() {
	store := storeDir(r.o.Getenv)
	if err := os.MkdirAll(store, 0o777); err != nil {
		_, _ = fmt.Fprintf(r.stderr, "crew reap: %v\n", err)
		return
	}
	logf, err := os.OpenFile(store+"/autosweep.log", os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o666)
	if err != nil {
		_, _ = fmt.Fprintf(r.stderr, "crew reap: %v\n", err)
		return
	}
	defer func() { _ = logf.Close() }()
	null, err := os.Open(os.DevNull)
	if err != nil {
		_, _ = fmt.Fprintf(r.stderr, "crew reap: %v\n", err)
		return
	}
	defer func() { _ = null.Close() }()
	cmd := exec.Command(r.o.Self, "reap-autosweep")
	cmd.Dir = r.trusted
	cmd.Stdin, cmd.Stdout, cmd.Stderr = null, logf, logf
	if err := cmd.Start(); err != nil {
		_, _ = fmt.Fprintf(r.stderr, "crew reap: %v\n", err)
		return
	}
	_ = cmd.Process.Release()
}

// RunAutosweep is the detached child `sweepAsync` starts: `_rate_autosweep
// async`, whose stdout and stderr are already the autosweep log. It ignores
// SIGHUP, as the arm's subshell did (it trapped HUP to the empty action), and
// SIGINT and SIGQUIT, as bash ignores them for an asynchronous command
// without job control. Its status is never read.
func RunAutosweep(paths bus.Paths, cwd string, o Options) int {
	signal.Ignore(syscall.SIGHUP, syscall.SIGINT, syscall.SIGQUIT)
	store := storeDir(o.Getenv)
	if err := os.MkdirAll(store, 0o777); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "crew reap: %v\n", err)
		return 1
	}
	sweeplog := store + "/autosweep.log"
	_, _ = fmt.Fprintf(os.Stdout, "%s start pid=%d repo=%s\n", o.stamp(), o.PID, bus.Toplevel(context.Background(), cwd))
	ld := store + "/ratings.sweep.lock.d"
	if lock.Acquire(ld, strconv.Itoa(o.PID)) {
		clean := &cleaner{}
		clean.hold(ld)
		stop := clean.notify()
		rc := o.RateRun(nil, paths, cwd, os.Stdout, os.Stderr)
		clean.cleanup()
		stop()
		_, _ = fmt.Fprintf(os.Stdout, "%s done rc=%d\n", o.stamp(), rc)
	} else {
		_, _ = fmt.Fprintf(os.Stdout, "%s a ratings sweep is already running — skipped\n", o.stamp())
	}
	trimLog(sweeplog, autosweepKeep)
	return 0
}

// trimLog keeps the last n lines of path, rewritten in place: a concurrent
// sweep holds the file open with O_APPEND, and replacing the inode would
// strand its `done rc=` line.
func trimLog(path string, n int) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return
	}
	_, _ = f.Write(tailLines(data, n))
	_ = f.Close()
}

// tailLines is `tail -n n`: a final line without its newline still counts.
func tailLines(data []byte, n int) []byte {
	i := len(data)
	if i > 0 && data[i-1] == '\n' {
		i--
	}
	for seen := 0; i > 0; i-- {
		if data[i-1] == '\n' {
			if seen++; seen == n {
				break
			}
		}
	}
	return data[i:]
}
