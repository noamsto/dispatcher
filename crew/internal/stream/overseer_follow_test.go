package stream

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/proc"
	"github.com/noamsto/dispatcher/crew/internal/repos"
)

// followRig is one host's overseer lane root shared by follows and the daemon
// they spawn. Spawn is recorded; by default it only rewrites daemon.json as a
// restarted daemon would, since in-process two daemons would share one lock.
type followRig struct {
	*ovRig
	root     string
	lane     string
	discover func(io.Writer) []repos.Repo
	proc     proc.Table

	mu     sync.Mutex
	spawns [][]string
	spawn  func(args []string) error

	daemon *followDaemon
}

func newFollowRig(t *testing.T) *followRig {
	root := t.TempDir()
	f := &followRig{ovRig: newOvRig(t), root: root, lane: filepath.Join(root, ovMe), discover: okRepos()}
	f.spawn = func([]string) error {
		f.fakeDaemon(f.clock.Now().UnixMilli()+int64(f.spawnCount()), 0)
		return nil
	}
	return f
}

func (f *followRig) spawnCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.spawns)
}

func (f *followRig) spawnArgs(i int) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.spawns[i]
}

func (f *followRig) options() FleetOptions {
	o := f.ovRig.options(f.discover)
	o.LaneRoot = f.root
	o.Proc = f.proc
	o.Spawn = func(args []string, stderrPath string) error {
		if stderrPath != filepath.Join(f.lane, "daemon.err") {
			f.t.Errorf("stderr path %q", stderrPath)
		}
		f.mu.Lock()
		f.spawns = append(f.spawns, args)
		f.mu.Unlock()
		return f.spawn(args)
	}
	return o
}

// fakeDaemon makes the lane read as a live daemon of this process.
func (f *followRig) fakeDaemon(startedMS, startOffset int64) {
	me := os.Getpid()
	lockd := filepath.Join(f.lane, "stream.lock.d")
	if err := os.MkdirAll(lockd, 0o755); err != nil {
		f.t.Fatal(err)
	}
	writeFile(f.t, filepath.Join(lockd, "pid"), fmt.Sprintf("%d\n", me))
	writeFile(f.t, filepath.Join(f.lane, "stream.tick"), fmt.Sprintf(`{"pid":%d,"ts":%d,"park":2}`, me, f.clock.Now().UnixMilli()))
	if err := writeDaemonInfo(filepath.Join(f.lane, daemonInfoName), daemonInfo{PID: int64(me), StartedMS: startedMS, StartOffset: startOffset}); err != nil {
		f.t.Fatal(err)
	}
}

// followDaemon is a real `--daemon` RunFleet in the background.
type followDaemon struct {
	tick  chan time.Time
	waits atomic.Int32
	done  chan int
}

// realDaemon makes Spawn start an in-process `--daemon` lane.
func (f *followRig) realDaemon() {
	f.spawn = func(args []string) error {
		d := &followDaemon{tick: make(chan time.Time), done: make(chan int, 1)}
		f.daemon = d
		o := f.options()
		o.Rediscover = func(time.Duration) <-chan time.Time {
			d.waits.Add(1)
			return d.tick
		}
		var out, errb lockedWriter
		go func() { d.done <- RunFleet(FleetRequest{Overseer: true, Daemon: true}, &out, &errb, o) }()
		waitFor(f.t, "the daemon's first iteration", func() bool { return d.waits.Load() >= 1 })
		f.t.Cleanup(func() {
			if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
				f.t.Fatal(err)
			}
			select {
			case <-d.done:
			case <-time.After(20 * time.Second):
				f.t.Fatal("the daemon did not stop on TERM")
			}
		})
		return nil
	}
}

func (f *followRig) kickDaemonUntil(what string, cond func() bool) {
	f.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			f.t.Fatalf("timed out kicking the daemon for %s", what)
		}
		n := f.daemon.waits.Load()
		f.daemon.tick <- time.Now()
		waitFor(f.t, "the daemon iteration", func() bool { return f.daemon.waits.Load() > n })
	}
}

func (f *followRig) logText() string {
	chunk, _, _, _, _ := readLog(filepath.Join(f.lane, logDirName), 0)
	return string(chunk)
}

// followRun is one `--follow` in the background; its polls are the test's.
type followRun struct {
	f     *followRig
	park  int64
	out   *lockedWriter
	errb  *lockedWriter
	tick  chan time.Time
	waits atomic.Int32
	done  chan int
}

