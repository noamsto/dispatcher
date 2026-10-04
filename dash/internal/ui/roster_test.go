package ui

import (
	"strconv"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"

	"github.com/noamsto/dispatcher/dash/internal/data"
)

func fixedRosterNow(t *testing.T) func() time.Time {
	t.Helper()
	snap := loadFullSnapshot(t)
	return fixedNow(snap)
}

func newTestRosterView(t *testing.T) rosterView {
	t.Helper()
	snap := loadFullSnapshot(t)
	return newRosterView(snap, rosterDeps{}, fixedRosterNow(t))
}

func TestRosterCodenameColourTrueColor(t *testing.T) {
	orig := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(orig)

	v := newTestRosterView(t)
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

func TestRosterNoActiveCrew(t *testing.T) {
	snap := loadFullSnapshot(t)
	snap.Roster.Crews = nil
	v := newRosterView(snap, rosterDeps{}, fixedRosterNow(t))
	out := v.View(80, 24)
	if !strings.Contains(out, "no active crew") {
		t.Fatalf("empty roster should read \"no active crew\":\n%s", out)
	}
}

func TestRosterSourceError(t *testing.T) {
	snap := loadFullSnapshot(t)
	msg := "exit 1"
	snap.Roster.Error = &msg
	v := newRosterView(snap, rosterDeps{}, fixedRosterNow(t))
	out := v.View(80, 24)
	if !strings.Contains(out, "unavailable: exit 1") {
		t.Fatalf("roster error should read unavailable: exit 1:\n%s", out)
	}
}

// TestRosterCrewErrorShown checks that a per-crew roster/hold source
// failure surfaces as "unavailable: <error>" under that crew's heading,
// instead of an empty worker list indistinguishable from an idle crew.
func TestRosterCrewErrorShown(t *testing.T) {
	snap := loadFullSnapshot(t)
	msg := "boom"
	snap.Roster.Crews[0].WorkersError = &msg
	v := newRosterView(snap, rosterDeps{}, fixedRosterNow(t))
	out := v.View(80, 24)
	if !strings.Contains(out, "unavailable: boom") {
		t.Fatalf("crew roster failure should read unavailable: boom:\n%s", out)
	}
}

// TestRosterHoldsErrorShownWorkersStillRender checks that a holds-only
// failure still renders the crew's worker table, alongside a "holds
// unavailable: <err>" line in place of the hold lines.
func TestRosterHoldsErrorShownWorkersStillRender(t *testing.T) {
	snap := loadFullSnapshot(t)
	msg := "hold boom"
	snap.Roster.Crews[0].HoldsError = &msg
	v := newRosterView(snap, rosterDeps{}, fixedRosterNow(t))
	out := v.View(80, 24)
	if !strings.Contains(out, "sage") || !strings.Contains(out, "coral") {
		t.Fatalf("holds-only failure should still render the crew's workers:\n%s", out)
	}
	if !strings.Contains(out, "holds unavailable: hold boom") {
		t.Fatalf("holds-only failure should read holds unavailable: hold boom:\n%s", out)
	}
}

// TestRosterFlatSkipsWorkersErrorCrew checks that the roster cursor list
// contains only workers that tableView actually renders. A crew with
// WorkersError set — even one carrying a (malformed/legacy) non-empty
// Workers slice — must contribute nothing to v.flat, so "enter" can never
// target a hidden row.
func TestRosterFlatSkipsWorkersErrorCrew(t *testing.T) {
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

	v := newRosterView(snap, rosterDeps{eventsPath: "testdata/events.jsonl"}, fixedRosterNow(t))
	if len(v.flat) != 1 {
		t.Fatalf("flat should count only the healthy crew's workers, got %d", len(v.flat))
	}

	nv, _ := v.Update(keyRune('G'))
	v = nv.(rosterView)
	if v.flat[v.cursor].crewID != "healthy" {
		t.Fatalf("cursor after G should target the healthy crew's worker, got crewID=%q", v.flat[v.cursor].crewID)
	}

	nv, cmd := v.Update(keyType(tea.KeyEnter))
	v = nv.(rosterView)
	if !v.detail {
		t.Fatalf("enter on the healthy crew's worker should open the detail view")
	}
	if cmd == nil {
		t.Fatalf("enter on the healthy crew's worker should return a fetch cmd")
	}
}

// TestRosterStateInjectionIsCleaned checks that a worker's raw "state"
// field does not carry a terminal escape or bidi override from untrusted
// bus data into the rendered table. The Ascii color profile makes the
// rendered frame itself style-free, so any surviving ESC/U+202E must have
// come from the (uncleaned) data.
func TestRosterStateInjectionIsCleaned(t *testing.T) {
	orig := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.Ascii)
	defer lipgloss.SetColorProfile(orig)

	snap := loadFullSnapshot(t)
	snap.Roster.Crews[0].Workers[0]["state"] = "\x1b]0;pwned\x07working‮"
	v := newRosterView(snap, rosterDeps{}, fixedRosterNow(t))
	out := v.View(80, 24)
	if strings.ContainsRune(out, 0x1b) {
		t.Errorf("rendered roster table contains a raw ESC from an injected state:\n%q", out)
	}
	if strings.ContainsRune(out, 0x202e) {
		t.Errorf("rendered roster table contains U+202E from an injected state:\n%q", out)
	}
}

