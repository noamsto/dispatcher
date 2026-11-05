package ui

import (
	"reflect"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/noamsto/dispatcher/dash/internal/data"
)

const wherePrefix = "sage — main:3.0 \"feat/584-crew-dash\" (lead pane)   "

func ok(stdout string) data.FakeResponse { return data.FakeResponse{Stdout: []byte(stdout)} }

func actionModel(t *testing.T, snap data.Snapshot, fake *data.FakeRunner, inTmux bool, crew ...string) Model {
	t.Helper()
	if len(crew) == 0 {
		crew = []string{"crew"}
	}
	return sized(loadedModel(snap, Deps{Runner: fake, Crew: crew, InTmux: inTmux, Now: snap.Now}), 100, 30)
}

// selectName moves the Live cursor onto the named worker.
func selectName(t *testing.T, m Model, name string) Model {
	t.Helper()
	for range 20 {
		if w, _ := m.views[0].Selection().(data.Worker); w.Name == name {
			return m
		}
		m = send(m, keyRune('j'))
	}
	t.Fatalf("worker %q not found", name)
	return m
}

// send delivers msg, runs the Cmd it returns and feeds an actionMsg result
// back. The status-clear tick Cmd the root returns for it is not run.
func send(m Model, msg tea.Msg) Model {
	nm, cmd := m.Update(msg)
	m = nm.(Model)
	if cmd == nil {
		return m
	}
	if am, isAction := cmd().(actionMsg); isAction {
		nm, _ = m.Update(am)
		m = nm.(Model)
	}
	return m
}

func typeText(m Model, s string) Model {
	return send(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)})
}

func argvs(f *data.FakeRunner) [][]string {
	var out [][]string
	for _, c := range f.CallsSnapshot() {
		out = append(out, append([]string{c.Name}, c.Args...))
	}
	return out
}

func wantCalls(t *testing.T, f *data.FakeRunner, want ...[]string) {
	t.Helper()
	if got := argvs(f); !reflect.DeepEqual(got, want) {
		t.Fatalf("calls = %q, want %q", got, want)
	}
}

func wantStatus(t *testing.T, m Model, want string) {
	t.Helper()
	if m.status != want {
		t.Fatalf("status = %q, want %q", m.status, want)
	}
}

func TestJumpRunsWhereThenSwitchClient(t *testing.T) {
	fake := &data.FakeRunner{Responses: map[string]data.FakeResponse{
		"crew where --crew c1 -- feat/584-crew-dash": ok(wherePrefix + "jump: ! tmux switch-client -t %42\n"),
		"tmux switch-client -t %42":                  ok(""),
	}}
	m := selectName(t, actionModel(t, loadFullSnapshot(t), fake, true), "sage")
	m = send(m, keyRune('t'))
	wantCalls(t, fake,
		[]string{"crew", "where", "--crew", "c1", "--", "feat/584-crew-dash"},
		[]string{"tmux", "switch-client", "-t", "%42"})
	wantStatus(t, m, "jump: ok")
}

func TestJumpHostLineRunsNoTmux(t *testing.T) {
	fake := &data.FakeRunner{Responses: map[string]data.FakeResponse{
		"crew where --crew c1 -- feat/584-crew-dash": ok(wherePrefix + "jump: ! tmux switch-client -t %42 [host mini]\n"),
	}}
	m := selectName(t, actionModel(t, loadFullSnapshot(t), fake, true), "sage")
	m = send(m, keyRune('t'))
	wantCalls(t, fake, []string{"crew", "where", "--crew", "c1", "--", "feat/584-crew-dash"})
	wantStatus(t, m, "jump failed: remote worker")
	if !m.statusErr {
		t.Fatalf("a failure must be flagged as an error")
	}
}

