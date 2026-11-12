package ui

import (
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/noamsto/dispatcher/dash/internal/data"
)

// Card geometry: boardCardW is the outer width, border and padding included;
// the content is boardCardW minus the frame.
const (
	boardCardW   = 28
	boardGap     = 1
	boardPadX    = 1
	boardContent = boardCardW - 2 - 2*boardPadX
)

// Board column groups, in display order.
const (
	grpWorking = iota
	grpBlocked
	grpPROpen
	grpDone
	numGroups
)

var groupTitles = [numGroups]string{"working", "blocked", "pr_open", "done/failed"}

// boardGroup maps a worker state to its column; a state this build does not
// know draws as working, like the SVG roster.
func boardGroup(state string) int {
	switch state {
	case "blocked":
		return grpBlocked
	case "pr_open":
		return grpPROpen
	case "done", "failed", "exited":
		return grpDone
	}
	return grpWorking
}

func stateColor(state string) lipgloss.Color {
	switch state {
	case "failed", "exited":
		return stateFailedColor
	}
	switch boardGroup(state) {
	case grpBlocked:
		return stateBlockedColor
	case grpPROpen:
		return statePROpenColor
	case grpDone:
		return stateDoneColor
	}
	return stateWorkingColor
}

// cardFields is the text of one worker's card before it is cut to fit. It
// follows the SVG card (crew/internal/rosterrender/card.go cardTextOf); the
// two modules cannot share code, so board_test.go and card_test.go assert the
// same fixture against both.
type cardFields struct {
	head   string // "codename · #N"
	state  string
	title  string
	meta   string // "tier·engine·model"
	tier   string
	host   string
	detail string
}

var (
	issueRe  = regexp.MustCompile(`[0-9]+`)
	foldIDRe = regexp.MustCompile(`\A(?:[A-Za-z]+-[0-9]+|[0-9]+)\z`)
)

func orQ(s string) string {
	if s == "" {
		return "?"
	}
	return s
}

// modelAlias shortens a model id to its family word or, failing that, to the
// part after the last slash.
func modelAlias(s string) string {
	if i := strings.LastIndex(s, "/"); i >= 0 {
		s = s[i+1:]
	}
	lower := strings.ToLower(s)
	for _, f := range []string{"opus", "sonnet", "haiku", "fable"} {
		if strings.Contains(lower, f) {
			return f
		}
	}
	return s
}

// oneLine is bus-sourced free text with control characters dropped and runs
// of whitespace collapsed.
func oneLine(s string) string { return strings.Join(strings.Fields(cleanText(s)), " ") }

func fieldsOf(w map[string]any) cardFields {
	f := func(k string) string { s, _ := strField(w, k); return s }
	branch := f("branch")
	codename, suffix, _ := strings.Cut(f("name"), "·")
	codename = oneLine(codename)
	if codename == "" {
		codename = "?"
	}
	rest := branch
	if _, r, ok := strings.Cut(rest, "/"); ok {
		rest = r
	}
	issue := issueRe.FindString(rest)
	if issue == "" && foldIDRe.MatchString(suffix) {
		issue = suffix
	}
	head := codename
	if issue != "" {
		head += " · "
		if strings.Trim(issue, "0123456789") == "" {
			head += "#"
		}
		head += issue
	}
	tier := oneLine(f("tier"))
	c := cardFields{
		head:   head,
		state:  strings.ReplaceAll(oneLine(f("state")), "_", " "),
		title:  oneLine(f("title")),
		tier:   tier,
		meta:   orQ(tier) + "·" + orQ(oneLine(f("engine"))) + "·" + orQ(oneLine(modelAlias(f("model")))),
		detail: oneLine(f("detail")),
	}
	if host := f("host"); host != "" {
		if c.host = oneLine(host); c.host == "" {
			c.host = "?"
		}
	}
	return c
}

// tierMarker stands in for the SVG card's border weight, which a terminal
// cannot vary.
func tierMarker(tier string) string {
	switch tier {
	case "deep":
		return "◆"
	case "trivial":
		return "·"
	}
	return "◇"
}

