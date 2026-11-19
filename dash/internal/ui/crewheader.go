package ui

import (
	"path/filepath"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/noamsto/dispatcher/dash/internal/data"
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
	// A crew whose dispatcher lives on another bus has no lane here.
	var homed []data.RosterCrew
	for _, c := range m.snap.Roster.Crews {
		if c.Dispatcher == nil || c.Dispatcher.Where == "here" {
			homed = append(homed, c)
		}
	}
	state := ""
	if len(homed) == 1 && homed[0].Lane != nil {
		switch homed[0].Lane.State {
		case "alive":
			state = "live"
		case "unknown":
		default:
			state = homed[0].Lane.State
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
