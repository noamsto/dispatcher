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
		reach = max(reach, math.Hypot(p.x, p.y)+cardReach)
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
			placeHolds(&sc, nil, nil)
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
			placeHolds(&sc, quotaHolds(6), nil)
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
	placeHolds(&sc, quotaHolds(20), nil)
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
	placeHolds(&sc, holds, nil)
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
	placeHolds(&sc, []queuedHold{quotaHold(0, "codex", "5h"), quotaHold(1, "claude", "7d"), quotaHold(2, "claude", "7d"), {id: "f", label: "f"}}, nil)
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

func TestHitRectCoversTheRoundBadge(t *testing.T) {
	p := placed{planet: planet{class: classWorking, rounds: 2}}
	wantW, wantH := cardW*72/2+roundBadgeW*72/2, cardH*72/2+roundBadgeH*72/2
	if hw, hh := hitRect(p); math.Abs(hw-wantW) > 1e-9 || math.Abs(hh-wantH) > 1e-9 {
		t.Errorf("hitRect = %v x %v, want %v x %v", hw, hh, wantW, wantH)
	}
	p.rounds = 0
	if hw, hh := hitRect(p); hw != cardW*72/2+planetPenwidth/2 || hh != cardH*72/2+planetPenwidth/2 {
		t.Errorf("hitRect without rounds = %v x %v", hw, hh)
	}
}

func TestDotHoldNodesAndAnchorEdges(t *testing.T) {
	_, sc := arrange(newSlotState(), []planet{wp("a", classWorking)})
	placeHolds(&sc, []queuedHold{quotaHold(0, "claude", "7d"), {id: "f", label: "fleet"}}, nil)
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
	}, nil)
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

func queuedModel(t *testing.T, queued string) map[string]any {
	t.Helper()
	return decodeOne(t, `{"rows":[],"holds":[],"roles":[],"queued":`+queued+`}`)
}

func tasks(n int) []queuedTask {
	var ts []queuedTask
	for i := range n {
		ts = append(ts, queuedTask{id: "u" + strconv.Itoa(i), label: strconv.Itoa(200 + i)})
	}
	return ts
}

func TestBuildQueued(t *testing.T) {
	m := queuedModel(t, `[
		{"id":"a","task":{"ref":null,"branch":"feat/12-x","title":"Secret Title"},"after":["feat/12-x"," #7 ",""]},
		{"id":"b","task":{"ref":"#40","branch":null,"title":"t"},"after":[]},
		{"id":"c","task":{"ref":null,"branch":null,"title":"t"},"after":[]},
		{"id":"d","task":{"ref":"a\"];x","branch":null,"title":"t"},"after":[]}]`)
	got := buildQueued(m)
	if len(got) != 4 {
		t.Fatalf("got %+v", got)
	}
	for i, want := range []string{"12", "40", "queued", "ax"} {
		if got[i].label != want {
			t.Errorf("task %d label %q, want %q", i, got[i].label, want)
		}
	}
	if want := []string{"feat/12-x", "#7"}; !reflect.DeepEqual(got[0].after, want) {
		t.Errorf("after = %q, want %q", got[0].after, want)
	}
	if strings.Contains(fmt.Sprint(got), "Secret Title") {
		t.Errorf("a title reached the result: %+v", got)
	}
	_, lg := buildPlanets(m, dotNow)
	if lg.queued != 4 {
		t.Errorf("lg.queued = %d, want 4", lg.queued)
	}
}

func TestDotQueuedHostileTextStaysOut(t *testing.T) {
	m := queuedModel(t, `[{"id":"a","task":{"ref":"a\"];evil","branch":null,"title":"t"},"after":["x\"]; evil -> y [","<b>"]}]`)
	ts := buildQueued(m)
	ts[0].repo = "r1"
	_, sc := arrange(newSlotState(), []planet{wp("a", classWorking)})
	placeHolds(&sc, nil, ts)
	out := renderDOT(sc, legend{queued: 1}, legendFull)
	if strings.Contains(out, "<b>") || strings.Contains(out, "evil -> y") || strings.Contains(out, "x\"];") {
		t.Errorf("hostile text reached the DOT:\n%s", out)
	}
	if !strings.Contains(out, `label="aevil"`) {
		t.Errorf("cleaned label missing:\n%s", out)
	}
}

func TestQueuedShareTheHoldOrbit(t *testing.T) {
	sc := hostScene()
	placeHolds(&sc, quotaHolds(2), tasks(2))
	if len(sc.holds) != 2 || len(sc.queued) != 2 {
		t.Fatalf("%d holds, %d tasks placed", len(sc.holds), len(sc.queued))
	}
	if len(sc.anchors) != 1 {
		t.Errorf("anchors = %+v, want the holds' one", sc.anchors)
	}
	var pts [][2]float64
	for _, h := range sc.holds {
		pts = append(pts, [2]float64{h.x, h.y})
	}
	for _, q := range sc.queued {
		pts = append(pts, [2]float64{q.x, q.y})
	}
	orbit := math.Hypot(pts[0][0], pts[0][1])
	if orbit+holdRadius > sc.extent+1e-6 {
		t.Errorf("extent %v does not cover the orbit", sc.extent)
	}
	for i, p := range pts {
		if r := math.Hypot(p[0], p[1]); math.Abs(r-orbit) > 1e-6 {
			t.Errorf("point %d at radius %v, orbit %v", i, r, orbit)
		}
		want := float64(i) * 90
		if got := compass(p[0], p[1]); math.Abs(got-want) > 1e-6 && !(i == 0 && got > 359.9999) {
			t.Errorf("point %d at %v degrees, want %v", i, got, want)
		}
	}

	capped := hostScene()
	placeHolds(&capped, quotaHolds(16), tasks(3))
	if len(capped.holds) != 16 || len(capped.queued) != 0 {
		t.Errorf("%d holds and %d tasks, want 16 and 0", len(capped.holds), len(capped.queued))
	}

	none := hostScene()
	before := none
	placeHolds(&none, nil, nil)
	if !reflect.DeepEqual(before, none) {
		t.Error("no holds and no tasks changed the scene")
	}
}

