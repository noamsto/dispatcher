package lock

import (
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestLockAcquireRelease(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ratings.lock.d")

	if !Acquire(dir, "12345") {
		t.Fatal("first acquire refused")
	}
	pid, err := os.ReadFile(filepath.Join(dir, "pid"))
	if err != nil || string(pid) != "12345\n" {
		t.Errorf("pid file = %q, %v, want \"12345\\n\"", pid, err)
	}
	// The same owner is an idempotent success, not a refusal.
	if !Acquire(dir, "12345") {
		t.Error("re-acquire by the owner refused; the helper returns 0 idempotently")
	}
	Release(dir)
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Error("release left the lock dir behind")
	}
	// Release is unconditional, so a double release is a no-op.
	Release(dir)
}

func TestLockHeldByLivePID(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ratings.lock.d")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// This test process is live, so the lock reads as held.
	writePID(t, dir, strconv.Itoa(os.Getpid()))
	if Acquire(dir, "99998") {
		t.Error("acquire succeeded against a live holder")
	}
	if _, err := os.Stat(filepath.Join(dir, "pid")); err != nil {
		t.Errorf("a refused acquire touched the holder's pid file: %v", err)
	}
	if Acquire(dir, strconv.Itoa(os.Getpid())) != true {
		t.Error("the live holder's own pid must re-acquire idempotently")
	}
}

func TestLockReclaimsDeadAndEmpty(t *testing.T) {
	for _, tc := range []struct{ name, held string }{
		{"dead pid", deadPID(t)},
		{"empty pid file", ""},
		{"garbage pid file", "not-a-pid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "ratings.lock.d")
			if err := os.Mkdir(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			writePID(t, dir, tc.held)
			backdate(t, dir)
			if !Acquire(dir, "12345") {
				t.Fatal("a stale lock was not reclaimed")
			}
			got, _ := os.ReadFile(filepath.Join(dir, "pid"))
			if string(got) != "12345\n" {
				t.Errorf("pid file after reclaim = %q, want \"12345\\n\"", got)
			}
		})
	}
}

// TestLockEmptyHolderFutureMtime: a slightly skewed clock still reads as
// mid-write, but a far-future mtime must not wedge the lock.
func TestLockEmptyHolderFutureMtime(t *testing.T) {
	for _, tc := range []struct {
		name    string
		offset  time.Duration
		acquire bool
		wantPID string
	}{
		{"1s ahead is held", time.Second, false, ""},
		{"10s ahead is reclaimed", 10 * time.Second, true, "12345\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "ratings.lock.d")
			if err := os.Mkdir(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			writePID(t, dir, "")
			future := time.Now().Add(tc.offset)
			if err := os.Chtimes(dir, future, future); err != nil {
				t.Fatal(err)
			}
			if got := Acquire(dir, "12345"); got != tc.acquire {
				t.Fatalf("Acquire = %v, want %v", got, tc.acquire)
			}
			got, _ := os.ReadFile(filepath.Join(dir, "pid"))
			if string(got) != tc.wantPID {
				t.Errorf("pid file = %q, want %q", got, tc.wantPID)
			}
		})
	}
}

// TestLockEmptyHolderInGraceIsHeld: the owner writes its pid after mkdir, so an
// empty holder in a just-made dir is an acquirer mid-write, not a dead one.
func TestLockEmptyHolderInGraceIsHeld(t *testing.T) {
	for _, tc := range []struct {
		name     string
		emptyPID bool
	}{
		{"no pid file yet", false},
		{"empty pid file", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "ratings.lock.d")
			if err := os.Mkdir(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if tc.emptyPID {
				writePID(t, dir, "")
			}
			const n = 8
			var ready, done sync.WaitGroup
			start := make(chan struct{})
			got := make([]bool, n)
			ready.Add(n)
			done.Add(n)
			for i := range n {
				go func() {
					defer done.Done()
					ready.Done()
					<-start
					got[i] = Acquire(dir, strconv.Itoa(20000+i))
				}()
			}
			ready.Wait()
			close(start)
			done.Wait()
			for i, ok := range got {
				if ok {
					t.Errorf("acquirer %d reclaimed a lock whose owner is mid-write", i)
				}
			}
			if _, err := os.Stat(dir); err != nil {
				t.Errorf("lock dir gone: %v", err)
			}
			data, err := os.ReadFile(filepath.Join(dir, "pid"))
			if tc.emptyPID && len(data) != 0 || !tc.emptyPID && !os.IsNotExist(err) {
				t.Errorf("pid file = %q, %v; a refused acquirer rewrote the holder", data, err)
			}
		})
	}
}

