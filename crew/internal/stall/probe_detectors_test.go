package stall

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/jsonv"
	"github.com/noamsto/dispatcher/crew/internal/stall/probe"
)

// pdWatch is a watch started at the clock seed, for driving detector ticks by
// hand.
func pdWatch(t *testing.T, f *fake, argv ...string) (*harness, *watch) {
	t.Helper()
	h := newHarness(t, f)
	w := h.watch(argv...)
	w.start = h.seed
	return h, w
}

// pdTick is the loop's preamble for one tick at start+off — a fresh bus view
// and the suppression it implies — followed by step.
func pdTick(t *testing.T, w *watch, off int64, tick int, step func() error) error {
	t.Helper()
	if err := w.refresh(); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	w.now = w.start + off
	w.tick = tick
	w.suppressed = w.bus.state == "blocked" && w.bus.source != "watchdog"
	return step()
}

func pdOK(t *testing.T, w *watch, off int64, tick int, step func() error) {
	t.Helper()
	if err := pdTick(t, w, off, tick, step); err != nil {
		t.Fatalf("tick at +%ds: %v", off, err)
	}
}

// pdRows is every watchdog row on the bus as state|detail.
func pdRows(t *testing.T, h *harness) []string {
	t.Helper()
	data, err := os.ReadFile(h.paths.Log)
	if err != nil {
		return nil
	}
	vs, err := jsonv.DecodeStream(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	var rows []string
	for _, v := range vs {
		body, _ := v.Get("body")
		src, _ := body.Get("source")
		if s, _ := src.AsString(); s != "watchdog" {
			continue
		}
		state, _ := body.Get("state")
		detail, _ := body.Get("detail")
		st, _ := state.AsString()
		dt, _ := detail.AsString()
		rows = append(rows, st+"|"+dt)
	}
	return rows
}

func wantRows(t *testing.T, h *harness, want ...string) {
	t.Helper()
	if got := pdRows(t, h); !slices.Equal(got, want) {
		t.Errorf("watchdog rows\n got %q\nwant %q", got, want)
	}
}

func countCalls(f *fake, prefix string) int {
	n := 0
	for _, c := range f.shCalls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

// ---- D4 ----

func TestD4LoadEpisode(t *testing.T) {
	f := newFake()
	h, w := pdWatch(t, f, "feat/x", "--pane", "%1")
	load, tops := "9.50 8", 0
	w.p.Load = func(context.Context) string { return load }
	w.p.Top = func(context.Context) string {
		tops++
		return "a/b claude 101 99.0\nc/d node 202 50.0\ne/f x 3 1.0"
	}
	pdOK(t, w, 0, 0, w.probePre)
	pdOK(t, w, 299, 1, w.probePre)
	wantRows(t, h)
	pdOK(t, w, 300, 2, w.probePre)
	const fired = "blocked|load: 1m load 9.50 on 8 cores for 300s (top: a/b claude 101 99.0 | c/d node 202 50.0)"
	wantRows(t, h, fired)
	pdOK(t, w, 400, 3, w.probePre)
	if tops != 1 {
		t.Errorf("top read %d times, want once at post time", tops)
	}
	load = "8 8"
	pdOK(t, w, 415, 4, w.probePre)
	wantRows(t, h, fired, "working|load: cleared")
	// The clear reset the window: a new episode needs a fresh --load.
	load = "10 8"
	pdOK(t, w, 430, 5, w.probePre)
	pdOK(t, w, 729, 6, w.probePre)
	pdOK(t, w, 730, 7, w.probePre)
	wantRows(t, h, fired, "working|load: cleared",
		"blocked|load: 1m load 10 on 8 cores for 300s (top: a/b claude 101 99.0 | c/d node 202 50.0)")
}

func TestD4TopSuffix(t *testing.T) {
	cases := []struct{ top, want string }{
		{"", "blocked|load: 1m load 8.5 on 8 cores for 60s"},
		{"a/b claude 1 99", "blocked|load: 1m load 8.5 on 8 cores for 60s (top: a/b claude 1 99)"},
		{"x\n\ny", "blocked|load: 1m load 8.5 on 8 cores for 60s (top: x | )"},
	}
	for _, tc := range cases {
		f := newFake()
		h, w := pdWatch(t, f, "feat/x", "--pane", "%1", "--load", "60")
		w.p.Load = func(context.Context) string { return "8.5 8" }
		w.p.Top = func(context.Context) string { return tc.top }
		pdOK(t, w, 0, 0, w.probePre)
		pdOK(t, w, 60, 1, w.probePre)
		wantRows(t, h, tc.want)
	}
}

// A malformed read is silent that tick: it neither starts, resets nor clears.
func TestD4MalformedLoad(t *testing.T) {
	f := newFake()
	h, w := pdWatch(t, f, "feat/x", "--pane", "%1", "--load", "100")
	loads := map[int64]string{0: "9 8", 20: "garbage", 40: "9.5", 50: "9 8 1", 60: "", 70: "9. 8", 80: "-9 8", 100: "9 8"}
	w.p.Load = func(context.Context) string { return loads[w.now-w.start] }
	for _, off := range []int64{0, 20, 40, 50, 60, 70, 80} {
		pdOK(t, w, off, 0, w.probePre)
	}
	wantRows(t, h)
	pdOK(t, w, 100, 0, w.probePre)
	wantRows(t, h, "blocked|load: 1m load 9 on 8 cores for 100s")
	loads[120] = "1 junk"
	pdOK(t, w, 120, 0, w.probePre)
	wantRows(t, h, "blocked|load: 1m load 9 on 8 cores for 100s")
}

func TestD4ComparesAsNumbers(t *testing.T) {
	cases := []struct {
		load string
		fire bool
	}{{"8.0 8", false}, {"8.01 8", true}, {"10 9", true}, {"9 10", false}, {"100.5 16", true}}
	for _, tc := range cases {
		f := newFake()
		h, w := pdWatch(t, f, "feat/x", "--pane", "%1", "--load", "0")
		w.p.Load = func(context.Context) string { return tc.load }
		pdOK(t, w, 0, 0, w.probePre)
		if got := len(pdRows(t, h)) == 1; got != tc.fire {
			t.Errorf("load %q: fired %v, want %v", tc.load, got, tc.fire)
		}
	}
}

func TestD4OffWhenSuppressedOrRole(t *testing.T) {
	for _, tc := range []struct{ id, row string }{
		{"feat/x", `{"state":"blocked","detail":"awaiting"}`},
		{"role:feat/x:rev", ""},
	} {
		f := newFake()
		h, w := pdWatch(t, f, tc.id, "--pane", "%1", "--load", "0", "--engine", "claude")
		if tc.row != "" {
			h.writeRows(h.status("worker:"+tc.id, 1, tc.row))
		}
		reads := 0
		w.p.Load = func(context.Context) string {
			reads++
			return "99 1"
		}
		pdOK(t, w, 0, 0, w.probePre)
		if reads != 0 {
			t.Errorf("%s: load read %d times", tc.id, reads)
		}
	}
}

// ---- D8 ----

func d8Watch(t *testing.T, f *fake, argv ...string) (*harness, *watch) {
	t.Helper()
	h, w := pdWatch(t, f, append([]string{"feat/x", "--pane", "%1", "--engine", "claude", "--budget-refresh", "0"}, argv...)...)
	w.cfg.budgetOn = true
	return h, w
}

func TestD8Episode(t *testing.T) {
	f := newFake()
	h, w := d8Watch(t, f)
	const blocked = "blocked|budget: claude 5h at 100% (no reset time)"
	steps := []struct {
		tick int
		sh   func(string, ...string) (string, int)
		rows []string
	}{
		{0, budgetSh("5h\t100\t\t", 0, "", 1), []string{blocked}},
		{4, budgetSh("7d\t99\t\t", 0, "", 1), []string{blocked}},
		{8, budgetSh("", 2, "", 1), []string{blocked}},
		{9, budgetSh("", 1, "", 1), []string{blocked}},
		{12, budgetSh("", 1, "", 1), []string{blocked, "working|budget: cleared"}},
		{16, budgetSh("", 1, "", 1), []string{blocked, "working|budget: cleared"}},
		{20, budgetSh("", 1, "spend control reached\t\t", 0), []string{blocked, "working|budget: cleared",
			"blocked|budget: claude limit reached: spend control reached"}},
	}
	for _, s := range steps {
		f.sh = s.sh
		pdOK(t, w, int64(s.tick)*15, s.tick, w.probePre)
		wantRows(t, h, s.rows...)
	}
	if n := countCalls(f, "budget "); n != 12 {
		t.Errorf("budget calls %d, want 12 (two per 4th tick)", n)
	}
}

// A cache that can't tell never announces anything, open episode or not.
func TestD8CantTellHolds(t *testing.T) {
	f := newFake()
	h, w := d8Watch(t, f)
	f.sh = budgetSh("", 2, "", 2)
	pdOK(t, w, 0, 0, w.probePre)
	f.sh = budgetSh("", 1, "", 1)
	pdOK(t, w, 60, 4, w.probePre)
	wantRows(t, h)
}

// A window episode raised by another watchdog on the branch holds this one's.
func TestD8EpisodeOpenElsewhere(t *testing.T) {
	f := newFake()
	h, w := d8Watch(t, f)
	h.writeRows(h.status("worker:feat/x", 1, `{"state":"blocked","source":"watchdog","detail":"budget: claude 5h at 99%"}`))
	f.sh = budgetSh("5h\t100\t\t", 0, "", 1)
	pdOK(t, w, 0, 0, w.probePre)
	f.sh = budgetSh("", 1, "", 1)
	pdOK(t, w, 60, 4, w.probePre)
	wantRows(t, h, "blocked|budget: claude 5h at 99%")
}

func TestD8Gating(t *testing.T) {
	cases := []struct {
		name  string
		row   string
		tick  int
		noD8  bool
		calls int
	}{
		{"on", "", 0, false, 2},
		{"off", "", 0, true, 0},
		{"between cadence ticks", "", 3, false, 0},
		{"pr_open", `{"state":"pr_open"}`, 0, false, 0},
		{"own blocked", `{"state":"blocked","detail":"awaiting"}`, 0, false, 0},
		{"watchdog blocked", `{"state":"blocked","source":"watchdog","detail":"stalled: x"}`, 0, false, 2},
		{"working", `{"state":"working"}`, 0, false, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			h, w := d8Watch(t, f)
			w.cfg.budgetOn = !tc.noD8
			if tc.row != "" {
				h.writeRows(h.status("worker:feat/x", 1, tc.row))
			}
			f.sh = budgetSh("", 1, "", 1)
			pdOK(t, w, 0, tc.tick, w.probePre)
			if n := countCalls(f, "budget "); n != tc.calls {
				t.Errorf("budget calls %d, want %d", n, tc.calls)
			}
		})
	}
}

// D8's own re-read can find the worker parked in an await; that suppresses
// the rest of the tick, D5 included.
func TestD8RaisesSuppressed(t *testing.T) {
	f := newFake()
	h, w := d8Watch(t, f)
	if err := w.refresh(); err != nil {
		t.Fatal(err)
	}
	h.writeRows(h.status("worker:feat/x", 1, `{"state":"blocked","detail":"awaiting"}`))
	cmds := 0
	w.p.PaneCmd = func(context.Context) string {
		cmds++
		return "bash"
	}
	f.sh = budgetSh("5h\t100\t\t", 0, "", 1)
	w.now, w.tick, w.busStale = w.start+600, 0, true
	if err := w.probePre(); err != nil {
		t.Fatal(err)
	}
	if !w.suppressed || w.busStale || cmds != 0 || countCalls(f, "budget ") != 0 {
		t.Errorf("suppressed=%v busStale=%v paneCmd=%d budget=%d", w.suppressed, w.busStale, cmds, countCalls(f, "budget "))
	}
	if err := w.d6(); err != nil || len(f.shCalls) != 0 {
		t.Errorf("d6 ran while suppressed: %v %q", err, f.shCalls)
	}
	wantRows(t, h)
}

// A refresh can hold the tick for minutes: a pr_open posted meanwhile still
// gates D8, because a real refresh re-reads the bus.
func TestD8RefreshRereadsBus(t *testing.T) {
	f := newFake()
	h, w := d8Watch(t, f, "--budget-refresh", "900")
	w.p.RefreshBudget = func(context.Context) {
		h.writeRows(h.status("worker:feat/x", 1, `{"state":"pr_open"}`))
	}
	f.sh = budgetSh("5h\t100\t\t", 0, "", 1)
	if err := w.refresh(); err != nil {
		t.Fatal(err)
	}
	reads := f.busReads
	w.now, w.tick = w.start, 0
	if err := w.probePre(); err != nil {
		t.Fatal(err)
	}
	if f.busReads != reads+1 || w.bus.state != "pr_open" || countCalls(f, "budget ") != 0 {
		t.Errorf("reads +%d state %q budget calls %d", f.busReads-reads, w.bus.state, countCalls(f, "budget "))
	}
	wantRows(t, h)
}

func TestD8BusStaleRereads(t *testing.T) {
	for _, stale := range []bool{false, true} {
		f := newFake()
		_, w := d8Watch(t, f)
		f.sh = budgetSh("", 1, "", 1)
		reads := f.busReads
		w.now, w.tick, w.busStale = w.start, 0, stale
		if err := w.probePre(); err != nil {
			t.Fatal(err)
		}
		want := 0
		if stale {
			want = 1
		}
		if f.busReads-reads != want || w.busStale {
			t.Errorf("stale=%v: reads +%d busStale=%v, want +%d false", stale, f.busReads-reads, w.busStale, want)
		}
	}
}

// busReadsRun runs a whole watch and returns how many times it read the bus.
func busReadsRun(t *testing.T, cache string, refreshes *int, extra ...string) int {
	t.Helper()
	f := newFake()
	f.sh = budgetSh("", 1, "", 1)
	h := newHarness(t, f)
	o := h.options()
	if cache != "" {
		writeFile(t, o.BudgetFile, replaceVerb(cache, strconv.FormatInt(h.seed, 10)))
	}
	newProbes := o.NewProbes
	o.NewProbes = func(pane string) probe.Probes {
		p := newProbes(pane)
		p.RefreshBudget = func(context.Context) { *refreshes++ }
		return p
	}
	argv := append([]string{"worker:feat/x", "--pane", "%9", "--engine", "claude", "--grace", "0", "--interval", "1",
		"--window", "0", "--idle", "999", "--dead", "999", "--budget-refresh", "900", "--max-life", "10"}, extra...)
	if code := Run(h.ctx, argv, h.paths, &h.stderr, o); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	return f.busReads
}

// A skipped D8 refresh re-reads the bus no more than D8 off; a real one reads
// once more.
func TestBusReadsD8(t *testing.T) {
	refreshes := 0
	off := busReadsRun(t, `{"fetched_epoch":%d}`, &refreshes, "--no-budget")
	skipped := busReadsRun(t, `{"fetched_epoch":%d}`, &refreshes)
	if refreshes != 0 {
		t.Fatalf("a fresh cache refreshed %d times", refreshes)
	}
	refreshed := busReadsRun(t, "", &refreshes)
	if off == 0 || skipped != off || refreshes != 1 || refreshed != off+1 {
		t.Errorf("off %d skipped %d refreshed %d (refreshes %d), want skipped = off, refreshed = off+1, one refresh",
			off, skipped, refreshed, refreshes)
	}
}

// A SIGTERM while the refresh command runs ends the watch with 143 and leaves
// no refresh lock behind.
func TestSigtermLockReleasedDuringRefresh(t *testing.T) {
	f := newFake()
	f.sh = budgetSh("", 1, "", 1)
	h := newHarness(t, f)
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	h.ctx = ctx
	o := h.options()
	lockDir := o.BudgetFile + ".refresh.d"
	held := make(chan bool, 1)
	newProbes := o.NewProbes
	o.NewProbes = func(pane string) probe.Probes {
		p := newProbes(pane)
		p.RefreshBudget = func(ctx context.Context) {
			_, err := os.Stat(lockDir + "/pid")
			held <- err == nil
			<-ctx.Done()
		}
		return p
	}
	go func() {
		select {
		case <-held:
			cancel(SignalError{Sig: syscall.SIGTERM})
		case <-t.Context().Done():
		}
	}()
	code := Run(ctx, []string{"worker:feat/x", "--pane", "%9", "--engine", "claude", "--grace", "0", "--interval", "1",
		"--budget-refresh", "1", "--max-life", "600"}, h.paths, &h.stderr, o)
	if code != 143 {
		t.Errorf("exit %d, want 143", code)
	}
	if _, err := os.Stat(lockDir); !os.IsNotExist(err) {
		t.Errorf("lock dir left behind: %v", err)
	}
	if f.samples != 1 || countCalls(f, "budget ") != 0 {
		t.Errorf("samples %d budget calls %d: the watch went on after the signal", f.samples, countCalls(f, "budget "))
	}
	wantRows(t, h)
}

// ---- D5 ----

const d5Detail = "stalled: launch-not-started — pane %1 still a shell (zsh) 150s after launch; the engine is not running"

func TestD5LaunchNotStarted(t *testing.T) {
	f := newFake()
	h, w := pdWatch(t, f, "feat/x", "--pane", "%1")
	f.paneCmd = "-zsh"
	pdOK(t, w, 149, 0, w.probePre)
	wantRows(t, h)
	pdOK(t, w, 150, 1, w.probePre)
	wantRows(t, h, "blocked|"+d5Detail)
	pdOK(t, w, 165, 2, w.probePre)
	wantRows(t, h, "blocked|"+d5Detail)
	f.paneCmd = ".claude-wrapped"
	pdOK(t, w, 180, 3, w.probePre)
	wantRows(t, h, "blocked|"+d5Detail, "working|stalled: launch-not-started cleared")
	if !w.engineSeen {
		t.Error("engine not marked seen")
	}
	// An engine that exits later is quiet:/dead: territory, never D5.
	cmds := 0
	w.p.PaneCmd = func(context.Context) string {
		cmds++
		return "bash"
	}
	pdOK(t, w, 900, 4, w.probePre)
	if cmds != 0 {
		t.Errorf("pane command read %d times after the engine was seen", cmds)
	}
	wantRows(t, h, "blocked|"+d5Detail, "working|stalled: launch-not-started cleared")
}

func TestD5Cases(t *testing.T) {
	cases := []struct {
		name, cmd, row string
		args           []string
		off            int64
		want           string
	}{
		{"bash", "bash", "", nil, 150, "stalled: launch-not-started — pane %1 still a shell (bash) 150s after launch; the engine is not running"},
		{"fish via --launch", "fish", "", []string{"--launch", "30"}, 30, "stalled: launch-not-started — pane %1 still a shell (fish) 30s after launch; the engine is not running"},
		{"working row", "sh", `{"state":"working"}`, nil, 150, "stalled: launch-not-started — pane %1 still a shell (sh) 150s after launch; the engine is not running"},
		{"empty command", "", "", nil, 500, ""},
		{"unknown command", "make", "", nil, 500, ""},
		{"double dash only strips one", "--bash", "", nil, 500, ""},
		{"engine", "claude", "", nil, 500, ""},
		{"pr_open", "bash", `{"state":"pr_open"}`, nil, 500, ""},
		{"watchdog blocked", "bash", `{"state":"blocked","source":"watchdog","detail":"stalled: x"}`, nil, 500, ""},
		{"suppressed", "bash", `{"state":"blocked","detail":"awaiting"}`, nil, 500, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			h, w := pdWatch(t, f, append([]string{"feat/x", "--pane", "%1"}, tc.args...)...)
			f.paneCmd = tc.cmd
			if tc.row != "" {
				h.writeRows(h.status("worker:feat/x", 1, tc.row))
			}
			pdOK(t, w, tc.off, 0, w.probePre)
			var got []string
			for _, r := range pdRows(t, h) {
				if strings.HasPrefix(r, "blocked|stalled: launch") {
					got = append(got, strings.TrimPrefix(r, "blocked|"))
				}
			}
			if (tc.want == "") != (len(got) == 0) || (tc.want != "" && got[0] != tc.want) {
				t.Errorf("rows %q, want %q", got, tc.want)
			}
		})
	}
}

