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
	c := m.snap.Roster.Crews[0]
	id := cleanText(c.ID)
	if id == "" {
		return ""
	}
	repo, state := m.repo, ""
	switch {
	case c.Lane != nil && c.Lane.State == "alive":
		state = "live"
	case c.Lane != nil && c.Lane.State != "" && c.Lane.State != "unknown":
		state = c.Lane.State
	}
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
