// Package event is the engine-neutral vocabulary shared by the claude and pi
// adapters and the supervisor. Options lives here rather than in the adapter
// packages, and this package imports transcript (never the reverse).
package event

import (
	"context"
	"encoding/json"
	"os"
	"time"

	"github.com/noamsto/dispatcher/spikes/headless-supervisor/internal/transcript"
)

type Kind string

const (
	KindInit       Kind = "init"
	KindTurnStart  Kind = "turn_start"
	KindText       Kind = "text"
	KindToolStart  Kind = "tool_start"
	KindToolResult Kind = "tool_result"
	KindPermission Kind = "permission"
	KindTurnEnd    Kind = "turn_end"
	KindRetry      Kind = "retry"
	KindError      Kind = "error"
	KindExit       Kind = "exit"
)

type Event struct {
	Kind      Kind
	At        time.Time
	Tool      string
	ToolUseID string
	Detail    Detail // for Permission and ToolStart
	Text      string // Text / ToolResult content / Error message
	IsError   bool
	SessionID string   // Init
	Req       *Request // Permission only
	Raw       json.RawMessage
}

// Detail.Kind is one of: bash | write | read | mcp | web | ask | plan | other.
type Detail struct {
	Kind      string
	Command   string
	Path      string
	URL       string
	Domain    string
	Server    string
	Options   []string
	Questions json.RawMessage
}

type Request struct {
	ID     string
	Engine string
	Detail Detail
	Tool   string
	Input  json.RawMessage
}

type Verdict string // allow_once | allow_session | deny | answer

type Decision struct {
	Verdict Verdict
	Reason  string
	Answers map[string]string
}

type Engine interface {
	Start(ctx context.Context) error
	// Events is closed when the child exits, after an Exit event.
	Events() <-chan Event
	// Send is a user message; mid-turn behaviour is engine-native.
	Send(text string) error
	Interrupt() error
	Respond(r *Request, d Decision) error
	// SessionID is the latest child-reported id, falling back to Options.SessionID.
	SessionID() string
	// Close is graceful: claude closes stdin; pi aborts (if streaming), then
	// closes stdin; both then Wait.
	Close() error
	Kill() error // SIGKILL
	Wait() error
}

type Options struct {
	Bin        string // resolved child binary
	Work       string // child cwd
	SessionID  string // new session id, or the id to resume
	Resume     bool
	Model      string // claude: "haiku"; pi: HSUP_PI_MODEL / "anthropic/claude-haiku-4-5"; error case: "stub/stub-model"
	Transcript *transcript.Writer
	Env        []string // extra KEY=VALUE appended after the child-env scrub
	// Inherit is passed to the child as extra fds (cmd.ExtraFiles). The
	// supervisor puts its session.lock here so the flock lives as long as the
	// child does, even if hsup itself dies.
	Inherit []*os.File

	// claude
	PermissionMode, MCPConfig, PromptToolMCP string
	Bare, Hooks                              bool

	// pi
	Extensions   []string // extra -e paths beyond gate.ts
	GatePath     string
	Approve, MCP bool
	ExtraArgs    []string // e.g. "--plan"
}
