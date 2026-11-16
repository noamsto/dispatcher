package ui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	vm "github.com/noamsto/dispatcher/dash/internal/view"
)

// ovPanel names the focusable panels in tab order; panelEvents is display
// only.
type ovPanel int

const (
	panelAttention ovPanel = iota
	panelChecklists
	panelAgents
	panelMonitors
	panelEvents
	numOvPanels
)

const (
	wideWidth      = 100
	eventsMinSpare = 8
)

type workerKey struct{ crew, branch string }

// overviewState is the UI state the renderer reads besides the view model.
type overviewState struct {
	focus    ovPanel
	selected workerKey
	stripOff int
	filter   string
}

type rect struct{ x, y, w, h int }

type entryKind int

const (
	eAttn entryKind = iota
	eCard
	eAgent
	eHeader
	eWatch
	ePR
	eBudget
	eEvent
)

// entry is one logical row of a panel: idx indexes the slice its kind reads,
// worker is the index into ov.Workers (or -1), lines its height.
type entry struct {
	kind   entryKind
	idx    int
	worker int
	lines  int
}

// placed is an entry with the absolute screen rectangle it is drawn at, which
// is also what a mouse click is tested against.
type placed struct {
	entry
	rect rect
}

type panelLayout struct {
	shown bool
	rect  rect
	inner rect
	items []placed
}

type overviewRects struct {
	w, h      int
	allClear  bool
	stripRows int
	stripOff  int
	cardLines int
	panels    [numOvPanels]panelLayout
}

// ovIndex resolves worker lookups, the filter and the selected worker once per
// frame.
type ovIndex struct {
	byKey map[workerKey]int
	vis   []bool
	sel   int
}

func newOvIndex(ov vm.OverviewView, st overviewState) ovIndex {
	ix := ovIndex{byKey: make(map[workerKey]int, len(ov.Workers)), vis: make([]bool, len(ov.Workers)), sel: -1}
	needle := strings.ToLower(st.filter)
	for i, w := range ov.Workers {
		ix.byKey[workerKey{w.Crew, w.Branch}] = i
		hay := strings.ToLower(workerLabel(w) + " " + w.Branch + " " + ovStr(w.Title, ""))
		ix.vis[i] = needle == "" || strings.Contains(hay, needle)
	}
	if i, ok := ix.byKey[st.selected]; ok {
		ix.sel = i
		return ix
	}
	for _, a := range ov.Attention {
		if i := ix.worker(a.Crew, a.Branch); i >= 0 {
			ix.sel = i
			return ix
		}
	}
	if len(ov.Workers) > 0 {
		ix.sel = 0
	}
	return ix
}

func (ix ovIndex) worker(crew, branch string) int {
	if i, ok := ix.byKey[workerKey{crew, branch}]; ok && branch != "" {
		return i
	}
	return -1
}

func ovStr(p *string, fallback string) string {
	if p == nil || *p == "" {
		return fallback
	}
	return *p
}

func workerLabel(w vm.OverviewWorker) string {
	l := ovStr(w.Name, w.Branch)
	if w.Origin != vm.OriginLocal && w.Host != "" {
		l += "@" + w.Host
	}
	return l
}

func ovAge(sec int64) string {
	switch {
	case sec < 5:
		return "now"
	case sec < 60:
		return fmt.Sprintf("%ds", sec)
	case sec < 3600:
		return fmt.Sprintf("%dm", sec/60)
	}
	return fmt.Sprintf("%dh", sec/3600)
}

func checklistEntries(ov vm.OverviewView, ix ovIndex, lines int) []entry {
	var es []entry
	for i := range ov.Workers {
		if ix.vis[i] {
			es = append(es, entry{kind: eCard, idx: i, worker: i, lines: lines})
		}
	}
	return es
}

func agentEntries(ov vm.OverviewView, ix ovIndex) []entry {
	var es []entry
	for i, a := range ov.Agents {
		wi := ix.worker(a.Crew, a.Branch)
		if wi >= 0 && !ix.vis[wi] {
			continue
		}
		es = append(es, entry{kind: eAgent, idx: i, worker: wi, lines: 2})
	}
	return es
}

