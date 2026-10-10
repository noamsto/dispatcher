package stall

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
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

// dispatch starts the watchdog as a non-interactive bash `&` job, which
// inherits SIGINT ignored; a Ctrl-C to dispatch's group must not reach it.
func TestNotifySignalsKeepsInheritedSigintIgnored(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not on PATH")
	}
	cmd := exec.Command(bash, "-c", `trap '' INT; exec "$0" -test.run='^TestHelperInt$'`, os.Args[0])
	cmd.Env = append(os.Environ(), "STALL_HELPER_INT=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("helper failed: %v\n%s", err, out)
	}
}

func TestHelperInt(t *testing.T) {
	if os.Getenv("STALL_HELPER_INT") != "1" {
		t.Skip("child-process helper")
	}
	if !signal.Ignored(syscall.SIGINT) {
		t.Fatal("helper did not inherit SIGINT ignored")
	}
	ctx, stop := NotifySignals(context.Background())
	defer stop()
	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
		t.Fatalf("an inherited-ignored SIGINT cancelled the watch: %v", context.Cause(ctx))
	case <-time.After(200 * time.Millisecond):
	}
	if !signal.Ignored(syscall.SIGINT) {
		t.Fatal("NotifySignals un-ignored SIGINT")
	}
}

// A second SIGTERM landing while an --sh write runs — up to that op's 2-minute
// cap after the first signal — has to end the process.
func TestSecondSignalEndsTheProcess(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperSecondSignal$")
	cmd.Env = append(os.Environ(), "STALL_HELPER_SECOND=1")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	lines := make(chan string, 8)
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()
	await := func(want string) {
		t.Helper()
		select {
		case line := <-lines:
			if line != want {
				t.Fatalf("helper printed %q, want %q (stderr %s)", line, want, stderr.String())
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("helper never printed %q (stderr %s)", want, stderr.String())
		}
	}
	signal := func() {
		t.Helper()
		if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
	}

	await("ready")
	signal()
	await("signalled")
	signal()

	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	select {
	case err := <-exited:
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("the helper exited %v, want killed by SIGTERM (stderr %s)", err, stderr.String())
		}
		if ws, ok := ee.Sys().(syscall.WaitStatus); !ok || !ws.Signaled() || ws.Signal() != syscall.SIGTERM {
			t.Fatalf("the helper exited %v, want killed by SIGTERM", err)
		}
	case <-time.After(5 * time.Second):
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Fatal("a second SIGTERM during the write was swallowed")
	}
}

// TestHelperSecondSignal is the child: it takes the first signal the way the
// watch does, then sits in a write that will not finish for a minute.
func TestHelperSecondSignal(t *testing.T) {
	if os.Getenv("STALL_HELPER_SECOND") != "1" {
		t.Skip("child-process helper")
	}
	ctx, stop := NotifySignals(context.Background())
	defer stop()
	go func() {
		<-ctx.Done()
		var se SignalError
		if !errors.As(context.Cause(ctx), &se) || se.Sig != syscall.SIGTERM {
			_, _ = fmt.Printf("bad cause: %v\n", context.Cause(ctx))
			return
		}
		_, _ = fmt.Println("signalled")
	}()
	_, _ = fmt.Println("ready")
	time.Sleep(time.Minute)
	_, _ = fmt.Println("survived the second SIGTERM")
}
