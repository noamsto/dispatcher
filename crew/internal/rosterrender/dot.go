package rosterrender

import (
	"bytes"
	"cmp"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// The palette (catppuccin mocha): ring colour is the worker's state.
const (
	colBG      = "#1e1e2e"
	colSun     = "#f9e2af"
	colWorking = "#a6e3a1"
	colBlocked = "#f38ba8"
	colPROpen  = "#89b4fa"
	colDone    = "#45475a"
	colFailed  = "#7f4a5a"
	colHolds   = "#fab387"
	colFill    = "#313244"
	colEdge    = "#585b70"
	colText    = "#cdd6f4"
	colDim     = "#6c7086"
	colLegend  = "#a6adc8"
	colWaiting = "#cba6f7"
)

const (
	moonSize = 0.16
	sunSize  = 1.0

	// canvasPad is the margin between the outermost thing and the invisible
	// corners; legendH is the height the legend adds under the canvas, which the
	// corners pre-pay in width so the picture comes out square.
	canvasPad = 24.0
	legendH   = 74.0

	// legendRowH is the height of the marker row the full legend gains when any
	// marker or wedge is drawn. The counts legend is one such row on top of the
	// padding the full legend's height already holds.
	legendRowH    = 20.0
	legendCountsH = legendH - 2*legendRowH

	// planetPenwidth is every node's ring width; a blocked planet's is heavier.
	// The map's hit radius includes half of it.
	planetPenwidth  = 3.0
	blockedPenwidth = 5.0

	// selectGap is how far the selection ring stands off a planet's edge.
	selectGap = 9.0

	// A waiting planet gets a dotted ring waitGap off its edge; a CI badge sits
	// at the upper right, its centre ciBadgeStandoff outside the planet's edge so
	// it overlaps the rim without covering the codename. The map's hit radius
	// covers both.
	waitGap         = 6.0
	waitPenwidth    = 2.5
	ciBadgeSize     = 0.2
	ciBadgeRadius   = ciBadgeSize * 72 / 2
	ciBadgeStandoff = ciBadgeRadius / 2

	// A review-round badge sits at the lower right, standing off like the CI one.
	roundBadgeW        = 0.3
	roundBadgeH        = 0.2
	roundBadgeStandoff = ciBadgeStandoff

	// Wedge boundary spokes start spokeInner from the dispatcher; the rim arc is
	// a polyline of points at most arcStep degrees apart, inset arcInset from the
	// wedge's edges so neighbouring arcs do not touch.
	spokeInner = 50.0
	arcStep    = 10.0
	arcInset   = 1.5

	// A wedge's divider and rim are hairlines; its fill is a wash of one palette
	// hue at wedgeFillOpacity, and every text gets a haloWidth outline in the
	// background colour so a line behind it never cuts the letters.
	dividerPenwidth  = 0.75
	wedgeFillOpacity = 0.1
	haloWidth        = 3.0

	staleAlpha = "55"

	// A host label is a plaintext node that grows with its text, so the map
	// bounds it by a width that overestimates Source Sans 3 at 12pt: the corners
	// must hold the whole label for Graphviz's bounding box to equal them.
	hostCharWidth = 7.2
	hostMargin    = 16.0
	hostMinWidth  = 54.0
)

// diagramFont is aeye's default render font (AEYE_D2_FONT). The native svg
// names it in every text element and aeye's resvg draws it from its font
// directory, which holds nothing else. Node sizes are fixed, so a host without
// this font only measures the legend and the host labels with a fallback one.
const diagramFont = "Source Sans 3"

// dotq quotes s as a DOT string.
func dotq(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s) + `"`
}

// svgRootRe is the root element's opening tag, with the four numbers of its viewBox.
var svgRootRe = regexp.MustCompile(`<svg\b[^>]*?\sviewBox="([-0-9.]+)\s+([-0-9.]+)\s+([-0-9.]+)\s+([-0-9.]+)"[^>]*>`)

