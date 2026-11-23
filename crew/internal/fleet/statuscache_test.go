package fleet

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func cacheOpts(t *testing.T) CacheOptions {
	t.Helper()
	return CacheOptions{Dir: filepath.Join(t.TempDir(), "crew"), User: "1", Wait: 5 * time.Second, Poll: 5 * time.Millisecond}
}

func countingScan(n *atomic.Int32, code int) func(out, errw io.Writer) int {
	return func(out, errw io.Writer) int {
		_, _ = fmt.Fprintf(out, "scan %d\n", n.Add(1))
		return code
	}
}

func cacheFiles(t *testing.T, dir string) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(dir, "fleet-status-*.json"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestCachedStatusServesWithinMaxAge(t *testing.T) {
	o := cacheOpts(t)
	var n atomic.Int32
	var a, b bytes.Buffer
	req := StatusRequest{JSON: true}
	if code := CachedStatus(context.Background(), req, time.Minute, &a, io.Discard, o, countingScan(&n, 0)); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if code := CachedStatus(context.Background(), req, time.Minute, &b, io.Discard, o, countingScan(&n, 0)); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if n.Load() != 1 || a.String() != b.String() || a.String() != "scan 1\n" {
		t.Errorf("scans %d, first %q, second %q", n.Load(), a.String(), b.String())
	}
}

func TestCachedStatusExpiredRescans(t *testing.T) {
	o := cacheOpts(t)
	var n atomic.Int32
	req := StatusRequest{JSON: true}
	CachedStatus(context.Background(), req, time.Minute, io.Discard, io.Discard, o, countingScan(&n, 0))
	files := cacheFiles(t, o.Dir)
	if len(files) != 1 {
		t.Fatalf("cache files %v", files)
	}
	old := time.Now().Add(-2 * time.Minute)
	if err := os.Chtimes(files[0], old, old); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	CachedStatus(context.Background(), req, time.Minute, &out, io.Discard, o, countingScan(&n, 0))
	if n.Load() != 2 || out.String() != "scan 2\n" {
		t.Errorf("scans %d, out %q", n.Load(), out.String())
	}
}

func TestCachedStatusNoMaxAgeNeverCaches(t *testing.T) {
	o := cacheOpts(t)
	var n atomic.Int32
	for range 2 {
		CachedStatus(context.Background(), StatusRequest{}, 0, io.Discard, io.Discard, o, countingScan(&n, 0))
	}
	if n.Load() != 2 {
		t.Errorf("scans %d", n.Load())
	}
	if _, err := os.Stat(o.Dir); err == nil {
		t.Error("cache dir created with no max-age")
	}
}

func TestCachedStatusFailedScanNotCached(t *testing.T) {
	o := cacheOpts(t)
	var n atomic.Int32
	var out, errw bytes.Buffer
	scan := func(out, errw io.Writer) int {
		n.Add(1)
		_, _ = fmt.Fprint(errw, "boom\n")
		return 1
	}
	if code := CachedStatus(context.Background(), StatusRequest{}, time.Minute, &out, &errw, o, scan); code != 1 || errw.String() != "boom\n" {
		t.Errorf("exit %d, stderr %q", code, errw.String())
	}
	if len(cacheFiles(t, o.Dir)) != 0 {
		t.Error("failed scan was cached")
	}
	CachedStatus(context.Background(), StatusRequest{}, time.Minute, io.Discard, io.Discard, o, scan)
	if n.Load() != 2 {
		t.Errorf("scans %d", n.Load())
	}
}

func TestCachedStatusConcurrentStaleCallersScanOnce(t *testing.T) {
	o := cacheOpts(t)
	var n atomic.Int32
	scan := func(out, errw io.Writer) int {
		time.Sleep(50 * time.Millisecond)
		_, _ = fmt.Fprintf(out, "scan %d\n", n.Add(1))
		return 0
	}
	const callers = 8
	outs := make([]bytes.Buffer, callers)
	var wg sync.WaitGroup
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			CachedStatus(context.Background(), StatusRequest{JSON: true}, time.Minute, &outs[i], io.Discard, o, scan)
		}()
	}
	wg.Wait()
	if n.Load() != 1 {
		t.Errorf("scans %d, want 1", n.Load())
	}
	for i := range outs {
		if outs[i].String() != "scan 1\n" {
			t.Errorf("caller %d got %q", i, outs[i].String())
		}
	}
}

