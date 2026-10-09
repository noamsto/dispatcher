package stall

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/testjson"
)

// lastLine is the log's last row, "" when it has none.
func lastLine(t *testing.T, h *harness) string {
	t.Helper()
	data, err := os.ReadFile(h.paths.Log)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	return lines[len(lines)-1]
}

func lineCount(h *harness) int {
	data, err := os.ReadFile(h.paths.Log)
	if err != nil || len(data) == 0 {
		return 0
	}
	return strings.Count(string(data), "\n")
}

func TestPostRow(t *testing.T) {
	h := newHarness(t, newFake())
	h.clock.Now = func() time.Time { return time.UnixMilli(1760000000123).Add(456 * time.Microsecond) }
	w := h.watch("worker:feat/x#s1-1", "--pane", "%1")
	if err := w.post("blocked", "quiet: pane unchanged for 1800s — é"); err != nil {
		t.Fatal(err)
	}
	got := lastLine(t, h)
	want := `{"ts":1760000000123,"crew_id":"c1","from":"worker:feat/x#s1-1","to":"dispatcher:c1","kind":"status",` +
		`"body":{"state":"blocked","detail":"quiet: pane unchanged for 1800s — é","source":"watchdog"}}`
	if got != want {
		t.Errorf("row\n got %s\nwant %s", got, want)
	}
}

// The row is value-equal to the arm's own jq construction, ts aside.
func TestPostRowMatchesArmJQ(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not on PATH")
	}
	h := newHarness(t, newFake())
	h.clock.Now = func() time.Time { return time.UnixMilli(1760000000123) }
	w := h.watch("worker:feat/x#s1-1", "--pane", "%1")
	detail := "prompt: \"quoted\"\ttab\\ — ✓"
	if err := w.post("working", detail); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("jq", "-nc", "--arg", "crew", "c1", "--arg", "from", "worker:feat/x#s1-1",
		"--arg", "state", "working", "--arg", "detail", detail,
		`{ts:(now*1000|floor), crew_id:$crew, from:$from, to:("dispatcher:"+$crew),
          kind:"status", body:{state:$state, detail:$detail, source:"watchdog"}}`).Output()
	if err != nil {
		t.Fatal(err)
	}
	mask := regexp.MustCompile(`"ts":[0-9]+`)
	gotV := testjson.MustParse(t, mask.ReplaceAllString(lastLine(t, h), `"ts":0`))
	wantV := testjson.MustParse(t, mask.ReplaceAllString(strings.TrimSpace(string(out)), `"ts":0`))
	if testjson.Compact(gotV) != testjson.Compact(wantV) {
		t.Errorf("row\n got %s\nwant %s", testjson.Compact(gotV), testjson.Compact(wantV))
	}
}

func TestPostAbortsOnTerminal(t *testing.T) {
	for _, state := range []string{"done", "failed", "exited"} {
		t.Run(state, func(t *testing.T) {
			f := newFake()
			h := newHarness(t, f)
			h.writeRows(h.status("worker:feat/x", 1, `{"state":"`+state+`"}`))
			w := h.watch("feat/x", "--pane", "%1")
			code, ok := codeOf(w.post("blocked", "quiet: x"))
			if !ok || code != 0 {
				t.Fatalf("post did not exit 0")
			}
			if n := lineCount(h); n != 1 {
				t.Errorf("log has %d lines, want the 1 seeded", n)
			}
			if len(f.opts) != 0 {
				t.Errorf("pane options set: %v", f.opts)
			}
		})
	}
}

func TestPostCreatesBusDir(t *testing.T) {
	h := newHarness(t, newFake())
	w := h.watch("feat/x", "--pane", "%1")
	if err := w.post("working", "x"); err != nil {
		t.Fatal(err)
	}
	if lineCount(h) != 1 {
		t.Error("no row appended")
	}
}

