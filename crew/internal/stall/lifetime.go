package stall

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

// SignalError is the cancellation cause NotifySignals records for a received
// signal.
type SignalError struct{ Sig syscall.Signal }

func (e SignalError) Error() string { return "signal: " + e.Sig.String() }

// NotifySignals returns a context cancelled with SignalError on the first
// SIGTERM or SIGINT, so the deferred lock release runs through the normal
// ctx-cancel path. SIGHUP is deliberately not handled: the watchdog runs under
// nohup, and Notify(SIGHUP) would undo the ignore it inherits. SIGINT is
// handled only when not inherited ignored, for the same reason: dispatch
// starts the watchdog as a non-interactive bash `&` job, which ignores SIGINT,
// so a Ctrl-C to dispatch's process group never killed it. The returned func
// stops notification and cancels with a nil cause; it is also safe to call
// after a signal, which the first one has already stopped notification for, so
// a second signal finds the default disposition waiting.
func NotifySignals(parent context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancelCause(parent)
	ch := make(chan os.Signal, 1)
	sigs := []os.Signal{syscall.SIGTERM}
	if !signal.Ignored(syscall.SIGINT) {
		sigs = append(sigs, syscall.SIGINT)
	}
	signal.Notify(ch, sigs...)
	go func() {
		select {
		case sig := <-ch:
			// Uninstall the handler here, not only in the returned stop: the arm
			// can hold on for an --sh write's 2-minute cap after the first
			// signal, and while a channel stays registered a second SIGTERM is
			// queued to a channel nobody reads and vanishes. Stopped, the
			// default disposition is back and a second signal ends the process.
			signal.Stop(ch)
			if s, ok := sig.(syscall.Signal); ok {
				cancel(SignalError{s})
			}
		case <-ctx.Done():
		}
	}()
	return ctx, func() {
		signal.Stop(ch)
		cancel(nil)
	}
}

// exitCodeFor is the status a shell reports for a process killed by the
// signal: 128+signo, which the bash arm's exit status was. Zero when the
// context ended any other way.
func exitCodeFor(ctx context.Context) int {
	if se, ok := context.Cause(ctx).(SignalError); ok {
		return 128 + int(se.Sig)
	}
	return 0
}
