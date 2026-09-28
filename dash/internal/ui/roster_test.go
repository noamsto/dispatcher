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

// TestRosterCrewErrorShown matches finding 2: a per-crew roster/hold source
// failure surfaces as "unavailable: <error>" under that crew's heading,
// instead of an empty worker list indistinguishable from an idle crew.
func TestRosterCrewErrorShown(t *testing.T) {
	snap := loadFullSnapshot(t)
	msg := "boom"
	snap.Roster.Crews[0].Error = &msg
	v := newRosterView(snap, rosterDeps{}, fixedRosterNow(t))
	out := v.View(80, 24)
	if !strings.Contains(out, "unavailable: boom") {
		t.Fatalf("crew roster failure should read unavailable: boom:\n%s", out)
	}
}

// TestRosterStateInjectionIsCleaned matches finding 5: a worker's raw
// "state" field must not carry a terminal escape or bidi override from
// untrusted bus data into the rendered table. The Ascii color profile makes
// the rendered frame itself style-free, so any surviving ESC/U+202E must
// have come from the (uncleaned) data.
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

// TestRosterEventKindInjectionIsCleaned covers the MEDIUM half of finding 5:
// eventLine concatenates an event's "kind" raw.
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

// TestRosterDetailEventsWindowFits80x12 matches finding 6's detail-view half
// (frame size) and the follow-up finding that the detail pane must be
// scrollable and start at the newest event: RecentEvents is oldest-first, so
// a top-anchored, unscrollable view clips exactly the events a user drilling
// in wants to see. 20 events at height 12 must fit the frame exactly, start
// scrolled to the newest event (last visible, first not), and let `g`
// scroll back to the oldest.
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
