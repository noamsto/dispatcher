package rosterrender

import (
	"cmp"
	"slices"
	"strconv"
	"strings"

	"github.com/noamsto/dispatcher/crew/internal/ascii"
)

// classQueued is the board's first column: holds and filed tasks, which have
// no worker yet.
const classQueued = "queued"

// boardClasses is the column order, the lifecycle left to right. The done
// column also holds failed and exited workers.
var boardClasses = [...]string{classQueued, classWorking, classBlocked, classPROpen, classDone}

var columnTitles = map[string]string{
	classQueued:  "QUEUED",
	classWorking: "WORKING",
	classBlocked: "BLOCKED",
	classPROpen:  "PR OPEN",
	classDone:    "DONE",
}

type cardKind int

const (
	kindPlanet cardKind = iota
	kindHold
	kindTask
)

// boardCard is one card: a worker, or a queued hold or task. A hold or task
// sets only name, state and class of the planet, plus repo for a task and
// engine for a quota hold.
type boardCard struct {
	planet
	kind   cardKind
	id     string // a hold's or task's queue id
	window string // the quota window a hold waits on
	// on points at a related card: `on #N` toward a stacked worker's base,
	// `after #N #M` toward the drawn workers a queued task waits on.
	on          string
	flow, pulse bool
	glow        []bool // per moon
}

// column is one state's cards in drawing order; a board never holds an empty one.
type column struct {
	class, title string
	n            int
	cards        []boardCard
}

// header is the column's caption, such as `WORKING 3`.
func (c column) header() string { return c.title + " " + strconv.Itoa(c.n) }

// lane is one repo's row band; label is empty when the crew spans one repo.
type lane struct {
	label, repo string
	cols        []column
}

// board is the whole picture before geometry. folded counts done or failed
// workers no longer drawn, hidden the workers past maxPlanets and more the
// holds and tasks past maxHolds, so the header can say what is not drawn.
type board struct {
	counts legend
	folded int
	hidden int
	more   int
	lanes  []lane
}

func (p planet) key() planetKey { return planetKey{p.repo, p.branch} }

// buildBoard sorts the drawn workers into one lane per repo and one column per
// state. Inside a column cards keep the persisted sequence order, so a redraw
// does not shuffle them and a stacked worker follows its base. Lanes come in
// the order their repos first appear in the persisted sequence, new ones after
// by label. Holds join the first lane's queued column, before the queued tasks;
// a task whose repo has no worker drawn opens its repo's lane.
func buildBoard(ps []planet, holds []queuedHold, queued []queuedTask, prev slotState, lg legend) (board, slotState) {
	groups := map[string][]planet{}
	labels := map[string]string{}
	for _, p := range ps {
		groups[p.repo] = append(groups[p.repo], p)
		if l, ok := labels[p.repo]; !ok || p.repoLabel < l {
			labels[p.repo] = p.repoLabel
		}
	}

	type draft struct {
		label, repo string
		cards       []boardCard
	}
	var drafts []draft
	next := newSlotState()
	room := maxPlanets
	for _, repo := range laneOrder(prev, groups, labels) {
		g := groups[repo]
		seq := sequence(repoSlots(prev, repo), g)
		seq = seq[:min(len(seq), room)]
		if len(seq) == 0 {
			continue
		}
		room -= len(seq)
		d := draft{label: labels[repo], repo: repo}
		for _, i := range seq {
			d.cards = append(d.cards, boardCard{planet: g[i], kind: kindPlanet})
			next.Order = append(next.Order, slotEntry{Branch: g[i].branch, Class: g[i].class, Host: g[i].host, Repo: repo})
		}
		drafts = append(drafts, d)
	}

	b := board{counts: lg, hidden: len(ps) - len(next.Order)}
	done := 0
	for _, p := range ps {
		if p.class == classDone {
			done++
		}
	}
	b.folded = max(lg.done+lg.failed-done, 0)

	var drawn []*boardCard
	for d := range drafts {
		for c := range drafts[d].cards {
			drawn = append(drawn, &drafts[d].cards[c])
		}
	}
	animate(drawn)
	byKey := make(map[planetKey]planet, len(drawn))
	for _, c := range drawn {
		byKey[c.key()] = c.planet
	}
	for _, c := range drawn {
		if base, ok := byKey[planetKey{c.repo, c.base}]; ok && c.base != c.branch {
			c.on = "on " + refLabel(base)
		}
	}

	b.more = max(len(holds)+len(queued)-maxHolds, 0)
	holds = holds[:min(len(holds), maxHolds)]
	queued = queued[:min(len(queued), maxHolds-len(holds))]
	for _, t := range queued {
		at := slices.IndexFunc(drafts, func(d draft) bool { return d.repo == t.repo })
		if at < 0 {
			drafts = append(drafts, draft{label: t.repoLabel, repo: t.repo})
			at = len(drafts) - 1
		}
		card := boardCard{planet: planet{name: t.label, state: classQueued, class: classQueued, repo: t.repo, repoLabel: t.repoLabel}, kind: kindTask, id: t.id}
		if refs := afterRefs(byKey, t); len(refs) > 0 {
			card.on = "after " + strings.Join(refs, " ")
		}
		drafts[at].cards = append(drafts[at].cards, card)
	}
	if len(holds) > 0 {
		if len(drafts) == 0 {
			drafts = append(drafts, draft{})
		}
		cards := make([]boardCard, 0, len(holds)+len(drafts[0].cards))
		for _, h := range holds {
			cards = append(cards, boardCard{planet: planet{name: h.label, state: "hold", class: classQueued, engine: h.engine}, kind: kindHold, id: h.id, window: h.window})
		}
		drafts[0].cards = append(cards, drafts[0].cards...)
	}

	for _, d := range drafts {
		l := lane{label: d.label, repo: d.repo}
		if len(drafts) == 1 {
			l.label = ""
		}
		for _, class := range boardClasses {
			col := column{class: class, title: columnTitles[class]}
			for _, c := range d.cards {
				if c.class == class {
					col.cards = append(col.cards, c)
				}
			}
			if col.n = len(col.cards); col.n > 0 {
				l.cols = append(l.cols, col)
			}
		}
		b.lanes = append(b.lanes, l)
	}
	return b, next
}

