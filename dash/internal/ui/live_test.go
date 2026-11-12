package ui

import (
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/noamsto/dispatcher/dash/internal/data"
)

func fixedLiveNow(t *testing.T) func() time.Time {
	t.Helper()
	snap := loadFullSnapshot(t)
	return fixedNow(snap)
}

func newTestLiveView(t *testing.T) liveView {
	t.Helper()
	snap := loadFullSnapshot(t)
	return newLiveView(snap, allLoaded, liveDeps{}, fixedLiveNow(t))
}

func asciiProfile(t *testing.T) {
	t.Helper()
	orig := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.Ascii)
	t.Cleanup(func() { lipgloss.SetColorProfile(orig) })
}

func press(v liveView, k tea.KeyMsg) liveView {
	nv, _ := v.Update(k)
	return nv.(liveView)
}

// headerIndex is the line of the worker table header.
func headerIndex(t *testing.T, lines []string) int {
	t.Helper()
	for i, l := range lines {
		if strings.Contains(l, "state") && strings.Contains(l, "age") {
			return i
		}
	}
	t.Fatalf("no table header in frame:\n%s", strings.Join(lines, "\n"))
	return -1
}

func TestLiveCodenameColourTrueColor(t *testing.T) {
	orig := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(orig)

	v := newTestLiveView(t)
	out := v.View(80, 24)
	found := false
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "sage") && strings.Contains(line, "38;5;137") {
			found = true
		}
	}
	if !found {
		t.Fatalf("codename line for tmux colour137 missing SGR 38;5;137:\n%s", out)
	}
}

func TestLiveRosterErrorAndNoCrew(t *testing.T) {
	asciiProfile(t)
	snap := loadFullSnapshot(t)
	msg := "exit 1"
	snap.Roster.Error = &msg
	out := newLiveView(snap, allLoaded, liveDeps{}, fixedLiveNow(t)).View(80, 24)
	if !strings.Contains(out, "roster unavailable: exit 1") {
		t.Fatalf("roster error should read roster unavailable: exit 1:\n%s", out)
	}

	snap = loadFullSnapshot(t)
	snap.Roster.Crews = nil
	out = newLiveView(snap, allLoaded, liveDeps{}, fixedLiveNow(t)).View(80, 24)
	if !strings.Contains(out, "no active crew") {
		t.Fatalf("empty roster should read \"no active crew\":\n%s", out)
	}
}

func TestLiveRosterPlaceholderBeforeLoad(t *testing.T) {
	asciiProfile(t)
	snap := loadFullSnapshot(t)
	out := newLiveView(snap, [numSources]bool{}, liveDeps{}, fixedLiveNow(t)).View(80, 24)
	for _, want := range []string{"loading budget…", "loading roster…"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing placeholder %q:\n%s", want, out)
		}
	}
	assertWidth(t, out, 80, 24)
}