// wrapWords takes the words of s that fit in w cells; a first word wider than
// w is cut. It returns the line and the rest of s.
func wrapWords(s string, w int) (line, rest string) {
	words := strings.Fields(s)
	n := 0
	for n < len(words) {
		next := words[n]
		if line != "" {
			next = line + " " + words[n]
		}
		if lipgloss.Width(next) > w {
			break
		}
		line = next
		n++
	}
	if n == 0 && len(words) > 0 {
		// A first word wider than the card is cut; the words after it go on.
		return truncateLine(words[0], w), strings.Join(words[1:], " ")
	}
	return line, strings.Join(words[n:], " ")
}

// cardRows are the content rows of a card, each exactly boardContent cells.
func cardRows(c cardFields, stateSt lipgloss.Style) []string {
	pad := func(s string) string { return padRight(truncateLine(s, boardContent), boardContent) }

	state := truncateLine(c.state, boardContent/2)
	stateW := lipgloss.Width(state)
	headW := max0(boardContent - stateW - 1)
	head := padRight(truncateLine(c.head, headW), boardContent-stateW) + stateSt.Render(state)

	t1, rest := wrapWords(c.title, boardContent)

	meta := tierMarker(c.tier) + " " + c.meta
	if c.host != "" {
		suffix := truncateLine(" @"+c.host, boardContent/2)
		room := max0(boardContent - lipgloss.Width(suffix))
		meta = truncateLine(meta, room) + suffix
	}

	return []string{
		head,
		pad(t1),
		pad(rest),
		pad(meta),
		warnStyle.Render(pad(c.detail)),
	}
}

// renderCard draws one card at exactly boardCardW cells: a rounded border in
// the state colour, or a double one when selected (visible without colour).
func renderCard(w map[string]any, selected bool) []string {
	c := fieldsOf(w)
	col := stateColor(strFieldOr(w, "state"))
	border := lipgloss.RoundedBorder()
	if selected {
		border = lipgloss.DoubleBorder()
	}
	st := lipgloss.NewStyle().Border(border).BorderForeground(col).Padding(0, boardPadX)
	rows := cardRows(c, lipgloss.NewStyle().Foreground(col))
	return strings.Split(st.Render(strings.Join(rows, "\n")), "\n")
}

func strFieldOr(w map[string]any, k string) string { s, _ := strField(w, k); return s }

// boardItem is one worker card.
type boardItem struct {
	crewID string
	repo   string
	group  int
	w      map[string]any
}

// boardView is the roster as lipgloss task cards, one column per state.
type boardView struct {
	actionUI

	snap   data.Snapshot
	loaded [numSources]bool
	now    func() time.Time

	items  []boardItem // display order: group, repo, then age
	failed []string    // crews whose worker list did not load
	cursor int
	selKey string // rowKey of the selected card; it follows the worker across reloads
}

func newBoardView(snap data.Snapshot, loaded [numSources]bool, now func() time.Time, acts actionDeps) boardView {
	v := boardView{now: now}
	v.acts = acts
	v.load(snap, loaded)
	return v
}

func (v *boardView) load(snap data.Snapshot, loaded [numSources]bool) {
	v.snap = snap
	v.loaded = loaded
	v.setRoster(snap.Roster)
}

func (v *boardView) setRoster(r data.RosterSection) {
	var items []boardItem
	v.failed = nil
	for _, c := range r.Crews {
		if c.WorkersError != nil {
			v.failed = append(v.failed, "crew "+cleanText(c.ID)+": workers unavailable: "+cleanText(*c.WorkersError))
			continue
		}
		for _, w := range c.Workers {
			state, _ := strField(w, "state")
			if state == "reaped" {
				continue
			}
			repo, ok := strField(w, "repo")
			if !ok || repo == "" {
				repo = c.ID
			}
			items = append(items, boardItem{crewID: c.ID, repo: repo, group: boardGroup(state), w: w})
		}
	}
	now := v.now()
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if a.group != b.group {
			return a.group < b.group
		}
		if a.repo != b.repo {
			return a.repo < b.repo
		}
		return workerAge(a.w, now) < workerAge(b.w, now)
	})
	v.items = items

	cursor := clamp(v.cursor, 0, len(items)-1)
	for i, it := range items {
		if rowKey(it.crewID, it.w) == v.selKey {
			cursor = i
			break
		}
	}
	v.moveTo(cursor)
}

