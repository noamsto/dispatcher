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
)

// slotEntry is one worker's persisted seat: slot is ring*ringSlots + position,
// class the sector class it was seated for.
type slotEntry struct {
	Slot  int    `json:"slot"`
	Class string `json:"class"`
}

// slotState is roster-render.slots.json: what makes a redraw keep every worker
// where it was. Hosts is first-seen order and fixes each host's wedge.
type slotState struct {
	V       int                  `json:"v"`
	Workers map[string]slotEntry `json:"workers"`
	Hosts   []string             `json:"hosts"`
}

func newSlotState() slotState {
	return slotState{V: slotsVer, Workers: map[string]slotEntry{}, Hosts: []string{}}
}

// loadSlots reads the state file. Missing, unreadable, corrupt or another
// version all read as empty: the next redraw just seats everyone afresh.
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
type seat struct {
	remote bool
	ring   int
	slot   int
	angle  float64
}

func (s seat) index() int { return s.ring*ringSlots + s.slot }

func seatAt(remote bool, index int) seat {
	slot := index % ringSlots
	return seat{remote: remote, ring: index / ringSlots, slot: slot, angle: float64(slot) * slotDeg}
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
		next:    slotState{V: slotsVer, Workers: map[string]slotEntry{}, Hosts: slices.Clone(prev.Hosts)},
	}
	if a.next.Hosts == nil {
		a.next.Hosts = []string{}
	}
	a.order = make([]int, len(ps))
	for i := range ps {
		a.order[i] = i
		a.byName[ps[i].branch] = i
	}
	sort.Slice(a.order, func(x, y int) bool { return ps[a.order[x]].branch < ps[a.order[y]].branch })

	for _, i := range a.order {
		if h := ps[i].host; h != "" && !slices.Contains(a.next.Hosts, h) {
			a.next.Hosts = append(a.next.Hosts, h)
		}
	}
	for i, h := range a.next.Hosts {
		a.hostIdx[h] = i
	}

	for _, i := range a.order {
		e, ok := prev.Workers[ps[i].branch]
		if ok && (ps[i].host != "" || e.Class == ps[i].class) {
			a.keep(i, e.Slot)
		}
	}
	for _, i := range a.order {
		a.seat(i)
	}
	return a.next, a.seats
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
	if index < 0 || !a.allowed(i, index%ringSlots) || a.used[a.space(i)][index] {
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

// seat places planet i if it is not yet placed. Its base goes first so the
// child can sit beside it.
func (a *seater) seat(i int) {
	if a.placed[i] || a.visited[i] {
		return
	}
	a.visited[i] = true
	if j, ok := a.byName[a.ps[i].base]; ok {
		a.seat(j)
	}
	for _, partner := range a.partners(i) {
		if s, ok := a.beside(i, a.seats[partner]); ok {
			a.put(i, s)
			return
		}
	}
	a.put(i, a.nearest(i))
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
func (a *seater) offset(i int, slot int) float64 {
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

// nearest is the free slot closest to i's anchor, ring by ring.
func (a *seater) nearest(i int) seat {
	remote := a.ps[i].host != ""
	ring := 0
	if remote {
		ring = a.hostIdx[a.ps[i].host] / (ringSlots / wedgeSlots)
	}
	for ; ; ring++ {
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
			return seatAt(remote, ring*ringSlots+best)
		}
	}
}

// beside is the free neighbour of q (same ring) nearest i's anchor.
func (a *seater) beside(i int, q seat) (seat, bool) {
	best, bestD, found := 0, 0.0, false
	for _, step := range []int{1, -1} {
		slot := q.slot + step
		if q.remote {
			if slot < 0 || slot >= ringSlots {
				continue
			}
		} else {
			slot = (slot + ringSlots) % ringSlots
		}
		if !a.allowed(i, slot) || a.used[a.space(i)][q.ring*ringSlots+slot] {
			continue
		}
		if d := a.offset(i, slot); !found || closer(d, bestD) {
			best, bestD, found = slot, d, true
		}
	}
	return seatAt(q.remote, q.ring*ringSlots+best), found
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
	order := make([]int, len(ps))
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(x, y int) bool { return ps[order[x]].branch < ps[order[y]].branch })

	drawn := map[string]bool{}
	for _, i := range order {
		s := seats[i]
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
		x, y := polar(r, (float64(col*wedgeSlots)+(wedgeSlots-1)/2.0)*slotDeg)
		sc.hosts = append(sc.hosts, hostLabel{host: h, x: x, y: y})
		sc.extent = max(sc.extent, r+hostLabelHalf)
	}
	return next, sc
}