// TestLiveCrewErrorsShown checks that per-crew roster/hold failures surface
// as their own lines instead of an empty list indistinguishable from an
// idle crew.
func TestLiveCrewErrorsShown(t *testing.T) {
	asciiProfile(t)
	snap := loadCrewErrSnapshot(t)
	out := newLiveView(snap, allLoaded, liveDeps{}, fixedNow(snap)).View(80, 24)
	for _, want := range []string{"crew c2: workers unavailable: boom", "crew c3: holds unavailable: nope"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	assertWidth(t, out, 80, 24)
}

// TestLiveHoldsErrorShownWorkersStillRender checks that a holds-only failure
// still renders the crew's workers, alongside its error line.
func TestLiveHoldsErrorShownWorkersStillRender(t *testing.T) {
	asciiProfile(t)
	snap := loadFullSnapshot(t)
	msg := "hold boom"
	snap.Roster.Crews[0].HoldsError = &msg
	out := newLiveView(snap, allLoaded, liveDeps{}, fixedLiveNow(t)).View(80, 24)
	if !strings.Contains(out, "sage") || !strings.Contains(out, "coral") {
		t.Fatalf("holds-only failure should still render the crew's workers:\n%s", out)
	}
	if !strings.Contains(out, "crew c1: holds unavailable: hold boom") {
		t.Fatalf("holds-only failure should read holds unavailable: hold boom:\n%s", out)
	}
}

// TestLiveRowsSkipWorkersErrorCrew checks that the cursor list contains only
// workers that are rendered. A crew with WorkersError set — even one
// carrying a (malformed/legacy) non-empty Workers slice — must contribute
// nothing to v.rows, so "enter" can never target a hidden row.
func TestLiveRowsSkipWorkersErrorCrew(t *testing.T) {
	snap := loadFullSnapshot(t)
	msg := "boom"
	snap.Roster.Crews[0].WorkersError = &msg // c1 keeps its 2 workers (malformed input)
	healthy := data.RosterCrew{
		ID: "healthy",
		Workers: []map[string]any{
			{"name": "h1", "branch": "feat/healthy-1", "ts": float64(snap.Now * 1000)},
		},
	}
	snap.Roster.Crews = append(snap.Roster.Crews, healthy)

	v := newLiveView(snap, allLoaded, liveDeps{eventsPath: "testdata/events.jsonl"}, fixedLiveNow(t))
	if len(v.rows) != 1 {
		t.Fatalf("rows should count only the healthy crew's workers, got %d", len(v.rows))
	}

	v = press(v, keyRune('G'))
	if v.rows[v.cursor].crewID != "healthy" {
		t.Fatalf("cursor after G should target the healthy crew's worker, got crewID=%q", v.rows[v.cursor].crewID)
	}

	nv, cmd := v.Update(keyType(tea.KeyEnter))
	v = nv.(liveView)
	if !v.detail {
		t.Fatalf("enter on the healthy crew's worker should open the detail view")
	}
	if cmd == nil {
		t.Fatalf("enter on the healthy crew's worker should return a fetch cmd")
	}
}

// TestLiveStateInjectionIsCleaned checks that a worker's raw "state" field
// does not carry a terminal escape or bidi override from untrusted bus data
// into the rendered table. The Ascii color profile makes the rendered frame
// itself style-free, so any surviving ESC/U+202E must have come from the
// (uncleaned) data.
func TestLiveStateInjectionIsCleaned(t *testing.T) {
	asciiProfile(t)
	snap := loadFullSnapshot(t)
	snap.Roster.Crews[0].Workers[0]["state"] = "\x1b]0;pwned\x07working\u202e"
	out := newLiveView(snap, allLoaded, liveDeps{}, fixedLiveNow(t)).View(80, 24)
	if strings.ContainsRune(out, 0x1b) {
		t.Errorf("rendered table contains a raw ESC from an injected state:\n%q", out)
	}
	if strings.ContainsRune(out, 0x202e) {
		t.Errorf("rendered table contains U+202E from an injected state:\n%q", out)
	}
}

// TestLivePRURLInjectionIsCleaned checks the same for the pr column and the
// selected-worker pane (which shows the full URL).
func TestLivePRURLInjectionIsCleaned(t *testing.T) {
	asciiProfile(t)
	snap := loadFullSnapshot(t)
	snap.Roster.Crews[0].Workers[0]["pr_url"] = "\x1b[2Jhttps://pr\u202e"
	v := newLiveView(snap, allLoaded, liveDeps{}, fixedLiveNow(t))
	for _, sel := range []liveView{v, press(v, keyRune('j'))} {
		out := sel.View(80, 24)
		if strings.ContainsRune(out, 0x1b) {
			t.Errorf("rendered table contains a raw ESC from an injected pr_url:\n%q", out)
		}
		if strings.ContainsRune(out, 0x202e) {
			t.Errorf("rendered table contains U+202E from an injected pr_url:\n%q", out)
		}
	}
}

// TestLiveEventKindInjectionIsCleaned checks eventLine, which concatenates
// an event's "kind" raw.
func TestLiveEventKindInjectionIsCleaned(t *testing.T) {
	asciiProfile(t)
	deps := liveDeps{eventsPath: "testdata/events.jsonl"}
	v := newLiveView(loadFullSnapshot(t), allLoaded, deps, fixedLiveNow(t))
	v = press(v, keyRune('j')) // sage: the worker with recorded events
	nv, cmd := v.Update(keyType(tea.KeyEnter))
	v = nv.(liveView)
	msg := cmd().(recentEventsMsg)
	if len(msg.events) == 0 {
		t.Fatalf("fixture has no events for the selected worker")
	}
	for i := range msg.events {
		msg.events[i]["kind"] = "status\x1b[31m\u202e"
	}
	nv, _ = v.Update(msg)
	v = nv.(liveView)
	out := v.View(80, 24)
	if strings.ContainsRune(out, 0x1b) {
		t.Errorf("rendered event detail contains a raw ESC from an injected kind:\n%q", out)
	}
	if strings.ContainsRune(out, 0x202e) {
		t.Errorf("rendered event detail contains U+202E from an injected kind:\n%q", out)
	}
}

// TestLiveDetailEventsWindowFits80x12 checks that the detail pane is
// scrollable and starts at the newest event: RecentEvents is oldest-first,
// so a top-anchored, unscrollable view would clip exactly the events a user
// drilling in wants to see. 20 events at height 12 must fit the frame
// exactly, start scrolled to the newest event (last visible, first not),
// and let `g` scroll back to the oldest.
func TestLiveDetailEventsWindowFits80x12(t *testing.T) {
	v := newTestLiveView(t)
	events := make([]data.Event, 20)
	for i := range events {
		events[i] = data.Event{
			"ts":   float64(1789999000000 + i*1000),
			"kind": "status",
			"body": map[string]any{"state": "evt" + strconv.Itoa(i)},
		}
	}
	nv, _ := v.Update(recentEventsMsg{events: events})
	v = nv.(liveView)
	v.detail = true

	out := v.View(80, 12)
	assertWidth(t, out, 80, 12)
	if !strings.Contains(out, "evt19") {
		t.Errorf("newest event (evt19) should be visible initially:\n%s", out)
	}
	if strings.Contains(out, "evt0") {
		t.Errorf("oldest event (evt0) should not be visible initially:\n%s", out)
	}

	v = press(v, keyRune('g'))
	out = v.View(80, 12)
	if !strings.Contains(out, "evt0") {
		t.Errorf("oldest event (evt0) should be visible after g:\n%s", out)
	}
}

func TestLiveHoldShown(t *testing.T) {
	v := newTestLiveView(t)
	out := v.View(80, 24)
	if !strings.Contains(out, "hold h1") {
		t.Fatalf("expected a hold line for h1:\n%s", out)
	}
}

func TestLiveAttentionLine(t *testing.T) {
	asciiProfile(t)
	out := newTestLiveView(t).View(80, 24)
	if !strings.Contains(out, "1 blocked · 1 hold") {
		t.Fatalf("attention line missing:\n%s", out)
	}

	snap := loadFullSnapshot(t)
	snap.Roster.Crews[0].Workers = snap.Roster.Crews[0].Workers[:1] // sage only: nothing blocked
	snap.Roster.Crews[0].Holds = nil
	out = newLiveView(snap, allLoaded, liveDeps{}, fixedLiveNow(t)).View(80, 24)
	if strings.Contains(out, "blocked ·") || strings.Contains(out, " hold") {
		t.Fatalf("attention line shown with nothing to report:\n%s", out)
	}
}

func TestLiveTickChangesAgeWithoutCollect(t *testing.T) {
	snap := loadFullSnapshot(t)
	cur := fixedLiveNow(t)()
	now := func() time.Time { return cur }
	v := newLiveView(snap, allLoaded, liveDeps{}, now)

	before := v.View(80, 24)
	cur = cur.Add(2 * time.Hour)
	nv, cmd := v.Update(tickMsg{})
	v = nv.(liveView)
	if cmd == nil {
		t.Fatalf("tickMsg did not re-arm the ticker")
	}
	after := v.View(80, 24)
	if before == after {
		t.Fatalf("age text did not change after a tick + advanced clock")
	}
}

func TestLiveDebounceRequestsRoster(t *testing.T) {
	cur := fixedLiveNow(t)()
	now := func() time.Time { return cur }
	v := newLiveView(loadFullSnapshot(t), allLoaded, liveDeps{}, now)

	nv, cmd1 := v.Update(busChangedMsg{})
	v = nv.(liveView)
	t1 := v.lastChange

	cur = cur.Add(10 * time.Millisecond) // a second change arrives shortly after
	nv, cmd2 := v.Update(busChangedMsg{})
	v = nv.(liveView)
	t2 := v.lastChange
	if !t2.After(t1) {
		t.Fatalf("second busChangedMsg did not advance lastChange (t1=%v t2=%v)", t1, t2)
	}

	// deps.watcher is nil here, so each returned Cmd is exactly its
	// debounceCmd (tea.Batch collapses a single non-nil Cmd to itself).
	msg1, ok := cmd1().(debounceFireMsg)
	if !ok {
		t.Fatalf("cmd1() = %T, want debounceFireMsg", cmd1())
	}
	nv, cmdAfter1 := v.Update(msg1)
	v = nv.(liveView)
	if cmdAfter1 != nil {
		t.Fatalf("stale debounce (t1) returned a Cmd, want nil (no request)")
	}

	msg2, ok := cmd2().(debounceFireMsg)
	if !ok {
		t.Fatalf("cmd2() = %T, want debounceFireMsg", cmd2())
	}
	nv, cmdAfter2 := v.Update(msg2)
	v = nv.(liveView)
	if cmdAfter2 == nil {
		t.Fatalf("fresh debounce (t2) did not return a request Cmd")
	}
	if result := cmdAfter2(); result != (requestRosterMsg{}) {
		t.Fatalf("fresh debounce Cmd result = %T, want requestRosterMsg", result)
	}
}

func TestLiveEnterShowsRecentEvents(t *testing.T) {
	deps := liveDeps{eventsPath: "testdata/events.jsonl"}
	v := newLiveView(loadFullSnapshot(t), allLoaded, deps, fixedLiveNow(t))
	v = press(v, keyRune('j')) // sage: the worker with recorded events

	nv, cmd := v.Update(keyType(tea.KeyEnter))
	v = nv.(liveView)
	if !v.detail {
		t.Fatalf("enter did not open the worker detail pane")
	}
	if cmd == nil {
		t.Fatalf("enter did not return a Cmd to load recent events")
	}
	nv, _ = v.Update(cmd())
	v = nv.(liveView)

	out := v.View(80, 24)
	if !strings.Contains(out, "working") {
		t.Fatalf("recent events for the cursor's worker should render its status events:\n%s", out)
	}

	v = press(v, keyType(tea.KeyEsc))
	if v.detail {
		t.Fatalf("esc did not close the detail pane")
	}
}

func TestLiveSelectionHasAllFields(t *testing.T) {
	v := newTestLiveView(t)
	sel, ok := v.Selection().(data.Worker)
	if !ok {
		t.Fatalf("Selection() = %T, want data.Worker", v.Selection())
	}
	want := data.Worker{Crew: "c1", Branch: "fix/123-widget", Session: "s1", Name: "coral", State: "blocked"}
	if sel != want {
		t.Fatalf("Selection() = %+v, want the blocked worker first: %+v", sel, want)
	}

	sel = press(v, keyRune('j')).Selection().(data.Worker)
	want = data.Worker{
		Crew: "c1", Branch: "feat/584-crew-dash", Session: "s1", Name: "sage", State: "working",
		PRURL: "https://github.com/noamsto/dispatcher/pull/999",
	}
	if sel != want {
		t.Fatalf("Selection() after j = %+v, want %+v", sel, want)
	}

	snap := loadCrewErrSnapshot(t)
	host := newLiveView(snap, allLoaded, liveDeps{}, fixedNow(snap)).Selection().(data.Worker)
	if host.Host != "mini" {
		t.Fatalf("Selection().Host = %q, want mini", host.Host)
	}
}

func TestLiveSelectionNilWithoutWorkers(t *testing.T) {
	snap := loadFullSnapshot(t)
	snap.Roster.Crews = nil
	if sel := newLiveView(snap, allLoaded, liveDeps{}, fixedLiveNow(t)).Selection(); sel != nil {
		t.Fatalf("Selection() = %+v, want nil", sel)
	}
}

func TestLiveErrorLinesInjectionIsCleaned(t *testing.T) {
	asciiProfile(t)
	msg := injectedErr
	cases := []struct {
		name string
		want string
		set  func(*data.Snapshot)
	}{
		{"roster", "roster unavailable: boom", func(s *data.Snapshot) { s.Roster.Error = &msg }},
		{"workers", "workers unavailable: boom", func(s *data.Snapshot) { s.Roster.Crews[0].WorkersError = &msg }},
		{"holds", "holds unavailable: boom", func(s *data.Snapshot) { s.Roster.Crews[0].HoldsError = &msg }},
		{"budget", "budget unavailable: boom", func(s *data.Snapshot) { s.Budget.Error = &msg }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			snap := loadFullSnapshot(t)
			c.set(&snap)
			out := newLiveView(snap, allLoaded, liveDeps{}, fixedLiveNow(t)).View(120, 24)
			requireNoControl(t, out)
			if !strings.Contains(out, c.want) {
				t.Errorf("missing %q:\n%q", c.want, out)
			}
		})
	}
}

