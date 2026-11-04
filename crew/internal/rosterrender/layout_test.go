package rosterrender

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
)

func wp(branch, class string) planet {
	return planet{branch: branch, class: class, size: 0.75}
}

func remoteWP(branch, class, host string) planet {
	p := wp(branch, class)
	p.host = host
	return p
}

func stackedWP(branch, class, base string) planet {
	p := wp(branch, class)
	p.base = base
	return p
}

// compass is the clockwise angle from 12 o'clock of a point, in degrees.
func compass(x, y float64) float64 { return math.Mod(math.Atan2(x, y)*180/math.Pi+360, 360) }

func near(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

func branches(sc scene) []string {
	out := make([]string, len(sc.planets))
	for i, p := range sc.planets {
		out[i] = p.branch
	}
	return out
}

func byBranch(sc scene) map[string]placed {
	out := map[string]placed{}
	for _, p := range sc.planets {
		out[p.branch] = p
	}
	return out
}

// uniform returns n working planets named w00, w01, ...
func uniform(n int) []planet {
	ps := make([]planet, n)
	for i := range ps {
		ps[i] = wp(fmt.Sprintf("w%02d", i), classWorking)
	}
	return ps
}

func TestLayoutEvenSpacingStartsAtTwelve(t *testing.T) {
	for _, n := range []int{1, 2, 3, 8, 16} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			_, sc := arrange(newSlotState(), uniform(n))
			if len(sc.planets) != n {
				t.Fatalf("drawn %d, want %d", len(sc.planets), n)
			}
			if got := sc.planets[0].angle; got != 0 {
				t.Errorf("first planet at %v degrees, want 12 o'clock", got)
			}
			for i, p := range sc.planets {
				next := sc.planets[(i+1)%n]
				gap := math.Mod(compass(next.x, next.y)-compass(p.x, p.y)+360, 360)
				if n == 1 {
					gap = 360
				}
				if !near(gap, 360/float64(n)) && math.Abs(gap-360/float64(n)) > 1e-6 {
					t.Errorf("gap %d→%d = %v, want %v", i, (i+1)%n, gap, 360/float64(n))
				}
			}
		})
	}
}

func TestLayoutBlockedFirstThenClockwise(t *testing.T) {
	ps := []planet{wp("d", classDone), wp("p", classPROpen), wp("w", classWorking), wp("b", classBlocked)}
	_, sc := arrange(newSlotState(), ps)
	if got, want := branches(sc), []string{"b", "w", "p", "d"}; !slices.Equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

func TestLayoutOrderStableWhenAnotherWorkerFlipsOrJoins(t *testing.T) {
	ps := []planet{wp("a", classWorking), wp("b", classWorking), wp("c", classWorking), wp("d", classWorking)}
	state, sc := arrange(newSlotState(), ps)
	if got, want := branches(sc), []string{"a", "b", "c", "d"}; !slices.Equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}

	ps[2].class = classBlocked
	state, sc = arrange(state, ps)
	if got, want := branches(sc), []string{"c", "a", "b", "d"}; !slices.Equal(got, want) {
		t.Fatalf("after flip = %v, want %v", got, want)
	}

	ps = append(ps, wp("e", classWorking), wp("0", classDone))
	_, sc = arrange(state, ps)
	if got, want := branches(sc), []string{"c", "a", "b", "d", "e", "0"}; !slices.Equal(got, want) {
		t.Fatalf("after join = %v, want %v", got, want)
	}
}

