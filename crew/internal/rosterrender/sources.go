package rosterrender

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/git"
	"github.com/noamsto/dispatcher/crew/internal/repos"
)

// rediscoverEvery is how long the daemon keeps the repos it joined before it
// walks the sweep roots again.
const rediscoverEvery = 300 * time.Second

// source is one repo bus the crew has rows on. slug is the origin's raw
// owner/name, empty when the origin names no GitHub repo. wedge is the name its
// wedge is drawn and keyed by, distinct among the sources of one list.
type source struct {
	paths              bus.Paths
	label, slug, wedge string
}

// sourceResult is one repo's planets with its legend counts and live count.
type sourceResult struct {
	planets []planet
	lg      legend
	live    int64
}

// clone is a copy no later in-place change to the planets or their moons can
// write back through.
func (r sourceResult) clone() sourceResult {
	r.planets = slices.Clone(r.planets)
	for i := range r.planets {
		r.planets[i].moons = slices.Clone(r.planets[i].moons)
	}
	return r
}

// discovery is the daemon's memory of the last source list, and of each joined
// repo's last good result, so a bus that tears for a pass keeps its wedge and
// its workers in the live count.
type discovery struct {
	at   time.Time
	srcs []source
	good map[string]sourceResult
}

func (d *discovery) remember(common string, r sourceResult) {
	if d == nil {
		return
	}
	if d.good == nil {
		d.good = map[string]sourceResult{}
	}
	d.good[common] = r.clone()
}

func (d *discovery) recall(common string) (sourceResult, bool) {
	if d == nil {
		return sourceResult{}, false
	}
	r, ok := d.good[common]
	return r.clone(), ok
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
	nameWedges(srcs)
	if d := o.disc; d != nil {
		d.at, d.srcs = now, srcs
		maps.DeleteFunc(d.good, func(common string, _ sourceResult) bool {
			return !slices.ContainsFunc(srcs, func(s source) bool { return s.paths.Common == common })
		})
	}
	return srcs
}

// nameWedges sets each source's wedge name: the repo's own name, then
// owner·name for the repos that share one, then a ·2, ·3 suffix in source order
// for any that still do. A wedge is named by the repo, not owner/name, because
// clean would drop the slash and run the two together.
func nameWedges(srcs []source) {
	names := make([]string, len(srcs))
	count := map[string]int{}
	for i, s := range srcs {
		_, name, _ := strings.Cut(s.label, "/")
		names[i] = clean(cmp.Or(name, s.label))
		count[names[i]]++
	}
	for i, s := range srcs {
		if owner, name, ok := strings.Cut(s.label, "/"); ok && count[names[i]] > 1 {
			names[i] = clean(owner + "·" + name)
		}
	}
	used := map[string]bool{}
	for i := range srcs {
		wedge := names[i]
		for n := 2; used[wedge]; n++ {
			suffix := "·" + strconv.Itoa(n)
			stem := []rune(names[i])
			wedge = string(stem[:min(len(stem), maxText-utf8.RuneCountInString(suffix))]) + suffix
		}
		used[wedge] = true
		srcs[i].wedge = wedge
	}
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
