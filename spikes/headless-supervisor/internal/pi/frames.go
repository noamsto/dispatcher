package pi

import (
	"encoding/json"
	"net/url"
	"strings"
	"time"

	"github.com/noamsto/dispatcher/spikes/headless-supervisor/internal/event"
)

// Title prefixes and titles used by pi/gate.ts to tag its extension UI calls.
const (
	titlePermission = "hsup-permission "
	titleAsk        = "hsup-ask "
	titleDenyReason = "hsup-deny-reason"
	titlePlan       = "Plan mode - what next?"
)

// frame is the union of the pi RPC fields the decoder reads.
type frame struct {
	Type    string          `json:"type"`
	ID      string          `json:"id"`
	Command string          `json:"command"`
	Success *bool           `json:"success"`
	Error   string          `json:"error"`
	Data    json.RawMessage `json:"data"`

	// message_update / message_end
	AssistantMessageEvent struct {
		Type  string `json:"type"`
		Delta string `json:"delta"`
	} `json:"assistantMessageEvent"`
	Message json.RawMessage `json:"message"`

	// tool_execution_*
	ToolCallID string          `json:"toolCallId"`
	ToolName   string          `json:"toolName"`
	Args       json.RawMessage `json:"args"`
	Result     json.RawMessage `json:"result"`
	IsError    bool            `json:"isError"`

	// agent_settled
	Aborted bool `json:"aborted"`

	// auto_retry_*
	Attempt    int    `json:"attempt"`
	FinalError string `json:"finalError"`
	ErrMessage string `json:"errorMessage"`

	// extension_ui_request
	Method  string          `json:"method"`
	Title   string          `json:"title"`
	Options json.RawMessage `json:"options"`
}

