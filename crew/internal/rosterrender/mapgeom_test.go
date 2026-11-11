package rosterrender

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"maps"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/drift"
)

// needTools ends the test when the real renderer is not here: a skip in the
// nix build sandbox, a failure under CREW_DRIFT_REQUIRED.
func needTools(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"neato", "resvg"} {
		if _, err := exec.LookPath(tool); err != nil {
			drift.Missing(t, tool, err)
		}
	}
	if os.Getenv("AEYE_D2_FONT_DIR") == "" {
		drift.Missing(t, "AEYE_D2_FONT_DIR", errors.New("not set"))
	}
}

var translateRe = regexp.MustCompile(`translate\(([-0-9.]+)[ ,]([-0-9.]+)\)`)

// svgCentres reads each planet's centre off the real svg, in viewBox units: an
// ellipse's cx, cy or the middle of a polygon's bounding box, under the graph's
// translate.
func svgCentres(t *testing.T, svg []byte, sc scene) map[string][2]float64 {
	t.Helper()
	root := parseSVG(t, svg)
	var tx, ty float64
	byID := map[string][2]float64{}
	root.walk(func(n svgNode) {
		// An animated node's class is "node aeye-pulse".
		switch strings.Fields(n.attr("class") + " -")[0] {
		case "graph":
			m := translateRe.FindStringSubmatch(n.attr("transform"))
			if m == nil {
				t.Fatalf("graph has no translate: %q", n.attr("transform"))
			}
			tx, _ = strconv.ParseFloat(m[1], 64)
			ty, _ = strconv.ParseFloat(m[2], 64)
		case "node":
			if e, ok := n.child("ellipse"); ok {
				cx, _ := strconv.ParseFloat(e.attr("cx"), 64)
				cy, _ := strconv.ParseFloat(e.attr("cy"), 64)
				byID[n.title()] = [2]float64{cx, cy}
			}
			// A card is a rounded box, which Graphviz draws as a path.
			var nums []string
			if p, ok := n.child("polygon"); ok {
				nums = pathNumRe.FindAllString(p.attr("points"), -1)
			} else if p, ok := n.child("path"); ok {
				nums = pathNumRe.FindAllString(p.attr("d"), -1)
			}
			if len(nums) >= 2 {
				x0, y0, x1, y1 := math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)
				for i := 0; i+1 < len(nums); i += 2 {
					x, _ := strconv.ParseFloat(nums[i], 64)
					y, _ := strconv.ParseFloat(nums[i+1], 64)
					x0, x1, y0, y1 = min(x0, x), max(x1, x), min(y0, y), max(y1, y)
				}
				byID[n.title()] = [2]float64{(x0 + x1) / 2, (y0 + y1) / 2}
			}
		}
	})
	out := map[string][2]float64{}
	for branch, id := range idOf(sc) {
		c, ok := byID[id]
		if !ok {
			t.Fatalf("no node %s for %s in the svg", id, branch)
		}
		out[branch] = [2]float64{c[0] + tx, c[1] + ty}
	}
	return out
}

// mapRender is the whole map path on a hand-built roster with the real neato and
// resvg: the geometry, the svg it came from and the decoded image.
type mapRender struct {
	sc      scene
	svg     []byte
	vbW     float64
	vbH     float64
	planets []mapPlanet
	img     image.Image
}

func renderMap(t *testing.T, ws []dotWorker, w, h int, sel string) mapRender {
	t.Helper()
	ps, _ := buildPlanets(dotModel(ws, 0), dotNow)
	_, sc := arrange(newSlotState(), ps)
	return renderScene(t, sc, w, h, sel)
}

func renderScene(t *testing.T, sc scene, w, h int, sel string) mapRender {
	t.Helper()
	needTools(t)
	ctx := context.Background()
	svg, err := Default().Graphviz(ctx, []byte(buildDOT(sc, nil, legendNone, sel, "")))
	if err != nil {
		t.Fatal(err)
	}
	svg = finishSVG(svg, sc.wedges)
	vbW, vbH, ok := viewBoxSize(svg)
	if !ok {
		t.Fatal("the svg has no viewBox")
	}
	data, err := Default().Resvg(ctx, svg, w, h)
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	halfX, halfY := mapHalf(sc)
	return mapRender{sc, svg, vbW, vbH, mapGeometry(sc, halfX, halfY, vbW, vbH, w, h), img}
}

