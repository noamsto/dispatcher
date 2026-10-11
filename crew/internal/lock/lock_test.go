package lock

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestLockAcquireRelease(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "ratings.lock.d")

	if !Acquire(dir, "12345") {
		t.Fatal("first acquire refused")
	}
	pid, err := os.ReadFile(filepath.Join(dir, "pid"))
	if err != nil || string(pid) != "12345\n" {
		t.Errorf("pid file = %q, %v, want \"12345\\n\"", pid, err)
	}
	assertNames(t, dir, "flock", "pid")
	assertNames(t, parent, "ratings.lock.d", "ratings.lock.d.flock")
	// The same owner is an idempotent success, not a refusal.
	if !Acquire(dir, "12345") {
		t.Error("re-acquire by the owner refused; the helper returns 0 idempotently")
	}
	Release(dir)
	assertNames(t, parent)
	// A double release is a no-op.
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
	Release(dir)
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
			defer Release(dir)
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
			got := Acquire(dir, "12345")
			defer Release(dir)
			if got != tc.acquire {
				t.Fatalf("Acquire = %v, want %v", got, tc.acquire)
			}
			pid, _ := os.ReadFile(filepath.Join(dir, "pid"))
			if string(pid) != tc.wantPID {
				t.Errorf("pid file = %q, want %q", pid, tc.wantPID)
			}
		})
	}
}

// TestLockEmptyHolderInGraceIsHeld: an old-build acquirer writes its pid after
// mkdir, so an empty holder in a just-made dir is mid-write, not dead.
func TestLockEmptyHolderInGraceIsHeld(t *testing.T) {
	for _, tc := range []struct {
		name     string
		emptyPID bool
	}{
		{"no pid file yet", false},
		{"empty pid file", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parent := t.TempDir()
			dir := filepath.Join(parent, "ratings.lock.d")
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
					Release(dir)
				}
			}
			assertNames(t, parent, "ratings.lock.d")
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
		Release(dir)
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
		Release(dir)
	}
}

// TestLockOutOfRangeHolderIsFree: a digits-only holder past the pid ceiling
// names no process, and probing it answers for a different one — syscall.Kill
// truncates to pid_t, so 4294967295 is a kill(-1) that succeeds. The record is
// replaced instead, the refusal bash's own kill handed the arm.
func TestLockOutOfRangeHolderIsFree(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "stream.lock.d")
	me := strconv.Itoa(os.Getpid())
	for _, holder := range []string{"4194305", "4294967295", "9223372036854775807", "99999999999999999999999"} {
		if pidAlive(holder) {
			t.Errorf("pidAlive(%q) reads alive; past the pid ceiling it must read free", holder)
		}
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		writePID(t, dir, holder)
		if !Acquire(dir, me) {
			t.Errorf("Acquire kept a lock held by out-of-range pid %s", holder)
			if err := os.RemoveAll(dir); err != nil {
				t.Fatal(err)
			}
			continue
		}
		Release(dir)
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

// TestLockReclaimsBashDeadPidFile: a pid text with extra trailing newlines
// still compares equal to the judged holder, which Holder trims.
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
	defer Release(dir)
	got, _ := os.ReadFile(filepath.Join(dir, "pid"))
	if string(got) != "A\n" {
		t.Errorf("pid file = %q, want \"A\\n\"", got)
	}
}

// TestFlockProcessesNeverOverlap: processes hammering one lock, each proving
// its hold with an O_EXCL sentinel, never hold it at once.
func TestFlockProcessesNeverOverlap(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ratings.lock.d")
	var kids []*child
	for range 6 {
		kids = append(kids, spawn(t, "contend", dir))
	}
	total := 0
	for _, c := range kids {
		lines, err := c.finish()
		if err != nil {
			t.Errorf("child %s: %v, said %q", c.pid, err, lines)
		}
		for _, line := range lines {
			if strings.HasPrefix(line, "overlap") {
				t.Errorf("child %s: %s", c.pid, line)
			}
			if n, ok := strings.CutPrefix(line, "count "); ok {
				k, _ := strconv.Atoi(n)
				t.Logf("child %s acquired %d times", c.pid, k)
				total += k
			}
		}
	}
	if total == 0 {
		t.Error("no child ever acquired the lock")
	}
}

// TestFlockSIGKILLFreesLock: the kernel drops a killed holder's flock, and its
// marked record is replaced.
func TestFlockSIGKILLFreesLock(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "stream.lock.d")
	c := spawn(t, "hold", dir)
	c.expect(t, "held")
	if err := c.cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_, _ = c.finish()
	if !Acquire(dir, "A") {
		t.Fatal("a SIGKILLed holder's lock was refused")
	}
	defer Release(dir)
	if got := Holder(dir); got != "A" {
		t.Errorf("holder = %q, want A", got)
	}
}

