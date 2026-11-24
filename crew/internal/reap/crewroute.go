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

// l2Check is the layout-2 validation gate: a branch a layout-2 crew owns is
// touched only when its worktree record names this repo and that crew, and
// every window in windows is stamped with that crew. Provenance is recomputed
// on every call, so a row appended after the run started is still validated.
// It returns why the branch is kept, and false.
func (r *run) l2Check(ctx context.Context, branch, wt string, windows []string) (string, bool) {
	crew, ok := bus.RepoCrewRows(r.paths).Crews[branch]
	if !ok {
		return "", true
	}
	if why := r.l2Record(wt, crew); why != "" {
		return why, false
	}
	return stampedCrew(ctx, r.p.Tmux, windows, crew)
}

// l2Windows is l2Check's window part alone, for the windows about to be killed.
func (r *run) l2Windows(ctx context.Context, branch string, windows []string) (string, bool) {
	crew, ok := bus.RepoCrewRows(r.paths).Crews[branch]
	if !ok {
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