// ---- Role-mode end-of-life ----

func TestRoleEndOfLife(t *testing.T) {
	f := newFake()
	h, w := pdWatch(t, f, "role:feat/x:rev", "--pane", "%1", "--engine", "claude")
	steps := []struct {
		cmd  string
		exit bool
	}{{"bash", false}, {"", false}, {"claude", false}, {"", false}, {"make", false}, {"-bash", true}}
	for i, s := range steps {
		f.paneCmd = s.cmd
		err := pdTick(t, w, int64(i)*500, i, w.probePre)
		code, isExit := codeOf(err)
		if isExit != s.exit || code != 0 || (!isExit && err != nil) {
			t.Fatalf("step %d (%q): err %v, want exit=%v", i, s.cmd, err, s.exit)
		}
	}
	// D5 is worker mode only: the booting shell never posted.
	wantRows(t, h)
}

// ---- D6 ----

// d6Script answers `--sh unread` and `--sh nudge`.
// nudgeRC is the verdict; a non-zero failUnread or failNudge is instead the
// raw status of a helper that failed to run.
type d6Script struct {
	dispatcher, oldest, nudgeOut   string
	nudgeRC, failUnread, failNudge int
}

func (s *d6Script) sh(op string, args ...string) (string, int) {
	switch op {
	case "unread":
		if s.failUnread != 0 {
			return "", s.failUnread
		}
		if args[len(args)-1] == "dispatcher" {
			return s.dispatcher, shOffset
		}
		return s.oldest, shOffset
	case "nudge":
		if s.failNudge != 0 {
			return s.nudgeOut, s.failNudge
		}
		return s.nudgeOut, s.nudgeRC + shOffset
	}
	return "", shOffset
}

