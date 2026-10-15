package stream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/crews"
	"github.com/noamsto/dispatcher/crew/internal/repos"
	"github.com/noamsto/dispatcher/crew/internal/watch"
)

const helperEnv = "CREW_FLEET_TEST_HELPER"

// TestHelperCrewWatch is the real `crew watch` a fleet bus loop re-enters: the
// stub script execs this test binary with the watch argv after `--`, and the bus
// is resolved from the cwd exactly as the crew script resolves it.
func TestHelperCrewWatch(t *testing.T) {
	if os.Getenv(helperEnv) != "1" {
		t.Skip("helper process for the fleet tests")
	}
	args := os.Args
	for i, a := range args {
		if a == "--" {
			args = args[i+1:]
			break
		}
	}
	for _, name := range []string{"GIT_DIR", "GIT_COMMON_DIR", "GIT_WORK_TREE"} {
		if v := os.Getenv(name); v != "" {
			fmt.Fprintf(os.Stderr, "helper: %s leaked into the child (%s)\n", name, v)
			os.Exit(97)
		}
	}
	cwd, err := os.Getwd()
	if err != nil {
		os.Exit(96)
	}
	paths, err := bus.Locate(context.Background(), cwd)
	if err != nil || realPath(paths.Common) != realPath(cwd) {
		fmt.Fprintf(os.Stderr, "helper: bus %q is not the cwd %q (%v)\n", paths.Common, cwd, err)
		os.Exit(95)
	}
	if len(args) == 0 || args[0] != "watch" {
		fmt.Fprintf(os.Stderr, "helper: unexpected argv %q\n", args)
		os.Exit(94)
	}
	os.Exit(watch.Run(watchRequest(args[1:]), paths, os.Stdout, os.Stderr, watch.Options{
		Now:   time.Now,
		Sleep: func(string, io.Writer) error { time.Sleep(50 * time.Millisecond); return nil },
	}))
}

// watchRequest maps the `--flag value` argv a stream child hands `crew watch`
// onto its typed request; the grammar itself is parsed by the crew binary.
func watchRequest(args []string) watch.Request {
	var r watch.Request
	for i := 0; i+1 < len(args); i += 2 {
		v := args[i+1]
		switch args[i] {
		case "--since":
			r.Since = &v
		case "--states":
			r.States = v
		case "--timeout":
			r.Timeout = v
		case "--interval":
			r.Interval = v
		case "--crew":
			r.Crew = v
		}
	}
	return r
}

func realPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

// watchStub is the crew script a bus loop re-enters, as the helper process.
func watchStub(t *testing.T) string {
	t.Helper()
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(t.TempDir(), "crew-stub.sh")
	writeFile(t, script, bashShebang()+"export "+helperEnv+"=1\nexec '"+bin+"' -test.run='^TestHelperCrewWatch$' -- \"$@\"\n")
	chmodExecutable(t, script)
	return script
}

// bashShebang is the interpreter line for a stub the stream execs directly:
// `#!/usr/bin/env bash` does not resolve inside the nix sandbox, so point it at
// the bash this environment has — the same patch tests/helpers.bash's crew_go_bin
// applies to a built entry.
func bashShebang() string {
	if b, err := exec.LookPath("bash"); err == nil {
		return "#!" + b + "\n"
	}
	return "#!/usr/bin/env bash\n"
}

