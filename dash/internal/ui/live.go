package ui

import (
	"fmt"
	"math"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/noamsto/dispatcher/dash/internal/data"
)

// liveDeps bundles the live view's live-refresh seams. eventsPath and
// watcher are resolved/started once by ui.Run (not here — see watcher.go
// and ui.go): eventsPath "" disables the enter-to-drill-in Cmd, watcher nil
// disables the bus-change reload (liveOffNote explains why).
type liveDeps struct {
	eventsPath  string
	watcher     busWatcher
	liveOffNote string
}

const debounceDelay = 300 * time.Millisecond

// rosterMsg tags the message types the live view's background
// watcher/ticker loop produces, so the root model can route them straight
// to the live view (see model.go) even while another tab is active —
// the watcher and the age ticker must keep running out of sight.
type rosterMsg interface{ isRosterMsg() }

type tickMsg struct{}
type busChangedMsg struct{}
type debounceFireMsg struct{ at time.Time }
type recentEventsMsg struct {
	events []data.Event
	err    string
}

func (tickMsg) isRosterMsg()         {}
func (busChangedMsg) isRosterMsg()   {}
func (debounceFireMsg) isRosterMsg() {}
func (recentEventsMsg) isRosterMsg() {}

func tickCmd() tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return tickMsg{} })
}

// waitForBusChange is the standard waitForActivity pattern: block on the
// watcher's channel, re-arming happens by the caller issuing this Cmd again
// in response to the busChangedMsg it produces. A closed channel (watcher
// shutting down) ends the loop with no message, never a panic.
func waitForBusChange(w busWatcher) tea.Cmd {
	return func() tea.Msg {
		if _, ok := <-w.Events(); !ok {
			return nil
		}
		return busChangedMsg{}
	}
}

func debounceCmd(at time.Time) tea.Cmd {
	return tea.Tick(debounceDelay, func(time.Time) tea.Msg { return debounceFireMsg{at: at} })
}

func requestRosterCmd() tea.Cmd {
	return func() tea.Msg { return requestRosterMsg{} }
}

// liveRow pairs a worker map with its owning crew id for the flat,
// cursor-navigable list (holds are informational only — not selectable).
type liveRow struct {
	crewID string
	w      map[string]any
}

// rowKey identifies a worker across reloads and re-sorts.
func rowKey(crewID string, w map[string]any) string {
	branch, _ := strField(w, "branch")
	session, _ := strField(w, "session")
	return crewID + "\x00" + branch + "\x00" + session
}

// liveMode is what owns the keyboard: the table, the reply prompt or the
// nudge confirm.
type liveMode int

const (
	modeNormal liveMode = iota
	modeReply
	modeConfirmNudge
)

type liveView struct {
	deps liveDeps
	acts actionDeps
	now  func() time.Time

	mode   liveMode
	input  textinput.Model // the reply prompt
	target data.Worker     // the worker the open prompt or confirm acts on

	snap   data.Snapshot
	loaded [numSources]bool

	rows         []liveRow
	showReaped   bool
	hiddenReaped int
	cursor       int
	selKey       string // rowKey of the cursor row; the cursor follows it across reloads

	lastChange time.Time

	detail        bool
	detailLoading bool
	detailEvents  []data.Event
	detailErr     string
	detailCursor  int // index into detailEvents — RecentEvents is oldest-first, so this starts at the last (newest) event
}

func newLiveView(snap data.Snapshot, loaded [numSources]bool, deps liveDeps, now func() time.Time) liveView {
	v := liveView{deps: deps, now: now}
	v.load(snap, loaded)
	return v
}

func (v *liveView) load(snap data.Snapshot, loaded [numSources]bool) {
	v.snap = snap
	v.loaded = loaded
	v.setRoster(snap.Roster)
}

// stateRank orders worker states by how much they need an operator.
func stateRank(state string) int {
	switch state {
	case "blocked":
		return 0
	case "failed":
		return 1
	case "working":
		return 2
	case "pr_open":
		return 3
	case "idle":
		return 4
	case "done":
		return 5
	case "reaped":
		return 7
	}
	return 6
}

