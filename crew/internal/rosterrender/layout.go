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

	// With two or more repos each gets a wedge: its outer arc sits wedgeRimGap
	// past the wedge's outermost ring and its name wedgeLabelGap past the arc.
	wedgeRimGap   = 55.0
	wedgeLabelGap = 18.0

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
	Repo   string `json:"repo,omitempty"`
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
	type key struct{ repo, branch string }
	seen := map[key]bool{}
	for _, e := range s.Order {
		k := key{e.Repo, e.Branch}
		if e.Branch == "" || seen[k] || len(out.Order) >= maxPlanets {
			continue
		}
		seen[k] = true
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

// wedge is one repo's sector: start and span in degrees clockwise from 12
// o'clock, rim the radius of its outer arc, and x, y the centre of its label.
type wedge struct {
	repo, label string
	start, span float64
	rim         float64
	x, y        float64
}

// scene is the whole layout: planets in ring order, a label per drawn remote
// host in first-seen order, the repo wedges (none for a single repo), and
// extent, the radius of the smallest circle around everything (the emitter fits
// the canvas to it).
type scene struct {
	planets []placed
	hosts   []hostLabel
	wedges  []wedge
	extent  float64
}

// place puts p on ring at radius r and angle, with its moons pushed outward, and
// grows the extent over both.
func (sc *scene) place(p planet, ring int, r, angle float64) {
	pl := placed{planet: p, ring: ring, angle: angle}
	pl.x, pl.y = polar(r, angle)
	pr := pl.size * 72 / 2
	sc.extent = max(sc.extent, r+pr)

	pl.moons = make([]moon, len(p.moons))
	for m, mn := range p.moons {
		along := (float64(m) - float64(len(pl.moons)-1)/2) * moonStep
		ux, uy := polar(1, angle)
		mn.x = pl.x + ux*(pr+moonGap) + uy*along
		mn.y = pl.y + uy*(pr+moonGap) - ux*along
		pl.moons[m] = mn
		sc.extent = max(sc.extent, math.Hypot(mn.x, mn.y)+moonRadius)
	}
	sc.planets = append(sc.planets, pl)
}

// hostRun sums the unit vectors of one host's planets, so its label can sit in
// their mean direction.
type hostRun struct{ x, y, first float64 }

func (h *hostRun) add(angle float64) {
	u, v := polar(1, angle)
	h.x += u
	h.y += v
}

// direction is the mean angle of the run, or its first planet's when they
// cancel out.
func (h *hostRun) direction() float64 {
	if math.Hypot(h.x, h.y) > 1e-9 {
		return math.Atan2(h.x, h.y) * 180 / math.Pi
	}
	return h.first
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

// arrange lays the planets out: on shared rings, or in one wedge per repo when
// they span two or more.
func arrange(prev slotState, ps []planet) (slotState, scene) {
	repos := map[string]bool{}
	for _, p := range ps {
		repos[p.repo] = true
	}
	if len(repos) < 2 {
		return arrangeRings(prev, ps)
	}
	return arrangeWedges(prev, ps)
}

// arrangeRings orders the planets, spaces them evenly and turns that into
// coordinates. Every second ring is turned half a step, so its spokes do not run
// under an inner planet.
func arrangeRings(prev slotState, ps []planet) (slotState, scene) {
	seq := sequence(prev, ps)
	next := newSlotState()
	sc := scene{extent: minExtent}

	runs := map[string]*hostRun{}
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
			sc.place(ps[i], ring, r, angle)
			next.Order = append(next.Order, slotEntry{Branch: ps[i].branch, Class: ps[i].class, Host: ps[i].host, Repo: ps[i].repo})

			if h := ps[i].host; h != "" {
				if runs[h] == nil {
					runs[h] = &hostRun{first: angle}
					hostOrder = append(hostOrder, h)
				}
				runs[h].add(angle)
			}
		}
	}

	for _, h := range hostOrder {
		r := outer + hostLabelGap
		x, y := polar(r, runs[h].direction())
		sc.hosts = append(sc.hosts, hostLabel{host: h, x: x, y: y})
		sc.extent = max(sc.extent, r+hostLabelHalf)
	}
	return next, sc
}

// wedgeSector is one repo's planets with their sequence.
type wedgeSector struct {
	repo, label string
	ps          []planet
	seq         []int
}

