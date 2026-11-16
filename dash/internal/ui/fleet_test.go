package ui

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/noamsto/dispatcher/dash/internal/data"
	vm "github.com/noamsto/dispatcher/dash/internal/view"
	"github.com/noamsto/dispatcher/dash/internal/view/viewtest"
)

func fleetModel(t *testing.T, calls *atomic.Int32) Model {
	t.Helper()
	deps := Deps{Now: viewtest.NowMS / 1000, Load: Loaders{
		Fleet: func(context.Context) vm.FleetInputs {
			calls.Add(1)
			return viewtest.FleetInputs()
		},
	}}
	m := NewModel(deps, fixedNow(loadSnapshot(t)), liveDeps{})
	m.active = liveTab
	return sized(m, 120, 40)
}

// fleetLoads counts the fleet loads cmd would start, by running every leaf.
func fleetLoads(cmd tea.Cmd) int {
	n := 0
	for _, msg := range flattenFleet(cmd) {
		if sm, ok := msg.(sourceMsg); ok && sm.src == srcFleet {
			n++
		}
	}
	return n
}

// flattenFleet runs cmd's leaves except the tick timers, which would block.
func flattenFleet(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	var out []tea.Msg
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	select {
	case msg := <-done:
		if batch, ok := msg.(tea.BatchMsg); ok {
			for _, c := range batch {
				out = append(out, flattenFleet(c)...)
			}
			return out
		}
		return append(out, msg)
	case <-time.After(50 * time.Millisecond):
		return nil
	}
}

func TestFleetTabLazyLoad(t *testing.T) {
	var calls atomic.Int32
	m := fleetModel(t, &calls)

	if got := fleetLoads(m.Init()); got != 0 || calls.Load() != 0 {
		t.Fatalf("Init requested fleet: loads=%d calls=%d", got, calls.Load())
	}
	if m.load[srcFleet].inFlight {
		t.Fatal("fleet marked in flight before activation")
	}
	if got := m.loadStatus(); got == "" || strings.Contains(got, "fleet") {
		t.Fatalf("footer lists fleet before activation: %q", got)
	}

	nm, cmd := m.Update(keyRune('8'))
	m = nm.(Model)
	if m.active != fleetTab {
		t.Fatalf("active = %d, want fleetTab", m.active)
	}
	if got := fleetLoads(cmd); got != 1 {
		t.Fatalf("switch to Fleet requested fleet %d times, want 1", got)
	}
	if !m.load[srcFleet].inFlight {
		t.Fatal("fleet not in flight after activation")
	}

	// away and back while the first load is in flight: no second load.
	nm, _ = m.Update(keyRune('2'))
	nm, cmd = nm.(Model).Update(keyRune('8'))
	m = nm.(Model)
	if got := fleetLoads(cmd); got != 0 {
		t.Fatalf("re-entry during flight started %d loads", got)
	}

	// land the result
	in := viewtest.FleetInputs()
	nm, _ = m.Update(sourceMsg{src: srcFleet, apply: func(*data.Snapshot) {}, fleet: &in})
	m = nm.(Model)
	if !m.load[srcFleet].loaded {
		t.Fatal("fleet not loaded after landing")
	}
	if m.views[fleetTab].(fleetView).fv.Summary.Crews == 0 {
		t.Fatal("fleet view not built from the landed inputs")
	}

	// R on Fleet refreshes it again
	nm, cmd = m.Update(keyRune('R'))
	m = nm.(Model)
	if got := fleetLoads(cmd); got != 1 {
		t.Fatalf("R requested fleet %d times, want 1", got)
	}
	if calls.Load() != 2 {
		t.Fatalf("loader calls = %d, want 2", calls.Load())
	}
}

func TestRefreshAllSkipsUnopenedFleet(t *testing.T) {
	var calls atomic.Int32
	m := fleetModel(t, &calls)
	for i := range m.load {
		m.load[i].inFlight = false
	}
	if got := fleetLoads(m.requestAll()); got != 0 {
		t.Fatalf("R before the Fleet tab opened requested fleet %d times", got)
	}
}