func (f *followRig) follow(req Request) *followRun {
	f.t.Helper()
	if req.Park == "" {
		req.Park = "5"
	}
	r := &followRun{f: f, out: &lockedWriter{}, errb: &lockedWriter{}, tick: make(chan time.Time), done: make(chan int, 1)}
	r.park, _ = positiveInt(req.Park)
	o := f.options()
	o.Rediscover = func(time.Duration) <-chan time.Time {
		r.waits.Add(1)
		return r.tick
	}
	go func() { r.done <- RunFleet(FleetRequest{Overseer: true, Follow: true, Request: req}, r.out, r.errb, o) }()
	return r
}

// started waits for the follow's first poll wait, by which its marker and
// first read are on stdout.
func (r *followRun) started() {
	r.f.t.Helper()
	waitFor(r.f.t, "the follow's first poll", func() bool { return r.waits.Load() >= 1 || len(r.done) > 0 })
	if len(r.done) > 0 {
		r.f.t.Fatalf("the follow exited early: rc %d stderr %q", <-r.done, r.errb.str())
	}
}

func (r *followRun) kick() {
	r.f.t.Helper()
	n := r.waits.Load()
	select {
	case r.tick <- time.Now():
	case <-time.After(10 * time.Second):
		r.f.t.Fatal("the follow never took the tick")
	}
	waitFor(r.f.t, "the kicked follow poll", func() bool { return r.waits.Load() > n || len(r.done) > 0 })
}

// finish lets the --park deadline pass and expects a clean exit.
func (r *followRun) finish() {
	r.f.t.Helper()
	r.f.clock.advance(time.Duration(r.park+1) * time.Second)
	r.kick()
	select {
	case rc := <-r.done:
		if rc != 0 {
			r.f.t.Fatalf("follow rc %d stderr %q", rc, r.errb.str())
		}
	case <-time.After(10 * time.Second):
		r.f.t.Fatal("the follow did not exit at its --park deadline")
	}
}

func (r *followRun) lines() []map[string]any { return decodeLinesT(r.f.t, r.out.str()) }

func (r *followRun) markers() []map[string]any {
	var out []map[string]any
	for _, l := range r.lines() {
		if l["stream"] == "follow" {
			out = append(out, l)
		}
	}
	return out
}

func wantMarker(t *testing.T, m map[string]any, lane, reason string) {
	t.Helper()
	if m["stream"] != "follow" || m["lane"] != lane || m["reason"] != reason {
		t.Fatalf("marker %v, want %s/%s", m, lane, reason)
	}
	if m["pid"] != float64(os.Getpid()) {
		t.Errorf("marker pid %v", m["pid"])
	}
}

func TestOverseerFollowStartsTheDaemonThenResumes(t *testing.T) {
	alpha := gitRepo(t, "alpha")
	appendRow(t, alpha, ovMsg(ovStart+1000, "c1", "dispatcher:c1", ovMe, "alpha ack"))
	f := newFollowRig(t)
	f.discover = okRepos(alpha)
	f.realDaemon()

	r := f.follow(Request{})
	r.started()
	waitFor(t, "the first batch", func() bool { return strings.Contains(r.out.str(), "alpha ack") })
	lines := r.lines()
	wantMarker(t, lines[0], "fresh", "started")
	if len(lines) != 2 || !strings.Contains(r.out.str(), `"alpha ack"`) {
		t.Fatalf("lines %v", lines)
	}
	if f.spawnCount() != 1 || !slices.Equal(f.spawnArgs(0), []string{"fleet", "stream", "--overseer", "--daemon"}) {
		t.Fatalf("spawns %v", f.spawns)
	}
	r.finish()
	if _, err := os.Stat(filepath.Join(f.lane, "follow.lock.d")); err == nil {
		t.Error("follow.lock.d was not released")
	}

	appendRow(t, alpha, ovMsg(ovStart+2000, "c1", "dispatcher:c1", ovMe, "beta ack"))
	f.kickDaemonUntil("the second batch", func() bool { return strings.Contains(f.logText(), "beta ack") })

	r2 := f.follow(Request{})
	r2.started()
	waitFor(t, "the second batch", func() bool { return strings.Contains(r2.out.str(), "beta ack") })
	lines = r2.lines()
	wantMarker(t, lines[0], "resume", "resume")
	if len(lines) != 2 || strings.Contains(r2.out.str(), "alpha ack") {
		t.Fatalf("resume lines %v", lines)
	}
	if f.spawnCount() != 1 {
		t.Errorf("spawned again: %v", f.spawns)
	}
	r2.finish()
}