func TestJumpRejectsNonPaneToken(t *testing.T) {
	fake := &data.FakeRunner{Responses: map[string]data.FakeResponse{
		"crew where --crew c1 -- feat/584-crew-dash": ok(wherePrefix + "jump: ! tmux switch-client -t main:3.0\n"),
	}}
	m := selectName(t, actionModel(t, loadFullSnapshot(t), fake, true), "sage")
	m = send(m, keyRune('t'))
	if got := len(fake.CallsSnapshot()); got != 1 {
		t.Fatalf("%d calls, want only the where call: %q", got, argvs(fake))
	}
	wantStatus(t, m, "jump failed: no pane id")
}

func TestJumpWhereFailureSurfacesStderr(t *testing.T) {
	fake := &data.FakeRunner{Responses: map[string]data.FakeResponse{
		"crew where --crew c1 -- feat/584-crew-dash": {Stderr: []byte("crew: no such worker\nmore\n"), Err: data.FakeExitError{Code: 2}},
	}}
	m := selectName(t, actionModel(t, loadFullSnapshot(t), fake, true), "sage")
	m = send(m, keyRune('t'))
	wantStatus(t, m, "jump failed: crew: no such worker")
}

func TestJumpDisabledOutsideTmux(t *testing.T) {
	fake := &data.FakeRunner{}
	m := selectName(t, actionModel(t, loadFullSnapshot(t), fake, false), "sage")
	m = send(m, keyRune('t'))
	wantCalls(t, fake)
	wantStatus(t, m, "jump: not inside tmux")
}

func TestJumpDisabledOnHostRow(t *testing.T) {
	fake := &data.FakeRunner{}
	snap := loadCrewErrSnapshot(t)
	m := actionModel(t, snap, fake, true)
	for range 20 {
		if w, _ := m.views[0].Selection().(data.Worker); w.Host != "" {
			break
		}
		m = send(m, keyRune('j'))
	}
	m = send(m, keyRune('t'))
	wantCalls(t, fake)
	wantStatus(t, m, "jump: remote worker (host mini)")
}

func TestActionsDisabledWithoutSelection(t *testing.T) {
	fake := &data.FakeRunner{}
	snap := loadFullSnapshot(t)
	snap.Roster.Crews[0].Workers = nil
	m := actionModel(t, snap, fake, true)
	m = send(m, keyRune('o'))
	wantCalls(t, fake)
	wantStatus(t, m, "open PR: no worker selected")
}

func TestOpenPRRunsGhView(t *testing.T) {
	url := "https://github.com/noamsto/dispatcher/pull/999"
	fake := &data.FakeRunner{Responses: map[string]data.FakeResponse{"gh pr view --web " + url: ok("")}}
	m := selectName(t, actionModel(t, loadFullSnapshot(t), fake, false), "sage")
	m = send(m, keyRune('o'))
	wantCalls(t, fake, []string{"gh", "pr", "view", "--web", url})
	wantStatus(t, m, "open PR: ok")
}

func TestOpenPRDisabledWithoutPR(t *testing.T) {
	fake := &data.FakeRunner{}
	m := selectName(t, actionModel(t, loadFullSnapshot(t), fake, true), "coral")
	m = send(m, keyRune('o'))
	wantCalls(t, fake)
	wantStatus(t, m, "open PR: no PR")
}

func TestReplySendsText(t *testing.T) {
	fake := &data.FakeRunner{Responses: map[string]data.FakeResponse{
		"crew reply --crew c1 -- worker:fix/123-widget go ahead": ok("sent\nignored\n"),
	}}
	m := selectName(t, actionModel(t, loadFullSnapshot(t), fake, true), "coral")
	m = send(m, keyRune('m'))
	if !strings.Contains(m.View(), "reply to coral: ") {
		t.Fatalf("prompt not rendered:\n%s", m.View())
	}
	m = typeText(m, "go ahead")
	m = send(m, keyType(tea.KeyEnter))
	wantCalls(t, fake, []string{"crew", "reply", "--crew", "c1", "--", "worker:fix/123-widget", "go ahead"})
	wantStatus(t, m, "reply: sent")
	if m.views[0].Capturing() {
		t.Fatalf("prompt should close after sending")
	}
}