// finishSVG turns Graphviz's native svg into what aeye's carousel takes. Graphviz
// entity-encodes the hyphen of a class (`aeye&#45;flow`), which aeye's text-level
// hook detection does not see; resvg cannot parse an unquoted multi-word
// font-family; and Graphviz's background polygon stops short of the viewBox edge
// and moves with the zoom, so a rect over the viewBox, in absolute user units
// because aeye zooms by rewriting the viewBox, goes in first. Every text gets a
// background-coloured halo, so a line that still passes behind a label does not
// cut its letters, and the repo wedges' fills go in under everything Graphviz
// drew. Text without a root viewBox is not an svg this knows, and comes back
// unchanged.
func finishSVG(svg []byte, wedges []wedge) []byte {
	m := svgRootRe.FindSubmatchIndex(svg)
	if m == nil {
		return svg
	}
	num := func(i int) string { return string(svg[m[2*i]:m[2*i+1]]) }
	rect := fmt.Sprintf(`<rect x="%s" y="%s" width="%s" height="%s" fill="%s"/>`, num(1), num(2), num(3), num(4), colBG)
	out := slices.Concat(svg[:m[1]], []byte(rect), svg[m[1]:])
	out = bytes.ReplaceAll(out, []byte("aeye&#45;"), []byte("aeye-"))
	out = bytes.ReplaceAll(out, []byte(`font-family="`+diagramFont+`"`), []byte(`font-family="'`+diagramFont+`'"`))
	out = textTagRe.ReplaceAllFunc(out, func(tag []byte) []byte {
		if bytes.Contains(tag, []byte("paint-order=")) {
			return tag
		}
		return slices.Concat(tag[:len(tag)-1], []byte(haloAttrs), tag[len(tag)-1:])
	})
	return injectWedgeFills(out, wedges)
}

var (
	textTagRe = regexp.MustCompile(`<text\b[^>]*>`)
	haloAttrs = fmt.Sprintf(` stroke="%s" stroke-width="%g" stroke-linejoin="round" paint-order="stroke"`, colBG, haloWidth)

	// sunRe finds the dispatcher's centre, which is layout (0, 0): Graphviz
	// moves the pinned layout by an offset it does not state, and y runs down.
	sunRe = regexp.MustCompile(`<title>dispatcher</title>\s*<ellipse[^>]*?\scx="([-0-9.]+)"\s+cy="([-0-9.]+)"`)
	// firstShapeRe is the first edge or node group, which the fills go in front of.
	firstShapeRe = regexp.MustCompile(`<g id="(?:edge|node)\d+"`)
)

// wedgeHues are the palette colours the repo wedges' fills cycle over.
var wedgeHues = [...]string{colWorking, colPROpen, colWaiting, colSun}

// injectWedgeFills draws each wedge as a faint pie slice from the dispatcher to
// its rim, in front of the background and behind every edge, node and label.
// Graphviz cannot fill a sector. An svg without a dispatcher or without any edge
// or node comes back unchanged.
func injectWedgeFills(svg []byte, wedges []wedge) []byte {
	sun, at := sunRe.FindSubmatch(svg), firstShapeRe.FindIndex(svg)
	if len(wedges) == 0 || sun == nil || at == nil {
		return svg
	}
	cx, _ := strconv.ParseFloat(string(sun[1]), 64)
	cy, _ := strconv.ParseFloat(string(sun[2]), 64)
	var fills strings.Builder
	for i, wd := range wedges {
		fills.WriteString(wedgeFill(cx, cy, wd, wedgeHues[i%len(wedgeHues)]))
	}
	return slices.Concat(svg[:at[0]], []byte(fills.String()), svg[at[0]:])
}

