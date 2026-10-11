// Package lock is the crew's single-holder lock on a `*.d` path.
//
// Exclusion is flock(2) on the regular file `<dir>.flock`: the kernel drops it
// when its holder dies, so no acquirer ever has to judge whether a holder
// crashed. Who holds the lock is reported by a record, the dir `<dir>` with
// `pid` (the owner text) and `flock` (the same text, a marker that a flock
// holder wrote it). Only the flock holder writes, replaces or removes the
// record, and always by renaming a whole dir, so `<dir>` never exists without
// its `pid`.
//
// The record stays a dir with a `pid` because builds from before the flock run
// crew.sh's mkdir protocol on the same paths until they exit: their mkdir fails
// on it and they read a live pid, so they refuse. A record whose marker does
// not match its `pid` is one of theirs, and judging it by their rules (a live
// pid, or an empty one within the grace, is held) is a one-time compatibility
// path that closes when the last of those processes exits.
package lock

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type handle struct {
	f     *os.File
	owner string
}

// holds is this process's held locks: a second fd on the same file would be a
// separate flock, so the owner it was taken for is what answers a re-acquire.
var (
	mu    sync.Mutex
	holds = map[string]*handle{}
)

// Test seams, each called with the acquirer's owner: judged after a record is
// judged replaceable and before it is retired, beforePublish before the new
// record is renamed into place.
var (
	judged        = func(string) {}
	beforePublish = func(string) {}
)

// Acquire takes the lock for owner, or refuses at once: callers poll. In this
// process, re-acquiring as the owner already holding it is a success and any
// other owner is refused.
func Acquire(dir, owner string) bool {
	mu.Lock()
	defer mu.Unlock()
	if h, ok := holds[dir]; ok {
		return h.owner == owner
	}
	f := openLocked(dir + ".flock")
	if f == nil {
		return false
	}
	if !claim(dir, owner) {
		drop(dir, f, "")
		return false
	}
	holds[dir] = &handle{f: f, owner: owner}
	return true
}

// openLocked returns path opened and flocked, or nil if another holder has it.
// A releasing holder unlinks the path, so a flock won on an fd opened before
// that is on a file no later acquirer opens; it is closed (the path is someone
// else's now) and the open retried.
func openLocked(path string) *os.File {
	for range 8 {
		f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
		if err != nil {
			return nil
		}
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			_ = f.Close()
			return nil
		}
		if same(f, path) {
			return f
		}
		_ = f.Close()
	}
	return nil
}

func same(f *os.File, path string) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	pi, err := os.Stat(path)
	return err == nil && os.SameFile(fi, pi)
}

// claim publishes owner's record while holding the flock, replacing whatever
// record is there unless it is an old-build holder's.
func claim(dir, owner string) bool {
	sweep(dir)
	if _, err := os.Lstat(dir); err != nil {
		return errors.Is(err, fs.ErrNotExist) && publish(dir, owner)
	}
	pid := Holder(dir)
	if !replaceable(dir, owner, pid) {
		return false
	}
	judged(owner)
	return retire(dir, pid) && publish(dir, owner)
}

// replaceable judges the record of a lock whose flock is free. Owner's own pid
// is a same-pid exec hop. A matching marker is a flock holder that is gone, so
// there is no liveness probe for pid reuse to fool.
func replaceable(dir, owner, pid string) bool {
	if pid == owner {
		return true
	}
	if marker, err := os.ReadFile(filepath.Join(dir, "flock")); err == nil && strings.TrimRight(string(marker), "\n") == pid {
		return true
	}
	if pid != "" {
		return !pidAlive(pid)
	}
	return !fresh(dir)
}

// sweep removes the old protocol's `<dir>.new.*` temp files, which only a
// crashed old-build acquirer leaves behind for long.
func sweep(dir string) {
	parent, prefix := filepath.Dir(dir), filepath.Base(dir)+".new."
	entries, err := os.ReadDir(parent)
	if err != nil {
		return
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), prefix) {
			_ = os.RemoveAll(filepath.Join(parent, e.Name()))
		}
	}
}

// retire removes the record whose pid was judged. It renames before removing
// because old-build actors work on `<dir>/pid` by path: once `<dir>` is gone
// their in-flight steps fail. One may have published between the judgment and
// the rename, so the retired pid is re-read and a changed record goes back.
func retire(dir, judged string) bool {
	stale := dir + ".stale"
	_ = os.RemoveAll(stale)
	if err := os.Rename(dir, stale); err != nil {
		return errors.Is(err, fs.ErrNotExist)
	}
	if Holder(stale) != judged {
		_ = os.Rename(stale, dir)
		return false
	}
	_ = os.RemoveAll(stale)
	return true
}

// publish builds owner's record beside dir and renames it into place. Go's
// os.Rename refuses an existing dir target, so an old-build acquirer that made
// `<dir>` since the judgment wins and this refuses.
func publish(dir, owner string) bool {
	tmp := dir + ".new"
	_ = os.RemoveAll(tmp)
	if err := os.Mkdir(tmp, 0o755); err != nil {
		return false
	}
	for _, name := range []string{"pid", "flock"} {
		if err := os.WriteFile(filepath.Join(tmp, name), []byte(owner+"\n"), 0o644); err != nil {
			_ = os.RemoveAll(tmp)
			return false
		}
	}
	beforePublish(owner)
	if err := os.Rename(tmp, dir); err != nil {
		_ = os.RemoveAll(tmp)
		return false
	}
	return true
}

// drop gives up the flock taken through f. While dir+".flock" is still the
// file f locked, owner's record is retired first, so a new acquirer never sees
// it without the flock behind it, and then the path goes; an empty owner leaves
// the record. Closing f frees the flock either way.
func drop(dir string, f *os.File, owner string) {
	if same(f, dir+".flock") {
		if owner != "" && Holder(dir) == owner {
			retire(dir, owner)
		}
		_ = os.Remove(dir + ".flock")
	}
	_ = f.Close()
}

// emptyGrace is how long, in whole epoch seconds, an old-build record with no
// pid text may be a live acquirer: that protocol writes its pid after mkdir, so
// a fresh empty record is mid-write and replacing it would give two owners.
const emptyGrace = 2

// fresh reports whether dir's mtime is within emptyGrace of now, on either side
// so a skewed clock cannot wedge the lock. Wall clock, because the mtime is; a
// vanished dir is not fresh.
func fresh(dir string) bool {
	fi, err := os.Stat(dir)
	if err != nil {
		return false
	}
	age := time.Now().Unix() - fi.ModTime().Unix()
	return age > -emptyGrace && age < emptyGrace
}

// Release gives up dir if this process holds it, and is a no-op otherwise. A
// record or lock file that is no longer this holder's (the crew dir was
// removed and re-created under it) is left alone.
func Release(dir string) {
	mu.Lock()
	defer mu.Unlock()
	h, ok := holds[dir]
	if !ok {
		return
	}
	delete(holds, dir)
	drop(dir, h.f, h.owner)
}

// Holder is `$(cat "$dir/pid" 2>/dev/null || true)`: the owner text a refused
// acquisition reports, and the pid `stream --force` decides whether to signal.
func Holder(dir string) string {
	held, _ := readHolder(dir)
	return held
}

// readHolder is `$(cat "$ld/pid" 2>/dev/null || true)`: command substitution
// drops trailing newlines, and an unreadable file is empty. exists tells a
// missing file from an empty one.
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
