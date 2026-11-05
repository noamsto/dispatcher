package stream

import (
	"fmt"
	"io"
	"os"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/repos"
)

// parkLane is one `crew fleet stream --overseer --park S` in the background.
// Its Rediscover channel is the test's tick and every wait it arms is counted,
// so the test knows when the park has reached its select. Unlike fleetLane a
// park is expected to exit on its own: wait() is the subject, not an error.
type parkLane struct {
	t      *testing.T
	out    *lockedWriter
	errb   *lockedWriter
	tick   chan time.Time
	waits  atomic.Int32
	done   chan int
	exited atomic.Bool
}

// startPark runs one park over the rig's options and discover. It does not wait
// for a first iteration: a park that has a msg on its first poll exits before it
// ever arms a Rediscover wait, so a test that needs the select waits on waits
// itself.
func startPark(t *testing.T, r *ovRig, discover func(io.Writer) []repos.Repo, req Request) *parkLane {
	t.Helper()
	p := &parkLane{
		t:    t,
		out:  &lockedWriter{},
		errb: &lockedWriter{},
		tick: make(chan time.Time),
		done: make(chan int, 1),
	}
	fo := r.options(discover)
	fo.LaneRoot = t.TempDir()
	fo.Rediscover = func(time.Duration) <-chan time.Time {
		p.waits.Add(1)
		return p.tick
	}
	go func() { p.done <- RunFleet(FleetRequest{Overseer: true, Request: req}, p.out, p.errb, fo) }()
	t.Cleanup(p.stop)
	return p
}

// kick is one Rediscover tick, returning once the park has processed it or
// exited. A tick that trips the park deadline exits instead of arming another
// wait, so the test can only wait for "waits grew or done fired".
func (p *parkLane) kick() {
	p.t.Helper()
	n := p.waits.Load()
	select {
	case p.tick <- time.Now():
	case <-time.After(10 * time.Second):
		p.t.Fatal("the park never took the tick")
	}
	waitFor(p.t, "the kicked park iteration", func() bool { return p.waits.Load() > n || len(p.done) > 0 })
}

// wait is the park's own exit, the subject of every test.
func (p *parkLane) wait() int {
	p.t.Helper()
	select {
	case rc := <-p.done:
		p.exited.Store(true)
		return rc
	case <-time.After(20 * time.Second):
		p.t.Fatal("the park did not return")
		return -1
	}
}

// alive is a non-blocking check that the park has not exited yet.
func (p *parkLane) alive() bool { return len(p.done) == 0 }

// stop TERMs a park still running so a failed assert never strands it. A park
// that already exited is left alone.
func (p *parkLane) stop() {
	p.t.Helper()
	if p.exited.Load() {
		return
	}
	select {
	case <-p.done:
		p.exited.Store(true)
		return
	default:
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		p.t.Fatal(err)
	}
	select {
	case <-p.done:
		p.exited.Store(true)
	case <-time.After(20 * time.Second):
		p.t.Fatal("the park did not stop on TERM")
	}
}

// A park that finds a msg batch on its first poll prints it, moves that bus's
// cursor and exits 0 without polling the later bus: the run ends at the first
// non-heartbeat line, and beta's cursor stays at the lane start.
func TestOverseerParkReturnsOnFirstLine(t *testing.T) {
	alpha, beta := gitRepo(t, "alpha"), gitRepo(t, "beta")
	appendRow(t, alpha, ovMsg(ovStart+1000, "c1", "dispatcher:c1", ovMe, "alpha ack"))
	appendRow(t, beta, ovMsg(ovStart+2000, "c2", "dispatcher:c2", ovMe, "beta ack"))
	r := newOvRig(t)

	p := startPark(t, r, okRepos(alpha, beta), Request{Park: "300"})
	if rc := p.wait(); rc != 0 {
		t.Fatalf("park exit = %d, want 0; stderr %q", rc, p.errb.str())
	}
	lines := decodeLinesT(t, p.out.str())
	if len(lines) != 1 || lines[0]["repo"] != "alpha" {
		t.Fatalf("park stdout = %v, want exactly alpha's batch", lines)
	}
	if got := eventTS(lines[0]); len(got) != 1 || got[0] != ovStart+1000 {
		t.Errorf("alpha's events = %v, want its msg at %d", got, ovStart+1000)
	}
	if got := r.cursor(alpha); got != fmt.Sprintf("%d\n", ovStart+1000) {
		t.Errorf("alpha cursor = %q, want %d", got, ovStart+1000)
	}
	if got := r.cursor(beta); got != fmt.Sprintf("%d\n", ovStart) {
		t.Errorf("beta cursor = %q, want the seeded lane start %d", got, ovStart)
	}
}