// setRoster rebuilds the sorted worker list (state rank, then age
// descending), leaving reaped workers out unless showReaped, and puts the cursor back on the worker it was on, falling back
// to the same index, clamped, when that worker vanished.
func (v *liveView) setRoster(r data.RosterSection) {
	var rows []liveRow
	hidden := 0
	for _, c := range r.Crews {
		if c.WorkersError != nil {
			continue
		}
		for _, w := range c.Workers {
			if state, _ := strField(w, "state"); state == "reaped" && !v.showReaped {
				hidden++
				continue
			}
			rows = append(rows, liveRow{crewID: c.ID, w: w})
		}
	}
	v.hiddenReaped = hidden
	now := v.now()
	sort.SliceStable(rows, func(i, j int) bool {
		si, _ := strField(rows[i].w, "state")
		sj, _ := strField(rows[j].w, "state")
		if ri, rj := stateRank(si), stateRank(sj); ri != rj {
			return ri < rj
		}
		return workerAge(rows[i].w, now) > workerAge(rows[j].w, now)
	})
	v.rows = rows

	cursor := clamp(v.cursor, 0, len(rows)-1)
	for i, row := range rows {
		if rowKey(row.crewID, row.w) == v.selKey {
			cursor = i
			break
		}
	}
	v.moveTo(cursor)
}

// moveTo clamps i into the row list and records the worker under it.
func (v *liveView) moveTo(i int) {
	v.cursor = clamp(i, 0, len(v.rows)-1)
	v.selKey = ""
	if v.cursor < len(v.rows) {
		row := v.rows[v.cursor]
		v.selKey = rowKey(row.crewID, row.w)
	}
}

// workerAge is always recomputed from the worker's ts against the passed-in
// now — never read from a stale age_s snapshot field — so the 1s tick can
// animate it without a collect. age_s is only the fallback for a worker
// with no ts.
func workerAge(w map[string]any, now time.Time) int64 {
	var tsMs float64
	switch v := w["ts"].(type) {
	case int64:
		tsMs = float64(v)
	case float64:
		tsMs = v
	default:
		if s, ok := w["age_s"].(float64); ok {
			return max(0, int64(s))
		}
	}
	return max(0, now.Unix()-int64(math.Floor(tsMs/1000)))
}

// Init starts the age ticker and, when a watcher was started (ui.Run
// resolved the bus log path successfully), the bus-change wait loop. Called
// by the root model's Init (model.go), which type-asserts views[0].
func (v liveView) Init() tea.Cmd {
	cmds := []tea.Cmd{tickCmd()}
	if v.deps.watcher != nil {
		cmds = append(cmds, waitForBusChange(v.deps.watcher))
	}
	return tea.Batch(cmds...)
}

func (v liveView) Update(msg tea.Msg) (view, tea.Cmd) {
	switch msg := msg.(type) {
	case snapshotMsg:
		v.load(msg.snap, msg.loaded)
		return v, nil

	case tickMsg:
		return v, tickCmd()

	case busChangedMsg:
		v.lastChange = v.now()
		var cmds []tea.Cmd
		if v.deps.watcher != nil {
			cmds = append(cmds, waitForBusChange(v.deps.watcher))
		}
		cmds = append(cmds, debounceCmd(v.lastChange))
		return v, tea.Batch(cmds...)

	case debounceFireMsg:
		if !msg.at.Equal(v.lastChange) {
			return v, nil // superseded by a newer change
		}
		return v, requestRosterCmd()

	case recentEventsMsg:
		v.detailLoading = false
		v.detailEvents = msg.events
		v.detailErr = msg.err
		// RecentEvents is oldest-first; start scrolled to the newest event
		// rather than the oldest — a user drilling in wants to land on what
		// just happened, not what happened first.
		v.detailCursor = max0(len(msg.events) - 1)
		return v, nil

	case tea.KeyMsg:
		switch {
		case v.detail:
			return v.handleDetailKey(msg)
		case v.mode == modeReply:
			return v.handleReplyKey(msg)
		case v.mode == modeConfirmNudge:
			return v.handleConfirmKey(msg)
		}
		return v.handleKey(msg)
	}
	return v, nil
}

