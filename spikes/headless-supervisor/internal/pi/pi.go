// Package pi drives `pi --mode rpc` as an event.Engine. Permission gating
// comes from pi/gate.ts, which turns tool calls into extension_ui_request
// select prompts that Respond answers.
package pi

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
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/noamsto/dispatcher/spikes/headless-supervisor/internal/event"
)

const (
	defaultModel = "anthropic/claude-haiku-4-5"
	graceWait    = 10 * time.Second
)

const verdictDeny = event.Verdict("deny")

var binLine = regexp.MustCompile(`(?m)^\s*bin=(/nix/store/\S+/bin/pi)\s*$`)

// ResolveBin returns the real pi binary: HSUP_PI_BIN, else the first
// `bin=` line of the PATH wrapper that is not pi-bolt. It never returns the wrapper.
func ResolveBin() (string, error) {
	if b := os.Getenv("HSUP_PI_BIN"); b != "" {
		return b, nil
	}
	wrapper, err := exec.LookPath("pi")
	if err != nil {
		return "", fmt.Errorf("pi not on PATH and HSUP_PI_BIN unset: %w", err)
	}
	f, err := os.Open(wrapper)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	head, err := io.ReadAll(io.LimitReader(f, 64<<10))
	if err != nil {
		return "", err
	}
	for _, m := range binLine.FindAllSubmatch(head, -1) {
		if !bytes.Contains(m[1], []byte("pi-bolt")) {
			return string(m[1]), nil
		}
	}
	return "", errors.New("no real pi `bin=` line in the PATH wrapper; set HSUP_PI_BIN")
}

// ExampleExtension returns the path of a bundled pi example extension for the
// pi binary bin.
func ExampleExtension(bin, name string) string {
	return filepath.Join(filepath.Dir(bin), "..", "libexec", "pi", "examples", "extensions", name, "index.ts")
}

// Args builds the pi argv for o.
func Args(o event.Options) []string {
	model := o.Model
	if model == "" {
		model = os.Getenv("HSUP_PI_MODEL")
	}
	if model == "" {
		model = defaultModel
	}
	args := []string{
		"--mode", "rpc", "--model", model,
		"--session-dir", filepath.Join(o.Work, ".pi-sessions"),
		"--no-extensions", "--no-skills", "--no-prompt-templates", "--no-context-files",
	}
	if o.MCP {
		// --no-extensions also disables the built-in MCP extension.
		args = append(args, "-e", "builtin:mcp")
	}
	for _, e := range o.Extensions {
		args = append(args, "-e", e)
	}
	// Last, so no later tool_call handler can mutate event.input after the
	// gate approved it.
	if o.GatePath != "" {
		args = append(args, "-e", o.GatePath)
	}
	if o.SessionID != "" {
		args = append(args, "--session-id", o.SessionID)
	}
	if o.Approve {
		args = append(args, "--approve")
	}
	if !o.MCP {
		args = append(args, "--no-mcp")
	}
	return append(args, o.ExtraArgs...)
}

// childEnv is the parent env minus worker identity, lead, tmux and Claude Code
// session variables, plus extra.
func childEnv(parent, extra []string) []string {
	var out []string
	for _, kv := range parent {
		k, _, _ := strings.Cut(kv, "=")
		if k == "CREW_WORKER_ID" || k == "CREW_ID" || strings.HasPrefix(k, "CREW_LEAD_") || k == "TMUX" || k == "TMUX_PANE" ||
			k == "CLAUDECODE" || k == "CLAUDE_CODE_CHILD_SESSION" || k == "CLAUDE_CODE_ENTRYPOINT" ||
			k == "CLAUDE_CODE_SESSION_ID" || k == "CLAUDE_CODE_EXECPATH" ||
			k == "CLAUDE_CODE_SUBAGENT_EFFORT" || k == "CLAUDE_CODE_SSE_PORT" {
			continue
		}
		out = append(out, kv)
	}
	return append(out, extra...)
}

