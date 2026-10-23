// The harness the ported `stream:` cases run on: the crew entry the loop
// re-enters, one running lane in the background, and the two ways a test counts
// the loop's iterations — a `watch` child that parks until the test steps it,
// and the tick the loop rewrites at the top of every cycle.
//
// The seams are the loop's own: `Options.Self` for the entry, `Options.Timer`
// and `Options.Clock` for its waits, and the real `crew watch` behind a helper
// process for the cases about cursors and locks.
package stream

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/clock"
	"github.com/noamsto/dispatcher/crew/internal/proc"
	"github.com/noamsto/dispatcher/crew/internal/watch"
)

// stub is one crew entry the stream re-enters, with the dir its bodies share.
type stub struct {
	t      *testing.T
	Script string
	Dir    string
}

// entryStub writes the entry a stream re-enters, one bash body per subcommand.
// Each body sees $d, the stub's dir, and every invocation is logged to
// $d/argv.log, which is how a case asserts what the inner `crew watch` was
// re-entered with. A subcommand with no body answers the way the real one does
// with nothing to report: `watch` exits 0 printing nothing, `hold due` prints
// `[]` and exits 1, `reap` says nothing.
func entryStub(t *testing.T, bodies map[string]string) *stub {
	t.Helper()
	dir := t.TempDir()
	settleOnce(t, filepath.Dir(dir))
	of := func(name, none string) string {
		if body, ok := bodies[name]; ok {
			return body
		}
		return none
	}
	script := filepath.Join(t.TempDir(), "crew-stub.sh")
	writeFile(t, script, bashShebang()+fmt.Sprintf(`d='%s'
printf '%%s\n' "$*" >> "$d/argv.log"
case "${1:-}" in
  watch) %s ;;
  hold) %s ;;
  reap) %s ;;
  *) exit 0 ;;
esac
`, dir,
		of("watch", "exit 0"),
		// `hold due` only: the stream's hold pre-check is the sole `hold` call.
		`if [ "${2:-}" = due ]; then `+of("hold due", "printf '[]\\n'; exit 1")+`
  fi
  exit 0`,
		of("reap", "exit 0")))
	chmodExecutable(t, script)
	return &stub{t: t, Script: script, Dir: dir}
}

// The sweep every stub registers for its test: stop and wait for every process a stub
// started, so none of them is writing into the temp dirs while `t.TempDir` removes
// them. Two classes reach past the lane. The parked `watch` body renames its counter in
// with a forked `mv` (parkedPrologue), and the loop waits only for the stub itself
// (`child.stop`), so the rename can still be in flight when `Run` returns. And a
// `crew reap` child is never awaited and never signalled (see children.go): it outlives
// its lane by design, and its first act is appending to the argv log in the stub's dir.
const (
	settleGrace = 5 * time.Second  // a forked command gets this long to finish on its own
	settleKill  = 10 * time.Second // then it is stopped with force
	settleBound = 20 * time.Second // past this, the sweep reports what it could not stop
)

// stubProc is one process of a test's own that is still running.
type stubProc struct {
	pid  int
	argv string
	body bool // argv names an executable regular file under the temp root: a stub body
}

// settleOnce registers the sweep once per test, from the first stub of that temp root.
// testing registers the one RemoveAll of the whole temp root on the test's first
// t.TempDir (fixture's) and runs cleanups last-in-first-out, so a sweep registered
// here — after that, and before any lane registers its stop, since a lane is always
// built on a stub — runs after every lane has stopped and before the dirs go away.
// Per stub, the second stub's sweep would land before the first lane's stop.
var settleRoots sync.Map

func settleOnce(t *testing.T, root string) {
	t.Helper()
	if _, loaded := settleRoots.LoadOrStore(root, struct{}{}); loaded {
		return
	}
	t.Cleanup(func() {
		defer settleRoots.Delete(root)
		settleStubs(t, root)
	})
}

