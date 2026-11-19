package ui

import (
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/noamsto/dispatcher/dash/internal/data"
	vm "github.com/noamsto/dispatcher/dash/internal/view"
)

// fleetPanel names the Fleet tab's focusable panels in tab order.
type fleetPanel int

const (
	fpAttention fleetPanel = iota
	fpCrews
	fpCapacity
	fpHolds
	fpHosts
	numFleetPanels
)

// fleetPanelMinH is a frame plus one row: a right-column panel below it is
// not drawn.
const (
	fleetPanelMinH = 3
	fleetCrewsPct  = 60
)

// Fleet entry kinds continue the Overview's, so place and placed serve both.
const (
	eCrew entryKind = eEvent + 1 + iota
	eSection
	eCapWeight
	eCapLoad
	eCapModel
	eCapBudget
	eHold
	eHost
	eSuggested
)

// fleetSections are the eSection titles, by entry idx.
var fleetSections = [...]string{"PEERS", "BUDGET", "suggested (not admitted)", "dead"}

const (
	secPeers = iota
	secBudget
	secSuggested
	secDead // titled by fleetRow.line: it carries the count and the key hint
)

// fleetState is what the operator asked for; resolveFleet turns it into what
// is drawn and acted on. Rows are keyed by crew key, hold ID and host name,
// so a selection survives refreshes and the filter.
type fleetState struct {
	focus    fleetPanel
	cursor   string // the strip cursor's attention item ID
	cursorAt int    // the cursor's index when set: tie-break and fallback
	stripOff int
	crew     string // CrewRow.Key of the chosen crew
	hold     string
	host     string
	capOff   int // Capacity has no selectable rows, so it scrolls
	filter   string
	showDead bool // the dead crews section is expanded
}

// fleetSel is the resolved selection: indexes into the FleetView's Crews,
// Holds, Hosts and Attention, or -1 for none.
type fleetSel struct{ crew, hold, host, cursor int }

type fleetRects struct {
	w, h      int
	errLine   bool // the last row is the source-errors line
	allClear  bool
	stripRows int
	stripOff  int
	sel       fleetSel
	panels    [numFleetPanels]panelLayout
}

// fleetIndex resolves crew keys and the filter once per frame.
type fleetIndex struct {
	byKey    map[string]int
	vis      []bool
	showDead bool
	deadN    int // dead groups with a row passing the filter, shown or not
	groupsN  int // groups (crew heads) with a row passing the filter
	tree     []subMark
}

// subMark is a sub row's place in its group's tree, among the rows drawn.
type subMark struct {
	last bool // the group's last visible sub row
	dupe bool // another sub row in the group shows the same repo
}

// markSubs fills ix.tree once ix.vis is final.
func (ix *fleetIndex) markSubs(fv vm.FleetView) {
	ix.tree = make([]subMark, len(fv.Crews))
	for start := 0; start < len(fv.Crews); {
		end := start + 1
		for end < len(fv.Crews) && fv.Crews[end].Sub {
			end++
		}
		repos := map[string]int{}
		lastVis := -1
		for i := start + 1; i < end; i++ {
			if ix.vis[i] {
				repos[strOf(fv.Crews[i].Repo)]++
				lastVis = i
			}
		}
		for i := start + 1; i < end; i++ {
			ix.tree[i] = subMark{last: i == lastVis, dupe: repos[strOf(fv.Crews[i].Repo)] > 1}
		}
		start = end
	}
}

// index is the one visibility rule for crew rows: the filter, the dead
// section's collapse, and the pin. A pinned dead crew (the chosen one, or the
// attention cursor's, whatever has focus) stays visible while collapsed so
// the cursor-to-crew link never points at a hidden row.
func (st fleetState) index(fv vm.FleetView) fleetIndex {
	needle := strings.ToLower(st.filter)
	pins := []string{st.crew}
	if len(fv.Attention) > 0 {
		pins = append(pins, strOf(fv.Attention[attnCursor(fv.Attention, st.cursor, st.cursorAt)].CrewKey))
	}
	ix := fleetIndex{byKey: make(map[string]int, len(fv.Crews)), vis: make([]bool, len(fv.Crews)), showDead: st.showDead}
	for start := 0; start < len(fv.Crews); {
		end := start + 1
		for end < len(fv.Crews) && fv.Crews[end].Sub {
			end++
		}
		matched := make([]bool, end-start)
		headHit, anyHit, pinned := false, false, false
		for i := start; i < end; i++ {
			c := fv.Crews[i]
			ix.byKey[c.Key] = i
			hay := strings.ToLower(strOf(c.Repo) + " " + c.Crew + " " + c.Host)
			matched[i-start] = needle == "" || strings.Contains(hay, needle)
			anyHit = anyHit || matched[i-start]
			pinned = pinned || slices.Contains(pins, c.Key)
		}
		headHit = matched[0]
		start0 := start
		start = end
		if !anyHit {
			continue
		}
		ix.groupsN++
		dead := fv.Crews[start0].Section == "dead"
		if dead {
			ix.deadN++
		}
		for i := start0; i < end; i++ {
			show := i == start0 || matched[i-start0] || headHit
			if dead && !st.showDead {
				show = show && (i == start0 && pinned || slices.Contains(pins, fv.Crews[i].Key))
			}
			ix.vis[i] = show
		}
	}
	ix.markSubs(fv)
	return ix
}

