package rosterrender

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/git"
	"github.com/noamsto/dispatcher/crew/internal/roster"
)

// passProbes is one pass's outside reads. The worktree list is shared because
// the fold re-reads it for every `exited` row, and `sync.OnceValues` memoizes the
// failure with the result, so `resolveExited` and the liveness rule see one probe
// outcome per pass.
//
// The tmux window list is the pass's too, so every repo shares one probe.
func (o Options) passProbes(ctx context.Context, common string, windows func() map[string]bool) (roster.Probes, roster.Liveness) {
	wt := sync.OnceValues(func() (string, error) { return git.Worktrees(ctx, o.Git, "", common) })
	return roster.Probes{Tmux: o.Probes.Tmux, Worktrees: wt},
		roster.Liveness{Windows: windows, Worktrees: wt}
}

type frame struct {
	b     board
	slots slotState
	live  int64
}

// add sums the worker counts of another repo's legend.
func (l *legend) add(o legend) {
	l.working += o.working
	l.blocked += o.blocked
	l.prOpen += o.prOpen
	l.done += o.done
	l.failed += o.failed
	l.holds += o.holds
	l.queued += o.queued
}

// buildFrame reads every repo bus the crew has rows on into a frame without
// writing anything. The reads are fallible and the arrangement is not, so an
// error means nothing was drawn. A repo joined besides the local one that cannot
// be read keeps its last good result when the discovery record has one, and is
// skipped otherwise, with a note either way; the local one's failure fails the
// frame. The budget strip's engine readings are always resolved; the fleet
// budget only when withBudget is set (the map, a fresh process per selection,
// skips it).
func buildFrame(ctx context.Context, o Options, paths bus.Paths, cdir, crew string, localRoles []string, withBudget bool) (frame, error) {
	now := float64(o.wallSec())
	windows := sync.OnceValue(func() map[string]bool { return roster.Windows(ctx, o.Probes.Tmux) })
	panes := sync.OnceValue(func() string { return o.rolePanes(ctx) })

	var (
		ps     []planet
		lg     legend
		live   int64
		wants  []ciWant
		holds  []queuedHold
		queued []queuedTask
	)
	for i, src := range o.sources(ctx, paths, crew) {
		roles := localRoles
		if i > 0 {
			roles = roleRows(panes(), src.paths.Dir, crew)
		}
		res, err := o.sourcePlanets(ctx, src, crew, now, roles, windows)
		if err != nil && i == 0 {
			return frame{}, err
		}
		if err != nil {
			last, ok := o.disc.recall(src.paths.Common)
			if !ok {
				say(o.Stderr, "crew: roster-render: %s: skipped (%s)\n", src.paths.Common, oneLine(err))
				continue
			}
			say(o.Stderr, "crew: roster-render: %s: skipped (%s); showing its last good frame\n", src.paths.Common, oneLine(err))
			res = last
			// A rediscovery may have renamed the lane since the frame was saved.
			for j := range res.planets {
				res.planets[j].repoLabel = src.lane
			}
			for j := range res.queued {
				res.queued[j].repoLabel = src.lane
			}
		} else if i > 0 {
			o.disc.remember(src.paths.Common, res)
		}
		for _, p := range res.planets {
			if p.class == classPROpen && src.slug != "" && prOf(p.prURL, src.slug) {
				wants = append(wants, ciWant{URL: p.prURL, Repo: src.slug})
			}
		}
		for j := range res.planets {
			res.planets[j].home = i == 0
		}
		ps = append(ps, res.planets...)
		lg.add(res.lg)
		live += res.live
		holds = append(holds, res.holds...)
		queued = append(queued, res.queued...)
	}
	at := o.Clock.Now()
	applyCI(ps, refreshCI(ctx, o.Gh, cdir, wants, at, o.Stderr), at)
	tally(ps, &lg)
	for _, p := range ps {
		if p.host == "" && (p.class == classWorking || p.class == classBlocked) {
			lg.weight += tierWeight(p.tier)
		}
	}
	lg.engines = o.engineReadings(now)
	if withBudget {
		lg.budget = o.disc.fleetBudget(ctx, at, o.Budget)
	}
	b, slots := buildBoard(ps, holds, queued, loadSlots(filepath.Join(cdir, fileSlots)), lg)
	return frame{b: b, slots: slots, live: live}, nil
}

// engineReadings is the budget strip's quota windows: the cache's when a file is
// configured, otherwise every fixed engine as unknown.
func (o Options) engineReadings(now float64) []engineReading {
	if o.BudgetFile != "" {
		return readEngineWindows(o.BudgetFile, int64(now))
	}
	out := make([]engineReading, len(engines))
	for i, name := range engines {
		out[i] = engineReading{name: name}
	}
	return out
}