type engine struct {
	opts   event.Options
	events chan event.Event
	stop   chan struct{} // closed when the child is gone or killed
	done   chan struct{} // closed after Exit is emitted

	cmd   *exec.Cmd
	stdin io.WriteCloser

	wmu sync.Mutex // serialises stdin writes

	mu         sync.Mutex
	dec        decoder
	streaming  bool
	bashActive int
	sessionID  string
	denyReason string
	methods    map[string]string // permission request id -> ui method
	waitErr    error
	stopOnce   sync.Once
}

// New returns an unstarted pi engine.
func New(o event.Options) (event.Engine, error) {
	if o.Bin == "" {
		return nil, errors.New("pi: Options.Bin is empty")
	}
	return &engine{
		opts:    o,
		events:  make(chan event.Event, 256),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
		methods: map[string]string{},
	}, nil
}

func (e *engine) Events() <-chan event.Event { return e.events }

func (e *engine) Start(ctx context.Context) error {
	e.cmd = exec.CommandContext(ctx, e.opts.Bin, Args(e.opts)...)
	e.cmd.Dir = e.opts.Work
	e.cmd.ExtraFiles = e.opts.Inherit
	e.cmd.Env = childEnv(os.Environ(), e.opts.Env)
	stdin, err := e.cmd.StdinPipe()
	if err != nil {
		return err
	}
	// Cancelling ctx ends the child with EOF on stdin, then SIGKILL once the
	// grace runs out. No abort frame: a write could block and stall the kill.
	e.cmd.Cancel = stdin.Close
	e.cmd.WaitDelay = graceWait
	stdout, err := e.cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := e.cmd.StderrPipe()
	if err != nil {
		return err
	}
	e.stdin = stdin
	if err := e.cmd.Start(); err != nil {
		return err
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); e.readStdout(stdout) }()
	go func() { defer wg.Done(); e.readStderr(stderr) }()
	go func() {
		wg.Wait()
		err := e.cmd.Wait()
		e.mu.Lock()
		e.waitErr = err
		e.mu.Unlock()
		x := event.Event{Kind: event.KindExit, At: time.Now(), IsError: err != nil}
		if err != nil {
			x.Text = err.Error()
		}
		e.emit(x)
		e.shutdown()
		close(e.events)
		close(e.done)
	}()
	return e.write(map[string]any{"type": "get_state"})
}

func (e *engine) shutdown() { e.stopOnce.Do(func() { close(e.stop) }) }

func (e *engine) emit(ev event.Event) {
	select {
	case e.events <- ev:
	case <-e.stop:
		// Consumer is gone and the child is dead or killed; drop.
		select {
		case e.events <- ev:
		default:
		}
	}
}

func (e *engine) readStdout(r io.Reader) {
	br := bufio.NewReaderSize(r, 64<<10)
	for {
		line, err := br.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			e.handle(bytes.TrimRight(line, "\n"))
		}
		if err != nil {
			return
		}
	}
}

func (e *engine) readStderr(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		e.note(map[string]string{"stderr": sc.Text()})
	}
}

func (e *engine) note(v any) {
	if e.opts.Transcript != nil {
		_ = e.opts.Transcript.Note(v)
	}
}

func (e *engine) handle(line []byte) {
	if e.opts.Transcript != nil {
		_ = e.opts.Transcript.Log("in", json.RawMessage(line))
	}
	e.mu.Lock()
	eff := e.dec.decode(line, time.Now())
	if eff.streaming != nil {
		e.streaming = *eff.streaming
	}
	if eff.sessionID != "" {
		e.sessionID = eff.sessionID
	}
	switch {
	case eff.toolStart == "bash":
		e.bashActive++
	case eff.toolEnd && e.bashActive > 0:
		e.bashActive--
	}
	reason := e.denyReason
	if eff.denyAsk != "" {
		e.denyReason = ""
	}
	for _, ev := range eff.events {
		if ev.Req != nil {
			e.methods[ev.Req.ID] = eff.method
		}
	}
	e.mu.Unlock()

	if eff.denyAsk != "" {
		e.note(map[string]string{"sup": "deny-reason answered internally", "reason": reason})
		_ = e.write(uiResponse(eff.denyAsk, map[string]any{"value": reason}))
	}
	if eff.settled {
		_ = e.write(map[string]any{"type": "get_state"})
	}
	for _, ev := range eff.events {
		e.emit(ev)
	}
}

