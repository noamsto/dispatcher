package ui

import (
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/noamsto/dispatcher/dash/internal/data"
	vm "github.com/noamsto/dispatcher/dash/internal/view"
)

// fleetView is the machine-wide Fleet tab. fv is cleaned once on arrival:
// peer and tailscale strings are untrusted. Its strip actions share the
// Overview's prompt machinery; enter drills into one crew's Overview.
type fleetView struct {
	now    func() time.Time
	fv     vm.FleetView
	in     vm.FleetInputs
	loaded bool
	st     fleetState
	pills  pillMode

	actionPrompt
	size *ovSize

	drilled   bool
	drillCrew string // the drilled crew's row key
	crumb     string
	drill     overviewView
}

func newFleetView(msg snapshotMsg, now func() time.Time) fleetView {
	v := fleetView{now: now, pills: pillModeOf(os.Getenv, lipgloss.ColorProfile()), size: &ovSize{}}
	v.apply(msg)
	return v
}

// apply takes a snapshot's fleet model and keeps the operator's state, which
// the layout resolves against it per frame. A drilled Overview is re-fed its
// crew's fresh inputs, keeping its own state, or closed when the crew is gone.
func (v *fleetView) apply(msg snapshotMsg) tea.Cmd {
	v.fv, v.in, v.loaded = cleanView(msg.fleet), msg.fleetIn, msg.loaded[srcFleet]
	if !v.drilled {
		return nil
	}
	in, ok := vm.CrewInputs(v.in, v.drillCrew)
	if !ok {
		v.drilled = false
		return statusCmd("crew gone: "+v.crumb, true)
	}
	nv, _ := v.drill.Update(snapshotMsg{in: in})
	v.drill = nv.(overviewView)
	return nil
}

func (v fleetView) Update(msg tea.Msg) (view, tea.Cmd) {
	switch msg := msg.(type) {
	case snapshotMsg:
		cmd := v.apply(msg)
		return v, cmd
	case tea.MouseMsg:
		if v.drilled {
			msg.Y-- // the breadcrumb
			return v.forward(msg)
		}
		if v.mode == ovNormal && msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonLeft {
			v.click(msg.X, msg.Y)
		}
	case tea.KeyMsg:
		cmd := v.handleKey(msg)
		return v, cmd
	default:
		if v.drilled {
			return v.forward(msg)
		}
		if v.mode == ovReply || v.mode == ovFilter {
			var cmd tea.Cmd
			v.input, cmd = v.input.Update(msg)
			return v, cmd
		}
	}
	return v, nil
}

func (v fleetView) forward(msg tea.Msg) (view, tea.Cmd) {
	nv, cmd := v.drill.Update(msg)
	v.drill = nv.(overviewView)
	return v, cmd
}

func (v fleetView) View(w, h int) string {
	*v.size = ovSize{w, h}
	if !v.loaded {
		return strings.Join(normalizeFrame(loadingLine(srcFleet, w), h, w), "\n")
	}
	if w <= 0 || h <= 0 {
		return ""
	}
	if v.drilled {
		crumb := truncateLine(" "+fg(colDim).Render("fleet ›")+" "+lipgloss.NewStyle().Bold(true).Render(v.crumb), w)
		lines := append([]string{crumb}, normalizeFrame(v.drill.View(w, h-1), h-1, w)...)
		return strings.Join(lines[:h], "\n")
	}
	base := renderFleet(w, h, v.fv, v.st, v.pills)
	if v.mode == ovNormal {
		return base
	}
	return drawOverlay(strings.Split(base, "\n"), w, h, v.pills, func(iw int) (panelSpec, []string) {
		if v.mode == ovHelp {
			return panelSpec{title: "HELP", color: colTeal, focused: true}, v.helpLines()
		}
		return v.promptPanel(iw)
	})
}