func (v liveView) handleKey(msg tea.KeyMsg) (view, tea.Cmd) {
	switch msg.String() {
	case "j", "down":
		v.moveTo(v.cursor + 1)
	case "k", "up":
		v.moveTo(v.cursor - 1)
	case "g":
		v.moveTo(0)
	case "G":
		v.moveTo(len(v.rows) - 1)
	case "t":
		return v.startAction(actJump)
	case "o":
		return v.startAction(actOpenPR)
	case "m":
		return v.startAction(actReply)
	case "n":
		return v.startAction(actNudge)
	case "a":
		v.showReaped = !v.showReaped
		v.setRoster(v.snap.Roster)
	case "enter":
		if v.cursor >= len(v.rows) || v.deps.eventsPath == "" {
			return v, nil
		}
		v.detail = true
		v.detailLoading = true
		v.detailEvents = nil
		v.detailErr = ""
		branch, _ := strField(v.rows[v.cursor].w, "branch")
		path := v.deps.eventsPath
		return v, func() tea.Msg {
			events, err := data.RecentEvents(path, branch, 20)
			m := recentEventsMsg{events: events}
			if err != nil {
				m.err = err.Error()
			}
			return m
		}
	}
	return v, nil
}

func (v liveView) handleDetailKey(msg tea.KeyMsg) (view, tea.Cmd) {
	switch msg.String() {
	case "esc":
		v.detail = false
	case "j", "down":
		v.detailCursor = clamp(v.detailCursor+1, 0, len(v.detailEvents)-1)
	case "k", "up":
		v.detailCursor = clamp(v.detailCursor-1, 0, len(v.detailEvents)-1)
	case "g":
		v.detailCursor = 0
	case "G":
		v.detailCursor = max0(len(v.detailEvents) - 1)
	}
	return v, nil
}

func (v liveView) Keys() []key.Binding {
	reapedHelp := "show reaped"
	if v.showReaped {
		reapedHelp = "hide reaped"
	}
	if v.detail {
		return []key.Binding{
			key.NewBinding(key.WithKeys("j", "k"), key.WithHelp("j/k", "scroll")),
			key.NewBinding(key.WithKeys("g", "G"), key.WithHelp("g/G", "top/bottom")),
			key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "back")),
		}
	}
	switch v.mode {
	case modeReply:
		return []key.Binding{
			key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "send")),
			key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "cancel")),
		}
	case modeConfirmNudge:
		return []key.Binding{
			key.NewBinding(key.WithKeys("y"), key.WithHelp("y", "confirm")),
			key.NewBinding(key.WithKeys("n", "esc"), key.WithHelp("n/esc", "cancel")),
		}
	}
	return []key.Binding{
		key.NewBinding(key.WithKeys("j", "k"), key.WithHelp("j/k", "move")),
		key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "events")),
		key.NewBinding(key.WithKeys("t"), key.WithHelp("t", "jump")),
		key.NewBinding(key.WithKeys("o"), key.WithHelp("o", "open PR")),
		key.NewBinding(key.WithKeys("m"), key.WithHelp("m", "reply")),
		key.NewBinding(key.WithKeys("n"), key.WithHelp("n", "nudge")),
		key.NewBinding(key.WithKeys("a"), key.WithHelp("a", reapedHelp)),
	}
}

func (v liveView) Selection() data.Selection {
	if v.cursor >= len(v.rows) {
		return nil
	}
	row := v.rows[v.cursor]
	f := func(k string) string { s, _ := strField(row.w, k); return s }
	return data.Worker{
		Crew:    row.crewID,
		Branch:  f("branch"),
		Session: f("session"),
		Name:    f("name"),
		State:   f("state"),
		PRURL:   f("pr_url"),
		Host:    f("host"),
	}
}

