package stall

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/clock"
	"github.com/noamsto/dispatcher/crew/internal/jsonv"
	"github.com/noamsto/dispatcher/crew/internal/stall/probe"
)

// fake is a scripted probe.Probes that counts what the loop asked for.
type fake struct {
	newProbes, samples, busReads int
	pane                         string
	sample                       func(n int) (string, bool) // n is the 0-based sample index
	paneModel, paneCmd           string
	sh                           func(op string, args ...string) (string, int)
	shCalls                      []string
	opts                         [][]string // tmux set-option argv, as the bash stub logs it
}

func newFake() *fake { return &fake{} }

func (f *fake) probes() probe.Probes {
	return probe.Probes{
		Sample: func(context.Context) (string, bool) {
			n := f.samples
			f.samples++
			if f.sample == nil {
				return "frame " + strconv.Itoa(n), true
			}
			return f.sample(n)
		},
		SampleColored: func(context.Context) string { return "" },
		PaneCmd:       func(context.Context) string { return f.paneCmd },
		PaneModel:     func(context.Context) string { return f.paneModel },
		Load:          func(context.Context) string { return "" },
		Top:           func(context.Context) string { return "" },
		RefreshBudget: func(context.Context) {},
		SetPaneOption: func(_ context.Context, name, value string) {
			f.opts = append(f.opts, []string{"set-option", "-p", "-t", f.pane, name, value})
		},
		Sh: func(_ context.Context, op string, args ...string) (string, int) {
			f.shCalls = append(f.shCalls, strings.Join(append([]string{op}, args...), " "))
			if f.sh == nil {
				return "", 0
			}
			return f.sh(op, args...)
		},
		BusRows: func(path string) ([]jsonv.Value, bool) {
			f.busReads++
			return fileRows(path)
		},
	}
}

// fileRows is the production BusRows contract over a real file.
func fileRows(path string) ([]jsonv.Value, bool) {
	st, err := os.Stat(path)
	if err != nil || !st.Mode().IsRegular() {
		return nil, false
	}
	fh, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer func() { _ = fh.Close() }()
	vs, _ := jsonv.DecodeStreamPrefix(fh)
	return vs, true
}

// harness is a temp bus plus a virtual clock seeded at the real time, the way
// CREW_CLOCK starts in production, so rows stamped seed*1000 are in this run.
type harness struct {
	t      *testing.T
	f      *fake
	crew   string
	paths  bus.Paths
	clock  clock.Clock
	seed   int64
	ctx    context.Context
	stderr bytes.Buffer
}

