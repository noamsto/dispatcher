package ui

import (
	"fmt"
	"slices"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	vm "github.com/noamsto/dispatcher/dash/internal/view"
)

// ovPanel names the focusable panels in tab order.
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

// overviewState is what the operator asked for, never a derived selection:
// resolve turns it into what is drawn and acted on, so nothing has to keep the
// two in step.
type overviewState struct {
	focus ovPanel
	ret   ovPanel // the body panel k last left for the strip; anything but agents or monitors means checklists
	// retWorker is the row k left from: the strip cursor's own worker is shown
	// there, so j needs it kept to land back on that row.
	retWorker workerKey
	cursor    string    // the strip cursor's attention item ID
	cursorAt  int       // the cursor's index when set: the tie-break, and the fallback once its item leaves
	worker    workerKey // the worker chosen outside the strip
	stripOff  int
	evCursor  int // the Recent Events cursor's row, clamped to the rows there are
	evOff     int // the first Recent Events row shown, kept so the window holds the cursor
	filter    string
	narrow    bool // stack the panels whatever the width
}

// isNarrow is whether the Overview stacks its panels: forced, or too narrow
// for three columns.
func isNarrow(w int, st overviewState) bool { return st.narrow || w < wideWidth }

// ovSel is the resolved selection: the worker every panel highlights and
// actions act on, and the strip cursor. Either is -1 for none.
type ovSel struct{ worker, cursor int }

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
	evCursor  int // -1 with no events
	evOff     int
	cardLines int
	narrow    bool
	evFromW   int       // sender column width shared by every event row
	evCrewW   int       // crew label column width; 0 when scoped
	focus     ovPanel   // the focus the layout was resolved for
	stack     []ovPanel // narrow only: the body panels and events, top to bottom
	sel       ovSel
	panels    [numOvPanels]panelLayout
}

// ovIndex resolves worker lookups and the filter once per frame.
type ovIndex struct {
	byKey  map[workerKey]int
	vis    []bool
	needle string
}

func newOvIndex(ov vm.OverviewView, st overviewState) ovIndex {
	ix := ovIndex{byKey: make(map[workerKey]int, len(ov.Workers)), vis: make([]bool, len(ov.Workers)),
		needle: strings.ToLower(st.filter)}
	for i, w := range ov.Workers {
		ix.byKey[workerKey{w.Crew, w.Branch}] = i
		ix.vis[i] = ix.matches(workerLabel(w) + " " + w.Branch + " " + ovStr(w.Title, ""))
	}
	return ix
}

// worker is the index of the worker row with this key, or -1. A row without a
// branch (a peer placeholder) is never found.
func (ix ovIndex) worker(crew, branch string) int {
	if i, ok := ix.byKey[workerKey{crew, branch}]; ok && branch != "" {
		return i
	}
	return -1
}

// pick is worker narrowed to a selectable one: it also passes the filter.
// Every row's worker comes from here, so a row can only highlight, or select
// on a click, a worker that resolve accepts.
func (ix ovIndex) pick(crew, branch string) int {
	if i := ix.worker(crew, branch); i >= 0 && ix.vis[i] {
		return i
	}
	return -1
}

// cursorIndex is the strip cursor's item: the item with the cursor's ID
// nearest its last index, else that index clamped, or -1 with no items.
func cursorIndex(ov vm.OverviewView, st overviewState) int {
	return attnCursor(ov.Attention, st.cursor, st.cursorAt)
}

func attnCursor(items []vm.AttentionItem, id string, at int) int {
	best := -1
	for i, a := range items {
		if a.ID == id && (best < 0 || absInt(i-at) < absInt(best-at)) {
			best = i
		}
	}
	if best < 0 && len(items) > 0 {
		best = clamp(at, 0, len(items)-1)
	}
	return best
}

// stripWindow sizes the attention strip of n > 0 items in a frame h rows
// high: its height with the frame, the rows shown and the first shown item.
// The window always holds the cursor, whatever offset was stored before a
// resize or a snapshot moved it.
func stripWindow(n, h, off, cursor int) (stripH, rows, start int) {
	want := min(n, 4)
	if h >= 40 {
		want = min(n, 9)
	}
	want = clamp(want, 1, (h-4)/2)
	stripH = min(want+2, h)
	rows = min(want, max0(stripH-2))
	start = clamp(off, 0, n-rows)
	if rows > 0 {
		start = clamp(start, cursor-rows+1, cursor)
	}
	return stripH, rows, start
}

