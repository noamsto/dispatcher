package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/clock"
	"github.com/noamsto/dispatcher/crew/internal/lane"
	"github.com/noamsto/dispatcher/crew/internal/proc/proctest"
	"github.com/noamsto/dispatcher/crew/internal/roster"
	"github.com/noamsto/dispatcher/crew/internal/tmux/tmuxtest"
)

var nowS = fixedNow.Unix()

// ago is the ms timestamp of a row posted s seconds before the fixed clock.
func ago(s int64) int64 { return (nowS - s) * 1000 }

func wstatus(s int64, crew, branch, state string, extra string) string {
	return fmt.Sprintf(`{"ts":%d,"crew_id":%q,"kind":"status","from":"worker:%s#s1-1","to":"dispatcher:%s","body":{"state":%q%s}}`,
		ago(s), crew, branch, crew, state, extra)
}

func wmsg(s int64, crew, from, to, body string) string {
	return fmt.Sprintf(`{"ts":%d,"crew_id":%q,"kind":"msg","from":%q,"to":%q,"body":%s}`, ago(s), crew, from, to, body)
}

type statusFix struct {
	t      *testing.T
	root   string
	common string
	env    map[string]string
	procs  *proctest.Table
	tmux   *tmuxtest.Fake
	opts   StatusOptions
}

