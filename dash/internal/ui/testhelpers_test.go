package ui

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/noamsto/dispatcher/dash/internal/data"
)

// loadSnapshot reads the shared fixture (also used by internal/once's golden
// tests) into a data.Snapshot.
func loadSnapshot(t *testing.T) data.Snapshot {
	t.Helper()
	raw, err := os.ReadFile("testdata/snapshot.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var snap data.Snapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}
	return snap
}

// fixedNow returns a clock frozen at the fixture's Now (as a fake "now" for
// status-stamp determinism in tests).
func fixedNow(snap data.Snapshot) func() time.Time {
	return func() time.Time { return time.Unix(snap.Now, 0).UTC() }
}

func keyRune(r rune) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}}
}

func keyStr(s string) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

func keyType(t tea.KeyType) tea.KeyMsg {
	return tea.KeyMsg{Type: t}
}

func sized(m Model, w, h int) Model {
	nm, _ := m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	return nm.(Model)
}