const d6ID = "worker:feat/x#s100-1"

func d6Watch(t *testing.T, s *d6Script, argv ...string) (*harness, *fake, *watch) {
	t.Helper()
	f := newFake()
	f.sh = s.sh
	h, w := pdWatch(t, f, append([]string{d6ID, "--pane", "%1", "--engine", "claude"}, argv...)...)
	return h, f, w
}

// agedMS is a msg ts that is age seconds old at start+off.
func agedMS(w *watch, off, age int64) int64 { return (w.start+off)*1000 - age*1000 }

const (
	roleDetail = "unread: role verdict undelivered for %ds — lead is working but has not read it; nudge it to run `crew await`"
	dispDetail = "unread: dispatcher directive undelivered for %ds — lead is working but has not reached a peek seam (long stage or idle on a background task)"
)

func TestD6OldestDetails(t *testing.T) {
	cases := []struct {
		src  string
		age  int64
		want string
	}{
		{"role", 700, "blocked|" + fmt.Sprintf(roleDetail, 700)},
		{"dispatcher", 600, "blocked|" + fmt.Sprintf(dispDetail, 600)},
		{"role", 599, ""},
		{"", 0, ""},
	}
	for _, tc := range cases {
		s := &d6Script{}
		h, f, w := d6Watch(t, s)
		if tc.src != "" {
			s.oldest = fmt.Sprintf("%d %s", agedMS(w, 0, tc.age), tc.src)
		}
		pdOK(t, w, 0, 0, w.d6)
		var want []string
		if tc.want != "" {
			want = []string{tc.want}
		}
		wantRows(t, h, want...)
		t0 := strconv.FormatInt((h.seed-1)*1000, 10)
		wantCalls := []string{
			"unread c1 feat/x worker:feat/x " + d6ID + " " + t0 + " dispatcher",
			"unread c1 feat/x worker:feat/x " + d6ID + " " + t0 + " oldest",
		}
		if !slices.Equal(f.shCalls, wantCalls) {
			t.Errorf("sh calls %q, want %q", f.shCalls, wantCalls)
		}
	}
}