// TestLockReadsBashPidFile pins the cross-language part: the bash helper writes
// `printf '%s\n' "$owner"`, and an older `crew` holding the lock must read as
// held (or stale) here exactly as there.
func TestLockReadsBashPidFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ratings.lock.d")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writePID(t, dir, strconv.Itoa(os.Getpid())+"\n\n")
	if Acquire(dir, "12345") {
		t.Error("trailing newlines hid a live holder")
	}
}

// TestLockZeroHolderIsHeld pins the comment on the helper's `kill -0`: a `0`
// holder reads as held, because `kill -0 0` signals the process group.
func TestLockZeroHolderIsHeld(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ratings.lock.d")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writePID(t, dir, "0")
	if Acquire(dir, "12345") {
		t.Error("a `0` holder must read as held")
	}
}

// TestLockOutOfRangeHolderIsFree: a digits-only holder past the pid ceiling
// names no process, and probing it answers for a different one — syscall.Kill
// truncates to pid_t, so 4294967295 is a kill(-1) that succeeds. The lock is
// reclaimed instead, the refusal bash's own kill handed the arm.
func TestLockOutOfRangeHolderIsFree(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "stream.lock.d")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	me := strconv.Itoa(os.Getpid())
	for _, holder := range []string{"4194305", "4294967295", "9223372036854775807", "99999999999999999999999"} {
		if pidAlive(holder) {
			t.Errorf("pidAlive(%q) reads alive; past the pid ceiling it must read free", holder)
		}
		writePID(t, dir, holder)
		if !Acquire(dir, me) {
			t.Errorf("Acquire kept a lock held by out-of-range pid %s", holder)
		}
		Release(dir)
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, holder := range []string{"1", "4194304"} {
		if !ValidHolder(holder) {
			t.Errorf("ValidHolder(%q) = false; the ceiling itself is a pid", holder)
		}
	}
	for _, holder := range []string{"", "0", "-1", "12x", "1 ", " 1", "0x10", "4194305", "99999999999999999999999"} {
		if ValidHolder(holder) {
			t.Errorf("ValidHolder(%q) = true; the arm refused to signal this holder", holder)
		}
	}
}

// TestLockStaleReclaimRaceSingleHolder: B judges the lock stale and stalls
// before its destructive step; A reclaims and holds the lock; B then resumes.
// B must not destroy A's live lock.
//
// The owners are arbitrary text, so the lock A leaves behind is itself judged
// stale by B.
func TestLockStaleReclaimRaceSingleHolder(t *testing.T) {
	for _, tc := range []struct{ name, held string }{
		{"dead pid", deadPID(t)},
		{"empty pid file past the grace", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "ratings.lock.d")
			if err := os.Mkdir(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			writePID(t, dir, tc.held)
			backdate(t, dir)

			b := park(t, &staleJudged, "B")
			bGot := b.async(dir, "B")
			b.waitReached(t)

			aGot := Acquire(dir, "A")
			b.unpark()

			gotB := recv(t, bGot, "B")
			if !aGot {
				t.Error("A, the first reclaimer, was refused")
			}
			if gotB {
				t.Error("B acquired a lock A already holds")
			}
			got, _ := os.ReadFile(filepath.Join(dir, "pid"))
			if string(got) != "A\n" {
				t.Errorf("pid file = %q, want \"A\\n\"", got)
			}
		})
	}
}