func TestLayoutOrderKeptWhenWorkerVanishes(t *testing.T) {
	ps := uniform(5)
	state, _ := arrange(newSlotState(), ps)
	_, sc := arrange(state, slices.Delete(slices.Clone(ps), 1, 3))
	if got, want := branches(sc), []string{"w00", "w03", "w04"}; !slices.Equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

func TestLayoutInputOrderDoesNotMatter(t *testing.T) {
	ps := []planet{wp("a", classWorking), wp("b", classWorking), wp("c", classBlocked)}
	_, x := arrange(newSlotState(), ps)
	_, y := arrange(newSlotState(), []planet{ps[2], ps[1], ps[0]})
	if !slices.Equal(branches(x), branches(y)) {
		t.Errorf("%v vs %v", branches(x), branches(y))
	}
}

func TestLayoutStackedAdjacent(t *testing.T) {
	ps := []planet{wp("a", classWorking), wp("z", classWorking), stackedWP("c", classWorking, "a"), wp("m", classBlocked)}
	state, sc := arrange(newSlotState(), ps)
	if got, want := branches(sc), []string{"m", "a", "c", "z"}; !slices.Equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}

	// The base finishes: it and its child move together, still adjacent.
	ps[0].class = classDone
	_, sc = arrange(state, ps)
	if got, want := branches(sc), []string{"m", "z", "a", "c"}; !slices.Equal(got, want) {
		t.Fatalf("after the base moved = %v, want %v", got, want)
	}
}

