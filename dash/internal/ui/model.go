// Package ui is the interactive Bubble Tea program: a root model holding
// tabs, a help bar and the current Snapshot, and one sub-model per view.
package ui

import (
	"context"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/help"
	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/noamsto/dispatcher/dash/internal/data"
	vm "github.com/noamsto/dispatcher/dash/internal/view"
)

// view is the interface every tab's sub-model satisfies. Capturing is true
// while a textinput owns keystrokes (the settings number input) — the root does
// not intercept global keys (q, digits, tab, r, ?) while it is true.
type view interface {
	Update(tea.Msg) (view, tea.Cmd)
	View(w, h int) string
	Keys() []key.Binding
	Selection() data.Selection
	Capturing() bool
}

// keyOwner is implemented by a view that sometimes wants a global key (tab,
// digits, r, ?) for itself; the root then forwards it instead of acting. It
// is never consulted for quit, `[`/`]` or `R`.
type keyOwner interface{ ownsKey(tea.KeyMsg) bool }

// tabOwner is implemented by a view that sometimes wants tab and shift+tab
// for itself; the root then forwards them instead of switching views.
type tabOwner interface{ ownsTab() bool }

// tabHooks is implemented by a view that does work while it is the active tab.
// enter gets the first view's selection, so a view can open on what the
// operator was looking at.
type tabHooks interface {
	enter(first data.Selection) (view, tea.Cmd)
	leave() (view, tea.Cmd)
}

var tabTitles = []string{"Overview", "Live", "Board", "Budget", "Runs", "Settings", "Map"}

// Tab indexes into Model.views and tabTitles.
const (
	overviewTab = iota
	liveTab
	boardTab
	budgetTab
	runsTab
	settingsTab
	mapTab
	numTabs
)

// snapshotMsg carries the view models built from the root's current sources
// to every view's Update, not just the active one, so a tab rebuilds its
// rows/tree even while off-screen.
type snapshotMsg struct {
	in     vm.Inputs        // the view-model inputs the root assembled from snap and the bus digest
	loaded [numSources]bool // per source: Runs shows its two sources' loading separately

	roster   vm.RosterView
	budget   vm.BudgetView
	runs     vm.RunsView
	settings vm.SettingsView
	mapv     vm.MapView
}

func newSnapshotMsg(in vm.Inputs, loaded [numSources]bool) snapshotMsg {
	return snapshotMsg{
		in:       in,
		loaded:   loaded,
		roster:   vm.BuildRoster(in),
		budget:   vm.BuildBudget(in),
		runs:     vm.BuildRuns(in),
		settings: vm.BuildSettings(in),
		mapv:     vm.BuildMap(in),
	}
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

	snap   data.Snapshot
	digest data.BusDigest
	crew   []string
	load   [numSources]loadState

	width, height int
	ready         bool

	status    string // last action outcome, shown above the help bar
	statusErr bool
	statusAt  time.Time

	active int // overviewTab at start
	views  [numTabs]view

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
		crew: deps.Crew,
		keys: DefaultKeyMap(),
		help: help.New(),
	}
	// Init (a value receiver) fires every loader, so mark them in flight
	// here: a request before their results land must set again, not start a
	// duplicate subprocess.
	for i := range m.load {
		m.load[i].inFlight = true
	}
	msg := newSnapshotMsg(m.viewInputs(), m.loaded())
	lv := newLiveView(msg.roster, msg.budget, live, now)
	acts := actionDeps{ctx: ctx, runner: deps.Runner, crew: deps.Crew, inTmux: deps.InTmux}
	lv.acts = acts
	ov := newOverviewView(msg.in, now)
	ov.acts = acts
	m.views = [numTabs]view{
		overviewTab: ov,
		liveTab:     lv,
		boardTab:    newBoardView(msg.roster, now, acts),
		budgetTab:   newBudgetView(msg.budget),
		runsTab:     newRunsView(msg.runs),
		settingsTab: newSettingsView(msg.settings, settingsDeps{
			ctx: ctx, runner: deps.Runner, bin: deps.DispatchConfig,
			getenv: os.Getenv, exec: tea.ExecProcess,
		}),
		mapTab: newMapView(msg.mapv, deps.Map, acts),
	}
	return m
}

// viewInputs is what the view-model builders read, assembled from the root's
// sources.
func (m Model) viewInputs() vm.Inputs {
	return inputsOf(m.snap, m.digest, m.loaded(), m.now().UnixMilli(), m.crew)
}

