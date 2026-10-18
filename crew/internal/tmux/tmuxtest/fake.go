// Package tmuxtest is a scripted tmux.Client for tests.
package tmuxtest

import (
	"context"
	"errors"
	"strings"
	"sync"

	"github.com/noamsto/dispatcher/crew/internal/tmux"
)

// ErrNoServer is what a call no Out key answers fails with: tmux with no server.
var ErrNoServer = errors.New("tmuxtest: no server")

var _ tmux.Client = (*Fake)(nil)

// Fake is a scripted tmux.Client, safe for concurrent use. Every call is
// recorded as its space-joined argv. Func, when set, answers every call;
// otherwise the longest Out key that is the joined argv or a word prefix of
// it answers, and a call no key matches fails with "" and ErrNoServer.
type Fake struct {
	Out  map[string]string
	Func func(args []string) (string, error)

	mu    sync.Mutex
	calls []string
}

// Run implements tmux.Client.
func (f *Fake) Run(_ context.Context, args ...string) (string, error) {
	joined := strings.Join(args, " ")
	f.mu.Lock()
	f.calls = append(f.calls, joined)
	f.mu.Unlock()

	if f.Func != nil {
		return f.Func(args)
	}
	best, found := "", false
	for k := range f.Out {
		if (joined == k || strings.HasPrefix(joined, k+" ")) && (!found || len(k) > len(best)) {
			best, found = k, true
		}
	}
	if !found {
		return "", ErrNoServer
	}
	return f.Out[best], nil
}

// Calls returns the recorded argv strings in call order (a copy).
func (f *Fake) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}
