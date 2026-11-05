package mapgeom

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/dash/internal/kitty"
)

func TestMaxCellsMatchesKitty(t *testing.T) {
	if MaxCells != kitty.MaxCells {
		t.Fatalf("MaxCells = %d, kitty.MaxCells = %d", MaxCells, kitty.MaxCells)
	}
}

// ringGeometry places n planets on the renderer's ring: radius
// max(150, n*100/2pi) points, one every 360/n degrees clockwise from 12
// o'clock, drawn at 1.4 px per point on a 1100x900 canvas, planet radius
// 30pt.
func ringGeometry(n int) Geometry {
	const scale = 1.4
	ring := math.Max(150, float64(n)*100/(2*math.Pi))
	g := Geometry{V: Version, W: 1100, H: 900, PNG: "/tmp/map.png"}
	for i := range n {
		a := 2 * math.Pi * float64(i) / float64(n)
		g.Planets = append(g.Planets, Planet{
			Branch: fmt.Sprintf("feat/%d-x", i),
			Name:   fmt.Sprintf("w%d", i),
			CX:     float64(g.W)/2 + ring*math.Sin(a)*scale,
			CY:     float64(g.H)/2 - ring*math.Cos(a)*scale,
			R:      30 * scale,
		})
	}
	return g
}

func cellOf(p Placement, x, y float64) (int, int) {
	return int(x) / p.CellW, int(y) / p.CellH
}

func TestHitSixteenWorkers(t *testing.T) {
	g := ringGeometry(16)
	p := Fit(110, 45, 10, 20)
	if p.PxW() != g.W || p.PxH() != g.H {
		t.Fatalf("fixture px %dx%d, placement wants %dx%d", g.W, g.H, p.PxW(), p.PxH())
	}

	for i, pl := range g.Planets {
		cx, cy := cellOf(p, pl.CX, pl.CY)
		if got := Hit(g, p, cx, cy); got != i {
			t.Errorf("centre cell (%d,%d) of planet %d: Hit = %d", cx, cy, i, got)
		}
	}

	for i := range g.Planets {
		a, b := g.Planets[i], g.Planets[(i+1)%len(g.Planets)]
		cx, cy := cellOf(p, (a.CX+b.CX)/2, (a.CY+b.CY)/2)
		if got := Hit(g, p, cx, cy); got != -1 {
			t.Errorf("cell (%d,%d) between planets %d and %d: Hit = %d, want -1", cx, cy, i, (i+1)%16, got)
		}
	}
}

func TestHitMissesOutsideCircles(t *testing.T) {
	g := ringGeometry(16)
	p := Fit(110, 45, 10, 20)
	for _, c := range []struct {
		name string
		x, y int
	}{
		{"top-left margin", 0, 0},
		{"bottom-right margin", 109, 44},
		{"ring centre", 55, 22},
		{"negative x", -1, 22},
		{"negative y", 55, -1},
		{"past last column", p.Cols, 22},
		{"past last row", 55, p.Rows},
	} {
		if got := Hit(g, p, c.x, c.y); got != -1 {
			t.Errorf("%s: Hit = %d, want -1", c.name, got)
		}
	}
}

func TestHitOverlapNearestWins(t *testing.T) {
	g := Geometry{V: Version, W: 100, H: 40, Planets: []Planet{
		{Branch: "a", CX: 30, CY: 20, R: 20},
		{Branch: "b", CX: 62, CY: 20, R: 20},
	}}
	p := Placement{Cols: 10, Rows: 2, CellW: 10, CellH: 20}
	for _, c := range []struct{ cellX, want int }{
		{1, 0},
		{3, 0}, // holds a's centre
		{4, 0}, // both circles reach it; a is 10 away, b 12
		{5, 1}, // both circles reach it; a is 20 away, b 2
		{6, 1}, // holds b's centre
		{8, 1},
	} {
		if got := Hit(g, p, c.cellX, 0); got != c.want {
			t.Errorf("cellX %d: Hit = %d, want %d", c.cellX, got, c.want)
		}
	}
}

func TestHitContainedCentreBeatsNearerCircle(t *testing.T) {
	// b's centre sits in the cell, a's circle is huge and covers it too.
	g := Geometry{V: Version, W: 100, H: 20, Planets: []Planet{
		{Branch: "a", CX: 0, CY: 10, R: 60},
		{Branch: "b", CX: 55, CY: 10, R: 5},
	}}
	p := Placement{Cols: 10, Rows: 1, CellW: 10, CellH: 20}
	if got := Hit(g, p, 5, 0); got != 1 {
		t.Errorf("Hit = %d, want 1", got)
	}
}

func TestHitHexagonUsesCircumscribedCircle(t *testing.T) {
	// A pointy-top hexagon of circumradius 20 centred at (50,20) has flats at
	// x = 50 +- 17.3; the cell at x 66..76 is outside the hexagon but inside
	// its circumscribed circle.
	g := Geometry{V: Version, W: 100, H: 40, Planets: []Planet{{Branch: "r", CX: 50, CY: 20, R: 20}}}
	p := Placement{Cols: 10, Rows: 2, CellW: 10, CellH: 20}
	if got := Hit(g, p, 6, 0); got != 0 {
		t.Errorf("Hit = %d, want 0", got)
	}
}

func TestHitRescalesToRenderedImage(t *testing.T) {
	// A 4x1 block of 10x10 cells is 40x10 px, below the renderer's 64 px
	// minimum, so the PNG comes back 64x16 and is stretched over the block.
	g := Geometry{V: Version, W: 64, H: 16, Planets: []Planet{{Branch: "a", CX: 56, CY: 8, R: 3}}}
	p := Fit(4, 1, 10, 10)
	if p.PxW() != 64 {
		t.Fatalf("PxW = %d, want 64", p.PxW())
	}
	if got := Hit(g, p, 3, 0); got != 0 {
		t.Errorf("last cell: Hit = %d, want 0", got)
	}
	if got := Hit(g, p, 0, 0); got != -1 {
		t.Errorf("first cell: Hit = %d, want -1", got)
	}
}

