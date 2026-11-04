package rosterrender

import (
	"encoding/base64"
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
	legendH   = 50.0
)

// dotq quotes s as a DOT string.
func dotq(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s) + `"`
}

// wrapper is the one-node D2 file that carries the svg into aeye, which only
// publishes .d2 sources. The picture is in the text as a data URI, so aeye's
// content-addressed cache sees every change to it.
func wrapper(svg []byte) string {
	return `diagram: "" {shape: image; icon: "data:image/svg+xml;base64,` + base64.StdEncoding.EncodeToString(svg) + `"; width: 900; height: 900}` + "\n"
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

// renderDOT is the whole picture as DOT for `neato -n`: every node is pinned, so
// Graphviz only draws. Ids are generated from the branch order (p1, p1m1, h1),
// never from user text, and every string goes through dotq.
func renderDOT(sc scene, lg legend) string {
	var b strings.Builder
	b.WriteString("digraph roster {\n")
	b.WriteString("  layout=neato; splines=false; outputorder=edgesfirst; pad=0.2; labelloc=b;\n")
	b.WriteString("  " + str("bgcolor", colBG) + "; " + str("fontname", "sans-serif") + "; fontsize=10; " + str("fontcolor", colLegend) + ";\n")
	b.WriteString("  node [shape=circle style=filled " + str("fontname", "sans-serif") + " fontsize=10 " +
		str("fontcolor", colText) + " " + str("fillcolor", colFill) + " penwidth=3 fixedsize=true];\n")
	b.WriteString("  edge [" + str("color", colEdge) + " penwidth=1.5 arrowhead=none];\n")

	half := sc.extent + canvasPad
	wide := half + legendH/2
	for i, c := range [4][2]float64{{-wide, half}, {wide, half}, {-wide, -half}, {wide, -half}} {
		stmt(&b, "c"+strconv.Itoa(i+1), `label=""`, "style=invis", "shape=point", "width=0", pos(c[0], c[1]))
	}
	stmt(&b, "dispatcher", str("label", "dispatcher"), num("width", sunSize), num("height", sunSize), pos(0, 0),
		str("color", colSun), str("fillcolor", colDone))

	ids := make(map[string]string, len(sc.planets))
	for i, p := range sc.planets {
		ids[p.branch] = "p" + strconv.Itoa(i+1)
	}
	for _, p := range sc.planets {
		stmt(&b, ids[p.branch], planetAttrs(p)...)
	}
	for _, p := range sc.planets {
		attrs := []string(nil)
		if p.class == classWorking {
			attrs = append(attrs, str("class", "aeye-flow"))
		}
		stmt(&b, "dispatcher -> "+ids[p.branch], attrs...)
	}
	for _, p := range sc.planets {
		for k, m := range p.moons {
			id := ids[p.branch] + "m" + strconv.Itoa(k+1)
			fill, extra := colEdge, []string(nil)
			if m.working {
				fill, extra = colWorking, []string{str("class", "aeye-glow")}
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
	b.WriteString("  label=<" + legendHTML(lg) + ">;\n}\n")
	return b.String()
}

func planetAttrs(p placed) []string {
	attrs := []string{str("label", p.label), num("width", p.size), num("height", p.size), pos(p.x, p.y)}
	if p.host != "" {
		attrs = append(attrs, "shape=hexagon")
	}
	switch {
	case p.failed:
		attrs = append(attrs, str("color", colFailed), str("style", "filled,dashed"), str("fillcolor", colBG), str("fontcolor", colDim))
	case p.class == classBlocked:
		attrs = append(attrs, str("color", colBlocked), "penwidth=5", str("class", "aeye-pulse"))
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
	static := []string{
		"size = tier",
		`<font color="` + colEdge + `">●</font> small dot = role`,
		`<font color="` + colPROpen + `">⇢</font> dashed arrow = stacked on`,
		"⬡ = remote host",
	}
	const gap = "&nbsp;&nbsp;&nbsp;&nbsp;"
	var b strings.Builder
	b.WriteString(`<table border="0" cellborder="0" cellspacing="0" cellpadding="4">` + "\n")
	b.WriteString("    <tr><td>" + strings.Join(keys, gap) + "</td></tr>\n")
	b.WriteString("    <tr><td>" + strings.Join(static, gap) + "</td></tr>\n")
	b.WriteString("  </table>")
	return b.String()
}
