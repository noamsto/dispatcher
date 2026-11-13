package rosterrender

import (
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

func TestBoardDrawsRoundsOnlyWhereThereAreAny(t *testing.T) {
	a, b := crewWorker("feat/1-a", "lime", "working", 2), crewWorker("feat/2-b", "sky", "working", 1)
	b.rounds = 3
	svg, _, _ := boardFixture{ps: []planet{a, b}}.render(defaultAspect)
	if _, card := cardWith(t, svg, "lime · #1"); regexp.MustCompile(`\| r\d+\z`).MatchString(card.text()) {
		t.Errorf("a worker with no rounds drew a chip: %s", card.text())
	}
	if _, card := cardWith(t, svg, "sky · #2"); !strings.HasSuffix(card.text(), " | r3") {
		t.Errorf("the round chip is missing: %s", card.text())
	}
}

func TestBoardHoldCardsNameTheirWindow(t *testing.T) {
	holds := []queuedHold{{id: "h0", label: "100", engine: "claude", window: "7d"}, {id: "f", label: "fleet"}}
	svg, _, _ := boardFixture{ps: []planet{worker("feat/1-a", "working")}, holds: holds}.render(defaultAspect)
	if _, card := cardWith(t, svg, "#100"); !strings.Contains(card.text(), "claude · 7d") {
		t.Errorf("the quota hold lacks its window: %s", card.text())
	}
	if _, card := cardWith(t, svg, "fleet"); !strings.Contains(card.text(), "fleet queue") {
		t.Errorf("the fleet hold: %s", card.text())
	}
	if !strings.Contains(svg, ">QUEUED 2<") {
		t.Errorf("no queued column for the holds")
	}
	if svg, _, _ := (boardFixture{ps: []planet{worker("feat/1-a", "working")}}).render(defaultAspect); strings.Contains(svg, "QUEUED") {
		t.Errorf("a queued column without holds")
	}
}

func TestBudgetStrip(t *testing.T) {
	budget, pct, zero := 6, 23.4, 0.0
	known := legend{weight: 4, budget: &budget, engines: []engineReading{
		{name: "claude", window: "7d", pct: &pct}, {name: "codex"}, {name: "pi", window: "5h", pct: &zero},
	}}
	if got, want := stripText(known), "crew weight 4 · fleet budget 6 · claude 7d 23% · codex ? · pi 5h 0%"; got != want {
		t.Errorf("strip = %q, want %q", got, want)
	}
	unknown := stripText(legend{engines: []engineReading{{name: "claude"}}})
	if want := "crew weight 0 · fleet budget ? · claude ?"; unknown != want {
		t.Errorf("strip = %q, want %q", unknown, want)
	}
	off := 0
	if got := stripText(legend{budget: &off}); !strings.Contains(got, "fleet budget off") {
		t.Errorf("a zero budget is checks disabled, got %q", got)
	}
	if got := stripText(legend{engines: []engineReading{{name: `<b>x"`, window: "7d", pct: &pct}}}); strings.ContainsAny(got, `<>"`) {
		t.Errorf("an engine name reached the strip as markup: %q", got)
	}
}

func queuedModel(t *testing.T, queued string) map[string]any {
	t.Helper()
	return decodeOne(t, `{"rows":[],"holds":[],"roles":[],"queued":`+queued+`}`)
}

func TestBuildQueued(t *testing.T) {
	m := queuedModel(t, `[
		{"id":"a","task":{"ref":null,"branch":"feat/12-x","title":"Secret Title"},"after":["feat/12-x"," #7 ",""]},
		{"id":"b","task":{"ref":"#40","branch":null,"title":"t"},"after":[]},
		{"id":"c","task":{"ref":null,"branch":null,"title":"t"},"after":[]},
		{"id":"d","task":{"ref":"a\"];x","branch":null,"title":"t"},"after":[]}]`)
	got := buildQueued(m)
	if len(got) != 4 {
		t.Fatalf("got %+v", got)
	}
	for i, want := range []string{"12", "40", "queued", "ax"} {
		if got[i].label != want {
			t.Errorf("task %d label %q, want %q", i, got[i].label, want)
		}
	}
	if want := []string{"feat/12-x", "#7"}; !reflect.DeepEqual(got[0].after, want) {
		t.Errorf("after = %q, want %q", got[0].after, want)
	}
	if strings.Contains(fmt.Sprint(got), "Secret Title") {
		t.Errorf("a title reached the result: %+v", got)
	}
	_, lg := buildPlanets(m, modelNow)
	if lg.queued != 4 {
		t.Errorf("lg.queued = %d, want 4", lg.queued)
	}
}

func TestQueuedHostileTextStaysOut(t *testing.T) {
	m := queuedModel(t, `[{"id":"a","task":{"ref":"a\"];evil","branch":null,"title":"t"},"after":["x\"]; evil -> y [","<b>"]}]`)
	svg, _, _ := emit(m)
	if strings.Contains(svg, "<b>") || strings.Contains(svg, "evil -> y") || strings.Contains(svg, "x\"]") || strings.Contains(svg, "x&quot;]") {
		t.Errorf("hostile text reached the svg:\n%s", svg)
	}
	if _, card := cardWith(t, svg, "aevil"); !strings.Contains(card.text(), "filed") {
		t.Errorf("the cleaned task card: %s", card.text())
	}
}

// TestQueuedAfterNamesOnlyMatchingWorkers: an after target names a drawn worker
// of the task's own repo by its branch, issue, PR number or Linear id, once per
// worker, and never by a number buried in the branch.
func TestQueuedAfterNamesOnlyMatchingWorkers(t *testing.T) {
	mk := func(branch, repo, pr string) planet {
		p := worker(branch, "working")
		p.repo, p.prURL = repo, pr
		return p
	}
	after := func(ps []planet, targets ...string) string {
		b, _ := buildBoard(ps, nil, []queuedTask{{id: "t", label: "9", repo: "r1", after: targets}}, newSlotState(), legend{})
		for _, c := range allCards(b) {
			if c.kind == kindTask {
				return c.on
			}
		}
		t.Fatal("no task card")
		return ""
	}
	r1 := mk("feat/12-x", "r1", "https://github.com/o/r/pull/31")
	for _, target := range []string{"feat/12-x", "#12", "12", "31", "#31"} {
		if got := after([]planet{r1}, target); got != "after #31" {
			t.Errorf("target %q: %q", target, got)
		}
	}
	if got := after([]planet{r1}, "12", "feat/12-x", "31"); got != "after #31" {
		t.Errorf("one worker named %q", got)
	}
	if got := after([]planet{r1}, "99", "feat/none"); got != "" {
		t.Errorf("an absent target: %q", got)
	}
	linear := mk("eng-12-fix-it", "r1", "")
	for _, target := range []string{"ENG-12", "eng-12", "eng-12-fix-it"} {
		if got := after([]planet{linear}, target); got != "after eng-12-fix-it" {
			t.Errorf("linear target %q: %q", target, got)
		}
	}
	if got := after([]planet{linear}, "ENG-1", "ENG-120"); got != "" {
		t.Errorf("a longer or shorter linear id matched: %q", got)
	}
	if got := after([]planet{mk("feat/grok-4-7-x", "r1", "")}, "#4"); got != "" {
		t.Errorf("#4 matched a grok-4-7 worker: %q", got)
	}
	if got := after([]planet{mk("feat/12-x", "r2", "")}, "12", "feat/12-x"); got != "" {
		t.Errorf("a worker of another repo matched: %q", got)
	}
}

func TestLegendAddSumsQueued(t *testing.T) {
	l := legend{queued: 1}
	l.add(legend{queued: 2})
	if l.queued != 3 {
		t.Errorf("queued = %d, want 3", l.queued)
	}
}
