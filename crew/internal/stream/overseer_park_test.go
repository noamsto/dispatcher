package stream

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	fleetpkg "github.com/noamsto/dispatcher/crew/internal/fleet"
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
	return startParkIn(t, r, discover, req, t.TempDir())
}

// startParkIn is startPark on a lane root the test picks. Two parks over one
// root share the lane's files, as two parks of one host do.
func startParkIn(t *testing.T, r *ovRig, discover func(io.Writer) []repos.Repo, req Request, root string) *parkLane {
	t.Helper()
	p := &parkLane{
		t:    t,
		out:  &lockedWriter{},
		errb: &lockedWriter{},
		tick: make(chan time.Time),
		done: make(chan int, 1),
	}
	fo := r.options(discover)
	fo.LaneRoot = root
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

// A partner host's transition wakes a park too: the bool federate returns
// reaches diff, so the park exits on that line rather than sleeping to its
// deadline. Its first document is the baseline, and the second one, past
// --interval, is the transition.
func TestOverseerParkWakesOnPartnerTransition(t *testing.T) {
	r := newOvRig(t)
	w := ovWorker("worker:feat/b#s1-1", "working")
	up := okHost("mbp", ovStart, ovRepo("beta", ovCrew("c9", "live", "attended", w)))
	down := okHost("mbp", ovStart+minute, ovRepo("beta", ovCrew("c9", "dead", "unattended", w)))
	r.docs = []fleetpkg.StatusDoc{withHosts(ovDoc(ovStart), up), withHosts(ovDoc(ovStart+minute), down)}

	p := startPark(t, r, okRepos(), Request{Park: "300"})
	waitFor(t, "the park's baseline document", func() bool { return p.waits.Load() >= 1 })
	if lines := decodeLinesT(t, p.out.str()); len(lines) != 0 {
		t.Fatalf("the baseline document printed %v, want nothing", lines)
	}

	r.clock.advance(time.Duration(defaultOverseerInterval) * time.Second)
	p.kick()
	if rc := p.wait(); rc != 0 {
		t.Fatalf("park exit = %d, want 0 on the partner transition; stderr %q", rc, p.errb.str())
	}
	got := ofEvent(decodeLinesT(t, p.out.str()), "dispatcher_down")
	if len(got) != 1 || got[0]["host"] != "mbp" || got[0]["crew"] != "c9" || got[0]["repo"] != "o/beta" {
		t.Fatalf("the park's partner transition = %v, want one dispatcher_down for mbp's c9", got)
	}
}

// A park is a fresh process, so the differ memory is the lane's own file: a
// condition off in one park and on in the next fires on the next park's first
// document instead of re-baselining it away.
func TestOverseerParkKeepsTheDifferAcrossParks(t *testing.T) {
	r := newOvRig(t)
	root := t.TempDir()
	r.docs = []fleetpkg.StatusDoc{ovDoc(ovStart, ovRepo("alpha", ovCrew("c1", "live", "unattended")))}

	first := startParkIn(t, r, okRepos(), Request{Park: "270"}, root)
	waitFor(t, "the first park's baseline document", func() bool { return first.waits.Load() >= 1 })
	if lines := decodeLinesT(t, first.out.str()); len(lines) != 0 {
		t.Fatalf("the baseline document printed %v, want nothing", lines)
	}
	r.clock.advance(271 * time.Second)
	first.kick()
	if rc := first.wait(); rc != 0 {
		t.Fatalf("first park exit = %d, want 0; stderr %q", rc, first.errb.str())
	}

	// The next park reads a document in which the same crew's dispatcher is no
	// longer live: a condition that turned true between the two parks.
	r.docs = []fleetpkg.StatusDoc{ovDoc(ovStart+minute, ovRepo("alpha", ovCrew("c1", "dead", "unattended")))}
	second := startParkIn(t, r, okRepos(), Request{Park: "270"}, root)
	if rc := second.wait(); rc != 0 {
		t.Fatalf("second park exit = %d, want 0 on the carried condition; stderr %q", rc, second.errb.str())
	}
	got := ofEvent(decodeLinesT(t, second.out.str()), "dispatcher_down")
	if len(got) != 1 || got[0]["repo"] != "o/alpha" || got[0]["crew"] != "c1" || got[0]["state"] != "dead" || got[0]["host"] != ovHost {
		t.Fatalf("the second park's first document = %v, want one dispatcher_down for alpha's c1", got)
	}
}

// The persistent lane never touches the differ file: without --park its memory
// is the process's own.
func TestOverseerPersistentLaneWritesNoDifferFile(t *testing.T) {
	r := newOvRig(t)
	r.docs = []fleetpkg.StatusDoc{ovDoc(ovStart, ovRepo("alpha", ovCrew("c1", "live", "unattended")))}
	root := t.TempDir()
	o := r.options(okRepos())
	o.LaneRoot = root
	var out, errb lockedWriter
	done := make(chan int, 1)
	go func() { done <- RunFleet(FleetRequest{Overseer: true}, &out, &errb, o) }()
	lane := filepath.Join(root, ovMe)
	waitFor(t, "the persistent lane's tick", func() bool {
		_, err := os.Stat(filepath.Join(lane, "stream.tick"))
		return err == nil
	})
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case rc := <-done:
		if rc != 0 {
			t.Errorf("exit = %d, want 0; stderr %q", rc, errb.str())
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the persistent lane did not stop on TERM")
	}
	if _, err := os.Stat(filepath.Join(lane, differName)); !os.IsNotExist(err) {
		t.Errorf("the persistent lane wrote %s (%v)", differName, err)
	}
}

// The differ memory's whole DTO is the park boundary's contract: the lane's
// seen flag, every condition key, its down crews and its lane_dead clock
// round-trip through snapshot/transitions, and a partner host carries its own
// moves, including the flattened deadEntry shape.
func TestDifferMovesRoundTripThroughJSON(t *testing.T) {
	tr := &transitions{
		seen: true,
		on: map[condKey]bool{
			{event: "abandoned", key: "k1", crew: "c1", id: "worker:feat/a#s1-1"}:                       true,
			{event: "blocked_aging", key: "k2", crew: "c2", id: "worker:feat/b#s2-1", kind: "budget"}:   true,
			{event: "capacity_stuck", kind: "overshoot", host: "h1", repo: "o/c", crew: "c3", id: "h9"}: true,
			{event: "lane_dead", key: "k4", crew: "c4"}:                                                 true,
		},
		down: map[crewRef]bool{
			{key: "k1", crew: "c1"}: true,
			{key: "k2", crew: "c2"}: true,
			{key: "k4", crew: "c4"}: true,
		},
		deadSince: map[crewRef]int64{
			{key: "k1", crew: "c9"}: ovStart,
			{key: "k2", crew: "c8"}: ovStart + minute,
			{key: "k5", crew: "c7"}: ovStart + 2*minute,
		},
	}
	back := tr.snapshot().transitions()
	if !back.seen {
		t.Error("seen did not round-trip")
	}
	if !reflect.DeepEqual(back.on, tr.on) {
		t.Errorf("on = %v, want %v", back.on, tr.on)
	}
	if !reflect.DeepEqual(back.down, tr.down) {
		t.Errorf("down = %v, want %v", back.down, tr.down)
	}
	if !reflect.DeepEqual(back.deadSince, tr.deadSince) {
		t.Errorf("deadSince = %v, want %v", back.deadSince, tr.deadSince)
	}

	data, err := json.Marshal(differFile{Version: differVersion,
		Partners: []hostMoves{{Host: "mbp", Seen: true, OK: true, Moves: tr.snapshot()}}})
	if err != nil {
		t.Fatalf("marshal differFile: %v", err)
	}
	var got differFile
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal differFile: %v", err)
	}
	if len(got.Partners) != 1 {
		t.Fatalf("partners = %v, want one", got.Partners)
	}
	if g := got.Partners[0]; g.Host != "mbp" || !g.Seen || !g.OK {
		t.Errorf("partner = %+v, want host mbp seen and ok", g)
	}
	if round := got.Partners[0].Moves.transitions(); !round.seen ||
		!reflect.DeepEqual(round.on, tr.on) || !reflect.DeepEqual(round.down, tr.down) || !reflect.DeepEqual(round.deadSince, tr.deadSince) {
		t.Errorf("partner moves = %+v, want the lane's own", round)
	}
	// The flattened deadEntry: refEntry's key/crew sit beside ts, not nested.
	if !strings.Contains(string(data), `"dead_since"`) || !strings.Contains(string(data), `"ts"`) {
		t.Errorf("differ JSON %s lacks the flattened dead_since entries", data)
	}
}

