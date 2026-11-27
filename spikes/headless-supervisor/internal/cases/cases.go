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

const (
	turnTimeout   = 180 * time.Second
	shutdownGrace = 20 * time.Second
	noMoreTools   = "Do not use any other tool."
)

// toolset names the tools a prompt must point the model at; they differ per engine.
type toolset struct {
	Bash, Write, Read, Web, Ask, MCP string
}

func toolsFor(engine string) toolset {
	if engine == "claude" {
		return toolset{Bash: "Bash", Write: "Write", Read: "Read", Web: "WebFetch", Ask: "AskUserQuestion", MCP: "mcp__echo__echo"}
	}
	return toolset{Bash: "bash", Write: "write", Read: "read", Web: "fetch_content", Ask: "ask_user", MCP: "mcp__echo__echo"}
}

// recorder is a Policy that remembers every request it decided.
type recorder struct {
	mu     sync.Mutex
	reqs   []event.Request
	decide func(*event.Request) event.Decision
}

func newRecorder(decide func(*event.Request) event.Decision) *recorder {
	return &recorder{decide: decide}
}

func (r *recorder) policy(req *event.Request) event.Decision {
	r.mu.Lock()
	r.reqs = append(r.reqs, *req)
	r.mu.Unlock()
	return r.decide(req)
}

func (r *recorder) requests() []event.Request {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]event.Request(nil), r.reqs...)
}

// find returns the first request of kind whose command, path, url, domain or
// server contains substr.
func (r *recorder) find(kind, substr string) (event.Request, bool) {
	for _, q := range r.requests() {
		d := q.Detail
		if d.Kind == kind && strings.Contains(strings.Join([]string{d.Command, d.Path, d.URL, d.Domain, d.Server}, "\x00"), substr) {
			return q, true
		}
	}
	return event.Request{}, false
}

func decision(v event.Verdict, reason string) event.Decision {
	return event.Decision{Verdict: v, Reason: reason}
}

// byKind decides by Detail.Kind; unlisted kinds are denied.
func byKind(m map[string]event.Decision) func(*event.Request) event.Decision {
	return func(r *event.Request) event.Decision {
		if d, ok := m[r.Detail.Kind]; ok {
			return d
		}
		return decision("deny", "unexpected permission kind "+r.Detail.Kind)
	}
}

func launch(ctx context.Context, env *CaseEnv, o event.Options, policy func(*event.Request) event.Decision, mon *supervisor.Monitor) (*supervisor.Run, error) {
	eng, err := env.Factory(o)
	if err != nil {
		return nil, err
	}
	run := &supervisor.Run{Engine: eng, T: env.Transcript, Policy: policy, Mon: mon}
	if err := run.Start(ctx); err != nil {
		return nil, err
	}
	return run, nil
}

func shutdown(run *supervisor.Run) {
	if err := run.Shutdown(shutdownGrace); err != nil {
		run.Note("shutdown: " + err.Error())
	}
}

// oneShot runs a single turn and returns the events it produced.
func oneShot(ctx context.Context, env *CaseEnv, o event.Options, policy func(*event.Request) event.Decision, prompt string) ([]event.Event, error) {
	run, err := launch(ctx, env, o, policy, nil)
	if err != nil {
		return nil, err
	}
	defer shutdown(run)
	mark := run.Mark()
	if _, err := run.Turn(prompt, turnTimeout); err != nil {
		return nil, fmt.Errorf("turn: %w", err)
	}
	return run.Events()[mark:], nil
}

func ofKind(evs []event.Event, k event.Kind) []event.Event {
	var out []event.Event
	for _, e := range evs {
		if e.Kind == k {
			out = append(out, e)
		}
	}
	return out
}

func textOf(evs []event.Event) string {
	var b strings.Builder
	for _, e := range ofKind(evs, event.KindText) {
		b.WriteString(e.Text)
	}
	return b.String()
}

// toolsOK fails unless a tool result arrived and none was an error.
func toolsOK(evs []event.Event) error {
	res := ofKind(evs, event.KindToolResult)
	if len(res) == 0 {
		return fmt.Errorf("no tool result arrived")
	}
	for _, e := range res {
		if e.IsError {
			return fmt.Errorf("tool result is an error: %s", clip(e.Text))
		}
	}
	return nil
}

// toolDenied fails unless an error tool result carries reason.
func toolDenied(evs []event.Event, reason string) error {
	res := ofKind(evs, event.KindToolResult)
	for _, e := range res {
		if e.IsError && strings.Contains(e.Text, reason) {
			return nil
		}
	}
	if len(res) == 0 {
		return fmt.Errorf("no tool result arrived")
	}
	return fmt.Errorf("no error tool result carries %q; got: %s", reason, clip(res[len(res)-1].Text))
}

func clip(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 200 {
		return s[:200] + "..."
	}
	return s
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// runInstruction is the prompt for a single explicit Bash call.
func runInstruction(t toolset, command string) string {
	return fmt.Sprintf("Use the %s tool to run exactly: %s . %s Then reply done.", t.Bash, command, noMoreTools)
}

// echoConfig writes an MCP config naming the mcpecho server at path and
// returns it; claude and pi share the mcpServers shape (pi adds exposure).
func echoConfig(env *CaseEnv, path string, extra map[string]any) error {
	if env.MCPEcho == "" {
		return fmt.Errorf("mcpecho binary unset (HSUP_MCPECHO or --mcpecho)")
	}
	srv := map[string]any{"command": env.MCPEcho, "args": []string{}}
	for k, v := range extra {
		srv[k] = v
	}
	b, err := json.Marshal(map[string]any{"mcpServers": map[string]any{"echo": srv}})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

// writeSlowScript creates work/slow.sh. claude's Bash tool refuses a
// standalone long `sleep`, so cases run the delay through this script.
func writeSlowScript(work string) error {
	return os.WriteFile(filepath.Join(work, "slow.sh"), []byte("#!/bin/sh\nsleep \"$1\"\necho slow-done\n"), 0o755) //nolint:gosec // must be executable
}

// slowInstruction is the prompt for a single `sh slow.sh <secs>` call.
func slowInstruction(t toolset, secs int) string {
	return runInstruction(t, fmt.Sprintf("sh slow.sh %d", secs))
}

func isSlowStart(e event.Event) bool {
	return isBashStart(e) && strings.Contains(e.Detail.Command, "slow.sh")
}

// slowResult returns the ToolResult that finished the slow.sh tool call, if
// one has arrived: matched by ToolUseID when the start carries one, else any
// result after the start.
func slowResult(evs []event.Event) (event.Event, bool) {
	i := firstIndex(evs, isSlowStart)
	if i < 0 {
		return event.Event{}, false
	}
	id := evs[i].ToolUseID
	for _, e := range evs[i+1:] {
		if e.Kind == event.KindToolResult && (id == "" || e.ToolUseID == id) {
			return e, true
		}
	}
	return event.Event{}, false
}

// stillRunning fails if the slow tool already finished.
func stillRunning(evs []event.Event) error {
	if r, done := slowResult(evs); done {
		return fmt.Errorf("tool finished before interrupt: %s", clip(r.Text))
	}
	return nil
}
