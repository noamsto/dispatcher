package nudge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/clock"
	"github.com/noamsto/dispatcher/crew/internal/frame"
	"github.com/noamsto/dispatcher/crew/internal/tmux/tmuxtest"
)

// fixture is one of the shared pane fixtures the frame package and the bats rows use, so
// a frame scripted here is the frame a bats row scripts there.
func fixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile("../frame/testdata/frames/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimRight(string(data), "\n")
}

const (
	idleReq = "worker:feat/x#s100-1"
	idleTS  = "1700000000000"
)

// fakePane scripts the captures a Pane call makes (in call order, the last frame
// repeating, as the bats tmux stub does) and logs every tmux argv.
type fakePane struct {
	frames   []string
	captures int
	colored  int
	tmux     []string
	// windows/panes answer the anchor gate's two reads; fail* is that read dying.
	windows, panes string
	failWindows    bool
	failPanes      bool
	enterFails     bool
	keysFail       bool
}

func (f *fakePane) frameAt(i int) string {
	if i >= len(f.frames) {
		return f.frames[len(f.frames)-1]
	}
	return f.frames[i]
}

// run answers the tmux calls. capture-pane counts plain and colored reads
// separately, as the bats stub does: rows script the plain ones, and the colored
// read answers the same frame (every fixture is unstyled). Other calls are logged.
func (f *fakePane) run(args []string) (string, error) {
	if args[0] == "capture-pane" {
		if args[len(args)-1] != "%9" {
			return "", tmuxtest.ErrNoServer
		}
		if slices.Contains(args, "-e") {
			f.colored++
			return f.frameAt(f.colored - 1), nil
		}
		f.captures++
		return f.frameAt(f.captures - 1), nil
	}
	f.tmux = append(f.tmux, strings.Join(args, " "))
	switch args[0] {
	case "list-windows":
		if f.failWindows {
			return "", tmuxtest.ErrNoServer
		}
		// The bats stub answers per query: the arm asks for six columns including
		// @crew_dir, the pane's own anchor gate for three. One fixture, projected.
		if !strings.Contains(args[len(args)-1], "@crew_dir") {
			return project(f.windows), nil
		}
		return f.windows, nil
	case "list-panes":
		if f.failPanes {
			return "", tmuxtest.ErrNoServer
		}
		return f.panes, nil
	case "send-keys":
		enter := len(args) > 3 && args[3] == "Enter"
		if enter && f.enterFails || !enter && f.keysFail {
			return "", tmuxtest.ErrNoServer
		}
	}
	return "", nil
}

// fakeBus is the anchor's fold input and the rows the call appended.
type fakeBus struct {
	rows      []any
	appended  []any
	eventsErr error
}

func (b *fakeBus) events() ([]any, error) { return b.rows, b.eventsErr }

func (b *fakeBus) append(row any) error {
	b.appended = append(b.appended, row)
	return nil
}

// env is a Deps.Getenv that answers from a map and defaults to "".
func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// newDeps is the Deps under test: fakes for every side effect, a real clock so the
// row's `now*1000|floor` stamp is checked against the wall clock.
func newDeps(p *fakePane, b *fakeBus, getenv map[string]string) Deps {
	return Deps{
		Tmux:   &tmuxtest.Fake{Func: p.run},
		Events: b.events,
		Append: b.append,
		Sleep:  func(context.Context, string) error { return nil },
		Getenv: env(getenv),
		Clock:  clock.Clock{Now: time.Now},
	}
}

// anchored is the window and pane the address gates read: @7 anchored to this crew's
// feat/x window, its lead pane running claude. The window row ends in the tmux
// server identity, the arm's last column.
func anchored() (string, string) {
	return "@7\tfeat/x\t/rep/crew\tc1\treef\t" + tmuxServer, "@7\t%9\tlead\tclaude"
}

// project drops the columns the anchor gate's shorter query does not ask for.
func project(rows string) string {
	if rows == "" {
		return ""
	}
	var out []string
	for _, r := range strings.Split(rows, "\n") {
		f := strings.Split(r, "\t")
		if len(f) >= 5 {
			out = append(out, f[0]+"\t"+f[1]+"\t"+f[3])
		}
	}
	return strings.Join(out, "\n")
}

// sessionRows is the raw bus a session's anchor check reads: a dispatch that opens
// the session and its newest status. `Deps.Events` is the log, not the fold — the
// fold is the code under test here.
func sessionRows(t *testing.T, sid string, state string) []any {
	t.Helper()
	session := sid[strings.LastIndex(sid, "#")+1:]
	return rows(t, fmt.Sprintf(`{"ts":1,"crew_id":"c1","kind":"dispatch","branch":"feat/x","session":%q}`, session),
		fmt.Sprintf(`{"ts":2,"crew_id":"c1","kind":"status","from":%q,"body":{"state":%q}}`, sid, state))
}

func rows(t *testing.T, lines ...string) []any {
	t.Helper()
	return anyRows(t, lines...)
}

func req(engine string) PaneRequest {
	return PaneRequest{
		Pane: "%9", Engine: engine, Session: idleReq, Crew: "c1",
		Actor: "dispatcher:c1", MsgTS: idleTS,
	}
}