// The first park stops before beta, so a second park re-delivers beta's batch
// and moves beta's cursor then: the msg it did not print was never skipped.
func TestOverseerParkSecondParkRedelivers(t *testing.T) {
	alpha, beta := gitRepo(t, "alpha"), gitRepo(t, "beta")
	appendRow(t, alpha, ovMsg(ovStart+1000, "c1", "dispatcher:c1", ovMe, "alpha ack"))
	appendRow(t, beta, ovMsg(ovStart+2000, "c2", "dispatcher:c2", ovMe, "beta ack"))
	r := newOvRig(t)

	first := startPark(t, r, okRepos(alpha, beta), Request{Park: "300"})
	if rc := first.wait(); rc != 0 {
		t.Fatalf("first park exit = %d, want 0; stderr %q", rc, first.errb.str())
	}
	for _, line := range decodeLinesT(t, first.out.str()) {
		if line["repo"] == "beta" {
			t.Fatalf("the first park delivered beta: %q", first.out.str())
		}
	}
	if got := r.cursor(beta); got != fmt.Sprintf("%d\n", ovStart) {
		t.Fatalf("beta cursor after the first park = %q, want the seed %d", got, ovStart)
	}

	second := startPark(t, r, okRepos(alpha, beta), Request{Park: "300"})
	if rc := second.wait(); rc != 0 {
		t.Fatalf("second park exit = %d, want 0; stderr %q", rc, second.errb.str())
	}
	lines := decodeLinesT(t, second.out.str())
	if len(lines) != 1 || lines[0]["repo"] != "beta" {
		t.Fatalf("second park stdout = %v, want exactly beta's batch", lines)
	}
	if got := eventTS(lines[0]); len(got) != 1 || got[0] != ovStart+2000 {
		t.Errorf("beta's events = %v, want its msg at %d", got, ovStart+2000)
	}
	if got := r.cursor(beta); got != fmt.Sprintf("%d\n", ovStart+2000) {
		t.Errorf("beta cursor = %q, want %d", got, ovStart+2000)
	}
}

// A park with nothing to deliver returns 0 once --park seconds have passed and
// prints no batch.
func TestOverseerParkTimesOutWithNothingDelivered(t *testing.T) {
	alpha := gitRepo(t, "alpha")
	r := newOvRig(t)

	p := startPark(t, r, okRepos(alpha), Request{Park: "270"})
	waitFor(t, "the park to reach its select", func() bool { return p.waits.Load() >= 1 })
	r.clock.advance(271 * time.Second)
	p.kick()
	if rc := p.wait(); rc != 0 {
		t.Fatalf("park exit = %d, want 0; stderr %q", rc, p.errb.str())
	}
	if lines := decodeLinesT(t, p.out.str()); len(lines) != 0 {
		t.Errorf("a park that timed out printed %v", lines)
	}
}

// A heartbeat is not a delivery: a park with --heartbeat 1 prints one and stays
// parked until its own deadline.
func TestOverseerParkHeartbeatDoesNotWake(t *testing.T) {
	r := newOvRig(t)

	p := startPark(t, r, okRepos(), Request{Park: "300", Heartbeat: "1"})
	waitFor(t, "the park to reach its select", func() bool { return p.waits.Load() >= 1 })
	r.clock.advance(2 * time.Second)
	p.kick()
	if !p.alive() {
		t.Fatalf("the park exited on a heartbeat; stdout %q", p.out.str())
	}
	lines := decodeLinesT(t, p.out.str())
	if len(lines) != 1 || lines[0]["stream"] != "heartbeat" {
		t.Fatalf("park stdout = %v, want only a heartbeat line", lines)
	}

	r.clock.advance(299 * time.Second)
	p.kick()
	if rc := p.wait(); rc != 0 {
		t.Fatalf("park exit = %d, want 0 after the deadline; stderr %q", rc, p.errb.str())
	}
}
