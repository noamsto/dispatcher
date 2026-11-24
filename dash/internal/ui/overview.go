package ui

import (
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/noamsto/dispatcher/dash/internal/data"
	vm "github.com/noamsto/dispatcher/dash/internal/view"
)

// ovMode is what owns the keyboard on the Overview.
type ovMode int

const (
	ovNormal ovMode = iota
	ovDetail
	ovHelp
	ovReply
	ovConfirm
	ovFilter
)

// ovTarget is the worker (or attention item) an action acts on.
type ovTarget struct {
	origin vm.Origin
	host   string
	branch string
	name   string // what the operator calls it: name, else branch
}

// ovPending is the reply or confirm that is open: the exact argv to run and
// the confirm prompt.
type ovPending struct {
	act    vm.Action
	target ovTarget
	prompt string
}

// ovSize is the body size of the last View, shared by every copy of the view
// so mouse hit-testing and scrolling use the geometry that was drawn.
type ovSize struct{ w, h int }

// overviewView is the Overview tab. It holds the latest view model, rebuilt
// from the root's inputs on every snapshot and on the 1 s tick so ages stay
// current.
type overviewView struct {
	now   func() time.Time
	in    vm.Inputs
	model vm.OverviewView
	st    overviewState
	pills pillMode

	actionPrompt
	size *ovSize
}

// actionPrompt is the action dispatch with its reply, confirm and filter
// prompts, shared by the Overview and the Fleet tab: one mode owns the
// keyboard, and every argv runs in its action's directory.
type actionPrompt struct {
	acts  actionDeps
	mode  ovMode
	input textinput.Model
	pend  ovPending
	insp  inspectState
}

func newOverviewView(in vm.Inputs, now func() time.Time) overviewView {
	v := overviewView{now: now, in: in, pills: pillModeOf(os.Getenv, lipgloss.ColorProfile()), size: &ovSize{}}
	v.rebuild()
	return v
}

func (v overviewView) Update(msg tea.Msg) (view, tea.Cmd) {
	switch msg := msg.(type) {
	case snapshotMsg:
		v.in = msg.in
		v.rebuild()
	case tickMsg:
		v.in.NowMS = v.now().UnixMilli()
		v.rebuild()
	case inspectMsg:
		v.applyInspect(msg)
	case tea.WindowSizeMsg:
		*v.size = ovSize{msg.Width, msg.Height}
	case tea.MouseMsg:
		if v.mode == ovNormal && msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonLeft {
			v.click(msg.X, msg.Y)
		}
	case tea.KeyMsg:
		cmd := v.handleKey(msg)
		return v, cmd
	default:
		if v.mode == ovReply || v.mode == ovFilter {
			var cmd tea.Cmd
			v.input, cmd = v.input.Update(msg)
			return v, cmd
		}
	}
	return v, nil
}

// rebuild refreshes the view model, cleaned once for the terminal. It writes
// no selection: the state holds intent, which the layout resolves per frame.
func (v *overviewView) rebuild() {
	v.model = cleanView(vm.BuildOverview(v.in))
}

func (v overviewView) View(w, h int) string {
	*v.size = ovSize{w, h}
	base := renderOverview(w, h, v.model, v.st, v.pills)
	if v.mode == ovNormal || w <= 0 || h <= 0 {
		return base
	}
	return v.overlay(strings.Split(base, "\n"), w, h)
}

// promptKeys is the help bar while a prompt is open, else nil.
func (p actionPrompt) promptKeys() []key.Binding {
	switch p.mode {
	case ovReply, ovFilter:
		return []key.Binding{
			key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", map[ovMode]string{ovReply: "send", ovFilter: "keep"}[p.mode])),
			key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", map[ovMode]string{ovReply: "cancel", ovFilter: "clear"}[p.mode])),
		}
	case ovConfirm:
		return []key.Binding{
			key.NewBinding(key.WithKeys("y"), key.WithHelp("y", "confirm")),
			key.NewBinding(key.WithKeys("n", "esc"), key.WithHelp("n/esc", "cancel")),
		}
	}
	return nil
}