func (v *boardView) moveTo(i int) {
	v.cursor = clamp(i, 0, len(v.items)-1)
	v.selKey = ""
	if v.cursor < len(v.items) {
		it := v.items[v.cursor]
		v.selKey = rowKey(it.crewID, it.w)
	}
}

// column is the items of one non-empty group.
type column struct {
	group int
	idx   []int // indexes into items
}

func (v boardView) columns() []column {
	var cols []column
	for i, it := range v.items {
		if n := len(cols); n == 0 || cols[n-1].group != it.group {
			cols = append(cols, column{group: it.group})
		}
		c := &cols[len(cols)-1]
		c.idx = append(c.idx, i)
	}
	return cols
}

// moveColumn moves the selection to the neighbouring column, keeping its
// position in the column.
func (v *boardView) moveColumn(d int) {
	cols := v.columns()
	for ci, c := range cols {
		pos := slices.Index(c.idx, v.cursor)
		if pos < 0 {
			continue
		}
		next := ci + d
		if next < 0 || next >= len(cols) {
			return
		}
		idx := cols[next].idx
		v.moveTo(idx[min(pos, len(idx)-1)])
		return
	}
}

func (v boardView) Update(msg tea.Msg) (view, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		v.width = msg.Width
		v.fit()
	case snapshotMsg:
		v.load(msg.snap, msg.loaded)
	case tea.KeyMsg:
		if cmd, ok := v.actionUI.handleKey(msg); ok {
			return v, cmd
		}
		return v.handleKey(msg)
	}
	return v, nil
}

