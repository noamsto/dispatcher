package overseer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/git"
	"github.com/noamsto/dispatcher/crew/internal/repos"
)

// stateRoot is the layout-2 state root of o's environment; Options may be zero
// in production, so a nil Getenv reads the process environment.
func stateRoot(o Options) string {
	getenv := o.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	return bus.StateRoot(getenv)
}

// layout2Crew is the state-root bus of crew when it classifies layout 2 here.
// ok is false when the crew is not layout 2 (the caller takes the layout-1
// path); with ok, a non-empty reason is the refusal.
func layout2Crew(o Options, crew string) (paths bus.Paths, m bus.Meta, reason string, ok bool) {
	state := stateRoot(o)
	if state == "" {
		return bus.Paths{}, bus.Meta{}, "", false
	}
	c, err := bus.StatePaths(state).Crew(crew)
	if bus.IsRefusal(err) {
		return bus.Paths{}, bus.Meta{}, err.Error(), true
	}
	if err != nil || c.Layout != 2 {
		return bus.Paths{}, bus.Meta{}, "", false
	}
	m, err = bus.ReadMeta(state, crew)
	if err != nil {
		return bus.Paths{}, bus.Meta{}, err.Error(), true
	}
	if m.Visiting {
		return bus.Paths{}, bus.Meta{}, fmt.Sprintf("crew %s is visiting this host; its dispatcher runs on %s", crew, m.HomeHost), true
	}
	return bus.StatePaths(state), m, "", true
}

// layout2Target is layout2Crew plus the check that repo names the crew's home.
func layout2Target(ctx context.Context, repo, crew string, o Options) (bus.Paths, string, bool) {
	paths, m, reason, ok := layout2Crew(o, crew)
	if !ok || reason != "" {
		return bus.Paths{}, reason, ok
	}
	if reason := homeMismatch(ctx, repo, crew, m, o); reason != "" {
		return bus.Paths{}, reason, true
	}
	return paths, "", true
}

// homeMismatch is why repo, a git common dir or a repo label, is not the home
// of m's crew, "" when it is.
func homeMismatch(ctx context.Context, repo, crew string, m bus.Meta, o Options) string {
	if homeMatches(ctx, repo, m, o) {
		return ""
	}
	home := repos.NoRepoLabel
	if m.HomeRepo != nil {
		home = *m.HomeRepo
	}
	return fmt.Sprintf("crew %s is homed in %s, not %s", crew, home, repo)
}

func homeMatches(ctx context.Context, repo string, m bus.Meta, o Options) bool {
	if m.HomeRepo == nil {
		return repo == repos.NoRepoLabel
	}
	if filepath.IsAbs(repo) {
		tag, err := bus.RepoTag(repo)
		return err == nil && tag == *m.HomeRepo
	}
	origin, _ := git.Origin(ctx, o.Git, *m.HomeRepo)
	return repos.Label(*m.HomeRepo, origin) == repo
}

// paneCrew2 is the one non-visiting layout-2 crew whose crews/<id>/pane file
// names pane, else the ambiguity reason; both are "" when none does.
func paneCrew2(state, pane string) (string, string) {
	var named []string
	for _, m := range bus.LiveMetas(state) {
		if m.Visiting {
			continue
		}
		data, err := os.ReadFile(bus.StoreDir(state, m.ID) + "/pane")
		if err == nil && strings.TrimSpace(string(data)) == pane {
			named = append(named, m.ID)
		}
	}
	switch len(named) {
	case 0:
		return "", ""
	case 1:
		return named[0], ""
	}
	return "", fmt.Sprintf("pane %s has no @crew_id stamp, and crews %s all name it as their dispatcher pane", pane, strings.Join(named, ", "))
}
