package ui

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/noamsto/dispatcher/dash/internal/data"
)

// sortColumns is the ratings table's sort-cycle order and display labels
// (spec §Runs): tier -> engine -> model -> n -> pr% -> success -> burn.
var sortColumns = []string{"tier", "engine", "model", "n", "pr%", "success", "burn"}

// sortRatingsNatural stably sorts an already-decoded RatingsGroups slice
// tier/engine/model ascending — the base order lessRatings' ties fall back
// to.
func sortRatingsNatural(groups []data.RatingGroup) []data.RatingGroup {
	sorted := append([]data.RatingGroup{}, groups...)
	sort.SliceStable(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		if a.Tier != b.Tier {
			return a.Tier < b.Tier
		}
		if a.Engine != b.Engine {
			return a.Engine < b.Engine
		}
		return a.Model < b.Model
	})
	return sorted
}

func aggValue(a data.Agg) float64 {
	if a.Value == nil {
		return math.Inf(-1)
	}
	return *a.Value
}

// lessRatings compares only the named column; ties fall back to the stable
// sort's input order, which is always the tier/engine/model-sorted natural
// slice — so cycling to "engine" or "model" still reads sensibly on ties.
func lessRatings(a, b data.RatingGroup, col string) bool {
	switch col {
	case "tier":
		return a.Tier < b.Tier
	case "engine":
		return a.Engine < b.Engine
	case "model":
		return a.Model < b.Model
	case "n":
		return aggValue(a.N) < aggValue(b.N)
	case "pr%":
		return aggValue(a.PrPct) < aggValue(b.PrPct)
	case "success":
		return aggValue(a.MergePct) < aggValue(b.MergePct)
	case "burn":
		return aggValue(a.BurnMedian) < aggValue(b.BurnMedian)
	}
	return false
}

// runsView is the Runs tab: a ratings table ("a") and a retro-rows list
// ("b"), `f`-focus-switched, each scrolling in its own share of the height;
// `enter` on a list row drills into a scrollable note detail.
type runsView struct {
	ratingsNatural []data.RatingGroup // tier/engine/model ascending — the sort base
	ratings        []data.RatingGroup // ratingsNatural, stably re-sorted per sortCol/desc
	ratingsErr     *string

	rows     []data.RetroRow // newest t0 first
	retroErr *string

	sortCol int
	desc    bool

	focus         int // 0 = ratings table, 1 = runs list
	ratingsCursor int
	rowsCursor    int

	detail       bool
	detailCursor int // index into the open row's Notes
}

func newRunsView(snap data.Snapshot) runsView {
	var v runsView
	v.load(snap)
	return v
}

func (v *runsView) load(snap data.Snapshot) {
	v.ratingsErr = snap.Runs.RatingsError
	if v.ratingsErr == nil {
		v.ratingsNatural = sortRatingsNatural(snap.Runs.RatingsGroups)
	} else {
		v.ratingsNatural = nil
	}
	v.applySort()
	v.ratingsCursor = clamp(v.ratingsCursor, 0, len(v.ratings)-1)

	v.retroErr = snap.Runs.RetroError
	var rows []data.RetroRow
	if v.retroErr == nil && snap.Runs.Retro != nil {
		rows = append([]data.RetroRow{}, snap.Runs.Retro.Rows...)
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].T0 > rows[j].T0 })
	v.rows = rows
	v.rowsCursor = clamp(v.rowsCursor, 0, len(v.rows)-1)
}

// applySort stably re-sorts ratingsNatural by the current column/direction:
// the reversed-argument trick keeps ties in natural order either way.
func (v *runsView) applySort() {
	sorted := append([]data.RatingGroup{}, v.ratingsNatural...)
	col := sortColumns[v.sortCol]
	sort.SliceStable(sorted, func(i, j int) bool {
		if v.desc {
			return lessRatings(sorted[j], sorted[i], col)
		}
		return lessRatings(sorted[i], sorted[j], col)
	})
	v.ratings = sorted
}

func (v runsView) Update(msg tea.Msg) (view, tea.Cmd) {
	switch msg := msg.(type) {
	case snapshotMsg:
		v.load(data.Snapshot(msg))
		return v, nil

	case tea.KeyMsg:
		if v.detail {
			return v.handleDetailKey(msg)
		}
		return v.handleKey(msg)
	}
	return v, nil
}

func (v runsView) handleKey(msg tea.KeyMsg) (view, tea.Cmd) {
	switch msg.String() {
	case "f":
		v.focus = 1 - v.focus
	case "s":
		v.sortCol = (v.sortCol + 1) % len(sortColumns)
		v.applySort()
		v.ratingsCursor = clamp(v.ratingsCursor, 0, len(v.ratings)-1)
	case "o":
		v.desc = !v.desc
		v.applySort()
	case "j", "down":
		v.moveCursor(1)
	case "k", "up":
		v.moveCursor(-1)
	case "g":
		v.setCursor(0)
	case "G":
		if v.focus == 0 {
			v.setCursor(len(v.ratings) - 1)
		} else {
			v.setCursor(len(v.rows) - 1)
		}
	case "enter":
		if v.focus == 1 && v.rowsCursor >= 0 && v.rowsCursor < len(v.rows) {
			v.detail = true
			v.detailCursor = 0
		}
	}
	return v, nil
}