func (v boardView) handleKey(msg tea.KeyMsg) (view, tea.Cmd) {
	switch msg.String() {
	case "j", "down":
		v.moveTo(v.cursor + 1)
	case "k", "up":
		v.moveTo(v.cursor - 1)
	case "h", "left":
		v.moveColumn(-1)
	case "l", "right":
		v.moveColumn(1)
	case "g":
		v.moveTo(0)
	case "G":
		v.moveTo(len(v.items) - 1)
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

func (v boardView) startAction(a action) (view, tea.Cmd) {
	sel, have := v.Selection().(data.Worker)
	return v, v.start(a, sel, have)
}

func (v boardView) Keys() []key.Binding {
	if keys := v.promptKeys(); keys != nil {
		return keys
	}
	return []key.Binding{
		key.NewBinding(key.WithKeys("h", "j", "k", "l", "left", "down", "up", "right"), key.WithHelp("hjkl", "move")),
		key.NewBinding(key.WithKeys("t"), key.WithHelp("t", "jump")),
		key.NewBinding(key.WithKeys("o"), key.WithHelp("o", "open PR")),
		key.NewBinding(key.WithKeys("m"), key.WithHelp("m", "reply")),
		key.NewBinding(key.WithKeys("n"), key.WithHelp("n", "nudge")),
	}
}

func (v boardView) Selection() data.Selection {
	if v.cursor >= len(v.items) {
		return nil
	}
	it := v.items[v.cursor]
	return workerFromRow(it.crewID, it.w)
}

func (v boardView) Capturing() bool { return v.mode != modeNormal }

// infoLine is the selected worker's crew and branch, the one line below the
// cards.
func (v boardView) infoLine() string {
	if v.cursor >= len(v.items) {
		return ""
	}
	it := v.items[v.cursor]
	branch, _ := strField(it.w, "branch")
	return cleanText(it.crewID + " · " + branch)
}

// groupStates is a representative state per group, for the header colour.
var groupStates = [numGroups]string{"working", "blocked", "pr_open", "done"}

// columnLines draws one group: a header, then its cards, with a dim repo line
// before each repo's run when the board spans several repos. selStart and
// selEnd bound the selected card's lines, -1 when it is not in this group.
func (v boardView) columnLines(c column, multiRepo bool) (lines []string, selStart, selEnd int) {
	selStart, selEnd = -1, -1
	title := groupTitles[c.group] + " (" + strconv.Itoa(len(c.idx)) + ")"
	hdr := lipgloss.NewStyle().Bold(true).Foreground(stateColor(groupStates[c.group])).
		Render(truncateLine(title, boardCardW))
	lines = append(lines, padRight(hdr, boardCardW))

	prev := ""
	for _, i := range c.idx {
		it := v.items[i]
		if multiRepo && it.repo != prev {
			lines = append(lines, padRight(ruleStyle.Render(truncateLine(cleanText(it.repo), boardCardW)), boardCardW))
			prev = it.repo
		}
		card := renderCard(it.w, i == v.cursor)
		if i == v.cursor {
			selStart, selEnd = len(lines), len(lines)+len(card)
		}
		lines = append(lines, card...)
	}
	return lines, selStart, selEnd
}

// View lays the columns side by side when they fit the width, else stacks
// them; either way the selected card is scrolled into view above the info
// line.
func (v boardView) View(w, h int) string {
	switch {
	case !v.loaded[srcRoster]:
		return loadingLine(srcRoster, w)
	case v.snap.Roster.Error != nil:
		return truncateLine("roster unavailable: "+cleanText(*v.snap.Roster.Error), w)
	}

	// Crews whose workers did not load are chrome above the columns, as on
	// Live, so a failed crew never reads as an empty one.
	var top []string
	for _, f := range v.failed {
		top = append(top, truncateLine(warnStyle.Render(f), w))
	}
	// Keep the info/prompt line and a card line on screen.
	if limit := max(h-2, 1); len(top) > limit {
		more := truncateLine(warnStyle.Render("+"+strconv.Itoa(len(top)-limit+1)+" more crews: workers unavailable"), w)
		top = append(top[:limit-1], more)
	}
	switch {
	case len(v.items) == 0 && len(top) == 0:
		return truncateLine("no workers", w)
	case len(v.items) == 0:
		return strings.Join(top, "\n")
	case w < boardCardW:
		return truncateLine("terminal too narrow for the board", w)
	}

	cols := v.columns()
	multiRepo := slices.ContainsFunc(v.items, func(it boardItem) bool { return it.repo != v.items[0].repo })
	side := len(cols)*boardCardW+(len(cols)-1)*boardGap <= w

	var blocks [][]string
	selStart, selEnd, base := 0, 0, 0
	for _, c := range cols {
		lines, s, e := v.columnLines(c, multiRepo)
		blocks = append(blocks, lines)
		if s >= 0 {
			selStart, selEnd = s, e
			if !side {
				selStart, selEnd = base+s, base+e
			}
		}
		base += len(lines) + 1
	}

	var body []string
	if side {
		body = joinColumns(blocks)
	} else {
		for i, b := range blocks {
			if i > 0 {
				body = append(body, "")
			}
			body = append(body, b...)
		}
	}

	room := max0(h - 1 - len(top))
	if len(body) > room {
		off := clamp(selEnd-room, 0, selStart)
		body = body[off:min(len(body), off+room)]
	}
	out := append(top, body...)
	out = append(out, truncateLine(v.prompt(v.infoLine()), w))
	return strings.Join(out, "\n")
}

// joinColumns sets blocks side by side, padding the shorter ones.
func joinColumns(blocks [][]string) []string {
	n := 0
	for _, b := range blocks {
		n = max(n, len(b))
	}
	blank := strings.Repeat(" ", boardCardW)
	out := make([]string, n)
	for i := range out {
		parts := make([]string, len(blocks))
		for j, b := range blocks {
			parts[j] = blank
			if i < len(b) {
				parts[j] = b[i]
			}
		}
		out[i] = strings.Join(parts, strings.Repeat(" ", boardGap))
	}
	return out
}