func TestLiveDetailErrInjectionIsCleaned(t *testing.T) {
	asciiProfile(t)
	v := newTestLiveView(t)
	v.detail = true
	nv, _ := v.Update(recentEventsMsg{err: injectedErr})
	out := nv.(liveView).View(120, 24)
	requireNoControl(t, out)
	if !strings.Contains(out, "unavailable: boom") {
		t.Errorf("missing unavailable: boom:\n%q", out)
	}
}

func TestLiveFieldsInjectionAreCleaned(t *testing.T) {
	asciiProfile(t)
	snap := loadFullSnapshot(t)
	snap.Roster.Crews = []data.RosterCrew{
		{
			ID: "c" + injectedErr,
			Workers: []map[string]any{
				{"name": "w1", "state": "working", "tier": "t" + injectedErr, "engine": "e" + injectedErr, "model": "m" + injectedErr, "age_s": float64(5)},
			},
			Holds: []map[string]any{
				{"id": "h" + injectedErr, "wait": map[string]any{"engine": "e" + injectedErr, "window": "w" + injectedErr}},
			},
		},
		{ID: "other"},
	}
	out := newLiveView(snap, allLoaded, liveDeps{}, fixedLiveNow(t)).View(160, 24)
	requireNoControl(t, out)
	for _, want := range []string{"cboom]0;pwn", "hold hboom]0;pwned[31m: eboom]0;pwned[31m wboom", "tboom]0;pwned[31m/"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%q", want, out)
		}
	}
}