// settleStubs stops every process still running that belongs to root and waits for it:
// a stub body with SIGTERM, since it may be parked on a step gate nobody will raise
// again, and a command a body forked by waiting, since a rename in flight has to
// finish. It returns once nothing is left; a process past the bound is reported.
func settleStubs(t *testing.T, root string) {
	t.Helper()
	started := time.Now()
	forked := map[int]time.Time{}
	quiet := 0
	for {
		// A few clean scans, so a child forked but not yet exec'd — invisible to argv
		// matching for microseconds — cannot slip past the last one.
		live := liveStubs(t, root)
		if len(live) == 0 {
			if quiet++; quiet >= 3 {
				return
			}
		} else {
			quiet = 0
			for _, p := range live {
				if p.body {
					_ = syscall.Kill(p.pid, syscall.SIGTERM)
					continue
				}
				// A forked command is waited for, each on a clock of its own.
				first, seen := forked[p.pid]
				switch {
				case !seen:
					forked[p.pid] = time.Now()
				case time.Since(started) > settleKill:
					_ = syscall.Kill(p.pid, syscall.SIGKILL)
				case time.Since(first) > settleGrace:
					_ = syscall.Kill(p.pid, syscall.SIGTERM)
				}
			}
		}
		if time.Since(started) > settleBound {
			var who []string
			for _, p := range liveStubs(t, root) {
				kind := "forked"
				if p.body {
					kind = "stub"
				}
				who = append(who, fmt.Sprintf("\tpid %d (%s) %s", p.pid, kind, p.argv))
			}
			t.Fatalf("stubs of %s were still running %s after the lane stopped:\n%s",
				root, settleBound, strings.Join(who, "\n"))
		}
		time.Sleep(time.Millisecond)
	}
}

// liveStubs is every process still running whose command line names a path under root:
// the stub bodies, and the commands they forked. Containment rather than whole-token
// matching, because a `bash -c` writer carries its paths inside one script element.
// A writer that names no path under root — a relative path after a cd, a path compiled
// in — is outside what the sweep can see; no body in this package writes that way.
//
// `ps` rather than /proc because the suite runs on darwin too, which has no /proc. A
// failed `ps` is fatal: an empty table would settle nothing and read as clean.
func liveStubs(t *testing.T, root string) []stubProc {
	t.Helper()
	out, err := exec.Command("ps", "-A", "-ww", "-o", "pid=,args=").Output()
	if err != nil {
		t.Fatalf("cannot see the stubs of %s: %v", root, err)
	}
	var live []stubProc
	for _, line := range strings.Split(string(out), "\n") {
		pidStr, args, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		pid, err := strconv.Atoi(pidStr)
		if err != nil || !strings.Contains(args, root+string(filepath.Separator)) {
			continue
		}
		argv := strings.Fields(args)
		live = append(live, stubProc{pid: pid, argv: strings.Join(argv, " "), body: isStubBody(argv, root)})
	}
	return live
}

// isStubBody reports whether argv names an executable regular file under root — the
// stub script, which is what a running body is made of. Not argv[0]: the stubs are
// shebang scripts, so the kernel runs `<bash> <script> <args…>` and the script is the
// second element. A directory carries its x bit too and is no executable, so the state
// dir and the bus dir never mark a body, and neither does a path quoted inside a
// `bash -c` script.
func isStubBody(argv []string, root string) bool {
	for _, tok := range argv {
		if !strings.HasPrefix(tok, root+string(filepath.Separator)) {
			continue
		}
		fi, err := os.Stat(tok)
		if err != nil {
			continue
		}
		if m := fi.Mode(); m.IsRegular() && m&0o111 != 0 {
			return true
		}
	}
	return false
}

// path is a file in the stub's dir.
func (s *stub) path(name string) string { return filepath.Join(s.Dir, name) }

func (s *stub) write(name, text string) {
	s.t.Helper()
	writeFile(s.t, s.path(name), text)
}

// has is the stub's own file there — a marker a child leaves behind when it is
// done, which is how a case proves a late write happened at all.
func (s *stub) has(name string) bool {
	_, err := os.Stat(s.path(name))
	return err == nil
}

func (s *stub) read(name string) string {
	s.t.Helper()
	return readFile(s.path(name))
}

