package rosterrender

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/noamsto/dispatcher/crew/internal/ascii"
	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/clock"
	"github.com/noamsto/dispatcher/crew/internal/entry"
	"github.com/noamsto/dispatcher/crew/internal/gh"
	"github.com/noamsto/dispatcher/crew/internal/git"
	"github.com/noamsto/dispatcher/crew/internal/proc"
	"github.com/noamsto/dispatcher/crew/internal/repos"
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
	// Proc answers whether the dispatcher pane is this process's ancestor.
	Proc proc.Table
	// Git reads the worktree list the row fold resolves `exited` rows from.
	Git git.Client
	// Clock is `_clock_now_f`, which drives the render timers so the suite can
	// force a sweep or a rebuild. The poll interval deliberately stays off it, so
	// a test can never sleep for hours, and `crew roster`'s ages stay on the wall
	// clock because that fold's child read `jq now`.
	Clock  clock.Clock
	Stderr io.Writer // every diagnostic, the arm's and the daemon's own
	// Stdout is where `--map` prints its geometry; the daemon never writes to it.
	Stdout io.Writer
	// RosterDir is $CREW_ROSTER_DIR.
	RosterDir string
	// Self is the build this process was started as — the arm's `readlink -f
	// "$0"`, which for a ported arm is the crew script the delegation exported as
	// CREW_SELF. Unset, the binary this process is running stands in for it.
	Self string
	// Path is $PATH, the walk the installed-crew lookup reads. StartPath is
	// $CREW_RR_START_PATH, the PATH this daemon started with before any wrapper
	// prefix; unset means this process's own PATH is what was recorded.
	StartPath string
	Path      string
	// PID is this process's id.
	PID int
	// Discover lists the repos that may carry the crew's rows; the roster joins
	// each whose bus has one. Nil draws the local bus alone.
	Discover func(stderr io.Writer) []repos.Repo
	// Gh looks up the CI state of open PRs. Nil asks GitHub nothing and draws only
	// the states already cached.
	Gh gh.Client
	// BudgetFile is the engine quota cache the legend's engine readings come from.
	// Empty draws every engine as unknown.
	BudgetFile string
	// Budget resolves the fleet budget. Nil, or an error, draws it as unknown.
	Budget func(ctx context.Context) (int, error)

	// disc remembers the joined repos and the resolved budget between passes; Run
	// sets it for every mode.
	disc *discovery
}

// wallSec is the arm's `date +%s`, the `now` every pass compares against, and
// wallTime the same clock for the diagram name's local-timezone stamp.
func (o Options) wallSec() int64 { return o.Clock.Now().Unix() }

// clockSec is the arm's `clock_now_f`: the virtual clock when CREW_CLOCK is set.
// The two are deliberately different reads, as they are in the arm — a test drives
// the timers through CREW_CLOCK while `now` stays real.
func (o Options) clockSec() int64 {
	return int64(o.Clock.NowF())
}

// say is the arm's `echo … >&2`: a line its caller cannot act on.
func say(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...)
}

// Run is the arm, flag for flag and refusal for refusal. It moves the process to
// the git common dir before anything else resolves a path, so a worktree reaped
// while the daemon lives cannot take the bus down with it.
func Run(ctx context.Context, req Request, paths bus.Paths, o Options) int {
	c, msg, code := check(req)
	if msg != "" {
		say(o.Stderr, "%s\n", msg)
		return code
	}
	o.disc = &discovery{}
	if c.mapMode {
		return runMap(ctx, c, paths, o)
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
		panePID, err := o.Probes.Tmux.Run(ctx, "display-message", "-p", "-t", c.pane, "#{pane_pid}")
		if err == nil && panePIDMatches(c.pane, panePID) && proc.IsAncestor(o.Proc, pidOf(panePID)) {
			_ = put(filepath.Join(cdir, filePane), c.pane+"\n", o.Stderr)
		} else {
			say(o.Stderr,
				"crew: roster-render: --pane '%s' is not this caller's pane — ignored\n", c.pane)
		}
	}

	// What the daemon, and every later build hop, is started with: the arm's
	// `rr_daemon_args`, --crew first.
	daemon := []string{"--crew", c.crew, "--interval", c.intervalText, "--quiet", c.quietText, "--legend", string(c.legend)}
	if c.noOpen {
		daemon = append(daemon, "--no-open")
	}

	switch {
	case c.detach:
		// The pane was validated above and the daemon reads it back from the
		// record, because a detached child can be reparented before it thinks
		// about ancestry. Returns as soon as the child is started.
		env := append(envFor(o.StartPath, o.Self), "CREW_ID="+c.crew)
		if err := o.Probes.SpawnDetached(detachArgv(o.Self, daemon), env); err != nil {
			say(o.Stderr, "crew: roster-render: %v\n", err)
			return exitFailure
		}
		return exitOK
	case c.once:
		// `_rr_pass` with no role argument lists them itself.
		roles := roleRows(o.rolePanes(ctx), paths.Dir, c.crew)
		if _, err := pass(ctx, o, paths, cdir, c.crew, c.noOpen, c.legend, roles); err != nil {
			say(o.Stderr, "crew: roster-render: %v\n", err)
			return exitFailure
		}
		return exitOK
	default:
		return o.daemon(ctx, paths, cdir, c, daemon)
	}
}