func TestLiveSortsBlockedFirst(t *testing.T) {
	asciiProfile(t)
	snap := loadManySnapshot(t)
	v := newLiveView(snap, allLoaded, liveDeps{}, fixedNow(snap))

	frame := strings.Split(v.View(160, 40), "\n")
	if first := frame[headerIndex(t, frame)+1]; !strings.Contains(first, "blocked") {
		t.Errorf("first worker row is not blocked: %q", first)
	}

	state := func(r liveRow) string { s, _ := strField(r.w, "state"); return s }
	for i := 1; i < len(v.rows); i++ {
		prev, cur := v.rows[i-1], v.rows[i]
		pr, cr := stateRank(state(prev)), stateRank(state(cur))
		if pr > cr {
			t.Fatalf("row %d (%s) sorts after row %d (%s)", i, state(cur), i-1, state(prev))
		}
		if pr == cr && workerAge(prev.w, fixedNow(snap)()) < workerAge(cur.w, fixedNow(snap)()) {
			t.Fatalf("rows %d and %d (%s) are not age-descending", i-1, i, state(cur))
		}
	}
	var order []string
	for _, r := range v.rows {
		if s := state(r); !slices.Contains(order, s) {
			order = append(order, s)
		}
	}
	if want := []string{"blocked", "working", "idle"}; !slices.Equal(order, want) {
		t.Errorf("state order = %v, want %v", order, want)
	}
}