var fleetHelp = []helpKey{
	{"tab / shift+tab", "cycle focus: attention, crews, capacity, holds, hosts"},
	{"j k / up down", "move in the focused panel (the attention strip scrolls)"},
	{"1-9", "act on the attention row with that number"},
	{"enter", "open the crew's Overview (crews or attention); esc returns"},
	{"L", "relaunch an orphaned crew's dispatcher (confirms, needs tmux)"},
	{"D", "show or hide the dead crews section"},
	{"/", "filter crews by repo, crew or host; enter keeps, esc clears"},
	{"[ ]", "views, R refresh"},
	{"?", "this help"},
	{"q", "quit"},
}

// helpLines is the key help, then every source error the fleet collect hit.
func (v fleetView) helpLines() []string {
	out := helpLines(fleetHelp)
	if len(v.fv.Errors) == 0 {
		return out
	}
	out = append(out, "", section("SOURCE ERRORS"))
	for _, e := range v.fv.Errors {
		out = append(out, " "+warnStyle.Render("!")+" "+e)
	}
	return out
}

func (v fleetView) Keys() []key.Binding {
	if v.drilled {
		if v.drill.mode != ovNormal || v.drill.st.filter != "" {
			return v.drill.Keys()
		}
		return append(v.drill.Keys(), key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "fleet")))
	}
	if ks := v.promptKeys(); ks != nil {
		return ks
	}
	return []key.Binding{
		key.NewBinding(key.WithKeys("tab"), key.WithHelp("tab", "focus")),
		key.NewBinding(key.WithKeys("1"), key.WithHelp("1-9", "attention")),
		key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "drill")),
		key.NewBinding(key.WithKeys("L"), key.WithHelp("L", "relaunch")),
		key.NewBinding(key.WithKeys("D"), key.WithHelp("D", "dead")),
		key.NewBinding(key.WithKeys("/"), key.WithHelp("/", "filter")),
	}
}

func (v fleetView) Selection() data.Selection { return nil }

func (v fleetView) Capturing() bool {
	if v.drilled {
		return v.drill.Capturing()
	}
	return v.prompting()
}

// ownsKey keeps tab, the digits and ? for the panels, and hands the drilled
// Overview the keys it owns.
func (v fleetView) ownsKey(msg tea.KeyMsg) bool {
	if v.drilled {
		return v.drill.ownsKey(msg)
	}
	switch s := msg.String(); s {
	case "tab", "shift+tab", "?":
		return true
	default:
		return len(s) == 1 && s[0] >= '1' && s[0] <= '9'
	}
}

func (fleetView) ownsTab() bool { return true }

func (v fleetView) enter(data.Selection) (view, tea.Cmd) {
	return v, tea.EnableMouseCellMotion
}

func (v fleetView) leave() (view, tea.Cmd) {
	v.mode, v.drill.mode = ovNormal, ovNormal
	return v, tea.DisableMouse
}

// bodySize is the last drawn body size, or a default before the first frame.
func (v fleetView) bodySize() (int, int) {
	if v.size.w <= 0 || v.size.h <= 0 {
		return 100, 30
	}
	return v.size.w, v.size.h
}

func (v fleetView) layout() fleetRects {
	w, h := v.bodySize()
	return fleetLayout(w, h, v.fv, v.st)
}

func (v fleetView) sel() fleetSel { return v.layout().sel }

func (v *fleetView) setCursor(i int) {
	v.st.cursor, v.st.cursorAt = v.fv.Attention[i].ID, i
}

// setFocus moves focus to p and pins the highlighted crew as the chosen one,
// so leaving the strip keeps the crew that was drawn.
func (v *fleetView) setFocus(p fleetPanel) {
	if s := v.sel(); s.crew >= 0 {
		v.st.crew = v.fv.Crews[s.crew].Key
	}
	v.st.focus = p
}

// settleFocus makes the drawn focus the stored one before the operator acts.
func (v *fleetView) settleFocus() {
	if f := effectiveFleetFocus(v.fv, v.st.focus); f != v.st.focus {
		v.setFocus(f)
	}
}

