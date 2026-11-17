package ui

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/noamsto/dispatcher/dash/internal/data"
	vm "github.com/noamsto/dispatcher/dash/internal/view"
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

// loadCrewErrSnapshot reads a fixture with one crew whose roster call failed,
// one whose hold list failed, and a worker on a remote host.
func loadCrewErrSnapshot(t *testing.T) data.Snapshot {
	t.Helper()
	raw, err := os.ReadFile("testdata/snapshot-crewerr.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var snap data.Snapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}
	return snap
}

// assertWidth fails unless frame is exactly h lines, none wider than w cells.
func assertWidth(t *testing.T, frame string, w, h int) {
	t.Helper()
	lines := strings.Split(frame, "\n")
	if len(lines) != h {
		t.Errorf("frame has %d lines, want %d", len(lines), h)
	}
	for i, l := range lines {
		if got := ansi.StringWidth(l); got > w {
			t.Errorf("line %d is %d cells wide, want <= %d: %q", i, got, w, l)
		}
	}
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
	m := NewModel(deps, func() time.Time { return time.Unix(deps.Now, 0).UTC() }, liveDeps{})
	m.active = liveTab
	return m
}

// loadedModel builds a root Model with every source of snap applied, with
// no live roster refresh (no watcher) — the default for tests that don't
// specifically exercise live-refresh wiring (see live_test.go for those).
func loadedModel(snap data.Snapshot, deps Deps) Model {
	root := NewModel(deps, fixedNow(snap), liveDeps{})
	root.active = liveTab
	var m tea.Model = root
	for _, msg := range []sourceMsg{
		{src: srcSettings, apply: func(s *data.Snapshot) { s.Settings = snap.Settings }},
		{src: srcBudget, apply: func(s *data.Snapshot) { s.Budget = snap.Budget }},
		{src: srcRetro, apply: func(s *data.Snapshot) {
			s.Runs.Retro, s.Runs.RetroError = snap.Runs.Retro, snap.Runs.RetroError
		}},
		{src: srcRatings, apply: func(s *data.Snapshot) {
			s.Runs.Ratings, s.Runs.RatingsGroups, s.Runs.RatingsError = snap.Runs.Ratings, snap.Runs.RatingsGroups, snap.Runs.RatingsError
		}},
		{src: srcRoster, apply: func(s *data.Snapshot) { s.Roster = snap.Roster }},
		{src: srcBus, apply: func(*data.Snapshot) {}},
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

// snapMsg is the snapshotMsg the root broadcasts once snap's sources landed
// with these loaded flags.
func snapMsg(snap data.Snapshot, loaded [numSources]bool) snapshotMsg {
	return newSnapshotMsg(inputsOf(snap, data.BusDigest{}, loaded, snap.Now*1000, nil, nil), loaded)
}

func testLiveView(snap data.Snapshot, loaded [numSources]bool, deps liveDeps, now func() time.Time) liveView {
	msg := snapMsg(snap, loaded)
	return newLiveView(msg.roster, msg.budget, deps, now)
}

func testBudgetView(snap data.Snapshot, loaded [numSources]bool) budgetView {
	return newBudgetView(snapMsg(snap, loaded).budget)
}

func testRunsView(snap data.Snapshot, loaded [numSources]bool) runsView {
	msg := snapMsg(snap, loaded)
	return newRunsView(msg.runs)
}

func testSettingsView(snap data.Snapshot, loaded [numSources]bool, deps settingsDeps) settingsView {
	return newSettingsView(snapMsg(snap, loaded).settings, deps)
}

// workerRow types one fixture roster row of crewID.
func workerRow(crewID string, w map[string]any) vm.WorkerRow {
	return vm.WorkerRowOf(nil, crewID, w)
}
