package rosterrender

import (
	"context"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/lock"
)

// rebuildBackstop is the arm's `-ge 60`: even a bus that never changes, and a crew
// whose every part is idle, gets one redraw a minute, so a diagram cannot sit
// stale on a quiet host.
const rebuildBackstop = 60

// daemon is the arm's loop: one renderer per crew by lock, one redraw per change
// of a signature the bus cannot hide, a crew with nothing live retired after its
// quiet window, and the whole process replaced in place when a newer `crew` is
// installed.
func (o Options) daemon(ctx context.Context, paths bus.Paths, cdir string, c call, daemonArgs []string) int {
	ld := filepath.Join(cdir, "roster-render.lock.d")
	owner := strconv.Itoa(o.PID)

	// Acquired before the signal handler is armed, like the arm's `trap` after its
	// `_lock_acquire`: a start refused because another renderer holds the lock must
	// not release the incumbent's.
	if !lock.Acquire(ld, owner) {
		return exitOK
	}
	// `_lock_release` is an unconditional `rm -rf`; the owner check is the arm's,
	// which keeps a renderer whose lock was reclaimed (crew dir deleted and
	// re-created) from deleting the new owner's.
	defer func() {
		if lock.Holder(ld) == owner {
			lock.Release(ld)
		}
	}()

	// The arm's `trap 'exit 0' INT TERM`, armed only now so a refused start stays a
	// silent no-op rather than a signal-driven exit.
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// The build this process is running, resolved once — the arm's
	// `_rr_self=$(readlink -f "$0")` before the loop. It must not be re-resolved per
	// tick: CREW_SELF names the entry, and the entry is the very symlink a switch
	// repoints, so resolving it again would follow the new build and read as "no hop
	// needed" forever.
	running := selfBuild(o.Self)

	// rr_live starts at 1, not 0: a renderer must not read its own crew as drained
	// before its first pass has said anything about it.
	var (
		live      int64 = 1
		sig             = ""
		lastBuild int64 = 0
		idleSince int64
		idleSet   = false
	)

	for {
		// Losing the lock — `crew deregister` removed the crew dir — ends the loop.
		if lock.Holder(ld) != owner {
			return exitOK
		}
		now := o.clockSec()
		roles := roleRows(o.Probes.RolePanes(), paths.Dir, c.crew)
		newSig := loopSig(busSize(paths), cdir, roles)
		if newSig != sig || now-lastBuild >= rebuildBackstop {
			// A failed pass keeps the last live count rather than reading as
			// drained: one malformed line must not retire the renderer.
			got, err := pass(ctx, o, paths, cdir, c.crew, c.noOpen, roles)
			if err == nil {
				live = got
			} else {
				say(o.Stderr, "crew: roster-render: %v\n", err)
			}
			sig = newSig
			lastBuild = now
		}

		if live == 0 {
			if !idleSet {
				idleSince, idleSet = now, true
			}
			if now-idleSince >= c.quiet {
				return exitOK
			}
		} else {
			idleSet = false
		}

		// The upgrade point is the sleep boundary, and a crew already draining is
		// left to exit rather than upgraded: the hop would restart its quiet window
		// in the new build, and the next dispatch starts that build anyway. A signal
		// that arrived during the pass is honoured here rather than execed through —
		// the arm's `trap 'exit 0'` fired between commands, so a killed renderer never
		// reached its exec either.
		if ctx.Err() != nil {
			return exitOK
		}
		if err := o.hop(running, idleSet, daemonArgs); err != nil {
			// The arm's `exec` under `set -e`: a build that cannot be started takes
			// the daemon down rather than spinning on it.
			say(o.Stderr, "crew: roster-render: %v\n", err)
			return exitFailure
		}

		if err := o.Probes.Sleep(ctx, time.Duration(c.interval)*time.Second); err != nil {
			return exitOK
		}
	}
}

// hop is the build hop: when the installed `crew` is a different build than this
// process, replace this one with it, PATH restored to the value this daemon
// started with so a hop carries one wrapper prefix rather than growing it.
//
// Only the running build is refused, so a rollback is followed the same way as an
// upgrade: the entry is the thing being followed, and which build it happens to
// name is not the daemon's business.
//
// A successful call never returns, so any error is the failure to start the other
// build. `idleSince` is whether the crew is already draining, which is the arm's
// one reason to stay on the running build.
func (o Options) hop(running string, idleSince bool, daemonArgs []string) error {
	if idleSince {
		return nil
	}
	want := o.Probes.InstalledCrew(o.Path)
	if want == "" || want == running {
		return nil
	}
	return o.Probes.Exec(want, append([]string{want, "roster-render"}, daemonArgs...),
		envFor(o.StartPath))
}

// selfBuild is the build this process was started as: the script path the
// delegation exported as CREW_SELF, resolved the way the arm resolved
// `readlink -f "$0"`. A worker entered directly, with no script in front of it,
// is its own binary.
func selfBuild(self string) string {
	if self != "" {
		return realPath(self)
	}
	if exe, err := os.Executable(); err == nil {
		return realPath(exe)
	}
	return ""
}

// envFor is the inherited environment with PATH restored and CREW_RR_START_PATH
// pinned, so a hop never inherits the wrapper prefix it just skipped.
func envFor(startPath string) []string {
	env := os.Environ()
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if !strings.HasPrefix(kv, "CREW_RR_START_PATH=") && !strings.HasPrefix(kv, "PATH=") {
			out = append(out, kv)
		}
	}
	return append(out, "PATH="+startPath, "CREW_RR_START_PATH="+startPath)
}

// realPath is `readlink -f`, and "" for anything that does not resolve — the empty
// path first of all.
func realPath(path string) string {
	if path == "" {
		return ""
	}
	r, err := filepath.EvalSymlinks(path)
	if err != nil {
		return ""
	}
	return r
}