// chmodExecutable gives a stub the bit the entry it stands in for has, since the
// stream execs `self` rather than putting bash in front of it.
func chmodExecutable(t *testing.T, path string) {
	t.Helper()
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

// scriptStub is a crew script whose `watch` runs body.
func scriptStub(t *testing.T, body string) string {
	t.Helper()
	script := filepath.Join(t.TempDir(), "crew-stub.sh")
	writeFile(t, script, bashShebang()+"case \"${1:-}\" in\n  watch) "+body+" ;;\n  *) exit 0 ;;\nesac\n")
	chmodExecutable(t, script)
	return script
}

// gitRepo is a fresh `git init` with an empty crew dir; it returns the git
// common dir.
func gitRepo(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	if out, err := exec.Command("git", "init", "-q", "-b", "main", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	common := realPath(filepath.Join(dir, ".git"))
	if err := os.MkdirAll(filepath.Join(common, "crew"), 0o755); err != nil {
		t.Fatal(err)
	}
	return common
}

func appendRow(t *testing.T, common, row string) {
	t.Helper()
	if err := bus.Append(filepath.Join(common, "crew", "events.jsonl"), row); err != nil {
		t.Fatal(err)
	}
}

func statusRow(ts int64, crew, from, state string) string {
	return fmt.Sprintf(`{"ts":%d,"crew_id":%q,"from":%q,"to":"dispatcher:%s","kind":"status","body":{"state":%q}}`,
		ts, crew, from, crew, state)
}

func okRepos(commons ...string) func(io.Writer) []repos.Repo {
	return func(io.Writer) []repos.Repo {
		out := make([]repos.Repo, len(commons))
		for i, c := range commons {
			out[i] = repos.Repo{Common: c, Status: repos.StatusOK}
		}
		return out
	}
}

func origins(urls map[string]string) func(string) string {
	return func(common string) string { return urls[common] }
}

// fleetLane is one RunFleet in the background. Its coordinator ticks only when
// the test kicks it, and every tick wait it arms is counted, so the test knows
// when an iteration has finished.
type fleetLane struct {
	t          *testing.T
	root       string
	out, errb  *lockedWriter
	tick       chan time.Time
	waits      atomic.Int32
	discovered atomic.Int32
	done       chan int
	stopped    bool
	rc         int
}

func startFleet(t *testing.T, fo FleetOptions, r FleetRequest) *fleetLane {
	t.Helper()
	l := &fleetLane{t: t, root: t.TempDir(), out: &lockedWriter{}, errb: &lockedWriter{},
		tick: make(chan time.Time), done: make(chan int, 1)}
	fo.LaneRoot = l.root
	inner := fo.Discover
	if inner == nil {
		inner = func(io.Writer) []repos.Repo { return nil }
	}
	fo.Discover = func(w io.Writer) []repos.Repo {
		defer l.discovered.Add(1)
		return inner(w)
	}
	fo.Rediscover = func(time.Duration) <-chan time.Time {
		l.waits.Add(1)
		return l.tick
	}
	go func() { l.done <- RunFleet(r, l.out, l.errb, fo) }()
	waitFor(t, "the lane's first iteration", func() bool { return l.waits.Load() >= 1 })
	t.Cleanup(func() { l.stop() })
	return l
}

// kick is one coordinator tick, returning once that iteration has finished.
func (l *fleetLane) kick() {
	l.t.Helper()
	n := l.waits.Load()
	select {
	case l.tick <- time.Now():
	case <-time.After(10 * time.Second):
		l.t.Fatal("the coordinator never took the tick")
	}
	waitFor(l.t, "the kicked iteration", func() bool { return l.waits.Load() > n })
}

// kickUntil kicks until cond holds: a rediscovery is due only once
// --rediscover seconds of wall clock have passed.
func (l *fleetLane) kickUntil(what string, cond func() bool) {
	l.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			l.t.Fatalf("timed out kicking for %s", what)
		}
		l.kick()
		time.Sleep(20 * time.Millisecond)
	}
}

func (l *fleetLane) stop() int {
	l.t.Helper()
	if l.stopped {
		return l.rc
	}
	l.stopped = true
	select {
	case l.rc = <-l.done:
		l.t.Errorf("the lane exited on its own with %d; stderr %q", l.rc, l.errb.str())
		return l.rc
	default:
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		l.t.Fatal(err)
	}
	select {
	case l.rc = <-l.done:
	case <-time.After(20 * time.Second):
		l.t.Fatal("the lane did not stop on TERM")
	}
	return l.rc
}

func (l *fleetLane) lines() []map[string]any { return decodeLinesT(l.t, l.out.str()) }

func (l *fleetLane) batches() []map[string]any {
	var out []map[string]any
	for _, line := range l.lines() {
		if _, ok := line["stream"]; !ok {
			out = append(out, line)
		}
	}
	return out
}

// delivered counts each event ts across the lane's batches, per repo.
func (l *fleetLane) delivered() map[string]map[int64]int {
	out := map[string]map[int64]int{}
	for _, b := range l.batches() {
		repo, _ := b["repo"].(string)
		if out[repo] == nil {
			out[repo] = map[int64]int{}
		}
		for _, ts := range eventTS(b) {
			out[repo][ts]++
		}
	}
	return out
}

func decodeLinesT(t *testing.T, text string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, s := range strings.Split(text, "\n") {
		if s == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(s), &m); err != nil {
			t.Fatalf("line %q is not one JSON object: %v", s, err)
		}
		out = append(out, m)
	}
	return out
}

