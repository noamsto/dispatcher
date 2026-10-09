package rate

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
)

func TestLockAcquireRelease(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ratings.lock.d")

	if !lockAcquire(dir, "12345") {
		t.Fatal("first acquire refused")
	}
	pid, err := os.ReadFile(filepath.Join(dir, "pid"))
	if err != nil || string(pid) != "12345\n" {
		t.Errorf("pid file = %q, %v, want \"12345\\n\"", pid, err)
	}
	// The same owner is an idempotent success, not a refusal.
	if !lockAcquire(dir, "12345") {
		t.Error("re-acquire by the owner refused; the helper returns 0 idempotently")
	}
	lockRelease(dir)
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Error("release left the lock dir behind")
	}
	// Release is unconditional, so a double release is a no-op.
	lockRelease(dir)
}

func TestLockHeldByLivePID(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ratings.lock.d")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// This test process is live, so the lock reads as held.
	writePID(t, dir, strconv.Itoa(os.Getpid()))
	if lockAcquire(dir, "99998") {
		t.Error("acquire succeeded against a live holder")
	}
	if _, err := os.Stat(filepath.Join(dir, "pid")); err != nil {
		t.Errorf("a refused acquire touched the holder's pid file: %v", err)
	}
	if lockAcquire(dir, strconv.Itoa(os.Getpid())) != true {
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
			if !lockAcquire(dir, "12345") {
				t.Fatal("a stale lock was not reclaimed")
			}
			got, _ := os.ReadFile(filepath.Join(dir, "pid"))
			if string(got) != "12345\n" {
				t.Errorf("pid file after reclaim = %q, want \"12345\\n\"", got)
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
	if lockAcquire(dir, "12345") {
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
	if lockAcquire(dir, "12345") {
		t.Error("a `0` holder must read as held")
	}
}

func writePID(t *testing.T, dir, text string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "pid"), []byte(text), 0o644); err != nil {
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