// crew is the visible crew row with this key, or -1.
func (ix fleetIndex) crew(key string) int {
	if i, ok := ix.byKey[key]; ok && ix.vis[i] {
		return i
	}
	return -1
}

func (ix fleetIndex) visible() []int {
	var out []int
	for i, v := range ix.vis {
		if v {
			out = append(out, i)
		}
	}
	return out
}

// effectiveFleetFocus is the panel holding focus as drawn: the strip hands it
// to the crews while it has no items.
func effectiveFleetFocus(fv vm.FleetView, f fleetPanel) fleetPanel {
	if f == fpAttention && len(fv.Attention) == 0 {
		return fpCrews
	}
	return f
}

// fleetShown is the body panels drawn at width w: all four when wide, else
// only the focused one (the crews while the strip has focus).
func fleetShown(w int, focus fleetPanel) []fleetPanel {
	switch {
	case w >= wideWidth:
		return []fleetPanel{fpCrews, fpCapacity, fpHolds, fpHosts}
	case focus == fpAttention:
		return []fleetPanel{fpCrews}
	}
	return []fleetPanel{focus}
}

// resolveFleet is the one answer to what is selected. With the strip focused
// the crew is the cursor item's crew, or none; otherwise the chosen crew if
// it passes the filter, else the first visible one when the crews are shown.
// Holds and hosts fall back to their first row the same way.
func resolveFleet(fv vm.FleetView, st fleetState, shown []fleetPanel) fleetSel {
	ix := st.index(fv)
	sel := fleetSel{crew: -1, hold: -1, host: -1, cursor: attnCursor(fv.Attention, st.cursor, st.cursorAt)}
	focus := effectiveFleetFocus(fv, st.focus)
	if focus == fpAttention {
		sel.crew = ix.crew(strOf(fv.Attention[sel.cursor].CrewKey))
	} else {
		sel.crew = ix.crew(st.crew)
		if vis := ix.visible(); sel.crew < 0 && len(vis) > 0 && slices.Contains(shown, fpCrews) {
			sel.crew = vis[0]
		}
	}
	sel.hold = slices.IndexFunc(fv.Holds, func(h vm.FleetHoldRow) bool { return h.ID == st.hold })
	if sel.hold < 0 && len(fv.Holds) > 0 && slices.Contains(shown, fpHolds) {
		sel.hold = 0
	}
	sel.host = slices.IndexFunc(fv.Hosts, func(h vm.FleetHostRow) bool { return h.Name == st.host })
	if sel.host < 0 && len(fv.Hosts) > 0 && slices.Contains(shown, fpHosts) {
		sel.host = 0
	}
	return sel
}

// crewEntries is one row per visible crew, a PEERS subheading before the
// first peer row and the dead subheading before the first visible dead row, or
// last when none is visible. An entry's worker is its crew index, what place keeps
// in view.
func crewEntries(fv vm.FleetView, ix fleetIndex) []entry {
	var es []entry
	peers, dead := false, false
	deadHead := entry{kind: eSection, idx: secDead, worker: -1, lines: 1}
	for _, i := range ix.visible() {
		switch {
		case fv.Crews[i].Origin == vm.OriginPeer && !peers:
			peers = true
			es = append(es, entry{kind: eSection, idx: secPeers, worker: -1, lines: 1})
		case fv.Crews[i].Section == "dead" && !dead:
			dead = true
			es = append(es, deadHead)
		}
		es = append(es, entry{kind: eCrew, idx: i, worker: i, lines: 1})
	}
	if !dead && ix.deadN > 0 {
		es = append(es, deadHead)
	}
	return es
}

// hasLegend: the Crews panel pins its counter legend to the last body row
// when there is room to leave at least three rows for crews.
func hasLegend(inner rect) bool { return inner.h >= 4 }

// fleetLegends are the counter legend from fullest to shortest; the Crews
// panel draws the first that fits its width.
var fleetLegends = []string{
	"w working · b blocked · f failed · pr pr_open · d done · x exited · m migrated · ? unknown · N! attention",
	"w working · b blocked · f failed · pr pr_open · N! attention",
	"w work · b block · f fail · pr pr · N! attn",
}

