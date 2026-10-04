package ui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"

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

// TestBudgetDetailWrapsMultiByteAndLongWord checks that the detail pane's
// verdict wrap is display-width-aware (a len()-based wrapper undercounts
// multi-byte runes) and hard-breaks a word longer than the width, never
// letting a rendered line exceed it.
func TestBudgetDetailWrapsMultiByteAndLongWord(t *testing.T) {
	v := newTestBudgetView(t)
	verdict := "日本語のテキストが混じった長い説明文です。 " + strings.Repeat("x", 120) + " tail"
	v.report.Engines["claude"].Windows[0].Verdict = &verdict
	v.cursor = 0
	nv, _ := v.Update(keyType(tea.KeyEnter))
	v = nv.(budgetView)
	if !v.detail {
		t.Fatalf("enter did not open detail")
	}
	// detailView directly, not View: View's normalizeFrame belt-and-suspenders
	// truncates every line to w regardless, which would mask exactly the
	// overflow this test exists to catch.
	out := v.detailView(80)
	for _, line := range strings.Split(out, "\n") {
		if w := ansi.StringWidth(line); w > 80 {
			t.Errorf("detail line %q is %d cells wide, want <= 80", line, w)
		}
	}
	if !strings.Contains(out, "日本語") {
		t.Errorf("detail pane missing multi-byte verdict text:\n%s", out)
	}
}

func TestBudgetUnknownEngines(t *testing.T) {
	v := newTestBudgetView(t)
	out := v.View(80, 24)
	if !strings.Contains(out, "pi: unknown") {
		t.Fatalf("pi engine not rendered unknown:\n%s", out)
	}
}

// TestBudgetLimitReachedReasonCleaned checks the renderer surfaces an engine's
// limit-reached reason and runs it through cleanText (tab/newline become a
// space), matching the text report()'s limit verdict.
func TestBudgetLimitReachedReasonCleaned(t *testing.T) {
	snap := loadSnapshot(t)
	snap.Budget.Report.Engines["cursor"].LimitReached = []byte(`{"reason":"limit\tat\n50%"}`)
	v := newBudgetView(snap)
	out := strings.Join(v.engineLines("cursor", snap.Budget.Report.Engines["cursor"], 200), "\n")
	if !strings.Contains(out, "limit reached: limit at 50%") {
		t.Fatalf("cleaned limit-reached reason missing:\n%s", out)
	}
}

// TestBudgetUnlimitedHidesWindowTable checks an unlimited plan renders the
// `unlimited` line in place of its window rows, the way report() does.
func TestBudgetUnlimitedHidesWindowTable(t *testing.T) {
	snap := loadSnapshot(t)
	eb := snap.Budget.Report.Engines["codex"]
	eb.Unlimited = true
	v := newBudgetView(snap)
	// claude's 5h+7d and cursor's month stay selectable; codex's 5h does not.
	if len(v.sel) != 3 {
		t.Fatalf("unlimited engine's windows still selectable: %+v", v.sel)
	}
	out := strings.Join(v.engineLines("codex", eb, 200), "\n")
	if !strings.Contains(out, "unlimited") {
		t.Fatalf("unlimited line missing:\n%s", out)
	}
	if strings.Contains(out, "5h") {
		t.Fatalf("window table rendered for an unlimited engine:\n%s", out)
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

func TestBudgetErrorInjectionIsCleaned(t *testing.T) {
	orig := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.Ascii)
	defer lipgloss.SetColorProfile(orig)

	snap := loadFullSnapshot(t)
	msg := injectedErr
	snap.Budget.Error = &msg
	out := newBudgetView(snap).View(120, 24)
	requireNoControl(t, out)
	if !strings.Contains(out, "unavailable: boom") {
		t.Errorf("missing unavailable: boom:\n%q", out)
	}
}

func TestBudgetFieldsInjectionAreCleaned(t *testing.T) {
	orig := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.Ascii)
	defer lipgloss.SetColorProfile(orig)

	snap := loadFullSnapshot(t)
	snap.Budget.Warnings = []string{injectedErr}
	proj := injectedErr
	for _, eb := range snap.Budget.Report.Engines {
		if eb == nil {
			continue
		}
		eb.Projection = &proj
		for i := range eb.Windows {
			eb.Windows[i].Key = "k" + injectedErr
		}
	}
	v := newBudgetView(snap)
	out := v.View(160, 40)
	requireNoControl(t, out)
	if !strings.Contains(out, "warning: boom") {
		t.Errorf("missing warning: boom:\n%q", out)
	}
}