// wedgeFill is the pie-slice path of wd around the svg point (cx, cy); the arc
// runs clockwise on screen.
func wedgeFill(cx, cy float64, wd wedge, col string) string {
	at := func(deg float64) string {
		x, y := polar(wd.rim, deg)
		return fmt.Sprintf("%.2f,%.2f", cx+x, cy-y)
	}
	large := 0
	if wd.span > 180 {
		large = 1
	}
	return fmt.Sprintf(`<path d="M%.2f,%.2f L%s A%.2f,%.2f 0 %d 1 %s Z" fill="%s" fill-opacity="%g" stroke="none"/>`+"\n",
		cx, cy, at(wd.start), wd.rim, wd.rim, large, at(wd.start+wd.span), col, wedgeFillOpacity)
}

// maxAnimated caps each animated class. Motion should mark what is live and
// recent; past about eight elements it turns into noise and costs per-frame
// render time. The legend counts carry the totals.
const maxAnimated = 8

// planetKey is a planet's identity: a branch name can recur in another repo.
type planetKey struct {
	repo, branch string
}

func (p placed) key() planetKey { return planetKey{p.repo, p.branch} }

type moonKey struct {
	planet planetKey
	k      int
}

// animatedSets picks what moves: flow spokes of working planets, pulses of
// blocked planets, glows of working moons. A stale planet is not live, so it
// neither flows nor pulses. Planets rank newest first, ties by repo and branch,
// and the glow cap counts moons, so the last planet may be taken partly.
func animatedSets(planets []placed) (flow, pulse map[planetKey]bool, glow map[moonKey]bool) {
	ranked := slices.Clone(planets)
	slices.SortFunc(ranked, func(x, y placed) int {
		return cmp.Or(cmp.Compare(y.ts, x.ts), strings.Compare(x.branch, y.branch), strings.Compare(x.repo, y.repo))
	})
	flow, pulse, glow = map[planetKey]bool{}, map[planetKey]bool{}, map[moonKey]bool{}
	for _, p := range ranked {
		switch {
		case p.stale:
		case p.class == classWorking && len(flow) < maxAnimated:
			flow[p.key()] = true
		case p.class == classBlocked && len(pulse) < maxAnimated:
			pulse[p.key()] = true
		}
		for k, m := range p.moons {
			if m.working && len(glow) < maxAnimated {
				glow[moonKey{p.key(), k}] = true
			}
		}
	}
	return
}

func str(k, v string) string { return k + "=" + dotq(v) }

func num(k string, v float64) string { return k + "=" + strconv.FormatFloat(v, 'g', -1, 64) }

func pos(x, y float64) string { return str("pos", pt(x)+","+pt(y)+"!") }

// pt formats a coordinate in points, never as "-0.00".
func pt(v float64) string {
	s := strconv.FormatFloat(v, 'f', 2, 64)
	if s == "-0.00" {
		return "0.00"
	}
	return s
}

func stmt(b *strings.Builder, head string, attrs ...string) {
	b.WriteString("  " + head)
	if len(attrs) > 0 {
		b.WriteString(" [" + strings.Join(attrs, " ") + "]")
	}
	b.WriteString(";\n")
}

func renderDOT(sc scene, lg legend, mode legendMode) string { return buildDOT(sc, &lg, mode, "", "") }

// labelWidth bounds the drawn width of a plaintext label node.
func labelWidth(text string) float64 {
	return max(hostMinWidth, float64(len(text))*hostCharWidth+hostMargin)
}

// mapHalf is the half extent of the map's canvas around the dispatcher, wide
// enough for the host and wedge labels.
func mapHalf(sc scene) (halfX, halfY float64) {
	half := sc.extent + canvasPad
	halfX, halfY = half, half
	for _, h := range sc.hosts {
		halfX = max(halfX, math.Abs(h.x)+labelWidth(h.host)/2)
	}
	for _, w := range sc.wedges {
		halfX = max(halfX, math.Abs(w.x)+labelWidth(w.label)/2)
	}
	for _, a := range sc.anchors {
		halfX = max(halfX, math.Abs(a.x)+labelWidth(a.text)/2)
	}
	return halfX, halfY
}