func (v *runsView) moveCursor(delta int) {
	if v.focus == 0 {
		v.ratingsCursor = clamp(v.ratingsCursor+delta, 0, len(v.ratings)-1)
	} else {
		v.rowsCursor = clamp(v.rowsCursor+delta, 0, len(v.rows)-1)
	}
}

func (v *runsView) setCursor(i int) {
	if v.focus == 0 {
		v.ratingsCursor = clamp(i, 0, len(v.ratings)-1)
	} else {
		v.rowsCursor = clamp(i, 0, len(v.rows)-1)
	}
}

func (v runsView) handleDetailKey(msg tea.KeyMsg) (view, tea.Cmd) {
	row, ok := v.detailRow()
	if !ok {
		v.detail = false
		return v, nil
	}
	switch msg.String() {
	case "esc":
		v.detail = false
	case "j", "down":
		v.detailCursor = clamp(v.detailCursor+1, 0, len(row.Notes)-1)
	case "k", "up":
		v.detailCursor = clamp(v.detailCursor-1, 0, len(row.Notes)-1)
	case "pgdown":
		v.detailCursor = clamp(v.detailCursor+5, 0, len(row.Notes)-1)
	case "pgup":
		v.detailCursor = clamp(v.detailCursor-5, 0, len(row.Notes)-1)
	case "g":
		v.detailCursor = 0
	case "G":
		v.detailCursor = len(row.Notes) - 1
	}
	return v, nil
}

func (v runsView) detailRow() (data.RetroRow, bool) {
	if v.rowsCursor < 0 || v.rowsCursor >= len(v.rows) {
		return data.RetroRow{}, false
	}
	return v.rows[v.rowsCursor], true
}

func (v runsView) Keys() []key.Binding {
	if v.detail {
		return []key.Binding{
			key.NewBinding(key.WithKeys("j", "k"), key.WithHelp("j/k", "scroll")),
			key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "back")),
		}
	}
	return []key.Binding{
		key.NewBinding(key.WithKeys("f"), key.WithHelp("f", "focus")),
		key.NewBinding(key.WithKeys("s"), key.WithHelp("s", "sort")),
		key.NewBinding(key.WithKeys("o"), key.WithHelp("o", "order")),
		key.NewBinding(key.WithKeys("j", "k"), key.WithHelp("j/k", "move")),
		key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "detail")),
	}
}

// Selection is the focused run row (list section only) — nil while the
// ratings table has focus, since there's no data.Selection variant for an
// aggregated ratings row.
func (v runsView) Selection() data.Selection {
	if v.focus != 1 {
		return nil
	}
	r, ok := v.detailRow()
	if !ok {
		return nil
	}
	crew := ""
	if r.Crew != nil {
		crew = *r.Crew
	}
	return data.Run{Crew: crew, Branch: r.Branch, T0: r.T0}
}

func (v runsView) Capturing() bool { return false }

func (v runsView) View(w, h int) string {
	if v.detail {
		return strings.Join(normalizeFrame(v.detailView(w, h), h, w), "\n")
	}
	topH := h / 2
	bottomH := h - topH
	top := v.ratingsLines(w, topH)
	bottom := v.rowsLines(w, bottomH)
	all := append(append([]string{}, top...), bottom...)
	return strings.Join(normalizeFrame(strings.Join(all, "\n"), h, w), "\n")
}

func (v runsView) sectionTitle(text string, focused bool) string {
	if focused {
		return tabActiveStyle.Render(text)
	}
	return tabInactiveStyle.Render(text)
}

func (v runsView) ratingsLines(w, h int) []string {
	title := truncateLine(v.sectionTitle("Ratings", v.focus == 0)+" — sort: "+sortColumns[v.sortCol]+" "+orderArrow(v.desc), w)
	if v.ratingsErr != nil {
		return []string{title, truncateLine("unavailable: "+*v.ratingsErr, w)}
	}
	if len(v.ratings) == 0 {
		return []string{title, truncateLine("no runs swept for this repo yet", w)}
	}

	headers := []string{"tier", "engine", "model", "n", "pr%", "success", "burn(med)"}
	rows := make([][]string, len(v.ratings))
	for i, g := range v.ratings {
		rows[i] = []string{
			g.Tier, g.Engine, g.Model,
			renderAgg(g.N.K, g.N.N, optNumber(g.N.Value)),
			renderAgg(g.PrPct.K, g.PrPct.N, optNumber(g.PrPct.Value)),
			renderAgg(g.MergePct.K, g.MergePct.N, optNumber(g.MergePct.Value)),
			renderAgg(g.BurnMedian.K, g.BurnMedian.N, optFmt1(g.BurnMedian.Value)),
		}
	}
	widths := tableWidths(append([][]string{headers}, rows...))
	left := []bool{true, true, true, false, false, false, false}

	bodyH := max0(h - 2) // title + header row
	out := []string{title, truncateLine("  "+headerStyle.Render(padRow(headers, widths, left)), w)}
	total := len(rows)
	offset := windowOffset(v.ratingsCursor, total, bodyH)
	end := offset + bodyH
	if end > total {
		end = total
	}
	for i := offset; i < end; i++ {
		prefix := "  "
		if v.focus == 0 && i == v.ratingsCursor {
			prefix = cursorStyle.Render("›") + " "
		}
		out = append(out, truncateLine(prefix+padRow(rows[i], widths, left), w))
	}
	return out
}