// prompting is true while a reply, confirm or filter prompt owns the keys.
func (p actionPrompt) prompting() bool {
	return p.mode == ovReply || p.mode == ovConfirm || p.mode == ovFilter
}

func (v overviewView) Keys() []key.Binding {
	if ks := v.promptKeys(); ks != nil {
		return ks
	}
	return []key.Binding{
		key.NewBinding(key.WithKeys("h", "j", "k", "l", "left", "down", "up", "right"), key.WithHelp("hjkl", "move")),
		key.NewBinding(key.WithKeys("tab"), key.WithHelp("tab", "focus")),
		key.NewBinding(key.WithKeys("1"), key.WithHelp("1-9", "attention")),
		key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "detail")),
		key.NewBinding(key.WithKeys("p"), key.WithHelp("p/n/r/m", "peek/nudge/reply/merge")),
		key.NewBinding(key.WithKeys("/"), key.WithHelp("/", "filter")),
	}
}

func (overviewView) Selection() data.Selection { return nil }

func (v overviewView) Capturing() bool { return v.prompting() }

// ownsKey keeps the global switching, refresh and help keys for the Overview's
// own use.
func (overviewView) ownsKey(msg tea.KeyMsg) bool {
	switch s := msg.String(); s {
	case "tab", "shift+tab", "r", "?":
		return true
	default:
		return len(s) == 1 && s[0] >= '1' && s[0] <= '9'
	}
}

func (overviewView) ownsTab() bool { return true }

func (v overviewView) enter(data.Selection) (view, tea.Cmd) {
	return v, tea.EnableMouseCellMotion
}

func (v overviewView) leave() (view, tea.Cmd) {
	v.mode = ovNormal
	return v, tea.DisableMouse
}

// bodySize is the last drawn body size, or a default before the first frame.
func (v overviewView) bodySize() (int, int) {
	if v.size.w <= 0 || v.size.h <= 0 {
		return 100, 30
	}
	return v.size.w, v.size.h
}

func (v overviewView) layout() overviewRects {
	w, h := v.bodySize()
	return overviewLayout(w, h, v.model, v.st)
}

func (v overviewView) sel() ovSel { return v.layout().sel }

func (v overviewView) target() (vm.OverviewWorker, bool) {
	s := v.sel()
	if s.worker < 0 {
		return vm.OverviewWorker{}, false
	}
	return v.model.Workers[s.worker], true
}

// setFocus moves focus to p and pins the highlighted worker as the chosen one,
// so changing panels (leaving the strip above all) keeps what was drawn.
func (v *overviewView) setFocus(p ovPanel) {
	if s := v.sel(); s.worker >= 0 {
		v.setWorker(s.worker)
	}
	v.st.focus = p
	if fullWidth(p) {
		v.st.ret, v.st.retWorker = 0, workerKey{}
	}
}

// settleFocus makes the drawn focus the stored one before the operator acts: a
// strip that emptied has handed focus to the checklists, and it stays there
// when items come back.
func (v *overviewView) settleFocus() {
	f := effectiveFocus(v.model, v.st.focus)
	if f == panelEvents && !v.layout().panels[panelEvents].shown {
		f = panelChecklists
	}
	if f != v.st.focus {
		v.setFocus(f)
	}
}

func (v *overviewView) setCursor(i int) {
	v.st.cursor, v.st.cursorAt = v.model.Attention[i].ID, i
}

func (v *overviewView) setWorker(i int) {
	w := v.model.Workers[i]
	v.st.worker = workerKey{w.Crew, w.Branch}
}

func (v *overviewView) handleKey(msg tea.KeyMsg) tea.Cmd {
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
	s := msg.String()
	if v.inspectKey(s) {
		return nil
	}
	if v.mode == ovDetail && (s == "esc" || s == "enter") {
		v.mode = ovNormal
		return nil
	}
	v.settleFocus()
	switch s {
	case "tab":
		v.cycle(1)
	case "shift+tab":
		v.cycle(-1)
	case "h", "left":
		v.hop(-1)
	case "l", "right":
		v.hop(1)
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
	case "p", "n", "r", "m":
		return v.actOnSelected(s)
	default:
		if len(s) == 1 && s[0] >= '1' && s[0] <= '9' {
			return v.digit(int(s[0] - '0'))
		}
	}
	return nil
}

