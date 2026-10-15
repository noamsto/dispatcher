package nudge

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/clock"
	"github.com/noamsto/dispatcher/crew/internal/jsonv"
)

// armFixture is the CLI under test with its side effects faked: the bus log and the
// paths dir are real (the wait lock and the appended rows are the contract), tmux,
// the unread scan, the log tail, the clock and the sleep are not.
type armFixture struct {
	t      *testing.T
	paths  bus.Paths
	stdout *bytes.Buffer
	stderr *bytes.Buffer
	pane   *fakePane
	bus    *fakeBus

	now    int64
	slept  []string
	env    map[string]string
	crewID string

	// unread is the dispatcher-mode scan pair, "<oldest> <newest>".
	unread string
	// leadReads makes every scan after the first answer empty: the lead read the
	// directive during the wait, which is how --wait stops without typing.
	leadReads   bool
	unreadScans int

	// rows are the log lines the arm reads; toplevel/workerTask drive the
	// dispatcher-only gate.
	rows         []any
	toplevel     string
	workerTask   bool
	releaseAfter int
	releasePath  string
	// holdLock is a live pid owning the wait lock, so the arm joins instead.
	holdLock string
}

const (
	featX  = "feat/x"
	leadID = "%9"
)

func newArm(t *testing.T) *armFixture {
	t.Helper()
	common := t.TempDir()
	f := &armFixture{
		t: t,
		paths: bus.Paths{
			Common: common, Dir: common + "/crew", Log: common + "/crew/events.jsonl",
		},
		stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{},
		pane:   &fakePane{frames: acceptedFrames(t)},
		bus:    &fakeBus{},
		now:    time.Now().UnixMilli(),
		crewID: "c1",
		unread: idleTS + " " + idleTS,
	}
	f.pane.windows, f.pane.panes = f.windowRows("@7", featX, "lead"), f.paneRows("@7", leadID, "lead", "claude")
	f.bus.rows = sessionRows(t, idleReq, "working")
	f.rows = dispatchRows(t, "claude")
	f.pane.captures, f.pane.colored = 0, 0
	return f
}

// windowRows is `list-windows`' answer for one window anchored to this checkout.
func (f *armFixture) windowRows(win, branch, name string) string {
	return win + "\t" + branch + "\t" + f.paths.Dir + "\tc1\t" + name
}

func (f *armFixture) paneRows(win, pane, role, cmd string) string {
	return win + "\t" + pane + "\t" + role + "\t" + cmd
}

func (f *armFixture) run(req Request) int {
	f.t.Helper()
	deps := newDeps(f.pane, f.bus, f.env)
	deps.Sleep = func(_ context.Context, secs string) error {
		f.slept = append(f.slept, secs)
		ms, _ := strconv.Atoi(secs)
		f.now += int64(ms) * 1000
		if f.releasePath != "" && len(f.slept) == f.releaseAfter {
			if err := os.RemoveAll(f.releasePath); err != nil {
				return err
			}
		}
		return nil
	}
	deps.Clock = f.clock()
	if f.holdLock != "" {
		if err := os.MkdirAll(f.lockDir(), 0o755); err != nil {
			f.t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(f.lockDir(), "pid"), []byte(f.holdLock+"\n"), 0o644); err != nil {
			f.t.Fatal(err)
		}
	}
	return Run(context.Background(), req, f.paths, f.stdout, f.stderr, Options{
		Deps:     deps,
		CrewID:   func() string { return f.crewID },
		Getenv:   env(f.env),
		Toplevel: func(context.Context) string { return f.toplevel },
		Exists: func(path string) bool {
			return f.workerTask && strings.HasSuffix(path, "/WORKER_TASK.md")
		},
		Unread: func(context.Context, string, string, string) string {
			f.unreadScans++
			if f.leadReads && f.unreadScans > 1 {
				return ""
			}
			return f.unread
		},
		Rows: func(int) ([]any, bool) { return f.rows, true },
		PID:  4242,
	})
}

