// Package rolewatch is `crew role-watch`, the per-role-pane supervisor ported
// from dispatch.sh's `--role-watch` arm: it reads the crew bus, admits only the
// role's lead or dispatcher (W1), queues their assignments and types each into
// the role pane only when a capture is positively an idle input box, then
// verifies the submit. The arm's header comment still documents the delivery
// rules; this port keeps its state machine, its tmux argv and its per-tick call
// order, because the dispatch bats stub matches argv and counts calls.
//
// Refusal lines keep the `dispatch: --role-watch` prefix on purpose: S7 will
// exec this subcommand behind `dispatch --role-watch` (until then bash runs
// role-watch) and the bats match those lines.
//
// Bus location: the bus is still discovered from the watcher's cwd, resolved
// once at startup by the caller (Options.Log). dispatch launches the watcher in
// the worker's own worktree and the only reader of its rows is that worker's
// own role pane, so anchoring would add a flag every launcher and resume path
// must pass and buy nothing against a writer that already shares the worktree.
//
// Crew id, a deliberate deviation: rows are posted under the pane window's
// @crew_id stamp first and Options.CrewID only when the window has none. The
// arm's `crew msg` took WORKER_TASK.md's crew_id first and the exported stamp
// second; the two differ only when they disagree, and then the stamp, set by
// dispatch and not editable by the worker, is the anchor.
package rolewatch

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/clock"
	"github.com/noamsto/dispatcher/crew/internal/tmux"
)

// Request is the command line as given; refusal lines echo the raw values.
type Request struct {
	Role, Pane, Branch, Engine, Interval, DeferNotice, SubmitRetries string
}

// Options is everything Run reads beyond its request.
type Options struct {
	Tmux          tmux.InputClient
	Clock         clock.Clock
	Stderr        io.Writer
	CurrentBranch func() (string, error) // the --branch default
	Log           string                 // the bus file; empty never reads
	LogErr        error                  // why the bus could not be located; refuses at startup
	// Post writes one msg row; a failed post is ignored, as `crew msg … || true` was.
	Post   func(ctx context.Context, crew, from, to, body string)
	CrewID func() string // the crew when the window carries no @crew_id
	PID    int
	Rand   func() int // the buffer name's $RANDOM
}

// NewClock is the arm's _rw_now/_rw_sleep pair: $DISPATCH_ROLE_WATCH_CLOCK has
// $CREW_CLOCK's semantics.
func NewClock(getenv func(string) string) clock.Clock {
	return clock.Clock{Now: time.Now, CrewClock: getenv("DISPATCH_ROLE_WATCH_CLOCK")}
}

var (
	reUint    = regexp.MustCompile(`^[0-9]+$`)
	reDecimal = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)
	rePaneID  = regexp.MustCompile(`^%[0-9]+$`)
)

// refusal is a startup refusal's stderr line and exit code.
type refusal struct {
	line string
	code int
}

func refuse(line string) *refusal { return &refusal{line, 1} }

// Run watches req.Pane until it is gone or marked exited (0), or refuses to
// start (the refusal's code). A done ctx ends it with 0 at any point.
func Run(ctx context.Context, req Request, o Options) int {
	w, r := start(ctx, req, o)
	if ctx.Err() != nil {
		return 0
	}
	if r != nil {
		fmt.Fprintln(o.Stderr, r.line)
		return r.code
	}
	return w.loop()
}

// start validates in the arm's order and reads the pane's stamps (F22); it
// writes nothing to tmux before every check has passed.
func start(ctx context.Context, req Request, o Options) (*watcher, *refusal) {
	switch {
	case req.Role == "":
		return nil, refuse("dispatch: --role-watch needs a role name")
	case req.Pane == "":
		return nil, refuse("dispatch: --role-watch needs --pane <id>")
	case !reUint.MatchString(req.SubmitRetries):
		return nil, refuse("dispatch: --role-watch: --submit-retries must be a non-negative integer, got " + req.SubmitRetries)
	case !reDecimal.MatchString(req.Interval):
		return nil, refuse("dispatch: --role-watch: --interval must be a non-negative number, got " + req.Interval)
	case !reUint.MatchString(req.DeferNotice):
		return nil, refuse("dispatch: --role-watch: --defer-notice must be a non-negative integer, got " + req.DeferNotice)
	}
	branch := req.Branch
	if branch == "" {
		var err error
		if branch, err = o.CurrentBranch(); err != nil {
			return nil, &refusal{"dispatch: --role-watch: cannot read the current branch: " + err.Error(), 128}
		}
	}
	if !rePaneID.MatchString(req.Pane) {
		return nil, refuse("dispatch: --role-watch: --pane must be a pane id (%N), got " + req.Pane)
	}
	if req.Role == "lead" {
		return nil, refuse("dispatch: --role-watch: role 'lead' is never watched")
	}
	w := &watcher{
		ctx: ctx, o: o,
		role: req.Role, pane: req.Pane, branch: branch, engine: req.Engine, interval: req.Interval,
		roleID: "role:" + branch + ":" + req.Role,
	}
	// Range errors clamp to the maximum, a grammar-valid count bash would also
	// never reach.
	w.submitRetries, _ = strconv.Atoi(req.SubmitRetries)
	deferNotice, _ := strconv.ParseInt(req.DeferNotice, 10, 64)
	w.deferNotice = deferNotice

	stamp := fields(w.tmux("display-message", "-p", "-t", w.pane, "#{@crew_role}|#{window_id}|#{@crew_delivery}"), 3)
	wRole, win := stamp[0], stamp[1]
	w.win, w.delivery = win, stamp[2]
	if wRole != w.role {
		return nil, refuse(fmt.Sprintf("dispatch: --role-watch: pane %s's @crew_role (%s) does not match --role %s", w.pane, wRole, w.role))
	}
	if b := w.tmux("show-options", "-wqv", "-t", win, "@crew_branch"); b != branch {
		return nil, refuse(fmt.Sprintf("dispatch: --role-watch: window %s's @crew_branch (%s) does not match --branch %s", win, b, branch))
	}
	w.wCrew = w.tmux("show-options", "-wqv", "-t", win, "@crew_id")
	w.crew = w.wCrew
	if w.crew == "" {
		w.crew = o.CrewID()
	}
	if o.LogErr != nil {
		return nil, &refusal{"dispatch: --role-watch: no crew bus: not in a git repository", 128}
	}
	w.logSize = -1
	w.since = o.Clock.RealMS()
	w.setState("idle")
	return w, nil
}

// fields is `IFS='|' read -r a b … <<<"$s"`: the first line split on `|`, the
// last name taking the rest, missing names empty.
func fields(s string, n int) []string {
	line, _, _ := strings.Cut(s, "\n")
	f := strings.SplitN(line, "|", n)
	for len(f) < n {
		f = append(f, "")
	}
	return f
}
