package ui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/noamsto/dispatcher/dash/internal/view/viewtest"
)

// sweepModel is a root Model on the Overview over the shared fixture plus 12
// blocked workers, so the strip always has more items than it can show.
func sweepModel() Model {
	in := viewtest.Inputs()
	crew := &in.Snap.Roster.Crews[0]
	for i := range 12 {
		crew.Workers = append(crew.Workers, map[string]any{
			"branch": fmt.Sprintf("feat/x%02d", i),
			"from":   fmt.Sprintf("worker:feat/x%02d", i),
			"state":  "blocked",
			"detail": fmt.Sprintf("question %d for the operator, long enough to need truncating in a narrow strip", i),
			"ts":     int64(1794800000000 + i*1000),
			"engine": "claude", "model": "sonnet", "tier": "standard",
			"title": fmt.Sprintf("Task feat/x%02d with a deliberately long title that has to be cut", i),
		})
	}
	m := NewModel(Deps{Now: in.Snap.Now, Crew: in.Crew}, func() time.Time { return time.UnixMilli(viewtest.NowMS) }, liveDeps{})
	m.snap, m.digest = in.Snap, in.Bus
	for i := range m.load {
		m.load[i] = loadState{loaded: true}
	}
	m.broadcast()
	m.active = overviewTab
	return m
}

func sweepSelect(m Model, branch string) Model {
	ov := m.views[overviewTab].(overviewView)
	for _, w := range ov.model.Workers {
		if w.Branch == branch {
			ov.st.selected = workerKey{w.Crew, w.Branch}
		}
	}
	m.views[overviewTab] = ov
	return m
}

func TestOverviewSizeSweep(t *testing.T) {
	asciiProfile(t)
	typed := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(strings.Repeat("x", 300))}
	states := []struct {
		name  string
		title string // the overlay's top-border title; empty for none
		open  func(Model) Model
	}{
		{"normal", "", func(m Model) Model { return m }},
		{"detail", "DETAIL", func(m Model) Model { return sweepKeys(sweepSelect(m, "feat/a"), keyType(tea.KeyEnter)) }},
		{"help", "HELP", func(m Model) Model { return sweepKeys(m, keyRune('?')) }},
		{"reply", "REPLY", func(m Model) Model { return sweepKeys(sweepSelect(m, "feat/c"), keyRune('r'), typed) }},
		{"merge", "CONFIRM", func(m Model) Model { return sweepKeys(sweepSelect(m, "feat/b"), keyRune('m')) }},
		{"filter", "FILTER", func(m Model) Model { return sweepKeys(m, keyRune('/'), typed) }},
	}
	for w := 100; w <= 200; w += 10 {
		for h := 30; h <= 60; h += 5 {
			base := sized(sweepModel(), w, h)
			for _, st := range states {
				frame := st.open(base).View()
				name := fmt.Sprintf("%dx%d %s", w, h, st.name)
				if len(strings.Split(frame, "\n")) != h {
					t.Errorf("%s: frame has %d lines, want %d", name, len(strings.Split(frame, "\n")), h)
					continue
				}
				assertWidth(t, frame, w, h)
				if st.title != "" && !hasOverlayTop(frame, st.title) {
					t.Errorf("%s: no overlay top border titled %s", name, st.title)
				}
			}
		}
	}
}

func sweepKeys(m Model, keys ...tea.KeyMsg) Model {
	for _, k := range keys {
		nm, _ := m.Update(k)
		m = nm.(Model)
	}
	return m
}

// hasOverlayTop reports whether some line carries a rounded top corner and the
// overlay's title pill.
func hasOverlayTop(frame, title string) bool {
	for _, l := range strings.Split(stripped(frame), "\n") {
		if strings.Contains(l, "╭") && strings.Contains(l, title) {
			return true
		}
	}
	return false
}
