package stream

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/clock"
	"github.com/noamsto/dispatcher/crew/internal/lock"
	"github.com/noamsto/dispatcher/crew/internal/remote"
	"github.com/noamsto/dispatcher/crew/internal/sender"
	"github.com/noamsto/dispatcher/crew/internal/watch"
)

// fakeTime is a clock the poll Sleep advances, so a timeout costs no real time.
type fakeTime struct {
	mu sync.Mutex
	t  time.Time
}

func (f *fakeTime) now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

func (f *fakeTime) sleep(string, io.Writer) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t = f.t.Add(2 * time.Second)
	return nil
}

type fleetWatchEnv struct {
	t     *testing.T
	ft    *fakeTime
	state string
	opts  FleetWatchOptions
}

func newFleetWatchEnv(t *testing.T, commons ...string) *fleetWatchEnv {
	t.Helper()
	ft := &fakeTime{t: time.Now()}
	e := &fleetWatchEnv{t: t, ft: ft, state: t.TempDir()}
	e.opts = FleetWatchOptions{
		Watch:     watch.Options{Clock: clock.Clock{Now: ft.now}, Sleep: ft.sleep},
		Discover:  okRepos(commons...),
		StateRoot: e.state,
	}
	return e
}

func (e *fleetWatchEnv) run(req FleetWatchRequest) (int, string, string) {
	e.t.Helper()
	if req.Crew == "" {
		req.Crew = "c1"
	}
	var out, errb lockedWriter
	rc := RunFleetWatch(req, &out, &errb, e.opts)
	return rc, out.str(), errb.str()
}

func (e *fleetWatchEnv) call(timeout string) (int, string, string) {
	e.t.Helper()
	return e.run(FleetWatchRequest{Timeout: timeout})
}

func wantBatch(t *testing.T, out, repo string, ts int64) {
	t.Helper()
	if !strings.Contains(out, fmt.Sprintf(`"repo":%q`, repo)) || !strings.Contains(out, fmt.Sprintf(`"ts":%d`, ts)) {
		t.Fatalf("want a batch for %s ts %d, got %q", repo, ts, out)
	}
}

func crewPaths(common string) bus.Paths {
	return bus.Paths{Common: common, Dir: common + "/crew", Log: common + "/crew/events.jsonl"}
}

func TestFleetWatchWakesOnEventInSecondRepo(t *testing.T) {
	alpha, beta := gitRepo(t, "alpha"), gitRepo(t, "beta")
	appendRow(t, alpha, statusRow(1000, "c1", "worker:feat/a", "working"))
	appendRow(t, beta, statusRow(1000, "c1", "worker:feat/b", "working"))
	appendRow(t, beta, statusRow(2000, "c1", "worker:feat/b", "pr_open"))
	e := newFleetWatchEnv(t, alpha, beta)

	rc, out, errb := e.call("60")
	if rc != 0 {
		t.Fatalf("rc %d, stderr %q", rc, errb)
	}
	wantBatch(t, out, "beta", 2000)
	if readCursor(beta) != "2000" {
		t.Errorf("beta cursor %q", readCursor(beta))
	}
}

func TestFleetWatchTimeoutPrintsNothing(t *testing.T) {
	alpha := gitRepo(t, "alpha")
	appendRow(t, alpha, statusRow(1000, "c1", "worker:feat/a", "working"))
	cursor := filepath.Join(alpha, "crew", "crews", "c1", "cursor")
	e := newFleetWatchEnv(t, alpha)

	rc, out, errb := e.call("3")
	if rc != 0 || out != "" {
		t.Fatalf("rc %d stdout %q", rc, out)
	}
	if !strings.Contains(errb, "crew: fleet watch park ended after 3s — no new events\n") {
		t.Errorf("stderr %q", errb)
	}
	if _, err := os.Stat(cursor); err == nil {
		t.Error("a cursor was written on an empty park")
	}
}