func TestPaneRefusalsBeforeTyping(t *testing.T) {
	wins, panes := anchored()
	for _, tc := range []struct {
		name string
		req  PaneRequest
		pane *fakePane
		bus  *fakeBus
		want string
	}{
		{
			name: "branch-only id",
			req:  PaneRequest{Pane: "%9", Engine: "claude", Session: "worker:feat/x", Crew: "c1", Actor: "d", MsgTS: idleTS},
			pane: &fakePane{}, bus: &fakeBus{},
			want: "'worker:feat/x' is not a sessioned worker id",
		},
		{
			name: "branch with a hash",
			req:  PaneRequest{Pane: "%9", Engine: "claude", Session: "worker:feat/a#b", Crew: "c1", Actor: "d", MsgTS: idleTS},
			pane: &fakePane{}, bus: &fakeBus{},
			want: "'worker:feat/a#b' is not a sessioned worker id",
		},
		{
			name: "empty msg ts",
			req:  PaneRequest{Pane: "%9", Engine: "claude", Session: idleReq, Crew: "c1", Actor: "d"},
			pane: &fakePane{}, bus: &fakeBus{},
			want: "msg ts '' is not a ms timestamp",
		},
		{
			name: "non-digit msg ts",
			req:  PaneRequest{Pane: "%9", Engine: "claude", Session: idleReq, Crew: "c1", Actor: "d", MsgTS: "17ab"},
			pane: &fakePane{}, bus: &fakeBus{},
			want: "msg ts '17ab' is not a ms timestamp",
		},
		{
			name: "codex lead",
			req:  req("codex"),
			pane: &fakePane{}, bus: &fakeBus{},
			want: "no verified idle lead frame for codex",
		},
		{
			name: "no engine recorded",
			req:  req(""),
			pane: &fakePane{}, bus: &fakeBus{},
			want: "no verified idle lead frame for an unknown engine",
		},
		{
			name: "unreadable pane",
			req:  req("claude"),
			pane: &fakePane{frames: []string{""}, windows: wins, panes: panes}, bus: &fakeBus{},
			want: "pane unreadable",
		},
		{
			name: "claude option-select dialog",
			req:  req("claude"),
			pane: &fakePane{frames: []string{fixture(t, "prompt_select.txt")}, windows: wins, panes: panes}, bus: &fakeBus{},
			want: "dialog on screen (option-select or workspace trust)",
		},
		{
			name: "claude workspace trust dialog",
			req:  req("claude"),
			pane: &fakePane{frames: []string{fixture(t, "prompt_trust.txt")}, windows: wins, panes: panes}, bus: &fakeBus{},
			want: "dialog on screen (option-select or workspace trust)",
		},
		{
			name: "permission dialog",
			req:  req("claude"),
			pane: &fakePane{frames: []string{fixture(t, "rw_permission.txt")}, windows: wins, panes: panes}, bus: &fakeBus{},
			want: "permission dialog on screen",
		},
		{
			name: "quota frame",
			req:  req("claude"),
			pane: &fakePane{frames: []string{fixture(t, "prompt_quota.txt")}, windows: wins, panes: panes}, bus: &fakeBus{},
			want: "quota frame on screen",
		},
		{
			name: "session limit frame",
			req:  req("claude"),
			pane: &fakePane{frames: []string{fixture(t, "session_limit_refusal.txt")}, windows: wins, panes: panes}, bus: &fakeBus{},
			want: "quota frame on screen",
		},
		{
			name: "live turn",
			req:  req("claude"),
			pane: &fakePane{frames: []string{fixture(t, "meter.2s.1.2k.txt")}, windows: wins, panes: panes}, bus: &fakeBus{},
			want: "live turn or no idle input box",
		},
		{
			name: "unsent draft",
			req:  req("claude"),
			pane: &fakePane{frames: []string{fixture(t, "nudge_draft.txt")}, windows: wins, panes: panes}, bus: &fakeBus{},
			want: "unsent input in the input box",
		},
		{
			name: "no finished-turn marker",
			req:  req("claude"),
			pane: &fakePane{frames: []string{fixture(t, "nudge_fresh.txt")}, windows: wins, panes: panes}, bus: &fakeBus{},
			want: "no finished-turn marker above the input box",
		},
		{
			name: "pi mid-turn",
			req:  req("pi"),
			pane: &fakePane{frames: []string{fixture(t, "nudge_pi_live.txt")}, windows: wins, panes: panes}, bus: &fakeBus{},
			want: "live turn or no idle input box",
		},
		{
			name: "pi draft",
			req:  req("pi"),
			pane: &fakePane{frames: []string{fixture(t, "nudge_pi_draft.txt")}, windows: wins, panes: panes}, bus: &fakeBus{},
			want: "unsent input in the input box",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.pane.captures, tc.pane.colored = 0, 0
			out, rc := Pane(context.Background(), tc.req, newDeps(tc.pane, tc.bus, nil))
			if rc != exitRefused || out != tc.want {
				t.Errorf("Pane = (%q, %d), want (%q, %d)", out, rc, tc.want, exitRefused)
			}
			if len(tc.bus.appended) != 0 {
				t.Errorf("a refusal appended %d rows, want none", len(tc.bus.appended))
			}
			for _, c := range tc.pane.tmux {
				if strings.HasPrefix(c, "send-keys") {
					t.Errorf("a refusal typed: %q", c)
				}
			}
		})
	}
}