func fleetLegend(iw int) string {
	for _, l := range fleetLegends {
		if ansi.StringWidth(l)+1 <= iw {
			return l
		}
	}
	return fleetLegends[len(fleetLegends)-1]
}

func capacityEntries(fv vm.FleetView) []entry {
	es := []entry{
		{kind: eCapWeight, worker: -1, lines: 1},
		{kind: eCapLoad, worker: -1, lines: 1},
	}
	for i := range fv.Capacity.LocalModels {
		es = append(es, entry{kind: eCapModel, idx: i, worker: -1, lines: 1})
	}
	if len(fv.Capacity.Budgets) > 0 {
		es = append(es, entry{kind: eSection, idx: secBudget, worker: -1, lines: 1})
		for i := range fv.Capacity.Budgets {
			es = append(es, entry{kind: eCapBudget, idx: i, worker: -1, lines: 1})
		}
	}
	return es
}

func holdEntries(fv vm.FleetView) []entry {
	es := make([]entry, len(fv.Holds))
	for i := range fv.Holds {
		es[i] = entry{kind: eHold, idx: i, worker: i, lines: 1}
	}
	return es
}

func hostEntries(fv vm.FleetView) []entry {
	es := make([]entry, 0, len(fv.Hosts)+len(fv.SuggestedHosts)+1)
	for i := range fv.Hosts {
		es = append(es, entry{kind: eHost, idx: i, worker: i, lines: 1})
	}
	if len(fv.SuggestedHosts) > 0 {
		es = append(es, entry{kind: eSection, idx: secSuggested, worker: -1, lines: 1})
		for i := range fv.SuggestedHosts {
			es = append(es, entry{kind: eSuggested, idx: i, worker: -1, lines: 1})
		}
	}
	return es
}

func fleetEntries(p fleetPanel, fv vm.FleetView, ix fleetIndex) []entry {
	switch p {
	case fpCrews:
		return crewEntries(fv, ix)
	case fpCapacity:
		return capacityEntries(fv)
	case fpHolds:
		return holdEntries(fv)
	}
	return hostEntries(fv)
}

// splitHeights shares total rows among panels: each gets fleetPanelMinH
// while rows last, the focused panel (index first, -1 for none) before the
// rest so focus is never left undrawn, then one row at a time up to its wanted
// height, and the last takes what is left.
func splitHeights(total int, want []int, first int) []int {
	hs := make([]int, len(want))
	left := total
	if first >= 0 {
		hs[first] = min(fleetPanelMinH, left)
		left -= hs[first]
	}
	for i := range hs {
		if i == first {
			continue
		}
		hs[i] = min(fleetPanelMinH, left)
		left -= hs[i]
	}
	for grew := true; grew && left > 0; {
		grew = false
		for i := range hs {
			if left > 0 && hs[i] < want[i] {
				hs[i]++
				left--
				grew = true
			}
		}
	}
	if len(hs) > 0 {
		hs[len(hs)-1] += left
	}
	return hs
}

// fleetLayout is the one geometry the renderer and mouse hit-testing share,
// and the only place the selection is resolved and the strip window computed.
func fleetLayout(w, h int, fv vm.FleetView, st fleetState) fleetRects {
	ix := st.index(fv)
	r := fleetRects{w: w, h: h}
	if len(fv.Errors) > 0 && h >= 2 {
		r.errLine = true
		h--
	}
	n := len(fv.Attention)
	y := 0
	if n == 0 {
		r.allClear = !fv.Pending.Status
		y = min(1, h)
	} else {
		var stripH int
		stripH, r.stripRows, r.stripOff = stripWindow(n, h, st.stripOff, attnCursor(fv.Attention, st.cursor, st.cursorAt))
		outer, inner := panelRect(rect{0, 0, w, stripH})
		pl := panelLayout{shown: true, rect: outer, inner: inner}
		for i := range r.stripRows {
			idx := r.stripOff + i
			pl.items = append(pl.items, placed{
				entry: entry{kind: eAttn, idx: idx, worker: ix.crew(strOf(fv.Attention[idx].CrewKey)), lines: 1},
				rect:  rect{inner.x, inner.y + i, inner.w, 1},
			})
		}
		r.panels[fpAttention] = pl
		y = stripH
	}
	bodyH := max0(h - y)
	var shown []fleetPanel
	if bodyH >= fleetPanelMinH {
		shown = fleetShown(w, effectiveFleetFocus(fv, st.focus))
	}
	r.sel = resolveFleet(fv, st, shown)
	if len(shown) == 0 {
		return r
	}

	rects := map[fleetPanel]rect{}
	if w >= wideWidth {
		crewsW := w * fleetCrewsPct / 100
		rects[fpCrews] = rect{0, y, crewsW, bodyH}
		right := []fleetPanel{fpCapacity, fpHolds, fpHosts}
		want := make([]int, len(right))
		for i, p := range right {
			want[i] = max(1, totalLines(fleetEntries(p, fv, ix))) + 2
		}
		py := y
		focus := slices.Index(right, effectiveFleetFocus(fv, st.focus))
		for i, ph := range splitHeights(bodyH, want, focus) {
			if ph >= fleetPanelMinH {
				rects[right[i]] = rect{crewsW, py, w - crewsW, ph}
			}
			py += ph
		}
	} else {
		rects[shown[0]] = rect{0, y, w, bodyH}
	}

	for p, pr := range rects {
		outer, inner := panelRect(pr)
		es := fleetEntries(p, fv, ix)
		selIdx := -1
		switch p {
		case fpCrews:
			selIdx = r.sel.crew
		case fpHolds:
			selIdx = r.sel.hold
		case fpHosts:
			selIdx = r.sel.host
		case fpCapacity:
			es = es[clamp(st.capOff, 0, max0(len(es)-inner.h)):]
		}
		placeIn := inner
		if p == fpCrews && hasLegend(inner) {
			placeIn.h--
		}
		r.panels[p] = panelLayout{shown: true, rect: outer, inner: inner, items: place(es, placeIn, selIdx)}
	}
	return r
}