// hitRadius is the distance from a planet's centre to the farthest thing drawn
// for it: its ring, the waiting ring and the CI badge.
func hitRadius(p placed) float64 {
	pr := p.size * 72 / 2
	pen := planetPenwidth
	if !p.failed && p.class == classBlocked {
		pen = blockedPenwidth
	}
	r := pr + pen/2
	if p.waiting {
		r = max(r, pr+waitGap+waitPenwidth/2)
	}
	if _, badgePen, ok := ciBadge(p.ci); ok {
		r = max(r, pr+ciBadgeStandoff+ciBadgeRadius+badgePen/2)
	}
	if p.rounds > 0 {
		r = max(r, pr+roundBadgeStandoff+math.Hypot(roundBadgeW*72/2, roundBadgeH*72/2))
	}
	return r
}

// ciBadge is the attributes a CI state draws its badge with, and its pen width,
// which the hit radius adds half of. An unknown state has no badge.
func ciBadge(state string) (attrs []string, pen float64, ok bool) {
	switch state {
	case "pass":
		return []string{`label=""`, str("fillcolor", colWorking), "penwidth=0"}, 0, true
	case "pending":
		return []string{`label=""`, str("fillcolor", colBG), str("color", colSun), "penwidth=2"}, 2, true
	case "fail":
		return []string{str("label", "!"), str("fillcolor", colBlocked), "penwidth=0", str("fontcolor", colBG), "fontsize=9"}, 0, true
	case "none":
		return []string{`label=""`, str("fillcolor", colBG), str("color", colDim), "penwidth=1", str("style", "filled,dashed")}, 1, true
	}
	return nil, 0, false
}

func markerRow(lg legend) bool {
	return lg.waiting > 0 || lg.ci > 0 || lg.stale > 0 || lg.repos > 1
}

// legendHeight is what the legend adds under the canvas.
func legendHeight(lg legend, mode legendMode) float64 {
	switch {
	case mode == legendNone:
		return 0
	case mode == legendCounts:
		return legendCountsH
	case markerRow(lg):
		return legendH + 2*legendRowH
	}
	return legendH + legendRowH
}

// stripText is the budget strip: this crew's live weight against the fleet
// budget, then each engine's most-used quota window. Unknown is `?`, never 0.
func stripText(lg legend) string {
	budget := "?"
	switch {
	case lg.budget == nil:
	case *lg.budget == 0:
		budget = "off"
	default:
		budget = strconv.Itoa(*lg.budget)
	}
	parts := []string{"crew weight " + strconv.Itoa(lg.weight) + " · fleet budget " + budget}
	for _, e := range lg.engines {
		if e.pct == nil {
			parts = append(parts, clean(e.name)+" ?")
			continue
		}
		parts = append(parts, clean(e.name)+" "+clean(e.window)+" "+strconv.FormatFloat(*e.pct, 'f', 0, 64)+"%")
	}
	return strings.Join(parts, " · ")
}