func uiResponse(id string, fields map[string]any) map[string]any {
	m := map[string]any{"type": "extension_ui_response", "id": id}
	for k, v := range fields {
		m[k] = v
	}
	return m
}

func (e *engine) write(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if e.opts.Transcript != nil {
		_ = e.opts.Transcript.Log("out", b)
	}
	e.wmu.Lock()
	defer e.wmu.Unlock()
	_, err = e.stdin.Write(append(b, '\n'))
	return err
}

func (e *engine) Send(text string) error {
	e.mu.Lock()
	streaming := e.streaming
	e.mu.Unlock()
	if streaming {
		return e.write(map[string]any{"type": "steer", "message": text})
	}
	return e.write(map[string]any{"type": "prompt", "message": text})
}

func (e *engine) Interrupt() error {
	e.mu.Lock()
	bash := e.bashActive > 0
	e.mu.Unlock()
	if bash {
		if err := e.write(map[string]any{"type": "abort_bash"}); err != nil {
			return err
		}
	}
	return e.write(map[string]any{"type": "abort"})
}

func (e *engine) Respond(r *event.Request, d event.Decision) error {
	e.mu.Lock()
	method := e.methods[r.ID]
	delete(e.methods, r.ID)
	if d.Verdict == verdictDeny {
		e.denyReason = d.Reason
	}
	e.mu.Unlock()
	if method == "" {
		method = "select"
	}
	return e.write(uiResponse(r.ID, responseFields(r, method, d)))
}

// responseFields maps a decision onto the extension_ui_response payload for
// the request's ui method.
func responseFields(r *event.Request, method string, d event.Decision) map[string]any {
	switch r.Detail.Kind {
	case "ask", "plan":
		if v := answerValue(d); v != "" {
			return map[string]any{"value": v}
		}
		if r.Detail.Kind == "plan" {
			return map[string]any{"value": planOption(r.Detail.Options, d.Verdict != verdictDeny)}
		}
		return map[string]any{"cancelled": true}
	}
	if method == "confirm" {
		return map[string]any{"confirmed": d.Verdict != verdictDeny}
	}
	if method == "input" {
		return map[string]any{"value": d.Reason}
	}
	if answerValue(d) != "" {
		return map[string]any{"value": answerValue(d)}
	}
	return map[string]any{"value": string(d.Verdict)}
}

// answerValue is the single chosen option of an answer decision.
func answerValue(d event.Decision) string {
	if d.Answers == nil {
		return ""
	}
	if v, ok := d.Answers["answer"]; ok {
		return v
	}
	for _, v := range d.Answers {
		return v
	}
	return ""
}

func planOption(opts []string, approve bool) string {
	prefix := "Stay"
	if approve {
		prefix = "Execute"
	}
	for _, o := range opts {
		if strings.HasPrefix(o, prefix) {
			return o
		}
	}
	return ""
}

func (e *engine) SessionID() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.sessionID != "" {
		return e.sessionID
	}
	return e.opts.SessionID
}

func (e *engine) Close() error {
	e.mu.Lock()
	streaming := e.streaming
	e.mu.Unlock()
	if streaming {
		_ = e.write(map[string]any{"type": "abort"})
		e.waitFor(func() bool {
			e.mu.Lock()
			defer e.mu.Unlock()
			return !e.streaming
		}, graceWait)
	}
	_ = e.stdin.Close()
	select {
	case <-e.done:
		return nil
	case <-time.After(graceWait):
		return e.Kill()
	}
}

func (e *engine) waitFor(cond func() bool, d time.Duration) {
	deadline := time.Now().Add(d)
	for !cond() && time.Now().Before(deadline) {
		select {
		case <-e.done:
			return
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func (e *engine) Kill() error {
	e.shutdown()
	if e.cmd == nil || e.cmd.Process == nil {
		return nil
	}
	if err := e.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return nil
}

func (e *engine) Wait() error {
	<-e.done
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.waitErr
}