func TestLiveStateRankOrder(t *testing.T) {
	order := []string{"blocked", "failed", "working", "pr_open", "idle", "done", "mystery", "reaped"}
	for i := 1; i < len(order); i++ {
		if stateRank(order[i-1]) >= stateRank(order[i]) {
			t.Errorf("%s (%d) should rank before %s (%d)", order[i-1], stateRank(order[i-1]), order[i], stateRank(order[i]))
		}
	}
}

func liveStates(v liveView) []string {
	var states []string
	for _, r := range v.rows {
		s, _ := strField(r.w, "state")
		states = append(states, s)
	}
	return states
}

func TestLiveReapedHiddenByDefault(t *testing.T) {
	asciiProfile(t)
	snap := loadCrewErrSnapshot(t)
	v := newLiveView(snap, allLoaded, liveDeps{}, fixedNow(snap))

	if want := []string{"blocked", "working", "idle", "done"}; !slices.Equal(liveStates(v), want) {
		t.Fatalf("rows = %v, want %v", liveStates(v), want)
	}
	out := v.View(120, 24)
	if !strings.Contains(out, "1 blocked · 1 hold · 1 reaped hidden") {
		t.Errorf("attention line missing the hidden count:\n%s", out)
	}
	if strings.Contains(out, "slate") {
		t.Errorf("reaped worker rendered while hidden:\n%s", out)
	}

	snap.Roster.Crews[0].Holds = nil
	snap.Roster.Crews[0].Workers = snap.Roster.Crews[0].Workers[2:]
	out = newLiveView(snap, allLoaded, liveDeps{}, fixedNow(snap)).View(120, 24)
	if !strings.Contains(out, "1 reaped hidden") || strings.Contains(out, "blocked") {
		t.Errorf("hidden count should show alone:\n%s", out)
	}
}