// checkCentres compares every planet's pixel centre with the one the real svg
// puts it at, letterboxed into the w x h image the way resvg centres it.
func checkCentres(t *testing.T, r mapRender, w, h int) {
	t.Helper()
	if b := r.img.Bounds(); b.Dx() != w || b.Dy() != h {
		t.Fatalf("png is %dx%d, want %dx%d", b.Dx(), b.Dy(), w, h)
	}
	s := min(float64(w)/r.vbW, float64(h)/r.vbH)
	ox, oy := (float64(w)-r.vbW*s)/2, (float64(h)-r.vbH*s)/2
	centres := svgCentres(t, r.svg, r.sc)
	if len(r.planets) != len(r.sc.planets) {
		t.Fatalf("%d planets, want %d", len(r.planets), len(r.sc.planets))
	}
	for i, p := range r.planets {
		if p.Branch != r.sc.planets[i].branch || p.Name != r.sc.planets[i].name {
			t.Errorf("planet %d is %s/%s, want ring order %s/%s", i, p.Branch, p.Name, r.sc.planets[i].branch, r.sc.planets[i].name)
		}
		c := centres[p.Branch]
		wantX, wantY := ox+c[0]*s, oy+c[1]*s
		if math.Abs(p.Cx-wantX) > 1.5 || math.Abs(p.Cy-wantY) > 1.5 {
			t.Errorf("%s at (%.2f, %.2f), the svg puts it at (%.2f, %.2f)", p.Branch, p.Cx, p.Cy, wantX, wantY)
		}
	}
}

// checkRings reads the rendered image at the left and right edges of each round
// planet's ring: something other than the background is drawn there.
func checkRings(t *testing.T, r mapRender) {
	t.Helper()
	bg := [3]int{0x1e, 0x1e, 0x2e}
	for i, p := range r.planets {
		pl := r.sc.planets[i]
		if pl.failed {
			continue
		}
		// The side borders at mid height: the card's own half width, which the
		// hit rectangle is at least as wide as.
		hw, _ := hitRect(pl)
		edge := cardW * 72 / 2 * p.HW / hw
		for _, dx := range []float64{-edge, edge} {
			// A light tier's border is under a pixel wide, so the best of the
			// pixels around the line is the one to judge.
			best := 0
			for near := -1; near <= 1; near++ {
				cr, cg, cb, _ := r.img.At(int(math.Round(p.Cx+dx))+near, int(math.Round(p.Cy))).RGBA()
				got := [3]int{int(cr >> 8), int(cg >> 8), int(cb >> 8)}
				dist := 0
				for k := range got {
					dist += int(math.Abs(float64(got[k] - bg[k])))
				}
				best = max(best, dist)
			}
			if best < 60 {
				t.Errorf("%s: no border pixel %.1f px from the centre", p.Branch, math.Abs(dx))
			}
		}
	}
}

func TestMapGeometryMatchesTheRealRender(t *testing.T) {
	for _, tc := range []struct {
		name string
		w, h int
	}{
		{"square", 800, 800},
		{"wide", 1200, 500},
		{"tall", 400, 900},
		{"small", 160, 120},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := renderMap(t, sixteenWorkers(), tc.w, tc.h, "")
			checkCentres(t, r, tc.w, tc.h)
			if tc.w == tc.h {
				checkRings(t, r)
			}
		})
	}
}

// TestMapGeometryWithWideHostLabels: every planet is remote, five on one host
// and three on another, so the two labels sit at 3 and 9 o'clock with different
// widths. The bounding box only equals the corners when
// the corners hold the whole label.
func TestMapGeometryWithWideHostLabels(t *testing.T) {
	long, short := "alpha-host-name-24-chars", "omega-12-chr"
	if len(long) != 24 {
		t.Fatalf("%q is %d characters, want 24", long, len(long))
	}
	var ws []dotWorker
	for i := range 8 {
		w := dotWorker{branch: "feat/" + strconv.Itoa(i+1) + "-x", name: "w" + strconv.Itoa(i+1), tier: "standard", state: "working", age: 10, host: short}
		if i < 5 {
			w.host = long
		}
		ws = append(ws, w)
	}
	for _, size := range [][2]int{{900, 900}, {1000, 400}} {
		r := renderMap(t, ws, size[0], size[1], "")
		if len(r.sc.hosts) != 2 {
			t.Fatalf("%d host labels, want 2", len(r.sc.hosts))
		}
		halfX, _ := mapHalf(r.sc)
		if half := r.sc.extent + canvasPad; halfX <= half {
			t.Fatalf("the labels do not widen the canvas (%v <= %v), so this case proves nothing", halfX, half)
		}
		checkCentres(t, r, size[0], size[1])
	}
}