func monitorEntries(ov vm.OverviewView, ix ovIndex) []entry {
	var es []entry
	m := ov.Monitors
	if len(m.Watchdogs) > 0 {
		es = append(es, entry{kind: eHeader, idx: 0, worker: -1, lines: 1})
		for i, w := range m.Watchdogs {
			es = append(es, entry{kind: eWatch, idx: i, worker: ix.worker(w.Crew, w.Branch), lines: 1})
		}
	}
	if len(m.PRChecks) > 0 {
		es = append(es, entry{kind: eHeader, idx: 1, worker: -1, lines: 1})
		for i, p := range m.PRChecks {
			es = append(es, entry{kind: ePR, idx: i, worker: ix.worker(p.Crew, p.Branch), lines: 1})
		}
	}
	if len(m.Budgets) > 0 {
		es = append(es, entry{kind: eHeader, idx: 2, worker: -1, lines: 1})
		for i := range m.Budgets {
			es = append(es, entry{kind: eBudget, idx: i, worker: -1, lines: 1})
		}
	}
	return es
}

func eventEntries(ov vm.OverviewView) []entry {
	es := make([]entry, len(ov.Events))
	for i := range ov.Events {
		es[i] = entry{kind: eEvent, idx: i, worker: -1, lines: 1}
	}
	return es
}

func totalLines(es []entry) int {
	n := 0
	for _, e := range es {
		n += e.lines
	}
	return n
}

// place windows es into inner so the selected worker's first entry stays
// visible: the window starts at the earliest entry that still fits it.
func place(es []entry, inner rect, selWorker int) []placed {
	sel := 0
	for i, e := range es {
		if selWorker >= 0 && e.worker == selWorker {
			sel = i
			break
		}
	}
	start := 0
	for start < sel && totalLines(es[start:sel+1]) > inner.h {
		start++
	}
	var out []placed
	y := inner.y
	for _, e := range es[start:] {
		if y+e.lines > inner.y+inner.h {
			break
		}
		out = append(out, placed{entry: e, rect: rect{inner.x, y, inner.w, e.lines}})
		y += e.lines
	}
	return out
}

func panelRect(r rect) (outer, inner rect) {
	frame := lipgloss.NewStyle().Border(lipgloss.RoundedBorder())
	fw, fh := frame.GetHorizontalFrameSize(), frame.GetVerticalFrameSize()
	left, top := frame.GetBorderLeftSize(), frame.GetBorderTopSize()
	return r, rect{r.x + left, r.y + top, max0(r.w - fw), max0(r.h - fh)}
}

// overviewLayout is the one geometry the renderer and mouse hit-testing share.
func overviewLayout(w, h int, ov vm.OverviewView, st overviewState) overviewRects {
	ix := newOvIndex(ov, st)
	r := overviewRects{w: w, h: h}
	n := len(ov.Attention)
	y := 0
	if n == 0 {
		r.allClear = true
		y = min(1, h)
	} else {
		rows := min(n, 4)
		if h >= 40 {
			rows = min(n, 9)
		}
		rows = clamp(rows, 1, (h-4)/2)
		stripH := min(rows+2, h)
		r.stripRows = min(rows, max0(stripH-2))
		r.stripOff = clamp(st.stripOff, 0, n-r.stripRows)
		outer, inner := panelRect(rect{0, 0, w, stripH})
		pl := panelLayout{shown: true, rect: outer, inner: inner}
		for i := range r.stripRows {
			idx := r.stripOff + i
			a := ov.Attention[idx]
			pl.items = append(pl.items, placed{
				entry: entry{kind: eAttn, idx: idx, worker: ix.worker(a.Crew, a.Branch), lines: 1},
				rect:  rect{inner.x, inner.y + i, inner.w, 1},
			})
		}
		r.panels[panelAttention] = pl
		y = stripH
	}
	bodyH := max0(h - y)
	if bodyH < 3 {
		return r
	}

	wide := w >= wideWidth
	var shown []ovPanel
	if wide {
		shown = []ovPanel{panelChecklists, panelAgents, panelMonitors}
	} else if st.focus == panelAgents || st.focus == panelMonitors {
		shown = []ovPanel{st.focus}
	} else {
		shown = []ovPanel{panelChecklists}
	}
	widths := map[ovPanel]int{}
	if wide {
		widths[panelChecklists] = w * 40 / 100
		widths[panelAgents] = w * 30 / 100
		widths[panelMonitors] = w - widths[panelChecklists] - widths[panelAgents]
	} else {
		widths[shown[0]] = w
	}

	contentH := 0
	for _, p := range shown {
		var es []entry
		switch p {
		case panelChecklists:
			es = checklistEntries(ov, ix, 3)
		case panelAgents:
			es = agentEntries(ov, ix)
		case panelMonitors:
			es = monitorEntries(ov, ix)
		}
		contentH = max(contentH, totalLines(es))
	}
	contentH += 2

	panelsH, eventsH := bodyH, 0
	if spare := bodyH - contentH; spare >= eventsMinSpare {
		panelsH, eventsH = contentH, spare
	}

	r.cardLines = 3
	if len(checklistEntries(ov, ix, 1))*3 > max0(panelsH-2) {
		r.cardLines = 2
	}
	x := 0
	for _, p := range shown {
		outer, inner := panelRect(rect{x, y, widths[p], panelsH})
		var es []entry
		switch p {
		case panelChecklists:
			es = checklistEntries(ov, ix, r.cardLines)
		case panelAgents:
			es = agentEntries(ov, ix)
		case panelMonitors:
			es = monitorEntries(ov, ix)
		}
		r.panels[p] = panelLayout{shown: true, rect: outer, inner: inner, items: place(es, inner, ix.sel)}
		x += widths[p]
	}
	if eventsH > 0 {
		outer, inner := panelRect(rect{0, y + panelsH, w, eventsH})
		r.panels[panelEvents] = panelLayout{shown: true, rect: outer, inner: inner, items: place(eventEntries(ov), inner, -1)}
	}
	return r
}

