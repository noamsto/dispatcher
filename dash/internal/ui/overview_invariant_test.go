package ui

import (
	"cmp"
	"context"
	"fmt"
	"hash/fnv"
	"maps"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/noamsto/dispatcher/dash/internal/data"
	vm "github.com/noamsto/dispatcher/dash/internal/view"
)

// TestOverviewSelectionInvariant walks the sweep model through random keys,
// clicks, resizes, ticks and snapshot churn, and after every step checks the
// drawn frame against what p actually runs: the highlighted worker is the
// one acted on, or nothing is highlighted and nothing is acted on.
func TestOverviewSelectionInvariant(t *testing.T) {
	asciiProfile(t)
	const seqs, steps, groups = 200, 40, 8
	// Every frame is rendered twice (as drawn and after a tick), so the seeds
	// run in parallel groups to keep the test near a few seconds.
	for g := range uint64(groups) {
		t.Run(fmt.Sprintf("group%d", g), func(t *testing.T) {
			t.Parallel()
			for seed := g + 1; seed <= seqs; seed += groups {
				walkSeed(t, seed, steps)
			}
		})
	}
}

func walkSeed(t *testing.T, seed uint64, steps int) []uint64 {
	t.Helper()
	rng := rand.New(rand.NewPCG(seed, 0))
	m := sized(sweepModel(), 150, 40)
	var hist []string
	var frames []uint64
	for range steps {
		var name string
		m, name = walkStep(rng, m)
		hist = append(hist, name)
		frames = append(frames, hashFrame(m.View()))
		if err := selectionInvariant(m); err != "" {
			t.Fatalf("seed %d after %d steps: %s\nsteps: %s", seed, len(hist), err, strings.Join(hist, " "))
		}
	}
	return frames
}

func hashFrame(s string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(s))
	return h.Sum64()
}

// TestOverviewWalkDeterministic guards the replay promise: a failing seed must
// draw the same frames when run again.
func TestOverviewWalkDeterministic(t *testing.T) {
	asciiProfile(t)
	for seed := uint64(1); seed <= 5; seed++ {
		if a, b := walkSeed(t, seed, 40), walkSeed(t, seed, 40); !slices.Equal(a, b) {
			t.Errorf("seed %d replays different frames", seed)
		}
	}
}

