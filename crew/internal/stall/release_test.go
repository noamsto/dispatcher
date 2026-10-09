package stall

import (
	"context"
	"os"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/frame"
	"github.com/noamsto/dispatcher/crew/internal/jsonv"
)

func frameText(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile("../frame/testdata/frames/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimRight(string(data), "\n")
}

// releaseWatch is a watch on feat/x whose worker posted body 999ms into the
// second the clock starts at, so the integer division by 1000 matters.
func releaseWatch(t *testing.T, f *fake, id, body string, extra ...string) (*harness, *watch) {
	t.Helper()
	h := newHarness(t, f)
	if body != "" {
		h.writeRows(h.status(id, 999, body))
	}
	argv := append([]string{id, "--pane", "%1", "--grace", "0", "--interval", "15", "--release", "15"}, extra...)
	w := h.watch(argv...)
	w.start = h.clock.Seconds()
	return h, w
}

func sampling(text string) func(int) (string, bool) {
	return func(int) (string, bool) { return text, true }
}

func TestReleaseClaudeNeedsTwoIdleTicks(t *testing.T) {
	cases := []struct {
		name, id, session string
	}{
		{"sessionless", "worker:feat/x", "-"},
		{"session suffix", "worker:feat/x#s100-1", "s100-1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			f.sample = sampling(frameText(t, "done_idle.txt"))
			h, w := releaseWatch(t, f, tc.id, `{"state":"done"}`, "--engine", "claude")
			code, ok := codeOf(w.finishedRelease())
			if !ok || code != 0 {
				t.Fatalf("finishedRelease = %d, %v; want exit 0", code, ok)
			}
			ts := strconv.FormatInt(h.seed*1000+999, 10)
			want := []string{"release feat/x " + tc.session + " done " + ts + " 15"}
			if !reflect.DeepEqual(f.shCalls, want) {
				t.Errorf("sh calls %q, want %q", f.shCalls, want)
			}
			if f.samples != 3 {
				t.Errorf("samples %d, want 3 (idle at +15s and +30s, after the sample at +0)", f.samples)
			}
		})
	}
}

func TestReleaseWaitsForTheGrace(t *testing.T) {
	f := newFake()
	f.sample = sampling(frameText(t, "done_idle.txt"))
	h, w := releaseWatch(t, f, "worker:feat/x", `{"state":"failed"}`, "--engine", "claude", "--release", "60")
	if code, ok := codeOf(w.finishedRelease()); !ok || code != 0 {
		t.Fatalf("finishedRelease = %d, %v", code, ok)
	}
	if len(f.shCalls) != 1 || !strings.HasSuffix(f.shCalls[0], " failed "+strconv.FormatInt(h.seed*1000+999, 10)+" 60") {
		t.Errorf("sh calls %q", f.shCalls)
	}
	// Samples at +0, 15, 30, 45 are inside the grace; +60 and +75 are the two idle ticks.
	if f.samples != 6 {
		t.Errorf("samples %d, want 6", f.samples)
	}
}

func TestReleaseClaudeLiveTurnNeverReleases(t *testing.T) {
	for _, name := range []string{"meter.2s.1.2k.txt", "claude_long.10.0k.txt"} {
		t.Run(name, func(t *testing.T) {
			f := newFake()
			f.sample = sampling(frameText(t, name))
			_, w := releaseWatch(t, f, "worker:feat/x", `{"state":"done"}`, "--engine", "claude", "--max-life", "90")
			if code, ok := codeOf(w.finishedRelease()); !ok || code != 0 {
				t.Fatalf("finishedRelease = %d, %v", code, ok)
			}
			if len(f.shCalls) != 0 || f.samples != 6 {
				t.Errorf("sh calls %q samples %d, want none and 6 (until max-life)", f.shCalls, f.samples)
			}
		})
	}
}

// An idle tick followed by a live one starts the count again.
func TestReleaseClaudeIdleTicksMustBeConsecutive(t *testing.T) {
	idle, live := frameText(t, "done_idle.txt"), frameText(t, "meter.2s.1.2k.txt")
	f := newFake()
	f.sample = func(n int) (string, bool) {
		if n%2 == 0 {
			return idle, true
		}
		return live, true
	}
	_, w := releaseWatch(t, f, "worker:feat/x", `{"state":"done"}`, "--engine", "claude", "--max-life", "90")
	if code, ok := codeOf(w.finishedRelease()); !ok || code != 0 {
		t.Fatalf("finishedRelease = %d, %v", code, ok)
	}
	if len(f.shCalls) != 0 {
		t.Errorf("released on non-consecutive idle ticks: %q", f.shCalls)
	}
}

