package rosterrender

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
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

// seatsByBranch runs assign and keys the seats by branch.
func seatsByBranch(prev slotState, ps []planet) (slotState, map[string]seat) {
	next, seats := assign(prev, ps)
	out := make(map[string]seat, len(ps))
	for i, p := range ps {
		out[p.branch] = seats[i]
	}
	return next, out
}

func adjacent(a, b seat) bool {
	d := ((a.slot-b.slot)%ringSlots + ringSlots) % ringSlots
	return a.ring == b.ring && a.remote == b.remote && (d == 1 || d == ringSlots-1)
}

func TestLayoutLoneBlockedAtTwelve(t *testing.T) {
	_, seats := seatsByBranch(newSlotState(), []planet{wp("feat/1-a", classBlocked)})
	s := seats["feat/1-a"]
	if s.ring != 0 || s.slot != 0 || s.angle != 0 {
		t.Fatalf("seat = %+v, want ring 0 slot 0 angle 0", s)
	}
}

func TestLayoutSectorAnchors(t *testing.T) {
	ps := []planet{
		wp("b", classBlocked), wp("w", classWorking), wp("p", classPROpen), wp("d", classDone),
	}
	_, seats := seatsByBranch(newSlotState(), ps)
	for branch, want := range map[string]float64{"b": 0, "w": 90, "p": 180, "d": 270} {
		if got := seats[branch].angle; got != want {
			t.Errorf("%s angle = %v, want %v", branch, got, want)
		}
	}
}

func TestSlotsKeptWhenAnotherWorkerFlips(t *testing.T) {
	ps := []planet{wp("a", classWorking), wp("b", classWorking), wp("c", classWorking)}
	state, before := seatsByBranch(newSlotState(), ps)

	ps[1].class = classBlocked
	_, after := seatsByBranch(state, ps)
	for _, branch := range []string{"a", "c"} {
		if after[branch] != before[branch] {
			t.Errorf("%s moved: %+v -> %+v", branch, before[branch], after[branch])
		}
	}
	if after["b"].slot != 0 {
		t.Errorf("flipped worker slot = %d, want 0 (blocked anchor)", after["b"].slot)
	}
}

func TestSlotsKeptWhenWorkerAdded(t *testing.T) {
	ps := []planet{wp("a", classWorking), wp("b", classBlocked), wp("c", classPROpen)}
	state, before := seatsByBranch(newSlotState(), ps)

	ps = append(ps, wp("0-new", classBlocked), wp("z", classWorking))
	_, after := seatsByBranch(state, ps)
	for _, branch := range []string{"a", "b", "c"} {
		if after[branch] != before[branch] {
			t.Errorf("%s moved: %+v -> %+v", branch, before[branch], after[branch])
		}
	}
}

func TestSlotsReleasedWhenWorkerVanishes(t *testing.T) {
	state, seats := seatsByBranch(newSlotState(), []planet{wp("a", classBlocked), wp("b", classBlocked)})
	freed := seats["a"]

	state, _ = seatsByBranch(state, []planet{wp("b", classBlocked)})
	if _, ok := state.Workers["a"]; ok {
		t.Fatalf("vanished worker still in state: %+v", state.Workers)
	}
	_, after := seatsByBranch(state, []planet{wp("b", classBlocked), wp("c", classBlocked)})
	if after["c"] != freed {
		t.Errorf("new worker seat = %+v, want the released %+v", after["c"], freed)
	}
}

func TestLayoutTiesGoClockwise(t *testing.T) {
	_, seats := seatsByBranch(newSlotState(), []planet{wp("a", classBlocked), wp("b", classBlocked), wp("c", classBlocked)})
	if seats["a"].slot != 0 || seats["b"].slot != 1 || seats["c"].slot != 15 {
		t.Fatalf("slots = %d %d %d, want 0 1 15", seats["a"].slot, seats["b"].slot, seats["c"].slot)
	}
}

