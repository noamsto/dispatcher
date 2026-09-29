package ui

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// layout.go holds the dimension math shared across views, so every view
// derives sizes from these instead of re-deriving its own -N tax.

// clamp bounds v to [lo, hi]. Callers pass hi < lo when there is no room;
// clamp returns lo in that case (an empty/degenerate range), never a value
// outside [lo, max(lo,hi)].
func clamp(v, lo, hi int) int {
	if hi < lo {
		hi = lo
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func max0(v int) int {
	if v < 0 {
		return 0
	}
	return v
}

// truncateLine clips s to at most w display cells, appending an ellipsis
// when it had to cut. Every uncontrolled string (paths, values, verdicts)
// goes through this rather than a raw slice.
func truncateLine(s string, w int) string {
	return ansi.Truncate(s, max0(w), "…")
}

// padRight pads s with spaces to width w (display cells), never trimming —
// callers truncate first if s might already exceed w.
func padRight(s string, w int) string {
	p := w - ansi.StringWidth(s)
	if p <= 0 {
		return s
	}
	return s + strings.Repeat(" ", p)
}

// normalizeFrame is the root's final safety net (belt-and-suspenders on top
// of each view already sizing itself correctly): force s to exactly h lines,
// each truncated to at most w display cells, so a resize can never leave a
// frame short or overflowing even at degenerate sizes like 20x5.
func normalizeFrame(s string, h, w int) []string {
	h = max0(h)
	w = max0(w)
	var lines []string
	if s != "" {
		lines = strings.Split(s, "\n")
	}
	out := make([]string, h)
	for i := 0; i < h; i++ {
		if i < len(lines) {
			out[i] = truncateLine(lines[i], w)
		} else {
			out[i] = ""
		}
	}
	return out
}

// windowOffset centers a viewport of h rows around cursor within total,
// clamped to [0, max(0, total-h)] — shared by every cursor-navigable list
// (the runs/roster tables below; the settings tree inlines the same math
// since it predates this helper).
func windowOffset(cursor, total, h int) int {
	if total <= h {
		return 0
	}
	return clamp(cursor-h/2, 0, total-h)
}

// tableWidths and padRow lay out a simple space-gapped table (header +
// rows), the display-width-aware TUI counterpart of the once renderer's
// rune-counted jq `pad_row`/table width helpers.
func tableWidths(rows [][]string) []int {
	if len(rows) == 0 {
		return nil
	}
	widths := make([]int, len(rows[0]))
	for _, row := range rows {
		for c, cell := range row {
			if c >= len(widths) {
				continue
			}
			if wd := ansi.StringWidth(cell); wd > widths[c] {
				widths[c] = wd
			}
		}
	}
	return widths
}

// padRow pads cells to widths; left[c] controls left- vs right-alignment;
// the last cell never trails with padding.
func padRow(cells []string, widths []int, left []bool) string {
	end := len(cells) - 1
	parts := make([]string, len(cells))
	for c, cell := range cells {
		p := 0
		if c < len(widths) {
			p = widths[c] - ansi.StringWidth(cell)
		}
		if p < 0 {
			p = 0
		}
		switch {
		case c < len(left) && !left[c]:
			parts[c] = strings.Repeat(" ", p) + cell
		case c == end:
			parts[c] = cell
		default:
			parts[c] = cell + strings.Repeat(" ", p)
		}
	}
	return strings.Join(parts, "  ")
}

// alignRight lays left out against a right-aligned badge within width w
// (display cells): left is truncated then padded to fill the remaining
// room (one gap column reserved when there is a badge), badge is truncated
// only if it alone would overflow w. leftOut's width plus badgeOut's width
// always sums to exactly w (for w>0), so leftOut + badgeOut renders at
// exactly w cells with no further padding needed. Both are plain (unstyled)
// — callers style the returned pieces' *content* and concatenate the
// results; they never re-render an already-styled string, so no style
// layer nests inside another.
func alignRight(left, badge string, w int) (leftOut, badgeOut string) {
	w = max0(w)
	badgeOut = truncateLine(badge, w)
	bw := ansi.StringWidth(badgeOut)
	avail := w - bw
	if badgeOut != "" {
		avail--
	}
	avail = max0(avail)
	leftOut = padRight(truncateLine(left, avail), avail)
	if badgeOut != "" {
		leftOut += " "
	}
	return leftOut, badgeOut
}