// Capturing is true while the reply prompt or nudge confirm owns the keys.
func (v liveView) Capturing() bool { return v.mode != modeNormal }

func (v liveView) View(w, h int) string {
	if v.detail {
		return strings.Join(normalizeFrame(v.detailView(w, h), h, w), "\n")
	}
	return strings.Join(normalizeFrame(v.tableView(w, h), h, w), "\n")
}

// selectedPaneH is the rule plus four selected-worker lines: where it
// lives, its PR, and two lines of detail.
const selectedPaneH = 5

// minBodyH is the fewest table lines (rows and holds) worth giving up the
// selected-worker pane for.
const minBodyH = 2

// tableView composes the Live frame top to bottom: budget strip, attention
// line, crew error lines, the worker table (header fixed, rows and hold
// lines windowed around the cursor) and the selected-worker pane pinned to
// the bottom. Every line is built to at most w cells; the chrome is never
// scrolled.
func (v liveView) tableView(w, h int) string {
	var top []string
	if v.deps.watcher == nil && v.deps.liveOffNote != "" {
		top = append(top, truncateLine(warnStyle.Render(v.deps.liveOffNote), w))
	}
	top = append(top, v.budgetLines(w)...)

	roster := v.snap.Roster
	switch {
	case !v.loaded[srcRoster]:
		top = append(top, loadingLine(srcRoster, w))
		return strings.Join(top, "\n")
	case roster.Error != nil:
		top = append(top, truncateLine("roster unavailable: "+cleanText(*roster.Error), w))
		return strings.Join(top, "\n")
	case len(roster.Crews) == 0:
		top = append(top, truncateLine("no active crew", w))
		return strings.Join(top, "\n")
	}

	top = append(top, v.attentionLines(w)...)

	var body []string
	cursorLine := 0
	if len(v.rows) == 0 {
		body = append(body, truncateLine("no workers", w))
	} else {
		var header string
		header, body, cursorLine = v.workerLines(w)
		top = append(top, header)
	}
	for _, c := range roster.Crews {
		for _, hd := range c.Holds {
			body = append(body, truncateLine("  "+holdLine(hd), w))
		}
	}

	// An open prompt or confirm renders on the pane, so it outranks the body.
	needBody := minBodyH
	if v.mode != modeNormal {
		needBody = 0
	}
	paneH := 0
	if len(v.rows) > 0 && h-len(top)-selectedPaneH >= needBody {
		paneH = selectedPaneH
	}
	bodyH := max0(h - len(top) - paneH)
	offset := windowOffset(cursorLine, len(body), bodyH)
	end := min(offset+bodyH, len(body))

	out := append(top, body[offset:end]...)
	if paneH > 0 {
		for len(out) < h-paneH {
			out = append(out, "")
		}
		out = append(out, v.selectedPane(w)...)
	}
	return strings.Join(out, "\n")
}

// attentionLines is the "N blocked · M holds · K reaped hidden" summary plus one line per
// crew-level failure, so a failing crew is never silently dropped.
func (v liveView) attentionLines(w int) []string {
	blocked := 0
	for _, r := range v.rows {
		if s, _ := strField(r.w, "state"); s == "blocked" {
			blocked++
		}
	}
	holds := 0
	for _, c := range v.snap.Roster.Crews {
		holds += len(c.Holds)
	}
	var parts []string
	if blocked > 0 {
		parts = append(parts, fmt.Sprintf("%d blocked", blocked))
	}
	if holds > 0 {
		noun := "holds"
		if holds == 1 {
			noun = "hold"
		}
		parts = append(parts, fmt.Sprintf("%d %s", holds, noun))
	}
	if v.hiddenReaped > 0 {
		parts = append(parts, fmt.Sprintf("%d reaped hidden", v.hiddenReaped))
	}
	var out []string
	if len(parts) > 0 {
		out = append(out, attentionStyle.Render(truncateLine(strings.Join(parts, " · "), w)))
	}
	for _, c := range v.snap.Roster.Crews {
		id := cleanText(c.ID)
		if c.WorkersError != nil {
			out = append(out, truncateLine(warnStyle.Render("crew "+id+": workers unavailable: "+cleanText(*c.WorkersError)), w))
		}
		if c.HoldsError != nil {
			out = append(out, truncateLine(warnStyle.Render("crew "+id+": holds unavailable: "+cleanText(*c.HoldsError)), w))
		}
	}
	return out
}