func (v *fleetView) handleKey(msg tea.KeyMsg) tea.Cmd {
	if v.drilled {
		return v.drillKey(msg)
	}
	switch v.mode {
	case ovFilter:
		return v.filterKey(msg, &v.st.filter)
	case ovReply:
		return v.replyKey(msg)
	case ovConfirm:
		return v.confirmKey(msg)
	case ovHelp:
		if s := msg.String(); s == "esc" || s == "?" || s == "enter" {
			v.mode = ovNormal
		}
		return nil
	}
	v.settleFocus()
	switch s := msg.String(); s {
	case "tab":
		v.cycle(1)
	case "shift+tab":
		v.cycle(-1)
	case "j", "down":
		v.move(1)
	case "k", "up":
		v.move(-1)
	case "enter":
		return v.enterKey()
	case "esc":
		v.st.filter = ""
	case "/":
		return v.openFilter(v.st.filter)
	case "?":
		v.mode = ovHelp
	case "L":
		return v.relaunchSelected()
	case "D":
		v.st.showDead = !v.st.showDead
	default:
		if len(s) == 1 && s[0] >= '1' && s[0] <= '9' {
			return v.digit(int(s[0] - '0'))
		}
	}
	return nil
}

// drillKey forwards to the drilled Overview; esc returns to the panels only
// when the Overview has no overlay, prompt or filter of its own to close.
func (v *fleetView) drillKey(msg tea.KeyMsg) tea.Cmd {
	if msg.String() == "esc" && v.drill.mode == ovNormal && v.drill.st.filter == "" {
		v.drilled = false
		return nil
	}
	nv, cmd := v.drill.Update(msg)
	v.drill = nv.(overviewView)
	return cmd
}

// cycle moves focus through the panels, skipping the strip when it is empty.
func (v *fleetView) cycle(d int) {
	order := []fleetPanel{fpCrews, fpCapacity, fpHolds, fpHosts}
	if len(v.fv.Attention) > 0 {
		order = append([]fleetPanel{fpAttention}, order...)
	}
	i := slices.Index(order, v.st.focus)
	v.setFocus(order[(i+d+len(order))%len(order)])
}

// step moves from cur through rows by d, entering at the near end when cur
// is not among them.
func step(rows []int, cur, d int) int {
	pos := slices.Index(rows, cur)
	switch {
	case pos < 0 && d < 0:
		pos = len(rows) - 1
	case pos < 0:
		pos = 0
	default:
		pos = clamp(pos+d, 0, len(rows)-1)
	}
	return rows[pos]
}

func indexes(n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = i
	}
	return out
}

// move steps the focused panel's selection from what is drawn; the strip
// scrolls like the Overview's and Capacity, with no rows to select, scrolls.
func (v *fleetView) move(d int) {
	r := v.layout()
	switch v.st.focus {
	case fpAttention:
		c := clamp(r.sel.cursor+d, 0, len(v.fv.Attention)-1)
		if c == r.sel.cursor {
			return
		}
		v.st.stripOff = r.stripOff + d
		v.setCursor(c)
		v.st.stripOff = v.layout().stripOff
	case fpCrews:
		if vis := v.st.index(v.fv).visible(); len(vis) > 0 {
			v.st.crew = v.fv.Crews[step(vis, r.sel.crew, d)].Key
		}
	case fpHolds:
		if n := len(v.fv.Holds); n > 0 {
			v.st.hold = v.fv.Holds[step(indexes(n), r.sel.hold, d)].ID
		}
	case fpHosts:
		if n := len(v.fv.Hosts); n > 0 {
			v.st.host = v.fv.Hosts[step(indexes(n), r.sel.host, d)].Name
		}
	case fpCapacity:
		maxOff := max0(len(capacityEntries(v.fv)) - r.panels[fpCapacity].inner.h)
		v.st.capOff = clamp(clamp(v.st.capOff, 0, maxOff)+d, 0, maxOff)
	}
}

// enterKey drills into the selected crew, or the strip cursor's crew.
func (v *fleetView) enterKey() tea.Cmd {
	s := v.sel()
	switch v.st.focus {
	case fpAttention:
		k := strOf(v.fv.Attention[s.cursor].CrewKey)
		if k == "" {
			return statusCmd("no crew for this item", true)
		}
		return v.openDrill(k)
	case fpCrews:
		if s.crew < 0 {
			return statusCmd("no crew selected", true)
		}
		return v.openDrill(v.fv.Crews[s.crew].Key)
	}
	return nil
}

