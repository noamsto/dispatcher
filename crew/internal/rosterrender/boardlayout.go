package rosterrender

import "math"

// Board geometry, in user units with y down. The canvas is boardW wide and as
// tall as the pane's aspect makes it; it grows taller only when the cards would
// otherwise drop below minScale.
const (
	boardW               = 1600.0
	minAspect, maxAspect = 0.4, 3.5

	boardPad   = 16.0
	hdrFrac    = 0.055
	hdrMin     = 56.0
	laneLabelH = 32.0
	laneGap    = 16.0
	shelfGap   = 20.0
	colHeadH   = 36.0
	colGap     = 24.0
	cardGap    = 12.0

	baseCardW = 320.0
	baseCardH = 160.0

	// minScale is the zoom threshold: a 160×80 card still sets its text at
	// minFont, readable at the viewer's default zoom. About twelve cards fit at
	// or above it on a 16:10 pane.
	minScale = 0.5
	maxScale = 6.0

	// maxCardFrac caps a card's height against the canvas and maxCardTall
	// against its own width, so a stretched card stays card-shaped instead of
	// filling a tall pane or turning into a pillar.
	maxCardFrac = 0.6
	maxCardTall = 0.9

	// fontPerH is a card's height over its font size: five text rows with
	// leading and padding.
	fontPerH         = 7.2
	minFont, maxFont = 11.0, 48.0
)

type rect struct{ x, y, w, h float64 }

// cardGeom is one card's rect; lane, col and card index it in the board.
type cardGeom struct {
	rect
	lane, col, card int
	font            float64
}

type colGeom struct {
	header rect
	cards  []cardGeom
}

// laneGeom's label is the zero rect for a lane without a label.
type laneGeom struct {
	label rect
	cols  []colGeom
}

type boardGeom struct {
	W, H  float64
	hdr   rect
	lanes []laneGeom
}

// cards is every card in reading order: lane, column, then each sub-column top
// to bottom, sub-columns left to right.
func (g boardGeom) cards() []cardGeom {
	var out []cardGeom
	for _, l := range g.lanes {
		for _, c := range l.cols {
			out = append(out, c.cards...)
		}
	}
	return out
}

func (l lane) labelH() float64 {
	if l.label == "" {
		return 0
	}
	return laneLabelH
}

// tallest is the most cards any one of cols holds.
func tallest(cols []column) int {
	n := 0
	for _, c := range cols {
		n = max(n, len(c.cards))
	}
	return n
}

// stackH is the height of rows cards of height ch stacked with their gaps.
func stackH(rows int, ch float64) float64 {
	return float64(rows)*ch + float64(rows-1)*cardGap
}

// shelfWidth is the width cols take side by side with rows cards to a
// sub-column, cards cw wide.
func shelfWidth(cols []column, rows int, cw float64) float64 {
	w := float64(len(cols)-1) * colGap
	for _, c := range cols {
		sub := ceilDiv(len(c.cards), rows)
		w += float64(sub)*cw + float64(sub-1)*cardGap
	}
	return w
}

func ceilDiv(a, b int) int { return (a + b - 1) / b }

// shelf is a run of a lane's columns laid side by side, cols[from:to], with
// the fewest rows that fit the width and the height they need.
type shelf struct {
	from, to, rows int
	need           float64
}

// laneFit wraps l's columns, in order, onto the shelves that fit width availW
// at scale s in the least height (fewer shelves on a tie). A wide pane keeps
// every column on one shelf; a tall one stacks them. ok is false when no
// split fits.
func laneFit(l lane, s, availW float64) (best []shelf, h float64, ok bool) {
	cw, ch := baseCardW*s, baseCardH*s
	n := len(l.cols)
	for mask := range 1 << (n - 1) {
		var split []shelf
		total, fits := l.labelH(), true
		for from, to := 0, 1; to <= n && fits; to++ {
			if to < n && mask&(1<<(to-1)) == 0 {
				continue
			}
			cols := l.cols[from:to]
			sh := shelf{from: from, to: to}
			for r := 1; r <= tallest(cols); r++ {
				if shelfWidth(cols, r, cw) <= availW {
					sh.rows = r
					break
				}
			}
			if fits = sh.rows > 0; fits {
				sh.need = colHeadH + stackH(sh.rows, ch)
				total += sh.need
				if len(split) > 0 {
					total += shelfGap
				}
				split = append(split, sh)
			}
			from = to
		}
		if fits && (!ok || total < h || (total == h && len(split) < len(best))) {
			best, h, ok = split, total, true
		}
	}
	return best, h, ok
}

