package stream

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/lock"
	"github.com/noamsto/dispatcher/crew/internal/repos"
)

// relayStub is the crew entry a relaying loop re-enters. `watch` parks briefly
// with nothing to report, or for 30 s once a `park` file exists; `remote-run`
// logs its argv and runs the next `relay.<n>` file (or `relay.default`) for a
// relay, `reap` for a reap. Each relay records its pid, which is also its
// process group.
func relayStub(t *testing.T) (script, dir string) {
	t.Helper()
	dir = t.TempDir()
	settleOnce(t, filepath.Dir(dir))
	script = filepath.Join(t.TempDir(), "crew-stub.sh")
	writeFile(t, script, bashShebang()+`d='`+dir+`'
case "${1:-}" in
  watch) echo watch >> "$d/watch.log"; [ -f "$d/park" ] && exec sleep 30; sleep 0.1; exit 0 ;;
  hold) printf '[]\n'; exit 1 ;;
  remote-run)
    printf '%s\n' "$*" >> "$d/argv.log"
    while [ "$#" -gt 0 ] && [ "$1" != "--" ]; do shift; done
    shift
    case "${2:-}" in
      relay)
        n=$(( $(cat "$d/relay.n" 2>/dev/null || echo 0) + 1 ))
        echo "$n" > "$d/relay.n.tmp.$$" && mv -f "$d/relay.n.tmp.$$" "$d/relay.n"
        echo "$$" > "$d/relay.$n.pid"
        f="$d/relay.$n"; [ -f "$f" ] || f="$d/relay.default"
        . "$f" ;;
      reap) [ -f "$d/reap" ] && . "$d/reap" ;;
    esac
    exit 0 ;;
  *) exit 0 ;;
esac
`)
	chmodExecutable(t, script)
	writeFile(t, filepath.Join(dir, "relay.default"), "sleep 30\n")
	return script, dir
}

const (
	relayBatch = `{"cursor":5,"events":[{"ts":5,"crew_id":"c1","from":"worker:feat/r","to":"dispatcher:c1","kind":"status","body":{"state":"working"}}]}`
	doneBatch  = `{"cursor":9,"events":[{"ts":9,"crew_id":"c1","from":"worker:feat/r","to":"dispatcher:c1","kind":"status","body":{"state":"done"}}]}`
)

// printThenSleep is a relay that prints lines and then stays connected.
func printThenSleep(lines ...string) string {
	var b strings.Builder
	for _, l := range lines {
		b.WriteString("printf '%s\\n' '" + l + "'\n")
	}
	return b.String() + "sleep 30\n"
}

func remoteDispatchRow(crew, host string) string {
	return `{"ts":1,"crew_id":"` + crew + `","from":"dispatcher:` + crew + `","kind":"dispatch","branch":"feat/r","host":"` + host + `","remote":true}`
}

func relayBus(t *testing.T) bus.Paths {
	t.Helper()
	p := fixture(t)
	if err := os.MkdirAll(p.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := bus.Append(p.Log, remoteDispatchRow("c1", "mini")); err != nil {
		t.Fatal(err)
	}
	return p
}

// fastRelay is relay timing a test can wait out.
func fastRelay() relayTiming {
	return relayTiming{heartbeat: 10 * time.Second, retry: 50 * time.Millisecond}
}

// streamRun is one single-stream Run in the background, stopped by TERM.
type streamRun struct {
	t       *testing.T
	out     *lockedWriter
	done    chan int
	stopped bool
}

func startStream(t *testing.T, p bus.Paths, self string, r Request, timing relayTiming) *streamRun {
	t.Helper()
	s := &streamRun{t: t, out: &lockedWriter{}, done: make(chan int, 1)}
	r.Crew = "c1"
	r.Park, r.Interval, r.Coalesce, r.Retry = "1", "1", "1", "1"
	r.Heartbeat = "3600"
	if r.ReapEvery == "" {
		r.ReapEvery = "0"
	}
	go func() {
		s.done <- Run(r, p, s.out, os.Stderr, Options{Self: self, Timer: func(time.Duration) <-chan time.Time { return ready() }, relay: timing})
	}()
	t.Cleanup(s.stop)
	return s
}

func (s *streamRun) stop() {
	s.t.Helper()
	if s.stopped {
		return
	}
	s.stopped = true
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		s.t.Fatal(err)
	}
	select {
	case <-s.done:
	case <-time.After(20 * time.Second):
		s.t.Fatal("the stream did not stop on TERM")
	}
}