func eventTS(batch map[string]any) []int64 {
	var out []int64
	events, _ := batch["events"].([]any)
	for _, e := range events {
		if m, ok := e.(map[string]any); ok {
			if ts, ok := m["ts"].(float64); ok {
				out = append(out, int64(ts))
			}
		}
	}
	return out
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func fastRequest() FleetRequest {
	return FleetRequest{Request: Request{Crew: "c1", Park: "1", Interval: "1", Coalesce: "1", Retry: "1"}}
}

// rediscoverEachSecond is fastRequest with a rediscovery due every second.
func rediscoverEachSecond() FleetRequest {
	r := fastRequest()
	r.Rediscover = "1"
	return r
}

func noLines(t *testing.T, l *fleetLane, kinds ...string) {
	t.Helper()
	for _, line := range l.lines() {
		for _, k := range kinds {
			if line["stream"] == k {
				t.Errorf("unexpected %s line: %v", k, line)
			}
		}
	}
}

func readCursor(common string) string {
	return strings.TrimSpace(readFile(filepath.Join(common, "crew", "crews", "c1", "cursor")))
}

func TestFleetDeliversFromEitherRepoOnceTagged(t *testing.T) {
	alpha, beta, gamma := gitRepo(t, "alpha"), gitRepo(t, "beta"), gitRepo(t, "gamma")
	appendRow(t, alpha, statusRow(1000, "c1", "worker:feat/a", "working"))
	appendRow(t, beta, statusRow(1000, "c1", "worker:feat/b", "working"))
	appendRow(t, gamma, statusRow(1000, "c2", "worker:feat/g", "working"))
	// git prefers GIT_DIR to the cwd: inherited, it would point every bus loop at
	// gamma. The helper fails loudly if it sees it.
	t.Setenv("GIT_DIR", gamma)

	l := startFleet(t, FleetOptions{
		Options:  Options{Self: watchStub(t)},
		Discover: okRepos(alpha, beta, gamma),
		Origin:   origins(map[string]string{alpha: "https://github.com/acme/alpha.git"}),
	}, fastRequest())

	appendRow(t, alpha, statusRow(2000, "c1", "worker:feat/a", "pr_open"))
	appendRow(t, beta, statusRow(2001, "c1", "worker:feat/b", "pr_open"))
	waitFor(t, "a batch from each repo", func() bool {
		d := l.delivered()
		return d["acme/alpha"][2000] > 0 && d["beta"][2001] > 0
	})
	appendRow(t, alpha, statusRow(3000, "c1", "worker:feat/a", "done"))
	waitFor(t, "the second alpha batch", func() bool { return l.delivered()["acme/alpha"][3000] > 0 })

	if rc := l.stop(); rc != 0 {
		t.Errorf("exit = %d, want 0", rc)
	}
	d := l.delivered()
	want := map[string]map[int64]int{"acme/alpha": {2000: 1, 3000: 1}, "beta": {2001: 1}}
	if fmt.Sprint(d) != fmt.Sprint(want) {
		t.Errorf("delivered = %v, want %v (out %q)", d, want, l.out.str())
	}
	noLines(t, l, "hold_due", "reap", "error")
	if got := readCursor(alpha); got != "3000" {
		t.Errorf("alpha cursor = %q, want 3000", got)
	}
	if got := readCursor(beta); got != "2001" {
		t.Errorf("beta cursor = %q, want 2001", got)
	}
	if _, err := os.Stat(filepath.Join(gamma, "crew", "crews", "c1")); !os.IsNotExist(err) {
		t.Errorf("gamma has no c1 row but gained a c1 crew dir (%v)", err)
	}
	if l.errb.str() != "" {
		t.Errorf("stderr = %q, want empty", l.errb.str())
	}
}

func TestFleetJoinsRepoAtNextRediscovery(t *testing.T) {
	alpha, gamma := gitRepo(t, "alpha"), gitRepo(t, "gamma")
	appendRow(t, alpha, statusRow(1000, "c1", "worker:feat/a", "working"))
	appendRow(t, gamma, statusRow(1000, "c2", "worker:feat/g", "working"))

	l := startFleet(t, FleetOptions{
		Options:  Options{Self: watchStub(t)},
		Discover: okRepos(alpha, gamma),
		Origin:   origins(nil),
	}, rediscoverEachSecond())

	// gamma's first c1 row lands after the first discovery passed it over.
	appendRow(t, gamma, statusRow(1500, "c1", "worker:feat/g", "working"))
	appendRow(t, gamma, statusRow(2000, "c1", "worker:feat/g", "pr_open"))
	time.Sleep(1200 * time.Millisecond)
	if n := l.delivered()["gamma"][2000]; n != 0 {
		t.Fatalf("gamma delivered before its rediscovery: %q", l.out.str())
	}
	if _, err := os.Stat(filepath.Join(gamma, "crew", "crews", "c1")); !os.IsNotExist(err) {
		t.Fatalf("gamma was watched before it joined (%v)", err)
	}

	l.kickUntil("gamma's rediscovery", func() bool { return l.discovered.Load() >= 2 })
	waitFor(t, "gamma's batch after the join", func() bool { return l.delivered()["gamma"][2000] > 0 })
	l.stop()
	if n := l.delivered()["gamma"][2000]; n != 1 {
		t.Errorf("gamma's event delivered %d times, want once", n)
	}
}

func TestFleetSkipsGoneAndNoBus(t *testing.T) {
	alpha, beta, nobus := gitRepo(t, "alpha"), gitRepo(t, "beta"), gitRepo(t, "nobus")
	gone := filepath.Join(t.TempDir(), "vanished", ".git")
	appendRow(t, alpha, statusRow(1000, "c1", "worker:feat/a", "working"))
	appendRow(t, beta, statusRow(1000, "c1", "worker:feat/b", "working"))
	var betaGone atomic.Bool

	l := startFleet(t, FleetOptions{
		Options: Options{Self: watchStub(t)},
		Discover: func(w io.Writer) []repos.Repo {
			say(w, "crew: fleet stream: /nowhere is not a directory\n")
			betaStatus := repos.StatusOK
			if betaGone.Load() {
				betaStatus = repos.StatusGone
			}
			return []repos.Repo{
				{Common: alpha, Status: repos.StatusOK},
				{Common: beta, Status: betaStatus},
				{Common: gone, Status: repos.StatusGone},
				{Common: nobus, Status: repos.StatusNoBus},
			}
		},
		Origin: origins(nil),
	}, rediscoverEachSecond())

	betaScratch := busScratch(filepath.Join(l.root, "c1"), beta)
	waitFor(t, "beta's bus loop", func() bool { _, err := os.Stat(betaScratch); return err == nil })
	betaGone.Store(true)
	l.kickUntil("two rediscoveries", func() bool { return l.discovered.Load() >= 3 })
	if _, err := os.Stat(betaScratch); !os.IsNotExist(err) {
		t.Errorf("beta went gone but its bus loop's scratch remains (%v)", err)
	}

	appendRow(t, alpha, statusRow(2000, "c1", "worker:feat/a", "pr_open"))
	waitFor(t, "alpha's batch", func() bool { return l.delivered()["alpha"][2000] > 0 })
	l.stop()

	stderr := l.errb.str()
	for _, want := range []string{
		"crew: fleet stream: " + gone + ": skipped (gone)\n",
		"crew: fleet stream: " + nobus + ": skipped (no bus)\n",
		"crew: fleet stream: " + beta + ": skipped (gone)\n",
		"crew: fleet stream: /nowhere is not a directory\n",
	} {
		if n := strings.Count(stderr, want); n != 1 {
			t.Errorf("%q noted %d times, want once; stderr %q", want, n, stderr)
		}
	}
}

func TestFleetRelativeSelfStillDelivers(t *testing.T) {
	for _, tc := range []struct {
		name string
		self func(stub string) string
	}{
		{"with a slash", func(stub string) string { return "./" + filepath.Base(stub) }},
		// bash runs a bare script name from the cwd before searching PATH.
		{"bare name in the cwd", func(stub string) string { return filepath.Base(stub) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			alpha := gitRepo(t, "alpha")
			appendRow(t, alpha, statusRow(1000, "c1", "worker:feat/a", "working"))
			stub := watchStub(t)
			// The child runs in alpha, not here, so only an absolute path still finds it.
			t.Chdir(filepath.Dir(stub))

			l := startFleet(t, FleetOptions{
				Options:  Options{Self: tc.self(stub)},
				Discover: okRepos(alpha),
				Origin:   origins(nil),
			}, fastRequest())

			appendRow(t, alpha, statusRow(2000, "c1", "worker:feat/a", "pr_open"))
			waitFor(t, "alpha's batch through a relative Self", func() bool { return l.delivered()["alpha"][2000] > 0 })
			l.stop()
		})
	}
}

func TestFleetLeavesRepoDroppedFromDiscovery(t *testing.T) {
	alpha, beta := gitRepo(t, "alpha"), gitRepo(t, "beta")
	appendRow(t, alpha, statusRow(1000, "c1", "worker:feat/a", "working"))
	appendRow(t, beta, statusRow(1000, "c1", "worker:feat/b", "working"))
	var betaListed atomic.Bool
	betaListed.Store(true)

	l := startFleet(t, FleetOptions{
		Options: Options{Self: watchStub(t)},
		Discover: func(io.Writer) []repos.Repo {
			if betaListed.Load() {
				return okRepos(alpha, beta)(nil)
			}
			return okRepos(alpha)(nil)
		},
		Origin: origins(nil),
	}, rediscoverEachSecond())

	betaScratch := busScratch(filepath.Join(l.root, "c1"), beta)
	waitFor(t, "beta's bus loop", func() bool { _, err := os.Stat(betaScratch); return err == nil })
	betaListed.Store(false)
	l.kickUntil("beta's loop to stop", func() bool { _, err := os.Stat(betaScratch); return os.IsNotExist(err) })
	// beta's watch child keeps writing under beta/.git until its loop exits,
	// so the tree only goes after the leave (#1038).
	if err := os.RemoveAll(filepath.Dir(beta)); err != nil {
		t.Fatal(err)
	}

	appendRow(t, alpha, statusRow(2000, "c1", "worker:feat/a", "pr_open"))
	waitFor(t, "alpha's batch", func() bool { return l.delivered()["alpha"][2000] > 0 })
	l.stop()

	want := "crew: fleet stream: " + beta + ": skipped (no longer discovered)\n"
	if n := strings.Count(l.errb.str(), want); n != 1 {
		t.Errorf("%q noted %d times, want once; stderr %q", want, n, l.errb.str())
	}
}

func TestFleetNoDoubleDeliveryWithPerRepoStream(t *testing.T) {
	alpha := gitRepo(t, "alpha")
	appendRow(t, alpha, statusRow(1000, "c1", "worker:feat/a", "working"))
	appendRow(t, alpha, statusRow(2000, "c1", "worker:feat/a", "pr_open"))
	cdir := filepath.Join(alpha, "crew", "crews", "c1")
	writeFile(t, filepath.Join(mkdir(t, filepath.Join(cdir, "stream.lock.d")), "pid"), "4242\n")
	writeFile(t, filepath.Join(cdir, "stream.tick"), fmt.Sprintf(`{"pid":4242,"ts":%d,"park":300}`, time.Now().UnixMilli()))
	var perRepoAlive atomic.Bool
	perRepoAlive.Store(true)

	l := startFleet(t, FleetOptions{
		Options: Options{Self: watchStub(t), Probes: crews.Probes{
			Alive: func(pid int) bool { return pid == 4242 && perRepoAlive.Load() },
		}},
		Discover: okRepos(alpha),
		Origin:   origins(nil),
	}, fastRequest())

	// (a) The per-repo stream is alive: the fleet never parks on this bus.
	const aliveNote = "crew: fleet stream: alpha: per-repo stream alive (pid 4242) — not watching\n"
	waitFor(t, "the yield note", func() bool { return strings.Contains(l.errb.str(), aliveNote) })
	deadline := time.Now().Add(2500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(cdir, "watch.lock.d")); err == nil {
			t.Fatal("the fleet parked on a bus whose per-repo stream is alive")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(l.batches()) != 0 || readCursor(alpha) != "" {
		t.Fatalf("the fleet delivered while yielding: out %q cursor %q", l.out.str(), readCursor(alpha))
	}

	// (b) The per-repo stream dies: the pending event arrives, once.
	perRepoAlive.Store(false)
	waitFor(t, "the pending event", func() bool { return l.delivered()["alpha"][2000] > 0 })
	waitFor(t, "the cursor", func() bool { return readCursor(alpha) == "2000" })

	// (c) Real watch consumers, one after another in this goroutine, race the
	// fleet's child for the same bus. An in-process pair would not serialise:
	// lock.Acquire is re-entrant for one pid.
	paths := bus.Paths{Common: alpha, Dir: filepath.Join(alpha, "crew"), Log: filepath.Join(alpha, "crew", "events.jsonl")}
	const n = 10
	appended := make(chan struct{})
	go func() {
		defer close(appended)
		for i := range n {
			if err := bus.Append(paths.Log, statusRow(int64(3000+10*i), "c1", fmt.Sprintf("worker:feat/h%d", i), "pr_open")); err != nil {
				return
			}
			time.Sleep(300 * time.Millisecond)
		}
	}()
	mine := map[int64]int{}
	union := func() map[int64]int {
		u := map[int64]int{}
		for ts, c := range mine {
			u[ts] += c
		}
		for ts, c := range l.delivered()["alpha"] {
			u[ts] += c
		}
		return u
	}
	complete := func() bool {
		u := union()
		for i := range n {
			if u[int64(3000+10*i)] == 0 {
				return false
			}
		}
		return true
	}
	stop := time.Now().Add(20 * time.Second)
	for i := 0; !complete() && time.Now().Before(stop); i++ {
		var out bytes.Buffer
		rc := watch.Run(watchRequest([]string{"--crew", "c1", "--timeout", "1", "--interval", "1"}), paths, &out, io.Discard, watch.Options{
			Now:   time.Now,
			Sleep: func(string, io.Writer) error { time.Sleep(30 * time.Millisecond); return nil },
		})
		// Varied gaps between consumers, so the fleet's child wins the lock too
		// rather than only ever finding it held.
		time.Sleep(time.Duration(200+250*(i%3)) * time.Millisecond)
		if rc != 0 {
			continue
		}
		for _, b := range decodeLinesT(t, out.String()) {
			for _, ts := range eventTS(b) {
				mine[ts]++
			}
		}
	}
	<-appended
	l.stop()

	u := union()
	for i := range n {
		if ts := int64(3000 + 10*i); u[ts] != 1 {
			t.Errorf("event %d delivered %d times across both lanes, want exactly once", ts, u[ts])
		}
	}
	if u[2000] != 1 {
		t.Errorf("event 2000 delivered %d times, want once", u[2000])
	}
	if c := strings.Count(l.errb.str(), aliveNote); c != 1 {
		t.Errorf("yield note printed %d times, want once", c)
	}
	if c := strings.Count(l.errb.str(), "crew: fleet stream: alpha: per-repo stream no longer alive — resumed\n"); c != 1 {
		t.Errorf("resume note printed %d times, want once; stderr %q", c, l.errb.str())
	}
	noLines(t, l, "hold_due", "reap")
}

func TestFleetStopTagsDrainedBatch(t *testing.T) {
	alpha := gitRepo(t, "alpha")
	appendRow(t, alpha, statusRow(1000, "c1", "worker:feat/a", "working"))
	batch := `{"cursor":2000,"events":[{"ts":2000,"crew_id":"c1","from":"worker:feat/a","to":"dispatcher:c1","kind":"status","body":{"state":"pr_open"}}]}`

	l := startFleet(t, FleetOptions{
		Options:  Options{Self: scriptStub(t, "printf '%s\\n' '"+batch+"'; exec sleep 60")},
		Discover: okRepos(alpha),
		Origin:   origins(map[string]string{alpha: "git@github.com:acme/alpha.git"}),
	}, fastRequest())

	outf := filepath.Join(busScratch(filepath.Join(l.root, "c1"), alpha), "stream.out")
	waitFor(t, "the parked child's batch", func() bool { _, ok := readIfNonEmpty(outf); return ok })
	if len(l.batches()) != 0 {
		t.Fatalf("the batch was printed before the stop: %q", l.out.str())
	}
	if rc := l.stop(); rc != 0 {
		t.Errorf("exit = %d, want 0", rc)
	}
	bs := l.batches()
	if len(bs) != 1 || bs[0]["repo"] != "acme/alpha" || bs[0]["cursor"] != float64(2000) {
		t.Errorf("drained = %v, want the one batch tagged acme/alpha", bs)
	}
}

func TestFleetKeepsCwdDotfiles(t *testing.T) {
	alpha := gitRepo(t, "alpha")
	appendRow(t, alpha, statusRow(1000, "c1", "worker:feat/a", "working"))
	cwd := t.TempDir()
	keep := filepath.Join(cwd, ".keep")
	writeFile(t, keep, "x")
	t.Chdir(cwd)

	l := startFleet(t, FleetOptions{
		Options:  Options{Self: scriptStub(t, "exec sleep 60")},
		Discover: okRepos(alpha),
		Origin:   origins(nil),
	}, fastRequest())
	lane := filepath.Join(l.root, "c1")
	waitFor(t, "alpha's bus loop", func() bool { _, err := os.Stat(busScratch(lane, alpha)); return err == nil })
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("the cwd's dotfile went at start: %v", err)
	}
	if rc := l.stop(); rc != 0 {
		t.Errorf("exit = %d, want 0", rc)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("the cwd's dotfile went at shutdown: %v", err)
	}
	for _, gone := range []string{"buses", "stream.lock.d"} {
		if _, err := os.Stat(filepath.Join(lane, gone)); !os.IsNotExist(err) {
			t.Errorf("%s survived the shutdown (%v)", gone, err)
		}
	}
	if _, err := os.Stat(filepath.Join(lane, "stream.tick")); err != nil {
		t.Errorf("stream.tick was removed: %v", err)
	}
}

func TestFleetLaneHeartbeat(t *testing.T) {
	r := fastRequest()
	r.Heartbeat = "1"
	l := startFleet(t, FleetOptions{Options: Options{Self: scriptStub(t, "exit 0")}}, r)
	l.kickUntil("a lane heartbeat", func() bool {
		for _, line := range decodeLinesT(t, l.out.str()) {
			if line["stream"] == "heartbeat" {
				return true
			}
		}
		return false
	})
	l.stop()
	line := l.lines()[0]
	if _, tagged := line["repo"]; tagged || line["crew"] != "c1" {
		t.Errorf("heartbeat = %v, want crew c1 and no repo", line)
	}
	if q, ok := line["quiet_s"].(float64); !ok || q < 1 {
		t.Errorf("quiet_s = %v, want at least the --heartbeat", line["quiet_s"])
	}
}

func runFleetStatus(t *testing.T, root string, probes crews.Probes) (int, map[string]any) {
	t.Helper()
	var out, errb bytes.Buffer
	rc := RunFleet(FleetRequest{Request: Request{Status: true, Crew: "c1"}}, &out, &errb, FleetOptions{LaneRoot: root, Options: Options{Probes: probes}})
	lines := decodeLinesT(t, out.String())
	if len(lines) != 1 {
		t.Fatalf("--status printed %q, stderr %q", out.String(), errb.String())
	}
	return rc, lines[0]
}

func TestFleetStatus(t *testing.T) {
	root := t.TempDir()
	if rc, line := runFleetStatus(t, root, crews.Probes{Alive: alive()}); rc != 2 || line["state"] != "dead" || line["crew"] != "c1" {
		t.Errorf("missing lane = (%d, %v), want (2, dead)", rc, line)
	}

	lane := filepath.Join(root, "c1")
	lockd := mkdir(t, filepath.Join(lane, "stream.lock.d"))
	writeFile(t, filepath.Join(lockd, "pid"), "4242\n")
	writeFile(t, filepath.Join(lane, "stream.tick"), fmt.Sprintf(`{"pid":4242,"ts":%d,"park":300}`, time.Now().UnixMilli()))
	if rc, line := runFleetStatus(t, root, crews.Probes{Alive: alive("4242")}); rc != 0 || line["state"] != "alive" || line["pid"] != float64(4242) {
		t.Errorf("live lock + fresh tick = (%d, %v), want (0, alive)", rc, line)
	}

	// --force reclaims a stale lane: a live holder whose tick stopped.
	holder := exec.Command("sleep", "60")
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { _ = holder.Wait(); close(exited) }()
	t.Cleanup(func() { _ = holder.Process.Kill() })
	writeFile(t, filepath.Join(lockd, "pid"), fmt.Sprintf("%d\n", holder.Process.Pid))
	writeFile(t, filepath.Join(lane, "stream.tick"), fmt.Sprintf(`{"pid":%d,"ts":1,"park":300}`, holder.Process.Pid))
	if rc, line := runFleetStatus(t, root, crews.Probes{}); rc != 1 || line["state"] != "stale" {
		t.Fatalf("stale lane = (%d, %v), want (1, stale)", rc, line)
	}

	var out, errb lockedWriter
	done := make(chan int, 1)
	force := fastRequest()
	force.Force = true
	go func() {
		done <- RunFleet(force, &out, &errb, FleetOptions{LaneRoot: root, Discover: okRepos()})
	}()
	me := fmt.Sprint(os.Getpid())
	waitFor(t, "the reclaimed lane's tick", func() bool {
		return readTick(filepath.Join(lane, "stream.tick")).ok && strings.Contains(readFile(filepath.Join(lane, "stream.tick")), `"pid":`+me)
	})
	select {
	case <-exited:
	case <-time.After(10 * time.Second):
		t.Error("--force never stopped the stale holder")
	}
	if got := strings.TrimSpace(readFile(filepath.Join(lockd, "pid"))); got != me {
		t.Errorf("lock holder = %q, want this process %s", got, me)
	}
	if rc, line := runFleetStatus(t, root, crews.Probes{}); rc != 0 || line["state"] != "alive" {
		t.Errorf("reclaimed lane = (%d, %v), want (0, alive)", rc, line)
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case rc := <-done:
		if rc != 0 {
			t.Errorf("exit = %d, want 0; stderr %q", rc, errb.str())
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the reclaimed lane did not stop on TERM")
	}
	if _, err := os.Stat(lockd); !os.IsNotExist(err) {
		t.Errorf("the lane lock survived the shutdown (%v)", err)
	}
}

// The checks after the grammar, which refuses an unknown flag, an empty value
// and --reap-every (crew/main_test.go). --rediscover is judged before the crew.
func TestFleetRefusals(t *testing.T) {
	crew := func(r Request) Request { r.Crew = "c1"; return r }
	for _, tc := range []struct {
		r   FleetRequest
		msg string
	}{
		{FleetRequest{}, "crew: fleet stream: --crew ID is required"},
		{FleetRequest{Request: Request{Status: true}}, "crew: fleet stream: --crew ID is required"},
		{FleetRequest{Request: Request{Park: "5"}}, "crew: fleet stream: --crew ID is required"},
		{FleetRequest{Rediscover: "0"}, "crew: --rediscover must be a positive integer number of seconds"},
		{FleetRequest{Request: crew(Request{}), Rediscover: "0"}, "crew: --rediscover must be a positive integer number of seconds"},
		{FleetRequest{Request: crew(Request{Park: "0"})}, "crew: --park must be a positive integer number of seconds"},
		{FleetRequest{Request: Request{Crew: "../escaped"}}, "crew: invalid crew id — expected only letters, digits, '.', '_' and '-'"},
	} {
		var out, errb bytes.Buffer
		// An ambient crew is never a fallback: the lane spans repos, so the crew
		// must be named.
		rc := RunFleet(tc.r, &out, &errb, FleetOptions{
			LaneRoot: t.TempDir(),
			Options:  Options{CrewID: func() string { return "c9" }},
		})
		if rc != exitUsage {
			t.Errorf("%+v exit = %d, want %d (%s)", tc.r, rc, exitUsage, errb.String())
		}
		if got := strings.TrimSuffix(errb.String(), "\n"); got != tc.msg {
			t.Errorf("%+v stderr = %q, want %q", tc.r, got, tc.msg)
		}
		if out.Len() != 0 {
			t.Errorf("%+v printed %q on stdout", tc.r, out.String())
		}
	}
}

func mkdir(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestFleetLeavesRepoNoLongerListedOK(t *testing.T) {
	alpha, beta := gitRepo(t, "alpha"), gitRepo(t, "beta")
	appendRow(t, alpha, statusRow(1000, "c1", "worker:feat/a", "working"))
	appendRow(t, beta, statusRow(1000, "c1", "worker:feat/b", "working"))
	var betaStatus atomic.Value
	betaStatus.Store(repos.StatusOK)

	l := startFleet(t, FleetOptions{
		Options: Options{Self: watchStub(t)},
		Discover: func(io.Writer) []repos.Repo {
			return []repos.Repo{
				{Common: alpha, Status: repos.StatusOK},
				{Common: beta, Status: betaStatus.Load().(repos.Status)},
			}
		},
		Origin: origins(nil),
	}, rediscoverEachSecond())

	betaScratch := busScratch(filepath.Join(l.root, "c1"), beta)
	waitFor(t, "beta's bus loop", func() bool { _, err := os.Stat(betaScratch); return err == nil })
	appendRow(t, beta, statusRow(2000, "c1", "worker:feat/b", "pr_open"))
	waitFor(t, "beta's first batch", func() bool { return l.delivered()["beta"][2000] > 0 })

	betaStatus.Store(repos.StatusNoBus)
	l.kickUntil("beta's loop to stop", func() bool { _, err := os.Stat(betaScratch); return os.IsNotExist(err) })

	appendRow(t, beta, statusRow(3000, "c1", "worker:feat/b", "done"))
	betaStatus.Store(repos.StatusOK)
	l.kickUntil("beta's new batch", func() bool { return l.delivered()["beta"][3000] > 0 })
	l.stop()

	for ts, n := range l.delivered()["beta"] {
		if n != 1 {
			t.Errorf("beta's event %d delivered %d times, want once", ts, n)
		}
	}
	want := "crew: fleet stream: " + beta + ": skipped (no bus)\n"
	if n := strings.Count(l.errb.str(), want); n != 1 {
		t.Errorf("%q noted %d times, want once; stderr %q", want, n, l.errb.str())
	}
}
