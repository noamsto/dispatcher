package rosterrender

import (
	"context"
	"fmt"
	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/clock"
	"github.com/noamsto/dispatcher/crew/internal/crews"
	"github.com/noamsto/dispatcher/crew/internal/roster"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

const (
	exitOK      = 0
	exitFailure = 1
	exitUsage   = 64
)

// Options is everything Run reads beyond the bus.
type Options struct {
	// Probes are the tmux, aeye and process-spawn reads.
	Probes Probes
	// Procs are the pid probes, shared with `crews`, `stall` and `adopt`.
	Procs crews.Probes
	// Roster is the row fold's PR probe — the same one `crew roster` runs.
	Roster roster.Probes
	// Clock is `_clock_now_f`, which drives the render timers so the suite can
	// force a sweep or a rebuild. The poll interval deliberately stays off it, so
	// a test can never sleep for hours, and `crew roster`'s ages stay on the wall
	// clock because that fold's child read `jq now`.
	Clock  clock.Clock
	Stderr io.Writer // every diagnostic, the arm's and the daemon's own
	// RosterDir is $CREW_ROSTER_DIR.
	RosterDir string
	// Self is the build this process was started as — the arm's `readlink -f
	// "$0"`, which for a ported arm is the crew script the delegation exported as
	// CREW_SELF. Empty is "no identity of my own": the installed entry is then
	// followed once and the script that answers it does the real comparison.
	Self string
	// Path is $PATH, the walk the installed-crew lookup reads. StartPath is
	// $CREW_RR_START_PATH, the PATH this daemon started with before any wrapper
	// prefix; unset means this process's own PATH is what was recorded.
	StartPath string
	Path      string
	// PID is this process's id.
	PID int
}

// wallSec is the arm's `date +%s`, the `now` every pass compares against, and
// wallTime the same clock for the diagram name's local-timezone stamp.
func (o Options) wallSec() int64 { return o.Clock.Now().Unix() }

// clockSec is the arm's `clock_now_f`: the virtual clock when CREW_CLOCK is set.
// The two are deliberately different reads, as they are in the arm — a test drives
// the timers through CREW_CLOCK while `now` stays real.
func (o Options) clockSec() int64 {
	n, _ := o.Clock.NowF().AsFloat()
	return int64(n)
}

// Run is the arm, flag for flag and refusal for refusal. It moves the process to
// the git common dir before anything else resolves a path, so a worktree reaped
// while the daemon lives cannot take the bus down with it.
// say is the arm's `echo … >&2`: a line the caller cannot act on, so its write
// error is discarded rather than pretending to be a status.
func say(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...)
}

func Run(ctx context.Context, argv []string, paths bus.Paths, o Options) int {
	c, msg, code := parse(argv)
	if msg != "" {
		say(o.Stderr, "%s\n", msg)
		return code
	}
	cdir := paths.CrewDir(c.crew)
	if err := os.MkdirAll(cdir, 0o755); err != nil {
		say(o.Stderr, "crew: roster-render: %v\n", err)
		return exitFailure
	}
	if o.StartPath == "" {
		o.StartPath = o.Path
	}
	// Resolved before the cd, exactly as the arm resolved `_rr_self` before its
	// own: `_rr_target` and every child resolve the bus from here on.
	if err := os.Chdir(paths.Common); err != nil {
		say(o.Stderr, "crew: roster-render: %v\n", err)
		return exitFailure
	}

	// The pane record is written before any lock, so a new dispatcher pane
	// retargets a renderer that is already running. Anchor, never discover.
	if c.pane != "" {
		if panePID, ok := o.Probes.PanePID(c.pane); ok && panePIDMatches(c.pane, panePID) &&
			o.Procs.IsAncestor(pidOf(panePID)) {
			_ = put(filepath.Join(cdir, filePane), c.pane+"\n", o.Stderr)
		} else {
			say(o.Stderr,
				"crew: roster-render: --pane '%s' is not this caller's pane — ignored\n", c.pane)
		}
	}

	// What the daemon, and every later build hop, is started with: the arm's
	// `rr_daemon_args`, --crew first.
	daemon := []string{"--crew", c.crew, "--interval", c.intervalText, "--quiet", c.quietText}
	if c.noOpen {
		daemon = append(daemon, "--no-open")
	}

	switch {
	case c.detach:
		// The pane was validated above and the daemon reads it back from the
		// record, because a detached child can be reparented before it thinks
		// about ancestry. Returns as soon as the child is started.
		env := append(envFor(o.StartPath), "CREW_ID="+c.crew)
		if err := o.Probes.SpawnDetached(detachArgv(o.Self, daemon), env); err != nil {
			say(o.Stderr, "crew: roster-render: %v\n", err)
			return exitFailure
		}
		return exitOK
	case c.once:
		// `_rr_pass` with no role argument lists them itself.
		roles := roleRows(o.Probes.RolePanes(), paths.Dir, c.crew)
		if _, err := pass(ctx, o, paths, cdir, c.crew, c.noOpen, roles); err != nil {
			say(o.Stderr, "crew: roster-render: %v\n", err)
			return exitFailure
		}
		return exitOK
	default:
		return o.daemon(ctx, paths, cdir, c, daemon)
	}
}

// detachArgv is the arm's `bash -euo pipefail "$_rr_self" roster-render …`. The
// script goes through bash rather than being exec'd: the repo copy carries no exec
// bit and the installed one is a wrapper whose shebang carries none of the flags
// the arm runs under. A worker entered directly, with no script in front of it,
// starts its own binary instead.
func detachArgv(self string, daemon []string) []string {
	if self == "" {
		if exe, err := os.Executable(); err == nil {
			return append([]string{exe, "roster-render"}, daemon...)
		}
		self = "crew"
	}
	return append([]string{"bash", "-euo", "pipefail", self, "roster-render"}, daemon...)
}

// panePIDRe is the arm's `^%[0-9]+$`: a pane id, not a window or session target.
var paneIDRe = regexp.MustCompile(`\A%[0-9]+\z`)

// panePIDMatches is the shape check on both the target and the pid read from it.
func panePIDMatches(pane, pid string) bool {
	return paneIDRe.MatchString(pane) && digitsOnly(pid)
}

// pidOf is the numeric read the ancestry check takes.
func pidOf(pid string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(pid))
	return n
}

func digitsOnly(s string) bool {
	s = strings.TrimSpace(s)
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