func absInt(n int) int { return max(n, -n) }

// resolve is the one answer to what is selected. With the strip focused it is
// the cursor and its item's worker, or no worker; otherwise the chosen worker,
// else the first the shown panels list. Only a worker that a shown panel draws
// on a row of its own can be selected, so the highlight and the target agree.
func resolve(ov vm.OverviewView, st overviewState, ix ovIndex, shown []ovPanel) ovSel {
	sel := ovSel{worker: -1, cursor: cursorIndex(ov, st)}
	if st.focus == panelEvents {
		return sel
	}
	var cands []int
	for _, p := range shown {
		cands = append(cands, panelWorkers(p, ov, ix)...)
	}
	want := ix.worker(st.worker.crew, st.worker.branch)
	if effectiveFocus(ov, st.focus) == panelAttention {
		a := ov.Attention[sel.cursor]
		want = ix.worker(a.Crew, a.Branch)
	}
	switch {
	case slices.Contains(cands, want):
		sel.worker = want
	case effectiveFocus(ov, st.focus) != panelAttention && len(cands) > 0:
		sel.worker = cands[0]
	}
	return sel
}

func panelWorkers(p ovPanel, ov vm.OverviewView, ix ovIndex) []int {
	var es []entry
	switch p {
	case panelChecklists:
		es = checklistEntries(ov, ix, 1)
	case panelAgents:
		es = agentEntries(ov, ix)
	case panelMonitors:
		es = monitorEntries(ov, ix)
	}
	var ws []int
	for _, e := range es {
		if e.worker >= 0 && !slices.Contains(ws, e.worker) {
			ws = append(ws, e.worker)
		}
	}
	return ws
}

func (ix ovIndex) matches(hay string) bool {
	return ix.needle == "" || strings.Contains(strings.ToLower(hay), ix.needle)
}