func newOvInput(prompt, value string) textinput.Model {
	in := textinput.New()
	in.Prompt = prompt
	in.SetValue(value)
	in.CursorEnd()
	in.Focus()
	return in
}

// cycle moves focus through the body panels in the order the layout shows
// them, whether or not they are drawn (focusing one draws it), plus the strip
// and Recent Events while they are drawn.
func (v *overviewView) cycle(d int) {
	r := v.layout()
	order := []ovPanel{panelChecklists, panelAgents, panelMonitors}
	if r.narrow {
		order = []ovPanel{panelAgents, panelChecklists, panelMonitors}
	}
	if r.panels[panelAttention].shown {
		order = slices.Insert(order, 0, panelAttention)
	}
	if r.panels[panelEvents].shown {
		order = append(order, panelEvents)
	}
	i := slices.Index(order, v.st.focus)
	v.setFocus(order[(i+d+len(order))%len(order)])
}

// hop moves focus to the panel drawn left or right of the focused one, if any.
func (v *overviewView) hop(d int) {
	if to, ok := v.layout().neighbour(v.st.focus, d, 0); ok {
		v.setFocus(to)
	}
}

// fullWidth panels span the row or the stack, so leaving one returns to the
// panel it was entered from.
func fullWidth(p ovPanel) bool { return p == panelAttention || p == panelEvents }

// retPanel is the body panel the focused full-width panel was entered from, if
// it is still drawn.
func (v *overviewView) retPanel(r overviewRects) (ovPanel, bool) {
	p := v.st.ret
	return p, (p == panelChecklists || p == panelAgents || p == panelMonitors) && r.panels[p].shown
}

// cross hands focus to the panel drawn above (d < 0) or below the focused one
// when j or k runs off its edge. Entering a full-width panel remembers the
// panel and row left; leaving one returns there.
func (v *overviewView) cross(r overviewRects, d int) {
	if v.mode != ovNormal {
		return
	}
	from := v.st.focus
	if fullWidth(from) {
		to, ok := v.retPanel(r)
		if !ok {
			to, ok = r.neighbour(from, 0, d)
		}
		if !ok {
			return
		}
		rw := v.st.retWorker
		v.setFocus(to)
		if i := newOvIndex(v.model, v.st).pick(rw.crew, rw.branch); i >= 0 {
			v.setWorker(i)
		}
		return
	}
	to, ok := r.neighbour(from, 0, d)
	if !ok {
		return
	}
	rw := workerKey{}
	if r.sel.worker >= 0 {
		w := v.model.Workers[r.sel.worker]
		rw = workerKey{w.Crew, w.Branch}
	}
	v.setFocus(to)
	switch to {
	case panelAttention:
		v.st.ret, v.st.retWorker = from, rw
		v.setCursor(len(v.model.Attention) - 1)
		v.st.stripOff = v.layout().stripOff
	case panelEvents:
		v.st.ret, v.st.retWorker = from, rw
		v.st.evCursor, v.st.evOff = 0, 0
	default:
		if ws := panelWorkers(to, v.model, newOvIndex(v.model, v.st)); len(ws) > 0 {
			v.setWorker(ws[max(0, (len(ws)-1)*(1-d)/2)])
		}
	}
}

