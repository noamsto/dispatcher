package cases

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/noamsto/dispatcher/spikes/headless-supervisor/internal/event"
	"github.com/noamsto/dispatcher/spikes/headless-supervisor/internal/supervisor"
)

func init() {
	Register(Case{Name: "acp-bash-allow", Engines: []string{"pi-acp", "claude-acp"}, Run: runACPBashAllow})
}

// runACPBashAllow allows the first ACP permission request once and checks the
// command ran, reporting what the request carried.
func runACPBashAllow(ctx context.Context, env *CaseEnv) (string, error) {
	var (
		mu  sync.Mutex
		req *event.Request
	)
	policy := func(r *event.Request) event.Decision {
		mu.Lock()
		defer mu.Unlock()
		if req == nil {
			req = r
		}
		return event.Decision{Verdict: "allow_once"}
	}
	eng, err := env.Factory(event.Options{Work: env.Work, Transcript: env.Transcript, GatePath: env.Gate})
	if err != nil {
		return "", err
	}
	run := &supervisor.Run{Engine: eng, T: env.Transcript, Policy: policy}
	if err := run.Start(ctx); err != nil {
		return "", err
	}
	defer func() { _ = run.Shutdown(10 * time.Second) }()

	const prompt = "Use the bash tool to run exactly: touch acp-probe.txt"
	if _, err := run.Turn(prompt, 3*time.Minute); err != nil {
		return "", fmt.Errorf("turn: %w", err)
	}
	mu.Lock()
	got := req
	mu.Unlock()
	if got == nil {
		return "", fmt.Errorf("no permission request arrived")
	}
	var result *event.Event
	for _, ev := range run.Events() {
		if ev.Kind == event.KindToolResult {
			result = &ev
		}
	}
	if result == nil || result.IsError {
		return "", fmt.Errorf("no successful tool result (got %+v)", result)
	}
	if _, err := os.Stat(filepath.Join(env.Work, "acp-probe.txt")); err != nil {
		return "", fmt.Errorf("probe file missing: %w", err)
	}
	return fmt.Sprintf("permission carried title=%q kind=%s command=%q rawInput=%s options=%s; tool result ok, file created",
		got.Tool, got.Detail.Kind, got.Detail.Command, got.Input, optionKinds(run.Events())), nil
}

// optionKinds lists name:kind for the options of the first permission frame.
func optionKinds(evs []event.Event) string {
	for _, ev := range evs {
		if ev.Kind != event.KindPermission {
			continue
		}
		var f struct {
			Params struct {
				Options []struct{ Name, Kind string } `json:"options"`
			} `json:"params"`
		}
		if json.Unmarshal(ev.Raw, &f) != nil {
			return "?"
		}
		var out []string
		for _, o := range f.Params.Options {
			out = append(out, o.Name+":"+o.Kind)
		}
		return strings.Join(out, ",")
	}
	return "?"
}
