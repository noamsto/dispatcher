package ui

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

var update = flag.Bool("update", false, "update golden files")

// Tab indexes in the root model's view order.
const (
	goldenTabLive = iota
	goldenTabBoard
	goldenTabBudget
	goldenTabRuns
	goldenTabSettings
)

func checkGolden(t *testing.T, name, got string, w, h int) {
	t.Helper()
	assertWidth(t, got, w, h)
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatalf("write golden %s: %v", path, err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v (run with -update to generate)", path, err)
	}
	if got != string(want) {
		t.Fatalf("golden %s mismatch:\n--- got ---\n%s\n--- want ---\n%s", path, got, string(want))
	}
}

// goldenSizes checks prefix_{80x24,160x40}.golden for the model build()
// returns, rendered on the given tab.
func goldenSizes(t *testing.T, prefix string, build func() Model, active int) {
	t.Helper()
	goldenSizesAt(t, prefix, build, active, [][2]int{{80, 24}, {160, 40}})
}

func goldenSizesAt(t *testing.T, prefix string, build func() Model, active int, sizes [][2]int) {
	t.Helper()
	asciiProfile(t)
	for _, sz := range sizes {
		w, h := sz[0], sz[1]
		m := sized(build(), w, h)
		m.active = active
		checkGolden(t, fmt.Sprintf("%s_%dx%d.golden", prefix, w, h), m.View(), w, h)
	}
}

func TestGoldenLive(t *testing.T) {
	snap := loadFullSnapshot(t)
	goldenSizes(t, "live", func() Model { return testModel(snap) }, goldenTabLive)
}

func TestGoldenLiveLoading(t *testing.T) {
	snap := loadFullSnapshot(t)
	goldenSizes(t, "live_loading", func() Model { return loadingModel(Deps{Now: snap.Now}) }, goldenTabLive)
}

// TestGoldenLiveMany and TestGoldenLiveManyEnd80x24 check that the Live body
// windows to h instead of letting normalizeFrame silently clip rows past it.
// The "many" fixture (3 crews, 18 workers, holds) overflows a 24-row frame,
// so the cursor-at-top and cursor-at-last-worker frames necessarily differ.
func TestGoldenLiveMany(t *testing.T) {
	snap := loadManySnapshot(t)
	goldenSizes(t, "live_many", func() Model { return testModel(snap) }, goldenTabLive)
}

func TestGoldenLiveManyEnd80x24(t *testing.T) {
	asciiProfile(t)
	m := sized(testModel(loadManySnapshot(t)), 80, 24)
	m.active = goldenTabLive
	nm, _ := m.Update(keyRune('G'))
	out := nm.(Model).View()
	checkGolden(t, "live_many_end_80x24.golden", out, 80, 24)

	if !strings.Contains(out, "›") {
		t.Errorf("cursor marker › not visible after moving to the last worker:\n%s", out)
	}
}

func TestGoldenLiveCrewErr(t *testing.T) {
	snap := loadCrewErrSnapshot(t)
	goldenSizes(t, "live_crewerr", func() Model { return testModel(snap) }, goldenTabLive)
}

func TestGoldenBudget(t *testing.T) {
	goldenSizes(t, "budget", func() Model { return newTestModel(t) }, goldenTabBudget)
}

func TestGoldenRuns(t *testing.T) {
	snap := loadFullSnapshot(t)
	goldenSizes(t, "runs", func() Model { return testModel(snap) }, goldenTabRuns)
}

func TestGoldenRunsDetail80x24(t *testing.T) {
	asciiProfile(t)
	m := sized(testModel(loadFullSnapshot(t)), 80, 24)
	m.active = goldenTabRuns
	nm, _ := m.Update(keyRune('f'))
	nm, _ = nm.Update(keyType(tea.KeyEnter))
	checkGolden(t, "runs_detail_80x24.golden", nm.(Model).View(), 80, 24)
}

func TestGoldenSettings(t *testing.T) {
	goldenSizesAt(t, "settings", func() Model { return newTestModel(t) }, goldenTabSettings, [][2]int{{80, 24}, {120, 40}})
}
