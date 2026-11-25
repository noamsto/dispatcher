// Package where is `crew where`: the human-usable address for a worker's pane —
// codename, `session:window.pane`, window name, role and the jump command a
// dispatcher relays to a human.
//
// The trust rule is the arm's and it is the point: a target resolves from
// dispatcher-anchored state only, the window's `@crew_*` tmux stamps and the
// bus's `dispatch` rows. Never from git discovery inside a worktree and never
// from the worker's env beyond `_crew_id`'s own default, because the command
// answers "where is that worker's pane" from outside its worktree.
//
// Its one read of the fold is the difference between a window that is gone and a
// target nothing ever dispatched: resolve.Resolve answers "the bus knows this
// branch", and the arm says so instead of claiming the target is unknown.
package where

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/identity"
	"github.com/noamsto/dispatcher/crew/internal/remote"
	"github.com/noamsto/dispatcher/crew/internal/resolve"
	"github.com/noamsto/dispatcher/crew/internal/tmux"
	"github.com/noamsto/dispatcher/crew/internal/tmuxstamp"
)

const (
	exitFailure = 1
	// exitRefused is a crew the resolver refuses (archived, index-only), so a
	// caller can tell it from a failure it may fall back on.
	exitRefused = 3
	// exitNoHome is a layout-2 crew registered outside a repo, which a relaunch
	// tells apart from a crew whose home is unusable.
	exitNoHome = 4
	// Usage is the arm's `crew where` signature, quoted in every refusal.
	Usage = "usage: crew where <target> [--crew ID] | crew where --log|--dir [--crew ID] | crew where --json [--crew ID] | crew where --home [--crew ID]"
	// jump is the line's tail: what a human or a dispatcher pastes to get there.
	jump = "   jump: ! tmux switch-client -t "
)

// Options is everything Run reads beyond the bus: the crew the arm defaults to
// (`_crew_id`), which `--crew` overrides, and tmux.
type Options struct {
	CrewID func() string
	Tmux   tmux.Client
	// Remote runs argv on host as crew with its stdout going to stdout, and
	// returns its status: how a worker dispatched there is asked where it is.
	// Nil routes nothing.
	Remote func(host, crew string, argv []string, stdout io.Writer, crewScope bool) int
}

// window is one crew-anchored window: id, branch, codename, session, window index,
// name. The id and index come from tmux itself; the rest are read per window.
type window struct {
	id, branch, name, session, index, winName string
}

// say writes to a stream whose failure is reported elsewhere (the exit status,
// or nowhere, for stderr) — main.go's helper, same reason.
func say(w io.Writer, format string, args ...any) { _, _ = fmt.Fprintf(w, format, args...) }

// die is `_where_die`: the arm's prefix, its exit status, nothing on stdout.
func die(stderr io.Writer, format string, args ...any) int {
	say(stderr, "crew: where: %s\n", fmt.Sprintf(format, args...))
	return exitFailure
}

// Request is the arm's validated arguments: Crew is the `--crew` override ("" for
// the `_crew_id` default) and Target is never empty.
type Request struct {
	Crew, Target string
}

// PathRequest is `crew where --log|--dir|--json|--home [--crew ID]`: Crew is the flag
// ("" for the task doc and env), Log picks the log over the dir, JSON prints
// the whole answer instead and Home prints the crew's home main checkout.
type PathRequest struct {
	Crew string
	Log  bool
	JSON bool
	Home bool
}

// pathJSON is the `--json` answer; Meta is null for a layout-1 crew.
type pathJSON struct {
	CrewID  string    `json:"crew_id"`
	Layout  int       `json:"layout"`
	Dir     string    `json:"dir"`
	Log     string    `json:"log"`
	RepoDir *string   `json:"repo_dir"`
	Meta    *bus.Meta `json:"meta"`
}