func TestLayoutStackedChildOfADifferentClassStaysInTheStack(t *testing.T) {
	ps := []planet{wp("a", classWorking), stackedWP("c", classDone, "a")}
	state, _ := arrange(newSlotState(), ps)
	ps = append(ps, wp("b", classWorking))
	_, sc := arrange(state, ps)
	if got, want := branches(sc), []string{"a", "c", "b"}; !slices.Equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

func TestLayoutNewBlockedWorkerGoesFirstDespiteAStackedChild(t *testing.T) {
	ps := []planet{wp("w", classWorking), stackedWP("c", classBlocked, "w"), wp("x", classWorking)}
	state, _ := arrange(newSlotState(), ps)
	_, sc := arrange(state, append(ps, wp("n", classBlocked)))
	if got, want := branches(sc), []string{"n", "w", "c", "x"}; !slices.Equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

func TestLayoutLateBaseGathersItsStackedChild(t *testing.T) {
	ps := []planet{wp("a", classWorking), stackedWP("c", classWorking, "b"), wp("d", classWorking)}
	state, _ := arrange(newSlotState(), ps)
	_, sc := arrange(state, append(ps, wp("b", classWorking)))
	if got, want := branches(sc), []string{"a", "d", "b", "c"}; !slices.Equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

func TestLayoutWorkerGoingLocalTakesItsClassPlace(t *testing.T) {
	ps := []planet{wp("a", classBlocked), wp("b", classDone), remoteWP("r", classBlocked, "mbp")}
	state, _ := arrange(newSlotState(), ps)
	ps[2].host = ""
	_, sc := arrange(state, ps)
	if got, want := branches(sc), []string{"a", "r", "b"}; !slices.Equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

func TestLayoutRemoteHostsAreContiguousRuns(t *testing.T) {
	ps := []planet{
		remoteWP("r1", classWorking, "mbp"), wp("a", classBlocked), remoteWP("s1", classDone, "nas"),
		wp("b", classDone), remoteWP("r2", classBlocked, "mbp"),
	}
	state, sc := arrange(newSlotState(), ps)
	if got, want := branches(sc), []string{"a", "b", "r1", "r2", "s1"}; !slices.Equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}

	// Class changes never move a remote worker; a new local worker stays local.
	ps[0].class = classDone
	ps = append(ps, wp("c", classWorking), remoteWP("s2", classWorking, "nas"), remoteWP("r3", classWorking, "mbp"))
	_, sc = arrange(state, ps)
	if got, want := branches(sc), []string{"a", "c", "b", "r1", "r2", "r3", "s1", "s2"}; !slices.Equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

func TestLayoutHostLabelSitsOutsideItsRun(t *testing.T) {
	ps := []planet{wp("a", classBlocked), remoteWP("r", classWorking, "mbp")}
	_, sc := arrange(newSlotState(), ps)
	if len(sc.hosts) != 1 {
		t.Fatalf("hosts = %+v", sc.hosts)
	}
	h, r := sc.hosts[0], byBranch(sc)["r"]
	if !near(compass(h.x, h.y), compass(r.x, r.y)) {
		t.Errorf("host label at %v, remote planet at %v", compass(h.x, h.y), compass(r.x, r.y))
	}
	if math.Hypot(h.x, h.y) <= math.Hypot(r.x, r.y) || sc.extent < math.Hypot(h.x, h.y) {
		t.Errorf("host label %v not outside the planet %v within extent %v", math.Hypot(h.x, h.y), math.Hypot(r.x, r.y), sc.extent)
	}
}

func TestLayoutSecondRingPastCapacity(t *testing.T) {
	for _, n := range []int{ringCap, ringCap + 1, 2 * ringCap} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			_, sc := arrange(newSlotState(), uniform(n))
			rings := map[int][]placed{}
			for _, p := range sc.planets {
				rings[p.ring] = append(rings[p.ring], p)
			}
			want := (n + ringCap - 1) / ringCap
			if len(rings) != want {
				t.Fatalf("rings = %d, want %d", len(rings), want)
			}
			for ring, ps := range rings {
				if len(ps) > ringCap {
					t.Errorf("ring %d holds %d", ring, len(ps))
				}
				step := 360 / float64(len(ps))
				for i := 1; i < len(ps); i++ {
					if gap := ps[i].angle - ps[i-1].angle; !near(gap, step) {
						t.Errorf("ring %d gap = %v, want %v", ring, gap, step)
					}
				}
			}
			if want > 1 && math.Hypot(rings[1][0].x, rings[1][0].y) <= math.Hypot(rings[0][0].x, rings[0][0].y) {
				t.Errorf("the second ring is not outside the first")
			}
		})
	}
}

func TestLayoutRadiusGrowsWithCountAndSpacingHolds(t *testing.T) {
	_, small := arrange(newSlotState(), uniform(2))
	_, big := arrange(newSlotState(), uniform(16))
	if !(small.extent < big.extent) {
		t.Errorf("extent %v with 2 workers, %v with 16", small.extent, big.extent)
	}
	for i, p := range big.planets {
		q := big.planets[(i+1)%len(big.planets)]
		if d := math.Hypot(p.x-q.x, p.y-q.y); d < p.size*72 {
			t.Errorf("planets %d and %d overlap: %v apart", i, (i+1)%len(big.planets), d)
		}
	}
}

// TestLayoutFitsTheCanvas: the extent is the content's radius plus a bounded
// margin, and from four workers up no quadrant of the picture is empty.
func TestLayoutFitsTheCanvas(t *testing.T) {
	for _, n := range []int{1, 2, 3, 4, 8, 16, 20} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			_, sc := arrange(newSlotState(), uniform(n))
			outer := 0.0
			var side [4]bool // right, left, top, bottom: planets on an axis count for both neighbours
			for _, p := range sc.planets {
				outer = max(outer, math.Hypot(p.x, p.y)+p.size*72/2)
				side[0] = side[0] || p.x > -1e-6
				side[1] = side[1] || p.x < 1e-6
				side[2] = side[2] || p.y > -1e-6
				side[3] = side[3] || p.y < 1e-6
			}
			if sc.extent < outer || sc.extent-outer > 60 {
				t.Errorf("extent %v around content radius %v", sc.extent, outer)
			}
			if n >= 4 && slices.Contains(side[:], false) {
				t.Errorf("an empty side: %v", side)
			}
		})
	}
}

func TestLayoutMoonsOutwardAndExtent(t *testing.T) {
	p := wp("a", classWorking)
	p.moons = []moon{{}, {working: true}}
	_, sc := arrange(newSlotState(), []planet{p})
	pl := sc.planets[0]
	for _, m := range pl.moons {
		if math.Hypot(m.x, m.y) <= math.Hypot(pl.x, pl.y) {
			t.Errorf("moon %v not outside its planet", m)
		}
		if sc.extent < math.Hypot(m.x, m.y) {
			t.Errorf("extent %v inside moon %v", sc.extent, math.Hypot(m.x, m.y))
		}
	}
}

