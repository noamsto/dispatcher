package rosterrender

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/clock"
	"github.com/noamsto/dispatcher/crew/internal/crews"
	"github.com/noamsto/dispatcher/crew/internal/roster"
)

// loop is a daemon run driven by fakes: the clock is a file the fake sleep
// advances (so the quiet window and the rebuild backstop are the suite's, while
// the poll stays instant), and every side effect is a recorded call.
type loop struct {
	t     *testing.T
	paths bus.Paths
	opts  Options

	mu       sync.Mutex
	hops     []hop
	slept    int
	panes    string
	clockNow int64
	exited   chan int
}

type hop struct {
	path      string
	argv, env []string
}

func newLoop(t *testing.T) *loop {
	t.Helper()
	dir := t.TempDir()
	p := bus.Paths{Common: dir, Dir: dir + "/crew", Log: dir + "/crew/events.jsonl"}
	if err := os.MkdirAll(p.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	l := &loop{t: t, paths: p, clockNow: 1_791_360_000, exited: make(chan int, 1)}
	clockFile := filepath.Join(dir, "clock")
	if err := os.WriteFile(clockFile, []byte("1791360000\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	l.opts = Options{
		Clock:  clock.Clock{Now: func() time.Time { return time.Unix(l.wallNow(), 0) }, CrewClock: clockFile},
		Stdout: &bytes.Buffer{},
		Stderr: &bytes.Buffer{},
		Probes: Probes{
			RolePanes: func() string { return l.rolePanes() },
			PanePID:   func(string) (string, bool) { return "", false },
			AeyeHelp: func(context.Context) (string, error) {
				return "", nil
			},
			AeyePublish:   func(context.Context, string, string, bool) error { return nil },
			InstalledCrew: func(string) string { return "" },
			SpawnDetached: func([]string, []string) error { return nil },
			Exec: func(path string, argv, env []string) error {
				l.mu.Lock()
				l.hops = append(l.hops, hop{path: path, argv: argv, env: env})
				l.mu.Unlock()
				return nil
			},
			Sleep: l.sleep,
		},
		Procs: crews.Probes{Parent: func(int) (int, bool) { return 0, false }},
		// `crew roster`'s own two reads: no live panes, no worktrees.
		Roster: roster.Probes{
			Panes:     func() string { return "" },
			Worktrees: func() (string, error) { return "", nil },
		},
		RosterDir: filepath.Join(dir, "d2"),
		PID:       os.Getpid(),
	}
	return l
}

func (l *loop) wallNow() int64 { return 1_791_360_000 }
func (l *loop) rolePanes() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.panes
}

func (l *loop) tick(seconds int64) {
	l.mu.Lock()
	l.clockNow += seconds
	n := l.clockNow
	l.mu.Unlock()
	_ = os.WriteFile(l.opts.Clock.CrewClock, []byte(strconv.FormatInt(n, 10)+"\n"), 0o644)
}

// seed writes one dispatched worker, optionally with a status, so the model has a
// row without a real bus reader.
func (l *loop) seed(t *testing.T, events ...string) {
	t.Helper()
	if len(events) == 0 {
		events = []string{`{"ts":1791360000000,"crew_id":"c1","kind":"dispatch","branch":"feat/1-a","session":"s1","worker_id":"worker:feat/1-a#s1","engine":"claude","model":"sonnet","tier":"standard","title":"Alpha","name":"sage","color":"green","tmux":"colour28"}`}
	}
	f, err := os.Create(l.paths.Log)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if _, err := f.WriteString(e + "\n"); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// ceiling is how many ticks a loop may take before the test calls it a hang.
const ceiling = 60

// sleep stands in for the wall-clock poll: it advances the virtual clock so the
// quiet window and the rebuild backstop move, and cancels at the ceiling rather
// than hanging the test if the loop never ends on its own.
func (l *loop) sleep(ctx context.Context, _ time.Duration) error {
	l.mu.Lock()
	l.slept++
	n := l.slept
	l.mu.Unlock()
	l.tick(1)
	if n >= ceiling {
		return context.Canceled
	}
	return ctx.Err()
}

// run drives the loop to its own exit, or to the ceiling. A code of exitOK means
// the daemon decided to stop; the tick count says whether that was the decision
// under test or the ceiling.
func (l *loop) run(args ...string) int {
	l.t.Helper()
	c, msg, code := parse(append([]string{"--crew", "c1"}, args...))
	if msg != "" {
		l.t.Fatalf("parse: %s (%d)", msg, code)
	}
	cdir := l.paths.CrewDir("c1")
	if err := os.MkdirAll(cdir, 0o755); err != nil {
		l.t.Fatal(err)
	}
	daemon := []string{"--crew", "c1", "--interval", c.intervalText, "--quiet", c.quietText}
	return l.opts.daemon(context.Background(), l.paths, cdir, c, daemon)
}

func (l *loop) hitCeiling() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.slept >= ceiling
}

func (l *loop) cdir() string { return l.paths.CrewDir("c1") }

func (l *loop) lockOwner() string { return record(l.cdir(), "roster-render.lock.d/pid") }

// TestDaemonFollowsTheInstalledEntryNotItsOwnStart is the floating-identity bug:
// CREW_SELF names the entry, and the entry is the symlink a switch repoints, so
// the running build is resolved once. A daemon that re-resolved it would follow
// the entry with its eyes shut and never hop.
func TestDaemonFollowsTheInstalledEntryNotItsOwnStart(t *testing.T) {
	l := newLoop(t)
	l.seed(t)
	self := filepath.Join(t.TempDir(), "crew")
	if err := os.WriteFile(self, []byte("#!/usr/bin/env bash\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	entry := filepath.Join(t.TempDir(), "entry")
	if err := os.MkdirAll(entry, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(entry, "crew")
	// The entry points at the running build, then a switch repoints it.
	if err := os.Symlink(self, link); err != nil {
		t.Fatal(err)
	}
	l.opts.Self = self
	l.opts.Path = entry
	l.opts.Probes.InstalledCrew = func(string) string { return InstalledCrew(entry) }

	// Unchanged: no hop, however many passes.
	if err := l.opts.hop(realPath(self), false, nil); err != nil {
		t.Fatalf("an unchanged installed entry made the daemon re-exec: %v", err)
	}
	newer := self + ".newer"
	if err := os.WriteFile(newer, []byte("#!/usr/bin/env bash\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(newer, link); err != nil {
		t.Fatal(err)
	}
	if err := l.opts.hop(realPath(self), false, nil); err != nil {
		t.Fatalf("a repointed entry did not make the daemon re-exec: %v", err)
	}
	if err := l.opts.hop(realPath(self), true, nil); err != nil {
		t.Errorf("a draining crew was upgraded: %v", err)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.hops) != 1 {
		t.Fatalf("hops = %d", len(l.hops))
	}
	got := l.hops[0]
	if got.path != mustReal(t, newer) {
		t.Errorf("exec'd %q, want %q", got.path, newer)
	}
	if strings.Join(got.argv, " ") != mustReal(t, newer)+" roster-render" {
		t.Errorf("argv = %v", got.argv)
	}
}

func mustReal(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// TestDaemonRestoresItsStartPathOnEveryHop is the arm's `PATH=… exec`: a hop
// carries one wrapper prefix, never N.
func TestDaemonRestoresItsStartPathOnEveryHop(t *testing.T) {
	env := envFor("/start/path")
	var path, start string
	for _, kv := range env {
		switch {
		case strings.HasPrefix(kv, "PATH="):
			path = strings.TrimPrefix(kv, "PATH=")
		case strings.HasPrefix(kv, "CREW_RR_START_PATH="):
			start = strings.TrimPrefix(kv, "CREW_RR_START_PATH=")
		}
	}
	if path != "/start/path" || start != "/start/path" {
		t.Errorf("PATH=%q CREW_RR_START_PATH=%q", path, start)
	}
	n := 0
	for _, kv := range env {
		if strings.HasPrefix(kv, "PATH=") || strings.HasPrefix(kv, "CREW_RR_START_PATH=") {
			n++
		}
	}
	if n != 2 {
		t.Errorf("the inherited copies survived: %d entries", n)
	}
}

// TestDaemonExitsAfterTheQuietWindow is the drained crew's retirement, and that a
// crew with something live never reaches it.
func TestDaemonExitsAfterTheQuietWindow(t *testing.T) {
	t.Run("drained", func(t *testing.T) {
		l := newLoop(t)
		// An empty bus has no row to be live: this is the drained crew.
		if err := os.WriteFile(l.paths.Log, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		code := l.run("--quiet", "5")
		if code != exitOK {
			t.Errorf("code = %d", code)
		}
		if l.slept < 5 || l.slept > 8 {
			t.Errorf("exited after %d ticks, want the 5s window and no more", l.slept)
		}
	})
	t.Run("live crew outlasts it", func(t *testing.T) {
		l := newLoop(t)
		l.seed(t)
		l.panes = l.cdir() + "\tc1\tfeat/1-a\tspec-critic\tidle\t\n"
		// The only way out of this loop is the ceiling: a live crew never
		// reaches the quiet window.
		l.run("--quiet", "5")
		if !l.hitCeiling() {
			t.Errorf("a live crew retired itself after %d ticks", l.slept)
		}
	})
}

// TestDaemonKeepsTheLastLiveCountWhenAPassFails: one malformed line must not read
// as a drained crew and retire the renderer. The bus turns unparseable after the
// first pass, and the daemon stays up through the whole quiet window.
func TestDaemonKeepsTheLastLiveCountWhenAPassFails(t *testing.T) {
	l := newLoop(t)
	l.seed(t)
	first := l.sleep
	l.opts.Probes.Sleep = func(ctx context.Context, d time.Duration) error {
		if l.slept == 1 {
			// The bus turns unparseable after the first pass.
			if err := os.WriteFile(l.paths.Log, []byte("{not json\n"), 0o644); err != nil {
				t.Error(err)
			}
		}
		return first(ctx, d)
	}
	l.run("--quiet", "5")
	if !l.hitCeiling() {
		t.Errorf("a malformed bus retired the renderer after %d ticks", l.slept)
	}
}

// TestDaemonRefusesASecondCopySilently: the incumbent keeps the lock, and the
// newcomer's pane record above it already retargeted the running renderer.
func TestDaemonRefusesASecondCopySilently(t *testing.T) {
	l := newLoop(t)
	l.seed(t)
	cdir := l.cdir()
	if err := os.MkdirAll(filepath.Join(cdir, "roster-render.lock.d"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A live pid: a dead one is a stale lock, which the helper reclaims.
	incumbent := strconv.Itoa(os.Getppid())
	if err := os.WriteFile(filepath.Join(cdir, "roster-render.lock.d/pid"), []byte(incumbent+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	l.opts.Stderr = &stderr
	if code := l.run(); code != exitOK {
		t.Errorf("code = %d", code)
	}
	if stderr.Len() != 0 {
		t.Errorf("a refused start was noisy: %q", stderr.String())
	}
	if l.lockOwner() != incumbent {
		t.Errorf("the newcomer took the incumbent's lock: %q", l.lockOwner())
	}
}

// TestDaemonExitsWhenItLosesTheLock: `crew deregister` removes the crew dir under
// a running renderer, and the loop notices at the top of its next pass.
func TestDaemonExitsWhenItLosesTheLock(t *testing.T) {
	l := newLoop(t)
	l.seed(t)
	first := l.sleep
	l.opts.Probes.Sleep = func(ctx context.Context, d time.Duration) error {
		if l.slept == 2 {
			if err := os.RemoveAll(filepath.Join(l.cdir(), "roster-render.lock.d")); err != nil {
				t.Error(err)
			}
		}
		return first(ctx, d)
	}
	if code := l.run("--quiet", "600"); code != exitOK {
		t.Errorf("code = %d", code)
	}
	if l.slept > 3 {
		t.Errorf("kept running after losing the lock: %d ticks", l.slept)
	}
}

// TestDaemonReleasesOnlyItsOwnLock: `_lock_release` is an unconditional rm -rf, so
// a renderer whose lock was reclaimed must not delete the new owner's.
func TestDaemonReleasesOnlyItsOwnLock(t *testing.T) {
	l := newLoop(t)
	l.seed(t)
	first := l.sleep
	l.opts.Probes.Sleep = func(ctx context.Context, d time.Duration) error {
		if l.slept == 1 {
			if err := os.WriteFile(filepath.Join(l.cdir(), "roster-render.lock.d/pid"), []byte("4242\n"), 0o644); err != nil {
				t.Error(err)
			}
		}
		return first(ctx, d)
	}
	if code := l.run("--quiet", "600"); code != exitOK {
		t.Errorf("code = %d", code)
	}
	if got := l.lockOwner(); got != "4242" {
		t.Errorf("the new owner's lock was removed: %q", got)
	}
}

// TestPassWritesOnlyWhenTheDiagramChanges is the arm's `cmp -s` gate, which is what
// keeps the aeye carousel from re-rendering an identical frame every interval.
func TestPassWritesOnlyWhenTheDiagramChanges(t *testing.T) {
	l := newLoop(t)
	l.seed(t)
	cdir := l.cdir()
	if err := os.MkdirAll(cdir, 0o755); err != nil {
		t.Fatal(err)
	}
	file := func() string {
		return target(l.paths.Common, "c1", l.opts.RosterDir, l.opts.Clock.Now)
	}
	if _, err := pass(context.Background(), l.opts, l.paths, cdir, "c1", true, nil); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(file())
	if err != nil {
		t.Fatal(err)
	}
	stamp, err := os.Stat(file())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pass(context.Background(), l.opts, l.paths, cdir, "c1", true, nil); err != nil {
		t.Fatal(err)
	}
	again, err := os.Stat(file())
	if err != nil {
		t.Fatal(err)
	}
	if !stamp.ModTime().Equal(again.ModTime()) {
		t.Errorf("an unchanged bus rewrote the diagram")
	}
	if got, _ := os.ReadFile(file()); string(got) != string(first) {
		t.Errorf("the diagram changed under an unchanged bus")
	}
}