func TestD6ClearOnDelivery(t *testing.T) {
	s := &d6Script{}
	h, _, w := d6Watch(t, s)
	s.oldest = fmt.Sprintf("%d role", agedMS(w, 0, 700))
	pdOK(t, w, 0, 0, w.d6)
	// Still unread: the open episode holds.
	pdOK(t, w, 60, 4, w.d6)
	posted := "blocked|" + fmt.Sprintf(roleDetail, 700)
	wantRows(t, h, posted)
	s.oldest = ""
	pdOK(t, w, 120, 8, w.d6)
	wantRows(t, h, posted, "working|unread: cleared")
}

func TestD6ClearOnSourceChange(t *testing.T) {
	s := &d6Script{}
	h, _, w := d6Watch(t, s, "--no-nudge")
	s.oldest = fmt.Sprintf("%d role", agedMS(w, 0, 700))
	pdOK(t, w, 0, 0, w.d6)
	s.oldest = fmt.Sprintf("%d dispatcher", agedMS(w, 60, 650))
	pdOK(t, w, 60, 4, w.d6)
	pdOK(t, w, 120, 8, w.d6)
	wantRows(t, h, "blocked|"+fmt.Sprintf(roleDetail, 700), "working|unread: cleared",
		"blocked|"+fmt.Sprintf(dispDetail, 710))
}