func newHarness(t *testing.T, f *fake) *harness {
	t.Helper()
	dir := t.TempDir()
	seed := time.Now().Unix()
	clockFile := dir + "/clock"
	if err := os.WriteFile(clockFile, []byte(strconv.FormatInt(seed, 10)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return &harness{
		t: t, f: f, crew: "c1", seed: seed, ctx: t.Context(),
		paths: bus.Paths{Common: dir, Dir: dir + "/crew", Log: dir + "/crew/events.jsonl"},
		clock: clock.Clock{Now: time.Now, CrewClock: clockFile},
	}
}

func (h *harness) options() Options {
	return Options{
		CrewID: func() string { return h.crew },
		Clock:  h.clock,
		NewProbes: func(pane string) probe.Probes {
			h.f.newProbes++
			h.f.pane = pane
			return h.f.probes()
		},
		BudgetFile: h.paths.Common + "/engine-budget.json",
		PID:        os.Getpid(),
	}
}

func (h *harness) run(argv ...string) (int, string) {
	h.stderr.Reset()
	code := Run(h.ctx, argv, h.paths, &h.stderr, h.options())
	return code, h.stderr.String()
}

func (h *harness) clockText() string {
	data, _ := os.ReadFile(h.clock.CrewClock)
	return strings.TrimSpace(string(data))
}

// watch builds the watch Run would, for driving its methods directly.
func (h *harness) watch(argv ...string) *watch {
	h.t.Helper()
	cfg, err := parseArgs(argv, func() string { return h.crew })
	if err != nil {
		h.t.Fatal(err)
	}
	cfg.runStartMS = (h.seed - 1) * 1000
	h.f.pane = cfg.pane
	return &watch{ctx: h.ctx, cfg: cfg, p: h.f.probes(), o: h.options(), paths: h.paths, stderr: &h.stderr}
}

func (h *harness) writeRows(rows ...string) {
	h.t.Helper()
	if err := os.MkdirAll(h.paths.Dir, 0o755); err != nil {
		h.t.Fatal(err)
	}
	fh, err := os.OpenFile(h.paths.Log, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		h.t.Fatal(err)
	}
	defer func() { _ = fh.Close() }()
	for _, r := range rows {
		if _, err := fh.WriteString(r + "\n"); err != nil {
			h.t.Fatal(err)
		}
	}
}

// status is a status row in this run (ts = the clock seed, plus offset ms).
func (h *harness) status(from string, offMS int64, body string) string {
	return fmt.Sprintf(`{"ts":%d,"crew_id":"c1","from":%q,"to":"dispatcher:c1","kind":"status","body":%s}`,
		h.seed*1000+offMS, from, body)
}

func codeOf(err error) (int, bool) {
	var c exitCode
	if errors.As(err, &c) {
		return int(c), true
	}
	return 0, false
}

func TestRefreshView(t *testing.T) {
	cases := []struct {
		name string
		id   string
		rows func(h *harness) []string
		want busView
	}{
		{"no log", "feat/x", nil, busView{}},
		{"last own row wins", "feat/x", func(h *harness) []string {
			return []string{
				h.status("worker:feat/x", 0, `{"state":"working","detail":"a","source":"w"}`),
				h.status("worker:feat/x#s1-1", 5, `{"state":"blocked","detail":"quiet: x","source":"watchdog"}`),
			}
		}, busView{ts: 5, state: "blocked", source: "watchdog", detail: "quiet: x"}},
		{"previous run ignored", "feat/x", func(h *harness) []string {
			return []string{
				h.status("worker:feat/x", -1500, `{"state":"done"}`),
			}
		}, busView{}},
		{"slack second kept", "feat/x", func(h *harness) []string {
			return []string{h.status("worker:feat/x", -1000, `{"state":"working"}`)}
		}, busView{ts: -1000, state: "working"}},
		{"other crew, kind, branch ignored", "feat/x", func(h *harness) []string {
			return []string{
				h.status("worker:feat/x", 1, `{"state":"working"}`),
				strings.Replace(h.status("worker:feat/x", 2, `{"state":"done"}`), `"crew_id":"c1"`, `"crew_id":"c2"`, 1),
				strings.Replace(h.status("worker:feat/x", 3, `{"state":"done"}`), `"kind":"status"`, `"kind":"msg"`, 1),
				h.status("worker:feat/xy", 4, `{"state":"done"}`),
				h.status("role:feat/x:rev", 5, `{"state":"done"}`),
			}
		}, busView{ts: 1, state: "working"}},
		// The arm's IFS-tab read shifted the detail into bus_source here; Go
		// reads by name.
		{"detail without source", "feat/x", func(h *harness) []string {
			return []string{h.status("worker:feat/x", 7, `{"state":"blocked","detail":"waiting on review"}`)}
		}, busView{ts: 7, state: "blocked", detail: "waiting on review"}},
		{"older session no step-aside", "worker:feat/x#s100-1", func(h *harness) []string {
			return []string{
				h.status("worker:feat/x#s50-9", 1, `{"state":"working"}`),
				h.status("worker:feat/x#s100-1", 2, `{"state":"working","source":"x"}`),
			}
		}, busView{ts: 2, state: "working", source: "x"}},
		{"sessionless never steps aside", "feat/x", func(h *harness) []string {
			return []string{h.status("worker:feat/x#s900-9", 1, `{"state":"working"}`)}
		}, busView{ts: 1, state: "working"}},
		{"role row", "role:feat/x:rev", func(h *harness) []string {
			return []string{
				h.status("role:feat/x:rev", 1, `{"state":"blocked","source":"watchdog","detail":"prompt: p"}`),
				h.status("worker:feat/x", 2, `{"state":"done"}`),
			}
		}, busView{ts: 1, state: "blocked", source: "watchdog", detail: "prompt: p"}},
		// jq skips an input that errors and carries on with the next.
		{"bad rows skipped alone", "feat/x", func(h *harness) []string {
			return []string{
				h.status("worker:feat/x", 1, `{"state":"working"}`),
				h.status("worker:feat/x", 2, `"not an object"`),
				strings.Replace(h.status("worker:feat/x", 3, `{"state":"done"}`), `"from":"worker:feat/x"`, `"from":7`, 1),
				`5`,
				h.status("worker:feat/x", 4, `{"state":"blocked","source":"watchdog","detail":"quiet: x"}`),
				h.status("worker:feat/x", 5, `"also bad"`),
			}
		}, busView{ts: 4, state: "blocked", source: "watchdog", detail: "quiet: x"}},
		{"torn tail keeps the prefix", "feat/x", func(h *harness) []string {
			return []string{h.status("worker:feat/x", 1, `{"state":"working"}`), `{"ts":`}
		}, busView{ts: 1, state: "working"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, newFake())
			if tc.rows != nil {
				h.writeRows(tc.rows(h)...)
			}
			w := h.watch(tc.id, "--pane", "%1")
			w.bus = busView{ts: 99, state: "stale"}
			if err := w.refresh(); err != nil {
				t.Fatalf("refresh: %v", err)
			}
			want := tc.want
			if want != (busView{}) {
				want.ts += h.seed * 1000
			}
			if w.bus != want {
				t.Errorf("view %+v, want %+v", w.bus, want)
			}
		})
	}
}

func TestRefreshStepAside(t *testing.T) {
	cases := []struct {
		from string
		step bool
	}{
		{"worker:feat/x#s200-5", true},
		{"worker:feat/x#s100-2", true},
		{"worker:feat/x#s100-1", false},
		{"worker:feat/x#s99-1", false},
		{"worker:feat/x", false},
	}
	for _, tc := range cases {
		t.Run(tc.from, func(t *testing.T) {
			h := newHarness(t, newFake())
			h.writeRows(
				h.status(tc.from, 1, `{"state":"working"}`),
				h.status("worker:feat/x#s100-1", 2, `{"state":"working"}`),
			)
			w := h.watch("worker:feat/x#s100-1", "--pane", "%1")
			code, isExit := codeOf(w.refresh())
			if isExit != tc.step || code != 0 {
				t.Errorf("exit=%v code=%d, want step-aside=%v", isExit, code, tc.step)
			}
		})
	}
}

// A malformed row does not hide a later successor's post.
func TestRefreshStepAsideAfterBadRow(t *testing.T) {
	h := newHarness(t, newFake())
	h.writeRows(
		h.status("worker:feat/x#s100-1", 1, `{"state":"working"}`),
		h.status("worker:feat/x#s100-1", 2, `"not an object"`),
		h.status("worker:feat/x#s200-5", 3, `{"state":"working"}`),
	)
	w := h.watch("worker:feat/x#s100-1", "--pane", "%1")
	if code, ok := codeOf(w.refresh()); !ok || code != 0 {
		t.Errorf("refresh did not step aside past the bad row")
	}
}

// A newer session's row before this run's start is a previous run, not a
// successor.
func TestRefreshStepAsideRunScoped(t *testing.T) {
	h := newHarness(t, newFake())
	h.writeRows(h.status("worker:feat/x#s200-5", -5000, `{"state":"working"}`))
	w := h.watch("worker:feat/x#s100-1", "--pane", "%1")
	if err := w.refresh(); err != nil {
		t.Errorf("refresh: %v", err)
	}
}

func TestStepAsideExitsRun(t *testing.T) {
	f := newFake()
	h := newHarness(t, f)
	h.writeRows(h.status("worker:feat/x#s200-5", 1, `{"state":"working"}`))
	code, _ := h.run("worker:feat/x#s100-1", "--pane", "%1", "--grace", "0")
	if code != 0 || f.samples != 0 {
		t.Errorf("exit %d after %d samples, want 0 before sampling", code, f.samples)
	}
}

func TestPaneGoneQuorum(t *testing.T) {
	f := newFake()
	f.sample = func(int) (string, bool) { return "", false }
	h := newHarness(t, f)
	code, stderr := h.run("feat/x", "--pane", "%1", "--grace", "0", "--interval", "15")
	if code != 0 || stderr != "" {
		t.Fatalf("exit %d stderr %q", code, stderr)
	}
	if f.samples != 3 {
		t.Errorf("samples %d, want 3", f.samples)
	}
	if got, want := h.clockText(), strconv.FormatInt(h.seed+30, 10); got != want {
		t.Errorf("clock %s, want %s (two sleeps between three fails)", got, want)
	}
}

// Fails must be consecutive: a good sample resets the count.
func TestPaneGoneQuorumResets(t *testing.T) {
	f := newFake()
	f.sample = func(n int) (string, bool) { return "x", n%3 == 2 }
	h := newHarness(t, f)
	code, _ := h.run("feat/x", "--pane", "%1", "--grace", "0", "--max-life", "300")
	if code != 0 || f.samples != 20 {
		t.Errorf("exit %d after %d samples, want 0 after 20 (max-life)", code, f.samples)
	}
}

func TestMaxLife(t *testing.T) {
	cases := []struct {
		grace   string
		samples int
	}{{"0", 4}, {"30", 2}}
	for _, tc := range cases {
		f := newFake()
		h := newHarness(t, f)
		code, _ := h.run("feat/x", "--pane", "%1", "--grace", tc.grace, "--max-life", "60")
		if code != 0 || f.samples != tc.samples {
			t.Errorf("grace %s: exit %d after %d samples, want 0 after %d", tc.grace, code, f.samples, tc.samples)
		}
	}
}

func TestBusCadence(t *testing.T) {
	f := newFake()
	h := newHarness(t, f)
	code, _ := h.run("feat/x", "--pane", "%1", "--grace", "0", "--max-life", "150")
	if code != 0 || f.samples != 10 {
		t.Fatalf("exit %d after %d samples", code, f.samples)
	}
	// The startup read, then ticks 4 and 8.
	if f.busReads != 3 {
		t.Errorf("bus reads %d, want 3", f.busReads)
	}
}

// A pane-gone tick reaching the cadence skips the read and marks the view
// stale; the next cadence read clears it.
func TestPaneGoneTickMarksBusStale(t *testing.T) {
	cases := []struct {
		maxLife string
		reads   int
		stale   bool
	}{{"75", 1, true}, {"135", 2, false}}
	for _, tc := range cases {
		f := newFake()
		f.sample = func(n int) (string, bool) { return "x", n != 3 }
		h := newHarness(t, f)
		w := h.watch("feat/x", "--pane", "%1", "--grace", "0", "--max-life", tc.maxLife)
		if code, ok := codeOf(w.run()); !ok || code != 0 {
			t.Fatalf("max-life %s: run ended with %d", tc.maxLife, code)
		}
		if f.busReads != tc.reads || w.busStale != tc.stale {
			t.Errorf("max-life %s: reads %d stale %v, want %d %v", tc.maxLife, f.busReads, w.busStale, tc.reads, tc.stale)
		}
	}
}

func TestTerminalStates(t *testing.T) {
	cases := []struct {
		name    string
		id      string
		args    []string
		state   string
		samples bool
	}{
		{"done release off", "feat/x", []string{"--release", "0"}, "done", false},
		{"failed role", "role:feat/x:rev", []string{"--engine", "claude"}, "failed", false},
		{"exited", "feat/x", nil, "exited", false},
		{"pr_open keeps watching", "feat/x", nil, "pr_open", true},
		{"working keeps watching", "feat/x", nil, "working", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			h := newHarness(t, f)
			from := "worker:" + tc.id
			if strings.HasPrefix(tc.id, "role:") {
				from = tc.id
			}
			h.writeRows(h.status(from, 1, `{"state":"`+tc.state+`"}`))
			argv := append([]string{tc.id, "--pane", "%1", "--grace", "0", "--max-life", "30"}, tc.args...)
			code, _ := h.run(argv...)
			if code != 0 || (f.samples > 0) != tc.samples {
				t.Errorf("exit %d samples %d, want 0 and sampling=%v", code, f.samples, tc.samples)
			}
		})
	}
}

