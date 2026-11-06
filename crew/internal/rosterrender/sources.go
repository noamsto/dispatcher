package rosterrender

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/git"
	"github.com/noamsto/dispatcher/crew/internal/repos"
)

// rediscoverEvery is how long the daemon keeps the repos it joined before it
// walks the sweep roots again.
const rediscoverEvery = 300 * time.Second

// source is one repo bus the crew has rows on. slug is the origin's raw
// owner/name, empty when the origin names no GitHub repo.
type source struct {
	paths       bus.Paths
	label, slug string
}

// discovery is the daemon's memory of the last source list.
type discovery struct {
	at   time.Time
	srcs []source
}

// sources is the local repo first, then every discovered repo whose bus holds an
// event for the crew. With a discovery record the list is reused for
// rediscoverEvery, so the 2 s tick never re-walks the sweep roots.
func (o Options) sources(ctx context.Context, local bus.Paths, crew string) []source {
	now := o.Clock.Now()
	if d := o.disc; d != nil && d.srcs != nil && now.Sub(d.at) < rediscoverEvery {
		return d.srcs
	}
	srcs := []source{o.sourceOf(ctx, local)}
	if o.Discover != nil {
		for _, r := range o.Discover(o.Stderr) {
			if r.Status != repos.StatusOK || sameDir(r.Common, local.Common) {
				continue
			}
			p := bus.Paths{Common: r.Common, Dir: r.Common + "/crew", Log: r.Common + "/crew/events.jsonl"}
			events, err := bus.ReadEventsTolerant(p.Log)
			if err != nil {
				say(o.Stderr, "crew: roster-render: %s: skipped (%s)\n", r.Common, oneLine(err))
				continue
			}
			if !hasCrew(events, crew) {
				continue
			}
			srcs = append(srcs, o.sourceOf(ctx, p))
		}
	}
	if o.disc != nil {
		o.disc.at, o.disc.srcs = now, srcs
	}
	return srcs
}

func (o Options) sourceOf(ctx context.Context, p bus.Paths) source {
	origin := ""
	if o.Git != nil {
		origin, _ = git.Origin(ctx, o.Git, p.Common)
	}
	s := source{paths: p, label: repos.Label(p.Common, origin)}
	if name := repos.Name(p.Common, origin); strings.Contains(name, "/") {
		s.slug = name
	}
	return s
}

func hasCrew(events []bus.Event, crew string) bool {
	for _, e := range events {
		if e.CrewID == crew {
			return true
		}
	}
	return false
}

// sameDir is whether two paths name one directory, symlinks followed when both
// resolve.
func sameDir(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && ra == rb
}

func oneLine(err error) string { return strings.Join(strings.Fields(err.Error()), " ") }

// joinedSig is the signature part for the repos joined besides the local one: a
// change on any of their buses, or in their role panes, redraws.
func joinedSig(others []source, panes, crew string) string {
	var b strings.Builder
	for _, s := range others {
		fmt.Fprintf(&b, "|%s|%d|%s", s.paths.Common, busSize(s.paths), joinRows(roleRows(panes, s.paths.Dir, crew)))
	}
	return b.String()
}