// buildDOT is the whole picture as DOT for `neato -n`: every node is pinned, so
// Graphviz only draws. Ids are generated from the branch order (p1, p1m1, h1),
// never from user text, and every string goes through dotq.
//
// A nil legend is the map: no graph label, so the corners are the canvas and
// the layout's coordinates map onto it by a constant offset, and a ring around
// the planet sel names, when there is one. A selRepo narrows it to that repo's
// wedge; empty rings the branch in every repo. Otherwise mode picks how much of
// the legend is drawn, and the corners pre-pay exactly that.
func buildDOT(sc scene, lg *legend, mode legendMode, sel, selRepo string) string {
	var b strings.Builder
	b.WriteString("digraph roster {\n")
	b.WriteString("  layout=neato; splines=false; outputorder=edgesfirst; pad=0.2; labelloc=b;\n")
	b.WriteString("  " + str("bgcolor", colBG) + "; " + str("fontname", diagramFont) + "; fontsize=13; " + str("fontcolor", colLegend) + ";\n")
	b.WriteString("  node [shape=circle style=filled " + str("fontname", diagramFont) + " fontsize=12 " +
		str("fontcolor", colText) + " " + str("fillcolor", colFill) + " " + num("penwidth", planetPenwidth) + " fixedsize=true];\n")
	b.WriteString("  edge [" + str("color", colEdge) + " penwidth=1.5 arrowhead=none];\n")

	half := sc.extent + canvasPad
	var halfX, halfY float64
	if lg == nil {
		halfX, halfY = mapHalf(sc)
	} else {
		halfX, halfY = half+legendHeight(*lg, mode)/2, half
	}
	for i, c := range [4][2]float64{{-halfX, halfY}, {halfX, halfY}, {-halfX, -halfY}, {halfX, -halfY}} {
		stmt(&b, "c"+strconv.Itoa(i+1), `label=""`, "style=invis", "shape=point", "width=0", pos(c[0], c[1]))
	}
	stmt(&b, "dispatcher", str("label", "dispatcher"), num("width", sunSize), num("height", sunSize), pos(0, 0),
		str("color", colSun), str("fillcolor", colDone))

	planets := slices.Clone(sc.planets)
	slices.SortFunc(planets, func(x, y placed) int {
		return cmp.Or(strings.Compare(x.repo, y.repo), strings.Compare(x.branch, y.branch))
	})
	ids := make(map[planetKey]string, len(planets))
	for i, p := range planets {
		ids[p.key()] = "p" + strconv.Itoa(i+1)
	}
	flow, pulse, glow := animatedSets(planets)
	for _, p := range planets {
		stmt(&b, ids[p.key()], planetAttrs(p, pulse[p.key()])...)
		if p.waiting {
			d := p.size + 2*waitGap/72
			stmt(&b, ids[p.key()]+"w", `label=""`, "style=dotted", num("width", d), num("height", d), pos(p.x, p.y),
				str("color", colWaiting), num("penwidth", waitPenwidth))
		}
		if attrs, _, ok := ciBadge(p.ci); ok {
			dx, dy := polar(p.size*72/2+ciBadgeStandoff, 45)
			stmt(&b, ids[p.key()]+"c", append([]string{num("width", ciBadgeSize), num("height", ciBadgeSize), pos(p.x+dx, p.y+dy)}, attrs...)...)
		}
		if p.rounds > 0 {
			dx, dy := polar(p.size*72/2+roundBadgeStandoff, 135)
			stmt(&b, ids[p.key()]+"r", str("label", "r"+strconv.Itoa(p.rounds)), "shape=box", str("style", "rounded,filled"),
				num("width", roundBadgeW), num("height", roundBadgeH), "fontsize=9", pos(p.x+dx, p.y+dy),
				str("fillcolor", colWaiting), str("fontcolor", colBG), "penwidth=0")
		}
	}
	for _, p := range planets {
		attrs := []string(nil)
		if flow[p.key()] {
			attrs = append(attrs, str("class", "aeye-flow"), "style=dashed")
		}
		stmt(&b, "dispatcher -> "+ids[p.key()], attrs...)
	}
	for i, w := range sc.wedges {
		wedgeDOT(&b, "w"+strconv.Itoa(i+1), w)
	}
	for _, p := range planets {
		for k, m := range p.moons {
			id := ids[p.key()] + "m" + strconv.Itoa(k+1)
			fill, extra := colEdge, []string(nil)
			if m.working {
				fill = colWorking
			}
			if glow[moonKey{p.key(), k}] {
				extra = []string{str("class", "aeye-glow")}
			}
			stmt(&b, id, append([]string{`label=""`, num("width", moonSize), num("height", moonSize), pos(m.x, m.y),
				str("fillcolor", fill), "penwidth=0"}, extra...)...)
			stmt(&b, ids[p.key()]+" -> "+id, str("color", colDone))
		}
	}
	for _, p := range sc.planets {
		if base, ok := ids[planetKey{p.repo, p.base}]; ok && p.base != p.branch {
			stmt(&b, ids[p.key()]+" -> "+base, "style=dashed", str("color", colPROpen), "arrowhead=normal", "arrowsize=0.6", "penwidth=2")
		}
	}
	for i, h := range sc.hosts {
		stmt(&b, "h"+strconv.Itoa(i+1), str("label", h.host), "shape=plaintext", "style=solid", "fixedsize=false",
			str("fontcolor", colLegend), pos(h.x, h.y))
	}
	anchorIDs := make(map[string]string, len(sc.anchors))
	for i, a := range sc.anchors {
		id := "a" + strconv.Itoa(i+1)
		anchorIDs[a.text] = id
		stmt(&b, id, str("label", a.text), "shape=plaintext", "style=solid", "fixedsize=false",
			str("fontcolor", colDim), pos(a.x, a.y))
	}
	for i, h := range sc.holds {
		id := "q" + strconv.Itoa(i+1)
		stmt(&b, id, str("label", h.label), num("width", holdSize), num("height", holdSize), "fontsize=10", pos(h.x, h.y),
			str("style", "filled,dashed"), str("color", colHolds+staleAlpha), str("fontcolor", colDim))
		if a, ok := anchorIDs[h.engine+" · "+h.window]; ok && h.engine != "" && h.window != "" {
			stmt(&b, id+" -> "+a, "style=dashed", str("color", colHolds+staleAlpha), "penwidth=1")
		}
	}
	rings := 0
	for _, p := range sc.planets {
		if p.branch != sel || (selRepo != "" && p.repoLabel != selRepo) {
			continue
		}
		rings++
		id := "sel"
		if rings > 1 {
			id += strconv.Itoa(rings)
		}
		d := p.size + 2*selectGap/72
		stmt(&b, id, `label=""`, "style=solid", num("width", d), num("height", d), pos(p.x, p.y), str("color", colSun))
	}
	if lg != nil && mode != legendNone {
		b.WriteString("  label=<" + legendHTML(*lg, mode) + ">;\n")
	}
	b.WriteString("}\n")
	return b.String()
}