func TestLoopState(t *testing.T) {
	f := newFake()
	f.sample = func(n int) (string, bool) { return "same", true }
	h := newHarness(t, f)
	h.writeRows(h.status("worker:feat/x", 1, `{"state":"blocked","detail":"waiting"}`))
	w := h.watch("feat/x", "--pane", "%1", "--grace", "0", "--max-life", "60")
	if _, ok := codeOf(w.run()); !ok {
		t.Fatal("run did not exit")
	}
	if !w.suppressed || w.quietFor != 45 || w.text != "same" || w.tick != 4 || w.now != h.seed+60 {
		t.Errorf("suppressed=%v quietFor=%d text=%q tick=%d now=%d", w.suppressed, w.quietFor, w.text, w.tick, w.now-h.seed)
	}
}

func TestSleepFailureExitsOne(t *testing.T) {
	f := newFake()
	h := newHarness(t, f)
	h.clock.CrewClock = ""
	code, stderr := h.run("feat/x", "--pane", "%1", "--grace", "abc")
	if code != 1 || stderr == "" || f.samples != 0 {
		t.Errorf("exit %d stderr %q samples %d, want 1 with sleep's message", code, stderr, f.samples)
	}
}

func TestSignalExitCode(t *testing.T) {
	f := newFake()
	h := newHarness(t, f)
	ctx, cancel := context.WithCancelCause(t.Context())
	cancel(SignalError{Sig: syscall.SIGTERM})
	h.ctx = ctx
	code, _ := h.run("feat/x", "--pane", "%1")
	if code != 143 || f.samples != 0 {
		t.Errorf("exit %d samples %d, want 143 before sampling", code, f.samples)
	}
}