// clock is the arm's clock: a fake wall clock the fake sleep advances, so a --wait
// budget is spent without the test spending it.
func (f *armFixture) clock() clock.Clock {
	return clock.Clock{Now: func() time.Time { return time.UnixMilli(f.now) }}
}

// runWith drives the arm with Deps a test has rewritten (a failing bus write),
// leaving the rest of the fixture alone.
func (f *armFixture) runWith(deps Deps, req Request) int {
	f.t.Helper()
	return Run(context.Background(), req, f.paths, f.stdout, f.stderr, Options{
		Deps: deps, CrewID: func() string { return f.crewID }, Getenv: env(f.env),
		Toplevel: func(context.Context) string { return f.toplevel },
		Exists:   func(string) bool { return false },
		Unread: func(context.Context, string, string, string) string {
			return f.unread
		},
		Rows: func(int) ([]any, bool) { return f.rows, true },
		PID:  4242,
	})
}

// ticks are the --wait loop's sleeps: the pane's own settle and gap are shorter, and
// counting only the interval keeps the loop's pacing what the assertion reads.
func (f *armFixture) ticks(interval string) int {
	n := 0
	for _, s := range f.slept {
		if s == interval {
			n++
		}
	}
	return n
}

func (f *armFixture) lockDir() string {
	return filepath.Join(f.paths.Dir, "nudge-wait", lockName(idleReq))
}

func (f *armFixture) out() string { return f.stdout.String() }
func (f *armFixture) errOut() string {
	return strings.TrimPrefix(strings.TrimSuffix(f.stderr.String(), "\n"), "crew: nudge: ")
}

// rowsText is the rows the call appended, one per line, in order.
func (f *armFixture) rowsText() string {
	var b strings.Builder
	for _, r := range f.bus.appended {
		b.WriteString(string(jsonv.Append(nil, r, jsonv.Options{})) + "\n")
	}
	return b.String()
}

func (f *armFixture) rowAt(i int) string {
	lines := strings.Split(strings.TrimRight(f.rowsText(), "\n"), "\n")
	if i >= len(lines) {
		f.t.Fatalf("%d rows appended, want at least %d:\n%s", len(lines), i+1, f.rowsText())
	}
	return lines[i]
}

// anyRows decodes each line into gojq form, numbers as json.Number.
func anyRows(t *testing.T, lines ...string) []any {
	t.Helper()
	out := make([]any, 0, len(lines))
	for _, l := range lines {
		dec := json.NewDecoder(strings.NewReader(l))
		dec.UseNumber()
		var v any
		if err := dec.Decode(&v); err != nil {
			t.Fatalf("anyRows %q: %v", l, err)
		}
		out = append(out, v)
	}
	return out
}

// dispatchRows is the log the engine lookup reads.
func dispatchRows(t *testing.T, engine string) []any {
	t.Helper()
	return anyRows(t, `{"ts":1,"crew_id":"c1","kind":"dispatch","branch":"feat/x","session":"s100-1",`+
		`"worker_id":"`+idleReq+`","engine":"`+engine+`"}`)
}

func TestRunIsDispatcherOnly(t *testing.T) {
	for _, tc := range []struct {
		name       string
		env        map[string]string
		toplevel   string
		workerTask bool
	}{
		{name: "worker session", env: map[string]string{"CREW_WORKER_ID": idleReq}},
		{name: "role session", env: map[string]string{"CREW_ROLE_ID": "role:feat/x#s100-1:reviewer"}},
		{name: "checkout with its own task file", toplevel: "/repo/work", workerTask: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newArm(t)
			f.env, f.toplevel, f.workerTask = tc.env, tc.toplevel, tc.workerTask
			if rc := f.run(Request{Target: idleReq}); rc != 2 {
				t.Errorf("rc = %d, want 2", rc)
			}
			// This refusal prints bare and exits 2: it is not a nudge that refused,
			// it is a caller who may not ask.
			if f.stderr.String() != "crew: nudge is a dispatcher command\n" {
				t.Errorf("stderr = %q", f.stderr.String())
			}
			if len(f.pane.tmux) != 0 {
				t.Errorf("a dispatcher-only refusal touched tmux: %v", f.pane.tmux)
			}
		})
	}
}

