package cleanup

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/filekind"
	"github.com/noamsto/dispatcher/crew/internal/reap"
	"github.com/noamsto/dispatcher/crew/internal/snooze"
	"github.com/noamsto/dispatcher/crew/internal/wtgit"
)

// previewLines caps each git listing shown per item.
const previewLines = 8

// Ref is an issue or PR and its GitHub state ("unknown" when gh could not say).
type Ref struct {
	Ref   string `json:"ref"`
	State string `json:"state"`
}

// Item is one worktree a human may want to clean up.
type Item struct {
	Repo     string `json:"repo"`
	Branch   string `json:"branch"`
	Codename string `json:"codename,omitempty"`
	Reason   string `json:"reason"`
	Stale    bool   `json:"stale"`
	Worktree string `json:"worktree"`
	State    string `json:"state,omitempty"`
	// AgeS is the seconds since the last status; 0 when the bus has none.
	AgeS     int64    `json:"age_s,omitempty"`
	Issues   []Ref    `json:"issues,omitempty"`
	PR       *Ref     `json:"pr,omitempty"`
	Status   []string `json:"status,omitempty"`
	Unpushed []string `json:"unpushed,omitempty"`
	Diffstat string   `json:"diffstat,omitempty"`

	paths bus.Paths
	cwd   string
	tip   string
	// callerCwd is where the user ran the command; reap changes the process's.
	callerCwd string
}

// repoName is the checkout's directory name, the archive's repo key.
func (it *Item) repoName() string { return filepath.Base(filepath.Dir(it.paths.Common)) }

// gather resolves every repo's candidates into items.
func (o Options) gather(ctx context.Context, buses []bus.Paths, cwds map[string]string, callerCwd string, stderr io.Writer) ([]*Item, int) {
	var items []*Item
	for _, p := range buses {
		cands, code := reap.Candidates(ctx, p, cwds[p.Common], stderr, o.Reap)
		if code != 0 {
			return nil, code
		}
		for _, c := range cands {
			it := o.item(ctx, p, cwds[p.Common], c)
			if it != nil {
				it.callerCwd = callerCwd
				items = append(items, it)
			}
		}
	}
	return items, 0
}

func (o Options) item(ctx context.Context, p bus.Paths, cwd string, c reap.Candidate) *Item {
	if c.Worktree == "" {
		// A tip-past keep names no worktree; the list is of worktrees.
		wt, ok := o.worktreeOf(ctx, p, c.Branch)
		if !ok {
			return nil
		}
		c.Worktree = wt
	}
	it := &Item{
		tip:  o.branchTip(ctx, p, c.Branch),
		Repo: p.Common, Branch: c.Branch, Codename: c.Codename, Reason: c.StaleReason,
		Stale: c.Stale, Worktree: c.Worktree, State: c.State, paths: p, cwd: cwd,
	}
	if !c.Stale && snooze.Active(p.Dir, c.Branch, it.snoozeKey(), o.now()) {
		return nil
	}
	if it.Reason == "" {
		it.Reason = c.Reason
	}
	if c.StatusTS > 0 {
		it.AgeS = max(0, (o.now().UnixMilli()-c.StatusTS)/1000)
	}
	for _, n := range c.Issues {
		it.Issues = append(it.Issues, Ref{"#" + n, o.ghState(ctx, "issue", n)})
	}
	if c.PR != "" {
		it.PR = &Ref{c.PR, o.ghState(ctx, "pr", c.PR)}
	}
	o.fillGit(ctx, it)
	return it
}

func (it *Item) snoozeKey() snooze.Key { return snooze.Key{Tip: it.tip, Worktree: it.Worktree} }

// branchTip is the branch's commit, "" when unreadable.
func (o Options) branchTip(ctx context.Context, p bus.Paths, branch string) string {
	cmd, err := wtgit.CommonCommand(ctx, p.Common, wtgit.Opts{Stderr: io.Discard}, "rev-parse", "--verify", "refs/heads/"+branch)
	if err != nil {
		return ""
	}
	out, _ := cmd.Output()
	return strings.TrimSpace(string(out))
}

// worktreeOf is the branch's worktree from `git worktree list`.
func (o Options) worktreeOf(ctx context.Context, p bus.Paths, branch string) (string, bool) {
	cmd, err := wtgit.CommonCommand(ctx, p.Common, wtgit.Opts{Stderr: io.Discard}, "worktree", "list", "--porcelain")
	if err != nil {
		return "", false
	}
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	var dir string
	for line := range strings.SplitSeq(string(out), "\n") {
		switch {
		case strings.HasPrefix(line, "worktree "):
			dir = strings.TrimPrefix(line, "worktree ")
		case line == "branch refs/heads/"+branch && filekind.IsDir(dir):
			return dir, true
		}
	}
	return "", false
}

