package rosterrender

import (
	"encoding/json"
	"io"
	"math"
	"os"
	"slices"
	"sort"
)

// Geometry. Planets sit evenly around the dispatcher, clockwise from 12
// o'clock, in a persisted sequence; coordinates are points with y up. A ring
// holds at most ringCap planets, and a roster past that spreads over more rings,
// each evenly spaced too. A ring's radius grows with its count so the planets
// keep arcPerPlanet of room, and never drops below minRadius.
const (
	ringCap      = 16
	arcPerPlanet = 100.0
	minRadius    = 150.0
	ringStep     = 90.0

	moonGap    = 14.0
	moonStep   = 14.0
	moonRadius = 6.0

	hostLabelGap  = 55.0
	hostLabelHalf = 20.0

	minExtent = minRadius + 40
	slotsVer  = 2

	// maxPlanets bounds the picture, so a bad state file or a runaway roster
	// cannot push planets out of frame.
	maxPlanets = 4 * ringCap
)

// slotEntry is one worker in the persisted sequence, with the class and host it
// was placed for.
type slotEntry struct {
	Branch string `json:"branch"`
	Class  string `json:"class"`
	Host   string `json:"host,omitempty"`
}

// slotState is roster-render.slots.json: the ring sequence, which is what makes
// a redraw keep every worker's place relative to the others. Angles are not
// stored; they follow from the sequence length.
type slotState struct {
	V     int         `json:"v"`
	Order []slotEntry `json:"order"`
}

func newSlotState() slotState {
	return slotState{V: slotsVer, Order: []slotEntry{}}
}

// loadSlots reads the state file. Missing, unreadable, corrupt or another
// version all read as empty: the next redraw just orders everyone afresh.
// Repeated and oversize entries are dropped.
func loadSlots(path string) slotState {
	data, err := os.ReadFile(path)
	if err != nil {
		return newSlotState()
	}
	var s slotState
	if json.Unmarshal(data, &s) != nil || s.V != slotsVer {
		return newSlotState()
	}
	out := newSlotState()
	seen := map[string]bool{}
	for _, e := range s.Order {
		if e.Branch == "" || seen[e.Branch] || len(out.Order) >= maxPlanets {
			continue
		}
		seen[e.Branch] = true
		out.Order = append(out.Order, e)
	}
	return out
}

// saveSlots writes the state through put, so a reader never sees half of it.
func saveSlots(path string, s slotState, stderr io.Writer) error {
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return put(path, string(data)+"\n", stderr)
}

// sequencer holds one sequence run.
type sequencer struct {
	ps       []planet
	byName   map[string]int
	seq      []int // indexes into ps, in ring order
	inSeq    map[int]bool
	visiting map[int]bool
}

// sequence is the ring order of the drawn planets, as indexes into ps. A planet
// keeps its persisted place unless it is new or its class changed (a remote
// planet ignores class); those are inserted afresh, a stacked planet right after
// its base, a local one before the first planet of a later class, a
// remote one at the end of its host's run (a new host's run goes last). Local
// planets come first and each host's planets are contiguous. Workers that were
// drawn are never displaced by ones that do not fit.
func sequence(prev slotState, ps []planet) []int {
	a := &sequencer{ps: ps, byName: map[string]int{}, inSeq: map[int]bool{}, visiting: map[int]bool{}}
	for i, p := range ps {
		a.byName[p.branch] = i
	}
	prior := map[int]bool{}
	for _, e := range prev.Order {
		i, ok := a.byName[e.Branch]
		if !ok || prior[i] {
			continue
		}
		prior[i] = true
		if e.Host != ps[i].host || (ps[i].host == "" && e.Class != ps[i].class) {
			continue
		}
		a.seq = append(a.seq, i)
		a.inSeq[i] = true
	}

	// A planet stacked on one that has to move moves with it.
	for moved := true; moved; {
		moved = false
		for _, i := range a.seq {
			if j, ok := a.baseOf(i); ok && !a.inSeq[j] && (prior[j] || len(a.seq) < maxPlanets) {
				delete(a.inSeq, i)
				moved = true
			}
		}
		a.seq = slices.DeleteFunc(a.seq, func(i int) bool { return !a.inSeq[i] })
	}
	a.normalize()

	pending := make([]int, 0, len(ps))
	for i := range ps {
		if !a.inSeq[i] {
			pending = append(pending, i)
		}
	}
	sort.SliceStable(pending, func(x, y int) bool {
		px, py := prior[pending[x]], prior[pending[y]]
		if px != py {
			return px
		}
		return ps[pending[x]].branch < ps[pending[y]].branch
	})
	for _, i := range pending {
		a.insert(i)
	}
	return a.seq
}

// baseOf is the planet i is stacked on, when it is drawn on the same host.
func (a *sequencer) baseOf(i int) (int, bool) {
	j, ok := a.byName[a.ps[i].base]
	return j, ok && j != i && a.ps[j].host == a.ps[i].host
}

// normalize puts local planets first and each host's planets together, hosts in
// the order they first appear.
func (a *sequencer) normalize() {
	var hosts []string
	for _, i := range a.seq {
		if h := a.ps[i].host; h != "" && !slices.Contains(hosts, h) {
			hosts = append(hosts, h)
		}
	}
	rank := func(i int) int {
		if a.ps[i].host == "" {
			return 0
		}
		return 1 + slices.Index(hosts, a.ps[i].host)
	}
	sort.SliceStable(a.seq, func(x, y int) bool { return rank(a.seq[x]) < rank(a.seq[y]) })
}

