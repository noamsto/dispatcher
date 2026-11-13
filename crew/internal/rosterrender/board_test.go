package rosterrender

import (
	"io"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
)

// worker is a drawn planet of class's state, in repo "r".
func worker(branch, state string) planet {
	class, failed := classify(state)
	return planet{branch: branch, name: branch, state: state, class: class, failed: failed, repo: "r", repoLabel: "r"}
}

func inRepo(p planet, repo string) planet {
	p.repo, p.repoLabel = repo, repo+"-label"
	return p
}

func colClasses(l lane) []string {
	var out []string
	for _, c := range l.cols {
		out = append(out, c.class)
	}
	return out
}

func colOf(t *testing.T, l lane, class string) column {
	t.Helper()
	for _, c := range l.cols {
		if c.class == class {
			return c
		}
	}
	t.Fatalf("no %s column in %v", class, colClasses(l))
	return column{}
}

func colBranches(c column) []string {
	var out []string
	for _, k := range c.cards {
		out = append(out, k.branch)
	}
	return out
}

func allCards(b board) []boardCard {
	var out []boardCard
	for _, l := range b.lanes {
		for _, c := range l.cols {
			out = append(out, c.cards...)
		}
	}
	return out
}

func TestBoardColumnsInLifecycleOrderAndEmptyOnesOmitted(t *testing.T) {
	cases := []struct {
		name   string
		ps     []planet
		holds  []queuedHold
		wantCl []string
	}{
		{"one working", []planet{worker("a", "working")}, nil, []string{classWorking}},
		{"no blocked", []planet{worker("d", "done"), worker("p", "pr_open"), worker("w", "working")},
			[]queuedHold{{id: "h", label: "hold"}}, []string{classQueued, classWorking, classPROpen, classDone}},
		{"failed and exited are done", []planet{worker("f", "failed"), worker("e", "exited"), worker("b", "blocked")},
			nil, []string{classBlocked, classDone}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, _ := buildBoard(tc.ps, tc.holds, nil, newSlotState(), legend{})
			if len(b.lanes) != 1 {
				t.Fatalf("lanes = %d, want 1", len(b.lanes))
			}
			if got := colClasses(b.lanes[0]); !slices.Equal(got, tc.wantCl) {
				t.Errorf("columns = %v, want %v", got, tc.wantCl)
			}
		})
	}
}

func TestBoardFailedAndExitedSitInDone(t *testing.T) {
	b, _ := buildBoard([]planet{worker("f", "failed"), worker("e", "exited"), worker("d", "done")}, nil, nil, newSlotState(), legend{})
	done := colOf(t, b.lanes[0], classDone)
	if got := colBranches(done); !slices.Equal(got, []string{"d", "e", "f"}) {
		t.Errorf("done column = %v", got)
	}
	for _, c := range done.cards {
		if c.failed != (c.branch != "d") {
			t.Errorf("%s failed = %v", c.branch, c.failed)
		}
	}
}

func TestBoardColumnHeader(t *testing.T) {
	b, _ := buildBoard([]planet{worker("a", "working"), worker("b", "working"), worker("c", "running"), worker("p", "pr_open")},
		nil, nil, newSlotState(), legend{})
	cases := map[string]string{classWorking: "WORKING 3", classPROpen: "PR OPEN 1"}
	for class, want := range cases {
		if got := colOf(t, b.lanes[0], class).header(); got != want {
			t.Errorf("%s header = %q, want %q", class, got, want)
		}
	}
}

