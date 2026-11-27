package supervisor

import (
	"sync"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/spikes/headless-supervisor/internal/event"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func TestMonitor(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	m := &Monitor{Clock: clk.Now, Silence: 10 * time.Second}

	want := func(s Status) {
		t.Helper()
		if got := m.Status(); got != s {
			t.Fatalf("status = %s, want %s", got, s)
		}
	}
	want(StatusIdle)
	clk.Advance(time.Hour)
	want(StatusIdle) // no open turn: silence is not staleness

	m.Observe(event.Event{Kind: event.KindTurnStart})
	want(StatusActive)
	clk.Advance(9 * time.Second)
	want(StatusActive)
	m.Observe(event.Event{Kind: event.KindToolStart}) // activity resets the window
	clk.Advance(9 * time.Second)
	want(StatusActive)
	clk.Advance(time.Second)
	want(StatusStale)

	m.Observe(event.Event{Kind: event.KindTurnEnd})
	want(StatusIdle)

	m.Begin()
	clk.Advance(10 * time.Second)
	want(StatusStale)

	m.Observe(event.Event{Kind: event.KindExit})
	want(StatusExited)
}

func TestMonitorZeroSilenceNeverStale(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1, 0)}
	m := &Monitor{Clock: clk.Now}
	m.Begin()
	clk.Advance(24 * time.Hour)
	if got := m.Status(); got != StatusActive {
		t.Fatalf("status = %s, want active", got)
	}
}

func TestWaitStaleDetectsHungTurn(t *testing.T) {
	f := newFake(event.Options{SessionID: "s"})
	r := &Run{Engine: f, Mon: &Monitor{Silence: 50 * time.Millisecond}}
	if err := r.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Shutdown(time.Second) }()
	if err := r.Send("hold"); err != nil {
		t.Fatal(err)
	}
	if err := r.WaitStale(5 * time.Second); err != nil {
		t.Fatal(err)
	}
}
