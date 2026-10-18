package reap

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/filekind"
	"github.com/noamsto/dispatcher/crew/internal/gitguard"
	"github.com/noamsto/dispatcher/crew/internal/lock"
	"github.com/noamsto/dispatcher/crew/internal/roster"
	"github.com/noamsto/dispatcher/crew/internal/wtgit"
)

// Options is what `crew reap` needs beyond its arguments and streams.
type Options struct {
	Probes Probes
	Getenv func(string) string
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
	r.repo = originGitHubRepo(ctx)
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
	if !r.p.HaveGh() {
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
	r.live = r.p.Tmux(ctx, "list-panes", "-a", "-F", "#{window_id}\t#{pane_id}\t#{pane_current_command}\t#{pane_current_path}")
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
	cmd := exec.CommandContext(ctx, "git", "worktree", "list", "--porcelain")
	cmd.Stderr = r.stderr
	out, err := cmd.Output()
	if err != nil {
		return "", shellStatus(err)
	}
	return roster.WorktreePath(string(out), branch), 0
}

// shellStatus is the shell's `$?` for a finished command: 127 when it never
// started.
func shellStatus(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if code := ee.ExitCode(); code > 0 {
			return code
		}
		return 1
	}
	return 127
}

// slugPrefix and originGitHubRepo are `_origin_github_repo`: `_origin_repo`'s
// sed (`s#(git@|https://)([^/:]+)[/:]##; s#\.git$##`), only for a github.com
// origin — the slug drops the host, and pr_urls are only ever github.com.
var slugPrefix = regexp.MustCompile(`(git@|https://)[^/:]+[/:]`)

func originGitHubRepo(ctx context.Context) string {
	out, _ := exec.CommandContext(ctx, "git", "config", "--get", "remote.origin.url").Output()
	url := strings.TrimRight(string(out), "\n")
	if !strings.HasPrefix(url, "https://github.com/") && !strings.HasPrefix(url, "git@github.com:") {
		return ""
	}
	lines := strings.Split(url, "\n")
	for i, line := range lines {
		if loc := slugPrefix.FindStringIndex(line); loc != nil {
			line = line[:loc[0]] + line[loc[1]:]
		}
		lines[i] = strings.TrimSuffix(line, ".git")
	}
	return strings.Join(lines, "\n")
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
	grace, err := strconv.ParseInt(r.idle, 10, 64)
	if err != nil {
		grace = math.MaxInt64
	}
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

// orphans reports windows stamped for this bus whose worktree is gone —
// report only: a human may be using one.
func (r *run) orphans(ctx context.Context) int {
	rows := r.p.Tmux(ctx, "list-windows", "-a", "-F", "#{window_id}\t#{@crew_branch}\t#{@crew_dir}")
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
		r.note("window " + wid + " is stamped " + branch + " but its worktree is gone — not killed")
	}
	return 0
}
