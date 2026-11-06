package rosterrender

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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
	slots slotState
	sc    scene
	lg    legend
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
}

// buildFrame reads every repo bus the crew has rows on into a frame without
// writing anything. The reads are fallible and the arrangement is not, so an
// error means nothing was drawn. A repo joined besides the local one that cannot
// be read keeps its last good result when the discovery record has one, and is
// skipped otherwise, with a note either way; the local one's failure fails the
// frame.
func buildFrame(ctx context.Context, o Options, paths bus.Paths, cdir, crew string, localRoles []string) (frame, error) {
	now := float64(o.wallSec())
	windows := sync.OnceValue(func() map[string]bool { return roster.Windows(ctx, o.Probes.Tmux) })
	panes := sync.OnceValue(func() string { return o.rolePanes(ctx) })

	var (
		ps    []planet
		lg    legend
		live  int64
		wants []ciWant
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
		} else if i > 0 {
			o.disc.remember(src.paths.Common, res)
		}
		for _, p := range res.planets {
			if p.class == classPROpen && src.slug != "" && prOf(p.prURL, src.slug) {
				wants = append(wants, ciWant{URL: p.prURL, Repo: src.slug})
			}
		}
		ps = append(ps, res.planets...)
		lg.add(res.lg)
		live += res.live
	}
	at := o.Clock.Now()
	applyCI(ps, refreshCI(ctx, o.Gh, cdir, wants, at, o.Stderr), at)
	tally(ps, &lg)
	slots, sc := arrange(loadSlots(filepath.Join(cdir, fileSlots)), ps)
	return frame{slots: slots, sc: sc, lg: lg, live: live}, nil
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
		ps[i].repo, ps[i].repoLabel = src.paths.Common, src.wedge
	}
	return sourceResult{planets: ps, lg: lg, live: live}, nil
}

// pass is `_rr_pass <crew> <cdir> <no_open> [role_rows]`: one render. Draw the
// diagram when its DOT text changed, publish when written or when the recorded
// pane is not the one last published to, and return the live count.
//
// The reads that build the picture are fallible and return before anything is
// written, so a malformed bus can never replace a good diagram. Graphviz is not:
// a failed run is logged, leaves the old picture and still returns the count,
// because an error here would freeze the daemon's quiet window. A refused `put`
// likewise stops the publish and still returns the count.
func pass(ctx context.Context, o Options, paths bus.Paths, cdir, crew string, noOpen bool, roleRows []string) (int64, error) {
	fr, err := buildFrame(ctx, o, paths, cdir, crew, roleRows)
	if err != nil {
		return 0, err
	}
	live := fr.live
	dot := renderDOT(fr.sc, fr.lg)

	t := target(paths.Common, crew, o.RosterDir, o.Clock.Now)
	pane := record(cdir, filePane)
	if !paneIDRe.MatchString(pane) {
		pane = ""
	}

	// A symlink never counts as the current diagram, so it falls through to the
	// write — which refuses it.
	prev, _ := os.ReadFile(filepath.Join(cdir, fileDot))
	if st, err := os.Lstat(t); err == nil && st.Mode().IsRegular() && string(prev) == dot {
		// Unchanged: publish only for a pane nobody has been published to yet.
		if pane != "" && pane != record(cdir, filePublished) {
			publish(ctx, o.Probes, cdir, t, pane, noOpen)
		}
		return live, nil
	}
	svg, err := o.Probes.Graphviz(ctx, []byte(dot))
	if err != nil {
		say(o.Stderr, "crew: roster-render: %v\n", err)
		return live, nil
	}
	if err := os.MkdirAll(filepath.Dir(t), 0o755); err != nil {
		return live, nil
	}
	if put(t, string(finishSVG(svg)), o.Stderr) != nil ||
		put(filepath.Join(cdir, fileDot), dot, o.Stderr) != nil ||
		saveSlots(filepath.Join(cdir, fileSlots), fr.slots, o.Stderr) != nil {
		return live, nil
	}
	if pane != "" {
		publish(ctx, o.Probes, cdir, t, pane, noOpen)
	}
	return live, nil
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

// loopSig is `rr_newsig="$rr_size|$(cat pane)|$rr_roles"`: the bus's size, the
// recorded pane and the role rows. A part of the diagram the bus never mentions —
// a role pane that started, or a dispatcher pane that moved — changes it on its
// own, and the size catches everything the bus can say. Repos joined besides
// this one add their parts through joinedSig.
func loopSig(size int64, cdir string, roleRows []string) string {
	return fmt.Sprintf("%d|%s|%s", size, record(cdir, filePane), joinRows(roleRows))
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