func TestReleaseRC3KeepsLooping(t *testing.T) {
	cases := []struct {
		name, engine, frame string
		release             string
		samples             int
	}{
		// Two fresh idle ticks after each refusal.
		{"claude", "claude", "done_idle.txt", "15", 5},
		// A refusal restarts the quiet window: +30 and +60.
		{"non-claude", "codex", "done_idle.txt", "30", 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			f.sample = sampling(frameText(t, tc.frame))
			f.sh = func(string, ...string) (string, int) {
				if len(f.shCalls) == 1 {
					return "", 3
				}
				return "", 0
			}
			_, w := releaseWatch(t, f, "worker:feat/x", `{"state":"done"}`, "--engine", tc.engine, "--release", tc.release)
			if code, ok := codeOf(w.finishedRelease()); !ok || code != 0 {
				t.Fatalf("finishedRelease = %d, %v", code, ok)
			}
			if len(f.shCalls) != 2 || f.samples != tc.samples {
				t.Errorf("sh calls %d samples %d, want 2 and %d", len(f.shCalls), f.samples, tc.samples)
			}
		})
	}
}

func TestReleaseOtherStatusExits(t *testing.T) {
	for _, rc := range []int{0, 1, 2, 4, -1} {
		t.Run(strconv.Itoa(rc), func(t *testing.T) {
			f := newFake()
			f.sample = sampling(frameText(t, "done_idle.txt"))
			f.sh = func(string, ...string) (string, int) { return "", rc }
			_, w := releaseWatch(t, f, "worker:feat/x", `{"state":"done"}`, "--engine", "claude")
			if code, ok := codeOf(w.finishedRelease()); !ok || code != 0 {
				t.Fatalf("finishedRelease = %d, %v", code, ok)
			}
			if len(f.shCalls) != 1 {
				t.Errorf("sh calls %q, want one", f.shCalls)
			}
		})
	}
}

func TestReleaseNonClaudeQuietWindow(t *testing.T) {
	idle := frameText(t, "done_idle.txt")
	cases := []struct {
		name    string
		sample  func(int) (string, bool)
		wantSh  int
		samples int
		maxLife string
		engine  string
	}{
		{"unchanged frame", sampling(idle), 1, 3, "300", "codex"},
		{"churning frame", func(n int) (string, bool) { return idle + strconv.Itoa(n), true }, 0, 10, "150", "codex"},
		{"frame that stops churning", func(n int) (string, bool) { return idle + strconv.Itoa(min(n, 2)), true }, 1, 5, "300", "codex"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			f.sample = tc.sample
			_, w := releaseWatch(t, f, "worker:feat/x", `{"state":"done"}`, "--engine", tc.engine, "--release", "30", "--max-life", tc.maxLife)
			if code, ok := codeOf(w.finishedRelease()); !ok || code != 0 {
				t.Fatalf("finishedRelease = %d, %v", code, ok)
			}
			if len(f.shCalls) != tc.wantSh || f.samples != tc.samples {
				t.Errorf("sh calls %d samples %d, want %d and %d", len(f.shCalls), f.samples, tc.wantSh, tc.samples)
			}
		})
	}
}

// A prompt on screen resets the quiet clock to zero for every non-claude engine.
func TestReleaseNonClaudePromptResetsQuiet(t *testing.T) {
	cases := []struct{ engine, frame string }{
		{"codex", "codex_hooks_review.txt"},
		{"pi", "prompt_select.txt"},
	}
	for _, tc := range cases {
		t.Run(tc.engine, func(t *testing.T) {
			text := frameText(t, tc.frame)
			f := newFake()
			f.sample = sampling(text)
			_, w := releaseWatch(t, f, "worker:feat/x", `{"state":"done"}`, "--engine", tc.engine, "--release", "30", "--max-life", "150")
			if !frame.IsPrompt(tc.engine, text) && !frame.IsPermissionPrompt(tc.engine, text) {
				t.Fatalf("%s is not a %s prompt", tc.frame, tc.engine)
			}
			if code, ok := codeOf(w.finishedRelease()); !ok || code != 0 {
				t.Fatalf("finishedRelease = %d, %v", code, ok)
			}
			if len(f.shCalls) != 0 {
				t.Errorf("released with a prompt on screen: %q", f.shCalls)
			}
		})
	}
}

func TestReleaseWatchdogFailedKeepsTheEvidence(t *testing.T) {
	f := newFake()
	_, w := releaseWatch(t, f, "worker:feat/x", `{"state":"failed","source":"watchdog","detail":"quiet: x"}`, "--engine", "claude")
	if code, ok := codeOf(w.finishedRelease()); !ok || code != 0 {
		t.Fatalf("finishedRelease = %d, %v", code, ok)
	}
	if len(f.shCalls) != 0 || f.samples != 0 {
		t.Errorf("sh calls %q samples %d, want neither", f.shCalls, f.samples)
	}
}

