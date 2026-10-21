package reap

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/filekind"
	"github.com/noamsto/dispatcher/crew/internal/gh"
	"github.com/noamsto/dispatcher/crew/internal/git"
	"github.com/noamsto/dispatcher/crew/internal/gitguard"
	"github.com/noamsto/dispatcher/crew/internal/lock"
	"github.com/noamsto/dispatcher/crew/internal/roster"
	"github.com/noamsto/dispatcher/crew/internal/wtgit"
)

// Options is what `crew reap` needs beyond its arguments and streams.
type Options struct {
	Probes Probes
	// PID owns the reap and ratings-sweep locks.
	PID int
	// Self is this binary, run as `reap-autosweep` for the async ratings sweep.
	Self string
	// RateRun is the in-process `crew rate` sweep.
	RateRun func(args []string, paths bus.Paths, cwd string, stdout, stderr io.Writer) int
}

// run is one `crew reap`: the arm's flags and globals, shared by the candidate
// loop, `_reap_remove` and the --discard block.
type run struct {
	flags
	paths bus.Paths
	// trusted is the directory the process changed into (`_wt_trusted_cwd`);
	// every cwd-discovered git, gh and gtrash runs from it. reapCwd is the
	// caller's own cwd, which still decides "it is the current worktree".
	trusted, reapCwd string
	// repo is `reap_repo`: origin's owner/name for a github.com origin, else "".
	repo           string
	p              Probes
	o              Options
	stdout, stderr io.Writer
	// live is the `list-panes` rows read once before the candidate loop.
	live string
	// The --discard state `_reap_remove` re-checks against: the saved patch
	// ("" when there was nothing to save), the status and the scratch-index
	// tree it was saved from.
	discardPatch, discardSt, discardTree string
	clean                                *cleaner
}

// Run is crew.sh's `reap)` arm: reclaim the window and worktree of workers
// whose PR has landed (or whose claimed issues closed). No crew filter: the
// workers worth reaping are the ones from earlier dispatcher sessions. The PR,
// not elapsed time, is the gate — a worker sits in `done` for as long as its PR
// takes to merge, and a time-based sweep would delete live work.
//
// Run changes the process's working directory to the trusted cwd, as the arm
// `cd`'d.
func Run(ctx context.Context, req Request, paths bus.Paths, cwd string, stdout, stderr io.Writer, o Options) int {
	r := &run{flags: newFlags(req), paths: paths, reapCwd: cwd, p: o.Probes, o: o, stdout: stdout, stderr: stderr, clean: &cleaner{}}
	// Never run reap's git from a worker's worktree (#633).
	dir, err := wtgit.TrustedCwd(paths.Common, cwd, stderr)
	if err == nil {
		err = os.Chdir(dir)
	}
	if err != nil {
		if !errors.Is(err, gitguard.ErrRefused) {
			_, _ = fmt.Fprintf(stderr, "crew: reap: %v\n", err)
		}
		return 1
	}
	r.trusted = dir
	// Resolved once, after the cwd is trusted: bus pr_urls are checked against it.
	r.repo = originGitHubRepo(ctx, r.p.Git)
	if st, err := os.Stat(paths.Log); err != nil || !st.Mode().IsRegular() {
		if r.discard != "" {
			return r.refuse("latest status is none")
		}
		return 0
	}

	stop := r.clean.notify()
	defer stop()
	defer r.clean.cleanup()

	if code := r.autosweep(); code != 0 {
		return code
	}
	if code, done := r.lockReap(); done {
		return code
	}
	if r.discard != "" {
		return r.discardBranch(ctx)
	}
	if code := r.idleRelease(ctx); code != 0 {
		return code
	}
	if !gh.Installed(ctx, r.p.Gh) {
		r.note("needs gh")
		return 0
	}
	if code := r.orphans(ctx); code != 0 {
		return code
	}
	rows, err := r.latest("")
	if err != nil {
		return r.foldFailed(err)
	}
	if len(rows) == 0 {
		r.note("nothing done to reap")
		return 0
	}
	// Panes running an engine, to tell "finished worker" from "someone is in
	// there right now". Path-keyed like roster: lazytmux rewrites window names.
	r.live = r.p.tmuxOut(ctx, "list-panes", "-a", "-F", "#{window_id}\t#{pane_id}\t#{pane_current_command}\t#{pane_current_path}")
	reaped := 0
	for _, row := range rows {
		if row.branch == "" {
			continue
		}
		removed, code := r.consider(ctx, row)
		if code != 0 {
			return code
		}
		if removed {
			reaped++
		}
	}
	if !r.dry && reaped == 0 {
		r.note("nothing reclaimed")
	}
	return 0
}

