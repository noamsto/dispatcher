package ui

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/noamsto/dispatcher/dash/internal/data"
	vm "github.com/noamsto/dispatcher/dash/internal/view"
	"github.com/noamsto/dispatcher/dash/internal/view/viewtest"
)

// ovFixture is the fixture Overview at w x h wired to a fake runner.
func ovFixture(t *testing.T, w, h int, inTmux bool, resp map[string]data.FakeResponse) (overviewView, *data.FakeRunner) {
	t.Helper()
	asciiProfile(t)
	fr := &data.FakeRunner{Responses: resp}
	v := newOverviewView(viewtest.Inputs(), overviewNow)
	v.acts = actionDeps{ctx: context.Background(), runner: fr, crew: []string{"crew"}, inTmux: inTmux}
	v.View(w, h)
	return v, fr
}

func ovPress(v overviewView, k tea.KeyMsg) (overviewView, tea.Cmd) {
	nv, cmd := v.Update(k)
	return nv.(overviewView), cmd
}

// ovKeys presses each key in turn: a named key, or typed text for anything else.
func ovKeys(v overviewView, keys ...string) (overviewView, tea.Cmd) {
	var cmd tea.Cmd
	for _, k := range keys {
		switch k {
		case "tab":
			v, cmd = ovPress(v, keyType(tea.KeyTab))
		case "shift+tab":
			v, cmd = ovPress(v, tea.KeyMsg{Type: tea.KeyShiftTab})
		case "enter":
			v, cmd = ovPress(v, keyType(tea.KeyEnter))
		case "esc":
			v, cmd = ovPress(v, keyType(tea.KeyEsc))
		case "left":
			v, cmd = ovPress(v, keyType(tea.KeyLeft))
		case "right":
			v, cmd = ovPress(v, keyType(tea.KeyRight))
		default:
			v, cmd = ovPress(v, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)})
		}
	}
	return v, cmd
}

// status runs cmd and returns the status line text it produces.
func status(t *testing.T, cmd tea.Cmd) string {
	t.Helper()
	if cmd == nil {
		return ""
	}
	am, ok := cmd().(actionMsg)
	if !ok {
		t.Fatalf("command did not produce an actionMsg")
	}
	return am.text
}

// selectBranch selects the worker on branch from the checklists, moving focus
// there first when the strip has it, since the strip selects its cursor's
// worker.
func selectBranch(t *testing.T, v *overviewView, branch string) {
	t.Helper()
	for i, w := range v.model.Workers {
		if w.Branch == branch {
			if v.st.focus == panelAttention {
				v.setFocus(panelChecklists)
			}
			v.setWorker(i)
			return
		}
	}
	t.Fatalf("no worker %s", branch)
}

func selBranch(v overviewView) string {
	w, _ := v.target()
	return w.Branch
}

func noCalls(t *testing.T, fr *data.FakeRunner) {
	t.Helper()
	if calls := fr.CallsSnapshot(); len(calls) != 0 {
		t.Errorf("runner was called: %v", calls)
	}
}

func stripped(s string) string { return ansi.Strip(s) }

func TestOverviewFocusCycle(t *testing.T) {
	v, _ := ovFixture(t, 150, 40, false, nil)
	want := []ovPanel{panelChecklists, panelAgents, panelMonitors, panelAttention, panelChecklists}
	for i, p := range want {
		v, _ = ovKeys(v, "tab")
		if v.st.focus != p {
			t.Fatalf("tab %d: focus = %v, want %v", i+1, v.st.focus, p)
		}
	}
	v, _ = ovKeys(v, "shift+tab")
	if v.st.focus != panelAttention {
		t.Errorf("shift+tab: focus = %v, want attention", v.st.focus)
	}
	v, _ = ovKeys(v, "shift+tab")
	if v.st.focus != panelMonitors {
		t.Errorf("shift+tab: focus = %v, want monitors", v.st.focus)
	}
}

func TestOverviewFocusSkipsEmptyStrip(t *testing.T) {
	asciiProfile(t)
	v := newOverviewView(allClearInputs(), overviewNow)
	v.View(100, 30)
	for range 4 {
		v, _ = ovKeys(v, "tab")
		if v.st.focus == panelAttention {
			t.Fatalf("focus reached the absent strip")
		}
	}
}

func TestOverviewMovePerPanel(t *testing.T) {
	v, _ := ovFixture(t, 150, 40, false, nil)
	// Attention: j moves the cursor and selects that item's worker.
	v, _ = ovKeys(v, "j")
	if v.sel().cursor != 1 || selBranch(v) != "feat/g" {
		t.Errorf("attention j: cursor=%d sel=%s", v.sel().cursor, selBranch(v))
	}
	// Checklists: j walks the worker order.
	v, _ = ovKeys(v, "tab")
	selectBranch(t, &v, "feat/a")
	v, _ = ovKeys(v, "j")
	if selBranch(v) != "feat/b" {
		t.Errorf("checklists j: sel=%s, want feat/b", selBranch(v))
	}
	v, _ = ovKeys(v, "k")
	if selBranch(v) != "feat/a" {
		t.Errorf("checklists k: sel=%s, want feat/a", selBranch(v))
	}
	// Agents: the same shared selection moves over agent rows.
	v, _ = ovKeys(v, "tab", "j")
	if selBranch(v) != "feat/b" {
		t.Errorf("agents j: sel=%s, want feat/b", selBranch(v))
	}
	// Monitors: j moves over rows that name a worker.
	v, _ = ovKeys(v, "tab", "j")
	if selBranch(v) == "" {
		t.Errorf("monitors j lost the selection")
	}
}

func TestOverviewSelectionShared(t *testing.T) {
	v, _ := ovFixture(t, 150, 40, false, nil)
	v, _ = ovKeys(v, "j", "j") // attention item 3: role_failed on feat/a
	if selBranch(v) != "feat/a" {
		t.Fatalf("attention item 3 selects %s, want feat/a", selBranch(v))
	}
	r := overviewLayout(150, 40, v.model, v.st)
	for _, p := range []ovPanel{panelChecklists, panelAgents} {
		sel := 0
		for _, it := range r.panels[p].items {
			if it.worker >= 0 && v.model.Workers[it.worker].Branch == "feat/a" {
				sel++
			}
		}
		if sel == 0 {
			t.Errorf("panel %d does not show the selected worker", p)
		}
	}
	var marked int
	for _, l := range strings.Split(stripped(v.View(150, 40)), "\n") {
		if strings.Contains(l, "▌") {
			marked++
		}
	}
	if marked < 3 {
		t.Errorf("selected worker is highlighted on %d lines, want it in every panel", marked)
	}
}

