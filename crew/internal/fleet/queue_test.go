package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/clock"
	"github.com/noamsto/dispatcher/crew/internal/git"
	"github.com/noamsto/dispatcher/crew/internal/git/gittest"
	"github.com/noamsto/dispatcher/crew/internal/hold"
	"github.com/noamsto/dispatcher/crew/internal/lock"
	"github.com/noamsto/dispatcher/crew/internal/proc/proctest"
	"github.com/noamsto/dispatcher/crew/internal/sender"
	"github.com/noamsto/dispatcher/crew/internal/tmux/tmuxtest"
)

const (
	livePID = "100"
	deadPID = "101"
	// headTimeout is the default fleetHeadTimeout, in seconds.
	headTimeout = 900
)

type queueFix struct {
	t           *testing.T
	root        string
	env         map[string]string
	commons     map[string]string // repo name → common dir
	origins     map[string]string // common dir → origin url
	panes       []string          // the lead scan's list-panes rows
	stamps      string            // the sender check's list-panes rows
	procs       *proctest.Table
	now         time.Time
	settings    QueueSettings
	settingsErr error
	opts        QueueOptions
}

// newQueueFix is a registry of the named repos, each with an empty bus, on a
// clock the test moves. Pid 100 is alive and 101 is not; no pane exists, the
// host load is low and the settings are the defaults.
func newQueueFix(t *testing.T, names ...string) *queueFix {
	t.Helper()
	root := t.TempDir()
	f := &queueFix{
		t: t, root: root, env: map[string]string{}, commons: map[string]string{}, origins: map[string]string{},
		procs: proctest.New(map[int]bool{100: true}, nil), now: fixedNow, settings: defaultQueueSettings,
	}
	f.env["HOME"] = filepath.Join(root, "home")
	f.env["CREW_SWEEP_ROOTS"] = root
	f.env["XDG_DATA_HOME"] = filepath.Join(root, "data")
	f.env["XDG_STATE_HOME"] = filepath.Join(root, "state")
	f.env["DISPATCH_LOADAVG_FILE"] = filepath.Join(root, "loadavg")
	f.loadavg("0.50")
	tm := &tmuxtest.Fake{Func: func(args []string) (string, error) {
		if len(args) == 4 && args[0] == "list-panes" {
			switch args[3] {
			case paneFormat:
				return strings.Join(f.panes, "\n"), nil
			case sender.Format:
				return f.stamps, nil
			}
		}
		return "", tmuxtest.ErrNoServer
	}}
	getenv := func(k string) string { return f.env[k] }
	f.opts = QueueOptions{
		LoadOptions: LoadOptions{
			Getenv:   getenv,
			Registry: filepath.Join(root, "registry"),
			Tmux:     tm,
			Git: &gittest.Fake{Func: func(_ string, args []string) (string, error) {
				if c, ok := strings.CutPrefix(args[0], "--git-dir="); ok && f.origins[c] != "" && slices.Contains(args, "remote.origin.url") {
					return f.origins[c], nil
				}
				if args[len(args)-1] == "--porcelain" {
					return "", nil
				}
				return "", &git.ExitError{Code: 1}
			}},
			Clock:      clock.Clock{Now: func() time.Time { return f.now }},
			BudgetFile: filepath.Join(root, "engine-budget.json"),
			Cmd: func(_ context.Context, name string, _ ...string) (string, error) {
				if name == "nproc" {
					return "4", nil
				}
				return "", errors.New("no such command")
			},
		},
		Proc:     f.procs,
		Settings: func(context.Context) (QueueSettings, error) { return f.settings, f.settingsErr },
		Sender:   sender.Probes{Getenv: getenv, Tmux: tm, Proc: f.procs},
	}
	for _, n := range names {
		f.repo(n, true)
	}
	return f
}

func (f *queueFix) write(path, text string) {
	f.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *queueFix) appendTo(path, text string) {
	f.t.Helper()
	fh, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = fh.Close() }()
	if _, err := fh.WriteString(text); err != nil {
		f.t.Fatal(err)
	}
}

// repo makes a repo with an empty bus and an acme origin, listed in the
// registry when registered.
func (f *queueFix) repo(name string, registered bool) string {
	f.t.Helper()
	common := filepath.Join(f.root, name, ".git")
	f.write(filepath.Join(common, "crew", "events.jsonl"), "")
	f.commons[name] = common
	f.origins[common] = "https://github.com/acme/" + name + ".git"
	if registered {
		f.appendTo(f.opts.Registry, common+"\n")
	}
	return common
}

func (f *queueFix) busPath(repo string) string {
	return filepath.Join(f.commons[repo], "crew", "events.jsonl")
}

func (f *queueFix) post(repo string, lines ...string) {
	f.t.Helper()
	f.appendTo(f.busPath(repo), strings.Join(lines, "\n")+"\n")
}

// ms is the wall clock agoS seconds before now, in ms.
func (f *queueFix) ms(agoS int64) int64 { return f.now.UnixMilli() - agoS*1000 }

func (f *queueFix) advance(s int64) { f.now = f.now.Add(time.Duration(s) * time.Second) }

// add posts a fleet hold for branch feat/<id>, added agoS seconds ago.
func (f *queueFix) add(repo, crew, id string, weight int, agoS int64) {
	f.t.Helper()
	f.addPrio(repo, crew, id, "normal", weight, agoS)
}

func (f *queueFix) addPrio(repo, crew, id, prio string, weight int, agoS int64) {
	f.t.Helper()
	f.post(repo, fleetAddRow(f.ms(agoS), crew, id, "feat/"+id, prio, weight))
}

func (f *queueFix) release(repo, crew, id string, agoS int64) {
	f.t.Helper()
	f.post(repo, releaseRow(f.ms(agoS), crew, id))
}

func fleetAddRow(tsMS int64, crew, id, branch, prio string, weight int) string {
	task := map[string]any{
		"ref": "r", "branch": branch, "tier": "standard", "engine": "claude", "model": "m", "effort": "e",
		"plan": nil, "mcp": nil, "draft": false, "shape": nil, "title": "t-" + id, "spec": nil,
	}
	if prio != "" {
		task["prio"] = prio
	}
	return holdRowJSON(tsMS, crew, map[string]any{"id": id, "wait": map[string]any{"kind": "fleet", "weight": weight}, "task": task})
}

func releaseRow(tsMS int64, crew, id string) string {
	return holdRowJSON(tsMS, crew, map[string]any{"id": id, "released": true})
}

func holdRowJSON(tsMS int64, crew string, body map[string]any) string {
	b, _ := json.Marshal(body)
	row, _ := json.Marshal(map[string]any{
		"ts": tsMS, "crew_id": crew, "from": "dispatcher:" + crew, "to": "hold:" + crew, "kind": "msg", "body": string(b),
	})
	return string(row)
}

func (f *queueFix) crewFile(repo, crew, name, text string) {
	f.t.Helper()
	f.write(filepath.Join(f.commons[repo], "crew", "crews", crew, name), text)
}

func (f *queueFix) register(repo, crew, pid string) {
	f.t.Helper()
	f.crewFile(repo, crew, "pid", pid+"\n")
}