// An episode of another prefix is not D6's to clear, and D6 never raises over
// it.
func TestD6OtherEpisode(t *testing.T) {
	s := &d6Script{}
	h, f, w := d6Watch(t, s)
	h.writeRows(h.status(d6ID, 1, `{"state":"blocked","source":"watchdog","detail":"stalled: x"}`))
	s.oldest = fmt.Sprintf("%d role", agedMS(w, 0, 700))
	s.dispatcher = fmt.Sprintf("%d %d", agedMS(w, 0, 700), agedMS(w, 0, 650))
	pdOK(t, w, 0, 0, w.d6)
	wantRows(t, h, "blocked|stalled: x")
	if len(f.shCalls) != 0 {
		t.Errorf("sh calls %q", f.shCalls)
	}
}

func TestD6Gating(t *testing.T) {
	cases := []struct {
		name string
		id   string
		args []string
		tick int
		row  string
	}{
		{"between cadence ticks", d6ID, nil, 2, ""},
		{"role mode", "role:feat/x:rev", nil, 0, ""},
		{"suppressed", d6ID, nil, 0, `{"state":"blocked","detail":"awaiting"}`},
	}
	for _, tc := range cases {
		s := &d6Script{}
		f := newFake()
		f.sh = s.sh
		h, w := pdWatch(t, f, append([]string{tc.id, "--pane", "%1", "--engine", "claude"}, tc.args...)...)
		if tc.row != "" {
			h.writeRows(h.status(d6ID, 1, tc.row))
		}
		pdOK(t, w, 0, tc.tick, w.d6)
		if len(f.shCalls) != 0 {
			t.Errorf("%s: sh calls %q", tc.name, f.shCalls)
		}
	}
}