func TestDigitsFollowScroll(t *testing.T) {
	v, fr := ovFixture(t, 100, 30, false, map[string]data.FakeResponse{
		"crew where --crew c1 -- feat/a": {Stdout: []byte("otter here\n")},
	})
	if got := overviewLayout(100, 30, v.model, v.st).stripRows; got != 4 || len(v.model.Attention) != 12 {
		t.Fatalf("strip shows %d of %d, want 4 of 12", got, len(v.model.Attention))
	}
	v, _ = ovKeys(v, "j", "j")
	if r := overviewLayout(100, 30, v.model, v.st); r.stripOff != 2 {
		t.Fatalf("stripOff = %d, want 2", r.stripOff)
	}
	_, cmd := ovKeys(v, "1")
	if got := status(t, cmd); got != "peek: otter here" {
		t.Errorf("status = %q", got)
	}
	calls := fr.CallsSnapshot()
	if len(calls) != 1 || calls[0].Name != "crew" || !slices.Equal(calls[0].Args, []string{"where", "--crew", "c1", "--", "feat/a"}) {
		t.Errorf("calls = %v, want one crew where on feat/a (item 3)", calls)
	}
}

func TestDigitNoItem(t *testing.T) {
	v, fr := ovFixture(t, 100, 30, false, nil)
	_, cmd := ovKeys(v, "5")
	if got := status(t, cmd); got != "no attention item 5" {
		t.Errorf("status = %q", got)
	}
	noCalls(t, fr)
}

const laneStatusJSON = `{"lane":"dead","pid":42,"lag_sec":120}`

func laneFixture(t *testing.T) (overviewView, *data.FakeRunner) {
	t.Helper()
	return ovFixture(t, 150, 40, false, map[string]data.FakeResponse{
		"crew stream --crew c2 --status": {Stdout: []byte(laneStatusJSON)},
	})
}

// settle runs cmd and feeds its inspectMsg back to v.
func settle(t *testing.T, v overviewView, cmd tea.Cmd) overviewView {
	t.Helper()
	if cmd == nil {
		t.Fatal("no command")
	}
	msg, ok := cmd().(inspectMsg)
	if !ok {
		t.Fatalf("command did not produce an inspectMsg")
	}
	nv, _ := v.Update(msg)
	return nv.(overviewView)
}

func assertLaneOverlay(t *testing.T, v overviewView, fr *data.FakeRunner) {
	t.Helper()
	got := stripped(v.View(150, 40))
	for _, w := range []string{"DETAIL", "lane: dead", "pid: 42", "lag_sec: 120", "crew stream --crew c2 --status"} {
		if !strings.Contains(got, w) {
			t.Errorf("overlay lacks %q:\n%s", w, got)
		}
	}
	if strings.Contains(got, `{"lane"`) {
		t.Errorf("overlay shows raw JSON:\n%s", got)
	}
	calls := fr.CallsSnapshot()
	if len(calls) != 1 || calls[0].Name != "crew" || !slices.Equal(calls[0].Args, []string{"stream", "--crew", "c2", "--status"}) {
		t.Errorf("calls = %v", calls)
	}
}

func TestInspectDigitOpensOverlay(t *testing.T) {
	v, fr := laneFixture(t) // item 4 is lane_dead on c2
	v, cmd := ovKeys(v, "4")
	if got := stripped(v.View(150, 40)); !strings.Contains(got, "running") {
		t.Errorf("no running placeholder:\n%s", got)
	}
	v = settle(t, v, cmd)
	assertLaneOverlay(t, v, fr)
	v, _ = ovKeys(v, "esc")
	if v.mode != ovNormal || strings.Contains(stripped(v.View(150, 40)), "pid: 42") {
		t.Errorf("esc did not close the overlay")
	}
}

func TestInspectEnterOpensOverlay(t *testing.T) {
	v, fr := laneFixture(t)
	v, _ = ovKeys(v, "j", "j", "j")
	v, cmd := ovKeys(v, "enter")
	v = settle(t, v, cmd)
	assertLaneOverlay(t, v, fr)
	if got := stripped(v.View(150, 40)); strings.Contains(got, "run: crew stream") {
		t.Errorf("status still echoes the command:\n%s", got)
	}
	v, _ = ovKeys(v, "enter")
	if v.mode != ovNormal {
		t.Errorf("enter did not close the overlay")
	}
}

func TestInspectStaleResultIgnored(t *testing.T) {
	v, _ := laneFixture(t)
	v, cmd := ovKeys(v, "4")
	v, _ = ovKeys(v, "esc") // closed before the result arrives
	v = settle(t, v, cmd)
	if v.mode != ovNormal || v.insp.open || strings.Contains(stripped(v.View(150, 40)), "pid: 42") {
		t.Errorf("a late result reopened the overlay")
	}

	// A result for another item does not touch the open overlay.
	v, _ = ovKeys(v, "4")
	before := v.insp
	nv, _ := v.Update(inspectMsg{id: "other", out: `{"x":1}`})
	if nv.(overviewView).insp != before {
		t.Errorf("a result for another item changed the overlay: %+v", nv.(overviewView).insp)
	}
}

func TestInspectRunsInItemDir(t *testing.T) {
	asciiProfile(t)
	dir := "/r/x"
	in := viewtest.Inputs()
	in.Dir = &dir
	fr := &data.FakeRunner{}
	v := newOverviewView(in, overviewNow)
	v.acts = actionDeps{ctx: context.Background(), runner: fr, crew: []string{"crew"}}
	v.View(150, 40)
	_, cmd := ovKeys(v, "4")
	cmd()
	if calls := fr.CallsSnapshot(); len(calls) != 1 || calls[0].Dir != dir {
		t.Errorf("calls = %+v, want one in %q", calls, dir)
	}
}

func TestInspectControlRunsNothing(t *testing.T) {
	v, fr := ovFixture(t, 150, 40, false, nil)
	v = poisonActions(v, false, true)
	for _, k := range []string{"4", "enter"} {
		nv, cmd := ovKeys(v, k)
		if cmd != nil {
			cmd()
		}
		if nv.insp.open {
			t.Errorf("%s opened an inspect overlay for a control-byte argv", k)
		}
	}
	noCalls(t, fr)
}

func TestInspectErrorShown(t *testing.T) {
	v, _ := ovFixture(t, 150, 40, false, map[string]data.FakeResponse{
		"crew stream --crew c2 --status": {Stderr: []byte("boom\n"), Err: errors.New("exit 1")},
	})
	v, cmd := ovKeys(v, "4")
	v = settle(t, v, cmd)
	if got := stripped(v.View(150, 40)); !strings.Contains(got, "error:") {
		t.Errorf("no error shown:\n%s", got)
	}
}

func TestMergeConfirm(t *testing.T) {
	resp := map[string]data.FakeResponse{"crew merge 12 --crew c1": {Stdout: []byte("merged\n")}}
	v, fr := ovFixture(t, 150, 40, false, resp)
	selectBranch(t, &v, "feat/b")
	v, _ = ovKeys(v, "m")
	if v.mode != ovConfirm || !v.Capturing() {
		t.Fatalf("m did not open the confirm (mode %v)", v.mode)
	}
	if !strings.Contains(stripped(v.View(150, 40)), "merge PR #12 (feat/b)? y/N") {
		t.Errorf("confirm prompt not shown")
	}
	v2, cmd := ovKeys(v, "n")
	if got := status(t, cmd); got != "merge: cancelled" || v2.Capturing() {
		t.Errorf("cancel: status %q capturing %v", got, v2.Capturing())
	}
	noCalls(t, fr)

	v3, cmd := ovKeys(v, "y")
	if got := status(t, cmd); got != "merge: merged" || v3.Capturing() {
		t.Errorf("confirm: status %q", got)
	}
	calls := fr.CallsSnapshot()
	if len(calls) != 1 || !slices.Equal(calls[0].Args, []string{"merge", "12", "--crew", "c1"}) {
		t.Errorf("calls = %v", calls)
	}
}

