package rolewatch

import (
	"fmt"
	"strconv"
	"strings"
)

// reguardWidth is _rw_reguard_width: dispatch's width guard ran once, and a
// later relayout can collapse the side column again. It is true when it
// attempted a re-guard, at most reguardMax times per assignment so a client too
// small to fit never thrashes.
func (w *watcher) reguardWidth() bool {
	if w.reguardTries >= reguardMax {
		return false
	}
	// select-layout would unzoom a window the user zoomed on purpose.
	if w.tmux("display-message", "-p", "-t", w.win, "#{window_zoomed_flag}") == "1" {
		return false
	}
	width := w.tmux("display-message", "-p", "-t", w.pane, "#{pane_width}")
	if !reUint.MatchString(width) {
		return false
	}
	minCols := w.roleWidthMin()
	if atoi(width) >= atoi(minCols) {
		return false
	}
	w.reguardTries++
	before := w.tmux("display-message", "-p", "-t", w.win, "#{window_layout}")
	w.guardRoleWidth()
	after := w.tmux("display-message", "-p", "-t", w.win, "#{window_layout}")
	fmt.Fprintf(w.o.Stderr, "role-watch: pane %s was %s cols (min %s) mid-run — re-guarded window %s (attempt %d/%d): layout %s -> %s\n",
		w.pane, width, minCols, w.win, w.reguardTries, reguardMax, before, after)
	return true
}

// roleWidthMin is role_width_min: the per-pane floor, 20 unless the window
// sets a whole number.
func (w *watcher) roleWidthMin() string {
	v := w.tmux("show-options", "-w", "-v", "-q", "-t", w.win, "@grid_refit_min_role_cols")
	if !reUint.MatchString(v) {
		return "20"
	}
	return v
}

// guardRoleWidth is guard_role_width: the first role pane below the floor gets
// the main-vertical fallback, and the raw layout goes to stderr.
func (w *watcher) guardRoleWidth() {
	minCols := w.roleWidthMin()
	out := w.tmux("list-panes", "-t", w.win, "-F", "#{pane_id}|#{@crew_role}|#{pane_width}")
	for _, line := range strings.Split(out, "\n") {
		f := fields(line, 3)
		pane, role, width := f[0], f[1], f[2]
		if role == "" || role == "lead" || !reUint.MatchString(width) {
			continue
		}
		if atoi(width) < atoi(minCols) {
			layout := w.tmux("display-message", "-p", "-t", w.win, "#{window_layout}")
			fmt.Fprintf(w.o.Stderr, "dispatch: role pane %s is %s cols (min %s) after refit — window_layout=%s; applying main-vertical fallback\n",
				pane, width, minCols, layout)
			w.layoutGrid()
			return
		}
	}
}

// layoutGrid is layout_grid: main-vertical with the lead pinned to 60%.
func (w *watcher) layoutGrid() {
	w.tmux("set-window-option", "-t", w.win, "main-pane-width", "60%")
	w.tmux("select-layout", "-t", w.win, "main-vertical")
}

// atoi reads a whole number already matched by reUint; an overflow clamps.
func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}