// sessionW is the session column's width whenever it is shown: the widest
// label, fixed so a pane-state change never shifts the counts column.
const sessionW = len("awaiting you")

// fleetCols are the Crews panel's column widths, sized to every visible row
// so scrolling never shifts them.
type fleetCols struct{ label, lane, session, counts int }

func crewCols(fv vm.FleetView, ix fleetIndex, iw int) fleetCols {
	var c fleetCols
	tail := 0
	for _, i := range ix.visible() {
		row := fv.Crews[i]
		c.label = max(c.label, ansi.StringWidth(crewLabel(row, ix.tree[i])))
		c.lane = max(c.lane, ansi.StringWidth(crewLane(row)))
		if crewSession(row) != "" {
			c.session = sessionW
		}
		c.counts = max(c.counts, ansi.StringWidth(workerCounts(row.Workers.ByState)))
		w := 0
		for _, m := range crewMarks(row) {
			w += ansi.StringWidth(m.text)
		}
		tail = max(tail, w)
	}
	// The label gives way so the columns after it and every row's trailing
	// marks stay on screen: margin, glyph and space lead, then the label.
	rest := 1 + c.lane + 1 + c.counts + tail
	if c.session > 0 {
		rest += c.session + 1
	}
	c.label = min(c.label, max(8, min(iw/2, iw-3-rest)))
	return c
}