func TestReplyEmptyTextRefused(t *testing.T) {
	for _, text := range []string{"", "   "} {
		fake := &data.FakeRunner{}
		m := selectName(t, actionModel(t, loadFullSnapshot(t), fake, true), "coral")
		m = send(m, keyRune('m'))
		m = typeText(m, text)
		m = send(m, keyType(tea.KeyEnter))
		wantCalls(t, fake)
		wantStatus(t, m, "reply: text required")
		if !m.views[0].Capturing() {
			t.Fatalf("prompt must stay open after an empty reply (text %q)", text)
		}
	}
}

func TestReplyEscCancels(t *testing.T) {
	fake := &data.FakeRunner{}
	m := selectName(t, actionModel(t, loadFullSnapshot(t), fake, true), "coral")
	m = send(m, keyRune('m'))
	m = typeText(m, "hello")
	m = send(m, keyType(tea.KeyEsc))
	wantCalls(t, fake)
	if m.views[0].Capturing() {
		t.Fatalf("esc should close the prompt")
	}
}

func TestReplyDisabledWhenNotBlocked(t *testing.T) {
	fake := &data.FakeRunner{}
	m := selectName(t, actionModel(t, loadFullSnapshot(t), fake, true), "sage")
	m = send(m, keyRune('m'))
	wantCalls(t, fake)
	wantStatus(t, m, "reply: not blocked")
	if m.views[0].Capturing() {
		t.Fatalf("no prompt should open on a worker that is not blocked")
	}
}

func TestNudgeConfirmThenRuns(t *testing.T) {
	fake := &data.FakeRunner{Responses: map[string]data.FakeResponse{"crew nudge --crew c1 -- sage": ok("")}}
	m := selectName(t, actionModel(t, loadFullSnapshot(t), fake, true), "sage")
	m = send(m, keyRune('n'))
	wantCalls(t, fake)
	if !strings.Contains(m.View(), "nudge sage? y/N") {
		t.Fatalf("confirm not rendered:\n%s", m.View())
	}
	m = send(m, keyRune('y'))
	wantCalls(t, fake, []string{"crew", "nudge", "--crew", "c1", "--", "sage"})
	wantStatus(t, m, "nudge: ok")
}

func TestNudgeDeclined(t *testing.T) {
	fake := &data.FakeRunner{}
	m := selectName(t, actionModel(t, loadFullSnapshot(t), fake, true), "sage")
	m = send(m, keyRune('n'))
	m = send(m, keyRune('n'))
	wantCalls(t, fake)
	wantStatus(t, m, "nudge: cancelled")
	if m.views[0].Capturing() {
		t.Fatalf("declining should close the confirm")
	}
}

func TestNudgeRefusalSurfacesStderr(t *testing.T) {
	fake := &data.FakeRunner{Responses: map[string]data.FakeResponse{
		"crew nudge --crew c1 -- sage": {
			Stderr: []byte("crew: nudge: no unread directive for sage — post it first\n"),
			Err:    data.FakeExitError{Code: 2},
		},
	}}
	m := selectName(t, actionModel(t, loadFullSnapshot(t), fake, true), "sage")
	m = send(m, keyRune('n'))
	m = send(m, keyRune('y'))
	wantStatus(t, m, "nudge failed: crew: nudge: no unread directive for sage — post it first")
	if !m.statusErr {
		t.Fatalf("a refusal must be flagged as an error")
	}
}

func TestNudgeExitCodeWhenNoStderr(t *testing.T) {
	fake := &data.FakeRunner{Responses: map[string]data.FakeResponse{
		"crew nudge --crew c1 -- sage": {Err: data.FakeExitError{Code: 2}},
	}}
	m := selectName(t, actionModel(t, loadFullSnapshot(t), fake, true), "sage")
	m = send(m, keyRune('n'))
	m = send(m, keyRune('y'))
	wantStatus(t, m, "nudge failed: exit 2")
}