// newStatusFix is one repo whose bus holds lines, found through a registry, on
// a clock fixed at fixedNow. Pids 100 (started long ago) and 102 (started
// recently) are alive.
func newStatusFix(t *testing.T, lines ...string) *statusFix {
	t.Helper()
	root := t.TempDir()
	f := &statusFix{t: t, root: root, env: map[string]string{}}
	f.common = mkRepo(t, root, "alpha", "", lines...)
	registry := filepath.Join(root, "registry")
	if err := os.WriteFile(registry, []byte(f.common+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	empty := t.TempDir()
	f.env["HOME"], f.env["CREW_SWEEP_ROOTS"], f.env["XDG_STATE_HOME"] = empty, empty, filepath.Join(root, "state")
	f.env["DISPATCH_LOADAVG_FILE"] = filepath.Join(root, "loadavg")
	if err := os.WriteFile(f.env["DISPATCH_LOADAVG_FILE"], []byte("0.50 0 0 1/1 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.procs = proctest.New(nil, nil)
	f.procs.Procs[100] = proctest.Proc{Elapsed: 5000}
	f.procs.Procs[102] = proctest.Proc{Elapsed: 500}
	f.tmux = &tmuxtest.Fake{Out: map[string]string{"list-panes": "", "list-windows": ""}}
	f.opts = StatusOptions{
		Options: Options{
			Getenv:   func(k string) string { return f.env[k] },
			Registry: registry,
			Tmux:     f.tmux,
			Git:      stubGit(f.common, nil),
			Clock:    clock.Clock{Now: func() time.Time { return fixedNow }},
		},
		Proc:   f.procs,
		Host:   func() (string, error) { return "box", nil },
		Budget: func(context.Context) (int, error) { return 8, nil },
		Cmd: func(_ context.Context, name string, _ ...string) (string, error) {
			if name == "nproc" {
				return "4", nil
			}
			return "", fmt.Errorf("no such command")
		},
		BudgetFile: filepath.Join(root, "engine-budget.json"),
	}
	return f
}

func (f *statusFix) windows(branches ...string) {
	f.tmux.Out["list-windows"] = strings.Join(branches, "\n")
}

func (f *statusFix) crewFile(crew, name, text string) string {
	f.t.Helper()
	path := filepath.Join(f.common, "crew", "crews", crew, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		f.t.Fatal(err)
	}
	return path
}

// pidfile registers crew under pid, the file last written 1000 s ago.
func (f *statusFix) pidfile(crew, pid string) {
	f.t.Helper()
	path := f.crewFile(crew, "pid", pid+"\n")
	then := time.Unix(nowS-1000, 0)
	if err := os.Chtimes(path, then, then); err != nil {
		f.t.Fatal(err)
	}
}

func (f *statusFix) collect(req StatusRequest) StatusDoc {
	f.t.Helper()
	return CollectStatus(f.t.Context(), req, io.Discard, f.opts)
}

func (d StatusDoc) crew(t *testing.T, id string) CrewStatus {
	t.Helper()
	for _, r := range d.Repos {
		for _, c := range r.Crews {
			if c.CrewID == id {
				return c
			}
		}
	}
	t.Fatalf("no crew %s in %+v", id, d.Repos)
	return CrewStatus{}
}

func (c CrewStatus) worker(t *testing.T, branch string) map[string]any {
	t.Helper()
	for _, w := range c.Workers {
		if w["branch"] == branch {
			return w
		}
	}
	t.Fatalf("no worker %s in crew %s: %v", branch, c.CrewID, c.Workers)
	return nil
}

func TestStatusDispatcherState(t *testing.T) {
	f := newStatusFix(t)
	f.pidfile("c-live", "100")
	f.pidfile("c-dead", "101")
	f.pidfile("c-recycled", "102") // alive, but started after its pid file was written
	f.pidfile("c-junk", "abc")
	f.crewFile("c-live", "pane", "%81\n")
	f.crewFile("c-closed", "pane", "%82\n")
	doc := f.collect(StatusRequest{})

	for _, tc := range []struct {
		crew, state string
		pid         int
		pane        string
	}{
		{"c-live", "live", 100, "%81"},
		{"c-dead", "dead", 101, ""},
		{"c-recycled", "dead", 102, ""},
		{"c-junk", "dead", 0, ""},
		{"c-closed", "closed", 0, "%82"},
	} {
		d := doc.crew(t, tc.crew).Dispatcher
		if d.State != tc.state {
			t.Errorf("%s state = %q, want %q", tc.crew, d.State, tc.state)
		}
		if (tc.pid == 0) != (d.PID == nil) || d.PID != nil && *d.PID != tc.pid {
			t.Errorf("%s pid = %v, want %d", tc.crew, d.PID, tc.pid)
		}
		if (tc.pane == "") != (d.Pane == nil) || d.Pane != nil && *d.Pane != tc.pane {
			t.Errorf("%s pane = %v, want %q", tc.crew, d.Pane, tc.pane)
		}
	}
}

func TestStatusLaneStates(t *testing.T) {
	f := newStatusFix(t)
	tick := func(crew, holder string, ageS int64) {
		f.crewFile(crew, "stream.lock.d/pid", holder+"\n")
		f.crewFile(crew, "stream.tick", fmt.Sprintf(`{"pid":%s,"ts":%d,"park":300}`, holder, ago(ageS)))
	}
	tick("lane-alive", "100", 10)
	tick("lane-stale", "100", 5000)
	tick("lane-dead", "101", 10)
	f.pidfile("lane-none", "100")
	doc := f.collect(StatusRequest{})

	for crew, want := range map[string]string{"lane-alive": "alive", "lane-stale": "stale", "lane-dead": "dead", "lane-none": ""} {
		got := doc.crew(t, crew).Dispatcher.Lane
		switch {
		case want == "" && got != nil:
			t.Errorf("%s lane = %q, want null", crew, *got)
		case want != "" && (got == nil || *got != want):
			t.Errorf("%s lane = %v, want %q", crew, got, want)
		}
	}
}

func TestStatusAttention(t *testing.T) {
	f := newStatusFix(t,
		// c-live is attended by a live dispatcher whatever its workers do.
		wstatus(100, "c-live", "feat/a", "working", ""),
		// c-down has a worker with a window: nobody is watching it.
		wstatus(100, "c-down", "feat/b", "working", ""),
		// c-leftover has only a row no window backs: cleanup, not a need.
		wstatus(100, "c-leftover", "feat/c", "working", ""),
		wstatus(100, "c-finished", "feat/d", "done", ""),
		wstatus(100, "c-held", "feat/e", "done", ""),
		wmsg(50, "c-held", "dispatcher:c-held", "hold:c-held",
			`"{\"id\":\"h1\",\"wait\":{\"engine\":\"claude\",\"window\":\"five_hour\",\"resets_at\":1700009999},\"task\":{}}"`),
	)
	f.windows("feat/b")
	f.pidfile("c-live", "100")
	for _, c := range []string{"c-down", "c-leftover", "c-finished", "c-held"} {
		f.pidfile(c, "101")
	}
	doc := f.collect(StatusRequest{})

	for crew, want := range map[string]string{
		"c-live": "attended", "c-down": "unattended", "c-leftover": "idle", "c-finished": "idle", "c-held": "unattended",
	} {
		if got := doc.crew(t, crew).Attention; got != want {
			t.Errorf("%s attention = %q, want %q", crew, got, want)
		}
	}
	if !doc.crew(t, "c-leftover").worker(t, "feat/c")["stale"].(bool) {
		t.Error("the leftover row should be stale")
	}
}

func TestStatusRemoteWorkers(t *testing.T) {
	dispatchRemote := fmt.Sprintf(`{"ts":%d,"crew_id":"c1","kind":"dispatch","branch":"r/1","name":"remy","host":"h1","remote":true}`, ago(60))
	var asked []string
	f := newStatusFix(t, dispatchRemote)
	f.opts.Remote = func(dir, crew string) roster.Capture {
		return func(_ context.Context, host string, _ []string, stdout, _ io.Writer) int {
			asked = append(asked, host)
			_, _ = io.WriteString(stdout, `[{"branch":"r/1","state":"working","live":true,"age_s":5}]`)
			return 0
		}
	}

	doc := f.collect(StatusRequest{})
	w := doc.crew(t, "c1").worker(t, "r/1")
	if len(asked) != 1 || w["state"] != "working" || w["remote_unprobed"] != false {
		t.Errorf("without CREW_OVERSEER the host is asked: asked=%v row=%v", asked, w)
	}

	asked = nil
	f.env["CREW_OVERSEER"] = "1"
	doc = f.collect(StatusRequest{})
	w = doc.crew(t, "c1").worker(t, "r/1")
	if len(asked) != 0 {
		t.Errorf("with CREW_OVERSEER no remote capture may run, asked %v", asked)
	}
	if w["remote_unprobed"] != true || w["host"] != "h1" || w["state"] != nil {
		t.Errorf("overseer row = %v, want the bus's placeholder marked remote_unprobed", w)
	}
	if _, has := w["host_error"]; has {
		t.Errorf("an unprobed row carries no host_error: %v", w)
	}
}

func TestStatusBlocked(t *testing.T) {
	const detail = `,"detail":"waiting (cycle 7 of 24)"`
	f := newStatusFix(t,
		// feat/b: an old run, a working row that breaks it, then three restamps.
		wstatus(1200, "c1", "feat/b", "blocked", `,"detail":"old"`),
		wstatus(1100, "c1", "feat/b", "working", ""),
		wstatus(900, "c1", "feat/b", "blocked", `,"detail":"first"`),
		wstatus(600, "c1", "feat/b", "blocked", `,"detail":"waiting (cycle 1 of 24)"`),
		wstatus(300, "c1", "feat/b", "blocked", detail),
		wmsg(200, "c1", "dispatcher:c1", "worker:feat/b#s1-1", `"go"`),
		// feat/c: the reply predates the run; posted by the watchdog; no cycle.
		wmsg(2000, "c1", "dispatcher:c1", "worker:feat/c#s1-1", `"early"`),
		wstatus(400, "c1", "feat/c", "blocked", `,"detail":"stalled: quiet","source":"watchdog"`),
		// feat/d: a msg from someone else is no answer.
		wstatus(300, "c1", "feat/d", "blocked", ""),
		wmsg(100, "c1", "dispatcher:other", "worker:feat/d#s1-1", `"x"`),
		wstatus(50, "c1", "feat/e", "working", ""),
	)
	c := f.collect(StatusRequest{}).crew(t, "c1")

	b := c.worker(t, "feat/b")["blocked"].(map[string]any)
	if b["since"] != ago(900) || b["cycle"] != 7 || b["budget_left_s"] != 17*300 || b["source"] != "worker" || b["answered"] != true {
		t.Errorf("feat/b blocked = %v", b)
	}
	b = c.worker(t, "feat/c")["blocked"].(map[string]any)
	if b["since"] != ago(400) || b["cycle"] != nil || b["budget_left_s"] != nil || b["source"] != "watchdog" || b["answered"] != false {
		t.Errorf("feat/c blocked = %v", b)
	}
	if b := c.worker(t, "feat/d")["blocked"].(map[string]any); b["answered"] != false {
		t.Errorf("feat/d blocked = %v, want unanswered", b)
	}
	if c.worker(t, "feat/e")["blocked"] != nil {
		t.Error("a working row has no blocked object")
	}
}

func TestStatusFinishedWindow(t *testing.T) {
	f := newStatusFix(t,
		wstatus(400, "c1", "feat/old", "done", ""),
		wstatus(100, "c1", "feat/fresh", "done", ""),
		wstatus(4000, "c1", "feat/open", "pr_open", `,"pr_url":"https://github.com/acme/alpha/pull/1"`),
		wstatus(400, "c1", "feat/gone", "done", ""),
	)
	f.windows("feat/old", "feat/fresh", "feat/open")
	c := f.collect(StatusRequest{}).crew(t, "c1")
	for branch, want := range map[string]bool{"feat/old": true, "feat/fresh": false, "feat/open": false, "feat/gone": false} {
		if got := c.worker(t, branch)["finished_window"]; got != want {
			t.Errorf("%s finished_window = %v, want %v", branch, got, want)
		}
	}
}

func TestStatusPRCandidates(t *testing.T) {
	url := func(n int) string { return fmt.Sprintf(`,"pr_url":"https://github.com/acme/alpha/pull/%d"`, n) }
	f := newStatusFix(t,
		wstatus(300, "c1", "feat/open", "pr_open", url(1)),
		wstatus(200, "c1", "feat/done", "pr_open", url(2)),
		wstatus(100, "c1", "feat/done", "done", ""),
		wstatus(100, "c1", "feat/nourl", "done", ""),
		wstatus(300, "c1", "feat/reaped", "done", url(3)),
		wstatus(100, "c1", "feat/reaped", "reaped", ""),
		wstatus(100, "c1", "feat/working", "working", url(4)),
	)
	c := f.collect(StatusRequest{}).crew(t, "c1")
	for branch, want := range map[string]bool{
		"feat/open": true, "feat/done": true, "feat/nourl": false, "feat/reaped": false, "feat/working": false,
	} {
		if got := c.worker(t, branch)["pr_candidate"]; got != want {
			t.Errorf("%s pr_candidate = %v, want %v", branch, got, want)
		}
	}
	if got := c.worker(t, "feat/reaped")["state"]; got != "reaped" {
		t.Errorf("reaped row state = %v", got)
	}
}

func TestStatusQuotaHolds(t *testing.T) {
	hold := func(id string, resets int64) string {
		return fmt.Sprintf(`"{\"id\":\"%s\",\"wait\":{\"engine\":\"claude\",\"window\":\"five_hour\",\"resets_at\":%d},\"task\":{}}"`, id, resets)
	}
	f := newStatusFix(t,
		wmsg(500, "c1", "dispatcher:c1", "hold:c1", hold("due", nowS-10)),
		wmsg(400, "c1", "dispatcher:c1", "hold:c1", hold("later", nowS+3600)),
		wmsg(300, "c1", "dispatcher:c1", "hold:c1", hold("gone", nowS-10)),
		wmsg(200, "c1", "dispatcher:c1", "hold:c1", `"{\"id\":\"gone\",\"released\":true}"`),
	)
	holds := f.collect(StatusRequest{}).crew(t, "c1").Holds
	want := []HoldStatus{{ID: "due", Kind: "quota", WaitedS: 500, Due: true}, {ID: "later", Kind: "quota", WaitedS: 400}}
	if fmt.Sprint(holds) != fmt.Sprint(want) {
		t.Errorf("holds = %+v, want %+v", holds, want)
	}
}

// A crew id is worker-written and becomes a path under crews/: one that is not
// a valid id never becomes a crew, so it cannot point status at a file the
// worker made.
func TestStatusIgnoresInvalidCrewIDs(t *testing.T) {
	f := newStatusFix(t,
		wstatus(10, "../../escape", "feat/a", "working", ""),
		wmsg(10, "../../escape", "dispatcher:../../escape", "hold:../../escape", `"{\"id\":\"h\"}"`),
		wstatus(10, "c1", "feat/b", "working", ""),
	)
	doc := f.collect(StatusRequest{})
	if got := len(doc.Repos[0].Crews); got != 1 || doc.Repos[0].Crews[0].CrewID != "c1" {
		t.Errorf("crews = %+v, want only c1", doc.Repos[0].Crews)
	}
}

// A bus the hold fold cannot read must not read as a crew with no holds.
func TestStatusUnreadableHoldFoldSkipsTheRepo(t *testing.T) {
	f := newStatusFix(t, "5",
		wmsg(10, "c1", "dispatcher:c1", "hold:c1", `"{\"id\":\"h\"}"`))
	doc := f.collect(StatusRequest{})
	if len(doc.Repos) != 0 || len(doc.Skipped) != 1 || doc.Skipped[0].Path != f.common {
		t.Errorf("repos = %+v, skipped = %+v, want the repo skipped", doc.Repos, doc.Skipped)
	}
}

func TestStatusOverseerInbox(t *testing.T) {
	f := newStatusFix(t,
		wstatus(10, "c1", "feat/a", "working", ""),
		// older than a day: outside the no-cursor window
		wmsg(2*86400, "c1", "dispatcher:c1", "overseer:box", `{"kind":"ack","re":1}`),
		wmsg(3000, "c1", "dispatcher:c1", "overseer:box", `{"kind":"ack","re":7}`),
		wmsg(2000, "c1", "dispatcher:c1", "overseer:box", `"{\"kind\":\"note\"}"`),
		// to another host's overseer: not ours
		wmsg(1000, "c1", "dispatcher:c1", "overseer:elsewhere", `{"kind":"ack"}`),
		// a worker and another crew's dispatcher cannot write the inbox
		wmsg(900, "c1", "worker:feat/a#s1-1", "overseer:box", `{"kind":"ack"}`),
		wmsg(800, "c1", "dispatcher:c2", "overseer:box", `{"kind":"ack"}`),
	)
	r := f.collect(StatusRequest{}).Repos[0]
	got := r.Crews[0].OverseerInbox
	if len(got) != 2 || got[0].TS != ago(3000) || *got[0].Kind != "ack" || *got[0].Re != 7 ||
		got[1].TS != ago(2000) || *got[1].Kind != "note" || got[1].Re != nil {
		t.Errorf("inbox = %+v, want the ack then the note", got)
	}
	if r.Misrouted != 2 {
		t.Errorf("misrouted = %d, want the worker's msg and the other crew's", r.Misrouted)
	}

	// The overseer's cursor, outside the bus, moves the window.
	if _, err := os.Stat(f.env["XDG_STATE_HOME"]); err == nil {
		t.Fatal("status must not write overseer state")
	}
	cursor := lane.OverseerCursor(f.env["XDG_STATE_HOME"], f.common)
	if err := os.MkdirAll(filepath.Dir(cursor), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cursor, []byte(fmt.Sprintf("%d\n", ago(2500))), 0o644); err != nil {
		t.Fatal(err)
	}
	r = f.collect(StatusRequest{}).Repos[0]
	if got := r.Crews[0].OverseerInbox; len(got) != 1 || got[0].TS != ago(2000) {
		t.Errorf("inbox above the cursor = %+v, want only the newer msg", got)
	}
	if r.Misrouted != 2 {
		t.Errorf("misrouted above the cursor = %d, want 2", r.Misrouted)
	}
}

func TestStatusSkippedReposAndNonGitCwd(t *testing.T) {
	f := newStatusFix(t, wstatus(10, "c1", "feat/a", "working", ""))
	gone := filepath.Join(f.root, "vanished", ".git")
	nobus := filepath.Join(f.root, "nobus", ".git")
	if err := os.MkdirAll(nobus, 0o755); err != nil {
		t.Fatal(err)
	}
	reg := f.common + "\n" + gone + "\n" + nobus + "\n"
	if err := os.WriteFile(f.opts.Registry, []byte(reg), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())

	var out, errb bytes.Buffer
	if code := Status(t.Context(), StatusRequest{JSON: true}, &out, &errb, f.opts); code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, errb.String())
	}
	var doc StatusDoc
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("not a document: %v\n%s", err, out.String())
	}
	if len(doc.Repos) != 1 || doc.Repos[0].Repo != "acme/alpha" {
		t.Errorf("repos = %+v, want the one readable repo", doc.Repos)
	}
	skipped := map[string]string{}
	for _, s := range doc.Skipped {
		skipped[s.Path] = s.Status
	}
	if skipped[gone] != "gone" || skipped[nobus] != "no bus" || len(skipped) != 2 {
		t.Errorf("skipped = %v", skipped)
	}
}

func TestStatusMachineAndShape(t *testing.T) {
	f := newStatusFix(t)
	var out bytes.Buffer
	Status(t.Context(), StatusRequest{JSON: true}, &out, io.Discard, f.opts)
	var raw map[string]any
	if err := json.Unmarshal(out.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"ts", "host", "machine", "queue", "repos", "skipped"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("document lacks %q: %v", k, raw)
		}
	}
	if raw["host"] != "box" || raw["ts"] != float64(fixedNow.UnixMilli()) {
		t.Errorf("host/ts = %v/%v", raw["host"], raw["ts"])
	}
	machine := raw["machine"].(map[string]any)
	if machine["budget"] != float64(8) || machine["cores"] != float64(4) || machine["load1"] != 0.5 {
		t.Errorf("machine = %v", machine)
	}
	queue := raw["queue"].(map[string]any)
	if queue["head"] != nil || len(queue["demoted"].([]any)) != 0 || len(queue["parked"].([]any)) != 0 {
		t.Errorf("queue = %v, want empty", queue)
	}
}