func TestLayoutStackedAdjacent(t *testing.T) {
	fillers := func() []planet {
		return []planet{wp("f1", classWorking), wp("f2", classWorking), wp("f3", classWorking)}
	}
	base, child := wp("feat/base", classWorking), wp("feat/child", classWorking)
	child.base = "feat/base"
	flipped := base
	flipped.class = classBlocked
	early, late := wp("feat/z-base", classWorking), wp("feat/a-child", classWorking)
	late.base = "feat/z-base"

	tests := []struct {
		name        string
		first, then []planet
		base, child string
	}{
		{"base first", append(fillers(), base), append(fillers(), base, child), "feat/base", "feat/child"},
		{"child first", append(fillers(), child), append(fillers(), child, base), "feat/base", "feat/child"},
		{"same pass child sorts first", nil, append(fillers(), late, early), "feat/z-base", "feat/a-child"},
		{"base reseated", append(fillers(), base, child), append(fillers(), flipped, child), "feat/base", "feat/child"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			state, _ := assign(newSlotState(), tc.first)
			_, seats := seatsByBranch(state, tc.then)
			if b, c := seats[tc.base], seats[tc.child]; !adjacent(b, c) {
				t.Fatalf("base %+v and child %+v not adjacent", b, c)
			}
		})
	}
}

func TestLayoutStackedFallsBackWhenBoxedIn(t *testing.T) {
	// w1 sits on slot 4 with w2 (5) and w3 (3) beside it, so the child has no
	// free neighbour of its base.
	boxed := []planet{wp("w1", classWorking), wp("w2", classWorking), wp("w3", classWorking)}
	state, seats := seatsByBranch(newSlotState(), boxed)
	if seats["w1"].slot != 4 || seats["w2"].slot != 5 || seats["w3"].slot != 3 {
		t.Fatalf("setup seats = %+v", seats)
	}
	child := wp("child", classWorking)
	child.base = "w1"
	_, got := seatsByBranch(state, append(boxed, child))
	if got["child"].slot != 6 {
		t.Fatalf("child seat = %+v, want the nearest free slot 6", got["child"])
	}
}

func TestLayoutRemoteWedge(t *testing.T) {
	ps := []planet{
		remoteWP("feat/1-a", classWorking, "mbp"),
		remoteWP("feat/2-b", classBlocked, "mbp"),
		remoteWP("feat/3-c", classWorking, "nuc"),
	}
	state, seats := seatsByBranch(newSlotState(), ps)
	a, b, c := seats["feat/1-a"], seats["feat/2-b"], seats["feat/3-c"]
	for _, s := range []seat{a, b, c} {
		if !s.remote {
			t.Fatalf("seat %+v not on the remote ring", s)
		}
	}
	if a.ring != b.ring || a.slot/wedgeSlots != b.slot/wedgeSlots {
		t.Errorf("mbp workers in different wedges: %+v %+v", a, b)
	}
	if a.slot == b.slot {
		t.Errorf("mbp workers share slot %d", a.slot)
	}
	if c.ring == a.ring && c.slot/wedgeSlots == a.slot/wedgeSlots {
		t.Errorf("second host shares the first host's wedge: %+v %+v", a, c)
	}
	if !reflect.DeepEqual(state.Hosts, []string{"mbp", "nuc"}) {
		t.Errorf("hosts = %v, want first-seen [mbp nuc]", state.Hosts)
	}
}

func TestLayoutRemoteWedgeOrderPersists(t *testing.T) {
	state, _ := seatsByBranch(newSlotState(), []planet{remoteWP("z", classWorking, "nuc")})
	_, seats := seatsByBranch(state, []planet{remoteWP("a", classWorking, "aaa"), remoteWP("z", classWorking, "nuc")})
	if seats["z"].slot/wedgeSlots == seats["a"].slot/wedgeSlots {
		t.Fatalf("new host took over an existing wedge: %+v", seats)
	}
	if got := seats["z"].slot / wedgeSlots; got != 0 {
		t.Errorf("first-seen host wedge = %d, want 0", got)
	}
}

func TestLayoutRemoteIgnoresClassChange(t *testing.T) {
	ps := []planet{remoteWP("a", classWorking, "mbp")}
	state, before := seatsByBranch(newSlotState(), ps)
	ps[0].class = classBlocked
	_, after := seatsByBranch(state, ps)
	if after["a"] != before["a"] {
		t.Fatalf("remote worker moved on class change: %+v -> %+v", before["a"], after["a"])
	}
}

