package ui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/noamsto/dispatcher/dash/internal/data"
	"github.com/noamsto/dispatcher/dash/internal/kitty"
	"github.com/noamsto/dispatcher/dash/internal/mapgeom"
)

const (
	mapOut  = "/tmp/crew-dash-map.png"
	mapBase = 1000

	placeholder = "\U0010EEEE"
)

// mapPx are the --px values the rig answers: the image block at 100x30,
// 80x24 and 160x40 with 10x20 pixel cells.
var mapPx = []string{"1000x500", "800x380", "1600x700"}

func loadMapGeometry(t *testing.T) mapgeom.Geometry {
	t.Helper()
	raw, err := os.ReadFile("testdata/map-geometry.json")
	if err != nil {
		t.Fatal(err)
	}
	g, err := mapgeom.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// mapSnapshot is a 16-worker crew c1 whose roster order is the reverse of the
// geometry's slot order, plus a one-worker crew c2. Planet i is working,
// blocked, pr_open or done for i%4 = 0..3; planet 3 is on host mini. The Live
// cursor, the blocked worker that has waited longest, is planet 1.
func mapSnapshot(t *testing.T) data.Snapshot {
	t.Helper()
	g := loadMapGeometry(t)
	states := []string{"working", "blocked", "pr_open", "done"}
	n := len(g.Planets)
	workers := make([]map[string]any, n)
	for i, p := range g.Planets {
		w := map[string]any{
			"branch": p.Branch, "name": p.Name, "state": states[i%4], "session": "s1",
			"ts": float64(1789999000000 + i*1000), "age_s": float64(1000 - i), "detail": "d",
		}
		if states[i%4] == "pr_open" {
			w["pr_url"] = fmt.Sprintf("https://github.com/o/r/pull/%d", 100+i)
		}
		if i == 3 {
			w["host"] = "mini"
		}
		workers[n-1-i] = w
	}
	zeta := map[string]any{
		"branch": "feat/200-zeta", "name": "zeta", "state": "working", "session": "s1",
		"ts": float64(1789999900000),
	}
	return data.Snapshot{Now: 1790000000, Roster: data.RosterSection{Crews: []data.RosterCrew{
		{ID: "c1", Workers: workers},
		{ID: "c2", Workers: []map[string]any{zeta}},
	}}}
}

// mapRig is a root Model wired to a FakeRunner for `crew roster-render` and a
// recorder standing in for /dev/tty.
type mapRig struct {
	t    *testing.T
	fake *data.FakeRunner
	tty  *bytes.Buffer
	deps Deps
	snap data.Snapshot
	seen []tea.Msg // every message a pumped Cmd produced
}

func newMapRig(t *testing.T, inTmux, capable bool) *mapRig {
	t.Helper()
	snap := mapSnapshot(t)
	r := &mapRig{t: t, fake: &data.FakeRunner{Responses: map[string]data.FakeResponse{}}, tty: &bytes.Buffer{}, snap: snap}
	r.deps = Deps{
		Runner: r.fake, Crew: []string{"crew"}, InTmux: inTmux, Now: snap.Now,
		Map: MapDeps{
			TTY:     r.tty,
			CellPx:  func() (int, int) { return 10, 20 },
			Capable: func() (bool, string) { return capable, map[bool]string{false: "needs kitty or ghostty"}[capable] },
			OutPath: mapOut,
			IDBase:  mapBase,
		},
	}
	g := loadMapGeometry(t)
	for _, px := range mapPx {
		r.respond("c1", px, g)
	}
	r.respond("c2", "1000x500", mapgeom.Geometry{V: 1, W: 1000, H: 500, PNG: mapOut,
		Planets: []mapgeom.Planet{{Branch: "feat/200-zeta", Name: "zeta", CX: 500, CY: 250, R: 26}}})
	return r
}

// respond answers the render of crew at px, with or without a --select, with g.
func (r *mapRig) respond(crew, px string, g mapgeom.Geometry) {
	r.t.Helper()
	raw, err := json.Marshal(g)
	if err != nil {
		r.t.Fatal(err)
	}
	key := "crew roster-render --map --crew " + crew + " --px " + px + " --out " + mapOut
	r.fake.Responses[key] = ok(string(raw))
	for _, p := range g.Planets {
		r.fake.Responses[key+" --select "+p.Branch] = ok(string(raw))
	}
}

func (r *mapRig) model(w, h int) Model {
	return sized(loadedModel(r.snap, r.deps), w, h)
}

// open sizes the model to 100x30 and shows the Map tab, running its render.
func (r *mapRig) open() Model {
	return r.press(r.model(100, 30), keyRune('5'))
}

// press delivers msg and runs the Cmds it leads to: renders land, action
// results reach the status line. The status-clear tick is not run.
func (r *mapRig) press(m Model, msg tea.Msg) Model {
	nm, cmd := m.Update(msg)
	return r.run(nm.(Model), cmd)
}

func (r *mapRig) run(m Model, cmd tea.Cmd) Model {
	for _, msg := range flattenCmd(cmd) {
		r.seen = append(r.seen, msg)
		switch msg := msg.(type) {
		case mapRenderedMsg:
			nm, next := m.Update(msg)
			m = r.run(nm.(Model), next)
		case actionMsg:
			nm, _ := m.Update(msg)
			m = nm.(Model)
		}
	}
	return m
}

func (r *mapRig) count(typeName string) int {
	n := 0
	for _, msg := range r.seen {
		if fmt.Sprintf("%T", msg) == typeName {
			n++
		}
	}
	return n
}

func mapSel(m Model) string { return m.views[mapTab].(mapView).sel }

func renders(f *data.FakeRunner) int {
	n := 0
	for _, c := range f.CallsSnapshot() {
		if slices.Contains(c.Args, "roster-render") {
			n++
		}
	}
	return n
}

// actionCalls is every recorded call that is not a render.
func actionCalls(f *data.FakeRunner) [][]string {
	var out [][]string
	for _, a := range argvs(f) {
		if !slices.Contains(a, "roster-render") {
			out = append(out, a)
		}
	}
	return out
}

func wantActionCalls(t *testing.T, f *data.FakeRunner, want ...[]string) {
	t.Helper()
	got := actionCalls(f)
	if len(got) != len(want) {
		t.Fatalf("action calls = %q, want %q", got, want)
	}
	for i := range got {
		if !slices.Equal(got[i], want[i]) {
			t.Fatalf("action calls = %q, want %q", got, want)
		}
	}
}

// selectBranch steps the Map selection to branch.
func (r *mapRig) selectBranch(m Model, branch string) Model {
	r.t.Helper()
	for range 20 {
		if mapSel(m) == branch {
			return m
		}
		m = r.press(m, keyRune('l'))
	}
	r.t.Fatalf("branch %q not reachable", branch)
	return m
}

func planetBranches(g mapgeom.Geometry) []string {
	var out []string
	for _, p := range g.Planets {
		out = append(out, p.Branch)
	}
	return out
}

func click(x, y int) tea.MouseMsg {
	return tea.MouseMsg{X: x, Y: y, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft}
}

// cellOf is the root-model mouse position of the cell at image pixel (px, py)
// for the 100x30 rig, where the block starts at the body's top-left and the
// tab bar is one line.
func cellOf(px, py float64) (x, y int) { return int(px / 10), int(py/20) + 1 }

func TestMapOpensOnLiveCursorCrewAndSelection(t *testing.T) {
	r := newMapRig(t, false, true)
	m := r.open()
	if m.active != mapTab {
		t.Fatalf("active = %d, want the map tab", m.active)
	}
	mv := m.views[mapTab].(mapView)
	if mv.crewID != "c1" || mv.sel != "feat/101-basil" {
		t.Fatalf("crew %q sel %q, want c1 on the Live cursor's worker", mv.crewID, mv.sel)
	}
	wantArgv := []string{"crew", "roster-render", "--map", "--crew", "c1", "--px", "1000x500", "--out", mapOut, "--select", "feat/101-basil"}
	if got := argvs(r.fake); len(got) != 1 || !slices.Equal(got[0], wantArgv) {
		t.Fatalf("calls = %q, want %q", got, wantArgv)
	}
}

func TestMapOpensOnFirstCrewWithWorkersWhenLiveHasNone(t *testing.T) {
	r := newMapRig(t, false, true)
	r.snap.Roster.Crews[0].Workers = nil
	m := r.open()
	if mv := m.views[mapTab].(mapView); mv.crewID != "c2" {
		t.Fatalf("crew = %q, want the first crew with workers", mv.crewID)
	}
}

func TestMapOpensOnTheCrewOfTheLiveCursor(t *testing.T) {
	r := newMapRig(t, false, true)
	r.snap.Roster.Crews[1].Workers[0]["state"] = "blocked"
	r.snap.Roster.Crews[1].Workers[0]["ts"] = float64(1789000000000)
	m := r.open()
	mv := m.views[mapTab].(mapView)
	if mv.crewID != "c2" || mv.sel != "feat/200-zeta" {
		t.Fatalf("crew %q sel %q, want c2 on zeta", mv.crewID, mv.sel)
	}
}

func TestMapCyclesCrews(t *testing.T) {
	r := newMapRig(t, false, true)
	m := r.press(r.open(), keyRune('c'))
	mv := m.views[mapTab].(mapView)
	if mv.crewID != "c2" || mv.sel != "feat/200-zeta" || len(mv.geom.Planets) != 1 {
		t.Fatalf("crew %q sel %q planets %d, want c2 with its own map", mv.crewID, mv.sel, len(mv.geom.Planets))
	}
	m = r.press(m, keyRune('c'))
	if mv := m.views[mapTab].(mapView); mv.crewID != "c1" {
		t.Fatalf("crew = %q after wrapping, want c1", mv.crewID)
	}
}

func TestMapKeyboardOrderIsSlotOrder(t *testing.T) {
	r := newMapRig(t, false, true)
	slots := planetBranches(loadMapGeometry(t))
	m := r.open()
	for i := 1; i <= len(slots); i++ {
		m = r.press(m, keyRune('l'))
		if want := slots[(1+i)%len(slots)]; mapSel(m) != want {
			t.Fatalf("after %d next: sel %q, want %q", i, mapSel(m), want)
		}
	}
	m = r.press(m, keyRune('h'))
	if want := slots[0]; mapSel(m) != want {
		t.Fatalf("prev from slot 1 = %q, want slot 0 %q", mapSel(m), want)
	}
	m = r.press(m, keyRune('h'))
	if want := slots[len(slots)-1]; mapSel(m) != want {
		t.Fatalf("prev from slot 0 = %q, want the last slot %q (wrap)", mapSel(m), want)
	}
}

func TestMapNextAndPrevKeys(t *testing.T) {
	slots := planetBranches(loadMapGeometry(t))
	keys := map[string]tea.KeyMsg{
		"right": keyType(tea.KeyRight), "down": keyType(tea.KeyDown), "l": keyRune('l'), "j": keyRune('j'), "tab": keyType(tea.KeyTab),
		"left": keyType(tea.KeyLeft), "up": keyType(tea.KeyUp), "h": keyRune('h'), "k": keyRune('k'), "shift+tab": keyType(tea.KeyShiftTab),
	}
	for name, want := range map[string]string{
		"right": slots[2], "down": slots[2], "l": slots[2], "j": slots[2], "tab": slots[2],
		"left": slots[0], "up": slots[0], "h": slots[0], "k": slots[0], "shift+tab": slots[0],
	} {
		t.Run(name, func(t *testing.T) {
			r := newMapRig(t, false, true)
			m := r.press(r.open(), keys[name])
			if mapSel(m) != want {
				t.Fatalf("sel = %q, want %q", mapSel(m), want)
			}
			if m.active != mapTab {
				t.Fatalf("%s left the map tab", name)
			}
		})
	}
}

func TestMapSelectionRendersWithRing(t *testing.T) {
	r := newMapRig(t, false, true)
	m := r.press(r.open(), keyRune('l'))
	calls := argvs(r.fake)
	if len(calls) != 2 {
		t.Fatalf("calls = %q, want open and one selection render", calls)
	}
	last := calls[1]
	if got := last[len(last)-2:]; !slices.Equal(got, []string{"--select", "feat/102-coral"}) {
		t.Fatalf("selection render ends %q, want --select feat/102-coral", got)
	}
	if mapSel(m) != "feat/102-coral" {
		t.Fatalf("sel = %q", mapSel(m))
	}
}

func TestMapKeyRepeatCoalescesIntoOneFollowUpRender(t *testing.T) {
	r := newMapRig(t, false, true)
	m := r.open()
	nm, first := m.Update(keyRune('l'))
	m = nm.(Model)
	nm, second := m.Update(keyRune('l'))
	m = nm.(Model)
	nm, third := m.Update(keyRune('l'))
	m = nm.(Model)
	if second != nil || third != nil {
		t.Fatalf("a key during an in-flight render must not start another")
	}
	m = r.run(m, first)
	if got := renders(r.fake); got != 3 {
		t.Fatalf("%d renders, want open, the first key and one follow-up", got)
	}
	calls := argvs(r.fake)
	if last := calls[len(calls)-1]; last[len(last)-1] != "feat/104-ember" {
		t.Fatalf("follow-up render did not carry the latest selection: %q", last)
	}
	if mapSel(m) != "feat/104-ember" {
		t.Fatalf("sel = %q", mapSel(m))
	}
}

func TestMapClickSelectsWithHeaderOffset(t *testing.T) {
	r := newMapRig(t, false, true)
	m := r.open()
	g := loadMapGeometry(t)
	for _, i := range []int{5, 0, 15} {
		x, y := cellOf(g.Planets[i].CX, g.Planets[i].CY)
		m = r.press(m, click(x, y))
		if mapSel(m) != g.Planets[i].Branch {
			t.Fatalf("click on planet %d: sel = %q, want %q", i, mapSel(m), g.Planets[i].Branch)
		}
	}
}

func TestMapClickBetweenPlanetsKeepsSelection(t *testing.T) {
	r := newMapRig(t, false, true)
	m := r.open()
	g := loadMapGeometry(t)
	before := renders(r.fake)
	x, y := cellOf((g.Planets[0].CX+g.Planets[1].CX)/2, (g.Planets[0].CY+g.Planets[1].CY)/2)
	m = r.press(m, click(x, y))
	if mapSel(m) != "feat/101-basil" {
		t.Fatalf("a click on empty space moved the selection to %q", mapSel(m))
	}
	if renders(r.fake) != before {
		t.Fatalf("a miss rendered again")
	}
}

func TestMapClickOnTabBarOrOtherButtonsIsIgnored(t *testing.T) {
	r := newMapRig(t, false, true)
	m := r.open()
	g := loadMapGeometry(t)
	x, y := cellOf(g.Planets[5].CX, g.Planets[5].CY)
	for name, msg := range map[string]tea.MouseMsg{
		"header row":   click(x, 0),
		"right button": {X: x, Y: y, Action: tea.MouseActionPress, Button: tea.MouseButtonRight},
		"left release": {X: x, Y: y, Action: tea.MouseActionRelease, Button: tea.MouseButtonLeft},
		"left motion":  {X: x, Y: y, Action: tea.MouseActionMotion, Button: tea.MouseButtonLeft},
	} {
		if got := mapSel(r.press(m, msg)); got != "feat/101-basil" {
			t.Fatalf("%s moved the selection to %q", name, got)
		}
	}
}

// Planet 0 spans pixel rows 34-86, so body row 0 (pixels 0-20) misses it and
// body row 1 hits. Only with the tab bar's line removed does root row 1 mean
// body row 0.
func TestMapClickHeaderOffsetBoundary(t *testing.T) {
	r := newMapRig(t, false, true)
	m := r.open()
	if got := mapSel(r.press(m, click(50, 1))); got != "feat/101-basil" {
		t.Fatalf("the first body row selected %q, want a miss", got)
	}
	if got := mapSel(r.press(m, click(50, 2))); got != "feat/100-amber" {
		t.Fatalf("the second body row selected %q, want planet 0", got)
	}
}

func TestMapActionsDispatchLiveArgv(t *testing.T) {
	t.Run("jump", func(t *testing.T) {
		r := newMapRig(t, true, true)
		r.fake.Responses["crew where --crew c1 -- feat/100-amber"] = ok(wherePrefix + "jump: ! tmux switch-client -t %42\n")
		r.fake.Responses["tmux switch-client -t %42"] = ok("")
		m := r.selectBranch(r.open(), "feat/100-amber")
		m = r.press(m, keyRune('t'))
		wantActionCalls(t, r.fake,
			[]string{"crew", "where", "--crew", "c1", "--", "feat/100-amber"},
			[]string{"tmux", "switch-client", "-t", "%42"})
		wantStatus(t, m, "jump: ok")
	})
	t.Run("jump outside tmux", func(t *testing.T) {
		r := newMapRig(t, false, true)
		m := r.press(r.open(), keyRune('t'))
		wantActionCalls(t, r.fake)
		wantStatus(t, m, "jump: not inside tmux")
	})
	t.Run("jump remote worker", func(t *testing.T) {
		r := newMapRig(t, true, true)
		m := r.selectBranch(r.open(), "feat/103-dune")
		m = r.press(m, keyRune('t'))
		wantActionCalls(t, r.fake)
		wantStatus(t, m, "jump: remote worker (host mini)")
	})
	t.Run("open PR", func(t *testing.T) {
		r := newMapRig(t, false, true)
		url := "https://github.com/o/r/pull/102"
		r.fake.Responses["gh pr view --web "+url] = ok("")
		m := r.selectBranch(r.open(), "feat/102-coral")
		m = r.press(m, keyRune('o'))
		wantActionCalls(t, r.fake, []string{"gh", "pr", "view", "--web", url})
		wantStatus(t, m, "open PR: ok")
	})
	t.Run("open PR without one", func(t *testing.T) {
		r := newMapRig(t, false, true)
		m := r.selectBranch(r.open(), "feat/100-amber")
		m = r.press(m, keyRune('o'))
		wantActionCalls(t, r.fake)
		wantStatus(t, m, "open PR: no PR")
	})
	t.Run("reply", func(t *testing.T) {
		r := newMapRig(t, false, true)
		r.fake.Responses["crew reply --crew c1 -- worker:feat/101-basil go ahead"] = ok("sent\n")
		m := r.press(r.open(), keyRune('m'))
		if !m.views[mapTab].Capturing() || !strings.Contains(m.View(), "reply to basil: ") {
			t.Fatalf("reply prompt not shown:\n%s", m.View())
		}
		m = typeText(m, "go ahead")
		m = r.press(m, keyType(tea.KeyEnter))
		wantActionCalls(t, r.fake, []string{"crew", "reply", "--crew", "c1", "--", "worker:feat/101-basil", "go ahead"})
		wantStatus(t, m, "reply: sent")
		if m.views[mapTab].Capturing() {
			t.Fatalf("prompt should close after sending")
		}
	})
	t.Run("reply while not blocked", func(t *testing.T) {
		r := newMapRig(t, false, true)
		m := r.selectBranch(r.open(), "feat/100-amber")
		m = r.press(m, keyRune('m'))
		wantActionCalls(t, r.fake)
		wantStatus(t, m, "reply: not blocked")
		if m.views[mapTab].Capturing() {
			t.Fatalf("no prompt for a worker that is not blocked")
		}
	})
	t.Run("nudge confirm", func(t *testing.T) {
		r := newMapRig(t, false, true)
		r.fake.Responses["crew nudge --crew c1 -- basil"] = ok("")
		m := r.press(r.open(), keyRune('n'))
		wantActionCalls(t, r.fake)
		if !strings.Contains(m.View(), "nudge basil? y/N") {
			t.Fatalf("confirm not shown:\n%s", m.View())
		}
		m = r.press(m, keyRune('y'))
		wantActionCalls(t, r.fake, []string{"crew", "nudge", "--crew", "c1", "--", "basil"})
		wantStatus(t, m, "nudge: ok")
	})
	t.Run("nudge declined", func(t *testing.T) {
		r := newMapRig(t, false, true)
		m := r.press(r.open(), keyRune('n'))
		m = r.press(m, keyRune('n'))
		wantActionCalls(t, r.fake)
		wantStatus(t, m, "nudge: cancelled")
	})
}

func TestMapKeysWhileCapturingGoToThePrompt(t *testing.T) {
	r := newMapRig(t, false, true)
	m := r.press(r.open(), keyRune('m'))
	for _, k := range []tea.KeyMsg{keyRune('q'), keyRune('5'), keyType(tea.KeyTab), keyRune('l')} {
		nm, cmd := m.Update(k)
		m = nm.(Model)
		if cmd != nil && k.Type != tea.KeyRunes {
			t.Fatalf("%v produced a Cmd while the reply prompt is open", k)
		}
	}
	if m.active != mapTab || !m.views[mapTab].Capturing() || mapSel(m) != "feat/101-basil" {
		t.Fatalf("keys escaped the prompt: active %d, sel %q", m.active, mapSel(m))
	}
}

func TestMapHasNoMergeOrReapedKey(t *testing.T) {
	r := newMapRig(t, false, true)
	m := r.open()
	for _, k := range []rune{'a', 'M', 'A', 'x'} {
		nm, cmd := m.Update(keyRune(k))
		if cmd != nil {
			t.Fatalf("key %q did something on the map", k)
		}
		m = nm.(Model)
	}
	wantActionCalls(t, r.fake)
	for _, b := range m.views[mapTab].Keys() {
		for _, k := range b.Keys() {
			if k == "a" || k == "M" {
				t.Fatalf("help advertises %q", k)
			}
		}
	}
}

func rosterMsgOf(mutate func(*data.RosterSection)) sourceMsg {
	return sourceMsg{srcRoster, func(s *data.Snapshot) { mutate(&s.Roster) }}
}

func TestMapPlanetWithoutWorkerReportsNotInRoster(t *testing.T) {
	r := newMapRig(t, true, true)
	m := r.selectBranch(r.open(), "feat/102-coral")
	m = r.press(m, rosterMsgOf(func(ro *data.RosterSection) {
		ws := ro.Crews[0].Workers
		ro.Crews[0].Workers = slices.DeleteFunc(slices.Clone(ws), func(w map[string]any) bool { return w["branch"] == "feat/102-coral" })
	}))
	if mapSel(m) != "feat/102-coral" {
		t.Fatalf("the roster change moved the selection to %q", mapSel(m))
	}
	if !strings.Contains(m.View(), "worker not in roster") {
		t.Fatalf("info line missing:\n%s", m.View())
	}
	if sel := m.views[mapTab].Selection(); sel != nil {
		t.Fatalf("Selection = %v, want nil", sel)
	}
	before := len(r.fake.CallsSnapshot())
	for _, k := range "tomn" {
		m = r.press(m, keyRune(k))
		wantStatus(t, m, map[rune]string{
			't': "jump: worker not in roster", 'o': "open PR: worker not in roster",
			'm': "reply: worker not in roster", 'n': "nudge: worker not in roster",
		}[k])
		if !m.statusErr {
			t.Fatalf("key %q: refusal not flagged as an error", k)
		}
	}
	if got := len(r.fake.CallsSnapshot()); got != before {
		t.Fatalf("%d runner calls for actions on a planet without a worker, want none", got-before)
	}
}

func TestMapReapedWorkerResolves(t *testing.T) {
	r := newMapRig(t, false, true)
	r.snap.Roster.Crews[0].Workers[16-1-6]["state"] = "reaped"
	m := r.selectBranch(r.open(), "feat/106-grove")
	if w, _ := m.views[mapTab].Selection().(data.Worker); w.Name != "grove" || w.State != "reaped" {
		t.Fatalf("Selection = %+v, want the reaped grove", w)
	}
	if v := m.View(); !strings.Contains(v, "grove · feat/106-grove · reaped") {
		t.Fatalf("info line missing:\n%s", v)
	}
	m = r.press(m, keyRune('n'))
	if !strings.Contains(m.View(), "nudge grove? y/N") {
		t.Fatalf("a reaped worker should still be actionable:\n%s", m.View())
	}
}

func TestMapSnapshotRendersOnlyWhenTheCrewChanged(t *testing.T) {
	changed := func(mutate func(*data.RosterSection)) func(*testing.T) {
		return func(t *testing.T) {
			r := newMapRig(t, false, true)
			m := r.open()
			before := renders(r.fake)
			m = r.press(m, rosterMsgOf(mutate))
			if got := renders(r.fake) - before; got != 1 {
				t.Fatalf("%d renders, want exactly 1", got)
			}
			// The same roster again is not a change.
			after := m.snap.Roster
			r.press(m, rosterMsgOf(func(ro *data.RosterSection) { *ro = after }))
			if got := renders(r.fake) - before; got != 1 {
				t.Fatalf("an identical snapshot rendered again (%d total)", got)
			}
		}
	}
	unchanged := func(src source, apply func(*data.Snapshot)) func(*testing.T) {
		return func(t *testing.T) {
			r := newMapRig(t, false, true)
			m := r.open()
			before := renders(r.fake)
			r.press(m, sourceMsg{src, apply})
			if got := renders(r.fake) - before; got != 0 {
				t.Fatalf("%d renders, want none", got)
			}
		}
	}
	worker := func(ro *data.RosterSection, branch string) map[string]any {
		for _, w := range ro.Crews[0].Workers {
			if w["branch"] == branch {
				return w
			}
		}
		t.Fatalf("no worker %s", branch)
		return nil
	}

	t.Run("state change", changed(func(ro *data.RosterSection) { worker(ro, "feat/104-ember")["state"] = "blocked" }))
	t.Run("PR change", changed(func(ro *data.RosterSection) { worker(ro, "feat/104-ember")["pr_url"] = "https://github.com/o/r/pull/9" }))
	t.Run("rename", changed(func(ro *data.RosterSection) { worker(ro, "feat/104-ember")["name"] = "cinder" }))
	t.Run("host change", changed(func(ro *data.RosterSection) { worker(ro, "feat/104-ember")["host"] = "mini" }))
	t.Run("worker leaves", changed(func(ro *data.RosterSection) { ro.Crews[0].Workers = ro.Crews[0].Workers[1:] }))

	t.Run("same roster", unchanged(srcRoster, func(s *data.Snapshot) {}))
	t.Run("timestamps and detail", unchanged(srcRoster, func(s *data.Snapshot) {
		for _, w := range s.Roster.Crews[0].Workers {
			w["ts"] = w["ts"].(float64) + 60000
			w["age_s"] = 5.0
			w["detail"] = "something else"
		}
	}))
	t.Run("budget only", unchanged(srcBudget, func(s *data.Snapshot) { s.Budget.Warnings = []string{"x"} }))
	t.Run("other crew", unchanged(srcRoster, func(s *data.Snapshot) { s.Roster.Crews[1].Workers[0]["state"] = "blocked" }))
}

func TestMapInactiveTabRendersOnEnter(t *testing.T) {
	r := newMapRig(t, false, true)
	m := r.press(r.open(), keyRune('1'))
	before := renders(r.fake)
	m = r.press(m, rosterMsgOf(func(ro *data.RosterSection) { ro.Crews[0].Workers[0]["state"] = "blocked" }))
	if got := renders(r.fake) - before; got != 0 {
		t.Fatalf("%d renders while the tab is hidden, want none", got)
	}
	m = r.press(m, keyRune('5'))
	if got := renders(r.fake) - before; got != 1 {
		t.Fatalf("%d renders on enter, want 1", got)
	}
	if !m.views[mapTab].(mapView).stored {
		t.Fatalf("nothing shown after entering")
	}
}

func TestMapNewPlanetKeepsSelectionByBranch(t *testing.T) {
	r := newMapRig(t, false, true)
	m := r.open()
	g := loadMapGeometry(t)
	g.Planets = append([]mapgeom.Planet{{Branch: "feat/099-zero", Name: "zero", CX: 40, CY: 40, R: 26}}, g.Planets...)
	r.respond("c1", "1000x500", g)
	m = r.press(m, rosterMsgOf(func(ro *data.RosterSection) {
		ro.Crews[0].Workers = append(ro.Crews[0].Workers, map[string]any{"branch": "feat/099-zero", "name": "zero", "state": "working", "session": "s1"})
	}))
	mv := m.views[mapTab].(mapView)
	if len(mv.geom.Planets) != 17 || mv.sel != "feat/101-basil" || mv.selIndex() != 2 {
		t.Fatalf("planets %d sel %q index %d, want 17 planets and basil now at slot 2", len(mv.geom.Planets), mv.sel, mv.selIndex())
	}
}

func TestMapSelectionFallsBackWhenItsPlanetLeaves(t *testing.T) {
	r := newMapRig(t, false, true)
	m := r.open()
	g := loadMapGeometry(t)
	g.Planets = slices.DeleteFunc(g.Planets, func(p mapgeom.Planet) bool { return p.Branch == "feat/101-basil" })
	r.respond("c1", "1000x500", g)
	// The render the roster change starts still carries the old selection.
	r.fake.Responses["crew roster-render --map --crew c1 --px 1000x500 --out "+mapOut+" --select feat/101-basil"] = r.fake.Responses["crew roster-render --map --crew c1 --px 1000x500 --out "+mapOut]
	m = r.press(m, rosterMsgOf(func(ro *data.RosterSection) {
		ro.Crews[0].Workers = slices.DeleteFunc(ro.Crews[0].Workers, func(w map[string]any) bool { return w["branch"] == "feat/101-basil" })
	}))
	if got, want := mapSel(m), g.Planets[0].Branch; got != want {
		t.Fatalf("sel = %q, want the first planet %q", got, want)
	}
	calls := argvs(r.fake)
	if last := calls[len(calls)-1]; last[len(last)-1] != g.Planets[0].Branch {
		t.Fatalf("the ring was not redrawn on the new selection: %q", last)
	}
}

func TestMapStaleRenderIsDropped(t *testing.T) {
	r := newMapRig(t, false, true)
	m := r.open()
	mv := m.views[mapTab].(mapView)
	written := r.tty.Len()
	one := mapgeom.Geometry{V: 1, W: 10, H: 10, Planets: []mapgeom.Planet{{Branch: "feat/x", R: 1, CX: 1, CY: 1}}}
	for _, msg := range []mapRenderedMsg{
		{gen: mv.gen - 1, crewID: "c1", place: mv.place, geom: one},
		{gen: mv.gen + 1, crewID: "c1", place: mv.place, geom: one},
		{gen: mv.gen - 1, crewID: "c1", err: fmt.Errorf("boom")},
	} {
		nm, cmd := m.Update(msg)
		if cmd != nil {
			t.Fatalf("a stale render produced a Cmd")
		}
		got := nm.(Model).views[mapTab].(mapView)
		if len(got.geom.Planets) != 16 || got.err != "" || got.curID != mv.curID {
			t.Fatalf("a stale render changed the view: planets %d err %q id %d", len(got.geom.Planets), got.err, got.curID)
		}
	}
	if r.tty.Len() != written {
		t.Fatalf("a stale render wrote to the terminal")
	}
}

func TestMapRenderErrorShowsMessage(t *testing.T) {
	asciiProfile(t)
	for name, resp := range map[string]data.FakeResponse{
		"command failed": {Stderr: []byte("crew: roster-render: neato not found\n"), Err: data.FakeExitError{Code: 1}},
		"bad output":     ok("not json"),
	} {
		t.Run(name, func(t *testing.T) {
			r := newMapRig(t, false, true)
			r.fake.Responses = map[string]data.FakeResponse{}
			for _, px := range mapPx {
				r.fake.Responses["crew roster-render --map --crew c1 --px "+px+" --out "+mapOut+" --select feat/101-basil"] = resp
			}
			m := r.open()
			frame := m.View()
			if !strings.Contains(frame, "render failed") || !strings.Contains(frame, "Press 1") {
				t.Fatalf("no failure message:\n%s", frame)
			}
			if name == "command failed" && !strings.Contains(frame, "neato not found") {
				t.Fatalf("the reason is missing:\n%s", frame)
			}
			assertNoImage(t, frame)
			if m.views[mapTab].(mapView).ownsTab() {
				t.Fatalf("a failed map owns tab")
			}
			if bytes.Contains(r.tty.Bytes(), []byte("a=T")) {
				t.Fatalf("an image was stored for a failed render")
			}
			m = r.press(m, keyType(tea.KeyTab))
			if m.active != 0 {
				t.Fatalf("tab did not switch views from the failed map: active %d", m.active)
			}
		})
	}
}

func assertNoImage(t *testing.T, frame string) {
	t.Helper()
	if strings.Contains(frame, "\x1b") || strings.Contains(frame, placeholder) {
		t.Fatalf("frame carries escape sequences or placeholder cells:\n%q", frame)
	}
}

func TestMapFallbackWhenNotCapable(t *testing.T) {
	asciiProfile(t)
	r := newMapRig(t, false, false)
	m := r.open()
	if m.active != mapTab {
		t.Fatalf("the tab should still open")
	}
	frame := m.View()
	for _, want := range []string{"needs kitty or ghostty", "allow-passthrough on", "Press 1 for the Live table"} {
		if !strings.Contains(frame, want) {
			t.Fatalf("fallback lacks %q:\n%s", want, frame)
		}
	}
	assertNoImage(t, frame)
	assertNoImage(t, m.views[mapTab].View(100, 24))
	assertWidth(t, frame, 100, 30)
	if got := len(r.fake.CallsSnapshot()); got != 0 {
		t.Fatalf("%d runner calls while not capable", got)
	}
	if r.tty.Len() != 0 || r.count("tea.enableMouseCellMotionMsg") != 0 {
		t.Fatalf("a terminal that cannot show images was written to or put in mouse mode")
	}
	if len(m.views[mapTab].Keys()) != 0 {
		t.Fatalf("the fallback advertises map keys")
	}
	m = r.press(m, keyType(tea.KeyTab))
	if m.active != 0 {
		t.Fatalf("tab did not switch views: active %d", m.active)
	}
	m = r.press(m, keyRune('5'))
	m = r.press(m, keyType(tea.KeyShiftTab))
	if m.active != 3 {
		t.Fatalf("shift+tab did not switch views: active %d", m.active)
	}
}

func TestMapFrameHasPlaceholdersOfExactWidth(t *testing.T) {
	r := newMapRig(t, false, true)
	m := r.open()
	frame := m.View()
	assertWidth(t, frame, 100, 30)
	lines := strings.Split(frame, "\n")
	var block []string
	for _, l := range lines {
		if strings.Contains(l, placeholder) {
			block = append(block, l)
		}
	}
	if len(block) != 25 {
		t.Fatalf("%d placeholder lines, want 25", len(block))
	}
	for i, l := range block {
		if got := ansi.StringWidth(l); got != 100 {
			t.Fatalf("placeholder line %d is %d cells, want 100", i, got)
		}
		if got := strings.Count(l, placeholder); got != 100 {
			t.Fatalf("placeholder line %d has %d cells, want 100", i, got)
		}
	}
	if !strings.Contains(frame, "basil · feat/101-basil · blocked") {
		t.Fatalf("info line missing:\n%s", frame)
	}
	if !strings.Contains(frame, "crew c1 · 4 working · 4 blocked · 4 pr open · 4 done") {
		t.Fatalf("counts line missing:\n%s", frame)
	}
}

func TestMapBlockIsCentredAndClicksFollowIt(t *testing.T) {
	r := newMapRig(t, false, true)
	m := r.model(400, 30)
	r.respond("c1", "2970x500", func() mapgeom.Geometry {
		g := loadMapGeometry(t)
		g.W = 2970
		for i := range g.Planets {
			g.Planets[i].CX *= 2.97
		}
		return g
	}())
	m = r.press(m, keyRune('5'))
	mv := m.views[mapTab].(mapView)
	if mv.place.Cols != 297 {
		t.Fatalf("cols = %d, want the 297 cap", mv.place.Cols)
	}
	left, _ := mv.origin()
	if left != (400-297)/2 {
		t.Fatalf("left = %d, want the block centred", left)
	}
	for _, l := range strings.Split(m.View(), "\n") {
		if strings.Contains(l, placeholder) {
			if !strings.HasPrefix(l, strings.Repeat(" ", left)+"\x1b[38;2;") {
				t.Fatalf("block line is not indented by %d", left)
			}
			break
		}
	}
	g := mv.geom
	i := 7
	x := left + int(g.Planets[i].CX/(float64(g.W)/float64(mv.place.Cols)))
	y := int(g.Planets[i].CY/20) + 1
	m = r.press(m, click(x, y))
	if mapSel(m) != g.Planets[i].Branch {
		t.Fatalf("click on the centred block selected %q, want %q", mapSel(m), g.Planets[i].Branch)
	}
}

var storeRe = regexp.MustCompile(`\x1b_Gi=(\d+),a=T,U=1,q=2,f=100,c=(\d+),r=(\d+),t=f;`)

func TestMapTransmitsAlternatingIDsAndDeletesOnLeave(t *testing.T) {
	r := newMapRig(t, false, true)
	m := r.open()
	if got := r.count("tea.clearScreenMsg"); got != 1 {
		t.Fatalf("%d repaints after the first transmit, want 1", got)
	}
	m = r.press(m, keyRune('l'))
	m = r.press(m, keyRune('l'))
	if got := r.count("tea.clearScreenMsg"); got != 1 {
		t.Fatalf("%d repaints after three transmits, want only the first", got)
	}

	out := r.tty.String()
	stores := storeRe.FindAllStringSubmatch(out, -1)
	if len(stores) != 3 {
		t.Fatalf("%d stores recorded, want 3: %q", len(stores), out)
	}
	for i, wantID := range []int{mapBase + 1, mapBase + 2, mapBase + 1} {
		if got, _ := strconv.Atoi(stores[i][1]); got != wantID {
			t.Fatalf("store %d used id %d, want %d", i, got, wantID)
		}
	}
	// The id about to be reused is deleted before its new image is stored.
	del := kitty.Delete(mapBase+1, false)
	store := strings.Index(out, fmt.Sprintf("\x1b_Gi=%d,", mapBase+1))
	if !strings.HasPrefix(out[store-len(del):], del) {
		t.Fatalf("the first store is not preceded by a delete of its id: %q", out)
	}
	last := strings.LastIndex(out, fmt.Sprintf("\x1b_Gi=%d,", mapBase+1))
	if !strings.HasPrefix(out[last-len(del):], del) {
		t.Fatalf("the reused id was not deleted first: %q", out)
	}

	// The store and the frame's placeholder block agree on cols and rows.
	frame := m.View()
	rows, cols := 0, 0
	for _, l := range strings.Split(frame, "\n") {
		if n := strings.Count(l, placeholder); n > 0 {
			rows++
			cols = n
		}
	}
	if c, _ := strconv.Atoi(stores[2][2]); c != cols {
		t.Fatalf("stored cols %d, placeholder cols %d", c, cols)
	}
	if rw, _ := strconv.Atoi(stores[2][3]); rw != rows {
		t.Fatalf("stored rows %d, placeholder rows %d", rw, rows)
	}
	if want := fmt.Sprintf("\x1b[38;2;%d;%d;%dm", 0, (mapBase+1)>>8&0xff, (mapBase+1)&0xff); !strings.Contains(frame, want) {
		t.Fatalf("frame does not reference the id of the last store: want %q", want)
	}

	if got := r.count("tea.enableMouseCellMotionMsg"); got != 1 {
		t.Fatalf("%d mouse enables, want 1", got)
	}
	mark := r.tty.Len()
	m = r.press(m, keyRune('1'))
	tail := r.tty.String()[mark:]
	if want := kitty.Delete(mapBase+1, false) + kitty.Delete(mapBase+2, false); tail != want {
		t.Fatalf("leaving wrote %q, want deletes of both ids", tail)
	}
	if got := r.count("tea.disableMouseMsg"); got != 1 {
		t.Fatalf("%d mouse disables, want 1", got)
	}
	if strings.Contains(m.View(), placeholder) {
		t.Fatalf("placeholders remain after leaving")
	}

	// Re-entering stores again and repaints once more.
	r.press(m, keyRune('5'))
	if got := r.count("tea.clearScreenMsg"); got != 2 {
		t.Fatalf("%d repaints after re-entering, want 2", got)
	}
}

func TestMapPassthroughUnderTmux(t *testing.T) {
	r := newMapRig(t, true, true)
	r.open()
	if !strings.HasPrefix(r.tty.String(), "\x1bPtmux;") {
		t.Fatalf("under tmux the store must be wrapped for passthrough: %q", r.tty.String())
	}
}

func TestMapTabKeyMovesSelectionWhenOwnedElseSwitchesViews(t *testing.T) {
	r := newMapRig(t, false, true)
	m := r.press(r.open(), keyType(tea.KeyTab))
	if m.active != mapTab || mapSel(m) != "feat/102-coral" {
		t.Fatalf("owned tab: active %d sel %q", m.active, mapSel(m))
	}
	m = r.press(m, keyType(tea.KeyShiftTab))
	if m.active != mapTab || mapSel(m) != "feat/101-basil" {
		t.Fatalf("owned shift+tab: active %d sel %q", m.active, mapSel(m))
	}

	// Before the first image lands the tab does not own tab.
	loading := r.model(100, 30)
	nm, _ := loading.Update(keyRune('5'))
	nm, _ = nm.Update(keyType(tea.KeyTab))
	if nm.(Model).active != 0 {
		t.Fatalf("tab while rendering: active %d, want a switch to view 0", nm.(Model).active)
	}
}

func TestMapTabBarShowsFiveTabs(t *testing.T) {
	asciiProfile(t)
	m := sized(testModel(loadFullSnapshot(t)), 80, 24)
	head := strings.Split(m.View(), "\n")[0]
	if got := strings.Fields(strings.ReplaceAll(strings.ReplaceAll(head, "│", " "), "▸", " ")); !slices.Equal(got, tabTitles) || len(tabTitles) != 5 {
		t.Fatalf("tab bar = %q", head)
	}
}

func TestMapResizeRendersWhenTheBlockChanges(t *testing.T) {
	r := newMapRig(t, false, true)
	m := r.open()
	before := renders(r.fake)
	m = r.press(m, tea.WindowSizeMsg{Width: 100, Height: 30})
	if renders(r.fake) != before {
		t.Fatalf("an unchanged size rendered again")
	}
	m = r.press(m, tea.WindowSizeMsg{Width: 80, Height: 24})
	if got := renders(r.fake) - before; got != 1 {
		t.Fatalf("%d renders after a resize, want 1", got)
	}
	calls := argvs(r.fake)
	if last := calls[len(calls)-1]; !slices.Contains(last, "800x380") {
		t.Fatalf("resize render did not ask for the new size: %q", last)
	}
	assertWidth(t, m.View(), 80, 24)
}

func TestMapHelpToggleKeepsFrameWithinTerminal(t *testing.T) {
	r := newMapRig(t, false, true)
	m := r.press(r.open(), keyRune('?'))
	assertWidth(t, m.View(), 100, 30)
	m = r.press(m, keyRune('?'))
	assertWidth(t, m.View(), 100, 30)
}

func TestMapStatusLineDoesNotResizeTheImage(t *testing.T) {
	r := newMapRig(t, true, true)
	m := r.open()
	before := renders(r.fake)
	m = r.press(m, keyRune('t'))
	if m.status == "" {
		t.Fatalf("no status shown")
	}
	if renders(r.fake) != before {
		t.Fatalf("a status line re-rendered the map")
	}
	assertWidth(t, m.View(), 100, 30)
}

func goldenMap(t *testing.T, prefix string, capable bool) {
	t.Helper()
	asciiProfile(t)
	for _, sz := range [][2]int{{80, 24}, {160, 40}} {
		r := newMapRig(t, false, capable)
		m := r.press(r.model(sz[0], sz[1]), keyRune('5'))
		checkGolden(t, fmt.Sprintf("%s_%dx%d.golden", prefix, sz[0], sz[1]), m.View(), sz[0], sz[1])
	}
}

func TestGoldenMapFallback(t *testing.T) { goldenMap(t, "map_fallback", false) }

func TestGoldenMap(t *testing.T) { goldenMap(t, "map", true) }

func TestMapRenderErrorRetriesOnTheNextSnapshotOfTheSameRoster(t *testing.T) {
	r := newMapRig(t, false, true)
	fail := data.FakeResponse{Stderr: []byte("crew: roster-render: neato not found\n"), Err: data.FakeExitError{Code: 1}}
	for _, px := range mapPx {
		r.fake.Responses["crew roster-render --map --crew c1 --px "+px+" --out "+mapOut+" --select feat/101-basil"] = fail
	}
	m := r.open()
	if m.views[mapTab].(mapView).err == "" {
		t.Fatalf("the render did not fail")
	}
	before := renders(r.fake)
	r.press(m, sourceMsg{srcRoster, func(s *data.Snapshot) {}})
	if got := renders(r.fake) - before; got != 1 {
		t.Fatalf("%d renders after a snapshot of the same roster, want 1 retry", got)
	}
}

// textLines is a frame without its placeholder lines, for failure messages.
func textLines(frame string) string {
	var out []string
	for _, l := range strings.Split(frame, "\n") {
		if !strings.Contains(l, placeholder) {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

// footer is the help bar line of a frame.
func footer(m Model) string {
	lines := strings.Split(m.View(), "\n")
	return lines[len(lines)-1]
}

func TestMapHelpAdvertisesTabAsSelectionOnlyWhileItOwnsTab(t *testing.T) {
	r := newMapRig(t, false, true)
	bar := footer(r.press(r.model(160, 40), keyRune('5')))
	if strings.Contains(bar, "next view") || !strings.Contains(bar, "←/→/tab select") {
		t.Fatalf("owning map footer: %q", bar)
	}
	for _, ownsTab := range []bool{false, true} {
		var helps []string
		for _, row := range (helpKeyMap{global: DefaultKeyMap(), ownsTab: ownsTab}).FullHelp() {
			for _, b := range row {
				helps = append(helps, b.Help().Desc)
			}
		}
		if got := slices.Contains(helps, "next view") || slices.Contains(helps, "prev view"); got == ownsTab {
			t.Fatalf("ownsTab %v: full help %q", ownsTab, helps)
		}
	}

	loading := r.model(160, 40)
	nm, _ := loading.Update(keyRune('5'))
	if bar := footer(nm.(Model)); !strings.Contains(bar, "tab next view") {
		t.Fatalf("map before its image lands lost %q: %q", "tab next view", bar)
	} else if strings.Contains(bar, "←/→/tab") || !strings.Contains(bar, "←/→ select") {
		t.Fatalf("loading map footer advertises tab as selection: %q", bar)
	}

	off := newMapRig(t, false, false)
	if bar := footer(off.press(off.model(160, 40), keyRune('5'))); !strings.Contains(bar, "tab next view") {
		t.Fatalf("fallback footer lost %q: %q", "tab next view", bar)
	} else if strings.Contains(bar, "←/→/tab") {
		t.Fatalf("fallback footer advertises tab as selection: %q", bar)
	}
}

func TestMapShrinkBeforeTheNewRenderLandsKeepsInfoLinesAndWidth(t *testing.T) {
	for name, prompt := range map[string]bool{"info line": false, "reply prompt": true} {
		t.Run(name, func(t *testing.T) {
			r := newMapRig(t, false, true)
			m := r.open()
			if prompt {
				m = r.press(m, keyRune('m'))
			}
			// The new render is started but not delivered.
			nm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
			m = nm.(Model)
			frame := m.View()
			assertWidth(t, frame, 80, 24)
			want := "basil · feat/101-basil · blocked"
			if prompt {
				want = "reply to basil: "
			}
			if !strings.Contains(frame, want) {
				t.Fatalf("%q missing after the shrink:\n%s", want, textLines(frame))
			}
			if !strings.Contains(frame, "crew c1 · 4 working") {
				t.Fatalf("counts line missing after the shrink:\n%s", textLines(frame))
			}
			for i, l := range strings.Split(frame, "\n") {
				if strings.Contains(l, placeholder) && (strings.Contains(l, "…") || strings.Count(l, placeholder) > 80) {
					t.Fatalf("line %d overflows the image block: %d cells, truncated %v", i, strings.Count(l, placeholder), strings.Contains(l, "…"))
				}
			}
		})
	}
}

// After a shrink the stored block is larger than what View shows; a click on a
// cell View does not draw must not pick the planet hidden there.
func TestMapClickOutsideTheShownBlockAfterAShrinkIsIgnored(t *testing.T) {
	r := newMapRig(t, false, true)
	m := r.open()
	g := loadMapGeometry(t)
	// The new render is started but not delivered. The box is now 80x21, so
	// 19 body rows of the 100x25 block show: body row 19 is the info line.
	nm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = nm.(Model)
	before := renders(r.fake)
	for name, planet := range map[string]int{
		"info line row":     6, // cell 78,20 in root rows
		"counts line row":   7,
		"past the last col": 3, // cell 86,9
	} {
		x, y := cellOf(g.Planets[planet].CX, g.Planets[planet].CY)
		got := r.press(m, click(x, y))
		if mapSel(got) != "feat/101-basil" {
			t.Fatalf("%s: click on hidden planet %d selected %q", name, planet, mapSel(got))
		}
	}
	if renders(r.fake) != before {
		t.Fatalf("a click outside the shown block rendered")
	}
	x, y := cellOf(g.Planets[0].CX, g.Planets[0].CY)
	if got := mapSel(r.press(m, click(x, y))); got != "feat/100-amber" {
		t.Fatalf("a click inside the shown block selected %q, want planet 0", got)
	}
}
