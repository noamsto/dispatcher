package ui

import (
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/noamsto/dispatcher/dash/internal/data"
	"github.com/noamsto/dispatcher/dash/internal/kitty"
	"github.com/noamsto/dispatcher/dash/internal/mapgeom"
)

// MapDeps are the terminal seams the Map tab needs. TTY receives the kitty
// store and delete sequences (the controlling terminal, never bubbletea's
// stdout); CellPx is the terminal's cell size in pixels; Capable says whether
// the terminal can show the image and, when not, why.
type MapDeps struct {
	TTY     io.Writer
	CellPx  func() (w, h int)
	Capable func() (ok bool, why string)
	OutPath string // the PNG crew renders into
	IDBase  int    // kitty.ID; the view alternates IDBase+1 and IDBase+2
}

// deleteImages removes both image ids from the terminal, so none outlives the
// program.
func (d MapDeps) deleteImages(inTmux bool) {
	if d.TTY == nil {
		return
	}
	if ok, _ := d.Capable(); !ok {
		return
	}
	for _, id := range []int{d.IDBase + 1, d.IDBase + 2} {
		io.WriteString(d.TTY, kitty.Delete(id, inTmux))
	}
}

// mapInfoLines is the info line plus the counts line under the image block.
const mapInfoLines = 2

// mapRenderedMsg is one `crew roster-render --map` result. gen ties it to the
// request that started it; place is the block the image was rendered for.
type mapRenderedMsg struct {
	gen    int
	crewID string
	place  mapgeom.Placement
	geom   mapgeom.Geometry
	err    error
}

// mapView is the roster as a kitty image: one planet per worker, drawn by
// crew, selectable by key or click. The PNG is stored on the terminal and the
// frame carries placeholder cells that reference it.
type mapView struct {
	actionUI

	deps MapDeps
	snap data.Snapshot

	active bool // the tab is showing; renders only run then
	shown  bool // a crew has been chosen
	crewID string
	sel    string // branch of the selected planet; follows the worker across renders

	boxW, boxH int // body cells reserved for the view, from the root

	geom   mapgeom.Geometry
	place  mapgeom.Placement // the block geom was rendered for
	stored bool              // an image is on the terminal
	curID  int
	paint  bool // the next transmit needs a full repaint
	err    string

	gen      int
	inFlight bool
	pending  bool // something changed while a render was in flight
	reqPlace mapgeom.Placement
	sig      string // roster signature of the latest request
}

func newMapView(snap data.Snapshot, deps MapDeps, acts actionDeps) mapView {
	v := mapView{deps: deps, snap: snap}
	v.acts = acts
	return v
}

func (v mapView) capable() (bool, string) {
	if v.deps.Capable == nil {
		return false, "no terminal image support configured"
	}
	return v.deps.Capable()
}

// ownsTab is true while the planets are on screen, so tab and shift+tab move
// the selection instead of switching views.
func (v mapView) ownsTab() bool { return v.stored && len(v.geom.Planets) > 0 }

// enter shows the tab. The first time, it opens on the crew of the Live
// cursor's worker.
func (v mapView) enter(live data.Selection) (view, tea.Cmd) {
	v.active, v.err, v.paint = true, "", true
	if ok, _ := v.capable(); !ok {
		return v, nil
	}
	if !v.shown {
		w, _ := live.(data.Worker)
		v.setCrew(v.pickCrew(w.Crew))
		if v.crewID == w.Crew {
			v.sel = w.Branch
		}
		v.shown = true
	}
	cmd := v.request()
	return v, tea.Batch(tea.EnableMouseCellMotion, cmd)
}

// leave hides the tab and removes its image; a render still in flight lands
// stale.
func (v mapView) leave() (view, tea.Cmd) {
	v.active, v.pending = false, false
	v.gen++
	v.drop()
	if ok, _ := v.capable(); !ok {
		return v, nil
	}
	return v, tea.DisableMouse
}

// resize records the body cells the root reserves and re-renders when the
// image block they fit has changed.
func (v mapView) resize(w, h int) (mapView, tea.Cmd) {
	if w == v.boxW && h == v.boxH {
		return v, nil
	}
	v.boxW, v.boxH, v.width = w, h, w
	v.fit()
	if !v.active {
		return v, nil
	}
	if ok, _ := v.capable(); !ok || v.placement() == v.reqPlace {
		return v, nil
	}
	cmd := v.request()
	return v, cmd
}

