package stall

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// sigterm is a context NotifySignals cancelled on SIGTERM, and its cancel.
func sigterm(t *testing.T) (context.Context, func()) {
	t.Helper()
	ctx, cancel := context.WithCancelCause(t.Context())
	return ctx, func() { cancel(SignalError{Sig: syscall.SIGTERM}) }
}

// A probe cut short by the signal reads as a failure ("", not alive); none of
// those is evidence, and the arm died on the signal without posting.
func TestEscalateCancelledPostsNothing(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(w *watch)
	}{
		{"quiet:", func(w *watch) { w.fd.d3At = w.start }},
		{"turn-stall:", func(w *watch) { w.fd.d2At = w.start }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newFDRig(t, "", "", "--idle", "60", "--dead", "120")
			w := r.w
			ctx, cancel := sigterm(t)
			cancel()
			w.ctx = ctx
			tc.set(w)
			w.now = w.start + 200
			code, ok := codeOf(w.escalate())
			if posted := r.posted(); !ok || code != 143 || posted != nil {
				t.Errorf("escalate = (%d, %v) posted %q, want 143 and nothing", code, ok, posted)
			}
		})
	}
}

func TestPaneGoneQuorumCancelled(t *testing.T) {
	f := newFake()
	h := newHarness(t, f)
	ctx, cancel := sigterm(t)
	h.ctx = ctx
	f.sample = func(n int) (string, bool) {
		if n == 2 {
			cancel()
		}
		return "", false
	}
	code, _ := h.run("feat/x", "--pane", "%1", "--grace", "0", "--interval", "15")
	if code != 143 || f.samples != 3 {
		t.Errorf("exit %d after %d samples, want 143 after 3", code, f.samples)
	}
}

func TestReleaseSampleCancelled(t *testing.T) {
	f := newFake()
	_, w := releaseWatch(t, f, "worker:feat/x", `{"state":"done"}`, "--engine", "claude")
	ctx, cancel := sigterm(t)
	w.ctx = ctx
	f.sample = func(int) (string, bool) {
		cancel()
		return "", false
	}
	if code, ok := codeOf(w.finishedRelease()); !ok || code != 143 {
		t.Errorf("finishedRelease = (%d, %v), want 143", code, ok)
	}
}

// A cancelled Top drops the `(top: …)` suffix; the arm never posted that row.
func TestD4TopCancelled(t *testing.T) {
	f := newFake()
	h, w := pdWatch(t, f, "feat/x", "--pane", "%1", "--load", "0")
	ctx, cancel := sigterm(t)
	w.ctx = ctx
	w.p.Load = func(context.Context) string { return "9.50 8" }
	w.p.Top = func(context.Context) string {
		cancel()
		return ""
	}
	if code, ok := codeOf(pdTick(t, w, 0, 0, w.d4)); !ok || code != 143 {
		t.Errorf("d4 = (%d, %v), want 143", code, ok)
	}
	wantRows(t, h)
}

// slowOp is an Sh that the signal arrives during: it reports whether the op
// ran to its end, as the arm's `$(…)` child did after the parent died.
func slowOp(cancel func(), finished *bool) func(ctx context.Context, op string, args ...string) (string, int) {
	return func(ctx context.Context, op string, args ...string) (string, int) {
		cancel()
		select {
		case <-ctx.Done():
			return "", -1
		case <-time.After(50 * time.Millisecond):
			*finished = true
			return "", shOffset
		}
	}
}

func TestNudgeSurvivesCancel(t *testing.T) {
	s := &d6Script{}
	h, f, w := d6Watch(t, s)
	ctx, cancel := sigterm(t)
	w.ctx = ctx
	s.dispatcher = fmt.Sprintf("%d %d", agedMS(w, 0, 700), agedMS(w, 0, 650))
	finished := false
	nudge := slowOp(cancel, &finished)
	f.shc = func(ctx context.Context, op string, args ...string) (string, int) {
		if op == "nudge" {
			return nudge(ctx, op, args...)
		}
		return s.sh(op, args...)
	}
	if code, ok := codeOf(pdTick(t, w, 0, 0, w.d6)); !ok || code != 143 {
		t.Errorf("d6 = (%d, %v), want 143", code, ok)
	}
	if !finished {
		t.Error("the nudge was killed mid-write")
	}
	wantRows(t, h)
}

func TestReleaseSurvivesCancel(t *testing.T) {
	f := newFake()
	f.sample = sampling(frameText(t, "done_idle.txt"))
	_, w := releaseWatch(t, f, "worker:feat/x", `{"state":"done"}`, "--engine", "claude")
	ctx, cancel := sigterm(t)
	w.ctx = ctx
	finished := false
	f.shc = slowOp(cancel, &finished)
	if code, ok := codeOf(w.finishedRelease()); !ok || code != 143 {
		t.Errorf("finishedRelease = (%d, %v), want 143", code, ok)
	}
	if !finished {
		t.Error("the release was killed mid-write")
	}
}

// A signal landing during the already-nudged log scan, after the last
// cancellable call-out, must not start the write: the arm died before typing.
func TestNudgeNotStartedAfterCancel(t *testing.T) {
	s := &d6Script{}
	h, f, w := d6Watch(t, s)
	ctx, cancel := sigterm(t)
	w.ctx = ctx
	s.dispatcher = fmt.Sprintf("%d %d", agedMS(w, 0, 700), agedMS(w, 0, 650))
	fifo := filepath.Join(t.TempDir(), "log.fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	f.shc = func(_ context.Context, op string, args ...string) (string, int) {
		if op == "unread" {
			// Hold nudged()'s read open until the signal has landed.
			w.paths.Log = fifo
			go func() {
				fh, err := os.OpenFile(fifo, os.O_WRONLY, 0)
				cancel()
				if err == nil {
					_ = fh.Close()
				}
			}()
		}
		return s.sh(op, args...)
	}
	if code, ok := codeOf(pdTick(t, w, 0, 0, w.d6)); !ok || code != 143 {
		t.Errorf("d6 = (%d, %v), want 143", code, ok)
	}
	if n := countCalls(f, "nudge "); n != 0 {
		t.Errorf("%d nudge calls after the signal, want none", n)
	}
	wantRows(t, h)
}

func TestShWriteNotStartedAfterCancel(t *testing.T) {
	for _, op := range []string{"nudge", "release"} {
		t.Run(op, func(t *testing.T) {
			f := newFake()
			_, w := pdWatch(t, f, "feat/x", "--pane", "%1")
			ctx, cancel := sigterm(t)
			cancel()
			w.ctx = ctx
			_, _, _, err := w.shWrite(op)
			if code, ok := codeOf(err); !ok || code != 143 || len(f.shCalls) != 0 {
				t.Errorf("shWrite = (%d, %v) with %d calls, want 143 and none", code, ok, len(f.shCalls))
			}
		})
	}
}