// TestLockDisplacedLivePidIsRestored: the reclaimer judged a dead pid, but a
// live holder took the lock before its rename. It must put that pid back
// untouched and refuse, and leave no scratch files behind.
func TestLockDisplacedLivePidIsRestored(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "ratings.lock.d")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writePID(t, dir, deadPID(t))
	backdate(t, dir)

	live := strconv.Itoa(os.Getpid()) + "\n"
	hook(t, &staleJudged, func(string) { writePID(t, dir, live) })

	if Acquire(dir, "A") {
		t.Error("acquired a lock whose live holder it displaced")
	}
	got, _ := os.ReadFile(filepath.Join(dir, "pid"))
	if string(got) != live {
		t.Errorf("pid file = %q, want the displaced %q restored", got, live)
	}
	assertNames(t, dir, "pid")
	assertNames(t, parent, "ratings.lock.d")
}

// TestLockCrashedDirReclaimRace: a crashed acquirer left a dir with no pid.
// A stalls with its pid written but not linked; B and C then race in. Exactly
// one acquirer links, and A's late link must lose.
func TestLockCrashedDirReclaimRace(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "ratings.lock.d")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	backdate(t, dir)
	owners := liveOwners(t, 3)
	a, b, c := owners[0], owners[1], owners[2]

	pa := park(t, &beforeLink, a)
	aGot := pa.async(dir, a)
	pa.waitReached(t)

	if !Acquire(dir, b) {
		t.Error("B could not take the crashed dir")
	}
	if Acquire(dir, c) {
		t.Error("C acquired a lock B holds")
	}
	pa.unpark()
	if recv(t, aGot, "A") {
		t.Error("A's late link displaced B")
	}
	got, _ := os.ReadFile(filepath.Join(dir, "pid"))
	if string(got) != b+"\n" {
		t.Errorf("pid file = %q, want %q", got, b+"\n")
	}
	assertNames(t, dir, "pid")
	assertNames(t, parent, "ratings.lock.d")
}

// TestLockRenameLostToRivalBacksOff: a dead-pid judge whose rename finds the pid
// already moved is facing a rival mid-reclaim, and must not touch the dir.
func TestLockRenameLostToRivalBacksOff(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "ratings.lock.d")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writePID(t, dir, deadPID(t))
	backdate(t, dir)
	hook(t, &staleJudged, func(string) {
		if err := os.Remove(filepath.Join(dir, "pid")); err != nil {
			t.Error(err)
		}
	})

	if Acquire(dir, "A") {
		t.Error("acquired after losing the rename to a rival")
	}
	assertNames(t, dir)
	assertNames(t, parent, "ratings.lock.d")
}

// TestLockPidlessFreshRecheckRefuses: a dir judged stale with no pid file that
// is touched before the reclaim is a rival mid-write, not a crashed dir.
func TestLockPidlessFreshRecheckRefuses(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "ratings.lock.d")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	backdate(t, dir)
	hook(t, &staleJudged, func(string) {
		now := time.Now()
		if err := os.Chtimes(dir, now, now); err != nil {
			t.Error(err)
		}
	})

	if Acquire(dir, "A") {
		t.Error("acquired a dir that became fresh after the judgment")
	}
	assertNames(t, dir)
	assertNames(t, parent, "ratings.lock.d")
}

// TestLockPidlessReleasedMidReclaimIsAcquired: a dir released between the failed
// mkdir and the holder read is simply free.
func TestLockPidlessReleasedMidReclaimIsAcquired(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ratings.lock.d")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	backdate(t, dir)
	hook(t, &staleJudged, func(string) { Release(dir) })

	if !Acquire(dir, "A") {
		t.Fatal("a lock released mid-reclaim was refused")
	}
	if got := Holder(dir); got != "A" {
		t.Errorf("holder = %q, want A", got)
	}
}

// TestLockMultipleOrphansAreLeft: more than one pid.stale.* is not a lone
// displacement, so nothing is restored, removed or acquired.
func TestLockMultipleOrphansAreLeft(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ratings.lock.d")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"pid.stale.x", "pid.stale.y"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(deadPID(t)+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	backdate(t, dir)

	if Acquire(dir, "A") {
		t.Error("acquired a dir holding several orphaned displacements")
	}
	assertNames(t, dir, "pid.stale.x", "pid.stale.y")
}

