package stall

import (
	"bytes"
	"strings"
	"testing"
)

func TestUsageLines(t *testing.T) {
	const sec = " must be a non-negative integer number of seconds"
	cases := []struct {
		name string
		req  Request
		crew string
		want string
	}{
		{"no arg", Request{}, "c1", usageMsg},
		{"empty arg", Request{Pane: ptr("%1")}, "c1", usageMsg},
		{"no crew before flags", Request{Target: "feat/x", Engine: ptr("")}, "", noCrewMsg},
		{"engine empty", Request{Target: "feat/x", Pane: ptr("%1"), Engine: ptr("")}, "c1", "crew: stall-watch: --engine needs a value"},
		{"grace empty", Request{Target: "feat/x", Pane: ptr("%1"), Grace: ptr("")}, "c1", "crew: stall-watch: --grace needs a value"},
		{"interval empty", Request{Target: "feat/x", Pane: ptr("%1"), Interval: ptr("")}, "c1", "crew: stall-watch: --interval needs a value"},
		{"empty value before missing pane", Request{Target: "feat/x", Grace: ptr("")}, "c1", "crew: stall-watch: --grace needs a value"},
		{"missing pane", Request{Target: "feat/x", Engine: ptr("claude")}, "c1", needPaneMsg},
		{"empty pane", Request{Target: "feat/x", Pane: ptr("")}, "c1", needPaneMsg},
		{"pane before release", Request{Target: "feat/x", Release: ptr("x")}, "c1", needPaneMsg},
		{"release empty", Request{Target: "feat/x", Pane: ptr("%1"), Release: ptr("")}, "c1", "crew: stall-watch: --release" + sec},
		{"release non-digit", Request{Target: "feat/x", Pane: ptr("%1"), Release: ptr("-1")}, "c1", "crew: stall-watch: --release" + sec},
		{"release before budget-refresh", Request{Target: "feat/x", Pane: ptr("%1"), BudgetRefresh: ptr("x"), Release: ptr("1s")}, "c1",
			"crew: stall-watch: --release" + sec},
		{"budget-refresh", Request{Target: "feat/x", Pane: ptr("%1"), BudgetRefresh: ptr("15m")}, "c1", "crew: stall-watch: --budget-refresh" + sec},
		{"stall", Request{Target: "feat/x", Pane: ptr("%1"), Stall: ptr("1.5")}, "c1", "crew: stall-watch: --stall" + sec},
		{"max-life", Request{Target: "feat/x", Pane: ptr("%1"), MaxLife: ptr("")}, "c1", "crew: stall-watch: --max-life" + sec},
		{"load", Request{Target: "feat/x", Pane: ptr("%1"), Load: ptr("x")}, "c1", "crew: stall-watch: --load" + sec},
		{"runaway-hits", Request{Target: "feat/x", Pane: ptr("%1"), RunawayHits: ptr("a")}, "c1",
			"crew: stall-watch: --runaway-hits must be a non-negative integer"},
		{"runaway-tokens", Request{Target: "feat/x", Pane: ptr("%1"), RunawayTokens: ptr("-5")}, "c1",
			"crew: stall-watch: --runaway-tokens must be a non-negative integer"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			h := newHarness(t, f)
			h.crew = tc.crew
			code, stderr := h.run(tc.req)
			if code != 1 {
				t.Errorf("exit %d, want 1", code)
			}
			if stderr != tc.want+"\n" {
				t.Errorf("stderr %q, want %q", stderr, tc.want+"\n")
			}
			if f.newProbes != 0 {
				t.Errorf("probes built %d times before a usage error", f.newProbes)
			}
		})
	}
}

func TestDefaults(t *testing.T) {
	c, err := parseArgs(Request{Target: "feat/x", Pane: ptr("%7")}, func() string { return "c1" })
	if err != nil {
		t.Fatal(err)
	}
	want := config{
		crew: "c1", fromID: "worker:feat/x", me: "worker:feat/x", branch: "feat/x", pane: "%7",
		engine: "unknown", nudgeOn: true, grace: "45", interval: "15",
		stall: 300, window: 900, idle: 1800, dead: 1800, maxLife: 43200, loadWin: 300,
		release: releaseGrace, bgWait: 7200, launch: 150, unread: 600, runawayHits: 3,
		runawayTokens: 1500, budgetRefresh: 900,
	}
	if c != want {
		t.Errorf("defaults\n got %+v\nwant %+v", c, want)
	}
}

