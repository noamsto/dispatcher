package rosterrender

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/proc/proctest"
)

// The other half of a render: who gets told. aeye's carousel is opened once per
// dispatcher pane and republished when the diagram changes, and a second start
// retargets the renderer already running rather than starting a second one.

// ownPane makes the pane the caller's own: tmux names `pid` for it, and that pid
// is one of this process's ancestors, which is the ancestry Run checks. Without
// this a --pane is a target the caller cannot place.
func (l *loop) ownPane(pid int) {
	l.panePID = strconv.Itoa(pid)
	l.opts.Proc = proctest.New(map[int]bool{pid: true}, map[int]int{os.Getpid(): pid})
}

// publishLines is the stub log the fixtures asserted against: one aeye
// publish-diagram call per line, in the order they went out.
func (l *loop) publishLines() []string {
	got := l.publishLog()
	if got == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(got, "\n"), "\n")
}

// TestAPaneWithoutPublishDiagramGetsNoPublish is the aeye probe: the renderer only
// publishes to an aeye whose --help lists publish-diagram as its own subcommand,
// and a plain aeye still gets its diagram.
func TestAPaneWithoutPublishDiagramGetsNoPublish(t *testing.T) {
	l := newLoop(t)
	l.seedBus(t)
	l.ownPane(400)

	code, stderr := l.once(Request{Pane: "%7"})
	if code != exitOK {
		t.Fatalf("code = %d, stderr %q", code, stderr)
	}
	if _, err := os.Stat(l.diagramPath()); err != nil {
		t.Fatalf("no diagram: %v", err)
	}
	if got := l.publishLines(); len(got) != 0 {
		t.Errorf("a plain aeye was asked to publish: %q", got)
	}
}

// TestPublishOpensOncePerPaneAndRepublishesOnChange is the carousel's own rule: a
// pane it has opened stays opened, so a change republishes without --open, and a
// new pane is handed the unchanged diagram — republished, not rewritten.
func TestPublishOpensOncePerPaneAndRepublishesOnChange(t *testing.T) {
	l := newLoop(t)
	l.seedBus(t)
	l.aeyeHelp = aeyePublishHelp
	l.ownPane(400)
	file := l.diagramPath()

	if code, stderr := l.once(Request{Pane: "%7"}); code != exitOK {
		t.Fatalf("code = %d, stderr %q", code, stderr)
	}
	want := "aeye publish-diagram " + file + " --pane %7 --open"
	if got := l.publishLines(); len(got) != 1 || got[0] != want {
		t.Fatalf("publishes = %q, want [%q]", got, want)
	}

	// The pane record persists: no --pane needed, and nothing changed.
	if code, stderr := l.once(Request{}); code != exitOK {
		t.Fatalf("code = %d, stderr %q", code, stderr)
	}
	if got := l.publishLines(); len(got) != 1 {
		t.Fatalf("an unchanged bus republished: %q", got)
	}

	l.appendBus(t, statusEvent("worker:feat/1-a#s1", 1791361500000, "working", "execute: lint", "", ""))
	if code, stderr := l.once(Request{}); code != exitOK {
		t.Fatalf("code = %d, stderr %q", code, stderr)
	}
	got := l.publishLines()
	if len(got) != 2 {
		t.Fatalf("publishes = %q, want two calls", got)
	}
	if want := "aeye publish-diagram " + file + " --pane %7"; got[1] != want {
		t.Errorf("a change re-opened the carousel: %q, want %q", got[1], want)
	}
	if !strings.Contains(l.diagram(), `working · execute: lint · since 08:25" {`) {
		t.Errorf("the new detail never reached the diagram:\n%s", l.diagram())
	}

	// A new pane republishes the unchanged file without rewriting it.
	stamp := rewind(t, file)
	if code, stderr := l.once(Request{Pane: "%8"}); code != exitOK {
		t.Fatalf("code = %d, stderr %q", code, stderr)
	}
	got = l.publishLines()
	if len(got) != 3 {
		t.Fatalf("publishes = %q, want three calls", got)
	}
	if want := "aeye publish-diagram " + file + " --pane %8 --open"; got[2] != want {
		t.Errorf("last publish = %q, want %q", got[2], want)
	}
	if after := stampOf(t, file); after != stamp {
		t.Errorf("a new pane rewrote the diagram: %s then %s", stamp, after)
	}
}

// TestNoOpenNeverPassesOpen is --no-open: every publish still goes out, and none
// of them raises the carousel — not for a second pane either.
func TestNoOpenNeverPassesOpen(t *testing.T) {
	l := newLoop(t)
	l.seedBus(t)
	l.aeyeHelp = aeyePublishHelp
	l.ownPane(400)

	for _, pane := range []string{"%7", "%8"} {
		if code, stderr := l.once(Request{Pane: pane, NoOpen: true}); code != exitOK {
			t.Fatalf("%s: code = %d, stderr %q", pane, code, stderr)
		}
	}
	got := l.publishLines()
	if len(got) != 2 {
		t.Fatalf("publishes = %q, want two calls", got)
	}
	if want := "aeye publish-diagram " + l.diagramPath() + " --pane %7"; got[0] != want {
		t.Errorf("publishes[0] = %q, want %q", got[0], want)
	}
	for _, line := range got {
		if strings.Contains(line, "--open") {
			t.Errorf("--no-open passed --open after all: %q", line)
		}
	}
}

