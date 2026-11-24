package reap

import (
	"github.com/noamsto/dispatcher/crew/internal/bus"
)

// indexCrewDocs is the rows of the layout-2 crews the repo index lists, read
// once on first call. A repo without a state root or an index yields none.
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
		for _, id := range bus.IndexCrews(paths.State, repoTag(paths.Common)) {
			if c, err := paths.Crew(id); err == nil && c.Layout == 2 {
				docs = append(docs, eventDocs(c.Log)()...)
			}
		}
		return docs
	}
}
