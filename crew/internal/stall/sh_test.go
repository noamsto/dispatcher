package stall

import (
	"fmt"
	"strconv"
	"testing"
)

// helperFailures are statuses of an `--sh` call that never reached its op:
// crew.sh's preamble or usage refusal (1), no such file (127), no bash or a
// signalled child (-1).
var helperFailures = []int{1, 127, -1}

func TestShVerdict(t *testing.T) {
	for _, tc := range []struct {
		rc, verdict int
		ok          bool
	}{
		{10, 0, true}, {11, 1, true}, {13, 3, true},
		{0, 0, false}, {1, 0, false}, {9, 0, false}, {-1, 0, false},
		{126, 0, false}, {127, 0, false}, {143, 0, false},
	} {
		if v, ok := shVerdict(tc.rc); v != tc.verdict || ok != tc.ok {
			t.Errorf("shVerdict(%d) = %d, %v; want %d, %v", tc.rc, v, ok, tc.verdict, tc.ok)
		}
	}
}

// A budget helper that failed to run is can't-tell: the open episode holds,
// and only a real clear verdict clears it.
func TestD8HelperFailureHolds(t *testing.T) {
	for _, rc := range helperFailures {
		t.Run(strconv.Itoa(rc), func(t *testing.T) {
			f := newFake()
			h, w := d8Watch(t, f)
			const blocked = "blocked|budget: claude 5h at 100% (no reset time)"
			f.sh = budgetSh("5h\t100\t\t", 0, "", 1)
			pdOK(t, w, 0, 0, w.probePre)
			f.sh = func(string, ...string) (string, int) { return "", rc }
			pdOK(t, w, 60, 4, w.probePre)
			wantRows(t, h, blocked)
			f.sh = budgetSh("", 1, "", 1)
			pdOK(t, w, 120, 8, w.probePre)
			wantRows(t, h, blocked, "working|budget: cleared")
		})
	}
}

// With no budget episode open, a budget helper that failed to run posts
// nothing and arms nothing: its status is not an exhausted verdict.
func TestD8HelperFailureNoEpisode(t *testing.T) {
	for _, rc := range helperFailures {
		t.Run(strconv.Itoa(rc), func(t *testing.T) {
			f := newFake()
			h, w := d8Watch(t, f)
			h.writeRows(h.status("worker:feat/x", 1, `{"state":"working"}`))
			f.sh = func(string, ...string) (string, int) { return "", rc }
			pdOK(t, w, 0, 0, w.probePre)
			pdOK(t, w, 60, 4, w.probePre)
			wantRows(t, h)
			if w.pd.d8At != 0 {
				t.Errorf("d8At armed at %d", w.pd.d8At-w.start)
			}
			if n := countCalls(f, "budget "); n == 0 {
				t.Error("budget helper never called")
			}
		})
	}
}

// A failed unread scan skips the tick's verdict: the open episode neither
// clears nor changes.
func TestD6UnreadHelperFailureHolds(t *testing.T) {
	for _, rc := range helperFailures {
		t.Run(strconv.Itoa(rc), func(t *testing.T) {
			s := &d6Script{}
			h, _, w := d6Watch(t, s, "--no-nudge")
			s.oldest = fmt.Sprintf("%d role", agedMS(w, 0, 700))
			pdOK(t, w, 0, 0, w.d6)
			posted := "blocked|" + fmt.Sprintf(roleDetail, 700)
			at := w.pd.d6At
			s.failUnread = rc
			pdOK(t, w, 60, 4, w.d6)
			wantRows(t, h, posted)
			if w.pd.d6At != at || w.pd.d6Src != "role" {
				t.Errorf("d6At %d d6Src %q changed", w.pd.d6At-w.start, w.pd.d6Src)
			}
			s.failUnread, s.oldest = 0, ""
			pdOK(t, w, 120, 8, w.d6)
			wantRows(t, h, posted, "working|unread: cleared")
		})
	}
}

// The same holds for a working lead with nothing open: no post either way.
func TestD6UnreadHelperFailureWorking(t *testing.T) {
	s := &d6Script{failUnread: 1}
	h, f, w := d6Watch(t, s)
	pdOK(t, w, 0, 0, w.d6)
	wantRows(t, h)
	if n := countCalls(f, "nudge "); n != 0 {
		t.Errorf("nudged %d times on a failed scan", n)
	}
}

// A nudge helper that failed to run is a plain refusal, whatever it printed:
// its output is not the op's, so it never latches the anchor stop.
func TestNudgeHelperFailure(t *testing.T) {
	for _, rc := range helperFailures {
		t.Run(strconv.Itoa(rc), func(t *testing.T) {
			s := &d6Script{nudgeOut: "anchor: pane %1 runs bash, not claude", failNudge: rc}
			h, f, w := d6Watch(t, s)
			s.dispatcher = fmt.Sprintf("%d %d", agedMS(w, 0, 700), agedMS(w, 0, 650))
			pdOK(t, w, 0, 0, w.d6)
			if w.pd.nudgeOff || w.pd.nudgedTS != 0 {
				t.Errorf("nudgeOff %v nudgedTS %d, want unchanged", w.pd.nudgeOff, w.pd.nudgedTS)
			}
			if countCalls(f, "unread ") != 2 {
				t.Errorf("verdict skipped: %q", f.shCalls)
			}
			wantRows(t, h)
		})
	}
}

// A release helper that failed to run is retried like a refused release.
func TestReleaseHelperFailureKeepsWatching(t *testing.T) {
	for _, rc := range helperFailures {
		t.Run(strconv.Itoa(rc), func(t *testing.T) {
			f := newFake()
			f.sample = sampling(frameText(t, "done_idle.txt"))
			f.sh = func(string, ...string) (string, int) {
				if len(f.shCalls) == 1 {
					return "", rc
				}
				return "", shOffset
			}
			_, w := releaseWatch(t, f, "worker:feat/x", `{"state":"done"}`, "--engine", "claude")
			if code, ok := codeOf(w.finishedRelease()); !ok || code != 0 {
				t.Fatalf("finishedRelease = %d, %v", code, ok)
			}
			if len(f.shCalls) != 2 || f.samples != 5 {
				t.Errorf("sh calls %d samples %d, want 2 and 5", len(f.shCalls), f.samples)
			}
		})
	}
}