// selectedPane is the rule plus four lines about the selected worker: where
// it lives (or the open reply prompt / nudge confirm), its full PR URL, and
// its detail text wrapped to the remaining two lines — the place truncated
// table cells are readable in full.
func (v liveView) selectedPane(w int) []string {
	out := []string{ruleStyle.Render(strings.Repeat("─", w))}
	sel, _ := v.Selection().(data.Worker)
	row := v.rows[v.cursor]

	branch, session := cleanText(sel.Branch), cleanText(sel.Session)
	if branch == "" {
		branch = "—"
	}
	where := branch + " · " + session
	if sel.Host != "" {
		where += " · host " + cleanText(sel.Host)
	}
	pr := "—"
	if sel.PRURL != "" {
		pr = cleanText(sel.PRURL)
	}
	out = append(out, truncateLine(v.promptOr(where, w), w), truncateLine("PR "+pr, w))

	detail, _ := strField(row.w, "detail")
	room := selectedPaneH - len(out)
	var wrapped []string
	if d := cleanText(detail); d != "" && w > 0 {
		wrapped = strings.Split(ansi.Wrap(d, w, ""), "\n")
	}
	if len(wrapped) > room {
		wrapped = wrapped[:room]
		wrapped[room-1] = ansi.Truncate(wrapped[room-1], max0(w-1), "") + "…"
	}
	out = append(out, wrapped...)
	for len(out) < selectedPaneH {
		out = append(out, "")
	}
	return out
}

// promptOr is the open reply prompt or nudge confirm, else fallback.
func (v liveView) promptOr(fallback string, w int) string {
	switch v.mode {
	case modeReply:
		in := v.input
		in.Width = max0(w - ansi.StringWidth(in.Prompt) - 1)
		return in.View()
	case modeConfirmNudge:
		return "nudge " + cleanText(nudgeTarget(v.target)) + "? y/N"
	}
	return fallback
}

// budgetLines is the strip of one segment per engine (its highest-used
// window), at most two lines.
func (v liveView) budgetLines(w int) []string {
	b := v.snap.Budget
	switch {
	case !v.loaded[srcBudget]:
		return []string{loadingLine(srcBudget, w)}
	case b.Error != nil:
		return []string{truncateLine("budget unavailable: "+cleanText(*b.Error), w)}
	case b.Report == nil:
		return []string{truncateLine("budget unavailable", w)}
	}
	names := make([]string, 0, len(b.Report.Engines))
	for name := range b.Report.Engines {
		names = append(names, name)
	}
	sort.Strings(names)
	segs := make([]budgetSeg, 0, len(names))
	for _, name := range names {
		segs = append(segs, engineSegment(name, b.Report.Engines[name]))
	}
	return flowSegments(segs, w, 2)
}

// budgetSeg is one engine's strip segment; color is "" for the unstyled
// (no data) case.
type budgetSeg struct{ text, color string }

func engineSegment(name string, e *data.EngineBudget) budgetSeg {
	name = cleanText(name)
	if e == nil || len(e.Windows) == 0 {
		return budgetSeg{text: name + " ?"}
	}
	top := e.Windows[0]
	for _, win := range e.Windows[1:] {
		if win.UsedPct > top.UsedPct {
			top = win
		}
	}
	text := fmt.Sprintf("%s %s %.0f%%", name, cleanText(top.Key), top.UsedPct)
	pct := top.UsedPct
	if _, limited := e.LimitReachedReason(); limited {
		text += " LIMIT"
		pct = max(pct, 100)
	}
	return budgetSeg{text: text, color: gaugeColor(pct)}
}