// RunPath prints the resolved crew's log or dir, or with JSON the whole answer. It reads no tmux, so the
// crew comes from the flag or crewID alone, and it needs a repo only for a
// layout-1 crew.
func RunPath(req PathRequest, paths bus.Paths, stdout, stderr io.Writer, crewID func() string) int {
	crew := req.Crew
	if crew == "" {
		crew = crewID()
	}
	if crew == "" {
		say(stderr, "crew: %v\n", bus.ErrNoCrew)
		return exitFailure
	}
	bc, err := paths.Crew(crew)
	switch {
	case bus.IsRefusal(err):
		say(stderr, "crew: %v\n", err)
		return exitRefused
	case errors.Is(err, bus.ErrNotRepo):
		say(stderr, "crew: not in a git repo\n")
		return exitFailure
	case err != nil:
		say(stderr, "crew: %v\n", err)
		return exitFailure
	}
	if req.JSON {
		return printJSON(bc, paths, stdout, stderr)
	}
	if req.Home {
		return printHome(bc, paths, stdout, stderr)
	}
	path := bc.Dir
	if req.Log {
		path = bc.Log
	}
	say(stdout, "%s\n", path)
	return 0
}

// printHome prints the main checkout a layout-2 crew was registered in, once its
// home_repo is shown to be a usable common dir on this host.
func printHome(bc bus.Crew, paths bus.Paths, stdout, stderr io.Writer) int {
	if bc.Layout != 2 {
		say(stderr, "crew: crew %s is layout 1 — its home is the repo it runs in\n", bc.ID)
		return exitFailure
	}
	m, err := bus.ReadMeta(paths.State, bc.ID)
	if err != nil {
		say(stderr, "crew: %v\n", err)
		return exitFailure
	}
	if m.Visiting {
		say(stderr, "crew: crew %s is visiting this host — relaunch it on %s\n", bc.ID, m.HomeHost)
		return exitFailure
	}
	if m.HomeRepo == nil {
		say(stderr, "crew: crew %s has no home repo — it cannot be spawned into a repo\n", bc.ID)
		return exitNoHome
	}
	h := *m.HomeRepo
	if !filepath.IsAbs(h) {
		say(stderr, "crew: crew %s: home repo %s is not absolute\n", bc.ID, h)
		return exitFailure
	}
	if tag, err := bus.RepoTag(h); err != nil || tag != h {
		say(stderr, "crew: crew %s: home repo %s is not its own realpath\n", bc.ID, h)
		return exitFailure
	}
	if filepath.Base(h) != ".git" {
		say(stderr, "crew: crew %s: home repo %s is not a .git directory\n", bc.ID, h)
		return exitFailure
	}
	say(stdout, "%s\n", filepath.Dir(h))
	return 0
}

func printJSON(bc bus.Crew, paths bus.Paths, stdout, stderr io.Writer) int {
	ans := pathJSON{CrewID: bc.ID, Layout: bc.Layout, Dir: bc.Dir, Log: bc.Log}
	if bc.RepoDir != "" {
		ans.RepoDir = &bc.RepoDir
	}
	if bc.Layout == 2 {
		m, err := bus.ReadMeta(paths.State, bc.ID)
		if err != nil {
			say(stderr, "crew: %v\n", err)
			return exitFailure
		}
		ans.Meta = &m
	}
	data, err := json.Marshal(ans)
	if err != nil {
		say(stderr, "crew: %v\n", err)
		return exitFailure
	}
	say(stdout, "%s\n", data)
	return 0
}

