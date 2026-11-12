package ui

import (
	"fmt"
	"maps"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/noamsto/dispatcher/dash/internal/data"
)

// boardSnapshot is 6 workers across 2 repos (crews) in mixed states: one
// remote worker, and one worker whose title and detail must be cut.
func boardSnapshot() data.Snapshot {
	w := func(name, branch, state, title, tier, model, detail string, extra map[string]any) map[string]any {
		m := map[string]any{
			"branch": branch, "session": "s1", "name": name, "state": state,
			"title": title, "tier": tier, "engine": "claude", "model": model,
			"detail": detail, "ts": float64(1789999900000),
		}
		maps.Copy(m, extra)
		return m
	}
	long := "Rework the dispatcher roster so every worker card shows the whole task title even when it is long"
	return data.Snapshot{
		Now: 1790000000,
		Roster: data.RosterSection{Crews: []data.RosterCrew{
			{ID: "alpha", Workers: []map[string]any{
				w("basil", "feat/101-basil", "working", "Add a board tab", "standard", "claude-sonnet-5-5", "writing tests", nil),
				w("coral", "feat/102-coral", "blocked", long, "deep", "claude-opus-5-5",
					"waiting on the dispatcher to answer a question about the plan of record and its scope",
					map[string]any{"host": "mini"}),
				w("slate", "fix/103-slate", "pr_open", "Fix the ledger", "trivial", "haiku", "PR open", map[string]any{"pr_url": "https://github.com/o/r/pull/9"}),
			}},
			{ID: "beta", Workers: []map[string]any{
				w("amber", "feat/201-amber", "working", "Port crew nudge", "deep", "gpt-5", "reading code", nil),
				w("onyx", "feat/202-onyx", "done", "Docs sweep", "trivial", "haiku", "done", nil),
				w("pearl", "feat/203-pearl", "failed", "Gate flake", "standard", "claude-sonnet-5-5", "gate failed", nil),
			}},
		}},
	}
}

func boardModel(t *testing.T, w, h int) Model {
	t.Helper()
	m := sized(testModel(boardSnapshot()), w, h)
	m.active = goldenTabBoard
	return m
}

func TestGoldenBoard(t *testing.T) {
	asciiProfile(t)
	for _, w := range []int{80, 120, 200} {
		checkGolden(t, fmt.Sprintf("board_%dx40.golden", w), boardModel(t, w, 40).View(), w, 40)
	}
}

func TestBoardLinesFitWidth(t *testing.T) {
	asciiProfile(t)
	for _, w := range []int{60, 80, 120, 200} {
		for _, h := range []int{12, 40} {
			frame := boardModel(t, w, h).views[1].View(w, h)
			for i, l := range strings.Split(frame, "\n") {
				if got := lipgloss.Width(l); got > w {
					t.Errorf("w=%d h=%d line %d is %d cells: %q", w, h, i, got, l)
				}
			}
			assertWidth(t, boardModel(t, w, h).View(), w, h)
		}
	}
}

func TestBoardCardsHaveFixedWidth(t *testing.T) {
	asciiProfile(t)
	for _, sel := range []bool{false, true} {
		for _, row := range boardSnapshot().Roster.Crews[0].Workers {
			for i, l := range renderCard(row, sel) {
				if got := lipgloss.Width(l); got != boardCardW {
					t.Errorf("sel=%v %v line %d is %d cells, want %d: %q", sel, row["name"], i, got, boardCardW, l)
				}
			}
		}
	}
}

func TestBoardHidesEmptyColumns(t *testing.T) {
	asciiProfile(t)
	out := boardModel(t, 200, 40).View()
	for _, title := range groupTitles {
		if !strings.Contains(out, title) {
			t.Errorf("column %q missing at 200 cols:\n%s", title, out)
		}
	}
	snap := boardSnapshot()
	snap.Roster.Crews = snap.Roster.Crews[:1]
	snap.Roster.Crews[0].Workers = snap.Roster.Crews[0].Workers[:1]
	m := sized(testModel(snap), 200, 40)
	m.active = goldenTabBoard
	out = m.View()
	if !strings.Contains(out, "working (1)") || strings.Contains(out, "blocked") {
		t.Errorf("empty columns not hidden:\n%s", out)
	}
	for l := range strings.SplitSeq(out, "\n") {
		if strings.TrimSpace(l) == "alpha" {
			t.Errorf("repo header drawn for a single repo:\n%s", out)
		}
	}
}