// TestRosterPRURLInjectionIsCleaned checks that a worker's raw "pr_url"
// field does not carry a terminal escape or bidi override into the
// rendered pr column. The Ascii color profile makes the rendered frame
// itself style-free, so any surviving ESC/U+202E must have come from the
// (uncleaned) data.
func TestRosterPRURLInjectionIsCleaned(t *testing.T) {
	orig := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.Ascii)
	defer lipgloss.SetColorProfile(orig)

	snap := loadFullSnapshot(t)
	snap.Roster.Crews[0].Workers[0]["pr_url"] = "\x1b[2Jhttps://pr‮"
	v := newRosterView(snap, rosterDeps{}, fixedRosterNow(t))
	out := v.View(80, 24)
	if strings.ContainsRune(out, 0x1b) {
		t.Errorf("rendered roster table contains a raw ESC from an injected pr_url:\n%q", out)
	}
	if strings.ContainsRune(out, 0x202e) {
		t.Errorf("rendered roster table contains U+202E from an injected pr_url:\n%q", out)
	}
}

// TestRosterEventKindInjectionIsCleaned checks eventLine, which
// concatenates an event's "kind" raw.
func TestRosterEventKindInjectionIsCleaned(t *testing.T) {
	orig := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.Ascii)
	defer lipgloss.SetColorProfile(orig)

	deps := rosterDeps{eventsPath: "testdata/events.jsonl"}
	v := newRosterView(loadFullSnapshot(t), deps, fixedRosterNow(t))
	nv, cmd := v.Update(keyType(tea.KeyEnter))
	v = nv.(rosterView)
	msg := cmd().(recentEventsMsg)
	for i := range msg.events {
		msg.events[i]["kind"] = "status\x1b[31m‮"
	}
	nv, _ = v.Update(msg)
	v = nv.(rosterView)
	out := v.View(80, 24)
	if strings.ContainsRune(out, 0x1b) {
		t.Errorf("rendered event detail contains a raw ESC from an injected kind:\n%q", out)
	}
	if strings.ContainsRune(out, 0x202e) {
		t.Errorf("rendered event detail contains U+202E from an injected kind:\n%q", out)
	}
}

