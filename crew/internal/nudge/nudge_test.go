package nudge

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/clock"
	"github.com/noamsto/dispatcher/crew/internal/jsonv"
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

// capture counts the two kinds separately, as the bats tmux stub does: the plain
// captures are the ones a row scripts (gate, gate-again, confirm, classify), and the
// colored read answers the same frame because every fixture here is unstyled.
func (f *fakePane) capture(_ context.Context, pane string, colored bool) (string, bool) {
	if pane != "%9" {
		return "", false
	}
	if colored {
		f.colored++
		return f.frameAt(f.colored - 1), true
	}
	f.captures++
	return f.frameAt(f.captures - 1), true
}

func (f *fakePane) frameAt(i int) string {
	if i >= len(f.frames) {
		return f.frames[len(f.frames)-1]
	}
	return f.frames[i]
}

func (f *fakePane) run(_ context.Context, args ...string) (string, bool) {
	f.tmux = append(f.tmux, strings.Join(args, " "))
	switch args[0] {
	case "list-windows":
		if f.failWindows {
			return "", false
		}
		// The bats stub answers per query: the arm asks for five columns including
		// @crew_dir, the pane's own anchor gate for three. One fixture, projected.
		if !strings.Contains(args[len(args)-1], "@crew_dir") {
			return project(f.windows), true
		}
		return f.windows, true
	case "list-panes":
		if f.failPanes {
			return "", false
		}
		return f.panes, true
	case "send-keys":
		if len(args) > 3 && args[3] == "Enter" {
			return "", !f.enterFails
		}
		return "", !f.keysFail
	}
	return "", true
}

// fakeBus is the anchor's fold input and the rows the call appended.
type fakeBus struct {
	rows      []jsonv.Value
	appended  []jsonv.Value
	eventsErr error
}

func (b *fakeBus) events() ([]jsonv.Value, error) { return b.rows, b.eventsErr }

func (b *fakeBus) append(row jsonv.Value) error {
	b.appended = append(b.appended, row)
	return nil
}

// row is a bus row from JSON text, the way a test writes one on the log.
func row(t *testing.T, text string) jsonv.Value {
	t.Helper()
	vs, err := jsonv.DecodeStream(strings.NewReader(text))
	if err != nil || len(vs) != 1 {
		t.Fatalf("row %q: %v", text, err)
	}
	return vs[0]
}

// env is a Deps.Getenv that answers from a map and defaults to "".
func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// newDeps is the Deps under test: fakes for every side effect, a real clock so the
// row's `now*1000|floor` stamp is checked against the wall clock.
func newDeps(p *fakePane, b *fakeBus, getenv map[string]string) Deps {
	return Deps{
		Capture: p.capture,
		Tmux:    p.run,
		Events:  b.events,
		Append:  b.append,
		Sleep:   func(context.Context, string) error { return nil },
		Getenv:  env(getenv),
		Clock:   clock.Clock{Now: time.Now},
	}
}

