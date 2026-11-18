package reap

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/lock"
)

const (
	// priorsImportEvery is the machine-wide minimum gap between two imports:
	// reap runs on every dispatch and from every stream.
	priorsImportEvery = time.Hour
	priorsStampFile   = "priors-import.stamp"
	priorsLockDir     = "priors-import.lock.d"
)

// priorsImportTimeout bounds one import so a hung push cannot pin the child.
var priorsImportTimeout = 5 * time.Minute

// importPriors sweeps agents' native memories into the priors fact store. It
// runs in the detached autosweep child, so its output is the autosweep log and
// its result never reaches reap's status. The stamp records when the last run
// started, so a failing import is not retried more than once an hour either.
func importPriors(store string, o Options, stdout, stderr io.Writer) {
	bin, err := exec.LookPath("priors")
	if err != nil {
		_, _ = fmt.Fprintf(stdout, "%s priors not on PATH — import skipped\n", o.stamp())
		return
	}
	ld := store + "/" + priorsLockDir
	if !lock.Acquire(ld, strconv.Itoa(o.PID)) {
		_, _ = fmt.Fprintf(stdout, "%s a priors import is already running — skipped\n", o.stamp())
		return
	}
	clean := &cleaner{}
	clean.hold(ld)
	stop := clean.notify()
	defer func() {
		clean.cleanup()
		stop()
	}()

	now := o.Probes.Clock.Now()
	stampPath := store + "/" + priorsStampFile
	if last, ok := readStamp(stampPath); ok && now.Sub(last) >= 0 && now.Sub(last) < priorsImportEvery {
		return
	}
	_ = os.WriteFile(stampPath, []byte(strconv.FormatInt(now.Unix(), 10)+"\n"), 0o666)

	_, _ = fmt.Fprintf(stdout, "%s priors import start pid=%d\n", o.stamp(), o.PID)
	ctx, cancel := context.WithTimeout(context.Background(), priorsImportTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "import")
	cmd.Stdout, cmd.Stderr = stdout, stderr
	// Own process group, so the timeout also kills what the import spawned
	// (a hung git or ssh), not just the direct child.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	rc := 0
	if err := cmd.Run(); err != nil {
		rc = -1
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			rc = ee.ExitCode()
		} else {
			_, _ = fmt.Fprintf(stderr, "priors import: %v\n", err)
		}
	}
	_, _ = fmt.Fprintf(stdout, "%s priors import done rc=%d\n", o.stamp(), rc)
}

// readStamp is the start time the stamp file records; a missing or unreadable
// stamp means no earlier run.
func readStamp(path string) (time.Time, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return time.Time{}, false
	}
	sec, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(sec, 0), true
}
