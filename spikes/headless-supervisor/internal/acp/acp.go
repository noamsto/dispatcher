// Package acp drives an Agent Client Protocol agent (newline-delimited
// JSON-RPC 2.0 over stdio) as an event.Engine. NewPiBolt runs
// `pi-bolt --mode acp`; NewClaude runs claude-agent-acp.
package acp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/noamsto/dispatcher/spikes/headless-supervisor/internal/claude"
	"github.com/noamsto/dispatcher/spikes/headless-supervisor/internal/event"
)

const (
	closeGrace    = 10 * time.Second
	handshakeWait = 60 * time.Second
	piModel       = "anthropic/claude-haiku-4-5"
	steerMethod   = "_session/steering"
)

// ACP permission option kinds.
const (
	kindAllowOnce   = "allow_once"
	kindAllowAlways = "allow_always"
	kindRejectOnce  = "reject_once"
)

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message) }

type msg struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *rpcError       `json:"error,omitempty"`
}

type option struct {
	OptionID string `json:"optionId"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
}

type toolCall struct {
	ToolCallID string          `json:"toolCallId"`
	Title      string          `json:"title"`
	Kind       string          `json:"kind"`
	Status     string          `json:"status"`
	RawInput   json.RawMessage `json:"rawInput"`
	RawOutput  json.RawMessage `json:"rawOutput"`
	Content    json.RawMessage `json:"content"`
	Locations  []struct {
		Path string `json:"path"`
	} `json:"locations"`
}

type pendingPerm struct {
	id      json.RawMessage
	options []option
}

type reply func(result json.RawMessage, err *rpcError)

type spec struct {
	name    string // engine name in Request.Engine
	bin     string
	args    []string
	env     []string
	newMeta map[string]any // session/new and session/load _meta
	config  [][2]string    // session/set_config_option {configId, value} pairs applied after session setup
}

type engine struct {
	spec spec
	opts event.Options

	cmd     *exec.Cmd
	stdin   io.WriteCloser
	events  chan event.Event
	done    chan struct{}
	waitErr error

	wmu sync.Mutex // serialises stdin writes

	mu        sync.Mutex
	nextID    int
	calls     map[int]reply
	perms     map[string]pendingPerm
	started   map[string]toolCall // announced tool calls by id, for permission requests that carry only a title
	text      strings.Builder
	sessionID string
	busy      bool
	queue     []string
	steering  bool
	replay    bool // session/load history is being replayed; drop it
}

// NewPiBolt returns an unstarted pi-bolt ACP engine. It resolves its own
// binary (ResolvePiBolt) and ignores Options.Bin.
func NewPiBolt(o event.Options) (event.Engine, error) {
	bin, err := ResolvePiBolt()
	if err != nil {
		return nil, err
	}
	model := o.Model
	if model == "" {
		model = os.Getenv("HSUP_PI_MODEL")
	}
	if model == "" {
		model = piModel
	}
	args := []string{
		"--mode", "acp", "--model", model,
		"--session-dir", filepath.Join(o.Work, ".pi-sessions"),
		"--no-extensions", "--no-skills", "--no-prompt-templates", "--no-context-files", "--no-mcp",
	}
	if o.GatePath != "" {
		args = append(args, "-e", o.GatePath)
	}
	for _, e := range o.Extensions {
		args = append(args, "-e", e)
	}
	if o.Approve {
		args = append(args, "--approve")
	} else {
		args = append(args, "--no-approve")
	}
	args = append(args, o.ExtraArgs...)
	return newEngine(spec{name: "pi-acp", bin: bin, args: args, env: o.Env}, o), nil
}

// claudeSessionMeta drops the user setting source so the operator's own
// allow rules (e.g. Bash(touch *)) cannot pre-approve calls before the client is asked.
var claudeSessionMeta = map[string]any{"claudeCode": map[string]any{"options": map[string]any{"settingSources": []string{"project", "local"}}}}

// NewClaude returns an unstarted claude-agent-acp engine. The agent's own
// claude-code pin is overridden with the supervisor's wrapped claude binary
// unless CLAUDE_CODE_EXECUTABLE is already set. It resolves its own binary
// (ResolveClaudeACP) and ignores Options.Bin.
func NewClaude(o event.Options) (event.Engine, error) {
	bin, err := ResolveClaudeACP()
	if err != nil {
		return nil, err
	}
	env := o.Env
	if os.Getenv("CLAUDE_CODE_EXECUTABLE") == "" {
		if cc, err := claude.ResolveBin(); err == nil {
			env = append(append([]string(nil), env...), "CLAUDE_CODE_EXECUTABLE="+cc)
		}
	}
	model := o.Model
	if model == "" {
		model = "haiku"
	}
	// claude-agent-acp starts in "auto" mode, which decides permissions itself and never asks the client.
	mode := o.PermissionMode
	if mode == "" {
		mode = "default"
	}
	return newEngine(spec{name: "claude-acp", bin: bin, env: env, newMeta: claudeSessionMeta, config: [][2]string{{"model", model}, {"mode", mode}}}, o), nil
}

// ResolvePiBolt returns HSUP_PIBOLT_BIN, else pi-bolt from PATH.
func ResolvePiBolt() (string, error) {
	if b := os.Getenv("HSUP_PIBOLT_BIN"); b != "" {
		return b, nil
	}
	p, err := exec.LookPath("pi-bolt")
	if err != nil {
		return "", fmt.Errorf("pi-bolt not on PATH and HSUP_PIBOLT_BIN unset: %w", err)
	}
	return p, nil
}

// ResolveClaudeACP returns HSUP_CLAUDE_ACP_BIN, else claude-agent-acp from PATH.
func ResolveClaudeACP() (string, error) {
	if b := os.Getenv("HSUP_CLAUDE_ACP_BIN"); b != "" {
		return b, nil
	}
	p, err := exec.LookPath("claude-agent-acp")
	if err != nil {
		return "", fmt.Errorf("claude-agent-acp not on PATH and HSUP_CLAUDE_ACP_BIN unset: %w", err)
	}
	return p, nil
}

func newEngine(s spec, o event.Options) *engine {
	return &engine{
		spec:    s,
		opts:    o,
		events:  make(chan event.Event, 256),
		done:    make(chan struct{}),
		calls:   map[int]reply{},
		perms:   map[string]pendingPerm{},
		started: map[string]toolCall{},
	}
}

func (e *engine) Start(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, e.spec.bin, e.spec.args...)
	cmd.ExtraFiles = e.opts.Inherit
	cmd.Dir = e.opts.Work
	cmd.Env = claude.ChildEnv(os.Environ(), e.spec.env)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	// Cancelling ctx ends the child the way Close does: EOF on stdin, then
	// SIGKILL only once the grace runs out.
	cmd.Cancel = stdin.Close
	cmd.WaitDelay = closeGrace
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	e.cmd, e.stdin = cmd, stdin
	for _, kv := range e.spec.env {
		if v, ok := strings.CutPrefix(kv, "CLAUDE_CODE_EXECUTABLE="); ok {
			e.note(map[string]string{"claude_code_executable": v})
		}
	}

	var errWG sync.WaitGroup
	errWG.Add(1)
	go func() {
		defer errWG.Done()
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 64*1024), 8<<20)
		for sc.Scan() {
			e.note(map[string]string{"stderr": sc.Text()})
		}
		if err := sc.Err(); err != nil {
			e.note(map[string]string{"stderr_error": err.Error()})
		}
	}()
	go func() {
		e.pump(stdout)
		errWG.Wait()
		e.waitErr = cmd.Wait()
		close(e.done)
		exit := event.Event{Kind: event.KindExit, At: time.Now()}
		if e.waitErr != nil {
			exit.Text = e.waitErr.Error()
		}
		e.events <- exit
		close(e.events)
	}()

	if err := e.handshake(ctx); err != nil {
		_ = e.Kill()
		return err
	}
	return nil
}

func (e *engine) handshake(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, handshakeWait)
	defer cancel()
	res, err := e.call(ctx, "initialize", map[string]any{
		"protocolVersion": 1,
		"clientCapabilities": map[string]any{
			"fs": map[string]any{"readTextFile": false, "writeTextFile": false},
		},
	})
	if err != nil {
		return fmt.Errorf("acp initialize: %w", err)
	}
	var init struct {
		AgentCapabilities struct {
			LoadSession bool `json:"loadSession"`
		} `json:"agentCapabilities"`
		Meta struct {
			Steering struct {
				Supported bool `json:"supported"`
			} `json:"steering"`
		} `json:"_meta"`
	}
	if err := json.Unmarshal(res, &init); err != nil {
		return fmt.Errorf("acp initialize result: %w", err)
	}
	e.mu.Lock()
	e.steering = init.Meta.Steering.Supported
	e.mu.Unlock()
	e.note(map[string]any{"acp_caps": map[string]any{"loadSession": init.AgentCapabilities.LoadSession, "steering": init.Meta.Steering.Supported}})

	if e.opts.Resume && e.opts.SessionID != "" {
		if init.AgentCapabilities.LoadSession {
			return e.loadSession(ctx)
		}
		e.note(map[string]string{"resume": "agent lacks loadSession; starting a new session"})
	}
	newParams := map[string]any{"cwd": e.opts.Work, "mcpServers": []any{}}
	if e.spec.newMeta != nil {
		newParams["_meta"] = e.spec.newMeta
	}
	res, err = e.call(ctx, "session/new", newParams)
	if err != nil {
		return fmt.Errorf("acp session/new: %w", err)
	}
	var sn struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(res, &sn); err != nil || sn.SessionID == "" {
		return fmt.Errorf("acp session/new result has no sessionId: %s", res)
	}
	e.setSession(sn.SessionID)
	return e.afterSession(ctx)
}

func (e *engine) loadSession(ctx context.Context) error {
	e.mu.Lock()
	e.replay = true
	e.mu.Unlock()
	_, err := e.call(ctx, "session/load", map[string]any{"sessionId": e.opts.SessionID, "cwd": e.opts.Work, "mcpServers": []any{}, "_meta": e.spec.newMeta})
	e.mu.Lock()
	e.replay = false
	e.mu.Unlock()
	if err != nil {
		return fmt.Errorf("acp session/load: %w", err)
	}
	e.setSession(e.opts.SessionID)
	return e.afterSession(ctx)
}

func (e *engine) afterSession(ctx context.Context) error {
	for _, kv := range e.spec.config {
		_, err := e.call(ctx, "session/set_config_option", map[string]any{"sessionId": e.SessionID(), "configId": kv[0], "value": kv[1]})
		if err != nil {
			e.note(map[string]string{"set_config_error": kv[0] + ": " + err.Error()})
			if kv[0] == "mode" {
				return fmt.Errorf("acp set mode %q: %w", kv[1], err)
			}
		}
	}
	e.emit(event.Event{Kind: event.KindInit, SessionID: e.SessionID()})
	return nil
}

func (e *engine) setSession(id string) {
	e.mu.Lock()
	e.sessionID = id
	e.mu.Unlock()
}

func (e *engine) pump(r io.Reader) {
	br := bufio.NewReaderSize(r, 1<<20)
	for {
		line, err := br.ReadBytes('\n')
		if line = bytes.TrimSpace(line); len(line) > 0 {
			e.handleLine(line)
		}
		if err != nil {
			if err != io.EOF {
				e.note(map[string]string{"stdout_error": err.Error()})
			}
			e.failCalls()
			return
		}
	}
}

// failCalls unblocks every outstanding request when the child's stdout ends.
func (e *engine) failCalls() {
	e.mu.Lock()
	calls := e.calls
	e.calls = map[int]reply{}
	e.mu.Unlock()
	for _, cb := range calls {
		cb(nil, &rpcError{Code: -32000, Message: "agent exited"})
	}
}

func (e *engine) handleLine(line []byte) {
	e.log("in", line)
	var m msg
	if err := json.Unmarshal(line, &m); err != nil {
		e.emit(event.Event{Kind: event.KindError, Text: "undecodable frame: " + err.Error(), IsError: true, Raw: append([]byte(nil), line...)})
		return
	}
	switch {
	case m.Method == "":
		e.handleResponse(m)
	case m.Method == "session/update" && len(m.ID) == 0:
		e.handleUpdate(m.Params, line)
	case m.Method == "session/request_permission" && len(m.ID) > 0:
		e.handlePermission(m, line)
	case len(m.ID) > 0:
		_ = e.writeFrame(map[string]any{"jsonrpc": "2.0", "id": m.ID, "error": &rpcError{Code: -32601, Message: "method not found: " + m.Method}})
	}
}

func (e *engine) handleResponse(m msg) {
	var id int
	if err := json.Unmarshal(m.ID, &id); err != nil {
		return
	}
	e.mu.Lock()
	cb := e.calls[id]
	delete(e.calls, id)
	e.mu.Unlock()
	if cb != nil {
		cb(m.Result, m.Error)
	}
}

func (e *engine) emit(ev event.Event) {
	if ev.At.IsZero() {
		ev.At = time.Now()
	}
	e.events <- ev
}

// flushText emits the coalesced assistant text, if any.
func (e *engine) flushText() {
	e.mu.Lock()
	t := e.text.String()
	e.text.Reset()
	e.mu.Unlock()
	if t != "" {
		e.emit(event.Event{Kind: event.KindText, Text: t})
	}
}

func (e *engine) handleUpdate(params json.RawMessage, raw []byte) {
	var p struct {
		Update json.RawMessage `json:"update"`
	}
	if json.Unmarshal(params, &p) != nil {
		return
	}
	var head struct {
		SessionUpdate string          `json:"sessionUpdate"`
		Content       json.RawMessage `json:"content"` // an object for message chunks, an array for tool updates
	}
	if json.Unmarshal(p.Update, &head) != nil {
		return
	}
	e.mu.Lock()
	replay := e.replay
	e.mu.Unlock()
	if replay {
		return
	}
	switch head.SessionUpdate {
	case "agent_message_chunk":
		var c struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(head.Content, &c) == nil && c.Type == "text" {
			e.mu.Lock()
			e.text.WriteString(c.Text)
			e.mu.Unlock()
		}
	case "tool_call", "tool_call_update":
		var tc toolCall
		if json.Unmarshal(p.Update, &tc) != nil {
			return
		}
		e.announce(tc, raw)
		if tc.Status == "completed" || tc.Status == "failed" {
			e.emit(event.Event{
				Kind: event.KindToolResult, Tool: tc.Title, ToolUseID: tc.ToolCallID,
				Text: resultText(tc), IsError: tc.Status == "failed", Raw: append([]byte(nil), raw...),
			})
		}
	}
}

func (e *engine) announce(tc toolCall, raw []byte) {
	e.mu.Lock()
	_, seen := e.started[tc.ToolCallID]
	if !seen {
		e.started[tc.ToolCallID] = tc
	}
	e.mu.Unlock()
	if seen {
		return
	}
	e.flushText()
	e.emit(event.Event{Kind: event.KindToolStart, Tool: tc.Title, ToolUseID: tc.ToolCallID, Detail: detail(tc), Raw: append([]byte(nil), raw...)})
}

func resultText(tc toolCall) string {
	var blocks []struct {
		Content struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	var parts []string
	if json.Unmarshal(tc.Content, &blocks) == nil {
		for _, b := range blocks {
			if b.Content.Text != "" {
				parts = append(parts, b.Content.Text)
			}
		}
	}
	if len(parts) > 0 {
		return strings.Join(parts, "\n")
	}
	if len(tc.RawOutput) > 0 && string(tc.RawOutput) != "null" {
		return string(tc.RawOutput)
	}
	return ""
}

// detail maps an ACP tool call to the engine-neutral Detail. ACP kinds:
// read, edit, delete, move, search, execute, think, fetch, switch_mode, other.
func detail(tc toolCall) event.Detail {
	d := event.Detail{Kind: "other"}
	switch tc.Kind {
	case "execute":
		d.Kind = "bash"
	case "edit", "delete", "move":
		d.Kind = "write"
	case "read", "search":
		d.Kind = "read"
	case "fetch":
		d.Kind = "web"
	case "switch_mode":
		d.Kind = "plan"
	}
	var in map[string]any
	_ = json.Unmarshal(tc.RawInput, &in)
	str := func(keys ...string) string {
		for _, k := range keys {
			if s, ok := in[k].(string); ok && s != "" {
				return s
			}
		}
		return ""
	}
	d.Command = str("command", "cmd")
	d.Path = str("path", "file_path", "filePath")
	if d.Path == "" && len(tc.Locations) > 0 {
		d.Path = tc.Locations[0].Path
	}
	d.URL = str("url")
	return d
}

func (e *engine) handlePermission(m msg, raw []byte) {
	var p struct {
		ToolCall toolCall `json:"toolCall"`
		Options  []option `json:"options"`
	}
	if err := json.Unmarshal(m.Params, &p); err != nil {
		_ = e.writeFrame(map[string]any{"jsonrpc": "2.0", "id": m.ID, "error": &rpcError{Code: -32602, Message: "invalid params"}})
		return
	}
	id := string(m.ID)
	e.mu.Lock()
	e.perms[id] = pendingPerm{id: m.ID, options: p.Options}
	if prior, ok := e.started[p.ToolCall.ToolCallID]; ok && p.ToolCall.Kind == "" && len(p.ToolCall.RawInput) == 0 {
		// pi-bolt's extension confirm/select carries only a title; the tool_call announced it.
		p.ToolCall.Kind, p.ToolCall.RawInput, p.ToolCall.Locations = prior.Kind, prior.RawInput, prior.Locations
	}
	e.mu.Unlock()
	d := detail(p.ToolCall)
	for _, o := range p.Options {
		d.Options = append(d.Options, o.Name)
	}
	e.flushText()
	e.emit(event.Event{
		Kind: event.KindPermission, Tool: p.ToolCall.Title, ToolUseID: p.ToolCall.ToolCallID, Detail: d,
		Req: &event.Request{ID: id, Engine: e.spec.name, Detail: d, Tool: p.ToolCall.Title, Input: p.ToolCall.RawInput},
		Raw: append([]byte(nil), raw...),
	})
}

func (e *engine) Respond(r *event.Request, d event.Decision) error {
	e.mu.Lock()
	pp, ok := e.perms[r.ID]
	e.mu.Unlock()
	if !ok {
		return fmt.Errorf("acp: no pending permission %q", r.ID)
	}
	outcome, err := chooseOutcome(pp.options, d)
	if err != nil {
		return err
	}
	if d.Verdict == "deny" && d.Reason != "" {
		e.note(map[string]string{"deny_reason": "not delivered: ACP session/request_permission has no reason channel"})
	}
	e.mu.Lock()
	delete(e.perms, r.ID)
	e.mu.Unlock()
	return e.writeFrame(map[string]any{"jsonrpc": "2.0", "id": pp.id, "result": map[string]any{"outcome": outcome}})
}

func chooseOutcome(opts []option, d event.Decision) (map[string]string, error) {
	byKind := func(kind string) (string, bool) {
		for _, o := range opts {
			if o.Kind == kind {
				return o.OptionID, true
			}
		}
		return "", false
	}
	selected := func(id string) map[string]string { return map[string]string{"outcome": "selected", "optionId": id} }
	var kind string
	switch d.Verdict {
	case "allow_once":
		kind = kindAllowOnce
	case "allow_session":
		kind = kindAllowAlways
	case "deny":
		if id, ok := byKind(kindRejectOnce); ok {
			return selected(id), nil
		}
		return map[string]string{"outcome": "cancelled"}, nil
	case "answer":
		for _, want := range d.Answers {
			for _, o := range opts {
				if strings.EqualFold(o.Name, want) {
					return selected(o.OptionID), nil
				}
			}
		}
		return nil, fmt.Errorf("acp: no option named by answers %v", d.Answers)
	default:
		return nil, fmt.Errorf("acp: unknown verdict %q", d.Verdict)
	}
	// pi-bolt maps allow_session to kind allow_once; the option name is the verdict.
	for _, o := range opts {
		if o.Name == string(d.Verdict) {
			return selected(o.OptionID), nil
		}
	}
	if id, ok := byKind(kind); ok {
		return selected(id), nil
	}
	return nil, fmt.Errorf("acp: no %s option offered", kind)
}

func (e *engine) Events() <-chan event.Event { return e.events }

func (e *engine) Send(text string) error { return e.send(text, true) }

// send starts a prompt, or while one runs either steers it (agent advertises
// _meta.steering) or queues the text as the next session/prompt.
func (e *engine) send(text string, allowSteer bool) error {
	e.mu.Lock()
	if e.sessionID == "" {
		e.mu.Unlock()
		return errors.New("acp: no session yet")
	}
	sid := e.sessionID
	if !e.busy {
		e.busy = true
		e.mu.Unlock()
		return e.startPrompt(text)
	}
	if allowSteer && e.steering {
		e.mu.Unlock()
		e.note(map[string]string{"send": "mid-turn: injected with " + steerMethod})
		return e.request(steerMethod, map[string]any{"sessionId": sid, "prompt": textBlocks(text)}, func(_ json.RawMessage, err *rpcError) {
			if err == nil {
				return
			}
			e.note(map[string]string{"steer_error": err.Error(), "send": "queued as next session/prompt"})
			_ = e.send(text, false)
		})
	}
	e.queue = append(e.queue, text)
	e.mu.Unlock()
	e.note(map[string]string{"send": "mid-turn: queued as next session/prompt (ACP has no mid-turn inject)"})
	return nil
}

func textBlocks(text string) []map[string]string {
	return []map[string]string{{"type": "text", "text": text}}
}

func (e *engine) startPrompt(text string) error {
	e.emit(event.Event{Kind: event.KindTurnStart})
	return e.request("session/prompt", map[string]any{"sessionId": e.SessionID(), "prompt": textBlocks(text)}, e.promptDone)
}

// promptDone runs on the read goroutine when session/prompt answers.
func (e *engine) promptDone(res json.RawMessage, rerr *rpcError) {
	e.flushText()
	end := event.Event{Kind: event.KindTurnEnd, Raw: append([]byte(nil), res...)}
	if rerr != nil {
		e.emit(event.Event{Kind: event.KindError, Text: rerr.Error(), IsError: true})
		end.IsError, end.Text = true, rerr.Message
	} else {
		var r struct {
			StopReason string `json:"stopReason"`
		}
		_ = json.Unmarshal(res, &r)
		end.Text = r.StopReason
	}
	e.emit(end)
	e.mu.Lock()
	if len(e.queue) == 0 {
		e.busy = false
		e.mu.Unlock()
		return
	}
	next := e.queue[0]
	e.queue = e.queue[1:]
	e.mu.Unlock()
	if err := e.startPrompt(next); err != nil {
		e.emit(event.Event{Kind: event.KindError, Text: err.Error(), IsError: true})
	}
}

func (e *engine) Interrupt() error {
	e.mu.Lock()
	pend := e.perms
	e.perms = map[string]pendingPerm{}
	e.mu.Unlock()
	// ACP: a cancelled turn answers its pending permission requests as cancelled.
	for _, p := range pend {
		_ = e.writeFrame(map[string]any{"jsonrpc": "2.0", "id": p.id, "result": map[string]any{"outcome": map[string]string{"outcome": "cancelled"}}})
	}
	return e.writeFrame(map[string]any{"jsonrpc": "2.0", "method": "session/cancel", "params": map[string]string{"sessionId": e.SessionID()}})
}

func (e *engine) SessionID() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.sessionID != "" {
		return e.sessionID
	}
	return e.opts.SessionID
}

// request sends a JSON-RPC request; cb runs on the read goroutine.
func (e *engine) request(method string, params any, cb reply) error {
	e.mu.Lock()
	e.nextID++
	id := e.nextID
	e.calls[id] = cb
	e.mu.Unlock()
	err := e.writeFrame(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err != nil {
		e.mu.Lock()
		delete(e.calls, id)
		e.mu.Unlock()
	}
	return err
}

// call is a blocking request used during the handshake.
func (e *engine) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	type out struct {
		res json.RawMessage
		err *rpcError
	}
	ch := make(chan out, 1)
	if err := e.request(method, params, func(r json.RawMessage, err *rpcError) { ch <- out{r, err} }); err != nil {
		return nil, err
	}
	select {
	case o := <-ch:
		if o.err != nil {
			return nil, o.err
		}
		return o.res, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (e *engine) writeFrame(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	e.wmu.Lock()
	defer e.wmu.Unlock()
	if e.stdin == nil {
		return errors.New("acp: not started")
	}
	e.log("out", b)
	_, err = e.stdin.Write(append(b, '\n'))
	return err
}

func (e *engine) log(dir string, frame []byte) {
	if e.opts.Transcript != nil {
		_ = e.opts.Transcript.Log(dir, frame)
	}
}

func (e *engine) note(v any) {
	if e.opts.Transcript != nil {
		_ = e.opts.Transcript.Note(v)
	}
}

func (e *engine) Close() error {
	e.wmu.Lock()
	if e.stdin != nil {
		_ = e.stdin.Close()
	}
	e.wmu.Unlock()
	select {
	case <-e.done:
	case <-time.After(closeGrace):
		_ = e.Kill()
	}
	return e.Wait()
}

func (e *engine) Kill() error {
	if e.cmd == nil || e.cmd.Process == nil {
		return errors.New("acp: not started")
	}
	return e.cmd.Process.Kill()
}

func (e *engine) Wait() error {
	<-e.done
	return e.waitErr
}