// laneTick gives crew a stream lane held by holder whose tick is ageS old.
func (f *queueFix) laneTick(repo, crew, holder string, ageS int64) {
	f.t.Helper()
	f.crewFile(repo, crew, "stream.lock.d/pid", holder+"\n")
	f.crewFile(repo, crew, "stream.tick", fmt.Sprintf(`{"pid":%s,"ts":%d,"park":300}`, holder, f.ms(ageS)))
}

func (f *queueFix) pane(cmd, name, branch, role, path string) {
	f.panes = append(f.panes, strings.Join([]string{cmd, name, branch, role, path}, "\x1f"))
}

// lead is a live lead of tier on branch feat/<name>, in a worktree of its own.
func (f *queueFix) lead(name, tier string) {
	f.t.Helper()
	dir := filepath.Join(f.root, "wt-"+name)
	f.write(filepath.Join(dir, "WORKER_TASK.md"), "tier: "+tier+"\n")
	f.pane("claude", name, "feat/"+name, "", dir)
}

func (f *queueFix) loadavg(v string) {
	f.t.Helper()
	f.write(f.env["DISPATCH_LOADAVG_FILE"], v+" 0 0 1/1 1\n")
}

func (f *queueFix) priorities(byRepo map[string]int64) {
	f.t.Helper()
	repos := map[string]int64{}
	for name, n := range byRepo {
		repos[f.commons[name]] = n
	}
	b, _ := json.Marshal(map[string]any{"repos": repos, "updated_ts": 1})
	f.write(filepath.Join(f.env["XDG_DATA_HOME"], "crew", "fleet-priority.json"), string(b))
}

func (f *queueFix) recordPath() string {
	return filepath.Join(f.env["XDG_STATE_HOME"], "crew", "fleet-due.json")
}

func (f *queueFix) record() map[holdKey]dueEntry { return readRecord(f.recordPath()) }

func (f *queueFix) writeRecord(rec map[holdKey]dueEntry) {
	f.t.Helper()
	b, _ := json.Marshal(dueDoc(rec))
	f.write(f.recordPath(), string(b))
}

// key is the record key of crew's hold id in repo.
func (f *queueFix) key(repo, crew, id string) holdKey { return holdKey{f.commons[repo], crew, id} }

func (f *queueFix) due(repo, crew string, record bool) hold.FleetVerdict {
	f.t.Helper()
	v, err := Due(f.t.Context(), f.commons[repo], crew, record, f.opts)
	if err != nil {
		f.t.Fatalf("Due(%s, %s, %v): %v", repo, crew, record, err)
	}
	return v
}

func (f *queueFix) queue(req QueueRequest) (code int, stdout, stderr string) {
	f.t.Helper()
	var out, errb bytes.Buffer
	code = Queue(f.t.Context(), req, &out, &errb, f.opts)
	return code, out.String(), errb.String()
}

func (f *queueFix) admit(req QueueRequest) (int, string) {
	f.t.Helper()
	if req.Admit == 0 {
		req.Admit = 2
	}
	code, out, _ := f.queue(req)
	return code, out
}

func (f *queueFix) doc() queueDoc {
	f.t.Helper()
	code, out, errb := f.queue(QueueRequest{JSON: true})
	if code != 0 {
		f.t.Fatalf("queue --json exit %d: %s", code, errb)
	}
	var d queueDoc
	if err := json.Unmarshal([]byte(out), &d); err != nil {
		f.t.Fatalf("not a queue document: %v\n%s", err, out)
	}
	return d
}

func (f *queueFix) statusOpts() StatusOptions {
	return StatusOptions{
		Options: Options{
			Getenv: f.opts.Getenv, Registry: f.opts.Registry, Tmux: f.opts.Tmux, Git: f.opts.Git, Clock: f.opts.Clock,
		},
		Proc:       f.procs,
		Settings:   f.opts.Settings,
		BudgetFile: f.opts.BudgetFile,
		Cmd:        f.opts.Cmd,
	}
}

func (d queueDoc) ids() []string {
	ids := []string{}
	for _, h := range d.Holds {
		ids = append(ids, h.ID)
	}
	return ids
}

func (d queueDoc) row(t *testing.T, id string) queueRow {
	t.Helper()
	for _, h := range d.Holds {
		if h.ID == id {
			return h
		}
	}
	t.Fatalf("no hold %s in %v", id, d.ids())
	return queueRow{}
}

func (d queueDoc) dueID() string {
	if d.Due == nil {
		return ""
	}
	return d.Due.ID
}

func (d queueDoc) headID() string {
	if d.Head == nil {
		return ""
	}
	return d.Head.ID
}

func reasonOf(r queueRow) string {
	if r.Reason == nil {
		return ""
	}
	return *r.Reason
}

// A repo the walk finds but the registry does not list is no member, and a
// crew whose dispatcher is not live, or whose lane is dead or stale, has its
// holds parked. A dispatcher pid that names an engine (a bare `crew register`)
// is as live as a launcher.
func TestQueueMembershipAndEligibility(t *testing.T) {
	f := newQueueFix(t, "alpha", "beta")
	f.repo("walk", false)
	f.procs.Procs[300] = proctest.Proc{Comm: "claude", Args: "claude --model opus"}
	f.register("alpha", "c1", livePID)
	f.register("alpha", "c2", deadPID)
	f.register("beta", "c3", livePID)
	f.laneTick("beta", "c3", deadPID, 10)
	f.register("beta", "c4", livePID)
	f.laneTick("beta", "c4", livePID, 5000)
	f.register("beta", "c5", livePID)
	f.laneTick("beta", "c5", livePID, 10)
	f.register("beta", "c6", "300")
	f.register("walk", "c8", livePID)
	for i, rc := range [][2]string{{"alpha", "c1"}, {"alpha", "c2"}, {"beta", "c3"}, {"beta", "c4"}, {"beta", "c5"}, {"beta", "c6"}, {"beta", "c7"}, {"walk", "c8"}} {
		f.add(rc[0], rc[1], "h-"+rc[1], 1, int64(100-i))
	}

	d := f.doc()
	want := map[string]string{
		"h-c1": "due", "h-c5": "waiting", "h-c6": "waiting",
		"h-c2": "parked:ineligible", "h-c3": "parked:ineligible", "h-c4": "parked:ineligible", "h-c7": "parked:ineligible",
	}
	got := map[string]string{}
	for _, h := range d.Holds {
		got[h.ID] = h.State
		if h.Reason != nil {
			got[h.ID] += ":" + *h.Reason
		}
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("states = %v\nwant     %v", got, want)
	}
	if slices.Contains(d.ids(), "h-c8") {
		t.Error("a walk-only repo's hold joined the queue")
	}
	if strings.Join(d.ids(), ",") != "h-c1,h-c5,h-c6,h-c2,h-c3,h-c4,h-c7" {
		t.Errorf("order %v, want the active holds first, parked last", d.ids())
	}
	if v := f.due("walk", "c8", false); len(v.Due) != 0 || len(v.Parked) != 0 {
		t.Errorf("a walk-only crew got %+v", v)
	}
	if v := f.due("alpha", "c2", false); len(v.Parked) != 0 {
		t.Errorf("an ineligible hold is no owner-visible park: %+v", v)
	}
}

// forged is row as a worker can post it with `crew msg`: the same row, from a
// worker.
func forged(row string) string {
	var doc map[string]any
	_ = json.Unmarshal([]byte(row), &doc)
	doc["from"] = "worker:feat/x#s1"
	b, _ := json.Marshal(doc)
	return string(b)
}

