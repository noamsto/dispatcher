package ui

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"

	"github.com/noamsto/dispatcher/dash/internal/data"
	vm "github.com/noamsto/dispatcher/dash/internal/view"
)

func trueColorProfile(t *testing.T) {
	t.Helper()
	orig := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(orig) })
}

func TestPillRound(t *testing.T) {
	trueColorProfile(t)
	got := pill("ATTENTION", colPillFg, colAmber, pillRound)
	if !strings.Contains(got, pillCapLeft) || !strings.Contains(got, pillCapRight) {
		t.Errorf("round pill lacks caps: %q", got)
	}
	if w, want := lipgloss.Width(got), pillWidth(len("ATTENTION"), pillRound); w != want {
		t.Errorf("width = %d, want %d", w, want)
	}
}

func TestPillSquare(t *testing.T) {
	trueColorProfile(t)
	got := pill("ATTENTION", colPillFg, colAmber, pillSquare)
	if strings.Contains(got, pillCapLeft) || strings.Contains(got, pillCapRight) {
		t.Errorf("square pill has caps: %q", got)
	}
	if w := lipgloss.Width(got); w != len("ATTENTION")+2 {
		t.Errorf("width = %d, want %d", w, len("ATTENTION")+2)
	}
}

func TestPillMode(t *testing.T) {
	env := func(kv map[string]string) func(string) string {
		return func(k string) string { return kv[k] }
	}
	tests := []struct {
		name    string
		env     map[string]string
		profile termenv.Profile
		want    pillMode
	}{
		{"default", map[string]string{"TERM": "xterm-256color"}, termenv.TrueColor, pillRound},
		{"env square", map[string]string{"CREW_DASH_PILLS": "square"}, termenv.TrueColor, pillSquare},
		{"linux console", map[string]string{"TERM": "linux"}, termenv.TrueColor, pillSquare},
		{"ascii profile", nil, termenv.Ascii, pillSquare},
		{"other env value", map[string]string{"CREW_DASH_PILLS": "round"}, termenv.ANSI256, pillRound},
	}
	for _, tc := range tests {
		if got := pillModeOf(env(tc.env), tc.profile); got != tc.want {
			t.Errorf("%s: mode = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestSparkline(t *testing.T) {
	tests := []struct {
		name   string
		counts []int
		width  int
		want   string
	}{
		{"scaled to max", []int{0, 1, 2, 4, 8}, 5, "▁▂▃▅█"},
		{"all zero", []int{0, 0, 0}, 3, "▁▁▁"},
		{"nil is dots", nil, 6, "······"},
		{"newest buckets win", []int{8, 0, 0, 4}, 3, "▁▁█"},
		{"short series pads left", []int{4}, 3, "··█"},
		{"zero width", []int{1}, 0, ""},
	}
	for _, tc := range tests {
		got := sparkline(tc.counts, tc.width)
		if got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
		if w := ansi.StringWidth(got); w != tc.width {
			t.Errorf("%s: width %d, want %d", tc.name, w, tc.width)
		}
	}
}

func TestPanelWidth(t *testing.T) {
	trueColorProfile(t)
	long := strings.Repeat("x", 300)
	for _, mode := range []pillMode{pillRound, pillSquare} {
		for w := 20; w <= 120; w++ {
			for _, h := range []int{2, 3, 7, 12} {
				sp := panelSpec{title: "CHECKLISTS and a very long title", count: "12 of 40 done", color: colTeal, focused: w%2 == 0}
				got := renderPanel(w, h, sp, mode, []string{long, "short", "", long})
				if len(got) != h {
					t.Fatalf("w=%d h=%d: %d lines", w, h, len(got))
				}
				for i, l := range got {
					if lw := ansi.StringWidth(l); lw != w {
						t.Fatalf("w=%d h=%d line %d is %d cells: %q", w, h, i, lw, l)
					}
				}
			}
		}
	}
}

// visibleCells walks s and reports, for each printed cell, whether a
// background SGR was active.
func visibleCells(t *testing.T, s string) (cells []rune, hasBg []bool) {
	t.Helper()
	bg := false
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		if rs[i] != 0x1b {
			cells = append(cells, rs[i])
			hasBg = append(hasBg, bg)
			continue
		}
		j := i + 2
		for j < len(rs) && rs[j] != 'm' {
			j++
		}
		params := strings.Split(string(rs[i+2:j]), ";")
		for k := 0; k < len(params); k++ {
			n, _ := strconv.Atoi(params[k])
			switch n {
			case 0:
				bg = false
			case 48:
				bg = true
			case 38, 58:
				k += 4
			}
		}
		i = j
	}
	return cells, hasBg
}

func TestRowBackground(t *testing.T) {
	trueColorProfile(t)
	const w = 30
	row := newSpans(w, colSelBg).
		add("▌", fg(colTeal)).
		add("1 ", fg(colDim)).
		col("amber", 8, fg(colText).Bold(true), false).
		add("blocked on a prompt that is much too long to fit in the row", fg(colAmber)).
		String()
	cells, hasBg := visibleCells(t, row)
	if len(cells) != w {
		t.Fatalf("row has %d cells, want %d: %q", len(cells), w, row)
	}
	for i, c := range cells {
		if !hasBg[i] {
			t.Errorf("cell %d (%q) has no background", i, c)
		}
	}

	plain := newSpans(w, nil).add("abc", fg(colText)).String()
	_, hasBg = visibleCells(t, plain)
	for i, b := range hasBg {
		if b {
			t.Errorf("unselected cell %d has a background", i)
		}
	}
}

func TestPhaseTrack(t *testing.T) {
	steps := func(statuses ...string) *vm.Phase {
		p := &vm.Phase{}
		for i, s := range statuses {
			p.Steps = append(p.Steps, vm.PhaseStep{Name: fmt.Sprint(i), Status: s})
		}
		return p
	}
	tests := []struct {
		name string
		in   *vm.Phase
		want string
	}{
		{"mixed", steps("done", "skipped", "current", "pending", "pending", "pending"), "●─-─◉─○─○─○"},
		{"all done", steps("done", "done", "done"), "●─●─●"},
		{"nil", nil, "?"},
		{"empty", &vm.Phase{}, "?"},
	}
	for _, tc := range tests {
		if got := phaseTrack(tc.in); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestBudgetBarDollarTextTruncatesBeforePercent(t *testing.T) {
	row := vm.BudgetRow{Label: "claude api-key", UsedPct: 60, Text: "$20.00 left expired", Critical: true}
	wide := ansi.Strip(budgetBar(row, newSpans(70, nil), 70))
	if !strings.Contains(wide, "claude api-key") || !strings.Contains(wide, "60%") || !strings.Contains(wide, "$20.00 left expired") {
		t.Errorf("wide row = %q", wide)
	}
	narrow := ansi.Strip(budgetBar(row, newSpans(36, nil), 36))
	if !strings.Contains(narrow, "60%") || strings.Contains(narrow, "expired") {
		t.Errorf("narrow row = %q, want the percent kept and the text cut", narrow)
	}
}

func TestBudgetBarCriticalIsRed(t *testing.T) {
	trueColorProfile(t)
	row := vm.BudgetRow{Label: "claude api-key", UsedPct: 25, Text: "$37 left expired"}
	healthy := budgetBar(row, newSpans(60, nil), 60)
	row.Critical = true
	if budgetBar(row, newSpans(60, nil), 60) == healthy {
		t.Error("a critical row renders like a healthy one at 25%")
	}
}

func TestCreditPctRedWhenExhausted(t *testing.T) {
	if got := creditPct(&data.AnthropicCredit{UsedPct: 25, RemainingUSD: 5}); got != 25 {
		t.Errorf("healthy credit pct = %v", got)
	}
	if got := creditPct(&data.AnthropicCredit{UsedPct: 25, RemainingUSD: 5, Expired: true}); got != 100 {
		t.Errorf("expired credit pct = %v", got)
	}
}