// Run is the arm: the two tmux reads, the resolution, the line.
func Run(ctx context.Context, req Request, paths bus.Paths, stdout, stderr io.Writer, o Options) int {
	crew, target := req.Crew, req.Target
	if crew == "" && o.CrewID != nil {
		crew = o.CrewID()
	}
	if crew != "" {
		c, err := paths.Crew(crew)
		if err != nil {
			return die(stderr, "invalid crew id %s", crew)
		}
		paths.Log = c.Log
	}

	// A tmux read failure is not "the target is gone".
	all, err := tmuxstamp.Windows(ctx, o.Tmux)
	if err != nil {
		return die(stderr, "cannot read tmux windows (is a tmux server running?)")
	}
	panes, err := tmuxstamp.Panes(ctx, o.Tmux)
	if err != nil {
		return die(stderr, "cannot read tmux panes (is a tmux server running?)")
	}
	server := ""
	if paths.Dir == "" {
		if server, err = o.Tmux.Run(ctx, "display-message", "-p", "#{pid}:#{start_time}"); err != nil {
			return die(stderr, "cannot read the tmux server identity (is a tmux server running?)")
		}
	}
	wins, err := readWindows(ctx, o.Tmux, all, paths, crew, server)
	if err != nil {
		return die(stderr, "%v", err)
	}

	if o.Remote != nil {
		host, branch, err := remote.ResolveTarget(resolve.Rows(paths.Log), target, crew, localMatch(wins, target))
		if err != nil {
			return die(stderr, "%v", err)
		}
		as := crew
		if host != "" && crew == "" {
			// No crew of its own: the remote is asked as the dispatch row's crew.
			events, _ := bus.ReadEventsTolerant(paths.Log)
			if host, as, err = remote.RouteAny(events, branch); err != nil {
				return die(stderr, "CREW_ID not set and %v", err)
			}
		}
		if host != "" {
			return whereRemote(ctx, host, branch, as, all, panes, paths, stdout, stderr, o)
		}
	}

	win, hit, code := resolveTarget(wins, panes, target, crew, paths, stderr)
	if code != 0 {
		return code
	}

	// A window addressed by branch or codename means its lead pane.
	if hit.ID == "" {
		var err error
		if hit, code, err = leadPane(ctx, o.Tmux, panes, win.id); err != nil {
			return die(stderr, "cannot read tmux pane stamps: %v", err)
		}
		if code != 0 {
			return die(stderr, "no pane in the window for '%s'", target)
		}
	}
	role, err := paneRole(ctx, o.Tmux, hit.ID)
	if err != nil {
		return die(stderr, "cannot read tmux pane stamps: %v", err)
	}
	// An older window stamped no @crew_name: the pool gives the codename dispatch
	// would have stamped.
	name := win.name
	if name == "" {
		name = poolName(win.branch)
	}

	// The line a human or a dispatcher relays verbatim: em dash, three spaces
	// before `jump:`, the window name raw between literal quotes (a quote is legal
	// in a branch name, which is what the window is named from).
	say(stdout, "%s — %s:%s.%s \"%s\" (%s pane)%s%s\n",
		name, win.session, win.index, hit.Index, win.winName, role, jump, hit.ID)
	return 0
}

// localMatch is the branch the local windows give a target (its own, else its
// codename's), joined with "," when several share the codename, and "" for none.
func localMatch(wins []window, target string) string {
	for _, w := range wins {
		if w.branch == target {
			return w.branch
		}
	}
	var matches []window
	for _, w := range wins {
		if w.name == target {
			matches = append(matches, w)
		}
	}
	return joinBranches(matches, ",")
}

// whereRemote answers for a worker dispatched on host. A mirror window on this
// machine is the jump target, and its line is the usual one; with no mirror the
// host is asked, and its line is passed on. Either way the line ends ` [host h]`.
func whereRemote(ctx context.Context, host, branch, crew string, all []tmuxstamp.Window, panes []tmuxstamp.Pane, paths bus.Paths, stdout, stderr io.Writer, o Options) int {
	mirror, ok, err := mirrorWindow(ctx, o.Tmux, all, branch, crew)
	if err != nil {
		return die(stderr, "cannot read tmux window stamps: %v", err)
	}
	if !ok {
		var out bytes.Buffer
		events, _ := bus.ReadEventsTolerant(paths.Log)
		crewScope := remote.BranchLayout(events, branch, crew) == 2
		if rc := o.Remote(host, crew, []string{"crew", "where", "--crew", crew, "--", branch}, &out, crewScope); rc != 0 {
			return rc
		}
		say(stdout, "%s [host %s]\n", strings.TrimSuffix(out.String(), "\n"), host)
		return 0
	}
	hit, code, err := leadPane(ctx, o.Tmux, panes, mirror.id)
	if err != nil {
		return die(stderr, "cannot read tmux pane stamps: %v", err)
	}
	if code != 0 {
		return die(stderr, "no pane in the window for '%s'", branch)
	}
	name := mirror.name
	if name == "" {
		for _, r := range resolve.Resolve(resolve.Rows(paths.Log), branch, crew) {
			if r.Branch == branch {
				name = r.Name
			}
		}
	}
	if name == "" {
		name = poolName(branch)
	}
	role, err := paneRole(ctx, o.Tmux, hit.ID)
	if err != nil {
		return die(stderr, "cannot read tmux pane stamps: %v", err)
	}
	say(stdout, "%s — %s:%s.%s \"%s\" (%s pane)%s%s [host %s]\n",
		name, mirror.session, mirror.index, hit.Index, mirror.winName, role, jump, hit.ID, host)
	return 0
}