func TestMergeNoPR(t *testing.T) {
	v, fr := ovFixture(t, 150, 40, false, nil)
	selectBranch(t, &v, "feat/a")
	_, cmd := ovKeys(v, "m")
	if got := status(t, cmd); got != "merge: no PR" {
		t.Errorf("status = %q", got)
	}
	noCalls(t, fr)
}

func TestReply(t *testing.T) {
	resp := map[string]data.FakeResponse{"crew reply --crew c1 -- worker:feat/c hi there": {Stdout: []byte("sent\n")}}
	v, fr := ovFixture(t, 150, 40, false, resp)
	selectBranch(t, &v, "feat/c")
	v, _ = ovKeys(v, "r")
	if v.mode != ovReply || !v.Capturing() {
		t.Fatalf("r did not open the reply input")
	}
	if !strings.Contains(stripped(v.View(150, 40)), "reply to feat/c: ") {
		t.Errorf("reply prompt not shown")
	}
	_, cmd := ovKeys(v, "enter")
	if got := status(t, cmd); got != "reply: text required" {
		t.Errorf("empty reply status = %q", got)
	}
	noCalls(t, fr)

	v, _ = ovKeys(v, "hi there")
	v, cmd = ovKeys(v, "enter")
	if got := status(t, cmd); got != "reply: sent" || v.Capturing() {
		t.Errorf("status = %q", got)
	}
	calls := fr.CallsSnapshot()
	want := []string{"reply", "--crew", "c1", "--", "worker:feat/c", "hi there"}
	if len(calls) != 1 || !slices.Equal(calls[0].Args, want) {
		t.Errorf("calls = %v, want args %v", calls, want)
	}
}

func TestReplyOverviewEscCancels(t *testing.T) {
	v, fr := ovFixture(t, 150, 40, false, nil)
	selectBranch(t, &v, "feat/c")
	v, _ = ovKeys(v, "r", "x", "esc")
	if v.mode != ovNormal {
		t.Errorf("esc left mode %v", v.mode)
	}
	noCalls(t, fr)
}

func TestNudgeConfirm(t *testing.T) {
	resp := map[string]data.FakeResponse{"crew nudge --crew c1 -- feat/e": {Stdout: []byte("nudged\n")}}
	v, fr := ovFixture(t, 150, 40, false, resp)
	selectBranch(t, &v, "feat/e")
	v, _ = ovKeys(v, "n")
	if !strings.Contains(stripped(v.View(150, 40)), "nudge feat/e? y/N") {
		t.Errorf("nudge prompt not shown")
	}
	v2, cmd := ovKeys(v, "x")
	if got := status(t, cmd); got != "nudge: cancelled" || v2.Capturing() {
		t.Errorf("cancel status = %q", got)
	}
	noCalls(t, fr)
	_, cmd = ovKeys(v, "y")
	if got := status(t, cmd); got != "nudge: nudged" {
		t.Errorf("status = %q", got)
	}
	calls := fr.CallsSnapshot()
	if len(calls) != 1 || !slices.Equal(calls[0].Args, []string{"nudge", "--crew", "c1", "--", "feat/e"}) {
		t.Errorf("calls = %v", calls)
	}
}

func TestPeek(t *testing.T) {
	resp := map[string]data.FakeResponse{
		"crew where --crew c1 -- feat/a": {Stdout: []byte("otter  jump: ! tmux switch-client -t %5\n")},
		"tmux switch-client -t %5":       {Stdout: []byte("")},
		"crew where --crew c1 -- feat/r": {Stdout: []byte("feat/r on halo\n")},
	}
	t.Run("local in tmux switches", func(t *testing.T) {
		v, fr := ovFixture(t, 150, 40, true, resp)
		selectBranch(t, &v, "feat/a")
		_, cmd := ovKeys(v, "p")
		if got := status(t, cmd); got != "jump: ok" {
			t.Errorf("status = %q", got)
		}
		calls := fr.CallsSnapshot()
		if len(calls) != 2 || calls[0].Args[0] != "where" || calls[1].Name != "tmux" ||
			!slices.Equal(calls[1].Args, []string{"switch-client", "-t", "%5"}) {
			t.Errorf("calls = %v", calls)
		}
	})
	t.Run("local outside tmux shows where", func(t *testing.T) {
		v, fr := ovFixture(t, 150, 40, false, resp)
		selectBranch(t, &v, "feat/a")
		_, cmd := ovKeys(v, "p")
		if got := status(t, cmd); !strings.HasPrefix(got, "peek: otter") {
			t.Errorf("status = %q", got)
		}
		if n := len(fr.CallsSnapshot()); n != 1 {
			t.Errorf("%d calls, want only where", n)
		}
	})
	t.Run("remote shows where only", func(t *testing.T) {
		v, fr := ovFixture(t, 150, 40, true, resp)
		selectBranch(t, &v, "feat/r")
		_, cmd := ovKeys(v, "p")
		if got := status(t, cmd); got != "peek: feat/r on halo" {
			t.Errorf("status = %q", got)
		}
		calls := fr.CallsSnapshot()
		if len(calls) != 1 || calls[0].Name != "crew" {
			t.Errorf("calls = %v, want where only", calls)
		}
	})
}

func TestPeerRefused(t *testing.T) {
	v, fr := ovFixture(t, 150, 40, true, nil)
	selectBranch(t, &v, "feat/p")
	for _, k := range []string{"p", "n", "r", "m"} {
		nv, cmd := ovKeys(v, k)
		if got := status(t, cmd); got != "peer worker: read-only" {
			t.Errorf("%s: status = %q", k, got)
		}
		if nv.mode != ovNormal {
			t.Errorf("%s opened mode %v", k, nv.mode)
		}
	}
	noCalls(t, fr)
}

func TestPeerAttentionDigitRefused(t *testing.T) {
	v, fr := ovFixture(t, 150, 40, true, nil)
	i := slices.IndexFunc(v.model.Attention, func(a vm.AttentionItem) bool { return a.Origin == vm.OriginPeer })
	v.setCursor(i)
	r := overviewLayout(150, 40, v.model, v.st)
	_, cmd := ovKeys(v, string(rune('1'+i-r.stripOff)))
	if got := status(t, cmd); got != "peer worker: read-only" {
		t.Errorf("status = %q", got)
	}
	noCalls(t, fr)
}

