package fleet

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/lock"
	"github.com/noamsto/dispatcher/crew/internal/repos"
)

// CacheOptions are the status cache's outside reads, so tests use a temp dir
// and short waits.
type CacheOptions struct {
	// Dir holds the cache files; empty (no cache home) means no caching.
	Dir string
	// User is part of the key: the output names that user's private paths.
	User string
	// Variant is part of the key: it names the inputs that change the scan
	// (see ScanVariant).
	Variant string
	// Now is the clock the file's age is read against; nil is time.Now.
	Now func() time.Time
	// Wait bounds how long a caller waits for another's refresh; zero is 20 s.
	Wait time.Duration
	// Poll is the interval of that wait; zero is 200 ms.
	Poll time.Duration
}

var lockSeq atomic.Uint64

// CachedStatus serves `fleet status` output younger than maxAge from the
// shared cache file. Otherwise one caller at a time runs scan and refreshes the
// file, and the rest wait for its result. Only an exit 0 scan is cached; any
// cache failure degrades to a plain scan.
func CachedStatus(ctx context.Context, req StatusRequest, maxAge time.Duration, stdout, stderr io.Writer, o CacheOptions, scan func(out, errw io.Writer) int) int {
	if o.Dir == "" || maxAge <= 0 {
		return scan(stdout, stderr)
	}
	now := o.Now
	if now == nil {
		now = time.Now
	}
	wait, poll := o.Wait, o.Poll
	if wait <= 0 {
		wait = 20 * time.Second
	}
	if poll <= 0 {
		poll = 200 * time.Millisecond
	}
	if !trustedDir(o.Dir) {
		return scan(stdout, stderr)
	}
	key := fmt.Sprintf("%x", sha256.Sum256(fmt.Appendf(nil, "json=%t gh=%t user=%s variant=%s", req.JSON, req.GH, o.User, o.Variant)))[:16]
	path := filepath.Join(o.Dir, "fleet-status-"+key+".json")

	fresh := func() ([]byte, bool) {
		fi, err := os.Stat(path)
		if err != nil {
			return nil, false
		}
		if age := now().Sub(fi.ModTime()); age < 0 || age >= maxAge {
			return nil, false
		}
		b, err := os.ReadFile(path)
		return b, err == nil
	}
	serve := func(b []byte) int {
		if _, err := stdout.Write(b); err != nil {
			return 1
		}
		return 0
	}
	refresh := func() int {
		var buf bytes.Buffer
		code := scan(&buf, stderr)
		if code == 0 && ctx.Err() == nil {
			_ = writeCacheFile(path, buf.Bytes())
		}
		if _, err := stdout.Write(buf.Bytes()); err != nil && code == 0 {
			return 1
		}
		return code
	}

	if b, ok := fresh(); ok {
		return serve(b)
	}
	lockDir := path + ".d"
	owner := strconv.Itoa(os.Getpid()) + "-" + strconv.FormatUint(lockSeq.Add(1), 10)
	// locked runs as the lock holder; a holder that failed or died leaves no
	// file, so waiters retry the lock rather than only watching for the file.
	locked := func() int {
		defer lock.Release(lockDir)
		if b, ok := fresh(); ok {
			return serve(b)
		}
		return refresh()
	}
	if lock.Acquire(lockDir, owner) {
		return locked()
	}
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	tick := time.NewTicker(poll)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return scan(stdout, stderr)
		case <-deadline.C:
			return refresh()
		case <-tick.C:
			if b, ok := fresh(); ok {
				return serve(b)
			}
			if lock.Acquire(lockDir, owner) {
				return locked()
			}
		}
	}
}

// trustedDir creates dir and reports whether it is a private directory of this
// user that can be written, so another user's planted dir or symlink is never
// read from.
func trustedDir(dir string) bool {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false
	}
	fi, err := os.Lstat(dir)
	if err != nil || !fi.IsDir() || fi.Mode().Perm()&0o077 != 0 {
		return false
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); !ok || int(st.Uid) != os.Getuid() {
		return false
	}
	f, err := os.CreateTemp(dir, ".probe-*")
	if err != nil {
		return false
	}
	_ = f.Close()
	return os.Remove(f.Name()) == nil
}

func writeCacheFile(path string, b []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".fleet-status-*")
	if err != nil {
		return err
	}
	name := f.Name()
	_, err = f.Write(b)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(name, path)
	}
	if err != nil {
		_ = os.Remove(name)
	}
	return err
}

// ScanVariant names the effective value of every environment input that
// changes what the status scan finds, so callers whose scans would differ never
// share a cache file while an unset variable and its default still do. Paths
// are resolved the way the scan resolves them. Every DISPATCH_* variable counts:
// dispatch-config layers them over the settings the scan reads. PATH and the gh
// auth environment are left out.
func ScanVariant(environ []string) string {
	vars := map[string]string{}
	var dispatch []string
	for _, kv := range environ {
		k, v, _ := strings.Cut(kv, "=")
		vars[k] = v
		if strings.HasPrefix(k, "DISPATCH_") {
			dispatch = append(dispatch, kv)
		}
	}
	slices.Sort(dispatch)
	lookup := func(k string) string { return vars[k] }
	roots := lookup("CREW_SWEEP_ROOTS")
	if roots == "" {
		roots = lookup("HOME")
	}
	statusDir := lookup("CLAUDE_STATUS_DIR")
	if statusDir == "" {
		statusDir = "/tmp/claude-status-" + strconv.Itoa(os.Getuid())
	}
	// $TMUX names the caller's own server socket, ahead of the default one.
	socket, _, _ := strings.Cut(lookup("TMUX"), ",")
	if socket == "" {
		socket = filepath.Join(cmp.Or(lookup("TMUX_TMPDIR"), "/tmp"), "tmux-"+strconv.Itoa(os.Getuid()), "default")
	}
	configDir := cmp.Or(lookup("XDG_CONFIG_HOME"), lookup("HOME")+"/.config")
	// The overseer gate refuses a worker or role session, so who asks matters.
	member := lookup("CREW_WORKER_ID") != "" || lookup("CREW_ROLE_ID") != ""
	return fmt.Sprintf("overseer=%q member=%t roots=%q registry=%q state=%q config=%q status=%q tmux=%q clock=%q dispatch=%q",
		lookup("CREW_OVERSEER"), member, roots, repos.RegistryPath(lookup), stateHome(lookup), configDir, statusDir, socket, lookup("CREW_CLOCK"), dispatch)
}