func TestMapHalfHoldsAHostLabel(t *testing.T) {
	sc := scene{extent: 300, hosts: []hostLabel{{host: strings.Repeat("h", 24), x: 280, y: 10}}}
	halfX, halfY := mapHalf(sc)
	if want := 280 + (24*hostCharWidth+hostMargin)/2; halfX != want {
		t.Errorf("halfX = %v, want %v", halfX, want)
	}
	if want := 300 + canvasPad; halfY != want {
		t.Errorf("halfY = %v, want %v", halfY, want)
	}
	if x, y := mapHalf(scene{extent: 300}); x != y || x != 300+canvasPad {
		t.Errorf("without hosts the canvas is square: %v x %v", x, y)
	}
}

func TestHitRect(t *testing.T) {
	const cw, ch = cardW * 72 / 2, cardH * 72 / 2
	for _, tc := range []struct {
		name   string
		mod    func(*placed)
		hw, hh float64
	}{
		{"plain", func(*placed) {}, cw + planetPenwidth/2, ch + planetPenwidth/2},
		{"deep", func(p *placed) { p.tier = "deep" }, cw + 4.5/2, ch + 4.5/2},
		{"trivial", func(p *placed) { p.tier = "trivial" }, cw + 1.75/2, ch + 1.75/2},
		{"blocked is no heavier", func(p *placed) { p.class = classBlocked }, cw + planetPenwidth/2, ch + planetPenwidth/2},
		{"waiting", func(p *placed) { p.class, p.waiting = classBlocked, true }, cw + waitGap + waitPenwidth/2, ch + waitGap + waitPenwidth/2},
		{"ci pass", func(p *placed) { p.ci = "pass" }, cw + ciBadgeRadius, ch + ciBadgeRadius},
		{"ci pending", func(p *placed) { p.ci = "pending" }, cw + ciBadgeRadius + 1, ch + ciBadgeRadius + 1},
		{"ci none", func(p *placed) { p.ci = "none" }, cw + ciBadgeRadius + 0.5, ch + ciBadgeRadius + 0.5},
		{"unknown ci", func(p *placed) { p.ci = "bogus" }, cw + planetPenwidth/2, ch + planetPenwidth/2},
		{"round chip", func(p *placed) { p.rounds = 2 }, cw + roundBadgeW*72/2, ch + roundBadgeH*72/2},
		{"remote host", func(p *placed) { p.host = "mbp" }, cw + hostBadgeSize*72/2 + hostBadgePen/2, ch + hostBadgeSize*72/2 + hostBadgePen/2},
		{"waiting and ci pending", func(p *placed) { p.waiting, p.ci = true, "pending" }, cw + ciBadgeRadius + 1, ch + ciBadgeRadius + 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := placed{planet: planet{tier: "standard", class: classWorking}}
			tc.mod(&p)
			if hw, hh := hitRect(p); math.Abs(hw-tc.hw) > 1e-9 || math.Abs(hh-tc.hh) > 1e-9 {
				t.Errorf("hitRect = %v x %v, want %v x %v", hw, hh, tc.hw, tc.hh)
			}
		})
	}
}

