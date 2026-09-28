package ui

import (
	"strings"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/noamsto/dispatcher/dash/internal/data"
)

// rosterView is a placeholder — the live Roster view (bus watcher, worker
// table, holds, event drill-in) lands in G3 (plan step 11).
type rosterView struct{}

func newRosterView(_ data.Snapshot) rosterView { return rosterView{} }

func (v rosterView) Update(msg tea.Msg) (view, tea.Cmd) {
	if _, ok := msg.(snapshotMsg); ok {
		return v, nil
	}
	return v, nil
}

func (v rosterView) Keys() []key.Binding       { return nil }
func (v rosterView) Selection() data.Selection { return nil }
func (v rosterView) Capturing() bool           { return false }

func (v rosterView) View(w, h int) string {
	return strings.Join(normalizeFrame("coming in the next step", h, w), "\n")
}