// anchored is the window and pane the address gates read: @7 anchored to this crew's
// feat/x window, its lead pane running claude.
func anchored() (string, string) {
	return "@7\tfeat/x\t/rep/crew\tc1\treef", "@7\t%9\tlead\tclaude"
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
func sessionRows(t *testing.T, sid string, state string) []jsonv.Value {
	t.Helper()
	session := sid[strings.LastIndex(sid, "#")+1:]
	return rows(t, fmt.Sprintf(`{"ts":1,"crew_id":"c1","kind":"dispatch","branch":"feat/x","session":%q}`, session),
		fmt.Sprintf(`{"ts":2,"crew_id":"c1","kind":"status","from":%q,"body":{"state":%q}}`, sid, state))
}

func rows(t *testing.T, lines ...string) []jsonv.Value {
	t.Helper()
	vs, err := jsonv.DecodeStream(strings.NewReader(strings.Join(lines, "\n")))
	if err != nil || len(vs) != len(lines) {
		t.Fatalf("rows: %v", err)
	}
	return vs
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
			pane: &fakePane{frames: []string{fixture(t, "done_idle.txt")}, windows: "@7\tfeat/x\t/rep/crew\tc2\treef", panes: panes},
			bus:  &fakeBus{rows: sessionRows(t, idleReq, "working")},
			want: "anchor: window @7 of %9 is not anchored to crew c1",
		},
		{
			name: "window of another branch",
			pane: &fakePane{frames: []string{fixture(t, "done_idle.txt")}, windows: "@7\tfeat/other\t/rep/crew\tc1\treef", panes: panes},
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
	got := string(jsonv.Append(nil, b.appended[0], jsonv.Options{}))
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
	got := string(jsonv.Append(nil, b.appended[0], jsonv.Options{}))
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

func TestPaneTypedButNotAccepted(t *testing.T) {
	wins, panes := anchored()
	for _, tc := range []struct {
		name   string
		frames []string
		enter  bool // enterFails
		enters int  // Enters the call may send: never twice, none when unconfirmed
		want   string
		detail string
	}{
		{
			name: "held: the line is still in the box after one Enter",
			frames: []string{
				fixture(t, "done_idle.txt"), fixture(t, "done_idle.txt"),
				fixture(t, "nudge_typed.txt"), fixture(t, "nudge_typed.txt"),
			},
			enters: 1,
			want:   "held",
			detail: "the line still sits in the input box after one Enter; not re-sent — a human must submit or clear it (crew where)",
		},
		{
			name: "unconfirmed: the box never showed the line, so no Enter",
			frames: []string{
				fixture(t, "done_idle.txt"), fixture(t, "done_idle.txt"),
				fixture(t, "done_idle.txt"), fixture(t, "done_idle.txt"),
			},
			enters: 0,
			want:   "unconfirmed",
			detail: "typed but not confirmed in the input box; no Enter sent — the text may be left as an unsent draft a human must clear (crew where), never re-typed",
		},
		{
			name: "unknown: the Enter failed",
			frames: []string{
				fixture(t, "done_idle.txt"), fixture(t, "done_idle.txt"), fixture(t, "nudge_typed.txt"),
			},
			enter:  true,
			enters: 1,
			want:   "unknown",
			detail: "typed and confirmed, but the Enter failed; the line may sit in the input box (crew where), never re-typed",
		},
		{
			// A draft that is not the line we typed: neither a live turn nor the
			// held line, so the pane is unverifiable.
			name:   "unknown: neither a live turn nor the held line",
			frames: []string{fixture(t, "done_idle.txt"), fixture(t, "done_idle.txt"), fixture(t, "nudge_typed.txt"), fixture(t, "nudge_draft.txt")},
			enters: 1,
			want:   "unknown",
			detail: "the pane after Enter is neither a live turn nor the held line; verify it (crew where), never re-typed",
			//
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &fakePane{frames: tc.frames, windows: wins, panes: panes, enterFails: tc.enter}
			b := &fakeBus{rows: sessionRows(t, idleReq, "working")}
			out, rc := Pane(context.Background(), req("claude"), newDeps(p, b, nil))
			if rc != exitTyped {
				t.Errorf("rc = %d, want 3 (%q)", rc, out)
			}
			if want := "nudge " + tc.want + ": %9 " + idleReq + " — " + tc.detail; out != want {
				t.Errorf("out = %q, want %q", out, want)
			}
			// Every typed attempt records exactly one row, and the Enter is never
			// repeated: at most one Enter call, none when unconfirmed.
			enters := 0
			for _, c := range p.tmux {
				if strings.HasSuffix(c, " Enter") {
					enters++
				}
			}
			if enters != tc.enters {
				t.Errorf("%d Enters, want %d", enters, tc.enters)
			}
			if len(b.appended) != 1 {
				t.Errorf("%d rows, want 1", len(b.appended))
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
	if _, rc := Pane(context.Background(), req("claude"), newDeps(p, b, nil)); rc != 0 {
		t.Fatalf("a wrapped line must still confirm, got rc %d", rc)
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
	d.Append = func(jsonv.Value) error { return errors.New("read-only bus") }
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
	ts, _ := b.appended[0].Get("ts")
	n, _ := ts.AsFloat()
	if n < float64(time.Now().Add(-time.Hour).UnixMilli()) {
		t.Errorf("row ts %v is not real time under CREW_CLOCK", n)
	}
}
