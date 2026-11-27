package cases

import (
	"context"
	"fmt"
	"time"

	"github.com/noamsto/dispatcher/spikes/headless-supervisor/internal/event"
	"github.com/noamsto/dispatcher/spikes/headless-supervisor/internal/supervisor"
)

func init() {
	Register(Case{Name: "hang", Run: hang})
}

// hang lets a long tool call go silent, detects it with the liveness
// monitor, then recovers by interrupting.
func hang(ctx context.Context, env *CaseEnv) (string, error) {
	const silence = 15 * time.Second
	t := toolsFor(env.Engine)
	rec := newRecorder(allowBash())
	run, err := launch(ctx, env, event.Options{}, rec.policy, &supervisor.Monitor{Silence: silence})
	if err != nil {
		return "", err
	}
	defer shutdown(run)

	if err := writeSlowScript(env.Work); err != nil {
		return "", err
	}
	run.SkipPending()
	t0 := time.Now()
	if err := run.Send(slowInstruction(t, 600)); err != nil {
		return "", err
	}
	if err := run.WaitStale(turnTimeout); err != nil {
		return "", fmt.Errorf("waiting for stale: %w", err)
	}
	detected := time.Since(t0).Round(time.Second)
	if firstIndex(run.Events(), isSlowStart) < 0 {
		return "", fmt.Errorf("went stale after %s before the sleep tool started (model latency, not a hang)", detected)
	}
	if err := stillRunning(run.Events()); err != nil {
		return "", err
	}
	t1 := time.Now()
	if err := run.Engine.Interrupt(); err != nil {
		return "", fmt.Errorf("interrupt: %w", err)
	}
	if _, err := run.WaitKind(event.KindTurnEnd, 20*time.Second); err != nil {
		return "", fmt.Errorf("no TurnEnd within 20s of the interrupt: %w", err)
	}
	return fmt.Sprintf("stale detected %s after the prompt (silence limit %s) during slow.sh 600 → interrupt → TurnEnd in %s",
		detected, silence, time.Since(t1).Round(100*time.Millisecond)), nil
}
