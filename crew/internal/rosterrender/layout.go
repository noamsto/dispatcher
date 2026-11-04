package rosterrender

import (
	"encoding/json"
	"io"
	"math"
	"os"
	"slices"
	"sort"
)

// Geometry. A ring holds ringSlots planets, slotDeg apart clockwise from 12
// o'clock; coordinates are points with y up. A remote ring is cut into wedges of
// wedgeSlots, one per host.
const (
	ringSlots  = 16
	slotDeg    = 360.0 / ringSlots
	wedgeSlots = 4

	ring0Radius = 240.0
	ringStep    = 90.0

	moonGap    = 14.0
	moonStep   = 14.0
	moonRadius = 6.0

	hostLabelGap  = 55.0
	hostLabelStep = 22.0
	hostLabelHalf = 20.0

	minExtent = ring0Radius + 40
	slotsVer  = 1

	// The picture holds maxRings rings, half local and half remote, so a bad
	// state file or a runaway roster cannot push planets out of frame.
	maxRings  = 8
	kindRings = maxRings / 2
	maxSlots  = kindRings * ringSlots
	maxHosts  = kindRings * (ringSlots / wedgeSlots)
)

// slotEntry is one worker's persisted seat: slot is ring*ringSlots + position,
// class the sector class it was seated for.
type slotEntry struct {
	Slot  int    `json:"slot"`
	Class string `json:"class"`
}

// slotState is roster-render.slots.json: what makes a redraw keep every worker
// where it was. Hosts is positional: a host's index fixes its wedge, and a host
// that left is a "" hole the next new host fills.
type slotState struct {
	V       int                  `json:"v"`
	Workers map[string]slotEntry `json:"workers"`
	Hosts   []string             `json:"hosts"`
}

func newSlotState() slotState {
	return slotState{V: slotsVer, Workers: map[string]slotEntry{}, Hosts: []string{}}
}

// loadSlots reads the state file. Missing, unreadable, corrupt or another
// version all read as empty: the next redraw just seats everyone afresh. Seats
// outside the picture and surplus or repeated hosts are dropped.
func loadSlots(path string) slotState {
	data, err := os.ReadFile(path)
	if err != nil {
		return newSlotState()
	}
	var s slotState
	if json.Unmarshal(data, &s) != nil || s.V != slotsVer {
		return newSlotState()
	}
	if s.Workers == nil {
		s.Workers = map[string]slotEntry{}
	}
	if s.Hosts == nil {
		s.Hosts = []string{}
	}
	for branch, e := range s.Workers {
		if e.Slot < 0 || e.Slot >= maxSlots {
			delete(s.Workers, branch)
		}
	}
	s.Hosts = s.Hosts[:min(len(s.Hosts), maxHosts)]
	seen := map[string]bool{}
	for i, h := range s.Hosts {
		if seen[h] {
			s.Hosts[i] = ""
		}
		seen[h] = true
	}
	return s
}

// saveSlots writes the state through put, so a reader never sees half of it.
func saveSlots(path string, s slotState, stderr io.Writer) error {
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return put(path, string(data)+"\n", stderr)
}

// seat is where one planet sits: a local or remote ring and a position on it.
// The zero seat is "no room": seated is false.
type seat struct {
	seated bool
	remote bool
	ring   int
	slot   int
	angle  float64
}

func (s seat) index() int { return s.ring*ringSlots + s.slot }

// seatAt turns a slot index into a seat. A remote seat sits half a slot off the
// local grid, so no spoke to a remote planet runs under an inner one.
func seatAt(remote bool, index int) seat {
	slot := index % ringSlots
	angle := float64(slot) * slotDeg
	if remote {
		angle += slotDeg / 2
	}
	return seat{seated: true, remote: remote, ring: index / ringSlots, slot: slot, angle: angle}
}

// seater holds one assign run. used[0] is the local rings' taken slots,
// used[1] the remote rings'.
type seater struct {
	ps      []planet
	order   []int // indexes into ps, by branch
	seats   []seat
	placed  []bool
	visited []bool
	used    [2]map[int]bool
	hostIdx map[string]int
	byName  map[string]int
	next    slotState
}