func TestFleetTabTick(t *testing.T) {
	var calls atomic.Int32
	m := fleetModel(t, &calls)
	nm, _ := m.Update(keyRune('8'))
	m = nm.(Model)
	gen := m.fleetGen
	m.load[srcFleet].inFlight = false

	nm, cmd := m.Update(fleetTickMsg{gen: gen})
	m = nm.(Model)
	if cmd == nil {
		t.Fatal("tick while Fleet active scheduled nothing")
	}
	if !m.load[srcFleet].inFlight {
		t.Fatal("tick did not request fleet")
	}

	// stale generation: dropped
	if _, cmd := m.Update(fleetTickMsg{gen: gen - 1}); cmd != nil {
		t.Fatal("stale tick rescheduled")
	}

	// another tab active: dropped, nothing requested
	m.active = liveTab
	m.load[srcFleet].inFlight = false
	nm, cmd = m.Update(fleetTickMsg{gen: gen})
	if cmd != nil || nm.(Model).load[srcFleet].inFlight {
		t.Fatal("tick off the Fleet tab scheduled work")
	}
}

func TestDigit8RoutesToFleet(t *testing.T) {
	if tabIndex("8") != fleetTab {
		t.Fatalf("tabIndex(8) = %d, want %d", tabIndex("8"), fleetTab)
	}
	if tabTitles[fleetTab] != "Fleet" {
		t.Fatalf("title = %q", tabTitles[fleetTab])
	}
}

func fleetNow() time.Time { return time.UnixMilli(viewtest.FleetNowMS) }

// fleetSnap is the snapshot the root broadcasts once in's fleet load landed.
func fleetSnap(in vm.FleetInputs) snapshotMsg {
	var loaded [numSources]bool
	loaded[srcFleet] = true
	return snapshotMsg{fleet: vm.BuildFleet(in), fleetIn: in, loaded: loaded}
}

// flFixture is the fixture Fleet tab at w x h wired to a fake runner, which
// records every call (with its Dir) and fails the ones with no response.
func flFixture(t *testing.T, w, h int, inTmux bool) (fleetView, *data.FakeRunner) {
	t.Helper()
	asciiProfile(t)
	fr := &data.FakeRunner{Responses: map[string]data.FakeResponse{}}
	v := newFleetView(fleetSnap(viewtest.FleetInputs()), fleetNow)
	v.acts = actionDeps{ctx: context.Background(), runner: fr, crew: []string{"crew"}, inTmux: inTmux}
	v.View(w, h)
	return v, fr
}

