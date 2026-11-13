package rosterrender

import (
	"fmt"
	"math"
	"testing"
)

// spread is a board of n cards dealt over lanes, each lane's columns uneven:
// the done column takes about three in seven of a lane's cards.
func spread(n, lanes int) board {
	var b board
	for i := range n {
		li, class := i%lanes, boardClasses[min(i/lanes%7, len(boardClasses)-1)]
		for len(b.lanes) <= li {
			b.lanes = append(b.lanes, lane{label: fmt.Sprintf("repo%d", len(b.lanes))})
		}
		l := &b.lanes[li]
		at := -1
		for ci, c := range l.cols {
			if c.class == class {
				at = ci
			}
		}
		if at < 0 {
			l.cols = append(l.cols, column{class: class, title: columnTitles[class]})
			at = len(l.cols) - 1
		}
		l.cols[at].cards = append(l.cols[at].cards, boardCard{planet: planet{branch: fmt.Sprint(i), class: class}})
		l.cols[at].n++
	}
	if len(b.lanes) == 1 {
		b.lanes[0].label = ""
	}
	return b
}

const eps = 1e-6

func (r rect) inside(w, h float64) bool {
	return r.x >= -eps && r.y >= -eps && r.x+r.w <= w+eps && r.y+r.h <= h+eps && r.w > 0 && r.h > 0
}

func (r rect) overlaps(o rect) bool {
	return r.x+eps < o.x+o.w && o.x+eps < r.x+r.w && r.y+eps < o.y+o.h && o.y+eps < r.y+r.h
}

func geomRects(g boardGeom) []rect {
	out := []rect{g.hdr}
	for _, l := range g.lanes {
		if l.label != (rect{}) {
			out = append(out, l.label)
		}
		for _, c := range l.cols {
			out = append(out, c.header)
			for _, k := range c.cards {
				out = append(out, k.rect)
			}
		}
	}
	return out
}

var (
	layoutCounts  = []int{1, 2, 6, 12, 30, 64}
	layoutAspects = []float64{0.6, 1.0, 1.6, 2.4}
)

func TestBoardLayoutInsideCanvasWithoutOverlap(t *testing.T) {
	for _, lanes := range []int{1, 3} {
		for _, n := range layoutCounts {
			for _, aspect := range layoutAspects {
				t.Run(fmt.Sprintf("%dlanes/%dcards/%g", lanes, n, aspect), func(t *testing.T) {
					b := spread(n, lanes)
					g := layoutBoard(b, aspect)
					if got := len(g.cards()); got != n {
						t.Fatalf("cards = %d, want %d", got, n)
					}
					rs := geomRects(g)
					for i, r := range rs {
						if !r.inside(g.W, g.H) {
							t.Errorf("rect %d %+v outside %gx%g", i, r, g.W, g.H)
						}
						for _, o := range rs[i+1:] {
							if r.overlaps(o) {
								t.Errorf("rect %+v overlaps %+v", r, o)
							}
						}
					}
				})
			}
		}
	}
}

func TestBoardLayoutReadingOrder(t *testing.T) {
	b := spread(30, 3)
	g := layoutBoard(b, 1.6)
	cards := g.cards()
	for i, c := range cards {
		if got := b.lanes[c.lane].cols[c.col].cards[c.card].branch; got == "" {
			t.Fatalf("card %d indexes nothing", i)
		}
		if i == 0 {
			continue
		}
		p := cards[i-1]
		switch {
		case p.lane != c.lane:
			if c.lane != p.lane+1 || c.col != 0 || c.card != 0 || c.y <= p.y {
				t.Errorf("lane step %+v -> %+v", p, c)
			}
		case p.col != c.col:
			// The next column sits to the right, or on a lower shelf.
			if c.col != p.col+1 || c.card != 0 || (c.x <= p.x && c.y < p.y+p.h) {
				t.Errorf("column step %+v -> %+v", p, c)
			}
		case c.card != p.card+1:
			t.Errorf("card step %+v -> %+v", p, c)
		case c.x == p.x && c.y <= p.y, c.x != p.x && (c.x <= p.x || c.y > p.y):
			t.Errorf("stack step %+v -> %+v", p.rect, c.rect)
		}
	}
}

func TestBoardLayoutLoneCardFillsTheWidth(t *testing.T) {
	for _, aspect := range append(layoutAspects, 3.5) {
		g := layoutBoard(spread(1, 1), aspect)
		c := g.cards()[0]
		if c.w < 0.8*g.W {
			t.Errorf("aspect %g: card width %g < 80%% of %g", aspect, c.w, g.W)
		}
	}
}

func TestBoardLayoutFewCardsFillTheHeight(t *testing.T) {
	for _, n := range []int{2, 3, 4, 6} {
		for _, aspect := range []float64{0.6, 1.0, 1.6} {
			g := layoutBoard(spread(n, 1), aspect)
			top, bottom := g.H, 0.0
			for _, c := range g.cards() {
				top, bottom = min(top, c.y), max(bottom, c.y+c.h)
				if c.h > maxCardTall*c.w+eps {
					t.Errorf("%d cards/%g: card %gx%g is taller than card-shaped", n, aspect, c.w, c.h)
				}
			}
			if body := g.H - g.hdr.h; bottom-top < 0.7*body {
				t.Errorf("%d cards/%g: cards span %g of a %g body", n, aspect, bottom-top, body)
			}
		}
	}
}