func TestOverseerFollowRotation(t *testing.T) {
	f := newFollowRig(t)
	f.fakeDaemon(ovStart, 0)
	dir := filepath.Join(f.lane, logDirName)
	log, err := openSegLog(dir, 200, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = log.Close() }()
	n := 0
	emit := func(count int) string {
		var b strings.Builder
		for range count {
			line := fmt.Sprintf(`{"n":"%03d","pad":"xxxxxxxxxxxxxxxxxxxx"}`+"\n", n)
			n++
			b.WriteString(line)
			if _, err := log.Write([]byte(line)); err != nil {
				t.Fatal(err)
			}
		}
		return b.String()
	}
	body := func(r *followRun) string {
		_, rest, _ := strings.Cut(r.out.str(), "\n")
		return rest
	}

	first := emit(3)
	r := f.follow(Request{})
	r.started()
	wantMarker(t, r.markers()[0], "fresh", "first")
	if got := body(r); got != first {
		t.Fatalf("first follow body %q, want %q", got, first)
	}
	r.finish()

	second := emit(10)
	if segs, _ := listSegments(dir); len(segs) < 3 {
		t.Fatalf("only %d segments: the log did not rotate", len(segs))
	}
	r = f.follow(Request{})
	r.started()
	wantMarker(t, r.markers()[0], "resume", "resume")
	if got := body(r); got != second {
		t.Fatalf("second follow body %q, want %q", got, second)
	}
	r.finish()

	emit(30)
	segs, _ := listSegments(dir)
	oldest := segs[0].start
	want, _, _, _, _ := readLog(dir, oldest)
	r = f.follow(Request{})
	r.started()
	m := r.markers()[0]
	wantMarker(t, m, "fresh", "gap")
	if m["offset"] != float64(oldest) {
		t.Errorf("gap offset %v, want %d", m["offset"], oldest)
	}
	if got := body(r); got != string(want) {
		t.Fatalf("gap follow body %q, want %q", got, want)
	}
	r.finish()
}

func TestOverseerFollowDaemonRestarted(t *testing.T) {
	f := newFollowRig(t)
	f.fakeDaemon(ovStart, 0)
	r := f.follow(Request{})
	r.started()
	r.finish()
	f.fakeDaemon(ovStart+5, 0)
	r = f.follow(Request{})
	r.started()
	wantMarker(t, r.markers()[0], "fresh", "restarted")
	r.finish()
	if f.spawnCount() != 0 {
		t.Errorf("spawns %v", f.spawns)
	}
}

func TestOverseerFollowForce(t *testing.T) {
	f := newFollowRig(t)
	f.fakeDaemon(ovStart, 0)
	r := f.follow(Request{Force: true, Interval: "7"})
	r.started()
	wantMarker(t, r.markers()[0], "fresh", "started")
	r.finish()
	want := []string{"fleet", "stream", "--overseer", "--daemon", "--force", "--interval", "7"}
	if f.spawnCount() != 1 || !slices.Equal(f.spawnArgs(0), want) {
		t.Fatalf("spawns %v, want %v", f.spawns, want)
	}
}

func TestOverseerFollowTakesOver(t *testing.T) {
	f := newFollowRig(t)
	f.fakeDaemon(ovStart, 0)
	lockd := mkdir(t, filepath.Join(f.lane, "follow.lock.d"))
	holder := exec.Command("sleep", "60")
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { _ = holder.Wait(); close(exited) }()
	t.Cleanup(func() { _ = holder.Process.Kill() })
	writeFile(t, filepath.Join(lockd, "pid"), fmt.Sprintf("%d\n", holder.Process.Pid))

	r := f.follow(Request{})
	r.started()
	select {
	case <-exited:
	default:
		t.Fatal("the old follower was not TERMed")
	}
	wantMarker(t, r.markers()[0], "fresh", "first")
	r.finish()
}

// deadProc reports this process dead while dead is set.
type deadProc struct {
	proc.OS
	dead atomic.Bool
}

func (p *deadProc) Alive(pid int) bool {
	if pid == os.Getpid() && p.dead.Load() {
		return false
	}
	return p.OS.Alive(pid)
}

func TestOverseerFollowDaemonDiesMidFollow(t *testing.T) {
	f := newFollowRig(t)
	p := &deadProc{}
	f.proc = p
	f.fakeDaemon(ovStart, 0)
	inner := f.spawn
	f.spawn = func(args []string) error {
		p.dead.Store(false)
		return inner(args)
	}
	r := f.follow(Request{})
	r.started()
	p.dead.Store(true)
	r.kick()
	waitFor(t, "the restart marker", func() bool { return len(r.markers()) == 2 })
	wantMarker(t, r.markers()[1], "fresh", "started")
	if f.spawnCount() != 1 || slices.Contains(f.spawnArgs(0), "--force") {
		t.Fatalf("spawns %v", f.spawns)
	}
	r.finish()
}

