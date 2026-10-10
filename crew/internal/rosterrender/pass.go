package rosterrender

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/noamsto/dispatcher/crew/internal/bus"
)

// pass is `_rr_pass <crew> <cdir> <no_open> [role_rows]`: one render. Write the
// diagram when its text changed, publish when written or when the recorded pane
// is not the one last published to, and return the live count.
//
// The three reads that build the picture are fallible and return before anything
// is written, so a malformed bus can never replace a good diagram. The write
// itself keeps the arm's asymmetry: `_rr_put`'s refusal (a symlinked or
// non-regular target) fails this branch of the `elif`, which means no publish —
// but the pass still prints its live count, because the arm reaches `printf`
// either way.
//
// The timers are not here. The 60s rebuild backstop, the quiet exit and the
// signature that decides whether to render at all belong to the loop, which owns
// the clock.
func pass(ctx context.Context, o Options, paths bus.Paths, cdir, crew string, noOpen bool, roleRows []string) (int64, error) {
	m, err := model(paths, crew, float64(o.wallSec()), roleRows, o.Roster)
	if err != nil {
		return 0, err
	}
	live, err := liveCount(m)
	if err != nil {
		return 0, err
	}
	text, err := renderText(m)
	if err != nil {
		return 0, err
	}

	t := target(paths.Common, crew, o.RosterDir, o.Clock.Now)
	pane := record(cdir, filePane)
	if !paneIDRe.MatchString(pane) {
		pane = ""
	}

	// `[ -f "$target" ] && [ ! -L "$target" ] && cmp -s`: a symlink never counts as
	// the current diagram, so it falls through to the write — which refuses it.
	if st, err := os.Lstat(t); err == nil && st.Mode().IsRegular() && st.Mode()&os.ModeSymlink == 0 {
		if prev, err := os.ReadFile(t); err == nil && string(prev) == text {
			// Unchanged: publish only for a pane nobody has been published to yet.
			if pane != "" && pane != record(cdir, filePublished) {
				publish(ctx, o.Probes, cdir, t, pane, noOpen)
			}
			return live, nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(t), 0o755); err != nil {
		return live, nil
	}
	if err := put(t, text, o.Stderr); err != nil {
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
// own, and the size catches everything the bus can say.
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
