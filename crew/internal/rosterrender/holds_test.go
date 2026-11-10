package rosterrender

import (
	"fmt"
	"math"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func quotaHold(i int, engine, window string) queuedHold {
	return queuedHold{id: "h" + strconv.Itoa(i), label: strconv.Itoa(100 + i), engine: engine, window: window}
}

func quotaHolds(n int) []queuedHold {
	var hs []queuedHold
	for i := range n {
		hs = append(hs, quotaHold(i, "claude", "7d"))
	}
	return hs
}

// planetReach is the farthest a planet or one of its moons draws from the sun.
func planetReach(sc scene) float64 {
	reach := 0.0
	for _, p := range sc.planets {
		reach = max(reach, math.Hypot(p.x, p.y)+p.size*72/2)
		for _, m := range p.moons {
			reach = max(reach, math.Hypot(m.x, m.y)+moonRadius)
		}
	}
	return reach
}

// clearOfBox reports whether a hold's circle stays off a label's box centred at
// (bx, by).
func clearOfBox(h placedHold, bx, by float64, text string) bool {
	dx := max(math.Abs(h.x-bx)-labelWidth(text)/2, 0)
	dy := max(math.Abs(h.y-by)-hostLabelHalf, 0)
	return math.Hypot(dx, dy) > holdRadius
}

func hostScene() scene {
	long := strings.Repeat("h", 24)
	ps := []planet{wp("a", classBlocked)}
	for i := range 5 {
		ps = append(ps, remoteWP("r"+strconv.Itoa(i), classWorking, long))
	}
	for i := range 3 {
		ps = append(ps, remoteWP("s"+strconv.Itoa(i), classWorking, "omega-12-chr"))
	}
	_, sc := arrange(newSlotState(), ps)
	return sc
}

func wedgeScene() scene {
	ps := append(repoUniform("alpha-repo-name-long-label", 5), repoUniform("b", 2)...)
	ps[0].host = strings.Repeat("h", 24)
	_, sc := arrange(newSlotState(), ps)
	return sc
}

func TestPlaceHoldsWithoutHoldsLeavesTheSceneAlone(t *testing.T) {
	for name, sc := range map[string]scene{"hosts": hostScene(), "wedges": wedgeScene()} {
		t.Run(name, func(t *testing.T) {
			before := sc
			placeHolds(&sc, nil)
			if !reflect.DeepEqual(before, sc) || sc.holds != nil || sc.anchors != nil {
				t.Errorf("an empty hold list changed the scene")
			}
		})
	}
}

func TestHoldsSitOnAnOrbitClearOfEveryLabel(t *testing.T) {
	for name, sc := range map[string]scene{"hosts": hostScene(), "wedges": wedgeScene()} {
		t.Run(name, func(t *testing.T) {
			if len(sc.hosts) == 0 {
				t.Fatal("no host labels")
			}
			preExtent := sc.extent
			placeHolds(&sc, quotaHolds(6))
			if len(sc.holds) != 6 {
				t.Fatalf("%d holds placed, want 6", len(sc.holds))
			}
			reach := planetReach(sc)
			var orbit float64
			for i, h := range sc.holds {
				r := math.Hypot(h.x, h.y)
				if i == 0 {
					orbit = r
				}
				if math.Abs(r-orbit) > 1e-6 {
					t.Errorf("hold %d at radius %v, the orbit is %v", i, r, orbit)
				}
				if r-holdRadius <= reach || r-holdRadius <= preExtent {
					t.Errorf("hold %d circle reaches %v, inside planets %v and extent %v", i, r-holdRadius, reach, preExtent)
				}
				for _, l := range sc.hosts {
					if !clearOfBox(h, l.x, l.y, l.host) {
						t.Errorf("hold %d overlaps host label %q", i, l.host)
					}
				}
				for _, w := range sc.wedges {
					if !clearOfBox(h, w.x, w.y, w.label) {
						t.Errorf("hold %d overlaps wedge label %q", i, w.label)
					}
				}
				if r+holdRadius > sc.extent+1e-6 {
					t.Errorf("hold %d outside the extent %v", i, sc.extent)
				}
				if want := float64(i) * 60; math.Abs(compass(h.x, h.y)-want) > 1e-6 && !(i == 0 && compass(h.x, h.y) > 359.9999) {
					t.Errorf("hold %d at %v degrees, want %v", i, compass(h.x, h.y), want)
				}
			}
		})
	}
}

func TestHoldsAreCappedAtSixteen(t *testing.T) {
	sc := hostScene()
	placeHolds(&sc, quotaHolds(20))
	if len(sc.holds) != maxHolds {
		t.Fatalf("%d holds drawn, want %d", len(sc.holds), maxHolds)
	}
	if sc.holds[maxHolds-1].id != "h15" {
		t.Errorf("the cap kept %s last, want the first 16 in order", sc.holds[maxHolds-1].id)
	}
}

func TestHoldAnchorsAreSharedPerEngineWindow(t *testing.T) {
	sc := hostScene()
	holds := []queuedHold{
		quotaHold(0, "claude", "7d"), quotaHold(1, "codex", "5h"), quotaHold(2, "claude", "7d"),
		{id: "f", label: "fleet"}, quotaHold(4, "claude", "7d"), {id: "half", label: "x", engine: "claude"},
	}
	placeHolds(&sc, holds)
	if len(sc.anchors) != 2 {
		t.Fatalf("anchors = %+v, want claude and codex once each", sc.anchors)
	}
	if sc.anchors[0].text != "claude · 7d" || sc.anchors[1].text != "codex · 5h" {
		t.Errorf("anchors = %+v", sc.anchors)
	}
	// claude holds sit at 0, 120 and 240 degrees of 6 slots: 0, 120, 240 cancel, so
	// the anchor falls back to the first hold's direction.
	for _, a := range sc.anchors {
		for _, h := range sc.holds {
			if math.Hypot(h.x, h.y) >= math.Hypot(a.x, a.y)-holdRadius {
				t.Errorf("anchor %q is not outside the hold orbit", a.text)
			}
		}
		if math.Hypot(a.x, a.y)+labelWidth(a.text)/2 > sc.extent+1e-6 {
			t.Errorf("anchor %q outside the extent", a.text)
		}
	}
}

func TestHoldAnchorFacesTheMeanDirectionOfItsHolds(t *testing.T) {
	sc := hostScene()
	placeHolds(&sc, []queuedHold{quotaHold(0, "codex", "5h"), quotaHold(1, "claude", "7d"), quotaHold(2, "claude", "7d"), {id: "f", label: "f"}})
	// 4 slots at 0, 90, 180, 270: claude holds at 90 and 180 average to 135.
	for _, a := range sc.anchors {
		if a.text == "claude · 7d" && math.Abs(compass(a.x, a.y)-135) > 1e-6 {
			t.Errorf("claude anchor at %v degrees, want 135", compass(a.x, a.y))
		}
	}
}

func TestDotReviewRoundBadge(t *testing.T) {
	ps := []planet{wp("a", classWorking), wp("b", classWorking)}
	ps[1].rounds = 3
	_, sc := arrange(newSlotState(), ps)
	out := renderDOT(sc, legend{}, legendFull)
	ids := idOf(sc)
	if strings.Contains(out, "  "+ids["a"]+"r [") {
		t.Errorf("a planet with no rounds drew a badge:\n%s", out)
	}
	line := nodeLine(t, out, ids["b"]+"r")
	for _, want := range []string{`label="r3"`, "shape=box", `style="rounded,filled"`, "width=0.3", "height=0.2", "fontsize=9",
		`fillcolor="` + colWaiting + `"`, `fontcolor="` + colBG + `"`, "penwidth=0"} {
		if !strings.Contains(line, want) {
			t.Errorf("badge lacks %s: %s", want, line)
		}
	}
	b := sc.planets[1]
	if b.branch != "b" {
		b = sc.planets[0]
	}
	x, y := posOf(t, line)
	if x <= b.x || y >= b.y {
		t.Errorf("badge at (%v, %v) is not lower right of the planet (%v, %v)", x, y, b.x, b.y)
	}
}

func TestHitRadiusCoversTheRoundBadge(t *testing.T) {
	const pr = standardSize * 72 / 2
	p := placed{planet: planet{size: standardSize, class: classWorking, rounds: 2}}
	want := pr + roundBadgeStandoff + math.Hypot(0.3*72/2, 0.2*72/2)
	if got := hitRadius(p); math.Abs(got-want) > 1e-9 {
		t.Errorf("hitRadius = %v, want %v", got, want)
	}
	p.rounds = 0
	if got := hitRadius(p); got != pr+planetPenwidth/2 {
		t.Errorf("hitRadius without rounds = %v", got)
	}
}

func TestDotHoldNodesAndAnchorEdges(t *testing.T) {
	_, sc := arrange(newSlotState(), []planet{wp("a", classWorking)})
	placeHolds(&sc, []queuedHold{quotaHold(0, "claude", "7d"), {id: "f", label: "fleet"}})
	out := renderDOT(sc, legend{holds: 2}, legendFull)
	for _, id := range []string{"q1", "q2"} {
		line := nodeLine(t, out, id)
		for _, want := range []string{"width=0.5", `style="filled,dashed"`, `color="` + colHolds + `55"`, `fontcolor="` + colDim + `"`} {
			if !strings.Contains(line, want) {
				t.Errorf("%s lacks %s: %s", id, want, line)
			}
		}
	}
	if !strings.Contains(nodeLine(t, out, "q1"), `label="100"`) || !strings.Contains(nodeLine(t, out, "q2"), `label="fleet"`) {
		t.Errorf("hold labels wrong:\n%s", out)
	}
	if a := nodeLine(t, out, "a1"); !strings.Contains(a, `label="claude · 7d"`) || !strings.Contains(a, "fixedsize=false") || !strings.Contains(a, `fontcolor="`+colDim+`"`) {
		t.Errorf("anchor node: %s", a)
	}
	if !strings.Contains(out, "  q1 -> a1 [") {
		t.Errorf("the quota hold has no edge to its anchor:\n%s", out)
	}
	if strings.Contains(out, "q2 ->") || strings.Contains(out, "-> q2") {
		t.Errorf("the fleet hold has an edge:\n%s", out)
	}
	if strings.Count(out, "-> a") != 1 {
		t.Errorf("anchor edges:\n%s", out)
	}
}

func TestDotWithoutHoldsDrawsNoHoldNodes(t *testing.T) {
	_, sc := arrange(newSlotState(), []planet{wp("a", classWorking)})
	out := renderDOT(sc, legend{}, legendFull)
	if strings.Contains(out, "  q1") || strings.Contains(out, "  a1") {
		t.Errorf("hold nodes without holds:\n%s", out)
	}
}

func TestLegendStrip(t *testing.T) {
	budget, pct, zero := 6, 23.4, 0.0
	known := legend{weight: 4, budget: &budget, engines: []engineReading{
		{name: "claude", window: "7d", pct: &pct}, {name: "codex"}, {name: "pi", window: "5h", pct: &zero},
	}}
	html := legendHTML(known, legendFull)
	if want := "crew weight 4 · fleet budget 6 · claude 7d 23% · codex ? · pi 5h 0%"; !strings.Contains(html, want) {
		t.Errorf("strip lacks %q:\n%s", want, html)
	}
	unknown := legendHTML(legend{engines: []engineReading{{name: "claude"}}}, legendFull)
	if want := "crew weight 0 · fleet budget ? · claude ?"; !strings.Contains(unknown, want) {
		t.Errorf("strip lacks %q:\n%s", want, unknown)
	}
	off := 0
	if got := stripText(legend{budget: &off}); !strings.Contains(got, "fleet budget off") {
		t.Errorf("a zero budget is checks disabled, got %q", got)
	}
	if strings.Contains(unknown, "claude 0") || strings.Contains(unknown, "budget 0") {
		t.Errorf("an unknown reading drew as zero:\n%s", unknown)
	}
	hostile := legendHTML(legend{engines: []engineReading{{name: `<b>x"`, window: "7d", pct: &pct}}}, legendFull)
	if strings.Contains(hostile, "<b>") {
		t.Errorf("engine name reached the label as markup:\n%s", hostile)
	}
	if legendHeight(legend{}, legendFull) != legendH+legendRowH {
		t.Errorf("legendHeight = %v, want the strip row always counted", legendHeight(legend{}, legendFull))
	}
}

func TestLegendHoldKey(t *testing.T) {
	if strings.Contains(legendHTML(legend{}, legendFull), "queued hold") {
		t.Error("the hold key shows without holds")
	}
	if !strings.Contains(legendHTML(legend{holds: 1}, legendFull), "dashed outer dot = queued hold") {
		t.Error("the hold key is missing with a hold")
	}
}

func holdScene(t *testing.T) scene {
	t.Helper()
	long, short := "alpha-host-name-24-chars", "omega-12-chr"
	var ws []dotWorker
	for i := range 8 {
		w := dotWorker{branch: "feat/" + strconv.Itoa(i+1) + "-x", name: "w" + strconv.Itoa(i+1), tier: "standard", state: "working", age: 10, host: short}
		if i < 5 {
			w.host = long
		}
		ws = append(ws, w)
	}
	ps, _ := buildPlanets(dotModel(ws, 0), dotNow)
	_, sc := arrange(newSlotState(), ps)
	placeHolds(&sc, []queuedHold{
		quotaHold(0, "claude-opus-long-engine", "seven_day_opus"), quotaHold(1, "codex-long-engine-name", "five_hour_window"),
		quotaHold(2, "cursor-long-engine-name", "monthly_window_x"), quotaHold(3, "claude-opus-long-engine", "seven_day_opus"),
		{id: "f", label: "fleet"}, quotaHold(5, "pi", "5h"),
	})
	return sc
}

// TestMapGeometryWithHoldsAndLongAnchors: holds and their wide quota anchors grow
// the extent, and the pixel centres still land where the real svg puts them.
func TestMapGeometryWithHoldsAndLongAnchors(t *testing.T) {
	for _, size := range [][2]int{{900, 900}, {1000, 400}} {
		t.Run(fmt.Sprint(size[0], "x", size[1]), func(t *testing.T) {
			sc := holdScene(t)
			if len(sc.anchors) != 4 {
				t.Fatalf("%d anchors, want 4", len(sc.anchors))
			}
			r := renderScene(t, sc, size[0], size[1], "")
			checkCentres(t, r, size[0], size[1])
		})
	}
}

func TestMapListsExactlyThePlanetsWithHoldsPresent(t *testing.T) {
	sc := holdScene(t)
	halfX, halfY := mapHalf(sc)
	got := mapGeometry(sc, halfX, halfY, 2*halfX+30, 2*halfY+30, 800, 800)
	if len(got) != len(sc.planets) || len(sc.holds) == 0 {
		t.Fatalf("%d map planets for %d planets and %d holds", len(got), len(sc.planets), len(sc.holds))
	}
	for i, p := range got {
		if p.Branch != sc.planets[i].branch {
			t.Errorf("planet %d is %s, want %s", i, p.Branch, sc.planets[i].branch)
		}
	}
}