func TestPaneAnchorRefusals(t *testing.T) {
	wins, panes := anchored()
	for _, tc := range []struct {
		name string
		pane *fakePane
		bus  *fakeBus
		want string
	}{
		{
			name: "windows unreadable",
			pane: &fakePane{frames: []string{fixture(t, "done_idle.txt")}, failWindows: true, panes: panes},
			bus:  &fakeBus{rows: sessionRows(t, idleReq, "working")},
			want: "anchor: cannot read tmux windows",
		},
		{
			name: "panes unreadable",
			pane: &fakePane{frames: []string{fixture(t, "done_idle.txt")}, windows: wins, failPanes: true},
			bus:  &fakeBus{rows: sessionRows(t, idleReq, "working")},
			want: "anchor: cannot read tmux panes",
		},
		{
			name: "pane gone",
			pane: &fakePane{frames: []string{fixture(t, "done_idle.txt")}, windows: wins, panes: "@7\t%8\tlead\tclaude"},
			bus:  &fakeBus{rows: sessionRows(t, idleReq, "working")},
			want: "anchor: no pane %9",
		},
		{
			name: "other crew",
			pane: &fakePane{frames: []string{fixture(t, "done_idle.txt")}, windows: "@7\tfeat/x\t/rep/crew\tc2\treef\t" + tmuxServer, panes: panes},
			bus:  &fakeBus{rows: sessionRows(t, idleReq, "working")},
			want: "anchor: window @7 of %9 is not anchored to crew c1",
		},
		{
			name: "window of another branch",
			pane: &fakePane{frames: []string{fixture(t, "done_idle.txt")}, windows: "@7\tfeat/other\t/rep/crew\tc1\treef\t" + tmuxServer, panes: panes},
			bus:  &fakeBus{rows: sessionRows(t, idleReq, "working")},
			want: "anchor: window @7 of %9 is not feat/x's window",
		},
		{
			name: "role pane",
			pane: &fakePane{frames: []string{fixture(t, "done_idle.txt")}, windows: wins, panes: "@7\t%9\treviewer\tclaude"},
			bus:  &fakeBus{rows: sessionRows(t, idleReq, "working")},
			want: "anchor: %9 is not the lead pane of @7",
		},
		{
			name: "unlabelled pane in a split window",
			pane: &fakePane{frames: []string{fixture(t, "done_idle.txt")}, windows: wins,
				panes: "@7\t%9\t\tclaude\n@7\t%10\t\tclaude"},
			bus:  &fakeBus{rows: sessionRows(t, idleReq, "working")},
			want: "anchor: %9 is not the lead pane of @7",
		},
		{
			name: "bare shell",
			pane: &fakePane{frames: []string{fixture(t, "done_idle.txt")}, windows: wins, panes: "@7\t%9\tlead\tzsh"},
			bus:  &fakeBus{rows: sessionRows(t, idleReq, "working")},
			want: "anchor: %9 is not running an engine (zsh)",
		},
		{
			name: "no command",
			pane: &fakePane{frames: []string{fixture(t, "done_idle.txt")}, windows: wins, panes: "@7\t%9\tlead\t"},
			bus:  &fakeBus{rows: sessionRows(t, idleReq, "working")},
			want: "anchor: %9 is not running an engine (no command)",
		},
		{
			name: "no session on the branch",
			pane: &fakePane{frames: []string{fixture(t, "done_idle.txt")}, windows: wins, panes: panes},
			bus:  &fakeBus{},
			want: "anchor: " + idleReq + " is not the live newest session on feat/x",
		},
		{
			name: "terminal newest",
			pane: &fakePane{frames: []string{fixture(t, "done_idle.txt")}, windows: wins, panes: panes},
			bus:  &fakeBus{rows: sessionRows(t, idleReq, "done")},
			want: "anchor: " + idleReq + " is not the live newest session on feat/x",
		},
		{
			name: "another session newest",
			pane: &fakePane{frames: []string{fixture(t, "done_idle.txt")}, windows: wins, panes: panes},
			bus:  &fakeBus{rows: sessionRows(t, "worker:feat/x#s200-2", "working")},
			want: "anchor: " + idleReq + " is not the live newest session on feat/x",
		},
		{
			name: "fold that cannot read the bus",
			pane: &fakePane{frames: []string{fixture(t, "done_idle.txt")}, windows: wins, panes: panes},
			bus:  &fakeBus{eventsErr: errors.New("boom")},
			want: "anchor: " + idleReq + " is not the live newest session on feat/x",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, rc := Pane(context.Background(), req("claude"), newDeps(tc.pane, tc.bus, nil))
			if rc != exitRefused || out != tc.want {
				t.Errorf("Pane = (%q, %d), want (%q, %d)", out, rc, tc.want, exitRefused)
			}
			if !strings.HasPrefix(out, "anchor:") {
				t.Errorf("anchor refusals must keep the `anchor:` prefix: %q", out)
			}
		})
	}
}

// acceptedFrames is one accepted nudge's capture sequence: the idle box twice (the
// frame gate runs before and after the anchor gate), the line back in the box after
// the keystroke, and the live turn after Enter.
func acceptedFrames(t *testing.T) []string {
	return []string{
		fixture(t, "done_idle.txt"),
		fixture(t, "done_idle.txt"),
		fixture(t, "nudge_typed.txt"),
		fixture(t, "meter.2s.1.2k.txt"),
	}
}

// typedArgv is the two keystrokes every accepted nudge must send and nothing else:
// the constant line typed literally, then one Enter.
var typedArgv = []string{
	`send-keys -t %9 -l crew inbox "$CREW_WORKER_ID"`,
	"send-keys -t %9 Enter",
}

// keys is the send-keys argv of a call, in order, ignoring the anchor gate's reads.
func keys(p *fakePane) []string {
	var out []string
	for _, c := range p.tmux {
		if strings.HasPrefix(c, "send-keys") {
			out = append(out, c)
		}
	}
	return out
}