func TestBoardSelectionAndActions(t *testing.T) {
	m := boardModel(t, 200, 40)
	sel := func(m Model) data.Worker {
		w, _ := m.views[1].Selection().(data.Worker)
		return w
	}
	first := sel(m)
	if first.State != "working" {
		t.Fatalf("first selection = %+v, want a working worker", first)
	}
	// The same worker's Live disabled reasons come from the same function.
	nm, cmd := m.Update(keyRune('o'))
	m = nm.(Model)
	if cmd == nil {
		t.Fatalf("o on a worker without a PR returned no status cmd")
	}
	am, ok := cmd().(actionMsg)
	if !ok || am.text != "open PR: no PR" || !am.err {
		t.Fatalf("o on %s = %+v, want %q", first.Name, am, "open PR: no PR")
	}

	// l moves to the blocked column; reply opens its prompt there, as on Live.
	nm, _ = m.Update(keyRune('l'))
	m = nm.(Model)
	if w := sel(m); w.State != "blocked" || w.Host != "mini" {
		t.Fatalf("after l selection = %+v, want the blocked remote worker", w)
	}
	nm, _ = m.Update(keyRune('m'))
	m = nm.(Model)
	if !m.views[1].Capturing() {
		t.Fatalf("m on a blocked worker did not open the reply prompt")
	}
	nm, _ = m.Update(keyType(tea.KeyEsc))
	m = nm.(Model)

	// Jump on a remote worker is refused with Live's reason.
	_, cmd = m.Update(keyRune('t'))
	am, _ = cmd().(actionMsg)
	if want := "jump: remote worker (host mini)"; am.text != want {
		t.Fatalf("t = %q, want %q", am.text, want)
	}

	// The same key on the same worker gives the same reason on Live.
	live := sized(testModel(boardSnapshot()), 200, 40)
	liveSel := func() data.Worker { w, _ := live.views[0].Selection().(data.Worker); return w }
	for range 6 {
		if liveSel().Name == "coral" {
			break
		}
		live.views[0], _ = live.views[0].Update(keyRune('j'))
	}
	if liveSel().Name != "coral" {
		t.Fatalf("live never reached coral: %+v", liveSel())
	}
	_, lcmd := live.views[0].Update(keyRune('t'))
	if lam, _ := lcmd().(actionMsg); lam != am {
		t.Fatalf("live t = %+v, board t = %+v", lam, am)
	}
}

func TestBoardNavigation(t *testing.T) {
	m := boardModel(t, 200, 40)
	name := func() string { w, _ := m.views[1].Selection().(data.Worker); return w.Name }
	press := func(r rune) {
		nm, _ := m.Update(keyRune(r))
		m = nm.(Model)
	}
	seen := []string{name()}
	for range 5 {
		press('j')
		seen = append(seen, name())
	}
	if got := strings.Join(seen, ","); got != "basil,amber,coral,slate,onyx,pearl" {
		t.Errorf("j walk = %s", got)
	}
	press('j')
	last := name()
	press('j')
	if name() != last {
		t.Errorf("j at the end moved from %s to %s", last, name())
	}
	for range 10 {
		press('k')
	}
	press('l')
	press('h')
	if w, _ := m.views[1].Selection().(data.Worker); w.State != "working" {
		t.Errorf("h from the blocked column = %+v", w)
	}
	nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRight})
	if w, _ := nm.(Model).views[1].Selection().(data.Worker); w.State != "blocked" {
		t.Errorf("right arrow = %+v", w)
	}
}

func TestBoardSelectedCardBorderIsDouble(t *testing.T) {
	asciiProfile(t)
	out := boardModel(t, 200, 40).View()
	if n := strings.Count(out, "╔"); n != 1 {
		t.Errorf("%d highlighted cards, want 1", n)
	}
}

func TestBoardResizeKeepsSelection(t *testing.T) {
	asciiProfile(t)
	m := boardModel(t, 200, 40)
	nm, _ := m.Update(keyRune('G'))
	m = nm.(Model)
	for _, sz := range [][2]int{{60, 14}, {80, 20}, {200, 40}, {30, 10}, {20, 5}} {
		m = sized(m, sz[0], sz[1])
		out := m.View()
		assertWidth(t, out, sz[0], sz[1])
		if sz[0] >= boardCardW && sz[1] >= 14 && !strings.Contains(out, "╔") {
			t.Errorf("%dx%d: selected card scrolled out of view:\n%s", sz[0], sz[1], out)
		}
	}
}