type message struct {
	Role         string `json:"role"`
	StopReason   string `json:"stopReason"`
	ErrorMessage string `json:"errorMessage"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// effect carries the state changes a frame implies beyond its events.
type effect struct {
	events    []event.Event
	streaming *bool  // agent_start / agent_settled
	sessionID string // get_state response
	settled   bool   // agent_settled: caller refreshes get_state
	denyAsk   string // id of the hsup-deny-reason input to answer internally
	method    string // ui method of a surfaced Permission, for Respond
	toolStart string // tool_execution_start: tool name
	toolEnd   bool
}

// decoder holds the per-run state decoding needs.
type decoder struct {
	text      strings.Builder
	streaming bool
	sessionID string
	errText   string // error of the current run, reported at agent_settled
}

func (d *decoder) decode(raw []byte, now time.Time) effect {
	var f frame
	var eff effect
	if json.Unmarshal(raw, &f) != nil {
		return eff
	}
	ev := func(k event.Kind) event.Event {
		return event.Event{Kind: k, At: now, Raw: append(json.RawMessage(nil), raw...)}
	}
	switch f.Type {
	case "agent_start":
		t := true
		eff.streaming = &t
		d.errText = ""
		if !d.streaming {
			eff.events = append(eff.events, ev(event.KindTurnStart))
		}
		d.streaming = true
	case "agent_settled":
		fl := false
		eff.streaming = &fl
		eff.settled = true
		d.streaming = false
		e := ev(event.KindTurnEnd)
		e.IsError, e.Text = d.errText != "" || f.Aborted, d.errText
		if f.Aborted && e.Text == "" {
			e.Text = "aborted"
		}
		eff.events = append(eff.events, e)
	case "message_update":
		if f.AssistantMessageEvent.Type == "text_delta" {
			d.text.WriteString(f.AssistantMessageEvent.Delta)
		}
	case "message_end":
		eff.events = append(eff.events, d.messageEnd(f, ev)...)
	case "tool_execution_start":
		e := ev(event.KindToolStart)
		e.Tool, e.ToolUseID, e.Detail = f.ToolName, f.ToolCallID, toolDetail(f.ToolName, f.Args)
		eff.toolStart = f.ToolName
		eff.events = append(eff.events, e)
	case "tool_execution_end":
		e := ev(event.KindToolResult)
		e.Tool, e.ToolUseID, e.IsError = f.ToolName, f.ToolCallID, f.IsError
		e.Text = resultText(f.Result)
		eff.toolEnd = true
		eff.events = append(eff.events, e)
	case "auto_retry_start":
		e := ev(event.KindRetry)
		e.Text = f.ErrMessage
		eff.events = append(eff.events, e)
	case "auto_retry_end":
		if f.Success != nil && !*f.Success {
			e := ev(event.KindError)
			e.IsError, e.Text = true, f.FinalError
			d.errText = f.FinalError
			eff.events = append(eff.events, e)
		}
	case "extension_error":
		e := ev(event.KindError)
		e.IsError, e.Text = true, string(raw)
		eff.events = append(eff.events, e)
	case "response":
		d.response(f, ev, &eff)
	case "extension_ui_request":
		d.uiRequest(f, raw, ev, &eff)
	}
	return eff
}

func (d *decoder) response(f frame, ev func(event.Kind) event.Event, eff *effect) {
	if f.Success != nil && !*f.Success {
		e := ev(event.KindError)
		e.IsError, e.Text = true, f.Command+": "+f.Error
		eff.events = append(eff.events, e)
		return
	}
	if f.Command != "get_state" {
		return
	}
	var data struct {
		SessionID string `json:"sessionId"`
	}
	if json.Unmarshal(f.Data, &data) != nil || data.SessionID == "" {
		return
	}
	eff.sessionID = data.SessionID
	if d.sessionID == data.SessionID {
		return
	}
	d.sessionID = data.SessionID
	e := ev(event.KindInit)
	e.SessionID = data.SessionID
	eff.events = append(eff.events, e)
}

func (d *decoder) messageEnd(f frame, ev func(event.Kind) event.Event) []event.Event {
	var m message
	var out []event.Event
	if json.Unmarshal(f.Message, &m) != nil || m.Role != "assistant" {
		d.text.Reset()
		return nil
	}
	text := assistantText(f.Message)
	if text == "" {
		text = d.text.String()
	}
	d.text.Reset()
	if text != "" {
		e := ev(event.KindText)
		e.Text = text
		out = append(out, e)
	}
	switch m.StopReason {
	case "error":
		e := ev(event.KindError)
		e.IsError, e.Text = true, m.ErrorMessage
		d.errText = m.ErrorMessage
		out = append(out, e)
	case "aborted":
		if d.errText == "" {
			d.errText = "aborted"
		}
	}
	return out
}

func (d *decoder) uiRequest(f frame, raw []byte, ev func(event.Kind) event.Event, eff *effect) {
	switch f.Method {
	case "select", "confirm", "input", "editor":
	default:
		return // notify, setStatus, setWidget, setTitle, ...: transcript only
	}
	if f.Method == "input" && f.Title == titleDenyReason {
		eff.denyAsk = f.ID
		return
	}
	var opts []string
	_ = json.Unmarshal(f.Options, &opts)
	e := ev(event.KindPermission)
	req := &event.Request{ID: f.ID, Engine: "pi"}
	switch {
	case strings.HasPrefix(f.Title, titlePermission):
		req.Detail, req.Tool, req.Input = permissionDetail(strings.TrimPrefix(f.Title, titlePermission))
	case strings.HasPrefix(f.Title, titleAsk):
		var q struct {
			Question string   `json:"question"`
			Options  []string `json:"options"`
		}
		payload := strings.TrimPrefix(f.Title, titleAsk)
		_ = json.Unmarshal([]byte(payload), &q)
		req.Detail = event.Detail{Kind: "ask", Options: q.Options, Questions: json.RawMessage(payload)}
		req.Tool = "ask_user"
	case f.Title == titlePlan:
		req.Detail = event.Detail{Kind: "plan", Options: opts}
		req.Tool = "plan"
	default:
		req.Detail = event.Detail{Kind: "other", Options: opts}
		req.Tool = f.Method
		req.Input = append(json.RawMessage(nil), raw...)
	}
	e.Tool, e.Detail, e.Req = req.Tool, req.Detail, req
	eff.method = f.Method
	eff.events = append(eff.events, e)
}

// permissionDetail parses the JSON gate.ts embeds in the select title.
func permissionDetail(payload string) (event.Detail, string, json.RawMessage) {
	var p struct {
		Kind    string          `json:"kind"`
		Tool    string          `json:"tool"`
		Command string          `json:"command"`
		Path    string          `json:"path"`
		URL     string          `json:"url"`
		Domain  string          `json:"domain"`
		Server  string          `json:"server"`
		Input   json.RawMessage `json:"input"`
	}
	if json.Unmarshal([]byte(payload), &p) != nil {
		return event.Detail{Kind: "other"}, "", nil
	}
	if p.Kind == "" {
		p.Kind = "other"
	}
	return event.Detail{Kind: p.Kind, Command: p.Command, Path: p.Path, URL: p.URL, Domain: p.Domain, Server: p.Server}, p.Tool, p.Input
}

func toolDetail(tool string, args json.RawMessage) event.Detail {
	var a struct {
		Command string `json:"command"`
		Path    string `json:"path"`
		URL     string `json:"url"`
	}
	_ = json.Unmarshal(args, &a)
	switch tool {
	case "bash":
		return event.Detail{Kind: "bash", Command: a.Command}
	case "write", "edit":
		return event.Detail{Kind: "write", Path: a.Path}
	case "read", "ls", "grep", "find":
		return event.Detail{Kind: "read", Path: a.Path}
	case "fetch_content", "web_search":
		u, _ := url.Parse(a.URL)
		d := event.Detail{Kind: "web", URL: a.URL}
		if u != nil {
			d.Domain = u.Hostname()
		}
		return d
	}
	if strings.HasPrefix(tool, "mcp__") {
		return event.Detail{Kind: "mcp"}
	}
	return event.Detail{Kind: "other"}
}

func assistantText(msg json.RawMessage) string {
	var m struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(msg, &m) != nil {
		return ""
	}
	return blocksText(m.Content)
}

func resultText(res json.RawMessage) string {
	var r struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(res, &r) != nil {
		return ""
	}
	return blocksText(r.Content)
}

func blocksText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []contentBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var b strings.Builder
	for _, c := range blocks {
		if c.Type == "text" {
			b.WriteString(c.Text)
		}
	}
	return b.String()
}