// renderOverview draws the Overview body as exactly h lines of at most w cells.
func renderOverview(w, h int, ov vm.OverviewView, st overviewState, mode pillMode) string {
	if w <= 0 || h <= 0 {
		return ""
	}
	r := overviewLayout(w, h, ov, st)
	ix := newOvIndex(ov, st)
	lines := make([]string, 0, h)
	if r.allClear {
		lines = append(lines, truncateLine(" "+fg(colGreen).Render("✓ all clear"), w))
	}

	specs := [numOvPanels]panelSpec{
		panelAttention:  {title: "ATTENTION", count: fmt.Sprintf("%d need action", len(ov.Attention)), color: colAmber},
		panelChecklists: {title: "CHECKLISTS", count: fmt.Sprintf("%d of %d done", ov.Summary.ByState["done"], ov.Summary.Workers), color: colTeal},
		panelAgents:     {title: "AGENTS", count: fmt.Sprintf("%d agents", ov.Summary.Agents), color: colPurple},
		panelMonitors:   {title: "MONITORS", count: fmt.Sprintf("%d watched", len(ov.Monitors.Watchdogs)), color: colAmber},
		panelEvents:     {title: "RECENT EVENTS", count: fmt.Sprintf("%d events", len(ov.Events)), color: colBlue},
	}
	body := func(p ovPanel) []string {
		pl := r.panels[p]
		sp := specs[p]
		sp.focused = st.focus == p || (p == panelChecklists && !r.panels[st.focus].shown)
		var content []string
		for _, it := range pl.items {
			content = append(content, rowLines(ov, ix, r, it, pl.inner.w)...)
		}
		return renderPanel(pl.rect.w, pl.rect.h, sp, mode, content)
	}

	if r.panels[panelAttention].shown {
		lines = append(lines, body(panelAttention)...)
	}
	var cols [][]string
	for _, p := range []ovPanel{panelChecklists, panelAgents, panelMonitors} {
		if r.panels[p].shown {
			cols = append(cols, body(p))
		}
	}
	if len(cols) > 0 {
		for i := range cols[0] {
			var sb strings.Builder
			for _, c := range cols {
				sb.WriteString(c[i])
			}
			lines = append(lines, sb.String())
		}
	}
	if r.panels[panelEvents].shown {
		lines = append(lines, body(panelEvents)...)
	}
	for len(lines) < h {
		lines = append(lines, "")
	}
	for i := range lines {
		lines[i] = truncateLine(lines[i], w)
	}
	return strings.Join(lines[:h], "\n")
}

func stateGlyph(state string) string {
	switch state {
	case "failed", "exited":
		return "✗"
	case "done":
		return "✓"
	case "working", "blocked", "pr_open":
		return "●"
	}
	return "○"
}

func glyphColor(state string) lipgloss.Color {
	switch state {
	case "working", "blocked", "pr_open", "failed", "exited", "done":
		return stateColor(state)
	}
	return colDim
}

func severityGlyph(s vm.Severity) (string, lipgloss.Color) {
	switch s {
	case vm.SeverityCritical:
		return "✗", colRed
	case vm.SeverityReady:
		return "✓", colGreen
	}
	return "!", colAmber
}

