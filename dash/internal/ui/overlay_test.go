package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
)

func TestOverlayComposite(t *testing.T) {
	orig := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(orig) })

	red := lipgloss.NewStyle().Foreground(lipgloss.Color("#f38ba8"))
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