func TestBoardLanePerRepo(t *testing.T) {
	cases := []struct {
		name   string
		ps     []planet
		queued []queuedTask
		want   []string // lane labels
	}{
		{"single repo has no label", []planet{worker("a", "working"), worker("b", "blocked")}, nil, []string{""}},
		{"two repos", []planet{inRepo(worker("a", "working"), "x"), inRepo(worker("b", "working"), "y")}, nil,
			[]string{"x-label", "y-label"}},
		{"queued task opens its repo's lane", []planet{inRepo(worker("a", "working"), "x")},
			[]queuedTask{{id: "q", label: "7", repo: "z", repoLabel: "z-label"}}, []string{"x-label", "z-label"}},
		{"queued task joins its repo's lane", []planet{inRepo(worker("a", "working"), "x")},
			[]queuedTask{{id: "q", label: "7", repo: "x", repoLabel: "x-label"}}, []string{""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, _ := buildBoard(tc.ps, nil, tc.queued, newSlotState(), legend{})
			var got []string
			for _, l := range b.lanes {
				got = append(got, l.label)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("lane labels = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBoardHoldsLeadTheFirstLanesQueuedColumn(t *testing.T) {
	ps := []planet{inRepo(worker("a", "working"), "x"), inRepo(worker("b", "working"), "y")}
	holds := []queuedHold{{id: "h1", label: "11", engine: "claude", window: "5h"}, {id: "h2", label: "hold"}}
	queued := []queuedTask{{id: "t1", label: "21", repo: "x"}, {id: "t2", label: "22", repo: "y"}}
	b, _ := buildBoard(ps, holds, queued, newSlotState(), legend{})
	q := colOf(t, b.lanes[0], classQueued)
	var ids []string
	var kinds []cardKind
	for _, c := range q.cards {
		ids, kinds = append(ids, c.id), append(kinds, c.kind)
	}
	if !slices.Equal(ids, []string{"h1", "h2", "t1"}) || !slices.Equal(kinds, []cardKind{kindHold, kindHold, kindTask}) {
		t.Errorf("first lane queued = %v %v", ids, kinds)
	}
	if q.cards[0].engine != "claude" || q.cards[0].window != "5h" {
		t.Errorf("quota hold lost its window: %+v", q.cards[0])
	}
	if got := colOf(t, b.lanes[1], classQueued).cards; len(got) != 1 || got[0].id != "t2" {
		t.Errorf("second lane queued = %+v", got)
	}
}

func TestBoardHoldsAloneMakeOneLane(t *testing.T) {
	b, _ := buildBoard(nil, []queuedHold{{id: "h", label: "hold"}}, nil, newSlotState(), legend{})
	if len(b.lanes) != 1 || b.lanes[0].label != "" || len(b.lanes[0].cols) != 1 || b.lanes[0].cols[0].class != classQueued {
		t.Errorf("lanes = %+v", b.lanes)
	}
}

func TestBoardQueuedCapCountsTheRest(t *testing.T) {
	var holds []queuedHold
	for i := range maxHolds + 3 {
		holds = append(holds, queuedHold{id: "h" + strconv.Itoa(i), label: "hold"})
	}
	b, _ := buildBoard(nil, holds, []queuedTask{{id: "t", label: "1"}}, newSlotState(), legend{})
	if n := len(colOf(t, b.lanes[0], classQueued).cards); n != maxHolds {
		t.Errorf("queued cards = %d, want %d", n, maxHolds)
	}
	if b.more != 4 {
		t.Errorf("more = %d, want 4", b.more)
	}
}

func TestBoardPlanetCapCountsHidden(t *testing.T) {
	var ps []planet
	for i := range maxPlanets + 5 {
		ps = append(ps, worker("w"+strconv.Itoa(1000+i), "working"))
	}
	b, next := buildBoard(ps, nil, nil, newSlotState(), legend{})
	if n := len(allCards(b)); n != maxPlanets || len(next.Order) != maxPlanets || b.hidden != 5 {
		t.Errorf("cards %d slots %d hidden %d", n, len(next.Order), b.hidden)
	}
}

// redraw runs buildBoard with the slot state persisted through its file, as
// the renderer does between passes.
func redraw(t *testing.T, path string, ps []planet) board {
	t.Helper()
	b, next := buildBoard(ps, nil, nil, loadSlots(path), legend{})
	if err := saveSlots(path, next, io.Discard); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestBoardOrderStableAcrossRedraws(t *testing.T) {
	path := filepath.Join(t.TempDir(), "slots.json")
	seed := slotState{V: slotsVer, Order: []slotEntry{
		{Branch: "w3", Class: classWorking, Repo: "r"},
		{Branch: "w1", Class: classWorking, Repo: "r"},
		{Branch: "w2", Class: classWorking, Repo: "r"},
	}}
	if err := saveSlots(path, seed, io.Discard); err != nil {
		t.Fatal(err)
	}
	first := redraw(t, path, []planet{worker("w1", "working"), worker("w2", "working"), worker("w3", "working")})
	if got := colBranches(colOf(t, first.lanes[0], classWorking)); !slices.Equal(got, []string{"w3", "w1", "w2"}) {
		t.Fatalf("first draw = %v", got)
	}

	// w2 turns blocked and w0 appears, inputs reversed: w3 and w1 keep their order.
	second := redraw(t, path, []planet{worker("w0", "working"), worker("w3", "working"), worker("w2", "blocked"), worker("w1", "working")})
	if got := colBranches(colOf(t, second.lanes[0], classWorking)); !slices.Equal(got, []string{"w3", "w1", "w0"}) {
		t.Errorf("second draw working = %v", got)
	}
	third := redraw(t, path, []planet{worker("w1", "working"), worker("w2", "blocked"), worker("w0", "working"), worker("w3", "working")})
	if got := colBranches(colOf(t, third.lanes[0], classWorking)); !slices.Equal(got, []string{"w3", "w1", "w0"}) {
		t.Errorf("third draw working = %v", got)
	}
}

func TestBoardLaneOrderFollowsTheSlots(t *testing.T) {
	path := filepath.Join(t.TempDir(), "slots.json")
	ps := []planet{inRepo(worker("a", "working"), "x"), inRepo(worker("b", "working"), "y")}
	if err := saveSlots(path, slotState{V: slotsVer, Order: []slotEntry{{Branch: "b", Class: classWorking, Repo: "y"}}}, io.Discard); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		b := redraw(t, path, ps)
		if b.lanes[0].repo != "y" || b.lanes[1].repo != "x" {
			t.Fatalf("lanes = %s, %s", b.lanes[0].repo, b.lanes[1].repo)
		}
	}
}

func TestBoardStackedFollowsBase(t *testing.T) {
	prev := slotState{V: slotsVer, Order: []slotEntry{
		{Branch: "feat/1-base", Class: classWorking, Repo: "r"},
		{Branch: "feat/2-mid", Class: classWorking, Repo: "r"},
	}}
	child := worker("feat/3-child", "working")
	child.base = "feat/1-base"
	b, _ := buildBoard([]planet{worker("feat/2-mid", "working"), child, worker("feat/1-base", "working")}, nil, nil, prev, legend{})
	if got := colBranches(colOf(t, b.lanes[0], classWorking)); !slices.Equal(got, []string{"feat/1-base", "feat/3-child", "feat/2-mid"}) {
		t.Errorf("working = %v", got)
	}
}

func TestBoardOnMarker(t *testing.T) {
	withPR := worker("feat/12-base", "pr_open")
	withPR.prURL = "https://github.com/o/r/pull/77"
	cases := []struct {
		name string
		base planet
		want string
	}{
		{"base PR number", withPR, "on #77"},
		{"base branch issue", worker("feat/12-base", "working"), "on #12"},
		{"base codename", worker("spike", "working"), "on spike"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			child := worker("feat/40-child", "working")
			child.base = tc.base.branch
			b, _ := buildBoard([]planet{tc.base, child}, nil, nil, newSlotState(), legend{})
			for _, c := range allCards(b) {
				want := ""
				if c.branch == child.branch {
					want = tc.want
				}
				if c.on != want {
					t.Errorf("%s on = %q, want %q", c.branch, c.on, want)
				}
			}
		})
	}

	t.Run("base not drawn", func(t *testing.T) {
		child := worker("feat/40-child", "working")
		child.base = "main"
		b, _ := buildBoard([]planet{child}, nil, nil, newSlotState(), legend{})
		if on := allCards(b)[0].on; on != "" {
			t.Errorf("on = %q", on)
		}
	})
}

func TestBoardAfterMarker(t *testing.T) {
	pr := worker("feat/5-pr", "pr_open")
	pr.prURL = "https://github.com/o/r/pull/90"
	ps := []planet{worker("feat/12-x", "working"), pr, worker("abc-7-linear", "working"), inRepo(worker("feat/13-y", "working"), "other")}
	cases := []struct {
		after []string
		want  string
	}{
		{[]string{"#12"}, "after #12"},
		{[]string{"90", "feat/12-x"}, "after #12 #90"},
		{[]string{"ABC-7"}, "after abc-7-linear"},
		{[]string{"13"}, ""}, // another repo's worker never matches
		{[]string{"#999"}, ""},
	}
	for _, tc := range cases {
		b, _ := buildBoard(ps, nil, []queuedTask{{id: "q", label: "50", repo: "r", after: tc.after}}, newSlotState(), legend{})
		for _, c := range allCards(b) {
			if c.kind == kindTask && c.on != tc.want {
				t.Errorf("after %v: on = %q, want %q", tc.after, c.on, tc.want)
			}
		}
	}
}

func TestBoardAnimationCapsNewestFirst(t *testing.T) {
	var ps []planet
	for i := range 12 {
		w := worker("w"+strconv.Itoa(10+i), "working")
		w.ts = float64(i)
		w.moons = []moon{{role: "reviewer", working: true}}
		b := worker("b"+strconv.Itoa(10+i), "blocked")
		b.ts = float64(i)
		ps = append(ps, w, b)
	}
	stale := worker("w99", "working")
	stale.ts, stale.stale = 100, true
	ps = append(ps, stale)

	b, _ := buildBoard(ps, nil, nil, newSlotState(), legend{})
	flow, pulse, glow := 0, 0, 0
	for _, c := range allCards(b) {
		if c.flow {
			flow++
			if c.ts < 4 {
				t.Errorf("%s (ts %v) flows past newer ones", c.branch, c.ts)
			}
		}
		if c.pulse {
			pulse++
		}
		if c.stale && (c.flow || c.pulse) {
			t.Errorf("stale %s animates", c.branch)
		}
		if len(c.glow) != len(c.moons) {
			t.Errorf("%s glow %v for %d moons", c.branch, c.glow, len(c.moons))
		}
		for _, g := range c.glow {
			if g {
				glow++
			}
		}
	}
	if flow != maxAnimated || pulse != maxAnimated || glow != maxAnimated {
		t.Errorf("flow %d pulse %d glow %d, want %d each", flow, pulse, glow, maxAnimated)
	}
}

func TestBoardFoldedCount(t *testing.T) {
	ps := []planet{worker("d1", "done"), worker("f1", "failed"), worker("w", "working")}
	b, _ := buildBoard(ps, nil, nil, newSlotState(), legend{done: 5, failed: 2, working: 1})
	if b.folded != 5 {
		t.Errorf("folded = %d, want 5", b.folded)
	}
}