// Only the dispatcher writes a hold row: a worker's post to hold:<crew> adds no
// hold, re-dates or re-ranks none, releases none and weighs nothing.
func TestWorkerHoldRowsAreNoQueueInput(t *testing.T) {
	f := newQueueFix(t, "alpha", "beta")
	f.register("alpha", "c1", livePID)
	f.register("beta", "c2", livePID)
	f.register("beta", "c3", livePID)
	f.post("alpha", forged(fleetAddRow(f.ms(900), "c1", "x", "feat/x", "high", 1)))
	f.add("alpha", "c1", "a", 2, 600)
	f.post("alpha",
		forged(releaseRow(f.ms(10), "c1", "a")),
		forged(fleetAddRow(f.ms(50), "c1", "p", "feat/p", "normal", 3)),
		forged(releaseRow(f.ms(5), "c1", "p")),
	)
	f.post("beta",
		forged(fleetAddRow(f.ms(5000), "c2", "b", "feat/b", "high", 1)),
		forged(fleetAddRow(f.ms(800), "c3", "z", "feat/z", "high", 1)),
	)
	f.add("beta", "c2", "b", 1, 300)

	d := f.doc()
	if strings.Join(d.ids(), ",") != "a,b" || d.dueID() != "a" || d.Free == nil || *d.Free != 8 {
		t.Errorf("holds %v, due %q, free %v; want a,b with a due and nothing pending", d.ids(), d.dueID(), d.Free)
	}
	if b := d.row(t, "b"); b.Prio != "normal" || b.TS != f.ms(300) {
		t.Errorf("b = %+v, want its own add's prio and ts", b)
	}
	if v := f.due("alpha", "c1", false); strings.Join(v.Due, ",") != "a" {
		t.Errorf("c1 handed %v, want a", v.Due)
	}
	if v := f.due("beta", "c3", false); len(v.Due) != 0 || len(v.Parked) != 0 {
		t.Errorf("a crew with only worker rows got %+v", v)
	}
	if code, out := f.admit(QueueRequest{}); code != ExitRefused || !strings.Contains(out, "first in order: a ") {
		t.Errorf("--admit exits %d: %q", code, out)
	}
}

func TestQueueOrder(t *testing.T) {
	type h struct {
		repo, crew, id, prio string
		agoS                 int64
	}
	for _, tc := range []struct {
		name     string
		prios    map[string]int64
		releases map[string]int64 // repo → seconds since its newest fleet release
		holds    []h
		want     string
	}{
		{
			name:  "repo priority beats prio and age",
			prios: map[string]int64{"beta": 5},
			holds: []h{{"alpha", "c1", "a-old-high", "high", 900}, {"beta", "c2", "b-new-low", "low", 10}},
			want:  "b-new-low,a-old-high",
		},
		{
			name:  "a negative repo priority sorts after the default",
			prios: map[string]int64{"alpha": -1},
			holds: []h{{"alpha", "c1", "a1", "normal", 900}, {"beta", "c2", "b1", "normal", 10}},
			want:  "b1,a1",
		},
		{
			name:  "then prio, with a missing or unknown prio as normal",
			holds: []h{{"alpha", "c1", "low", "low", 900}, {"alpha", "c1", "none", "", 800}, {"alpha", "c1", "odd", "urgent", 700}, {"alpha", "c1", "high", "high", 10}},
			want:  "high,none,odd,low",
		},
		{
			name:     "then fairness: never released first, then the oldest last release",
			releases: map[string]int64{"alpha": 100, "beta": 1000},
			holds:    []h{{"alpha", "c1", "a1", "normal", 900}, {"beta", "c2", "b1", "normal", 500}, {"gamma", "c3", "g1", "normal", 10}},
			want:     "g1,b1,a1",
		},
		{
			name:  "then age",
			holds: []h{{"alpha", "c1", "young", "normal", 10}, {"alpha", "c1", "old", "normal", 900}},
			want:  "old,young",
		},
		{
			name:  "then common dir, crew and id",
			holds: []h{{"beta", "c1", "x", "normal", 100}, {"alpha", "c2", "y", "normal", 100}, {"alpha", "c1", "z", "normal", 100}, {"alpha", "c1", "w", "normal", 100}},
			want:  "w,z,y,x",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newQueueFix(t, "alpha", "beta", "gamma")
			for _, hh := range tc.holds {
				f.register(hh.repo, hh.crew, livePID)
				f.addPrio(hh.repo, hh.crew, hh.id, hh.prio, 1, hh.agoS)
			}
			for repo, agoS := range tc.releases {
				f.add(repo, "cr", "rel-"+repo, 1, agoS+10)
				f.release(repo, "cr", "rel-"+repo, agoS)
			}
			if tc.prios != nil {
				f.priorities(tc.prios)
			}
			d := f.doc()
			if got := strings.Join(d.ids(), ","); got != tc.want {
				t.Errorf("order %s, want %s", got, tc.want)
			}
			if d.Holds[0].State != "due" {
				t.Errorf("the first hold in order is %s, want due", d.Holds[0].State)
			}
		})
	}
}

// Due is the first fitting non-demoted hold, or, when none fits, the first
// fitting demoted one. Past fleetBackfillAfter only the head may be due among
// the non-demoted holds, and a fitting one behind it still keeps the demoted
// fallback closed. a (w2, 600 s) leads b (w1, 300 s) and c (w1, 100 s); a
// live trivial lead leaves a budget of 2 one free.
func TestDueOneAtATime(t *testing.T) {
	for _, tc := range []struct {
		name     string
		budget   int
		lead     bool
		backfill int
		demoted  []string
		loadavg  string
		want     string
	}{
		{name: "the first fitting hold", budget: 8, want: "a"},
		{name: "a hold that fits passes one that does not", budget: 2, lead: true, want: "b"},
		{name: "past backfill only the head", budget: 2, lead: true, backfill: 500},
		{name: "past backfill the head when it fits", budget: 2, backfill: 500, want: "a"},
		{name: "a demoted hold is passed over", budget: 2, lead: true, demoted: []string{"b"}, want: "c"},
		{name: "a demoted hold only when no other fits", budget: 2, lead: true, demoted: []string{"b", "c"}, want: "b"},
		{name: "a fitting hold behind a blocked head keeps the demoted out", budget: 2, lead: true, backfill: 500, demoted: []string{"c"}},
		{name: "nothing while the load is over the cores", budget: 8, loadavg: "9.00"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newQueueFix(t, "alpha", "beta")
			f.settings.Budget = tc.budget
			if tc.lead {
				f.lead("busy", "trivial")
			}
			if tc.backfill > 0 {
				f.settings.BackfillAfter = tc.backfill
			}
			if tc.loadavg != "" {
				f.loadavg(tc.loadavg)
			}
			f.register("alpha", "c1", livePID)
			f.register("beta", "c2", livePID)
			f.add("alpha", "c1", "a", 2, 600)
			f.add("beta", "c2", "b", 1, 300)
			f.add("beta", "c2", "c", 1, 100)
			rec := map[holdKey]dueEntry{}
			for _, id := range tc.demoted {
				rec[f.key("beta", "c2", id)] = dueEntry{Demoted: 1}
			}
			f.writeRecord(rec)

			if got := f.doc().dueID(); got != tc.want {
				t.Errorf("due %q, want %q", got, tc.want)
			}
			var got []string
			got = append(got, f.due("alpha", "c1", false).Due...)
			got = append(got, f.due("beta", "c2", false).Due...)
			if strings.Join(got, ",") != tc.want {
				t.Errorf("the crews were handed %v, want %q", got, tc.want)
			}
		})
	}
}