func (v mapView) Update(msg tea.Msg) (view, tea.Cmd) {
	switch msg := msg.(type) {
	case snapshotMsg:
		v.snap = msg.snap
		if v.shown && !slices.Contains(v.crewIDs(), v.crewID) {
			v.setCrew(v.pickCrew(""))
			cmd := v.request()
			return v, cmd
		}
		if v.active && v.crewID != "" && v.signature() != v.sig {
			cmd := v.request()
			return v, cmd
		}
		return v, nil

	case mapRenderedMsg:
		v.inFlight = false
		var cmd tea.Cmd
		if msg.gen == v.gen {
			cmd = v.accept(msg)
		}
		if v.pending {
			next := v.request()
			return v, tea.Batch(cmd, next)
		}
		return v, cmd

	case tea.MouseMsg:
		if msg.Action != tea.MouseActionPress || msg.Button != tea.MouseButtonLeft || !v.stored || v.mode != modeNormal {
			return v, nil
		}
		left, top := v.origin()
		x, y := msg.X-left, msg.Y-top
		cols, rows := v.visible(v.boxW)
		if x >= cols || y >= rows {
			return v, nil
		}
		i := mapgeom.Hit(v.geom, v.place, x, y)
		if i < 0 {
			return v, nil
		}
		cmd := v.selectPlanet(i)
		return v, cmd

	case tea.KeyMsg:
		if cmd, ok := v.actionUI.handleKey(msg); ok {
			return v, cmd
		}
		return v.handleKey(msg)
	}
	return v, nil
}

func (v mapView) handleKey(msg tea.KeyMsg) (view, tea.Cmd) {
	switch msg.String() {
	case "right", "down", "l", "j", "tab":
		cmd := v.step(1)
		return v, cmd
	case "left", "up", "h", "k", "shift+tab":
		cmd := v.step(-1)
		return v, cmd
	case "c":
		ids := v.crewIDs()
		if len(ids) < 2 {
			return v, nil
		}
		v.setCrew(ids[(slices.Index(ids, v.crewID)+1)%len(ids)])
		cmd := v.request()
		return v, cmd
	case "t":
		return v.startAction(actJump)
	case "o":
		return v.startAction(actOpenPR)
	case "m":
		return v.startAction(actReply)
	case "n":
		return v.startAction(actNudge)
	}
	return v, nil
}

// startAction runs the action on the selected planet's worker. A planet the
// roster no longer lists has no worker to act on.
func (v mapView) startAction(a action) (view, tea.Cmd) {
	w, ok := v.worker()
	if !ok {
		reason := "worker not in roster"
		if v.sel == "" {
			reason = "no worker selected"
		}
		return v, statusCmd(a.String()+": "+reason, true)
	}
	cmd := v.actionUI.start(a, w, true)
	return v, cmd
}

func (v mapView) worker() (data.Worker, bool) {
	return workerByBranch(v.snap, v.crewID, v.sel)
}

func (v mapView) selIndex() int {
	return slices.IndexFunc(v.geom.Planets, func(p mapgeom.Planet) bool { return p.Branch == v.sel })
}

// step moves the selection d planets along the slot order, wrapping.
func (v *mapView) step(d int) tea.Cmd {
	n := len(v.geom.Planets)
	if n == 0 {
		return nil
	}
	cur := v.selIndex()
	if cur < 0 && d < 0 {
		cur = 0
	}
	return v.selectPlanet(((cur+d)%n + n) % n)
}

func (v *mapView) selectPlanet(i int) tea.Cmd {
	branch := v.geom.Planets[i].Branch
	if branch == v.sel {
		return nil
	}
	v.sel = branch
	return v.request()
}

// crewIDs lists the crews that have workers to draw.
func (v mapView) crewIDs() []string {
	var ids []string
	for _, c := range v.snap.Roster.Crews {
		if c.WorkersError == nil && len(c.Workers) > 0 {
			ids = append(ids, c.ID)
		}
	}
	return ids
}

// pickCrew is prefer when it has workers, else the first crew that does.
func (v mapView) pickCrew(prefer string) string {
	ids := v.crewIDs()
	if slices.Contains(ids, prefer) {
		return prefer
	}
	if len(ids) > 0 {
		return ids[0]
	}
	return ""
}

