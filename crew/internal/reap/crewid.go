package reap

import (
	"context"
	"encoding/json"
	"os"
	"strings"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/tmux"
	"github.com/noamsto/dispatcher/crew/internal/wtgit"
)

// rowCrewID is the crew id a reap or release row carries: the first valid one
// of the window stamps, the worktree record's fifth line, then the newest
// dispatch row of the branch across docs and indexDocs (the layout-2 crews the
// repo index lists; may be nil), by ts with ties to docs. The record is read
// only when no stamp names a crew, and the logs only when neither does. It must
// run before the windows are killed and the record removed.
func rowCrewID(ctx context.Context, tm tmux.Client, windows []string, wt, branch string, docs, indexDocs func() []map[string]any) string {
	for _, w := range windows {
		out, _ := tm.Run(ctx, "display-message", "-p", "-t", w, "#{@crew_id}")
		if id := strings.TrimRight(out, "\n"); bus.ValidCrewID(id) {
			return id
		}
	}
	if id := recordCrewID(wt); bus.ValidCrewID(id) {
		return id
	}
	id, ts := newestDispatch(docs(), branch)
	if indexDocs == nil {
		return id
	}
	if iid, its := newestDispatch(indexDocs(), branch); iid != "" && (id == "" || its > ts) {
		return iid
	}
	return id
}

// newestDispatch is the valid crew id and ts of the last dispatch row of
// branch in docs, "" when there is none.
func newestDispatch(docs []map[string]any, branch string) (string, float64) {
	var newest map[string]any
	for _, doc := range docs {
		if doc["kind"] == "dispatch" && doc["branch"] == branch {
			newest = doc
		}
	}
	id, _ := newest["crew_id"].(string)
	if !bus.ValidCrewID(id) {
		return "", 0
	}
	var ts float64
	switch v := newest["ts"].(type) {
	case json.Number:
		ts, _ = v.Float64()
	case float64:
		ts = v
	}
	return id, ts
}

// rowLog is the log a row carrying crew id goes to; an id that does not resolve
// keeps the repo log.
func rowLog(paths bus.Paths, id string) string {
	if c, err := paths.Crew(id); err == nil {
		return c.Log
	}
	return paths.Log
}

// recordCrewID is line 5 of wt's worktree record, "" when the record is
// missing, not a regular file or has no fifth line.
func recordCrewID(wt string) string {
	rec, err := wtgit.AnchorPath(wt)
	if err != nil {
		return ""
	}
	if fi, err := os.Lstat(rec); err != nil || !fi.Mode().IsRegular() {
		return ""
	}
	data, err := os.ReadFile(rec)
	if err != nil {
		return ""
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) < 5 {
		return ""
	}
	return lines[4]
}

// eventDocs adapts a bus read to rowCrewID's docs.
func eventDocs(log string) func() []map[string]any {
	return func() []map[string]any {
		events, _ := bus.ReadEvents(log)
		docs := make([]map[string]any, 0, len(events))
		for _, ev := range events {
			if doc, ok := ev.Doc.(map[string]any); ok {
				docs = append(docs, doc)
			}
		}
		return docs
	}
}

// repoTag is bus.RepoTag with an unresolvable common dir read as no tag.
func repoTag(common string) string {
	tag, _ := bus.RepoTag(common)
	return tag
}