func TestReleaseSampleFailureExits(t *testing.T) {
	f := newFake()
	f.sample = func(int) (string, bool) { return "", false }
	_, w := releaseWatch(t, f, "worker:feat/x", `{"state":"done"}`, "--engine", "claude")
	if code, ok := codeOf(w.finishedRelease()); !ok || code != 0 {
		t.Fatalf("finishedRelease = %d, %v", code, ok)
	}
	// No pane-gone quorum here: the first failed sample ends the watch.
	if len(f.shCalls) != 0 || f.samples != 1 {
		t.Errorf("sh calls %q samples %d, want none and 1", f.shCalls, f.samples)
	}
}

func TestReleaseMaxLifeBeforeSampling(t *testing.T) {
	f := newFake()
	_, w := releaseWatch(t, f, "worker:feat/x", `{"state":"done"}`, "--engine", "claude", "--max-life", "15")
	if code, ok := codeOf(w.finishedRelease()); !ok || code != 0 {
		t.Fatalf("finishedRelease = %d, %v", code, ok)
	}
	if len(f.shCalls) != 0 || f.samples != 1 {
		t.Errorf("sh calls %q samples %d, want none and 1", f.shCalls, f.samples)
	}
}

// Before the worker's own row appears the loop sleeps and re-reads, with no
// max-life check, until a later status ends it.
func TestReleaseEmptyStateLoopsWithoutMaxLife(t *testing.T) {
	f := newFake()
	h, w := releaseWatch(t, f, "worker:feat/x", "", "--engine", "claude", "--max-life", "10")
	reads := 0
	w.p.BusRows = func(path string) ([]jsonv.Value, bool) {
		reads++
		if reads == 5 {
			h.writeRows(h.status("worker:feat/x", 1, `{"state":"working"}`))
		}
		return fileRows(path)
	}
	if err := w.finishedRelease(); err != nil {
		t.Fatalf("finishedRelease = %v, want nil", err)
	}
	if got, want := h.clock.Seconds(), h.seed+4*15; got != want {
		t.Errorf("clock +%d, want +60 (four sleeps past max-life 10)", got-h.seed)
	}
	if f.samples != 0 || len(f.shCalls) != 0 {
		t.Errorf("samples %d sh calls %q, want none", f.samples, f.shCalls)
	}
}

func TestReleaseSleepSignalExits(t *testing.T) {
	f := newFake()
	h, w := releaseWatch(t, f, "worker:feat/x", "", "--engine", "claude")
	ctx, cancel := context.WithCancelCause(t.Context())
	cancel(SignalError{Sig: syscall.SIGTERM})
	w.ctx, h.ctx = ctx, ctx
	if code, ok := codeOf(w.finishedRelease()); !ok || code != 143 {
		t.Errorf("finishedRelease = %d, %v; want exit 143", code, ok)
	}
}

func TestReleaseStepsAside(t *testing.T) {
	f := newFake()
	h := newHarness(t, f)
	h.writeRows(
		h.status("worker:feat/x#s100-1", 1, `{"state":"done"}`),
		h.status("worker:feat/x#s200-5", 2, `{"state":"working"}`),
	)
	w := h.watch("worker:feat/x#s100-1", "--pane", "%1", "--release", "15")
	w.start = h.clock.Seconds()
	if code, ok := codeOf(w.finishedRelease()); !ok || code != 0 || f.samples != 0 {
		t.Errorf("finishedRelease = %d, %v after %d samples; want exit 0 before sampling", code, ok, f.samples)
	}
}

// A worker that re-opens the session hands control back to the main loop,
// which samples again with fresh change tracking.
func TestReleaseReopenedResumesWatching(t *testing.T) {
	f := newFake()
	idle := frameText(t, "done_idle.txt")
	h := newHarness(t, f)
	h.writeRows(h.status("worker:feat/x", 1, `{"state":"done"}`))
	f.sample = func(n int) (string, bool) {
		if n == 1 {
			h.writeRows(h.status("worker:feat/x", 2, `{"state":"working"}`))
		}
		return idle, true
	}
	code, _ := h.run("feat/x", "--pane", "%1", "--grace", "0", "--interval", "15", "--release", "120",
		"--engine", "claude", "--max-life", "90", "--no-budget")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	// Two release-loop samples, then the main loop keeps sampling until max-life.
	if len(f.shCalls) != 0 || f.samples <= 2 {
		t.Errorf("sh calls %q samples %d, want none and more than 2", f.shCalls, f.samples)
	}
}
