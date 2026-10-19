// Package gittest is a scripted git.Client for tests.
package gittest

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/noamsto/dispatcher/crew/internal/git"
)

var _ git.Client = (*Fake)(nil)

// Fake is a scripted git.Client. Func, when set, answers every call; otherwise
// Out[key] does, and an unmatched call fails with a git.ExitError of 128.
type Fake struct {
	Out  map[string]string
	Func func(dir string, args []string) (string, error)

	mu    sync.Mutex
	calls []string
}

// key is the space-joined argv, prefixed "<dir>: " when dir is set.
func key(dir string, args ...string) string {
	k := strings.Join(args, " ")
	if dir != "" {
		return dir + ": " + k
	}
	return k
}

func (f *Fake) Run(_ context.Context, dir string, args ...string) (string, error) {
	k := key(dir, args...)
	f.mu.Lock()
	f.calls = append(f.calls, k)
	f.mu.Unlock()

	if f.Func != nil {
		return f.Func(dir, args)
	}
	if out, ok := f.Out[k]; ok {
		return out, nil
	}
	return "", &git.ExitError{Code: 128, Stderr: fmt.Sprintf("gittest: no answer for %s\n", k)}
}

// Calls returns the key of each call, in order.
func (f *Fake) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}