// setCrew switches the displayed crew, dropping everything drawn for the last.
func (v *mapView) setCrew(id string) {
	if id == v.crewID {
		return
	}
	v.crewID, v.sel, v.err = id, "", ""
	v.gen++
	v.drop()
}

// signature is what the image shows of the displayed crew: every worker's
// branch, state, name, PR and host. Timestamps and detail text change without
// moving a planet or its colour, so they stay out.
func (v mapView) signature() string {
	var rows []string
	for _, c := range v.snap.Roster.Crews {
		if c.ID != v.crewID || c.WorkersError != nil {
			continue
		}
		for _, w := range c.Workers {
			var f []string
			for _, k := range []string{"branch", "state", "name", "pr_url", "host"} {
				s, _ := strField(w, k)
				f = append(f, s)
			}
			rows = append(rows, strings.Join(f, "\x00"))
		}
	}
	slices.Sort(rows)
	return strings.Join(rows, "\n")
}

// placement is the image block that fits the reserved box under the info
// lines.
func (v mapView) placement() mapgeom.Placement {
	cw, ch := v.deps.CellPx()
	return mapgeom.Fit(v.boxW, v.boxH-mapInfoLines, cw, ch)
}

// request starts a render of the displayed crew with the current selection and
// size. Only one render runs at a time: a request while one is in flight is
// remembered and re-run, with the latest state, when it lands.
func (v *mapView) request() tea.Cmd {
	if !v.active || v.crewID == "" || v.boxH < mapInfoLines+1 || v.boxW < 1 {
		return nil
	}
	if ok, _ := v.capable(); !ok {
		return nil
	}
	if v.inFlight {
		v.pending = true
		return nil
	}
	v.inFlight, v.pending = true, false
	v.gen++
	v.reqPlace, v.sig = v.placement(), v.signature()
	return v.renderCmd(v.gen, v.crewID, v.sel, v.reqPlace)
}

func (v mapView) renderCmd(gen int, crewID, sel string, p mapgeom.Placement) tea.Cmd {
	argv := append(slices.Clone(v.acts.crew), "roster-render", "--map", "--crew", crewID,
		"--px", fmt.Sprintf("%dx%d", p.PxW(), p.PxH()), "--out", v.deps.OutPath)
	if sel != "" {
		argv = append(argv, "--select", sel)
	}
	ctx, runner := v.acts.ctx, v.acts.runner
	return func() tea.Msg {
		msg := mapRenderedMsg{gen: gen, crewID: crewID, place: p}
		out, err := runArgv(ctx, runner, argv)
		if err != nil {
			msg.err = err
			return msg
		}
		msg.geom, msg.err = mapgeom.Parse([]byte(out))
		return msg
	}
}

// accept stores the rendered image on the terminal under the id not on screen.
// The id is deleted first because it still holds the image from two renders
// ago. The placeholder cells' colour carries the id, so the swap repaints just
// those lines; only the first image after entering needs a full repaint.
func (v *mapView) accept(msg mapRenderedMsg) tea.Cmd {
	if msg.err != nil {
		v.err, v.sig = cleanText(msg.err.Error()), ""
		v.drop()
		return nil
	}
	id := v.deps.IDBase + 1
	if v.curID == id {
		id++
	}
	inTmux := v.acts.inTmux
	io.WriteString(v.deps.TTY, kitty.Delete(id, inTmux))
	io.WriteString(v.deps.TTY, kitty.Store(id, v.deps.OutPath, msg.place.Cols, msg.place.Rows, inTmux))
	v.geom, v.place, v.curID, v.stored, v.err = msg.geom, msg.place, id, true, ""

	// The selection may have left the crew since the render started; the next
	// render redraws the ring on a planet that is there.
	if v.selIndex() < 0 && len(v.geom.Planets) > 0 {
		v.sel = v.geom.Planets[0].Branch
		v.pending = true
	}
	if v.paint {
		v.paint = false
		return tea.ClearScreen
	}
	return nil
}

// drop removes every image this view put on the terminal.
func (v *mapView) drop() {
	if !v.stored {
		return
	}
	for _, id := range []int{v.deps.IDBase + 1, v.deps.IDBase + 2} {
		io.WriteString(v.deps.TTY, kitty.Delete(id, v.acts.inTmux))
	}
	v.stored, v.curID, v.geom = false, 0, mapgeom.Geometry{}
}

