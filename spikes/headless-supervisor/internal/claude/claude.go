// Package claude adapts the claude CLI's stream-json + control protocol
// (--permission-prompt-tool stdio) to event.Engine.
package claude

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
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/noamsto/dispatcher/spikes/headless-supervisor/internal/event"
)

const closeGrace = 10 * time.Second

// hookCallbackID is the callback id registered for the PreToolUse hook.
const hookCallbackID = "pre1"

// Args builds the child argv (without the binary).
func Args(o event.Options) []string {
	model := o.Model
	if model == "" {
		model = "haiku"
	}
	promptTool := "stdio"
	if o.PromptToolMCP != "" {
		promptTool = o.PromptToolMCP
	}
	args := []string{
		"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
		"--permission-prompt-tool", promptTool,
		"--model", model,
		"--setting-sources", "local", "--strict-mcp-config",
	}
	switch {
	case o.SessionID == "":
	case o.Resume:
		args = append(args, "--resume", o.SessionID)
	default:
		args = append(args, "--session-id", o.SessionID)
	}
	if o.PermissionMode != "" {
		args = append(args, "--permission-mode", o.PermissionMode)
	}
	if o.MCPConfig != "" {
		args = append(args, "--mcp-config", o.MCPConfig)
	}
	if o.Bare {
		args = append(args, "--bare")
	}
	return append(args, o.ExtraArgs...)
}

// ChildEnv is the parent environment minus the crew/tmux identity variables and
// Claude Code's own session markers, plus extra. Inherited session markers make
// the child's TUI refuse to persist its transcript.
func ChildEnv(environ, extra []string) []string {
	out := make([]string, 0, len(environ)+len(extra))
	for _, kv := range environ {
		k, _, _ := strings.Cut(kv, "=")
		switch {
		case k == "CREW_WORKER_ID", k == "CREW_ID", k == "TMUX", k == "TMUX_PANE",
			k == "CLAUDECODE", k == "CLAUDE_CODE_CHILD_SESSION", k == "CLAUDE_CODE_ENTRYPOINT",
			k == "CLAUDE_CODE_SESSION_ID", k == "CLAUDE_CODE_EXECPATH",
			k == "CLAUDE_CODE_SUBAGENT_EFFORT", k == "CLAUDE_CODE_SSE_PORT",
			strings.HasPrefix(k, "CREW_LEAD_"):
			continue
		}
		out = append(out, kv)
	}
	return append(out, extra...)
}

var execLine = regexp.MustCompile(`(?m)^\s*exec\s+-a\s+claude\s+(\S+)`)

// ResolveBin returns the real claude binary: HSUP_CLAUDE_BIN if set, else the
// path named by the PATH wrapper's `exec -a claude <bin>` line. It never
// returns the wrapper itself.
func ResolveBin() (string, error) {
	if b := os.Getenv("HSUP_CLAUDE_BIN"); b != "" {
		return b, nil
	}
	p, err := exec.LookPath("claude")
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	if !bytes.HasPrefix(data, []byte("#!")) {
		return p, nil
	}
	return ParseWrapper(data)
}

// ParseWrapper extracts the binary from the last `exec -a claude` line.
func ParseWrapper(script []byte) (string, error) {
	m := execLine.FindAllSubmatch(script, -1)
	if len(m) == 0 {
		return "", errors.New("claude wrapper has no `exec -a claude <bin>` line; set HSUP_CLAUDE_BIN")
	}
	return string(m[len(m)-1][1]), nil
}

type engine struct {
	opts event.Options

	cmd    *exec.Cmd
	stdin  io.WriteCloser
	events chan event.Event

	wmu       sync.Mutex // guards stdin writes and closeOnce state
	stdinDone bool
	reqSeq    int

	smu     sync.Mutex
	session string

	done    chan struct{}
	waitErr error
}

// New returns an unstarted claude engine.
func New(opts event.Options) (event.Engine, error) {
	if opts.Bin == "" {
		return nil, errors.New("claude: Options.Bin is empty")
	}
	return &engine{
		opts:    opts,
		events:  make(chan event.Event, 1024),
		done:    make(chan struct{}),
		session: opts.SessionID,
	}, nil
}