// Whatever the state, an evaluation hands at most one crew a due hold, and
// every reader agrees which.
func TestDueHandsAtMostOneCrewADueHold(t *testing.T) {
	rng := rand.New(rand.NewPCG(1426, 4))
	type rc struct{ repo, crew string }
	crews := []rc{{"alpha", "c1"}, {"alpha", "c2"}, {"beta", "c1"}, {"gamma", "c3"}}
	dues := 0
	for range 30 {
		f := newQueueFix(t, "alpha", "beta", "gamma")
		f.settings = QueueSettings{Budget: rng.IntN(7), BackfillAfter: rng.IntN(3) * 600, HeadTimeout: rng.IntN(3) * 300}
		for _, c := range crews {
			switch rng.IntN(5) {
			case 0:
				f.register(c.repo, c.crew, deadPID)
			case 1:
				f.register(c.repo, c.crew, livePID)
				f.laneTick(c.repo, c.crew, livePID, int64(rng.IntN(2000)))
			default:
				f.register(c.repo, c.crew, livePID)
			}
		}
		rec := map[holdKey]dueEntry{}
		for i := range rng.IntN(9) {
			c := crews[rng.IntN(len(crews))]
			id := fmt.Sprintf("h%d", i)
			f.addPrio(c.repo, c.crew, id, []string{"high", "normal", "low", ""}[rng.IntN(4)], 1+rng.IntN(3), int64(rng.IntN(3000)))
			switch rng.IntN(6) {
			case 0:
				rec[f.key(c.repo, c.crew, id)] = dueEntry{Demoted: f.ms(100)}
			case 1:
				rec[f.key(c.repo, c.crew, id)] = dueEntry{FirstDue: f.ms(int64(rng.IntN(1000)))}
			case 2:
				f.release(c.repo, c.crew, id, int64(rng.IntN(300)))
			}
		}
		for i := range rng.IntN(3) {
			f.lead(fmt.Sprintf("l%d", i), []string{"trivial", "standard", "deep"}[rng.IntN(3)])
		}
		if rng.IntN(8) == 0 {
			f.loadavg("9.00")
		}
		f.writeRecord(rec)
		for range 4 {
			c := crews[rng.IntN(len(crews))]
			f.due(c.repo, c.crew, true)
			var handed []string
			for _, c := range crews {
				handed = append(handed, f.due(c.repo, c.crew, false).Due...)
			}
			if len(handed) > 1 {
				t.Fatalf("two crews handed a due hold: %v", handed)
			}
			if want := f.doc().dueID(); strings.Join(handed, "") != want {
				t.Fatalf("crews were handed %v, the queue says %q", handed, want)
			}
			dues += len(handed)
			f.advance(int64(rng.IntN(700)))
		}
	}
	if dues == 0 {
		t.Error("no state had a due hold: the seeds test nothing")
	}
}

// A fleet hold released within 120 s whose branch has no lead pane still
// weighs, so a second release cannot spend the same capacity.
func TestDueFreeIncludesPending(t *testing.T) {
	f := newQueueFix(t, "alpha", "beta")
	f.settings.Budget = 4
	f.register("alpha", "c1", livePID)
	f.add("alpha", "c1", "h", 2, 300)
	f.add("beta", "c2", "r", 3, 400)
	f.release("beta", "c2", "r", 60)

	d := f.doc()
	if d.Free == nil || *d.Free != 1 || d.dueID() != "" {
		t.Errorf("60 s after a deep release: free %v, due %q; want 1 and nothing", d.Free, d.dueID())
	}
	f.advance(70)
	if d := f.doc(); *d.Free != 4 || d.dueID() != "h" {
		t.Errorf("130 s after: free %d, due %q; want 4 and h", *d.Free, d.dueID())
	}
	f.advance(-70)
	f.pane("fish", "rook", "feat/r", "", "/nonexistent")
	if d := f.doc(); *d.Free != 4 || d.dueID() != "h" {
		t.Errorf("with the released branch's lead pane up: free %d, due %q; want 4 and h", *d.Free, d.dueID())
	}

	// A weight that is no number cannot be weighed: released, it weighs
	// nothing; outstanding, it is parked as oversize.
	unweighable := func(id string) string {
		return holdRowJSON(f.ms(20), "c2", map[string]any{"id": id, "wait": map[string]any{"kind": "fleet", "weight": "heavy"}, "task": map[string]any{"branch": "feat/" + id}})
	}
	f.post("beta", unweighable("u"), releaseRow(f.ms(10), "c2", "u"), unweighable("w"))
	if d := f.doc(); *d.Free != 4 || d.dueID() != "h" || reasonOf(d.row(t, "w")) != "oversize" {
		t.Errorf("with an unweighable release: free %d, due %q, w %+v; want 4, h and w oversize", *d.Free, d.dueID(), d.row(t, "w"))
	}
}

// A full machine demotes nothing: a hold that never fits is never due, so its
// clock never starts.
func TestCongestionDemotesNothing(t *testing.T) {
	f := newQueueFix(t, "alpha", "beta")
	f.settings.Budget = 2
	f.lead("busy", "standard")
	f.register("alpha", "c1", livePID)
	f.register("beta", "c2", livePID)
	f.add("alpha", "c1", "a", 1, 100)
	f.add("beta", "c2", "b", 1, 50)
	for range 4 {
		if v := f.due("beta", "c2", true); len(v.Due) != 0 {
			t.Fatalf("due %v on a full machine", v.Due)
		}
		f.advance(headTimeout)
	}
	if _, err := os.Stat(f.recordPath()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the record was written with nothing due: %v", err)
	}
	f.panes = nil
	if v := f.due("alpha", "c1", true); strings.Join(v.Due, ",") != "a" {
		t.Errorf("once capacity frees, due %v, want a", v.Due)
	}
	if got := f.record(); len(got) != 1 || got[f.key("alpha", "c1", "a")] != (dueEntry{FirstDue: f.now.UnixMilli()}) {
		t.Errorf("record %v, want a's clock started now", got)
	}
}

// A hold that stops being due (capacity was taken again) loses its clock, so
// the time it waited undue never counts toward a demotion.
func TestTheClockCountsOnlyTimeDue(t *testing.T) {
	f := newQueueFix(t, "alpha")
	f.settings.Budget = 3
	f.register("alpha", "c1", livePID)
	f.add("alpha", "c1", "a", 2, 100)
	f.due("alpha", "c1", true)
	f.advance(headTimeout / 2)
	f.lead("taker", "standard")
	if v := f.due("alpha", "c1", true); len(v.Due) != 0 {
		t.Fatalf("due %v with the capacity taken", v.Due)
	}
	if got := f.record(); len(got) != 0 {
		t.Errorf("record %v, want the clock cleared", got)
	}
	f.advance(headTimeout / 2)
	f.panes = nil
	f.due("alpha", "c1", true)
	if got := f.record(); got[f.key("alpha", "c1", "a")] != (dueEntry{FirstDue: f.now.UnixMilli()}) {
		t.Errorf("record %v, want a due since now, not demoted", got)
	}
}

