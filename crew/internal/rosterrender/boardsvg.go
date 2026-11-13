package rosterrender

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/noamsto/dispatcher/crew/internal/ascii"
)

// Card furniture, in em of the card's font, so a stretched card scales its
// text, badges and chips together.
const (
	padXEm     = 0.8
	padYEm     = 0.4
	rowEm      = 1.2
	baselineEm = 0.92 // a row's baseline below its top, centring the x-height
	footEm     = 1.1
	chipEm     = 0.9
	chipFontEm = 0.7
	cornerEm   = 0.6
	ciEm       = 0.35
	hexEm      = 0.42

	// The waiting ring and the selection ring stand off the card's edge; both
	// stay inside half of cardGap so neighbouring cards' rings never touch.
	waitOut   = 3.0
	waitPen   = 1.5
	selectOut = 6.0
	selectPen = 1.5

	// dimOpacity fades a stale, done or failed card.
	dimOpacity = 0.55
)

// headerInfo is the header bar's text: the crew on the left, the budget strip
// (stripText) on the right.
type headerInfo struct {
	crew, strip string
}

// selection is the worker the --select ring marks; repo, when set, narrows it
// to that repo's label.
type selection struct {
	branch, repo string
}

var xmlEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;")

func escText(s string) string { return xmlEscaper.Replace(s) }

// fmtNum writes a coordinate with at most two decimals.
func fmtNum(v float64) string { return strconv.FormatFloat(math.Round(v*100)/100, 'f', -1, 64) }

// svgText writes one text element; anchor and weight are optional.
func svgText(b *strings.Builder, x, y, size float64, fill, anchor, weight, s string) {
	fmt.Fprintf(b, `<text x="%s" y="%s" font-size="%s" fill="%s"`, fmtNum(x), fmtNum(y), fmtNum(size), fill)
	if anchor != "" {
		fmt.Fprintf(b, ` text-anchor="%s"`, anchor)
	}
	if weight != "" {
		fmt.Fprintf(b, ` font-weight="%s"`, weight)
	}
	b.WriteString(">" + escText(s) + "</text>\n")
}

func classColour(class string, failed bool) string {
	switch class {
	case classQueued:
		return colHolds
	case classWorking:
		return colWorking
	case classBlocked:
		return colBlocked
	case classPROpen:
		return colPROpen
	case classDone:
		if failed {
			return colFailed
		}
		return colDone
	}
	return colWorking
}

// ciStyle is how a CI state draws its badge; an unknown state has none.
func ciStyle(state string) (fill, stroke, glyph string, dashed, ok bool) {
	switch state {
	case "pass":
		return colWorking, "", "", false, true
	case "pending":
		return colBG, colSun, "", false, true
	case "fail":
		return colBlocked, "", "!", false, true
	case "none":
		return colBG, colDim, "", true, true
	}
	return "", "", "", false, false
}

// renderBoardSVG draws the board laid out by g as a standalone SVG whose
// viewBox is exactly the canvas. Ids come from lane, column and card indexes,
// never from bus text, and every string is escaped.
func renderBoardSVG(b board, g boardGeom, hdr headerInfo, sel selection) string {
	var s strings.Builder
	fmt.Fprintf(&s, `<svg xmlns="http://www.w3.org/2000/svg" width="%[1]s" height="%[2]s" viewBox="0 0 %[1]s %[2]s" font-family="'%[3]s'">`+"\n",
		fmtNum(g.W), fmtNum(g.H), diagramFont)
	fmt.Fprintf(&s, `<rect x="0" y="0" width="%s" height="%s" fill="%s"/>`+"\n", fmtNum(g.W), fmtNum(g.H), colBG)
	writeHeader(&s, b, g.hdr, hdr)
	for li, lg := range g.lanes {
		l := b.lanes[li]
		if l.label != "" {
			r := lg.label
			fmt.Fprintf(&s, `<rect x="%s" y="%s" width="%s" height="%s" rx="6" fill="%s" fill-opacity="0.6"/>`+"\n",
				fmtNum(r.x), fmtNum(r.y), fmtNum(r.w), fmtNum(r.h), colFill)
			size := 0.55 * r.h
			svgText(&s, r.x+12, r.y+r.h/2+0.35*size, size, colLegend, "", "600", truncAt(l.label, r.w-24, size))
		}
		for ci, cg := range lg.cols {
			col := l.cols[ci]
			writeColumnHeader(&s, cg.header, col)
			for _, cd := range cg.cards {
				c := col.cards[cd.card]
				selected := c.kind == kindPlanet && sel.branch != "" && c.branch == sel.branch && (sel.repo == "" || c.repoLabel == sel.repo)
				writeCard(&s, fmt.Sprintf("card-%d-%d-%d", li, ci, cd.card), c, cd, selected)
			}
		}
	}
	s.WriteString("</svg>\n")
	return s.String()
}