// refLabel is how a marker names a drawn worker: its PR number, else its
// branch's issue, else its codename.
func refLabel(p planet) string {
	if n := prNumber(p.prURL); n != "" {
		return "#" + n
	}
	if n := branchIssue(p.branch); n != "" {
		return "#" + n
	}
	return p.name
}

// afterRefs names each distinct drawn worker of the task's own repo that one of
// its after targets matches: a branch, a number that is the anchored issue of
// the worker's branch or the number its PR url ends in, or a Linear id the
// branch is named for. The raw targets are never drawn.
func afterRefs(byKey map[planetKey]planet, t queuedTask) []string {
	var keys []planetKey
	for k := range byKey {
		if k.repo == t.repo {
			keys = append(keys, k)
		}
	}
	slices.SortFunc(keys, func(x, y planetKey) int { return strings.Compare(x.branch, y.branch) })
	var out []string
	for _, k := range keys {
		p := byKey[k]
		pr, issue := prNumber(p.prURL), branchIssue(p.branch)
		for _, a := range t.after {
			n := strings.TrimPrefix(a, "#")
			if a == p.branch || (ascii.IsDigits(n) && (n == issue || n == pr)) || linearTarget(a, p.branch) {
				out = append(out, refLabel(p))
				break
			}
		}
	}
	return out
}

// animate sets what moves, each kind capped at maxAnimated: the flow of working
// cards, the pulse of blocked ones and the glow of working role chips. A stale
// card is not live, so it neither flows nor pulses. Cards rank newest first,
// ties by branch and repo, and the glow cap counts chips, so the last card may
// be taken partly.
func animate(cards []*boardCard) {
	ranked := slices.Clone(cards)
	slices.SortFunc(ranked, func(x, y *boardCard) int {
		return cmp.Or(cmp.Compare(y.ts, x.ts), strings.Compare(x.branch, y.branch), strings.Compare(x.repo, y.repo))
	})
	flow, pulse, glow := 0, 0, 0
	for _, c := range ranked {
		switch {
		case c.stale:
		case c.class == classWorking && flow < maxAnimated:
			c.flow = true
			flow++
		case c.class == classBlocked && pulse < maxAnimated:
			c.pulse = true
			pulse++
		}
		c.glow = make([]bool, len(c.moons))
		for k, m := range c.moons {
			if m.working && glow < maxAnimated {
				c.glow[k] = true
				glow++
			}
		}
	}
}