func renderFleet(w, h int, fv vm.FleetView, st fleetState, mode pillMode) string {
	if w <= 0 || h <= 0 {
		return ""
	}
	r := fleetLayout(w, h, fv, st)
	focus := effectiveFleetFocus(fv, st.focus)
	ix := st.index(fv)
	lines := make([]string, 0, h)
	if r.allClear {
		lines = append(lines, truncateLine(" "+fg(colGreen).Render("✓ all clear"), w))
	} else if len(fv.Attention) == 0 {
		lines = append(lines, truncateLine(" "+fg(colDim).Render("loading…"), w))
	}

	crewsCount := fmt.Sprintf("%d crews · %d workers", fv.Summary.Crews, fv.Summary.Workers)
	if st.filter != "" {
		crewsCount = fmt.Sprintf("%d of %d crews", ix.groupsN, fv.Summary.Crews)
	}
	capCount := ""
	if fv.Capacity.Free != nil {
		capCount = fmt.Sprintf("%d free", *fv.Capacity.Free)
	}
	specs := [numFleetPanels]panelSpec{
		fpAttention: {title: "ATTENTION", count: fmt.Sprintf("%d need action", len(fv.Attention)), color: colAmber},
		fpCrews:     {title: "CREWS", count: crewsCount, color: colTeal},
		fpCapacity:  {title: "CAPACITY", count: capCount, color: colPurple},
		fpHolds:     {title: "HOLDS", count: fmt.Sprintf("%d holds", len(fv.Holds)), color: colBlue},
		fpHosts:     {title: "HOSTS", count: fmt.Sprintf("%d unhealthy of %d", fv.Summary.HostsUnhealthy, fv.Summary.Hosts), color: colGreen},
	}
	fr := fleetRow{fv: fv, r: r, focus: focus, tree: ix.tree, deadN: ix.deadN, showDead: ix.showDead}
	if pl := r.panels[fpCrews]; pl.shown {
		fr.cols = crewCols(fv, ix, pl.inner.w)
	}
	pd := fv.Pending
	pending := [numFleetPanels]bool{
		fpCrews: pd.Status, fpCapacity: pd.Status,
		fpHolds: pd.Queue, fpHosts: pd.Hosts || pd.Reach,
	}
	body := func(p fleetPanel) []string {
		pl := r.panels[p]
		sp := specs[p]
		sp.focused = focus == p
		var content []string
		for _, it := range pl.items {
			content = append(content, fr.line(it, pl.inner.w))
		}
		if len(pl.items) == 0 && p != fpAttention {
			empty := " none"
			if pending[p] {
				empty = " loading…"
			}
			content = append(content, newSpans(pl.inner.w, nil).add(empty, fg(colDim)).String())
		}
		if p == fpCrews && hasLegend(pl.inner) {
			for len(content) < pl.inner.h-1 {
				content = append(content, newSpans(pl.inner.w, nil).String())
			}
			content = append(content, newSpans(pl.inner.w, nil).add(" "+fleetLegend(pl.inner.w), fg(colDim)).String())
		}
		return renderPanel(pl.rect.w, pl.rect.h, sp, mode, content)
	}

	if r.panels[fpAttention].shown {
		lines = append(lines, body(fpAttention)...)
	}
	if left := r.panels[fpCrews]; left.shown && left.rect.w < w {
		var right []string
		for _, p := range []fleetPanel{fpCapacity, fpHolds, fpHosts} {
			pl := r.panels[p]
			if !pl.shown {
				continue
			}
			for len(right) < pl.rect.y-left.rect.y {
				right = append(right, strings.Repeat(" ", w-left.rect.w))
			}
			right = append(right, body(p)...)
		}
		blank := strings.Repeat(" ", w-left.rect.w)
		for i, l := range body(fpCrews) {
			rl := blank
			if i < len(right) {
				rl = right[i]
			}
			lines = append(lines, l+rl)
		}
	} else {
		for _, p := range []fleetPanel{fpCrews, fpCapacity, fpHolds, fpHosts} {
			if r.panels[p].shown {
				lines = append(lines, body(p)...)
			}
		}
	}
	bodyH := h
	if r.errLine {
		bodyH--
	}
	for len(lines) < bodyH {
		lines = append(lines, "")
	}
	lines = lines[:bodyH]
	if r.errLine {
		lines = append(lines, fg(colDim).Render(fmt.Sprintf(" ! %d source errors: %s", len(fv.Errors), fv.Errors[0])))
	}
	for i := range lines {
		lines[i] = truncateLine(lines[i], w)
	}
	return strings.Join(lines, "\n")
}

// fleetRow renders one placed entry of any Fleet panel.
type fleetRow struct {
	fv    vm.FleetView
	r     fleetRects
	focus fleetPanel
	cols  fleetCols
	tree  []subMark

	deadN    int
	showDead bool
}

// selected: the strip highlights its cursor while focused, else the chosen
// crew's items; the crew row always shows the chosen crew; holds and hosts
// highlight only in their focused panel.
func (fr fleetRow) selected(it placed) bool {
	sel := fr.r.sel
	switch it.kind {
	case eAttn:
		if fr.focus == fpAttention {
			return it.idx == sel.cursor
		}
		return it.worker >= 0 && it.worker == sel.crew
	case eCrew:
		return it.worker == sel.crew
	case eHold:
		return fr.focus == fpHolds && it.worker == sel.hold
	case eHost:
		return fr.focus == fpHosts && it.worker == sel.host
	}
	return false
}

func (fr fleetRow) line(it placed, iw int) string {
	if it.kind == eSection {
		title := fleetSections[it.idx]
		if it.idx == secDead {
			hint := "D expand"
			if fr.showDead {
				hint = "D collapse"
			}
			title = fmt.Sprintf("dead (%d) · %s", fr.deadN, hint)
		}
		return sectionLine(title, iw)
	}
	var bg lipgloss.TerminalColor = lipgloss.NoColor{}
	sel := fr.selected(it)
	if sel {
		bg = colSelBg
	}
	s := newSpans(iw, bg)
	if sel {
		s.add("▌", fg(colTeal))
	} else {
		s.add(" ", lipgloss.NewStyle())
	}
	fv := fr.fv
	switch it.kind {
	case eAttn:
		return attnStripLine(fv.Attention[fr.r.stripOff:fr.r.stripOff+fr.r.stripRows], it.idx-fr.r.stripOff, fleetAttnLabel, s)
	case eCrew:
		return fr.crewLine(it.idx, s)
	case eCapWeight:
		return weightLine(fv.Capacity, s, iw)
	case eCapLoad:
		return loadLine(fv.Capacity, s)
	case eCapModel:
		m := fv.Capacity.LocalModels[it.idx]
		c := colText
		if m.InUse >= m.MaxConcurrent {
			c = colAmber
		}
		return s.col(m.ID, gaugeLabelW, fg(colText), false).add(" ", lipgloss.NewStyle()).
			add(fmt.Sprintf("%d/%d", m.InUse, m.MaxConcurrent), fg(c)).String()
	case eCapBudget:
		return budgetBar(fv.Capacity.Budgets[it.idx], s, iw)
	case eHold:
		return fleetHoldLine(fv.Holds[it.idx], s, iw)
	case eHost:
		return fleetHostLine(fv.Hosts, it.idx, s, fv.Pending.Reach)
	}
	sh := fv.SuggestedHosts[it.idx]
	return s.col(sh.Name, suggestedNameW(fv.SuggestedHosts), fg(colDim), false).add(" ", lipgloss.NewStyle()).
		add(onlineWord(sh.Online), fg(onlineColor(sh.Online))).String()
}