// mirrorWindow is the local window that mirrors a remote worker's: the one whose
// @bridge_crew_branch is the branch and whose @bridge_crew_id is unset or this crew's.
func mirrorWindow(ctx context.Context, c tmux.Client, all []tmuxstamp.Window, branch, crew string) (window, bool, error) {
	for _, w := range all {
		b, ok, err := tmuxstamp.WindowOption(ctx, c, w.ID, "@bridge_crew_branch")
		if err != nil {
			return window{}, false, err
		}
		if !ok || b != branch {
			continue
		}
		id, ok, err := tmuxstamp.WindowOption(ctx, c, w.ID, "@bridge_crew_id")
		if err != nil {
			return window{}, false, err
		}
		if !ok || id != "" && id != crew {
			continue
		}
		win, ok, err := describe(ctx, c, w)
		if err != nil {
			return window{}, false, err
		}
		if ok {
			return win, true, nil
		}
	}
	return window{}, false, nil
}

// describe fills a window's display fields: its codename, session and name, each its
// own read. The session name is asked of the winlink the index came from. A window
// whose value is unclean is not one to point at.
func describe(ctx context.Context, c tmux.Client, w tmuxstamp.Window) (window, bool, error) {
	name, ok1, err1 := tmuxstamp.WindowOption(ctx, c, w.ID, "@crew_name")
	session, ok2, err2 := tmuxstamp.Field(ctx, c, w.Target(), "#{session_name}")
	winName, ok3, err3 := tmuxstamp.Field(ctx, c, w.Target(), "#{window_name}")
	return window{id: w.ID, name: name, session: session, index: w.Index, winName: winName},
		ok1 && ok2 && ok3, errors.Join(err1, err2, err3)
}

// readWindows is the arm's crew filter over the server's windows: a non-empty
// branch, this repo's crew dir, and this crew's id when the window carries one.
// An empty caller crew (no CREW_ID, no WORKER_TASK.md) filters nothing at all. A
// state-only call (dir "") has no checkout, so a window is this crew's only when
// its id says so and the crew's own dispatch, resume or window row for its branch
// names its tmux window id, and (a row that recorded one) the identity of this tmux
// server, which the row's id is unique within: any process on the tmux server can
// stamp a window with a crew's id, so the stamp alone binds nothing
// (remote.WindowBound). The identity is `server`, read once from its own
// `display-message`, which no window option can shift.
func readWindows(ctx context.Context, c tmux.Client, all []tmuxstamp.Window, paths bus.Paths, crew string, server string) ([]window, error) {
	dir := paths.Dir
	var events []bus.Event
	if dir == "" && crew != "" {
		var err error
		if events, err = remote.BoundRows(paths.Log); err != nil {
			return nil, fmt.Errorf("cannot read the crew log: %w", err)
		}
	}
	var wins []window
	for _, w := range all {
		var v [3]string
		skip := false
		for i, name := range []string{"@crew_branch", "@crew_id", "@crew_dir"} {
			val, ok, err := tmuxstamp.WindowOption(ctx, c, w.ID, name)
			if err != nil {
				return nil, fmt.Errorf("cannot read tmux window stamps: %w", err)
			}
			v[i] = val
			if !ok || i == 0 && val == "" || i == 1 && crew != "" && val != "" && val != crew {
				skip = true
				break
			}
		}
		branch, id, wdir := v[0], v[1], v[2]
		switch {
		case skip:
			continue
		case dir == "":
			if crew == "" || id != crew || !remote.WindowBound(events, branch, crew, w.ID, wdir, server) {
				continue
			}
		case wdir != dir:
			continue
		}
		win, ok, err := describe(ctx, c, w)
		if err != nil {
			return nil, fmt.Errorf("cannot read tmux window stamps: %w", err)
		}
		if ok {
			win.branch = branch
			wins = append(wins, win)
		}
	}
	return wins, nil
}

// paneRole is what a pane's label prints: its @crew_role, "lead" for a plain
// (non-grid) dispatch window's lone pane, which stamps none, and "unknown" for a
// value that cannot be read cleanly.
func paneRole(ctx context.Context, c tmux.Client, id string) (string, error) {
	role, ok, err := tmuxstamp.PaneOption(ctx, c, id, "@crew_role")
	switch {
	case err != nil:
		return "", err
	case !ok:
		return "unknown", nil
	case role == "":
		return "lead", nil
	}
	return role, nil
}