// TestRosterDetailEventsWindowFits80x12 checks that the detail pane is
// scrollable and starts at the newest event: RecentEvents is oldest-first,
// so a top-anchored, unscrollable view would clip exactly the events a user
// drilling in wants to see. 20 events at height 12 must fit the frame
// exactly, start scrolled to the newest event (last visible, first not),
// and let `g` scroll back to the oldest.
func TestRosterDetailEventsWindowFits80x12(t *testing.T) {
	v := newTestRosterView(t)
	events := make([]data.Event, 20)
	for i := range events {
		events[i] = data.Event{
			"ts":   float64(1789999000000 + i*1000),
			"kind": "status",
			"body": map[string]any{"state": "evt" + strconv.Itoa(i)},
		}
	}
	nv, _ := v.Update(recentEventsMsg{events: events})
	v = nv.(rosterView)
	v.detail = true

	out := v.View(80, 12)
	lines := strings.Split(out, "\n")
	if len(lines) != 12 {
		t.Fatalf("frame has %d lines, want 12", len(lines))
	}
	for _, l := range lines {
		if w := ansi.StringWidth(l); w > 80 {
			t.Errorf("line %q is %d cells wide, want <= 80", l, w)
		}
	}
	if !strings.Contains(out, "evt19") {
		t.Errorf("newest event (evt19) should be visible initially:\n%s", out)
	}
	if strings.Contains(out, "evt0") {
		t.Errorf("oldest event (evt0) should not be visible initially:\n%s", out)
	}

	nv, _ = v.Update(keyRune('g'))
	v = nv.(rosterView)
	out = v.View(80, 12)
	if !strings.Contains(out, "evt0") {
		t.Errorf("oldest event (evt0) should be visible after g:\n%s", out)
	}
}

func TestRosterHoldShown(t *testing.T) {
	v := newTestRosterView(t)
	out := v.View(80, 24)
	if !strings.Contains(out, "hold h1") {
		t.Fatalf("expected a hold line for h1:\n%s", out)
	}
}

func TestRosterTickChangesAgeWithoutCollect(t *testing.T) {
	calls := 0
	deps := rosterDeps{collect: func() data.RosterSection { calls++; return data.RosterSection{} }}
	snap := loadFullSnapshot(t)
	base := fixedRosterNow(t)
	cur := base()
	now := func() time.Time { return cur }
	v := newRosterView(snap, deps, now)

	before := v.View(80, 24)
	cur = cur.Add(2 * time.Hour)
	nv, cmd := v.Update(tickMsg{})
	v = nv.(rosterView)
	if cmd == nil {
		t.Fatalf("tickMsg did not re-arm the ticker")
	}
	after := v.View(80, 24)
	if before == after {
		t.Fatalf("age text did not change after a tick + advanced clock")
	}
	if calls != 0 {
		t.Fatalf("tickMsg triggered %d collect(s), want 0", calls)
	}
}

func TestRosterBusChangeDebouncedToOneCollect(t *testing.T) {
	calls := 0
	deps := rosterDeps{collect: func() data.RosterSection { calls++; return data.RosterSection{Crews: []data.RosterCrew{}} }}
	base := fixedRosterNow(t)
	cur := base()
	now := func() time.Time { return cur }
	v := newRosterView(loadFullSnapshot(t), deps, now)

	nv, cmd1 := v.Update(busChangedMsg{})
	v = nv.(rosterView)
	t1 := v.lastChange

	cur = cur.Add(10 * time.Millisecond) // a second change arrives shortly after
	nv, cmd2 := v.Update(busChangedMsg{})
	v = nv.(rosterView)
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
	v = nv.(rosterView)
	if cmdAfter1 != nil {
		t.Fatalf("stale debounce (t1) returned a Cmd, want nil (no collect)")
	}
	if calls != 0 {
		t.Fatalf("stale debounce collected: calls = %d, want 0", calls)
	}

	msg2, ok := cmd2().(debounceFireMsg)
	if !ok {
		t.Fatalf("cmd2() = %T, want debounceFireMsg", cmd2())
	}
	nv, cmdAfter2 := v.Update(msg2)
	v = nv.(rosterView)
	if cmdAfter2 == nil {
		t.Fatalf("fresh debounce (t2) did not return a collect Cmd")
	}
	result := cmdAfter2()
	if _, ok := result.(rosterCollectedMsg); !ok {
		t.Fatalf("fresh debounce Cmd result = %T, want rosterCollectedMsg", result)
	}
	if calls != 1 {
		t.Fatalf("collect called %d times, want exactly 1", calls)
	}
}