func TestFilter(t *testing.T) {
	v, _ := ovFixture(t, 150, 40, false, nil)
	// From the checklists: the focused strip selects only its cursor's worker.
	v, _ = ovKeys(v, "tab", "/")
	if v.mode != ovFilter || !v.Capturing() {
		t.Fatalf("/ did not open the filter")
	}
	v, _ = ovKeys(v, "feat/b")
	for _, p := range []ovPanel{panelChecklists, panelAgents} {
		items := overviewLayout(150, 40, v.model, v.st).panels[p].items
		if len(items) != 1 || v.model.Workers[items[0].worker].Branch != "feat/b" {
			t.Errorf("panel %d under filter: %d rows", p, len(items))
		}
	}
	if selBranch(v) != "feat/b" {
		t.Errorf("selection %s not moved onto the match", selBranch(v))
	}
	kept, _ := ovKeys(v, "enter")
	if kept.mode != ovNormal || kept.st.filter != "feat/b" {
		t.Errorf("enter: mode %v filter %q", kept.mode, kept.st.filter)
	}
	cleared, _ := ovKeys(v, "esc")
	if cleared.mode != ovNormal || cleared.st.filter != "" {
		t.Errorf("esc: mode %v filter %q", cleared.mode, cleared.st.filter)
	}
	if n := len(overviewLayout(150, 40, cleared.model, cleared.st).panels[panelChecklists].items); n < 5 {
		t.Errorf("filter still narrows after esc: %d cards", n)
	}
	// esc in normal mode also drops a kept filter.
	again, _ := ovKeys(kept, "esc")
	if again.st.filter != "" {
		t.Errorf("normal esc kept the filter")
	}
}

func TestHelpOverlay(t *testing.T) {
	v, _ := ovFixture(t, 150, 40, false, nil)
	v, _ = ovKeys(v, "?")
	got := stripped(v.View(150, 40))
	for _, want := range []string{"HELP", "m merge (confirms)", "[ ] views, R refresh", "enter", "tab / shift+tab"} {
		if !strings.Contains(got, want) {
			t.Errorf("help lacks %q", want)
		}
	}
	v, _ = ovKeys(v, "esc")
	if v.mode != ovNormal || strings.Contains(stripped(v.View(150, 40)), "HELP") {
		t.Errorf("esc did not close help")
	}
}

func TestDetailOverlayShowsWhatClears(t *testing.T) {
	v, _ := ovFixture(t, 150, 40, false, nil)
	selectBranch(t, &v, "feat/a")
	v, _ = ovKeys(v, "enter")
	got := stripped(v.View(150, 40))
	if want := "To clear: restart the role pane"; !strings.Contains(got, want) {
		t.Errorf("detail lacks %q:\n%s", want, got)
	}
}

func TestDetailOverlay(t *testing.T) {
	v, _ := ovFixture(t, 150, 40, false, nil)
	selectBranch(t, &v, "feat/a")
	v, _ = ovKeys(v, "enter")
	got := stripped(v.View(150, 40))
	for _, want := range []string{"DETAIL", "otter", "feat/a", "PHASE", "execute", "ACCEPTANCE",
		"execute subagents: not reported", "plan-critic", "reviewer", "crew where --crew c1 -- feat/a", "RECENT EVENTS"} {
		if !strings.Contains(got, want) {
			t.Errorf("detail lacks %q", want)
		}
	}
	v, _ = ovKeys(v, "j")
	if v.mode != ovDetail || selBranch(v) != "feat/b" {
		t.Errorf("j in detail: mode %v sel %s", v.mode, selBranch(v))
	}
	if !strings.Contains(stripped(v.View(150, 40)), "heron") {
		t.Errorf("detail did not follow the selection")
	}
	v, _ = ovKeys(v, "esc")
	if v.mode != ovNormal {
		t.Errorf("esc left mode %v", v.mode)
	}
}

