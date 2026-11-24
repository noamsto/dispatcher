package fleet

import (
	"io"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/repos"
)

// noRepo labels the group of layout-2 crews whose meta names no home repo.
const noRepo = "(no repo)"

// layout2Crew is a live layout-2 crew: its home repo's realpath ("" for none)
// and where its dir and log are.
type layout2Crew struct{ id, home, dir, log string }

// readLayout2 is every crew under the state root with a readable meta.json. One
// whose meta.json is there but unreadable is left out with a stderr note: its
// file pins it to layout 2, so its rows and holds can't be placed anywhere else.
func readLayout2(state string, stderr io.Writer) []layout2Crew {
	if state == "" {
		return nil
	}
	metas, bad := bus.ScanMetas(state)
	for _, b := range bad {
		if stderr != nil {
			say(stderr, "crew fleet: crew %s: unreadable meta.json (%s) — not listed\n", b.ID, oneLine(b.Err))
		}
	}
	var out []layout2Crew
	for _, m := range metas {
		bc, err := bus.StatePaths(state).Crew(m.ID)
		if err != nil || bc.Layout != 2 {
			continue
		}
		c := layout2Crew{id: m.ID, dir: bc.Dir, log: bc.Log}
		if m.HomeRepo != nil {
			c.home = *m.HomeRepo
		}
		out = append(out, c)
	}
	return out
}

// knownCommons are the repos a layout-2 home can be matched to: those that
// still exist, whether or not their repo bus has events.
func knownCommons(found []repos.Repo) []string {
	var out []string
	for _, r := range found {
		if r.Status == repos.StatusOK || r.Status == repos.StatusNoBus {
			out = append(out, r.Common)
		}
	}
	return out
}

// groupLayout2 groups crews by the common dir they are shown under: a known
// repo's own path when the crew's home is that repo (compared by realpath),
// else the home itself, "" for a crew with none.
func groupLayout2(crews []layout2Crew, known []string) map[string][]layout2Crew {
	byTag := map[string]string{}
	for _, c := range known {
		if tag, err := bus.RepoTag(c); err == nil && byTag[tag] == "" {
			byTag[tag] = c
		}
	}
	groups := map[string][]layout2Crew{}
	for _, c := range crews {
		common := c.home
		if known, ok := byTag[c.home]; ok {
			common = known
		}
		groups[common] = append(groups[common], c)
	}
	return groups
}

// withoutCrews is events minus the rows of the given layout-2 crews: a crew
// pinned to layout 2 has no live state in a repo bus.
func withoutCrews(events []bus.Event, crews []layout2Crew) []bus.Event {
	if len(crews) == 0 {
		return events
	}
	ids := map[string]bool{}
	for _, c := range crews {
		ids[c.id] = true
	}
	out := make([]bus.Event, 0, len(events))
	for _, e := range events {
		if !ids[e.CrewID] {
			out = append(out, e)
		}
	}
	return out
}