func TestRosterEnterShowsRecentEvents(t *testing.T) {
	deps := rosterDeps{eventsPath: "testdata/events.jsonl"}
	v := newRosterView(loadFullSnapshot(t), deps, fixedRosterNow(t))

	nv, cmd := v.Update(keyType(tea.KeyEnter))
	v = nv.(rosterView)
	if !v.detail {
		t.Fatalf("enter did not open the worker detail pane")
	}
	if cmd == nil {
		t.Fatalf("enter did not return a Cmd to load recent events")
	}
	msg := cmd()
	nv, _ = v.Update(msg)
	v = nv.(rosterView)

	out := v.View(80, 24)
	if !strings.Contains(out, "working") {
		t.Fatalf("recent events for the cursor's worker should render its status events:\n%s", out)
	}

	nv, _ = v.Update(keyType(tea.KeyEsc))
	v = nv.(rosterView)
	if v.detail {
		t.Fatalf("esc did not close the detail pane")
	}
}

func TestRosterSelectionIsCursorWorker(t *testing.T) {
	v := newTestRosterView(t)
	sel, ok := v.Selection().(data.Worker)
	if !ok {
		t.Fatalf("Selection() = %T, want data.Worker", v.Selection())
	}
	if sel.Branch != "feat/584-crew-dash" || sel.Crew != "c1" {
		t.Fatalf("Selection() = %+v, want the first worker row", sel)
	}
}

func TestRosterErrorLinesInjectionIsCleaned(t *testing.T) {
	orig := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.Ascii)
	defer lipgloss.SetColorProfile(orig)

	msg := injectedErr
	cases := []struct {
		name string
		want string
		set  func(*data.Snapshot)
	}{
		{"roster", "unavailable: boom", func(s *data.Snapshot) { s.Roster.Error = &msg }},
		{"workers", "unavailable: boom", func(s *data.Snapshot) { s.Roster.Crews[0].WorkersError = &msg }},
		{"holds", "holds unavailable: boom", func(s *data.Snapshot) { s.Roster.Crews[0].HoldsError = &msg }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			snap := loadFullSnapshot(t)
			c.set(&snap)
			out := newRosterView(snap, rosterDeps{}, fixedRosterNow(t)).View(120, 24)
			requireNoControl(t, out)
			if !strings.Contains(out, c.want) {
				t.Errorf("missing %q:\n%q", c.want, out)
			}
		})
	}
}

func TestRosterDetailErrInjectionIsCleaned(t *testing.T) {
	orig := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.Ascii)
	defer lipgloss.SetColorProfile(orig)

	v := newTestRosterView(t)
	v.detail = true
	nv, _ := v.Update(recentEventsMsg{err: injectedErr})
	out := nv.(rosterView).View(120, 24)
	requireNoControl(t, out)
	if !strings.Contains(out, "unavailable: boom") {
		t.Errorf("missing unavailable: boom:\n%q", out)
	}
}

func TestRosterFieldsInjectionAreCleaned(t *testing.T) {
	orig := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.Ascii)
	defer lipgloss.SetColorProfile(orig)

	snap := loadFullSnapshot(t)
	snap.Roster.Crews = []data.RosterCrew{{
		ID: "c" + injectedErr,
		Workers: []map[string]any{
			{"name": "w1", "state": "working", "tier": "t" + injectedErr, "engine": "e" + injectedErr, "model": "m" + injectedErr, "age_s": float64(5)},
		},
		Holds: []map[string]any{
			{"id": "h" + injectedErr, "wait": map[string]any{"engine": "e" + injectedErr, "window": "w" + injectedErr}},
		},
	}}
	out := newRosterView(snap, rosterDeps{}, fixedRosterNow(t)).View(160, 24)
	requireNoControl(t, out)
	for _, want := range []string{"crew cboom", "hold hboom]0;pwned[31m: eboom]0;pwned[31m wboom"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%q", want, out)
		}
	}
}