func inputsOf(snap data.Snapshot, digest data.BusDigest, l [numSources]bool, nowMS int64, crew []string) vm.Inputs {
	return vm.Inputs{
		Snap: snap,
		Bus:  digest,
		Loaded: vm.Loaded{
			Settings: l[srcSettings], Budget: l[srcBudget],
			Retro: l[srcRetro], Ratings: l[srcRatings], Roster: l[srcRoster], Bus: l[srcBus],
		},
		NowMS: nowMS,
		Crew:  crew,
	}
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
	if lv, ok := m.views[liveTab].(liveView); ok {
		cmds = append(cmds, lv.Init())
	}
	cmds = append(cmds, tea.EnableMouseCellMotion)
	return tea.Batch(cmds...)
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	nm, cmd := m.update(msg)
	sync := nm.syncMapBox()
	return nm, tea.Batch(cmd, sync)
}

// mapBox is the body cells the Map view may use. A status line is always
// reserved so the image does not resize each time one appears.
func (m Model) mapBox() (w, h int) {
	footer := strings.Split(m.renderFooter(), "\n")
	return m.width, max0(m.height - len(m.headerLines()) - 1 - len(footer))
}

// syncMapBox tells the Map view its box while it is the active tab; the other
// tabs change the footer's height, so the box is recomputed on switching.
func (m *Model) syncMapBox() tea.Cmd {
	if m.active != mapTab || !m.ready {
		return nil
	}
	w, h := m.mapBox()
	nv, cmd := m.views[mapTab].(mapView).resize(w, h)
	m.views[mapTab] = nv
	return cmd
}

// handoffSelection is what a view being entered opens on: the worker the tab
// being left has selected (Live or Board), else Live's selection.
func (m Model) handoffSelection() data.Selection {
	if sel := m.views[m.active].Selection(); sel != nil {
		if _, ok := sel.(data.Worker); ok {
			return sel
		}
	}
	return m.views[liveTab].Selection()
}

// switchTab makes tab i active, running the leaving and entering views'
// hooks.
func (m *Model) switchTab(i int) tea.Cmd {
	if i == m.active {
		return nil
	}
	var leaveCmd, enterCmd tea.Cmd
	if h, ok := m.views[m.active].(tabHooks); ok {
		var nv view
		nv, leaveCmd = h.leave()
		m.views[m.active] = nv
	}
	first := m.handoffSelection()
	m.active = i
	sync := m.syncMapBox()
	if h, ok := m.views[i].(tabHooks); ok {
		var nv view
		nv, enterCmd = h.enter(first)
		m.views[i] = nv
	}
	// Map and Overview both toggle mouse reporting: the leave must land
	// before the enter, which a plain Batch does not guarantee.
	return tea.Sequence(leaveCmd, tea.Batch(sync, enterCmd))
}

func (m Model) update(msg tea.Msg) (Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.ready = true
		// The Live, Board and Overview views keep the width for their reply prompt
		// and key handling.
		var cmds []tea.Cmd
		for _, i := range []int{liveTab, boardTab, overviewTab} {
			nv, cmd := m.views[i].Update(msg)
			m.views[i] = nv
			cmds = append(cmds, cmd)
		}
		return m, tea.Batch(cmds...)

	case sourceMsg:
		msg.apply(&m.snap)
		if msg.bus != nil {
			m.digest = *msg.bus
		}
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
		return m, tea.Batch(m.request(srcRoster), m.request(srcBus))

	case rosterMsg:
		// The live view's background watcher/ticker loop must keep
		// running (and its results must land) regardless of which tab is
		// active — route straight to liveTab rather than m.active. The tick
		// also refreshes the Overview's ages.
		nv, cmd := m.views[liveTab].Update(msg)
		m.views[liveTab] = nv
		if _, ok := msg.(tickMsg); ok {
			ov, ocmd := m.views[overviewTab].Update(msg)
			m.views[overviewTab] = ov
			cmd = tea.Batch(cmd, ocmd)
		}
		return m, cmd

	case requestSettingsMsg:
		return m, m.request(srcSettings)

	case mapRenderedMsg:
		// A render lands whichever tab is active, like the roster messages.
		nv, cmd := m.views[mapTab].Update(msg)
		m.views[mapTab] = nv
		return m, cmd

	case settingsWriteMsg, editorDoneMsg, effectiveReadyMsg:
		// Write and editor results land whichever tab is active.
		nv, cmd := m.views[settingsTab].Update(msg)
		m.views[settingsTab] = nv
		return m, cmd

	case tea.MouseMsg:
		msg.Y -= len(m.headerLines())
		nv, cmd := m.views[m.active].Update(msg)
		m.views[m.active] = nv
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
	msg := newSnapshotMsg(m.viewInputs(), m.loaded())
	var cmds []tea.Cmd
	for i, v := range m.views {
		nv, cmd := v.Update(msg)
		m.views[i] = nv
		cmds = append(cmds, cmd)
	}
	return cmds
}