// mapOut is `--map`'s stdout: the image's size, where it is, and each planet in
// slot order, which is the keyboard order.
type mapOut struct {
	V       int         `json:"v"`
	W       int         `json:"w"`
	H       int         `json:"h"`
	PNG     string      `json:"png"`
	Planets []mapPlanet `json:"planets"`
}

// runMap is one render of the crew's picture to a PNG, with the planets' places
// on stdout. It runs before the crew dir or the working directory is touched
// and writes nothing but the PNG: no lock, no records, no publish, and the slot
// sequence is read but not saved, so the daemon's own diagram keeps its places.
// It never asks GitHub either, so it draws the CI states the daemon cached.
func runMap(ctx context.Context, c call, paths bus.Paths, o Options) int {
	o.Gh = nil
	fail := func(err error) int {
		say(o.Stderr, "crew: roster-render: %v\n", err)
		return exitFailure
	}
	out, err := filepath.Abs(c.out)
	if err != nil {
		return fail(err)
	}
	fr, err := buildFrame(ctx, o, paths, paths.CrewDir(c.crew), c.crew, roleRows(o.rolePanes(ctx), paths.Dir, c.crew), false)
	if err != nil {
		return fail(err)
	}
	svg, err := o.Probes.Graphviz(ctx, []byte(buildDOT(fr.sc, nil, legendNone, c.selectBranch, c.selectRepo)))
	if err != nil {
		return fail(err)
	}
	svg = finishSVG(svg)
	vbW, vbH, ok := viewBoxSize(svg)
	if !ok {
		return fail(errors.New("neato: the svg has no viewBox"))
	}
	png, err := o.Probes.Resvg(ctx, svg, c.pxW, c.pxH)
	if err != nil {
		return fail(err)
	}
	if err := put(out, string(png), io.Discard); err != nil {
		return fail(err)
	}
	halfX, halfY := mapHalf(fr.sc)
	data, err := json.Marshal(mapOut{
		V: 1, W: c.pxW, H: c.pxH, PNG: out,
		Planets: mapGeometry(fr.sc, halfX, halfY, vbW, vbH, c.pxW, c.pxH),
	})
	if err != nil {
		return fail(err)
	}
	say(o.Stdout, "%s\n", data)
	return exitOK
}

// detachArgv names the entry the detached daemon re-enters. A worker entered
// directly, with no entry in front of it, starts its own binary instead.
func detachArgv(self string, daemon []string) []string {
	if self == "" {
		if exe, err := os.Executable(); err == nil {
			return append([]string{exe, "roster-render"}, daemon...)
		}
		self = "crew"
	}
	return entry.EntryArgv(self, append([]string{"roster-render"}, daemon...)...)
}

// panePIDRe is the arm's `^%[0-9]+$`: a pane id, not a window or session target.
var paneIDRe = regexp.MustCompile(`\A%[0-9]+\z`)

// panePIDMatches is the shape check on both the target and the pid read from it.
func panePIDMatches(pane, pid string) bool {
	return paneIDRe.MatchString(pane) && ascii.IsDigits(strings.TrimSpace(pid))
}

// pidOf is the numeric read the ancestry check takes.
func pidOf(pid string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(pid))
	return n
}