// Two runs with the overseer's state dir deleted between them read the same
// fleet: nothing is remembered in between.
func TestStatusIsStateless(t *testing.T) {
	f := newStatusFix(t,
		wstatus(100, "c1", "feat/a", "working", ""),
		wstatus(100, "c1", "feat/b", "blocked", `,"detail":"(cycle 2 of 24)"`),
		wmsg(50, "c1", "dispatcher:c1", "overseer:box", `{"kind":"ack"}`),
	)
	f.pidfile("c1", "100")
	state := f.env["XDG_STATE_HOME"]
	snapshot := func() string {
		t.Helper()
		if err := os.RemoveAll(state); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		Status(t.Context(), StatusRequest{JSON: true}, &out, io.Discard, f.opts)
		return out.String()
	}
	first, second := snapshot(), snapshot()
	if first != second {
		t.Errorf("runs differ:\n%s\n%s", first, second)
	}
	if _, err := os.Stat(state); err == nil {
		t.Error("status wrote the overseer state dir")
	}
}

func TestUnsentInput(t *testing.T) {
	const rule = "────────────────────────────────"
	frame := func(row string) string {
		return strings.Join([]string{"  ⎿  Done", rule, row, rule, "  ? for shortcuts"}, "\n")
	}
	plain := func(row string) string {
		return strings.NewReplacer("\x1b[2m", "", "\x1b[0m", "", "\x1b[39m", "").Replace(frame(row))
	}
	ghost := "\x1b[39m❯\u00a0\x1b[2mmerge it\x1b[0m"
	typed := "\x1b[39m❯\u00a0merge it"
	for _, tc := range []struct {
		name, plain, colored string
		want                 bool
	}{
		{"dim ghost text is a suggestion", plain(ghost), frame(ghost), false},
		{"the same text typed is input", plain(typed), frame(typed), true},
		{"an empty prompt has none", plain("❯\u00a0"), frame("❯\u00a0"), false},
		{"without a colored capture a suggestion cannot be ruled out", plain(typed), "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tm := &tmuxtest.Fake{Func: func(args []string) (string, error) {
				if args[0] != "capture-pane" {
					return "", tmuxtest.ErrNoServer
				}
				for _, a := range args {
					if a == "-e" {
						return tc.colored, nil
					}
				}
				return tc.plain, nil
			}}
			if got := UnsentInput(t.Context(), tm, "%81"); got != tc.want {
				t.Errorf("UnsentInput = %v, want %v", got, tc.want)
			}
			if tc.colored != "" && !strings.Contains(strings.Join(tm.Calls(), "\n"), "capture-pane -e -p -t %81") {
				t.Errorf("the read must be a colored capture, calls: %v", tm.Calls())
			}
		})
	}
}