// TestMapGeometryRadiusCoversTheMarkers: r reaches the outer edge of a planet's
// waiting ring and CI badge, in pixels, and a plain planet's is what it was.
func TestMapGeometryRadiusCoversTheMarkers(t *testing.T) {
	ps, _ := repoPlanets(wedgeRoster())
	_, sc := arrange(newSlotState(), ps)
	halfX, halfY := mapHalf(sc)
	vbW, vbH := 2*halfX+30, 2*halfY+30
	s := min(800/vbW, 800/vbH)
	var rings, badges, plain int
	for i, got := range mapGeometry(sc, halfX, halfY, vbW, vbH, 800, 800) {
		p := sc.planets[i]
		hw, hh := cardHalf(cardPen(p.tier))
		cw, ch := cardW*72/2, cardH*72/2
		if p.waiting {
			rings++
			hw, hh = max(hw, cw+waitGap+waitPenwidth/2), max(hh, ch+waitGap+waitPenwidth/2)
		}
		if _, badgePen, ok := ciBadge(p.ci); ok {
			badges++
			hw, hh = max(hw, cw+ciBadgeRadius+badgePen/2), max(hh, ch+ciBadgeRadius+badgePen/2)
		}
		if p.host != "" {
			hw, hh = max(hw, cw+hostBadgeSize*72/2), max(hh, ch+hostBadgeSize*72/2)
		}
		if got.HW < hw*s-0.01 || got.HH < hh*s-0.01 {
			t.Errorf("%s: hit rect %v x %v, short of the outer edge of what is drawn, %v x %v", p.branch, got.HW, got.HH, hw*s, hh*s)
		}
		if want := round2(math.Hypot(got.HW, got.HH)); math.Abs(got.R-want) > 0.02 {
			t.Errorf("%s: r = %v, want the rectangle's circumscribed circle %v", p.branch, got.R, want)
		}
		if !p.waiting && p.ci == "" && p.host == "" {
			plain++
			whw, whh := cardHalf(cardPen(p.tier))
			if got.HW != round2(whw*s) || got.HH != round2(whh*s) {
				t.Errorf("%s: a plain card's hit rect = %v x %v, want %v x %v", p.branch, got.HW, got.HH, round2(whw*s), round2(whh*s))
			}
		}
	}
	if rings == 0 || badges != 4 || plain == 0 {
		t.Errorf("%d rings, %d badges, %d plain planets: the roster does not cover every case", rings, badges, plain)
	}
}

