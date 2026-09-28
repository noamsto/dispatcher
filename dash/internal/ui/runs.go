package ui

import (
	"strings"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/noamsto/dispatcher/dash/internal/data"
)

// runsView is a placeholder — the Runs view (ratings table + retro list,
// sort/detail) lands in G3 (plan step 10).
type runsView struct{}

func newRunsView(_ data.Snapshot) runsView { return runsView{} }

func (v runsView) Update(msg tea.Msg) (view, tea.Cmd) {
	if _, ok := msg.(snapshotMsg); ok {
		return v, nil
	}
	return v, nil
}

func (v runsView) Keys() []key.Binding       { return nil }
func (v runsView) Selection() data.Selection { return nil }
func (v runsView) Capturing() bool           { return false }

func (v runsView) View(w, h int) string {
	return strings.Join(normalizeFrame("coming in the next step", h, w), "\n")
}