func TestRunUsage(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  Request
		want string
	}{
		{"no target", Request{}, "usage: crew nudge"},
		{"pane id target", Request{Target: "%9"}, "a pane id is not an anchored address"},
		{"no crew", Request{Target: featX}, "CREW_ID not set"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newArm(t)
			if tc.name == "no crew" {
				f.crewID = ""
			}
			if rc := f.run(tc.req); rc != 1 {
				t.Errorf("rc = %d, want 1", rc)
			}
			if !strings.Contains(f.errOut(), tc.want) {
				t.Errorf("stderr = %q, want %q", f.errOut(), tc.want)
			}
		})
	}
}

func TestRunResolvesTarget(t *testing.T) {
	t.Run("branch", func(t *testing.T) {
		f := newArm(t)
		if rc := f.run(Request{Target: featX}); rc != 0 {
			t.Fatalf("rc = %d, stderr = %q", rc, f.errOut())
		}
		if !strings.Contains(f.out(), "nudge accepted: "+leadID+" "+idleReq) {
			t.Errorf("out = %q", f.out())
		}
	})

	t.Run("crew from the request beats the checkout's", func(t *testing.T) {
		f := newArm(t)
		f.crewID = ""
		if rc := f.run(Request{Crew: "c1", Target: featX}); rc != 0 {
			t.Fatalf("rc = %d, stderr = %q", rc, f.errOut())
		}
	})
	t.Run("codename", func(t *testing.T) {
		f := newArm(t)
		f.pane.windows = f.windowRows("@7", featX, "reef")
		if rc := f.run(Request{Target: "reef"}); rc != 0 {
			t.Fatalf("rc = %d, stderr = %q", rc, f.errOut())
		}
	})

	t.Run("ambiguous codename", func(t *testing.T) {
		f := newArm(t)
		f.pane.windows = strings.Join([]string{
			f.windowRows("@7", featX, "reef"),
			f.windowRows("@8", "feat/y", "reef"),
		}, "\n")
		if rc := f.run(Request{Target: "reef"}); rc != 1 ||
			!strings.Contains(f.errOut(), "ambiguous codename 'reef' — matches feat/x,feat/y; pass a branch") {
			t.Errorf("rc = %d, stderr = %q", rc, f.errOut())
		}
	})

	t.Run("another checkout's window", func(t *testing.T) {
		f := newArm(t)
		f.pane.windows = "@7\tfeat/x\t/other/crew\tc1\treef"
		if rc := f.run(Request{Target: featX}); rc != 1 || !strings.Contains(f.errOut(), "no worker matches 'feat/x'") {
			t.Errorf("rc = %d, stderr = %q", rc, f.errOut())
		}
	})

	t.Run("another crew's window", func(t *testing.T) {
		f := newArm(t)
		f.pane.windows = "@7\tfeat/x\t" + f.paths.Dir + "\tc2\treef"
		if rc := f.run(Request{Target: featX}); rc != 1 || !strings.Contains(f.errOut(), "no worker matches 'feat/x'") {
			t.Errorf("rc = %d, stderr = %q", rc, f.errOut())
		}
	})

	t.Run("branch with no window falls back to the bus", func(t *testing.T) {
		f := newArm(t)
		f.pane.windows = ""
		// A branch on the bus with no live window: the target resolved, the pane did
		// not survive, which is a different answer from "no worker matches".
		if err := os.MkdirAll(filepath.Dir(f.paths.Log), 0o755); err != nil {
			t.Fatal(err)
		}
		line := `{"ts":1,"crew_id":"c1","kind":"dispatch","branch":"feat/x","worker_id":"` + idleReq + `"}`
		if err := bus.Append(f.paths.Log, line); err != nil {
			t.Fatal(err)
		}
		if rc := f.run(Request{Target: featX}); rc != 1 ||
			!strings.Contains(f.errOut(), "no live pane for 'feat/x' (branch feat/x) — its window is gone") {
			t.Errorf("rc = %d, stderr = %q", rc, f.errOut())
		}
	})

	t.Run("window with no pane", func(t *testing.T) {
		f := newArm(t)
		f.pane.panes = ""
		if rc := f.run(Request{Target: featX}); rc != 1 || !strings.Contains(f.errOut(), "no pane in the window for 'feat/x'") {
			t.Errorf("rc = %d, stderr = %q", rc, f.errOut())
		}
	})

	t.Run("role pane is not the lead", func(t *testing.T) {
		f := newArm(t)
		// The lead pane is found by @crew_role=lead; a window whose only pane is a
		// role pane still resolves, because the arm falls back to any pane.
		f.pane.panes = strings.Join([]string{
			f.paneRows("@7", "%8", "reviewer", "claude"),
			f.paneRows("@7", leadID, "lead", "claude"),
		}, "\n")
		if rc := f.run(Request{Target: featX}); rc != 0 {
			t.Fatalf("rc = %d, stderr = %q", rc, f.errOut())
		}
		for _, c := range f.pane.tmux {
			if strings.Contains(c, "send-keys -t %8") {
				t.Errorf("typed into the role pane: %q", c)
			}
		}
	})

	t.Run("tmux unreadable", func(t *testing.T) {
		f := newArm(t)
		f.pane.failWindows = true
		if rc := f.run(Request{Target: featX}); rc != 1 ||
			!strings.Contains(f.errOut(), "cannot read tmux windows (is a tmux server running?)") {
			t.Errorf("rc = %d, stderr = %q", rc, f.errOut())
		}
		f = newArm(t)
		f.pane.failPanes = true
		if rc := f.run(Request{Target: featX}); rc != 1 ||
			!strings.Contains(f.errOut(), "cannot read tmux panes (is a tmux server running?)") {
			t.Errorf("rc = %d, stderr = %q", rc, f.errOut())
		}
	})
}

