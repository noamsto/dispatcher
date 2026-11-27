package supervisor

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/spikes/headless-supervisor/internal/event"
)

func startRun(t *testing.T, policy func(*event.Request) event.Decision) (*Run, *fakeEngine) {
	t.Helper()
	f := newFake(event.Options{SessionID: "sess-1"})
	r := &Run{Engine: f, Policy: policy}
	if err := r.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Shutdown(time.Second) })
	return r, f
}

func TestTurnCollectsText(t *testing.T) {
	r, _ := startRun(t, nil)
	mark := r.Mark()
	end, err := r.Turn("hi", 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if end.Kind != event.KindTurnEnd {
		t.Fatalf("got %s", end.Kind)
	}
	if got := r.TextSince(mark); got != "echo:hi" {
		t.Fatalf("text = %q", got)
	}
	if r.LastEvent().IsZero() {
		t.Fatal("LastEvent not set")
	}
}

func TestPolicyAnswersPermission(t *testing.T) {
	var seen *event.Request
	r, f := startRun(t, func(q *event.Request) event.Decision {
		seen = q
		return event.Decision{Verdict: "deny", Reason: "no"}
	})
	if _, err := r.Turn("ask", 2*time.Second); err != nil {
		t.Fatal(err)
	}
	if seen == nil || seen.Detail.Command != "touch x" {
		t.Fatalf("policy saw %+v", seen)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.responded) != 1 || f.responded[0].Verdict != "deny" {
		t.Fatalf("responded = %+v", f.responded)
	}
}

func TestNilPolicyDenies(t *testing.T) {
	r, f := startRun(t, nil)
	if _, err := r.Turn("ask", 2*time.Second); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.responded) != 1 || f.responded[0].Verdict != "deny" {
		t.Fatalf("responded = %+v", f.responded)
	}
}

func TestWaitForTimeoutAndExit(t *testing.T) {
	r, _ := startRun(t, nil)
	_, err := r.WaitFor(func(e event.Event) bool { return e.Kind == event.KindRetry }, 50*time.Millisecond)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	if err := r.Shutdown(time.Second); err != nil {
		t.Fatal(err)
	}
	_, err = r.WaitFor(func(e event.Event) bool { return e.Kind == event.KindRetry }, time.Second)
	if !errors.Is(err, ErrExited) {
		t.Fatalf("err = %v, want ErrExited", err)
	}
}

func TestWaitForSeesExitEvent(t *testing.T) {
	r, _ := startRun(t, nil)
	go func() { _ = r.Engine.Close() }()
	ev, err := r.WaitKind(event.KindExit, 2*time.Second)
	if err != nil || ev.Kind != event.KindExit {
		t.Fatalf("ev=%+v err=%v", ev, err)
	}
}

func TestRespondErrorFallsBackToDeny(t *testing.T) {
	r, f := startRun(t, func(*event.Request) event.Decision { return event.Decision{Verdict: "allow_once"} })
	f.mu.Lock()
	f.failRespond = 1
	f.mu.Unlock()
	if _, err := r.Turn("ask", 2*time.Second); err != nil {
		t.Fatalf("child left waiting: %v", err)
	}
	f.mu.Lock()
	got := append([]event.Decision(nil), f.responded...)
	f.mu.Unlock()
	if len(got) != 2 || got[0].Verdict != "allow_once" || got[1].Verdict != "deny" ||
		got[1].Reason != "supervisor error: respond broke" {
		t.Fatalf("responded = %+v", got)
	}
	var logged bool
	for _, ev := range r.Events() {
		if ev.Kind == event.KindError && strings.Contains(ev.Text, "respond broke") {
			logged = true
		}
	}
	if !logged {
		t.Fatal("first Respond failure not recorded")
	}
}