// A detached background shell is no reason to refuse a directive, which is what the
// frame gate's skipBg is for: the same accepted nudge on a lead parked on a shell,
// with the shell still on screen after the Enter.
func TestPaneAcceptedClaudeBackgroundShell(t *testing.T) {
	wins, panes := anchored()
	bg := fixture(t, "bgwait_churned.txt")
	p := &fakePane{frames: []string{
		bg, bg,
		fixture(t, "nudge_bgwait_typed.txt"),
		bg,
	}, windows: wins, panes: panes}
	b := &fakeBus{rows: sessionRows(t, idleReq, "working")}
	out, rc := Pane(context.Background(), req("claude"), newDeps(p, b, nil))
	if rc != 0 || !strings.HasPrefix(out, "nudge accepted: %9 ") {
		t.Fatalf("Pane = (%q, %d), want accepted on a background-shell frame", out, rc)
	}
	if got := keys(p); !slices.Equal(got, typedArgv) {
		t.Errorf("keys = %q, want %q", got, typedArgv)
	}
	if got := rowJSON(t, b.appended[0]); !strings.Contains(got, `"result":"accepted"`) {
		t.Errorf("row %s, want accepted", got)
	}
}

// The frame gate runs again between the anchor gate and the keystroke, so a draft a
// human started in that window stops the typing: the refusal is the draft's, and the
// pane never got a key.
func TestPaneRefusesADraftThatAppearsBeforeTyping(t *testing.T) {
	wins, panes := anchored()
	p := &fakePane{frames: []string{
		fixture(t, "done_idle.txt"),
		fixture(t, "nudge_draft.txt"),
	}, windows: wins, panes: panes}
	b := &fakeBus{rows: sessionRows(t, idleReq, "working")}
	out, rc := Pane(context.Background(), req("claude"), newDeps(p, b, nil))
	if rc != exitRefused || out != "unsent input in the input box" {
		t.Errorf("Pane = (%q, %d), want the draft refusal", out, rc)
	}
	if got := keys(p); len(got) != 0 {
		t.Errorf("the re-check typed anyway: %q", got)
	}
	if len(b.appended) != 0 {
		t.Errorf("a refusal records no row: %d", len(b.appended))
	}
}

func TestPaneAcceptedClaude(t *testing.T) {
	wins, panes := anchored()
	p := &fakePane{frames: acceptedFrames(t), windows: wins, panes: panes}
	b := &fakeBus{rows: sessionRows(t, idleReq, "working")}
	out, rc := Pane(context.Background(), req("claude"), newDeps(p, b, map[string]string{"CREW_NUDGE_SETTLE": "0", "CREW_NUDGE_GAP": "0"}))
	if rc != 0 {
		t.Fatalf("Pane = (%q, %d), want accepted", out, rc)
	}
	if want := "nudge accepted: %9 " + idleReq + " — the lead took the line (live turn or emptied input box)"; out != want {
		t.Errorf("out = %q, want %q", out, want)
	}
	// The keystrokes are the whole contract: the fixed line typed literally, then
	// one Enter, in that order, with the two anchor reads between the gates.
	want := []string{
		"list-windows -a -F #{window_id}\t#{@crew_branch}\t#{@crew_id}",
		"list-panes -a -F #{window_id}\t#{pane_id}\t#{@crew_role}\t#{pane_current_command}",
		`send-keys -t %9 -l crew inbox "$CREW_WORKER_ID"`,
		"send-keys -t %9 Enter",
	}
	if strings.Join(p.tmux, "\n") != strings.Join(want, "\n") {
		t.Errorf("tmux argv:\n%s\nwant:\n%s", strings.Join(p.tmux, "\n"), strings.Join(want, "\n"))
	}
	if p.captures != 4 {
		t.Errorf("%d captures, want 4 (gate, gate, confirm, classify)", p.captures)
	}
	if len(b.appended) != 1 {
		t.Fatalf("%d rows appended, want 1", len(b.appended))
	}
	got := rowJSON(t, b.appended[0])
	for _, want := range []string{
		`"crew_id":"c1"`, `"kind":"nudge"`, `"from":"dispatcher:c1"`, `"to":"worker:feat/x#s100-1"`,
		`"branch":"feat/x"`, `"pane":"%9"`, `"engine":"claude"`, `"result":"accepted"`,
		`"msg_ts":1700000000000`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("row %s lacks %s", got, want)
		}
	}
}

func TestPaneAcceptedPi(t *testing.T) {
	wins, panes := anchored()
	p := &fakePane{frames: []string{
		fixture(t, "nudge_pi_idle.txt"), fixture(t, "nudge_pi_idle.txt"),
		fixture(t, "nudge_pi_typed.txt"), fixture(t, "nudge_pi_live.txt"),
	}, windows: wins, panes: panes}
	b := &fakeBus{rows: sessionRows(t, idleReq, "working")}
	out, rc := Pane(context.Background(), req("pi"), newDeps(p, b, nil))
	if rc != 0 || !strings.HasPrefix(out, "nudge accepted: %9") {
		t.Fatalf("Pane = (%q, %d), want accepted", out, rc)
	}
	if got := keys(p); !slices.Equal(got, typedArgv) {
		t.Errorf("keys = %q, want %q", got, typedArgv)
	}
	if got := rowJSON(t, b.appended[0]); !strings.Contains(got, `"result":"accepted"`) ||
		!strings.Contains(got, `"engine":"pi"`) {
		t.Errorf("row %s, want accepted on pi", got)
	}
}

