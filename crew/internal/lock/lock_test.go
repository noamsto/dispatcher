package lock

import (
	"os"
	"os/exec"
	"path/filepath"
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