func TestPostPaneOptions(t *testing.T) {
	cases := []struct {
		id, state string
		want      [][]string
	}{
		{"feat/x", "blocked", [][]string{
			{"set-option", "-p", "-t", "%1", "@crew_state", "blocked"},
			{"set-option", "-p", "-t", "%1", "@crew_detail", "d"},
			{"set-option", "-p", "-t", "%1", "@crew_source", "watchdog"},
		}},
		{"feat/x", "working", [][]string{
			{"set-option", "-p", "-t", "%1", "@crew_state", "working"},
			{"set-option", "-p", "-t", "%1", "@crew_detail", "d"},
			{"set-option", "-p", "-t", "%1", "@crew_source", ""},
		}},
		{"role:feat/x:rev", "blocked", nil},
	}
	for _, tc := range cases {
		f := newFake()
		h := newHarness(t, f)
		w := h.watch(tc.id, "--pane", "%1")
		if err := w.post(tc.state, "d"); err != nil {
			t.Fatal(err)
		}
		if strings.Join(flatten(f.opts), "|") != strings.Join(flatten(tc.want), "|") {
			t.Errorf("%s %s: opts %q, want %q", tc.id, tc.state, f.opts, tc.want)
		}
	}
}

func flatten(rows [][]string) []string {
	var out []string
	for _, r := range rows {
		out = append(out, strings.Join(r, "\x1f"))
	}
	return out
}

func TestPostBlocked(t *testing.T) {
	cases := []struct {
		name   string
		body   string // the bus's latest own row; "" = none
		prefix string
		posted bool
	}{
		{"empty bus", "", "quiet:", true},
		{"working", `{"state":"working"}`, "quiet:", true},
		{"same prefix open", `{"state":"blocked","source":"watchdog","detail":"quiet: pane unchanged"}`, "quiet:", false},
		{"other prefix open", `{"state":"blocked","source":"watchdog","detail":"stalled: no output"}`, "quiet:", true},
		{"sticky prompt", `{"state":"blocked","source":"watchdog","detail":"prompt: interactive"}`, "quiet:", false},
		{"sticky quota", `{"state":"blocked","source":"watchdog","detail":"quota: exhausted"}`, "load:", false},
		{"prompt over quota", `{"state":"blocked","source":"watchdog","detail":"quota: exhausted"}`, "prompt:", false},
		{"worker's own blocked", `{"state":"blocked","detail":"quiet: mine"}`, "quiet:", true},
		{"prefix is literal", `{"state":"blocked","source":"watchdog","detail":"stalled: launch-not-started — x"}`, "stalled: launch-not-started", false},
		{"pr_open", `{"state":"pr_open","source":"watchdog","detail":"quiet: x"}`, "quiet:", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, newFake())
			before := 0
			if tc.body != "" {
				h.writeRows(h.status("worker:feat/x", 1, tc.body))
				before = 1
			}
			w := h.watch("feat/x", "--pane", "%1")
			posted, err := w.postBlocked(tc.prefix, tc.prefix+" new")
			if err != nil {
				t.Fatal(err)
			}
			if posted != tc.posted || (lineCount(h) > before) != tc.posted {
				t.Errorf("posted=%v lines=%d, want posted=%v", posted, lineCount(h), tc.posted)
			}
			if tc.posted && !strings.Contains(lastLine(t, h), `"state":"blocked","detail":"`+tc.prefix+` new"`) {
				t.Errorf("row %s", lastLine(t, h))
			}
		})
	}
}

func TestPostBlockedAbortsOnTerminal(t *testing.T) {
	h := newHarness(t, newFake())
	h.writeRows(h.status("worker:feat/x", 1, `{"state":"done"}`))
	w := h.watch("feat/x", "--pane", "%1")
	posted, err := w.postBlocked("quiet:", "quiet: x")
	if code, ok := codeOf(err); posted || !ok || code != 0 {
		t.Errorf("posted=%v err=%v, want exit 0", posted, err)
	}
}