func TestMapPlanetNamesItsRepoOnlyInAMultiRepoScene(t *testing.T) {
	ps, _ := repoPlanets(wedgeRoster())
	_, multi := arrange(newSlotState(), ps)
	halfX, halfY := mapHalf(multi)
	data, err := json.Marshal(mapGeometry(multi, halfX, halfY, 2*halfX, 2*halfY, 800, 800))
	if err != nil {
		t.Fatal(err)
	}
	for _, repo := range []string{"alpha", "beta"} {
		if !strings.Contains(string(data), `"repo":"`+repo+`"`) {
			t.Errorf("no planet names repo %s in %s", repo, data)
		}
	}

	_, single := arrange(newSlotState(), ps[:6])
	if len(single.wedges) != 0 {
		t.Fatal("one repo drew wedges")
	}
	halfX, halfY = mapHalf(single)
	data, err = json.Marshal(mapGeometry(single, halfX, halfY, 2*halfX, 2*halfY, 800, 800))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"repo"`) {
		t.Errorf("a single-repo scene carries a repo: %s", data)
	}
	old, _ := buildPlanets(dotModel(sixteenWorkers(), 0), dotNow)
	_, sc := arrange(newSlotState(), old)
	halfX, halfY = mapHalf(sc)
	data, _ = json.Marshal(mapGeometry(sc, halfX, halfY, 2*halfX, 2*halfY, 800, 800))
	if strings.Contains(string(data), `"repo"`) {
		t.Errorf("a plain roster carries a repo: %s", data)
	}
}

// TestMapGeometryWithWedges: the wedge labels widen the corners and the
// planets of every wedge still land where the real svg puts them.
func TestMapGeometryWithWedges(t *testing.T) {
	ws := wedgeRoster()
	for i := range ws {
		// The svg names a planet by branch, so a branch can be drawn once here.
		if ws[i].repo == "beta" && ws[i].branch == "feat/1-shared" {
			ws[i].branch = "feat/10-beta"
		}
		if ws[i].base == "feat/1-shared" {
			ws[i].base = "feat/10-beta"
		}
	}
	ps, _ := repoPlanets(ws)
	_, sc := arrange(newSlotState(), ps)
	if len(sc.wedges) != 2 {
		t.Fatalf("%d wedges, want 2", len(sc.wedges))
	}
	for _, size := range [][2]int{{900, 900}, {1000, 400}} {
		r := renderScene(t, sc, size[0], size[1], "")
		checkCentres(t, r, size[0], size[1])
	}
}

func TestMapGeometryIsDeterministic(t *testing.T) {
	a := renderMap(t, sixteenWorkers(), 640, 480, "")
	b := renderMap(t, sixteenWorkers(), 640, 480, "")
	if !slices.Equal(a.planets, b.planets) {
		t.Errorf("two renders of one roster placed the planets differently:\n%v\n%v", a.planets, b.planets)
	}
}

// TestMapGeometryScalesWithTheBox: the planets keep their relative places when
// the image is resized, and a blocked planet's radius includes its heavier ring.
func TestMapGeometryScalesWithTheBox(t *testing.T) {
	ps, _ := buildPlanets(dotModel(sixteenWorkers(), 0), dotNow)
	_, sc := arrange(newSlotState(), ps)
	halfX, halfY := mapHalf(sc)
	vb := 2*halfX + 30
	small := mapGeometry(sc, halfX, halfY, vb, vb, 400, 400)
	big := mapGeometry(sc, halfX, halfY, vb, vb, 800, 800)
	for i := range small {
		if math.Abs(big[i].Cx-2*small[i].Cx) > 0.02 || math.Abs(big[i].R-2*small[i].R) > 0.02 {
			t.Errorf("%s: %+v at 400 and %+v at 800", small[i].Branch, small[i], big[i])
		}
		hw, hh := hitRect(sc.planets[i])
		if want := math.Hypot(hw, hh) * 400 / vb; math.Abs(small[i].R-want) > 0.01 {
			t.Errorf("%s: r = %v, want %v", small[i].Branch, small[i].R, want)
		}
		if want := hw * 400 / vb; math.Abs(small[i].HW-want) > 0.01 {
			t.Errorf("%s: hw = %v, want %v", small[i].Branch, small[i].HW, want)
		}
	}
}

func TestSelectionRingOnlyForAKnownBranch(t *testing.T) {
	ps, _ := buildPlanets(dotModel(sixteenWorkers(), 0), dotNow)
	_, sc := arrange(newSlotState(), ps)
	known := sc.planets[3]

	got := buildDOT(sc, nil, legendNone, known.branch, "")
	var ring []string
	for line := range strings.SplitSeq(got, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "sel [") {
			ring = append(ring, line)
		}
	}
	if len(ring) != 1 {
		t.Fatalf("%d ring nodes, want 1:\n%s", len(ring), got)
	}
	for _, want := range []string{
		"shape=box", `style="rounded"`, `color="` + colSun + `"`, `label=""`,
		"width=" + strconv.FormatFloat(cardW+2*selectGap/72, 'g', -1, 64),
		"height=" + strconv.FormatFloat(cardH+2*selectGap/72, 'g', -1, 64), "pos=" + dotq(pt(known.x)+","+pt(known.y)+"!"),
	} {
		if !strings.Contains(ring[0], want) {
			t.Errorf("ring %q lacks %s", ring[0], want)
		}
	}
	if strings.Contains(ring[0], "fillcolor") || strings.Contains(ring[0], "filled") {
		t.Errorf("the ring must not be filled: %q", ring[0])
	}
	if strings.Index(got, "sel [") < strings.LastIndex(got, "p"+strconv.Itoa(len(sc.planets))+" [") {
		t.Errorf("the ring is declared before the last planet")
	}

	for _, sel := range []string{"", "feat/999-nobody"} {
		if out := buildDOT(sc, nil, legendNone, sel, ""); strings.Contains(out, "sel [") {
			t.Errorf("select %q drew a ring", sel)
		}
	}
	lg := legend{}
	if out := renderDOT(sc, lg, legendFull); strings.Contains(out, "sel [") {
		t.Error("the legend render drew a ring")
	}
}

func TestMapDOTHasNoLegend(t *testing.T) {
	ps, lg := buildPlanets(dotModel(sixteenWorkers(), 2), dotNow)
	_, sc := arrange(newSlotState(), ps)
	if out := buildDOT(sc, nil, legendNone, "", ""); strings.Contains(out, "\n  label=<") || !strings.HasSuffix(out, "}\n") {
		t.Errorf("the map carries a graph label:\n%s", out)
	}
	if out := buildDOT(sc, &lg, legendFull, "", ""); out != renderDOT(sc, lg, legendFull) || !strings.Contains(out, "\n  label=<") {
		t.Error("a legend render is no longer renderDOT's")
	}
}

func TestSelectionRingRendersAroundThePlanet(t *testing.T) {
	ws := sixteenWorkers()
	sel := "feat/101-alpha"
	r := renderMap(t, ws, 800, 800, sel)
	var p mapPlanet
	for _, c := range r.planets {
		if c.Branch == sel {
			p = c
		}
	}
	// The ring stands selectGap out from the card's side edge, scaled; its
	// stroke is a pixel or two wide, so look at the pixels around its centre line.
	s := min(800/r.vbW, 800/r.vbH)
	x := p.Cx + (cardW*72/2+selectGap)*s
	found := false
	for _, dx := range []int{-1, 0, 1} {
		cr, cg, cb, _ := r.img.At(int(math.Round(x))+dx, int(math.Round(p.Cy))).RGBA()
		found = found || [3]int{int(cr >> 8), int(cg >> 8), int(cb >> 8)} == [3]int{0xf9, 0xe2, 0xaf}
	}
	if !found {
		t.Errorf("no pixel of %s on the ring's side near x=%.1f", colSun, x)
	}
}

func TestResvgRewritesOnlyTheRoot(t *testing.T) {
	in := `<svg width="100pt" height="50pt" viewBox="0.00 0.00 100.00 50.00" xmlns="x"><rect width="3" height="4"/></svg>`
	want := `<svg width="640px" height="480px" viewBox="0.00 0.00 100.00 50.00" xmlns="x"><rect width="3" height="4"/></svg>`
	if got := string(sizeSVG([]byte(in), 640, 480)); got != want {
		t.Errorf("sizeSVG =\n%s\nwant\n%s", got, want)
	}
}

func TestMapOutputShapeAndSlotOrder(t *testing.T) {
	l := newLoop(t)
	l.seedBus(t)
	var stdout bytes.Buffer
	var sizes [][2]int
	var dots []string
	l.opts.Stdout = &stdout
	l.opts.Probes.Graphviz = func(_ context.Context, dot []byte) ([]byte, error) {
		dots = append(dots, string(dot))
		return []byte(`<svg width="500pt" height="400pt" viewBox="0.00 0.00 500.00 400.00" xmlns="x"></svg>`), nil
	}
	l.opts.Probes.Resvg = func(_ context.Context, svg []byte, w, h int) ([]byte, error) {
		sizes = append(sizes, [2]int{w, h})
		return []byte("PNG"), nil
	}
	out := filepath.Join(t.TempDir(), "map.png")
	run := func(extra ...string) string {
		t.Helper()
		stdout.Reset()
		req := Request{Crew: l.crew, Map: true, Px: "300x200", Out: out}
		if len(extra) > 0 {
			req.Select = extra[0]
		}
		if code := Run(context.Background(), req, l.paths, l.opts); code != exitOK {
			t.Fatalf("code %d: %s", code, l.opts.Stderr.(*bytes.Buffer))
		}
		return stdout.String()
	}

	first := run()
	var got struct {
		V       int
		W, H    int
		PNG     string
		Planets []map[string]any
	}
	dec := json.NewDecoder(strings.NewReader(first))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&got); err != nil {
		t.Fatalf("%v in %q", err, first)
	}
	if !strings.HasSuffix(first, "}\n") || strings.Count(first, "\n") != 1 {
		t.Errorf("stdout is not one JSON line: %q", first)
	}
	if got.V != 1 || got.W != 300 || got.H != 200 || got.PNG != out {
		t.Errorf("header = %+v, want v 1, 300x200, %s", got, out)
	}
	if png, err := os.ReadFile(out); err != nil || string(png) != "PNG" {
		t.Errorf("png = %q, %v", png, err)
	}
	if !slices.Equal(sizes[0][:], []int{300, 200}) {
		t.Errorf("resvg sized %v", sizes[0])
	}
	if len(got.Planets) != 4 {
		t.Fatalf("%d planets, want the 4 workers", len(got.Planets))
	}
	for _, p := range got.Planets {
		keys := make([]string, 0, len(p))
		for k := range p {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		if !slices.Equal(keys, []string{"branch", "cx", "cy", "hh", "hw", "name", "r"}) {
			t.Errorf("planet keys = %v", keys)
		}
	}
	if strings.Contains(dots[0], "sel [") {
		t.Error("no --select, but a ring was drawn")
	}

	// A persisted sequence is the slot order, read and never rewritten.
	fr, err := buildFrame(context.Background(), l.opts, l.paths, l.cdir(), l.crew, roleRows(l.rolePanes(), l.paths.Dir, l.crew), false)
	if err != nil {
		t.Fatal(err)
	}
	order := slices.Clone(fr.slots.Order)
	slices.Reverse(order)
	slotsPath := filepath.Join(l.cdir(), fileSlots)
	if err := os.MkdirAll(l.cdir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := saveSlots(slotsPath, slotState{V: slotsVer, Order: order}, os.Stderr); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(slotsPath)
	if err != nil {
		t.Fatal(err)
	}
	second := run("feat/2-b")
	var branches []string
	for _, p := range decodePlanets(t, second) {
		branches = append(branches, p.Branch)
	}
	var want []string
	for _, e := range order {
		want = append(want, e.Branch)
	}
	if !slices.Equal(branches, want) {
		t.Errorf("planets in order %v, want the persisted order %v", branches, want)
	}
	if after, _ := os.ReadFile(slotsPath); !bytes.Equal(before, after) {
		t.Errorf("map mode rewrote the slots file:\n%s\n%s", before, after)
	}
	if !strings.Contains(dots[1], "sel [") {
		t.Error("a known --select drew no ring")
	}
	run("feat/9-nobody")
	if strings.Contains(dots[2], "sel [") {
		t.Error("an unknown --select drew a ring")
	}
	if again := run(); again != run() {
		t.Error("two renders of one roster gave different geometry")
	}
}

func decodePlanets(t *testing.T, line string) []mapPlanet {
	t.Helper()
	var got mapOut
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatal(err)
	}
	return got.Planets
}

func snapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		out[path] = info.Mode().String() + " " + strconv.FormatInt(info.Size(), 10) + " " + info.ModTime().String()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestMapWritesNothingButThePNG: a crew with no dir of its own is read, drawn and
// left as it was found, from another working directory, with no lock or record.
func TestMapWritesNothingButThePNG(t *testing.T) {
	l := newLoop(t)
	l.seedBus(t)
	var stdout bytes.Buffer
	l.opts.Stdout = &stdout
	l.opts.Probes.Graphviz = func(context.Context, []byte) ([]byte, error) {
		return []byte(`<svg width="1pt" height="1pt" viewBox="0 0 10 10"></svg>`), nil
	}
	l.opts.Probes.Resvg = func(context.Context, []byte, int, int) ([]byte, error) { return []byte("PNG"), nil }
	l.opts.Probes.AeyePublish = func(context.Context, string, string, bool) error {
		t.Error("map mode published to aeye")
		return nil
	}
	if _, err := os.Stat(l.cdir()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the crew dir exists before the run: %v", err)
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, l.paths.Common)
	out := filepath.Join(t.TempDir(), "m.png")
	code := Run(context.Background(), Request{Crew: l.crew, Map: true, Px: "128x128", Out: out}, l.paths, l.opts)
	if code != exitOK {
		t.Fatalf("code %d: %s", code, l.opts.Stderr.(*bytes.Buffer))
	}
	if after := snapshot(t, l.paths.Common); !maps.Equal(before, after) {
		t.Errorf("the bus dir changed:\nbefore %v\nafter  %v", before, after)
	}
	if now, _ := os.Getwd(); now != wd {
		t.Errorf("map mode moved the working directory to %s", now)
	}
	if _, err := os.Stat(l.cdir()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("map mode created the crew dir: %v", err)
	}
	if _, err := os.Stat(out); err != nil {
		t.Errorf("no png: %v", err)
	}
}

func TestMapFailuresNameTheTool(t *testing.T) {
	for _, tc := range []struct {
		name    string
		graph   error
		raster  error
		want    string
		outFile bool
	}{
		{name: "no neato", graph: errors.New(`neato: exec: "neato": executable file not found in $PATH`), want: "crew: roster-render: neato:"},
		{name: "no resvg", raster: errors.New(`resvg: exec: "resvg": executable file not found in $PATH`), want: "crew: roster-render: resvg:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := newLoop(t)
			l.seedBus(t)
			var stdout bytes.Buffer
			l.opts.Stdout = &stdout
			l.opts.Probes.Graphviz = func(context.Context, []byte) ([]byte, error) {
				return []byte(`<svg viewBox="0 0 10 10"></svg>`), tc.graph
			}
			l.opts.Probes.Resvg = func(context.Context, []byte, int, int) ([]byte, error) { return nil, tc.raster }
			out := filepath.Join(t.TempDir(), "m.png")
			code := Run(context.Background(), Request{Crew: l.crew, Map: true, Px: "128x128", Out: out}, l.paths, l.opts)
			stderr := l.opts.Stderr.(*bytes.Buffer).String()
			if code != exitFailure || !strings.HasPrefix(stderr, tc.want) || strings.Count(stderr, "\n") != 1 {
				t.Errorf("code %d, stderr %q, want exit 1 and one %q line", code, stderr, tc.want)
			}
			if stdout.Len() != 0 {
				t.Errorf("stdout = %q on a failure", stdout.String())
			}
			if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("a failed render left a png: %v", err)
			}
		})
	}
}

func TestMapRefusals(t *testing.T) {
	ok := Request{Crew: "c1", Map: true, Px: "640x480", Out: "/tmp/x.png"}
	with := func(f func(*Request)) Request {
		r := ok
		f(&r)
		return r
	}
	for _, tc := range []struct {
		name string
		req  Request
		msg  string
	}{
		{"once", with(func(r *Request) { r.Once = true }), "crew: roster-render: --map and --once are mutually exclusive"},
		{"detach", with(func(r *Request) { r.Detach = true }), "crew: roster-render: --map and --detach are mutually exclusive"},
		{"pane", with(func(r *Request) { r.Pane = "%3" }), "crew: roster-render: --map and --pane are mutually exclusive"},
		{"no-open", with(func(r *Request) { r.NoOpen = true }), "crew: roster-render: --map and --no-open are mutually exclusive"},
		{"interval", with(func(r *Request) { r.Interval = "5" }), "crew: roster-render: --map and --interval are mutually exclusive"},
		{"quiet", with(func(r *Request) { r.Quiet = "0" }), "crew: roster-render: --map and --quiet are mutually exclusive"},
		{"legend", with(func(r *Request) { r.Legend = "none" }), "crew: roster-render: --map and --legend are mutually exclusive"},
		{"no crew", with(func(r *Request) { r.Crew = "" }), errCrewRequired},
		{"bad crew", with(func(r *Request) { r.Crew = "a/b" }), errCrewID},
		{"no px", with(func(r *Request) { r.Px = "" }), "crew: roster-render: --map requires --px"},
		{"no out", with(func(r *Request) { r.Out = "" }), "crew: roster-render: --map requires --out"},
		{"px one side", with(func(r *Request) { r.Px = "640" }), errMapPx},
		{"px upper x", with(func(r *Request) { r.Px = "640X480" }), errMapPx},
		{"px empty height", with(func(r *Request) { r.Px = "640x" }), errMapPx},
		{"px signed", with(func(r *Request) { r.Px = "-64x480" }), errMapPx},
		{"px words", with(func(r *Request) { r.Px = "axb" }), errMapPx},
		{"px too narrow", with(func(r *Request) { r.Px = "63x480" }), errMapPx},
		{"px too short", with(func(r *Request) { r.Px = "640x63" }), errMapPx},
		{"px too wide", with(func(r *Request) { r.Px = "4097x480" }), errMapPx},
		{"px too tall", with(func(r *Request) { r.Px = "640x4097" }), errMapPx},
		{"px three parts", with(func(r *Request) { r.Px = "64x64x64" }), errMapPx},
		{"px without map", Request{Crew: "c1", Px: "640x480"}, errMapOnly},
		{"select without map", Request{Crew: "c1", Select: "feat/x"}, errMapOnly},
		{"out without map", Request{Crew: "c1", Out: "/tmp/x.png"}, errMapOnly},
		{"select-repo without map", Request{Crew: "c1", SelectRepo: "alpha"}, errMapOnly},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, msg, code := check(tc.req); msg != tc.msg || code != exitUsage {
				t.Errorf("check = %q, %d; want %q, %d", msg, code, tc.msg, exitUsage)
			}
		})
	}
	for _, px := range []string{"64x64", "4096x4096", "640x480"} {
		c, msg, code := check(with(func(r *Request) { r.Px = px }))
		if msg != "" || code != exitOK || !c.mapMode {
			t.Errorf("px %s refused: %q", px, msg)
		}
	}
}
