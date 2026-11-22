package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"

	vm "github.com/noamsto/dispatcher/dash/internal/view"
)

func TestCodenameStyleUsesTmuxColourOrFallback(t *testing.T) {
	trueColor(t)
	fallback := lipgloss.NewStyle().Italic(true)
	good, bad := "colour25", "sky"
	for _, tc := range []struct {
		name string
		tmux *string
		want string // substring of the rendered cell
	}{
		{"tmux colour", &good, "38;5;25"},
		{"missing", nil, "\x1b[3m"},
		{"malformed", &bad, "\x1b[3m"},
	} {
		got := codenameStyle(tc.tmux, fallback).Render("moss")
		if !strings.Contains(got, tc.want) {
			t.Errorf("%s: %q lacks %q", tc.name, got, tc.want)
		}
	}
	if got := codenameStyle(&good, fallback).Render("moss"); !strings.Contains(got, "\x1b[1;") {
		t.Errorf("tmux codename not bold: %q", got)
	}
}

func TestSelectedRowKeepsCodenameColour(t *testing.T) {
	trueColor(t)
	c := "colour25"
	fallback := lipgloss.NewStyle().Italic(true)
	sel := newSpans(10, lipgloss.Color("#223344"))
	got := sel.add("moss", codenameStyle(&c, fallback)).String()
	if !strings.Contains(got, "38;5;25") {
		t.Errorf("selected row = %q, want the tmux colour", got)
	}
	if !strings.Contains(got, "48;2;34;51;68") {
		t.Errorf("selected row = %q, want the selection background", got)
	}
}

func TestBoardCardHeadUsesTmuxColour(t *testing.T) {
	trueColor(t)
	row := func(extra map[string]any) []string {
		w := map[string]any{"branch": "feat/1-a", "name": "moss", "state": "working", "tier": "standard"}
		for k, v := range extra {
			w[k] = v
		}
		return cardRows(fieldsOf(workerRow("c", w)), lipgloss.NewStyle())
	}
	if got := row(map[string]any{"tmux": "colour25"})[0]; !strings.Contains(got, "38;5;25") {
		t.Errorf("head = %q, want the tmux colour", got)
	}
	if got := row(nil)[0]; strings.Contains(got, "38;5;") {
		t.Errorf("head without tmux = %q, want the default style", got)
	}
}

func TestEventTmuxFollowsEventFrom(t *testing.T) {
	c := "colour25"
	ov := vm.OverviewView{Workers: []vm.OverviewWorker{{Branch: "feat/x", Tmux: &c}}}
	for from, want := range map[string]bool{
		"worker:feat/x#s1":   true,
		"role:feat/x:critic": true,
		"role:feat/x":        false,
		"worker:feat/other":  false,
		"dispatcher:c":       false,
	} {
		if got := eventTmux(ov, from) != nil; got != want {
			t.Errorf("eventTmux(%q) set = %v, want %v", from, got, want)
		}
	}
}

func TestOverviewLinesUseTmuxColour(t *testing.T) {
	trueColor(t)
	c := "colour25"
	ov := vm.OverviewView{Workers: []vm.OverviewWorker{{Branch: "feat/x", Tmux: &c}}}
	detail := strings.Join(detailLines(ov, 0, 80), "\n")
	if !strings.Contains(detail, "38;5;25") {
		t.Errorf("detail header lacks the tmux colour: %q", detail)
	}
}