func TestRunSessionGate(t *testing.T) {
	for _, tc := range []struct {
		name   string
		rows   []jsonv.Value
		target string
		want   string
	}{
		{
			name: "no session on the branch", rows: nil,
			want: "no session on feat/x — dispatch a worker before nudging one",
		},
		{
			name: "stopped newest session",
			rows: sessionRows(t, idleReq, "done"),
			want: "newest session on feat/x is done — a stopped session never reads its inbox; re-dispatch with the context baked in",
		},
		{
			name: "lead has not started",
			rows: rows(t, `{"ts":1,"crew_id":"c1","kind":"dispatch","branch":"feat/x","session":"s100-1"}`),
			want: "lead has not posted its first status yet — wait for it to start",
		},
		{
			name: "branch-only id on the bus",
			rows: rows(t, `{"ts":2,"crew_id":"c1","kind":"status","from":"worker:feat/x","body":{"state":"working"}}`),
			want: "feat/x has no session id on the bus — a branch-only address can never reach a live worker's inbox; re-dispatch",
		},
		{
			name:   "an older session was named",
			rows:   sessionRows(t, idleReq, "working"),
			target: "worker:feat/x#s9-9",
			want:   "worker:feat/x#s9-9 is not the newest session on feat/x (" + idleReq + ")",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newArm(t)
			f.bus.rows = tc.rows
			target := tc.target
			if target == "" {
				target = featX
			}
			if rc := f.run(Request{Target: target}); rc != 2 {
				t.Errorf("rc = %d, want 2 (a refusal, not a failure)", rc)
			}
			if f.errOut() != tc.want {
				t.Errorf("stderr = %q, want %q", f.errOut(), tc.want)
			}
			for _, c := range f.pane.tmux {
				if strings.HasPrefix(c, "send-keys") {
					t.Errorf("a session refusal typed: %q", c)
				}
			}
		})
	}
}