// fleetAttnLabel names who an attention item is about: the worker, else the
// crew, else the host it concerns.
func fleetAttnLabel(a vm.AttentionItem) string {
	switch {
	case a.Worker != "":
		return a.Worker
	case a.Crew != "":
		return a.Crew
	case a.Host != "":
		return "@" + a.Host
	}
	return ""
}

// shortCrew is a crew id cut to its first 8 runes: dispatcher crew ids are
// long and have no codename.
func shortCrew(id string) string {
	r := []rune(id)
	if len(r) <= 8 {
		return id
	}
	return string(r[:8])
}

// cloneName is the base name of a sub row's clone directory, falling back to
// the common dir's; sub rows of a group share their crew id, so this is what
// tells apart rows on the same repo.
func cloneName(c vm.FleetCrewRow) string {
	for _, p := range []*string{c.Dir, c.Common} {
		if p != nil && strings.Trim(*p, "/") != "" {
			return path.Base(*p)
		}
	}
	return ""
}

// subParts splits a sub row's label into its tree connector, repo, and the
// clone-name suffix that tells apart rows sharing a repo.
func subParts(c vm.FleetCrewRow, m subMark) (conn, repo, suffix string) {
	conn = "  ├─ "
	if m.last {
		conn = "  ╰─ "
	}
	if m.dupe {
		if name := cloneName(c); name != "" {
			suffix = " " + name
		}
	}
	return conn, strOf(c.Repo), suffix
}

// crewLabel is the row's name: a peer's "@host crew", a head's "crew @host
// repo", a sub row's tree connector and repo.
func crewLabel(c vm.FleetCrewRow, m subMark) string {
	switch {
	case c.Origin == vm.OriginPeer:
		return "@" + c.Host + " " + c.Crew
	case c.Sub:
		conn, repo, suffix := subParts(c, m)
		return conn + repo + suffix
	}
	return shortCrew(c.Crew) + " @" + c.Host + " " + strOf(c.Repo)
}

// labelCol writes the label column: a sub row's connector and suffix dim, its
// repo in text, all clipped to the column width.
func labelCol(s *spans, c vm.FleetCrewRow, m subMark, width int, text lipgloss.Style) {
	if !c.Sub || c.Origin == vm.OriginPeer {
		s.col(crewLabel(c, m), width, text, false)
		return
	}
	conn, repo, suffix := subParts(c, m)
	rest := width
	for _, p := range []struct {
		t  string
		st lipgloss.Style
	}{{conn, fg(colDim)}, {repo, text}, {suffix, fg(colDim)}} {
		t := ansi.Truncate(cleanText(p.t), max0(rest), "…")
		rest -= ansi.StringWidth(t)
		s.add(t, p.st)
	}
	s.add(strings.Repeat(" ", max0(rest)), lipgloss.NewStyle())
}

func crewLane(c vm.FleetCrewRow) string {
	if c.Dispatcher == nil {
		return ""
	}
	return ovStr(c.Dispatcher.Lane, "—")
}

// crewSession labels what a head row's dispatcher session is doing when that
// is worth a word: working and unknown stay blank, the glyph says working.
func crewSession(c vm.FleetCrewRow) string {
	if c.Dispatcher == nil || c.Sub {
		return ""
	}
	switch c.Dispatcher.Session {
	case vm.SessionNeedsYou:
		return "needs you"
	case vm.SessionStandby:
		return "standby"
	case vm.SessionIdle:
		return "awaiting you"
	}
	return ""
}

func sessionStyle(c vm.FleetCrewRow) lipgloss.Style {
	switch c.Dispatcher.Session {
	case vm.SessionNeedsYou:
		return fg(colAmber).Bold(true)
	case vm.SessionStandby:
		return fg(colGreen).Faint(true)
	}
	return fg(colDim)
}