func TestFlagsParse(t *testing.T) {
	c, err := parseArgs(Request{
		Target: "feat/x", Pane: ptr("%2"), Engine: ptr("pi"), Grace: ptr("1.5"), Interval: ptr("2m"),
		Stall: ptr("1"), Window: ptr("2"), Idle: ptr("3"), Dead: ptr("4"), MaxLife: ptr("5"),
		Load: ptr("6"), Release: ptr("0"), BgWait: ptr("8"), Launch: ptr("9"), Unread: ptr("10"),
		RunawayHits: ptr("11"), RunawayTokens: ptr("12"), NoBudget: true, BudgetRefresh: ptr("13"),
		NoNudge: true,
	}, func() string { return "c1" })
	if err != nil {
		t.Fatal(err)
	}
	got := []int64{c.stall, c.window, c.idle, c.dead, c.maxLife, c.loadWin, c.release, c.bgWait,
		c.launch, c.unread, c.runawayHits, c.runawayTokens, c.budgetRefresh}
	want := []int64{1, 2, 3, 4, 5, 6, 0, 8, 9, 10, 11, 12, 13}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("numeric %d = %d, want %d", i, got[i], want[i])
		}
	}
	if c.pane != "%2" || c.engine != "pi" || c.grace != "1.5" || c.interval != "2m" || !c.noBudget || c.nudgeOn {
		t.Errorf("parsed %+v", c)
	}
}

func TestIdentity(t *testing.T) {
	cases := []struct {
		arg                          string
		role                         bool
		fromID, me, branch, ownEpoch string
	}{
		{"role:feat/x:rev", true, "role:feat/x:rev", "role:feat/x:rev", "feat/x", ""},
		{"role:feat/x:y:rev", true, "role:feat/x:y:rev", "role:feat/x:y:rev", "feat/x:y", ""},
		{"worker:feat/x#s1-1", false, "worker:feat/x#s1-1", "worker:feat/x", "feat/x", "1"},
		{"feat/x#s1700000000-4242", false, "worker:feat/x#s1700000000-4242", "worker:feat/x", "feat/x", "1700000000"},
		{"feat/x", false, "worker:feat/x", "worker:feat/x", "feat/x", ""},
		{"worker:feat/x", false, "worker:feat/x", "worker:feat/x", "feat/x", ""},
		{"feat/a#b", false, "worker:feat/a#b", "worker:feat/a#b", "feat/a#b", ""},
		{"worker:feat/a#b#s3-9", false, "worker:feat/a#b#s3-9", "worker:feat/a#b", "feat/a#b", "3"},
		{"feat/a#sx#s3-9", false, "worker:feat/a#sx#s3-9", "worker:feat/a#sx", "feat/a#sx", "3"},
	}
	for _, tc := range cases {
		t.Run(tc.arg, func(t *testing.T) {
			c, err := parseArgs(Request{Target: tc.arg, Pane: ptr("%1")}, func() string { return "c1" })
			if err != nil {
				t.Fatal(err)
			}
			if c.roleMode != tc.role || c.fromID != tc.fromID || c.me != tc.me || c.branch != tc.branch || c.ownEpoch != tc.ownEpoch {
				t.Errorf("got role=%v from=%q me=%q branch=%q epoch=%q", c.roleMode, c.fromID, c.me, c.branch, c.ownEpoch)
			}
		})
	}
}

func TestSignatureTable(t *testing.T) {
	type sigs struct{ prompt, meter, session, cursor, bgwait, runaway bool }
	cases := []struct {
		engine string
		role   bool
		want   sigs
	}{
		{"claude", false, sigs{true, true, true, false, true, true}},
		{"pi", false, sigs{false, false, false, false, false, true}},
		{"codex", false, sigs{true, false, false, false, false, false}},
		{"cursor", false, sigs{false, false, false, true, false, false}},
		{"unknown", false, sigs{}},
		{"gemini", false, sigs{}},
		{"claude", true, sigs{true, true, true, false, true, true}},
		{"pi", true, sigs{false, false, false, false, false, true}},
		{"codex", true, sigs{false, false, false, false, false, false}},
		{"cursor", true, sigs{false, false, false, false, false, false}},
	}
	for _, tc := range cases {
		id := "feat/x"
		if tc.role {
			id = "role:feat/x:rev"
		}
		c, err := parseArgs(Request{Target: id, Pane: ptr("%1"), Engine: ptr(tc.engine)}, func() string { return "c1" })
		if err != nil {
			t.Fatal(err)
		}
		got := sigs{c.sigPrompt, c.sigMeter, c.sigSessionLimit, c.sigCursorLimit, c.sigBgwait, c.sigRunaway}
		if got != tc.want {
			t.Errorf("%s role=%v: got %+v, want %+v", tc.engine, tc.role, got, tc.want)
		}
	}
}