func TestRunEngineGate(t *testing.T) {
	f := newArm(t)
	f.rows = nil
	if rc := f.run(Request{Target: featX}); rc != 2 ||
		f.errOut() != "no dispatch or resume row records "+idleReq+"'s engine" {
		t.Errorf("rc = %d, stderr = %q", rc, f.errOut())
	}

	// A resume row carries the engine the same way, and the pi frames decide with it.
	f = newArm(t)
	f.rows = anyRows(t, `{"ts":3,"crew_id":"c1","kind":"resume","branch":"feat/x","worker_id":"`+idleReq+`","engine":"pi"}`)
	f.pane.frames = []string{
		fixture(t, "nudge_pi_idle.txt"), fixture(t, "nudge_pi_idle.txt"),
		fixture(t, "nudge_pi_typed.txt"), fixture(t, "nudge_pi_live.txt"),
	}
	if rc := f.run(Request{Target: featX}); rc != 0 {
		t.Errorf("rc = %d, stderr = %q, want accepted on pi frames", rc, f.errOut())
	}
}

func TestRunUnreadGate(t *testing.T) {
	f := newArm(t)
	f.unread = ""
	if rc := f.run(Request{Target: featX}); rc != 2 ||
		!strings.Contains(f.errOut(), "no unread msg from dispatcher:c1 to "+idleReq+
			" — post the directive first (crew reply), then nudge") {
		t.Errorf("rc = %d, stderr = %q", rc, f.errOut())
	}
	for _, c := range f.pane.tmux {
		if strings.HasPrefix(c, "send-keys") {
			t.Errorf("the unread gate typed: %q", c)
		}
	}
}

func TestRunReportsPaneOutcome(t *testing.T) {
	f := newArm(t)
	f.pane.frames = []string{fixture(t, "nudge_draft.txt")}
	if rc := f.run(Request{Target: featX}); rc != 2 {
		t.Fatalf("rc = %d, want 2", rc)
	}
	if f.errOut() != "refused — unsent input in the input box" {
		t.Errorf("stderr = %q", f.errOut())
	}
	if len(f.bus.appended) != 0 {
		t.Errorf("a refusal records no row: %s", f.rowsText())
	}

	// A nudge row the bus refuses to take is not a failure: the pane was typed, and
	// the outcome line is what tells the dispatcher the box may hold the line.
	f = newArm(t)
	f.pane.frames = acceptedFrames(t)
	deps := newDeps(f.pane, f.bus, f.env)
	deps.Append = func(jsonv.Value) error { return os.ErrPermission }
	if rc := f.runWith(deps, Request{Target: featX}); rc != 0 || !strings.Contains(f.out(), "nudge accepted: %9") {
		t.Errorf("rc = %d, out = %q, want the accepted outcome at 0", rc, f.out())
	}

	// A `nudge_wait` row is a plain statement in the arm, so its write failure does
	// reach `set -e`: the wait ends at 1 rather than reporting an outcome no row holds.
	f = newArm(t)
	f.pane.frames = []string{fixture(t, "meter.2s.1.2k.txt")}
	deps = newDeps(f.pane, f.bus, f.env)
	deps.Append = func(jsonv.Value) error { return os.ErrPermission }
	deps.Sleep = func(context.Context, string) error { return nil }
	deps.Clock = f.clock()
	if rc := f.runWith(deps, Request{Wait: "30", Target: idleReq}); rc != 1 ||
		!strings.Contains(f.errOut(), "permission denied") {
		t.Errorf("rc = %d, stderr = %q, want the wait-row write failure", rc, f.errOut())
	}
}

func TestRunWaitJoining(t *testing.T) {
	f := newArm(t)
	f.holdLock = strconv.Itoa(os.Getpid())
	// A joiner waits for the lock: it reports the first waiter's outcome once it owns
	// the lead, never while another call holds it.
	f.releasePath, f.releaseAfter = f.lockDir(), 1
	// The first waiter resolved this lead's nudge after the joiner started, for a msg
	// at least as new: the joiner reports it instead of typing.
	f.rows = append(dispatchRows(t, "claude"), anyRows(t,
		`{"ts":9999999999999,"crew_id":"c1","kind":"nudge_wait","state":"resolved","to":"`+idleReq+
			`","result":"accepted","rc":0,"detail":"the lead took the line","msg_ts":`+idleTS+`}`)...)
	if rc := f.run(Request{Wait: "30", Target: idleReq}); rc != 0 {
		t.Fatalf("rc = %d, stderr = %q", rc, f.errOut())
	}
	if !strings.Contains(f.out(), "nudge joining: "+leadID+" "+idleReq+" — another crew nudge --wait (pid "+f.holdLock+") owns this lead's wait") {
		t.Errorf("out = %q", f.out())
	}
	if !strings.Contains(f.out(), "nudge joined: "+leadID+" "+idleReq+" — accepted: the lead took the line") {
		t.Errorf("out = %q", f.out())
	}
	if len(f.bus.appended) != 0 {
		t.Errorf("a joiner records no row of its own: %s", f.rowsText())
	}
}

