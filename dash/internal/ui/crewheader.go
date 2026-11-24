package ui

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/noamsto/dispatcher/dash/internal/data"
)

// RepoOfEvents names the repo whose bus log sits at <common>/crew/events.jsonl:
// the checkout dir for a ".git" common dir, else the common dir itself.
func RepoOfEvents(eventsPath string) string {
	if eventsPath == "" {
		return ""
	}
	return data.RepoName(filepath.Dir(filepath.Dir(eventsPath)))
}

// crewHeader is the one-line "crew › repo id state" naming the crew a
// single-crew tab is scoped to, or the unscoped repo with its crew count; ""
// when there is no scope and no crew to count.
func (m Model) crewHeader() string {
	sc := m.snap.Roster.Scope
	parts := []string{}
	if m.repo != "" {
		parts = append(parts, cleanText(m.repo))
	}
	line := " " + fg(colDim).Render("crew ›") + " "
	if sc.Crew == "" {
		n := len(m.snap.Roster.Crews)
		if n == 0 {
			return ""
		}
		label := fmt.Sprintf("unscoped (%d crews)", n)
		if n == 1 {
			label = "unscoped (1 crew)"
		}
		line += lipgloss.NewStyle().Bold(true).Render(strings.Join(parts, " "))
		if len(parts) > 0 {
			line += " "
		}
		line += fg(colDim).Render("· " + label)
		if sc.Err != "" {
			line += fg(colAmber).Render(" · scope probe failed")
		}
		return truncateLine(line, m.width)
	}
	parts = append(parts, cleanText(sc.Crew))
	line += lipgloss.NewStyle().Bold(true).Render(strings.Join(parts, " "))
	if !sc.Known {
		return truncateLine(line+fg(colAmber).Render(" · unknown crew"), m.width)
	}
	if state := m.crewLaneState(sc.Crew); state != "" {
		line += fg(colDim).Render(" · " + cleanText(state))
	}
	return truncateLine(line, m.width)
}

// crewLaneState is the dispatcher lane state of crew id; a crew whose
// dispatcher lives on another bus has no lane here.
func (m Model) crewLaneState(id string) string {
	for _, c := range m.snap.Roster.Crews {
		if c.ID != id || c.Lane == nil || (c.Dispatcher != nil && c.Dispatcher.Where != "here") {
			continue
		}
		switch c.Lane.State {
		case "alive":
			return "live"
		case "unknown":
			return ""
		}
		return c.Lane.State
	}
	return ""
}