func TestLiveToggleShowsReaped(t *testing.T) {
	asciiProfile(t)
	snap := loadCrewErrSnapshot(t)
	v := newLiveView(snap, allLoaded, liveDeps{}, fixedNow(snap))

	helpOf := func(v liveView) string {
		keys := v.Keys()
		return keys[len(keys)-1].Help().Key + " " + keys[len(keys)-1].Help().Desc
	}
	if got := helpOf(v); got != "a show reaped" {
		t.Errorf("help = %q, want %q", got, "a show reaped")
	}
	v = press(v, keyRune('a'))
	if got := helpOf(v); got != "a hide reaped" {
		t.Errorf("help = %q, want %q", got, "a hide reaped")
	}
	if want := []string{"blocked", "working", "idle", "done", "reaped"}; !slices.Equal(liveStates(v), want) {
		t.Fatalf("rows after a = %v, want %v", liveStates(v), want)
	}
	out := v.View(120, 24)
	if !strings.Contains(out, "slate") || strings.Contains(out, "reaped hidden") {
		t.Errorf("reaped worker should show with no hidden count:\n%s", out)
	}

	v = press(v, keyRune('G'))
	if got := v.Selection().(data.Worker); got.State != "reaped" {
		t.Fatalf("G selected %q, want the reaped worker", got.State)
	}
	v = press(v, keyRune('a'))
	if want := []string{"blocked", "working", "idle", "done"}; !slices.Equal(liveStates(v), want) {
		t.Fatalf("rows after second a = %v, want %v", liveStates(v), want)
	}
	if v.cursor != len(v.rows)-1 || v.Selection() == nil {
		t.Errorf("cursor = %d with %d rows, want clamped onto the last visible row", v.cursor, len(v.rows))
	}

	v = press(v, keyRune('k'))
	before := v.Selection().(data.Worker)
	v = press(v, keyRune('a'))
	if after := v.Selection().(data.Worker); after != before {
		t.Errorf("selection after toggling = %+v, want the still-visible %+v", after, before)
	}
}

func TestLiveCursorFollowsWorkerAcrossReload(t *testing.T) {
	snap := loadManySnapshot(t)
	v := newLiveView(snap, allLoaded, liveDeps{}, fixedNow(snap))
	for range 3 {
		v = press(v, keyRune('j'))
	}
	before := v.Selection().(data.Worker)
	oldCursor := v.cursor

	crews := slices.Clone(snap.Roster.Crews)
	for i := range crews {
		crews[i].Workers = slices.Clone(crews[i].Workers)
		for k, w := range crews[i].Workers {
			if crews[i].ID == before.Crew && w["branch"] == before.Branch && w["session"] == before.Session {
				moved := maps.Clone(w)
				moved["state"] = "idle" // sorts to a later position
				crews[i].Workers[k] = moved
			}
		}
	}
	snap.Roster.Crews = crews
	nv, _ := v.Update(snapshotMsg{snap: snap, loaded: allLoaded})
	v = nv.(liveView)

	if v.cursor == oldCursor {
		t.Fatalf("worker did not move in the sort (cursor stayed at %d)", oldCursor)
	}
	if after := v.Selection().(data.Worker); after.Crew != before.Crew || after.Branch != before.Branch || after.Session != before.Session {
		t.Fatalf("selection after reload = %+v, want the same worker as %+v", after, before)
	}
}

func TestLiveCursorClampsWhenWorkerVanishes(t *testing.T) {
	snap := loadManySnapshot(t)
	v := newLiveView(snap, allLoaded, liveDeps{}, fixedNow(snap))
	v = press(v, keyRune('G'))
	snap.Roster.Crews = snap.Roster.Crews[:1]
	nv, _ := v.Update(snapshotMsg{snap: snap, loaded: allLoaded})
	v = nv.(liveView)
	if v.Selection() == nil || v.cursor != len(v.rows)-1 {
		t.Fatalf("cursor = %d with %d rows, want clamped to the last row", v.cursor, len(v.rows))
	}
}

func TestLivePRNumber(t *testing.T) {
	cases := map[string]string{
		"https://github.com/o/r/pull/999":       "#999",
		"https://github.com/o/r/pull/12/files":  "#12",
		"https://example.com/not-a-pr":          "https://example.com/not-a-pr",
		"https://github.com/o/r/issues/5":       "https://github.com/o/r/issues/5",
		"https://github.com/o/r/pull/notnumber": "https://github.com/o/r/pull/notnumber",
	}
	for in, want := range cases {
		if got := prCell(in); got != want {
			t.Errorf("prCell(%q) = %q, want %q", in, got, want)
		}
	}

	now := fixedLiveNow(t)()
	row := liveRow{crewID: "c1", w: map[string]any{"pr_url": "https://github.com/o/r/pull/999"}}
	if got := liveCells(row, now)[colPR]; got != "#999" {
		t.Errorf("pr cell = %q, want #999", got)
	}
	row.w["pr_url"] = nil
	if got := liveCells(row, now)[colPR]; got != "—" {
		t.Errorf("null pr cell = %q, want —", got)
	}

	asciiProfile(t)
	lines := strings.Split(newTestLiveView(t).View(80, 24), "\n")
	sage := lines[headerIndex(t, lines)+2]
	if !strings.Contains(sage, "sage") || !strings.Contains(sage, "#999") || strings.Contains(sage, "https") {
		t.Errorf("sage's row should show #999 and no URL: %q", sage)
	}
}

