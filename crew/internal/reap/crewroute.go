package reap

import (
	"context"
	"os"
	"strconv"
	"strings"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/gitguard"
	"github.com/noamsto/dispatcher/crew/internal/tmux"
	"github.com/noamsto/dispatcher/crew/internal/wtgit"
)

// indexCrewDocs is the dispatch rows of the layout-2 crews the repo index
// lists that name this repo, read once on first call. A crew dispatching in
// several repos has the other repos' rows filtered out.
func indexCrewDocs(paths bus.Paths) func() []map[string]any {
	var docs []map[string]any
	var done bool
	return func() []map[string]any {
		if done {
			return docs
		}
		done = true
		if paths.State == "" {
			return nil
		}
		tag := repoTag(paths.Common)
		for _, id := range bus.IndexCrews(paths.State, tag) {
			c, err := paths.Crew(id)
			if err != nil || c.Layout != 2 {
				continue
			}
			for _, doc := range eventDocs(c.Log)() {
				if doc["kind"] == "dispatch" && doc["repo"] == tag {
					docs = append(docs, doc)
				}
			}
		}
		return docs
	}
}

// l2Owner is the crew that owns branch now, and whether one does. A branch an
// earlier read this run saw a layout-2 crew own counts as layout 2 even when
// the claim has vanished or moved to another crew since: why is then the
// refusal, as it is when the crews' rows cannot be read.
func (r *run) l2Owner(branch string) (crew string, l2 bool, why string) {
	rows, err := bus.RepoCrewRows(r.paths)
	if err != nil {
		return "", true, "layout-2 rows unreadable: " + err.Error()
	}
	seen, was := r.l2Seen[branch]
	r.noteSeen(rows.Crews)
	crew, l2 = rows.Crews[branch]
	switch {
	case !l2 && was:
		return "", true, "layout-2 claim vanished"
	case l2 && was && crew != seen:
		return "", true, "layout-2 owner changed"
	}
	return crew, l2, ""
}

// noteSeen records each branch's first layout-2 owner seen this run.
func (r *run) noteSeen(crews map[string]string) {
	if len(crews) == 0 {
		return
	}
	if r.l2Seen == nil {
		r.l2Seen = map[string]string{}
	}
	for b, id := range crews {
		if _, ok := r.l2Seen[b]; !ok {
			r.l2Seen[b] = id
		}
	}
}

// l2Check is the layout-2 validation gate: a branch a layout-2 crew owns is
// touched only when its worktree record names this repo and that crew, and
// every window in windows is stamped with that crew. Provenance is recomputed
// on every call, so a row appended after the run started is still validated.
// It returns why the branch is kept, and false.
func (r *run) l2Check(ctx context.Context, branch, wt string, windows []string) (string, bool) {
	crew, l2, why := r.l2Owner(branch)
	if why != "" {
		return why, false
	}
	if !l2 {
		return "", true
	}
	if why := r.l2Record(wt, crew); why != "" {
		return why, false
	}
	return stampedCrew(ctx, r.p.Tmux, windows, crew)
}

// l2Windows is l2Check's window part alone, for the windows about to be killed.
func (r *run) l2Windows(ctx context.Context, branch string, windows []string) (string, bool) {
	crew, l2, why := r.l2Owner(branch)
	if why != "" {
		return why, false
	}
	if !l2 {
		return "", true
	}
	return stampedCrew(ctx, r.p.Tmux, windows, crew)
}

// l2Record is why wt's worktree record does not vouch for crew in this repo,
// "" when it does: line 2 is compared the way TrustedCwd compares it.
func (r *run) l2Record(wt, crew string) string {
	rec, err := wtgit.AnchorPath(wt)
	if err != nil {
		return "no worktree record"
	}
	if fi, err := os.Lstat(rec); err != nil || !fi.Mode().IsRegular() {
		return "worktree record is not a regular file"
	}
	data, err := os.ReadFile(rec)
	if err != nil {
		return "worktree record is unreadable"
	}
	lines := strings.Split(string(data), "\n")
	want, err := gitguard.RealpathMissing(r.paths.Common + "/crew")
	if err != nil || len(lines) < 2 || lines[1] != want {
		return "worktree record names another repo"
	}
	if len(lines) < 5 || lines[4] != crew {
		return "worktree record names another crew than " + crew
	}
	return ""
}

// stampedCrew reports whether every window's @crew_id is crew.
func stampedCrew(ctx context.Context, tm tmux.Client, windows []string, crew string) (string, bool) {
	for _, w := range windows {
		out, _ := tm.Run(ctx, "display-message", "-p", "-t", w, "#{@crew_id}")
		if id := strings.TrimRight(out, "\n"); id != crew {
			return "window " + w + " @crew_id is " + strconv.Quote(id) + ", not " + crew, false
		}
	}
	return "", true
}