// boardFit is every lane's shelves at scale s and the summed height they need;
// ok is false when some lane does not fit the width.
func boardFit(b board, s, availW float64) (shelves [][]shelf, need []float64, total float64, ok bool) {
	shelves, need = make([][]shelf, len(b.lanes)), make([]float64, len(b.lanes))
	for i, l := range b.lanes {
		sh, h, fits := laneFit(l, s, availW)
		if !fits {
			return nil, nil, 0, false
		}
		shelves[i], need[i] = sh, h
		total += h
	}
	return shelves, need, total, true
}

// layoutBoard places every card. It finds the largest card scale at which all
// lanes fit the canvas, gives the height left over to the lanes and their
// shelves in proportion to what they need, and stretches each shelf's cards
// across its full width and down its height, so columns stay proportional to
// their sub-columns. Past minScale the canvas grows taller instead of the cards
// shrinking further.
func layoutBoard(b board, aspect float64) boardGeom {
	aspect = min(max(aspect, minAspect), maxAspect)
	h0 := math.Round(boardW / aspect)
	hdrH := max(hdrMin, hdrFrac*h0)
	g := boardGeom{W: boardW, H: h0, hdr: rect{0, 0, boardW, hdrH}}
	if len(b.lanes) == 0 {
		return g
	}

	availW := boardW - 2*boardPad
	availH := h0 - hdrH - 2*boardPad - float64(len(b.lanes)-1)*laneGap
	fits := func(s float64) bool {
		_, _, total, ok := boardFit(b, s, availW)
		return ok && total <= availH
	}
	s := min(maxScale, maxCardFrac*h0/baseCardH)
	switch {
	case fits(s):
	case !fits(minScale):
		s = minScale
		_, _, total, _ := boardFit(b, s, availW)
		g.H = math.Ceil(g.H + total - availH)
		availH = total
	default:
		lo, hi := minScale, s
		for range 40 {
			if mid := (lo + hi) / 2; fits(mid) {
				lo = mid
			} else {
				hi = mid
			}
		}
		s = lo
	}

	shelves, need, total, _ := boardFit(b, s, availW)
	y := hdrH + boardPad
	for li, l := range b.lanes {
		laneH := need[li] + (availH-total)*need[li]/total
		lg := laneGeom{cols: make([]colGeom, len(l.cols))}
		top := y
		if l.labelH() > 0 {
			lg.label = rect{boardPad, y, availW, laneLabelH}
			top += laneLabelH
		}
		fixed, shelvesNeed := l.labelH()+float64(len(shelves[li])-1)*shelfGap, 0.0
		for _, sh := range shelves[li] {
			shelvesNeed += sh.need
		}
		for _, sh := range shelves[li] {
			h := sh.need + (laneH-fixed-shelvesNeed)*sh.need/shelvesNeed
			placeShelf(&lg, li, l.cols[sh.from:sh.to], sh, s, h, top, h0)
			top += h + shelfGap
		}
		g.lanes = append(g.lanes, lg)
		y += laneH + laneGap
	}
	return g
}

// placeShelf lays one shelf's columns into lg: the most rows its height holds,
// the width shared out per sub-column, and the cards stretched down the rows.
// A card held back by its shape caps leaves its stack centred in the shelf.
func placeShelf(lg *laneGeom, li int, cols []column, sh shelf, s, h, top, h0 float64) {
	availW := boardW - 2*boardPad
	cw, ch := baseCardW*s, baseCardH*s
	innerH := h - colHeadH
	// The epsilon keeps a shelf sized exactly for its rows from losing one.
	rows := int(math.Floor((innerH+cardGap)/(ch+cardGap) + 1e-9))
	rows = min(max(rows, sh.rows), tallest(cols))

	subs := 0
	for _, c := range cols {
		subs += ceilDiv(len(c.cards), rows)
	}
	wide := cw + (availW-shelfWidth(cols, rows, cw))/float64(subs)
	tall := min((innerH-float64(rows-1)*cardGap)/float64(rows), maxCardFrac*h0, maxCardTall*wide)
	font := min(max(tall/fontPerH, minFont), maxFont)
	top += (h - colHeadH - stackH(rows, tall)) / 2

	x := boardPad
	for i, c := range cols {
		ci := sh.from + i
		sub := ceilDiv(len(c.cards), rows)
		colW := float64(sub)*wide + float64(sub-1)*cardGap
		cg := colGeom{header: rect{x, top, colW, colHeadH}}
		for k := range c.cards {
			cx := x + float64(k/rows)*(wide+cardGap)
			cy := top + colHeadH + float64(k%rows)*(tall+cardGap)
			cg.cards = append(cg.cards, cardGeom{rect: rect{cx, cy, wide, tall}, lane: li, col: ci, card: k, font: font})
		}
		lg.cols[ci] = cg
		x += colW + colGap
	}
}
