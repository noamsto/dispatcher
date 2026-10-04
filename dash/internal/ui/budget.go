package ui

import (
	"strings"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/progress"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/noamsto/dispatcher/dash/internal/data"
)

var budgetEngineOrder = []string{"claude", "codex", "cursor", "pi"}

// winRef locates one selectable window row: engine name + index into that
// engine's Windows slice.
type winRef struct {
	engine string
	idx    int
}

type budgetView struct {
	now      int64 // snap.Now — the budget pane's "fetched X ago" clock (once.go parity)
	report   *data.BudgetReport
	warnings []string
	err      *string

	sel    []winRef
	cursor int
	detail bool
}

func newBudgetView(snap data.Snapshot) budgetView {
	v := budgetView{
		now:      snap.Now,
		report:   snap.Budget.Report,
		warnings: snap.Budget.Warnings,
		err:      snap.Budget.Error,
	}
	v.rebuildSel()
	return v
}

func (v *budgetView) rebuildSel() {
	v.sel = nil
	if v.report == nil {
		return
	}
	for _, e := range budgetEngineOrder {
		eb := v.report.Engines[e]
		if eb == nil || eb.Unlimited {
			// An unlimited engine renders no window rows, so its windows are
			// not selectable either — renderer and selection stay in step by
			// construction, however the producer shapes the report.
			continue
		}
		for i := range eb.Windows {
			v.sel = append(v.sel, winRef{engine: e, idx: i})
		}
	}
	v.cursor = clamp(v.cursor, 0, len(v.sel)-1)
}

func (v budgetView) curWindow() (data.Window, bool) {
	if v.cursor < 0 || v.cursor >= len(v.sel) {
		return data.Window{}, false
	}
	ref := v.sel[v.cursor]
	return v.report.Engines[ref.engine].Windows[ref.idx], true
}

func (v budgetView) Update(msg tea.Msg) (view, tea.Cmd) {
	switch msg := msg.(type) {
	case snapshotMsg:
		snap := data.Snapshot(msg)
		v.now = snap.Now
		v.report = snap.Budget.Report
		v.warnings = snap.Budget.Warnings
		v.err = snap.Budget.Error
		v.rebuildSel()
		return v, nil

	case tea.KeyMsg:
		if v.detail {
			switch msg.String() {
			case "esc", "enter":
				v.detail = false
			}
			return v, nil
		}
		switch msg.String() {
		case "j", "down":
			v.cursor = clamp(v.cursor+1, 0, len(v.sel)-1)
		case "k", "up":
			v.cursor = clamp(v.cursor-1, 0, len(v.sel)-1)
		case "g":
			v.cursor = 0
		case "G":
			v.cursor = len(v.sel) - 1
		case "enter":
			if _, ok := v.curWindow(); ok {
				v.detail = true
			}
		}
	}
	return v, nil
}

func (v budgetView) Keys() []key.Binding {
	return []key.Binding{
		key.NewBinding(key.WithKeys("j", "k"), key.WithHelp("j/k", "move")),
		key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "verdict")),
	}
}

func (v budgetView) Selection() data.Selection {
	// No data.Selection variant exists yet for an engine/window (only
	// SettingRow/Worker/Run) — nothing to bind to here until one is added.
	return nil
}

func (v budgetView) Capturing() bool {
	return false
}

func (v budgetView) View(w, h int) string {
	if v.err != nil {
		lines := []string{truncateLine("unavailable: "+*v.err, w)}
		return strings.Join(normalizeFrame(strings.Join(lines, "\n"), h, w), "\n")
	}
	if v.detail {
		return strings.Join(normalizeFrame(v.detailView(w), h, w), "\n")
	}
	return strings.Join(normalizeFrame(v.tableView(w), h, w), "\n")
}

func (v budgetView) tableView(w int) string {
	var lines []string
	if v.report != nil {
		age := v.now - v.report.FetchedEpoch
		fetched := "fetched " + reltime(age) + " ago"
		if age > 7200 {
			fetched = staleStyle.Render(fetched + " (stale)")
		}
		lines = append(lines, truncateLine(fetched, w))
	}
	for _, warn := range v.warnings {
		lines = append(lines, truncateLine(warnStyle.Render("warning: "+warn), w))
	}

	if v.report == nil {
		return strings.Join(lines, "\n")
	}
	for _, e := range budgetEngineOrder {
		lines = append(lines, v.engineLines(e, v.report.Engines[e], w)...)
	}
	return strings.Join(lines, "\n")
}

