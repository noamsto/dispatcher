package rosterrender

import (
	"fmt"
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

// drawn is one redraw's sequence: every drawn worker in board order and the
// repos in lane order.
type drawn struct {
	order []slotEntry
	lanes []string
}

// arrange is one redraw: the board built over prev, as its next slot state and
// what it drew.
func arrange(prev slotState, ps []planet) (slotState, drawn) {
	_, next := buildBoard(ps, nil, nil, prev, legend{})
	d := drawn{order: next.Order}
	for _, e := range next.Order {
		if !slices.Contains(d.lanes, e.Repo) {
			d.lanes = append(d.lanes, e.Repo)
		}
	}
	return next, d
}

func branches(d drawn) []string {
	out := make([]string, len(d.order))
	for i, e := range d.order {
		out[i] = e.Branch
	}
	return out
}

func repoBranches(d drawn, repo string) []string {
	var out []string
	for _, e := range d.order {
		if e.Repo == repo {
			out = append(out, e.Branch)
		}
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

func TestSequenceBlockedFirstThenClockwise(t *testing.T) {
	ps := []planet{wp("d", classDone), wp("p", classPROpen), wp("w", classWorking), wp("b", classBlocked)}
	_, sc := arrange(newSlotState(), ps)
	if got, want := branches(sc), []string{"b", "w", "p", "d"}; !slices.Equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

func TestSequenceOrderStableWhenAnotherWorkerFlipsOrJoins(t *testing.T) {
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

func TestSequenceOrderKeptWhenWorkerVanishes(t *testing.T) {
	ps := uniform(5)
	state, _ := arrange(newSlotState(), ps)
	_, sc := arrange(state, slices.Delete(slices.Clone(ps), 1, 3))
	if got, want := branches(sc), []string{"w00", "w03", "w04"}; !slices.Equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

func TestSequenceInputOrderDoesNotMatter(t *testing.T) {
	ps := []planet{wp("a", classWorking), wp("b", classWorking), wp("c", classBlocked)}
	_, x := arrange(newSlotState(), ps)
	_, y := arrange(newSlotState(), []planet{ps[2], ps[1], ps[0]})
	if !slices.Equal(branches(x), branches(y)) {
		t.Errorf("%v vs %v", branches(x), branches(y))
	}
}

func TestSequenceStackedAdjacent(t *testing.T) {
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

func TestSequenceStackedChildOfADifferentClassStaysInTheStack(t *testing.T) {
	ps := []planet{wp("a", classWorking), stackedWP("c", classDone, "a")}
	state, _ := arrange(newSlotState(), ps)
	ps = append(ps, wp("b", classWorking))
	_, sc := arrange(state, ps)
	if got, want := branches(sc), []string{"a", "c", "b"}; !slices.Equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

func TestSequenceNewBlockedWorkerGoesFirstDespiteAStackedChild(t *testing.T) {
	ps := []planet{wp("w", classWorking), stackedWP("c", classBlocked, "w"), wp("x", classWorking)}
	state, _ := arrange(newSlotState(), ps)
	_, sc := arrange(state, append(ps, wp("n", classBlocked)))
	if got, want := branches(sc), []string{"n", "w", "c", "x"}; !slices.Equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

func TestSequenceLateBaseGathersItsStackedChild(t *testing.T) {
	ps := []planet{wp("a", classWorking), stackedWP("c", classWorking, "b"), wp("d", classWorking)}
	state, _ := arrange(newSlotState(), ps)
	_, sc := arrange(state, append(ps, wp("b", classWorking)))
	if got, want := branches(sc), []string{"a", "d", "b", "c"}; !slices.Equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

func TestSequenceWorkerGoingLocalTakesItsClassPlace(t *testing.T) {
	ps := []planet{wp("a", classBlocked), wp("b", classDone), remoteWP("r", classBlocked, "mbp")}
	state, _ := arrange(newSlotState(), ps)
	ps[2].host = ""
	_, sc := arrange(state, ps)
	if got, want := branches(sc), []string{"a", "r", "b"}; !slices.Equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

func TestSequenceRemoteHostsAreContiguousRuns(t *testing.T) {
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

func TestSequenceCapacityDropsExtraPlanetsButNeverDisplacesDrawnOnes(t *testing.T) {
	ps := uniform(maxPlanets + 1)
	ps[0].branch = "zzz" // sorts last, so it is the one that does not fit
	state, sc := arrange(newSlotState(), ps)
	if len(sc.order) != maxPlanets || len(state.Order) != maxPlanets {
		t.Fatalf("drawn %d, persisted %d, want %d", len(sc.order), len(state.Order), maxPlanets)
	}
	if slices.Contains(branches(sc), "zzz") {
		t.Fatalf("the overflow worker was drawn")
	}
	ps[0].branch = "000" // would sort first, but the drawn ones keep their place
	again, sc2 := arrange(state, ps)
	if slices.Contains(branches(sc2), "000") || !reflect.DeepEqual(again, state) {
		t.Errorf("a new worker displaced a drawn one")
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

func TestLanesKeepSameBranchInTwoReposApart(t *testing.T) {
	ps := []planet{repoWP("a", "feat"), repoWP("b", "feat"), repoWP("a", "z")}
	state, sc := arrange(newSlotState(), ps)
	if len(sc.order) != 3 {
		t.Fatalf("drawn %d, want 3", len(sc.order))
	}
	if got, want := repoBranches(sc, "a"), []string{"feat", "z"}; !slices.Equal(got, want) || !slices.Equal(repoBranches(sc, "b"), []string{"feat"}) {
		t.Errorf("repo a = %v, repo b = %v", got, repoBranches(sc, "b"))
	}

	path := filepath.Join(t.TempDir(), "slots.json")
	if err := saveSlots(path, state, nil); err != nil {
		t.Fatal(err)
	}
	if got := loadSlots(path); !reflect.DeepEqual(got, state) || len(got.Order) != 3 {
		t.Errorf("round trip = %+v, want %+v", got, state)
	}
}

func TestLanesStackResolvesOnlyInsideItsRepo(t *testing.T) {
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

func TestLanesSamePlaceOnRedraw(t *testing.T) {
	ps := append(repoUniform("a", 6), repoUniform("b", 4)...)
	state, sc := arrange(newSlotState(), ps)
	again, sc2 := arrange(state, ps)
	if !reflect.DeepEqual(sc, sc2) || !reflect.DeepEqual(again, state) {
		t.Errorf("a redraw moved workers")
	}
}

func TestLanesGrowingARepoKeepsTheOthersOrder(t *testing.T) {
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
	if !slices.Equal(sc.lanes, []string{"a", "b"}) {
		t.Errorf("lane order changed: %v", sc.lanes)
	}

	// A new repo sorting before both still goes after them.
	ps := append(append(slices.Clone(grown), b...), repoWP("0", "n1"))
	_, sc = arrange(state, ps)
	if want := []string{"a", "b", "0"}; !slices.Equal(sc.lanes, want) {
		t.Errorf("lane order = %v, want %v", sc.lanes, want)
	}
	if got := repoBranches(sc, "b"); !slices.Equal(got, wantB) {
		t.Errorf("repo b = %v after a third repo, want %v", got, wantB)
	}
	if got, want := repoBranches(sc, "a"), append(slices.Clone(wantA), "a0"); !slices.Equal(got, want) {
		t.Errorf("repo a = %v after a third repo, want %v", got, want)
	}
}

func TestLanesLoadAStateFromTheSingleRepoEra(t *testing.T) {
	path := filepath.Join(t.TempDir(), "slots.json")
	body := `{"v":2,"order":[{"branch":"a1","class":"working"},{"branch":"b1","class":"working"},{"branch":"a2","class":"working"}]}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	ps := []planet{repoWP("a", "a1"), repoWP("a", "a2"), repoWP("b", "b1")}
	state, sc := arrange(loadSlots(path), ps)
	if len(sc.lanes) != 2 || len(sc.order) != 3 || len(state.Order) != 3 {
		t.Fatalf("lanes %d, workers %d, persisted %d", len(sc.lanes), len(sc.order), len(state.Order))
	}
	for _, e := range state.Order {
		if e.Repo == "" {
			t.Errorf("entry %+v persisted without its repo", e)
		}
	}
}

func TestLanesTotalNeverExceedsCapacity(t *testing.T) {
	ps := append(repoUniform("a", 30), repoUniform("b", 30)...)
	ps = append(ps, repoUniform("c", 30)...)
	state, sc := arrange(newSlotState(), ps)
	if len(sc.order) != maxPlanets || len(state.Order) != maxPlanets {
		t.Fatalf("drawn %d, persisted %d, want %d", len(sc.order), len(state.Order), maxPlanets)
	}
	if n := len(repoBranches(sc, "a")) + len(repoBranches(sc, "b")); n != 60 {
		t.Errorf("earlier repos hold %d, want all 60", n)
	}
	again, sc2 := arrange(state, ps)
	if len(sc2.order) != maxPlanets || !reflect.DeepEqual(again, state) {
		t.Errorf("a redraw changed the capped state")
	}
}

func TestLanesInputOrderDoesNotMatter(t *testing.T) {
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

func TestSequenceRepairsAHandEditedSequence(t *testing.T) {
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