func TestLayoutCapacityDropsExtraPlanetsButNeverDisplacesDrawnOnes(t *testing.T) {
	ps := uniform(maxPlanets + 1)
	ps[0].branch = "zzz" // sorts last, so it is the one that does not fit
	state, sc := arrange(newSlotState(), ps)
	if len(sc.planets) != maxPlanets || len(state.Order) != maxPlanets {
		t.Fatalf("drawn %d, persisted %d, want %d", len(sc.planets), len(state.Order), maxPlanets)
	}
	if byBranch(sc)["zzz"].branch != "" {
		t.Fatalf("the overflow worker was drawn")
	}
	ps[0].branch = "000" // would sort first, but the drawn ones keep their place
	again, sc2 := arrange(state, ps)
	if byBranch(sc2)["000"].branch != "" || !reflect.DeepEqual(again, state) {
		t.Errorf("a new worker displaced a drawn one")
	}
}

func TestLayoutEmptyRosterHasExtent(t *testing.T) {
	state, sc := arrange(newSlotState(), nil)
	if sc.extent < minRadius || len(sc.planets) != 0 || len(state.Order) != 0 {
		t.Fatalf("scene = %+v state = %+v", sc, state)
	}
}

func TestSlotsJSONRoundTrip(t *testing.T) {
	state, _ := arrange(newSlotState(), []planet{wp("a", classWorking), remoteWP("r", classBlocked, "mbp")})
	path := filepath.Join(t.TempDir(), "slots.json")
	if err := saveSlots(path, state, nil); err != nil {
		t.Fatal(err)
	}
	if got := loadSlots(path); !reflect.DeepEqual(got, state) {
		t.Errorf("round trip = %+v, want %+v", got, state)
	}
}

func TestSlotsLoadBadInputIsEmpty(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"corrupt":      "{not json",
		"other-v":      `{"v":99,"order":[{"branch":"a","class":"working"}]}`,
		"old-v1-state": `{"v":1,"workers":{"a":{"slot":0,"class":"working"}},"hosts":[]}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, name)
			if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			if got := loadSlots(path); !reflect.DeepEqual(got, newSlotState()) {
				t.Errorf("loadSlots = %+v, want empty", got)
			}
		})
	}
	if got := loadSlots(filepath.Join(dir, "missing")); !reflect.DeepEqual(got, newSlotState()) {
		t.Errorf("missing file = %+v", got)
	}
}

func TestSlotsLoadDropsRepeatedBlankAndOversize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "slots.json")
	body := `{"v":2,"order":[{"branch":"a","class":"working"},{"branch":"a","class":"done"},{"branch":"","class":"done"}`
	for i := range maxPlanets + 3 {
		body += fmt.Sprintf(`,{"branch":"x%d","class":"working"}`, i)
	}
	if err := os.WriteFile(path, []byte(body+"]}"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := loadSlots(path)
	if len(got.Order) != maxPlanets || got.Order[0] != (slotEntry{Branch: "a", Class: classWorking}) || got.Order[1].Branch != "x0" {
		t.Errorf("loaded %d entries, head %+v", len(got.Order), got.Order[:2])
	}
}

func TestLayoutRepairsAHandEditedSequence(t *testing.T) {
	state := slotState{V: slotsVer, Order: []slotEntry{
		{Branch: "r1", Class: classWorking, Host: "mbp"}, {Branch: "a", Class: classWorking},
		{Branch: "s1", Class: classWorking, Host: "nas"}, {Branch: "r2", Class: classWorking, Host: "mbp"},
		{Branch: "gone", Class: classWorking}, {Branch: "a", Class: classWorking},
	}}
	ps := []planet{remoteWP("r1", classWorking, "mbp"), wp("a", classWorking), remoteWP("s1", classWorking, "nas"), remoteWP("r2", classWorking, "mbp")}
	_, sc := arrange(state, ps)
	if got, want := branches(sc), []string{"a", "r1", "r2", "s1"}; !slices.Equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}