func orderArrow(desc bool) string {
	if desc {
		return "▼"
	}
	return "▲"
}

func (v runsView) rowsLines(w, h int) []string {
	title := truncateLine(v.sectionTitle("Runs (newest first)", v.focus == 1), w)
	if v.retroErr != nil {
		return []string{title, truncateLine("unavailable: "+*v.retroErr, w)}
	}
	if len(v.rows) == 0 {
		return []string{title, truncateLine("no retro notes yet", w)}
	}

	bodyH := max0(h - 1) // title
	out := []string{title}
	total := len(v.rows)
	offset := windowOffset(v.rowsCursor, total, bodyH)
	end := offset + bodyH
	if end > total {
		end = total
	}
	for i := offset; i < end; i++ {
		prefix := "  "
		if v.focus == 1 && i == v.rowsCursor {
			prefix = cursorStyle.Render("›") + " "
		}
		out = append(out, truncateLine(prefix+rowLine(v.rows[i]), w))
	}
	return out
}

// rowLine renders one retro row per spec §Runs: a run row is "branch
// tier/engine/model outcome tags"; a dispatcher row is "crew <id>
// <session_summary detail> tags(other notes)".
func rowLine(r data.RetroRow) string {
	if r.Kind == "dispatcher" {
		crew := ""
		if r.Crew != nil {
			crew = *r.Crew
		}
		summary := ""
		for _, n := range r.Notes {
			if n.Tag == "session_summary" {
				summary = n.Detail
				break
			}
		}
		return "crew " + crew + "  " + cleanText(summary) + "  " + tagCounts(r.Notes, "session_summary")
	}
	tem := r.Tier + "/" + r.Engine + "/" + r.Model
	return r.Branch + "  " + tem + "  " + r.Outcome + "  " + tagCounts(r.Notes, "")
}

// tagCounts ports once.go's tagstrDash: "tag x2" for a repeated tag, in
// first-seen order, excluding excludeTag (empty excludes nothing).
func tagCounts(notes []data.Note, excludeTag string) string {
	var order []string
	seen := map[string]struct{}{}
	counts := map[string]int{}
	for _, n := range notes {
		if n.Tag == excludeTag {
			continue
		}
		counts[n.Tag]++
		if _, ok := seen[n.Tag]; !ok {
			seen[n.Tag] = struct{}{}
			order = append(order, n.Tag)
		}
	}
	parts := make([]string, 0, len(order))
	for _, t := range order {
		if counts[t] > 1 {
			parts = append(parts, fmt.Sprintf("%s x%d", t, counts[t]))
		} else {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, ", ")
}

func (v runsView) detailView(w, h int) string {
	row, ok := v.detailRow()
	if !ok {
		return ""
	}
	label := row.Branch
	if row.Kind == "dispatcher" {
		crew := ""
		if row.Crew != nil {
			crew = *row.Crew
		}
		label = "crew " + crew
	}
	// T0 is a bus timestamp — milliseconds, like every other event ts in
	// this system (confirmed against real `crew retro --report --json`
	// output, e.g. t0: 1789027428338) — isoUTC wants seconds.
	header := label + "  " + row.Tier + "/" + row.Engine + "/" + row.Model + "  " + row.Outcome + "  " + isoUTC(row.T0/1000)

	// Render every note as its own block, tracking each block's starting
	// line so the viewport can center on the cursor's note (Update only
	// ever moves detailCursor — it never sees w/h — so scrolling is always
	// re-derived here from that cursor, the same pattern settingsView uses
	// for its tree).
	var lines []string
	starts := make([]int, len(row.Notes))
	for i, n := range row.Notes {
		starts[i] = len(lines)
		lines = append(lines, cursorStyle.Render(cleanText(n.Seam)+" · "+cleanText(n.Tag)))
		wrapped := ansi.Wrap(cleanText(n.Detail), max0(w), "")
		if wrapped == "" {
			wrapped = " "
		}
		lines = append(lines, strings.Split(wrapped, "\n")...)
		lines = append(lines, "")
	}

	headerLines := []string{truncateLine(headerStyle.Render(header), w), ""}
	bodyH := max0(h - len(headerLines))
	total := len(lines)
	cursorLine := 0
	if v.detailCursor >= 0 && v.detailCursor < len(starts) {
		cursorLine = starts[v.detailCursor]
	}
	offset := windowOffset(cursorLine, total, bodyH)
	end := offset + bodyH
	if end > total {
		end = total
	}
	visible := lines[offset:end]

	out := append([]string{}, headerLines...)
	for _, l := range visible {
		out = append(out, truncateLine(l, w))
	}
	return strings.Join(out, "\n")
}