// The demotion clock is recorded by any dispatcher's run, not only the owner's.
// A demoted hold stays behind until it is released, and a demoted hold due for
// another timeout is parked and reported to its owner.
func TestDemotionCountsTimeDue(t *testing.T) {
	f := newQueueFix(t, "alpha", "beta", "gamma")
	f.register("alpha", "c1", livePID)
	f.register("beta", "c2", livePID)
	f.register("gamma", "c3", livePID)
	f.add("alpha", "c1", "a", 2, 100)
	f.add("beta", "c2", "b", 2, 50)
	ka, kb := f.key("alpha", "c1", "a"), f.key("beta", "c2", "b")
	t0 := f.now.UnixMilli()

	// c1 never runs a recording `crew hold due`; c2 does.
	if v := f.due("beta", "c2", true); len(v.Due) != 0 {
		t.Fatalf("c2 handed %v while a leads", v.Due)
	}
	if got := f.record(); got[ka] != (dueEntry{FirstDue: t0}) {
		t.Fatalf("record %v, want a due since t0", got)
	}
	f.advance(headTimeout - 1)
	f.due("beta", "c2", true)
	if got := f.record(); got[ka] != (dueEntry{FirstDue: t0}) {
		t.Fatalf("a timeout less a second: record %v", got)
	}

	f.advance(1)
	t1 := f.now.UnixMilli()
	if v := f.due("beta", "c2", true); strings.Join(v.Due, ",") != "b" {
		t.Fatalf("at the timeout c2 is handed %v, want b in the same run", v.Due)
	}
	if got := f.record(); len(got) != 2 || got[ka] != (dueEntry{Demoted: t1}) || got[kb] != (dueEntry{FirstDue: t1}) {
		t.Fatalf("record %v, want a demoted and b due", got)
	}
	if v := f.due("alpha", "c1", false); len(v.Due) != 0 || len(v.Parked) != 0 {
		t.Errorf("a demoted hold is neither due nor parked yet: %+v", v)
	}
	d := f.doc()
	if a := d.row(t, "a"); a.State != "demoted" || a.DemotedAt == nil || *a.DemotedAt != t1 || a.FirstDue != nil {
		t.Errorf("a = %+v, want demoted at t1", a)
	}
	if d.headID() != "b" || strings.Join(d.ids(), ",") != "b,a" {
		t.Errorf("head %q, order %v; want b ahead of the demoted a", d.headID(), d.ids())
	}

	// A release elsewhere frees capacity and leaves a where it is.
	f.add("gamma", "c3", "g", 1, 1)
	f.release("gamma", "c3", "g", 0)
	f.due("beta", "c2", true)
	if d := f.doc(); d.dueID() != "b" || d.row(t, "a").State != "demoted" || d.headID() != "b" {
		t.Errorf("after a release elsewhere: due %q head %q a %s", d.dueID(), d.headID(), d.row(t, "a").State)
	}

	// b is dispatched; a, alone, is due as the demoted fallback.
	f.release("beta", "c2", "b", 0)
	f.advance(10)
	t2 := f.now.UnixMilli()
	f.due("beta", "c2", true)
	if v := f.due("alpha", "c1", false); strings.Join(v.Due, ",") != "a" {
		t.Fatalf("a alone: c1 handed %v", v.Due)
	}
	if got := f.record(); len(got) != 1 || got[ka] != (dueEntry{FirstDue: t2, Demoted: t1}) {
		t.Fatalf("record %v, want a demoted and due since t2", got)
	}

	// A worker's rows are no input to the clock.
	f.post("alpha", worker(f.now.Unix(), "c1", "feat/a", "done"), worker(f.now.Unix(), "c1", "feat/a", "done"))
	f.advance(headTimeout)
	t3 := f.now.UnixMilli()
	if v := f.due("beta", "c2", true); len(v.Due) != 0 {
		t.Errorf("after the second timeout c2 is handed %v", v.Due)
	}
	if got := f.record(); got[ka] != (dueEntry{Demoted: t1, Parked: t3}) {
		t.Fatalf("record %v, want a parked at t3", got)
	}
	v := f.due("alpha", "c1", false)
	if len(v.Due) != 0 || fmt.Sprint(v.Parked) != fmt.Sprint([]hold.ParkedHold{{ID: "a", Reason: "demoted"}}) {
		t.Errorf("the owner sees %+v, want a parked as demoted", v)
	}
	d = f.doc()
	if a := d.row(t, "a"); a.State != "parked" || reasonOf(a) != "demoted" || d.dueID() != "" || d.headID() != "" {
		t.Errorf("a = %+v, due %q, head %q", a, d.dueID(), d.headID())
	}
	if code, out := f.admit(QueueRequest{}); code != ExitFits || out != "" {
		t.Errorf("with only a parked hold, --admit exits %d: %q", code, out)
	}
}

// A hold id is unique only within its crew's sink: two holds that share one,
// here even the crew id, keep clocks of their own.
func TestHoldsSharingAnIDKeepTheirOwnClocks(t *testing.T) {
	f := newQueueFix(t, "alpha", "beta")
	f.register("alpha", "c1", livePID)
	f.register("beta", "c1", livePID)
	f.add("alpha", "c1", "h", 2, 100)
	f.add("beta", "c1", "h", 2, 50)
	f.due("alpha", "c1", true)
	f.advance(headTimeout)
	if v := f.due("beta", "c1", true); strings.Join(v.Due, ",") != "h" {
		t.Errorf("at the timeout beta's c1 is handed %v, want its own h", v.Due)
	}
	now := f.now.UnixMilli()
	want := map[holdKey]dueEntry{f.key("alpha", "c1", "h"): {Demoted: now}, f.key("beta", "c1", "h"): {FirstDue: now}}
	if got := f.record(); !maps.Equal(got, want) {
		t.Errorf("record %v, want %v", got, want)
	}
	if v := f.due("alpha", "c1", false); len(v.Due) != 0 {
		t.Errorf("alpha's demoted h is handed %v", v.Due)
	}
	states := map[string]string{}
	for _, r := range f.doc().Holds {
		states[r.Common] = r.State
	}
	if want := map[string]string{f.commons["alpha"]: "demoted", f.commons["beta"]: "due"}; fmt.Sprint(states) != fmt.Sprint(want) {
		t.Errorf("states %v, want %v", states, want)
	}
}