func (m Model) handleKey(msg tea.KeyMsg) (Model, tea.Cmd) {
	if !m.views[m.active].Capturing() {
		if key.Matches(msg, m.keys.Quit) {
			return m, tea.Quit
		}
		if cmd, handled := m.handleGlobalKey(msg); handled {
			return m, cmd
		}
	}

	nv, cmd := m.views[m.active].Update(msg)
	m.views[m.active] = nv
	return m, cmd
}

// handleGlobalKey acts on the keys every view shares unless the active view
// owns the key. `[`, `]` and `R` are never owned.
func (m *Model) handleGlobalKey(msg tea.KeyMsg) (tea.Cmd, bool) {
	switch {
	case key.Matches(msg, m.keys.PrevView):
		return m.switchTab((m.active - 1 + numTabs) % numTabs), true
	case key.Matches(msg, m.keys.NextView):
		return m.switchTab((m.active + 1) % numTabs), true
	case msg.String() == "R":
		return m.requestAll(), true
	}
	if owner, ok := m.views[m.active].(keyOwner); ok && owner.ownsKey(msg) {
		return nil, false
	}
	owner, _ := m.views[m.active].(tabOwner)
	ownsTab := owner != nil && owner.ownsTab()
	switch {
	case key.Matches(msg, m.keys.Help):
		m.help.ShowAll = !m.help.ShowAll
		return nil, true
	case key.Matches(msg, m.keys.Refresh):
		return m.requestAll(), true
	case key.Matches(msg, m.keys.Next) && !ownsTab:
		return m.switchTab((m.active + 1) % numTabs), true
	case key.Matches(msg, m.keys.Prev) && !ownsTab:
		return m.switchTab((m.active - 1 + numTabs) % numTabs), true
	}
	if i := tabIndex(msg.String()); i >= 0 {
		return m.switchTab(i), true
	}
	return nil, false
}

func (m Model) View() string {
	if !m.ready {
		return ""
	}

	footer := m.renderFooter()
	headerLines := m.headerLines()
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

// headerLines is the tab bar, which sits above every view's body.
func (m Model) headerLines() []string { return strings.Split(m.renderTabs(), "\n") }

// statusLines is the one-line action status, or nothing when unset.
func (m Model) statusLines() []string {
	if m.status == "" {
		return nil
	}
	line := truncateLine(cleanText(m.status), m.width)
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
	owner, _ := m.views[m.active].(tabOwner)
	digits, _ := m.views[m.active].(keyOwner)
	capturing := m.views[m.active].Capturing()
	keys := h.View(helpKeyMap{
		global:     m.keys,
		view:       m.views[m.active].Keys(),
		capturing:  capturing,
		ownsTab:    !capturing && owner != nil && owner.ownsTab(),
		ownsDigits: !capturing && digits != nil && digits.ownsKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'1'}}),
	})
	// help can overshoot Width by one item; clip it so the status stays whole.
	keys = ansi.Truncate(keys, h.Width, "…")
	line := keys + statusStyle.Render(status)
	return truncateLine(line, m.width)
}

// helpKeyMap adapts the active view's flat Keys() list plus the root's global
// KeyMap into bubbles/help's key.Map interface. The view's keys come first so
// a narrow bar truncates the globals, and while the view is capturing the
// globals are omitted because they only type into its prompt.
type helpKeyMap struct {
	global     KeyMap
	view       []key.Binding
	capturing  bool
	ownsTab    bool // the view uses tab for its own selection
	ownsDigits bool // the view uses the digit keys for its own actions
}

func (k helpKeyMap) ShortHelp() []key.Binding {
	if k.capturing {
		return k.view
	}
	return append(slices.Clone(k.view), k.global.shortHelp(k.ownsTab, k.ownsDigits)...)
}

func (k helpKeyMap) FullHelp() [][]key.Binding {
	var rows [][]key.Binding
	if len(k.view) > 0 {
		rows = append(rows, k.view)
	}
	if k.capturing {
		return rows
	}
	return append(rows, k.global.fullHelp(k.ownsTab, k.ownsDigits)...)
}