func TestFleetWatchSharesCursorWithWatchRun(t *testing.T) {
	alpha := gitRepo(t, "alpha")
	appendRow(t, alpha, statusRow(1000, "c1", "worker:feat/a", "pr_open"))
	e := newFleetWatchEnv(t, alpha)
	runWatch := func(timeout string) (int, string) {
		var out, errb lockedWriter
		rc := watch.Run(watch.Request{Crew: "c1", Timeout: timeout}, crewPaths(alpha), &out, &errb, e.opts.Watch)
		return rc, out.str()
	}

	if rc, out, _ := e.call("60"); rc != 0 {
		t.Fatalf("fleet rc %d", rc)
	} else {
		wantBatch(t, out, "alpha", 1000)
	}
	if rc, out := runWatch("3"); rc != 0 || out != "" {
		t.Fatalf("watch.Run redelivered the fleet's batch: rc %d out %q", rc, out)
	}

	appendRow(t, alpha, statusRow(2000, "c1", "worker:feat/a", "done"))
	if rc, out := runWatch("60"); rc != 0 || !strings.Contains(out, `"ts":2000`) {
		t.Fatalf("watch.Run rc %d out %q", rc, out)
	}
	if rc, out, _ := e.call("3"); rc != 0 || out != "" {
		t.Fatalf("fleet redelivered watch.Run's batch: rc %d out %q", rc, out)
	}
}

func TestFleetWatchReportsHeldWatchLock(t *testing.T) {
	alpha, beta := gitRepo(t, "alpha"), gitRepo(t, "beta")
	appendRow(t, alpha, statusRow(1000, "c1", "worker:feat/a", "pr_open"))
	appendRow(t, beta, statusRow(1000, "c1", "worker:feat/b", "pr_open"))
	held := filepath.Join(alpha, "crew", "crews", "c1", "watch.lock.d")
	mkdir(t, filepath.Dir(held))
	if !lock.Acquire(held, "someone-else") {
		t.Fatal("could not hold the lock")
	}
	t.Cleanup(func() { lock.Release(held) })
	e := newFleetWatchEnv(t, alpha, beta)

	rc, out, errb := e.call("60")
	if rc != 0 {
		t.Fatalf("rc %d", rc)
	}
	wantBatch(t, out, "beta", 1000)
	if strings.Contains(out, `"repo":"alpha"`) || readCursor(alpha) != "" {
		t.Errorf("alpha delivered under a held lock: %q", out)
	}
	if !strings.Contains(errb, "crew: fleet watch: alpha: another watch is already running for this crew — skipped\n") {
		t.Errorf("stderr %q", errb)
	}
	if lock.Holder(held) != "someone-else" {
		t.Errorf("lock holder now %q", lock.Holder(held))
	}
}

func TestFleetWatchOneRepoPerCall(t *testing.T) {
	alpha, beta := gitRepo(t, "alpha"), gitRepo(t, "beta")
	appendRow(t, alpha, statusRow(1000, "c1", "worker:feat/a", "pr_open"))
	appendRow(t, beta, statusRow(1000, "c1", "worker:feat/b", "pr_open"))
	e := newFleetWatchEnv(t, alpha, beta)

	_, out1, _ := e.call("60")
	_, out2, _ := e.call("60")
	first, second := "alpha", "beta"
	if alpha > beta {
		first, second = second, first
	}
	wantBatch(t, out1, first, 1000)
	wantBatch(t, out2, second, 1000)
	if strings.Contains(out1, `"repo":"`+second+`"`) || strings.Contains(out2, `"repo":"`+first+`"`) {
		t.Errorf("a call carried two repos: %q / %q", out1, out2)
	}
	if rc, out, _ := e.call("3"); rc != 0 || out != "" {
		t.Errorf("third call rc %d out %q", rc, out)
	}
}

func TestFleetWatchRotatesAfterLastDelivery(t *testing.T) {
	alpha, beta := gitRepo(t, "alpha"), gitRepo(t, "beta")
	if alpha > beta {
		alpha, beta = beta, alpha
	}
	a, b := filepath.Base(filepath.Dir(alpha)), filepath.Base(filepath.Dir(beta))
	appendRow(t, alpha, statusRow(1000, "c1", "worker:feat/a", "pr_open"))
	appendRow(t, beta, statusRow(1000, "c1", "worker:feat/b", "pr_open"))
	e := newFleetWatchEnv(t, alpha, beta)

	_, out, _ := e.call("60")
	wantBatch(t, out, a, 1000)
	appendRow(t, alpha, statusRow(2000, "c1", "worker:feat/a", "done"))
	_, out, _ = e.call("60")
	wantBatch(t, out, b, 1000)
	_, out, _ = e.call("60")
	wantBatch(t, out, a, 2000)
}

