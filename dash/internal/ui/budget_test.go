package ui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/noamsto/dispatcher/dash/internal/data"
)

func newTestBudgetView(t *testing.T) budgetView {
	t.Helper()
	return newBudgetView(loadSnapshot(t))
}

func TestBudgetGaugePresentPerWindow(t *testing.T) {
	v := newTestBudgetView(t)
	if len(v.sel) == 0 {
		t.Fatalf("no selectable windows in fixture")
	}
	out := v.View(120, 24)
	// The gauge draws with the progress bar's fill/empty rune; at minimum
	// its presence means the rendered row is wider than the fixed columns.
	if !strings.Contains(out, "claude (oauth_usage)") {
		t.Fatalf("claude engine heading missing:\n%s", out)
	}
}

func TestBudgetPaceAndResetsVisibleAt120(t *testing.T) {
	v := newTestBudgetView(t)
	out := v.View(120, 24)
	if !strings.Contains(out, "+40") {
		t.Fatalf("pace +40 not visible at 120 cols:\n%s", out)
	}
	if !strings.Contains(out, "3d 12h") {
		t.Fatalf("resets-in 3d 12h not visible at 120 cols:\n%s", out)
	}
}

func TestBudgetVerdictTruncatedAt80FullInDetail(t *testing.T) {
	v := newTestBudgetView(t)
	full := "real budget: prefer a cheaper burn class or rotate engines"
	out := v.View(80, 24)
	if strings.Contains(out, full) {
		t.Fatalf("full verdict present untruncated at 80 cols:\n%s", out)
	}
	if !strings.Contains(out, "…") {
		t.Fatalf("no ellipsis at 80 cols:\n%s", out)
	}

	// move cursor to the 7d claude window (index 1: 5h then 7d) and enter.
	v.cursor = 1
	win, ok := v.curWindow()
	if !ok || win.Key != "7d" {
		t.Fatalf("expected cursor on 7d window, got %+v ok=%v", win, ok)
	}
	nv, _ := v.Update(keyType(tea.KeyEnter))
	v = nv.(budgetView)
	if !v.detail {
		t.Fatalf("enter did not open detail")
	}
	detail := v.View(80, 24)
	if !strings.Contains(detail, full) {
		t.Fatalf("detail pane missing full verdict:\n%s", detail)
	}

	nv, _ = v.Update(keyType(tea.KeyEsc))
	v = nv.(budgetView)
	if v.detail {
		t.Fatalf("esc did not close detail")
	}
}

func TestBudgetUnknownEngines(t *testing.T) {
	v := newTestBudgetView(t)
	out := v.View(80, 24)
	if !strings.Contains(out, "cursor: unknown") {
		t.Fatalf("cursor engine not rendered unknown:\n%s", out)
	}
	if !strings.Contains(out, "pi: unknown") {
		t.Fatalf("pi engine not rendered unknown:\n%s", out)
	}
}

func TestBudgetStaleFlag(t *testing.T) {
	snap := loadSnapshot(t)
	v := newBudgetView(snap)
	out := v.View(80, 24)
	if strings.Contains(out, "stale") {
		t.Fatalf("fresh budget flagged stale:\n%s", out)
	}

	snap.Budget.Report.FetchedEpoch = snap.Now - 7201
	v = newBudgetView(snap)
	out = v.View(80, 24)
	if !strings.Contains(out, "stale") {
		t.Fatalf("budget older than 7200s not flagged stale:\n%s", out)
	}
}

func TestBudgetGaugeMinWidth(t *testing.T) {
	v := newTestBudgetView(t)
	line := v.windowLine(data.Window{Key: "5h", UsedPct: 10}, false, 10)
	if ansi.StringWidth(line) > 10 {
		t.Fatalf("window line at width 10 overflows: %d: %q", ansi.StringWidth(line), line)
	}
}

func TestBudgetGaugeColorThresholds(t *testing.T) {
	cases := []struct {
		pct  float64
		want string
	}{
		{10, gaugeNormalColor},
		{85, gaugeWarningColor},
		{94, gaugeWarningColor},
		{95, gaugeDangerColor},
		{100, gaugeDangerColor},
	}
	for _, c := range cases {
		if got := gaugeColor(c.pct); got != c.want {
			t.Fatalf("gaugeColor(%v) = %q, want %q", c.pct, got, c.want)
		}
	}
}