// sourcePlanets is one repo's planets, each carrying the repo it came from, with
// its legend counts and live count.
func (o Options) sourcePlanets(ctx context.Context, src source, crew string, now float64, roles []string, windows func() map[string]bool) (sourceResult, error) {
	rp, rule := o.passProbes(ctx, src.paths.Common, windows)
	m, err := model(ctx, src.paths, crew, now, roles, rp, rule)
	if err != nil {
		return sourceResult{}, err
	}
	live, err := liveCount(m)
	if err != nil {
		return sourceResult{}, err
	}
	ps, lg := buildPlanets(m, now)
	for i := range ps {
		ps[i].repo, ps[i].repoLabel = src.paths.Common, src.lane
	}
	queued := buildQueued(m)
	for i := range queued {
		queued[i].repo, queued[i].repoLabel = src.paths.Common, src.lane
	}
	return sourceResult{planets: ps, lg: lg, live: live, holds: buildHolds(m), queued: queued}, nil
}

// pass is `_rr_pass <crew> <cdir> <no_open> [role_rows]`: one render. Draw the
// board at aspect when its svg changed, publish when written or when the
// recorded pane is not the one last published to, and return the live count.
//
// The reads that build the picture are fallible and return before anything is
// written, so a malformed bus can never replace a good diagram. A refused `put`
// stops the publish and still returns the count, because an error here would
// freeze the daemon's quiet window.
func pass(ctx context.Context, o Options, paths bus.Paths, cdir, crew string, noOpen bool, aspect float64, roleRows []string) (int64, error) {
	fr, err := buildFrame(ctx, o, paths, cdir, crew, roleRows, true)
	if err != nil {
		return 0, err
	}
	live := fr.live
	svg := renderBoardSVG(fr.b, layoutBoard(fr.b, aspect), headerInfo{crew: crew, strip: stripText(fr.b.counts)}, selection{})

	t := target(paths.Common, crew, o.RosterDir, o.Clock.Now)
	pane := record(cdir, filePane)
	if !paneIDRe.MatchString(pane) {
		pane = ""
	}

	// A symlink never counts as the current diagram, so it falls through to the
	// write — which refuses it.
	prev, _ := os.ReadFile(filepath.Join(cdir, fileDrawn))
	if st, err := os.Lstat(t); err == nil && st.Mode().IsRegular() && string(prev) == svg {
		// Unchanged: publish only for a pane nobody has been published to yet.
		if pane != "" && pane != record(cdir, filePublished) {
			publish(ctx, o.Probes, cdir, t, pane, noOpen)
		}
		return live, nil
	}
	if err := os.MkdirAll(filepath.Dir(t), 0o755); err != nil {
		return live, nil
	}
	if put(t, svg, o.Stderr) != nil ||
		put(filepath.Join(cdir, fileDrawn), svg, o.Stderr) != nil ||
		saveSlots(filepath.Join(cdir, fileSlots), fr.slots, o.Stderr) != nil {
		return live, nil
	}
	dropLegacyLegend(t)
	if pane != "" {
		publish(ctx, o.Probes, cdir, t, pane, noOpen)
	}
	return live, nil
}

// dropLegacyLegend removes the `.legend.svg` key an earlier build left beside the
// diagram: the carousel watches that directory, so it would sit there frozen with
// the counts it last drew. Only a regular file goes.
func dropLegacyLegend(diagram string) {
	path := strings.TrimSuffix(diagram, ".svg") + ".legend.svg"
	if st, err := os.Lstat(path); err == nil && st.Mode().IsRegular() {
		_ = os.Remove(path)
	}
}

// viewer is the size of the carousel pane beside the recorded dispatcher pane;
// ok is false when no pane is recorded or tmux cannot say.
func (o Options) viewer(ctx context.Context, cdir string) (cols, rows int, ok bool) {
	pane := record(cdir, filePane)
	if !paneIDRe.MatchString(pane) {
		return 0, 0, false
	}
	return o.Probes.ViewerSize(ctx, pane)
}

// defaultAspect is the board's shape when the viewer's size is unknown.
const defaultAspect = 16.0 / 10

// paneAspect is the pixel aspect of a cols x rows pane, a cell being taken as
// twice as tall as it is wide.
func paneAspect(cols, rows int, ok bool) float64 {
	if !ok || cols <= 0 || rows <= 0 {
		return defaultAspect
	}
	return float64(cols) / float64(2*rows)
}

// busSize is the loop's `rr_size=0; [ ! -f "$log" ] || rr_size=$(wc -c <"$log")`:
// a bus that does not exist yet is a zero, not a missing signature part.
func busSize(paths bus.Paths) int64 {
	st, err := os.Stat(paths.Log)
	if err != nil {
		return 0
	}
	return st.Size()
}

// loopSig is `rr_newsig="$rr_size|$(cat pane)|$rr_roles"` plus the board's
// aspect: the bus's size, the recorded pane, the role rows and the viewer's
// shape. A part of the diagram the bus never mentions — a role pane that
// started, a dispatcher pane that moved, a resized viewer — changes it on its
// own, and the size catches everything the bus can say. Repos joined besides
// this one add their parts through joinedSig.
func loopSig(size int64, cdir string, roleRows []string, aspect float64) string {
	return fmt.Sprintf("%d|%s|%s|%g", size, record(cdir, filePane), joinRows(roleRows), aspect)
}

func joinRows(rows []string) string {
	out := ""
	for i, r := range rows {
		if i > 0 {
			out += "\n"
		}
		out += r
	}
	return out
}