func TestFleetWatchNoCursorInRepoWithoutCrewRows(t *testing.T) {
	alpha, gamma := gitRepo(t, "alpha"), gitRepo(t, "gamma")
	appendRow(t, alpha, statusRow(1000, "c1", "worker:feat/a", "pr_open"))
	appendRow(t, gamma, statusRow(1000, "c2", "worker:feat/g", "pr_open"))
	e := newFleetWatchEnv(t, gamma, alpha)

	if rc, out, _ := e.call("60"); rc != 0 {
		t.Fatalf("rc %d", rc)
	} else {
		wantBatch(t, out, "alpha", 1000)
	}
	if _, err := os.Stat(filepath.Join(gamma, "crew", "crews")); err == nil {
		t.Error("a crew dir appeared in a repo with no rows for the crew")
	}
}

func TestFleetWatchSkipsLivePerRepoStream(t *testing.T) {
	alpha := gitRepo(t, "alpha")
	appendRow(t, alpha, statusRow(1000, "c1", "worker:feat/a", "pr_open"))
	cdir := filepath.Join(alpha, "crew", "crews", "c1")
	writeFile(t, filepath.Join(mkdir(t, filepath.Join(cdir, "stream.lock.d")), "pid"), "4242\n")
	e := newFleetWatchEnv(t, alpha)
	writeFile(t, filepath.Join(cdir, "stream.tick"), fmt.Sprintf(`{"pid":4242,"ts":%d,"park":300}`, e.ft.now().UnixMilli()))
	e.opts.Proc = aliveFunc{f: func(pid int) bool { return pid == 4242 }}

	rc, out, errb := e.call("3")
	if rc != 0 || out != "" {
		t.Fatalf("rc %d stdout %q", rc, out)
	}
	const note = "crew: fleet watch: alpha: per-repo stream alive (pid 4242) — not watching\n"
	if strings.Count(errb, note) != 1 {
		t.Errorf("stderr %q", errb)
	}
	if _, err := os.Stat(filepath.Join(cdir, "watch.lock.d")); err == nil {
		t.Error("fleet watch took the watch lock beside a live per-repo stream")
	}
	if readCursor(alpha) != "" {
		t.Errorf("cursor moved: %q", readCursor(alpha))
	}
}

func TestFleetWatchOverseerMsgWakes(t *testing.T) {
	alpha := gitRepo(t, "alpha")
	appendRow(t, alpha, statusRow(1000, "c1", "worker:feat/a", "working"))
	appendRow(t, alpha, `{"ts":2000,"crew_id":"c1","from":"overseer:mbp","to":"dispatcher:c1","kind":"msg","body":"hello"}`)
	e := newFleetWatchEnv(t, alpha)
	e.opts.Watch.Hosts = sender.NewOverseerHosts(sender.Probes{
		Self:  func() (string, error) { return "halo", nil },
		Hosts: func() (map[string]remote.Host, error) { return map[string]remote.Host{"mbp": {}}, nil },
	})

	rc, out, errb := e.call("60")
	if rc != 0 {
		t.Fatalf("rc %d stderr %q", rc, errb)
	}
	wantBatch(t, out, "alpha", 2000)
	if !strings.Contains(out, `"body":"hello"`) {
		t.Errorf("trusted overseer body altered: %q", out)
	}
}

func TestFleetWatchRefusals(t *testing.T) {
	e := newFleetWatchEnv(t)
	tests := []struct {
		name string
		req  FleetWatchRequest
		want string
	}{
		{"missing crew", FleetWatchRequest{}, "crew: fleet watch: --crew ID is required\n"},
		{"bad timeout", FleetWatchRequest{Crew: "c1", Timeout: "abc"}, "crew: --timeout must be a positive integer number of seconds\n"},
		{"zero timeout", FleetWatchRequest{Crew: "c1", Timeout: "0"}, "crew: --timeout must be > 0"},
		{"empty states", FleetWatchRequest{Crew: "c1", States: ","}, "crew: --states must be non-empty\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var out, errb lockedWriter
			rc := RunFleetWatch(tc.req, &out, &errb, e.opts)
			if rc != exitUsage || out.str() != "" || !strings.HasPrefix(errb.str(), tc.want) {
				t.Errorf("rc %d stdout %q stderr %q", rc, out.str(), errb.str())
			}
		})
	}
}