func TestNudgeGating(t *testing.T) {
	cases := []struct {
		name  string
		id    string
		args  []string
		nudge bool
	}{
		{"claude session", d6ID, nil, true},
		{"pi", d6ID, []string{"--engine", "pi"}, true},
		{"codex", d6ID, []string{"--engine", "codex"}, false},
		{"--no-nudge", d6ID, []string{"--no-nudge"}, false},
		{"sessionless", "feat/x", nil, false},
	}
	for _, tc := range cases {
		s := &d6Script{}
		f := newFake()
		f.sh = s.sh
		_, w := pdWatch(t, f, append([]string{tc.id, "--pane", "%1", "--engine", "claude"}, tc.args...)...)
		s.dispatcher = fmt.Sprintf("%d %d", agedMS(w, 0, 700), agedMS(w, 0, 650))
		s.oldest = fmt.Sprintf("%d dispatcher", agedMS(w, 0, 700))
		pdOK(t, w, 0, 0, w.d6)
		if got := countCalls(f, "nudge ") == 1; got != tc.nudge {
			t.Errorf("%s: nudged %v, want %v (calls %q)", tc.name, got, tc.nudge, f.shCalls)
		}
		if got := slices.ContainsFunc(f.shCalls, func(c string) bool { return strings.HasSuffix(c, " dispatcher") }); got != tc.nudge {
			t.Errorf("%s: dispatcher scan %v, want %v", tc.name, got, tc.nudge)
		}
	}
}

