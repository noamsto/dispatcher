package supervisor

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/spikes/headless-supervisor/internal/event"
	"github.com/noamsto/dispatcher/spikes/headless-supervisor/internal/lock"
)

type serverHarness struct {
	state string
	mu    sync.Mutex
	opts  []event.Options
	fakes []*fakeEngine
	done  chan error
}

func startServer(t *testing.T) *serverHarness {
	t.Helper()
	h := &serverHarness{state: stateDir(t), done: make(chan error, 1)}
	factory := func(o event.Options) (event.Engine, error) {
		f := newFake(o)
		h.mu.Lock()
		h.opts = append(h.opts, o)
		h.fakes = append(h.fakes, f)
		h.mu.Unlock()
		return f, nil
	}
	s := NewServer(Config{Engine: "fake", Work: t.TempDir(), State: h.state, Factory: factory, Grace: time.Second})
	ctx, cancel := context.WithCancel(t.Context())
	go func() { h.done <- s.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-h.done
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := Call(ctx, h.state, CtlRequest{Cmd: "status"}); err == nil {
			return h
		}
		if time.Now().After(deadline) {
			t.Fatal("server did not come up")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (h *serverHarness) call(t *testing.T, cmd, arg string) (CtlResponse, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	return Call(ctx, h.state, CtlRequest{Cmd: cmd, Arg: arg})
}

func TestControlPauseResumeSend(t *testing.T) {
	h := startServer(t)

	resp, err := h.call(t, "send", "one")
	if err != nil || resp.Text != "echo:one" {
		t.Fatalf("send: %+v %v", resp, err)
	}
	h.fakes[0].setSession("forked-2") // child reported a new id

	if _, err := h.call(t, "pause", ""); err != nil {
		t.Fatal(err)
	}
	sess, err := ReadSession(h.state)
	if err != nil || sess.SessionID != "forked-2" || sess.Engine != "fake" {
		t.Fatalf("session.json = %+v, %v", sess, err)
	}
	if st, _ := h.call(t, "status", ""); st.State != "paused" {
		t.Fatalf("state = %q", st.State)
	}
	if _, err := h.call(t, "send", "x"); err == nil || !strings.Contains(err.Error(), "paused") {
		t.Fatalf("send while paused: %v", err)
	}

	if _, err := h.call(t, "resume", ""); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	o := h.opts[1]
	h.mu.Unlock()
	if !o.Resume || o.SessionID != "forked-2" {
		t.Fatalf("resume options = %+v", o)
	}
	resp, err = h.call(t, "send", "two")
	if err != nil || resp.Text != "echo:two" {
		t.Fatalf("send after resume: %+v %v", resp, err)
	}
}

func TestControlResumeFailsWhileLockHeld(t *testing.T) {
	h := startServer(t)
	if _, err := h.call(t, "pause", ""); err != nil {
		t.Fatal(err)
	}
	other, err := lock.Acquire(filepath.Join(h.state, LockFile))
	if err != nil {
		t.Fatalf("lock not released by pause: %v", err)
	}
	if _, err := h.call(t, "resume", ""); err == nil || !strings.Contains(err.Error(), "lock held") {
		t.Fatalf("resume under held lock: %v", err)
	}
	if st, _ := h.call(t, "status", ""); st.State != "paused" {
		t.Fatalf("state = %q, want paused", st.State)
	}
	if err := other.Release(); err != nil {
		t.Fatal(err)
	}
	if _, err := h.call(t, "resume", ""); err != nil {
		t.Fatalf("resume after release: %v", err)
	}
}

func TestControlPauseInterruptsSendInFlight(t *testing.T) {
	h := startServer(t)
	sendErr := make(chan error, 1)
	go func() {
		_, err := h.call(t, "send", "hold")
		sendErr <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		st, _ := h.call(t, "status", "")
		if st.Turn == StatusActive {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("turn never opened")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := h.call(t, "pause", ""); err != nil {
		t.Fatal(err)
	}
	if err := <-sendErr; err == nil || !strings.Contains(err.Error(), "turn ended in error: aborted") {
		t.Fatalf("interrupted send should report the aborted turn: %v", err)
	}
	h.fakes[0].mu.Lock()
	defer h.fakes[0].mu.Unlock()
	if h.fakes[0].interrupt != 1 {
		t.Fatalf("interrupts = %d, want 1", h.fakes[0].interrupt)
	}
}

func TestControlUnknownCommand(t *testing.T) {
	h := startServer(t)
	_, err := h.call(t, "bogus", "")
	if err == nil || !strings.Contains(err.Error(), "unknown command") {
		t.Fatalf("err = %v", err)
	}
}

func TestServeRefusesHeldLock(t *testing.T) {
	state := stateDir(t)
	l, err := lock.Acquire(filepath.Join(state, LockFile))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()
	s := NewServer(Config{Engine: "fake", Work: t.TempDir(), State: state, Factory: func(o event.Options) (event.Engine, error) { return newFake(o), nil }})
	if err := s.Serve(t.Context()); !errors.Is(err, lock.ErrHeld) {
		t.Fatalf("err = %v, want ErrHeld", err)
	}
}

// An external flock(1) holder (what takeover.sh wraps the TUI in) must block resume.
func TestControlResumeFailsWhileFlockProcessHolds(t *testing.T) {
	flock, err := exec.LookPath("flock")
	if err != nil {
		t.Skip("flock not on PATH")
	}
	h := startServer(t)
	if _, err := h.call(t, "pause", ""); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(h.state, LockFile)
	cmd := exec.Command(flock, "-o", path, "sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		l, err := lock.Acquire(path)
		if errors.Is(err, lock.ErrHeld) {
			break
		}
		if err == nil {
			_ = l.Release()
		}
		if time.Now().After(deadline) {
			t.Fatal("flock never took the lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	_, err = h.call(t, "resume", "")
	if err == nil || !strings.Contains(err.Error(), "a TUI owns this session") {
		t.Fatalf("resume under external flock: %v", err)
	}
	if st, _ := h.call(t, "status", ""); st.State != "paused" {
		t.Fatalf("state = %q, want paused", st.State)
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	if _, err := h.call(t, "resume", ""); err != nil {
		t.Fatalf("resume after flock exit: %v", err)
	}
}

func TestSessionRecordsTUILaunchFields(t *testing.T) {
	state, work := stateDir(t), t.TempDir()
	s := NewServer(Config{
		Engine: "fake", Work: work, State: state, Grace: time.Second,
		Factory:    func(o event.Options) (event.Engine, error) { return newFake(o), nil },
		Options:    event.Options{Bin: "/bin/eng", Model: "m1", GatePath: "/g/gate.ts", SessionID: "sid-1"},
		SessionDir: filepath.Join(work, ".pi-sessions"),
	})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx) }()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		sess, err := ReadSession(state)
		if err == nil {
			want := Session{Engine: "fake", SessionID: "sid-1", Work: work, Bin: "/bin/eng", Model: "m1", SessionDir: filepath.Join(work, ".pi-sessions"), Gate: "/g/gate.ts"}
			if sess != want {
				t.Fatalf("session = %+v, want %+v", sess, want)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("session.json never written: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestControlLaunchInheritsLock(t *testing.T) {
	h := startServer(t)
	if _, err := h.call(t, "pause", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := h.call(t, "resume", ""); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(h.state, LockFile)
	h.mu.Lock()
	defer h.mu.Unlock()
	for i, o := range h.opts {
		if len(o.Inherit) != 1 || o.Inherit[0].Name() != want {
			t.Fatalf("launch %d Inherit = %v, want the lock file %s", i, o.Inherit, want)
		}
	}
}

func TestControlPauseKeepsLockWhenShutdownFails(t *testing.T) {
	h := startServer(t)
	f := h.fakes[0]
	f.setStuck(true)
	t.Cleanup(func() { f.setStuck(false) }) // let teardown finish
	if _, err := h.call(t, "pause", ""); err == nil || !strings.Contains(err.Error(), "event stream still open") {
		t.Fatalf("pause with a child that won't exit: %v", err)
	}
	if st, _ := h.call(t, "status", ""); st.State != "running" {
		t.Fatalf("state = %q, want running", st.State)
	}
	if _, err := lock.Acquire(filepath.Join(h.state, LockFile)); !errors.Is(err, lock.ErrHeld) {
		t.Fatalf("lock after failed pause: want ErrHeld, got %v", err)
	}
}

func TestControlPauseSucceedsWhenReapedChildExitsNonZero(t *testing.T) {
	h := startServer(t)
	f := h.fakes[0]
	f.mu.Lock()
	f.closeErr = errors.New("exit status 1")
	f.mu.Unlock()
	resp, err := h.call(t, "pause", "")
	if err != nil {
		t.Fatalf("pause after a non-zero exit: %v", err)
	}
	if resp.State != "paused" || !strings.Contains(resp.Text, "exit status 1") {
		t.Fatalf("pause response = %+v", resp)
	}
	l, err := lock.Acquire(filepath.Join(h.state, LockFile))
	if err != nil {
		t.Fatalf("lock after pause: %v", err)
	}
	_ = l.Release()
	if _, err := h.call(t, "resume", ""); err != nil {
		t.Fatalf("resume: %v", err)
	}
}

func TestServeSessionWriteFailureStartsNothing(t *testing.T) {
	state := stateDir(t)
	// rename(2) cannot replace a directory, so writeSession fails.
	if err := os.Mkdir(filepath.Join(state, SessionFile), 0o700); err != nil {
		t.Fatal(err)
	}
	built := 0
	s := NewServer(Config{Engine: "fake", Work: t.TempDir(), State: state, Grace: time.Second,
		Factory: func(o event.Options) (event.Engine, error) { built++; return newFake(o), nil }})
	if err := s.Serve(t.Context()); err == nil {
		t.Fatal("Serve succeeded with an unwritable session.json")
	}
	if built != 0 {
		t.Fatalf("engine built %d times despite the session write failing", built)
	}
	l, err := lock.Acquire(filepath.Join(state, LockFile))
	if err != nil {
		t.Fatalf("lock not released: %v", err)
	}
	_ = l.Release()
}

func TestServerDefaultPolicyDenies(t *testing.T) {
	s := NewServer(Config{})
	if d := s.cfg.Policy(&event.Request{}); d.Verdict != "deny" || d.Reason != "hsup serve: no policy configured" {
		t.Fatalf("default decision = %+v", d)
	}
}

func TestPrepareState(t *testing.T) {
	root := t.TempDir()

	fresh := filepath.Join(root, "fresh")
	if err := PrepareState(fresh); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(fresh); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("fresh state: %v %v", fi.Mode(), err)
	}
	if err := PrepareState(fresh); err != nil {
		t.Fatalf("reusing own 0700 dir: %v", err)
	}

	open := filepath.Join(root, "open")
	if err := os.Mkdir(open, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(open, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := PrepareState(open); err == nil {
		t.Fatal("accepted a 0755 state dir")
	}

	link := filepath.Join(root, "link")
	if err := os.Symlink(fresh, link); err != nil {
		t.Fatal(err)
	}
	if err := PrepareState(link); err == nil {
		t.Fatal("accepted a symlinked state dir")
	}
}

// stateDir is a 0700 state dir; t.TempDir follows the umask and is refused.
func stateDir(t *testing.T) string {
	t.Helper()
	d := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(d, 0o700); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestCheckStateNeverCreates(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	if err := CheckState(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing dir: %v", err)
	}
	if _, err := os.Lstat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("CheckState created the dir")
	}
	ok := stateDir(t)
	if err := CheckState(ok); err != nil {
		t.Fatalf("own 0700 dir: %v", err)
	}
	if err := os.Chmod(ok, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := CheckState(ok); err == nil {
		t.Fatal("accepted a 0750 state dir")
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(stateDir(t), link); err != nil {
		t.Fatal(err)
	}
	if err := CheckState(link); err == nil {
		t.Fatal("accepted a symlinked state dir")
	}
}

func TestReadSessionRefusesUntrustedFile(t *testing.T) {
	state := stateDir(t)
	path := filepath.Join(state, SessionFile)
	if err := os.WriteFile(path, []byte(`{"engine":"pi","session_id":"s"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if s, err := ReadSession(state); err != nil || s.SessionID != "s" {
		t.Fatalf("own 0600 file: %+v %v", s, err)
	}
	if err := os.Chmod(path, 0o620); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSession(state); err == nil {
		t.Fatal("accepted a group-writable session.json")
	}

	target := filepath.Join(t.TempDir(), "elsewhere.json")
	if err := os.WriteFile(target, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	linked := stateDir(t)
	if err := os.Symlink(target, filepath.Join(linked, SessionFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSession(linked); err == nil {
		t.Fatal("followed a symlinked session.json")
	}
}