// writeHeader draws the header bar: the crew and what the board does not draw
// on the left, the budget strip on the right, the strip giving way first.
func writeHeader(s *strings.Builder, b board, r rect, hdr headerInfo) {
	fmt.Fprintf(s, `<rect x="%s" y="%s" width="%s" height="%s" fill="%s"/>`+"\n", fmtNum(r.x), fmtNum(r.y), fmtNum(r.w), fmtNum(r.h), colFill)
	parts := []string{hdr.crew}
	for _, n := range []struct {
		n    int
		text string
	}{{b.folded, "done earlier"}, {b.more, "more queued"}, {b.hidden, "not shown"}} {
		if n.n > 0 {
			parts = append(parts, "+"+strconv.Itoa(n.n)+" "+n.text)
		}
	}
	left := strings.Join(slices.DeleteFunc(parts, func(p string) bool { return p == "" }), " · ")
	size := min(0.4*r.h, 26)
	avail := r.w - 2*boardPad
	if hdr.strip != "" {
		left = truncAt(left, 0.55*avail, size)
	}
	base := r.y + r.h/2 + 0.35*size
	svgText(s, r.x+boardPad, base, size, colText, "", "600", left)
	room := avail - textWidthAt(left, size) - size
	if hdr.strip == "" || room <= textWidthAt(ellipsis, size) {
		return
	}
	svgText(s, r.x+r.w-boardPad, base, size, colLegend, "end", "", truncAt(hdr.strip, room, size))
}

// writeColumnHeader draws a state-colour dot and `WORKING 3`.
func writeColumnHeader(s *strings.Builder, r rect, col column) {
	dot := 0.16 * r.h
	fmt.Fprintf(s, `<circle cx="%s" cy="%s" r="%s" fill="%s"/>`+"\n", fmtNum(r.x+dot+2), fmtNum(r.y+r.h/2), fmtNum(dot), classColour(col.class, false))
	size := 0.5 * r.h
	x := r.x + 2*dot + 10
	svgText(s, x, r.y+r.h/2+0.35*size, size, colText, "", "600", truncAt(col.header(), r.x+r.w-x, size))
}

// cardRow is one optional text row of a card; rows with the lowest prio are
// kept when the card is too short for all of them.
type cardRow struct {
	text, fill string
	prio       int
}