func (e *engine) Start(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, e.opts.Bin, Args(e.opts)...)
	cmd.ExtraFiles = e.opts.Inherit
	cmd.Dir = e.opts.Work
	cmd.Env = ChildEnv(os.Environ(), e.opts.Env)
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

	var errWG sync.WaitGroup
	errWG.Add(1)
	go func() {
		defer errWG.Done()
		e.pumpStderr(stderr)
	}()
	go func() {
		e.pumpStdout(stdout)
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

	if e.opts.Hooks {
		return e.writeFrame(map[string]any{
			"type":       "control_request",
			"request_id": e.nextID(),
			"request": map[string]any{
				"subtype": "initialize",
				"hooks": map[string]any{
					"PreToolUse": []any{map[string]any{
						"matcher":         "Bash",
						"hookCallbackIds": []string{hookCallbackID},
					}},
				},
			},
		})
	}
	return nil
}

func (e *engine) pumpStdout(r io.Reader) {
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
			return
		}
	}
}

func (e *engine) handleLine(line []byte) {
	e.log("in", line)
	if id, _, ok := UnsupportedRequest(line); ok {
		_ = e.writeFrame(map[string]any{
			"type": "control_response",
			"response": map[string]any{
				"subtype":    "error",
				"request_id": id,
				"error":      "unsupported by hsup",
			},
		})
	}
	evs, err := Decode(line, time.Now())
	if err != nil {
		e.events <- event.Event{Kind: event.KindError, At: time.Now(), Text: "undecodable frame: " + err.Error(), IsError: true, Raw: append([]byte(nil), line...)}
		return
	}
	for _, ev := range evs {
		if ev.Kind == event.KindInit && ev.SessionID != "" {
			e.smu.Lock()
			e.session = ev.SessionID
			e.smu.Unlock()
		}
		e.events <- ev
	}
}

func (e *engine) pumpStderr(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 8<<20)
	for sc.Scan() {
		e.note(map[string]string{"stderr": sc.Text()})
	}
	if err := sc.Err(); err != nil {
		e.note(map[string]string{"stderr_error": err.Error()})
	}
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

func (e *engine) nextID() string {
	e.wmu.Lock()
	defer e.wmu.Unlock()
	e.reqSeq++
	return fmt.Sprintf("hsup-%d", e.reqSeq)
}

func (e *engine) writeFrame(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	e.wmu.Lock()
	defer e.wmu.Unlock()
	if e.stdin == nil || e.stdinDone {
		return errors.New("claude: stdin closed")
	}
	e.log("out", b)
	_, err = e.stdin.Write(append(b, '\n'))
	return err
}

func (e *engine) Events() <-chan event.Event { return e.events }

func (e *engine) Send(text string) error {
	return e.writeFrame(map[string]any{
		"type":               "user",
		"message":            map[string]any{"role": "user", "content": text},
		"parent_tool_use_id": nil,
	})
}

func (e *engine) Interrupt() error {
	return e.writeFrame(map[string]any{
		"type":       "control_request",
		"request_id": e.nextID(),
		"request":    map[string]any{"subtype": "interrupt"},
	})
}

func (e *engine) SessionID() string {
	e.smu.Lock()
	defer e.smu.Unlock()
	return e.session
}

func (e *engine) closeStdin() {
	e.wmu.Lock()
	defer e.wmu.Unlock()
	if e.stdin != nil && !e.stdinDone {
		e.stdinDone = true
		_ = e.stdin.Close()
	}
}

func (e *engine) Close() error {
	e.closeStdin()
	select {
	case <-e.done:
	case <-time.After(closeGrace):
		_ = e.Kill()
	}
	return e.Wait()
}

func (e *engine) Kill() error {
	if e.cmd == nil || e.cmd.Process == nil {
		return errors.New("claude: not started")
	}
	return e.cmd.Process.Kill()
}

func (e *engine) Wait() error {
	<-e.done
	return e.waitErr
}