func TestBoardLayoutColumnWidthFollowsNeed(t *testing.T) {
	for _, n := range layoutCounts {
		for _, aspect := range layoutAspects {
			b := spread(n, 1)
			g := layoutBoard(b, aspect)
			cols := g.lanes[0].cols
			for i := range cols {
				for j := range cols {
					sameShelf := cols[i].header.y == cols[j].header.y
					if sameShelf && len(cols[i].cards) > len(cols[j].cards) && cols[i].header.w+eps < cols[j].header.w {
						t.Errorf("%d cards/%g: column of %d is %g wide, narrower than %g for %d",
							n, aspect, len(cols[i].cards), cols[i].header.w, cols[j].header.w, len(cols[j].cards))
					}
				}
			}
		}
	}
}

func TestBoardLayoutGrowsOnlyPastTheZoomThreshold(t *testing.T) {
	tall := board{lanes: []lane{{cols: []column{{class: classWorking, cards: make([]boardCard, 64)}}}}}
	cases := []struct {
		name   string
		b      board
		aspect float64
		grows  bool
	}{
		{"12 cards", spread(12, 1), 1.6, false},
		{"12 cards in 3 lanes", spread(12, 3), 1.6, false},
		{"64 cards", spread(64, 1), 1.6, false},
		{"64 cards in one column, wide pane", tall, 2.4, true},
		{"64 cards in 3 lanes, wide pane", spread(64, 3), 2.4, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := layoutBoard(tc.b, tc.aspect)
			h0 := math.Round(boardW / tc.aspect)
			c := workerCard(tc.b, g)
			switch {
			case tc.grows && (g.H <= h0 || math.Abs(c.h-minScale*baseCardH) > eps):
				t.Errorf("H %g (pane %g), card height %g: want growth at the minimum scale", g.H, h0, c.h)
			case !tc.grows && (g.H != h0 || c.h < minScale*baseCardH-eps):
				t.Errorf("H %g (pane %g), card height %g: want the pane's height", g.H, h0, c.h)
			}
		})
	}
}

func TestBoardLayoutAspectAndFont(t *testing.T) {
	cases := []struct {
		aspect, wantH float64
	}{
		{1.6, 1000},
		{10, math.Round(boardW / maxAspect)},
		{0.1, math.Round(boardW / minAspect)},
	}
	for _, tc := range cases {
		g := layoutBoard(spread(6, 1), tc.aspect)
		if g.W != boardW || g.H != tc.wantH {
			t.Errorf("aspect %g: canvas %gx%g, want %gx%g", tc.aspect, g.W, g.H, boardW, tc.wantH)
		}
		if g.hdr.h < hdrMin {
			t.Errorf("aspect %g: header %g below %g", tc.aspect, g.hdr.h, hdrMin)
		}
		for _, c := range g.cards() {
			if c.font < minFont || c.font > maxFont {
				t.Errorf("aspect %g: font %g outside %g..%g", tc.aspect, c.font, minFont, maxFont)
			}
		}
	}
	if g := layoutBoard(board{}, 1.6); len(g.lanes) != 0 || g.H != 1000 {
		t.Errorf("empty board: %+v", g)
	}
}

func TestBoardLayoutLaneLabelOnlyWhenNamed(t *testing.T) {
	if g := layoutBoard(spread(6, 1), 1.6); g.lanes[0].label != (rect{}) {
		t.Errorf("single lane has a label band %+v", g.lanes[0].label)
	}
	for i, l := range layoutBoard(spread(6, 3), 1.6).lanes {
		if l.label.h != laneLabelH {
			t.Errorf("lane %d label band %+v", i, l.label)
		}
	}
}

// TestBoardLayoutSingleCardFillsTheCanvas: with one card, the card spans the
// canvas below the header both ways.
func TestBoardLayoutSingleCardFillsTheCanvas(t *testing.T) {
	for _, aspect := range []float64{1.0, 1.5, 1.6, 2.0} {
		g := layoutBoard(spread(1, 1), aspect)
		c := g.cards()[0]
		if body := g.H - g.hdr.h; c.h < 0.85*body {
			t.Errorf("aspect %g: card %g tall in a %g body", aspect, c.h, body)
		}
		if c.w < 0.85*g.W {
			t.Errorf("aspect %g: card %g wide in a %g canvas", aspect, c.w, g.W)
		}
	}
}

func TestBoardLayoutBandHeightFollowsContent(t *testing.T) {
	b := board{lanes: []lane{{cols: []column{
		{class: classQueued, cards: []boardCard{
			{planet: planet{branch: "q1"}, kind: kindTask}, {planet: planet{branch: "q2"}, kind: kindHold},
		}, n: 2},
		{class: classWorking, cards: []boardCard{
			{planet: planet{branch: "w1", title: "A title long enough to need a second line", detail: "doing things"}},
			{planet: planet{branch: "w2", title: "Another title long enough to wrap", detail: "more"}},
		}, n: 2},
	}}}}
	for _, aspect := range []float64{1.0, 1.6, 2.4} {
		g := layoutBoard(b, aspect)
		queued, working := g.lanes[0].cols[0].cards[0], g.lanes[0].cols[1].cards[0]
		if queued.y >= working.y {
			t.Fatalf("aspect %g: queued band is not above the working band", aspect)
		}
		if queued.h >= working.h {
			t.Errorf("aspect %g: one-line queued card %g tall, not shorter than a worker card %g", aspect, queued.h, working.h)
		}
	}
}

// workerCard is the first card outside the queued column, whose cards are
// shorter than a worker's.
func workerCard(b board, g boardGeom) cardGeom {
	for li, l := range b.lanes {
		for ci, c := range l.cols {
			if c.class != classQueued {
				return g.lanes[li].cols[ci].cards[0]
			}
		}
	}
	return g.cards()[0]
}
