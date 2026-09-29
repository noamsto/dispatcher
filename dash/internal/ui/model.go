// Package ui is the interactive Bubble Tea program: a root model holding
// tabs, a help bar and the current Snapshot, and one sub-model per view.
package ui

import (
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

var tabTitles = []string{"Settings", "Budget", "Runs", "Roster"}

// snapshotMsg carries a freshly collected Snapshot back from an async `r`
// refresh. It is forwarded to every view's Update, not just the active one,
// so a tab rebuilds its rows/tree even while off-screen.
type snapshotMsg data.Snapshot

// Model is the root Bubble Tea model.
type Model struct {
	collect func() data.Snapshot
	now     func() time.Time

	width, height int
	ready         bool

	active int
	views  [4]view

	help         help.Model
	keys         KeyMap
	refreshing   bool
	refreshedAt  time.Time
	hasRefreshed bool
}

// NewModel builds the root model from an initial Snapshot. collect re-runs
// the whole collection (bound to "r"); now stamps "refreshed HH:MM:SS" and
// is injected so tests get a deterministic clock; roster carries the
// Roster tab's live-refresh seams (watcher, events path, roster-only
// re-collect — see roster.go), already resolved/started by ui.Run.
func NewModel(snapshot data.Snapshot, collect func() data.Snapshot, now func() time.Time, roster rosterDeps) Model {
	h := help.New()
	m := Model{
		collect: collect,
		now:     now,
		keys:    DefaultKeyMap(),
		help:    h,
		// Seeded from the initial Snapshot's own collection time (not
		// now()) — main.go already collected it before ui.Run starts, so
		// the footer should read that moment, not "just started".
		refreshedAt:  time.Unix(snapshot.Now, 0).In(now().Location()),
		hasRefreshed: true,
	}
	m.views = [4]view{
		newSettingsView(snapshot),
		newBudgetView(snapshot),
		newRunsView(snapshot),
		newRosterView(snapshot, roster, now),
	}
	return m
}

// Init starts the roster view's background watcher/ticker loop (see
// roster.go's rosterView.Init) — the only view with startup Cmds, so the
// root doesn't carry a general per-view Init in the shared `view` interface
// for the sake of one caller.
func (m Model) Init() tea.Cmd {
	if rv, ok := m.views[3].(rosterView); ok {
		return rv.Init()
	}
	return nil
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.ready = true
		return m, nil

	case snapshotMsg:
		m.refreshing = false
		m.refreshedAt = m.now()
		m.hasRefreshed = true
		for i, v := range m.views {
			nv, _ := v.Update(msg)
			m.views[i] = nv
		}
		return m, nil

	case rosterMsg:
		// The roster view's background watcher/ticker loop must keep
		// running (and its results must land) regardless of which tab is
		// active — route straight to views[3] rather than m.active.
		nv, cmd := m.views[3].Update(msg)
		m.views[3] = nv
		return m, cmd

	case tea.KeyMsg:
		return m.handleKey(msg)
	}

	nv, cmd := m.views[m.active].Update(msg)
	m.views[m.active] = nv
	return m, cmd
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
			m.refreshing = true
			return m, m.refreshCmd()
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

func (m Model) refreshCmd() tea.Cmd {
	collect := m.collect
	return func() tea.Msg {
		return snapshotMsg(collect())
	}
}

func (m Model) View() string {
	if !m.ready {
		return ""
	}

	tabBar := m.renderTabs()
	footer := m.renderFooter()
	headerLines := strings.Split(tabBar, "\n")
	footerLines := strings.Split(footer, "\n")

	bodyH := max0(m.height - len(headerLines) - len(footerLines))
	bodyW := max0(m.width)
	body := m.views[m.active].View(bodyW, bodyH)
	bodyLines := normalizeFrame(body, bodyH, bodyW)

	all := make([]string, 0, m.height)
	all = append(all, headerLines...)
	all = append(all, bodyLines...)
	all = append(all, footerLines...)
	for len(all) < m.height {
		all = append(all, "")
	}
	if len(all) > m.height {
		all = all[:m.height]
	}
	return strings.Join(all, "\n")
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

func (m Model) renderFooter() string {
	status := "not yet refreshed"
	if m.refreshing {
		status = "refreshing…"
	} else if m.hasRefreshed {
		status = "refreshed " + m.refreshedAt.Format("15:04:05")
	}
	status = "  " + status

	// Reserve room for the status segment before letting bubbles/help
	// truncate the key list itself, so a narrow terminal drops help hints
	// before it drops the refresh timestamp.
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
