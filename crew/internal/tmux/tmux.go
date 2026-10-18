// Package tmux is the one seam to the tmux binary, so callers can be tested
// against a scripted Client instead of a live server.
package tmux

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// waitDelay bounds Wait after the context is done, for a child that left a
// grandchild holding the stdout pipe.
const waitDelay = 2 * time.Second

// Client runs tmux. Run is `tmux args… 2>/dev/null`: stdout with its trailing
// newlines stripped, returned even on failure as $(…) keeps it, and a non-nil
// error for a non-zero exit, a tmux not on PATH, or a done ctx.
type Client interface {
	Run(ctx context.Context, args ...string) (string, error)
}

// Exec is the Client that execs the tmux on PATH. Each call is a child in its
// own process group, killed as a group when ctx is done, so a PATH stub that
// forks dies with it; Wait is bounded by WaitDelay.
type Exec struct{}

// Run implements Client.
func (Exec) Run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "tmux", args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = waitDelay
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	return strings.TrimRight(out.String(), "\n"), err
}

// Output is c.Run for a read whose failure is no output, as `$(tmux … || true)`
// blanked it, rather than whatever a failing tmux printed.
func Output(ctx context.Context, c Client, args ...string) string {
	out, err := c.Run(ctx, args...)
	if err != nil {
		return ""
	}
	return out
}