// move steps the strip cursor, the Recent Events cursor, or the selected
// worker over the focused panel's rows, from what is drawn; at a panel's edge
// it crosses to the panel in that direction.
func (v *overviewView) move(d int) {
	r := v.layout()
	switch v.st.focus {
	case panelAttention:
		c := clamp(r.sel.cursor+d, 0, len(v.model.Attention)-1)
		if c == r.sel.cursor {
			if d > 0 {
				v.cross(r, d)
			}
			return
		}
		// The window scrolls one row with the cursor until it reaches an end;
		// the layout clamps it and keeps the cursor inside, and that window is
		// the one kept.
		v.st.stripOff = r.stripOff + d
		v.setCursor(c)
		v.st.stripOff = v.layout().stripOff
		return
	case panelEvents:
		c := clamp(r.evCursor+d, 0, len(v.model.Events)-1)
		if r.evCursor < 0 || c == r.evCursor {
			if d < 0 {
				v.cross(r, d)
			}
			return
		}
		v.st.evOff, v.st.evCursor = r.evOff, c
		v.st.evOff = v.layout().evOff
		return
	}
	ws := panelWorkers(v.st.focus, v.model, newOvIndex(v.model, v.st))
	pos := slices.Index(ws, r.sel.worker)
	if len(ws) == 0 || d < 0 && pos == 0 || d > 0 && pos == len(ws)-1 {
		v.cross(r, d)
		return
	}
	switch {
	case pos < 0 && d < 0:
		pos = len(ws) - 1
	case pos < 0:
		pos = 0
	default:
		pos += d
	}
	v.setWorker(ws[pos])
}

// enterKey on the strip hands focus to the checklists with the cursor's worker
// selected, or inspects an item that names no worker. Elsewhere it
// opens the detail overlay on the checklists, where j and k then move.
func (v *overviewView) enterKey() tea.Cmd {
	s := v.sel()
	strip := v.st.focus == panelAttention
	switch {
	case s.worker >= 0:
		v.setFocus(panelChecklists)
		if !strip {
			v.insp, v.mode = inspectState{}, ovDetail
		}
	case strip && inspectable(v.model.Attention[s.cursor]):
		return v.startInspect(v.model.Attention[s.cursor])
	case strip && v.model.Attention[s.cursor].Action != nil:
		return statusCmd("run: "+shownCommand(*v.model.Attention[s.cursor].Action), false)
	}
	return nil
}

// openFilter starts the filter prompt on the current filter text.
func (p *actionPrompt) openFilter(filter string) tea.Cmd {
	p.mode = ovFilter
	p.input = newOvInput("/", filter)
	return textinput.Blink
}

// filterKey edits *filter live: enter keeps it, esc clears it.
func (p *actionPrompt) filterKey(msg tea.KeyMsg, filter *string) tea.Cmd {
	switch msg.String() {
	case "enter":
		p.mode = ovNormal
		return nil
	case "esc", "ctrl+c":
		p.mode = ovNormal
		*filter = ""
		return nil
	}
	var cmd tea.Cmd
	p.input, cmd = p.input.Update(msg)
	*filter = p.input.Value()
	return cmd
}

// digit acts on the attention row labelled n in the strip as drawn, moving the
// cursor, and the selection when the item names a worker, onto it.
func (v *overviewView) digit(n int) tea.Cmd {
	r := v.layout()
	if n > r.stripRows {
		return statusCmd("no attention item "+strconv.Itoa(n), true)
	}
	it := r.panels[panelAttention].items[n-1]
	v.setCursor(it.idx)
	if it.worker >= 0 {
		v.setWorker(it.worker)
	}
	a := v.model.Attention[it.idx]
	if a.Action == nil {
		return statusCmd("peer worker: read-only", true)
	}
	if a.Action.Key == "" {
		if it.worker < 0 && inspectable(a) {
			return v.startInspect(a)
		}
		return statusCmd("run: "+shownCommand(*a.Action), false)
	}
	t := ovTarget{origin: a.Origin, host: a.Host, branch: a.Branch, name: a.Worker}
	if i := newOvIndex(v.model, v.st).worker(a.Crew, a.Branch); i >= 0 {
		t = workerTarget(v.model.Workers[i])
	}
	return v.dispatch(*a.Action, t)
}

func workerTarget(w vm.OverviewWorker) ovTarget {
	return ovTarget{origin: w.Origin, host: w.Host, branch: w.Branch, name: vm.NudgeTarget(strOf(w.Name), w.Branch)}
}

var actionVerbs = map[string]string{"p": "peek", "n": "nudge", "r": "reply", "m": "merge"}