func TestLayoutWedgeSpillsToNextRemoteRing(t *testing.T) {
	var ps []planet
	for _, b := range []string{"a", "b", "c", "d", "e"} {
		ps = append(ps, remoteWP(b, classWorking, "mbp"))
	}
	_, seats := seatsByBranch(newSlotState(), ps)
	rings := map[int]int{}
	for _, s := range seats {
		rings[s.ring]++
		if s.slot/wedgeSlots != 0 {
			t.Errorf("worker left the wedge: %+v", s)
		}
	}
	if rings[0] != 4 || rings[1] != 1 {
		t.Fatalf("remote rings = %v, want 4 on ring 0, 1 on ring 1", rings)
	}
}

func TestLayoutSecondRing(t *testing.T) {
	var ps []planet
	for i := range 17 {
		ps = append(ps, wp(string(rune('a'+i)), classWorking))
	}
	_, seats := seatsByBranch(newSlotState(), ps)
	seen := map[seat]bool{}
	byRing := map[int]int{}
	for _, s := range seats {
		if seen[s] {
			t.Fatalf("duplicate seat %+v", s)
		}
		seen[s] = true
		byRing[s.ring]++
	}
	if byRing[0] != 16 || byRing[1] != 1 {
		t.Fatalf("ring population = %v, want 16 on ring 0 and 1 on ring 1", byRing)
	}
	if seats["q"].ring != 1 {
		t.Errorf("17th worker ring = %d, want 1", seats["q"].ring)
	}
}

func TestLayoutInputOrderDoesNotMatter(t *testing.T) {
	ps := []planet{wp("c", classBlocked), wp("a", classBlocked), wp("b", classWorking), wp("d", classDone)}
	_, first := seatsByBranch(newSlotState(), ps)
	rev := []planet{ps[3], ps[2], ps[1], ps[0]}
	_, second := seatsByBranch(newSlotState(), rev)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("seats depend on input order:\n%+v\n%+v", first, second)
	}
}

func TestSlotsDuplicateOrNegativeStateIsRepaired(t *testing.T) {
	state := newSlotState()
	state.Workers["a"] = slotEntry{Slot: 4, Class: classWorking}
	state.Workers["b"] = slotEntry{Slot: 4, Class: classWorking}
	state.Workers["c"] = slotEntry{Slot: -3, Class: classWorking}
	_, seats := seatsByBranch(state, []planet{wp("a", classWorking), wp("b", classWorking), wp("c", classWorking)})
	seen := map[seat]bool{}
	for _, s := range seats {
		if seen[s] || s.slot < 0 {
			t.Fatalf("bad seats: %+v", seats)
		}
		seen[s] = true
	}
}

func TestSlotsJSONRoundTrip(t *testing.T) {
	state, _ := assign(newSlotState(), []planet{
		wp("a", classBlocked), wp("b", classWorking), remoteWP("c", classWorking, "mbp"),
	})
	path := filepath.Join(t.TempDir(), "roster-render.slots.json")
	if err := saveSlots(path, state, nil); err != nil {
		t.Fatal(err)
	}
	if got := loadSlots(path); !reflect.DeepEqual(got, state) {
		t.Fatalf("round trip:\n got %+v\nwant %+v", got, state)
	}
}

func TestSlotsLoadBadInputIsEmpty(t *testing.T) {
	for name, content := range map[string]string{
		"corrupt":         "{not json",
		"unknown version": `{"v":2,"workers":{"a":{"slot":3,"class":"working"}},"hosts":["x"]}`,
		"no version":      `{"workers":{"a":{"slot":3,"class":"working"}}}`,
		"wrong type":      `[1,2]`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "slots.json")
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			got := loadSlots(path)
			if !reflect.DeepEqual(got, newSlotState()) {
				t.Fatalf("state = %+v, want empty", got)
			}
		})
	}
	if got := loadSlots(filepath.Join(t.TempDir(), "missing")); !reflect.DeepEqual(got, newSlotState()) {
		t.Fatalf("missing file state = %+v, want empty", got)
	}
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

