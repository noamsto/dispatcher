// Package lock is crew.sh's `_lock_acquire`/`_lock_release` protocol, a copy
// rather than a reimplementation: an older installed `crew` and the bash arms
// that stayed there (`stream`, `nudge`, the autosweep spawner inside `reap`)
// take the same lock dirs, so the gate (mkdir), the owner file (`pid`) and the
// liveness probe (bare `kill -0`) have to agree byte-for-byte across the two
// languages.
//
// It backs every Go caller of that helper: `rate`'s `ratings.lock.d` and
// `watch`'s `watch.lock.d` (which bash `stream` also takes, through the child
// `crew watch` it re-enters).
package lock

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// Acquire is the helper: mkdir is the ONLY gate (atomic on POSIX), the pid file
// inside is for reclaim. Re-acquiring as the same owner is a success.
func Acquire(dir, owner string) bool {
	if tryDir(dir, owner) {
		return true
	}
	held := holder(dir)
	if held == owner {
		return true
	}
	if held != "" && pidAlive(held) {
		return false
	}
	// Stale (owner PID dead or empty) — reclaim through the same mkdir gate.
	_ = os.RemoveAll(dir)
	return tryDir(dir, owner)
}

// Release is `_lock_release`: an unconditional `rm -rf` with no owner check,
// which is why a caller arms it only while it holds the lock.
func Release(dir string) { _ = os.RemoveAll(dir) }

func tryDir(dir, owner string) bool {
	if err := os.Mkdir(dir, 0o755); err != nil {
		return false
	}
	_ = os.WriteFile(filepath.Join(dir, "pid"), []byte(owner+"\n"), 0o644)
	return true
}

// holder is `$(cat "$ld/pid" 2>/dev/null || true)`: command substitution drops
// trailing newlines, and a missing or unreadable file is empty.
func holder(dir string) string {
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