func TestNudgeScanParse(t *testing.T) {
	cases := []struct {
		out   string
		nudge bool
	}{
		{"%[1]d %[2]d", true},
		{"  %[1]d \t %[2]d  ", true},
		{"%[1]d %[2]d\nmore", true},
		{"%[1]d", false},
		{"%[1]d %[2]d 9", false},
		{"%[1]dx %[2]d", false},
		{"%[1]d -%[2]d", false},
		{"", false},
	}
	for _, tc := range cases {
		s := &d6Script{nudgeRC: 0}
		_, f, w := d6Watch(t, s)
		if tc.out != "" {
			s.dispatcher = fmt.Sprintf(tc.out, agedMS(w, 0, 700), agedMS(w, 0, 650))
		}
		pdOK(t, w, 0, 0, w.d6)
		if got := countCalls(f, "nudge ") == 1; got != tc.nudge {
			t.Errorf("%q: nudged %v, want %v", tc.out, got, tc.nudge)
		}
	}
}

func TestNudgeThresholds(t *testing.T) {
	cases := []struct {
		age   int64
		nudge bool
	}{{599, false}, {600, true}}
	for _, tc := range cases {
		s := &d6Script{}
		_, f, w := d6Watch(t, s)
		s.dispatcher = fmt.Sprintf("%d %d", agedMS(w, 0, tc.age), agedMS(w, 0, 10))
		pdOK(t, w, 0, 0, w.d6)
		if got := countCalls(f, "nudge ") == 1; got != tc.nudge {
			t.Errorf("age %d: nudged %v, want %v", tc.age, got, tc.nudge)
		}
	}
}

// An accepted nudge skips this tick's verdict; the same directive is never
// nudged twice, and its verdict follows on the next cadence tick.
func TestNudgeAccepted(t *testing.T) {
	s := &d6Script{nudgeOut: "nudged", nudgeRC: 0}
	h, f, w := d6Watch(t, s)
	newest := agedMS(w, 0, 650)
	s.dispatcher = fmt.Sprintf("%d %d", agedMS(w, 0, 700), newest)
	s.oldest = fmt.Sprintf("%d dispatcher", agedMS(w, 0, 700))
	pdOK(t, w, 0, 0, w.d6)
	wantRows(t, h)
	if want := []string{"nudge %1 claude " + d6ID + " c1 " + strconv.FormatInt(newest, 10)}; countCalls(f, "unread ") != 1 ||
		!slices.Equal(f.shCalls[1:], want) {
		t.Errorf("sh calls %q, want the scan then %q", f.shCalls, want)
	}
	pdOK(t, w, 60, 4, w.d6)
	if n := countCalls(f, "nudge "); n != 1 {
		t.Errorf("nudged %d times for one directive", n)
	}
	wantRows(t, h, "blocked|"+fmt.Sprintf(dispDetail, 760))
	// A newer directive while our own unread: episode is open nudges again.
	s.dispatcher = fmt.Sprintf("%d %d", agedMS(w, 120, 820), agedMS(w, 120, 10))
	pdOK(t, w, 120, 8, w.d6)
	if n := countCalls(f, "nudge "); n != 2 {
		t.Errorf("nudged %d times, want 2 after a newer directive", n)
	}
}

// A typed-but-unaccepted nudge is reported with _post, so our own open
// unread: episode does not swallow it.
func TestNudgeTypedNotAccepted(t *testing.T) {
	s := &d6Script{nudgeOut: "typed: prompt still holds the text — check the pane", nudgeRC: 3}
	h, _, w := d6Watch(t, s)
	s.oldest = fmt.Sprintf("%d role", agedMS(w, 0, 700))
	pdOK(t, w, 0, 0, w.d6)
	s.dispatcher = fmt.Sprintf("%d %d", agedMS(w, 60, 700), agedMS(w, 60, 650))
	pdOK(t, w, 60, 4, w.d6)
	wantRows(t, h, "blocked|"+fmt.Sprintf(roleDetail, 700),
		"blocked|unread: dispatcher directive undelivered for 700s — auto-nudge typed but not accepted (typed: prompt still holds the text); verify the pane with crew where")
	if w.pd.d6At != w.now || w.pd.d6Src != "dispatcher" {
		t.Errorf("d6At %d d6Src %q", w.pd.d6At-w.start, w.pd.d6Src)
	}
}