func ovClick(v overviewView, x, y int) overviewView {
	nv, _ := v.Update(tea.MouseMsg{X: x, Y: y, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	return nv.(overviewView)
}

func TestMouse(t *testing.T) {
	v, _ := ovFixture(t, 150, 40, false, nil)
	r := overviewLayout(150, 40, v.model, v.st)

	row := r.panels[panelAttention].items[2]
	cv := ovClick(v, row.rect.x+3, row.rect.y)
	if cv.sel().cursor != row.idx || cv.st.focus != panelAttention || selBranch(cv) != "feat/a" {
		t.Errorf("attention click: cursor %d focus %v sel %s", cv.sel().cursor, cv.st.focus, selBranch(cv))
	}

	card := r.panels[panelChecklists].items[3]
	cw := v.model.Workers[card.worker]
	cv = ovClick(v, card.rect.x+2, card.rect.y+1) // the card's second line
	if cv.st.focus != panelChecklists || cv.st.worker != (workerKey{cw.Crew, cw.Branch}) {
		t.Errorf("card click: focus %v sel %v", cv.st.focus, cv.st.worker)
	}

	ag := r.panels[panelAgents]
	cv = ovClick(v, ag.rect.x+ag.rect.w-2, ag.rect.y+ag.rect.h-1)
	if cv.st.focus != panelAgents {
		t.Errorf("agents click: focus %v", cv.st.focus)
	}
	agRow := ag.items[1]
	cv = ovClick(v, agRow.rect.x, agRow.rect.y)
	if cv.st.focus != panelAgents || cv.st.worker.branch != v.model.Agents[agRow.idx].Branch {
		t.Errorf("agent row click: focus %v sel %v", cv.st.focus, cv.st.worker)
	}

	// A click outside every panel, a right click and a click under an overlay change nothing.
	if cv = ovClick(v, 0, 149); cv.st != v.st {
		t.Errorf("click outside panels changed state")
	}
	nv, _ := v.Update(tea.MouseMsg{X: row.rect.x, Y: row.rect.y, Action: tea.MouseActionPress, Button: tea.MouseButtonRight})
	if nv.(overviewView).st != v.st {
		t.Errorf("right click changed state")
	}
	hv, _ := ovKeys(v, "?")
	if cv = ovClick(hv, row.rect.x, row.rect.y); cv.st != hv.st {
		t.Errorf("click under an overlay changed state")
	}
}

func TestMouseThroughRoot(t *testing.T) {
	asciiProfile(t)
	m := testModel(loadManySnapshot(t))
	m.active = overviewTab
	m = sized(m, 150, 40)
	_ = m.View()
	ov := m.views[overviewTab].(overviewView)
	cards := ov.layout().panels[panelChecklists].items
	if len(cards) < 2 {
		t.Fatalf("only %d cards in the fixture", len(cards))
	}
	card := cards[len(cards)-1]
	y := card.rect.y + len(m.headerLines())
	nm, _ := m.Update(tea.MouseMsg{X: card.rect.x + 2, Y: y, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	got := nm.(Model).views[overviewTab].(overviewView)
	w := ov.model.Workers[card.worker]
	if got.st.focus != panelChecklists || got.st.worker != (workerKey{w.Crew, w.Branch}) {
		t.Errorf("root click: focus %v sel %v, want %s", got.st.focus, got.st.worker, w.Branch)
	}
}

func TestRootKeysOnOverview(t *testing.T) {
	press := func(m Model, k tea.KeyMsg) (Model, tea.Cmd) {
		nm, cmd := m.Update(k)
		return nm.(Model), cmd
	}
	base := testModel(loadSnapshot(t))
	base.active = overviewTab
	base = sized(base, 150, 40)

	m, _ := press(base, keyRune(']'))
	if m.active != liveTab {
		t.Errorf("] on Overview -> tab %d, want Live", m.active)
	}
	m, _ = press(base, keyRune('['))
	if m.active != fleetTab {
		t.Errorf("[ on Overview -> tab %d, want Fleet", m.active)
	}

	_, cmd := press(base, keyRune('R'))
	seen := map[source]bool{}
	for _, msg := range flattenCmd(cmd) {
		if sm, ok := msg.(sourceMsg); ok {
			seen[sm.src] = true
		}
	}
	if len(seen) != int(numSources)-1 { // fleet only once its tab opened
		t.Errorf("R requested %d sources, want %d", len(seen), numSources-1)
	}

	live := base
	live.active = liveTab
	m, _ = press(live, keyRune('1'))
	if m.active != overviewTab {
		t.Errorf("1 on Live -> tab %d, want Overview", m.active)
	}

	// Digits and tab stay with the Overview.
	m, _ = press(base, keyRune('2'))
	if m.active != overviewTab {
		t.Errorf("2 on Overview switched to tab %d", m.active)
	}
	m, _ = press(base, keyType(tea.KeyTab))
	before := base.views[overviewTab].(overviewView).st.focus
	if m.active != overviewTab || m.views[overviewTab].(overviewView).st.focus == before {
		t.Errorf("tab on Overview did not cycle focus (active %d)", m.active)
	}
}

func TestRootCapturingOverviewKeepsQ(t *testing.T) {
	m := testModel(loadSnapshot(t))
	m.active = overviewTab
	m = sized(m, 150, 40)
	nm, _ := m.Update(keyRune('/'))
	m = nm.(Model)
	if !m.views[overviewTab].Capturing() {
		t.Fatalf("filter does not capture")
	}
	nm, cmd := m.Update(keyRune('q'))
	for _, msg := range flattenCmd(cmd) {
		if _, quit := msg.(tea.QuitMsg); quit {
			t.Fatalf("q quit while the filter was open")
		}
	}
	if got := nm.(Model).views[overviewTab].(overviewView).st.filter; got != "q" {
		t.Errorf("filter = %q, want q", got)
	}
}

func TestOverviewRoleFailureGlyph(t *testing.T) {
	v, _ := ovFixture(t, 150, 40, false, nil)
	a := v.model.Agents[0]
	if a.Branch != "feat/a" {
		t.Fatalf("first agent %s", a.Branch)
	}
	for _, r := range a.Roles {
		want := "●"
		if r.State == "blocked" {
			want = "✗"
		}
		if got := stateGlyph(roleState(r)); got != want {
			t.Errorf("role %s %s: glyph %s, want %s", *r.Role, r.State, got, want)
		}
	}
	if got := stateGlyph(roleState(vm.AgentNode{State: "failed"})); got != "✗" {
		t.Errorf("failed role glyph %s", got)
	}
	if got := stateGlyph(roleState(vm.AgentNode{State: "blocked"})); got != "●" {
		t.Errorf("plain blocked role glyph %s", got)
	}
}

func TestOverviewAllClearFocus(t *testing.T) {
	asciiProfile(t)
	v := newOverviewView(allClearInputs(), overviewNow)
	v.View(150, 40)
	if back, _ := ovKeys(v, "shift+tab"); back.st.focus != panelMonitors {
		t.Errorf("shift+tab from the drawn CHECKLISTS focus: focus = %v, want monitors", back.st.focus)
	}
	before := selBranch(v)
	v, _ = ovKeys(v, "j")
	if selBranch(v) == before {
		t.Errorf("j with no strip did not move the selection off %s", before)
	}
	want := []ovPanel{panelAgents, panelMonitors, panelChecklists, panelAgents}
	for i, p := range want {
		v, _ = ovKeys(v, "tab")
		if v.st.focus != p {
			t.Fatalf("tab %d: focus = %v, want %v", i+1, v.st.focus, p)
		}
	}
	v, _ = ovKeys(v, "shift+tab", "shift+tab")
	if v.st.focus != panelMonitors {
		t.Errorf("shift+tab twice from agents: focus = %v, want monitors", v.st.focus)
	}
}

func TestOverviewFooterDropsOwnedDigits(t *testing.T) {
	asciiProfile(t)
	base := sized(testModel(loadSnapshot(t)), 200, 40)
	footer := func(tab int) string {
		m := base
		m.active = tab
		return stripped(m.renderFooter())
	}
	if f := footer(overviewTab); strings.Contains(f, "1-8") {
		t.Errorf("Overview footer advertises view digits: %q", f)
	}
	if f := footer(liveTab); !strings.Contains(f, "1-8 views") {
		t.Errorf("Live footer lacks view digits: %q", f)
	}
}

// stripMarks is the attention items whose strip rows carry the selection bar.
func stripMarks(v overviewView, w, h int) []int {
	r := overviewLayout(w, h, v.model, v.st)
	lines := strings.Split(stripped(v.View(w, h)), "\n")
	var marked []int
	for _, it := range r.panels[panelAttention].items {
		if l := lines[it.rect.y]; strings.Contains(l, "▌") {
			marked = append(marked, it.idx)
		}
	}
	return marked
}

func TestStripCursorDrawn(t *testing.T) {
	v, fr := ovFixture(t, 150, 40, false, nil)
	if got := stripMarks(v, 150, 40); !slices.Equal(got, []int{0}) {
		t.Errorf("initial strip marks = %v, want item 0", got)
	}
	v, _ = ovKeys(v, "j", "j", "j") // item 4: lane_dead on c2, no worker
	if a := v.model.Attention[v.sel().cursor]; a.Kind != vm.KindLaneDead {
		t.Fatalf("item 4 is %s, want lane_dead", a.Kind)
	}
	if got := stripMarks(v, 150, 40); !slices.Equal(got, []int{3}) {
		t.Errorf("strip marks = %v, want the cursor's item 3", got)
	}
	for _, k := range []string{"p", "n", "r", "m"} {
		nv, cmd := ovKeys(v, k)
		if got := status(t, cmd); got != actionVerbs[k]+": no worker selected" {
			t.Errorf("%s on a lane item: status = %q", k, got)
		}
		if nv.mode != ovNormal {
			t.Errorf("%s opened mode %v", k, nv.mode)
		}
	}
	nv, cmd := ovKeys(v, "enter")
	if nv.mode != ovDetail || cmd == nil {
		t.Errorf("enter on a lane item: mode %v", nv.mode)
	}
	noCalls(t, fr)

	// Away from the strip, its rows follow the selected worker again.
	v, _ = ovKeys(v, "tab")
	selectBranch(t, &v, "feat/a")
	v.st.stripOff = 0
	if got := stripMarks(v, 150, 40); !slices.Equal(got, []int{2}) {
		t.Errorf("unfocused strip marks = %v, want feat/a's item 2", got)
	}
}

func TestMergePromptUsesActionPR(t *testing.T) {
	v, fr := ovFixture(t, 150, 40, false, nil)
	i := slices.IndexFunc(v.model.Attention, func(a vm.AttentionItem) bool { return a.Kind == vm.KindMergeReady })
	if i < 0 {
		t.Fatalf("fixture has no merge_ready item")
	}
	act := v.model.Attention[i].Action
	if act == nil || act.PR == nil {
		t.Fatalf("merge_ready action carries no PR: %+v", act)
	}
	// The worker row's PR is not what the action merges.
	for k, w := range v.model.Workers {
		if w.Branch == v.model.Attention[i].Branch {
			stale := 99
			v.model.Workers[k].PR = &stale
		}
	}
	v.setCursor(i)
	r := overviewLayout(150, 40, v.model, v.st)
	v, _ = ovKeys(v, string(rune('1'+i-r.stripOff)))
	if v.mode != ovConfirm {
		t.Fatalf("merge digit did not open the confirm (mode %v)", v.mode)
	}
	want := "merge PR #" + act.Argv[slices.Index(act.Argv, "merge")+1] + " (feat/b)? y/N"
	if v.pend.prompt != want || *act.PR != 12 {
		t.Errorf("prompt = %q, want %q", v.pend.prompt, want)
	}
	noCalls(t, fr)
}

func TestEventSenderNames(t *testing.T) {
	v, _ := ovFixture(t, 200, 60, false, nil)
	for from, want := range map[string]string{
		"worker:feat/a#s1":     "otter",
		"worker:feat/c":        "feat/c",
		"role:feat/a:reviewer": "otter:reviewer",
		"worker:feat/zz#s9":    "feat/zz",
		"pr-watch:12":          "pr-watch:12",
	} {
		if got := eventFrom(v.model, from); got != want {
			t.Errorf("eventFrom(%q) = %q, want %q", from, got, want)
		}
	}
	r := overviewLayout(200, 60, v.model, v.st)
	ev := r.panels[panelEvents]
	lines := strings.Split(stripped(v.View(200, 60)), "\n")[ev.inner.y : ev.inner.y+ev.inner.h]
	text := strings.Join(lines, "\n")
	if strings.Contains(text, "worker:") || !strings.Contains(text, "otter:reviewer") {
		t.Errorf("events panel names senders by raw id:\n%s", text)
	}
}

// poisonActions plants an ESC-laced word in every attention action's command
// and argv, in the way that makes ansi.Strip eat a quote shellQuote inserted.
func poisonActions(v overviewView, command, argv bool) overviewView {
	for _, a := range v.model.Attention {
		if a.Action == nil {
			continue
		}
		if command {
			a.Action.Command = "'abc\x1b'\\\\'X; id #'"
		}
		if argv {
			a.Action.Argv = slices.Clone(a.Action.Argv)
			for i := range a.Action.Argv {
				a.Action.Argv[i] += "\x1b'\\'X; id #"
			}
		}
	}
	return v
}

func TestControlCommandNeverShownCleaned(t *testing.T) {
	for _, p := range []struct{ command, argv bool }{{true, false}, {false, true}, {true, true}} {
		for _, sz := range [][2]int{{150, 40}, {100, 30}, {80, 24}} {
			v, _ := ovFixture(t, sz[0], sz[1], false, nil)
			v = poisonActions(v, p.command, p.argv)
			seen := false
			for _, b := range []string{"feat/a", "feat/b", "feat/c", "feat/r"} {
				selectBranch(t, &v, b)
				v, _ = ovKeys(v, "enter")
				got := stripped(v.View(sz[0], sz[1]))
				v, _ = ovKeys(v, "esc")
				if strings.Contains(got, "id #") || strings.Contains(got, "abc") {
					t.Errorf("%v %v detail shows a cleaned command:\n%s", p, sz, got)
				}
				seen = seen || strings.Contains(got, "control bytes")
			}
			if !seen {
				t.Errorf("%v %v: no detail showed the notice", p, sz)
			}
			for n := 1; n <= 9; n++ {
				v, cmd := ovKeys(v, strconv.Itoa(n))
				_ = v
				if got := statusText(cmd); strings.HasPrefix(got, "run: ") && got != "run: "+controlNotice {
					t.Errorf("%v %v digit %d status = %q", p, sz, n, got)
				}
			}
			v, cmd := ovKeys(v, "enter")
			if got := statusText(cmd); strings.HasPrefix(got, "run: ") && got != "run: "+controlNotice {
				t.Errorf("%v %v enter status = %q", p, sz, got)
			}
		}
	}
}

func TestCleanCommandShownVerbatim(t *testing.T) {
	v, _ := ovFixture(t, 150, 40, false, nil)
	var cmd string
	for _, a := range v.model.Attention {
		if a.Action != nil && a.Action.Key == "" {
			cmd = a.Action.Command
		}
	}
	if cmd == "" || hasControl(vm.Action{Command: cmd}) {
		t.Fatalf("fixture has no clean inspect command (%q)", cmd)
	}
	for range v.model.Attention {
		if nv, _ := ovKeys(v, "enter"); nv.mode == ovDetail && strings.Contains(stripped(nv.View(150, 40)), cmd) {
			return
		}
		v, _ = ovKeys(v, "j")
	}
	t.Errorf("no attention item opened an overlay showing %s", cmd)
}

// statusText is the text of cmd's actionMsg, or "" for any other result.
func statusText(cmd tea.Cmd) string {
	if cmd == nil {
		return ""
	}
	am, _ := cmd().(actionMsg)
	return am.text
}

func TestTinyBodyNoSelection(t *testing.T) {
	asciiProfile(t)
	m := sweepKeys(sized(sweepModel(), 80, 8), keyType(tea.KeyTab), keyRune('j'), keyRune('j'), keyRune('j'))
	frame := stripped(m.View())
	ov := m.views[overviewTab].(overviewView)
	if w := ov.sel().worker; w >= 0 {
		t.Fatalf("worker %d selected with no entry drawn:\n%s", w, frame)
	}
	_, cmd := m.Update(keyRune('p'))
	if got := statusText(cmd); got != actionVerbs["p"]+": no worker selected" {
		t.Errorf("p status = %q\n%s", got, frame)
	}
}

func TestOverviewActionsUseDir(t *testing.T) {
	asciiProfile(t)
	dir := "/r/x"
	in := viewtest.Inputs()
	in.Dir = &dir
	resp := map[string]data.FakeResponse{
		"crew where --crew c1 -- feat/r":                 {Stdout: []byte("feat/r on halo\n")},
		"crew nudge --crew c1 -- feat/e":                 {Stdout: []byte("nudged\n")},
		"crew merge 12 --crew c1":                        {Stdout: []byte("merged\n")},
		"crew reply --crew c1 -- worker:feat/c hi there": {Stdout: []byte("sent\n")},
	}
	fr := &data.FakeRunner{Responses: resp}
	v := newOverviewView(in, overviewNow)
	v.acts = actionDeps{ctx: context.Background(), runner: fr, crew: []string{"crew"}}
	v.View(150, 40)

	run := func(branch string, keys ...string) {
		t.Helper()
		selectBranch(t, &v, branch)
		var cmd tea.Cmd
		v, cmd = ovKeys(v, keys...)
		status(t, cmd)
	}
	run("feat/r", "p")
	run("feat/e", "n", "y")
	run("feat/b", "m", "y")
	run("feat/c", "r", "hi there", "enter")

	calls := fr.CallsSnapshot()
	if len(calls) != 4 {
		t.Fatalf("calls = %v", calls)
	}
	for _, c := range calls {
		if c.Dir != dir {
			t.Errorf("%s %v ran in %q, want %q", c.Name, c.Args, c.Dir, dir)
		}
	}
}

func TestOverviewAttentionUsesReapKeeps(t *testing.T) {
	in := viewtest.Inputs()
	m := NewModel(Deps{
		Now: in.Snap.Now, Crew: in.Crew,
		Load: Loaders{ReapKeeps: func() map[data.WorkerKey]string {
			return map[data.WorkerKey]string{{Crew: "c1", Branch: "feat/h"}: "unmerged commits"}
		}},
	}, func() time.Time { return time.UnixMilli(viewtest.NowMS) }, liveDeps{})
	m.snap, m.digest = in.Snap, in.Bus
	for i := range m.load {
		m.load[i] = loadState{loaded: true}
	}
	for _, a := range vm.BuildOverview(m.viewInputs()).Attention {
		if a.ID == "failed:c1:feat/h" {
			if want := "kept by reap: unmerged commits"; a.Clears != want {
				t.Fatalf("clears = %q, want %q", a.Clears, want)
			}
			return
		}
	}
	t.Fatal("no failed:c1:feat/h item")
}

// ovAt puts focus on panel p at its i-th worker (negative counts from the end).
func ovAt(t *testing.T, v overviewView, p ovPanel, i int) overviewView {
	t.Helper()
	ws := panelWorkers(p, v.model, newOvIndex(v.model, v.st))
	if len(ws) == 0 {
		t.Fatalf("panel %v has no workers", p)
	}
	if i < 0 {
		i += len(ws)
	}
	v.setFocus(p)
	v.setWorker(ws[i])
	return v
}

func ovStripAt(v overviewView, i int) overviewView {
	v.setFocus(panelAttention)
	v.setCursor(i)
	return v
}

func TestOverviewHjklStripEdgeWithStale(t *testing.T) {
	asciiProfile(t)
	in := viewtest.Inputs()
	for _, c := range in.Snap.Roster.Crews {
		for _, w := range c.Workers {
			if w["branch"] == "feat/h" {
				w["needs_cleanup"] = true
				w["cleanup_reason"] = "uncommitted changes; issue closed"
			}
		}
	}
	v := newOverviewView(in, overviewNow)
	v.View(150, 40)
	if slices.IndexFunc(v.model.Attention, func(a vm.AttentionItem) bool { return a.Kind == vm.KindStale }) < 0 {
		t.Fatal("stale worker is not an attention item")
	}
	last := len(v.model.Attention) - 1
	if w, _ := ovKeys(ovStripAt(v, last), "k"); w.st.focus != panelAttention || w.sel().cursor != last-1 {
		t.Errorf("k from stale: focus=%v cursor=%d", w.st.focus, w.sel().cursor)
	}
	if w, _ := ovKeys(ovStripAt(v, last), "j"); w.st.focus != panelChecklists {
		t.Errorf("j past stale: focus=%v", w.st.focus)
	}
}

func TestOverviewHjklStrip(t *testing.T) {
	v, _ := ovFixture(t, 150, 40, false, nil)
	last := len(v.model.Attention) - 1
	if last < 2 {
		t.Fatalf("fixture strip too short: %d", last+1)
	}
	t.Run("first k stays", func(t *testing.T) {
		w, _ := ovKeys(ovStripAt(v, 0), "k")
		if w.st.focus != panelAttention || w.sel().cursor != 0 {
			t.Errorf("focus=%v cursor=%d", w.st.focus, w.sel().cursor)
		}
	})
	t.Run("first j next", func(t *testing.T) {
		w, _ := ovKeys(ovStripAt(v, 0), "j")
		if w.st.focus != panelAttention || w.sel().cursor != 1 {
			t.Errorf("focus=%v cursor=%d", w.st.focus, w.sel().cursor)
		}
	})
	t.Run("last k previous", func(t *testing.T) {
		w, _ := ovKeys(ovStripAt(v, last), "k")
		if w.st.focus != panelAttention || w.sel().cursor != last-1 {
			t.Errorf("focus=%v cursor=%d", w.st.focus, w.sel().cursor)
		}
	})
	t.Run("last j returns to checklists by default", func(t *testing.T) {
		w, _ := ovKeys(ovStripAt(v, last), "j")
		if w.st.focus != panelChecklists {
			t.Errorf("focus=%v", w.st.focus)
		}
	})
	t.Run("last j returns to the panel k left from", func(t *testing.T) {
		for _, p := range []ovPanel{panelAgents, panelMonitors, panelChecklists} {
			w, _ := ovKeys(ovAt(t, v, p, 0), "k")
			if w.st.focus != panelAttention || w.sel().cursor != last {
				t.Fatalf("%v: k focus=%v cursor=%d, want strip last", p, w.st.focus, w.sel().cursor)
			}
			w, _ = ovKeys(w, "j")
			if w.st.focus != p {
				t.Errorf("%v: j returned to %v", p, w.st.focus)
			}
		}
	})
	t.Run("h and l are no-ops", func(t *testing.T) {
		for _, k := range []string{"h", "l", "left", "right"} {
			w, _ := ovKeys(ovStripAt(v, 1), k)
			if w.st.focus != panelAttention || w.sel().cursor != 1 {
				t.Errorf("%s: focus=%v cursor=%d", k, w.st.focus, w.sel().cursor)
			}
		}
	})
}

func TestOverviewHjklPanels(t *testing.T) {
	v, _ := ovFixture(t, 150, 40, false, nil)
	last := len(v.model.Attention) - 1
	for i, p := range []ovPanel{panelChecklists, panelAgents, panelMonitors} {
		name := []string{"checklists", "agents", "monitors"}[i]
		ws := panelWorkers(p, v.model, newOvIndex(v.model, v.st))
		if len(ws) < 2 {
			t.Fatalf("panel %v needs two workers, has %d", p, len(ws))
		}
		t.Run(name+" first k goes to the strip last item", func(t *testing.T) {
			w, _ := ovKeys(ovAt(t, v, p, 0), "k")
			if w.st.focus != panelAttention || w.sel().cursor != last {
				t.Errorf("focus=%v cursor=%d", w.st.focus, w.sel().cursor)
			}
		})
		t.Run(name+" first j next", func(t *testing.T) {
			w, _ := ovKeys(ovAt(t, v, p, 0), "j")
			if w.st.focus != p || w.sel().worker != ws[1] {
				t.Errorf("focus=%v worker=%d, want %d", w.st.focus, w.sel().worker, ws[1])
			}
		})
		t.Run(name+" last j stays", func(t *testing.T) {
			w, _ := ovKeys(ovAt(t, v, p, -1), "j")
			if w.st.focus != p || w.sel().worker != ws[len(ws)-1] {
				t.Errorf("focus=%v worker=%d", w.st.focus, w.sel().worker)
			}
		})
		t.Run(name+" last k previous", func(t *testing.T) {
			w, _ := ovKeys(ovAt(t, v, p, -1), "k")
			if w.st.focus != p || w.sel().worker != ws[len(ws)-2] {
				t.Errorf("focus=%v worker=%d", w.st.focus, w.sel().worker)
			}
		})
	}
}

func TestOverviewHjklEmptyPanel(t *testing.T) {
	v, _ := ovFixture(t, 150, 40, false, nil)
	v.st.filter = "no-such-worker"
	v.setFocus(panelAgents)
	w, _ := ovKeys(v, "j")
	if w.st.focus != panelAgents {
		t.Errorf("j on an empty panel: focus=%v", w.st.focus)
	}
	w, _ = ovKeys(w, "k")
	if w.st.focus != panelAttention || w.sel().cursor != len(w.model.Attention)-1 {
		t.Errorf("k on an empty panel: focus=%v cursor=%d", w.st.focus, w.sel().cursor)
	}
}

func TestOverviewHjklEmptyStrip(t *testing.T) {
	v := newOverviewView(allClearInputs(), overviewNow)
	v.View(150, 40)
	v = ovAt(t, v, panelChecklists, 0)
	w, _ := ovKeys(v, "k")
	if w.st.focus != panelChecklists {
		t.Errorf("k with no strip: focus=%v", w.st.focus)
	}
}

func TestOverviewHjklAcrossRow(t *testing.T) {
	for _, tc := range []struct {
		name string
		w    int
		keys []string
		want []ovPanel
	}{
		{"wide l", 150, []string{"l", "l", "l"}, []ovPanel{panelAgents, panelMonitors, panelMonitors}},
		{"wide h", 150, []string{"l", "l", "h", "h", "h"}, []ovPanel{panelAgents, panelMonitors, panelAgents, panelChecklists, panelChecklists}},
		{"wide arrows", 150, []string{"right", "right", "left"}, []ovPanel{panelAgents, panelMonitors, panelAgents}},
		{"narrow", 80, []string{"l", "l", "l", "h", "h", "h"}, []ovPanel{panelAgents, panelMonitors, panelMonitors, panelAgents, panelChecklists, panelChecklists}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v, _ := ovFixture(t, tc.w, 30, false, nil)
			v = ovAt(t, v, panelChecklists, 0)
			for i, k := range tc.keys {
				v, _ = ovKeys(v, k)
				if v.st.focus != tc.want[i] {
					t.Fatalf("%s %d: focus=%v, want %v", k, i+1, v.st.focus, tc.want[i])
				}
				if tc.w < wideWidth {
					if !v.layout().panels[tc.want[i]].shown {
						t.Fatalf("%s %d: focused %v not shown", k, i+1, tc.want[i])
					}
				}
			}
		})
	}
}

func TestOverviewHjklNarrowReturn(t *testing.T) {
	v, _ := ovFixture(t, 80, 30, false, nil)
	v = ovAt(t, v, panelChecklists, 0)
	v, _ = ovKeys(v, "l", "k")
	if v.st.focus != panelAttention {
		t.Fatalf("focus=%v", v.st.focus)
	}
	v, _ = ovKeys(v, "j")
	if v.st.focus != panelAgents {
		t.Errorf("returned to %v, want agents", v.st.focus)
	}
}

func TestOverviewHjklHelpAndFooter(t *testing.T) {
	v, _ := ovFixture(t, 150, 40, false, nil)
	if !slices.ContainsFunc(v.Keys(), func(b key.Binding) bool { return b.Help().Key == "hjkl" }) {
		t.Error("footer has no hjkl hint")
	}
	if !slices.ContainsFunc(overviewHelp, func(h helpKey) bool { return strings.Contains(h.keys, "h j k l") }) {
		t.Error("help has no h j k l row")
	}
}

func TestOverviewHjklReturnRestoresWorker(t *testing.T) {
	for _, p := range []ovPanel{panelChecklists, panelAgents, panelMonitors} {
		v, _ := ovFixture(t, 150, 40, false, nil)
		v = ovAt(t, v, p, 0)
		want := v.sel().worker
		v, _ = ovKeys(v, "k")
		if v.st.focus != panelAttention {
			t.Fatalf("panel %v: k focus=%v", p, v.st.focus)
		}
		v, _ = ovKeys(v, "j")
		if v.st.focus != p || v.sel().worker != want {
			t.Errorf("panel %v: focus=%v worker=%d, want %v worker=%d", p, v.st.focus, v.sel().worker, p, want)
		}
	}
}

func TestOverviewHjklStaleReturnPanel(t *testing.T) {
	v, _ := ovFixture(t, 150, 40, false, nil)
	v = ovAt(t, v, panelAgents, 0)
	v, _ = ovKeys(v, "k")
	for i := 0; i < 6 && v.st.focus == panelAttention; i++ {
		v, _ = ovKeys(v, "tab")
	}
	for i := 0; i < 6 && v.st.focus != panelAttention; i++ {
		v, _ = ovKeys(v, "tab")
	}
	if v.st.focus != panelAttention {
		t.Fatalf("focus=%v, never back on the strip", v.st.focus)
	}
	v = ovStripAt(v, len(v.model.Attention)-1)
	v, _ = ovKeys(v, "j")
	if v.st.focus != panelChecklists {
		t.Errorf("focus=%v, want checklists", v.st.focus)
	}
}

func TestOverviewHjklDetailGuard(t *testing.T) {
	v, _ := ovFixture(t, 150, 40, false, nil)
	v = ovAt(t, v, panelChecklists, 0)
	want := v.sel().worker
	v, _ = ovKeys(v, "enter")
	if v.mode != ovDetail {
		t.Fatalf("mode=%v, want detail", v.mode)
	}
	v, _ = ovKeys(v, "k")
	if v.st.focus != panelChecklists || v.mode != ovDetail || v.sel().worker != want {
		t.Errorf("focus=%v mode=%v worker=%d, want checklists detail %d", v.st.focus, v.mode, v.sel().worker, want)
	}
}

func TestOverviewMarksStaleWorker(t *testing.T) {
	asciiProfile(t)
	in := viewtest.Inputs()
	for _, c := range in.Snap.Roster.Crews {
		for _, w := range c.Workers {
			if w["branch"] == "feat/h" {
				w["needs_cleanup"] = true
				w["cleanup_reason"] = "uncommitted changes; issue closed"
			}
		}
	}
	v := newOverviewView(in, overviewNow)
	got := stripped(v.View(150, 40))
	if !strings.Contains(got, "stale") {
		t.Errorf("no stale marker:\n%s", got)
	}
	if !strings.Contains(got, "○ stale") {
		t.Errorf("no stale marker on the worker card:\n%s", got)
	}
}

func TestDetailOverlayStaleWorkerAgents(t *testing.T) {
	asciiProfile(t)
	in := viewtest.Inputs()
	for _, c := range in.Snap.Roster.Crews {
		for _, w := range c.Workers {
			if w["branch"] == "feat/h" {
				w["needs_cleanup"] = true
				w["cleanup_reason"] = "uncommitted changes; issue closed"
			}
		}
	}
	v := newOverviewView(in, overviewNow)
	v.View(150, 40)
	selectBranch(t, &v, "feat/h")
	v, _ = ovKeys(v, "enter")
	got := stripped(v.View(150, 40))
	if want := "none — stale, waiting on human cleanup"; !strings.Contains(got, want) {
		t.Errorf("detail lacks %q:\n%s", want, got)
	}
}
