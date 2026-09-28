package ui

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/noamsto/dispatcher/dash/internal/data"
)

// rosterDeps bundles the roster view's live-refresh seams. eventsPath and
// watcher are resolved/started once by ui.Run (not here — see watcher.go
// and ui.go): eventsPath "" disables the enter-to-drill-in Cmd, watcher nil
// disables the bus-change re-collect (liveOffNote explains why). collect
// re-runs only the roster sources (data.CollectRoster).
type rosterDeps struct {
	eventsPath  string
	watcher     busWatcher
	liveOffNote string
	collect     func() data.RosterSection
}

const debounceDelay = 300 * time.Millisecond

// rosterMsg tags the four message types the roster view's background
// watcher/ticker loop produces, so the root model can route them straight
// to the roster view (see model.go) even while another tab is active —
// the watcher and the age ticker must keep running out of sight.
type rosterMsg interface{ isRosterMsg() }

type tickMsg struct{}
type busChangedMsg struct{}
type debounceFireMsg struct{ at time.Time }
type rosterCollectedMsg data.RosterSection
type recentEventsMsg struct {
	events []data.Event
	err    string
}

func (tickMsg) isRosterMsg()            {}
func (busChangedMsg) isRosterMsg()      {}
func (debounceFireMsg) isRosterMsg()    {}
func (rosterCollectedMsg) isRosterMsg() {}
func (recentEventsMsg) isRosterMsg()    {}

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

func collectRosterCmd(collect func() data.RosterSection) tea.Cmd {
	return func() tea.Msg { return rosterCollectedMsg(collect()) }
}

// rosterWorkerRow pairs a worker map with its owning crew id for the flat,
// cursor-navigable list (holds are informational only — not selectable).
type rosterWorkerRow struct {
	crewID string
	w      map[string]any
}

type rosterView struct {
	deps rosterDeps
	now  func() time.Time

	crews []data.RosterCrew
	err   *string

	flat   []rosterWorkerRow
	cursor int

	lastChange time.Time

	detail        bool
	detailLoading bool
	detailEvents  []data.Event
	detailErr     string
	detailCursor  int // index into detailEvents — RecentEvents is oldest-first, so this starts at the last (newest) event
}

func newRosterView(snap data.Snapshot, deps rosterDeps, now func() time.Time) rosterView {
	v := rosterView{deps: deps, now: now}
	v.load(snap)
	return v
}

func (v *rosterView) load(snap data.Snapshot) {
	v.setRoster(snap.Roster)
}

func (v *rosterView) setRoster(r data.RosterSection) {
	v.crews = r.Crews
	v.err = r.Error
	v.flat = nil
	for _, c := range v.crews {
		for _, w := range c.Workers {
			v.flat = append(v.flat, rosterWorkerRow{crewID: c.ID, w: w})
		}
	}
	v.cursor = clamp(v.cursor, 0, len(v.flat)-1)
}

// Init starts the age ticker and, when a watcher was started (ui.Run
// resolved the bus log path successfully), the bus-change wait loop. Called
// by the root model's Init (model.go), which type-asserts views[3].
func (v rosterView) Init() tea.Cmd {
	cmds := []tea.Cmd{tickCmd()}
	if v.deps.watcher != nil {
		cmds = append(cmds, waitForBusChange(v.deps.watcher))
	}
	return tea.Batch(cmds...)
}

func (v rosterView) Update(msg tea.Msg) (view, tea.Cmd) {
	switch msg := msg.(type) {
	case snapshotMsg:
		v.load(data.Snapshot(msg))
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
		if !msg.at.Equal(v.lastChange) || v.deps.collect == nil {
			return v, nil // superseded by a newer change, or no collector wired
		}
		return v, collectRosterCmd(v.deps.collect)

	case rosterCollectedMsg:
		v.setRoster(data.RosterSection(msg))
		return v, nil

	case recentEventsMsg:
		v.detailLoading = false
		v.detailEvents = msg.events
		v.detailErr = msg.err
		// RecentEvents is oldest-first; start scrolled to the newest event
		// rather than the oldest (finding: a user drilling in wants to land
		// on what just happened, not what happened first).
		v.detailCursor = max0(len(msg.events) - 1)
		return v, nil

	case tea.KeyMsg:
		if v.detail {
			return v.handleDetailKey(msg)
		}
		return v.handleKey(msg)
	}
	return v, nil
}