func TestActionsUseResolvedCrewPrefix(t *testing.T) {
	fake := &data.FakeRunner{Responses: map[string]data.FakeResponse{
		"bash -euo pipefail /x/crew nudge --crew c1 -- sage": ok(""),
	}}
	m := selectName(t, actionModel(t, loadFullSnapshot(t), fake, true, "bash", "-euo", "pipefail", "/x/crew"), "sage")
	m = send(m, keyRune('n'))
	m = send(m, keyRune('y'))
	wantCalls(t, fake, []string{"bash", "-euo", "pipefail", "/x/crew", "nudge", "--crew", "c1", "--", "sage"})
}

func TestGlobalKeysGoToPromptWhileCapturing(t *testing.T) {
	fake := &data.FakeRunner{}
	m := selectName(t, actionModel(t, loadFullSnapshot(t), fake, true), "coral")
	m = send(m, keyRune('m'))
	for _, r := range "q1r" {
		nm, cmd := m.Update(keyRune(r))
		m = nm.(Model)
		if cmd != nil {
			if _, quit := cmd().(tea.QuitMsg); quit {
				t.Fatalf("%q quit the program while the prompt was open", r)
			}
		}
	}
	if m.active != 0 {
		t.Fatalf("digit switched tab to %d while the prompt was open", m.active)
	}
	if got := m.views[0].(liveView).input.Value(); got != "q1r" {
		t.Fatalf("prompt text = %q, want q1r", got)
	}
	wantCalls(t, fake)
}

func TestStatusClearsOnMatchingTickOnly(t *testing.T) {
	fake := &data.FakeRunner{}
	m := selectName(t, actionModel(t, loadFullSnapshot(t), fake, true), "sage")
	m = send(m, keyRune('m'))
	stale := clearStatusMsg{at: m.statusAt.Add(-1)}
	nm, _ := m.Update(stale)
	m = nm.(Model)
	wantStatus(t, m, "reply: not blocked")
	nm, _ = m.Update(clearStatusMsg{at: m.statusAt})
	wantStatus(t, nm.(Model), "")
}

func TestStatusLineReservesBodyHeight(t *testing.T) {
	fake := &data.FakeRunner{}
	m := selectName(t, actionModel(t, loadFullSnapshot(t), fake, true), "sage")
	m = send(m, keyRune('m'))
	out := m.View()
	assertWidth(t, out, 100, 30)
	lines := strings.Split(out, "\n")
	if !strings.Contains(lines[len(lines)-2], "reply: not blocked") {
		t.Fatalf("status line should sit directly above the help bar:\n%s", out)
	}
}

func TestStatusLineTruncatedToWidth(t *testing.T) {
	fake := &data.FakeRunner{}
	m := sized(actionModel(t, loadFullSnapshot(t), fake, true), 20, 12)
	nm, _ := m.Update(actionMsg{text: strings.Repeat("long ", 20), err: true})
	assertWidth(t, nm.(Model).View(), 20, 12)
}

func TestHelpBarKeysAreHandled(t *testing.T) {
	snap := loadFullSnapshot(t)
	fake := &data.FakeRunner{}
	// coral sorts first, so j has somewhere to go.
	v := newLiveView(snap, allLoaded, liveDeps{eventsPath: "testdata/events.jsonl"}, fixedNow(snap))
	v.acts = actionDeps{runner: fake, crew: []string{"crew"}, inTmux: true}
	for _, b := range v.Keys() {
		k := b.Keys()[0]
		msg := keyRune([]rune(k)[0])
		if k == "enter" {
			msg = keyType(tea.KeyEnter)
		}
		nv, cmd := v.Update(msg)
		after := nv.(liveView)
		if cmd == nil && after.cursor == v.cursor && after.mode == v.mode && after.detail == v.detail && after.showReaped == v.showReaped {
			t.Errorf("help-bar key %q is a silent no-op", k)
		}
	}
}

