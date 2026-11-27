package cases

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/noamsto/dispatcher/spikes/headless-supervisor/internal/event"
)

func init() {
	Register(Case{Name: "midturn", Run: midturn})
	Register(Case{Name: "interrupt", Run: interruptCase})
	Register(Case{Name: "kill-resume", Run: killResume})
}

func allowBash() func(*event.Request) event.Decision {
	return byKind(map[string]event.Decision{"bash": decision("allow_session", "")})
}

// midturn sends a user message while a turn is running and records whether
// the engine acted on it inside that turn (injected) or after it (queued).
func midturn(ctx context.Context, env *CaseEnv) (string, error) {
	t := toolsFor(env.Engine)
	rec := newRecorder(allowBash())
	run, err := launch(ctx, env, event.Options{}, rec.policy, nil)
	if err != nil {
		return "", err
	}
	defer shutdown(run)

	run.SkipPending()
	first := fmt.Sprintf("Use the %s tool three times, one call at a time, running exactly these commands in this order: sleep 3, then sleep 3, then sleep 3. %s Then reply done.", t.Bash, noMoreTools)
	if err := run.Send(first); err != nil {
		return "", err
	}
	if _, err := run.WaitKind(event.KindToolResult, turnTimeout); err != nil {
		return "", fmt.Errorf("waiting for the first tool result: %w", err)
	}
	extra := fmt.Sprintf("Also, before you finish, use the %s tool to run exactly: touch midturn.txt", t.Bash)
	if err := run.Send(extra); err != nil {
		return "", err
	}
	if _, err := run.WaitKind(event.KindTurnEnd, turnTimeout); err != nil {
		return "", fmt.Errorf("waiting for the first TurnEnd: %w", err)
	}

	mode := "injected"
	if !midturnStarted(run.Events()) {
		mode = "queued"
		if _, err := run.WaitKind(event.KindTurnEnd, turnTimeout); err != nil {
			return "", fmt.Errorf("midturn message was not acted on before the first TurnEnd and no second turn followed: %w", err)
		}
	}
	evs := run.Events()
	i, j := firstIndex(evs, midturnTool), firstIndex(evs, func(e event.Event) bool { return e.Kind == event.KindTurnEnd })
	if i < 0 {
		return "", fmt.Errorf("midturn message was never acted on")
	}
	if !exists(filepath.Join(env.Work, "midturn.txt")) {
		return "", fmt.Errorf("midturn.txt does not exist (mode %s)", mode)
	}
	if mode == "injected" && i > j {
		mode = "queued"
	}
	return fmt.Sprintf("midturn message %s: midturn.txt exists (touch ToolStart at event %d, first TurnEnd at %d)", mode, i, j), nil
}

func midturnTool(e event.Event) bool {
	return e.Kind == event.KindToolStart && strings.Contains(e.Detail.Command, "midturn.txt")
}

func midturnStarted(evs []event.Event) bool { return firstIndex(evs, midturnTool) >= 0 }

func firstIndex(evs []event.Event, pred func(event.Event) bool) int {
	for i, e := range evs {
		if pred(e) {
			return i
		}
	}
	return -1
}

func isBashStart(e event.Event) bool {
	return e.Kind == event.KindToolStart && e.Detail.Kind == "bash"
}

