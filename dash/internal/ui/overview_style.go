package ui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"

	vm "github.com/noamsto/dispatcher/dash/internal/view"
)

// Overview palette, set by applyPalette (theme.go). Spans carry colours; the profile downsamples them, so an
// ASCII profile renders the same layout without any SGR.
var (
	colText      lipgloss.Color
	colDim       lipgloss.Color
	colBorderOff lipgloss.Color
	colSelBg     lipgloss.Color
	colPillFg    lipgloss.Color
	colTeal      lipgloss.Color
	colPurple    lipgloss.Color
	colAmber     lipgloss.Color
	colRed       lipgloss.Color
	colGreen     lipgloss.Color
	colBlue      lipgloss.Color
)

func fg(c lipgloss.Color) lipgloss.Style { return lipgloss.NewStyle().Foreground(c) }

const (
	pillCapLeft  = ""
	pillCapRight = ""
)

type pillMode int

const (
	pillRound pillMode = iota
	pillSquare
)

// pillModeOf resolves the pill style once: square when asked for, on the Linux
// console (no powerline glyphs) or without colour; round otherwise.
func pillModeOf(getenv func(string) string, profile termenv.Profile) pillMode {
	if getenv("CREW_DASH_PILLS") == "square" || getenv("TERM") == "linux" || profile == termenv.Ascii {
		return pillSquare
	}
	return pillRound
}

func pillWidth(textW int, mode pillMode) int {
	if mode == pillRound {
		return textW + 4
	}
	return textW + 2
}

func pill(text string, fgc, bg lipgloss.Color, mode pillMode) string {
	body := lipgloss.NewStyle().Foreground(fgc).Background(bg).Bold(true).Render(" " + text + " ")
	if mode == pillSquare {
		return body
	}
	capSt := fg(bg)
	return capSt.Render(pillCapLeft) + body + capSt.Render(pillCapRight)
}

const sparkGlyphs = "▁▂▃▄▅▆▇█"

// sparkline draws the newest width buckets scaled to the largest one. Missing
// activity (nil) is a run of dots; a short series is left-padded with dots.
func sparkline(counts []int, width int) string {
	if width <= 0 {
		return ""
	}
	if counts == nil {
		return strings.Repeat("·", width)
	}
	if len(counts) > width {
		counts = counts[len(counts)-width:]
	}
	peak := 0
	for _, v := range counts {
		peak = max(peak, v)
	}
	glyphs := []rune(sparkGlyphs)
	var b strings.Builder
	b.WriteString(strings.Repeat("·", width-len(counts)))
	for _, v := range counts {
		level := 0
		if v > 0 {
			level = (v*7 + peak - 1) / peak
		}
		b.WriteRune(glyphs[level])
	}
	return b.String()
}

type trackCell struct{ glyph, status string }

func phaseCells(p *vm.Phase) []trackCell {
	if p == nil || len(p.Steps) == 0 {
		return nil
	}
	cells := make([]trackCell, len(p.Steps))
	for i, s := range p.Steps {
		g := "○"
		switch s.Status {
		case "done":
			g = "●"
		case "current":
			g = "◉"
		case "skipped":
			g = "-"
		}
		cells[i] = trackCell{glyph: g, status: s.Status}
	}
	return cells
}

func phaseTrack(p *vm.Phase) string {
	cells := phaseCells(p)
	if cells == nil {
		return "?"
	}
	parts := make([]string, len(cells))
	for i, c := range cells {
		parts[i] = c.glyph
	}
	return strings.Join(parts, "─")
}

func trackStyle(status string) lipgloss.Style {
	switch status {
	case "done":
		return fg(colGreen)
	case "current":
		return fg(colTeal).Bold(true)
	}
	return fg(colDim)
}

// spans accumulates one display line of styled spans up to limit cells. The
// row background goes into every span's own style, so a selected row never
// needs its escape codes rewritten afterwards.
type spans struct {
	sb    strings.Builder
	w     int
	limit int
	bg    lipgloss.TerminalColor
}

func newSpans(limit int, bg lipgloss.TerminalColor) *spans {
	if bg == nil {
		bg = lipgloss.NoColor{}
	}
	return &spans{limit: max0(limit), bg: bg}
}

// add appends text, truncated to the cells left before the limit. Every
// Overview row is built here, so this is where worker-controlled text loses
// its control characters and escape sequences.
func (s *spans) add(text string, st lipgloss.Style) *spans {
	text = cleanText(text)
	room := s.limit - s.w
	if room <= 0 || text == "" {
		return s
	}
	if ansi.StringWidth(text) > room {
		text = ansi.Truncate(text, room, "…")
	}
	s.w += ansi.StringWidth(text)
	s.sb.WriteString(st.Background(s.bg).Render(text))
	return s
}

func (s *spans) col(text string, width int, st lipgloss.Style, right bool) *spans {
	t := ansi.Truncate(cleanText(text), max0(width), "…")
	pad := strings.Repeat(" ", max0(width-ansi.StringWidth(t)))
	if right {
		t = pad + t
	} else {
		t += pad
	}
	return s.add(t, st)
}

func (s *spans) padTo(c int) *spans {
	return s.add(strings.Repeat(" ", max0(min(c, s.limit)-s.w)), lipgloss.NewStyle())
}

func (s *spans) String() string {
	s.padTo(s.limit)
	return s.sb.String()
}

type panelSpec struct {
	title, count string
	color        lipgloss.Color
	focused      bool
}

// renderPanel draws a rounded panel of exactly w x h cells with the title pill
// in the top border and the count right of it. Body lines are truncated and
// padded to the inner width.
func renderPanel(w, h int, sp panelSpec, mode pillMode, body []string) []string {
	frame := lipgloss.NewStyle().Border(lipgloss.RoundedBorder())
	iw := w - frame.GetHorizontalFrameSize()
	ih := h - frame.GetVerticalFrameSize()
	if iw < 1 || ih < 0 {
		blank := strings.Repeat(" ", max0(w))
		out := make([]string, max0(h))
		for i := range out {
			out[i] = blank
		}
		return out
	}
	b := lipgloss.RoundedBorder()
	bc := colBorderOff
	if sp.focused {
		bc = sp.color
	}
	bs := fg(bc)

	title := sp.title
	if sp.focused {
		title = "▸ " + title
	}
	const tail = 1
	textMax := iw - tail - 1 - pillWidth(0, mode)
	var head string
	headW := 0
	if textMax > 0 {
		title = ansi.Truncate(title, textMax, "…")
		head = pill(title, colPillFg, sp.color, mode)
		headW = pillWidth(ansi.StringWidth(title), mode)
	}
	count := ""
	countW := 0
	if sp.count != "" && iw-headW-tail-(ansi.StringWidth(sp.count)+2) >= 1 {
		count = fg(colDim).Render(" " + sp.count + " ")
		countW = ansi.StringWidth(sp.count) + 2
	}
	fill := iw - headW - countW - tail
	top := bs.Render(b.TopLeft) + head + bs.Render(strings.Repeat(b.Top, max0(fill))) + count +
		bs.Render(b.Top+b.TopRight)

	out := make([]string, 0, h)
	out = append(out, top)
	left, right := bs.Render(b.Left), bs.Render(b.Right)
	for i := range ih {
		line := ""
		if i < len(body) {
			line = ansi.Truncate(body[i], iw, "…")
		}
		out = append(out, left+padRight(line, iw)+right)
	}
	if h >= 2 {
		out = append(out, bs.Render(b.BottomLeft+strings.Repeat(b.Bottom, iw)+b.BottomRight))
	}
	return out[:min(len(out), h)]
}