func (v *overviewView) actOnSelected(k string) tea.Cmd {
	w, ok := v.target()
	if !ok {
		return statusCmd(actionVerbs[k]+": no worker selected", true)
	}
	if w.Origin == vm.OriginPeer || len(w.Actions) == 0 {
		return statusCmd("peer worker: read-only", true)
	}
	i := slices.IndexFunc(w.Actions, func(a vm.Action) bool { return a.Key == k })
	if i < 0 {
		reason := "unavailable"
		if k == "m" {
			reason = "no PR"
		}
		return statusCmd(actionVerbs[k]+": "+reason, true)
	}
	return v.dispatch(w.Actions[i], workerTarget(w))
}

func (p *actionPrompt) dispatch(a vm.Action, t ovTarget) tea.Cmd {
	switch a.Key {
	case "p":
		var then func(string) ([]string, error)
		act := actPeek
		if t.origin == vm.OriginLocal && p.acts.inTmux {
			act = actJump
			then = func(out string) ([]string, error) {
				pane, err := parseJumpPane(out)
				if err != nil {
					return nil, err
				}
				return switchArgv(pane), nil
			}
		}
		return runActionCmd(p.acts.ctx, p.acts.runner, act, deref(a.Dir), a.Argv, then)
	case "n":
		p.openConfirm(a, t, "nudge "+cleanText(t.name)+"? y/N")
	case "m":
		if a.PR == nil {
			return statusCmd("merge: no PR", true)
		}
		p.openConfirm(a, t, "merge PR #"+strconv.Itoa(*a.PR)+" ("+cleanText(t.branch)+")? y/N")
	case "r":
		p.mode, p.pend = ovReply, ovPending{act: a, target: t}
		p.input = newOvInput("", "")
		return textinput.Blink
	}
	return nil
}

func (p *actionPrompt) openConfirm(a vm.Action, t ovTarget, prompt string) {
	p.mode, p.pend = ovConfirm, ovPending{act: a, target: t, prompt: prompt}
}

func (p *actionPrompt) confirmKey(msg tea.KeyMsg) tea.Cmd {
	p.mode = ovNormal
	verb := p.pend.act.Verb
	if msg.String() != "y" {
		return statusCmd(verb+": cancelled", false)
	}
	act := actNudge
	switch {
	case p.pend.act.Key == "m":
		act = actMerge
	case verb == "relaunch":
		// dispatcher --relaunch opens the crew's new window in $TMUX.
		if !p.acts.inTmux {
			return statusCmd("relaunch needs tmux", true)
		}
		act = actRelaunch
	}
	return runActionCmd(p.acts.ctx, p.acts.runner, act, deref(p.pend.act.Dir), p.pend.act.Argv, nil)
}

func (p *actionPrompt) replyKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "esc", "ctrl+c":
		p.mode = ovNormal
		return nil
	case "enter":
		text := strings.TrimSpace(p.input.Value())
		if text == "" {
			return statusCmd("reply: text required", true)
		}
		p.mode = ovNormal
		argv := slices.Clone(p.pend.act.Argv)
		for i, a := range argv {
			if a == "<text>" {
				argv[i] = text
			}
		}
		return runActionCmd(p.acts.ctx, p.acts.runner, actReply, deref(p.pend.act.Dir), argv, nil)
	}
	var cmd tea.Cmd
	p.input, cmd = p.input.Update(msg)
	return cmd
}

func (v *overviewView) click(x, y int) {
	r := v.layout()
	in := func(rc rect) bool { return x >= rc.x && x < rc.x+rc.w && y >= rc.y && y < rc.y+rc.h }
	for p := panelAttention; p <= panelEvents; p++ {
		pl := r.panels[p]
		if !pl.shown || !in(pl.rect) {
			continue
		}
		v.setFocus(p)
		for _, it := range pl.items {
			if !in(it.rect) {
				continue
			}
			if p == panelAttention {
				v.setCursor(it.idx)
			} else if p == panelEvents {
				v.st.evCursor, v.st.evOff = it.idx, r.evOff
			} else if it.worker >= 0 {
				v.setWorker(it.worker)
			}
			return
		}
		return
	}
}