// TestLockReclaimStress hammers each stale shape with concurrent acquirers whose
// every seam sleeps a random moment, and checks the lock never has two holders.
func TestLockReclaimStress(t *testing.T) {
	if testing.Short() {
		t.Skip("stress test")
	}
	const rounds, acquirers = 150, 6
	owners := liveOwners(t, acquirers)
	dead := deadPID(t)
	sleepy := func(string) { time.Sleep(time.Duration(rand.IntN(200)) * time.Microsecond) }
	hook(t, &staleJudged, sleepy)
	hook(t, &displaced, sleepy)
	hook(t, &beforeLink, sleepy)

	for _, tc := range []struct {
		name string
		seed func(t *testing.T, dir string)
	}{
		{"dead pid", func(t *testing.T, dir string) { writePID(t, dir, dead) }},
		{"empty pid file", func(t *testing.T, dir string) { writePID(t, dir, "") }},
		{"no pid file", func(*testing.T, string) {}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			for round := range rounds {
				dir := filepath.Join(root, strconv.Itoa(round))
				if err := os.Mkdir(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				tc.seed(t, dir)
				backdate(t, dir)

				var wg sync.WaitGroup
				var winners []string
				var mu sync.Mutex
				for _, owner := range owners {
					wg.Add(1)
					go func() {
						defer wg.Done()
						if Acquire(dir, owner) {
							mu.Lock()
							winners = append(winners, owner)
							mu.Unlock()
						}
					}()
				}
				wg.Wait()

				if len(winners) > 1 {
					t.Fatalf("round %d: %d holders %q", round, len(winners), winners)
				}
				if len(winners) == 1 {
					got, _ := os.ReadFile(filepath.Join(dir, "pid"))
					if string(got) != winners[0]+"\n" {
						t.Fatalf("round %d: pid file = %q, winner %q", round, got, winners[0])
					}
				}
			}
		})
	}
}

// TestLockOrphanedDisplacementIsRestored: a reclaimer that crashed between its
// rename and its delete leaves the dead holder's pid under pid.stale.*. The
// next acquirer restores it, and the one after reclaims it as an ordinary
// dead-pid dir.
func TestLockOrphanedDisplacementIsRestored(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ratings.lock.d")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	dead := deadPID(t) + "\n"
	if err := os.WriteFile(filepath.Join(dir, "pid.stale.x"), []byte(dead), 0o644); err != nil {
		t.Fatal(err)
	}
	backdate(t, dir)

	if Acquire(dir, "A") {
		t.Error("acquired a dir that held an orphaned displacement")
	}
	got, _ := os.ReadFile(filepath.Join(dir, "pid"))
	if string(got) != dead {
		t.Errorf("pid file = %q, want the orphan %q restored", got, dead)
	}
	assertNames(t, dir, "pid")

	if !Acquire(dir, "A") {
		t.Error("the restored dead pid was not reclaimed")
	}
	got, _ = os.ReadFile(filepath.Join(dir, "pid"))
	if string(got) != "A\n" {
		t.Errorf("pid file = %q, want \"A\\n\"", got)
	}
}

// TestLockPublishIsAtomic: between writing the temp file and linking it the
// pid file does not exist, so no reader or rival ever sees it empty or partial.
func TestLockPublishIsAtomic(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stale bool
		pid   bool
	}{
		{"fresh dir", false, false},
		{"reclaim", true, true},
		{"pidless dir", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parent := t.TempDir()
			dir := filepath.Join(parent, "ratings.lock.d")
			if tc.stale {
				if err := os.Mkdir(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				if tc.pid {
					writePID(t, dir, deadPID(t))
				}
				backdate(t, dir)
			}
			calls := 0
			hook(t, &beforeLink, func(string) {
				calls++
				if _, err := os.Stat(filepath.Join(dir, "pid")); !os.IsNotExist(err) {
					t.Errorf("pid file exists before the link: %v", err)
				}
			})
			if !Acquire(dir, "A") {
				t.Fatal("acquire refused")
			}
			if calls != 1 {
				t.Errorf("beforeLink ran %d times, want 1", calls)
			}
			got, _ := os.ReadFile(filepath.Join(dir, "pid"))
			if string(got) != "A\n" {
				t.Errorf("pid file = %q, want \"A\\n\"", got)
			}
			assertNames(t, dir, "pid")
			assertNames(t, parent, "ratings.lock.d")
		})
	}
}