func TestStatusTableGolden(t *testing.T) {
	const url = `,"pr_url":"https://github.com/acme/alpha/pull/1"`
	f := newStatusFix(t,
		wstatus(900, "c1", "feat/a", "blocked", `,"detail":"(cycle 3 of 24)"`),
		wstatus(300, "c1", "feat/b", "working", ""),
		wstatus(200, "c1", "feat/c", "pr_open", url),
		wstatus(100, "c2", "feat/d", "done", url),
		wmsg(60, "c2", "dispatcher:c2", "hold:c2",
			`"{\"id\":\"h1\",\"wait\":{\"engine\":\"claude\",\"window\":\"five_hour\",\"resets_at\":1700009999},\"task\":{}}"`),
	)
	f.windows("feat/a", "feat/b", "feat/c")
	f.pidfile("c1", "100")
	f.crewFile("c1", "stream.lock.d/pid", "100\n")
	f.crewFile("c1", "stream.tick", fmt.Sprintf(`{"pid":100,"ts":%d,"park":300}`, ago(5)))
	f.pidfile("c2", "101")
	f.crewFile("c3", "pane", "%9\n")

	var out bytes.Buffer
	if code := Status(t.Context(), StatusRequest{}, &out, io.Discard, f.opts); code != 0 {
		t.Fatalf("exit %d", code)
	}
	golden := filepath.Join("testdata", "status_table.golden")
	if os.Getenv("UPDATE_GOLDEN") != "" {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(golden, out.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if out.String() != string(want) {
		t.Errorf("table differs from %s:\n got:\n%s\nwant:\n%s", golden, out.String(), want)
	}
}