func TestPostClear(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		prefix string
		want   string // detail of the clearing row, "" = none
	}{
		{"own prefix", `{"state":"blocked","source":"watchdog","detail":"quiet: pane unchanged"}`, "quiet:", "quiet: cleared"},
		{"other prefix", `{"state":"blocked","source":"watchdog","detail":"prompt: p"}`, "quiet:", ""},
		{"worker blocked", `{"state":"blocked","detail":"quiet: mine"}`, "quiet:", ""},
		{"working", `{"state":"working","source":"watchdog","detail":"quiet: cleared"}`, "quiet:", ""},
		{"empty bus", "", "quiet:", ""},
		{"long prefix", `{"state":"blocked","source":"watchdog","detail":"stalled: launch-not-started — pane %1"}`,
			"stalled: launch-not-started", "stalled: launch-not-started cleared"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			h := newHarness(t, f)
			before := 0
			if tc.body != "" {
				h.writeRows(h.status("worker:feat/x", 1, tc.body))
				before = 1
			}
			w := h.watch("feat/x", "--pane", "%1")
			if err := w.postClear(tc.prefix); err != nil {
				t.Fatal(err)
			}
			if tc.want == "" {
				if lineCount(h) != before {
					t.Errorf("cleared: %s", lastLine(t, h))
				}
				return
			}
			if !strings.Contains(lastLine(t, h), `"body":{"state":"working","detail":"`+tc.want+`","source":"watchdog"}`) {
				t.Errorf("row %s", lastLine(t, h))
			}
			if f.opts[2][5] != "" {
				t.Errorf("a clearance carried source %q", f.opts[2][5])
			}
		})
	}
}

func TestEngineAlive(t *testing.T) {
	cases := map[string]bool{"": false, "claude": true, ".claude-wrapped": true, "bash": false, "codex": true}
	for cmd, want := range cases {
		f := newFake()
		f.paneCmd = cmd
		w := newHarness(t, f).watch("feat/x", "--pane", "%1")
		if got := w.engineAlive(); got != want {
			t.Errorf("engineAlive(%q) = %v, want %v", cmd, got, want)
		}
	}
}

// publishPaneState must issue exactly the tmux argv `_publish_pane_state`
// does, the 40-character cut included, until the bash copy goes.
func TestPublishPaneStateMatchesCrewSh(t *testing.T) {
	src, err := os.ReadFile(crewScript)
	if err != nil {
		t.Skip("crew.sh is not in this tree")
	}
	fn := regexp.MustCompile(`(?ms)^_publish_pane_state\(\) \{\n.*?^\}\n`).Find(src)
	if fn == nil {
		t.Fatal("crew.sh no longer defines _publish_pane_state")
	}
	bin := t.TempDir()
	stub := "#!/usr/bin/env bash\nfor a in \"$@\"; do printf '%s\\x1f' \"$a\"; done >>\"$TMUX_LOG\"\necho >>\"$TMUX_LOG\"\n"
	if err := os.WriteFile(filepath.Join(bin, "tmux"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("é", 30) + strings.Repeat("✓", 20) + " tail"
	cases := []struct{ pane, state, detail, source string }{
		{"%1", "blocked", "quiet: pane unchanged for 1800s", "watchdog"},
		{"%1", "working", "quiet: cleared", ""},
		{"%2", "blocked", long, "watchdog"},
		{"%3", "failed", strings.Repeat("x", 41), ""},
		{"", "blocked", "d", "watchdog"},
	}
	for _, tc := range cases {
		log := filepath.Join(t.TempDir(), "tmux.log")
		cmd := exec.Command("bash", "-c", string(fn)+`_publish_pane_state "$@"`, "bash", tc.pane, tc.state, tc.detail, tc.source)
		cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "TMUX_LOG="+log, "LC_ALL=C.UTF-8")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("bash: %v %s", err, out)
		}
		data, _ := os.ReadFile(log)
		bash := string(data)

		f := newFake()
		h := newHarness(t, f)
		w := h.watch("feat/x", "--pane", "%0")
		w.cfg.pane = tc.pane
		f.pane = tc.pane
		w.publishPaneState(tc.state, tc.detail, tc.source)
		var goLog strings.Builder
		for _, argv := range f.opts {
			for _, a := range argv {
				goLog.WriteString(a + "\x1f")
			}
			goLog.WriteString("\n")
		}
		if goLog.String() != bash {
			t.Errorf("%q %q:\n bash %q\n go   %q", tc.pane, tc.detail, bash, goLog.String())
		}
	}
}
