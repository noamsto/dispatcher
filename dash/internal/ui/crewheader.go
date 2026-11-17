package ui

import (
	"path/filepath"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// repoOfEvents names the repo whose bus log sits at <common>/crew/events.jsonl:
// the checkout dir for a ".git" common dir, else the common dir itself.
func repoOfEvents(eventsPath string) string {
	if eventsPath == "" {
		return ""
	}
	common := filepath.Dir(filepath.Dir(eventsPath))
	if filepath.Base(common) == ".git" {
		common = filepath.Dir(common)
	}
	return filepath.Base(common)
}

// crewHeader is the one-line "crew › repo id state" naming the crew a
// single-crew tab is scoped to; "" until the roster names one.
func (m Model) crewHeader() string {
	if len(m.snap.Roster.Crews) == 0 {
		return ""
	}
	ids := make([]string, 0, len(m.snap.Roster.Crews))
	for _, c := range m.snap.Roster.Crews {
		if id := cleanText(c.ID); id != "" {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return ""
	}
	id := strings.Join(ids, ", ")
	// One crew has one dispatcher to report; several name no single state.
	state := ""
	if c := m.snap.Roster.Crews[0]; len(m.snap.Roster.Crews) == 1 && c.Lane != nil {
		switch c.Lane.State {
		case "alive":
			state = "live"
		case "unknown":
		default:
			state = c.Lane.State
		}
	}
	repo := m.repo
	parts := []string{}
	if repo != "" {
		parts = append(parts, cleanText(repo))
	}
	parts = append(parts, id)
	line := " " + fg(colDim).Render("crew ›") + " " + lipgloss.NewStyle().Bold(true).Render(strings.Join(parts, " "))
	if state != "" {
		line += fg(colDim).Render(" · " + cleanText(state))
	}
	return truncateLine(line, m.width)
}
