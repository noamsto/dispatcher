package stream

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
)

// layout2Lane is a stream for a layout-2 crew c1 resolved outside any repo.
type layout2Lane struct {
	cdir string
	s    *stub
	clk  *fakeClock
	done chan int
}

func startLayout2Lane(t *testing.T, reapEvery string) *layout2Lane {
	t.Helper()
	state := t.TempDir()
	if err := bus.CreateMeta(state, bus.Meta{ID: "c1", HomeHost: "h", CreatedBy: "register"}); err != nil {
		t.Fatal(err)
	}
	l := &layout2Lane{
		cdir: bus.StoreDir(state, "c1"),
		s: entryStub(t, map[string]string{
			"watch": `sleep 0.05; exit 0`,
			"reap":  `printf 'crew reap: reaped feat/x\n'`,
		}),
		clk:  &fakeClock{},
		done: make(chan int, 1),
	}
	r := Request{Crew: "c1", Park: "1", Interval: "1", Coalesce: "1", Retry: "1", Heartbeat: "3600", ReapEvery: reapEvery}
	go func() {
		l.done <- Run(r, bus.StatePaths(state), &bytes.Buffer{}, &bytes.Buffer{}, Options{
			Self: l.s.Script, Clock: clockNow(l.clk), peer: peerHooks{hosts: noPeers},
		})
	}()
	waitFor(t, "the lane's lock", func() bool { return exists(filepath.Join(l.cdir, "stream.lock.d")) })
	t.Cleanup(l.stop)
	return l
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func (l *layout2Lane) stop() {
	select {
	case <-l.done:
		return
	default:
	}
	_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
	select {
	case <-l.done:
	case <-time.After(10 * time.Second):
	}
}

func TestStreamLayout2OutsideARepoRunsNoReap(t *testing.T) {
	l := startLayout2Lane(t, "600")
	l.clk.advance(601 * time.Second)
	waitFor(t, "several watch iterations past the cadence", func() bool { return l.s.calls("watch") >= 6 })
	if got := l.s.calls("reap"); got != 0 {
		t.Errorf("a layout-2 lane outside a repo ran %d reaps", got)
	}
}

func TestStreamLayout2LaneHoldsItsCrewDirLock(t *testing.T) {
	l := startLayout2Lane(t, "0")
	if !exists(filepath.Join(l.cdir, "stream.lock.d")) {
		t.Fatal("no stream.lock.d in the layout-2 crew dir")
	}
}

func TestStreamLayout2SecondStreamIsRefused(t *testing.T) {
	state := t.TempDir()
	if err := bus.CreateMeta(state, bus.Meta{ID: "c1", HomeHost: "h", CreatedBy: "register"}); err != nil {
		t.Fatal(err)
	}
	lockd := filepath.Join(bus.StoreDir(state, "c1"), "stream.lock.d")
	holder := startForceChild(t, "hold", lockd, t.TempDir())
	if got := holder.next(t); got != "held" {
		t.Fatalf("the holder said %q", got)
	}
	var errb bytes.Buffer
	rc := Run(Request{Crew: "c1", Park: "1", Interval: "1"}, bus.StatePaths(state), &bytes.Buffer{}, &errb,
		Options{Self: entryStub(t, nil).Script})
	if rc != 1 || !strings.Contains(errb.String(), "another stream is already running for this crew (c1)") {
		t.Errorf("rc %d stderr %q, want the refusal", rc, errb.String())
	}
}