// assign seats every planet and returns the state to persist. A planet keeps
// its persisted slot unless it is new or its class changed (a remote planet
// ignores class); the rest take the free slot nearest their anchor, bases ahead
// of their stacked children, a stacked pair beside each other when a neighbour
// is free. Slots of workers no longer drawn are released. The seats are
// index-aligned with ps; branch order, not input order, decides every tie.
func assign(prev slotState, ps []planet) (slotState, []seat) {
	a := &seater{
		ps:      ps,
		seats:   make([]seat, len(ps)),
		placed:  make([]bool, len(ps)),
		visited: make([]bool, len(ps)),
		used:    [2]map[int]bool{{}, {}},
		hostIdx: map[string]int{},
		byName:  map[string]int{},
		next:    slotState{V: slotsVer, Workers: map[string]slotEntry{}},
	}
	a.order = branchOrder(ps)
	drawn := map[string]bool{}
	for i := range ps {
		a.byName[ps[i].branch] = i
		drawn[ps[i].host] = true
	}

	// A host with no drawn planet leaves a hole, so the hosts after it keep their
	// wedges; a new host takes the first hole.
	hosts := slices.Clone(prev.Hosts)
	for k, h := range hosts {
		if !drawn[h] {
			hosts[k] = ""
		}
	}
	for _, i := range a.order {
		h := ps[i].host
		if h == "" || slices.Contains(hosts, h) {
			continue
		}
		if k := slices.Index(hosts, ""); k >= 0 {
			hosts[k] = h
		} else if len(hosts) < maxHosts {
			hosts = append(hosts, h)
		}
	}
	for k, h := range hosts {
		if h != "" {
			a.hostIdx[h] = k
		}
	}
	for len(hosts) > 0 && hosts[len(hosts)-1] == "" {
		hosts = hosts[:len(hosts)-1]
	}
	a.next.Hosts = append([]string{}, hosts...)

	for _, i := range a.order {
		e, ok := prev.Workers[ps[i].branch]
		if ok && a.hosted(i) && (ps[i].host != "" || e.Class == ps[i].class) {
			a.keep(i, e.Slot)
		}
	}
	for _, i := range a.order {
		a.seat(i)
	}
	return a.next, a.seats
}

// branchOrder is the indexes of ps sorted by branch.
func branchOrder(ps []planet) []int {
	order := make([]int, len(ps))
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(x, y int) bool { return ps[order[x]].branch < ps[order[y]].branch })
	return order
}

// hosted reports whether i has a wedge to sit in: a local planet always does, a
// remote one only if its host fit in the host list.
func (a *seater) hosted(i int) bool {
	_, ok := a.hostIdx[a.ps[i].host]
	return a.ps[i].host == "" || ok
}

func (a *seater) space(i int) int {
	if a.ps[i].host != "" {
		return 1
	}
	return 0
}

// column is the wedge a remote planet's host owns on every remote ring.
func (a *seater) column(i int) int { return a.hostIdx[a.ps[i].host] % (ringSlots / wedgeSlots) }

func (a *seater) allowed(i, slot int) bool {
	return a.ps[i].host == "" || slot/wedgeSlots == a.column(i)
}

// keep restores a persisted seat if it is still valid and free.
func (a *seater) keep(i, index int) {
	if index < 0 || index >= maxSlots || !a.allowed(i, index%ringSlots) || a.used[a.space(i)][index] {
		return
	}
	a.put(i, seatAt(a.ps[i].host != "", index))
}

func (a *seater) put(i int, s seat) {
	a.used[a.space(i)][s.index()] = true
	a.seats[i] = s
	a.placed[i] = true
	a.next.Workers[a.ps[i].branch] = slotEntry{Slot: s.index(), Class: a.ps[i].class}
}

// seat places planet i if it is not yet placed and there is room. Its base goes
// first so the child can sit beside it.
func (a *seater) seat(i int) {
	if a.placed[i] || a.visited[i] || !a.hosted(i) {
		return
	}
	a.visited[i] = true
	if j, ok := a.byName[a.ps[i].base]; ok {
		a.seat(j)
	}
	for _, partner := range a.partners(i) {
		if s, ok := a.near(i, a.seats[partner]); ok {
			a.put(i, s)
			return
		}
	}
	if s, ok := a.nearest(i); ok {
		a.put(i, s)
	}
}

// partners are the placed planets i is stacked with: its base, then the
// children built on it, all on the same host.
func (a *seater) partners(i int) []int {
	var out []int
	if j, ok := a.byName[a.ps[i].base]; ok && j != i {
		out = append(out, j)
	}
	for _, j := range a.order {
		if j != i && a.ps[j].base == a.ps[i].branch {
			out = append(out, j)
		}
	}
	return slices.DeleteFunc(out, func(j int) bool { return !a.placed[j] || a.ps[j].host != a.ps[i].host })
}

// anchor is the in-ring position i wants to be near.
func (a *seater) anchor(i int) float64 {
	if a.ps[i].host != "" {
		return float64(a.column(i)*wedgeSlots) + (wedgeSlots-1)/2.0
	}
	return sectorAnchor[a.ps[i].class] / slotDeg
}

// offset is the signed clockwise distance from anchor to slot; on a local ring
// it wraps, so 8 is the farthest either way.
func (a *seater) offset(i, slot int) float64 {
	d := float64(slot) - a.anchor(i)
	if a.ps[i].host != "" {
		return d
	}
	d = math.Mod(d, ringSlots)
	if d > ringSlots/2 {
		d -= ringSlots
	}
	if d <= -ringSlots/2 {
		d += ringSlots
	}
	return d
}

// closer reports whether offset d beats best: nearer, or equally near on the
// clockwise side.
func closer(d, best float64) bool {
	return math.Abs(d) < math.Abs(best) || (math.Abs(d) == math.Abs(best) && d > best)
}

