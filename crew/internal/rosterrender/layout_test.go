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
	return planet{branch: branch, class: class}
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
		if d := math.Hypot(p.x-q.x, p.y-q.y); d < 2*cardReach {
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
				outer = max(outer, math.Hypot(p.x, p.y)+cardReach)
				side[0] = side[0] || p.x > -1e-6
				side[1] = side[1] || p.x < 1e-6
				side[2] = side[2] || p.y > -1e-6
				side[3] = side[3] || p.y < 1e-6
			}
			if sc.extent < outer-1e-6 || sc.extent-outer > 60 {
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

func repoWP(repo, branch string) planet {
	p := wp(branch, classWorking)
	p.repo, p.repoLabel = repo, repo
	return p
}

// repoUniform returns n working planets named w00, w01, ... in one repo.
func repoUniform(repo string, n int) []planet {
	ps := uniform(n)
	for i := range ps {
		ps[i].repo, ps[i].repoLabel = repo, repo
	}
	return ps
}

func wedgeOf(t *testing.T, sc scene, repo string) wedge {
	t.Helper()
	for _, w := range sc.wedges {
		if w.repo == repo {
			return w
		}
	}
	t.Fatalf("no wedge for %q in %+v", repo, sc.wedges)
	return wedge{}
}

func repoBranches(sc scene, repo string) []string {
	var out []string
	for _, p := range sc.planets {
		if p.repo == repo {
			out = append(out, p.branch)
		}
	}
	return out
}

type slotPos struct {
	ring  int
	angle float64
}

func positions(sc scene) map[[2]string]slotPos {
	out := map[[2]string]slotPos{}
	for _, p := range sc.planets {
		out[[2]string{p.repo, p.branch}] = slotPos{p.ring, p.angle}
	}
	return out
}

func TestWedgesPartitionTheCircleAndHoldTheirPlanets(t *testing.T) {
	ps := append(repoUniform("a", 5), repoUniform("b", 1)...)
	ps = append(ps, repoUniform("c", 3)...)
	state, sc := arrange(newSlotState(), ps)
	if len(sc.wedges) != 3 || len(sc.planets) != 9 || len(state.Order) != 9 {
		t.Fatalf("wedges %d, planets %d, persisted %d", len(sc.wedges), len(sc.planets), len(state.Order))
	}
	next, total := 0.0, 0.0
	for _, w := range sc.wedges {
		if !near(w.start, next) {
			t.Errorf("wedge %q starts at %v, want %v", w.repo, w.start, next)
		}
		next = w.start + w.span
		total += w.span
	}
	if !near(total, 360) {
		t.Errorf("spans sum to %v", total)
	}
	if got, want := sc.wedges[0].repo+sc.wedges[1].repo+sc.wedges[2].repo, "abc"; got != want {
		t.Errorf("wedge order %q, want %q", got, want)
	}
	// weights 5, 2 (floor) and 3
	if want := 360 * 2.0 / 10; !near(sc.wedges[1].span, want) {
		t.Errorf("one-planet wedge spans %v, want %v", sc.wedges[1].span, want)
	}
	for _, p := range sc.planets {
		w := wedgeOf(t, sc, p.repo)
		if p.angle <= w.start || p.angle >= w.start+w.span {
			t.Errorf("%s/%s at %v outside wedge [%v, %v]", p.repo, p.branch, p.angle, w.start, w.start+w.span)
		}
		if !near(compass(p.x, p.y), p.angle) {
			t.Errorf("%s/%s at %v but angle %v", p.repo, p.branch, compass(p.x, p.y), p.angle)
		}
		if p.ring == 0 && !near(math.Hypot(p.x, p.y), minRadius) {
			t.Errorf("%s/%s ring 0 at radius %v", p.repo, p.branch, math.Hypot(p.x, p.y))
		}
	}
	for _, w := range sc.wedges {
		if w.label != w.repo {
			t.Errorf("wedge %q labelled %q", w.repo, w.label)
		}
	}
}

func TestWedgesKeepSameBranchInTwoReposApart(t *testing.T) {
	ps := []planet{repoWP("a", "feat"), repoWP("b", "feat"), repoWP("a", "z")}
	state, sc := arrange(newSlotState(), ps)
	if len(sc.planets) != 3 {
		t.Fatalf("drawn %d, want 3", len(sc.planets))
	}
	pos := positions(sc)
	if pos[[2]string{"a", "feat"}] == pos[[2]string{"b", "feat"}] {
		t.Errorf("the same branch in two repos shares a place")
	}

	path := filepath.Join(t.TempDir(), "slots.json")
	if err := saveSlots(path, state, nil); err != nil {
		t.Fatal(err)
	}
	if got := loadSlots(path); !reflect.DeepEqual(got, state) || len(got.Order) != 3 {
		t.Errorf("round trip = %+v, want %+v", got, state)
	}
}

func TestWedgesStackResolvesOnlyInsideItsRepo(t *testing.T) {
	c := stackedWP("c", classWorking, "a")
	c.repo, c.repoLabel = "a", "a"
	ps := []planet{repoWP("a", "a"), repoWP("a", "z"), c, repoWP("b", "a"), repoWP("b", "b"), repoWP("b", "c")}
	_, sc := arrange(newSlotState(), ps)
	if got, want := repoBranches(sc, "a"), []string{"a", "c", "z"}; !slices.Equal(got, want) {
		t.Errorf("repo a = %v, want %v", got, want)
	}
	if got, want := repoBranches(sc, "b"), []string{"a", "b", "c"}; !slices.Equal(got, want) {
		t.Errorf("repo b = %v, want %v", got, want)
	}
}

func TestWedgesSamePlaceOnRedraw(t *testing.T) {
	ps := append(repoUniform("a", 6), repoUniform("b", 4)...)
	state, sc := arrange(newSlotState(), ps)
	again, sc2 := arrange(state, ps)
	if !reflect.DeepEqual(positions(sc), positions(sc2)) || !reflect.DeepEqual(again, state) {
		t.Errorf("a redraw moved planets")
	}
}

func TestWedgesGrowingARepoKeepsTheOthersOrder(t *testing.T) {
	a := []planet{repoWP("a", "a1"), repoWP("a", "a2"), repoWP("a", "a3")}
	b := []planet{repoWP("b", "b1"), repoWP("b", "b2")}
	state, sc := arrange(newSlotState(), append(slices.Clone(a), b...))
	wantA, wantB := repoBranches(sc, "a"), repoBranches(sc, "b")

	grown := append(slices.Clone(a), repoWP("a", "a0"))
	state, sc = arrange(state, append(grown, b...))
	if got := repoBranches(sc, "b"); !slices.Equal(got, wantB) {
		t.Errorf("repo b = %v after a grew, want %v", got, wantB)
	}
	if got := repoBranches(sc, "a"); !slices.Equal(got, append(slices.Clone(wantA), "a0")) {
		t.Errorf("repo a = %v, want %v then the new one", got, wantA)
	}
	if sc.wedges[0].repo != "a" || sc.wedges[1].repo != "b" {
		t.Errorf("wedge order changed: %+v", sc.wedges)
	}

	// A new repo sorting before both still goes after them.
	ps := append(append(slices.Clone(grown), b...), repoWP("0", "n1"))
	_, sc = arrange(state, ps)
	var order []string
	for _, w := range sc.wedges {
		order = append(order, w.repo)
	}
	if want := []string{"a", "b", "0"}; !slices.Equal(order, want) {
		t.Errorf("wedge order = %v, want %v", order, want)
	}
	if got := repoBranches(sc, "b"); !slices.Equal(got, wantB) {
		t.Errorf("repo b = %v after a third repo, want %v", got, wantB)
	}
	if got, want := repoBranches(sc, "a"), append(slices.Clone(wantA), "a0"); !slices.Equal(got, want) {
		t.Errorf("repo a = %v after a third repo, want %v", got, want)
	}
}

func TestWedgesLoadAStateFromTheSingleRepoEra(t *testing.T) {
	path := filepath.Join(t.TempDir(), "slots.json")
	body := `{"v":2,"order":[{"branch":"a1","class":"working"},{"branch":"b1","class":"working"},{"branch":"a2","class":"working"}]}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	ps := []planet{repoWP("a", "a1"), repoWP("a", "a2"), repoWP("b", "b1")}
	state, sc := arrange(loadSlots(path), ps)
	if len(sc.wedges) != 2 || len(sc.planets) != 3 || len(state.Order) != 3 {
		t.Fatalf("wedges %d, planets %d, persisted %d", len(sc.wedges), len(sc.planets), len(state.Order))
	}
	for _, e := range state.Order {
		if e.Repo == "" {
			t.Errorf("entry %+v persisted without its repo", e)
		}
	}
}

func TestWedgesAWedgeOfManyPlanetsUsesOuterRings(t *testing.T) {
	ps := append(repoUniform("a", 40), repoUniform("b", 2)...)
	_, sc := arrange(newSlotState(), ps)
	if len(sc.planets) != 42 {
		t.Fatalf("drawn %d, want 42", len(sc.planets))
	}
	w := wedgeOf(t, sc, "a")
	counts := map[int]int{}
	outer := 0.0
	for _, p := range sc.planets {
		if p.repo != "a" {
			continue
		}
		counts[p.ring]++
		outer = max(outer, math.Hypot(p.x, p.y))
		if want := minRadius + float64(p.ring)*ringStep; !near(math.Hypot(p.x, p.y), want) {
			t.Errorf("ring %d planet at radius %v, want %v", p.ring, math.Hypot(p.x, p.y), want)
		}
	}
	if len(counts) < 2 {
		t.Fatalf("40 planets on %d rings", len(counts))
	}
	last := 0
	for ring := range counts {
		last = max(last, ring)
	}
	for ring := 0; ring <= last; ring++ {
		fit := int((minRadius + float64(ring)*ringStep) * w.span * math.Pi / 180 / arcPerPlanet)
		switch n := counts[ring]; {
		case fit == 0 && n != 0:
			t.Errorf("ring %d holds %d though no card fits it", ring, n)
		case n > fit:
			t.Errorf("ring %d holds %d, cap %d", ring, n, fit)
		case ring < last && n < fit:
			t.Errorf("ring %d holds %d but a further ring is in use", ring, n)
		}
	}
	if !near(w.rim, outer+cardReach+wedgeRimGap) {
		t.Errorf("rim %v, want %v", w.rim, outer+cardReach+wedgeRimGap)
	}
}

func TestWedgesTotalNeverExceedsCapacity(t *testing.T) {
	ps := append(repoUniform("a", 30), repoUniform("b", 30)...)
	ps = append(ps, repoUniform("c", 30)...)
	state, sc := arrange(newSlotState(), ps)
	if len(sc.planets) != maxPlanets || len(state.Order) != maxPlanets {
		t.Fatalf("drawn %d, persisted %d, want %d", len(sc.planets), len(state.Order), maxPlanets)
	}
	if n := len(repoBranches(sc, "a")) + len(repoBranches(sc, "b")); n != 60 {
		t.Errorf("earlier repos hold %d, want all 60", n)
	}
	again, sc2 := arrange(state, ps)
	if len(sc2.planets) != maxPlanets || !reflect.DeepEqual(again, state) {
		t.Errorf("a redraw changed the capped state")
	}
}

func TestWedgesOneRepoIsTheRingLayout(t *testing.T) {
	for name, repo := range map[string]string{"empty": "", "named": "a"} {
		t.Run(name, func(t *testing.T) {
			ps := uniform(5)
			ps[1].host = "mbp"
			for i := range ps {
				ps[i].repo, ps[i].repoLabel = repo, repo
			}
			state, sc := arrange(newSlotState(), ps)
			wantState, wantSc := arrangeRings(newSlotState(), ps)
			if !reflect.DeepEqual(state, wantState) || !reflect.DeepEqual(sc, wantSc) {
				t.Errorf("single-repo layout differs from the ring layout")
			}
			if len(sc.wedges) != 0 {
				t.Errorf("wedges = %+v", sc.wedges)
			}
			for _, e := range state.Order {
				if e.Repo != repo {
					t.Errorf("entry %+v, want repo %q", e, repo)
				}
			}
		})
	}
}

func TestWedgesInputOrderDoesNotMatter(t *testing.T) {
	ps := append(repoUniform("a", 4), repoUniform("b", 3)...)
	ps = append(ps, repoWP("c", "x"), repoWP("c", "feat"))
	ps[1].host = "mbp"
	rev := slices.Clone(ps)
	slices.Reverse(rev)
	sx, x := arrange(newSlotState(), ps)
	sy, y := arrange(newSlotState(), rev)
	if !reflect.DeepEqual(sx, sy) || !reflect.DeepEqual(x, y) {
		t.Errorf("the scene depends on the order planets arrive in")
	}
}

func TestWedgesExtentHoldsEverything(t *testing.T) {
	ps := append(repoUniform("a", 20), repoUniform("b", 3)...)
	ps[0].moons = []moon{{}, {working: true}}
	ps[1].host, ps[2].host, ps[21].host = "mbp", "mbp", "nas"
	_, sc := arrange(newSlotState(), ps)
	if len(sc.hosts) != 2 {
		t.Fatalf("hosts = %+v", sc.hosts)
	}
	check := func(what string, r float64) {
		t.Helper()
		if sc.extent < r-1e-9 {
			t.Errorf("extent %v inside %s at %v", sc.extent, what, r)
		}
	}
	for _, p := range sc.planets {
		check(p.branch, math.Hypot(p.x, p.y)+cardReach)
		for _, m := range p.moons {
			check("a moon", math.Hypot(m.x, m.y)+moonRadius)
		}
	}
	for _, w := range sc.wedges {
		check("wedge "+w.repo, w.rim+wedgeLabelGap+hostLabelHalf)
		check("label "+w.repo, math.Hypot(w.x, w.y)+hostLabelHalf)
		if !near(compass(w.x, w.y), w.start+w.span/2) {
			t.Errorf("label of %q at %v, wedge middle %v", w.repo, compass(w.x, w.y), w.start+w.span/2)
		}
	}
	for _, h := range sc.hosts {
		check("host "+h.host, math.Hypot(h.x, h.y)+hostLabelHalf)
		for _, w := range sc.wedges {
			a := compass(h.x, h.y)
			if a < w.start || a > w.start+w.span {
				continue
			}
			if r := math.Hypot(h.x, h.y); r < w.rim+wedgeLabelGap+2*hostLabelHalf {
				t.Errorf("host %s at %v overlaps the label band of wedge %s", h.host, r, w.repo)
			}
		}
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