// say reports an outcome, always.
func (r *run) say(msg string) { _, _ = fmt.Fprintf(r.stdout, "crew reap: %s\n", msg) }

// note is kept-worker bookkeeping, silenced under --quiet so the dispatch call
// site stays silent unless something actually happened.
func (r *run) note(msg string) {
	if !r.quiet {
		r.say(msg)
	}
}

// refuse is a --discard refusal; the arm exits 1.
func (r *run) refuse(why string) int {
	_, _ = fmt.Fprintf(r.stderr, "crew reap: refusing --discard %s — %s\n", r.discard, why)
	return 1
}

// foldFailed is the port doc's fold rule for the candidate fold: one
// `crew: reap: <log>: …` line, 2 unreadable or 5 corrupt.
func (r *run) foldFailed(err error) int {
	msg, code := bus.JQFailure(err)
	if errors.Is(err, bus.ErrNoLog) {
		msg, code = "No such file or directory", 2
	}
	_, _ = fmt.Fprintf(r.stderr, "crew: reap: %s: %s\n", r.paths.Log, msg)
	return code
}

// guardOK reports a direct gitguard call the way `crew-go git-guard` did for
// the arm: a refusal has printed its reason, any other error (an unreadable
// baseline) is printed here. Either way the guard did not pass.
func (r *run) guardOK(err error) bool {
	if err == nil {
		return true
	}
	if !errors.Is(err, gitguard.ErrRefused) {
		_, _ = fmt.Fprintf(r.stderr, "refusing git: %v\n", err)
	}
	return false
}

// worktreePath is `git worktree list --porcelain | awk …` from the trusted
// cwd. A failed git is the arm's `set -e` abort: its status comes back
// non-zero.
func (r *run) worktreePath(ctx context.Context, branch string) (string, int) {
	out, err := r.p.Git.Run(ctx, "", "worktree", "list", "--porcelain")
	if err != nil {
		r.gitStderr(err)
		return "", shellStatus(err)
	}
	return roster.WorktreePath(out, branch), 0
}

// gitStderr passes a failed git's captured stderr to the reap's.
func (r *run) gitStderr(err error) {
	if ge := (*git.ExitError)(nil); errors.As(err, &ge) {
		_, _ = io.WriteString(r.stderr, ge.Stderr)
	}
}

// shellStatus is the shell's `$?` for a finished command: 127 when it never
// started.
func shellStatus(err error) int {
	if err == nil {
		return 0
	}
	var ee interface{ ExitCode() int }
	if errors.As(err, &ee) {
		if code := ee.ExitCode(); code > 0 {
			return code
		}
		return 1
	}
	return 127
}

// originGitHubRepo is `_origin_github_repo`: `_origin_repo` only for a
// github.com origin — the slug drops the host, and pr_urls are only ever
// github.com.
func originGitHubRepo(ctx context.Context, g git.Client) string {
	url, _ := g.Run(ctx, "", "config", "--get", "remote.origin.url")
	if !strings.HasPrefix(url, "https://github.com/") && !strings.HasPrefix(url, "git@github.com:") {
		return ""
	}
	return git.RepoSlug(url)
}

// lockReap takes `reap.lock.d`: two reaps racing a removal on one tree strand
// it. dispatch waits (it reads branch state right after its reap); the stream
// passes --no-wait. done means the arm exits with code here.
func (r *run) lockReap() (code int, done bool) {
	ld := r.paths.Dir + "/reap.lock.d"
	owner := strconv.Itoa(r.o.PID)
	for waited := 0; !lock.Acquire(ld, owner); waited++ {
		// A skipped --discard would read as done, its state neither saved nor reclaimed.
		if r.nowait {
			if r.discard != "" {
				return r.refuse("another reap is running"), true
			}
			r.note("another reap is running — skipped")
			return 0, true
		}
		if waited >= 120 {
			if r.discard != "" {
				return r.refuse("another reap is still running after 120s"), true
			}
			r.note("another reap is still running after 120s — skipped")
			return 0, true
		}
		r.p.Sleep(time.Second)
	}
	r.clean.hold(ld)
	return 0, false
}