func TestRunWaitJoinerTimesOut(t *testing.T) {
	f := newArm(t)
	f.holdLock = strconv.Itoa(os.Getpid())
	f.rows = dispatchRows(t, "claude")
	// A zero budget refuses on the first look: the other waiter still owns the lead.
	if rc := f.run(Request{Wait: "0", Target: idleReq}); rc != 2 {
		t.Fatalf("rc = %d, stderr = %q", rc, f.errOut())
	}
	if want := "refused — still waiting on another crew nudge --wait (pid " + f.holdLock + ")"; f.errOut() != want {
		t.Errorf("stderr = %q, want %q", f.errOut(), want)
	}
	for _, c := range f.pane.tmux {
		if strings.HasPrefix(c, "send-keys") {
			t.Errorf("a joiner typed: %q", c)
		}
	}
}

func TestRunWaitAlreadyTyped(t *testing.T) {
	f := newArm(t)
	f.rows = append(dispatchRows(t, "claude"), anyRows(t,
		`{"ts":2,"crew_id":"c1","kind":"nudge","to":"`+idleReq+`","result":"held","detail":"still in the box","msg_ts":`+idleTS+`}`)...)
	if rc := f.run(Request{Wait: "30", Target: idleReq}); rc != 3 {
		t.Fatalf("rc = %d, want 3 (a held nudge is typed-but-not-accepted)", rc)
	}
	if !strings.Contains(f.out(), "nudge already typed: "+leadID+" "+idleReq+" — held: still in the box") {
		t.Errorf("out = %q", f.out())
	}
	for _, c := range f.pane.tmux {
		if strings.HasPrefix(c, "send-keys") {
			t.Errorf("--wait that found a nudge typed: %q", c)
		}
	}

	// The same check with an accepted outcome exits 0.
	f = newArm(t)
	f.rows = append(dispatchRows(t, "claude"), anyRows(t,
		`{"ts":2,"crew_id":"c1","kind":"nudge","to":"`+idleReq+`","result":"accepted","detail":"took the line","msg_ts":`+idleTS+`}`)...)
	if rc := f.run(Request{Wait: "30", Target: idleReq}); rc != 0 {
		t.Fatalf("rc = %d, stderr = %q", rc, f.errOut())
	}
}

func TestRunWaitLeadReadIt(t *testing.T) {
	f := newArm(t)
	f.rows = dispatchRows(t, "claude")
	// The directive was there when the call started and is gone by the next look:
	// the lead read it, so nothing is typed.
	f.leadReads = true
	if rc := f.run(Request{Wait: "30", Target: idleReq}); rc != 0 {
		t.Fatalf("rc = %d, stderr = %q", rc, f.errOut())
	}
	if want := "nudge not needed: " + leadID + " " + idleReq +
		" — the lead read the msg during the wait; nothing typed"; !strings.Contains(f.out(), want) {
		t.Errorf("out = %q, want %q", f.out(), want)
	}
	for _, c := range f.pane.tmux {
		if strings.HasPrefix(c, "send-keys") {
			t.Errorf("a read msg was typed for: %q", c)
		}
	}
}