// TestNoPaneEverGivenWritesTheFileAndNeverPublishes: with no pane there is no
// carousel to raise, whatever aeye can do.
func TestNoPaneEverGivenWritesTheFileAndNeverPublishes(t *testing.T) {
	l := newLoop(t)
	l.seedBus(t)
	l.aeyeHelp = aeyePublishHelp

	if code, stderr := l.once(Request{}); code != exitOK {
		t.Fatalf("code = %d, stderr %q", code, stderr)
	}
	if _, err := os.Stat(l.diagramPath()); err != nil {
		t.Fatalf("no diagram: %v", err)
	}
	if got := l.publishLines(); len(got) != 0 {
		t.Errorf("a render with no pane published: %q", got)
	}
}

// TestAFailingPublishStillExitsAndIsRetried is the publish's own bookkeeping: the
// diagram is the deliverable, so a failed aeye call never fails the render, but a
// failure is not recorded as a success and the next pass retries it — and a failure
// that follows a success clears the record that success left.
func TestAFailingPublishStillExitsAndIsRetried(t *testing.T) {
	l := newLoop(t)
	l.seedBus(t)
	l.aeyeHelp = aeyePublishHelp
	l.ownPane(400)
	published := filepath.Join(l.cdir(), filePublished)
	failed := errors.New("aeye is not listening")
	count := func() int { return len(l.publishLines()) }

	l.publishErr = failed
	if code, stderr := l.once(Request{Pane: "%7"}); code != exitOK {
		t.Fatalf("code = %d, stderr %q", code, stderr)
	}
	if count() != 1 {
		t.Fatalf("%d publishes, want 1", count())
	}
	if got := record(l.cdir(), filePane); got != "%7" {
		t.Errorf("the pane record is %q", got)
	}
	if _, err := os.Stat(published); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a failed publish recorded a success: %v", err)
	}

	// The failure was not recorded, so an unchanged bus retries the publish.
	if code, stderr := l.once(Request{}); code != exitOK {
		t.Fatalf("code = %d, stderr %q", code, stderr)
	}
	if count() != 2 {
		t.Errorf("%d publishes, want the retry", count())
	}

	// A success records the pane, and a failure after it clears that record.
	l.publishErr = nil
	if code, stderr := l.once(Request{}); code != exitOK {
		t.Fatalf("code = %d, stderr %q", code, stderr)
	}
	if count() != 3 {
		t.Fatalf("%d publishes, want 3", count())
	}
	if got := record(l.cdir(), filePublished); got != "%7" {
		t.Errorf("the published record is %q", got)
	}

	l.publishErr = failed
	l.appendBus(t, statusEvent("worker:feat/1-a#s1", 1791361500000, "working", "execute: lint", "", ""))
	if code, stderr := l.once(Request{}); code != exitOK {
		t.Fatalf("code = %d, stderr %q", code, stderr)
	}
	if count() != 4 {
		t.Fatalf("%d publishes, want 4", count())
	}
	if _, err := os.Stat(published); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a failure left the success record in place: %v", err)
	}

	l.publishErr = nil
	if code, stderr := l.once(Request{}); code != exitOK {
		t.Fatalf("code = %d, stderr %q", code, stderr)
	}
	if count() != 5 {
		t.Errorf("%d publishes, want the retry of that failure", count())
	}
	if got := record(l.cdir(), filePublished); got != "%7" {
		t.Errorf("the published record is %q", got)
	}
}

// TestAPaneTheCallerCannotPlaceIsIgnored is the --pane guard, both ways it fails:
// an id that is not `%N`, and one whose shell is not this process's ancestor. The
// diagram is still written, nothing is published, and the refusal to take the pane
// is said out loud.
func TestAPaneTheCallerCannotPlaceIsIgnored(t *testing.T) {
	for _, tc := range []struct{ name, pane string }{
		{"not a pane id", "bogus"},
		{"a pane that is not this caller's", "%7"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := newLoop(t)
			l.seedBus(t)
			l.aeyeHelp = aeyePublishHelp
			// The pane names a live pid; it is just not one of ours.
			l.panePID = "400"
			l.opts.Proc = proctest.New(map[int]bool{400: true}, nil)

			code, stderr := l.once(Request{Pane: tc.pane})
			if code != exitOK {
				t.Fatalf("code = %d, stderr %q", code, stderr)
			}
			if _, err := os.Stat(l.diagramPath()); err != nil {
				t.Errorf("no diagram: %v", err)
			}
			if got := record(l.cdir(), filePane); got != "" {
				t.Errorf("an unusable pane was recorded: %q", got)
			}
			if got := l.publishLines(); len(got) != 0 {
				t.Errorf("an unusable pane published: %q", got)
			}
			want := "crew: roster-render: --pane '" + tc.pane + "' is not this caller's pane — ignored\n"
			if stderr != want {
				t.Errorf("stderr = %q, want %q", stderr, want)
			}
		})
	}
}