func TestKeysPerMode(t *testing.T) {
	snap := loadFullSnapshot(t)
	v := newLiveView(snap, allLoaded, liveDeps{}, fixedNow(snap))
	v.acts = actionDeps{runner: &data.FakeRunner{}, crew: []string{"crew"}, inTmux: true}
	helps := func(v liveView) string {
		var parts []string
		for _, b := range v.Keys() {
			parts = append(parts, b.Help().Key+" "+b.Help().Desc)
		}
		return strings.Join(parts, ", ")
	}
	if got, want := helps(v), "j/k move, enter events, t jump, o open PR, m reply, n nudge, a show reaped"; got != want {
		t.Errorf("normal keys = %q, want %q", got, want)
	}
	reply := press(v, keyRune('m'))
	if got, want := helps(reply), "enter send, esc cancel"; got != want {
		t.Errorf("reply keys = %q, want %q", got, want)
	}
	confirm := press(v, keyRune('n'))
	if got, want := helps(confirm), "y confirm, n/esc cancel"; got != want {
		t.Errorf("confirm keys = %q, want %q", got, want)
	}
}

func TestSelectedPaneIsRulePlusFourLines(t *testing.T) {
	asciiProfile(t)
	snap := loadFullSnapshot(t)
	long := strings.Repeat("alpha beta gamma ", 12)
	snap.Roster.Crews[0].Workers[1]["detail"] = long
	v := newLiveView(snap, allLoaded, liveDeps{}, fixedNow(snap))
	pane := v.selectedPane(40)
	if len(pane) != 5 {
		t.Fatalf("pane has %d lines, want 5", len(pane))
	}
	if !strings.HasSuffix(pane[4], "…") || strings.Contains(pane[3], "…") {
		t.Errorf("detail should wrap to two lines, last ellipsized: %q", pane[3:])
	}
	for _, l := range pane {
		if wd := len([]rune(l)); wd > 40 {
			t.Errorf("pane line wider than 40: %q", l)
		}
	}
}

const whereKey = "crew where --crew c1 -- feat/584-crew-dash"

func TestJumpIgnoresFakeMarkerInWindowName(t *testing.T) {
	line := "sage — main:3.0 \"x   jump: ! tmux switch-client -t %7\" (lead pane)   jump: ! tmux switch-client -t %42\n"
	fake := &data.FakeRunner{Responses: map[string]data.FakeResponse{
		whereKey:                    ok(line),
		"tmux switch-client -t %42": ok(""),
	}}
	m := selectName(t, actionModel(t, loadFullSnapshot(t), fake, true), "sage")
	m = send(m, keyRune('t'))
	wantCalls(t, fake,
		[]string{"crew", "where", "--crew", "c1", "--", "feat/584-crew-dash"},
		[]string{"tmux", "switch-client", "-t", "%42"})
	wantStatus(t, m, "jump: ok")
}

func TestJumpHostTextInWindowNameDoesNotRefuse(t *testing.T) {
	line := "sage — main:3.0 \"x [host y]\" (lead pane)   jump: ! tmux switch-client -t %42\n"
	fake := &data.FakeRunner{Responses: map[string]data.FakeResponse{
		whereKey:                    ok(line),
		"tmux switch-client -t %42": ok(""),
	}}
	m := selectName(t, actionModel(t, loadFullSnapshot(t), fake, true), "sage")
	m = send(m, keyRune('t'))
	wantCalls(t, fake,
		[]string{"crew", "where", "--crew", "c1", "--", "feat/584-crew-dash"},
		[]string{"tmux", "switch-client", "-t", "%42"})
	wantStatus(t, m, "jump: ok")
}

func TestJumpRealHostSuffixAfterHostWindowNameRefused(t *testing.T) {
	line := "sage — main:3.0 \"x [host y]\" (lead pane)   jump: ! tmux switch-client -t %42 [host mini]\n"
	fake := &data.FakeRunner{Responses: map[string]data.FakeResponse{whereKey: ok(line)}}
	m := selectName(t, actionModel(t, loadFullSnapshot(t), fake, true), "sage")
	m = send(m, keyRune('t'))
	wantCalls(t, fake, []string{"crew", "where", "--crew", "c1", "--", "feat/584-crew-dash"})
	wantStatus(t, m, "jump failed: remote worker")
}