func (v rosterView) handleKey(msg tea.KeyMsg) (view, tea.Cmd) {
	switch msg.String() {
	case "j", "down":
		v.cursor = clamp(v.cursor+1, 0, len(v.flat)-1)
	case "k", "up":
		v.cursor = clamp(v.cursor-1, 0, len(v.flat)-1)
	case "g":
		v.cursor = 0
	case "G":
		v.cursor = len(v.flat) - 1
	case "enter":
		if v.cursor < 0 || v.cursor >= len(v.flat) || v.deps.eventsPath == "" {
			return v, nil
		}
		v.detail = true
		v.detailLoading = true
		v.detailEvents = nil
		v.detailErr = ""
		branch, _ := strField(v.flat[v.cursor].w, "branch")
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

func (v rosterView) handleDetailKey(msg tea.KeyMsg) (view, tea.Cmd) {
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

func (v rosterView) Keys() []key.Binding {
	if v.detail {
		return []key.Binding{
			key.NewBinding(key.WithKeys("j", "k"), key.WithHelp("j/k", "scroll")),
			key.NewBinding(key.WithKeys("g", "G"), key.WithHelp("g/G", "top/bottom")),
			key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "back")),
		}
	}
	return []key.Binding{
		key.NewBinding(key.WithKeys("j", "k"), key.WithHelp("j/k", "move")),
		key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "events")),
	}
}

func (v rosterView) Selection() data.Selection {
	if v.cursor < 0 || v.cursor >= len(v.flat) {
		return nil
	}
	row := v.flat[v.cursor]
	branch, _ := strField(row.w, "branch")
	session, _ := strField(row.w, "session")
	return data.Worker{Crew: row.crewID, Branch: branch, Session: session}
}

func (v rosterView) Capturing() bool { return false }

func (v rosterView) View(w, h int) string {
	if v.detail {
		return strings.Join(normalizeFrame(v.detailView(w, h), h, w), "\n")
	}
	return strings.Join(normalizeFrame(v.tableView(w, h), h, w), "\n")
}