func TestOverseerFollowRefusesANonDaemonHolder(t *testing.T) {
	f := newFollowRig(t)
	f.fakeDaemon(ovStart, 0)
	if err := writeDaemonInfo(filepath.Join(f.lane, daemonInfoName), daemonInfo{PID: int64(os.Getpid()) + 1, StartedMS: ovStart}); err != nil {
		t.Fatal(err)
	}
	r := f.follow(Request{})
	select {
	case rc := <-r.done:
		if rc != 1 {
			t.Fatalf("rc %d, want 1", rc)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the follow did not refuse")
	}
	want := fmt.Sprintf("crew: fleet stream: the overseer lane is held by a non-daemon lane (pid %d); use --follow --force to replace it\n", os.Getpid())
	if got := r.errb.str(); got != want {
		t.Errorf("stderr %q, want %q", got, want)
	}
	if f.spawnCount() != 0 || r.out.str() != "" {
		t.Errorf("spawns %v stdout %q", f.spawns, r.out.str())
	}
}

func TestOverseerFollowTakenOverDecision(t *testing.T) {
	lane := t.TempDir()
	if takenOver(lane, 42) {
		t.Fatal("no marker read as taken over")
	}
	writeFile(t, filepath.Join(lane, takeoverName), "42")
	if !takenOver(lane, 42) || takenOver(lane, 43) {
		t.Fatal("the marker must name exactly its pid")
	}
}

func TestOverseerFollowTakesOverWritesTheMarker(t *testing.T) {
	f := newFollowRig(t)
	f.fakeDaemon(ovStart, 0)
	lockd := mkdir(t, filepath.Join(f.lane, "follow.lock.d"))
	holder := exec.Command("sleep", "60")
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = holder.Wait() }()
	t.Cleanup(func() { _ = holder.Process.Kill() })
	writeFile(t, filepath.Join(lockd, "pid"), fmt.Sprintf("%d\n", holder.Process.Pid))
	var named atomic.Bool
	go func() {
		for range 2000 {
			if takenOver(f.lane, holder.Process.Pid) {
				named.Store(true)
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()

	r := f.follow(Request{})
	r.started()
	if !named.Load() {
		t.Error("the holder was TERMed without a takeover marker naming it")
	}
	if _, err := os.Stat(filepath.Join(f.lane, takeoverName)); err == nil {
		t.Error("the new follow left follow.takeover behind")
	}
	r.finish()
}

func TestOverseerFollowKeepsAMarkerNamingItself(t *testing.T) {
	for _, tc := range []struct {
		name string
		pid  int
		kept bool
	}{
		{"self", os.Getpid(), true},
		{"other", os.Getpid() + 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lane := t.TempDir()
			marker := filepath.Join(lane, takeoverName)
			writeFile(t, marker, fmt.Sprint(tc.pid))
			if !takeFollow(filepath.Join(lane, "follow.lock.d"), Options{}, io.Discard) {
				t.Fatal("takeFollow failed on a free lock")
			}
			_, err := os.Stat(marker)
			if kept := err == nil; kept != tc.kept {
				t.Errorf("marker kept %v, want %v", kept, tc.kept)
			}
		})
	}
}

// term TERMs this process, which only the running follow has subscribed to.
func (r *followRun) term() int {
	r.f.t.Helper()
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		r.f.t.Fatal(err)
	}
	select {
	case rc := <-r.done:
		return rc
	case <-time.After(10 * time.Second):
		r.f.t.Fatal("the follow did not stop on TERM")
		return -1
	}
}

func TestOverseerFollowTakenOverPrintsEnded(t *testing.T) {
	f := newFollowRig(t)
	f.fakeDaemon(ovStart, 0)
	r := f.follow(Request{})
	r.started()
	writeFile(t, filepath.Join(f.lane, takeoverName), fmt.Sprint(os.Getpid()))
	if rc := r.term(); rc != 0 {
		t.Fatalf("rc %d", rc)
	}
	m := r.markers()
	if len(m) != 2 {
		t.Fatalf("markers %v", m)
	}
	wantMarker(t, m[1], "ended", "taken_over")
	if m[1]["offset"] != m[0]["offset"] {
		t.Errorf("ended offset %v, want %v", m[1]["offset"], m[0]["offset"])
	}
}

func TestOverseerFollowTERMWithoutTakeoverIsSilent(t *testing.T) {
	f := newFollowRig(t)
	f.fakeDaemon(ovStart, 0)
	writeFile(t, filepath.Join(f.lane, takeoverName), fmt.Sprint(os.Getpid()+1))
	r := f.follow(Request{})
	r.started()
	if rc := r.term(); rc != 0 {
		t.Fatalf("rc %d", rc)
	}
	if m := r.markers(); len(m) != 1 {
		t.Fatalf("markers %v", m)
	}
}

func TestOverseerFollowSignalDuringDaemonStart(t *testing.T) {
	f := newFollowRig(t)
	f.spawn = func([]string) error { return nil }
	r := f.follow(Request{})
	waitFor(t, "the spawn", func() bool { return f.spawnCount() == 1 })
	start := time.Now()
	if rc := r.term(); rc != 0 {
		t.Fatalf("rc %d stderr %q", rc, r.errb.str())
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("the start wait ignored the signal for %v", time.Since(start))
	}
	if r.out.str() != "" {
		t.Errorf("stdout %q", r.out.str())
	}
	if _, err := os.Stat(filepath.Join(f.lane, "follow.lock.d")); err == nil {
		t.Error("follow.lock.d was not released")
	}
}

func TestOverseerFollowForcesAStaleDaemon(t *testing.T) {
	f := newFollowRig(t)
	f.fakeDaemon(ovStart, 0)
	r := f.follow(Request{})
	r.started()
	staleTick := func() {
		writeFile(t, filepath.Join(f.lane, "stream.tick"), fmt.Sprintf(`{"pid":%d,"ts":%d,"park":2}`, os.Getpid(), f.clock.Now().UnixMilli()-100_000))
	}
	staleTick()
	r.kick()
	if f.spawnCount() != 0 || len(r.markers()) != 1 {
		t.Fatalf("forced on the first stale poll: spawns %v", f.spawns)
	}
	staleTick()
	r.kick()
	if f.spawnCount() != 1 || !slices.Contains(f.spawnArgs(0), "--force") {
		t.Fatalf("spawns %v", f.spawns)
	}
	m := r.markers()
	if len(m) != 2 {
		t.Fatalf("markers %v", m)
	}
	wantMarker(t, m[1], "fresh", "started")
	r.finish()
}

func TestOverseerFollowGapMidFollow(t *testing.T) {
	f := newFollowRig(t)
	f.fakeDaemon(ovStart, 0)
	dir := filepath.Join(f.lane, logDirName)
	log, err := openSegLog(dir, 100, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = log.Close() }()
	emit := func(from, count int) {
		for i := from; i < from+count; i++ {
			if _, err := fmt.Fprintf(log, `{"n":"%03d","pad":"xxxxxxxxxxxxxxxxxxxx"}`+"\n", i); err != nil {
				t.Fatal(err)
			}
		}
	}
	emit(0, 2)
	r := f.follow(Request{})
	r.started()
	before := r.out.str()
	emit(2, 20)
	segs, _ := listSegments(dir)
	oldest := segs[0].start
	m0 := r.markers()[0]
	if float64(oldest) <= m0["offset"].(float64)+float64(len(before)) {
		t.Fatalf("the log did not prune past the follower: oldest %d", oldest)
	}
	want, _, _, _, _ := readLog(dir, oldest)
	r.kick()
	m := r.markers()
	if len(m) != 2 {
		t.Fatalf("markers %v", m)
	}
	wantMarker(t, m[1], "fresh", "gap")
	if m[1]["offset"] != float64(oldest) {
		t.Errorf("gap offset %v, want %d", m[1]["offset"], oldest)
	}
	_, rest, _ := strings.Cut(strings.TrimPrefix(r.out.str(), before), "\n")
	if rest != string(want) {
		t.Fatalf("after the gap %q, want %q", rest, want)
	}
	after := r.out.str()
	r.kick()
	if r.out.str() != after {
		t.Fatalf("lines repeated: %q", strings.TrimPrefix(r.out.str(), after))
	}
	r.finish()
}

func TestOverseerFollowReadErrorLine(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	f := newFollowRig(t)
	f.fakeDaemon(ovStart, 0)
	dir := mkdir(t, filepath.Join(f.lane, logDirName))
	r := f.follow(Request{})
	r.started()
	if err := os.Chmod(dir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	r.kick()
	r.kick()
	var errs []map[string]any
	for _, l := range r.lines() {
		if l["stream"] == "error" {
			errs = append(errs, l)
		}
	}
	if len(errs) != 1 {
		t.Fatalf("error lines %v, want one per heartbeat", errs)
	}
	if errs[0]["crew"] != "overseer:"+ovHost || !strings.Contains(errs[0]["detail"].(string), "permission denied") {
		t.Errorf("error line %v", errs[0])
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	r.finish()
}