func TestRunWaitParksThenTypes(t *testing.T) {
	f := newArm(t)
	// Mid-turn on the first look, idle on the second: one waiting row, then the nudge.
	f.pane.frames = []string{
		fixture(t, "meter.2s.1.2k.txt"),
		fixture(t, "done_idle.txt"), fixture(t, "done_idle.txt"),
		fixture(t, "nudge_typed.txt"), fixture(t, "meter.2s.1.2k.txt"),
	}
	f.env = map[string]string{"CREW_NUDGE_WAIT_INTERVAL": "5"}
	if rc := f.run(Request{Wait: "30", Target: idleReq}); rc != 0 {
		t.Fatalf("rc = %d, stderr = %q", rc, f.errOut())
	}
	if !strings.Contains(f.out(), "nudge waiting: "+leadID+" "+idleReq+" — live turn; re-checking every 5s for up to 30s") {
		t.Errorf("out = %q", f.out())
	}
	if f.ticks("5") != 1 {
		t.Errorf("slept %v, want one 5s tick", f.slept)
	}
	if _, err := os.Stat(f.lockDir()); err == nil {
		t.Error("the wait lock leaked")
	}
	first, second, third := f.rowAt(0), f.rowAt(1), f.rowAt(2)
	for _, want := range []string{`"kind":"nudge_wait"`, `"state":"waiting"`, `"result":""`, `"detail":"live turn"`} {
		if !strings.Contains(first, want) {
			t.Errorf("waiting row %s lacks %s", first, want)
		}
	}
	if strings.Contains(first, `"rc"`) {
		t.Errorf("waiting row %s has an rc before there is one", first)
	}
	if !strings.Contains(second, `"kind":"nudge"`) || !strings.Contains(second, `"result":"accepted"`) {
		t.Errorf("second row %s, want the accepted nudge", second)
	}
	for _, want := range []string{`"kind":"nudge_wait"`, `"state":"resolved"`, `"result":"accepted"`, `"rc":0`} {
		if !strings.Contains(third, want) {
			t.Errorf("resolved row %s lacks %s", third, want)
		}
	}
}

func TestRunWaitTimeout(t *testing.T) {
	f := newArm(t)
	f.pane.frames = []string{fixture(t, "meter.2s.1.2k.txt")}
	f.env = map[string]string{"CREW_NUDGE_WAIT_INTERVAL": "5"}
	if rc := f.run(Request{Wait: "10", Target: idleReq}); rc != 2 {
		t.Fatalf("rc = %d, stderr = %q", rc, f.errOut())
	}
	if want := "refused — live turn or no idle input box (waited 10s)"; f.errOut() != want {
		t.Errorf("stderr = %q, want %q", f.errOut(), want)
	}
	// Two ticks of 5s spend the 10s budget; the third look times out.
	if f.ticks("5") != 2 {
		t.Errorf("slept %v, want two 5s ticks", f.slept)
	}
	if len(f.bus.appended) != 2 {
		t.Fatalf("%s, want a waiting row and a resolved timeout", f.rowsText())
	}
	if !strings.Contains(f.rowAt(0), `"state":"waiting"`) {
		t.Errorf("first row = %s", f.rowAt(0))
	}
	timeout := f.rowAt(1)
	for _, want := range []string{`"state":"resolved"`, `"result":"timeout"`, `"rc":2`, `"msg_ts":1700000000000`} {
		if !strings.Contains(timeout, want) {
			t.Errorf("timeout row %s lacks %s", timeout, want)
		}
	}
	// The lock this call created is gone on every exit path.
	if _, err := os.Stat(f.lockDir()); err == nil {
		t.Error("the wait lock leaked")
	}
}

func TestRunWaitRefusesNonLiveTurnImmediately(t *testing.T) {
	f := newArm(t)
	f.pane.frames = []string{fixture(t, "nudge_draft.txt")}
	if rc := f.run(Request{Wait: "30", Target: idleReq}); rc != 2 {
		t.Fatalf("rc = %d, stderr = %q", rc, f.errOut())
	}
	if len(f.slept) != 0 {
		t.Errorf("a dialog or draft must not be waited out, slept %v", f.slept)
	}
	if len(f.bus.appended) != 0 {
		t.Errorf("a refusal before any waiting row records nothing: %s", f.rowsText())
	}
}