// arrangeWedges gives each repo an angular sector, clockwise from 12 o'clock in
// the order the repos first appear in the persisted sequence (new ones after,
// by label), and lays its planets out inside it. A sector's span follows its
// planet count, with a floor of two so a lone worker's wedge still has room.
// Planets fill the shared ring radii from the inside out, each ring spread
// evenly across the sector's arc.
//
// maxPlanets bounds the total: repos are filled in wedge order, so a new
// planet in an earlier repo can leave a later repo's last planets undrawn.
func arrangeWedges(prev slotState, ps []planet) (slotState, scene) {
	groups := map[string][]planet{}
	labels := map[string]string{}
	for _, p := range ps {
		groups[p.repo] = append(groups[p.repo], p)
		if l, ok := labels[p.repo]; !ok || p.repoLabel < l {
			labels[p.repo] = p.repoLabel
		}
	}

	var sectors []wedgeSector
	weights, room := 0, maxPlanets
	for _, repo := range wedgeOrder(prev, groups, labels) {
		g := groups[repo]
		seq := sequence(repoSlots(prev, repo), g)
		seq = seq[:min(len(seq), room)]
		if len(seq) == 0 {
			continue
		}
		room -= len(seq)
		weights += max(len(seq), 2)
		sectors = append(sectors, wedgeSector{repo: repo, label: labels[repo], ps: g, seq: seq})
	}

	next := newSlotState()
	sc := scene{extent: minExtent}
	start := 0.0
	for _, s := range sectors {
		span := 360 * float64(max(len(s.seq), 2)) / float64(weights)
		w := wedge{repo: s.repo, label: s.label, start: start, span: span}
		hosts := map[string]*hostRun{}
		var hostOrder []string

		outer, k := 0.0, 0
		for ring := 0; k < len(s.seq); ring++ {
			r := minRadius + float64(ring)*ringStep
			fit := max(1, int(r*span*math.Pi/180/arcPerPlanet))
			m := min(fit, len(s.seq)-k)
			outer = r
			for j := range m {
				i := s.seq[k]
				k++
				angle := start + span*(float64(j)+0.5)/float64(m)
				sc.place(s.ps[i], ring, r, angle)
				next.Order = append(next.Order, slotEntry{Branch: s.ps[i].branch, Class: s.ps[i].class, Host: s.ps[i].host, Repo: s.repo})

				if h := s.ps[i].host; h != "" {
					if hosts[h] == nil {
						hosts[h] = &hostRun{first: angle}
						hostOrder = append(hostOrder, h)
					}
					hosts[h].add(angle)
				}
			}
		}

		w.rim = outer + wedgeRimGap
		w.x, w.y = polar(w.rim+wedgeLabelGap, start+span/2)
		sc.extent = max(sc.extent, w.rim+wedgeLabelGap+hostLabelHalf)
		sc.wedges = append(sc.wedges, w)

		hostR := w.rim + wedgeLabelGap + 2*hostLabelHalf + 6
		for _, h := range hostOrder {
			x, y := polar(hostR, hosts[h].direction())
			sc.hosts = append(sc.hosts, hostLabel{host: h, x: x, y: y})
			sc.extent = max(sc.extent, hostR+hostLabelHalf)
		}
		start += span
	}
	return next, sc
}

// wedgeOrder is the repos in wedge order: those already in the persisted
// sequence first, by their first entry, then the rest by label.
func wedgeOrder(prev slotState, groups map[string][]planet, labels map[string]string) []string {
	var repos []string
	for _, e := range prev.Order {
		if _, ok := groups[e.Repo]; ok && e.Repo != "" && !slices.Contains(repos, e.Repo) {
			repos = append(repos, e.Repo)
		}
	}
	var fresh []string
	for repo := range groups {
		if !slices.Contains(repos, repo) {
			fresh = append(fresh, repo)
		}
	}
	sort.Slice(fresh, func(x, y int) bool {
		if labels[fresh[x]] != labels[fresh[y]] {
			return labels[fresh[x]] < labels[fresh[y]]
		}
		return fresh[x] < fresh[y]
	})
	return append(repos, fresh...)
}

// repoSlots is the part of the persisted sequence that belongs to repo.
func repoSlots(prev slotState, repo string) slotState {
	out := newSlotState()
	for _, e := range prev.Order {
		if e.Repo == repo {
			out.Order = append(out.Order, e)
		}
	}
	return out
}