func TestCachedStatusModes(t *testing.T) {
	o := cacheOpts(t)
	var n atomic.Int32
	CachedStatus(context.Background(), StatusRequest{}, time.Minute, io.Discard, io.Discard, o, countingScan(&n, 0))
	files := cacheFiles(t, o.Dir)
	if len(files) != 1 {
		t.Fatalf("cache files %v", files)
	}
	if fi, err := os.Stat(files[0]); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("file %v, %v", fi, err)
	}
	if fi, err := os.Stat(o.Dir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("dir %v, %v", fi, err)
	}
}

func TestCachedStatusKeysDifferByFlagsAndUser(t *testing.T) {
	o := cacheOpts(t)
	var n atomic.Int32
	other := o
	other.User = "2"
	variant := o
	variant.Variant = "overseer"
	for _, c := range []struct {
		req StatusRequest
		o   CacheOptions
	}{
		{StatusRequest{}, o}, {StatusRequest{JSON: true}, o}, {StatusRequest{GH: true}, o}, {StatusRequest{JSON: true, GH: true}, o}, {StatusRequest{}, other}, {StatusRequest{}, variant},
	} {
		CachedStatus(context.Background(), c.req, time.Minute, io.Discard, io.Discard, c.o, countingScan(&n, 0))
	}
	if n.Load() != 6 || len(cacheFiles(t, o.Dir)) != 6 {
		t.Errorf("scans %d, files %d", n.Load(), len(cacheFiles(t, o.Dir)))
	}
}

func waiterFinishesEarly(t *testing.T, holder func(out, errw io.Writer) int, cancelHolder bool) {
	t.Helper()
	o := cacheOpts(t)
	o.Wait = 10 * time.Second
	started := make(chan struct{})
	release := make(chan struct{})
	hctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hold := func(out, errw io.Writer) int {
		close(started)
		<-release
		if cancelHolder {
			cancel()
		}
		return holder(out, errw)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		CachedStatus(hctx, StatusRequest{}, time.Minute, io.Discard, io.Discard, o, hold)
	}()
	<-started
	var n atomic.Int32
	waited := make(chan time.Duration)
	go func() {
		begin := time.Now()
		CachedStatus(context.Background(), StatusRequest{}, time.Minute, io.Discard, io.Discard, o, countingScan(&n, 0))
		waited <- time.Since(begin)
	}()
	time.Sleep(50 * time.Millisecond)
	close(release)
	if d := <-waited; d > 3*time.Second {
		t.Errorf("waiter took %v", d)
	}
	<-done
	if n.Load() != 1 {
		t.Errorf("waiter scans %d, want 1", n.Load())
	}
}

func TestCachedStatusFailedHolderDoesNotStallWaiter(t *testing.T) {
	waiterFinishesEarly(t, func(out, errw io.Writer) int { return 1 }, false)
}

func TestCachedStatusHolderWithoutFileDoesNotStallWaiter(t *testing.T) {
	waiterFinishesEarly(t, func(out, errw io.Writer) int { return 0 }, true)
}

