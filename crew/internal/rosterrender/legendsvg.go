package rosterrender

import (
	"encoding/xml"
	"fmt"
	"strconv"
	"strings"
)

const (
	keyMargin = 24.0
	keyRowH   = 30.0
	keyTierH  = 44.0
	keyGapH   = 12.0
	keyMinW   = 520.0
	keyFont   = 14

	// keySymX is the centre of a row's symbol and keyTextX where its text starts.
	keySymX  = keyMargin + 20
	keyTextX = keySymX + 28

	// keyScale draws the tier circles at half their size in the diagram, which
	// keeps the key short and the three sizes in their true ratio.
	keyScale   = 0.5
	keyTierGap = 40.0
	keyRing    = 11.0
)

// keyRow is one line of the key: a symbol drawn around its centre, then text.
type keyRow struct {
	h     float64
	sym   func(cx, cy float64) string
	text  string
	textX float64
}

// legendTarget is where the full key of the diagram at diagram lives: its
// name with `.svg` replaced by `.legend.svg`, in the same directory.
func legendTarget(diagram string) string {
	return strings.TrimSuffix(diagram, ".svg") + ".legend.svg"
}

func xmlText(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

func svgNum(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

func keyCircle(cx, cy, r float64, fill, stroke string, pen float64, extra string) string {
	return fmt.Sprintf(`<circle cx="%s" cy="%s" r="%s" fill="%s" stroke="%s" stroke-width="%s"%s/>`,
		svgNum(cx), svgNum(cy), svgNum(r), fill, stroke, svgNum(pen), extra)
}

// fadedRing is the dashed, translucent stroke the diagram draws for the dim of a
// stale worker and for a queued hold.
const fadedRing = ` stroke-dasharray="4 3" stroke-opacity="0.33"`

// legendSVG is the full key as a standalone svg: every symbol the diagram draws
// with its meaning, and the budget strip, which changes with the bus. The marker
// keys follow the same conditions the full graph legend gives them.
func legendSVG(lg legend) string {
	state := func(ring, fill string, pen float64, extra string) func(cx, cy float64) string {
		return func(cx, cy float64) string { return keyCircle(cx, cy, keyRing, fill, ring, pen, extra) }
	}
	badge := func(fill, stroke string, pen float64, extra string) func(cx, cy float64) string {
		return func(cx, cy float64) string {
			return keyCircle(cx, cy, ciBadgeRadius, fill, stroke, pen, extra)
		}
	}
	rows := []keyRow{
		{sym: state(colWorking, colFill, planetPenwidth, ""), text: "working"},
		{sym: state(colBlocked, colFill, blockedPenwidth, ""), text: "blocked"},
		{sym: state(colPROpen, colFill, planetPenwidth, ""), text: "pr open"},
		{sym: state(colDone, colBG, planetPenwidth, ""), text: "done"},
		{sym: state(colFailed, colBG, planetPenwidth, ` stroke-dasharray="4 3"`), text: "failed"},
		{h: keyTierH, text: "size = tier: deep, standard, trivial", textX: keySymX + 100, sym: func(cx, cy float64) string {
			var b strings.Builder
			x := cx
			for _, tier := range []string{"deep", "standard", "trivial"} {
				r := tierSize[tier] * 72 / 2 * keyScale
				b.WriteString(keyCircle(x, cy, r, colFill, colLegend, planetPenwidth, ""))
				x += keyTierGap
			}
			return b.String()
		}},
		{text: "small dot = role", sym: func(cx, cy float64) string {
			return keyCircle(cx, cy, moonSize*72/2, colEdge, colEdge, 0, "")
		}},
		{text: "hexagon = remote host", sym: func(cx, cy float64) string {
			const dx, dy = keyRing, keyRing * 0.866
			pts := [][2]float64{{-dx, 0}, {-dx / 2, -dy}, {dx / 2, -dy}, {dx, 0}, {dx / 2, dy}, {-dx / 2, dy}}
			var p []string
			for _, v := range pts {
				p = append(p, svgNum(cx+v[0])+","+svgNum(cy+v[1]))
			}
			return `<polygon points="` + strings.Join(p, " ") + `" fill="` + colFill + `" stroke="` + colLegend +
				`" stroke-width="` + svgNum(planetPenwidth) + `"/>`
		}},
		{text: "dashed arrow = stacked on", sym: func(cx, cy float64) string {
			return fmt.Sprintf(`<line x1="%s" y1="%s" x2="%s" y2="%s" stroke="%s" stroke-width="2" stroke-dasharray="5 3"/>`+
				`<polygon points="%s,%s %s,%s %s,%s" fill="%s"/>`,
				svgNum(cx-14), svgNum(cy), svgNum(cx+8), svgNum(cy), colPROpen,
				svgNum(cx+15), svgNum(cy), svgNum(cx+6), svgNum(cy-4.5), svgNum(cx+6), svgNum(cy+4.5), colPROpen)
		}},
	}
	if lg.holds > 0 {
		rows = append(rows, keyRow{text: "dashed outer dot = queued hold", sym: state(colHolds, colFill, planetPenwidth, fadedRing)})
	}
	if lg.repos > 1 {
		rows = append(rows, keyRow{text: "wedge = repo", sym: func(cx, cy float64) string {
			return fmt.Sprintf(`<path d="M %s %s L %s %s A 22 22 0 0 1 %s %s Z" fill="none" stroke="%s" stroke-width="1.5"/>`,
				svgNum(cx-10), svgNum(cy+10), svgNum(cx-10), svgNum(cy-12), svgNum(cx+12), svgNum(cy+10), colDim)
		}})
	}
	if lg.waiting > 0 {
		rows = append(rows, keyRow{text: "waiting on you", sym: func(cx, cy float64) string {
			return keyCircle(cx, cy, keyRing, colFill, colWaiting, waitPenwidth, ` stroke-dasharray="1 4" stroke-linecap="round"`)
		}})
	}
	if lg.ci > 0 {
		rows = append(rows,
			keyRow{text: "ci pass", sym: badge(colWorking, colWorking, 0, "")},
			keyRow{text: "ci running", sym: badge(colBG, colSun, 2, "")},
			keyRow{text: "ci fail", sym: func(cx, cy float64) string {
				return keyCircle(cx, cy, ciBadgeRadius, colBlocked, colBlocked, 0, "") +
					fmt.Sprintf(`<text x="%s" y="%s" fill="%s" font-size="9" text-anchor="middle">!</text>`, svgNum(cx), svgNum(cy+3), colBG)
			}},
			keyRow{text: "no runner", sym: badge(colBG, colDim, 1, ` stroke-dasharray="2 2"`)},
		)
	}
	if lg.stale > 0 {
		rows = append(rows, keyRow{text: "dim = no status " + strconv.Itoa(int(staleStatus.Minutes())) + "m+",
			sym: state(colWorking, colFill, planetPenwidth, fadedRing)})
	}

	var body strings.Builder
	y, width := keyMargin, keyMinW
	for _, r := range rows {
		h := r.h
		if h == 0 {
			h = keyRowH
		}
		textX := r.textX
		if textX == 0 {
			textX = keyTextX
		}
		cy := y + h/2
		body.WriteString(r.sym(keySymX, cy))
		fmt.Fprintf(&body, `<text x="%s" y="%s" fill="%s">%s</text>`+"\n", svgNum(textX), svgNum(cy+5), colLegend, xmlText(r.text))
		width = max(width, textX+labelWidth(r.text)+keyMargin)
		y += h
	}
	strip := stripText(lg)
	y += keyGapH
	fmt.Fprintf(&body, `<text x="%s" y="%s" fill="%s">%s</text>`+"\n", svgNum(keyMargin), svgNum(y+keyRowH/2+5), colDim, xmlText(strip))
	width = max(width, keyMargin+labelWidth(strip)+keyMargin)
	y += keyRowH + keyMargin

	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" width="%[1]s" height="%[2]s" viewBox="0 0 %[1]s %[2]s" font-family="'%[3]s'" font-size="%[4]d">`+"\n",
		svgNum(width), svgNum(y), diagramFont, keyFont)
	fmt.Fprintf(&b, `<rect x="0" y="0" width="%s" height="%s" fill="%s"/>`+"\n", svgNum(width), svgNum(y), colBG)
	b.WriteString(body.String())
	b.WriteString("</svg>\n")
	return b.String()
}