// dispatcherGlyphState maps a dispatcher state onto the worker state whose
// glyph and colour read the same: live runs (blocked when it waits on the
// operator), dead failed, closed idle.
func dispatcherGlyphState(c vm.FleetCrewRow) string {
	if c.Dispatcher == nil {
		return ""
	}
	switch c.Dispatcher.State {
	case "live":
		if c.Dispatcher.Session == vm.SessionNeedsYou {
			return "blocked"
		}
		return "working"
	case "dead":
		return "failed"
	}
	return ""
}

var countAbbrevs = []struct{ state, abbr string }{
	{"working", "w"}, {"blocked", "b"}, {"pr_open", "pr"}, {"done", "d"},
	{"failed", "f"}, {"exited", "x"}, {"migrated", "m"}, {"unknown", "?"},
}

// workerCounts is by_state as "3w 1b 1pr": known states abbreviated in a
// fixed order, any other state after them by name.
func workerCounts(by map[string]int) string {
	var parts []string
	seen := map[string]bool{}
	for _, a := range countAbbrevs {
		seen[a.state] = true
		if n := by[a.state]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d%s", n, a.abbr))
		}
	}
	var rest []string
	for st, n := range by {
		if !seen[st] && n > 0 {
			rest = append(rest, st)
		}
	}
	slices.Sort(rest)
	for _, st := range rest {
		parts = append(parts, fmt.Sprintf("%d %s", by[st], st))
	}
	if len(parts) == 0 {
		return "—"
	}
	return strings.Join(parts, " ")
}

func laneColor(lane string) lipgloss.Color {
	switch lane {
	case "alive":
		return colGreen
	case "stale":
		return colAmber
	}
	return colDim
}

func (fr fleetRow) crewLine(idx int, s *spans) string {
	c := fr.fv.Crews[idx]
	gs := dispatcherGlyphState(c)
	text, counts := fg(colText).Bold(true), fg(colText)
	if c.Origin == vm.OriginPeer {
		text, counts = fg(colDim), fg(colDim)
	}
	lane := crewLane(c)
	s.add(stateGlyph(gs), fg(glyphColor(gs))).
		add(" ", lipgloss.NewStyle())
	labelCol(s, c, fr.tree[idx], fr.cols.label, text)
	s.add(" ", lipgloss.NewStyle()).
		col(lane, fr.cols.lane, fg(laneColor(lane)), false).
		add(" ", lipgloss.NewStyle())
	if fr.cols.session > 0 {
		label := crewSession(c)
		st := lipgloss.NewStyle()
		if label != "" {
			st = sessionStyle(c)
		}
		s.col(label, fr.cols.session, st, false).add(" ", lipgloss.NewStyle())
	}
	s.col(workerCounts(c.Workers.ByState), fr.cols.counts, counts, false)
	for _, m := range crewMarks(c) {
		s.add(m.text, m.st)
	}
	return s.String()
}

type crewMark struct {
	text string
	st   lipgloss.Style
}

// crewMarks are the spans after the counts: attention, orphaned, idle repos.
func crewMarks(c vm.FleetCrewRow) []crewMark {
	var ms []crewMark
	if c.Attention > 0 {
		ms = append(ms, crewMark{fmt.Sprintf(" %d!", c.Attention), fg(colAmber).Bold(true)})
	}
	if c.Orphaned {
		ms = append(ms, crewMark{" orphaned", fg(colRed).Bold(true)})
	}
	if c.IdleRepos > 0 {
		ms = append(ms, crewMark{fmt.Sprintf(" +%d idle repos", c.IdleRepos), fg(colDim)})
	}
	return ms
}

func weightLine(c vm.FleetCapacity, s *spans, iw int) string {
	s.col("weight", gaugeLabelW, fg(colText), false).add(" ", lipgloss.NewStyle())
	if c.Budget == nil || c.Weight == nil {
		return s.add("?", fg(colDim)).String()
	}
	text := fmt.Sprintf(" %d/%d", *c.Weight, *c.Budget)
	if c.Pending != nil && *c.Pending > 0 {
		text += fmt.Sprintf(" (+%d)", *c.Pending)
	}
	barW := clamp(iw-1-gaugeLabelW-1-ansi.StringWidth(text), 4, 20)
	used, pct := 0.0, 0.0
	if *c.Budget > 0 {
		used = float64(*c.Weight) / float64(*c.Budget)
		pct = float64(*c.Weight+derefInt(c.Pending)) / float64(*c.Budget) * 100
	}
	filled := clamp(int(used*float64(barW)+0.5), 0, barW)
	col := lipgloss.Color(gaugeColor(pct))
	return s.add(strings.Repeat("█", filled), fg(col)).
		add(strings.Repeat("░", barW-filled), fg(colBorderOff)).
		add(text, fg(col)).String()
}