// TestFlockMarkerRecordIsReplacedWithoutProbe: a marked record with a free
// flock is a gone holder even when its pid now names a live process (reuse).
func TestFlockMarkerRecordIsReplacedWithoutProbe(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "stream.lock.d")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	live := strconv.Itoa(os.Getppid()) + "\n"
	writePID(t, dir, live)
	if err := os.WriteFile(filepath.Join(dir, "flock"), []byte(live), 0o644); err != nil {
		t.Fatal(err)
	}
	if !Acquire(dir, "A") {
		t.Fatal("a marked record with a free flock was refused")
	}
	defer Release(dir)
	if got := Holder(dir); got != "A" {
		t.Errorf("holder = %q, want A", got)
	}
}

// TestFlockHolderStableUnderContention: refused contenders never disturb the
// record, so Holder names the holder on every read.
func TestFlockHolderStableUnderContention(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "stream.lock.d")
	c := spawn(t, "hold", dir)
	c.expect(t, "held")

	var stop atomic.Bool
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			owner := "c" + strconv.Itoa(i)
			for !stop.Load() {
				if Acquire(dir, owner) {
					t.Errorf("%s acquired a held lock", owner)
					Release(dir)
				}
			}
		})
	}
	reads := 0
	for deadline := time.Now().Add(300 * time.Millisecond); time.Now().Before(deadline); reads++ {
		if got := Holder(dir); got != c.pid {
			t.Errorf("read %d: holder = %q, want %s", reads, got, c.pid)
			break
		}
	}
	stop.Store(true)
	wg.Wait()
	c.stop(t)
}

func TestFlockInProcessOwners(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "stream.lock.d")
	if !Acquire(dir, "A") {
		t.Fatal("A refused on a free lock")
	}
	if !Acquire(dir, "A") {
		t.Error("A's re-acquire refused")
	}
	if Acquire(dir, "B") {
		t.Error("B acquired the lock A holds in this process")
	}
	Release(dir)
	if !Acquire(dir, "B") {
		t.Fatal("B refused after A released")
	}
	defer Release(dir)
	if got := Holder(dir); got != "B" {
		t.Errorf("holder = %q, want B", got)
	}
}

// TestFlockAdoptsOwnRecord: a same-pid exec hop finds its own record, written
// by either build, and adopts it.
func TestFlockAdoptsOwnRecord(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "roster-render.lock.d")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writePID(t, dir, "12345\n")
	if !Acquire(dir, "12345") {
		t.Fatal("the owner's own record was not adopted")
	}
	defer Release(dir)
	got, err := os.ReadFile(filepath.Join(dir, "flock"))
	if err != nil || string(got) != "12345\n" {
		t.Errorf("marker = %q, %v, want \"12345\\n\"", got, err)
	}
}

// TestFlockEmptyDirBeforePublishRefuses: an old-build acquirer's mkdir landing
// before the publish rename wins; nothing of ours is left behind.
func TestFlockEmptyDirBeforePublishRefuses(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "ratings.lock.d")
	hook(t, &beforePublish, func(string) {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Error(err)
		}
	})
	if Acquire(dir, "A") {
		t.Error("acquired over an old-build acquirer's fresh dir")
		Release(dir)
	}
	assertNames(t, parent, "ratings.lock.d")
	assertNames(t, dir)
}

// TestFlockRetiredRecordChangedIsRestored: an old-build acquirer published
// between the judgment and the retire; its record goes back untouched.
func TestFlockRetiredRecordChangedIsRestored(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "ratings.lock.d")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writePID(t, dir, deadPID(t))
	backdate(t, dir)
	live := strconv.Itoa(os.Getppid()) + "\n"
	hook(t, &judged, func(string) { writePID(t, dir, live) })

	if Acquire(dir, "A") {
		t.Error("acquired a record an old-build acquirer had just published")
		Release(dir)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "pid"))
	if string(got) != live {
		t.Errorf("pid file = %q, want the live %q restored", got, live)
	}
	assertNames(t, dir, "pid")
	assertNames(t, parent, "ratings.lock.d")
}

// TestFlockReleaseSkipsAnotherOwnersRecord: Release removes only its own
// record, and the record left behind is then judged as an old-build one.
func TestFlockReleaseSkipsAnotherOwnersRecord(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "ratings.lock.d")
	if !Acquire(dir, "A") {
		t.Fatal("A refused on a free lock")
	}
	writePID(t, dir, deadPID(t))
	Release(dir)
	assertNames(t, parent, "ratings.lock.d")

	if !Acquire(dir, "B") {
		t.Fatal("B refused a record whose pid is dead")
	}
	defer Release(dir)
	if got := Holder(dir); got != "B" {
		t.Errorf("holder = %q, want B", got)
	}
}

// TestFlockReleaseAfterLockFileReplaced: the crew dir was deleted and another
// process holds the re-created lock; the old holder's Release leaves it alone.
func TestFlockReleaseAfterLockFileReplaced(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "roster-render.lock.d")
	if !Acquire(dir, "A") {
		t.Fatal("A refused on a free lock")
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(dir + ".flock"); err != nil {
		t.Fatal(err)
	}
	c := spawn(t, "hold", dir)
	c.expect(t, "held")

	Release(dir)
	if got := Holder(dir); got != c.pid {
		t.Errorf("holder = %q, want the new holder %s", got, c.pid)
	}
	if _, err := os.Stat(dir + ".flock"); err != nil {
		t.Errorf("the new holder's lock file is gone: %v", err)
	}
	c.stop(t)
}