// flowSegments lays segs onto at most maxLines lines of width w, joined by
// colGap spaces; what does not fit on the last line is ellipsized. The
// colour is applied to each already-fitted segment text.
func flowSegments(segs []budgetSeg, w, maxLines int) []string {
	render := func(s budgetSeg, text string) string {
		if s.color == "" {
			return text
		}
		return lipgloss.NewStyle().Foreground(lipgloss.Color(s.color)).Render(text)
	}
	var lines []string
	var cur strings.Builder
	curW := 0
	for _, s := range segs {
		gap := 0
		if curW > 0 {
			gap = colGap
		}
		segW := ansi.StringWidth(s.text)
		if curW+gap+segW > w {
			if curW > 0 && len(lines)+1 < maxLines {
				lines = append(lines, cur.String())
				cur.Reset()
				curW, gap = 0, 0
			} else if curW > 0 {
				if room := w - curW - gap; room > 0 {
					cur.WriteString(strings.Repeat(" ", gap) + render(s, truncateLine(s.text+"…", room)))
				}
				break
			}
		}
		text := truncateLine(s.text, w)
		cur.WriteString(strings.Repeat(" ", gap) + render(s, text))
		curW += gap + ansi.StringWidth(text)
	}
	if curW > 0 {
		lines = append(lines, cur.String())
	}
	return lines
}

// Worker table columns. Widths are computed per frame from the content
// within each column's [min, max]; one flex column absorbs the remainder.
type colID int

const (
	colCrew colID = iota
	colName
	colState
	colAge
	colTier
	colPR
	colDetail
	numCols
)

type col struct {
	id       colID
	title    string
	min, max int
	flex     bool
}

const (
	livePrefixW = 2 // cursor marker and its space
	colGap      = 2
	unbounded   = 1 << 20
	detailMinW  = 120 // narrowest frame that gets the detail column
)

// liveColumns lists the columns shown at frame width w: crew only when more
// than one crew is alive; tier/engine/model flexes below detailMinW, where
// the detail column does not exist; at or above it detail flexes instead.
func liveColumns(w int, multiCrew bool) []col {
	var cols []col
	if multiCrew {
		cols = append(cols, col{id: colCrew, title: "crew", min: 4, max: 12})
	}
	tierMax := 30
	if w >= detailMinW {
		tierMax = 56
	}
	cols = append(cols,
		col{id: colName, title: "name", min: 6, max: 14},
		col{id: colState, title: "state", min: 7, max: 9},
		col{id: colAge, title: "age", min: 3, max: 6},
		col{id: colTier, title: "tier/engine/model", min: 10, max: tierMax, flex: w < detailMinW},
		col{id: colPR, title: "pr", min: 3, max: 8},
	)
	if w >= detailMinW {
		cols = append(cols, col{id: colDetail, title: "detail", min: 10, max: unbounded, flex: true})
	}
	return cols
}

// layoutColumns picks the kept columns and their widths so a row fits in w.
// When the minimums cannot fit it drops detail, then crew, then pr; the
// caller truncates the composed row as the last resort.
func layoutColumns(cols []col, rows [][numCols]string, w int) ([]col, []int) {
	drop := []colID{colDetail, colCrew, colPR}
	for {
		widths := fitColumns(cols, rows, w)
		if tableWidth(widths) <= w {
			return cols, widths
		}
		dropped := false
		for len(drop) > 0 && !dropped {
			id := drop[0]
			drop = drop[1:]
			if i := slices.IndexFunc(cols, func(c col) bool { return c.id == id }); i >= 0 {
				cols = slices.Delete(slices.Clone(cols), i, i+1)
				dropped = true
			}
		}
		if !dropped {
			return cols, widths
		}
	}
}

func tableWidth(widths []int) int {
	if len(widths) == 0 {
		return livePrefixW
	}
	total := livePrefixW + colGap*(len(widths)-1)
	for _, wd := range widths {
		total += wd
	}
	return total
}