func TestCachedStatusUntrustedDirScansPlainly(t *testing.T) {
	for name, mode := range map[string]os.FileMode{"group-readable": 0o750, "read-only": 0o500} {
		t.Run(name, func(t *testing.T) {
			if name == "read-only" && os.Getuid() == 0 {
				t.Skip("root writes anywhere")
			}
			o := cacheOpts(t)
			if err := os.Mkdir(o.Dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(o.Dir, mode); err != nil {
				t.Fatal(err)
			}
			o.Wait = 10 * time.Second
			var n atomic.Int32
			var out bytes.Buffer
			begin := time.Now()
			if code := CachedStatus(context.Background(), StatusRequest{}, time.Minute, &out, io.Discard, o, countingScan(&n, 0)); code != 0 || out.String() != "scan 1\n" {
				t.Errorf("exit %d, out %q", code, out.String())
			}
			if time.Since(begin) > 3*time.Second || len(cacheFiles(t, o.Dir)) != 0 {
				t.Error("untrusted dir was not scanned plainly")
			}
		})
	}
}

func TestCachedStatusSymlinkDirScansPlainly(t *testing.T) {
	o := cacheOpts(t)
	target := t.TempDir()
	if err := os.Symlink(target, o.Dir); err != nil {
		t.Fatal(err)
	}
	var n atomic.Int32
	CachedStatus(context.Background(), StatusRequest{}, time.Minute, io.Discard, io.Discard, o, countingScan(&n, 0))
	if len(cacheFiles(t, target)) != 0 {
		t.Error("cached through a symlinked dir")
	}
}

func TestCachedStatusKeysDifferByScanEnv(t *testing.T) {
	home := t.TempDir()
	uid := strconv.Itoa(os.Getuid())
	envOf := func(over map[string]string) []string {
		m := map[string]string{"HOME": home}
		maps.Copy(m, over)
		var env []string
		for k, v := range m {
			env = append(env, k+"="+v)
		}
		return env
	}
	for _, c := range []struct {
		name string
		env  map[string]string
		same bool
	}{
		{"identical", nil, true},
		{"default registry spelled out", map[string]string{"XDG_DATA_HOME": home + "/.local/share"}, true},
		{"default state spelled out", map[string]string{"XDG_STATE_HOME": home + "/.local/state"}, true},
		{"default roots spelled out", map[string]string{"CREW_SWEEP_ROOTS": home}, true},
		{"default tmux dir spelled out", map[string]string{"TMUX_TMPDIR": "/tmp"}, true},
		{"sweep roots", map[string]string{"CREW_SWEEP_ROOTS": "/srv/other"}, false},
		{"registry", map[string]string{"XDG_DATA_HOME": "/srv/data"}, false},
		{"state", map[string]string{"XDG_STATE_HOME": "/srv/state"}, false},
		{"tmux tmpdir", map[string]string{"TMUX_TMPDIR": "/srv/tmux"}, false},
		{"tmux socket", map[string]string{"TMUX": "/srv/sock,123,0"}, false},
		{"overseer", map[string]string{"CREW_OVERSEER": "1"}, false},
		{"status dir", map[string]string{"CLAUDE_STATUS_DIR": "/srv/status"}, false},
		{"default tmux socket spelled out", map[string]string{"TMUX": "/tmp/tmux-" + uid + "/default,1,0"}, true},
		{"default status dir spelled out", map[string]string{"CLAUDE_STATUS_DIR": "/tmp/claude-status-" + uid}, true},
		{"default config dir spelled out", map[string]string{"XDG_CONFIG_HOME": home + "/.config"}, true},
		{"loadavg", map[string]string{"DISPATCH_LOADAVG_FILE": "/srv/loadavg"}, false},
		{"dispatch budget", map[string]string{"DISPATCH_FLEET_BUDGET": "8"}, false},
		{"dispatch config bin", map[string]string{"DISPATCH_CONFIG_BIN": "/srv/cfg"}, false},
		{"locked settings", map[string]string{"DISPATCH_LOCKED_SETTINGS": "/srv/locked.json"}, false},
		{"config dir", map[string]string{"XDG_CONFIG_HOME": "/srv/config"}, false},
		{"overseer host", map[string]string{"CREW_OVERSEER": "other"}, false},
		{"worker session", map[string]string{"CREW_WORKER_ID": "w"}, false},
		{"clock", map[string]string{"CREW_CLOCK": "/srv/clock"}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			o := cacheOpts(t)
			var n atomic.Int32
			a, b := o, o
			a.Variant = ScanVariant(envOf(nil))
			b.Variant = ScanVariant(envOf(c.env))
			CachedStatus(context.Background(), StatusRequest{}, time.Minute, io.Discard, io.Discard, a, countingScan(&n, 0))
			CachedStatus(context.Background(), StatusRequest{}, time.Minute, io.Discard, io.Discard, b, countingScan(&n, 0))
			want := int32(2)
			if c.same {
				want = 1
			}
			if n.Load() != want {
				t.Errorf("scans %d, want %d", n.Load(), want)
			}
		})
	}
}
