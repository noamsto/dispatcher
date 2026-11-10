// Package tmux is the one seam to the tmux binary.
package tmux

import (
	"context"
	"io"
	"os/exec"
	"strings"
	"time"
)

// Client runs tmux. Run is `tmux args… 2>/dev/null`: stdout with its trailing
// newlines stripped, returned even on failure as $(…) keeps it, and a non-nil
// error for a non-zero exit, a tmux not on PATH, or a done ctx.
type Client interface {
	Run(ctx context.Context, args ...string) (string, error)
}

// InputClient is a Client that can also feed tmux a stdin, for the
// `load-buffer -b <buf> -` that reads the buffer's text from it.
type InputClient interface {
	Client
	RunInput(ctx context.Context, input string, args ...string) (string, error)
}

// Exec runs the tmux on PATH. WaitDelay bounds Wait if a stub's child holds stdout.
type Exec struct{}

func (e Exec) Run(ctx context.Context, args ...string) (string, error) {
	return e.run(ctx, nil, args)
}

// RunInput is Run with input on tmux's stdin.
func (e Exec) RunInput(ctx context.Context, input string, args ...string) (string, error) {
	return e.run(ctx, strings.NewReader(input), args)
}

func (Exec) run(ctx context.Context, stdin io.Reader, args []string) (string, error) {
	cmd := exec.CommandContext(ctx, "tmux", args...)
	cmd.Stdin = stdin
	cmd.WaitDelay = 2 * time.Second
	out, err := cmd.Output()
	return strings.TrimRight(string(out), "\n"), err
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