// wedgeDOT draws one repo's sector under the id prefix w: the boundary spoke at
// its start, the rim arc as a polyline of invisible points, and its name. The
// last wedge ends where the first starts, so only the start gets a spoke.
func wedgeDOT(b *strings.Builder, w string, wd wedge) {
	point := func(id string, r, deg float64) {
		x, y := polar(r, deg)
		stmt(b, id, `label=""`, "style=invis", "shape=point", "width=0", pos(x, y))
	}
	point(w+"s0", spokeInner, wd.start)
	point(w+"s1", wd.rim, wd.start)
	hairline := []string{str("color", colDim+staleAlpha), num("penwidth", dividerPenwidth)}
	stmt(b, w+"s0 -> "+w+"s1", hairline...)

	lo, arc := wd.start+arcInset, wd.span-2*arcInset
	steps := max(1, int(math.Ceil(arc/arcStep)))
	for i := range steps + 1 {
		id := w + "a" + strconv.Itoa(i)
		point(id, wd.rim, lo+arc*float64(i)/float64(steps))
		if i > 0 {
			stmt(b, w+"a"+strconv.Itoa(i-1)+" -> "+id, hairline...)
		}
	}
	stmt(b, w, str("label", wd.label), "shape=plaintext", "style=solid", "fixedsize=false",
		str("fontcolor", colLegend), pos(wd.x, wd.y))
}

