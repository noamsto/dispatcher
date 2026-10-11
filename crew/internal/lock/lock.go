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
	"errors"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Acquire is the helper: mkdir creates the dir and linking the pid file in is
// the gate (link refuses an existing pid), for a fresh lock and a reclaim alike.
// The pid file inside is for reclaim. Re-acquiring as the same owner is a
// success.
func Acquire(dir, owner string) bool {
	if tryDir(dir, owner) {
		return true
	}
	held, exists := readHolder(dir)
	if held == owner {
		return true
	}
	if held == "" && fresh(dir) {
		return false
	}
	if held != "" && pidAlive(held) {
		return false
	}
	// Stale (owner PID dead, or empty in a dir past the grace).
	staleJudged(owner)
	if !exists {
		return reclaimMissing(dir, owner)
	}
	return reclaim(dir, owner, held)
}

// Test seams, each called with the acquirer's owner: staleJudged after the
// stale judgment and before the reclaim, displaced between the displacing
// rename and its restore, beforeLink between the temp write and the link.
var (
	staleJudged = func(string) {}
	displaced   = func(string) {}
	beforeLink  = func(string) {}
)

// reclaim removes the holder judged stale without ever removing the lock dir
// out from under a rival: another acquirer may have judged the same holder, won
// the reclaim and be live by now. So the destructive step is a compare-and-swap
// on the pid file. Renaming it into the dir (exactly one rival can move a given
// file) lets us read what we took: the judged holder is deleted and replaced by
// our pid, anything else is someone's live pid and goes back. A rename that
// fails means a rival is mid-reclaim, so we back off.
func reclaim(dir, owner, judged string) bool {
	pid := filepath.Join(dir, "pid")
	moved := pid + ".stale." + owner + "." + nonce()
	if err := os.Rename(pid, moved); err != nil {
		return false
	}
	displaced(owner)
	data, err := os.ReadFile(moved)
	if err != nil {
		_ = os.Link(moved, pid)
		_ = os.Remove(moved)
		return false
	}
	if strings.TrimRight(string(data), "\n") != judged {
		// Link, not rename, so a pid published in the gap is never clobbered. The
		// rename bumped the dir mtime, so late judges refuse until this lands;
		// that is the grace assumption, not a proof (see docs/crew-go-port.md).
		_ = os.Link(moved, pid)
		_ = os.Remove(moved)
		return false
	}
	_ = os.Remove(moved)
	return publish(dir, owner)
}

// reclaimMissing handles a dir past the grace whose pid file did not exist when
// it was judged: a crashed acquirer, or a rival mid-displacement. There is no
// file to swap, and a pid published after the judgment must not be displaced, so
// it never renames and never removes the dir. A lone pid.stale.* is a rival's
// in-flight displacement or a crashed reclaimer's leftover: putting it back is
// always safe, and turns a wedged dir into an ordinary dead-pid dir for the next
// acquirer. With more than one nothing is guessed. Otherwise it links its pid
// into the existing dir, and link refuses if a rival got there first.
func reclaimMissing(dir, owner string) bool {
	if fresh(dir) {
		return false
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return tryDir(dir, owner) // released between our failed mkdir and the read
	}
	if err != nil {
		return false
	}
	var orphans []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "pid.stale.") {
			orphans = append(orphans, filepath.Join(dir, e.Name()))
		}
	}
	switch len(orphans) {
	case 0:
		return publish(dir, owner)
	case 1:
		_ = os.Link(orphans[0], filepath.Join(dir, "pid"))
		_ = os.Remove(orphans[0])
	}
	return false
}

// publish writes owner's pid so the pid file is never visible empty or partial:
// a sibling temp (outside dir, so a crash here leaves the dir truly empty) is
// linked into place, and link refuses if a pid is already there.
func publish(dir, owner string) bool {
	tmp := dir + ".new." + owner + "." + nonce()
	if err := os.WriteFile(tmp, []byte(owner+"\n"), 0o644); err != nil {
		return false
	}
	defer os.Remove(tmp)
	beforeLink(owner)
	return os.Link(tmp, filepath.Join(dir, "pid")) == nil
}

func nonce() string { return strconv.FormatUint(rand.Uint64(), 36) }

// emptyGrace is how long, in whole epoch seconds, a dir with no pid text may
// be a live acquirer: the owner writes its pid after mkdir, so a fresh empty
// holder is mid-write and reclaiming it would give two owners.
const emptyGrace = 2

// fresh reports whether dir's mtime is within emptyGrace of now, on either side
// so a skewed clock cannot wedge the lock. Wall clock, because the mtime is; a
// vanished dir is not fresh and falls through to the reclaim.
func fresh(dir string) bool {
	fi, err := os.Stat(dir)
	if err != nil {
		return false
	}
	age := time.Now().Unix() - fi.ModTime().Unix()
	return age > -emptyGrace && age < emptyGrace
}

// Release is `_lock_release`: an unconditional `rm -rf` with no owner check,
// which is why a caller arms it only while it holds the lock.
func Release(dir string) { _ = os.RemoveAll(dir) }

// Holder is `$(cat "$dir/pid" 2>/dev/null || true)`: the owner text a refused
// acquisition reports, and the pid `stream --force` decides whether to signal.
func Holder(dir string) string {
	held, _ := readHolder(dir)
	return held
}

func tryDir(dir, owner string) bool {
	if err := os.Mkdir(dir, 0o755); err != nil {
		return false
	}
	return publish(dir, owner)
}

// readHolder is `$(cat "$ld/pid" 2>/dev/null || true)`: command substitution
// drops trailing newlines, and an unreadable file is empty. exists tells a
// missing file from an empty one, which the reclaim treats differently.
func readHolder(dir string) (held string, exists bool) {
	data, err := os.ReadFile(filepath.Join(dir, "pid"))
	if err != nil {
		return "", !errors.Is(err, fs.ErrNotExist)
	}
	return strings.TrimRight(string(data), "\n"), true
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
