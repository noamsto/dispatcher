package cases

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/noamsto/dispatcher/spikes/headless-supervisor/internal/event"
	"github.com/noamsto/dispatcher/spikes/headless-supervisor/internal/stub"
	"github.com/noamsto/dispatcher/spikes/headless-supervisor/internal/supervisor"
)

func init() {
	Register(Case{Name: "error", Run: errorCase})
}

const errorTimeout = 120 * time.Second

// errorCase points the engine at a local stub that answers with a rate limit,
// an overload and a credit error, and checks the supervisor sees Retry events
// and a terminal error.
func errorCase(ctx context.Context, env *CaseEnv) (string, error) {
	script := stub.DefaultScript()
	if env.Engine == "pi" {
		script = piScript()
	}
	srv, err := stub.Start(script)
	if err != nil {
		return "", err
	}
	defer func() { _ = srv.Close() }()

	o := event.Options{}
	if env.Engine == "claude" {
		o.Bare = true
		o.Env = []string{"ANTHROPIC_API_KEY=stub-not-a-key", "ANTHROPIC_BASE_URL=" + srv.URL}
	} else {
		if env.Gate == "" {
			return "", fmt.Errorf("pi gate path unset (HSUP_GATE or --gate)")
		}
		o.Model = "stub/stub-model"
		o.Extensions = []string{filepath.Join(filepath.Dir(env.Gate), "errstub.ts")}
		o.Env = []string{"HSUP_STUB_URL=" + srv.URL}
	}
	run, err := launch(ctx, env, o, nil, nil)
	if err != nil {
		return "", err
	}
	defer shutdown(run)
	run.Note("supervisor rule: Retry with 429 + retry-after -> wait and let the engine retry; unified status rejected -> hold until resetsAt; credit-style 400 -> terminal, escalate")

	run.SkipPending()
	if err := run.Send("Reply with the single word: ok"); err != nil {
		return "", err
	}
	var retries []string
	var terminal string
	_, err = run.WaitFor(func(e event.Event) bool {
		switch e.Kind {
		case event.KindRetry:
			retries = append(retries, e.Text)
		case event.KindError:
			terminal = "Error: " + clip(e.Text)
			// pi emits an Error per failed attempt before each Retry; only
			// the settled turn tells which one was last.
			return env.Engine != "pi"
		case event.KindTurnEnd:
			if env.Engine == "pi" {
				return true
			}
			terminal = fmt.Sprintf("TurnEnd is_error=%v", e.IsError)
			if !e.IsError {
				terminal = ""
			}
			return true
		}
		return false
	}, errorTimeout)

	if env.Engine == "pi" {
		return piErrorVerdict(retries, terminal, srv.Hits(), err)
	}
	summary := fmt.Sprintf("%d Retry event(s) [%s], stub hits %d", len(retries), strings.Join(first(retries, 4), "; "), srv.Hits())
	switch {
	case terminal != "" && len(retries) >= 1:
		return summary + ", terminal " + terminal, nil
	case terminal != "":
		return "", fmt.Errorf("terminal %s without any Retry event", terminal)
	case err == nil:
		return "", fmt.Errorf("turn ended without an error after %s", summary)
	case errors.Is(err, supervisor.ErrTimeout) && len(retries) >= 2:
		return summary + ", no terminal event within " + errorTimeout.String() + " (retrying until shutdown)", nil
	}
	return "", fmt.Errorf("%w; %s", err, summary)
}

func first(s []string, n int) []string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// piScript is two overloads then a rate limit (repeated once the script runs
// out), covering pi's retry of both a 5xx and a 429 until its attempts end.
func piScript() []stub.Response {
	overloaded := stub.Response{Status: 529, Body: `{"type":"error","error":{"type":"overloaded_error","message":"stub: overloaded"}}`}
	return []stub.Response{
		overloaded, overloaded,
		{Status: http.StatusTooManyRequests, Body: `{"type":"error","error":{"type":"rate_limit_error","message":"stub: rate limited"}}`},
	}
}

func piErrorVerdict(retries []string, terminal string, hits int, waitErr error) (string, error) {
	if terminal == "" {
		return "", fmt.Errorf("no terminal error from pi (%d retries, %d stub hits, wait: %v)", len(retries), hits, waitErr)
	}
	if !strings.Contains(terminal, "429") || !strings.Contains(terminal, "rate_limit_error") {
		return "", fmt.Errorf("terminal %s lacks the 429/rate_limit_error status and type", terminal)
	}
	statuses := make([]string, len(retries))
	for i, r := range retries {
		statuses[i], _, _ = strings.Cut(r, " ")
	}
	return fmt.Sprintf("pi auto-retried [%s] (%d Retry events); gave up with terminal rate_limit_error (%s), stub hits %d",
		strings.Join(statuses, ", "), len(retries), clip(terminal), hits), nil
}
