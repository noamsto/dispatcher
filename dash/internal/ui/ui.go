// Package ui is the interactive Bubble Tea program. Run starts it; the rest
// of the package (model.go, keys.go, styles.go, layout.go, and one file per
// view) is unexported implementation.
package ui

import (
	"os"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/noamsto/dispatcher/dash/internal/data"
)

// Run starts the interactive dashboard. collect re-collects a fresh
// Snapshot, bound to "r" and the roster bus watcher.
func Run(initial data.Snapshot, collect func() data.Snapshot) error {
	if os.Getenv("NO_COLOR") != "" {
		lipgloss.SetColorProfile(termenv.Ascii)
	}
	m := NewModel(initial, collect, time.Now)
	_, err := tea.NewProgram(m, tea.WithAltScreen()).Run()
	return err
}