// idleRelease kills the WINDOW of a session that reached a terminal state more
// than --idle seconds ago and still sits there; the worktree stays, because a
// worker legitimately sits in `done` for as long as its PR takes to merge
// (#17). A release row that cannot be written aborts the reap, as the arm's
// `set -e` did.
func (r *run) idleRelease(ctx context.Context) int {
	grace := r.grace()
	d := Deps{Common: r.paths.Common, Log: r.paths.Log, Say: r.say, Note: r.note, P: r.p}
	for _, row := range r.idleRows() {
		if row[0] == "" {
			continue
		}
		ts, _ := strconv.ParseFloat(row[3], 64)
		if _, err := ReleaseWindows(ctx, d, row[0], row[1], row[2], int64(ts), grace, r.dry); err != nil {
			_, _ = fmt.Fprintf(r.stderr, "crew: reap: %v\n", err)
			return 1
		}
	}
	return 0
}

// orphans reports windows stamped for this bus whose worktree is gone. Once the
// branch's PR has landed the window is closed too: `gh pr merge
// --delete-branch` removes the worktree itself, and both kill paths key on the
// worktree, so nothing else reaches such a window. A window with no PR, an open
// PR, an unreadable PR state, one a person is using, or this reap's own window
// keeps the list-only note.
func (r *run) orphans(ctx context.Context) int {
	self := r.selfWindow(ctx)
	rows := r.p.tmuxOut(ctx, "list-windows", "-a", "-F", "#{window_id}\t#{@crew_branch}\t#{@crew_dir}")
	for line := range strings.SplitSeq(rows, "\n") {
		f := fields(line, 3)
		wid, branch, dir := f[0], f[1], f[2]
		if wid == "" || branch == "" || dir != r.paths.Dir {
			continue
		}
		wt, code := r.worktreePath(ctx, branch)
		if code != 0 {
			return code
		}
		if wt != "" && filekind.IsDir(wt) {
			continue
		}
		num, state := r.orphanPR(ctx, branch)
		keep := state == "" || wid == self
		if !keep {
			keep = r.orphanInUse(ctx, wid)
		}
		if keep {
			r.note("window " + wid + " is stamped " + branch + " but its worktree is gone — not killed")
			continue
		}
		what := branch + ", PR #" + num + " " + state + ", worktree already gone"
		if r.dry {
			r.note("would close window " + wid + " (" + what + ")")
			continue
		}
		_, _ = r.p.Tmux.Run(ctx, "kill-window", "-t", wid)
		r.note("closed window " + wid + " (" + what + ")")
	}
	return 0
}

// selfWindow is the window this reap is running in, or "" outside tmux. It is
// never killed: Occupants excludes it for the same reason — the fold would die
// with it.
func (r *run) selfWindow(ctx context.Context) string {
	pane := os.Getenv("TMUX_PANE")
	if pane == "" {
		return ""
	}
	return r.p.tmuxOut(ctx, "display-message", "-p", "-t", pane, "#{window_id}")
}

// orphanPR is the orphan pass's PR check: the branch's own PR, found the way
// reclaim finds it (`findPR`, gh's `pr list --head`) and read the way `prGate`
// reads it. Anything but MERGED or CLOSED comes back stateless, so the caller
// keeps the window. There is no head-ancestor check: `prGate` needs one because
// a PR vouches for a worktree, and here the PR is looked up under the branch
// stamped on the window and only a window is destroyed. `findPR` takes gh's
// first non-fork row, so a branch name reused for new work whose only PR is an
// old merged one closes its window — the worktree is already gone and the branch
// itself is left alone.
func (r *run) orphanPR(ctx context.Context, branch string) (num, state string) {
	c := &candidate{latestRow: latestRow{branch: branch}, pr: "-"}
	if !r.findPR(ctx, c) || !prURLInRepo(c.pr, r.repo) {
		return "", ""
	}
	state, _ = r.p.Gh.Run(ctx, "pr", "view", c.pr, "--json", "state", "--jq", ".state")
	switch state {
	case "MERGED", "CLOSED":
	default:
		return "", ""
	}
	// prURLInRepo has anchored the url on /pull/<digits>.
	return c.pr[strings.LastIndex(c.pr, "/")+1:], state
}

// orphanInUse reports whether a person is evidently using a window that has no
// worktree: the active window of an attached client, output within the grace, or
// an engine pane that is not provably idle — the guards release applies, minus
// its transcript check, which is keyed on the worktree path that is gone here.
// The caller's note stays the generic one, so the why is not reported.
func (r *run) orphanInUse(ctx context.Context, wid string) bool {
	if _, ok := windowsInUse(ctx, r.p, []string{wid}, r.grace()); ok {
		return true
	}
	panes := r.p.tmuxOut(ctx, "list-panes", "-a", "-F", "#{window_id}\t#{pane_id}\t#{pane_current_command}")
	_, busy := framesBusy(ctx, r.p, []Occupant{windowOccupant(panes, wid, "")})
	return busy
}
