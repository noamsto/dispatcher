// Package gh is the one seam to the gh binary.
package gh

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strings"
)

// Client runs `gh args…`. Both methods return the output with trailing
// newlines stripped, kept on failure as `$(… || true)` keeps it, and the exit
// status as an error. A gh that is not installed returns an error wrapping
// exec.ErrNotFound (see Missing).
type Client interface {
	// Run drops stderr.
	Run(ctx context.Context, args ...string) (string, error)
	// Combined merges stderr into the result, as `gh … 2>&1`.
	Combined(ctx context.Context, args ...string) (string, error)
}

// Exec runs the gh on PATH.
type Exec struct{}

func (Exec) Run(ctx context.Context, args ...string) (string, error) {
	return run(ctx, args, false)
}

func (Exec) Combined(ctx context.Context, args ...string) (string, error) {
	return run(ctx, args, true)
}

func run(ctx context.Context, args []string, merge bool) (string, error) {
	cmd := exec.CommandContext(ctx, "gh", args...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	if merge {
		cmd.Stderr = &buf
	}
	err := cmd.Run()
	return strings.TrimRight(buf.String(), "\n"), err
}

// Missing reports that err is gh not being installed.
func Missing(err error) bool { return errors.Is(err, exec.ErrNotFound) }

// Installed is `command -v gh`: a no-op call that starts gh.
func Installed(ctx context.Context, c Client) bool {
	_, err := c.Run(ctx, "--version")
	return !Missing(err)
}
