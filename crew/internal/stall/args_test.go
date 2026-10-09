package stall

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

const crewScript = "../../../adapters/core/crew.sh"

func TestUsageLines(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		crew string
		want string
	}{
		{"no arg", nil, "c1", usageMsg},
		{"empty arg", []string{"", "--pane", "%1"}, "c1", usageMsg},
		{"no crew before flags", []string{"feat/x", "--bogus"}, "", noCrewMsg},
		{"unknown arg", []string{"feat/x", "--pane", "%1", "--bogus"}, "c1", "crew: stall-watch: unknown arg '--bogus'"},
		{"unknown arg before missing pane", []string{"feat/x", "extra"}, "c1", "crew: stall-watch: unknown arg 'extra'"},
		{"value flag last", []string{"feat/x", "--pane"}, "c1", ""},
		{"grace last", []string{"feat/x", "--pane", "%1", "--grace"}, "c1", ""},
		{"value flag last before unknown check", []string{"feat/x", "--release"}, "c1", ""},
		{"missing pane", []string{"feat/x", "--engine", "claude"}, "c1", needPaneMsg},
		{"empty pane", []string{"feat/x", "--pane", ""}, "c1", needPaneMsg},
		{"pane before release", []string{"feat/x", "--release", "x"}, "c1", needPaneMsg},
		{"release empty", []string{"feat/x", "--pane", "%1", "--release", ""}, "c1",
			"crew: stall-watch: --release must be a non-negative integer number of seconds"},
		{"release non-digit", []string{"feat/x", "--pane", "%1", "--release", "-1"}, "c1",
			"crew: stall-watch: --release must be a non-negative integer number of seconds"},
		{"release before budget-refresh", []string{"feat/x", "--pane", "%1", "--budget-refresh", "x", "--release", "1s"}, "c1",
			"crew: stall-watch: --release must be a non-negative integer number of seconds"},
		{"budget-refresh", []string{"feat/x", "--pane", "%1", "--budget-refresh", "15m"}, "c1",
			"crew: stall-watch: --budget-refresh must be a non-negative integer number of seconds"},
		{"stall", []string{"feat/x", "--pane", "%1", "--stall", "1.5"}, "c1",
			"crew: stall-watch: --stall must be a non-negative integer number of seconds"},
		{"max-life", []string{"feat/x", "--pane", "%1", "--max-life", ""}, "c1",
			"crew: stall-watch: --max-life must be a non-negative integer number of seconds"},
		{"load", []string{"feat/x", "--pane", "%1", "--load", "x"}, "c1",
			"crew: stall-watch: --load must be a non-negative integer number of seconds"},
		{"runaway-hits", []string{"feat/x", "--pane", "%1", "--runaway-hits", "a"}, "c1",
			"crew: stall-watch: --runaway-hits must be a non-negative integer"},
		{"runaway-tokens", []string{"feat/x", "--pane", "%1", "--runaway-tokens", "-5"}, "c1",
			"crew: stall-watch: --runaway-tokens must be a non-negative integer"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			h := newHarness(t, f)
			h.crew = tc.crew
			code, stderr := h.run(tc.argv...)
			if code != 1 {
				t.Errorf("exit %d, want 1", code)
			}
			want := ""
			if tc.want != "" {
				want = tc.want + "\n"
			}
			if stderr != want {
				t.Errorf("stderr %q, want %q", stderr, want)
			}
			if f.newProbes != 0 {
				t.Errorf("probes built %d times before a usage error", f.newProbes)
			}
		})
	}
}

func TestDefaults(t *testing.T) {
	c, err := parseArgs([]string{"feat/x", "--pane", "%7"}, func() string { return "c1" })
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
	c, err := parseArgs([]string{
		"feat/x", "--pane", "%1", "--engine", "pi", "--grace", "1.5", "--interval", "2m",
		"--stall", "1", "--window", "2", "--idle", "3", "--dead", "4", "--max-life", "5",
		"--load", "6", "--release", "0", "--bg-wait", "8", "--launch", "9", "--unread", "10",
		"--runaway-hits", "11", "--runaway-tokens", "12", "--no-budget", "--budget-refresh", "13",
		"--no-nudge", "--pane", "%2",
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

// release_grace is crew.sh's default the arm read; the Go copy must not drift.
func TestReleaseGraceMatchesCrewSh(t *testing.T) {
	src, err := os.ReadFile(crewScript)
	if err != nil {
		t.Skip("crew.sh is not in this tree")
	}
	m := regexp.MustCompile(`(?m)^release_grace=([0-9]+)$`).FindSubmatch(src)
	if m == nil {
		t.Fatal("crew.sh no longer defines release_grace")
	}
	n, _ := strconv.ParseInt(string(m[1]), 10, 64)
	if n != releaseGrace {
		t.Errorf("crew.sh release_grace=%d, Go has %d", n, releaseGrace)
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
			c, err := parseArgs([]string{tc.arg, "--pane", "%1"}, func() string { return "c1" })
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
		c, err := parseArgs([]string{id, "--pane", "%1", "--engine", tc.engine}, func() string { return "c1" })
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
		args     []string
		model    string
		local    string
		want     bool
		wantShOp string
	}{
		{"claude", []string{"--engine", "claude"}, "", "", true, ""},
		{"codex", []string{"--engine", "codex"}, "", "", true, ""},
		{"cursor", []string{"--engine", "cursor"}, "", "", true, ""},
		{"unknown engine", nil, "", "", false, ""},
		{"no-budget", []string{"--engine", "claude", "--no-budget"}, "", "", false, ""},
		{"pi no model", []string{"--engine", "pi"}, "", "x", true, ""},
		{"pi remote model", []string{"--engine", "pi"}, "openrouter/x", "", true, "local-model openrouter/x"},
		{"pi local model", []string{"--engine", "pi"}, "qwen", "{\"id\":\"qwen\"}", false, "local-model qwen"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			f.paneModel = tc.model
			f.sh = func(op string, args ...string) (string, int) { return tc.local, 1 }
			c, err := parseArgs(append([]string{"feat/x", "--pane", "%1"}, tc.args...), func() string { return "c1" })
			if err != nil {
				t.Fatal(err)
			}
			if got := decideBudget(t.Context(), c, f.probes()); got != tc.want {
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
		args  []string
		model string
		local string
	}{
		{"codex no-budget", []string{"--engine", "codex", "--no-budget"}, "", ""},
		{"unknown engine", nil, "", ""},
		{"pi local model", []string{"--engine", "pi"}, "qwen", "local"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			f.paneModel = tc.model
			f.sh = func(string, ...string) (string, int) { return tc.local, 0 }
			h := newHarness(t, f)
			before := h.clockText()
			code, stderr := h.run(append([]string{"role:feat/x:rev", "--pane", "%1"}, tc.args...)...)
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
	code, _ := h.run("role:feat/x:rev", "--pane", "%1", "--engine", "claude", "--no-budget", "--grace", "0")
	if code != 0 || f.samples != 3 {
		t.Errorf("exit %d after %d samples, want 0 after 3", code, f.samples)
	}
}

func TestNewProbesGetsPane(t *testing.T) {
	f := newFake()
	f.sample = func(int) (string, bool) { return "", false }
	h := newHarness(t, f)
	h.run("feat/x", "--pane", "%9", "--grace", "0")
	if f.newProbes != 1 || f.pane != "%9" {
		t.Errorf("NewProbes called %d times with %q", f.newProbes, f.pane)
	}
}
