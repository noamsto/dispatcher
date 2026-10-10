// Package lock is crew.sh's `_lock_acquire`/`_lock_release` protocol, a copy
// rather than a reimplementation: an older installed `crew` and the bash arms
// that stayed there (`nudge`, `roster-render`, the autosweep spawner inside
// `reap`) take the same lock dirs, so the gate (mkdir), the owner file (`pid`)
// and the liveness probe (bare `kill -0`) have to agree byte-for-byte across the
// two languages.
//
// It backs every Go caller of that helper: `rate`'s `ratings.lock.d`, and
// `watch`'s `watch.lock.d` and `stream`'s `stream.lock.d`, each of which a
// process on the other side of a fork also reads.
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

// Holder is `$(cat "$dir/pid" 2>/dev/null || true)`: the owner text a refused
// acquisition reports, and the pid `stream --force` decides whether to signal.
func Holder(dir string) string { return holder(dir) }

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

// MaxPID is the highest pid the kernel will hand out: Linux's pid_max is
// capped at 4194304, and every other system this tool runs on is lower.
// A holder above it is not a process — and past int32 it is not even a
// signal target, because syscall.Kill truncates its pid to pid_t: 4294967295
// arrives at the kernel as -1, "every process this user may signal".
const MaxPID = 4194304

// ValidHolder reports whether a holder text can name a process: the arm's
// `case "$holder" in "" | *[!0-9]* | 0)` guard — digits only, not the bare
// "0" — plus the range a pid can actually be. The bare "0" is refused because
// that is what the arm refused to signal: `kill -TERM 0` is this process group.
func ValidHolder(text string) bool {
	_, ok := validHolder(text)
	return ok
}

func validHolder(text string) (int, bool) {
	if text == "" || text == "0" || !isDigits(text) {
		return 0, false
	}
	n, err := strconv.ParseInt(text, 10, 64)
	if err != nil || n > MaxPID {
		return 0, false
	}
	return int(n), true
}

func isDigits(text string) bool {
	if text == "" {
		return false
	}
	for _, r := range text {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// pidAlive is a bare `kill -0`, not the roster's `_pid_alive`: a `0` holder
// must read as held (the process-group semantics `stream --force` pins) and a
// non-numeric, out-of-range or dead owner as free. Out-of-range is free because
// the probe would be truncated to pid_t and answer for a different process —
// 4294967295 is kill(-1), every process this user may signal — the values past
// its own range bash's kill refused outright. Lock holders are always this
// tool's own uid, so EPERM never enters and `err == nil` is the whole answer.
func pidAlive(text string) bool {
	pid, err := strconv.Atoi(text)
	if err != nil || pid > MaxPID {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}