// TestLockReclaimsBashDeadPidFile: a pid text with extra trailing newlines
// still compares equal to the judged holder, which holder() trims.
func TestLockReclaimsBashDeadPidFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ratings.lock.d")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writePID(t, dir, deadPID(t)+"\n\n")
	backdate(t, dir)
	if !Acquire(dir, "A") {
		t.Fatal("a dead pid with trailing newlines was not reclaimed")
	}
	got, _ := os.ReadFile(filepath.Join(dir, "pid"))
	if string(got) != "A\n" {
		t.Errorf("pid file = %q, want \"A\\n\"", got)
	}
}

// parkGuard bounds every wait on a parked acquirer, so a missed hook or a
// missed release fails the test instead of hanging it.
const parkGuard = 5 * time.Second

// parked is one owner stalled inside a test seam.
type parked struct {
	reached chan struct{}
	release chan struct{}
	reach   sync.Once
	free    sync.Once
	wg      sync.WaitGroup
}

// hook installs fn in seam before any goroutine starts and restores the old
// seam on cleanup.
func hook(t *testing.T, seam *func(string), fn func(string)) {
	t.Helper()
	old := *seam
	*seam = fn
	t.Cleanup(func() { *seam = old })
}

// park installs a hook in seam that blocks owner until unpark; other owners
// pass through. The cleanup frees the owner and waits for every goroutine
// started through async before the seam is restored.
func park(t *testing.T, seam *func(string), owner string) *parked {
	t.Helper()
	p := &parked{reached: make(chan struct{}), release: make(chan struct{})}
	hook(t, seam, func(o string) {
		if o != owner {
			return
		}
		p.reach.Do(func() { close(p.reached) })
		select {
		case <-p.release:
		case <-time.After(parkGuard):
		}
	})
	t.Cleanup(p.wg.Wait)
	t.Cleanup(p.unpark)
	return p
}

// async runs Acquire(dir, owner) in a goroutine the cleanup waits for.
func (p *parked) async(dir, owner string) <-chan bool {
	got := make(chan bool, 1)
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		got <- Acquire(dir, owner)
	}()
	return got
}

func (p *parked) waitReached(t *testing.T) {
	t.Helper()
	select {
	case <-p.reached:
	case <-time.After(parkGuard):
		t.Fatal("the parked acquirer never reached its seam")
	}
}

func (p *parked) unpark() { p.free.Do(func() { close(p.release) }) }

func recv(t *testing.T, ch <-chan bool, who string) bool {
	t.Helper()
	select {
	case got := <-ch:
		return got
	case <-time.After(parkGuard):
		t.Fatalf("%s never returned", who)
		return false
	}
}

// assertNames fails unless dir holds exactly want, which catches `.stale.` and
// `.new.` litter.
func assertNames(t *testing.T, dir string, want ...string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	if !slices.Equal(got, want) {
		t.Errorf("%s holds %q, want %q", dir, got, want)
	}
}

func writePID(t *testing.T, dir, text string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "pid"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// backdate ages dir past the empty-holder grace; call it after the last write
// into dir, which bumps the mtime.
func backdate(t *testing.T, dir string) {
	t.Helper()
	old := time.Now().Add(-10 * time.Second)
	if err := os.Chtimes(dir, old, old); err != nil {
		t.Fatal(err)
	}
}

// liveOwners starts n idle child processes and returns their pids as owner
// texts, so every owner reads as a live holder and none can be reclaimed.
func liveOwners(t *testing.T, n int) []string {
	t.Helper()
	var owners []string
	for range n {
		cmd := exec.Command("sleep", "600")
		if err := cmd.Start(); err != nil {
			t.Skipf("cannot spawn a child: %v", err)
		}
		t.Cleanup(func() {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		})
		owners = append(owners, strconv.Itoa(cmd.Process.Pid))
	}
	return owners
}

// deadPID returns a pid that has already exited. Recycling it to a live
// process during a test run would be a surprise on any shared host, but it is
// the honest version of "the holder is gone".
func deadPID(t *testing.T) string {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Skipf("cannot spawn a child: %v", err)
	}
	// cmd.Process is gone once Run returned; its pid is what a crashed sweep
	// would have left in the pid file.
	return strconv.Itoa(cmd.Process.Pid)
}