// insert places planet i, its base first so the child can follow it.
func (a *sequencer) insert(i int) {
	if a.inSeq[i] || a.visiting[i] {
		return
	}
	a.visiting[i] = true
	if j, ok := a.baseOf(i); ok {
		a.insert(j)
	}
	if len(a.seq) >= maxPlanets {
		return
	}
	a.seq = slices.Insert(a.seq, a.position(i), i)
	a.inSeq[i] = true
}

func (a *sequencer) position(i int) int {
	p := a.ps[i]
	if j, ok := a.baseOf(i); ok && a.inSeq[j] {
		return a.afterBlock(slices.Index(a.seq, j))
	}
	if p.host != "" {
		last := -1
		for k, x := range a.seq {
			if a.ps[x].host == p.host {
				last = k
			}
		}
		if last < 0 {
			return len(a.seq)
		}
		return a.afterBlock(last)
	}
	// Stacked planets sit out of class order, so only top-level ones anchor the
	// scan: the new planet goes before the first one of a later class.
	for k, x := range a.seq {
		if a.ps[x].host != "" {
			return k
		}
		if _, stacked := a.baseOf(x); !stacked && classRank[a.ps[x].class] > classRank[p.class] {
			return k
		}
	}
	return len(a.seq)
}

// afterBlock is the index after seq[at] and the planets stacked on it, so a new
// planet never lands inside a stack.
func (a *sequencer) afterBlock(at int) int {
	stack := map[int]bool{a.seq[at]: true}
	pos := at + 1
	for pos < len(a.seq) {
		if j, ok := a.baseOf(a.seq[pos]); !ok || !stack[j] {
			break
		}
		stack[a.seq[pos]] = true
		pos++
	}
	return pos
}

// placed is a planet with its ring, angle (degrees clockwise from 12 o'clock)
// and position in points.
type placed struct {
	planet
	ring  int
	angle float64
	x, y  float64
}

// hostLabel is where a remote host's name goes.
type hostLabel struct {
	host string
	x, y float64
}

// scene is the whole layout: planets in ring order, a label per drawn remote
// host in first-seen order, and extent, the radius of the smallest circle around
// everything (the emitter fits the canvas to it).
type scene struct {
	planets []placed
	hosts   []hostLabel
	extent  float64
}

func polar(r, deg float64) (x, y float64) {
	rad := deg * math.Pi / 180
	return r * math.Sin(rad), r * math.Cos(rad)
}

// ringSizes splits n planets over as few rings as fit, as evenly as possible.
func ringSizes(n int) []int {
	rings := (n + ringCap - 1) / ringCap
	sizes := make([]int, rings)
	for k := range sizes {
		sizes[k] = n / rings
		if k < n%rings {
			sizes[k]++
		}
	}
	return sizes
}

// arrange orders the planets, spaces them evenly and turns that into
// coordinates. Every second ring is turned half a step, so its spokes do not run
// under an inner planet.
func arrange(prev slotState, ps []planet) (slotState, scene) {
	seq := sequence(prev, ps)
	next := newSlotState()
	sc := scene{extent: minExtent}

	type run struct{ x, y, first float64 }
	runs := map[string]*run{}
	var hostOrder []string
	outer, k := 0.0, 0
	for ring, n := range ringSizes(len(seq)) {
		r := max(minRadius, float64(n)*arcPerPlanet/(2*math.Pi))
		if ring > 0 {
			r = max(r, outer+ringStep)
		}
		outer = r
		turn := 0.0
		if ring%2 == 1 {
			turn = 180 / float64(n)
		}
		for j := range n {
			i := seq[k]
			k++
			angle := math.Mod(float64(j)*360/float64(n)+turn, 360)
			pl := placed{planet: ps[i], ring: ring, angle: angle}
			pl.x, pl.y = polar(r, angle)
			pr := pl.size * 72 / 2
			sc.extent = max(sc.extent, r+pr)

			pl.moons = make([]moon, len(ps[i].moons))
			for m, mn := range ps[i].moons {
				along := (float64(m) - float64(len(pl.moons)-1)/2) * moonStep
				ux, uy := polar(1, angle)
				mn.x = pl.x + ux*(pr+moonGap) + uy*along
				mn.y = pl.y + uy*(pr+moonGap) - ux*along
				pl.moons[m] = mn
				sc.extent = max(sc.extent, math.Hypot(mn.x, mn.y)+moonRadius)
			}
			sc.planets = append(sc.planets, pl)
			next.Order = append(next.Order, slotEntry{Branch: ps[i].branch, Class: ps[i].class, Host: ps[i].host})

			if h := ps[i].host; h != "" {
				u, v := polar(1, angle)
				if runs[h] == nil {
					runs[h] = &run{first: angle}
					hostOrder = append(hostOrder, h)
				}
				runs[h].x += u
				runs[h].y += v
			}
		}
	}

	for _, h := range hostOrder {
		deg := runs[h].first
		if math.Hypot(runs[h].x, runs[h].y) > 1e-9 {
			deg = math.Atan2(runs[h].x, runs[h].y) * 180 / math.Pi
		}
		r := outer + hostLabelGap
		x, y := polar(r, deg)
		sc.hosts = append(sc.hosts, hostLabel{host: h, x: x, y: y})
		sc.extent = max(sc.extent, r+hostLabelHalf)
	}
	return next, sc
}