func TestLayoutCoordinates(t *testing.T) {
	ps := []planet{wp("b", classBlocked), wp("w", classWorking), wp("p", classPROpen)}
	_, sc := arrange(newSlotState(), ps)
	got := map[string]placed{}
	for _, p := range sc.planets {
		got[p.branch] = p
	}
	if b := got["b"]; !near(b.x, 0) || !near(b.y, ring0Radius) {
		t.Errorf("blocked at (%v,%v), want (0,%v): 12 o'clock is +y", b.x, b.y, ring0Radius)
	}
	if w := got["w"]; !near(w.x, ring0Radius) || !near(w.y, 0) {
		t.Errorf("working at (%v,%v), want (%v,0): 3 o'clock is +x", w.x, w.y, ring0Radius)
	}
	if p := got["p"]; !near(p.x, 0) || !near(p.y, -ring0Radius) {
		t.Errorf("pr_open at (%v,%v), want (0,-%v)", p.x, p.y, ring0Radius)
	}
}

func TestLayoutRingRadii(t *testing.T) {
	var ps []planet
	for i := range 17 {
		ps = append(ps, wp(string(rune('a'+i)), classBlocked))
	}
	ps = append(ps, remoteWP("r", classWorking, "mbp"))
	_, sc := arrange(newSlotState(), ps)
	for _, p := range sc.planets {
		r := math.Hypot(p.x, p.y)
		var want float64
		switch {
		case p.remote:
			want = ring0Radius + 2*ringStep
		case p.ring == 1:
			want = ring0Radius + ringStep
		default:
			want = ring0Radius
		}
		if !near(r, want) {
			t.Errorf("%s ring %d remote %v radius %v, want %v", p.branch, p.ring, p.remote, r, want)
		}
	}
}

func TestLayoutMoonsOutwardAndExtent(t *testing.T) {
	p := wp("b", classBlocked)
	p.moons = []moon{{role: "critic", working: true}, {role: "reviewer"}}
	_, sc := arrange(newSlotState(), []planet{p, remoteWP("r", classWorking, "mbp")})
	var pl placed
	for _, q := range sc.planets {
		if q.branch == "b" {
			pl = q
		}
	}
	if len(pl.moons) != 2 || !pl.moons[0].working || pl.moons[1].working {
		t.Fatalf("moons = %+v", pl.moons)
	}
	if pl.moons[0].x == pl.moons[1].x && pl.moons[0].y == pl.moons[1].y {
		t.Fatal("moons overlap")
	}
	for _, m := range pl.moons {
		if math.Hypot(m.x, m.y) <= math.Hypot(pl.x, pl.y) {
			t.Errorf("moon (%v,%v) is not outward of planet (%v,%v)", m.x, m.y, pl.x, pl.y)
		}
		if sc.extent < math.Hypot(m.x, m.y) {
			t.Errorf("extent %v inside moon %v", sc.extent, math.Hypot(m.x, m.y))
		}
	}
	if len(sc.hosts) != 1 || sc.hosts[0].host != "mbp" {
		t.Fatalf("host labels = %+v", sc.hosts)
	}
	h := sc.hosts[0]
	if sc.extent < math.Hypot(h.x, h.y) {
		t.Errorf("extent %v inside host label %v", sc.extent, math.Hypot(h.x, h.y))
	}
}