// nearest is the free slot closest to i's anchor, ring by ring; false when its
// rings are full.
func (a *seater) nearest(i int) (seat, bool) {
	remote := a.ps[i].host != ""
	ring := 0
	if remote {
		ring = a.hostIdx[a.ps[i].host] / (ringSlots / wedgeSlots)
	}
	for ; ring < kindRings; ring++ {
		best, bestD := -1, 0.0
		for slot := range ringSlots {
			if !a.allowed(i, slot) || a.used[a.space(i)][ring*ringSlots+slot] {
				continue
			}
			if d := a.offset(i, slot); best < 0 || closer(d, bestD) {
				best, bestD = slot, d
			}
		}
		if best >= 0 {
			return seatAt(remote, ring*ringSlots+best), true
		}
	}
	return seat{}, false
}

// nearReach is how many slots from its partner a stacked planet may sit: any
// farther and the dashed edge between the pair is a chord across the circle.
const nearReach = 2

// near is the free slot within nearReach of q: on q's ring first, then on the
// next ring outward at q's angle, closest first; of two equally far it takes
// the one nearer i's anchor, then the clockwise one. False when none is free,
// and the caller falls back to nearest.
func (a *seater) near(i int, q seat) (seat, bool) {
	for ring := q.ring; ring <= min(q.ring+1, kindRings-1); ring++ {
		for d := range nearReach + 1 {
			if d == 0 && ring == q.ring {
				continue
			}
			best, bestOff, found := 0, 0.0, false
			for _, slot := range []int{q.slot + d, q.slot - d} {
				if q.remote && (slot < 0 || slot >= ringSlots) {
					continue
				}
				slot = (slot + ringSlots) % ringSlots
				if !a.allowed(i, slot) || a.used[a.space(i)][ring*ringSlots+slot] {
					continue
				}
				if off := a.offset(i, slot); !found || closer(off, bestOff) {
					best, bestOff, found = slot, off, true
				}
			}
			if found {
				return seatAt(q.remote, ring*ringSlots+best), true
			}
		}
	}
	return seat{}, false
}

// placed is a planet with its seat and its position in points.
type placed struct {
	planet
	seat
	x, y float64
}

// hostLabel is where a remote wedge's host name goes.
type hostLabel struct {
	host string
	x, y float64
}

// scene is the whole layout: planets in branch order, a label per drawn remote
// host in first-seen order, and extent, the radius of the smallest circle around
// everything (the emitter squares the canvas with it).
type scene struct {
	planets []placed
	hosts   []hostLabel
	extent  float64
}

func ringRadius(ring int) float64 { return ring0Radius + ringStep*float64(ring) }

func polar(r, deg float64) (x, y float64) {
	rad := deg * math.Pi / 180
	return r * math.Sin(rad), r * math.Cos(rad)
}

// arrange seats the planets and turns the seats into coordinates. Remote rings
// start just past the outermost local ring in use.
func arrange(prev slotState, ps []planet) (slotState, scene) {
	next, seats := assign(prev, ps)

	localRings, remoteRings := 1, 0
	for _, s := range seats {
		if s.remote {
			remoteRings = max(remoteRings, s.ring+1)
		} else {
			localRings = max(localRings, s.ring+1)
		}
	}
	radius := func(s seat) float64 {
		if s.remote {
			return ringRadius(localRings + s.ring)
		}
		return ringRadius(s.ring)
	}

	sc := scene{extent: minExtent}
	drawn := map[string]bool{}
	for _, i := range branchOrder(ps) {
		s := seats[i]
		if !s.seated {
			continue
		}
		r := radius(s)
		pl := placed{planet: ps[i], seat: s}
		pl.x, pl.y = polar(r, s.angle)
		pr := pl.size * 72 / 2
		sc.extent = max(sc.extent, r+pr)

		pl.moons = make([]moon, len(ps[i].moons))
		for k, m := range ps[i].moons {
			along := (float64(k) - float64(len(pl.moons)-1)/2) * moonStep
			ux, uy := polar(1, s.angle)
			m.x = pl.x + ux*(pr+moonGap) + uy*along
			m.y = pl.y + uy*(pr+moonGap) - ux*along
			pl.moons[k] = m
			sc.extent = max(sc.extent, math.Hypot(m.x, m.y)+moonRadius)
		}
		sc.planets = append(sc.planets, pl)
		if ps[i].host != "" {
			drawn[ps[i].host] = true
		}
	}

	for k, h := range next.Hosts {
		if !drawn[h] {
			continue
		}
		col := k % (ringSlots / wedgeSlots)
		r := ringRadius(localRings+remoteRings-1) + hostLabelGap + hostLabelStep*float64(k/(ringSlots/wedgeSlots))
		x, y := polar(r, (float64(col*wedgeSlots)+wedgeSlots/2.0)*slotDeg)
		sc.hosts = append(sc.hosts, hostLabel{host: h, x: x, y: y})
		sc.extent = max(sc.extent, r+hostLabelHalf)
	}
	return next, sc
}
