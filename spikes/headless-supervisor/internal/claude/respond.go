package claude

import (
	"encoding/json"
	"errors"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/noamsto/dispatcher/spikes/headless-supervisor/internal/event"
)

func (e *engine) Respond(r *event.Request, d event.Decision) error {
	if r == nil {
		return errors.New("claude: nil request")
	}
	var payload any
	var err error
	if r.Engine == engineHookName {
		payload = HookOutput(d)
	} else {
		var note string
		if payload, note, err = permissionResult(r, d); err != nil {
			return err
		}
		if note != "" {
			e.note(map[string]string{"respond_note": note, "request_id": r.ID})
		}
	}
	return e.writeFrame(map[string]any{
		"type": "control_response",
		"response": map[string]any{
			"subtype":    "success",
			"request_id": r.ID,
			"response":   payload,
		},
	})
}

// HookOutput is the PreToolUse hook_callback reply for a decision.
func HookOutput(d event.Decision) map[string]any {
	out := map[string]any{"hookEventName": "PreToolUse"}
	switch d.Verdict {
	case "allow_once", "allow_session":
		out["permissionDecision"] = "allow"
	case "deny":
		out["permissionDecision"] = "deny"
		out["permissionDecisionReason"] = d.Reason
	default:
		out["permissionDecision"] = "deny"
		out["permissionDecisionReason"] = "supervisor: unsupported verdict " + string(d.Verdict)
	}
	return map[string]any{"hookSpecificOutput": out}
}

// PermissionResult is the can_use_tool reply (SDK PermissionResult) for a decision.
func PermissionResult(r *event.Request, d event.Decision) (map[string]any, error) {
	res, _, err := permissionResult(r, d)
	return res, err
}

// permissionResult also returns a note when it downgraded allow_session to
// allow_once because no safe rule scope could be derived.
func permissionResult(r *event.Request, d event.Decision) (map[string]any, string, error) {
	switch d.Verdict {
	case "deny":
		return map[string]any{"behavior": "deny", "message": d.Reason}, "", nil
	case "allow_once":
		return allow(r.Input), "", nil
	case "allow_session":
		res := allow(r.Input)
		rule := map[string]any{"toolName": r.Tool}
		content, ok := ruleContent(r)
		if !ok {
			return res, "allow_session downgraded to allow_once: no rule scope for tool " + r.Tool, nil
		}
		if content != "" {
			rule["ruleContent"] = content
		}
		res["updatedPermissions"] = []any{map[string]any{
			"type":        "addRules",
			"rules":       []any{rule},
			"behavior":    "allow",
			"destination": "session",
		}}
		return res, "", nil
	case "answer":
		in := map[string]json.RawMessage{}
		if len(r.Input) > 0 {
			if err := json.Unmarshal(r.Input, &in); err != nil {
				return nil, "", err
			}
		}
		answers, err := json.Marshal(d.Answers)
		if err != nil {
			return nil, "", err
		}
		in["answers"] = answers
		return map[string]any{"behavior": "allow", "updatedInput": in}, "", nil
	}
	return nil, "", errors.New("claude: unknown verdict " + string(d.Verdict))
}

func allow(input json.RawMessage) map[string]any {
	if len(input) == 0 {
		input = json.RawMessage(`{}`)
	}
	return map[string]any{"behavior": "allow", "updatedInput": input}
}

// ruleContent scopes a session rule: Bash to its first word, WebFetch to the
// domain, file tools to the target's directory. MCP tools are allowed per tool,
// the claude convention. ok is false when no scope can be derived, so the
// caller must not widen the grant to the whole tool.
func ruleContent(r *event.Request) (content string, ok bool) {
	switch {
	case r.Tool == "Bash":
		if f := strings.Fields(r.Detail.Command); len(f) > 0 {
			return f[0] + ":*", true
		}
	case r.Tool == "WebFetch":
		if r.Detail.Domain != "" {
			return "domain:" + r.Detail.Domain, true
		}
		if u, err := url.Parse(r.Detail.URL); err == nil && u.Hostname() != "" {
			return "domain:" + u.Hostname(), true
		}
	case fileTools[r.Tool]:
		p := r.Detail.Path
		if !filepath.IsAbs(p) {
			break
		}
		// claude reads "/x/**" as project-relative; "//x/**" is absolute. A
		// root or glob-bearing dir would widen the grant beyond the target.
		d := filepath.Dir(filepath.Clean(p))
		if d == "/" || strings.ContainsAny(d, "*?[]{}") {
			break
		}
		return "/" + d + "/**", true
	case strings.HasPrefix(r.Tool, "mcp__"):
		return "", true
	}
	return "", false
}

var fileTools = map[string]bool{
	"Write": true, "Edit": true, "MultiEdit": true, "NotebookEdit": true, "Read": true,
}
