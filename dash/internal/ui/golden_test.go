package ui

import (
	"flag"
	"os"
	"path/filepath"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

var update = flag.Bool("update", false, "update golden files")

func goldenFrame(t *testing.T, active, w, h int) string {
	t.Helper()
	orig := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.Ascii)
	defer lipgloss.SetColorProfile(orig)

	m := sized(newTestModel(t), w, h)
	m.active = active
	return m.View()
}

func checkGolden(t *testing.T, name, got string) {
	t.Helper()
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

func TestGoldenSettings80x24(t *testing.T) {
	checkGolden(t, "settings_80x24.golden", goldenFrame(t, 0, 80, 24))
}

func TestGoldenSettings120x40(t *testing.T) {
	checkGolden(t, "settings_120x40.golden", goldenFrame(t, 0, 120, 40))
}

func TestGoldenBudget80x24(t *testing.T) {
	checkGolden(t, "budget_80x24.golden", goldenFrame(t, 1, 80, 24))
}

func TestGoldenBudget120x40(t *testing.T) {
	checkGolden(t, "budget_120x40.golden", goldenFrame(t, 1, 120, 40))
}

func goldenFrameFull(t *testing.T, active, w, h int) string {
	t.Helper()
	orig := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.Ascii)
	defer lipgloss.SetColorProfile(orig)

	snap := loadFullSnapshot(t)
	m := sized(testModel(snap), w, h)
	m.active = active
	return m.View()
}

func TestGoldenRuns80x24(t *testing.T) {
	checkGolden(t, "runs_80x24.golden", goldenFrameFull(t, 2, 80, 24))
}

func TestGoldenRuns120x40(t *testing.T) {
	checkGolden(t, "runs_120x40.golden", goldenFrameFull(t, 2, 120, 40))
}

func TestGoldenRunsDetail80x24(t *testing.T) {
	snap := loadFullSnapshot(t)
	orig := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.Ascii)
	defer lipgloss.SetColorProfile(orig)

	m := sized(testModel(snap), 80, 24)
	m.active = 2
	nm, _ := m.Update(keyRune('f'))
	m = nm.(Model)
	nm, _ = m.Update(keyType(tea.KeyEnter))
	m = nm.(Model)
	checkGolden(t, "runs_detail_80x24.golden", m.View())
}

func TestGoldenRoster80x24(t *testing.T) {
	checkGolden(t, "roster_80x24.golden", goldenFrameFull(t, 3, 80, 24))
}

func TestGoldenRoster120x40(t *testing.T) {
	checkGolden(t, "roster_120x40.golden", goldenFrameFull(t, 3, 120, 40))
}
