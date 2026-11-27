package claude

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/noamsto/dispatcher/spikes/headless-supervisor/internal/event"
)

// Request.Engine values; Respond picks the reply shape from them.
const (
	engineName     = "claude"
	engineHookName = "claude-hook"
)

type frame struct {
	Type      string          `json:"type"`
	Subtype   string          `json:"subtype"`
	SessionID string          `json:"session_id"`
	RequestID string          `json:"request_id"`
	Request   json.RawMessage `json:"request"`
	Message   struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
	IsError       bool   `json:"is_error"`
	Result        string `json:"result"`
	ErrorStatus   any    `json:"error_status"`
	Error         string `json:"error"`
	RateLimitInfo struct {
		Status string `json:"status"`
	} `json:"rate_limit_info"`
}

type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

type controlRequest struct {
	Subtype     string          `json:"subtype"`
	ToolName    string          `json:"tool_name"`
	Input       json.RawMessage `json:"input"`
	BlockedPath string          `json:"blocked_path"`
	ToolUseID   string          `json:"tool_use_id"`
	MCPServer   *struct {
		Name string `json:"name"`
	} `json:"mcp_server"`
}

// UnsupportedRequest reports the id and subtype of a control_request that
// Decode does not handle, so the engine can answer it instead of leaving the
// child blocked.
func UnsupportedRequest(line []byte) (id, subtype string, ok bool) {
	var f frame
	if json.Unmarshal(line, &f) != nil || f.Type != "control_request" {
		return "", "", false
	}
	var cr controlRequest
	if json.Unmarshal(f.Request, &cr) != nil {
		return "", "", false
	}
	switch cr.Subtype {
	case "can_use_tool", "hook_callback":
		return "", "", false
	}
	return f.RequestID, cr.Subtype, true
}

// Decode turns one raw stdout line into zero or more events.
func Decode(line []byte, at time.Time) ([]event.Event, error) {
	var f frame
	if err := json.Unmarshal(line, &f); err != nil {
		return nil, err
	}
	raw := json.RawMessage(append([]byte(nil), line...))
	mk := func(k event.Kind) event.Event { return event.Event{Kind: k, At: at, Raw: raw} }

	switch f.Type {
	case "system":
		switch f.Subtype {
		case "init":
			ev := mk(event.KindInit)
			ev.SessionID = f.SessionID
			return []event.Event{ev}, nil
		case "api_retry":
			ev := mk(event.KindRetry)
			ev.Text = fmt.Sprintf("error_status=%v error=%s", f.ErrorStatus, f.Error)
			return []event.Event{ev}, nil
		}
	case "rate_limit_event":
		if f.RateLimitInfo.Status == "" || strings.HasPrefix(f.RateLimitInfo.Status, "allowed") {
			return nil, nil
		}
		ev := mk(event.KindRetry)
		ev.Text = "rate_limit status=" + f.RateLimitInfo.Status
		return []event.Event{ev}, nil
	case "assistant":
		return decodeAssistant(f.Message.Content, mk), nil
	case "user":
		return decodeUser(f.Message.Content, mk), nil
	case "result":
		ev := mk(event.KindTurnEnd)
		ev.Text = f.Result
		ev.IsError = f.IsError || (f.Subtype != "" && f.Subtype != "success")
		return []event.Event{ev}, nil
	case "control_request":
		ev, ok, err := decodeControlRequest(f, mk)
		if err != nil || !ok {
			return nil, err
		}
		return []event.Event{ev}, nil
	}
	return nil, nil
}

func decodeAssistant(content json.RawMessage, mk func(event.Kind) event.Event) []event.Event {
	var blocks []block
	if json.Unmarshal(content, &blocks) != nil {
		return nil
	}
	var out []event.Event
	for _, b := range blocks {
		switch b.Type {
		case "text":
			ev := mk(event.KindText)
			ev.Text = b.Text
			out = append(out, ev)
		case "tool_use":
			ev := mk(event.KindToolStart)
			ev.Tool, ev.ToolUseID = b.Name, b.ID
			ev.Detail = detail(b.Name, b.Input, "", "")
			out = append(out, ev)
		}
	}
	return out
}