// keyOf is a named key, or typed text for anything else.
func keyOf(k string) tea.KeyMsg {
	switch k {
	case "tab":
		return keyType(tea.KeyTab)
	case "shift+tab":
		return tea.KeyMsg{Type: tea.KeyShiftTab}
	case "enter":
		return keyType(tea.KeyEnter)
	case "esc":
		return keyType(tea.KeyEsc)
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
}

func flKeys(v fleetView, keys ...string) (fleetView, tea.Cmd) {
	var cmd tea.Cmd
	for _, k := range keys {
		var nv view
		nv, cmd = v.Update(keyOf(k))
		v = nv.(fleetView)
	}
	return v, cmd
}

// selectCrew focuses the Crews panel with the crew row key chosen.
func selectCrew(t *testing.T, v *fleetView, key string) {
	t.Helper()
	if !slices.ContainsFunc(v.fv.Crews, func(c vm.FleetCrewRow) bool { return c.Key == key }) {
		t.Fatalf("no crew %s", key)
	}
	v.st.focus, v.st.crew = fpCrews, key
}

func selCrewKey(v fleetView) string {
	if s := v.sel(); s.crew >= 0 {
		return v.fv.Crews[s.crew].Key
	}
	return ""
}

// attnIndex is the first attention item of this kind.
func attnIndex(t *testing.T, fv vm.FleetView, kind vm.Kind) int {
	t.Helper()
	i := slices.IndexFunc(fv.Attention, func(a vm.AttentionItem) bool { return a.Kind == kind })
	if i < 0 {
		t.Fatalf("no %s item", kind)
	}
	return i
}

// digitFor is the digit key of attention item i in the strip as drawn.
func digitFor(t *testing.T, v fleetView, i int) string {
	t.Helper()
	for n, it := range v.layout().panels[fpAttention].items {
		if it.idx == i {
			return strconv.Itoa(n + 1)
		}
	}
	t.Fatalf("item %d not shown in the strip", i)
	return ""
}

func breadcrumb(v fleetView, w, h int) string {
	return strings.TrimSpace(stripped(strings.Split(v.View(w, h), "\n")[0]))
}

const (
	keyA1 = "lap|/r/a/.git|c1"
	keyA3 = "lap|/r/a/.git|c3"
	keyB1 = "lap|/r/b/.git|c1"
	keyB5 = "lap|/r/b/.git|c5"
)

func TestFleetFocusCycle(t *testing.T) {
	v, _ := flFixture(t, 120, 40, false)
	want := []fleetPanel{fpCrews, fpCapacity, fpHolds, fpHosts, fpAttention, fpCrews}
	for i, p := range want {
		v, _ = flKeys(v, "tab")
		if v.st.focus != p {
			t.Fatalf("tab %d: focus = %v, want %v", i+1, v.st.focus, p)
		}
	}
	v, _ = flKeys(v, "shift+tab", "shift+tab")
	if v.st.focus != fpHosts {
		t.Errorf("shift+tab x2: focus = %v, want hosts", v.st.focus)
	}

	e, _ := flFixture(t, 120, 40, false)
	e.fv.Attention = nil
	for range 6 {
		e, _ = flKeys(e, "tab")
		if e.st.focus == fpAttention {
			t.Fatal("focus reached the absent strip")
		}
	}
}

func TestFleetMove(t *testing.T) {
	v, _ := flFixture(t, 120, 40, false)
	v, _ = flKeys(v, "j")
	if v.sel().cursor != 1 {
		t.Errorf("attention j: cursor = %d", v.sel().cursor)
	}

	vis := newFleetIndex(v.fv, "").visible()
	selectCrew(t, &v, v.fv.Crews[vis[0]].Key)
	v, _ = flKeys(v, "j")
	if got := selCrewKey(v); got != v.fv.Crews[vis[1]].Key {
		t.Errorf("crews j: %s, want %s", got, v.fv.Crews[vis[1]].Key)
	}
	v, _ = flKeys(v, "k", "k")
	if got := selCrewKey(v); got != v.fv.Crews[vis[0]].Key {
		t.Errorf("crews k k: %s, want the first row (clamped)", got)
	}

	v, _ = flKeys(v, "tab", "tab", "j") // holds
	if v.st.hold != v.fv.Holds[1].ID {
		t.Errorf("holds j: %q, want %q", v.st.hold, v.fv.Holds[1].ID)
	}
	v, _ = flKeys(v, "tab", "j") // hosts
	if v.st.host != v.fv.Hosts[1].Name {
		t.Errorf("hosts j: %q, want %q", v.st.host, v.fv.Hosts[1].Name)
	}
}

func TestFleetDigitsFollowScroll(t *testing.T) {
	v, fr := flFixture(t, 100, 30, false)
	if r := v.layout(); r.stripRows != 4 || len(v.fv.Attention) != 9 {
		t.Fatalf("strip shows %d of %d, want 4 of 9", r.stripRows, len(v.fv.Attention))
	}
	v, _ = flKeys(v, "j", "j")
	if r := v.layout(); r.stripOff != 2 {
		t.Fatalf("stripOff = %d, want 2", r.stripOff)
	}
	want := v.fv.Attention[2].Action.Argv
	_, cmd := flKeys(v, "1")
	statusText(cmd)
	calls := fr.CallsSnapshot()
	if len(calls) != 1 || !slices.Equal(calls[0].Args, want[1:]) || calls[0].Dir != "/r/a" {
		t.Errorf("calls = %+v, want %v in /r/a (item 3)", calls, want)
	}
}

func TestFleetDigitActionDir(t *testing.T) {
	v, fr := flFixture(t, 120, 40, false)
	run := func(kind vm.Kind, then ...string) {
		t.Helper()
		var cmd tea.Cmd
		v, cmd = flKeys(v, append([]string{digitFor(t, v, attnIndex(t, v.fv, kind))}, then...)...)
		statusText(cmd)
	}
	run(vm.KindMergeReady)
	if v.mode != ovConfirm || !strings.Contains(stripped(v.View(120, 40)), "merge PR #12 (feat/b)? y/N") {
		t.Fatalf("merge digit did not open its confirm (mode %v)", v.mode)
	}
	v, cmd := flKeys(v, "y")
	statusText(cmd)
	run(vm.KindRemoteWatchDead)
	run(vm.KindBlocked, "hello", "enter")

	want := []struct {
		dir  string
		args []string
	}{
		{"/r/a", []string{"merge", "12", "--crew", "c1"}},
		{"/r/a", []string{"where", "--crew", "c1", "--", "feat/far"}},
		{"/r/b", []string{"reply", "--crew", "c5", "--", "worker:feat/c5", "hello"}},
	}
	calls := fr.CallsSnapshot()
	if len(calls) != len(want) {
		t.Fatalf("calls = %+v", calls)
	}
	for i, w := range want {
		if calls[i].Dir != w.dir || !slices.Equal(calls[i].Args, w.args) {
			t.Errorf("call %d = %+v, want %v in %s", i, calls[i], w.args, w.dir)
		}
	}
}

func TestFleetInspect(t *testing.T) {
	v, fr := flFixture(t, 120, 40, false)
	for _, k := range []vm.Kind{vm.KindHoldMatured, vm.KindLaneStale} {
		i := attnIndex(t, v.fv, k)
		_, cmd := flKeys(v, digitFor(t, v, i))
		got := status(t, cmd)
		if !strings.HasPrefix(got, "run: ") || !strings.Contains(got, strings.Join(v.fv.Attention[i].Action.Argv, " ")) {
			t.Errorf("%s: status = %q", k, got)
		}
	}
	noCalls(t, fr)
}

func TestFleetPeerRefused(t *testing.T) {
	v, fr := flFixture(t, 120, 40, false)
	_, cmd := flKeys(v, digitFor(t, v, attnIndex(t, v.fv, vm.KindPeerStale)))
	if got := status(t, cmd); got != "peer worker: read-only" {
		t.Errorf("peer item: status = %q", got)
	}
	_, cmd = flKeys(v, digitFor(t, v, attnIndex(t, v.fv, vm.KindRelayLost)))
	if got := status(t, cmd); got != "no action" {
		t.Errorf("relay_lost item: status = %q", got)
	}
	noCalls(t, fr)

	s, _ := flFixture(t, 100, 30, false)
	if _, cmd = flKeys(s, "5"); statusText(cmd) != "no attention item 5" {
		t.Errorf("digit past the strip: %q", statusText(cmd))
	}
}

// drilledBody is the Overview the drill-down must show for key under a
// one-line breadcrumb in a w x h body.
func drilledBody(t *testing.T, key string, w, h int) []string {
	t.Helper()
	in, ok := vm.CrewInputs(viewtest.FleetInputs(), key)
	if !ok {
		t.Fatalf("no crew inputs for %s", key)
	}
	return normalizeFrame(newOverviewView(in, fleetNow).View(w, h-1), h-1, w)
}

func TestFleetDrillRoundTrip(t *testing.T) {
	v, _ := flFixture(t, 120, 40, false)
	selectCrew(t, &v, keyB5)
	v, _ = flKeys(v, "enter")
	if !v.drilled {
		t.Fatal("enter on a crew row did not drill")
	}
	frame := v.View(120, 40)
	assertWidth(t, frame, 120, 40)
	lines := strings.Split(frame, "\n")
	if got := breadcrumb(v, 120, 40); got != "fleet › b c5" {
		t.Errorf("breadcrumb = %q", got)
	}
	if want := drilledBody(t, keyB5, 120, 40); !slices.Equal(lines[1:], want) {
		t.Errorf("drilled body differs from the crew's Overview:\n%s\nwant:\n%s", strings.Join(lines[1:], "\n"), strings.Join(want, "\n"))
	}
	if !v.ownsKey(keyOf("r")) || !v.ownsKey(keyOf("1")) {
		t.Error("drilled Fleet does not hand r and digits to its Overview")
	}

	// The Overview's own keys: its filter, then esc only clears that.
	v, _ = flKeys(v, "/", "zz")
	if !v.Capturing() || v.drill.st.filter != "zz" {
		t.Fatalf("drilled / did not open the Overview filter (filter %q)", v.drill.st.filter)
	}
	v, _ = flKeys(v, "esc")
	if !v.drilled || v.drill.st.filter != "" || v.Capturing() {
		t.Fatalf("esc in the filter: drilled %v filter %q", v.drilled, v.drill.st.filter)
	}
	v, _ = flKeys(v, "?")
	if v.drill.mode != ovHelp || !strings.Contains(stripped(v.View(120, 40)), "HELP") {
		t.Fatal("drilled ? did not open the Overview help")
	}
	v, _ = flKeys(v, "esc")
	if !v.drilled || v.drill.mode != ovNormal {
		t.Fatal("esc on the help overlay left the drill-down")
	}

	v, _ = flKeys(v, "esc")
	if v.drilled {
		t.Fatal("esc with nothing open did not return to the panels")
	}
	if got := selCrewKey(v); got != keyB5 {
		t.Errorf("selected crew after return = %s, want %s", got, keyB5)
	}
	if v.ownsKey(keyOf("r")) {
		t.Error("the panels own r")
	}
}

func TestFleetDrillFromAttention(t *testing.T) {
	v, _ := flFixture(t, 120, 40, false)
	v.setCursor(attnIndex(t, v.fv, vm.KindBlocked))
	v, _ = flKeys(v, "enter")
	if !v.drilled || v.drillCrew != keyB5 {
		t.Fatalf("enter on the strip: drilled %v crew %q", v.drilled, v.drillCrew)
	}
	if got := breadcrumb(v, 120, 40); got != "fleet › b c5" {
		t.Errorf("breadcrumb = %q", got)
	}

	p, _ := flFixture(t, 120, 40, false)
	selectCrew(t, &p, "mbp||p1")
	p, _ = flKeys(p, "enter")
	if got := breadcrumb(p, 120, 40); got != "fleet › @mbp p1" {
		t.Errorf("peer breadcrumb = %q", got)
	}
}

func TestFleetDrillActionDir(t *testing.T) {
	v, fr := flFixture(t, 150, 40, false)
	selectCrew(t, &v, keyB1)
	v, _ = flKeys(v, "enter")
	if !v.drilled {
		t.Fatal("no drill")
	}
	v.View(150, 40)
	steps := map[string][]string{"p": {"p"}, "n": {"n", "y"}, "m": {"m", "y"}, "r": {"r", "hi", "enter"}}
	for _, k := range []string{"p", "n", "m", "r"} {
		wi := slices.IndexFunc(v.drill.model.Workers, func(w vm.OverviewWorker) bool {
			return slices.ContainsFunc(w.Actions, func(a vm.Action) bool { return a.Key == k })
		})
		if wi < 0 {
			t.Fatalf("no drilled worker with action %s", k)
		}
		selectBranch(t, &v.drill, v.drill.model.Workers[wi].Branch)
		var cmd tea.Cmd
		v, cmd = flKeys(v, steps[k]...)
		statusText(cmd)
	}
	calls := fr.CallsSnapshot()
	if len(calls) != 4 {
		t.Fatalf("calls = %+v", calls)
	}
	for _, c := range calls {
		if c.Dir != "/r/b" {
			t.Errorf("%v ran in %q, want /r/b", c.Args, c.Dir)
		}
	}
}

func TestFleetDrillRefresh(t *testing.T) {
	v, _ := flFixture(t, 120, 40, false)
	selectCrew(t, &v, keyA1)
	v, _ = flKeys(v, "enter", "/", "feat", "enter")
	before := v.drill.model.GeneratedAt

	in := viewtest.FleetInputs()
	in.NowMS += 60_000
	nv, cmd := v.Update(fleetSnap(in))
	v = nv.(fleetView)
	if cmd != nil || !v.drilled {
		t.Fatal("refresh left the drill-down")
	}
	if v.drill.model.GeneratedAt != before+60 {
		t.Errorf("drilled Overview not rebuilt: generated_at %d, was %d", v.drill.model.GeneratedAt, before)
	}
	if v.drill.st.filter != "feat" {
		t.Errorf("refresh dropped the Overview's filter: %q", v.drill.st.filter)
	}

	gone := viewtest.FleetInputs()
	gone.Fleet = data.FleetSection{}
	nv, cmd = v.Update(fleetSnap(gone))
	v = nv.(fleetView)
	if v.drilled {
		t.Fatal("drill-down kept a crew that vanished")
	}
	if got := status(t, cmd); !strings.Contains(got, "c1") {
		t.Errorf("status = %q", got)
	}
}

func TestFleetRelaunchConfirm(t *testing.T) {
	v, fr := flFixture(t, 120, 40, true)
	selectCrew(t, &v, keyA3)
	v, _ = flKeys(v, "L")
	if v.mode != ovConfirm || !v.Capturing() {
		t.Fatalf("L on the orphaned crew did not confirm (mode %v)", v.mode)
	}
	frame := v.View(120, 40)
	assertWidth(t, frame, 120, 40)
	if !strings.Contains(stripped(frame), "relaunch crew c3 (a)? y/N") {
		t.Errorf("relaunch prompt not shown:\n%s", stripped(frame))
	}
	n, cmd := flKeys(v, "n")
	if got := status(t, cmd); got != "relaunch: cancelled" || n.Capturing() {
		t.Errorf("n: status %q", got)
	}
	noCalls(t, fr)

	_, cmd = flKeys(v, "y")
	statusText(cmd)
	calls := fr.CallsSnapshot()
	want := []string{"--relaunch", "c3", "--repo", "/r/a"}
	if len(calls) != 1 || calls[0].Name != "dispatcher" || !slices.Equal(calls[0].Args, want) {
		t.Fatalf("calls = %+v, want dispatcher %v", calls, want)
	}

	// Outside tmux: refused, nothing runs.
	out, fr2 := flFixture(t, 120, 40, false)
	selectCrew(t, &out, keyA3)
	_, cmd = flKeys(out, "L", "y")
	if got := status(t, cmd); got != "relaunch needs tmux" {
		t.Errorf("outside tmux: status %q", got)
	}
	noCalls(t, fr2)

	// The dispatcher_gone digit opens the same confirm.
	d, _ := flFixture(t, 120, 40, true)
	d, _ = flKeys(d, digitFor(t, d, attnIndex(t, d.fv, vm.KindDispatcherGone)))
	if d.mode != ovConfirm || d.pend.prompt != "relaunch crew c3 (a)? y/N" {
		t.Errorf("dispatcher_gone digit: mode %v prompt %q", d.mode, d.pend.prompt)
	}

	for _, key := range []string{keyA1, "mbp||p1"} {
		o, fr3 := flFixture(t, 120, 40, true)
		selectCrew(t, &o, key)
		o, cmd = flKeys(o, "L")
		if got := status(t, cmd); got != "no relaunch for this crew" || o.mode != ovNormal {
			t.Errorf("L on %s: status %q mode %v", key, got, o.mode)
		}
		noCalls(t, fr3)
	}
}

func TestFleetFilter(t *testing.T) {
	v, _ := flFixture(t, 120, 40, false)
	v, _ = flKeys(v, "tab", "/")
	if !v.Capturing() {
		t.Fatal("/ did not open the filter")
	}
	v, _ = flKeys(v, "p2")
	if n := len(newFleetIndex(v.fv, v.st.filter).visible()); n != 2 {
		t.Fatalf("filter p2 shows %d crews, want 2", n)
	}
	v, _ = flKeys(v, "enter")
	if v.Capturing() || v.st.filter != "p2" {
		t.Fatalf("enter: capturing %v filter %q", v.Capturing(), v.st.filter)
	}
	if !strings.Contains(stripped(v.View(120, 40)), "2 of 8 crews") {
		t.Error("filtered count not shown")
	}
	for _, needle := range []string{"box", "b", "c5"} {
		if len(newFleetIndex(v.fv, needle).visible()) == 0 {
			t.Errorf("filter %q (host/repo/crew) matched nothing", needle)
		}
	}
	v, _ = flKeys(v, "esc")
	if v.st.filter != "" {
		t.Errorf("esc kept the filter %q", v.st.filter)
	}
	v, _ = flKeys(v, "/", "zz", "esc")
	if v.st.filter != "" || v.Capturing() {
		t.Errorf("esc in the filter: %q", v.st.filter)
	}
}

func flClick(v fleetView, x, y int) fleetView {
	nv, _ := v.Update(tea.MouseMsg{X: x, Y: y, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	return nv.(fleetView)
}

// lastCrewRow is the last crew row placed in the Crews panel.
func lastCrewRow(r fleetRects) placed {
	var out placed
	for _, it := range r.panels[fpCrews].items {
		if it.kind == eCrew {
			out = it
		}
	}
	return out
}

func TestFleetMouse(t *testing.T) {
	v, _ := flFixture(t, 120, 40, false)
	r := fleetLayout(120, 40, v.fv, v.st)

	crew := lastCrewRow(r)
	cv := flClick(v, crew.rect.x+1, crew.rect.y)
	if cv.st.focus != fpCrews || selCrewKey(cv) != v.fv.Crews[crew.idx].Key {
		t.Errorf("crew click: focus %v crew %s", cv.st.focus, selCrewKey(cv))
	}

	hold := r.panels[fpHolds].items[1]
	cv = flClick(v, hold.rect.x, hold.rect.y)
	if cv.st.focus != fpHolds || cv.st.hold != v.fv.Holds[hold.idx].ID {
		t.Errorf("hold click: focus %v hold %q", cv.st.focus, cv.st.hold)
	}

	hp := r.panels[fpHosts]
	cv = flClick(v, hp.rect.x+hp.rect.w-1, hp.rect.y)
	if cv.st.focus != fpHosts {
		t.Errorf("hosts border click: focus %v", cv.st.focus)
	}

	row := r.panels[fpAttention].items[4]
	cv = flClick(v, row.rect.x, row.rect.y)
	if cv.st.focus != fpAttention || cv.sel().cursor != row.idx {
		t.Errorf("strip click: focus %v cursor %d", cv.st.focus, cv.sel().cursor)
	}

	hv, _ := flKeys(v, "?")
	if cv = flClick(hv, crew.rect.x, crew.rect.y); cv.st != hv.st {
		t.Error("click under the help overlay changed state")
	}

	// Through the root: the tab bar's rows come off Y.
	var calls atomic.Int32
	m := fleetModel(t, &calls)
	nm, _ := m.Update(keyRune('8'))
	in := viewtest.FleetInputs()
	nm, _ = nm.Update(sourceMsg{src: srcFleet, apply: func(*data.Snapshot) {}, fleet: &in})
	m = nm.(Model)
	_ = m.View()
	fl := m.views[fleetTab].(fleetView)
	last := lastCrewRow(fl.layout())
	nm, _ = m.Update(tea.MouseMsg{X: last.rect.x + 1, Y: last.rect.y + len(m.headerLines()), Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	got := nm.(Model).views[fleetTab].(fleetView)
	if selCrewKey(got) != fl.fv.Crews[last.idx].Key {
		t.Errorf("root click selected %s, want %s", selCrewKey(got), fl.fv.Crews[last.idx].Key)
	}
}

func TestFleetErrorsShown(t *testing.T) {
	v, _ := flFixture(t, 120, 40, false)
	v.fv.Errors = []string{"fleet status: exit 2", "digest /r/b: unreadable"}
	frame := v.View(120, 40)
	assertWidth(t, frame, 120, 40)
	lines := strings.Split(stripped(frame), "\n")
	if got := strings.TrimSpace(lines[len(lines)-1]); got != "! 2 source errors: fleet status: exit 2" {
		t.Errorf("errors line = %q", got)
	}
	hv, _ := flKeys(v, "?")
	help := stripped(hv.View(120, 40))
	for _, e := range v.fv.Errors {
		if !strings.Contains(help, e) {
			t.Errorf("help overlay misses %q", e)
		}
	}
}

func TestFleetRootKeys(t *testing.T) {
	var calls atomic.Int32
	m := fleetModel(t, &calls)
	nm, _ := m.Update(keyRune('8'))
	in := viewtest.FleetInputs()
	nm, _ = nm.Update(sourceMsg{src: srcFleet, apply: func(*data.Snapshot) {}, fleet: &in})
	m = nm.(Model)

	nm, _ = m.Update(keyType(tea.KeyTab))
	if nm.(Model).active != fleetTab || nm.(Model).views[fleetTab].(fleetView).st.focus != fpCrews {
		t.Error("tab left the Fleet tab instead of moving focus")
	}
	nm, cmd := m.Update(keyRune('7'))
	if nm.(Model).active != fleetTab {
		t.Error("digit 7 switched tabs")
	}
	statusText(cmd)
	if nm, _ = m.Update(keyRune('[')); nm.(Model).active != mapTab {
		t.Error("[ did not switch to the previous tab")
	}
	if nm, _ = m.Update(keyRune(']')); nm.(Model).active != overviewTab {
		t.Error("] did not wrap to Overview")
	}
	m.load[srcFleet].inFlight = false
	if _, cmd = m.Update(keyRune('R')); fleetLoads(cmd) != 1 {
		t.Error("R did not refresh the fleet")
	}
	_, cmd = m.Update(keyRune('q'))
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Error("q did not quit")
	}
}

type chanWatcher struct{ ch chan struct{} }

func (w chanWatcher) Events() <-chan struct{} { return w.ch }
func (w chanWatcher) Close() error            { close(w.ch); return nil }

func hasFleetChange(msgs []tea.Msg) bool {
	return slices.ContainsFunc(msgs, func(msg tea.Msg) bool { _, ok := msg.(fleetChangedMsg); return ok })
}

func TestFleetPollRequestsOnlyWhenActive(t *testing.T) {
	var calls atomic.Int32
	w := chanWatcher{ch: make(chan struct{}, 1)}
	deps := Deps{Now: viewtest.NowMS / 1000, Load: Loaders{
		Fleet: func(context.Context) vm.FleetInputs {
			calls.Add(1)
			return viewtest.FleetInputs()
		},
	}}
	m := sized(NewModel(deps, fixedNow(loadSnapshot(t)), liveDeps{fleetWatcher: w}), 120, 40)
	m.active = liveTab

	w.ch <- struct{}{}
	if !hasFleetChange(flattenFleet(m.Init())) {
		t.Fatal("Init does not wait on the fleet watcher")
	}

	// Off the Fleet tab: re-armed, nothing loaded.
	nm, cmd := m.Update(fleetChangedMsg{})
	m = nm.(Model)
	if cmd == nil || m.load[srcFleet].inFlight {
		t.Fatalf("change off the Fleet tab: cmd %v, fleet in flight %v", cmd != nil, m.load[srcFleet].inFlight)
	}

	nm, _ = m.Update(keyRune('8'))
	m = nm.(Model)
	m.load[srcFleet].inFlight = false
	nm, cmd = m.Update(fleetChangedMsg{})
	m = nm.(Model)
	if !m.load[srcFleet].inFlight {
		t.Fatal("change on the Fleet tab did not request the fleet")
	}
	w.ch <- struct{}{}
	if !hasFleetChange(flattenFleet(cmd)) {
		t.Error("change on the Fleet tab did not re-arm the watcher")
	}

	// A closed watcher ends the wait with no message.
	_ = w.Close()
	if msg := waitForFleetChange(w)(); msg != nil {
		t.Errorf("closed watcher produced %v", msg)
	}
}

func TestFleetDrilledKeysEscHint(t *testing.T) {
	m := fleetSelectCrew(t, sized(fleetRootModel(t, viewtest.FleetInputs()), 120, 40), keyB5)
	m = sweepKeys(m, keyType(tea.KeyEnter))
	hasEsc := func(m Model) bool {
		return slices.ContainsFunc(m.views[fleetTab].Keys(), func(b key.Binding) bool { return b.Help().Desc == "fleet" })
	}
	if !hasEsc(m) {
		t.Fatal("drilled Overview with no filter: no esc -> fleet hint")
	}
	m = sweepKeys(m, keyRune('/'), keyRune('x'), keyType(tea.KeyEnter))
	if hasEsc(m) {
		t.Error("drilled Overview with a kept filter still hints esc -> fleet")
	}
	m = sweepKeys(m, keyType(tea.KeyEsc))
	if !hasEsc(m) {
		t.Error("after esc cleared the filter the esc -> fleet hint is missing")
	}
}

func TestFleetChangeCooldown(t *testing.T) {
	var calls atomic.Int32
	deps := Deps{Now: viewtest.NowMS / 1000, Load: Loaders{
		Fleet: func(context.Context) vm.FleetInputs { calls.Add(1); return viewtest.FleetInputs() },
	}}
	m := sized(NewModel(deps, fixedNow(loadSnapshot(t)), liveDeps{}), 120, 40)
	m.active = liveTab
	nm, _ := m.Update(keyRune('8'))
	in := viewtest.FleetInputs()
	nm, _ = nm.Update(sourceMsg{src: srcFleet, apply: func(*data.Snapshot) {}, fleet: &in})
	m = nm.(Model)

	// Inside the cooldown: no load, one deferred settle however many changes.
	for range 3 {
		nm, _ = m.Update(fleetChangedMsg{})
		m = nm.(Model)
	}
	if m.load[srcFleet].inFlight {
		t.Fatal("change inside the cooldown loaded the fleet")
	}
	if !m.fleetSettle {
		t.Fatal("change inside the cooldown scheduled no deferred load")
	}

	// The deferred settle then loads once.
	nm, cmd := m.Update(fleetSettleMsg{})
	m = nm.(Model)
	if m.fleetSettle || !m.load[srcFleet].inFlight || cmd == nil {
		t.Fatalf("settle: pending %v, in flight %v", m.fleetSettle, m.load[srcFleet].inFlight)
	}

	// Past the cooldown a change loads at once.
	m.load[srcFleet].inFlight = false
	m.load[srcFleet].at = m.now().Add(-fleetCooldown)
	nm, _ = m.Update(fleetChangedMsg{})
	if !nm.(Model).load[srcFleet].inFlight {
		t.Error("change after the cooldown did not load")
	}
}