func TestLiveSelectedPaneShowsFullFields(t *testing.T) {
	asciiProfile(t)
	v := newTestLiveView(t)

	// coral is selected first: blocked, no PR, long detail.
	out := v.View(80, 24)
	for _, want := range []string{"fix/123-widget · s1", "PR —", "waiting on codex budget window to reset before"} {
		if !strings.Contains(out, want) {
			t.Errorf("coral pane missing %q:\n%s", want, out)
		}
	}
	assertWidth(t, out, 80, 24)

	out = press(v, keyRune('j')).View(80, 24)
	for _, want := range []string{"feat/584-crew-dash · s1", "PR https://github.com/noamsto/dispatcher/pull/999", "writing golden tests"} {
		if !strings.Contains(out, want) {
			t.Errorf("sage pane missing %q:\n%s", want, out)
		}
	}
	assertWidth(t, out, 80, 24)

	snap := loadCrewErrSnapshot(t)
	out = newLiveView(snap, allLoaded, liveDeps{}, fixedNow(snap)).View(80, 24)
	if !strings.Contains(out, "fix/123-widget · s1 · host mini") {
		t.Errorf("host missing from the pane:\n%s", out)
	}
}

func TestLiveBudgetStrip(t *testing.T) {
	asciiProfile(t)
	snap := loadSnapshot(t)
	v := newLiveView(snap, allLoaded, liveDeps{}, fixedNow(snap))
	out := v.View(120, 24)
	for _, want := range []string{"claude 7d 90%", "codex 5h 30%", "cursor month 60% LIMIT", "pi ?"} {
		if !strings.Contains(out, want) {
			t.Errorf("budget strip missing %q:\n%s", want, out)
		}
	}
	if strings.Index(out, "claude") > strings.Index(out, "codex") || strings.Index(out, "codex") > strings.Index(out, "cursor") {
		t.Errorf("engines not in name order:\n%s", out)
	}

	if wrapped := v.budgetLines(30); len(wrapped) != 2 || strings.Contains(wrapped[1], "…") {
		t.Fatalf("strip at 30 cells should wrap onto 2 whole lines: %q", wrapped)
	}
	narrow := v.budgetLines(24)
	if len(narrow) != 2 {
		t.Fatalf("narrow strip has %d lines, want 2: %q", len(narrow), narrow)
	}
	for _, l := range narrow {
		assertWidth(t, l, 24, 1)
	}
	if !strings.HasSuffix(narrow[1], "…") {
		t.Errorf("second strip line not ellipsized: %q", narrow[1])
	}

	loading := newLiveView(snap, [numSources]bool{}, liveDeps{}, fixedNow(snap)).View(80, 24)
	if !strings.Contains(loading, "loading budget…") {
		t.Errorf("budget placeholder missing:\n%s", loading)
	}
}

func TestLiveBudgetSegmentColour(t *testing.T) {
	cases := []struct {
		pct   float64
		limit bool
		want  string
	}{
		{10, false, gaugeNormalColor},
		{90, false, gaugeWarningColor},
		{97, false, gaugeDangerColor},
		{60, true, gaugeDangerColor},
	}
	for _, c := range cases {
		e := &data.EngineBudget{Windows: []data.Window{{Key: "5h", UsedPct: c.pct}}}
		if c.limit {
			e.LimitReached = []byte(`{"reason":"hit"}`)
		}
		if got := engineSegment("x", e).color; got != c.want {
			t.Errorf("pct %v limit %v: color %q, want %q", c.pct, c.limit, got, c.want)
		}
	}
}

func TestLiveCrewColumnOnlyWithManyCrews(t *testing.T) {
	asciiProfile(t)
	many := loadManySnapshot(t)
	lines := strings.Split(newLiveView(many, allLoaded, liveDeps{}, fixedNow(many)).View(120, 40), "\n")
	if h := lines[headerIndex(t, lines)]; !strings.Contains(h, "crew") {
		t.Errorf("3 crews: header lacks the crew column: %q", h)
	}
	lines = strings.Split(newTestLiveView(t).View(120, 40), "\n")
	if h := lines[headerIndex(t, lines)]; strings.Contains(h, "crew") {
		t.Errorf("1 crew: header has a crew column: %q", h)
	}
}