func TestDotQueuedNodeAndWaitEdge(t *testing.T) {
	mk := func(branch, repo, issue, pr string) planet {
		p := wp(branch, classWorking)
		p.repo, p.issue, p.prURL = repo, issue, pr
		return p
	}
	render := func(ps []planet, after ...string) (string, map[planetKey]string) {
		_, sc := arrange(newSlotState(), ps)
		placeHolds(&sc, nil, []queuedTask{{id: "t", label: "9", repo: "r1", after: after}})
		return renderDOT(sc, legend{queued: 1}, legendFull), keyIDs(sc)
	}
	r1 := mk("feat/12-x", "r1", "12", "https://github.com/o/r/pull/31")
	out, ids := render([]planet{r1}, "12")
	id := ids[planetKey{r1.repo, r1.branch}]
	node := nodeLine(t, out, "u1")
	for _, want := range []string{"width=0.5", `style="dotted"`, `color="` + colDim + staleAlpha + `"`} {
		if !strings.Contains(node, want) {
			t.Errorf("node lacks %s: %s", want, node)
		}
	}
	if !strings.Contains(out, "  u1 -> "+id+" [") || !strings.Contains(out, "style=dotted") {
		t.Errorf("no wait edge to %s:\n%s", id, out)
	}
	for _, target := range []string{"feat/12-x", "#12", "31", "#31"} {
		if out, _ := render([]planet{r1}, target); !strings.Contains(out, "  u1 -> "+id+" [") {
			t.Errorf("target %q drew no edge", target)
		}
	}
	if out, _ := render([]planet{r1}, "99", "feat/none"); strings.Contains(out, "u1 ->") {
		t.Errorf("an absent target drew an edge:\n%s", out)
	}
	linear := mk("eng-12-fix-it", "r1", "", "")
	lid := keyIDs(func() scene { _, sc := arrange(newSlotState(), []planet{linear}); return sc }())[planetKey{linear.repo, linear.branch}]
	for _, target := range []string{"ENG-12", "eng-12", "eng-12-fix-it"} {
		if out, _ := render([]planet{linear}, target); !strings.Contains(out, "  u1 -> "+lid+" [") {
			t.Errorf("linear target %q drew no edge", target)
		}
	}
	if out, _ := render([]planet{linear}, "ENG-1", "ENG-120"); strings.Contains(out, "u1 ->") {
		t.Errorf("a longer or shorter linear id matched:\n%s", out)
	}
	grok := mk("feat/grok-4-7-x", "r1", "4", "")
	if out, _ := render([]planet{grok}, "#4"); strings.Contains(out, "u1 ->") {
		t.Errorf("#4 drew an edge to a grok-4-7 planet:\n%s", out)
	}
	other := mk("feat/12-x", "r2", "12", "")
	if out, _ := render([]planet{other}, "12", "feat/12-x"); strings.Contains(out, "u1 ->") {
		t.Errorf("a planet of another repo matched:\n%s", out)
	}
	if out, _ := render([]planet{r1}, "12", "feat/12-x", "31"); strings.Count(out, "u1 ->") != 1 {
		t.Errorf("want one edge per planet:\n%s", out)
	}
}

func TestLegendQueuedKey(t *testing.T) {
	if html := legendHTML(legend{queued: 2}, legendCounts); !strings.Contains(html, "queued 2") {
		t.Errorf("counts legend lacks the queued count:\n%s", html)
	}
	if html := legendHTML(legend{queued: 2}, legendFull); !strings.Contains(html, "dotted outer dot = queued task") {
		t.Errorf("full legend lacks the queued shape key:\n%s", html)
	}
	for _, mode := range []legendMode{legendCounts, legendFull} {
		if html := legendHTML(legend{}, mode); strings.Contains(html, "queued") {
			t.Errorf("queued key without tasks:\n%s", html)
		}
	}
	if !strings.Contains(legendSVG(legend{queued: 1}), "dotted outer dot = queued task") {
		t.Error("legend svg lacks the queued key")
	}
	if strings.Contains(legendSVG(legend{}), "queued task") {
		t.Error("legend svg has the queued key without tasks")
	}
}

func TestLegendAddSumsQueued(t *testing.T) {
	l := legend{queued: 1}
	l.add(legend{queued: 2})
	if l.queued != 3 {
		t.Errorf("queued = %d, want 3", l.queued)
	}
}