// resolveTarget is the arm's resolution: a `%pane` id keeps that exact pane,
// else the branch, else the codename, else whatever the bus says about it. The
// zero pane means "this window's lead pane".
func resolveTarget(wins []window, panes []tmuxstamp.Pane, target, crew string, paths bus.Paths, stderr io.Writer) (window, tmuxstamp.Pane, int) {
	if isPaneID(target) {
		hit, ok := findPane(panes, func(p tmuxstamp.Pane) bool { return p.ID == target })
		if !ok {
			return window{}, tmuxstamp.Pane{}, die(stderr, "no pane %s", target)
		}
		for _, w := range wins {
			if w.id == hit.Window {
				return w, hit, 0
			}
		}
		return window{}, tmuxstamp.Pane{}, die(stderr, "pane %s is not a pane of this crew", target)
	}

	var byBranch []window
	for _, w := range wins {
		if w.branch == target {
			byBranch = append(byBranch, w)
		}
	}
	// A state-only call picks windows by stamp and dispatch row alone, so two for
	// one branch (a copied stamp, a stale window) is refused rather than guessed.
	if len(byBranch) > 1 && paths.Dir == "" {
		return window{}, tmuxstamp.Pane{}, die(stderr, "ambiguous branch '%s' — %d windows carry it", target, len(byBranch))
	}
	if len(byBranch) > 0 {
		return byBranch[0], tmuxstamp.Pane{}, 0
	}

	var matches []window
	for _, w := range wins {
		if w.name == target {
			matches = append(matches, w)
		}
	}
	if len(matches) > 1 {
		return window{}, tmuxstamp.Pane{}, die(stderr, "ambiguous codename '%s' — matches %s; pass a branch or %%pane",
			target, joinBranches(matches, ","))
	}
	if len(matches) == 1 {
		return matches[0], tmuxstamp.Pane{}, 0
	}

	// No window carries it: the bus knows the target, or nothing does. The arm
	// joined the branches with "," and read the result's shape as its message.
	var branches []string
	for _, r := range resolve.Resolve(resolve.Rows(paths.Log), target, crew) {
		branches = append(branches, r.Branch)
	}
	dbr := strings.Join(branches, ",")
	switch {
	case strings.Contains(dbr, ","):
		return window{}, tmuxstamp.Pane{}, die(stderr, "ambiguous target '%s' — matches %s; pass a branch or %%pane", target, dbr)
	case dbr != "":
		return window{}, tmuxstamp.Pane{}, die(stderr, "no live pane for '%s' (branch %s) — its window is gone", target, dbr)
	}
	return window{}, tmuxstamp.Pane{}, die(stderr, "no worker matches '%s'", target)
}

// leadPane is the pane the grid stamped lead, else the window's first pane. The
// exit status is non-zero for a window with no pane at all.
func leadPane(ctx context.Context, c tmux.Client, panes []tmuxstamp.Pane, winID string) (tmuxstamp.Pane, int, error) {
	var first tmuxstamp.Pane
	for _, p := range panes {
		if p.Window != winID {
			continue
		}
		if first.ID == "" {
			first = p
		}
		role, _, err := tmuxstamp.PaneOption(ctx, c, p.ID, "@crew_role")
		if err != nil {
			return tmuxstamp.Pane{}, exitFailure, err
		}
		if role == "lead" {
			return p, 0, nil
		}
	}
	if first.ID == "" {
		return tmuxstamp.Pane{}, exitFailure, nil
	}
	return first, 0, nil
}

// isPaneID is the arm's `'%'[0-9]*` case: a `%` and then a digit, so `%lead` or
// a bare `%` is a name.
func isPaneID(target string) bool {
	return len(target) >= 2 && target[0] == '%' && target[1] >= '0' && target[1] <= '9'
}

// findPane is the first pane matching, and whether there was one at all.
func findPane(panes []tmuxstamp.Pane, match func(tmuxstamp.Pane) bool) (tmuxstamp.Pane, bool) {
	for _, p := range panes {
		if match(p) {
			return p, true
		}
	}
	return tmuxstamp.Pane{}, false
}

func joinBranches(wins []window, sep string) string {
	branches := make([]string, len(wins))
	for i, w := range wins {
		branches[i] = w.branch
	}
	return strings.Join(branches, sep)
}

// poolName is `_identity <branch> | jq -r .name`: the codename the pool gives
// this branch, which is what `dispatch` stamped when it made the window.
func poolName(branch string) string {
	return identity.At(identity.Slot(branch)).Name
}