func TestSlotsLoadDropsOutOfRangeAndOversize(t *testing.T) {
	var hosts []string
	for i := range maxHosts + 5 {
		hosts = append(hosts, "h"+strconv.Itoa(i))
	}
	hostJSON, _ := json.Marshal(hosts)
	content := fmt.Sprintf(`{"v":1,"workers":{
		"huge":{"slot":1000000000000000000,"class":"working"},
		"neg":{"slot":-1,"class":"working"},
		"edge":{"slot":%d,"class":"working"},
		"ok":{"slot":%d,"class":"working"}},"hosts":%s}`, maxSlots, maxSlots-1, hostJSON)
	path := filepath.Join(t.TempDir(), "slots.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	got := loadSlots(path)
	if len(got.Workers) != 1 || got.Workers["ok"].Slot != maxSlots-1 {
		t.Errorf("workers = %+v, want only ok", got.Workers)
	}
	if len(got.Hosts) != maxHosts {
		t.Errorf("hosts = %d, want %d", len(got.Hosts), maxHosts)
	}

	if err := os.WriteFile(path, []byte(`{"v":1,"workers":{},"hosts":["a","a","","b"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := loadSlots(path).Hosts; !reflect.DeepEqual(got, []string{"a", "", "", "b"}) {
		t.Errorf("hosts = %q, want duplicates blanked", got)
	}
}

func TestLayoutIgnoresOutOfRangePersistedSlot(t *testing.T) {
	state := newSlotState()
	state.Workers["a"] = slotEntry{Slot: math.MaxInt, Class: classWorking}
	state.Workers["r"] = slotEntry{Slot: maxSlots * 1000, Class: classWorking}
	next, seats := seatsByBranch(state, []planet{wp("a", classWorking), remoteWP("r", classWorking, "mbp")})
	for branch, s := range seats {
		if !s.seated || s.ring >= kindRings {
			t.Errorf("%s seat = %+v", branch, s)
		}
		if next.Workers[branch].Slot >= maxSlots {
			t.Errorf("%s persisted slot %d", branch, next.Workers[branch].Slot)
		}
	}
}

func TestLayoutLocalCapacityDropsExtraPlanetsButCountsThem(t *testing.T) {
	n := maxSlots + 1
	rows := make([]map[string]any, n)
	for i := range rows {
		rows[i] = planetRow(fmt.Sprintf("feat/%03d-x", i), "working")
	}
	ps, lg := buildPlanets(planetModel(rows...), planetNow)
	state, sc := arrange(newSlotState(), ps)
	if len(sc.planets) != maxSlots || len(state.Workers) != maxSlots {
		t.Fatalf("drawn %d, persisted %d, want %d", len(sc.planets), len(state.Workers), maxSlots)
	}
	if lg.working != n {
		t.Errorf("legend working = %d, want all %d counted", lg.working, n)
	}
	for _, p := range sc.planets {
		if p.ring >= kindRings {
			t.Fatalf("%s on ring %d", p.branch, p.ring)
		}
	}
	// A worker that was drawn is never displaced by one that does not fit.
	again, sc2 := arrange(state, ps)
	if len(sc2.planets) != maxSlots || !reflect.DeepEqual(again, state) {
		t.Errorf("second pass changed the layout: %d drawn", len(sc2.planets))
	}
}

func TestLayoutRemoteCapacityDropsExtraHosts(t *testing.T) {
	var ps []planet
	for i := range maxHosts + 1 {
		ps = append(ps, remoteWP(fmt.Sprintf("w%02d", i), classWorking, fmt.Sprintf("h%02d", i)))
	}
	state, seats := seatsByBranch(newSlotState(), ps)
	drawn := 0
	for _, s := range seats {
		if s.seated {
			drawn++
			if s.ring >= kindRings {
				t.Errorf("seat %+v beyond the remote rings", s)
			}
		}
	}
	if drawn != maxHosts || len(state.Hosts) != maxHosts || len(state.Workers) != maxHosts {
		t.Fatalf("drawn %d, hosts %d, persisted %d, want %d", drawn, len(state.Hosts), len(state.Workers), maxHosts)
	}
}

func hostWorkers(hosts ...string) []planet {
	var ps []planet
	for _, h := range hosts {
		ps = append(ps, remoteWP("w-"+h, classWorking, h))
	}
	return ps
}

func wedgeOf(s seat) int { return s.ring*(ringSlots/wedgeSlots) + s.slot/wedgeSlots }

func TestSlotsGoneHostsFreeTheirWedgeIndex(t *testing.T) {
	state, _ := seatsByBranch(newSlotState(), hostWorkers("h1", "h2", "h3", "h4"))

	state, seats := seatsByBranch(state, hostWorkers("h5"))
	if s := seats["w-h5"]; s.ring != 0 || wedgeOf(s) != 0 {
		t.Errorf("h5 seat = %+v, want the first freed wedge on remote ring 0", s)
	}
	if !reflect.DeepEqual(state.Hosts, []string{"h5"}) {
		t.Errorf("hosts = %q, want trailing holes trimmed", state.Hosts)
	}
}

func TestSlotsSurvivingHostsKeepWedgesWhileHolesFill(t *testing.T) {
	state, first := seatsByBranch(newSlotState(), hostWorkers("h1", "h2", "h3", "h4"))

	state, second := seatsByBranch(state, hostWorkers("h2", "h3", "h5"))
	if !reflect.DeepEqual(state.Hosts, []string{"h5", "h2", "h3"}) {
		t.Fatalf("hosts = %q, want h5 in the first hole", state.Hosts)
	}
	for _, h := range []string{"w-h2", "w-h3"} {
		if second[h] != first[h] {
			t.Errorf("%s moved: %+v -> %+v", h, first[h], second[h])
		}
	}

	state, third := seatsByBranch(state, hostWorkers("h1", "h2", "h3", "h5"))
	wedges := map[int]string{}
	for h, s := range third {
		if other, dup := wedges[wedgeOf(s)]; dup {
			t.Errorf("%s and %s share wedge %d", h, other, wedgeOf(s))
		}
		wedges[wedgeOf(s)] = h
	}
	if third["w-h2"] != first["w-h2"] || state.Hosts[3] != "h1" {
		t.Errorf("hosts = %q, h2 %+v", state.Hosts, third["w-h2"])
	}
}

func TestLayoutStackedChildStaysNearBaseWhenNeighboursTaken(t *testing.T) {
	for _, tc := range []struct {
		class string
		slot  int
	}{{classWorking, 2}, {classPROpen, 14}} {
		t.Run(tc.class, func(t *testing.T) {
			ps := []planet{wp("a-base", classBlocked), wp("b1", classBlocked), wp("b2", classBlocked)}
			state, seats := seatsByBranch(newSlotState(), ps)
			if seats["a-base"].slot != 0 || seats["b1"].slot != 1 || seats["b2"].slot != 15 {
				t.Fatalf("setup seats = %+v", seats)
			}
			child := wp("child", tc.class)
			child.base = "a-base"
			_, got := seatsByBranch(state, append(ps, child))
			if c := got["child"]; c.ring != 0 || c.slot != tc.slot {
				t.Fatalf("child seat = %+v, want ring 0 slot %d (two from its base)", c, tc.slot)
			}
		})
	}
}

func TestLayoutRemoteSeatsSitBetweenInnerAngles(t *testing.T) {
	s := seatAt(true, 5)
	if want := 5.5 * slotDeg; s.angle != want || s.index() != 5 {
		t.Fatalf("remote seat = %+v, want angle %v and index 5", s, want)
	}
	if got := seatAt(false, 5).angle; got != 5*slotDeg {
		t.Errorf("local angle = %v", got)
	}

	ps := []planet{wp("a", classBlocked), remoteWP("r", classWorking, "mbp")}
	_, sc := arrange(newSlotState(), ps)
	if len(sc.hosts) != 1 {
		t.Fatalf("hosts = %+v", sc.hosts)
	}
	h := sc.hosts[0]
	var r placed
	for _, p := range sc.planets {
		if p.remote {
			r = p
		}
	}
	// One wedge holds slots 0-3 at 0.5..3.5 slots; its label sits at their centre.
	if got, want := compass(h.x, h.y), 2*slotDeg; !near(got, want) {
		t.Errorf("host label angle = %v, want %v", got, want)
	}
	if got := compass(r.x, r.y); !near(math.Mod(got/slotDeg, 1), 0.5) {
		t.Errorf("remote planet angle %v is not half a slot off the inner grid", got)
	}
}

// compass is the clockwise angle from 12 o'clock of a point, in degrees.
func compass(x, y float64) float64 { return math.Mod(math.Atan2(x, y)*180/math.Pi+360, 360) }

func TestLayoutEmptyRosterHasExtent(t *testing.T) {
	state, sc := arrange(newSlotState(), nil)
	if sc.extent <= ring0Radius || len(sc.planets) != 0 || len(state.Workers) != 0 {
		t.Fatalf("scene = %+v state = %+v", sc, state)
	}
}