// D9's continue line is the other text Pane types: the same gates, its own argv,
// and msg_ts 0 on the row so an auto-continue never reads as covering a directive.
func TestPaneContinueLine(t *testing.T) {
	wins, panes := anchored()
	r := req("pi")
	r.Line, r.MsgTS, r.Actor = ContinueLine, "0", "watchdog"
	p := &fakePane{frames: []string{
		fixture(t, "nudge_pi_idle.txt"), fixture(t, "nudge_pi_idle.txt"),
		strings.ReplaceAll(fixture(t, "nudge_pi_typed.txt"), `crew inbox "$CREW_WORKER_ID"`, ContinueLine),
		fixture(t, "nudge_pi_live.txt"),
	}, windows: wins, panes: panes}
	b := &fakeBus{rows: sessionRows(t, idleReq, "working")}
	out, rc := Pane(context.Background(), r, newDeps(p, b, nil))
	if rc != 0 || !strings.HasPrefix(out, "nudge accepted: %9") {
		t.Fatalf("Pane = (%q, %d), want accepted", out, rc)
	}
	typed := "send-keys -t %9 -l " + ContinueLine
	found := false
	for _, a := range p.tmux {
		found = found || a == typed
	}
	if !found {
		t.Errorf("no argv typing the continue line; tmux:\n%s", strings.Join(p.tmux, "\n"))
	}
	got := rowJSON(t, b.appended[0])
	for _, want := range []string{`"from":"watchdog"`, `"msg_ts":0`, `"result":"accepted"`} {
		if !strings.Contains(got, want) {
			t.Errorf("row %s lacks %s", got, want)
		}
	}
}

// A pi lead whose box is empty again after Enter took the line: accepted, not held.
func TestPanePiEmptiedBox(t *testing.T) {
	wins, panes := anchored()
	p := &fakePane{frames: []string{
		fixture(t, "nudge_pi_idle.txt"), fixture(t, "nudge_pi_idle.txt"),
		fixture(t, "nudge_pi_typed.txt"), fixture(t, "nudge_pi_idle.txt"),
	}, windows: wins, panes: panes}
	b := &fakeBus{rows: sessionRows(t, idleReq, "working")}
	out, rc := Pane(context.Background(), req("pi"), newDeps(p, b, nil))
	if rc != 0 || !strings.Contains(out, "nudge accepted") {
		t.Errorf("Pane = (%q, %d), want accepted", out, rc)
	}
}

const notDeliveredDetail = "the lead took the line but its read mark has not advanced past the msg; it may still read it (crew where, or re-check later), never re-typed"

