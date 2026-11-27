package supervisor

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/noamsto/dispatcher/spikes/headless-supervisor/internal/event"
)

// fakeEngine answers each Send with TurnStart, an optional permission round
// trip, Text "echo:<msg>" and TurnEnd.
type fakeEngine struct {
	opts   event.Options
	events chan event.Event

	mu        sync.Mutex
	sessionID string
	closed    bool
	responded []event.Decision
	interrupt int
	sent      []string

	failRespond int   // Respond fails this many times before succeeding
	stuck       bool  // Close and Kill leave the event stream open
	closeErr    error // Close ends the stream but reports this, like a non-zero exit
}

func newFake(o event.Options) *fakeEngine {
	return &fakeEngine{opts: o, events: make(chan event.Event, 64), sessionID: o.SessionID}
}

func (f *fakeEngine) emit(ev event.Event) {
	ev.At = time.Now()
	f.events <- ev
}

func (f *fakeEngine) Start(context.Context) error {
	f.emit(event.Event{Kind: event.KindInit, SessionID: f.sessionID})
	return nil
}
func (f *fakeEngine) Events() <-chan event.Event { return f.events }

func (f *fakeEngine) Send(text string) error {
	f.mu.Lock()
	f.sent = append(f.sent, text)
	f.mu.Unlock()
	f.emit(event.Event{Kind: event.KindTurnStart})
	switch text {
	case "hold":
		return nil
	case "ask":
		f.emit(event.Event{Kind: event.KindPermission, Req: &event.Request{ID: "r1", Tool: "Bash", Detail: event.Detail{Kind: "bash", Command: "touch x"}}})
		return nil
	}
	f.emit(event.Event{Kind: event.KindText, Text: "echo:" + text})
	f.emit(event.Event{Kind: event.KindTurnEnd})
	return nil
}

func (f *fakeEngine) Interrupt() error {
	f.mu.Lock()
	f.interrupt++
	f.mu.Unlock()
	f.emit(event.Event{Kind: event.KindTurnEnd, IsError: true, Text: "aborted"})
	return nil
}

func (f *fakeEngine) Respond(r *event.Request, d event.Decision) error {
	f.mu.Lock()
	f.responded = append(f.responded, d)
	if f.failRespond > 0 {
		f.failRespond--
		f.mu.Unlock()
		return errors.New("respond broke")
	}
	f.mu.Unlock()
	f.emit(event.Event{Kind: event.KindText, Text: "decided:" + string(d.Verdict)})
	f.emit(event.Event{Kind: event.KindTurnEnd})
	return nil
}

func (f *fakeEngine) SessionID() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sessionID
}

func (f *fakeEngine) setStuck(v bool) {
	f.mu.Lock()
	f.stuck = v
	f.mu.Unlock()
}

func (f *fakeEngine) setSession(id string) {
	f.mu.Lock()
	f.sessionID = id
	f.mu.Unlock()
}

func (f *fakeEngine) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed || f.stuck {
		return nil
	}
	f.closed = true
	f.events <- event.Event{Kind: event.KindExit, At: time.Now()}
	close(f.events)
	return f.closeErr
}

func (f *fakeEngine) Kill() error { return errors.Join(f.Close()) }
func (f *fakeEngine) Wait() error { return nil }