// rowLines renders one placed entry as len == entry.lines lines of width iw.
func rowLines(ov vm.OverviewView, ix ovIndex, r overviewRects, it placed, iw int) []string {
	var bg lipgloss.TerminalColor = lipgloss.NoColor{}
	selected := it.worker >= 0 && it.worker == ix.sel && it.kind != eHeader
	if selected {
		bg = colSelBg
	}
	line := func() *spans {
		s := newSpans(iw, bg)
		if selected {
			return s.add("▌", fg(colTeal))
		}
		return s.add(" ", lipgloss.NewStyle())
	}
	switch it.kind {
	case eAttn:
		return []string{attnLine(ov, r, it, line())}
	case eCard:
		return cardLines(ov, r.cardLines, it, line, iw)
	case eAgent:
		return agentLines(ov, it, line)
	case eHeader:
		return []string{headerLine(it.idx, iw)}
	case eWatch:
		return []string{watchLine(ov, it, line(), iw)}
	case ePR:
		return []string{prLine(ov, it, line())}
	case eBudget:
		return []string{budgetLine(ov, it, line(), iw)}
	}
	return []string{ovEventLine(ov, it, line(), iw)}
}

func attnLine(ov vm.OverviewView, r overviewRects, it placed, s *spans) string {
	a := ov.Attention[it.idx]
	visible := ov.Attention[r.stripOff : r.stripOff+r.stripRows]
	workerW, actionW := 0, 0
	for _, v := range visible {
		workerW = max(workerW, ansi.StringWidth(attnWorker(v)))
		if v.Action != nil {
			actionW = max(actionW, ansi.StringWidth(v.Action.Label))
		}
	}
	iw := s.limit
	workerW = min(workerW, max(4, min(14, iw/5)))
	actionPart := 0
	if actionW > 0 {
		actionPart = 3 + actionW
	}
	const ageW = 4
	msgW := iw - 5 - workerW - 1 - 1 - ageW - actionPart - 1
	glyph, gc := severityGlyph(a.Severity)

	s.add(fmt.Sprint(it.idx-r.stripOff+1), fg(colDim).Bold(true)).
		add(" ", lipgloss.NewStyle()).
		add(glyph, fg(gc)).
		add(" ", lipgloss.NewStyle()).
		col(attnWorker(a), workerW, fg(colText).Bold(true), false).
		add(" ", lipgloss.NewStyle()).
		col(a.Message, msgW, fg(colText), false).
		add(" ", lipgloss.NewStyle()).
		col(ovAge(a.AgeSec), ageW, fg(colDim), true)
	if a.Action != nil {
		s.add(" → ", fg(colDim)).add(a.Action.Label, fg(gc))
	}
	return s.String()
}

func attnWorker(a vm.AttentionItem) string {
	if a.Worker != "" {
		return a.Worker
	}
	return a.Crew
}

func cardLines(ov vm.OverviewView, lines int, it placed, line func() *spans, iw int) []string {
	w := ov.Workers[it.idx]
	ac := "AC —"
	if w.Acceptance != nil {
		ac = fmt.Sprintf("AC %d/%d", w.Acceptance.Done, w.Acceptance.Total)
	}
	rightW := ansi.StringWidth(ac) + 1

	l1 := line()
	l1.add(stateGlyph(w.State), fg(glyphColor(w.State))).add(" ", lipgloss.NewStyle())
	l1.limit = max0(iw - rightW)
	l1.add(workerLabel(w), fg(colText).Bold(true)).add(" "+ovStr(w.Tier, "?"), fg(colDim))
	l1.limit = iw
	l1.padTo(iw-rightW+1).add(ac, fg(colDim))

	l2 := line().add("  ", lipgloss.NewStyle())
	cells := phaseCells(w.Phase)
	if cells == nil {
		l2.add("?", fg(colDim))
	}
	for i, c := range cells {
		if i > 0 {
			l2.add("─", fg(colDim))
		}
		l2.add(c.glyph, trackStyle(c.status))
	}
	l2.add(" "+phaseLabel(w), fg(colDim))

	out := []string{l1.String(), l2.String()}
	if lines >= 3 {
		out = append(out, line().add("  "+ovStr(w.Title, "—"), fg(colDim)).String())
	}
	return out
}

func phaseLabel(w vm.OverviewWorker) string {
	if w.Phase == nil {
		return ""
	}
	if w.Phase.Current != nil {
		return *w.Phase.Current
	}
	return w.State
}