func TestRunWaitRowFollowsCrewClock(t *testing.T) {
	f := newArm(t)
	clockFile := t.TempDir() + "/clock"
	if err := os.WriteFile(clockFile, []byte("1000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.pane.frames = []string{
		fixture(t, "meter.2s.1.2k.txt"),
		fixture(t, "done_idle.txt"), fixture(t, "done_idle.txt"),
		fixture(t, "nudge_typed.txt"), fixture(t, "meter.2s.1.2k.txt"),
	}
	deps := newDeps(f.pane, f.bus, f.env)
	deps.Clock = clock.Clock{Now: func() time.Time { return time.UnixMilli(f.now) }, CrewClock: clockFile}
	deps.Sleep = func(_ context.Context, secs string) error {
		f.slept = append(f.slept, secs)
		return nil
	}
	if rc := Run(context.Background(), Request{Wait: "30", Target: idleReq}, f.paths, f.stdout, f.stderr, Options{
		Deps: deps, CrewID: func() string { return f.crewID }, Getenv: env(f.env),
		Toplevel: func(context.Context) string { return "" },
		Exists:   func(string) bool { return false },
		Unread: func(context.Context, string, string, string) string {
			return f.unread
		},
		Rows: func(int) ([]any, bool) { return f.rows, true },
		PID:  4242,
	}); rc != 0 {
		t.Fatalf("rc = %d, stderr = %q", rc, f.errOut())
	}
	// CREW_CLOCK moves the wait rows and the loop's waits; the nudge row's stamp is jq's
	// `now*1000|floor`, which is real time either way.
	waiting, nudge, resolved := f.rowAt(0), f.rowAt(1), f.rowAt(2)
	for _, rowText := range []string{waiting, resolved} {
		if !strings.Contains(rowText, `"ts":1000000`) {
			t.Errorf("wait row %s does not follow CREW_CLOCK", rowText)
		}
	}
	if strings.Contains(nudge, `"ts":1000000,`) {
		t.Errorf("nudge row %s followed CREW_CLOCK", nudge)
	}
	if !strings.Contains(nudge, `"ts":17`) && !strings.Contains(nudge, `"ts":18`) {
		t.Errorf("nudge row %s is not stamped with real time", nudge)
	}
}

func TestRunWaitParsesOnlyDigits(t *testing.T) {
	for _, tc := range []struct {
		name   string
		req    Request
		budget string
	}{
		{"no value waits the default", Request{Wait: "1800", Target: idleReq}, "1800"},
		{"value", Request{Wait: "7", Target: idleReq}, "7"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newArm(t)
			f.pane.frames = []string{fixture(t, "meter.2s.1.2k.txt")}
			f.env = map[string]string{"CREW_NUDGE_WAIT_INTERVAL": "1"}
			// The budget shows up verbatim in the timeout line, which is how the test
			// reads back what `--wait` took.
			if rc := f.run(tc.req); rc != 2 {
				t.Fatalf("rc = %d, stderr = %q", rc, f.errOut())
			}
			if !strings.Contains(f.errOut(), "(waited "+tc.budget+"s)") {
				t.Errorf("stderr = %q, want a %ss budget", f.errOut(), tc.budget)
			}
		})
	}

}

func TestNewRequest(t *testing.T) {
	str := func(s string) *string { return &s }
	for _, tc := range []struct {
		name   string
		target string
		wait   *string
		want   Request
		ok     bool
	}{
		{"no wait", "nova", nil, Request{Target: "nova"}, true},
		{"bare wait is the default", "nova", str(""), Request{Target: "nova", Wait: "1800"}, true},
		{"digits", "nova", str("5"), Request{Target: "nova", Wait: "5"}, true},
		// `--wait abc nova` is two targets, which the arm refuses rather than reading as a wait.
		{"word with a target is a second target", "nova", str("abc"), Request{Target: "nova"}, false},
		// `--wait nova` is a wait for a target named nova, budget still the default.
		{"word with no target is the target", "", str("nova"), Request{Target: "nova", Wait: "1800"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := NewRequest("", tc.target, tc.wait)
			if ok != tc.ok || (ok && got != tc.want) {
				t.Errorf("NewRequest = %+v, %v; want %+v, %v", got, ok, tc.want, tc.ok)
			}
		})
	}
}