// fitColumns gives each column clamp(content, min, max), lets the flex
// column take what the others leave (never below its min), and if that is
// still too wide shrinks the non-flex columns toward their mins right to
// left.
func fitColumns(cols []col, rows [][numCols]string, w int) []int {
	widths := make([]int, len(cols))
	fixed, flex := 0, -1
	for i, c := range cols {
		content := ansi.StringWidth(c.title)
		for _, r := range rows {
			content = max(content, ansi.StringWidth(r[c.id]))
		}
		widths[i] = clamp(content, c.min, c.max)
		if c.flex {
			flex = i
		} else {
			fixed += widths[i]
		}
	}
	budget := w - livePrefixW - colGap*max0(len(cols)-1)
	if flex >= 0 {
		widths[flex] = clamp(min(budget-fixed, widths[flex]), cols[flex].min, cols[flex].max)
	}
	over := -budget
	for _, wd := range widths {
		over += wd
	}
	for i := len(cols) - 1; i >= 0 && over > 0; i-- {
		if cols[i].flex {
			continue
		}
		cut := min(over, widths[i]-cols[i].min)
		widths[i] -= cut
		over -= cut
	}
	return widths
}

// liveCells are one worker's plain (cleaned, unstyled) cell texts by column.
func liveCells(r liveRow, now time.Time) [numCols]string {
	w := r.w
	or := func(k string) string {
		if s, ok := strField(w, k); ok && s != "" {
			return cleanText(s)
		}
		return "—"
	}
	name := or("name")
	if _, ok := strField(w, "name"); !ok {
		name = or("branch")
	}
	var c [numCols]string
	c[colCrew] = cleanText(r.crewID)
	c[colName] = name
	c[colState] = or("state")
	c[colAge] = reltime(workerAge(w, now))
	c[colTier] = or("tier") + "/" + or("engine") + "/" + or("model")
	c[colPR] = prCell(or("pr_url"))
	c[colDetail] = or("detail")
	return c
}

var prURLRe = regexp.MustCompile(`^https?://[^/\s]+/[^/\s]+/[^/\s]+/pull/(\d+)(?:[/?#]\S*)?$`)

// prCell shows a PR URL as #<n>; a value that does not parse stays raw (the
// column truncates it).
func prCell(url string) string {
	if m := prURLRe.FindStringSubmatch(url); m != nil {
		return "#" + m[1]
	}
	return url
}

// workerLines is the table header, one line per worker, and the index (into
// the worker lines) of the cursor's line.
func (v liveView) workerLines(w int) (header string, lines []string, cursorLine int) {
	now := v.now()
	cells := make([][numCols]string, len(v.rows))
	for i, r := range v.rows {
		cells[i] = liveCells(r, now)
	}
	cols, widths := layoutColumns(liveColumns(w, len(v.snap.Roster.Crews) > 1), cells, w)

	compose := func(prefix string, text func(c col) string) string {
		var b strings.Builder
		b.WriteString(prefix)
		for i, c := range cols {
			cell := text(c)
			if i < len(cols)-1 {
				cell = padCell(cell, widths[i], true) + strings.Repeat(" ", colGap)
			}
			b.WriteString(cell)
		}
		return truncateLine(b.String(), w)
	}
	header = headerStyle.Render(compose("  ", func(c col) string {
		return truncateLine(c.title, widthOf(cols, widths, c.id))
	}))
	for i, r := range v.rows {
		prefix := "  "
		if i == v.cursor {
			prefix = cursorStyle.Render("›") + " "
			cursorLine = i
		}
		lines = append(lines, compose(prefix, func(c col) string {
			cell := truncateLine(cells[i][c.id], widthOf(cols, widths, c.id))
			if c.id == colName {
				return tmuxColorStyle(r.w).Render(cell)
			}
			return cell
		}))
	}
	return header, lines, cursorLine
}

func widthOf(cols []col, widths []int, id colID) int {
	for i, c := range cols {
		if c.id == id {
			return widths[i]
		}
	}
	return 0
}

// padCell pads s to w display cells (never trimming — callers truncate
// first if s might already exceed w); unlike padRight it works fine on an
// already-styled string since ansi.StringWidth ignores escape codes.
func padCell(s string, w int, left bool) string {
	p := w - ansi.StringWidth(s)
	if p <= 0 {
		return s
	}
	pad := strings.Repeat(" ", p)
	if left {
		return s + pad
	}
	return pad + s
}