// A crew whose hold fold fails is unknown this evaluation, not released: it
// hides no other crew's hold, its repo's releases still weigh and rank it, and
// a recording run leaves its clocks as they were.
func TestAFailedFoldCostsOnlyItsCrew(t *testing.T) {
	f := newQueueFix(t, "alpha", "beta")
	f.register("alpha", "c1", livePID)
	f.register("alpha", "c2", livePID)
	f.register("beta", "c3", livePID)
	f.add("alpha", "c1", "a", 1, 600)
	f.add("alpha", "c2", "b", 1, 300)
	f.add("alpha", "c2", "r", 3, 400)
	f.release("alpha", "c2", "r", 30)
	f.post("alpha", fmt.Sprintf(`{"ts":%d,"crew_id":"c2","from":"dispatcher:c2","to":"hold:c2","kind":"msg","body":"5"}`, f.ms(200)))
	f.add("beta", "c3", "g", 1, 100)

	d := f.doc()
	if strings.Join(d.ids(), ",") != "g,a" || d.dueID() != "g" || d.Free == nil || *d.Free != 5 {
		t.Errorf("holds %v, due %q, free %v; want g ahead of the just-released alpha's a, and r pending", d.ids(), d.dueID(), d.Free)
	}

	kb := f.key("alpha", "c2", "b")
	f.writeRecord(map[holdKey]dueEntry{kb: {Demoted: 1}, f.key("alpha", "c1", "gone"): {Demoted: 1}})
	f.due("alpha", "c1", true)
	want := map[holdKey]dueEntry{kb: {Demoted: 1}, f.key("beta", "c3", "g"): {FirstDue: f.now.UnixMilli()}}
	if got := f.record(); !maps.Equal(got, want) {
		t.Errorf("record %v, want %v", got, want)
	}
}

// A registered repo whose bus cannot be read this scan keeps its clocks.
func TestAnUnreadableBusKeepsItsClocks(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode 000 file")
	}
	f := newQueueFix(t, "alpha", "beta")
	f.register("alpha", "c1", livePID)
	f.register("beta", "c2", livePID)
	f.add("alpha", "c1", "a", 1, 600)
	f.add("beta", "c2", "b", 1, 300)
	kb := f.key("beta", "c2", "b")
	f.writeRecord(map[holdKey]dueEntry{kb: {Demoted: 1}})
	if err := os.Chmod(f.busPath("beta"), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(f.busPath("beta"), 0o644) })

	f.due("alpha", "c1", true)
	want := map[holdKey]dueEntry{kb: {Demoted: 1}, f.key("alpha", "c1", "a"): {FirstDue: f.now.UnixMilli()}}
	if got := f.record(); !maps.Equal(got, want) {
		t.Errorf("record %v, want %v", got, want)
	}
	f.release("alpha", "c1", "a", 0)
	f.due("alpha", "c1", true)
	if got := f.record(); !maps.Equal(got, map[holdKey]dueEntry{kb: {Demoted: 1}}) {
		t.Errorf("with no hold read anywhere: record %v, want b's entry kept", got)
	}
}

// Losing the record restarts the clocks: the demotion comes a timeout later.
func TestLosingTheRecordRestartsTheClocks(t *testing.T) {
	f := newQueueFix(t, "alpha")
	f.register("alpha", "c1", livePID)
	f.add("alpha", "c1", "a", 2, 100)
	ka := f.key("alpha", "c1", "a")
	f.due("alpha", "c1", true)
	if err := os.Remove(f.recordPath()); err != nil {
		t.Fatal(err)
	}
	f.advance(headTimeout)
	f.due("alpha", "c1", true)
	if got := f.record(); got[ka] != (dueEntry{FirstDue: f.now.UnixMilli()}) {
		t.Fatalf("record %v, want a fresh clock", got)
	}
	for name, text := range map[string]string{
		"a corrupt record":                   "{not json",
		"a record keyed by the bare hold id": fmt.Sprintf(`{"holds":{"a":{"first_due":%d}}}`, f.ms(headTimeout)),
	} {
		f.write(f.recordPath(), text)
		f.advance(headTimeout)
		f.due("alpha", "c1", true)
		if got := f.record(); got[ka] != (dueEntry{FirstDue: f.now.UnixMilli()}) {
			t.Fatalf("%s: %v, want a fresh clock", name, got)
		}
	}
	f.advance(headTimeout)
	f.due("alpha", "c1", true)
	if got := f.record(); got[ka].Demoted != f.now.UnixMilli() {
		t.Errorf("record %v, want a demoted one timeout after its clock restarted", got)
	}
}

func TestHeadTimeoutZeroNeverDemotes(t *testing.T) {
	f := newQueueFix(t, "alpha")
	f.settings.HeadTimeout = 0
	f.register("alpha", "c1", livePID)
	f.add("alpha", "c1", "a", 2, 100)
	t0 := f.now.UnixMilli()
	for range 3 {
		if v := f.due("alpha", "c1", true); strings.Join(v.Due, ",") != "a" {
			t.Fatalf("due %v", v.Due)
		}
		f.advance(100 * headTimeout)
	}
	if got := f.record(); got[f.key("alpha", "c1", "a")] != (dueEntry{FirstDue: t0}) {
		t.Errorf("record %v, want a due since t0 and never demoted", got)
	}
}

// A hold heavier than the budget (lowered after it was added) never binds:
// parked as oversize, reported to its owner, and no bar to a fresh lead.
func TestOversizeHoldIsParked(t *testing.T) {
	f := newQueueFix(t, "alpha")
	f.settings.Budget = 2
	f.register("alpha", "c1", livePID)
	f.add("alpha", "c1", "deep", 3, 100)
	d := f.doc()
	if r := d.row(t, "deep"); r.State != "parked" || reasonOf(r) != "oversize" || d.dueID() != "" || d.headID() != "" {
		t.Errorf("deep = %+v, due %q, head %q", r, d.dueID(), d.headID())
	}
	v := f.due("alpha", "c1", false)
	if len(v.Due) != 0 || fmt.Sprint(v.Parked) != fmt.Sprint([]hold.ParkedHold{{ID: "deep", Reason: "oversize"}}) {
		t.Errorf("the owner sees %+v", v)
	}
	if code, out := f.admit(QueueRequest{}); code != ExitFits || out != "" {
		t.Errorf("--admit exits %d: %q", code, out)
	}
	f.settings.Budget = 0
	if r := f.doc().row(t, "deep"); reasonOf(r) != "oversize" {
		t.Errorf("budget 0: %+v, want oversize", r)
	}
}

