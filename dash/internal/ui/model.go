// Package ui is the interactive Bubble Tea program: a root model holding
// tabs, a help bar and the current Snapshot, and one sub-model per view.
package ui

import (
	"context"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/help"
	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/noamsto/dispatcher/dash/internal/data"
)

// view is the interface every tab's sub-model satisfies. Capturing is true
// while a textinput owns keystrokes (the settings filter) — the root does
// not intercept global keys (q, digits, tab, r, ?) while it is true.
type view interface {
	Update(tea.Msg) (view, tea.Cmd)
	View(w, h int) string
	Keys() []key.Binding
	Selection() data.Selection
	Capturing() bool
}

var tabTitles = []string{"Live", "Budget", "Runs", "Settings"}

// snapshotMsg carries the root model's current Snapshot and per-source
// loaded flags to every view's Update, not just the active one, so a tab
// rebuilds its rows/tree even while off-screen.
type snapshotMsg struct {
	snap   data.Snapshot
	loaded [numSources]bool
}

// requestRosterMsg asks the root to reload the roster source (emitted by the
// live view's bus-change debounce).
type requestRosterMsg struct{}

// clearStatusMsg clears the status line it was scheduled for; a newer
// status has a later at and survives.
type clearStatusMsg struct{ at time.Time }

const statusTTL = 10 * time.Second

// Model is the root Bubble Tea model.
type Model struct {
	now     func() time.Time
	ctx     context.Context
	loaders Loaders

	snap data.Snapshot
	load [numSources]loadState

	width, height int
	ready         bool

	status    string // last action outcome, shown above the help bar
	statusErr bool
	statusAt  time.Time

	active int
	views  [4]view

	help help.Model
	keys KeyMap
}

// NewModel builds the root model with an empty Snapshot: every view renders
// its loading placeholder until Init's loaders land. now stamps "refreshed
// HH:MM:SS" and is injected so tests get a deterministic clock; live carries
// the Live tab's live-refresh seams (watcher, events path), already
// resolved/started by ui.Run.
func NewModel(deps Deps, now func() time.Time, live liveDeps) Model {
	ctx := deps.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	m := Model{
		now:     now,
		ctx:     ctx,
		loaders: deps.Load,
		snap: data.Snapshot{
			Now:    deps.Now,
			Roster: data.RosterSection{Crews: []data.RosterCrew{}},
		},
		keys: DefaultKeyMap(),
		help: help.New(),
	}
	// Init (a value receiver) fires every loader, so mark them in flight
	// here: a request before their results land must set again, not start a
	// duplicate subprocess.
	for i := range m.load {
		m.load[i].inFlight = true
	}
	lv := newLiveView(m.snap, m.loaded(), live, now)
	lv.acts = actionDeps{ctx: ctx, runner: deps.Runner, crew: deps.Crew, inTmux: deps.InTmux}
	m.views = [4]view{
		lv,
		newBudgetView(m.snap, m.loaded()),
		newRunsView(m.snap, m.loaded()),
		newSettingsView(m.snap, m.loaded()),
	}
	return m
}

func (m Model) loaded() [numSources]bool {
	var out [numSources]bool
	for i, st := range m.load {
		out[i] = st.loaded
	}
	return out
}

// request starts a load of s, or marks it to reload when the in-flight one
// lands.
func (m *Model) request(s source) tea.Cmd {
	st := &m.load[s]
	if st.inFlight {
		st.again = true
		return nil
	}
	st.inFlight = true
	return m.loaders.cmd(m.ctx, s)
}

func (m *Model) requestAll() tea.Cmd {
	cmds := make([]tea.Cmd, 0, numSources)
	for s := source(0); s < numSources; s++ {
		cmds = append(cmds, m.request(s))
	}
	return tea.Batch(cmds...)
}

// Init starts every source loader concurrently plus the live view's
// background watcher/ticker loop (see live.go's liveView.Init) — the
// only view with startup Cmds, so the root doesn't carry a general per-view
// Init in the shared `view` interface for the sake of one caller. Nothing
// here runs before the first View.
func (m Model) Init() tea.Cmd {
	cmds := make([]tea.Cmd, 0, numSources+1)
	for s := source(0); s < numSources; s++ {
		cmds = append(cmds, m.loaders.cmd(m.ctx, s))
	}
	if lv, ok := m.views[0].(liveView); ok {
		cmds = append(cmds, lv.Init())
	}
	return tea.Batch(cmds...)
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.ready = true
		return m, nil

	case sourceMsg:
		msg.apply(&m.snap)
		st := &m.load[msg.src]
		st.inFlight, st.loaded, st.at = false, true, m.now()
		cmds := m.broadcast()
		if st.again {
			st.again = false
			cmds = append(cmds, m.request(msg.src))
		}
		return m, tea.Batch(cmds...)

	case actionMsg:
		m.status, m.statusErr, m.statusAt = msg.text, msg.err, m.now()
		at := m.statusAt
		return m, tea.Tick(statusTTL, func(time.Time) tea.Msg { return clearStatusMsg{at: at} })

	case clearStatusMsg:
		if msg.at.Equal(m.statusAt) {
			m.status = ""
		}
		return m, nil

	case requestRosterMsg:
		return m, m.request(srcRoster)

	case rosterMsg:
		// The live view's background watcher/ticker loop must keep
		// running (and its results must land) regardless of which tab is
		// active — route straight to views[0] rather than m.active.
		nv, cmd := m.views[0].Update(msg)
		m.views[0] = nv
		return m, cmd

	case tea.KeyMsg:
		return m.handleKey(msg)
	}

	nv, cmd := m.views[m.active].Update(msg)
	m.views[m.active] = nv
	return m, cmd
}

