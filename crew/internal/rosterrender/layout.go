package rosterrender

import (
	"encoding/json"
	"io"
	"os"
	"slices"
	"sort"
)

const (
	// maxHolds bounds the queued holds and tasks drawn, holds first.
	maxHolds = 16
	slotsVer = 2

	// maxPlanets bounds the picture, so a bad state file or a runaway roster
	// cannot push the board past any readable scale.
	maxPlanets = 64
)

// slotEntry is one worker in the persisted sequence, with the class and host it
// was placed for.
type slotEntry struct {
	Branch string `json:"branch"`
	Class  string `json:"class"`
	Host   string `json:"host,omitempty"`
	Repo   string `json:"repo,omitempty"`
}

// slotState is roster-render.slots.json: the card sequence, which is what makes
// a redraw keep every worker's place relative to the others.
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
	seq      []int // indexes into ps, in sequence order
	inSeq    map[int]bool
	visiting map[int]bool
}

// sequence is the order of the drawn planets, as indexes into ps. A planet
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

// laneOrder is the repos in lane order: those already in the persisted
// sequence first, by their first entry, then the rest by label.
func laneOrder(prev slotState, groups map[string][]planet, labels map[string]string) []string {
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
