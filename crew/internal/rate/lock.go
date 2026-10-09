// The ratings-store lock, a copy of crew.sh's `_lock_acquire`/`_lock_release`
// protocol rather than a reimplementation: an older installed `crew` and the
// autosweep spawner inside `reap` take the same `ratings.lock.d`, so the gate
// (mkdir), the owner file (`pid`) and the liveness probe (bare `kill -0`) have
// to agree byte-for-byte across the two languages.
package rate

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// lockAcquire is the helper: mkdir is the ONLY gate (atomic on POSIX), the
// pid file inside is for reclaim. It reports true when acquired or when the
// holder is this same pid (idempotent re-run), false when a different live pid
// holds it or the reclaim race was lost.
func lockAcquire(dir, owner string) bool {
	if tryLockDir(dir, owner) {
		return true
	}
	held := lockHolder(dir)
	if held == owner {
		return true
	}
	if held != "" && pidAlive(held) {
		return false
	}
	// Stale (owner PID dead or empty) — reclaim through the same mkdir gate.
	_ = os.RemoveAll(dir)
	return tryLockDir(dir, owner)
}

// lockRelease is `_lock_release`: an unconditional `rm -rf` with no owner
// check, which is why the sweep arms it only while it holds the lock.
func lockRelease(dir string) { _ = os.RemoveAll(dir) }

func tryLockDir(dir, owner string) bool {
	if err := os.Mkdir(dir, 0o755); err != nil {
		return false
	}
	_ = os.WriteFile(filepath.Join(dir, "pid"), []byte(owner+"\n"), 0o644)
	return true
}

// lockHolder is `$(cat "$ld/pid" 2>/dev/null || true)`: command substitution
// drops trailing newlines, and a missing or unreadable file is empty.
func lockHolder(dir string) string {
	data, err := os.ReadFile(filepath.Join(dir, "pid"))
	if err != nil {
		return ""
	}
	return strings.TrimRight(string(data), "\n")
}

// pidAlive is a bare `kill -0`, not the roster's `_pid_alive`: a `0` holder
// must read as held (the process-group semantics `stream --force` pins) and a
// non-numeric or dead owner as free. Lock holders are always this tool's own
// uid, so EPERM never enters and `err == nil` is the whole answer.
func pidAlive(text string) bool {
	pid, err := strconv.Atoi(text)
	if err != nil {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}