// interruptCase interrupts a running tool and steers the same session after.
func interruptCase(ctx context.Context, env *CaseEnv) (string, error) {
	t := toolsFor(env.Engine)
	rec := newRecorder(allowBash())
	run, err := launch(ctx, env, event.Options{}, rec.policy, nil)
	if err != nil {
		return "", err
	}
	defer shutdown(run)

	if err := writeSlowScript(env.Work); err != nil {
		return "", err
	}
	run.SkipPending()
	if err := run.Send(slowInstruction(t, 60)); err != nil {
		return "", err
	}
	if _, err := run.WaitFor(isSlowStart, turnTimeout); err != nil {
		return "", fmt.Errorf("waiting for the slow.sh tool call: %w", err)
	}
	// Let the permission round trip finish and the command actually start.
	time.Sleep(2 * time.Second)
	if err := stillRunning(run.Events()); err != nil {
		return "", err
	}
	t0 := time.Now()
	if err := run.Engine.Interrupt(); err != nil {
		return "", fmt.Errorf("interrupt: %w", err)
	}
	if _, err := run.WaitKind(event.KindTurnEnd, 15*time.Second); err != nil {
		return "", fmt.Errorf("no TurnEnd within 15s of the interrupt: %w", err)
	}
	took := time.Since(t0).Round(100 * time.Millisecond)
	cancelled := "no ToolResult"
	if r, ok := slowResult(run.Events()); ok {
		cancelled = "ToolResult " + strconv.Quote(clip(r.Text))
	}

	if _, err := run.Turn(runInstruction(t, "touch steered.txt"), turnTimeout); err != nil {
		return "", fmt.Errorf("steering turn: %w", err)
	}
	if !exists(filepath.Join(env.Work, "steered.txt")) {
		return "", fmt.Errorf("steered.txt does not exist after the follow-up")
	}
	return fmt.Sprintf("slow.sh running at interrupt → TurnEnd in %.1fs (%s); follow-up in the same session created steered.txt", took.Seconds(), cancelled), nil
}

// killResume kills the child mid-tool and resumes the session on the latest
// child-reported id.
func killResume(ctx context.Context, env *CaseEnv) (string, error) {
	t := toolsFor(env.Engine)
	const codeword = "PLUM-7731"
	rec := newRecorder(allowBash())
	run, err := launch(ctx, env, event.Options{}, rec.policy, nil)
	if err != nil {
		return "", err
	}
	defer shutdown(run)

	if _, err := run.Turn("Remember the codeword "+codeword+". Reply with just: ok", turnTimeout); err != nil {
		return "", fmt.Errorf("turn 1: %w", err)
	}
	if err := writeSlowScript(env.Work); err != nil {
		return "", err
	}
	run.SkipPending()
	if err := run.Send(slowInstruction(t, 120)); err != nil {
		return "", err
	}
	if _, err := run.WaitFor(isSlowStart, turnTimeout); err != nil {
		return "", fmt.Errorf("waiting for the slow.sh tool call: %w", err)
	}
	time.Sleep(2 * time.Second)
	if r, done := slowResult(run.Events()); done {
		return "", fmt.Errorf("tool finished before kill: %s", clip(r.Text))
	}
	id := run.Engine.SessionID()
	if id == "" {
		return "", fmt.Errorf("engine reported no session id before the kill")
	}
	if err := run.Engine.Kill(); err != nil {
		return "", fmt.Errorf("kill: %w", err)
	}
	select {
	case <-run.Done():
	case <-time.After(shutdownGrace):
		return "", fmt.Errorf("event stream still open %s after kill", shutdownGrace)
	}
	run.Note("child killed mid-tool; resuming on latest reported session id")

	run2, err := launch(ctx, env, event.Options{SessionID: id, Resume: true}, rec.policy, nil)
	if err != nil {
		return "", fmt.Errorf("resume: %w", err)
	}
	defer shutdown(run2)
	mark := run2.Mark()
	if _, err := run2.Turn("What was the codeword? Reply with it only.", turnTimeout); err != nil {
		return "", fmt.Errorf("resumed turn: %w", err)
	}
	if !strings.Contains(run2.TextSince(mark), codeword) {
		return "", fmt.Errorf("resumed session did not recall the codeword: %s", clip(run2.TextSince(mark)))
	}
	same := "differs from"
	for _, e := range run2.Events() {
		if e.Kind == event.KindInit {
			if e.SessionID == id {
				same = "equals"
			}
			break
		}
	}
	return fmt.Sprintf("killed mid-tool, resumed on latest reported id; resumed Init session id %s the original; codeword recalled", same), nil
}