// effectiveFocus is the panel that holds focus as drawn: the strip hands it to
// the checklists while it has no items. The narrow layout always draws the
// focused panel, so the strip is the only one that hides.
func effectiveFocus(ov vm.OverviewView, f ovPanel) ovPanel {
	if f == panelAttention && len(ov.Attention) == 0 {
		return panelChecklists
	}
	return f
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

// rowLabel names a monitor or agent row that has no worker: its own branch,
// and its host when it has one, as the placeholder's checklist card reads.
func rowLabel(branch, host string) string {
	if host == "" {
		return branch
	}
	return branch + "@" + host
}

// ageColW is the width of the right-aligned age column in event and
// attention rows.
const ageColW = 4

func ovAge(sec int64) string {
	return vm.Age(sec)
}

// checklistEntries is one card per visible worker. A peer placeholder with no
// branch gets worker -1: nothing can select or act on it.
func checklistEntries(ov vm.OverviewView, ix ovIndex, lines int) []entry {
	var es []entry
	for i, w := range ov.Workers {
		if !ix.vis[i] {
			continue
		}
		wk := i
		if w.Branch == "" {
			wk = -1
		}
		es = append(es, entry{kind: eCard, idx: i, worker: wk, lines: lines})
	}
	return es
}

func agentEntries(ov vm.OverviewView, ix ovIndex) []entry {
	var es []entry
	for i, a := range ov.Agents {
		// A lead with no worker row (a peer placeholder without a branch) is
		// matched on its own branch and host.
		wi := ix.worker(a.Crew, a.Branch)
		if wi >= 0 && !ix.vis[wi] || wi < 0 && !ix.matches(a.Branch+" "+a.Host) {
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
			es = append(es, entry{kind: eWatch, idx: i, worker: ix.pick(w.Crew, w.Branch), lines: 1})
		}
	}
	if len(m.PRChecks) > 0 {
		es = append(es, entry{kind: eHeader, idx: 1, worker: -1, lines: 1})
		for i, p := range m.PRChecks {
			es = append(es, entry{kind: ePR, idx: i, worker: ix.pick(p.Crew, p.Branch), lines: 1})
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

// overviewLayout is the one geometry the renderer, mouse hit-testing and every
// action share, and the only place the selection is resolved and the strip
// window computed.
func overviewLayout(w, h int, ov vm.OverviewView, st overviewState) overviewRects {
	r := layoutPanels(w, h, ov, st)
	r.focus = st.focus
	if st.focus == panelEvents && !r.panels[panelEvents].shown {
		// Events focused but not drawn (a resize took them): the checklists
		// hold focus, as settleFocus makes it on the next key.
		st.focus = panelChecklists
		r = layoutPanels(w, h, ov, st)
		r.focus = st.focus
	}
	return r
}

func layoutPanels(w, h int, ov vm.OverviewView, st overviewState) overviewRects {
	ix := newOvIndex(ov, st)
	narrow := isNarrow(w, st)
	r := overviewRects{w: w, h: h, narrow: narrow}
	n := len(ov.Attention)
	y := 0
	if n == 0 {
		r.allClear = true
		y = min(1, h)
	} else {
		var stripH int
		stripH, r.stripRows, r.stripOff = stripWindow(n, h, st.stripOff, cursorIndex(ov, st))
		if cap := max(3, h/3); narrow && stripH > cap {
			stripH = cap
			r.stripRows = min(r.stripRows, max0(stripH-2))
			r.stripOff = clamp(r.stripOff, 0, n-r.stripRows)
			if r.stripRows > 0 {
				c := cursorIndex(ov, st)
				r.stripOff = clamp(r.stripOff, c-r.stripRows+1, c)
			}
		}
		outer, inner := panelRect(rect{0, 0, w, stripH})
		pl := panelLayout{shown: true, rect: outer, inner: inner}
		for i := range r.stripRows {
			idx := r.stripOff + i
			a := ov.Attention[idx]
			pl.items = append(pl.items, placed{
				entry: entry{kind: eAttn, idx: idx, worker: ix.pick(a.Crew, a.Branch), lines: 1},
				rect:  rect{inner.x, inner.y + i, inner.w, 1},
			})
		}
		r.panels[panelAttention] = pl
		y = stripH
	}
	bodyH := max0(h - y)
	if narrow {
		r.stackBody(ov, st, ix, y, bodyH)
		return r
	}
	var shown []ovPanel
	if bodyH >= 4 { // 2-row frame plus the smallest, 2-line entry
		shown = []ovPanel{panelChecklists, panelAgents, panelMonitors}
	}
	r.sel = resolve(ov, st, ix, shown)
	if len(shown) == 0 {
		return r
	}

	widths := map[ovPanel]int{}
	widths[panelChecklists] = w * 40 / 100
	widths[panelAgents] = w * 30 / 100
	widths[panelMonitors] = w - widths[panelChecklists] - widths[panelAgents]

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
		contentH = max(contentH, max(1, totalLines(es)))
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
	anchor := r.windowAnchor(ix, st)
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
		r.panels[p] = panelLayout{shown: true, rect: outer, inner: inner, items: place(es, inner, anchor)}
		x += widths[p]
	}
	if eventsH > 0 {
		outer, inner := panelRect(rect{0, y + panelsH, w, eventsH})
		r.placeEvents(ov, st, outer, inner)
	}
	return r
}

// windowAnchor is the worker the body panels keep in view. With Recent Events
// focused nothing is selected, but the panels stay scrolled where they were.
func (r overviewRects) windowAnchor(ix ovIndex, st overviewState) int {
	if st.focus == panelEvents {
		return ix.pick(st.worker.crew, st.worker.branch)
	}
	return r.sel.worker
}

// placeEvents lays the event rows into inner, one line each. Like the strip,
// the window keeps the offset it had and moves only to hold the cursor.
func (r *overviewRects) placeEvents(ov vm.OverviewView, st overviewState, outer, inner rect) {
	es := eventEntries(ov)
	rows := min(inner.h, len(es))
	r.evCursor, r.evOff = -1, 0
	if len(es) > 0 {
		r.evCursor = clamp(st.evCursor, 0, len(es)-1)
		r.evOff = clamp(clamp(st.evOff, 0, len(es)-rows), r.evCursor-rows+1, r.evCursor)
	}
	r.evFromW, r.evCrewW = 0, 0
	for _, e := range ov.Events {
		r.evFromW = max(r.evFromW, ansi.StringWidth(cleanText(eventFrom(ov, e.Worker))))
		if ov.Scope.Crew == nil {
			r.evCrewW = max(r.evCrewW, ansi.StringWidth(cleanText(crewTag(e.Crew))))
		}
	}
	r.evFromW = min(r.evFromW, inner.w/3)
	pl := panelLayout{shown: true, rect: outer, inner: inner}
	for i := range rows {
		pl.items = append(pl.items, placed{entry: es[r.evOff+i], rect: rect{inner.x, inner.y + i, inner.w, 1}})
	}
	r.panels[panelEvents] = pl
}

// neighbour is the shown panel next to from in direction (dx, dy), each -1, 0
// or 1: it must lie wholly beyond from's edge and overlap it across the other
// axis. The nearest wins, then the one closest to from's centre (a full-width
// panel counts as its left edge, so it prefers the first panel of a row).
func (r overviewRects) neighbour(from ovPanel, dx, dy int) (ovPanel, bool) {
	f := r.panels[from].rect
	if !r.panels[from].shown {
		return 0, false
	}
	overlap := func(a0, a1, b0, b1 int) bool { return a0 < b1 && b0 < a1 }
	cx, cy := f.x+f.w/2, f.y+f.h/2
	if f.w >= r.w {
		cx = 0
	}
	best, bestGap, bestOff, found := ovPanel(0), 0, 0, false
	for p := range numOvPanels {
		q := r.panels[p].rect
		if p == from || !r.panels[p].shown {
			continue
		}
		var gap, off int
		switch {
		case dx < 0 && overlap(f.y, f.y+f.h, q.y, q.y+q.h):
			gap, off = f.x-(q.x+q.w), absInt(q.y+q.h/2-cy)
		case dx > 0 && overlap(f.y, f.y+f.h, q.y, q.y+q.h):
			gap, off = q.x-(f.x+f.w), absInt(q.y+q.h/2-cy)
		case dy < 0 && overlap(f.x, f.x+f.w, q.x, q.x+q.w):
			gap, off = f.y-(q.y+q.h), absInt(q.x+q.w/2-cx)
		case dy > 0 && overlap(f.x, f.x+f.w, q.x, q.x+q.w):
			gap, off = q.y-(f.y+f.h), absInt(q.x+q.w/2-cx)
		default:
			continue
		}
		if gap < 0 {
			continue
		}
		if !found || gap < bestGap || gap == bestGap && off < bestOff {
			best, bestGap, bestOff, found = p, gap, off, true
		}
	}
	return best, found
}

func panelEntries(p ovPanel, ov vm.OverviewView, ix ovIndex, cardLines int) []entry {
	switch p {
	case panelChecklists:
		return checklistEntries(ov, ix, cardLines)
	case panelAgents:
		return agentEntries(ov, ix)
	}
	return monitorEntries(ov, ix)
}

// stackBody lays the body panels out one above another from row y in bodyH
// rows. The focused panel is drawn before any other is dropped, then agents,
// checklists and monitors; each takes what its rows need up to a share of what
// is left, and events fill the rest.
func (r *overviewRects) stackBody(ov vm.OverviewView, st overviewState, ix ovIndex, y, bodyH int) {
	const minH = 4 // 2-row frame plus the smallest, 2-line entry
	var shown []ovPanel
	if bodyH >= minH {
		prio := []ovPanel{panelAgents, panelChecklists, panelMonitors}
		if f := effectiveFocus(ov, st.focus); f != panelAttention && f != panelEvents {
			prio = slices.Insert(slices.DeleteFunc(prio, func(p ovPanel) bool { return p == f }), 0, f)
		}
		keep := prio[:min(len(prio), bodyH/minH)]
		for _, p := range []ovPanel{panelAgents, panelChecklists, panelMonitors} {
			if slices.Contains(keep, p) {
				shown = append(shown, p)
			}
		}
	}
	r.sel = resolve(ov, st, ix, shown)

	anchor := r.windowAnchor(ix, st)
	r.cardLines = 3
	left := bodyH
	for i, p := range shown {
		want := max(1, totalLines(panelEntries(p, ov, ix, 3))) + 2
		share := left - minH*(len(shown)-1-i)
		switch {
		case i == len(shown)-1:
			share = max(left*2/3, min(want, left-minH)) // events get what is left when it is enough
		case p == panelAgents:
			share = min(share, bodyH/3)
		case p == panelChecklists:
			share = min(share, left/2)
		}
		ph := clamp(want, minH, max(minH, share))
		if left-ph < minH && (i == len(shown)-1 || left-ph < 0) {
			ph = left // no room for another panel: absorb the slack
		}
		if p == panelChecklists && len(checklistEntries(ov, ix, 1))*3 > max0(ph-2) {
			r.cardLines = 2
		}
		outer, inner := panelRect(rect{0, y, r.w, ph})
		es := panelEntries(p, ov, ix, r.cardLines)
		r.panels[p] = panelLayout{shown: true, rect: outer, inner: inner, items: place(es, inner, anchor)}
		r.stack = append(r.stack, p)
		y += ph
		left -= ph
	}
	if left >= minH {
		outer, inner := panelRect(rect{0, y, r.w, left})
		r.placeEvents(ov, st, outer, inner)
		r.stack = append(r.stack, panelEvents)
	}
}

func renderOverview(w, h int, ov vm.OverviewView, st overviewState, mode pillMode) string {
	if w <= 0 || h <= 0 {
		return ""
	}
	r := overviewLayout(w, h, ov, st)
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
		sp.focused = effectiveFocus(ov, r.focus) == p
		var content []string
		for _, it := range pl.items {
			content = append(content, rowLines(ov, r, st, it, pl.inner.w)...)
		}
		if len(content) == 0 {
			content = []string{" " + fg(colDim).Render(emptyReason(ov, p))}
		}
		return renderPanel(pl.rect.w, pl.rect.h, sp, mode, content)
	}

	if r.panels[panelAttention].shown {
		lines = append(lines, body(panelAttention)...)
	}
	if r.narrow {
		for _, p := range r.stack {
			lines = append(lines, body(p)...)
		}
	}
	var cols [][]string
	for _, p := range []ovPanel{panelChecklists, panelAgents, panelMonitors} {
		if !r.narrow && r.panels[p].shown {
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
	if !r.narrow && r.panels[panelEvents].shown {
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

// emptyReason says why a panel has no rows. It speaks only when the input has
// none, so a search filter that hides every row stays silent.
func emptyReason(ov vm.OverviewView, p ovPanel) string {
	switch p {
	case panelChecklists:
		if len(ov.Workers) == 0 {
			return "no live workers in this crew"
		}
	case panelAgents:
		if len(ov.Agents) == 0 {
			return "no live agents"
		}
	case panelMonitors:
		m := ov.Monitors
		if len(m.Watchdogs)+len(m.PRChecks)+len(m.Budgets) == 0 {
			return "nothing watched"
		}
	case panelEvents:
		if len(ov.Events) == 0 {
			return "no bus events yet"
		}
	}
	return ""
}

func stateGlyph(state string) string {
	switch state {
	case "failed", "exited":
		return "✗"
	case "done":
		return "✓"
	case "migrated":
		return "→"
	case "working", "blocked", "answered", "pr_open":
		return "●"
	}
	return "○"
}

func glyphColor(state string) lipgloss.Color {
	switch state {
	case "working", "blocked", "answered", "pr_open", "failed", "exited", "done", "migrated":
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

func rowLines(ov vm.OverviewView, r overviewRects, st overviewState, it placed, iw int) []string {
	var bg lipgloss.TerminalColor = lipgloss.NoColor{}
	selected := it.worker >= 0 && it.worker == r.sel.worker && it.kind != eHeader
	if it.kind == eAttn && effectiveFocus(ov, r.focus) == panelAttention {
		// The focused strip highlights its own cursor, which may name no worker.
		selected = it.idx == r.sel.cursor
	}
	if it.kind == eEvent {
		selected = r.focus == panelEvents && it.idx == r.evCursor
	}
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
		return agentLines(ov, it, line, r.narrow)
	case eHeader:
		return []string{headerLine(it.idx, iw)}
	case eWatch:
		return []string{watchLine(ov, it, line(), iw)}
	case ePR:
		return []string{prLine(ov, it, line())}
	case eBudget:
		return []string{budgetLine(ov, it, line(), iw)}
	}
	return []string{ovEventLine(ov, r, it, line(), iw)}
}

func attnLine(ov vm.OverviewView, r overviewRects, it placed, s *spans) string {
	return attnStripLine(ov.Attention[r.stripOff:r.stripOff+r.stripRows], it.idx-r.stripOff, attnWorker, s)
}

// clearsMinWidth is the message column width below which the strip drops the
// "what clears it" suffix; the drill overlay always shows it.
const clearsMinWidth = 72

// attnStripLine draws row i of an attention strip window: its digit, glyph,
// who (from label), message, age and action, columns sized to the window.
func attnStripLine(visible []vm.AttentionItem, i int, label func(vm.AttentionItem) string, s *spans) string {
	a := visible[i]
	workerW, actionW := 0, 0
	for _, v := range visible {
		workerW = max(workerW, ansi.StringWidth(label(v)))
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
	msgW := iw - 5 - workerW - 1 - 1 - ageColW - actionPart - 1
	glyph, gc := severityGlyph(a.Severity)
	workerSt := fg(colText).Bold(true)
	if a.Worker != "" {
		workerSt = codenameStyle(a.Tmux, workerSt)
	}
	msg := a.Message
	if msgW >= clearsMinWidth && a.Clears != "" {
		msg += " — " + a.Clears
	}

	s.add(fmt.Sprint(i+1), fg(colDim).Bold(true)).
		add(" ", lipgloss.NewStyle()).
		add(glyph, fg(gc)).
		add(" ", lipgloss.NewStyle()).
		col(label(a), workerW, workerSt, false).
		add(" ", lipgloss.NewStyle()).
		col(msg, msgW, fg(colText), false).
		add(" ", lipgloss.NewStyle()).
		col(ovAge(a.AgeSec), ageColW, fg(colDim), true)
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
	l1.add(workerLabel(w), codenameStyle(w.Tmux, fg(colText).Bold(true))).add(" "+ovStr(w.Tier, "?"), fg(colDim))
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
	if d := w.Dispatcher; d != nil && d.Where == "elsewhere" && d.Repo != nil {
		l2.add(" · dispatcher in "+*d.Repo, fg(colDim))
	}

	out := []string{l1.String(), l2.String()}
	if lines >= 3 {
		out = append(out, line().add("  "+ovStr(w.Title, "—"), fg(colDim)).String())
	}
	return out
}

func phaseLabel(w vm.OverviewWorker) string {
	if w.NeedsCleanup {
		return "stale"
	}
	if w.Phase == nil {
		return ""
	}
	if w.Phase.Current != nil {
		return *w.Phase.Current
	}
	return w.State
}

func agentLines(ov vm.OverviewView, it placed, line func() *spans, narrow bool) []string {
	a := ov.Agents[it.idx]
	label := rowLabel(a.Branch, a.Host)
	labelStyle := fg(colText).Bold(true)
	if it.worker >= 0 {
		label = workerLabel(ov.Workers[it.worker])
	}
	l1 := line()
	if it.worker >= 0 {
		labelStyle = codenameStyle(ov.Workers[it.worker].Tmux, labelStyle)
	}
	l1.add(stateGlyph(a.Lead.State), fg(glyphColor(a.Lead.State))).
		add(" ", lipgloss.NewStyle()).
		add(label, labelStyle).
		add(" "+ovStr(a.Lead.Model, ovStr(a.Lead.Engine, "?")), fg(colPurple))
	if !narrow {
		l1.add(" "+ovStr(a.Lead.Tier, "?"), fg(colDim))
	}
	l2 := line().add("  roles ", fg(colDim))
	switch {
	case a.Roles == nil:
		l2.add("?", fg(colDim))
	case len(a.Roles) == 0:
		l2.add("—", fg(colDim))
	}
	for _, role := range a.Roles {
		name := ovStr(role.Role, "?")
		state := roleState(role)
		l2.add(stateGlyph(state), fg(glyphColor(state))).
			add(" "+ansi.Truncate(name, 3, "")+" ", fg(colDim))
	}
	running := 0
	for _, n := range a.Execute {
		if n.State == "running" {
			running++
		}
	}
	if running > 0 {
		l2.add("  sub ", fg(colDim)).
			add(stateGlyph("working"), fg(glyphColor("working"))).
			add(fmt.Sprint(running), fg(colText))
	}
	return []string{l1.String(), l2.String()}
}

func headerLine(section, iw int) string {
	return sectionLine([...]string{"WATCHDOG", "PR CHECKS", "BUDGET"}[section], iw)
}

// sectionLine is a dim subheading ruled to the inner width.
func sectionLine(title string, iw int) string {
	s := newSpans(iw, nil).add(" "+title+" ", fg(colDim))
	return s.add(strings.Repeat("─", max0(iw-s.w)), fg(colBorderOff)).String()
}

func watchLine(ov vm.OverviewView, it placed, s *spans, iw int) string {
	w := ov.Monitors.Watchdogs[it.idx]
	name := rowLabel(w.Branch, w.Host)
	nameSt := fg(colText)
	if it.worker >= 0 {
		name = workerLabel(ov.Workers[it.worker])
		nameSt = codenameStyle(ov.Workers[it.worker].Tmux, nameSt)
	}
	const nameW, labelW = 10, 10
	sparkW := clamp(iw-1-nameW-1-1-labelW, 4, 30)
	return s.col(name, nameW, nameSt, false).
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
	return budgetBar(ov.Monitors.Budgets[it.idx], s, iw)
}

// gaugeLabelW is the label column of a gauge row (budget windows, capacity).
const gaugeLabelW = 11

// budgetBar is one budget window: label, usage gauge, percent and pace arrow.
func budgetBar(b vm.BudgetRow, s *spans, iw int) string {
	const pctW = 5
	barW := clamp(iw-1-gaugeLabelW-1-pctW-2, 4, 20)
	filled := clamp(int(b.UsedPct/100*float64(barW)+0.5), 0, barW)
	pace := ""
	switch b.Pace {
	case "ahead":
		pace = " ↑"
	case "behind":
		pace = " ↓"
	}
	c := lipgloss.Color(gaugeColor(b.UsedPct))
	return s.col(b.Label, gaugeLabelW, fg(colText), false).
		add(" ", lipgloss.NewStyle()).
		add(strings.Repeat("█", filled), fg(c)).
		add(strings.Repeat("░", barW-filled), fg(colBorderOff)).
		col(fmt.Sprintf("%.0f%%", b.UsedPct), pctW, fg(c), true).
		add(pace, fg(colDim)).String()
}

func ovEventLine(ov vm.OverviewView, r overviewRects, it placed, s *spans, iw int) string {
	e := ov.Events[it.idx]
	sp := func() *spans { return s.add(" ", lipgloss.NewStyle()) }
	fromW := r.evFromW
	if !r.narrow {
		fromW = clamp(iw/4, 12, 24)
	}
	s.col(ovAge(ov.GeneratedAt-e.TS), ageColW, fg(colDim), true)
	used := 1 + ageColW
	if r.evCrewW > 0 {
		sp().col(crewTag(e.Crew), r.evCrewW, fg(colDim), false)
		used += 1 + r.evCrewW
	}
	sp().col(eventFrom(ov, e.Worker), fromW, codenameStyle(eventTmux(ov, e.Worker), fg(colText).Bold(true)), false)
	used += 1 + fromW
	return sp().col(e.Text, iw-used-1, fg(colText), false).String()
}

// eventFrom names a bus sender as the panels do: worker:<branch>[#session] is
// the worker's codename (else its branch) and role:<branch>:<role> is that
// name with the role after a colon. Other senders show as sent.
func eventFrom(ov vm.OverviewView, from string) string {
	if rest, ok := strings.CutPrefix(from, "worker:"); ok {
		branch, _, _ := strings.Cut(rest, "#")
		return branchName(ov, branch)
	}
	if rest, ok := strings.CutPrefix(from, "role:"); ok {
		if branch, role, ok := strings.Cut(rest, ":"); ok {
			return branchName(ov, branch) + ":" + role
		}
	}
	return from
}

// eventTmux is the tmux colour of the worker a bus sender names, nil for any
// other sender or a worker not in the roster.
func eventTmux(ov vm.OverviewView, from string) *string {
	var branch string
	if rest, ok := strings.CutPrefix(from, "worker:"); ok {
		branch, _, _ = strings.Cut(rest, "#")
	} else if rest, ok := strings.CutPrefix(from, "role:"); ok {
		var hasRole bool
		if branch, _, hasRole = strings.Cut(rest, ":"); !hasRole {
			return nil
		}
	} else {
		return nil
	}
	for _, w := range ov.Workers {
		if w.Branch == branch {
			return w.Tmux
		}
	}
	return nil
}

// branchName is the codename of the worker on branch, else the branch.
func branchName(ov vm.OverviewView, branch string) string {
	for _, w := range ov.Workers {
		if w.Branch == branch {
			return ovStr(w.Name, branch)
		}
	}
	return branch
}

// roleState is a role pane's state for display: a failed role, or one blocked
// by its engine exiting, shows as failed.
func roleState(r vm.AgentNode) string {
	if r.State == "blocked" && strings.Contains(ovStr(r.Detail, ""), "engine exited") {
		return "failed"
	}
	return r.State
}