// TestBoardCardFieldsMatchSVGCard asserts the card text of the fixture that
// crew/internal/rosterrender/card_test.go asserts for the SVG card
// (TestCardFieldsMatchDashBoard): keep the two in step.
func TestBoardCardFieldsMatchSVGCard(t *testing.T) {
	got := fieldsOf(map[string]any{
		"branch": "feat/12-x", "name": "lime", "state": "pr_open", "title": "fix the bug",
		"tier": "deep", "engine": "claude", "model": "claude-opus-5-5", "detail": "running tests",
	})
	want := cardFields{
		head: "lime · #12", state: "pr open", title: "fix the bug",
		tier: "deep", meta: "deep·claude·opus", detail: "running tests",
	}
	if got != want {
		t.Errorf("fields = %+v, want %+v", got, want)
	}
}

func TestBoardRemoteHostShowsOnRow3(t *testing.T) {
	asciiProfile(t)
	rows := cardRows(fieldsOf(map[string]any{
		"branch": "feat/1-a", "name": "a", "state": "working", "tier": "standard",
		"engine": "claude", "model": "claude-sonnet-5-5", "host": "mini-host",
	}), lipgloss.NewStyle())
	if !strings.Contains(rows[3], "@mini-host") {
		t.Errorf("row 3 = %q lacks the host", rows[3])
	}
	if lipgloss.Width(rows[3]) != boardContent {
		t.Errorf("row 3 is %d cells, want %d", lipgloss.Width(rows[3]), boardContent)
	}
}

func TestBoardFailedCrewIsNotAnEmptyBoard(t *testing.T) {
	asciiProfile(t)
	snap := boardSnapshot()
	boom := "boom"
	snap.Roster.Crews[1].WorkersError = &boom
	m := sized(testModel(snap), 120, 40)
	m.active = goldenTabBoard
	if out := m.View(); !strings.Contains(out, "crew beta: workers unavailable: boom") || !strings.Contains(out, "basil") {
		t.Errorf("failed crew not reported beside the loaded one:\n%s", out)
	}
	snap.Roster.Crews = snap.Roster.Crews[1:]
	m = sized(testModel(snap), 120, 40)
	m.active = goldenTabBoard
	if out := m.View(); !strings.Contains(out, "workers unavailable: boom") || strings.Contains(out, "no workers") {
		t.Errorf("sole failed crew shown as no workers:\n%s", out)
	}
}

func TestBoardManyFailedCrewsKeepPrompt(t *testing.T) {
	asciiProfile(t)
	snap := boardSnapshot()
	boom := "boom"
	for i := range 6 {
		snap.Roster.Crews = append(snap.Roster.Crews, data.RosterCrew{ID: fmt.Sprintf("bad%d", i), WorkersError: &boom})
	}
	m := sized(testModel(snap), 120, 8)
	m.active = goldenTabBoard
	out := m.views[1].View(120, 6)
	lines := strings.Split(out, "\n")
	if len(lines) > 6 || !strings.Contains(lines[len(lines)-1], "·") || !strings.Contains(out, "more crews") {
		t.Errorf("info line or overflow note missing:\n%s", out)
	}
}

func TestBoardMapOpensOnBoardSelection(t *testing.T) {
	m := boardModel(t, 200, 40)
	nm, _ := m.Update(keyRune('j')) // basil -> amber (crew beta)
	m = nm.(Model)
	if w, _ := m.handoffSelection().(data.Worker); w.Name != "amber" {
		t.Errorf("handoff = %+v, want the board's selected worker", m.handoffSelection())
	}
}

func TestBoardCardCutsLongStateHostAndFirstWord(t *testing.T) {
	asciiProfile(t)
	card := renderCard(map[string]any{
		"branch": "feat/1-a", "name": "a", "state": "waiting_for_dispatcher_answer_now",
		"title": strings.Repeat("x", 40) + " second words", "host": "strata.tail1234.ts.net",
		"engine": "claude", "model": "opus", "tier": "deep",
	}, false)
	for i, l := range card {
		if got := lipgloss.Width(l); got != boardCardW {
			t.Errorf("line %d is %d cells, want %d: %q", i, got, boardCardW, l)
		}
	}
	if !strings.Contains(card[3], "second words") {
		t.Errorf("words after an over-wide first word lost: %q", card[3])
	}
}

func TestBoardPromptReplacesInfoLine(t *testing.T) {
	m := boardModel(t, 120, 40)
	nm, _ := m.Update(keyRune('l'))
	nm, _ = nm.Update(keyRune('m'))
	if out := nm.(Model).View(); !strings.Contains(out, "reply to coral") {
		t.Errorf("reply prompt not shown:\n%s", out)
	}
}