func strField(w map[string]any, key string) (string, bool) {
	v, ok := w[key]
	if !ok || v == nil {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

// tmuxColorStyle renders a codename in its recorded tmux colour ("colour137"
// -> lipgloss.Color("137")); a missing or malformed field is the default
// style.
func tmuxColorStyle(w map[string]any) lipgloss.Style {
	s, ok := strField(w, "tmux")
	if !ok {
		return lipgloss.NewStyle()
	}
	n := strings.TrimPrefix(s, "colour")
	if n == s {
		return lipgloss.NewStyle()
	}
	if _, err := strconv.Atoi(n); err != nil {
		return lipgloss.NewStyle()
	}
	return lipgloss.NewStyle().Foreground(lipgloss.Color(n))
}

func holdLine(h map[string]any) string {
	id := cleanText(fmt.Sprintf("%v", h["id"]))
	wait, _ := h["wait"].(map[string]any)
	task, _ := h["task"].(map[string]any)
	engine, _ := wait["engine"].(string)
	window, _ := wait["window"].(string)
	engine, window = cleanText(engine), cleanText(window)
	resetsAt, _ := wait["resets_at"].(float64)
	title := ""
	if task != nil {
		if s, ok := task["title"].(string); ok {
			title = s
		}
	}
	return "hold " + id + ": " + engine + " " + window + " until " + isoUTC(int64(resetsAt)) + " — " + cleanText(title)
}

// detailView windows the event list to h the same way tableView windows the
// worker table: the header is never scrolled, and the body is clipped by
// windowOffset (centered on detailCursor) instead of by normalizeFrame
// silently dropping rows past h. detailCursor starts at the newest event
// (RecentEvents is oldest-first) and is moved by j/k/g/G in handleDetailKey.
func (v liveView) detailView(w, h int) string {
	header := []string{truncateLine(headerStyle.Render("recent events"), w)}
	var body []string
	switch {
	case v.detailLoading:
		body = append(body, truncateLine("loading…", w))
	case v.detailErr != "":
		body = append(body, truncateLine("unavailable: "+cleanText(v.detailErr), w))
	case len(v.detailEvents) == 0:
		body = append(body, truncateLine("no recent events", w))
	default:
		for _, ev := range v.detailEvents {
			body = append(body, truncateLine(eventLine(ev), w))
		}
	}

	bodyH := max0(h - len(header))
	total := len(body)
	offset := windowOffset(v.detailCursor, total, bodyH)
	end := offset + bodyH
	if end > total {
		end = total
	}
	out := append([]string{}, header...)
	out = append(out, body[offset:end]...)
	return strings.Join(out, "\n")
}

func eventLine(ev data.Event) string {
	var ts float64
	if v, ok := ev["ts"].(float64); ok {
		ts = v
	}
	t := time.UnixMilli(int64(ts)).UTC().Format("15:04:05")
	kind, _ := ev["kind"].(string)
	return t + "  " + cleanText(kind) + "  " + cleanText(eventDetail(ev))
}

// eventDetail renders a status event as "state: detail" (or just "state")
// and a msg event as "to <target> <body>"; any other kind (dispatch,
// resume, …) shows blank — the header still carries the timestamp/kind.
func eventDetail(ev data.Event) string {
	switch ev["kind"] {
	case "status":
		body, _ := ev["body"].(map[string]any)
		state, _ := body["state"].(string)
		if d, ok := body["detail"].(string); ok && d != "" {
			return state + ": " + d
		}
		return state
	case "msg":
		to, _ := ev["to"].(string)
		var bodyStr string
		switch b := ev["body"].(type) {
		case string:
			bodyStr = b
		case nil:
			bodyStr = ""
		default:
			bodyStr = fmt.Sprintf("%v", b)
		}
		return "to " + to + " " + bodyStr
	default:
		return ""
	}
}