func (v budgetView) engineLines(e string, eb *data.EngineBudget, w int) []string {
	if eb == nil {
		return []string{truncateLine(e+": unknown", w)}
	}
	heading := e + " (" + eb.Source + ")"
	if eb.PlanType != nil {
		heading += " [" + *eb.PlanType + "]"
	}
	out := []string{truncateLine(headerStyle.Render(heading), w)}
	if eb.Unlimited {
		out = append(out, truncateLine("  unlimited", w))
	} else {
		var curRef winRef
		hasCur := v.cursor >= 0 && v.cursor < len(v.sel)
		if hasCur {
			curRef = v.sel[v.cursor]
		}
		for i, win := range eb.Windows {
			selected := hasCur && curRef.engine == e && curRef.idx == i
			out = append(out, v.windowLine(win, selected, w))
		}
	}
	if eb.Source == "openrouter_key" {
		if eb.TargetUSD != nil {
			out = append(out, truncateLine(
				"  spend $"+usd(f64(eb.SpendUSD))+" of $"+usd(*eb.TargetUSD)+" target, "+
					formatNumber(f64(eb.ElapsedPct))+"% of month elapsed", w))
		} else {
			out = append(out, truncateLine("  spend $"+usd(f64(eb.SpendUSD))+" month-to-date (no target)", w))
		}
		if eb.Projection != nil {
			out = append(out, truncateLine("  "+*eb.Projection, w))
		}
	}

	if reason, ok := eb.LimitReachedReason(); ok {
		out = append(out, truncateLine("  limit reached: "+cleanText(reason), w))
	}
	return out
}

// windowLine lays out one window row: name, gauge, used%, pace, resets-in,
// verdict (truncated); the gauge takes what's left after the fixed columns,
// floored at 10.
func (v budgetView) windowLine(win data.Window, selected bool, w int) string {
	const nameW, usedW, paceW, resetsW = 6, 5, 4, 8
	prefix := "  "
	if selected {
		prefix = cursorStyle.Render("›") + " "
	}
	fixed := nameW + usedW + paceW + resetsW + 5 // 5 single-space gaps
	remaining := max0(w - fixed - 2)             // 2 = prefix width
	gaugeW := clamp(remaining/3, 10, max0(remaining))
	verdictW := max0(remaining - gaugeW)

	name := padRight(truncateLine(win.Key, nameW), nameW)
	pct := win.UsedPct
	if pct < 0 {
		pct = 0
	}
	pm := progress.New(progress.WithSolidFill(gaugeColor(pct)), progress.WithWidth(gaugeW), progress.WithoutPercentage())
	frac := pct / 100
	if frac > 1 {
		frac = 1
	}
	gauge := pm.ViewAs(frac)

	usedTxt := formatNumber(win.UsedPct) + "%"
	usedCell := rightAlignCell(usedTxt, usedW)

	paceTxt := pace(win.AheadPts)
	paceCell := rightAlignCell(paceTxt, paceW)

	resetsTxt := "—"
	if win.ResetsInS != nil {
		resetsTxt = reltime(*win.ResetsInS)
	}
	resetsCell := rightAlignCell(resetsTxt, resetsW)

	verdictTxt := "—"
	if win.Verdict != nil {
		verdictTxt = *win.Verdict
	}
	verdictCell := truncateLine(verdictTxt, verdictW)

	line := prefix + name + " " + gauge + " " + usedCell + " " + paceCell + " " + resetsCell + " " + verdictCell
	return truncateLine(line, w)
}

// rightAlignCell pads s with leading spaces to width w (truncating first if
// s alone would overflow).
func rightAlignCell(s string, w int) string {
	s = truncateLine(s, w)
	p := w - ansi.StringWidth(s)
	if p <= 0 {
		return s
	}
	return strings.Repeat(" ", p) + s
}

func (v budgetView) detailView(w int) string {
	win, ok := v.curWindow()
	if !ok {
		return ""
	}
	verdict := "—"
	if win.Verdict != nil {
		verdict = *win.Verdict
	}
	header := headerStyle.Render(win.Key + " — full verdict")
	// This is the one place the dashboard deliberately wraps instead of
	// clipping, since it is the verdict's own full-text detail; ansi.Wrap is
	// display-width-aware and hard-breaks a word longer than w — len()
	// undercounts multi-byte runes.
	wrapped := strings.Split(ansi.Wrap(verdict, max0(w), ""), "\n")
	lines := append([]string{truncateLine(header, w), ""}, wrapped...)
	lines = append(lines, "", "esc back")
	return strings.Join(lines, "\n")
}