func (v *fleetView) openDrill(crewKey string) tea.Cmd {
	in, ok := vm.CrewInputs(v.in, crewKey)
	if !ok {
		return statusCmd("crew gone", true)
	}
	v.crumb = crewKey
	if i := slices.IndexFunc(v.fv.Crews, func(c vm.FleetCrewRow) bool { return c.Key == crewKey }); i >= 0 {
		c := v.fv.Crews[i]
		v.crumb = strOf(c.Repo) + " " + c.Crew
		if c.Origin == vm.OriginPeer {
			v.crumb = "@" + c.Host + " " + c.Crew
		}
	}
	v.drill = newOverviewView(in, v.now)
	v.drill.acts = v.acts
	v.drilled, v.drillCrew, v.st.crew = true, crewKey, crewKey
	return nil
}

// digit acts on the attention row labelled n in the strip as drawn, moving
// the cursor onto it.
func (v *fleetView) digit(n int) tea.Cmd {
	r := v.layout()
	if n > r.stripRows {
		return statusCmd("no attention item "+strconv.Itoa(n), true)
	}
	it := r.panels[fpAttention].items[n-1]
	v.setCursor(it.idx)
	a := v.fv.Attention[it.idx]
	switch {
	case a.Action == nil && a.Origin == vm.OriginPeer:
		return statusCmd("peer worker: read-only", true)
	case a.Action == nil:
		return statusCmd("no action", true)
	case a.Action.Verb == "relaunch":
		repo := ""
		if i := slices.IndexFunc(v.fv.Crews, func(c vm.FleetCrewRow) bool { return c.Key == strOf(a.CrewKey) }); i >= 0 {
			repo = strOf(v.fv.Crews[i].Repo)
		}
		v.openRelaunch(*a.Action, a.Crew, repo)
		return nil
	case a.Action.Key == "":
		return statusCmd("run: "+shownCommand(*a.Action), false)
	}
	t := ovTarget{origin: a.Origin, host: a.Host, branch: a.Branch, name: vm.NudgeTarget(a.Worker, a.Branch)}
	return v.dispatch(*a.Action, t)
}

// relaunchSelected confirms relaunching the selected crew row's dispatcher,
// which only an orphaned local crew offers.
func (v *fleetView) relaunchSelected() tea.Cmd {
	s := v.sel()
	if s.crew < 0 || (v.st.focus != fpCrews && v.st.focus != fpAttention) {
		return statusCmd("no relaunch for this crew", true)
	}
	c := v.fv.Crews[s.crew]
	i := slices.IndexFunc(c.Actions, func(a vm.Action) bool { return a.Verb == "relaunch" })
	if !c.Orphaned || c.Origin == vm.OriginPeer || i < 0 {
		return statusCmd("no relaunch for this crew", true)
	}
	v.openRelaunch(c.Actions[i], c.Crew, strOf(c.Repo))
	return nil
}

func (v *fleetView) openRelaunch(a vm.Action, crewID, repo string) {
	v.openConfirm(a, ovTarget{origin: vm.OriginLocal, name: crewID}, "relaunch crew "+crewID+" ("+repo+")? y/N")
}

func (v *fleetView) click(x, y int) {
	r := v.layout()
	hit := func(rc rect) bool { return x >= rc.x && x < rc.x+rc.w && y >= rc.y && y < rc.y+rc.h }
	for p := fpAttention; p < numFleetPanels; p++ {
		pl := r.panels[p]
		if !pl.shown || !hit(pl.rect) {
			continue
		}
		v.setFocus(p)
		for _, it := range pl.items {
			if !hit(it.rect) {
				continue
			}
			switch it.kind {
			case eAttn:
				v.setCursor(it.idx)
			case eCrew:
				v.st.crew = v.fv.Crews[it.idx].Key
			case eHold:
				v.st.hold = v.fv.Holds[it.idx].ID
			case eHost:
				v.st.host = v.fv.Hosts[it.idx].Name
			}
			return
		}
		return
	}
}