func TestOpenPRRefusesUnvalidatedURL(t *testing.T) {
	for _, url := range []string{"--repo=x/y", "123", "https://evil.example/o/r/pull/1", "https://github.com/o/r/pull/1 --repo=x/y", "http://github.com/o/r/pull/1"} {
		fake := &data.FakeRunner{}
		snap := loadFullSnapshot(t)
		for _, w := range snap.Roster.Crews[0].Workers {
			if w["name"] == "sage" {
				w["pr_url"] = url
			}
		}
		m := selectName(t, actionModel(t, snap, fake, true), "sage")
		m = send(m, keyRune('o'))
		wantCalls(t, fake)
		wantStatus(t, m, "open PR: no valid PR URL")
	}
}

func TestReplyCtrlCCancels(t *testing.T) {
	fake := &data.FakeRunner{}
	m := selectName(t, actionModel(t, loadFullSnapshot(t), fake, true), "coral")
	m = send(m, keyRune('m'))
	m = typeText(m, "hello")
	nm, cmd := m.Update(keyType(tea.KeyCtrlC))
	m = nm.(Model)
	if cmd != nil {
		if _, quit := cmd().(tea.QuitMsg); quit {
			t.Fatalf("ctrl+c quit the program instead of cancelling the prompt")
		}
	}
	wantCalls(t, fake)
	if m.views[0].Capturing() {
		t.Fatalf("ctrl+c should close the prompt")
	}
}

func TestReplyLongTextScrollsToShowTail(t *testing.T) {
	asciiProfile(t)
	fake := &data.FakeRunner{}
	m := selectName(t, actionModel(t, loadFullSnapshot(t), fake, true), "coral")
	m = sized(m, 80, 24)
	m = send(m, keyRune('m'))
	m = typeText(m, strings.Repeat("x", 80)+"ENDMARK")
	out := m.View()
	assertWidth(t, out, 80, 24)
	if !strings.Contains(out, "ENDMARK") {
		t.Fatalf("tail of a long reply is cut off:\n%s", out)
	}
}

func TestReplyResizeAfterOpenKeepsTailVisible(t *testing.T) {
	asciiProfile(t)
	fake := &data.FakeRunner{}
	m := selectName(t, actionModel(t, loadFullSnapshot(t), fake, true), "coral")
	m = send(m, keyRune('m'))
	m = sized(m, 80, 24)
	m = typeText(m, strings.Repeat("x", 80)+"ENDMARK")
	if out := m.View(); !strings.Contains(out, "ENDMARK") {
		t.Fatalf("tail of a long reply is cut off after resize:\n%s", out)
	}
}

func footerOf(m Model) string {
	lines := strings.Split(m.View(), "\n")
	return lines[len(lines)-1]
}

func TestFooterShowsLiveKeysAt80Cols(t *testing.T) {
	asciiProfile(t)
	m := sized(testModel(loadFullSnapshot(t)), 80, 24)
	foot := footerOf(m)
	for _, want := range []string{"j/k move", "enter events", "t jump"} {
		if !strings.Contains(foot, want) {
			t.Errorf("footer lacks %q: %q", want, foot)
		}
	}
}

func TestFooterShowsOnlyPromptKeysWhileCapturing(t *testing.T) {
	asciiProfile(t)
	fake := &data.FakeRunner{}
	m := selectName(t, actionModel(t, loadFullSnapshot(t), fake, true), "coral")
	m = sized(m, 80, 24)
	m = send(m, keyRune('m'))
	foot := footerOf(m)
	for _, want := range []string{"enter send", "esc cancel"} {
		if !strings.Contains(foot, want) {
			t.Errorf("footer lacks %q: %q", want, foot)
		}
	}
	for _, bad := range []string{"q quit", "r refresh"} {
		if strings.Contains(foot, bad) {
			t.Errorf("footer lists %q while the prompt captures keys: %q", bad, foot)
		}
	}
}