func (s *streamRun) lines() []map[string]any { return decodeLinesT(s.t, s.out.str()) }

// line is the first output line that match accepts.
func line(lines []map[string]any, match func(map[string]any) bool) map[string]any {
	for _, l := range lines {
		if match(l) {
			return l
		}
	}
	return nil
}

func isRelayed(l map[string]any) bool { _, ok := l["cursor"]; return ok && l["host"] == "mini" }

func isStream(kind string) func(map[string]any) bool {
	return func(l map[string]any) bool { return l["stream"] == kind && l["host"] == "mini" }
}

func argvLog(dir string) []string {
	text := strings.TrimSpace(readFile(filepath.Join(dir, "argv.log")))
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

func relayStarts(dir string) int {
	n := 0
	for _, a := range argvLog(dir) {
		if strings.Contains(a, " -- crew relay ") {
			n++
		}
	}
	return n
}

func relayPgid(t *testing.T, dir string, n int) int {
	t.Helper()
	pid, err := strconv.Atoi(strings.TrimSpace(readFile(filepath.Join(dir, "relay."+strconv.Itoa(n)+".pid"))))
	if err != nil {
		t.Fatalf("relay %d recorded no pid: %v", n, err)
	}
	return pid
}

func groupGone(pgid int) bool { return errors.Is(syscall.Kill(-pgid, 0), syscall.ESRCH) }

func TestRelayForwardsBatchAndStoresCursor(t *testing.T) {
	p := relayBus(t)
	self, dir := relayStub(t)
	writeFile(t, filepath.Join(dir, "relay.1"), printThenSleep(`{"relay":"heartbeat","ts":3}`, relayBatch))
	s := startStream(t, p, self, Request{}, fastRelay())

	cursor := filepath.Join(p.CrewDir("c1"), "relay", "mini.cursor")
	waitFor(t, "the relayed batch and its cursor", func() bool {
		return line(s.lines(), isRelayed) != nil && strings.TrimSpace(readFile(cursor)) == "5"
	})
	b := line(s.lines(), isRelayed)
	if b["cursor"] != float64(5) {
		t.Errorf("cursor = %v, want 5", b["cursor"])
	}
	if _, ok := b["repo"]; ok {
		t.Errorf("a single-stream batch carries repo: %v", b)
	}
	events, _ := b["events"].([]any)
	if len(events) != 1 {
		t.Fatalf("events = %v", b["events"])
	}
	ev, _ := events[0].(map[string]any)
	if ev["host"] != "mini" || ev["from"] != "worker:feat/r" {
		t.Errorf("event = %v, want the remote row plus host mini", ev)
	}
	if got := line(s.lines(), func(l map[string]any) bool { return l["relay"] != nil }); got != nil {
		t.Errorf("a relay heartbeat reached stdout: %v", got)
	}
	want := "remote-run --host mini --env CREW_ID=c1 -- crew relay --crew c1 --since 0 --states " + defaultStates
	if a := argvLog(dir); len(a) == 0 || a[0] != want {
		t.Errorf("argv = %q, want %q", a, want)
	}
	if n := relayStarts(dir); n != 1 {
		t.Errorf("relay started %d times, want 1", n)
	}
}

func TestRelayLinkLossReportsAndResumesFromCursor(t *testing.T) {
	p := relayBus(t)
	self, dir := relayStub(t)
	writeFile(t, filepath.Join(dir, "relay.1"),
		"printf '%s\\n' '"+strings.Replace(relayBatch, `"cursor":5`, `"cursor":7`, 1)+"'\necho boom >&2\necho second >&2\nexit 255\n")
	s := startStream(t, p, self, Request{}, fastRelay())

	waitFor(t, "the relay to restart", func() bool { return relayStarts(dir) >= 2 })
	waitFor(t, "the link-loss error", func() bool { return line(s.lines(), isStream("error")) != nil })
	e := line(s.lines(), isStream("error"))
	if e["crew"] != "c1" || e["rc"] != float64(255) || e["detail"] != "relay to mini lost: boom" {
		t.Errorf("error line = %v", e)
	}
	if a := argvLog(dir); !strings.Contains(a[1], " --since 7 ") {
		t.Errorf("the restart did not resume from the stored cursor: %q", a[1])
	}
}

func TestRelayWatchdogKillsASilentChild(t *testing.T) {
	p := relayBus(t)
	self, dir := relayStub(t)
	writeFile(t, filepath.Join(dir, "relay.default"), printThenSleep(`{"relay":"heartbeat","ts":1}`))
	timing := fastRelay()
	timing.heartbeat = 100 * time.Millisecond
	s := startStream(t, p, self, Request{}, timing)

	waitFor(t, "the watchdog error", func() bool { return line(s.lines(), isStream("error")) != nil })
	e := line(s.lines(), isStream("error"))
	if e["detail"] != "relay to mini lost: no line for 300ms" || e["rc"] != float64(128+int(syscall.SIGKILL)) {
		t.Errorf("error line = %v", e)
	}
	waitFor(t, "a reconnect", func() bool { return relayStarts(dir) >= 2 })
	// A relay killed before it recorded its pid has no group left to check.
	for n := 1; n < relayStarts(dir); n++ {
		if readFile(filepath.Join(dir, "relay."+strconv.Itoa(n)+".pid")) == "" {
			continue
		}
		pgid := relayPgid(t, dir, n)
		waitFor(t, "the silent relay's group to be killed", func() bool { return groupGone(pgid) })
		return
	}
	t.Fatal("no replaced relay recorded its pid")
}

func TestRelayWaitsForAHeldLock(t *testing.T) {
	p := relayBus(t)
	self, dir := relayStub(t)
	lockd := filepath.Join(p.CrewDir("c1"), "relay", "mini.lock.d")
	if err := os.MkdirAll(filepath.Dir(lockd), 0o755); err != nil {
		t.Fatal(err)
	}
	if !lock.Acquire(lockd, "another-loop") {
		t.Fatal("could not take the relay lock")
	}
	held := true
	defer func() {
		if held {
			lock.Release(lockd)
		}
	}()
	startStream(t, p, self, Request{}, fastRelay())

	waitFor(t, "several cycles", func() bool { return strings.Count(readFile(filepath.Join(dir, "watch.log")), "\n") >= 3 })
	if n := relayStarts(dir); n != 0 {
		t.Fatalf("a relay started %d times while another loop held the lock", n)
	}
	lock.Release(lockd)
	held = false
	waitFor(t, "the relay once the lock is free", func() bool { return relayStarts(dir) == 1 })
}

func TestRelayDoneBatchTriggersRemoteReap(t *testing.T) {
	p := relayBus(t)
	self, dir := relayStub(t)
	writeFile(t, filepath.Join(dir, "relay.1"), printThenSleep(doneBatch))
	writeFile(t, filepath.Join(dir, "reap"), "echo 'crew reap: removed feat/r'\n")
	s := startStream(t, p, self, Request{ReapEvery: "3600"}, fastRelay())

	waitFor(t, "the remote reap line", func() bool { return line(s.lines(), isStream("reap")) != nil })
	r := line(s.lines(), isStream("reap"))
	lines, _ := r["lines"].([]any)
	if r["crew"] != "c1" || len(lines) != 1 || lines[0] != "removed feat/r" {
		t.Errorf("reap line = %v", r)
	}
	want := "remote-run --host mini --env CREW_ID=c1 -- crew reap --quiet"
	found := false
	for _, a := range argvLog(dir) {
		found = found || a == want
	}
	if !found {
		t.Errorf("argv = %q, want a %q", argvLog(dir), want)
	}
}

func TestRelayReapEveryZeroReapsNothing(t *testing.T) {
	p := relayBus(t)
	self, dir := relayStub(t)
	writeFile(t, filepath.Join(dir, "relay.1"), printThenSleep(doneBatch))
	timing := fastRelay()
	timing.reapEvery = 50 * time.Millisecond
	s := startStream(t, p, self, Request{ReapEvery: "0"}, timing)

	waitFor(t, "the done batch", func() bool { return line(s.lines(), isRelayed) != nil })
	waitFor(t, "several cycles", func() bool { return strings.Count(readFile(filepath.Join(dir, "watch.log")), "\n") >= 3 })
	for _, a := range argvLog(dir) {
		if strings.Contains(a, "crew reap") {
			t.Errorf("--reap-every 0 still ran %q", a)
		}
	}
}

func TestRelayUndecodableLineIsAnErrorAndKeepsTheCursor(t *testing.T) {
	p := relayBus(t)
	self, dir := relayStub(t)
	writeFile(t, filepath.Join(dir, "relay.1"), printThenSleep(`{"cursor":8,"events":[3]}`, `not json`, relayBatch))
	s := startStream(t, p, self, Request{}, fastRelay())

	waitFor(t, "the good batch", func() bool { return line(s.lines(), isRelayed) != nil })
	var errs, batches int
	for _, l := range s.lines() {
		switch {
		case isStream("error")(l):
			errs++
		case isRelayed(l):
			batches++
			if l["cursor"] != float64(5) {
				t.Errorf("an undecodable batch was forwarded: %v", l)
			}
		}
	}
	if errs == 0 || batches != 1 {
		t.Errorf("errors = %d, batches = %d, want some and 1: %s", errs, batches, s.out.str())
	}
	cursor := filepath.Join(p.CrewDir("c1"), "relay", "mini.cursor")
	waitFor(t, "the good batch's cursor", func() bool { return strings.TrimSpace(readFile(cursor)) == "5" })
}

func TestRelayCleanupKillsTheProcessGroup(t *testing.T) {
	p := relayBus(t)
	self, dir := relayStub(t)
	s := startStream(t, p, self, Request{}, fastRelay())

	waitFor(t, "the relay", func() bool { return readFile(filepath.Join(dir, "relay.1.pid")) != "" })
	pgid := relayPgid(t, dir, 1)
	if groupGone(pgid) {
		t.Fatal("the relay's group is gone before cleanup")
	}
	s.stop()
	waitFor(t, "the relay's group to be gone", func() bool { return groupGone(pgid) })
	if _, err := os.Stat(filepath.Join(p.CrewDir("c1"), "relay", "mini.lock.d")); !os.IsNotExist(err) {
		t.Error("relay/mini.lock.d survived the cleanup")
	}
}

func TestRelayNoRemoteRowsStartsNothing(t *testing.T) {
	p := fixture(t)
	if err := os.MkdirAll(p.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := bus.Append(p.Log, `{"ts":1,"crew_id":"c1","from":"dispatcher:c1","kind":"dispatch","branch":"feat/l","host":"mini"}`); err != nil {
		t.Fatal(err)
	}
	self, dir := relayStub(t)
	startStream(t, p, self, Request{}, fastRelay())

	waitFor(t, "several cycles", func() bool { return strings.Count(readFile(filepath.Join(dir, "watch.log")), "\n") >= 3 })
	if a := argvLog(dir); len(a) != 0 {
		t.Errorf("a local dispatch row started %q", a)
	}
	if _, err := os.Stat(filepath.Join(p.CrewDir("c1"), "relay")); !os.IsNotExist(err) {
		t.Error("a relay dir was created with no remote row")
	}
}

func TestFleetRelayLabelsRepoAndReapsPeriodically(t *testing.T) {
	alpha := gitRepo(t, "alpha")
	appendRow(t, alpha, remoteDispatchRow("c1", "mini"))
	self, dir := relayStub(t)
	writeFile(t, filepath.Join(dir, "relay.1"), printThenSleep(relayBatch))
	timing := fastRelay()
	timing.reapEvery = 50 * time.Millisecond
	l := startFleet(t, FleetOptions{
		Options:  Options{Self: self, relay: timing},
		Discover: okRepos(alpha),
		Git:      origin(alpha, "git@github.com:o/alpha.git"),
	}, fastRequest())

	waitFor(t, "the relayed batch", func() bool { return line(l.lines(), isRelayed) != nil })
	b := line(l.lines(), isRelayed)
	if want := repos.Name(alpha, "git@github.com:o/alpha.git"); b["repo"] != want {
		t.Errorf("repo = %v, want %q", b["repo"], want)
	}
	cursor := filepath.Join(alpha, "crew", "crews", "c1", "relay", "mini.cursor")
	waitFor(t, "the cursor in the repo's crew dir", func() bool { return strings.TrimSpace(readFile(cursor)) == "5" })
	waitFor(t, "a periodic remote reap", func() bool {
		for _, a := range argvLog(dir) {
			if strings.HasSuffix(a, "-- crew reap --quiet") {
				return true
			}
		}
		return false
	})
}

// A remote dispatch row that lands while the loop is parked starts its relay
// on the --interval cadence, not once the park ends.
func TestRelayStartsWhileTheLoopIsParked(t *testing.T) {
	p := fixture(t)
	if err := os.MkdirAll(p.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := bus.Append(p.Log, `{"ts":1,"crew_id":"c1","from":"dispatcher:c1","kind":"dispatch","branch":"feat/l"}`); err != nil {
		t.Fatal(err)
	}
	self, dir := relayStub(t)
	writeFile(t, filepath.Join(dir, "park"), "")
	startStream(t, p, self, Request{}, fastRelay())

	waitFor(t, "the loop to park", func() bool { return readFile(filepath.Join(dir, "watch.log")) != "" })
	if err := bus.Append(p.Log, remoteDispatchRow("c1", "mini")); err != nil {
		t.Fatal(err)
	}
	appended := time.Now()
	waitFor(t, "the relay while parked", func() bool { return relayStarts(dir) == 1 })
	if waited := time.Since(appended); waited > 5*time.Second {
		t.Errorf("the relay started %v after its row, want about --interval (1s)", waited)
	}
	if n := strings.Count(readFile(filepath.Join(dir, "watch.log")), "\n"); n != 1 {
		t.Errorf("the loop parked %d times, want the relay to start inside the first park", n)
	}
}

// sync reads the bus only when its size or mtime moved: an unchanged log is
// not re-read on every tick.
func TestRelaySyncSkipsAnUnchangedLog(t *testing.T) {
	p := relayBus(t)
	self, _ := relayStub(t)
	rs := &relays{
		out: &lockedWriter{}, procs: children{self: self}, log: p.Log, cdir: p.CrewDir("c1"),
		crew: "c1", states: defaultStates, owner: "test", retry: time.Second, heartbeat: 10 * time.Second,
		now: func() int64 { return 0 }, hosts: map[string]*relay{}, stop: make(chan struct{}),
	}
	defer rs.shutdown()
	rs.sync()
	st, err := os.Stat(p.Log)
	if err != nil {
		t.Fatal(err)
	}
	// Same size, same mtime, a different host: only a re-read would see it.
	text := strings.Replace(readFile(p.Log), `"mini"`, `"mino"`, 1)
	writeFile(t, p.Log, text)
	if err := os.Chtimes(p.Log, st.ModTime(), st.ModTime()); err != nil {
		t.Fatal(err)
	}
	rs.sync()
	if rs.hosts["mino"] != nil {
		t.Error("sync re-read a log whose size and mtime had not moved")
	}
	if err := bus.Append(p.Log, remoteDispatchRow("c1", "maxi")); err != nil {
		t.Fatal(err)
	}
	rs.sync()
	if rs.hosts["mini"] == nil || rs.hosts["maxi"] == nil {
		t.Errorf("hosts = %v, want mini and maxi", rs.hosts)
	}
}