func TestPaneTypedButNotAccepted(t *testing.T) {
	wins, panes := anchored()
	for _, tc := range []struct {
		name   string
		frames []string
		enter  bool // enterFails
		keys   []string
		want   string
		detail string
		// delivered is Deps.Delivered, handed the 1-based poll number; nil leaves the
		// check off. polls is how many times Pane is expected to have asked.
		delivered func(poll int) bool
		env       map[string]string
		polls     int
		// sleepFail is the 1-based poll sleep that errors, as a cancelled context's
		// does; 0 never fails. Its poll is then the last.
		sleepFail int
	}{
		{
			name: "held: the line is still in the box after one Enter",
			frames: []string{
				fixture(t, "done_idle.txt"), fixture(t, "done_idle.txt"),
				fixture(t, "nudge_typed.txt"), fixture(t, "nudge_typed.txt"),
			},
			keys:   typedArgv,
			want:   "held",
			detail: "the line still sits in the input box after one Enter; not re-sent — a human must submit or clear it (crew where)",
		},
		{
			name: "unconfirmed: the box never showed the line, so no Enter",
			frames: []string{
				fixture(t, "done_idle.txt"), fixture(t, "done_idle.txt"),
				fixture(t, "done_idle.txt"), fixture(t, "done_idle.txt"),
			},
			keys:   typedArgv[:1],
			want:   "unconfirmed",
			detail: "typed but not confirmed in the input box; no Enter sent — the text may be left as an unsent draft a human must clear (crew where), never re-typed",
		},
		{
			name: "unknown: the Enter failed",
			frames: []string{
				fixture(t, "done_idle.txt"), fixture(t, "done_idle.txt"), fixture(t, "nudge_typed.txt"),
			},
			enter:  true,
			keys:   typedArgv,
			want:   "unknown",
			detail: "typed and confirmed, but the Enter failed; the line may sit in the input box (crew where), never re-typed",
		},
		{
			// A draft that is not the line we typed: neither a live turn nor the
			// held line, so the pane is unverifiable.
			name:   "unknown: neither a live turn nor the held line",
			frames: []string{fixture(t, "done_idle.txt"), fixture(t, "done_idle.txt"), fixture(t, "nudge_typed.txt"), fixture(t, "nudge_draft.txt")},
			keys:   typedArgv,
			want:   "unknown",
			detail: "the pane after Enter is neither a live turn nor the held line; verify it (crew where), never re-typed",
			//
		},
		{
			// The pane looks accepted, but the lead's mark never covers the msg: one
			// typed row that says so, and the line is not typed again.
			name:      "not delivered: accepted by the pane, the lead's mark never advances",
			frames:    acceptedFrames(t),
			keys:      typedArgv,
			want:      "typed, not delivered",
			detail:    notDeliveredDetail,
			delivered: func(int) bool { return false },
			polls:     20,
		},
		{
			name:      "not delivered: CREW_NUDGE_DELIVER bounds the polls",
			frames:    acceptedFrames(t),
			keys:      typedArgv,
			want:      "typed, not delivered",
			detail:    notDeliveredDetail,
			delivered: func(int) bool { return false },
			env:       map[string]string{"CREW_NUDGE_DELIVER": "3"},
			polls:     3,
		},
		{
			name:      "not delivered: CREW_NUDGE_DELIVER=0 is the default, not no polls",
			frames:    acceptedFrames(t),
			keys:      typedArgv,
			want:      "typed, not delivered",
			detail:    notDeliveredDetail,
			delivered: func(int) bool { return false },
			env:       map[string]string{"CREW_NUDGE_DELIVER": "0"},
			polls:     20,
		},
		{
			name:      "not delivered: a negative CREW_NUDGE_DELIVER is the default",
			frames:    acceptedFrames(t),
			keys:      typedArgv,
			want:      "typed, not delivered",
			detail:    notDeliveredDetail,
			delivered: func(int) bool { return false },
			env:       map[string]string{"CREW_NUDGE_DELIVER": "-2"},
			polls:     20,
		},
		{
			name:      "not delivered: a failing sleep stops the polling",
			frames:    acceptedFrames(t),
			keys:      typedArgv,
			want:      "typed, not delivered",
			detail:    notDeliveredDetail,
			delivered: func(int) bool { return false },
			sleepFail: 2,
			polls:     2,
		},
		{
			name:      "accepted: the mark advances on the third poll",
			frames:    acceptedFrames(t),
			keys:      typedArgv,
			want:      "accepted",
			detail:    "the lead took the line (live turn or emptied input box)",
			delivered: func(poll int) bool { return poll >= 3 },
			polls:     3,
		},
		{
			name:   "accepted: no Delivered check leaves accepted alone",
			frames: acceptedFrames(t),
			keys:   typedArgv,
			want:   "accepted",
			detail: "the lead took the line (live turn or emptied input box)",
		},
		{
			// held is already not accepted, so the mark is never asked about.
			name: "held: Delivered is not polled",
			frames: []string{
				fixture(t, "done_idle.txt"), fixture(t, "done_idle.txt"),
				fixture(t, "nudge_typed.txt"), fixture(t, "nudge_typed.txt"),
			},
			keys:      typedArgv,
			want:      "held",
			detail:    "the line still sits in the input box after one Enter; not re-sent — a human must submit or clear it (crew where)",
			delivered: func(int) bool { return true },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &fakePane{frames: tc.frames, windows: wins, panes: panes, enterFails: tc.enter}
			b := &fakeBus{rows: sessionRows(t, idleReq, "working")}
			d := newDeps(p, b, tc.env)
			polls := 0
			if tc.sleepFail > 0 {
				// The settle and the gap sleep come before the poll sleeps.
				n := 0
				d.Sleep = func(context.Context, string) error {
					if n++; n == tc.sleepFail+2 {
						return context.Canceled
					}
					return nil
				}
			}
			if tc.delivered != nil {
				d.Delivered = func(context.Context, PaneRequest) bool {
					polls++
					return tc.delivered(polls)
				}
			}
			out, rc := Pane(context.Background(), req("claude"), d)
			wantRC := exitTyped
			if tc.want == "accepted" {
				wantRC = 0
			}
			if rc != wantRC {
				t.Errorf("rc = %d, want %d (%q)", rc, wantRC, out)
			}
			if polls != tc.polls {
				t.Errorf("Delivered polled %d times, want %d", polls, tc.polls)
			}
			if want := "nudge " + tc.want + ": %9 " + idleReq + " — " + tc.detail; out != want {
				t.Errorf("out = %q, want %q", out, want)
			}
			// Every typed attempt records exactly one row, and the keystrokes are the
			// line and at most one Enter: none when unconfirmed, never a second one.
			if got := keys(p); !slices.Equal(got, tc.keys) {
				t.Errorf("keys = %q, want %q", got, tc.keys)
			}
			if len(b.appended) != 1 {
				t.Fatalf("%d rows, want 1", len(b.appended))
			}
			if got := rowJSON(t, b.appended[0]); !strings.Contains(got, `"kind":"nudge"`) ||
				!strings.Contains(got, `"result":"`+tc.want+`"`) {
				t.Errorf("row %s, want a nudge row with result %q", got, tc.want)
			}
		})
	}
}

// The line the box wrapped onto two rows still compares equal, because boxText drops
// whitespace: that is the whole reason the comparison is whitespace-free.
func TestPaneWrappedLineAccepted(t *testing.T) {
	wins, panes := anchored()
	p := &fakePane{frames: []string{
		fixture(t, "done_idle.txt"), fixture(t, "done_idle.txt"),
		fixture(t, "nudge_typed_wrapped.txt"), fixture(t, "meter.2s.1.2k.txt"),
	}, windows: wins, panes: panes}
	b := &fakeBus{rows: sessionRows(t, idleReq, "working")}
	out, rc := Pane(context.Background(), req("claude"), newDeps(p, b, nil))
	if rc != 0 {
		t.Fatalf("a wrapped line must still confirm, got (%q, %d)", out, rc)
	}
	if got := keys(p); !slices.Equal(got, typedArgv) {
		t.Errorf("keys = %q, want %q", got, typedArgv)
	}
	if got := rowJSON(t, b.appended[0]); !strings.Contains(got, `"result":"accepted"`) {
		t.Errorf("row %s, want accepted", got)
	}
}

func TestPaneSendKeysFailureRefuses(t *testing.T) {
	wins, panes := anchored()
	p := &fakePane{frames: []string{fixture(t, "done_idle.txt")}, windows: wins, panes: panes, keysFail: true}
	b := &fakeBus{rows: sessionRows(t, idleReq, "working")}
	out, rc := Pane(context.Background(), req("claude"), newDeps(p, b, nil))
	if rc != exitRefused || out != "send-keys failed" {
		t.Errorf("Pane = (%q, %d), want the send-keys refusal", out, rc)
	}
	if len(b.appended) != 0 {
		t.Error("a refused keystroke records no row")
	}
}