func agentLines(ov vm.OverviewView, it placed, line func() *spans) []string {
	a := ov.Agents[it.idx]
	label := a.Branch
	if it.worker >= 0 {
		label = workerLabel(ov.Workers[it.worker])
	}
	l1 := line().
		add(stateGlyph(a.Lead.State), fg(glyphColor(a.Lead.State))).
		add(" ", lipgloss.NewStyle()).
		add(label, fg(colText).Bold(true)).
		add(" "+ovStr(a.Lead.Model, ovStr(a.Lead.Engine, "?")), fg(colPurple)).
		add(" "+ovStr(a.Lead.Tier, "?"), fg(colDim))
	l2 := line().add("  roles ", fg(colDim))
	switch {
	case a.Roles == nil:
		l2.add("?", fg(colDim))
	case len(a.Roles) == 0:
		l2.add("—", fg(colDim))
	}
	for _, role := range a.Roles {
		name := ovStr(role.Role, "?")
		l2.add(stateGlyph(role.State), fg(glyphColor(role.State))).
			add(" "+ansi.Truncate(name, 3, "")+" ", fg(colDim))
	}
	return []string{l1.String(), l2.String()}
}

func headerLine(section, iw int) string {
	title := [...]string{"WATCHDOG", "PR CHECKS", "BUDGET"}[section]
	s := newSpans(iw, nil).add(" "+title+" ", fg(colDim))
	return s.add(strings.Repeat("─", max0(iw-s.w)), fg(colBorderOff)).String()
}

func watchLine(ov vm.OverviewView, it placed, s *spans, iw int) string {
	w := ov.Monitors.Watchdogs[it.idx]
	name := w.Branch
	if it.worker >= 0 {
		name = workerLabel(ov.Workers[it.worker])
	}
	const nameW, labelW = 10, 10
	sparkW := clamp(iw-1-nameW-1-1-labelW, 4, 30)
	return s.col(name, nameW, fg(colText), false).
		add(" ", lipgloss.NewStyle()).
		add(sparkline(w.Activity, sparkW), fg(glyphColor(w.State))).
		add(" ", lipgloss.NewStyle()).
		add(w.Label, fg(glyphColor(w.State))).String()
}

func checkColor(v, good, bad string) lipgloss.Color {
	switch v {
	case good:
		return colGreen
	case bad:
		return colRed
	}
	return colDim
}

func prLine(ov vm.OverviewView, it placed, s *spans) string {
	p := ov.Monitors.PRChecks[it.idx]
	return s.add(fmt.Sprintf("#%d ", p.PR), fg(colBlue)).
		add("●", fg(checkColor(p.Checks, "SUCCESS", "FAILURE"))).add(" checks ", fg(colDim)).
		add("●", fg(checkColor(p.Mergeable, "MERGEABLE", "CONFLICTING"))).add(" merge ", fg(colDim)).
		add(ovAge(p.AgeSec), fg(colDim)).String()
}

func budgetLine(ov vm.OverviewView, it placed, s *spans, iw int) string {
	b := ov.Monitors.Budgets[it.idx]
	const labelW, pctW = 11, 5
	barW := clamp(iw-1-labelW-1-pctW-2, 4, 20)
	filled := clamp(int(b.UsedPct/100*float64(barW)+0.5), 0, barW)
	pace := ""
	switch b.Pace {
	case "ahead":
		pace = " ↑"
	case "behind":
		pace = " ↓"
	}
	c := lipgloss.Color(gaugeColor(b.UsedPct))
	return s.col(b.Label, labelW, fg(colText), false).
		add(" ", lipgloss.NewStyle()).
		add(strings.Repeat("█", filled), fg(c)).
		add(strings.Repeat("░", barW-filled), fg(colBorderOff)).
		col(fmt.Sprintf("%.0f%%", b.UsedPct), pctW, fg(c), true).
		add(pace, fg(colDim)).String()
}

func ovEventLine(ov vm.OverviewView, it placed, s *spans, iw int) string {
	e := ov.Events[it.idx]
	const workerW = 14
	return s.col(ovAge(ov.GeneratedAt-e.TS), 4, fg(colDim), true).
		add(" ", lipgloss.NewStyle()).
		col(e.Worker, workerW, fg(colText).Bold(true), false).
		add(" ", lipgloss.NewStyle()).
		col(e.Text, iw-1-4-1-workerW-1, fg(colText), false).String()
}