func decodeUser(content json.RawMessage, mk func(event.Kind) event.Event) []event.Event {
	var blocks []block
	if json.Unmarshal(content, &blocks) != nil {
		return nil
	}
	var out []event.Event
	for _, b := range blocks {
		if b.Type != "tool_result" {
			continue
		}
		ev := mk(event.KindToolResult)
		ev.ToolUseID, ev.IsError = b.ToolUseID, b.IsError
		ev.Text = resultText(b.Content)
		out = append(out, ev)
	}
	return out
}

func resultText(content json.RawMessage) string {
	var s string
	if json.Unmarshal(content, &s) == nil {
		return s
	}
	var blocks []block
	if json.Unmarshal(content, &blocks) != nil {
		return string(content)
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func decodeControlRequest(f frame, mk func(event.Kind) event.Event) (event.Event, bool, error) {
	var cr controlRequest
	if err := json.Unmarshal(f.Request, &cr); err != nil {
		return event.Event{}, false, err
	}
	var req event.Request
	switch cr.Subtype {
	case "can_use_tool":
		server := ""
		if cr.MCPServer != nil {
			server = cr.MCPServer.Name
		}
		req = event.Request{
			ID: f.RequestID, Engine: engineName, Tool: cr.ToolName, Input: cr.Input,
			Detail: detail(cr.ToolName, cr.Input, cr.BlockedPath, server),
		}
	case "hook_callback":
		var hi struct {
			ToolName  string          `json:"tool_name"`
			ToolInput json.RawMessage `json:"tool_input"`
			ToolUseID string          `json:"tool_use_id"`
		}
		if err := json.Unmarshal(cr.Input, &hi); err != nil {
			return event.Event{}, false, fmt.Errorf("hook_callback %s: %w", f.RequestID, err)
		}
		req = event.Request{
			ID: f.RequestID, Engine: engineHookName, Tool: hi.ToolName, Input: hi.ToolInput,
			Detail: detail(hi.ToolName, hi.ToolInput, "", ""),
		}
		cr.ToolUseID = hi.ToolUseID
	default:
		ev := mk(event.KindError)
		ev.Text, ev.IsError = "unhandled control_request "+cr.Subtype, true
		return ev, true, nil
	}
	ev := mk(event.KindPermission)
	ev.Tool, ev.ToolUseID, ev.Detail, ev.Req = req.Tool, cr.ToolUseID, req.Detail, &req
	return ev, true, nil
}

// detail maps a tool call to the engine-neutral Detail. blocked is the CLI's
// blocked_path (permission requests only); server the mcp_server name.
func detail(tool string, input json.RawMessage, blocked, server string) event.Detail {
	var in map[string]json.RawMessage
	_ = json.Unmarshal(input, &in)
	str := func(k string) string {
		var s string
		_ = json.Unmarshal(in[k], &s)
		return s
	}
	first := func(keys ...string) string {
		for _, k := range keys {
			if s := str(k); s != "" {
				return s
			}
		}
		return blocked
	}
	switch {
	case tool == "Bash":
		return event.Detail{Kind: "bash", Command: str("command")}
	case tool == "Write" || tool == "Edit" || tool == "MultiEdit" || tool == "NotebookEdit":
		return event.Detail{Kind: "write", Path: first("file_path", "notebook_path")}
	case tool == "Read" || tool == "Glob" || tool == "Grep":
		return event.Detail{Kind: "read", Path: first("file_path", "path")}
	case strings.HasPrefix(tool, "mcp__"):
		if server == "" {
			if parts := strings.SplitN(tool, "__", 3); len(parts) == 3 {
				server = parts[1]
			}
		}
		return event.Detail{Kind: "mcp", Server: server}
	case tool == "WebFetch" || tool == "WebSearch":
		d := event.Detail{Kind: "web", URL: str("url")}
		if u, err := url.Parse(d.URL); err == nil {
			d.Domain = u.Hostname()
		}
		return d
	case tool == "AskUserQuestion":
		return event.Detail{Kind: "ask", Questions: in["questions"], Options: questionOptions(in["questions"])}
	case tool == "ExitPlanMode":
		return event.Detail{Kind: "plan"}
	}
	return event.Detail{Kind: "other"}
}

// questionOptions lists the option labels of the first question.
func questionOptions(questions json.RawMessage) []string {
	var qs []struct {
		Options []struct {
			Label string `json:"label"`
		} `json:"options"`
	}
	if json.Unmarshal(questions, &qs) != nil || len(qs) == 0 {
		return nil
	}
	var out []string
	for _, o := range qs[0].Options {
		out = append(out, o.Label)
	}
	return out
}