func derefInt(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

func loadLine(c vm.FleetCapacity, s *spans) string {
	s.col("load", gaugeLabelW, fg(colText), false).add(" ", lipgloss.NewStyle())
	if c.Load1 == nil || c.Cores == nil {
		return s.add("?", fg(colDim)).String()
	}
	over := c.LoadOver != nil && *c.LoadOver
	col := colText
	if over {
		col = colRed
	}
	s.add(fmt.Sprintf("%s/%d", optFmt1(c.Load1), *c.Cores), fg(col))
	if over {
		s.add(" load over", fg(colRed).Bold(true))
	}
	return s.String()
}

func prioColor(p string) lipgloss.Color {
	switch p {
	case "high":
		return colRed
	case "low", "quota":
		return colDim
	}
	return colText
}

func fleetHoldLine(hd vm.FleetHoldRow, s *spans, iw int) string {
	const prioW, stateW, ageW = 6, 15, 4
	prio := ovStr(hd.Prio, hd.Kind)
	where := hd.Repo + " " + hd.Crew
	if hd.Branch != nil {
		where = hd.Repo + "/" + *hd.Branch
	}
	state := hd.State
	if hd.Reason != nil {
		state += ":" + *hd.Reason
	}
	sc := colText
	switch {
	case hd.Matured:
		sc = colAmber
	case hd.State == "parked":
		sc = colDim
	}
	whereW := max(4, iw-1-prioW-1-1-stateW-1-ageW)
	return s.col(prio, prioW, fg(prioColor(prio)), false).
		add(" ", lipgloss.NewStyle()).
		col(where, whereW, fg(colText), false).
		add(" ", lipgloss.NewStyle()).
		col(state, stateW, fg(sc), false).
		add(" ", lipgloss.NewStyle()).
		col(ovAge(hd.WaitedS), ageW, fg(colDim), true).String()
}

// hostNameW is the admitted hosts' name column. Suggested hosts sit under
// their own subheading and size theirs apart, so a long tailnet name never
// pushes the admitted rows' sync column out of a narrow panel.
func hostNameW(hosts []vm.FleetHostRow) int {
	w := 4
	for _, h := range hosts {
		w = max(w, ansi.StringWidth(h.Name))
	}
	return min(w, 12)
}

func suggestedNameW(hosts []data.SuggestedHost) int {
	w := 4
	for _, h := range hosts {
		w = max(w, ansi.StringWidth(h.Name))
	}
	return min(w, 16)
}

func onlineWord(on bool) string {
	if on {
		return "online"
	}
	return "offline"
}

func onlineColor(on bool) lipgloss.Color {
	if on {
		return colGreen
	}
	return colDim
}

func relayColor(state string) lipgloss.Color {
	switch state {
	case "up":
		return colGreen
	case "down":
		return colRed
	}
	return colDim
}

func syncText(sy vm.FleetHostSync) (string, lipgloss.Color) {
	lag := ""
	if sy.LagSec != nil {
		lag = " " + ovAge(*sy.LagSec)
	}
	switch sy.State {
	case "n/a":
		return "—", colDim
	case "ok":
		return "ok" + lag, colGreen
	case "failed":
		return "failed" + lag, colRed
	}
	return sy.State + lag, colAmber
}

// reachMark is the reach probe as a glyph (online, offline, unreachable or
// unprobed) and the tag of the source that answered.
func reachMark(re vm.FleetHostReach) (string, string, lipgloss.Color) {
	tag := ""
	switch strOf(re.Source) {
	case "tailscale":
		tag = "ts"
	case "ssh":
		tag = "ssh"
	}
	switch {
	case re.Error != nil:
		return "✗", tag, colRed
	case re.Online == nil:
		return "?", tag, colDim
	case *re.Online:
		return "●", tag, colGreen
	}
	return "○", tag, colDim
}

func fleetHostLine(hosts []vm.FleetHostRow, i int, s *spans, probing bool) string {
	const tagW, relayW = 3, 4
	h := hosts[i]
	sync, sc := syncText(h.Sync)
	glyph, tag, rc := reachMark(h.Reach)
	if probing {
		glyph, tag, rc = "…", "", colDim
	}
	return s.col(h.Name, hostNameW(hosts), fg(colText).Bold(true), false).
		add(" ", lipgloss.NewStyle()).
		add(glyph, fg(rc)).
		add(" ", lipgloss.NewStyle()).
		col(tag, tagW, fg(colDim), false).
		add(" relay ", fg(colDim)).
		col(h.Relay.State, relayW, fg(relayColor(h.Relay.State)), false).
		add(" sync ", fg(colDim)).
		add(sync, fg(sc)).String()
}