func TestLiveDetailColumnOnlyFrom120(t *testing.T) {
	asciiProfile(t)
	v := newTestLiveView(t)
	for w, want := range map[int]bool{119: false, 120: true} {
		lines := strings.Split(v.View(w, 24), "\n")
		if got := strings.Contains(lines[headerIndex(t, lines)], "detail"); got != want {
			t.Errorf("width %d: detail column = %v, want %v", w, got, want)
		}
	}
	out := v.View(160, 24)
	if !strings.Contains(out, "writing golden tests") {
		t.Errorf("detail text missing at 160:\n%s", out)
	}
}

func TestLiveTierColumnFullAt200(t *testing.T) {
	asciiProfile(t)
	snap := loadFullSnapshot(t)
	snap.Roster.Crews = []data.RosterCrew{
		{
			ID: "c1",
			Workers: []map[string]any{
				{"name": "w1", "state": "working", "tier": "standard", "engine": "pi", "model": "openrouter/deepseek/deepseek-v4.1-flash", "age_s": float64(5)},
			},
		},
	}
	out := newLiveView(snap, allLoaded, liveDeps{}, fixedLiveNow(t)).View(200, 24)
	if want := "standard/pi/openrouter/deepseek/deepseek-v4.1-flash"; !strings.Contains(out, want) {
		t.Errorf("tier/engine/model truncated at 200, want %q:\n%s", want, out)
	}
}

// TestLiveWidthTable checks every frame line stays within its width across
// widths and fixtures — on the raw frame, before the root's normalizeFrame
// safety net — and that the column layout itself fits where its minimums
// allow.
func TestLiveWidthTable(t *testing.T) {
	asciiProfile(t)
	crewerr := loadCrewErrSnapshot(t)
	many := loadManySnapshot(t)
	snaps := map[string]data.Snapshot{"many": many, "crewerr": crewerr}
	for name, snap := range snaps {
		for _, w := range []int{40, 60, 80, 100, 120, 160, 200} {
			for _, h := range []int{12, 24, 40} {
				v := newLiveView(snap, allLoaded, liveDeps{}, fixedNow(snap))
				for step := range 3 {
					raw := strings.Split(v.tableView(w, h), "\n")
					if len(raw) > h {
						t.Errorf("%s %dx%d step %d: raw frame has %d lines", name, w, h, step, len(raw))
					}
					for _, l := range raw {
						assertWidth(t, l, w, 1)
					}
					assertWidth(t, v.View(w, h), w, h)
					v = press(v, keyRune('G'))
				}
			}

			now := fixedNow(snap)()
			cells := make([][numCols]string, 0)
			for _, c := range snap.Roster.Crews {
				for _, wk := range c.Workers {
					cells = append(cells, liveCells(liveRow{crewID: c.ID, w: wk}, now))
				}
			}
			cols, widths := layoutColumns(liveColumns(w, len(snap.Roster.Crews) > 1), cells, w)
			if got := tableWidth(widths); got > w {
				t.Errorf("%s width %d: layout is %d cells wide (%d columns)", name, w, got, len(cols))
			}
		}
	}
}

func TestLiveTinyFrames(t *testing.T) {
	asciiProfile(t)
	snap := loadFullSnapshot(t)
	v := newLiveView(snap, allLoaded, liveDeps{}, fixedNow(snap))
	for _, sz := range [][2]int{{20, 5}, {80, 6}, {80, 8}, {1, 1}} {
		assertWidth(t, v.View(sz[0], sz[1]), sz[0], sz[1])
	}
}

func TestLiveWindowKeepsCursorVisible(t *testing.T) {
	asciiProfile(t)
	snap := loadManySnapshot(t)
	v := press(newLiveView(snap, allLoaded, liveDeps{}, fixedNow(snap)), keyRune('G'))
	out := v.View(80, 24)
	assertWidth(t, out, 80, 24)
	if !strings.Contains(out, "›") {
		t.Errorf("cursor marker not visible after G:\n%s", out)
	}
	if !strings.Contains(out, "state") {
		t.Errorf("table header scrolled away:\n%s", out)
	}
}

func TestLiveSelectedPaneDroppedAtTinyHeight(t *testing.T) {
	asciiProfile(t)
	v := newTestLiveView(t)
	if out := v.View(80, 24); !strings.Contains(out, "PR ") {
		t.Fatalf("pane missing at 24 lines:\n%s", out)
	}
	if out := v.View(80, 8); strings.Contains(out, "PR ") {
		t.Fatalf("pane should be dropped at 8 lines:\n%s", out)
	}
}