func TestBudgetOn(t *testing.T) {
	cases := []struct {
		name     string
		req      Request
		model    string
		local    string
		want     bool
		wantShOp string
	}{
		{"claude", Request{Engine: ptr("claude")}, "", "", true, ""},
		{"codex", Request{Engine: ptr("codex")}, "", "", true, ""},
		{"cursor", Request{Engine: ptr("cursor")}, "", "", true, ""},
		{"unknown engine", Request{}, "", "", false, ""},
		{"no-budget", Request{Engine: ptr("claude"), NoBudget: true}, "", "", false, ""},
		{"pi no model", Request{Engine: ptr("pi")}, "", "x", true, ""},
		{"pi remote model", Request{Engine: ptr("pi")}, "openrouter/x", "", true, "local-model openrouter/x"},
		{"pi local model", Request{Engine: ptr("pi")}, "qwen", "{\"id\":\"qwen\"}", false, "local-model qwen"},
		{"pi local model helper failed", Request{Engine: ptr("pi")}, "qwen", "{\"id\":\"qwen\"}", true, "local-model qwen"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			f.paneModel = tc.model
			f.sh = func(op string, args ...string) (string, int) {
				if strings.HasSuffix(tc.name, "helper failed") {
					return tc.local, 1
				}
				return tc.local, shOffset
			}
			req := tc.req
			req.Target, req.Pane = "feat/x", ptr("%1")
			c, err := parseArgs(req, func() string { return "c1" })
			if err != nil {
				t.Fatal(err)
			}
			w := &watch{ctx: t.Context(), cfg: c, p: f.probes(), stderr: &bytes.Buffer{}}
			if got := w.decideBudget(t.Context()); got != tc.want {
				t.Errorf("budgetOn = %v, want %v", got, tc.want)
			}
			if got := strings.Join(f.shCalls, ";"); got != tc.wantShOp {
				t.Errorf("Sh calls %q, want %q", got, tc.wantShOp)
			}
		})
	}
}

// A non-claude role watch with D8 off has nothing to detect: it exits before
// the grace sleep and before any sample.
func TestNonClaudeRoleNoBudgetExitsEarly(t *testing.T) {
	cases := []struct {
		name  string
		req   Request
		model string
		local string
	}{
		{"codex no-budget", Request{Engine: ptr("codex"), NoBudget: true}, "", ""},
		{"unknown engine", Request{}, "", ""},
		{"pi local model", Request{Engine: ptr("pi")}, "qwen", "local"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			f.paneModel = tc.model
			f.sh = func(string, ...string) (string, int) { return tc.local, shOffset }
			h := newHarness(t, f)
			before := h.clockText()
			req := tc.req
			req.Target, req.Pane = "role:feat/x:rev", ptr("%1")
			code, stderr := h.run(req)
			if code != 0 || stderr != "" {
				t.Fatalf("exit %d stderr %q", code, stderr)
			}
			if f.samples != 0 || f.busReads != 0 {
				t.Errorf("samples %d bus reads %d, want none", f.samples, f.busReads)
			}
			if after := h.clockText(); after != before {
				t.Errorf("clock moved %s → %s: the grace sleep ran", before, after)
			}
		})
	}
}

func TestClaudeRoleNoBudgetStillWatches(t *testing.T) {
	f := newFake()
	f.sample = func(int) (string, bool) { return "", false }
	h := newHarness(t, f)
	code, _ := h.run(Request{Target: "role:feat/x:rev", Pane: ptr("%1"), Engine: ptr("claude"), NoBudget: true, Grace: ptr("0")})
	if code != 0 || f.samples != 3 {
		t.Errorf("exit %d after %d samples, want 0 after 3", code, f.samples)
	}
}

func TestNewProbesGetsPane(t *testing.T) {
	f := newFake()
	f.sample = func(int) (string, bool) { return "", false }
	h := newHarness(t, f)
	h.run(Request{Target: "feat/x", Pane: ptr("%9"), Grace: ptr("0")})
	if f.newProbes != 1 || f.pane != "%9" {
		t.Errorf("NewProbes called %d times with %q", f.newProbes, f.pane)
	}
}
