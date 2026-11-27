package supervisor

import (
	"sync"
	"time"

	"github.com/noamsto/dispatcher/spikes/headless-supervisor/internal/event"
)

// Status is the Monitor's verdict.
type Status string

const (
	StatusIdle   Status = "idle"   // no turn open
	StatusActive Status = "active" // turn open, events recent
	StatusStale  Status = "stale"  // turn open, silent for Silence
	StatusExited Status = "exited"
)

// Monitor reports a turn as stale when no event has arrived for Silence while
// the turn is open. The zero value is usable: real clock, never stale.
type Monitor struct {
	Clock   func() time.Time
	Silence time.Duration

	mu     sync.Mutex
	last   time.Time
	open   bool
	exited bool
}

func (m *Monitor) now() time.Time {
	if m.Clock != nil {
		return m.Clock()
	}
	return time.Now()
}

// Begin opens a turn (a user message was sent).
func (m *Monitor) Begin() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.open = true
	m.last = m.now()
}

// Observe feeds one engine event.
func (m *Monitor) Observe(ev event.Event) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.last = m.now()
	switch ev.Kind {
	case event.KindTurnStart:
		m.open = true
	case event.KindTurnEnd:
		m.open = false
	case event.KindExit:
		m.open = false
		m.exited = true
	}
}

func (m *Monitor) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch {
	case m.exited:
		return StatusExited
	case !m.open:
		return StatusIdle
	case m.Silence > 0 && m.now().Sub(m.last) >= m.Silence:
		return StatusStale
	}
	return StatusActive
}

// WaitStale polls until the monitor reports stale, logs "stale detected" and
// returns nil; it errors on timeout or when the engine exits first.
func (r *Run) WaitStale(timeout time.Duration) error {
	poll := min(max(r.Mon.Silence/10, 10*time.Millisecond), time.Second)
	deadline := time.Now().Add(timeout)
	for {
		switch r.Mon.Status() {
		case StatusStale:
			r.Note("stale detected")
			return nil
		case StatusExited:
			return ErrExited
		}
		if time.Now().After(deadline) {
			return ErrTimeout
		}
		time.Sleep(poll)
	}
}