// ghState is `gh <kind> view <ref> --json state`, "unknown" when unreadable.
func (o Options) ghState(ctx context.Context, kind, ref string) string {
	state, err := o.Reap.Probes.Gh.Run(ctx, kind, "view", ref, "--json", "state", "--jq", ".state")
	if err != nil || state == "" {
		return "unknown"
	}
	return state
}

// fillGit adds the worktree's status, unpushed commits and diffstat. A read
// that fails leaves its field empty: the item is for deciding, not proving.
func (o Options) fillGit(ctx context.Context, it *Item) {
	admin, err := wtgit.AdminDir(it.paths.Common, it.Worktree)
	if err != nil {
		return
	}
	if st, err := wtgit.Status(ctx, admin, it.Worktree, wtgit.Opts{Stderr: io.Discard}, "--untracked-files=all"); err == nil && st != "" {
		it.Status = strings.Split(st, "\n")
	}
	if cmd, err := wtgit.CommonCommand(ctx, it.paths.Common, wtgit.Opts{Stderr: io.Discard},
		"log", "--oneline", "refs/heads/"+it.Branch, "--not", "--remotes"); err == nil {
		if out, err := cmd.Output(); err == nil && len(out) > 0 {
			it.Unpushed = strings.Split(strings.TrimRight(string(out), "\n"), "\n")
		}
	}
	if cmd, err := wtgit.Command(ctx, admin, it.Worktree, wtgit.Opts{Stderr: io.Discard}, "diff", "--stat", "HEAD", "--"); err == nil {
		if out, err := cmd.Output(); err == nil {
			it.Diffstat = strings.TrimRight(string(out), "\n")
		}
	}
}

// plain replaces control characters, so worker-written text (commit subjects,
// file names, bus values) cannot drive the reviewer's terminal.
func plain(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return '?'
		}
		return r
	}, s)
}

// render is the block a human reads before deciding.
func (it *Item) render(w io.Writer) {
	it = it.sanitized()
	name := it.Branch
	if it.Codename != "" {
		name += " (" + it.Codename + ")"
	}
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }
	p("%s: %s\n", filepath.Base(filepath.Dir(it.Repo)), name)
	p("  reason:   %s\n", it.Reason)
	last := "no status on the bus"
	if it.State != "" {
		last = it.State
		if it.AgeS > 0 {
			last += ", " + age(it.AgeS) + " ago"
		}
	}
	p("  last:     %s\n", last)
	p("  issue/PR: %s\n", it.links())
	p("  worktree: %s\n", it.Worktree)
	listing(w, "uncommitted", it.Status)
	listing(w, "unpushed", it.Unpushed)
	if it.Diffstat != "" {
		p("  diffstat:\n")
		lines := strings.Split(it.Diffstat, "\n")
		for _, l := range lines[:min(len(lines), previewLines)] {
			p("    %s\n", l)
		}
		if len(lines) > previewLines {
			p("    … %d more\n", len(lines)-previewLines)
		}
	}
}

// sanitized is a copy whose text fields are safe to print.
func (it *Item) sanitized() *Item {
	c := *it
	c.State = plain(it.State)
	c.Branch, c.Codename, c.Reason, c.Worktree, c.Diffstat = plain(it.Branch), plain(it.Codename), plain(it.Reason), plain(it.Worktree), plain(it.Diffstat)
	c.Status, c.Unpushed = mapLines(it.Status), mapLines(it.Unpushed)
	return &c
}

func mapLines(in []string) []string {
	out := make([]string, len(in))
	for i, l := range in {
		out[i] = plain(l)
	}
	return out
}

func (it *Item) links() string {
	var parts []string
	for _, i := range it.Issues {
		parts = append(parts, "issue "+i.Ref+" "+i.State)
	}
	if it.PR != nil {
		parts = append(parts, "PR "+it.PR.Ref+" "+it.PR.State)
	} else {
		parts = append(parts, "no PR")
	}
	return strings.Join(parts, ", ")
}

func listing(w io.Writer, what string, lines []string) {
	if len(lines) == 0 {
		return
	}
	_, _ = fmt.Fprintf(w, "  %s: %d\n", what, len(lines))
	for _, l := range lines[:min(len(lines), previewLines)] {
		_, _ = fmt.Fprintf(w, "    %s\n", l)
	}
	if len(lines) > previewLines {
		_, _ = fmt.Fprintf(w, "    … %d more\n", len(lines)-previewLines)
	}
}

// age is a duration in the largest whole unit.
func age(secs int64) string {
	d := time.Duration(secs) * time.Second
	switch {
	case d >= 24*time.Hour:
		return strconv.FormatInt(int64(d/(24*time.Hour)), 10) + "d"
	case d >= time.Hour:
		return strconv.FormatInt(int64(d/time.Hour), 10) + "h"
	default:
		return strconv.FormatInt(int64(d/time.Minute), 10) + "m"
	}
}
