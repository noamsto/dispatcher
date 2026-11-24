package reply

import (
	"os"
	"os/exec"
	"syscall"
)

// DetachedNudge is the production Options.Nudge: it starts the lead's
// `crew nudge --wait` in its own session and returns as soon as it is running,
// so a reply never waits on the wait it just started.
func DetachedNudge() func(argv []string, logPath string) error {
	return detachedNudge(crewEntry())
}

// detachedNudge builds the starter around one entrypoint; a test passes its own
// self to prove an exec failure surfaces instead of vanishing.
func detachedNudge(self string) func(argv []string, logPath string) error {
	return func(argv []string, logPath string) error {
		cmd := exec.Command(self, argv...)
		// A nil Stdin is /dev/null: the wait reads nothing, and it must never
		// read the dispatcher's own input.
		cmd.Stdin = nil
		// The log is next to the wait loop's nudge-wait/ lock. Append both
		// streams to it so the started process leaves its own trace.
		_, statErr := os.Stat(logPath)
		f, err := os.OpenFile(logPath, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o666)
		if err != nil {
			return err
		}
		cmd.Stdout, cmd.Stderr = f, f
		// Setsid leaves the caller's session and process group, so a dispatcher
		// that exits does not take the wait with it.
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := cmd.Start(); err != nil {
			_ = f.Close()
			// A fresh log the failed start created is not a log the caller
			// wrote; drop it so "could not start" leaves nothing behind.
			if os.IsNotExist(statErr) {
				_ = os.Remove(logPath)
			}
			return err
		}
		_ = f.Close()
		return cmd.Process.Release()
	}
}

// crewEntry is the `crew` the starter re-enters: $CREW_SELF, then the one on
// PATH, then the bare name — stream's crewScript, kept here rather than importing
// the loop.
func crewEntry() string {
	if self := os.Getenv("CREW_SELF"); self != "" {
		return self
	}
	if self, err := exec.LookPath("crew"); err == nil {
		return self
	}
	return "crew"
}