// TestASecondStartHandsTheRunningRendererItsPane is the hand-off in both halves.
// The newcomer finds the lock taken, so it leaves its pane in the record and exits
// silently; the renderer already running counts that record in its signature and
// publishes the unchanged diagram to the new pane.
func TestASecondStartHandsTheRunningRendererItsPane(t *testing.T) {
	t.Run("the newcomer retargets the record", func(t *testing.T) {
		l := newLoop(t)
		l.seedBus(t)
		l.aeyeHelp = aeyePublishHelp
		l.ownPane(400)
		// An incumbent renderer holds the crew: a live pid in the lock.
		lock := filepath.Join(l.cdir(), "roster-render.lock.d")
		if err := os.MkdirAll(lock, 0o755); err != nil {
			t.Fatal(err)
		}
		incumbent := strconv.Itoa(os.Getppid())
		if err := os.WriteFile(filepath.Join(lock, "pid"), []byte(incumbent+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		code, out := l.start(Request{Pane: "%9"})
		if code != exitOK {
			t.Fatalf("code = %d, stderr %q", code, out)
		}
		if got := record(l.cdir(), filePane); got != "%9" {
			t.Errorf("the running renderer was never retargeted: pane record %q", got)
		}
		if out != "" {
			t.Errorf("a start that found the lock was noisy: %q", out)
		}
		if got := l.lockOwner(); got != incumbent {
			t.Errorf("the newcomer took the incumbent's lock: %q", got)
		}
		if got := l.publishLines(); len(got) != 0 {
			t.Errorf("the newcomer published over the incumbent's shoulder: %q", got)
		}
	})

	t.Run("the running renderer picks it up", func(t *testing.T) {
		l := newLoop(t)
		// A drained crew, so the loop ends on its quiet window whatever happens.
		if err := os.WriteFile(l.paths.Log, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		l.aeyeHelp = aeyePublishHelp
		// The newcomer arrives between two passes: it writes the record and leaves.
		first := l.sleep
		l.opts.Probes.Sleep = func(ctx context.Context, d time.Duration) error {
			if l.slept == 1 {
				if err := os.WriteFile(filepath.Join(l.cdir(), filePane), []byte("%9\n"), 0o644); err != nil {
					t.Error(err)
				}
			}
			return first(ctx, d)
		}
		if code := l.run("5"); code != exitOK {
			t.Fatalf("code = %d", code)
		}
		want := []string{"aeye publish-diagram " + l.diagramPath() + " --pane %9 --open"}
		if got := l.publishLines(); strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("publishes = %q, want %q", got, want)
		}
	})
}

// TestDetachRecordsThePaneAndLeavesADaemonOnTheLock is --detach: the pane is
// validated and recorded here, the daemon is started with the arm's own argv and
// this crew's id, and the caller returns without taking the lock itself.
func TestDetachRecordsThePaneAndLeavesADaemonOnTheLock(t *testing.T) {
	l := newLoop(t)
	l.seedBus(t)
	l.ownPane(400)
	self := filepath.Join(t.TempDir(), "crew")
	if err := os.WriteFile(self, []byte("#!/usr/bin/env bash\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	l.opts.Self = self

	code, stderr := l.start(Request{Detach: true, Pane: "%7"})
	if code != exitOK {
		t.Fatalf("code = %d, stderr %q", code, stderr)
	}
	if got := record(l.cdir(), filePane); got != "%7" {
		t.Errorf("pane record = %q, want %%7", got)
	}
	if len(l.spawns) != 1 {
		t.Fatalf("%d children, want 1", len(l.spawns))
	}
	want := strings.Join([]string{self, "roster-render", "--crew", "c1", "--interval", "2", "--quiet", "1800"}, " ")
	if got := strings.Join(l.spawns[0], " "); got != want {
		t.Errorf("spawned %q, want %q", got, want)
	}
	if env := strings.Join(l.spawnEnv, "\n"); !strings.Contains(env, "CREW_ID=c1") {
		t.Errorf("the daemon was not told which crew it renders: %q", env)
	}
	if _, err := os.Stat(filepath.Join(l.cdir(), "roster-render.lock.d")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("--detach took the lock itself: %v", err)
	}
}

// stampOf is the file's mtime, the read the fixtures used to catch a rewrite: an
// unchanged pass leaves the epoch where `touch -d @1` put it.
func stampOf(t *testing.T, path string) string {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return strconv.FormatInt(st.ModTime().Unix(), 10)
}

// rewind is `touch -d @1`: any rewrite moves the mtime back off the epoch.
func rewind(t *testing.T, path string) string {
	t.Helper()
	if err := os.Chtimes(path, time.Unix(1, 0), time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	return stampOf(t, path)
}
