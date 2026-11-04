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

// loadManySnapshot reads a fixture with 3 crews and 18 workers plus holds —
// enough to overflow a roster frame's height, for the roster windowing
// tests and golden frames.
func loadManySnapshot(t *testing.T) data.Snapshot {
	t.Helper()
	raw, err := os.ReadFile("testdata/snapshot-many.json")
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

func keyType(t tea.KeyType) tea.KeyMsg {
	return tea.KeyMsg{Type: t}
}

// allLoaded is the loaded flags of a view built straight from a fixture.
var allLoaded = [numSources]bool{true, true, true, true, true}

// loadingModel builds a root Model with no source applied.
func loadingModel(deps Deps) Model {
	return NewModel(deps, func() time.Time { return time.Unix(deps.Now, 0).UTC() }, liveDeps{})
}

// loadedModel builds a root Model with every source of snap applied, with
// no live roster refresh (no watcher) — the default for tests that don't
// specifically exercise live-refresh wiring (see roster_test.go for those).
func loadedModel(snap data.Snapshot, deps Deps) Model {
	var m tea.Model = NewModel(deps, fixedNow(snap), liveDeps{})
	for _, msg := range []sourceMsg{
		{srcSettings, func(s *data.Snapshot) { s.Settings = snap.Settings }},
		{srcBudget, func(s *data.Snapshot) { s.Budget = snap.Budget }},
		{srcRetro, func(s *data.Snapshot) {
			s.Runs.Retro, s.Runs.RetroError = snap.Runs.Retro, snap.Runs.RetroError
		}},
		{srcRatings, func(s *data.Snapshot) {
			s.Runs.Ratings, s.Runs.RatingsGroups, s.Runs.RatingsError = snap.Runs.Ratings, snap.Runs.RatingsGroups, snap.Runs.RatingsError
		}},
		{srcRoster, func(s *data.Snapshot) { s.Roster = snap.Roster }},
	} {
		m, _ = m.Update(msg)
	}
	return m.(Model)
}

func testModel(snap data.Snapshot) Model {
	return loadedModel(snap, Deps{Now: snap.Now})
}

// flattenCmd runs cmd and every Cmd of the BatchMsgs it yields, returning
// the leaf messages. Every loader must return promptly.
func flattenCmd(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	batch, ok := msg.(tea.BatchMsg)
	if !ok {
		return []tea.Msg{msg}
	}
	var out []tea.Msg
	for _, c := range batch {
		out = append(out, flattenCmd(c)...)
	}
	return out
}

func sized(m Model, w, h int) Model {
	nm, _ := m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	return nm.(Model)
}