func planetAttrs(p placed, pulse bool) []string {
	attrs := []string{str("label", p.label), num("width", p.size), num("height", p.size), pos(p.x, p.y)}
	if p.host != "" {
		attrs = append(attrs, "shape=hexagon")
	}
	ring := func(col string) string {
		if p.stale {
			return str("color", col+staleAlpha)
		}
		return str("color", col)
	}
	switch {
	case p.failed:
		attrs = append(attrs, str("color", colFailed), str("style", "filled,dashed"), str("fillcolor", colBG), str("fontcolor", colDim))
	case p.class == classBlocked:
		attrs = append(attrs, ring(colBlocked), num("penwidth", blockedPenwidth))
		if pulse {
			attrs = append(attrs, str("class", "aeye-pulse"))
		}
	case p.class == classPROpen:
		attrs = append(attrs, ring(colPROpen))
	case p.class == classDone:
		attrs = append(attrs, str("color", colDone), str("fillcolor", colBG), str("fontcolor", colDim))
	default:
		attrs = append(attrs, ring(colWorking))
	}
	if p.stale {
		attrs = append(attrs, str("style", "filled,dashed"), str("fontcolor", colDim))
	}
	return attrs
}

// legendHTML is the graph label: the state counts, each key a dot in the
// state's own colour, then in full mode the fixed keys, which counts mode leaves
// to the legend file. It holds fixed text and integers only.
func legendHTML(lg legend, mode legendMode) string {
	key := func(col, text string) string { return `<font color="` + col + `">●</font> ` + text }
	count := func(name string, n int) string { return name + " " + strconv.Itoa(n) }
	keys := []string{
		key(colWorking, count("working", lg.working)),
		key(colBlocked, count("blocked", lg.blocked)),
		key(colPROpen, count("pr open", lg.prOpen)),
		key(colDone, count("done", lg.done)),
	}
	if lg.failed > 0 {
		keys = append(keys, key(colFailed, count("failed", lg.failed)))
	}
	if lg.holds > 0 {
		keys = append(keys, key(colHolds, count("holds", lg.holds)))
	}
	const gap = "&nbsp;&nbsp;&nbsp;&nbsp;"
	var b strings.Builder
	b.WriteString(`<table border="0" cellborder="0" cellspacing="0" cellpadding="4">` + "\n")
	b.WriteString("    <tr><td>" + strings.Join(keys, gap) + "</td></tr>\n")
	if mode == legendCounts {
		b.WriteString("  </table>")
		return b.String()
	}
	shapes := []string{
		"size = tier",
		`<font color="` + colEdge + `">●</font> small dot = role`,
		"hexagon = remote host",
	}
	if lg.holds > 0 {
		shapes = append(shapes, `<font color="`+colHolds+`">●</font> dashed outer dot = queued hold`)
	}
	stacked := `<font color="` + colPROpen + `">→</font> dashed arrow = stacked on`
	b.WriteString("    <tr><td>" + strings.Join(shapes, gap) + "</td></tr>\n")
	b.WriteString("    <tr><td>" + stacked + "</td></tr>\n")
	if markerRow(lg) {
		var markers []string
		if lg.repos > 1 {
			markers = append(markers, "wedge = repo")
		}
		if lg.waiting > 0 {
			markers = append(markers, key(colWaiting, count("waiting on you", lg.waiting)))
		}
		if lg.ci > 0 {
			markers = append(markers, key(colWorking, "ci pass"), key(colSun, "ci running"), key(colBlocked, "ci fail"), key(colDim, "no runner"))
		}
		if lg.stale > 0 {
			markers = append(markers, "dim = no status "+strconv.Itoa(int(staleStatus.Minutes()))+"m+")
		}
		b.WriteString("    <tr><td>" + strings.Join(markers, gap) + "</td></tr>\n")
	}
	b.WriteString(`    <tr><td><font color="` + colDim + `">` + stripText(lg) + "</font></td></tr>\n")
	b.WriteString("  </table>")
	return b.String()
}