// tableView windows its body (crew headings, table headers, worker rows and
// hold lines) to h the same way runs.go's ratingsLines/rowsLines do: the
// chrome (live-off note) is never scrolled, and windowOffset keeps the
// cursor's worker row on-screen instead of letting normalizeFrame silently
// clip rows past h (charm-tui skill, trap 9).
func (v rosterView) tableView(w, h int) string {
	var top []string
	if v.deps.watcher == nil && v.deps.liveOffNote != "" {
		top = append(top, truncateLine(warnStyle.Render(v.deps.liveOffNote), w))
	}
	if v.err != nil {
		top = append(top, truncateLine("unavailable: "+*v.err, w))
		return strings.Join(top, "\n")
	}
	if len(v.crews) == 0 {
		top = append(top, truncateLine("no active crew", w))
		return strings.Join(top, "\n")
	}

	headers := []string{"name", "state", "tier/engine/model", "detail", "age", "pr"}
	left := []bool{true, true, true, true, true, true}
	var body []string
	cursorLine := 0
	flatIdx := 0
	for _, c := range v.crews {
		body = append(body, truncateLine(headerStyle.Render("crew "+c.ID), w))
		if c.Error != nil {
			body = append(body, truncateLine("  unavailable: "+*c.Error, w))
			continue
		}
		if len(c.Workers) > 0 {
			rows := make([][]string, len(c.Workers))
			for i, wm := range c.Workers {
				rows[i] = rosterWorkerCells(wm, v.now())
			}
			widths := tableWidths(append([][]string{headers}, rows...))
			body = append(body, truncateLine("  "+padRow(headers, widths, left), w))
			for i, r := range rows {
				prefix := "  "
				if flatIdx == v.cursor {
					prefix = cursorStyle.Render("›") + " "
					cursorLine = len(body)
				}
				name := tmuxColorStyle(c.Workers[i]).Render(padCell(r[0], widths[0], true))
				rest := padRow(r[1:], widths[1:], left[1:])
				body = append(body, truncateLine(prefix+name+"  "+rest, w))
				flatIdx++
			}
		}
		for _, hd := range c.Holds {
			body = append(body, truncateLine("  "+holdLine(hd), w))
		}
	}

	bodyH := max0(h - len(top))
	total := len(body)
	offset := windowOffset(cursorLine, total, bodyH)
	end := offset + bodyH
	if end > total {
		end = total
	}
	out := append([]string{}, top...)
	out = append(out, body[offset:end]...)
	return strings.Join(out, "\n")
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

// Column caps for the worker table (name/state/tier/detail/pr): fixed, not
// derived from the terminal width, so six columns plus five 2-space gaps
// plus the 2-cell cursor prefix always sums under 80 — the narrowest
// golden width — instead of the final truncateLine cutting off whichever
// column lands past the edge (originally age/pr, silently, since detail
// alone was capped at 40). age gets its own smaller cap since reltime's
// output is always short.
const (
	rosterNameCap   = 8
	rosterStateCap  = 8
	rosterTierCap   = 18
	rosterDetailCap = 14
	rosterAgeCap    = 5
	rosterPRCap     = 14
)

// rosterWorkerCells caps every free-text column itself (not just the
// final-line truncateLine safety net) so one long value can't blow out
// every column's width via tableWidths and push later columns past the
// line's own truncation. age is always recomputed from the worker's ts
// against the passed-in now — never read from a stale age_s snapshot field
// — so the 1s tick can animate it without a collect.
func rosterWorkerCells(w map[string]any, now time.Time) []string {
	name := "—"
	if s, ok := strField(w, "name"); ok {
		name = s
	} else if s, ok := strField(w, "branch"); ok {
		name = s
	}
	state := "—"
	if s, ok := strField(w, "state"); ok {
		state = s
	}
	tier, engine, model := "—", "—", "—"
	if s, ok := strField(w, "tier"); ok {
		tier = s
	}
	if s, ok := strField(w, "engine"); ok {
		engine = s
	}
	if s, ok := strField(w, "model"); ok {
		model = s
	}
	detail := "—"
	if s, ok := strField(w, "detail"); ok && s != "" {
		detail = cleanText(s)
	}
	var tsMs float64
	switch v := w["ts"].(type) {
	case int64:
		tsMs = float64(v)
	case float64:
		tsMs = v
	}
	ageS := now.Unix() - int64(math.Floor(tsMs/1000))
	if ageS < 0 {
		ageS = 0
	}
	pr := "—"
	if s, ok := strField(w, "pr_url"); ok {
		pr = s
	}
	return []string{
		truncateLine(cleanText(name), rosterNameCap),
		truncateLine(cleanText(state), rosterStateCap),
		truncateLine(tier+"/"+engine+"/"+model, rosterTierCap),
		truncateLine(detail, rosterDetailCap),
		truncateLine(reltime(ageS), rosterAgeCap),
		truncateLine(pr, rosterPRCap),
	}
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
// style (spec §Roster).
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
	id := fmt.Sprintf("%v", h["id"])
	wait, _ := h["wait"].(map[string]any)
	task, _ := h["task"].(map[string]any)
	engine, _ := wait["engine"].(string)
	window, _ := wait["window"].(string)
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
func (v rosterView) detailView(w, h int) string {
	header := []string{truncateLine(headerStyle.Render("recent events"), w)}
	var body []string
	switch {
	case v.detailLoading:
		body = append(body, truncateLine("loading…", w))
	case v.detailErr != "":
		body = append(body, truncateLine("unavailable: "+v.detailErr, w))
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
