package data

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Runner is the seam every read-only source call goes through — an exec
// implementation in production, a fake in tests. It never sees a shell: args
// are passed straight to the named program.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) (stdout, stderr []byte, err error)
	// RunIn is Run with dir as the working directory; "" means the caller's.
	RunIn(ctx context.Context, dir, name string, args ...string) (stdout, stderr []byte, err error)
}

// ExecRunner runs a real subprocess.
type ExecRunner struct{}

func (e ExecRunner) Run(ctx context.Context, name string, args ...string) ([]byte, []byte, error) {
	return e.RunIn(ctx, "", name, args...)
}

func (ExecRunner) RunIn(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	// A grandchild (e.g. an ssh ProxyCommand) can keep the pipes open after the
	// kill; WaitDelay bounds Wait so the context deadline still holds.
	cmd.WaitDelay = 2 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

type exitCoder interface{ ExitCode() int }

// exitCode extracts the process exit code from a Run error: 0 when err is
// nil, 127 when the program could not even be started (matching a shell's
// "command not found").
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ec exitCoder
	if errors.As(err, &ec) {
		return ec.ExitCode()
	}
	return 127
}

// FailureLine is the first stderr line of a failed Run, or "exit <rc>" when
// there is none: the one-line reason an action reports.
func FailureLine(stderr []byte, err error) string {
	return firstLineOrExit(stderr, exitCode(err))
}

// FakeExitError lets a FakeRunner response report a non-zero exit without a
// real subprocess; it satisfies exitCoder the same way *exec.ExitError does.
type FakeExitError struct{ Code int }

func (e FakeExitError) Error() string { return fmt.Sprintf("exit status %d", e.Code) }
func (e FakeExitError) ExitCode() int { return e.Code }

// FakeRunner is the test double: canned responses keyed by "name arg1 arg2
// …", every call recorded for assertions (e.g. "roster was re-collected
// exactly once"). Run is safe for concurrent use; concurrent readers should
// use CallsSnapshot rather than Calls.
type FakeRunner struct {
	Responses map[string]FakeResponse
	Calls     []FakeCall

	mu sync.Mutex
}

type FakeCall struct {
	Dir  string
	Name string
	Args []string
}

// FakeResponse.Wait, when non-nil, holds the call (after it is recorded)
// until the channel closes or ctx is done, so tests can keep a source open.
type FakeResponse struct {
	Stdout []byte
	Stderr []byte
	Err    error
	Wait   <-chan struct{}
}

func (f *FakeRunner) Run(ctx context.Context, name string, args ...string) ([]byte, []byte, error) {
	return f.RunIn(ctx, "", name, args...)
}

func (f *FakeRunner) RunIn(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
	key := fakeKey(name, args)
	f.mu.Lock()
	f.Calls = append(f.Calls, FakeCall{Dir: dir, Name: name, Args: append([]string{}, args...)})
	resp, ok := f.Responses[key]
	f.mu.Unlock()
	if !ok {
		return nil, nil, fmt.Errorf("fake runner: no response for %q", key)
	}
	if resp.Wait != nil {
		select {
		case <-resp.Wait:
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
	}
	return resp.Stdout, resp.Stderr, resp.Err
}

func (f *FakeRunner) CallsSnapshot() []FakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]FakeCall(nil), f.Calls...)
}

func fakeKey(name string, args []string) string {
	return strings.Join(append([]string{name}, args...), " ")
}