// The arm ran `_nudge_pane` inside `$(…)` on the left of `||`, where bash suppresses
// errexit: a bus it could not write lost the row and the pane's outcome still printed,
// because the line really was typed and a human has to know the box may hold it.
func TestPaneAppendFailureStillReportsTheOutcome(t *testing.T) {
	wins, panes := anchored()
	p := &fakePane{frames: acceptedFrames(t), windows: wins, panes: panes}
	b := &fakeBus{rows: sessionRows(t, idleReq, "working")}
	d := newDeps(p, b, nil)
	d.Append = func(any) error { return errors.New("read-only bus") }
	var warned string
	d.Warn = func(msg string) { warned = msg }
	out, rc := Pane(context.Background(), req("claude"), d)
	if rc != 0 || !strings.HasPrefix(out, "nudge accepted: %9 "+idleReq) {
		t.Errorf("Pane = (%q, %d), want the accepted outcome at 0", out, rc)
	}
	if !strings.Contains(warned, "read-only bus") || !strings.Contains(warned, "could not be written") {
		t.Errorf("warned = %q, want the nudge row and its reason", warned)
	}
}

// The nudge row's ts is jq's `now*1000|floor`: real time. CREW_CLOCK moves the wait
// rows and the loop's waits, never this stamp.
func TestNudgeRowStampIgnoresCrewClock(t *testing.T) {
	wins, panes := anchored()
	clockFile := t.TempDir() + "/clock"
	if err := os.WriteFile(clockFile, []byte("1000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := &fakePane{frames: acceptedFrames(t), windows: wins, panes: panes}
	b := &fakeBus{rows: sessionRows(t, idleReq, "working")}
	d := newDeps(p, b, nil)
	d.Clock = clock.Clock{Now: time.Now, CrewClock: clockFile}
	if _, rc := Pane(context.Background(), req("claude"), d); rc != 0 {
		t.Fatalf("want accepted")
	}
	n := float64(b.appended[0].(nudgeRowDoc).TS)
	if n < float64(time.Now().Add(-time.Hour).UnixMilli()) {
		t.Errorf("row ts %v is not real time under CREW_CLOCK", n)
	}
}

// rowJSON is an appended row as the bus line it becomes.
func rowJSON(t *testing.T, row any) string {
	t.Helper()
	b, err := json.Marshal(row)
	if err != nil {
		t.Fatalf("marshal row: %v", err)
	}
	return string(b)
}

func TestDigitsLiteral(t *testing.T) {
	for in, want := range map[string]string{"0": "0", "000": "0", "007": "7", "1700000000000": "1700000000000"} {
		if got := digitsLiteral(in); got != want {
			t.Errorf("digitsLiteral(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNudgeRowLeadingZeroMsgTS(t *testing.T) {
	r := req("claude")
	r.MsgTS = "007"
	if got := rowJSON(t, nudgeRow(r, "accepted", "", 1)); !strings.Contains(got, `"msg_ts":7`) {
		t.Errorf("row %s lacks msg_ts 7", got)
	}
}

// The turn a blocked tool call ended: an Interrupted row above an empty box has no
// finished-turn marker, yet the lead waits for direction, so the nudge goes through.
func TestPaneAcceptedClaudeInterrupted(t *testing.T) {
	wins, panes := anchored()
	idle := fixture(t, "nudge_interrupted.txt")
	typed := strings.Replace(idle, "\n❯\n", "\n❯ crew inbox \"$CREW_WORKER_ID\"\n", 1)
	p := &fakePane{frames: []string{idle, idle, typed, fixture(t, "meter.2s.1.2k.txt")}, windows: wins, panes: panes}
	b := &fakeBus{rows: sessionRows(t, idleReq, "working")}
	out, rc := Pane(context.Background(), req("claude"), newDeps(p, b, map[string]string{"CREW_NUDGE_SETTLE": "0", "CREW_NUDGE_GAP": "0"}))
	if rc != 0 || !strings.HasPrefix(out, "nudge accepted: %9 ") {
		t.Fatalf("Pane = (%q, %d), want accepted on an Interrupted frame", out, rc)
	}
	if got := keys(p); !slices.Equal(got, typedArgv) {
		t.Errorf("keys = %q, want %q", got, typedArgv)
	}
}

// The Interrupted allowance covers only the missing finished-turn marker: any other
// busy signal on the same frame still refuses.
func TestPaneRefusesInterruptedFramesThatAreNotIdle(t *testing.T) {
	wins, panes := anchored()
	idle := fixture(t, "nudge_interrupted.txt")
	limit := "  ⎿  You've hit your session limit · resets 7pm (Asia/Jerusalem)\n     /upgrade to increase your usage limit.\n" + idle
	limit = strings.Replace(limit, "  -- INSERT -- ⏵⏵ auto mode on · ← for agents", "  ⚠ /low-priority to continue now at lower priority · uses your weekly limit", 1)
	cases := []struct {
		name  string
		frame string
		want  string
	}{
		{"typed input", strings.Replace(idle, "\n❯\n", "\n❯ hello draft text\n", 1), "unsent input in the input box"},
		{"live turn", strings.Replace(idle, "  ⎿  Interrupted", "✻ Churning… (2s · ↓ 1.2k tokens)\n  ⎿  Interrupted", 1), "live turn or no idle input box"},
		{"session limit", limit, "quota frame on screen"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !frame.InterruptedAboveBox(tc.frame) {
				t.Fatal("setup: Interrupted row is not directly above the box")
			}
			p := &fakePane{frames: []string{tc.frame}, windows: wins, panes: panes}
			out, rc := Pane(context.Background(), req("claude"), newDeps(p, &fakeBus{}, nil))
			if rc != exitRefused || out != tc.want {
				t.Errorf("Pane = (%q, %d), want (%q, %d)", out, rc, tc.want, exitRefused)
			}
			if got := keys(p); len(got) != 0 {
				t.Errorf("typed into a refused pane: %q", got)
			}
		})
	}
}

// tickClock is a clock the test moves by hand.
type tickClock struct{ now time.Time }

func (c *tickClock) read() time.Time { return c.now }

func agentDeps(t *testing.T, p *fakePane, clk *tickClock) Deps {
	d := newDeps(p, &fakeBus{rows: sessionRows(t, idleReq, "working")}, map[string]string{"CREW_NUDGE_SETTLE": "0", "CREW_NUDGE_GAP": "0"})
	d.Clock = clock.Clock{Now: clk.read}
	return d
}

// A claude lead idle after a background agent finished has no done marker; the
// frame counts as idle only once it held byte-identical across two samples.
func TestPaneAgentFinishedStable(t *testing.T) {
	wins, panes := anchored()
	idle := fixture(t, "nudge_agent_finished.txt")
	typed := strings.Replace(idle, "\n❯\n", "\n❯ crew inbox \"$CREW_WORKER_ID\"\n", 1)
	clk := &tickClock{now: time.Unix(1800000000, 0)}
	sample := &AgentSample{}
	r := req("claude")
	r.Agent = sample

	p := &fakePane{frames: []string{idle}, windows: wins, panes: panes}
	out, rc := Pane(context.Background(), r, agentDeps(t, p, clk))
	if rc != exitRefused || out != ReasonAgentPending {
		t.Fatalf("first sample = (%q, %d), want pending refusal", out, rc)
	}
	clk.now = clk.now.Add((agentStableSecs - 1) * time.Second)
	if out, rc = Pane(context.Background(), r, agentDeps(t, p, clk)); rc != exitRefused || out != ReasonAgentPending {
		t.Fatalf("before the window = (%q, %d), want pending refusal", out, rc)
	}
	if got := keys(p); len(got) != 0 {
		t.Fatalf("typed before the frame was stable: %q", got)
	}

	clk.now = clk.now.Add(time.Second)
	p = &fakePane{frames: []string{idle, idle, typed, fixture(t, "meter.2s.1.2k.txt")}, windows: wins, panes: panes}
	out, rc = Pane(context.Background(), r, agentDeps(t, p, clk))
	if rc != 0 || !strings.HasPrefix(out, "nudge accepted: %9 ") {
		t.Fatalf("stable sample = (%q, %d), want accepted", out, rc)
	}
	if got := keys(p); !slices.Equal(got, typedArgv) {
		t.Errorf("keys = %q, want %q", got, typedArgv)
	}
}

func TestPaneAgentFinishedChanged(t *testing.T) {
	wins, panes := anchored()
	idle := fixture(t, "nudge_agent_finished.txt")
	other := strings.Replace(idle, "5m 8s", "5m 9s", 1)
	clk := &tickClock{now: time.Unix(1800000000, 0)}
	r := req("claude")
	r.Agent = &AgentSample{}

	p := &fakePane{frames: []string{idle}, windows: wins, panes: panes}
	Pane(context.Background(), r, agentDeps(t, p, clk))
	clk.now = clk.now.Add(agentStableSecs * 3 * time.Second)
	p = &fakePane{frames: []string{other}, windows: wins, panes: panes}
	out, rc := Pane(context.Background(), r, agentDeps(t, p, clk))
	if rc != exitRefused || out != ReasonAgentPending {
		t.Fatalf("changed frame = (%q, %d), want pending refusal", out, rc)
	}
	if got := keys(p); len(got) != 0 {
		t.Errorf("typed into a frame that changed: %q", got)
	}
}

func TestPaneAgentFinishedSingleLook(t *testing.T) {
	wins, panes := anchored()
	p := &fakePane{frames: []string{fixture(t, "nudge_agent_finished.txt")}, windows: wins, panes: panes}
	out, rc := Pane(context.Background(), req("claude"), newDeps(p, &fakeBus{}, nil))
	if rc != exitRefused || out != ReasonAgentOnce || !strings.Contains(out, "--wait") {
		t.Fatalf("Pane = (%q, %d), want the --wait hint refusal", out, rc)
	}
	if got := keys(p); len(got) != 0 {
		t.Errorf("typed on a single look: %q", got)
	}
}

// A frame that is still idle but changed after the stability proof is a new
// completion row, not the one that was confirmed: it never types.
func TestPaneAgentFinishedChangedAfterConfirm(t *testing.T) {
	wins, panes := anchored()
	a := fixture(t, "nudge_agent_finished.txt")
	a2 := strings.Replace(a, "5m 8s", "5m 9s", 1)
	clk := &tickClock{now: time.Unix(1800000000, 0)}
	r := req("claude")
	r.Agent = &AgentSample{frame: a, at: clk.now.Unix() - 2*agentStableSecs}
	p := &fakePane{frames: []string{a, a2}, windows: wins, panes: panes}
	out, rc := Pane(context.Background(), r, agentDeps(t, p, clk))
	if rc != exitRefused || out != ReasonAgentPending {
		t.Fatalf("Pane = (%q, %d), want pending refusal", out, rc)
	}
	if got := keys(p); len(got) != 0 {
		t.Errorf("typed into a changed frame: %q", got)
	}
}