// broadcast forwards the current Snapshot to every view.
func (m *Model) broadcast() []tea.Cmd {
	msg := snapshotMsg{snap: m.snap, loaded: m.loaded()}
	var cmds []tea.Cmd
	for i, v := range m.views {
		nv, cmd := v.Update(msg)
		m.views[i] = nv
		cmds = append(cmds, cmd)
	}
	return cmds
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if !m.views[m.active].Capturing() {
		switch {
		case key.Matches(msg, m.keys.Quit):
			return m, tea.Quit
		case key.Matches(msg, m.keys.Help):
			m.help.ShowAll = !m.help.ShowAll
			return m, nil
		case key.Matches(msg, m.keys.Refresh):
			return m, m.requestAll()
		case key.Matches(msg, m.keys.Next):
			m.active = (m.active + 1) % len(m.views)
			return m, nil
		case key.Matches(msg, m.keys.Prev):
			m.active = (m.active - 1 + len(m.views)) % len(m.views)
			return m, nil
		default:
			if i := tabIndex(msg.String()); i >= 0 {
				m.active = i
				return m, nil
			}
		}
	}

	nv, cmd := m.views[m.active].Update(msg)
	m.views[m.active] = nv
	return m, cmd
}

func (m Model) View() string {
	if !m.ready {
		return ""
	}

	tabBar := m.renderTabs()
	footer := m.renderFooter()
	headerLines := strings.Split(tabBar, "\n")
	footerLines := strings.Split(footer, "\n")

	statusLines := m.statusLines()
	bodyH := max0(m.height - len(headerLines) - len(statusLines) - len(footerLines))
	bodyW := max0(m.width)
	body := m.views[m.active].View(bodyW, bodyH)
	bodyLines := normalizeFrame(body, bodyH, bodyW)

	all := make([]string, 0, m.height)
	all = append(all, headerLines...)
	all = append(all, bodyLines...)
	all = append(all, statusLines...)
	all = append(all, footerLines...)
	for len(all) < m.height {
		all = append(all, "")
	}
	if len(all) > m.height {
		all = all[:m.height]
	}
	return strings.Join(all, "\n")
}

// statusLines is the one-line action status, or nothing when unset.
func (m Model) statusLines() []string {
	if m.status == "" {
		return nil
	}
	line := truncateLine(m.status, m.width)
	if m.statusErr {
		line = warnStyle.Render(line)
	}
	return []string{line}
}

// renderTabs marks the active tab with a leading "▸ " (and a blank "  " on
// every inactive one, so no tab's width shifts when the active one
// changes) — color alone is invisible under NO_COLOR/ASCII, where
// tabActiveStyle's bold+underline survives as no visual difference at all.
func (m Model) renderTabs() string {
	parts := make([]string, len(tabTitles))
	for i, t := range tabTitles {
		st := tabInactiveStyle
		marker := "  "
		if i == m.active {
			st = tabActiveStyle
			marker = "▸ "
		}
		parts[i] = st.Render(marker + t)
	}
	line := strings.Join(parts, tabSepStyle.Render(" │ "))
	return truncateLine(line, m.width)
}

// loadStatus is the footer's status text: the in-flight sources while any
// load is running, else when the latest one landed.
func (m Model) loadStatus() string {
	var inFlight []string
	var latest time.Time
	for s, st := range m.load {
		if st.inFlight {
			inFlight = append(inFlight, source(s).String())
		}
		if st.loaded && st.at.After(latest) {
			latest = st.at
		}
	}
	switch {
	case len(inFlight) > 0:
		return "loading " + strings.Join(inFlight, ", ") + "…"
	case latest.IsZero():
		return "loading…"
	}
	return "refreshed " + latest.Format("15:04:05")
}

func (m Model) renderFooter() string {
	status := "  " + m.loadStatus()

	// Reserve room for the status segment before letting bubbles/help
	// truncate the key list itself, so a narrow terminal drops help hints
	// before it drops the load status.
	h := m.help
	h.Width = max0(m.width - ansi.StringWidth(status))
	keys := h.View(helpKeyMap{global: m.keys, view: m.views[m.active].Keys()})
	line := keys + statusStyle.Render(status)
	return truncateLine(line, m.width)
}

// helpKeyMap adapts the root's global KeyMap plus the active view's flat
// Keys() list into bubbles/help's key.Map interface.
type helpKeyMap struct {
	global KeyMap
	view   []key.Binding
}

func (k helpKeyMap) ShortHelp() []key.Binding {
	return append(append([]key.Binding{}, k.global.ShortHelp()...), k.view...)
}

func (k helpKeyMap) FullHelp() [][]key.Binding {
	rows := k.global.FullHelp()
	if len(k.view) > 0 {
		rows = append(rows, k.view)
	}
	return rows
}