func writeCard(s *strings.Builder, id string, c boardCard, g cardGeom, selected bool) {
	f := g.font
	col := classColour(c.class, c.failed)
	dim := c.stale || c.class == classDone
	fmt.Fprintf(s, `<g id="%s"`, id)
	if dim {
		fmt.Fprintf(s, ` opacity="%s"`, fmtNum(dimOpacity))
	}
	s.WriteString(">\n")

	pen := cardPen(c.tier) * min(f/cardFont, 2)
	dash := ""
	switch {
	case c.kind == kindHold:
		pen = planetPenwidth
		dash = fmt.Sprintf(` stroke-dasharray="%s %s"`, fmtNum(0.6*f), fmtNum(0.4*f))
	case c.kind == kindTask:
		pen = planetPenwidth
		dash = fmt.Sprintf(` stroke-dasharray="0.01 %s" stroke-linecap="round"`, fmtNum(0.5*f))
	case dim:
		dash = fmt.Sprintf(` stroke-dasharray="%s %s"`, fmtNum(0.6*f), fmtNum(0.4*f))
	}
	rx := cornerEm * f
	fmt.Fprintf(s, `<rect x="%s" y="%s" width="%s" height="%s" rx="%s" fill="%s" stroke="%s" stroke-width="%s"%s`,
		fmtNum(g.x), fmtNum(g.y), fmtNum(g.w), fmtNum(g.h), fmtNum(rx), colFill, col, fmtNum(pen), dash)
	if c.pulse {
		s.WriteString(` class="aeye-pulse"`)
	}
	s.WriteString("/>\n")
	if c.flow {
		fmt.Fprintf(s, `<path class="aeye-flow" d="M%s,%s H%s" fill="none" stroke="%s" stroke-width="%s"%s/>`+"\n",
			fmtNum(g.x+rx), fmtNum(g.y), fmtNum(g.x+g.w-rx), col, fmtNum(1.6*pen), flowDash)
	}
	ring := func(out, pen float64, stroke, extra string) {
		fmt.Fprintf(s, `<rect x="%s" y="%s" width="%s" height="%s" rx="%s" fill="none" stroke="%s" stroke-width="%s"%s/>`+"\n",
			fmtNum(g.x-out), fmtNum(g.y-out), fmtNum(g.w+2*out), fmtNum(g.h+2*out), fmtNum(rx+out), stroke, fmtNum(pen), extra)
	}
	if c.waiting {
		ring(waitOut, waitPen, colWaiting, ` stroke-dasharray="1.5 3"`)
	}
	if selected {
		ring(selectOut, selectPen, colSun, "")
	}

	left, right := g.x+padXEm*f, g.x+g.w-padXEm*f
	rowW := right - left
	top := g.y + padYEm*f
	writeHead(s, c, f, left, right, top)

	footer := c.kind == kindPlanet && (len(c.moons) > 0 || c.rounds > 0)
	foot := 0.0
	if footer {
		foot = footEm * f
	}
	fit := int((g.h - 2*padYEm*f - foot) / (rowEm * f))
	for i, r := range cardLines(c, rowW, f, fit-1) {
		svgText(s, left, top+float64(i+1)*rowEm*f+baselineEm*f, f, r.fill, "", "", r.text)
	}
	if footer {
		writeFooter(s, c, f, left, right, g.y+g.h-padYEm*f-foot)
	}
	s.WriteString("</g>\n")
}

// writeHead draws row 0: the remote host chip, the head text, the state word
// and the CI badge, right to left in what gives way first.
func writeHead(s *strings.Builder, c boardCard, f, left, right, top float64) {
	mid := top + rowEm*f/2
	x := left
	if c.host != "" {
		hr := hexEm * f
		cx := x + hr
		var pts []string
		for k := range 6 {
			a := math.Pi / 3 * float64(k)
			pts = append(pts, fmtNum(cx+hr*math.Cos(a))+","+fmtNum(mid+hr*math.Sin(a)))
		}
		fmt.Fprintf(s, `<polygon points="%s" fill="%s" stroke="%s" stroke-width="1.5"/>`+"\n", strings.Join(pts, " "), colFill, colLegend)
		x += 2*hr + 0.25*f
		size := chipFontEm * f
		host := truncAt(c.host, 0.35*(right-left), size)
		svgText(s, x, mid+0.35*size, size, colLegend, "", "", host)
		x += textWidthAt(host, size) + 0.4*f
	}
	if fill, stroke, glyph, dashed, ok := ciStyle(c.ci); ok && c.kind == kindPlanet {
		r := ciEm * f
		cx := right - r
		extra := ""
		if stroke != "" {
			extra = fmt.Sprintf(` stroke="%s" stroke-width="%s"`, stroke, fmtNum(0.12*f))
		}
		if dashed {
			extra += ` stroke-dasharray="2 2"`
		}
		fmt.Fprintf(s, `<circle cx="%s" cy="%s" r="%s" fill="%s"%s/>`+"\n", fmtNum(cx), fmtNum(mid), fmtNum(r), fill, extra)
		if glyph != "" {
			size := 1.6 * r
			svgText(s, cx, mid+0.35*size, size, colBG, "middle", "700", glyph)
		}
		right -= 2*r + 0.35*f
	}
	stateCol := classColour(c.class, c.failed)
	if c.stale || c.class == classDone {
		stateCol = colDim
	}
	state := truncAt(stateWord(c.state), 0.5*(right-left), f)
	svgText(s, right, top+baselineEm*f, f, stateCol, "end", "", state)
	room := right - textWidthAt(state, f) - 0.5*f - x
	if room <= textWidthAt(ellipsis, f) {
		return
	}
	svgText(s, x, top+baselineEm*f, f, colText, "", "600", truncAt(headOf(c), room, f))
}

