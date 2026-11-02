package stream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/clock"
	fleetpkg "github.com/noamsto/dispatcher/crew/internal/fleet"
	lanepkg "github.com/noamsto/dispatcher/crew/internal/lane"
	"github.com/noamsto/dispatcher/crew/internal/proc"
	"github.com/noamsto/dispatcher/crew/internal/repos"
)

const (
	ovHost = "testhost"
	ovMe   = "overseer:" + ovHost
	// ovStart is every overseer lane's start on its test clock.
	ovStart = int64(1_800_000_000_000)
	minute  = int64(60_000)
)

// msClock is a lane clock the test sets: the transition period, the misrouted
// window and the seed all read it, and nothing moves it but the test.
type msClock struct{ ms atomic.Int64 }

func newMSClock(ms int64) *msClock {
	c := &msClock{}
	c.ms.Store(ms)
	return c
}

func (c *msClock) Now() time.Time          { return time.UnixMilli(c.ms.Load()) }
func (c *msClock) advance(d time.Duration) { c.ms.Add(d.Milliseconds()) }

// ovRig is one overseer lane's surroundings: its state home, clock, the entry
// it must never re-enter, and the status documents it is fed in turn (the last
// one repeats).
type ovRig struct {
	t     *testing.T
	state string
	clock *msClock
	self  *stub
	docs  []fleetpkg.StatusDoc
	calls atomic.Int32
	flush func() error
	notes string
}

func newOvRig(t *testing.T) *ovRig {
	return &ovRig{t: t, state: t.TempDir(), clock: newMSClock(ovStart), self: entryStub(t, nil)}
}

func (r *ovRig) options(discover func(io.Writer) []repos.Repo) FleetOptions {
	return FleetOptions{
		Options:  Options{Clock: clock.Clock{Now: r.clock.Now}, Self: r.self.Script},
		Discover: discover,
		Getenv: func(k string) string {
			if k == "XDG_STATE_HOME" {
				return r.state
			}
			return ""
		},
		Host:  func() (string, error) { return ovHost, nil },
		Flush: r.flush,
		CollectStatus: func(_ context.Context, stderr io.Writer) fleetpkg.StatusDoc {
			n := int(r.calls.Add(1))
			if r.notes != "" {
				say(stderr, "%s", r.notes)
			}
			if len(r.docs) == 0 {
				return fleetpkg.StatusDoc{TS: r.clock.Now().UnixMilli(), Host: ovHost}
			}
			return r.docs[min(n, len(r.docs))-1]
		},
	}
}

func (r *ovRig) start(discover func(io.Writer) []repos.Repo) *fleetLane {
	r.t.Helper()
	l := startFleet(r.t, r.options(discover), FleetRequest{Overseer: true})
	r.t.Cleanup(func() {
		if r.self.has("argv.log") {
			r.t.Errorf("the overseer lane re-entered the crew entry: %q", r.self.argv())
		}
	})
	return l
}

func (r *ovRig) cursor(common string) string {
	return readFile(lanepkg.OverseerCursor(r.state, common))
}

func ovMsg(ts int64, crew, from, to, body string) string {
	return fmt.Sprintf(`{"ts":%d,"crew_id":%q,"from":%q,"to":%q,"kind":"msg","body":%q}`, ts, crew, from, to, body)
}

func ofEvent(lines []map[string]any, event string) []map[string]any {
	var out []map[string]any
	for _, l := range lines {
		if l["stream"] == "overseer" && l["event"] == event {
			out = append(out, l)
		}
	}
	return out
}