// origin is the body cell of the image block's top-left corner: the block is
// centred in the reserved box above the info lines.
func (v mapView) origin() (left, top int) {
	return max0(v.boxW-v.place.Cols) / 2, max0(v.boxH-mapInfoLines-v.place.Rows) / 2
}

// visible is how many cells of the stored block fit the box of width w. After a
// shrink the old block outlives the new box until the re-render lands; fewer
// cells just crop the stored image.
func (v mapView) visible(w int) (cols, rows int) {
	left, top := v.origin()
	return min(v.place.Cols, max0(w-left)), min(v.place.Rows, max0(v.boxH-mapInfoLines-top))
}

func (v mapView) Keys() []key.Binding {
	if keys := v.promptKeys(); keys != nil {
		return keys
	}
	if ok, _ := v.capable(); !ok {
		return nil
	}
	selectHelp := "←/→"
	if v.ownsTab() {
		selectHelp = "←/→/tab"
	}
	keys := []key.Binding{
		key.NewBinding(key.WithKeys("left", "right", "h", "l"), key.WithHelp(selectHelp, "select")),
		key.NewBinding(key.WithKeys("t"), key.WithHelp("t", "jump")),
		key.NewBinding(key.WithKeys("o"), key.WithHelp("o", "open PR")),
		key.NewBinding(key.WithKeys("m"), key.WithHelp("m", "reply")),
		key.NewBinding(key.WithKeys("n"), key.WithHelp("n", "nudge")),
	}
	if len(v.crewIDs()) > 1 {
		keys = append(keys, key.NewBinding(key.WithKeys("c"), key.WithHelp("c", "crew")))
	}
	return keys
}

func (v mapView) Selection() data.Selection {
	if w, ok := v.worker(); ok {
		return w
	}
	return nil
}

// Capturing is true while the reply prompt or nudge confirm owns the keys.
func (v mapView) Capturing() bool { return v.mode != modeNormal }

func (v mapView) View(w, h int) string {
	if ok, why := v.capable(); !ok {
		return mapMessage(w, "Map unavailable: "+why)
	}
	switch {
	case v.err != "":
		return mapMessage(w, "Map unavailable: render failed: "+v.err)
	case v.crewID == "":
		return truncateLine("no workers to map", w)
	case v.boxH < mapInfoLines+1:
		return truncateLine("terminal too small for the map", w)
	case !v.stored:
		return truncateLine("rendering the map…", w)
	}

	left, top := v.origin()
	cols, rows := v.visible(w)
	lines := make([]string, 0, top+rows+mapInfoLines)
	for range top {
		lines = append(lines, "")
	}
	pad := strings.Repeat(" ", left)
	for _, l := range kitty.Placeholders(v.curID, cols, rows) {
		lines = append(lines, padRight(pad+l, w))
	}
	lines = append(lines, truncateLine(v.prompt(v.infoLine()), w), truncateLine(v.countsLine(), w))
	return strings.Join(lines, "\n")
}

// mapMessage is the plain-text view for a terminal that cannot show the image
// or a failed render: no escape sequences, and a way out.
func mapMessage(w int, reason string) string {
	lines := []string{
		reason,
		"The Map tab needs kitty or ghostty.",
		"Under tmux, also run: tmux set -g allow-passthrough on",
		"Press 1 for the Live table.",
	}
	for i, l := range lines {
		lines[i] = truncateLine(cleanText(l), w)
	}
	return strings.Join(lines, "\n")
}

// infoLine describes the selected planet's worker.
func (v mapView) infoLine() string {
	if v.sel == "" {
		return "no workers"
	}
	w, ok := v.worker()
	if !ok {
		return "worker not in roster: " + cleanText(v.sel)
	}
	var parts []string
	for _, s := range []string{w.Name, w.Branch, w.State, prCell(w.PRURL)} {
		if s = cleanText(s); s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, " · ")
}

// countsLine is the displayed crew's workers per state.
func (v mapView) countsLine() string {
	counts := map[string]int{}
	for _, c := range v.snap.Roster.Crews {
		if c.ID != v.crewID || c.WorkersError != nil {
			continue
		}
		for _, w := range c.Workers {
			s, _ := strField(w, "state")
			counts[s]++
		}
	}
	return fmt.Sprintf("crew %s · %d working · %d blocked · %d pr open · %d done",
		cleanText(v.crewID), counts["working"], counts["blocked"], counts["pr_open"], counts["done"])
}
