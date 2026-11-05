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
)

const (
	moonSize = 0.16
	sunSize  = 1.0

	// canvasPad is the margin between the outermost thing and the invisible
	// corners; legendH is the height the legend adds under the canvas, which the
	// corners pre-pay in width so the picture comes out square.
	canvasPad = 24.0
	legendH   = 74.0

	// planetPenwidth is every node's ring width; a blocked planet's is heavier.
	// The map's hit radius includes half of it.
	planetPenwidth  = 3.0
	blockedPenwidth = 5.0

	// selectGap is how far the selection ring stands off a planet's edge.
	selectGap = 9.0

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
// because aeye zooms by rewriting the viewBox, goes in first. Text without a root
// viewBox is not an svg this knows, and comes back unchanged.
func finishSVG(svg []byte) []byte {
	m := svgRootRe.FindSubmatchIndex(svg)
	if m == nil {
		return svg
	}
	num := func(i int) string { return string(svg[m[2*i]:m[2*i+1]]) }
	rect := fmt.Sprintf(`<rect x="%s" y="%s" width="%s" height="%s" fill="%s"/>`, num(1), num(2), num(3), num(4), colBG)
	out := slices.Concat(svg[:m[1]], []byte(rect), svg[m[1]:])
	out = bytes.ReplaceAll(out, []byte("aeye&#45;"), []byte("aeye-"))
	return bytes.ReplaceAll(out, []byte(`font-family="`+diagramFont+`"`), []byte(`font-family="'`+diagramFont+`'"`))
}

// maxAnimated caps each animated class. Motion should mark what is live and
// recent; past about eight elements it turns into noise and costs per-frame
// render time. The legend counts carry the totals.
const maxAnimated = 8

type moonKey struct {
	branch string
	k      int
}

// animatedSets picks what moves: flow spokes of working planets, pulses of
// blocked planets, glows of working moons. Planets rank newest first, ties by
// branch, and the glow cap counts moons, so the last planet may be taken partly.
func animatedSets(planets []placed) (flow, pulse map[string]bool, glow map[moonKey]bool) {
	ranked := slices.Clone(planets)
	slices.SortFunc(ranked, func(x, y placed) int {
		return cmp.Or(cmp.Compare(y.ts, x.ts), strings.Compare(x.branch, y.branch))
	})
	flow, pulse, glow = map[string]bool{}, map[string]bool{}, map[moonKey]bool{}
	for _, p := range ranked {
		switch {
		case p.class == classWorking && len(flow) < maxAnimated:
			flow[p.branch] = true
		case p.class == classBlocked && len(pulse) < maxAnimated:
			pulse[p.branch] = true
		}
		for k, m := range p.moons {
			if m.working && len(glow) < maxAnimated {
				glow[moonKey{p.branch, k}] = true
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

// renderDOT is the diagram with its legend.
func renderDOT(sc scene, lg legend) string { return buildDOT(sc, &lg, "") }

// mapHalf is the half extent of the map's canvas around the dispatcher, wide
// enough for the host labels.
func mapHalf(sc scene) (halfX, halfY float64) {
	half := sc.extent + canvasPad
	halfX, halfY = half, half
	for _, h := range sc.hosts {
		w := max(hostMinWidth, float64(len(h.host))*hostCharWidth+hostMargin)
		halfX = max(halfX, math.Abs(h.x)+w/2)
	}
	return halfX, halfY
}

// buildDOT is the whole picture as DOT for `neato -n`: every node is pinned, so
// Graphviz only draws. Ids are generated from the branch order (p1, p1m1, h1),
// never from user text, and every string goes through dotq.
//
// A nil legend is the map: no graph label, so the corners are the canvas and
// the layout's coordinates map onto it by a constant offset, and a ring around
// the planet sel names, when there is one.
func buildDOT(sc scene, lg *legend, sel string) string {
	var b strings.Builder
	b.WriteString("digraph roster {\n")
	b.WriteString("  layout=neato; splines=false; outputorder=edgesfirst; pad=0.2; labelloc=b;\n")
	b.WriteString("  " + str("bgcolor", colBG) + "; " + str("fontname", diagramFont) + "; fontsize=13; " + str("fontcolor", colLegend) + ";\n")
	b.WriteString("  node [shape=circle style=filled " + str("fontname", diagramFont) + " fontsize=12 " +
		str("fontcolor", colText) + " " + str("fillcolor", colFill) + " " + num("penwidth", planetPenwidth) + " fixedsize=true];\n")
	b.WriteString("  edge [" + str("color", colEdge) + " penwidth=1.5 arrowhead=none];\n")

	half := sc.extent + canvasPad
	halfX, halfY := half+legendH/2, half
	if lg == nil {
		halfX, halfY = mapHalf(sc)
	}
	for i, c := range [4][2]float64{{-halfX, halfY}, {halfX, halfY}, {-halfX, -halfY}, {halfX, -halfY}} {
		stmt(&b, "c"+strconv.Itoa(i+1), `label=""`, "style=invis", "shape=point", "width=0", pos(c[0], c[1]))
	}
	stmt(&b, "dispatcher", str("label", "dispatcher"), num("width", sunSize), num("height", sunSize), pos(0, 0),
		str("color", colSun), str("fillcolor", colDone))

	planets := slices.Clone(sc.planets)
	slices.SortFunc(planets, func(x, y placed) int { return strings.Compare(x.branch, y.branch) })
	ids := make(map[string]string, len(planets))
	for i, p := range planets {
		ids[p.branch] = "p" + strconv.Itoa(i+1)
	}
	flow, pulse, glow := animatedSets(planets)
	for _, p := range planets {
		stmt(&b, ids[p.branch], planetAttrs(p, pulse[p.branch])...)
	}
	for _, p := range planets {
		attrs := []string(nil)
		if flow[p.branch] {
			attrs = append(attrs, str("class", "aeye-flow"))
		}
		stmt(&b, "dispatcher -> "+ids[p.branch], attrs...)
	}
	for _, p := range planets {
		for k, m := range p.moons {
			id := ids[p.branch] + "m" + strconv.Itoa(k+1)
			fill, extra := colEdge, []string(nil)
			if m.working {
				fill = colWorking
			}
			if glow[moonKey{p.branch, k}] {
				extra = []string{str("class", "aeye-glow")}
			}
			stmt(&b, id, append([]string{`label=""`, num("width", moonSize), num("height", moonSize), pos(m.x, m.y),
				str("fillcolor", fill), "penwidth=0"}, extra...)...)
			stmt(&b, ids[p.branch]+" -> "+id, str("color", colDone))
		}
	}
	for _, p := range sc.planets {
		if base, ok := ids[p.base]; ok && p.base != p.branch {
			stmt(&b, ids[p.branch]+" -> "+base, "style=dashed", str("color", colPROpen), "arrowhead=normal", "arrowsize=0.6", "penwidth=2")
		}
	}
	for i, h := range sc.hosts {
		stmt(&b, "h"+strconv.Itoa(i+1), str("label", h.host), "shape=plaintext", "style=solid", "fixedsize=false",
			str("fontcolor", colLegend), pos(h.x, h.y))
	}
	for _, p := range sc.planets {
		if p.branch != sel {
			continue
		}
		d := p.size + 2*selectGap/72
		stmt(&b, "sel", `label=""`, "style=solid", num("width", d), num("height", d), pos(p.x, p.y), str("color", colSun))
	}
	if lg != nil {
		b.WriteString("  label=<" + legendHTML(*lg) + ">;\n")
	}
	b.WriteString("}\n")
	return b.String()
}

func planetAttrs(p placed, pulse bool) []string {
	attrs := []string{str("label", p.label), num("width", p.size), num("height", p.size), pos(p.x, p.y)}
	if p.host != "" {
		attrs = append(attrs, "shape=hexagon")
	}
	switch {
	case p.failed:
		attrs = append(attrs, str("color", colFailed), str("style", "filled,dashed"), str("fillcolor", colBG), str("fontcolor", colDim))
	case p.class == classBlocked:
		attrs = append(attrs, str("color", colBlocked), num("penwidth", blockedPenwidth))
		if pulse {
			attrs = append(attrs, str("class", "aeye-pulse"))
		}
	case p.class == classPROpen:
		attrs = append(attrs, str("color", colPROpen))
	case p.class == classDone:
		attrs = append(attrs, str("color", colDone), str("fillcolor", colBG), str("fontcolor", colDim))
	default:
		attrs = append(attrs, str("color", colWorking))
	}
	return attrs
}

// legendHTML is the graph label: the state counts, each key a dot in the
// state's own colour, then the fixed keys. It holds fixed text and integers only.
func legendHTML(lg legend) string {
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
	shapes := []string{
		"size = tier",
		`<font color="` + colEdge + `">●</font> small dot = role`,
		"hexagon = remote host",
	}
	stacked := `<font color="` + colPROpen + `">→</font> dashed arrow = stacked on`
	const gap = "&nbsp;&nbsp;&nbsp;&nbsp;"
	var b strings.Builder
	b.WriteString(`<table border="0" cellborder="0" cellspacing="0" cellpadding="4">` + "\n")
	b.WriteString("    <tr><td>" + strings.Join(keys, gap) + "</td></tr>\n")
	b.WriteString("    <tr><td>" + strings.Join(shapes, gap) + "</td></tr>\n")
	b.WriteString("    <tr><td>" + stacked + "</td></tr>\n")
	b.WriteString("  </table>")
	return b.String()
}