// argv is every invocation the stub saw, in order.
func (s *stub) argv() []string {
	text := strings.TrimSpace(readFile(s.path("argv.log")))
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

// calls is how many times the stub saw one subcommand.
func (s *stub) calls(sub string) int {
	n := 0
	for _, a := range s.argv() {
		if a == sub || strings.HasPrefix(a, sub+" ") {
			n++
		}
	}
	return n
}

// steps raises the gate parkedWatch waits on to n, releasing that many parks.
func (s *stub) steps(n int) { s.write("steps", strconv.Itoa(n)+"\n") }

// park releases the lane's current park and waits until the loop has begun the
// next iteration: its tick rewritten at the top, and the following park's child
// started. That is the loop's whole cycle, so a case that calls park n times
// knows exactly n parks have completed.
func (l *lane) park() {
	l.t.Helper()
	l.stepN++
	tick := tickTS(l.paths)
	l.stub.steps(l.stepN)
	waitFor(l.t, fmt.Sprintf("the iteration after park %d", l.stepN), func() bool {
		return tickTS(l.paths) > tick && l.stub.calls("watch") >= l.stepN+1
	})
}

// parkedPrologue is the call counter and the step gate every parked `watch`
// body opens with. Both state files are rewritten while sibling stubs poll
// them, so every write lands through a rename — and the gate keeps sleeping
// on a read that is not a number, where `[ -lt ]` would error out the loop
// and release the park early.
func parkedPrologue() string {
	return `n=$(( $(cat "$d/calls" 2>/dev/null || echo 0) + 1 ))
  printf '%s\n' "$n" > "$d/calls.tmp.$$" && mv -f "$d/calls.tmp.$$" "$d/calls"
  while :; do s=$(cat "$d/steps" 2>/dev/null || echo 0)
    case "$s" in ''|*[!0-9]*) s=0 ;; esac
    [ "$s" -ge "$n" ] && break
    sleep 0.02
  done
  `
}

// parkedWatch is a `watch` body that records its call number and parks until
// the test raises the step gate to it, then exits rc. Every iteration of the
// loop is then one step the test took, so "one line per three parks" is a count.
func parkedWatch(rc string) string { return parkedPrologue() + "exit " + rc }

// parkedWatchFailing is a parked `watch` whose failure is the given body, run
// once the test releases the park; the body is expected to exit.
func parkedWatchFailing(body string) string { return parkedPrologue() + body + "\n  exit 1" }

// printsThenPark is a `watch` body that prints one line and parks, standing in
// for a park that expired with a batch on it.
func printsThenPark(line string) string {
	return parkedPrologue() + "printf '%s\\n' '" + line + "'\n  exit 0"
}

// printsOnceThenPark prints its line on the first call only: the batch a lane
// delivers once, then nothing but parks.
func printsOnceThenPark(line string) string {
	return parkedPrologue() + `if [ "$n" = 1 ]; then printf '%s\n' '` + line + `'; fi
  exit 0`
}

const (
	streamWatchEnv = "CREW_STREAM_TEST_WATCH_HELPER"
	streamBusVar   = "CREW_STREAM_TEST_BUS"
)

// TestHelperStreamWatch is the real `crew watch` a ported case parks on: the
// stub execs this test binary with the watch argv after `--`, on the bus the
// test named in the environment. Cursor seeding, watch.lock.d and the signal
// handlers are the real ones, which is what the cursor and lock cases assert.
func TestHelperStreamWatch(t *testing.T) {
	if os.Getenv(streamWatchEnv) != "1" {
		t.Skip("helper process for the stream tests")
	}
	args := os.Args
	for i, a := range args {
		if a == "--" {
			args = args[i+1:]
			break
		}
	}
	dir := os.Getenv(streamBusVar)
	if dir == "" {
		fmt.Fprintln(os.Stderr, "helper: no bus named")
		os.Exit(93)
	}
	paths := bus.Paths{Common: dir, Dir: dir + "/crew", Log: dir + "/crew/events.jsonl"}
	if len(args) == 0 || args[0] != "watch" {
		fmt.Fprintf(os.Stderr, "helper: unexpected argv %q\n", args)
		os.Exit(94)
	}
	os.Exit(watch.Run(watchRequest(args[1:]), paths, os.Stdout, os.Stderr, watch.Options{
		Clock: clock.Clock{Now: time.Now},
		Sleep: func(string, io.Writer) error { time.Sleep(50 * time.Millisecond); return nil },
	}))
}

// realWatch is the entry whose `watch` is the real one, on the bus at p. The
// pid the stub records is the watch's own: the exec keeps it.
func realWatch(t *testing.T, p bus.Paths) *stub {
	t.Helper()
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	s := entryStub(t, map[string]string{"watch": `printf '%s\n' "$$" > "$d/watch.pid"
  export ` + streamWatchEnv + `=1
  export ` + streamBusVar + `='` + p.Common + `'
  exec '` + bin + `' -test.run='^TestHelperStreamWatch$' -test.count=1 -test.timeout=60s -- "$@"`})
	return s
}

// streamBus is a bus dir with nothing on it yet.
func streamBus(t *testing.T) bus.Paths {
	t.Helper()
	p := fixture(t)
	if err := os.MkdirAll(p.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func postRow(t *testing.T, p bus.Paths, row string) {
	t.Helper()
	if err := bus.Append(p.Log, row); err != nil {
		t.Fatal(err)
	}
}

// statusDetailRow is a status row with a detail, which is what a blocked
// restamp is judged on.
func statusDetailRow(ts int64, crew, from, state, detail string) string {
	return fmt.Sprintf(`{"ts":%d,"crew_id":%q,"from":%q,"to":"dispatcher:%s","kind":"status","body":{"state":%q,"detail":%q}}`,
		ts, crew, from, crew, state, detail)
}

// lane is one `crew stream` in the background, stopped by TERM like the lane a
// dispatcher ends.
type lane struct {
	t       *testing.T
	paths   bus.Paths
	stub    *stub
	out     *lockedWriter
	errb    *lockedWriter
	done    chan int
	stepN   int
	stopped bool
	rc      int
}

// laneOpts are the Options a case overrides; every zero value is the harness
// default (an instant timer and the wall clock).
type laneOpts struct {
	Timer  func(time.Duration) <-chan time.Time
	Clock  clock.Clock
	Proc   proc.Table
	CrewID func() string
}

// startLane runs one stream with the defaults every case inherits: --park,
// --interval, --coalesce and --retry of 1s, a --heartbeat nothing reaches, and
// --reap-every 0 so no cadence reap runs unless the case asks for one.
func startLane(t *testing.T, p bus.Paths, s *stub, r Request, o laneOpts) *lane {
	t.Helper()
	l := &lane{t: t, paths: p, stub: s, out: &lockedWriter{}, errb: &lockedWriter{}, done: make(chan int, 1)}
	r.Crew = "c1"
	for _, f := range []struct {
		name string
		dst  *string
		def  string
	}{{"park", &r.Park, "1"}, {"interval", &r.Interval, "1"}, {"coalesce", &r.Coalesce, "1"},
		{"retry", &r.Retry, "1"}, {"heartbeat", &r.Heartbeat, "3600"}, {"reap-every", &r.ReapEvery, "0"}} {
		if *f.dst == "" {
			*f.dst = f.def
		}
	}
	timer := o.Timer
	if timer == nil {
		timer = func(time.Duration) <-chan time.Time { return ready() }
	}
	// The tick the loop writes at the top of its first iteration is both proof
	// it is running and the instant its cadence clock was taken: a case that
	// advances a fake clock has to do it after this point, or the start instant
	// it measures against is the advanced one.
	before := tickTS(p)
	go func() {
		l.done <- Run(r, p, l.out, l.errb, Options{
			Self: s.Script, Timer: timer, Clock: o.Clock, Proc: o.Proc, CrewID: o.CrewID,
		})
	}()
	t.Cleanup(l.stop)
	waitFor(t, "the lane's first iteration", func() bool { return tickTS(p) > before })
	return l
}

// stop ends the lane with TERM and returns its exit status.
func (l *lane) stop() {
	l.t.Helper()
	if l.stopped {
		return
	}
	l.stopped = true
	l.signal(syscall.SIGTERM)
	l.rc = l.waitRC()
}

// signal is one signal to this process, where `Run` catches it.
func (l *lane) signal(s os.Signal) {
	l.t.Helper()
	if err := syscall.Kill(os.Getpid(), s.(syscall.Signal)); err != nil {
		l.t.Fatal(err)
	}
}

// waitRC waits for Run to return. The lane is over once it has: a later stop
// must not signal this process again with no handler left to catch it.
func (l *lane) waitRC() int {
	l.t.Helper()
	l.stopped = true
	select {
	case l.rc = <-l.done:
		return l.rc
	case <-time.After(20 * time.Second):
		l.t.Fatal("the stream did not stop")
	}
	return -1
}

func (l *lane) lines() []map[string]any { return decodeLinesT(l.t, l.out.str()) }

// ofKind is every output line of one stream kind, in order.
func (l *lane) ofKind(kind string) []map[string]any {
	var out []map[string]any
	for _, v := range l.lines() {
		if v["stream"] == kind {
			out = append(out, v)
		}
	}
	return out
}

// batches is every pass-through batch line: a line with a cursor and no stream
// tag.
func (l *lane) batches() []map[string]any {
	var out []map[string]any
	for _, v := range l.lines() {
		if _, tagged := v["stream"]; !tagged {
			out = append(out, v)
		}
	}
	return out
}

func (l *lane) raw() string { return l.out.str() }

// waitLines waits for n output lines in all.
func (l *lane) waitLines(n int) {
	l.t.Helper()
	waitFor(l.t, fmt.Sprintf("%d stream lines (have %d: %s)", n, len(l.lines()), l.raw()),
		func() bool { return len(l.lines()) >= n })
}

// waitKind waits for n lines of one kind.
func (l *lane) waitKind(kind string, n int) {
	l.t.Helper()
	waitFor(l.t, fmt.Sprintf("%d %s lines (have %d: %s)", n, kind, len(l.ofKind(kind)), l.raw()),
		func() bool { return len(l.ofKind(kind)) >= n })
}

// holdIDsOf is a hold_due line's matured set, as the ids it announces.
func holdIDsOf(t *testing.T, v any) string {
	t.Helper()
	rows, ok := v.([]any)
	if !ok {
		t.Fatalf("holds = %#v, want an array", v)
	}
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		m, ok := row.(map[string]any)
		if !ok {
			t.Fatalf("hold row = %#v", row)
		}
		id, _ := m["id"].(string)
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return strings.Join(ids, ",")
}

// parkUntil keeps releasing parks until cond holds, which is how a case waits
// for something the loop only does at the top of an iteration — flushing a reap
// child's pair, say.
func (l *lane) parkUntil(what string, cond func() bool) {
	l.t.Helper()
	for i := 0; i < 8 && !cond(); i++ {
		l.park()
	}
	waitFor(l.t, what, cond)
}

// tickTS is the tick the loop rewrote at the top of its last iteration; 0 for
// no tick yet.
func tickTS(p bus.Paths) int64 {
	v, err := decodeObject(readFile(filepath.Join(p.CrewDir("c1"), "stream.tick")))
	if err != nil {
		return 0
	}
	ts, ok := digitsField(v, "ts")
	if !ok {
		return 0
	}
	return ts
}

// waitTickAfter waits for the tick to move past ts, which only the top of an
// iteration does — the proof the loop is still turning.
func waitTickAfter(t *testing.T, p bus.Paths, ts int64) int64 {
	t.Helper()
	waitFor(t, "the tick to move past "+strconv.FormatInt(ts, 10), func() bool {
		now := tickTS(p)
		return now > ts
	})
	return tickTS(p)
}

// turnParks waits for n further iterations, each proven by the tick it rewrote
// at its top. Called after the step that releases a park, it is how a case says
// "the loop turned twice and said nothing".
func turnParks(t *testing.T, p bus.Paths, n int) {
	t.Helper()
	tick := tickTS(p)
	for i := 0; i < n; i++ {
		tick = waitTickAfter(t, p, tick)
	}
}

// cdirFile is a path in the crew's state dir.
func cdirFile(p bus.Paths, name string) string {
	return filepath.Join(p.CrewDir("c1"), name)
}

// seedFile writes a file in the crew's state dir, which the stream has not
// necessarily created yet.
func seedFile(t *testing.T, p bus.Paths, name, text string) {
	t.Helper()
	if err := os.MkdirAll(p.CrewDir("c1"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, cdirFile(p, name), text)
}

// existsIn is the file there, if it is.
func existsIn(p bus.Paths, name string) bool {
	_, err := os.Stat(cdirFile(p, name))
	return err == nil
}

// reapPairs is the per-child reap output files still in the crew's dir.
func reapPairs(p bus.Paths) []string {
	matches, _ := filepath.Glob(cdirFile(p, "stream.reap.out") + ".*")
	return matches
}

// fakeClock is the loop's clock with a handle on it: the reap cadence and the
// error-suppression window are where the loop measures elapsed time, and a case
// that wants one of them should not have to wait for it.
type fakeClock struct {
	off atomic.Int64
}

func (c *fakeClock) Now() time.Time {
	return time.Now().Add(time.Duration(c.off.Load()) * time.Millisecond)
}

func (c *fakeClock) advance(d time.Duration) { c.off.Add(int64(d / time.Millisecond)) }

// clockNow is a fake clock as the loop wants one.
func clockNow(c *fakeClock) clock.Clock { return clock.Clock{Now: c.Now} }

// stepClock is a lane clock that advances a millisecond per read instead of with
// the wall clock. The tick `lane.park` waits on moves on every iteration, so no
// park can stall, and a --heartbeat window closes only once the loop has read
// its clock heartbeat*1000 times — a case that parks a few times never gets
// there, however the machine is scheduled.
//
// `fakeClock` is the other half: it is the wall clock plus an offset, and an
// offset cancels in `now - ts`, so it can pull a window towards a case (what the
// cadence and suppression cases want) but can never hold one open.
type stepClock struct {
	base  time.Time
	reads atomic.Int64
}

func newStepClock() *stepClock { return &stepClock{base: time.Now()} }

func (c *stepClock) Now() time.Time {
	return c.base.Add(time.Duration(c.reads.Add(1)) * time.Millisecond)
}

// laneTime is how far the loop's clock has come: the window a --heartbeat case
// holds open, counted in reads rather than in seconds.
func (c *stepClock) laneTime() time.Duration {
	return time.Duration(c.reads.Load()) * time.Millisecond
}

func clockStep(c *stepClock) clock.Clock { return clock.Clock{Now: c.Now} }