// Only a recording Due writes the record; no evaluator writes a bus row.
func TestOnlyARecordingDueWrites(t *testing.T) {
	f := newQueueFix(t, "alpha", "beta")
	f.register("alpha", "c1", livePID)
	f.add("alpha", "c1", "a", 2, 100)
	f.post("beta", worker(f.now.Unix(), "c2", "feat/x", "working"))
	buses := map[string][]byte{}
	for name := range f.commons {
		buses[name], _ = os.ReadFile(f.busPath(name))
	}
	readers := map[string]func(){
		"queue --json":  func() { f.queue(QueueRequest{JSON: true}) },
		"queue table":   func() { f.queue(QueueRequest{}) },
		"queue --admit": func() { f.queue(QueueRequest{Admit: 2, Hold: "a"}) },
		"status":        func() { CollectStatus(t.Context(), StatusRequest{}, io.Discard, f.statusOpts()) },
		"due read-only": func() { f.due("alpha", "c1", false) },
	}
	for name, read := range readers {
		read()
		if _, err := os.Stat(f.recordPath()); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s wrote the record (%v)", name, err)
		}
	}
	// A record the evaluation would change (a demotion, a stale entry) is left
	// byte for byte by every reader.
	f.writeRecord(map[holdKey]dueEntry{f.key("alpha", "c1", "a"): {FirstDue: f.ms(headTimeout + 1)}, f.key("alpha", "c1", "gone"): {Demoted: 1}})
	before, _ := os.ReadFile(f.recordPath())
	for name, read := range readers {
		read()
		if after, _ := os.ReadFile(f.recordPath()); !bytes.Equal(before, after) {
			t.Errorf("%s changed the record:\n%s\n%s", name, before, after)
		}
	}

	f.due("alpha", "c1", true)
	now := f.now.UnixMilli()
	if got := f.record(); len(got) != 1 || got[f.key("alpha", "c1", "a")] != (dueEntry{FirstDue: now, Demoted: now}) {
		t.Errorf("record %v, want a demoted, due again, and gone dropped", got)
	}
	if info, err := os.Stat(f.recordPath()); err != nil || info.Mode().Perm() != 0o644 {
		t.Errorf("record mode %v (%v), want 0644", info.Mode(), err)
	}

	// While another holder has the lock, a recording run evaluates read-only.
	defer func(w time.Duration) { lockWait = w }(lockWait)
	lockWait = 50 * time.Millisecond
	lockDir := filepath.Join(f.env["XDG_STATE_HOME"], "crew", "fleet-due.lock.d")
	if !lock.Acquire(lockDir, "someone-else") {
		t.Fatal("could not take the record lock")
	}
	before, _ = os.ReadFile(f.recordPath())
	f.advance(headTimeout)
	f.due("alpha", "c1", true)
	lock.Release(lockDir)
	if after, _ := os.ReadFile(f.recordPath()); !bytes.Equal(before, after) {
		t.Errorf("a run without the lock wrote the record:\n%s\n%s", before, after)
	}

	// With no fleet hold left, a recording run empties a record that has
	// entries, and writes none where there is none.
	f.release("alpha", "c1", "a", 0)
	f.due("alpha", "c1", true)
	if data, _ := os.ReadFile(f.recordPath()); string(data) != `{"holds":{}}`+"\n" {
		t.Errorf("record %q, want it emptied", data)
	}
	if err := os.Remove(f.recordPath()); err != nil {
		t.Fatal(err)
	}
	f.due("alpha", "c1", true)
	if _, err := os.Stat(f.recordPath()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("an empty queue wrote a record (%v)", err)
	}

	for name, want := range buses {
		if name == "alpha" {
			continue // the test's own release row
		}
		if got, _ := os.ReadFile(f.busPath(name)); !bytes.Equal(got, want) {
			t.Errorf("%s's bus changed", name)
		}
	}
	got, _ := os.ReadFile(f.busPath("alpha"))
	if want := append(buses["alpha"], []byte(releaseRow(f.ms(0), "c1", "a")+"\n")...); !bytes.Equal(got, want) {
		t.Errorf("alpha's bus has more than the test's rows:\n%s", got)
	}
}

// `crew hold due` records only under a dispatcher identity: a worker session
// evaluates the same queue read-only.
func TestHoldDueRecordsOnlyForTheDispatcher(t *testing.T) {
	f := newQueueFix(t, "alpha")
	f.register("alpha", "c1", livePID)
	f.add("alpha", "c1", "a", 2, 100)
	common := f.commons["alpha"]
	paths := bus.Paths{Common: common, Dir: common + "/crew", Log: common + "/crew/events.jsonl"}
	run := func(probes sender.Probes) []string {
		t.Helper()
		var out, errb bytes.Buffer
		code := hold.Run(t.Context(), hold.Request{Action: "due", JSON: true, WithParked: true, Crew: "c1"}, paths, &out, &errb, hold.Options{
			Clock:  f.opts.Clock,
			Sender: probes,
			FleetDue: func(ctx context.Context, common, crew string, record bool) (hold.FleetVerdict, error) {
				return Due(ctx, common, crew, record, f.opts)
			},
		})
		var doc struct {
			Due []struct {
				ID string `json:"id"`
			} `json:"due"`
		}
		if code != 0 || json.Unmarshal(out.Bytes(), &doc) != nil {
			t.Fatalf("hold due exit %d: %s%s", code, out.String(), errb.String())
		}
		var ids []string
		for _, d := range doc.Due {
			ids = append(ids, d.ID)
		}
		return ids
	}
	workerEnv := sender.Probes{Getenv: func(k string) string {
		if k == "CREW_WORKER_ID" {
			return "worker:feat/x#s1"
		}
		return ""
	}}
	if ids := run(workerEnv); strings.Join(ids, ",") != "a" {
		t.Errorf("a worker session sees due %v, want a", ids)
	}
	if _, err := os.Stat(f.recordPath()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a worker session wrote the record (%v)", err)
	}
	if ids := run(sender.Probes{}); strings.Join(ids, ",") != "a" {
		t.Errorf("the dispatcher sees due %v, want a", ids)
	}
	if got := f.record(); got[f.key("alpha", "c1", "a")] != (dueEntry{FirstDue: f.now.UnixMilli()}) {
		t.Errorf("record %v, want a's clock started", got)
	}
}

func TestQueueAdmit(t *testing.T) {
	f := newQueueFix(t, "alpha", "beta")
	f.register("alpha", "c1", livePID)
	f.register("beta", "c2", livePID)
	f.register("beta", "c3", deadPID)
	if code, out := f.admit(QueueRequest{}); code != ExitFits || out != "" {
		t.Errorf("no fleet hold: exit %d %q", code, out)
	}

	f.add("alpha", "c1", "a", 2, 600)
	f.add("beta", "c2", "b", 1, 300)
	refusal := "fleet holds are waiting — first in order: a (acme/alpha, crew c1, waited 10m)\n"
	for _, tc := range []struct {
		name string
		req  QueueRequest
		want int
	}{
		{"a fresh lead waits behind the holds", QueueRequest{}, ExitRefused},
		{"the release of the due hold", QueueRequest{Hold: "a"}, ExitFits},
		{"the due hold in its own crew", QueueRequest{Hold: "a", Crew: "c1"}, ExitFits},
		{"the due id in another crew", QueueRequest{Hold: "a", Crew: "c2"}, ExitRefused},
		{"a waiting hold that is not due", QueueRequest{Hold: "b"}, ExitRefused},
		{"a branch with no lead pane", QueueRequest{Own: []string{"feat/other"}}, ExitRefused},
	} {
		code, out := f.admit(tc.req)
		if code != tc.want || (code == ExitRefused) != (out == refusal) || (code == ExitFits && out != "") {
			t.Errorf("%s: exit %d %q, want %d", tc.name, code, out, tc.want)
		}
	}
	f.pane("fish", "mine", "feat/mine", "", "/nonexistent")
	if code, out := f.admit(QueueRequest{Own: []string{"feat/other", "feat/mine"}}); code != ExitFits || out != "" {
		t.Errorf("a re-dispatch over a branch with a lead pane: exit %d %q", code, out)
	}

	ka, kb := f.key("alpha", "c1", "a"), f.key("beta", "c2", "b")
	f.writeRecord(map[holdKey]dueEntry{ka: {Demoted: 1}, kb: {Demoted: 1}})
	if code, out := f.admit(QueueRequest{}); code != ExitRefused || !strings.Contains(out, "first in order: a ") {
		t.Errorf("demoted holds still wait: exit %d %q", code, out)
	}
	f.writeRecord(map[holdKey]dueEntry{ka: {Parked: 1}, kb: {Demoted: 1, Parked: 1}})
	f.add("beta", "c3", "dead-crew", 1, 900)
	if code, out := f.admit(QueueRequest{}); code != ExitFits || out != "" {
		t.Errorf("only parked holds remain: exit %d %q", code, out)
	}

	f.writeRecord(nil)
	f.settingsErr = errors.New("boom")
	if code, _, errb := f.queue(QueueRequest{Admit: 2}); code != ExitError || errb != "crew: fleet queue: boom\n" {
		t.Errorf("a settings error: exit %d %q", code, errb)
	}
	f.settingsErr = nil
	for _, req := range []QueueRequest{{Admit: -1}, {Hold: "a"}, {Own: []string{"feat/x"}}} {
		if code, _, errb := f.queue(req); code != ExitError || errb != QueueUsage+"\n" {
			t.Errorf("%+v: exit %d %q, want the usage", req, code, errb)
		}
	}
}