func walkStep(rng *rand.Rand, m Model) (Model, string) {
	press := func(names ...string) (Model, string) {
		for _, n := range names {
			var k tea.KeyMsg
			switch n {
			case "tab":
				k = keyType(tea.KeyTab)
			case "shift+tab":
				k = keyType(tea.KeyShiftTab)
			case "enter":
				k = keyType(tea.KeyEnter)
			case "esc":
				k = keyType(tea.KeyEsc)
			default:
				k = keyRune([]rune(n)[0])
			}
			m = sweepKeys(m, k)
		}
		return m, strings.Join(names, "+")
	}
	switch n := rng.IntN(20); {
	case n < 2:
		return press("tab")
	case n < 3:
		return press("shift+tab")
	case n < 5:
		return press("j")
	case n < 7:
		return press("k")
	case n < 9:
		return press("enter")
	case n < 10:
		return press("esc")
	case n < 12:
		return press(string(rune('1' + rng.IntN(9))))
	case n < 13:
		return press("/", string("abcegopx0z"[rng.IntN(10)]), "enter")
	case n < 14:
		return press("/", "esc")
	case n < 16:
		x, y := rng.IntN(m.width), rng.IntN(m.height)
		nm, _ := m.Update(tea.MouseMsg{X: x, Y: y, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
		return nm.(Model), fmt.Sprintf("click(%d,%d)", x, y)
	case n < 17:
		w, h := 80+rng.IntN(121), 24+rng.IntN(37)
		if rng.IntN(2) == 0 { // small terminals, where panels lose their entries
			w, h = 20+rng.IntN(60), 6+rng.IntN(7)
		}
		return sized(m, w, h), fmt.Sprintf("resize(%dx%d)", w, h)
	case n < 18:
		nm, _ := m.Update(tickMsg{})
		return nm.(Model), "tick"
	}
	nm, _ := m.Update(snapshotMsg{in: churnedInputs(rng)})
	return nm.(Model), "snapshot"
}

// churnedInputs is the sweep fixture with workers removed, unblocked, blocked
// or aged and failed roles cleared, which removes and reorders attention
// items and can take the selected worker away.
func churnedInputs(rng *rand.Rand) vm.Inputs {
	in := sweepInputs()
	for i := range in.Snap.Roster.Crews {
		c := &in.Snap.Roster.Crews[i]
		c.Workers = slices.DeleteFunc(c.Workers, func(map[string]any) bool { return rng.IntN(8) == 0 })
		for _, w := range c.Workers {
			switch rng.IntN(6) {
			case 0:
				w["state"] = "working"
			case 1:
				w["state"] = "blocked"
			}
			if rng.IntN(3) == 0 {
				age := rng.IntN(3_600_000)
				switch ts := w["ts"].(type) {
				case int64:
					w["ts"] = ts - int64(age)
				case float64:
					w["ts"] = ts - float64(age)
				}
			}
		}
	}
	in.Snap.Roster.Peers = slices.DeleteFunc(in.Snap.Roster.Peers, func(map[string]any) bool { return rng.IntN(4) == 0 })
	keys := slices.SortedFunc(maps.Keys(in.Bus.Roles), func(a, b data.RoleKey) int {
		return cmp.Or(cmp.Compare(a.Crew, b.Crew), cmp.Compare(a.Branch, b.Branch), cmp.Compare(a.Role, b.Role))
	})
	for _, k := range keys {
		if rng.IntN(3) == 0 {
			delete(in.Bus.Roles, k)
		}
	}
	return in
}

// selectionInvariant checks the frame m draws, and the frame after a tick,
// against the argv p runs on a copy of m. It returns "" when they agree.
func selectionInvariant(m Model) string {
	frame := m.View()
	if nm, _ := m.Update(tickMsg{}); nm.(Model).View() != frame {
		return "a tick with unchanged inputs changed the frame"
	}
	lines := strings.Split(ansi.Strip(frame), "\n")
	if len(lines) != m.height {
		return fmt.Sprintf("frame has %d lines, want %d", len(lines), m.height)
	}
	ov := m.views[overviewTab].(overviewView)
	switch ov.mode {
	case ovNormal:
	case ovDetail:
		return detailInvariant(ov, lines)
	default:
		return ""
	}

	top := len(m.headerLines())
	r := ov.layout()
	body := lines[top : top+r.h]
	cell := func(x, y int) string { return ansi.Cut(body[y], x, x+1) }
	marks := 0
	for _, l := range body {
		marks += strings.Count(l, "▌")
	}

	var strip, rest []workerKey
	found, evMarks := 0, 0
	for p := panelAttention; p <= panelEvents; p++ {
		for _, it := range r.panels[p].items {
			n := 0
			for l := range it.lines {
				if cell(it.rect.x, it.rect.y+l) == "▌" {
					n++
				}
			}
			found += n
			if n == 0 {
				continue
			}
			if n != it.lines || it.kind == eHeader {
				return fmt.Sprintf("panel %d item %d is highlighted on %d of %d lines", p, it.idx, n, it.lines)
			}
			if p == panelEvents {
				evMarks++
				continue
			}
			k := drawnKey(ov.model, it)
			if p == panelAttention {
				strip = append(strip, k)
				continue
			}
			if k.branch == "" {
				return fmt.Sprintf("highlighted panel %d row %d names no branch", p, it.idx)
			}
			if !slices.Contains(rest, k) {
				rest = append(rest, k)
			}
		}
	}
	if found != marks {
		return fmt.Sprintf("%d selection bars drawn, %d on rows", marks, found)
	}
	if evMarks > 1 || evMarks == 1 && ov.st.focus != panelEvents {
		return fmt.Sprintf("%d event rows highlighted with focus on %v", evMarks, ov.st.focus)
	}
	if len(rest) > 1 {
		return fmt.Sprintf("panels highlight %d workers: %v", len(rest), rest)
	}

	stripFocused := r.panels[panelAttention].shown && strings.Contains(body[r.panels[panelAttention].rect.y], "▸ ATTENTION")
	if stripFocused && len(strip) != 1 {
		return fmt.Sprintf("the focused strip shows %d cursor rows, want 1", len(strip))
	}

	got, text := probePeek(ov)
	switch {
	case len(rest) == 1 && got == nil && text == "peer worker: read-only":
		if w := workerByKey(ov.model, rest[0]); w == nil || w.Origin != vm.OriginPeer {
			return fmt.Sprintf("p refused %v as a peer", rest[0])
		}
	case len(rest) == 1:
		if got == nil || *got != rest[0] {
			return fmt.Sprintf("%v is highlighted but p ran on %v (%q)", rest[0], got, text)
		}
	case got != nil || text != "peek: no worker selected":
		return fmt.Sprintf("nothing is highlighted but p ran on %v (%q)", got, text)
	}

	for _, k := range strip {
		switch {
		case stripFocused && len(rest) == 1 && k != rest[0]:
			return fmt.Sprintf("the strip cursor is on %v but %v is selected", k, rest[0])
		case stripFocused && len(rest) == 0 && k.branch != "" && hasCard(ov.model, r, k):
			return fmt.Sprintf("the strip cursor's worker %v is drawn but not selected", k)
		case !stripFocused && (len(rest) == 0 || k != rest[0]):
			return fmt.Sprintf("the unfocused strip highlights %v, the panels %v", k, rest)
		}
	}
	return ""
}

// narrowDetail is the width below which the detail title may omit the worker.
const narrowDetail = 40

// detailInvariant checks that the detail overlay names the worker p acts on.
func detailInvariant(ov overviewView, lines []string) string {
	if ov.insp.open {
		return "" // an inspect overlay acts on no worker
	}
	got, text := probePeek(ov)
	if got == nil {
		if text == "peer worker: read-only" || text == "peek: no worker selected" {
			return ""
		}
		return fmt.Sprintf("p in the detail overlay: %q", text)
	}
	label := workerLabel(*workerByKey(ov.model, *got))
	for _, l := range lines {
		if strings.Contains(l, "DETAIL") && strings.Contains(l, label) {
			return ""
		}
	}
	// The title drops the label when the box is too narrow to carry it.
	if len(lines) > 0 && ansi.StringWidth(lines[0]) < narrowDetail {
		return ""
	}
	return fmt.Sprintf("the detail overlay does not name %s, which p acts on", label)
}

// drawnKey is the worker a drawn row names, read from the model row it shows.
func drawnKey(ov vm.OverviewView, it placed) workerKey {
	switch it.kind {
	case eAttn:
		return workerKey{ov.Attention[it.idx].Crew, ov.Attention[it.idx].Branch}
	case eCard:
		return workerKey{ov.Workers[it.idx].Crew, ov.Workers[it.idx].Branch}
	case eAgent:
		return workerKey{ov.Agents[it.idx].Crew, ov.Agents[it.idx].Branch}
	case eWatch:
		return workerKey{ov.Monitors.Watchdogs[it.idx].Crew, ov.Monitors.Watchdogs[it.idx].Branch}
	case ePR:
		return workerKey{ov.Monitors.PRChecks[it.idx].Crew, ov.Monitors.PRChecks[it.idx].Branch}
	}
	return workerKey{}
}

func workerByKey(ov vm.OverviewView, k workerKey) *vm.OverviewWorker {
	for i, w := range ov.Workers {
		if w.Crew == k.crew && w.Branch == k.branch && k.branch != "" {
			return &ov.Workers[i]
		}
	}
	return nil
}

// hasCard reports whether the checklists draw a card for k.
func hasCard(ov vm.OverviewView, r overviewRects, k workerKey) bool {
	return slices.ContainsFunc(r.panels[panelChecklists].items, func(it placed) bool { return drawnKey(ov, it) == k })
}

// probePeek presses p on a copy of ov with a recording runner and returns the
// worker the peek argv names (nil when nothing ran) and the status it shows.
func probePeek(ov overviewView) (*workerKey, string) {
	fr := &data.FakeRunner{}
	ov.acts = actionDeps{ctx: context.Background(), runner: fr}
	_, cmd := ov.Update(keyRune('p'))
	text := ""
	if cmd != nil {
		if am, ok := cmd().(actionMsg); ok {
			text = am.text
		}
	}
	calls := fr.CallsSnapshot()
	if len(calls) == 0 {
		return nil, text
	}
	args := calls[0].Args
	i := slices.Index(args, "where")
	if i < 0 || len(args) != i+5 || args[i+1] != "--crew" || args[i+3] != "--" {
		return &workerKey{"?", strings.Join(args, " ")}, text
	}
	return &workerKey{args[i+2], args[i+4]}, text
}

func TestOverviewNarrowClosedBorders(t *testing.T) {
	asciiProfile(t)
	for w := 40; w < wideWidth; w += 3 {
		for _, h := range []int{14, 20, 30, 40} {
			v := newOverviewView(sweepInputs(), overviewNow)
			assertClosedBorders(t, fmt.Sprintf("%dx%d", w, h), stripped(v.View(w, h)))
		}
	}
}

// assertClosedBorders fails when a line has a rounded corner or side border
// without its partner: every panel row opens and closes with the same family
// of glyph, so a clipped or bled frame shows up as an odd count.
func assertClosedBorders(t *testing.T, name, frame string) {
	t.Helper()
	for i, l := range strings.Split(frame, "\n") {
		if strings.Count(l, "╭") != strings.Count(l, "╮") || strings.Count(l, "╰") != strings.Count(l, "╯") {
			t.Errorf("%s: line %d has unbalanced corners: %q", name, i, l)
		}
		if n := strings.Count(l, "│"); n%2 != 0 && !strings.HasPrefix(strings.TrimSpace(l), "▸") {
			t.Errorf("%s: line %d has an unclosed side border: %q", name, i, l)
		}
	}
}
