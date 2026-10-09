package stall

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"testing"
	"time"
)

func TestNotifySignalsExitCode(t *testing.T) {
	for _, tc := range []struct {
		name string
		sig  syscall.Signal
		want int
	}{
		{"SIGTERM", syscall.SIGTERM, 143},
		{"SIGINT", syscall.SIGINT, 130},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, stop := NotifySignals(context.Background())
			defer stop()
			if err := syscall.Kill(os.Getpid(), tc.sig); err != nil {
				t.Fatal(err)
			}
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
				t.Fatal("context not cancelled within 1s")
			}
			var se SignalError
			if !errors.As(context.Cause(ctx), &se) || se.Sig != tc.sig {
				t.Fatalf("cause = %v, want SignalError{%v}", context.Cause(ctx), tc.sig)
			}
			if got := exitCodeFor(ctx); got != tc.want {
				t.Fatalf("exitCodeFor = %d, want %d", got, tc.want)
			}
			stop() // safe after a signal
		})
	}
}

func TestSignalErrorMessage(t *testing.T) {
	if got := (SignalError{syscall.SIGTERM}).Error(); got != "signal: terminated" {
		t.Fatalf("Error() = %q", got)
	}
}

func TestExitCodePlainCancel(t *testing.T) {
	ctx, stop := NotifySignals(context.Background())
	stop()
	if ctx.Err() == nil {
		t.Fatal("stop did not cancel the context")
	}
	if got := exitCodeFor(ctx); got != 0 {
		t.Fatalf("exitCodeFor = %d, want 0", got)
	}
}

func TestExitCodeParentCancel(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	ctx, stop := NotifySignals(parent)
	defer stop()
	cancel()
	<-ctx.Done()
	if got := exitCodeFor(ctx); got != 0 {
		t.Fatalf("exitCodeFor = %d, want 0", got)
	}
}

func TestNotifySignalsKeepsSighupIgnored(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperHup$")
	cmd.Env = append(os.Environ(), "STALL_HELPER_HUP=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("helper failed: %v\n%s", err, out)
	}
}

func TestHelperHup(t *testing.T) {
	if os.Getenv("STALL_HELPER_HUP") != "1" {
		t.Skip("child-process helper")
	}
	signal.Ignore(syscall.SIGHUP)
	_, stop := NotifySignals(context.Background())
	defer stop()
	if !signal.Ignored(syscall.SIGHUP) {
		t.Fatal("NotifySignals un-ignored SIGHUP")
	}
}