// tree lists every path under dir, relative to it.
func tree(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if rel, _ := filepath.Rel(dir, path); rel != "." {
			out = append(out, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestOverseerDeliversDispatcherMsgsOnceTaggedAndLeavesDispatcherCursor(t *testing.T) {
	alpha := gitRepo(t, "alpha")
	appendRow(t, alpha, statusRow(1000, "c1", "worker:feat/a", "working"))
	dispatcherCursor := filepath.Join(alpha, "crew", "crews", "c1", "cursor")
	mkdir(t, filepath.Dir(dispatcherCursor))
	writeFile(t, dispatcherCursor, "1000\n")

	r := newOvRig(t)
	l := r.start(okRepos(alpha))
	if got := r.cursor(alpha); got != fmt.Sprintf("%d\n", ovStart) {
		t.Fatalf("seeded cursor = %q, want the lane's start", got)
	}

	appendRow(t, alpha, ovMsg(ovStart+1000, "c1", "dispatcher:c1", ovMe, "ack 1"))
	appendRow(t, alpha, ovMsg(ovStart+2000, "c2", "dispatcher:c2", ovMe, "ack 2"))
	l.kick()
	l.kick()
	l.stop()

	batches := l.batches()
	if len(batches) != 1 {
		t.Fatalf("batches = %v, want one", batches)
	}
	if batches[0]["repo"] != "alpha" || batches[0]["cursor"] != float64(ovStart+2000) {
		t.Errorf("batch = %v, want repo alpha and cursor %d", batches[0], ovStart+2000)
	}
	if got := eventTS(batches[0]); !reflect.DeepEqual(got, []int64{ovStart + 1000, ovStart + 2000}) {
		t.Errorf("delivered %v, want both crews' msgs once", got)
	}
	if got := r.cursor(alpha); got != fmt.Sprintf("%d\n", ovStart+2000) {
		t.Errorf("overseer cursor = %q", got)
	}
	if got := readFile(dispatcherCursor); got != "1000\n" {
		t.Errorf("the dispatcher's cursor changed to %q", got)
	}
	if got := tree(t, filepath.Join(alpha, "crew", "crews")); !reflect.DeepEqual(got, []string{"c1", filepath.Join("c1", "cursor")}) {
		t.Errorf("crews/ holds %v, want only the dispatcher's own cursor", got)
	}
}

func TestOverseerReportsMisroutedSenderNeverBody(t *testing.T) {
	alpha := gitRepo(t, "alpha")
	appendRow(t, alpha, statusRow(1000, "c1", "worker:feat/a", "working"))
	r := newOvRig(t)
	l := r.start(okRepos(alpha))

	appendRow(t, alpha, ovMsg(ovStart+1000, "c1", "worker:feat/a#s1-1", ovMe, "BODY-SENTINEL wake up"))
	l.kick()
	l.kick()
	l.stop()

	if out := l.out.str(); strings.Contains(out, "BODY-SENTINEL") {
		t.Errorf("a misrouted body reached stdout: %s", out)
	}
	if b := l.batches(); len(b) != 0 {
		t.Errorf("a worker's msg to the overseer was delivered: %v", b)
	}
	lines := ofEvent(l.lines(), "misrouted_msg")
	if len(lines) != 1 {
		t.Fatalf("misrouted lines = %v, want one", lines)
	}
	want := map[string]any{"stream": "overseer", "event": "misrouted_msg", "repo": "alpha", "crew": "c1",
		"from": "worker:feat/a#s1-1", "misrouted": float64(1), "ts": float64(ovStart)}
	if !reflect.DeepEqual(lines[0], want) {
		t.Errorf("misrouted line = %v, want %v", lines[0], want)
	}
	if got := r.cursor(alpha); got != fmt.Sprintf("%d\n", ovStart+1000) {
		t.Errorf("cursor = %q: a misrouted msg must still move it", got)
	}
}

func TestOverseerMisroutedIsOneLinePerRepoPerInterval(t *testing.T) {
	alpha := gitRepo(t, "alpha")
	appendRow(t, alpha, statusRow(1000, "c1", "worker:feat/a", "working"))
	r := newOvRig(t)
	l := r.start(okRepos(alpha))

	post := func(from, to int) {
		for i := from; i < to; i++ {
			appendRow(t, alpha, ovMsg(ovStart+int64(i)+1, "c1", fmt.Sprintf("worker:feat/b%d#s%d-1", i, i), ovMe, "x"))
		}
	}
	post(0, 50)
	l.kick()
	if got := ofEvent(l.lines(), "misrouted_msg"); len(got) != 1 || got[0]["misrouted"] != float64(50) || got[0]["from"] != "worker:feat/b0#s0-1" {
		t.Fatalf("fifty senders in one poll = %v, want one line with misrouted 50 naming the first", got)
	}

	// --interval defaults to 60 s in overseer mode: 59 s on, the window is still shut.
	r.clock.advance(59 * time.Second)
	post(50, 53)
	l.kick()
	if got := ofEvent(l.lines(), "misrouted_msg"); len(got) != 1 {
		t.Fatalf("misrouted lines inside the window = %v, want still one", got)
	}
	r.clock.advance(time.Second)
	post(53, 54)
	l.kick()
	l.stop()
	got := ofEvent(l.lines(), "misrouted_msg")
	if len(got) != 2 || got[1]["misrouted"] != float64(54) || got[1]["from"] != "worker:feat/b53#s53-1" {
		t.Errorf("after the window = %v, want a second line with the running count 54", got)
	}
}

func TestOverseerRedeliversWhenFlushFails(t *testing.T) {
	alpha := gitRepo(t, "alpha")
	appendRow(t, alpha, statusRow(1000, "c1", "worker:feat/a", "working"))
	r := newOvRig(t)
	var atFlush []string
	r.flush = func() error {
		atFlush = append(atFlush, r.cursor(alpha))
		return errors.New("stdout closed")
	}
	first := r.start(okRepos(alpha))
	appendRow(t, alpha, ovMsg(ovStart+1000, "c1", "dispatcher:c1", ovMe, "ack"))
	first.kick()
	first.stop()

	seed := fmt.Sprintf("%d\n", ovStart)
	if len(atFlush) == 0 || atFlush[0] != seed {
		t.Fatalf("cursor at flush = %q, want the seed %q still in place", atFlush, seed)
	}
	if got := r.cursor(alpha); got != seed {
		t.Fatalf("cursor after a failed flush = %q, want %q", got, seed)
	}
	if got := first.delivered()["alpha"][ovStart+1000]; got == 0 {
		t.Fatalf("the batch was never printed: %q", first.out.str())
	}

	// The next lane is the restart after a kill between flush and cursor write.
	r.flush = nil
	r.clock.advance(time.Minute)
	second := r.start(okRepos(alpha))
	second.stop()
	if got := second.delivered()["alpha"][ovStart+1000]; got != 1 {
		t.Errorf("the restarted lane delivered the msg %d times, want once: %q", got, second.out.str())
	}
	if got := r.cursor(alpha); got != fmt.Sprintf("%d\n", ovStart+1000) {
		t.Errorf("cursor = %q after the redelivery", got)
	}
}

// A flush that failed leaves the log's stat unrecorded: the next poll of the
// same lane delivers the batch again though the log did not change.
func TestOverseerPollsAnUnchangedLogAgainAfterAFailedFlush(t *testing.T) {
	alpha := gitRepo(t, "alpha")
	appendRow(t, alpha, statusRow(1000, "c1", "worker:feat/a", "working"))
	r := newOvRig(t)
	failed := false
	r.flush = func() error {
		if failed {
			return nil
		}
		failed = true
		return errors.New("stdout closed")
	}
	l := r.start(okRepos(alpha))
	appendRow(t, alpha, ovMsg(ovStart+1000, "c1", "dispatcher:c1", ovMe, "ack"))
	l.kick()
	seed := fmt.Sprintf("%d\n", ovStart)
	if got := r.cursor(alpha); got != seed {
		t.Fatalf("cursor after a failed flush = %q, want %q", got, seed)
	}
	l.kick()
	l.kick()
	l.stop()
	if got := l.delivered()["alpha"][ovStart+1000]; got != 2 {
		t.Errorf("the msg was printed %d times, want twice (the failed flush, then the retry): %q", got, l.out.str())
	}
	if got := r.cursor(alpha); got != fmt.Sprintf("%d\n", ovStart+1000) {
		t.Errorf("cursor = %q after the retry", got)
	}
}

// A log whose size and mtime are those of the last completed poll is not read.
func TestOverseerSkipsAnUnchangedLog(t *testing.T) {
	alpha := gitRepo(t, "alpha")
	log := filepath.Join(alpha, "crew", "events.jsonl")
	appendRow(t, alpha, ovMsg(ovStart-1000, "c1", "dispatcher:c1", ovMe, "old"))
	r := newOvRig(t)
	l := r.start(okRepos(alpha))
	before, err := os.Stat(log)
	if err != nil {
		t.Fatal(err)
	}
	// Same size and mtime, a msg the lane has not seen: only a read finds it.
	if err := os.WriteFile(log, []byte(strings.Replace(readFile(log), fmt.Sprint(ovStart-1000), fmt.Sprint(ovStart+1000), 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(log, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	l.kick()
	if out := l.out.str(); out != "" {
		t.Fatalf("stdout = %q, want nothing: the log's stat was unchanged", out)
	}
	appendRow(t, alpha, ovMsg(ovStart+2000, "c1", "dispatcher:c1", ovMe, "new"))
	l.kick()
	l.stop()
	if got := l.delivered()["alpha"]; got[ovStart+1000] != 1 || got[ovStart+2000] != 1 {
		t.Errorf("delivered %v, want both once after the log changed", got)
	}
}

// Production leaves Flush nil, so the write's own error is the only guard: a
// batch that did not reach stdout leaves the cursor where it was.
func TestOverseerRedeliversWhenTheBatchWriteFails(t *testing.T) {
	alpha := gitRepo(t, "alpha")
	appendRow(t, alpha, statusRow(1000, "c1", "worker:feat/a", "working"))
	r := newOvRig(t)
	l := r.start(okRepos(alpha))
	var failed atomic.Bool
	l.out.failWhen(func(p []byte) bool {
		return bytes.Contains(p, []byte(`"events":`)) && failed.CompareAndSwap(false, true)
	})
	appendRow(t, alpha, ovMsg(ovStart+1000, "c1", "dispatcher:c1", ovMe, "ack"))
	l.kick()
	seed := fmt.Sprintf("%d\n", ovStart)
	if !failed.Load() || r.cursor(alpha) != seed {
		t.Fatalf("cursor after a failed write = %q (write failed: %v), want the seed %q", r.cursor(alpha), failed.Load(), seed)
	}
	l.kick()
	l.stop()
	if got := l.delivered()["alpha"][ovStart+1000]; got != 1 {
		t.Errorf("the msg reached stdout %d times, want once on the retry: %q", got, l.out.str())
	}
	if got := r.cursor(alpha); got != fmt.Sprintf("%d\n", ovStart+1000) {
		t.Errorf("cursor = %q after the retry", got)
	}
}

// A log the fold cannot read is an error line, and its stat stays unrecorded:
// mended in place, at the same size and mtime, it is folded on the next poll.
func TestOverseerRetriesALogItCannotFold(t *testing.T) {
	alpha := gitRepo(t, "alpha")
	log := filepath.Join(alpha, "crew", "events.jsonl")
	appendRow(t, alpha, statusRow(1000, "c1", "worker:feat/a", "working"))
	r := newOvRig(t)
	l := r.start(okRepos(alpha))
	appendRow(t, alpha, ovMsg(ovStart+1000, "c1", "dispatcher:c1", ovMe, "ack"))
	f, err := os.OpenFile(log, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{not json}\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	l.kick()
	l.kick()
	var errs []map[string]any
	for _, line := range l.lines() {
		if line["stream"] == "error" {
			errs = append(errs, line)
		}
	}
	if len(errs) != 1 || errs[0]["repo"] != "alpha" || errs[0]["crew"] != ovMe || !strings.HasPrefix(errs[0]["detail"].(string), "fold: ") {
		t.Fatalf("error lines = %v, want one fold: line for alpha", errs)
	}
	if got := r.cursor(alpha); got != fmt.Sprintf("%d\n", ovStart) {
		t.Fatalf("cursor = %q after a failed fold, want the seed", got)
	}

	before, err := os.Stat(log)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(log, []byte(strings.Replace(readFile(log), "{not json}", "          ", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(log, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	l.kick()
	l.stop()
	if got := l.delivered()["alpha"][ovStart+1000]; got != 1 {
		t.Errorf("the msg was delivered %d times once the log was mended, want once: %q", got, l.out.str())
	}
	if got := r.cursor(alpha); got != fmt.Sprintf("%d\n", ovStart+1000) {
		t.Errorf("cursor = %q after the mended fold", got)
	}
}

// TERM cuts a hung status read short, and the document it returns then is
// dropped: built under a cancelled context, it may read a failed probe as a
// cleared condition.
func TestOverseerStopsOnTermDuringAHungStatusRead(t *testing.T) {
	r := newOvRig(t)
	o := r.options(okRepos())
	w := ovWorker("worker:feat/a#s1-1", "working")
	entered := make(chan struct{}, 1)
	calls := 0
	o.CollectStatus = func(ctx context.Context, _ io.Writer) fleetpkg.StatusDoc {
		if calls++; calls == 1 {
			return ovDoc(ovStart, ovRepo("alpha", ovCrew("c1", "live", "attended", w)))
		}
		entered <- struct{}{}
		<-ctx.Done()
		return ovDoc(ovStart+minute, ovRepo("alpha", ovCrew("c1", "dead", "unattended", w)))
	}
	l := startFleet(t, o, FleetRequest{Overseer: true})
	r.clock.advance(time.Minute)
	select {
	case l.tick <- time.Now():
	case <-time.After(10 * time.Second):
		t.Fatal("the lane never took the tick")
	}
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the second status read never started")
	}
	start := time.Now()
	if rc := l.stop(); rc != 0 {
		t.Errorf("exit = %d, want 0; stderr %q", rc, l.errb.str())
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("the lane took %s to stop on TERM", took)
	}
	if got := ofEvent(l.lines(), "dispatcher_down"); len(got) != 0 {
		t.Errorf("a cancelled status read woke the overseer: %v", got)
	}
	for _, line := range l.lines() {
		if line["stream"] == "error" {
			t.Errorf("a status read TERM cut short printed an error line: %v", line)
		}
	}
}

// bareOverseer is an overseer with only what diff reads: its clock stands at
// ovStart, and collect is its status read.
func bareOverseer(out io.Writer, collect func(context.Context, io.Writer) fleetpkg.StatusDoc) *overseer {
	return &overseer{
		c:       call{crew: ovMe, heartbeat: defaultHeartbeat},
		o:       FleetOptions{Options: Options{Clock: clock.Clock{Now: newMSClock(ovStart).Now}}},
		out:     &laneWriter{w: out, now: time.Now},
		notes:   newNotes(io.Discard),
		sup:     &suppress{key: map[string]string{}, ts: map[string]int64{}},
		moves:   newTransitions(),
		collect: collect,
	}
}

// A status read gets its timeout at most. The document it returns past that is
// dropped with one error line per --heartbeat, and the next one in time still
// fires; a read that completes re-arms the error line.
func TestOverseerDiffDropsADocumentPastItsTimeout(t *testing.T) {
	var out lockedWriter
	w := ovWorker("worker:feat/a#s1-1", "working")
	hang := true
	v := bareOverseer(&out, func(ctx context.Context, _ io.Writer) fleetpkg.StatusDoc {
		if hang {
			<-ctx.Done()
		}
		return ovDoc(ovStart+minute, ovRepo("alpha", ovCrew("c1", "dead", "unattended", w)))
	})
	v.timeout = 10 * time.Millisecond
	v.moves.next(ovDoc(ovStart, ovRepo("alpha", ovCrew("c1", "live", "attended", w))), ovStart)
	v.diff(t.Context())
	want := map[string]any{"stream": "error", "crew": ovMe, "rc": float64(1),
		"detail": "status: no document within 10ms", "ts": float64(ovStart)}
	if got := decodeLinesT(t, out.str()); len(got) != 1 || !reflect.DeepEqual(got[0], want) {
		t.Fatalf("a timed-out status read printed %v, want [%v]", got, want)
	}
	v.diff(t.Context())
	if got := decodeLinesT(t, out.str()); len(got) != 1 {
		t.Fatalf("a second timed-out read within --heartbeat printed again: %q", out.str())
	}

	hang = false
	v.timeout = time.Minute
	v.diff(t.Context())
	if got := ofEvent(decodeLinesT(t, out.str()), "dispatcher_down"); len(got) != 1 {
		t.Errorf("the next read in time = %v, want one dispatcher_down", got)
	}
	hang = true
	v.timeout = 10 * time.Millisecond
	v.diff(t.Context())
	var errs int
	for _, line := range decodeLinesT(t, out.str()) {
		if line["stream"] == "error" {
			errs++
		}
	}
	if errs != 2 {
		t.Errorf("a timed-out read after a completed one gave %d error lines in all, want 2: %q", errs, out.str())
	}
}

// --interval paces the diffs, never a read's deadline: at --interval 1 a read
// slower than a second still completes.
func TestOverseerStatusDeadlineIsNotTheInterval(t *testing.T) {
	r := newOvRig(t)
	o := r.options(okRepos())
	left := make(chan time.Duration, 1)
	o.CollectStatus = func(ctx context.Context, _ io.Writer) fleetpkg.StatusDoc {
		deadline, _ := ctx.Deadline()
		select {
		case left <- time.Until(deadline):
		default:
		}
		return ovDoc(ovStart)
	}
	startFleet(t, o, FleetRequest{Request: Request{Interval: "1"}, Overseer: true})
	select {
	case got := <-left:
		if got <= 10*time.Second {
			t.Errorf("a status read at --interval 1 had %s left, want more than 10s", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the lane never read the status")
	}
}

// A read past its timeout is no baseline: the first that completes is, and it
// wakes nothing whatever it holds.
func TestOverseerFirstCompletedReadIsTheBaseline(t *testing.T) {
	var out lockedWriter
	w := ovWorker("worker:feat/a#s1-1", "working")
	hang := true
	v := bareOverseer(&out, func(ctx context.Context, _ io.Writer) fleetpkg.StatusDoc {
		if hang {
			<-ctx.Done()
			return ovDoc(ovStart, ovRepo("alpha", ovCrew("c1", "live", "attended", w)))
		}
		return ovDoc(ovStart+minute, ovRepo("alpha", ovCrew("c1", "dead", "unattended", w)))
	})
	v.timeout = 10 * time.Millisecond
	v.diff(t.Context())
	hang = false
	v.timeout = time.Minute
	v.diff(t.Context())
	for _, line := range decodeLinesT(t, out.str()) {
		if line["stream"] == "overseer" {
			t.Errorf("the first completed read woke the overseer: %v", line)
		}
	}
}

func TestOverseerSeedsMissingCursorAtStart(t *testing.T) {
	alpha := gitRepo(t, "alpha")
	appendRow(t, alpha, ovMsg(ovStart-1000, "c1", "dispatcher:c1", ovMe, "before the lane"))
	r := newOvRig(t)
	l := r.start(okRepos(alpha))
	if got := r.cursor(alpha); got != fmt.Sprintf("%d\n", ovStart) {
		t.Fatalf("cursor = %q, want the lane's start %d", got, ovStart)
	}
	appendRow(t, alpha, ovMsg(ovStart+500, "c1", "dispatcher:c1", ovMe, "after"))
	l.kick()
	l.stop()
	d := l.delivered()["alpha"]
	if d[ovStart-1000] != 0 || d[ovStart+500] != 1 {
		t.Errorf("delivered %v, want only the msg after the start", d)
	}
}

// Status rows and msgs to anyone else never wake the overseer: they are each
// crew's dispatcher's wakes, or sinks nobody waits on.
func TestOverseerIgnoresOtherRecipientsAndStatusRows(t *testing.T) {
	alpha := gitRepo(t, "alpha")
	appendRow(t, alpha, statusRow(1000, "c1", "worker:feat/a", "working"))
	r := newOvRig(t)
	l := r.start(okRepos(alpha))
	ts := ovStart
	for _, state := range []string{"working", "blocked", "pr_open", "done", "failed", "exited"} {
		ts++
		appendRow(t, alpha, statusRow(ts, "c1", "worker:feat/a", state))
	}
	for _, to := range []string{"dispatcher:c1", "worker:feat/a", "role:feat/a:reviewer", "metrics:c1", "retro:c1", "review:c1", "hold:c1", "*"} {
		ts++
		appendRow(t, alpha, ovMsg(ts, "c1", "dispatcher:c1", to, "x"))
	}
	l.kick()
	l.stop()
	if out := l.out.str(); out != "" {
		t.Errorf("stdout = %q, want nothing", out)
	}
	if got := r.cursor(alpha); got != fmt.Sprintf("%d\n", ovStart) {
		t.Errorf("cursor = %q, want the seed untouched", got)
	}
}

func TestOverseerLaneDiffsInjectedStatusEveryInterval(t *testing.T) {
	r := newOvRig(t)
	r.notes = "crew: fleet status: /gone/.git: skipped\n"
	r.docs = []fleetpkg.StatusDoc{
		ovDoc(ovStart, ovRepo("alpha", ovCrew("c1", "live", "attended", ovWorker("worker:feat/a#s1-1", "working")))),
		ovDoc(ovStart+minute, ovRepo("alpha", ovCrew("c1", "dead", "unattended", ovWorker("worker:feat/a#s1-1", "working")))),
	}
	l := r.start(okRepos())
	if n := r.calls.Load(); n != 1 {
		t.Fatalf("status computed %d times at start, want the baseline once", n)
	}
	l.kick()
	if n := r.calls.Load(); n != 1 {
		t.Fatalf("status computed %d times before --interval passed", n)
	}
	r.clock.advance(time.Minute)
	l.kick()
	l.kick()
	l.stop()
	got := ofEvent(l.lines(), "dispatcher_down")
	want := map[string]any{"stream": "overseer", "event": "dispatcher_down", "repo": "o/alpha", "crew": "c1",
		"state": "dead", "ts": float64(ovStart + minute)}
	if len(got) != 1 || !reflect.DeepEqual(got[0], want) {
		t.Errorf("dispatcher_down lines = %v, want [%v]", got, want)
	}
	if n := strings.Count(l.errb.str(), r.notes); n != 1 {
		t.Errorf("status note said %d times, want once: %q", n, l.errb.str())
	}
}

func TestOverseerAndCrewAreExclusive(t *testing.T) {
	var out, errb bytes.Buffer
	r := newOvRig(t)
	rc := RunFleet(FleetRequest{Request: Request{Crew: "c1"}, Overseer: true}, &out, &errb, r.options(okRepos()))
	if rc != exitUsage || errb.String() != "crew: fleet stream: --overseer and --crew are exclusive\n" || out.Len() != 0 {
		t.Errorf("rc %d stdout %q stderr %q", rc, out.String(), errb.String())
	}
}

func TestOverseerHostFailureIsOneLine(t *testing.T) {
	var out, errb bytes.Buffer
	r := newOvRig(t)
	o := r.options(okRepos())
	o.LaneRoot = t.TempDir()
	o.Host = func() (string, error) { return "", errors.New("self name \"x y\"\nis not valid") }
	rc := RunFleet(FleetRequest{Overseer: true}, &out, &errb, o)
	if rc != exitUsage || errb.String() != "crew: fleet stream: host name: self name \"x y\" is not valid\n" || out.Len() != 0 {
		t.Errorf("rc %d stdout %q stderr %q", rc, out.String(), errb.String())
	}
}

func TestOverseerStatusAndForce(t *testing.T) {
	root := t.TempDir()
	r := newOvRig(t)
	runStatus := func(tbl proc.Table) (int, map[string]any) {
		t.Helper()
		var out, errb bytes.Buffer
		o := r.options(okRepos())
		o.LaneRoot = root
		o.Proc = tbl
		rc := RunFleet(FleetRequest{Request: Request{Status: true}, Overseer: true}, &out, &errb, o)
		lines := decodeLinesT(t, out.String())
		if len(lines) != 1 {
			t.Fatalf("--status printed %q, stderr %q", out.String(), errb.String())
		}
		return rc, lines[0]
	}
	if rc, line := runStatus(alive()); rc != 2 || line["state"] != "dead" || line["crew"] != ovMe {
		t.Errorf("missing lane = (%d, %v), want (2, dead, %s)", rc, line, ovMe)
	}

	lane := filepath.Join(root, ovMe)
	lockd := mkdir(t, filepath.Join(lane, "stream.lock.d"))
	holder := exec.Command("sleep", "60")
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { _ = holder.Wait(); close(exited) }()
	t.Cleanup(func() { _ = holder.Process.Kill() })
	writeFile(t, filepath.Join(lockd, "pid"), fmt.Sprintf("%d\n", holder.Process.Pid))
	writeFile(t, filepath.Join(lane, "stream.tick"), fmt.Sprintf(`{"pid":%d,"ts":1,"park":300}`, holder.Process.Pid))
	if rc, line := runStatus(alive(fmt.Sprint(holder.Process.Pid))); rc != 1 || line["state"] != "stale" {
		t.Fatalf("stale lane = (%d, %v), want (1, stale)", rc, line)
	}

	var out, errb lockedWriter
	done := make(chan int, 1)
	o := r.options(okRepos())
	o.LaneRoot = root
	go func() {
		done <- RunFleet(FleetRequest{Request: Request{Force: true}, Overseer: true}, &out, &errb, o)
	}()
	me := fmt.Sprint(os.Getpid())
	waitFor(t, "the reclaimed lane's tick", func() bool {
		return strings.Contains(readFile(filepath.Join(lane, "stream.tick")), `"pid":`+me)
	})
	select {
	case <-exited:
	case <-time.After(10 * time.Second):
		t.Error("--force never stopped the stale holder")
	}
	if got := strings.TrimSpace(readFile(filepath.Join(lockd, "pid"))); got != me {
		t.Errorf("lock holder = %q, want this process %s", got, me)
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

func TestSenderTextKeepsOnlyTheSenderCharset(t *testing.T) {
	for in, want := range map[string]string{
		"worker:feat/x#s1-1":     "worker:feat/x#s1-1",
		"dispatcher:a.b_c@h":     "dispatcher:a.b_c@h",
		"worker:x \"y\"\n\u2603": "worker:x??y???",
		"\xff":                   "?",
		strings.Repeat("a", 200): strings.Repeat("a", 120),
		strings.Repeat("é", 200): strings.Repeat("?", 120),
	} {
		if got := senderText(in); got != want {
			t.Errorf("senderText(%q) = %q, want %q", in, got, want)
		}
	}
}

// The transition differ, driven directly: conditions read the document's own
// ts, so successive documents are the clock.

func ovDoc(ts int64, rs ...fleetpkg.RepoStatus) fleetpkg.StatusDoc {
	return fleetpkg.StatusDoc{TS: ts, Host: ovHost, Queue: fleetpkg.QueueStatus{Demoted: []fleetpkg.QueueDemote{}, Parked: []fleetpkg.QueueParked{}}, Repos: rs}
}

func ovRepo(name string, crews ...fleetpkg.CrewStatus) fleetpkg.RepoStatus {
	return fleetpkg.RepoStatus{Repo: "o/" + name, Common: "/git/" + name + "/.git", Crews: crews}
}

func ovCrew(id, state, attention string, workers ...map[string]any) fleetpkg.CrewStatus {
	return fleetpkg.CrewStatus{CrewID: id, Dispatcher: fleetpkg.Dispatcher{State: state}, Attention: attention, Workers: workers}
}

func withLane(c fleetpkg.CrewStatus, lane string) fleetpkg.CrewStatus {
	c.Dispatcher.Lane = &lane
	return c
}

func ovWorker(from, state string) map[string]any {
	branch, _, _ := strings.Cut(strings.TrimPrefix(from, "worker:"), "#")
	return map[string]any{"from": from, "branch": branch, "state": state, "stale": false, "blocked": nil}
}

// blockedWorker is a blocked row as CollectStatus derives it: since an int64,
// budget_left_s an int or nil.
func blockedWorker(from string, since int64, budget any, source string, answered bool) map[string]any {
	w := ovWorker(from, "blocked")
	w["source"] = source
	w["blocked"] = map[string]any{"since": since, "cycle": nil, "budget_left_s": budget, "source": source, "answered": answered}
	return w
}

func withQueue(d fleetpkg.StatusDoc, q fleetpkg.QueueStatus, budget, weight int) fleetpkg.StatusDoc {
	if q.Demoted == nil {
		q.Demoted = []fleetpkg.QueueDemote{}
	}
	if q.Parked == nil {
		q.Parked = []fleetpkg.QueueParked{}
	}
	d.Queue = q
	d.Machine = &fleetpkg.LoadDoc{Budget: budget, Weight: weight}
	return d
}

// step is one evaluation, its lines decoded as a reader sees them.
func step(t *testing.T, tr *transitions, d fleetpkg.StatusDoc) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range tr.next(d, d.TS+1) {
		var m map[string]any
		if err := json.Unmarshal([]byte(encode(line)), &m); err != nil {
			t.Fatalf("line %v: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

// steps runs each document in turn and returns each one's events by name.
func steps(t *testing.T, docs ...fleetpkg.StatusDoc) [][]string {
	t.Helper()
	tr := newTransitions()
	var out [][]string
	for _, d := range docs {
		var names []string
		for _, l := range step(t, tr, d) {
			names = append(names, l["event"].(string))
		}
		out = append(out, names)
	}
	return out
}

func wantSteps(t *testing.T, got [][]string, want ...[]string) {
	t.Helper()
	norm := func(v [][]string) string { return fmt.Sprint(v) }
	if norm(got) != norm(want) {
		t.Errorf("events per document = %v, want %v", got, want)
	}
}

func none() []string               { return nil }
func evs(names ...string) []string { return names }

func TestTransitionsBaselineEmitsNothing(t *testing.T) {
	alarming := withQueue(ovDoc(ovStart+time.Hour.Milliseconds(),
		ovRepo("alpha",
			ovCrew("c1", "dead", "unattended", ovWorker("worker:feat/a#s1-1", "working")),
			withLane(ovCrew("c2", "live", "attended", blockedWorker("worker:feat/b#s1-1", ovStart, 600, "worker", false)), "dead"),
			ovCrew("c3", "live", "attended", func() map[string]any {
				w := ovWorker("worker:feat/c#s1-1", "failed")
				w["detail"] = "blocked, no dispatcher reply"
				return w
			}()),
		)),
		fleetpkg.QueueStatus{
			Head:    &fleetpkg.QueueHead{Repo: "o/alpha", CrewID: "c1", ID: "h1", WaitedS: 5000},
			Demoted: []fleetpkg.QueueDemote{{Repo: "o/alpha", CrewID: "c1", ID: "h2"}},
			Parked:  []fleetpkg.QueueParked{{Repo: "o/alpha", CrewID: "c1", ID: "h3", Reason: "oversize"}},
		}, 4, 9)
	later := alarming
	later.TS += minute
	wantSteps(t, steps(t, alarming, later), none(), none())
}

func TestTransitionsDispatcherDown(t *testing.T) {
	w := ovWorker("worker:feat/a#s1-1", "working")
	tr := newTransitions()
	step(t, tr, ovDoc(ovStart, ovRepo("alpha", ovCrew("c1", "live", "attended", w))))
	got := step(t, tr, ovDoc(ovStart+minute, ovRepo("alpha", ovCrew("c1", "dead", "unattended", w))))
	want := []map[string]any{{"stream": "overseer", "event": "dispatcher_down", "repo": "o/alpha", "crew": "c1",
		"state": "dead", "ts": float64(ovStart + minute + 1)}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("live → dead = %v, want %v", got, want)
	}
	if got := step(t, tr, ovDoc(ovStart+2*minute, ovRepo("alpha", ovCrew("c1", "dead", "unattended", w)))); len(got) != 0 {
		t.Errorf("still dead = %v, want nothing", got)
	}
}

func TestTransitionsNoDownForIdleCrew(t *testing.T) {
	wantSteps(t, steps(t,
		ovDoc(ovStart, ovRepo("alpha", ovCrew("c1", "live", "attended"))),
		ovDoc(ovStart+minute, ovRepo("alpha", ovCrew("c1", "dead", "idle"))),
		ovDoc(ovStart+2*minute, ovRepo("alpha", ovCrew("c1", "closed", "idle"))),
	), none(), none(), none())
}

// An idle crew that gains work again with no dispatcher is down again.
func TestTransitionsIdleTurningUnattendedIsDown(t *testing.T) {
	wantSteps(t, steps(t,
		ovDoc(ovStart, ovRepo("alpha", ovCrew("c1", "closed", "idle"))),
		ovDoc(ovStart+minute, ovRepo("alpha", ovCrew("c1", "closed", "unattended", ovWorker("worker:feat/a#s1-1", "working")))),
	), none(), evs("dispatcher_down"))
}

func TestTransitionsDispatcherUpOnlyAfterDown(t *testing.T) {
	w := ovWorker("worker:feat/a#s1-1", "working")
	repo := func(c1, c2 string) fleetpkg.RepoStatus {
		att := func(s string) string {
			if s == "live" {
				return "attended"
			}
			return "unattended"
		}
		return ovRepo("alpha", ovCrew("c1", c1, att(c1), w), ovCrew("c2", c2, att(c2), w))
	}
	tr := newTransitions()
	var got [][]map[string]any
	for i, d := range [][2]string{
		{"live", "dead"}, // baseline: c2 is already down, so it never gets an up
		{"dead", "live"},
		{"live", "live"},
		{"live", "live"},
		{"dead", "live"},
		{"live", "live"},
	} {
		got = append(got, step(t, tr, ovDoc(ovStart+int64(i)*minute, repo(d[0], d[1]))))
	}
	var names [][]string
	for _, lines := range got {
		var n []string
		for _, l := range lines {
			n = append(n, l["event"].(string)+" "+l["crew"].(string))
		}
		names = append(names, n)
	}
	wantSteps(t, names, none(), evs("dispatcher_down c1"), evs("dispatcher_up c1"), none(), evs("dispatcher_down c1"), evs("dispatcher_up c1"))
	if up := got[2][0]; up["repo"] != "o/alpha" || len(up) != 5 {
		t.Errorf("dispatcher_up line = %v, want stream, event, repo, crew and ts", up)
	}
}

func TestTransitionsLaneDeadAfterTenMinutes(t *testing.T) {
	blocked := blockedWorker("worker:feat/a#s1-1", ovStart, nil, "worker", true)
	at := func(m int64, lane string, w map[string]any) fleetpkg.StatusDoc {
		return ovDoc(ovStart+m*minute, ovRepo("alpha", withLane(ovCrew("c1", "live", "attended", w), lane)))
	}
	tr := newTransitions()
	var names [][]string
	var fired map[string]any
	for _, d := range []fleetpkg.StatusDoc{
		at(0, "dead", blocked), at(9, "dead", blocked), at(10, "dead", blocked), at(11, "dead", blocked),
		at(12, "alive", blocked), at(13, "dead", blocked), at(22, "dead", blocked), at(23, "dead", blocked),
	} {
		var n []string
		for _, l := range step(t, tr, d) {
			n = append(n, l["event"].(string))
			fired = l
		}
		names = append(names, n)
	}
	wantSteps(t, names, none(), none(), evs("lane_dead"), none(), none(), none(), none(), evs("lane_dead"))
	if fired["dead_s"] != float64(600) || fired["crew"] != "c1" || fired["repo"] != "o/alpha" {
		t.Errorf("lane_dead line = %v, want dead_s 600", fired)
	}

	stale := blockedWorker("worker:feat/a#s1-1", ovStart, nil, "worker", true)
	stale["stale"] = true
	working := ovWorker("worker:feat/a#s1-1", "working")
	for _, w := range []map[string]any{stale, working} {
		wantSteps(t, steps(t, at(0, "dead", w), at(10, "dead", w), at(20, "dead", w)), none(), none(), none())
	}
}

func TestTransitionsBlockedAgingByAgeThenBudget(t *testing.T) {
	for _, crew := range []struct{ state, attention string }{{"live", "attended"}, {"dead", "unattended"}} {
		at := func(m int64, budget any) fleetpkg.StatusDoc {
			return ovDoc(ovStart+m*minute, ovRepo("alpha",
				ovCrew("c1", crew.state, crew.attention, blockedWorker("worker:feat/a#s1-1", ovStart, budget, "worker", false))))
		}
		tr := newTransitions()
		var lines [][]map[string]any
		for _, d := range []fleetpkg.StatusDoc{at(0, nil), at(29, nil), at(30, nil), at(31, 1800), at(32, 1500), at(33, 1200)} {
			lines = append(lines, step(t, tr, d))
		}
		for i, wantN := range []int{0, 0, 1, 0, 1, 0} {
			if len(lines[i]) != wantN {
				t.Errorf("%s: document %d = %v, want %d lines", crew.attention, i, lines[i], wantN)
			}
		}
		if t.Failed() {
			continue
		}
		age := map[string]any{"stream": "overseer", "event": "blocked_aging", "repo": "o/alpha", "crew": "c1",
			"session": "worker:feat/a#s1-1", "branch": "feat/a", "trigger": "age", "since": float64(ovStart),
			"budget_left_s": nil, "ts": float64(ovStart + 30*minute + 1)}
		if !reflect.DeepEqual(lines[2][0], age) {
			t.Errorf("%s: age line = %v, want %v", crew.attention, lines[2][0], age)
		}
		if l := lines[4][0]; l["trigger"] != "budget" || l["budget_left_s"] != float64(1500) {
			t.Errorf("%s: budget line = %v", crew.attention, l)
		}
	}
}

func TestTransitionsWatchdogBlockOnlyInUnattendedCrew(t *testing.T) {
	at := func(m int64, state, attention, source string, answered bool) fleetpkg.StatusDoc {
		return ovDoc(ovStart+m*minute, ovRepo("alpha",
			ovCrew("c1", state, attention, blockedWorker("worker:feat/a#s1-1", ovStart, 600, source, answered))))
	}
	wantSteps(t, steps(t, at(0, "live", "attended", "watchdog", false), at(40, "live", "attended", "watchdog", false)),
		none(), none())
	wantSteps(t, steps(t, at(0, "dead", "unattended", "watchdog", false), at(40, "dead", "unattended", "watchdog", false)),
		none(), evs("blocked_aging"))
	// Once the dispatcher has answered, the wait is its own again.
	wantSteps(t, steps(t, at(0, "live", "attended", "worker", true), at(40, "live", "attended", "worker", true)),
		none(), none())
}

func TestTransitionsAbandoned(t *testing.T) {
	failed := func(detail string) map[string]any {
		w := ovWorker("worker:feat/a#s1-1", "failed")
		w["detail"] = detail
		return w
	}
	at := func(m int64, w map[string]any) fleetpkg.StatusDoc {
		return ovDoc(ovStart+m*minute, ovRepo("alpha", ovCrew("c1", "live", "attended", w)))
	}
	tr := newTransitions()
	step(t, tr, at(0, ovWorker("worker:feat/a#s1-1", "working")))
	got := step(t, tr, at(1, failed("blocked, no dispatcher reply")))
	want := []map[string]any{{"stream": "overseer", "event": "abandoned", "repo": "o/alpha", "crew": "c1",
		"session": "worker:feat/a#s1-1", "branch": "feat/a", "ts": float64(ovStart + minute + 1)}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("abandoned = %v, want %v", got, want)
	}
	if got := step(t, tr, at(2, failed("blocked, no dispatcher reply"))); len(got) != 0 {
		t.Errorf("still failed = %v, want nothing", got)
	}
	if got := step(t, tr, at(3, ovWorker("worker:feat/a#s1-1", "working"))); len(got) != 0 {
		t.Errorf("working again = %v, want nothing", got)
	}
	if got := step(t, tr, at(4, failed("blocked, no dispatcher reply"))); len(got) != 1 || got[0]["event"] != "abandoned" {
		t.Errorf("failed again = %v, want abandoned again", got)
	}
	wantSteps(t, steps(t, at(0, ovWorker("worker:feat/a#s1-1", "working")), at(1, failed("tests red"))), none(), none())
}

// A stamped lead pane can post any worker:<branch>#s<sid>: the session and
// branch reach a line in the sender charset only.
func TestTransitionsFilterWorkerText(t *testing.T) {
	from := "worker:feat/a\u202e#s1\u200b-1\u0085"
	failed := ovWorker(from+"x", "failed")
	failed["detail"] = "blocked, no dispatcher reply"
	at := func(m int64, ws ...map[string]any) fleetpkg.StatusDoc {
		return ovDoc(ovStart+m*minute, ovRepo("alpha", ovCrew("c1", "live", "attended", ws...)))
	}
	blocked := blockedWorker(from, ovStart, nil, "worker", false)
	tr := newTransitions()
	step(t, tr, at(0, blocked, ovWorker(from+"x", "working")))
	got := step(t, tr, at(30, blocked, failed))
	want := map[string]map[string]any{
		"abandoned":     {"session": "worker:feat/a?#s1?-1?x", "branch": "feat/a?"},
		"blocked_aging": {"session": "worker:feat/a?#s1?-1?", "branch": "feat/a?"},
	}
	if len(got) != 2 {
		t.Fatalf("lines = %v, want abandoned and blocked_aging", got)
	}
	for _, l := range got {
		w := want[l["event"].(string)]
		if l["session"] != w["session"] || l["branch"] != w["branch"] {
			t.Errorf("%s line = %v, want session %q branch %q", l["event"], l, w["session"], w["branch"])
		}
	}
}

func TestTransitionsFilterRepoAndQueueText(t *testing.T) {
	const dirty = "o/al\u202epha\u200b\u0085"
	const clean = "o/al?pha??"
	w := ovWorker("worker:feat/a#s1-1", "working")
	repo := ovRepo("alpha", ovCrew("c1", "live", "attended", w))
	repo.Repo = dirty
	tr := newTransitions()
	step(t, tr, ovDoc(ovStart, repo))
	repo.Crews = []fleetpkg.CrewStatus{ovCrew("c1", "dead", "unattended", w)}
	got := step(t, tr, ovDoc(ovStart+minute, repo))
	if len(got) != 1 || got[0]["repo"] != clean {
		t.Errorf("repo label = %v, want %q", got, clean)
	}

	q := fleetpkg.QueueStatus{Head: &fleetpkg.QueueHead{Repo: dirty, CrewID: "c\u202e1", ID: "h\u200b1", WaitedS: headWaitS}}
	tr = newTransitions()
	step(t, tr, withQueue(ovDoc(ovStart), fleetpkg.QueueStatus{}, 10, 5))
	got = step(t, tr, withQueue(ovDoc(ovStart+minute), q, 10, 5))
	if len(got) != 1 || got[0]["repo"] != clean || got[0]["crew"] != "c?1" || got[0]["hold"] != "h?1" {
		t.Errorf("capacity_stuck = %v, want repo %q crew c?1 hold h?1", got, clean)
	}
}

func TestFleetRepoNameFiltersOrigin(t *testing.T) {
	common := "/git/alpha/.git"
	o := FleetOptions{Git: origin(common, "https://github.com/acme/al\u202epha\u200b.git")}
	if got, want := o.repoName(common), "acme/al?pha?"; got != want {
		t.Errorf("repoName = %q, want %q", got, want)
	}
}

func TestTransitionsBlockedAgingRearms(t *testing.T) {
	at := func(m, since int64, answered bool) fleetpkg.StatusDoc {
		return ovDoc(ovStart+m*minute, ovRepo("alpha", ovCrew("c1", "live", "attended",
			blockedWorker("worker:feat/a#s1-1", ovStart+since*minute, nil, "worker", answered))))
	}
	wantSteps(t, steps(t, at(0, 0, false), at(30, 0, false), at(31, 0, true), at(32, 32, false), at(61, 32, false), at(62, 32, false)),
		none(), evs("blocked_aging"), none(), none(), none(), evs("blocked_aging"))
}

func TestTransitionsCapacityStuck(t *testing.T) {
	head := func(waited int64) *fleetpkg.QueueHead {
		return &fleetpkg.QueueHead{Repo: "o/alpha", CrewID: "c1", ID: "h1", WaitedS: waited}
	}
	demoted := func(ids ...string) []fleetpkg.QueueDemote {
		out := []fleetpkg.QueueDemote{}
		for _, id := range ids {
			out = append(out, fleetpkg.QueueDemote{Repo: "o/beta", CrewID: "c2", ID: id})
		}
		return out
	}
	parked := []fleetpkg.QueueParked{
		{Repo: "o/beta", CrewID: "c2", ID: "h4", Reason: "demoted"},
		{Repo: "o/beta", CrewID: "c2", ID: "h5", Reason: "oversize"},
		{Repo: "o/beta", CrewID: "c2", ID: "h6", Reason: "ineligible"},
	}
	at := func(m int64, q fleetpkg.QueueStatus, weight int) fleetpkg.StatusDoc {
		return withQueue(ovDoc(ovStart+m*minute), q, 10, weight)
	}
	tr := newTransitions()
	var got [][]map[string]any
	for _, d := range []fleetpkg.StatusDoc{
		at(0, fleetpkg.QueueStatus{Head: head(100)}, 5),
		at(1, fleetpkg.QueueStatus{Head: head(1800)}, 5),
		at(2, fleetpkg.QueueStatus{Head: head(1900), Demoted: demoted("h2")}, 5),
		at(3, fleetpkg.QueueStatus{Head: head(2000), Demoted: demoted("h2", "h3")}, 5),
		at(4, fleetpkg.QueueStatus{Head: head(2100), Demoted: demoted("h2", "h3"), Parked: parked}, 5),
		at(5, fleetpkg.QueueStatus{Head: head(2200), Demoted: demoted("h2", "h3"), Parked: parked}, 12),
		at(6, fleetpkg.QueueStatus{Head: head(2300), Demoted: demoted("h2", "h3"), Parked: parked}, 12),
		at(7, fleetpkg.QueueStatus{Head: head(100)}, 5),
		at(8, fleetpkg.QueueStatus{Head: head(1800)}, 5),
	} {
		got = append(got, step(t, tr, d))
	}
	var names [][]string
	for _, lines := range got {
		var n []string
		for _, l := range lines {
			n = append(n, l["cause"].(string)+" "+l["hold"].(string))
		}
		names = append(names, n)
	}
	wantSteps(t, names,
		none(),
		evs("head_wait h1"),
		evs("demoted h2"),
		evs("demoted h3"),
		evs("parked h4", "oversize h5"),
		evs("overshoot "),
		none(),
		none(),
		evs("head_wait h1"),
	)
	if t.Failed() {
		return
	}
	headWait := map[string]any{"stream": "overseer", "event": "capacity_stuck", "repo": "o/alpha", "crew": "c1",
		"host": ovHost, "cause": "head_wait", "hold": "h1", "waited_s": float64(1800), "ts": float64(ovStart + minute + 1)}
	if !reflect.DeepEqual(got[1][0], headWait) {
		t.Errorf("head_wait line = %v, want %v", got[1][0], headWait)
	}
	overshoot := map[string]any{"stream": "overseer", "event": "capacity_stuck", "repo": "", "crew": "",
		"host": ovHost, "cause": "overshoot", "hold": "", "ts": float64(ovStart + 5*minute + 1)}
	if !reflect.DeepEqual(got[5][0], overshoot) {
		t.Errorf("overshoot line = %v, want %v", got[5][0], overshoot)
	}
}

// causes runs each document in turn and returns its capacity_stuck lines as
// "<cause> <hold>".
func causes(t *testing.T, docs ...fleetpkg.StatusDoc) [][]string {
	t.Helper()
	tr := newTransitions()
	var out [][]string
	for _, d := range docs {
		var names []string
		for _, l := range step(t, tr, d) {
			names = append(names, l["cause"].(string)+" "+l["hold"].(string))
		}
		out = append(out, names)
	}
	return out
}

// One hold moving to another cause is a new condition.
func TestTransitionsCapacityCauseIsInTheKey(t *testing.T) {
	at := func(m int64, q fleetpkg.QueueStatus) fleetpkg.StatusDoc {
		return withQueue(ovDoc(ovStart+m*minute), q, 10, 5)
	}
	h1 := []fleetpkg.QueueDemote{{Repo: "o/alpha", CrewID: "c1", ID: "h1"}}
	wantSteps(t, causes(t,
		at(0, fleetpkg.QueueStatus{}),
		at(1, fleetpkg.QueueStatus{Demoted: h1}),
		at(2, fleetpkg.QueueStatus{Parked: []fleetpkg.QueueParked{{Repo: "o/alpha", CrewID: "c1", ID: "h1", Reason: "demoted"}}}),
	), none(), evs("demoted h1"), evs("parked h1"))

	head := func(waited int64) *fleetpkg.QueueHead {
		return &fleetpkg.QueueHead{Repo: "o/alpha", CrewID: "c1", ID: "h2", WaitedS: waited}
	}
	wantSteps(t, causes(t,
		at(0, fleetpkg.QueueStatus{Head: head(100)}),
		at(1, fleetpkg.QueueStatus{Head: head(1800)}),
		at(2, fleetpkg.QueueStatus{Head: head(1900), Demoted: []fleetpkg.QueueDemote{{Repo: "o/alpha", CrewID: "c1", ID: "h2"}}}),
	), none(), evs("head_wait h2"), evs("demoted h2"))
}

func TestTransitionsOvershootRearms(t *testing.T) {
	q := fleetpkg.QueueStatus{Demoted: []fleetpkg.QueueDemote{{Repo: "o/alpha", CrewID: "c1", ID: "h1"}}}
	at := func(m int64, weight int) fleetpkg.StatusDoc {
		return withQueue(ovDoc(ovStart+m*minute), q, 10, weight)
	}
	wantSteps(t, causes(t, at(0, 5), at(1, 12), at(2, 12), at(3, 5), at(4, 12)),
		none(), evs("overshoot "), none(), none(), evs("overshoot "))
}

func TestTransitionsCapacityNeverWithoutBudgetOrQueue(t *testing.T) {
	q := fleetpkg.QueueStatus{Head: &fleetpkg.QueueHead{Repo: "o/alpha", CrewID: "c1", ID: "h1", WaitedS: 10}}
	wantSteps(t, steps(t,
		withQueue(ovDoc(ovStart), q, 0, 0),
		withQueue(ovDoc(ovStart+minute), q, 0, 8),
	), none(), none())
	wantSteps(t, steps(t,
		withQueue(ovDoc(ovStart), fleetpkg.QueueStatus{}, 4, 2),
		withQueue(ovDoc(ovStart+minute), fleetpkg.QueueStatus{}, 4, 9),
	), none(), none())
}

func TestTransitionsSkippedRepoKeepsItsKeys(t *testing.T) {
	w := ovWorker("worker:feat/a#s1-1", "working")
	blocked := blockedWorker("worker:feat/b#s1-1", ovStart, nil, "worker", true)
	at := func(m int64, c1 string) fleetpkg.StatusDoc {
		attention := "attended"
		if c1 != "live" {
			attention = "unattended"
		}
		return ovDoc(ovStart+m*minute, ovRepo("alpha",
			ovCrew("c1", c1, attention, w),
			withLane(ovCrew("c2", "live", "attended", blocked), "dead")))
	}
	skipped := func(m int64) fleetpkg.StatusDoc { return ovDoc(ovStart + m*minute) }
	tr := newTransitions()
	var names [][]string
	for _, d := range []fleetpkg.StatusDoc{at(0, "live"), at(1, "dead"), skipped(5), at(10, "dead")} {
		var n []string
		for _, l := range step(t, tr, d) {
			n = append(n, l["event"].(string)+" "+l["crew"].(string))
		}
		names = append(names, n)
	}
	// c1 stays down across the skip, so it is not down again; c2's lane has been
	// dead since the baseline, the skipped round included.
	wantSteps(t, names, none(), evs("dispatcher_down c1"), none(), evs("lane_dead c2"))
}

func TestTransitionsNeverWakeOnRawStatusRows(t *testing.T) {
	var docs []fleetpkg.StatusDoc
	for i, w := range []map[string]any{
		ovWorker("worker:feat/a#s1-1", "working"),
		blockedWorker("worker:feat/a#s1-1", ovStart+minute, 7200, "worker", false),
		ovWorker("worker:feat/a#s1-1", "pr_open"),
		ovWorker("worker:feat/a#s1-1", "done"),
		func() map[string]any {
			w := ovWorker("worker:feat/a#s1-1", "failed")
			w["detail"] = "tests red"
			return w
		}(),
		ovWorker("worker:feat/a#s1-1", "exited"),
	} {
		docs = append(docs, ovDoc(ovStart+int64(i)*minute, ovRepo("alpha", ovCrew("c1", "live", "attended", w))))
	}
	for i, lines := range steps(t, docs...) {
		if len(lines) != 0 {
			t.Errorf("document %d woke %v", i, lines)
		}
	}
}