// TestFlockSweepsOldLitter: the old protocol's temp files beside the record and
// displaced pids inside a dead one are gone once the lock is taken.
func TestFlockSweepsOldLitter(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "ratings.lock.d")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	dead := deadPID(t) + "\n"
	writePID(t, dir, dead)
	if err := os.WriteFile(filepath.Join(dir, "pid.stale.x"), []byte(dead), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir+".new.abc", []byte(dead), 0o644); err != nil {
		t.Fatal(err)
	}
	backdate(t, dir)

	if !Acquire(dir, "A") {
		t.Fatal("a dead record with litter was refused")
	}
	defer Release(dir)
	assertNames(t, parent, "ratings.lock.d", "ratings.lock.d.flock")
	assertNames(t, dir, "flock", "pid")
}

const (
	helperEnv = "CREW_LOCK_HELPER"
	dirEnv    = "CREW_LOCK_DIR"
)

// TestHelperHolder is the child process the TestFlock* rows spawn; it does
// nothing unless helperEnv names its mode. "hold" acquires as its own pid,
// prints "held" and releases on stdin EOF or SIGTERM. "contend" loops on the
// lock for a second, proving each hold with an O_EXCL sentinel, and prints
// "count N" (or "overlap ..." and exits 1).
func TestHelperHolder(*testing.T) {
	mode, dir := os.Getenv(helperEnv), os.Getenv(dirEnv)
	me := strconv.Itoa(os.Getpid())
	switch mode {
	case "":
		return
	case "hold":
		if !Acquire(dir, me) {
			fmt.Println("refused")
			os.Exit(1)
		}
		stop := make(chan os.Signal, 2)
		signal.Notify(stop, syscall.SIGTERM)
		go func() {
			_, _ = io.Copy(io.Discard, os.Stdin)
			stop <- syscall.SIGTERM
		}()
		fmt.Println("held")
		<-stop
		Release(dir)
	case "contend":
		occupied := filepath.Join(filepath.Dir(dir), "occupied")
		count := 0
		for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
			if Acquire(dir, me) {
				f, err := os.OpenFile(occupied, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
				if err != nil {
					fmt.Println("overlap:", err)
					os.Exit(1)
				}
				_ = f.Close()
				time.Sleep(time.Millisecond)
				if err := os.Remove(occupied); err != nil {
					fmt.Println("overlap:", err)
					os.Exit(1)
				}
				Release(dir)
				count++
			}
			time.Sleep(100 * time.Microsecond)
		}
		fmt.Println("count", count)
	default:
		fmt.Println("unknown mode", mode)
		os.Exit(1)
	}
}

// child is a running TestHelperHolder process; the cleanup kills and reaps it,
// since a zombie still answers kill -0.
type child struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	lines chan string
	pid   string

	once sync.Once
	tail []string
	err  error
}

func spawn(t *testing.T, mode, dir string) *child {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperHolder$", "-test.timeout=30s")
	// A race-built child otherwise sleeps a second at exit.
	cmd.Env = append(os.Environ(), helperEnv+"="+mode, dirEnv+"="+dir, "GORACE="+os.Getenv("GORACE")+" atexit_sleep_ms=0")
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	c := &child{cmd: cmd, stdin: stdin, lines: make(chan string, 64), pid: strconv.Itoa(cmd.Process.Pid)}
	go func() {
		defer close(c.lines)
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			c.lines <- sc.Text()
		}
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = c.finish()
	})
	return c
}

func (c *child) expect(t *testing.T, want string) {
	t.Helper()
	select {
	case got, ok := <-c.lines:
		if !ok || got != want {
			t.Fatalf("child %s said %q (open %v), want %q", c.pid, got, ok, want)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("child %s never said %q", c.pid, want)
	}
}

// finish reads the child's remaining output and reaps it.
func (c *child) finish() ([]string, error) {
	c.once.Do(func() {
		for line := range c.lines {
			c.tail = append(c.tail, line)
		}
		c.err = c.cmd.Wait()
	})
	return c.tail, c.err
}

// stop closes the child's stdin, which makes a holder release and exit 0.
func (c *child) stop(t *testing.T) {
	t.Helper()
	_ = c.stdin.Close()
	if lines, err := c.finish(); err != nil {
		t.Errorf("child %s: %v, said %q", c.pid, err, lines)
	}
}

// hook installs fn in seam and restores the old seam on cleanup.
func hook(t *testing.T, seam *func(string), fn func(string)) {
	t.Helper()
	old := *seam
	*seam = fn
	t.Cleanup(func() { *seam = old })
}

// assertNames fails unless dir holds exactly want, which catches `.flock`,
// `.new` and `.stale` litter.
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
