package reply

import (
	"os"
	"os/exec"
	"syscall"

	"github.com/noamsto/dispatcher/crew/internal/entry"
)

// DetachedNudge is the production Options.Nudge: it starts the lead's
// `crew nudge --wait` in its own session and returns as soon as it is running,
// so a reply never waits on the wait it just started.
func DetachedNudge() func(argv []string, logPath string) error {
	return detachedNudge(entry.CrewEntry(os.Getenv, exec.LookPath))
}

// detachedNudge builds the starter around one entrypoint; a test passes its own
// self to prove an exec failure surfaces instead of vanishing.
func detachedNudge(self string) func(argv []string, logPath string) error {
	return func(argv []string, logPath string) error {
		cmd, err := startDetached(self, argv, logPath)
		if err != nil {
			return err
		}
		return cmd.Process.Release()
	}
}

// startDetached builds and starts the wait process; the caller owns the returned
// command, so a test can wait on it. Production releases it.
func startDetached(self string, argv []string, logPath string) (*exec.Cmd, error) {
	cmd := exec.Command(self, argv...)
	// A nil Stdin is /dev/null: the wait reads nothing, and it must never
	// read the dispatcher's own input.
	cmd.Stdin = nil
	// The log is next to the wait loop's nudge-wait/ lock. Append both streams
	// to it so the started process leaves its own trace. 0o600 and O_NOFOLLOW:
	// the log is the caller's private file and is never a symlink swapped
	// under the open.
	f, err := os.OpenFile(logPath, os.O_WRONLY|os.O_APPEND|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	cmd.Stdout, cmd.Stderr = f, f
	// Setsid leaves the caller's session and process group, so a dispatcher
	// that exits does not take the wait with it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		_ = f.Close()
		return nil, err
	}
	_ = f.Close()
	return cmd, nil
}