func TestReplyLongPromptKeepsTailVisibleOnNarrowWidth(t *testing.T) {
	asciiProfile(t)
	branch := "feat/" + strings.Repeat("b", 45)
	snap := loadFullSnapshot(t)
	for _, w := range snap.Roster.Crews[0].Workers {
		if w["name"] == "coral" {
			delete(w, "name")
			w["branch"] = branch
		}
	}
	m := actionModel(t, snap, &data.FakeRunner{}, true)
	for range 20 {
		if w, _ := m.views[0].Selection().(data.Worker); w.Branch == branch {
			break
		}
		m = send(m, keyRune('j'))
	}
	m = sized(m, 60, 24)
	m = send(m, keyRune('m'))
	m = typeText(m, strings.Repeat("x", 40)+"ENDMARK")
	out := m.View()
	assertWidth(t, out, 60, 24)
	if !strings.Contains(out, "ENDMARK") {
		t.Fatalf("tail of a reply is cut off under a long prompt:\n%s", out)
	}
}

func TestFooterShowsFilterKeysWhileSettingsFilterOpen(t *testing.T) {
	asciiProfile(t)
	m := sized(testModel(loadFullSnapshot(t)), 80, 24)
	m.active = 3
	m = send(m, keyRune('/'))
	foot := footerOf(m)
	for _, want := range []string{"enter apply", "esc clear"} {
		if !strings.Contains(foot, want) {
			t.Errorf("footer lacks %q: %q", want, foot)
		}
	}
	if strings.Contains(foot, "expand/collapse") {
		t.Errorf("footer lists tree keys while the filter captures keys: %q", foot)
	}
}

func TestFooterKeepsWholeLoadStatusWhenKeysOverflow(t *testing.T) {
	asciiProfile(t)
	snap := loadFullSnapshot(t)
	m := sized(loadingModel(Deps{Now: snap.Now}), 160, 40)
	foot := footerOf(m)
	if want := "loading settings, budget, retro, ratings, roster…"; !strings.Contains(foot, want) {
		t.Fatalf("footer cut the load status, want %q in %q", want, foot)
	}
	assertWidth(t, foot, 160, 1)
}

func TestWorkerByBranch(t *testing.T) {
	msg := "boom"
	row := func(branch, state string) map[string]any {
		return map[string]any{"branch": branch, "name": "n-" + branch, "state": state, "pr_url": "https://github.com/o/r/pull/1", "host": "h", "session": "s"}
	}
	want := func(crew, branch, state string) data.Worker {
		return data.Worker{Crew: crew, Branch: branch, Session: "s", Name: "n-" + branch, State: state, PRURL: "https://github.com/o/r/pull/1", Host: "h"}
	}
	snap := data.Snapshot{Roster: data.RosterSection{Crews: []data.RosterCrew{
		{ID: "c1", Workers: []map[string]any{row("feat/a", "working"), row("feat/gone", "reaped")}},
		{ID: "c2", Workers: []map[string]any{row("feat/b", "idle")}},
		{ID: "bad", Workers: []map[string]any{row("feat/x", "idle")}, WorkersError: &msg},
	}}}

	tests := []struct {
		name, crew, branch string
		want               data.Worker
		ok                 bool
	}{
		{"found", "c1", "feat/a", want("c1", "feat/a", "working"), true},
		{"reaped found", "c1", "feat/gone", want("c1", "feat/gone", "reaped"), true},
		{"wrong crew", "c2", "feat/a", data.Worker{}, false},
		{"unknown branch", "c1", "feat/nope", data.Worker{}, false},
		{"crew with workers error", "bad", "feat/x", data.Worker{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := workerByBranch(snap, tt.crew, tt.branch)
			if ok != tt.ok || got != tt.want {
				t.Fatalf("workerByBranch(%q, %q) = %+v, %v; want %+v, %v", tt.crew, tt.branch, got, ok, tt.want, tt.ok)
			}
		})
	}
}