// The lane_dead clock and a partner host's memory survive a park boundary: park
// 2 resumes c1's dead age from park 1's baseline (dead_s 660, not 0) and still
// knows mbp was ok, so it reports the unreachable edge exactly once.
func TestOverseerParkKeepsDeadSinceAndPartnerAcrossParks(t *testing.T) {
	deadLane := func(ts int64) fleetpkg.StatusDoc {
		w := ovWorker("worker:feat/a#s1-1", "pr_open")
		return ovDoc(ts, ovRepo("alpha", withLane(ovCrew("c1", "live", "attended", w), "dead")))
	}
	r := newOvRig(t)
	root := t.TempDir()
	r.docs = []fleetpkg.StatusDoc{withHosts(deadLane(ovStart), okHost("mbp", ovStart))}

	first := startParkIn(t, r, okRepos(), Request{Park: "270"}, root)
	waitFor(t, "the first park's baseline document", func() bool { return first.waits.Load() >= 1 })
	if lines := ofEvent(decodeLinesT(t, first.out.str()), "lane_dead"); len(lines) != 0 {
		t.Fatalf("the baseline document fired lane_dead: %v", lines)
	}
	r.clock.advance(271 * time.Second)
	first.kick()
	if rc := first.wait(); rc != 0 {
		t.Fatalf("first park exit = %d, want 0; stderr %q", rc, first.errb.str())
	}

	r.docs = []fleetpkg.StatusDoc{withHosts(deadLane(ovStart+11*minute), downHost("mbp", ovStart, "ssh failed"))}
	second := startParkIn(t, r, okRepos(), Request{Park: "270"}, root)
	if rc := second.wait(); rc != 0 {
		t.Fatalf("second park exit = %d, want 0; stderr %q", rc, second.errb.str())
	}
	lines := decodeLinesT(t, second.out.str())
	dead := ofEvent(lines, "lane_dead")
	if len(dead) != 1 || dead[0]["crew"] != "c1" || dead[0]["repo"] != "o/alpha" || dead[0]["dead_s"] != float64(660) {
		t.Fatalf("lane_dead = %v, want one for alpha's c1 with dead_s 660", dead)
	}
	unreachable := ofEvent(lines, "host_unreachable")
	if len(unreachable) != 1 || unreachable[0]["host"] != "mbp" {
		t.Fatalf("host_unreachable = %v, want one for mbp", unreachable)
	}
}

