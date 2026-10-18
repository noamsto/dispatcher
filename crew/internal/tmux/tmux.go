// Package tmux is the one seam to the tmux binary.
package tmux

import (
	"context"
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

// Exec runs the tmux on PATH. WaitDelay bounds Wait if a stub's child holds stdout.
type Exec struct{}

func (Exec) Run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "tmux", args...)
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
