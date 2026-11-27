// Package supervisor drives one engine child: it pumps events, answers
// permission requests through a policy, and offers the wait/turn primitives the
// cases and the control socket build on.
package supervisor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/noamsto/dispatcher/spikes/headless-supervisor/internal/event"
	"github.com/noamsto/dispatcher/spikes/headless-supervisor/internal/transcript"
)

// Factory builds an engine from options; the CLI fills a Registry with the
// adapters so this package never imports them.
type Factory func(event.Options) (event.Engine, error)

// Registry maps an engine name to its Factory.
type Registry map[string]Factory

func (r Registry) New(name string, o event.Options) (event.Engine, error) {
	f, ok := r[name]
	if !ok {
		return nil, fmt.Errorf("unknown engine %q", name)
	}
	return f(o)
}

var (
	ErrTimeout = errors.New("timed out waiting for event")
	ErrExited  = errors.New("engine exited before the event arrived")
)

// Run owns one started engine. Every event is recorded; Permission events are
// answered by Policy (nil Policy denies).
type Run struct {
	Engine event.Engine
	T      *transcript.Writer // optional
	Policy func(*event.Request) event.Decision
	Mon    *Monitor // optional; Start creates one with no silence limit

	mu       sync.Mutex
	events   []event.Event
	cursor   int
	last     time.Time
	notify   chan struct{}
	finished bool
	done     chan struct{}
}

// Start starts the engine and begins pumping its events.
func (r *Run) Start(ctx context.Context) error {
	if r.Mon == nil {
		r.Mon = &Monitor{}
	}
	r.notify = make(chan struct{})
	r.done = make(chan struct{})
	if err := r.Engine.Start(ctx); err != nil {
		return err
	}
	go r.pump()
	return nil
}

func (r *Run) pump() {
	defer close(r.done)
	for ev := range r.Engine.Events() {
		r.record(ev)
		if ev.Kind == event.KindPermission && ev.Req != nil {
			r.answer(ev.Req)
		}
	}
	r.mu.Lock()
	r.finished = true
	r.wake()
	r.mu.Unlock()
}

func (r *Run) record(ev event.Event) {
	r.Mon.Observe(ev)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
	r.last = time.Now()
	r.wake()
}

// wake releases every WaitFor sleeper; callers hold r.mu.
func (r *Run) wake() {
	close(r.notify)
	r.notify = make(chan struct{})
}

func (r *Run) answer(req *event.Request) {
	d := event.Decision{Verdict: "deny", Reason: "no policy"}
	if r.Policy != nil {
		d = r.Policy(req)
	}
	if err := r.respond(req, d); err != nil {
		// A child left without an answer waits forever; fall back to a deny once.
		_ = r.respond(req, event.Decision{Verdict: "deny", Reason: "supervisor error: " + err.Error()})
	}
}

// respond logs d, sends it, and records a failure as an Error event.
func (r *Run) respond(req *event.Request, d event.Decision) error {
	r.note(map[string]any{
		"sup": "decision", "id": req.ID, "engine": req.Engine, "tool": req.Tool,
		"kind": req.Detail.Kind, "command": req.Detail.Command, "path": req.Detail.Path,
		"url": req.Detail.URL, "server": req.Detail.Server,
		"verdict": d.Verdict, "reason": d.Reason,
	})
	err := r.Engine.Respond(req, d)
	if err != nil {
		r.record(event.Event{Kind: event.KindError, At: time.Now(), Text: "respond: " + err.Error(), IsError: true})
	}
	return err
}

func (r *Run) note(v any) {
	if r.T != nil {
		_ = r.T.Note(v)
	}
}

// Note logs a supervisor remark to the transcript.
func (r *Run) Note(text string) { r.note(map[string]string{"sup": text}) }

// Events returns a copy of every event recorded so far.
func (r *Run) Events() []event.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]event.Event(nil), r.events...)
}

// LastEvent is when the latest event was recorded; zero before the first.
func (r *Run) LastEvent() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.last
}

// Mark is an index into the event history for TextSince.
func (r *Run) Mark() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.events)
}

// TextSince joins the Text events recorded at or after mark.
func (r *Run) TextSince(mark int) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var b strings.Builder
	for _, ev := range r.events[min(mark, len(r.events)):] {
		if ev.Kind == event.KindText {
			b.WriteString(ev.Text)
		}
	}
	return b.String()
}

// WaitFor consumes events in order, skipping those that fail pred, and returns
// the first that matches. Events consumed here are not seen by later calls.
func (r *Run) WaitFor(pred func(event.Event) bool, timeout time.Duration) (event.Event, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		r.mu.Lock()
		var ev event.Event
		have := r.cursor < len(r.events)
		if have {
			ev = r.events[r.cursor]
			r.cursor++
		}
		ch, finished := r.notify, r.finished
		r.mu.Unlock()

		switch {
		case have:
			if pred(ev) {
				return ev, nil
			}
		case finished:
			return event.Event{}, ErrExited
		default:
			select {
			case <-ch:
			case <-timer.C:
				return event.Event{}, ErrTimeout
			}
		}
	}
}

// WaitKind is WaitFor on a single event kind.
func (r *Run) WaitKind(k event.Kind, timeout time.Duration) (event.Event, error) {
	return r.WaitFor(func(e event.Event) bool { return e.Kind == k }, timeout)
}

// SkipPending discards events recorded so far so the next WaitFor sees only new ones.
func (r *Run) SkipPending() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cursor = len(r.events)
}

// Send is a user message; it opens a turn for liveness purposes.
func (r *Run) Send(text string) error {
	r.Mon.Begin()
	return r.Engine.Send(text)
}

// Turn discards pending events, sends text and waits for the turn to end.
func (r *Run) Turn(text string, timeout time.Duration) (event.Event, error) {
	r.SkipPending()
	if err := r.Send(text); err != nil {
		return event.Event{}, err
	}
	return r.WaitKind(event.KindTurnEnd, timeout)
}

// Done is closed once the engine's event stream has ended.
func (r *Run) Done() <-chan struct{} { return r.done }

// Shutdown closes the engine gracefully and kills it when grace elapses.
func (r *Run) Shutdown(grace time.Duration) error {
	res := make(chan error, 1)
	go func() { res <- r.Engine.Close() }()
	var err error
	select {
	case err = <-res:
	case <-time.After(grace):
		kerr := r.Engine.Kill()
		select {
		case <-res:
			err = kerr
		case <-time.After(grace):
			err = errors.Join(kerr, errors.New("shutdown: engine did not exit after kill"))
		}
	}
	select {
	case <-r.done:
	case <-time.After(grace):
		err = errors.Join(err, errors.New("shutdown: event stream still open"))
	}
	return err
}