// A differ file of an unknown version, or one that is not JSON at all, is no
// memory: the park re-baselines on its first document instead of carrying the
// down condition the file claims, then times out and still exits 0.
func TestOverseerParkRebaselinesOnABadDifferFile(t *testing.T) {
	for name, body := range map[string]string{
		"unknown version": `{"version":9,"lane":{"seen":true,"on":[]},"partners":[]}`,
		"not json":        `{not json`,
	} {
		t.Run(name, func(t *testing.T) {
			r := newOvRig(t)
			root := t.TempDir()
			lane := filepath.Join(root, ovMe)
			if err := os.MkdirAll(lane, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(lane, differName), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			r.docs = []fleetpkg.StatusDoc{ovDoc(ovStart, ovRepo("alpha", ovCrew("c1", "dead", "unattended")))}

			p := startParkIn(t, r, okRepos(), Request{Park: "270"}, root)
			waitFor(t, "the park's first document", func() bool { return p.waits.Load() >= 1 })
			if lines := ofEvent(decodeLinesT(t, p.out.str()), "dispatcher_down"); len(lines) != 0 {
				t.Fatalf("the bad differ file carried a condition: %v", lines)
			}
			r.clock.advance(271 * time.Second)
			p.kick()
			if rc := p.wait(); rc != 0 {
				t.Fatalf("park exit = %d, want 0; stderr %q", rc, p.errb.str())
			}
		})
	}
}

// A persistent lane ignores a differ file a park left in its lane: without
// --park its memory is the process's own, so its first document is a baseline
// even when the file claims the condition was already on.
func TestOverseerPersistentLaneIgnoresAPreExistingDifferFile(t *testing.T) {
	r := newOvRig(t)
	r.docs = []fleetpkg.StatusDoc{ovDoc(ovStart, ovRepo("alpha", ovCrew("c1", "dead", "unattended")))}
	root := t.TempDir()
	lane := filepath.Join(root, ovMe)
	if err := os.MkdirAll(lane, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lane, differName),
		[]byte(`{"version":1,"lane":{"seen":true,"on":[]},"partners":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	o := r.options(okRepos())
	o.LaneRoot = root
	reached := make(chan struct{}, 4)
	o.Rediscover = func(time.Duration) <-chan time.Time {
		select {
		case reached <- struct{}{}:
		default:
		}
		return make(chan time.Time)
	}
	var out, errb lockedWriter
	done := make(chan int, 1)
	go func() { done <- RunFleet(FleetRequest{Overseer: true}, &out, &errb, o) }()
	// The select is reached only after the first diff, so this handshake proves
	// the first document was folded before the stdout assertion.
	select {
	case <-reached:
	case <-time.After(20 * time.Second):
		t.Fatal("the persistent lane never diffed its first document")
	}
	if lines := ofEvent(decodeLinesT(t, out.str()), "dispatcher_down"); len(lines) != 0 {
		t.Errorf("the persistent lane printed %v for a pre-existing differ file", lines)
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
		t.Fatal("the persistent lane did not stop on TERM")
	}
}
