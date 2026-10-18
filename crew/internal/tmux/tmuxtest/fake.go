// Package tmuxtest is a scripted tmux.Client for tests.
package tmuxtest

import (
	"context"
	"errors"
	"strings"
	"sync"

	"github.com/noamsto/dispatcher/crew/internal/tmux"
)

// ErrNoServer is what a call no Out key answers fails with.
var ErrNoServer = errors.New("tmuxtest: no server")

var _ tmux.Client = (*Fake)(nil)

// Fake is a scripted tmux.Client, safe for concurrent use. Func, when set,
// answers every call; otherwise the longest Out key that is a word prefix of
// the space-joined argv answers, and an unmatched call fails with ErrNoServer.
type Fake struct {
	Out  map[string]string
	Func func(args []string) (string, error)

	mu    sync.Mutex
	calls []string
}

func (f *Fake) Run(_ context.Context, args ...string) (string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, strings.Join(args, " "))
	f.mu.Unlock()

	if f.Func != nil {
		return f.Func(args)
	}
	for n := len(args); n > 0; n-- {
		if out, ok := f.Out[strings.Join(args[:n], " ")]; ok {
			return out, nil
		}
	}
	return "", ErrNoServer
}

// Calls returns the space-joined argv of each call, in order.
func (f *Fake) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}