func TestQueueDocument(t *testing.T) {
	f := newQueueFix(t, "alpha", "beta")
	f.settings.Budget = 3
	f.register("alpha", "c1", livePID)
	f.register("beta", "c2", livePID)
	f.priorities(map[string]int64{"beta": 2})
	f.addPrio("alpha", "c1", "a", "high", 2, 600)
	f.addPrio("beta", "c2", "b", "", 3, 300)
	f.add("beta", "c2", "c", 1, 100)
	f.writeRecord(map[holdKey]dueEntry{f.key("beta", "c2", "c"): {Demoted: f.ms(50)}})

	d := f.doc()
	if d.TS != f.now.UnixMilli() || d.Budget != 3 || d.Free == nil || *d.Free != 3 || d.LoadOver {
		t.Errorf("doc header %+v", d)
	}
	if d.Due == nil || *d.Due != (queueRef{Repo: "acme/beta", Common: f.commons["beta"], CrewID: "c2", ID: "b"}) {
		t.Errorf("due %+v", d.Due)
	}
	if d.Head == nil || d.Head.ID != "b" || d.Head.WaitedS != 300 {
		t.Errorf("head %+v", d.Head)
	}
	b := d.row(t, "b")
	if b.Branch != "feat/b" || b.Prio != "normal" || b.RepoPrio != 2 || b.Weight != 3 || b.TS != f.ms(300) ||
		b.WaitedS != 300 || b.State != "due" || b.Reason != nil || b.FirstDue == nil || *b.FirstDue != f.now.UnixMilli() || b.DemotedAt != nil {
		t.Errorf("b = %+v", b)
	}
	if c := d.row(t, "c"); c.State != "demoted" || c.DemotedAt == nil || *c.DemotedAt != f.ms(50) || c.FirstDue != nil {
		t.Errorf("c = %+v", c)
	}
	if a := d.row(t, "a"); a.State != "waiting" || a.Repo != "acme/alpha" || a.Prio != "high" || a.RepoPrio != 0 {
		t.Errorf("a = %+v", a)
	}
	if strings.Join(d.ids(), ",") != "b,a,c" {
		t.Errorf("order %v, want b,a then the demoted c", d.ids())
	}

	code, out, _ := f.queue(QueueRequest{JSON: true, Crew: "c1"})
	var only queueDoc
	if err := json.Unmarshal([]byte(out), &only); code != 0 || err != nil || strings.Join(only.ids(), ",") != "a" || only.dueID() != "b" {
		t.Errorf("--crew c1: exit %d, holds %v, due %q", code, only.ids(), only.dueID())
	}
	var raw map[string]any
	_ = json.Unmarshal([]byte(out), &raw)
	row := raw["holds"].([]any)[0].(map[string]any)
	for _, k := range []string{"repo", "common", "crew_id", "id", "branch", "prio", "repo_prio", "weight", "ts", "waited_s", "state", "reason", "first_due", "demoted_at"} {
		if _, ok := row[k]; !ok {
			t.Errorf("a hold row lacks %q: %v", k, row)
		}
	}
}

func TestQueueTable(t *testing.T) {
	f := newQueueFix(t, "alpha")
	f.register("alpha", "c1", livePID)
	f.post("alpha", fleetAddRow(f.ms(90), "c1", "a", "feat/a\x1b[2J", "high", 2))
	f.add("alpha", "c9", "b", 1, 30)
	code, out, _ := f.queue(QueueRequest{})
	want := "STATE              REPO        CREW  ID  PRIO    WEIGHT  WAITED  BRANCH\n" +
		"due                acme/alpha  c1    a   high    2       1m      feat/a?[2J\n" +
		"parked:ineligible  acme/alpha  c9    b   normal  1       30s     feat/b\n"
	if code != 0 || out != want {
		t.Errorf("exit %d, table\n%s\nwant\n%s", code, out, want)
	}
	f.settingsErr = errors.New("boom")
	if code, _, errb := f.queue(QueueRequest{}); code != ExitError || errb != "crew: fleet queue: boom\n" {
		t.Errorf("a settings error: exit %d %q", code, errb)
	}
}

func TestDueErrors(t *testing.T) {
	f := newQueueFix(t, "alpha")
	f.register("alpha", "c1", livePID)
	if _, err := Due(t.Context(), f.commons["alpha"], "c1", true, f.opts); err != nil {
		t.Errorf("no fleet hold reads no settings: %v", err)
	}
	f.add("alpha", "c1", "a", 2, 100)
	f.settingsErr = errors.New("boom")
	if _, err := Due(t.Context(), f.commons["alpha"], "c1", false, f.opts); err == nil || err.Error() != "boom" {
		t.Errorf("a settings error: %v", err)
	}
	f.settingsErr = nil
	f.write(filepath.Join(f.env["XDG_STATE_HOME"], "crew"), "a file where the state dir goes")
	if _, err := Due(t.Context(), f.commons["alpha"], "c1", true, f.opts); err == nil {
		t.Error("a record that cannot be written must fail the run")
	}
	if v, err := Due(t.Context(), f.commons["alpha"], "c1", false, f.opts); err != nil || strings.Join(v.Due, ",") != "a" {
		t.Errorf("a read-only run needs no record dir: %v %v", v, err)
	}
}

func TestResolveQueueSettings(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "dispatch-config")
	for _, tc := range []struct {
		settings string
		want     QueueSettings
	}{
		{`{}`, QueueSettings{8, 1200, 900}},
		{`{"fleetBudget":3,"fleetBackfillAfter":60.9,"fleetHeadTimeout":0}`, QueueSettings{3, 60, 0}},
		{`{"fleetBudget":-1,"fleetBackfillAfter":-5,"fleetHeadTimeout":1e30}`, QueueSettings{0, 0, maxSeconds}},
		{`{"fleetBudget":99999999}`, QueueSettings{1000000, 1200, 900}},
	} {
		if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf '%s\\n' '"+tc.settings+"'\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		got, err := ResolveQueueSettings(t.Context(), bin, io.Discard)
		if err != nil || got != tc.want {
			t.Errorf("%s: %+v, %v; want %+v", tc.settings, got, err, tc.want)
		}
	}
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho '{\"fleetHeadTimeout\":\"x\"}'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveQueueSettings(t.Context(), bin, io.Discard); err == nil {
		t.Error("a non-numeric setting must error")
	}
	if _, err := ResolveQueueSettings(t.Context(), filepath.Join(dir, "absent"), io.Discard); err == nil {
		t.Error("a missing dispatch-config must error")
	}
}