func TestFit(t *testing.T) {
	for _, c := range []struct {
		name                 string
		boxCols, boxRows     int
		cellW, cellH         int
		cols, rows, pxW, pxH int
	}{
		{"exact", 110, 45, 10, 20, 110, 45, 1100, 900},
		{"capped at diacritics", 400, 400, 8, 8, 297, 297, 2376, 2376},
		{"minimum one cell", 0, -3, 10, 20, 1, 1, 64, 64},
		{"clamped to renderer maximum", 297, 297, 20, 40, 204, 102, 4080, 4080},
		{"below renderer minimum", 3, 2, 10, 10, 3, 2, 64, 64},
	} {
		p := Fit(c.boxCols, c.boxRows, c.cellW, c.cellH)
		if p.Cols != c.cols || p.Rows != c.rows || p.PxW() != c.pxW || p.PxH() != c.pxH {
			t.Errorf("%s: got %dx%d cells %dx%d px, want %dx%d cells %dx%d px",
				c.name, p.Cols, p.Rows, p.PxW(), p.PxH(), c.cols, c.rows, c.pxW, c.pxH)
		}
	}
}

func goodJSON() map[string]any {
	return map[string]any{
		"v": 1, "w": 800, "h": 600, "png": "/tmp/m.png",
		"planets": []any{
			map[string]any{"branch": "a", "name": "lime", "cx": 100, "cy": 100, "r": 30},
			map[string]any{"branch": "b", "name": "plum", "cx": 400.5, "cy": 300.25, "r": 30},
		},
	}
}

func planetsOf(m map[string]any) []any { return m["planets"].([]any) }

func TestParse(t *testing.T) {
	data, _ := json.Marshal(goodJSON())
	g, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if g.W != 800 || g.H != 600 || g.PNG != "/tmp/m.png" || len(g.Planets) != 2 {
		t.Fatalf("unexpected geometry %+v", g)
	}
	if p := g.Planets[1]; p.Branch != "b" || p.Name != "plum" || p.CX != 400.5 || p.CY != 300.25 || p.R != 30 {
		t.Errorf("planet 1 = %+v", p)
	}
}

func TestParseRejects(t *testing.T) {
	planet := func(branch string, cx, cy, r float64) map[string]any {
		return map[string]any{"branch": branch, "name": "n", "cx": cx, "cy": cy, "r": r}
	}
	many := make([]any, MaxPlanets+1)
	for i := range many {
		many[i] = planet(fmt.Sprintf("b%d", i), 10, 10, 5)
	}
	for _, c := range []struct {
		name   string
		mutate func(m map[string]any)
		want   string
	}{
		{"wrong version", func(m map[string]any) { m["v"] = 2 }, "version"},
		{"missing version", func(m map[string]any) { delete(m, "v") }, "version"},
		{"zero width", func(m map[string]any) { m["w"] = 0 }, "image size"},
		{"negative height", func(m map[string]any) { m["h"] = -5 }, "image size"},
		{"empty branch", func(m map[string]any) { planetsOf(m)[0] = planet("", 10, 10, 5) }, "no branch"},
		{"duplicate branch", func(m map[string]any) { planetsOf(m)[1] = planet("a", 10, 10, 5) }, "duplicate"},
		{"zero radius", func(m map[string]any) { planetsOf(m)[0] = planet("a", 10, 10, 0) }, "bad circle"},
		{"negative radius", func(m map[string]any) { planetsOf(m)[0] = planet("a", 10, 10, -1) }, "bad circle"},
		{"centre left of image", func(m map[string]any) { planetsOf(m)[0] = planet("a", -1, 10, 5) }, "outside"},
		{"centre right of image", func(m map[string]any) { planetsOf(m)[0] = planet("a", 801, 10, 5) }, "outside"},
		{"centre above image", func(m map[string]any) { planetsOf(m)[0] = planet("a", 10, -0.5, 5) }, "outside"},
		{"centre below image", func(m map[string]any) { planetsOf(m)[0] = planet("a", 10, 601, 5) }, "outside"},
		{"too many planets", func(m map[string]any) { m["planets"] = many }, "max"},
	} {
		m := goodJSON()
		c.mutate(m)
		data, _ := json.Marshal(m)
		if _, err := Parse(data); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want containing %q", c.name, err, c.want)
		}
	}

	for _, raw := range []string{"", "not json", `{"v":1,"w":"x"}`, `{"v":1,"w":10,"h":10,"planets":[{"branch":"a","cx":1,"cy":1,"r":1e999}]}`} {
		if _, err := Parse([]byte(raw)); err == nil {
			t.Errorf("Parse(%q) accepted", raw)
		}
	}
}

func TestParseAcceptsMaxPlanetsAndEmpty(t *testing.T) {
	m := goodJSON()
	planets := make([]any, MaxPlanets)
	for i := range planets {
		planets[i] = map[string]any{"branch": fmt.Sprintf("b%d", i), "name": "n", "cx": 10, "cy": 10, "r": 5}
	}
	m["planets"] = planets
	data, _ := json.Marshal(m)
	if g, err := Parse(data); err != nil || len(g.Planets) != MaxPlanets {
		t.Errorf("64 planets: len %d, err %v", len(g.Planets), err)
	}

	if g, err := Parse([]byte(`{"v":1,"w":10,"h":10,"png":"p","planets":[]}`)); err != nil || len(g.Planets) != 0 {
		t.Errorf("no planets: %+v, %v", g, err)
	}
}
