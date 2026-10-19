// Package ghtest is a scripted gh.Client for tests.
package ghtest

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/noamsto/dispatcher/crew/internal/gh"
)

var _ gh.Client = (*Fake)(nil)

// Fake is a scripted gh.Client; Run and Combined answer alike. Func, when set,
// answers every call; otherwise Out[key] does, and an unmatched call fails.
type Fake struct {
	Out  map[string]string
	Func func(args []string) (string, error)

	mu    sync.Mutex
	calls []string
}

func (f *Fake) Run(_ context.Context, args ...string) (string, error) { return f.answer(args) }

func (f *Fake) Combined(_ context.Context, args ...string) (string, error) { return f.answer(args) }

func (f *Fake) answer(args []string) (string, error) {
	k := strings.Join(args, " ")
	f.mu.Lock()
	f.calls = append(f.calls, k)
	f.mu.Unlock()

	if f.Func != nil {
		return f.Func(args)
	}
	if out, ok := f.Out[k]; ok {
		return out, nil
	}
	return "", fmt.Errorf("ghtest: no answer for %s", k)
}

// Calls returns the space-joined argv of each call, in order.
func (f *Fake) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}
