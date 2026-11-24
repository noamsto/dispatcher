package reap

import (
	"github.com/noamsto/dispatcher/crew/internal/bus"
)

// indexCrewDocs is the dispatch rows of the layout-2 crews the repo index
// lists that name this repo, read once on first call: a crew dispatching in
// several repos keeps the other repos' rows out. A repo without a state root
// or an index yields none.
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