func TestNudgeRefusals(t *testing.T) {
	cases := []struct {
		out   string
		latch bool
	}{
		{"anchor: pane %1 runs bash, not claude", true},
		{"busy: the lead is mid-keystroke", false},
		{"", false},
	}
	for _, tc := range cases {
		s := &d6Script{nudgeOut: tc.out, nudgeRC: 2}
		h, f, w := d6Watch(t, s)
		s.dispatcher = fmt.Sprintf("%d %d", agedMS(w, 0, 700), agedMS(w, 0, 650))
		pdOK(t, w, 0, 0, w.d6)
		if w.pd.nudgeOff != tc.latch {
			t.Errorf("%q: nudgeOff %v, want %v", tc.out, w.pd.nudgeOff, tc.latch)
		}
		// A refusal does not skip the verdict, and a newer directive retries
		// unless the anchor latch is set.
		if countCalls(f, "unread ") != 2 {
			t.Errorf("%q: verdict skipped: %q", tc.out, f.shCalls)
		}
		s.dispatcher = fmt.Sprintf("%d %d", agedMS(w, 60, 760), agedMS(w, 60, 5))
		pdOK(t, w, 60, 4, w.d6)
		want := 2
		if tc.latch {
			want = 1
		}
		if n := countCalls(f, "nudge "); n != want {
			t.Errorf("%q: nudges %d, want %d", tc.out, n, want)
		}
		wantRows(t, h)
	}
}

func nudgeRow(crew, to string, msgTS string) string {
	ts := ""
	if msgTS != "" {
		ts = `,"msg_ts":` + msgTS
	}
	return fmt.Sprintf(`{"ts":1,"crew_id":%q,"from":"dispatcher:c1","to":%q,"kind":"nudge"%s}`, crew, to, ts)
}

func TestNudgeAlreadyNudged(t *testing.T) {
	cases := []struct {
		name  string
		rows  func(newest string) []string
		skip  bool
		lines int // filler lines appended after rows
	}{
		{"same directive", func(n string) []string { return []string{nudgeRow("c1", d6ID, n)} }, true, 0},
		{"newer directive nudged", func(n string) []string { return []string{nudgeRow("c1", d6ID, n+"0")} }, true, 0},
		{"older directive only", func(n string) []string { return []string{nudgeRow("c1", d6ID, "1")} }, false, 0},
		{"no msg_ts", func(string) []string { return []string{nudgeRow("c1", d6ID, "")} }, false, 0},
		{"other crew", func(n string) []string { return []string{nudgeRow("c2", d6ID, n)} }, false, 0},
		{"other session", func(n string) []string { return []string{nudgeRow("c1", "worker:feat/x#s99-1", n)} }, false, 0},
		{"among junk lines", func(n string) []string {
			return []string{"not json", `"a string"`, `[1]`, `{"ts":`, nudgeRow("c1", d6ID, n), "1 2", ""}
		}, true, 0},
		{"within the last 2000 lines", func(n string) []string { return []string{nudgeRow("c1", d6ID, n)} }, true, 1999},
		{"beyond the last 2000 lines", func(n string) []string { return []string{nudgeRow("c1", d6ID, n)} }, false, 2000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &d6Script{nudgeRC: 0}
			h, f, w := d6Watch(t, s)
			newest := agedMS(w, 0, 650)
			s.dispatcher = fmt.Sprintf("%d %d", agedMS(w, 0, 700), newest)
			rows := tc.rows(strconv.FormatInt(newest, 10))
			for range tc.lines {
				rows = append(rows, `{"kind":"msg"}`)
			}
			h.writeRows(rows...)
			pdOK(t, w, 0, 0, w.d6)
			if got := countCalls(f, "nudge ") == 0; got != tc.skip {
				t.Errorf("skipped nudge %v, want %v", got, tc.skip)
			}
			if w.pd.nudgedTS != newest {
				t.Errorf("nudgedTS %d, want %d", w.pd.nudgedTS, newest)
			}
			// An already-nudged directive does not skip the verdict.
			if tc.skip && countCalls(f, "unread ") != 2 {
				t.Errorf("verdict skipped: %q", f.shCalls)
			}
		})
	}
}

// Every bus read is bounded: refresh reads this run's rows, nudged the last
// 2000 lines. A whole-log read every 4th tick — and again before every post —
// costs memory proportional to a log that never shrinks.
func TestBusReadsAreBounded(t *testing.T) {
	s := &d6Script{}
	_, f, w := d6Watch(t, s)
	s.dispatcher = fmt.Sprintf("%d %d", agedMS(w, 0, 700), agedMS(w, 0, 650))
	runRead := fmt.Sprintf("%d/0", w.cfg.runStartMS)
	tailRead := fmt.Sprintf("0/%d", nudgedTailLines)
	seen := map[string]int{}
	f.busRows = func(path string, sinceMS int64, maxLines int) ([]jsonv.Value, bool) {
		seen[fmt.Sprintf("%d/%d", sinceMS, maxLines)]++
		return probe.BusRows(path, sinceMS, maxLines)
	}
	pdOK(t, w, 0, 0, w.d6)
	if seen[runRead] == 0 || seen[tailRead] == 0 {
		t.Fatalf("reads %v, want the run's rows (%s) and the nudge scan (%s)", seen, runRead, tailRead)
	}
	delete(seen, runRead)
	delete(seen, tailRead)
	for unbounded := range seen {
		t.Errorf("bus read %s is unbounded", unbounded)
	}
}
