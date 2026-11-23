package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"

	vm "github.com/noamsto/dispatcher/dash/internal/view"
)

func TestOverlayComposite(t *testing.T) {
	orig := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(orig) })

	red := lipgloss.NewStyle().Foreground(lipgloss.Color(mochaPalette.red))
	row := red.Render(strings.Repeat("a", 30))
	base := []string{row, row, row, row}
	box := []string{"+--------+", "|  box   |", "+--------+"}
	const w, x, y = 30, 5, 1

	out := composite(base, box, x, y, w)
	if len(out) != len(base) {
		t.Fatalf("composite returned %d lines, want %d", len(out), len(base))
	}
	for i, l := range out {
		if got := ansi.StringWidth(l); got > w {
			t.Errorf("line %d is %d cells, want <= %d", i, got, w)
		}
	}
	if out[0] != base[0] {
		t.Errorf("the line above the box changed")
	}
	want := strings.Repeat("a", x) + "|  box   |" + strings.Repeat("a", w-x-10)
	if got := ansi.Strip(out[2]); got != want {
		t.Errorf("row 2 = %q, want %q", got, want)
	}
	// Left and right base segments keep their own colour.
	const sgr = "\x1b[38;2;243;139;168m"
	boxAt := strings.Index(out[2], "|  box   |")
	if !strings.Contains(out[2][:boxAt], sgr) {
		t.Errorf("left segment lost its colour: %q", out[2])
	}
	if !strings.Contains(out[2][boxAt+len("|  box   |"):], sgr) {
		t.Errorf("right segment lost its colour: %q", out[2])
	}
}

func TestOverlayCompositeClipsAtRightEdge(t *testing.T) {
	base := []string{strings.Repeat("a", 12), strings.Repeat("a", 12)}
	out := composite(base, []string{"XXXXXXXX"}, 8, 0, 12)
	if got := ansi.Strip(out[0]); got != "aaaaaaaaXXXX" {
		t.Errorf("clipped row = %q", got)
	}
}

func TestOverlayCompositePadsShortBase(t *testing.T) {
	out := composite([]string{"ab"}, []string{"XX"}, 4, 0, 10)
	if got := ansi.Strip(out[0]); got != "ab  XX" {
		t.Errorf("short base row = %q", got)
	}
}

// A wide character cut by either box edge must not widen the row.
func TestOverlayCompositeWideCharEdges(t *testing.T) {
	const w = 30
	base := []string{strings.Repeat("漢", w/2)}
	for _, tc := range []struct{ x, bw int }{{4, 9}, {6, 9}, {5, 10}, {4, 10}} {
		out := composite(base, []string{strings.Repeat("X", tc.bw)}, tc.x, 0, w)
		if got := ansi.StringWidth(out[0]); got != w {
			t.Errorf("x=%d bw=%d: row is %d cells, want %d: %q", tc.x, tc.bw, got, w, ansi.Strip(out[0]))
		}
	}
}

func TestDetailEventTextColumnIsStable(t *testing.T) {
	const text = "pushed fix"
	render := func(now, ts int64) string {
		ov := vm.OverviewView{
			GeneratedAt: now,
			Workers:     []vm.OverviewWorker{{Branch: "feat/x"}},
			Events:      []vm.EventRow{{TS: ts, Worker: "worker:feat/x", Text: text}},
		}
		for _, l := range detailLines(ov, 0, 80) {
			if p := ansi.Strip(l); strings.Contains(p, text) {
				return p
			}
		}
		t.Fatal("no event line")
		return ""
	}
	if a, b := render(1000, 990), render(1005, 990); a != b {
		t.Errorf("same bucket differs: %q vs %q", a, b)
	}
	for _, tc := range []struct{ from, to int64 }{{50, 70}, {540, 600}, {50, 3_600_000}, {3_600_000, 3_700_000}} {
		a, b := render(1000+tc.from, 1000), render(1000+tc.to, 1000)
		if ia, ib := ansi.StringWidth(a[:strings.Index(a, text)]), ansi.StringWidth(b[:strings.Index(b, text)]); ia != ib {
			t.Errorf("%ds->%ds: text column %d -> %d: %q vs %q", tc.from, tc.to, ia, ib, a, b)
		}
	}
}