func headOf(c boardCard) string {
	if c.kind != kindPlanet && ascii.IsDigits(c.name) {
		return "#" + c.name
	}
	return cardHead(c.planet)
}

// cardLines is the text below the head, at most n rows in reading order. The
// second title line gives way first, then the detail, then the meta row; the
// `on` marker leads the detail row and outranks the meta row.
func cardLines(c boardCard, w, f float64, n int) []cardRow {
	if n <= 0 {
		return nil
	}
	var rows []cardRow
	switch c.kind {
	case kindHold:
		text := "fleet queue"
		if c.engine != "" || c.window != "" {
			text = orQ(c.engine) + " · " + orQ(c.window)
		}
		rows = []cardRow{{truncAt(text, w, f), colLegend, 1}}
	case kindTask:
		text := "filed"
		if c.on != "" {
			text = c.on
		}
		rows = []cardRow{{truncAt(text, w, f), colLegend, 1}}
	case kindPlanet:
		line, rest := wrapAt(c.title, w, f)
		second := truncAt(strings.TrimSpace(rest), w, f)
		detail := c.detail
		if c.on != "" {
			detail = strings.TrimSuffix(c.on+" · "+detail, " · ")
		}
		detailPrio, metaPrio := 3, 2
		if c.on != "" {
			detailPrio, metaPrio = 2, 3
		}
		rows = []cardRow{
			{line, colText, 1},
			{second, colText, 4},
			{truncAt(metaText(c.planet), w, f), colLegend, metaPrio},
			{truncAt(detail, w, f), colDim, detailPrio},
		}
	}
	rows = slices.DeleteFunc(rows, func(r cardRow) bool { return r.text == "" })
	if len(rows) > n {
		// The second title line is the first to go, so the first one ends in the
		// ellipsis instead.
		if c.kind == kindPlanet && rows[0].prio == 1 {
			rows[0].text = truncAt(c.title, w, f)
		}
		keep := slices.Clone(rows)
		slices.SortStableFunc(keep, func(a, b cardRow) int { return a.prio - b.prio })
		cut := keep[n-1].prio
		rows = slices.DeleteFunc(rows, func(r cardRow) bool { return r.prio > cut })
	}
	return rows
}

// writeFooter draws the role chips from the left, then `+K` for any that do
// not fit, and the review-round chip on the right.
func writeFooter(s *strings.Builder, c boardCard, f, left, right, top float64) {
	h, size := chipEm*f, chipFontEm*f
	y := top + (footEm*f-h)/2
	base := y + h/2 + 0.35*size
	pill := func(x, w float64, fill, stroke, class string) {
		fmt.Fprintf(s, `<rect x="%s" y="%s" width="%s" height="%s" rx="%s" fill="%s"`, fmtNum(x), fmtNum(y), fmtNum(w), fmtNum(h), fmtNum(h/2), fill)
		if stroke != "" {
			fmt.Fprintf(s, ` stroke="%s" stroke-width="1"`, stroke)
		}
		if class != "" {
			fmt.Fprintf(s, ` class="%s"`, class)
		}
		s.WriteString("/>\n")
	}
	if c.rounds > 0 {
		text := "r" + strconv.Itoa(c.rounds)
		w := textWidthAt(text, size) + 0.6*f
		pill(right-w, w, colFill, colEdge, "")
		svgText(s, right-w/2, base, size, colLegend, "middle", "", text)
		right -= w + 0.3*f
	}
	x := left
	more := textWidthAt("+99", size)
	for k, m := range c.moons {
		role := clean(m.role)
		w := textWidthAt(role, size) + 0.6*f
		limit := right
		if k < len(c.moons)-1 {
			limit -= more + 0.3*f
		}
		if x+w > limit {
			svgText(s, x, base, size, colDim, "", "", "+"+strconv.Itoa(len(c.moons)-k))
			return
		}
		fill, class := colEdge, ""
		if m.working {
			fill = colWorking
		}
		if k < len(c.glow) && c.glow[k] {
			class = "aeye-glow"
		}
		pill(x, w, fill, "", class)
		svgText(s, x+w/2, base, size, colBG, "middle", "", role)
		x += w + 0.3*f
	}
}
