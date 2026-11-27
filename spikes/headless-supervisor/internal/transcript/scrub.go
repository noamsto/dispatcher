package transcript

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"os"
	"os/user"
	"regexp"
	"slices"
	"strings"
	"sync"
)

// Scrubber rewrites frames before they reach disk. Each path rule replaces its
// prefix with its placeholder in a single pass, so placeholders are never
// rescanned and nested directories take the longest matching rule.
type Scrubber struct {
	Paths []PathRule
}

// PathRule maps an absolute directory prefix to the placeholder written in its place.
type PathRule struct {
	Prefix      string
	Placeholder string
}

// wordStart captures the byte before a token so a prefix such as "sk-" only
// matches at a word start (Go regexp has no lookbehind); replacements keep ${1}.
const wordStart = `(^|[^A-Za-z0-9_])`

var (
	secretValueRes = []*regexp.Regexp{
		regexp.MustCompile(wordStart + `sk-ant-[A-Za-z0-9_-]+`),
		regexp.MustCompile(wordStart + `sk-or-[A-Za-z0-9_-]+`),
		regexp.MustCompile(wordStart + `sk-[A-Za-z0-9_-]{20,}`),
		regexp.MustCompile(wordStart + `gh[pousr]_[A-Za-z0-9]{20,}`),
		regexp.MustCompile(wordStart + `github_pat_[A-Za-z0-9_]+`),
		regexp.MustCompile(wordStart + `AIza[0-9A-Za-z_-]{30,}`),
		regexp.MustCompile(`eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`),
	}
	bearerRe    = regexp.MustCompile(`Bearer [A-Za-z0-9._~+/=-]+`)
	envLineRe   = regexp.MustCompile(`(?m)^([A-Z][A-Z0-9_]*(?:KEY|TOKEN|SECRET|PASSWORD)[A-Z0-9_]*)=.*$`)
	emailRe     = regexp.MustCompile(`[^\s"@]+@[^\s"@]+\.[a-z]{2,}`)
	secretKeyRe = regexp.MustCompile(`(?i)(api[-_]?key|token|secret|authorization|password|cookie|credential)`)

	currentUser = sync.OnceValues(user.Current)
	hostRe      = sync.OnceValue(func() *regexp.Regexp {
		h, err := os.Hostname()
		if err != nil || h == "" {
			return nil
		}
		return regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(h) + `\b`)
	})
)

// Scrub returns raw with paths, secrets and env-like fields removed, re-encoded
// compactly. It fails closed: a frame that cannot be decoded is not written.
func (s *Scrubber) Scrub(raw json.RawMessage) (json.RawMessage, error) {
	u, err := currentUser()
	if err != nil {
		return nil, fmt.Errorf("scrub: current user: %w", err)
	}
	p := pass{paths: s.pathRules(), host: hostRe()}
	if u.Username != "" {
		p.user = regexp.MustCompile(`\b` + regexp.QuoteMeta(u.Username) + `\b`)
	}

	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("scrub: decode frame: %w", err)
	}
	return marshal(p.walk(reduceFrame(v)))
}

// pathRules returns the rules with empty prefixes dropped, longest prefix first.
func (s *Scrubber) pathRules() pathRules {
	var rules pathRules
	for _, r := range s.Paths {
		if r.Prefix != "" {
			rules = append(rules, r)
		}
	}
	slices.SortStableFunc(rules, func(a, b PathRule) int { return cmp.Compare(len(b.Prefix), len(a.Prefix)) })
	return rules
}

// pathRules rewrites in one pass. A prefix only matches on a path boundary, so
// "/tmp" does not match inside "/tmp.x", and emitted placeholders are never rescanned.
type pathRules []PathRule

const pathChars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789._/-"

func (rs pathRules) replace(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if r, ok := rs.match(s, i); ok {
			b.WriteString(r.Placeholder)
			i += len(r.Prefix)
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// match requires a boundary on both sides: the previous byte must not be a
// path character, so a fragment ending "/tmp" cannot start a rule on "/var/tmp/x".
func (rs pathRules) match(s string, i int) (PathRule, bool) {
	if i > 0 && strings.IndexByte(pathChars, s[i-1]) >= 0 {
		return PathRule{}, false
	}
	for _, r := range rs {
		rest, found := strings.CutPrefix(s[i:], r.Prefix)
		if found && (rest == "" || rest[0] == '/') {
			return r, true
		}
	}
	return PathRule{}, false
}

type pass struct {
	paths pathRules
	user  *regexp.Regexp // nil when the OS user name is empty
	host  *regexp.Regexp // nil when the hostname is unknown
}

func (p pass) walk(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			switch {
			case dropKey(k):
				delete(t, k)
			case inventoryKeys[k] && isCollection(val):
				t[k] = fmt.Sprintf("[dropped: %d]", collectionLen(val))
			case secretKey(k):
				t[k] = "[redacted]"
			default:
				t[k] = p.walk(val)
			}
		}
		return t
	case []any:
		for i, e := range t {
			t[i] = p.walk(e)
		}
		return t
	case string:
		return p.str(t)
	}
	return v
}

func (p pass) str(s string) string {
	s = p.paths.replace(s)
	for _, re := range secretValueRes {
		s = re.ReplaceAllString(s, "${1}[redacted]")
	}
	s = envLineRe.ReplaceAllString(s, "${1}=[redacted]")
	s = bearerRe.ReplaceAllLiteralString(s, "Bearer [redacted]")
	s = emailRe.ReplaceAllLiteralString(s, "[email]")
	if p.host != nil {
		s = p.host.ReplaceAllLiteralString(s, "$HOST")
	}
	if p.user != nil {
		s = p.user.ReplaceAllLiteralString(s, "$USER")
	}
	return s
}

// secretKey reports whether k names a credential. Token counters such as
// input_tokens are usage numbers, not secrets.
func secretKey(k string) bool {
	lk := strings.ToLower(k)
	if strings.HasSuffix(lk, "tokens") || strings.HasSuffix(lk, "token_count") || strings.HasSuffix(lk, "tokencount") {
		return false
	}
	return secretKeyRe.MatchString(k)
}

// inventoryKeys name claude frame fields that list the owner's installed
// commands, skills, agents, plugins or MCP servers.
var inventoryKeys = map[string]bool{
	"slash_commands": true, "skills": true, "agents": true, "plugins": true,
	"mcp_servers": true, "output_style": true, "commands": true,
}

func isCollection(v any) bool {
	switch v.(type) {
	case []any, map[string]any:
		return true
	}
	return false
}

func collectionLen(v any) int {
	switch t := v.(type) {
	case []any:
		return len(t)
	case map[string]any:
		return len(t)
	}
	return 0
}

func dropKey(k string) bool {
	switch k {
	case "account", "authStatus", "credential_expires_at", "email", "env", "environment":
		return true
	}
	return strings.HasSuffix(k, "_dir") || strings.HasSuffix(k, "Dir")
}

// reduceFrame keeps only the fields the transcript needs from the claude frames
// that carry an owner's inventory, account or rate-limit detail.
func reduceFrame(v any) any {
	m, ok := v.(map[string]any)
	if !ok {
		return v
	}
	if reduceACP(m) {
		return m
	}
	switch m["type"] {
	case "system":
		if m["subtype"] == "init" {
			return reduceInit(m)
		}
	case "control_response":
		if payload, ok := initReplyPayload(m); ok {
			return reduceInitReply(m, payload)
		}
	case "rate_limit_event":
		if info, ok := m["rate_limit_info"].(map[string]any); ok {
			m["rate_limit_info"] = pick(info, "status", "rateLimitType", "resetsAt")
		}
	}
	return v
}

// reduceACP strips the owner's command, model, mode and config inventory from
// ACP session/update notifications and session setup results.
func reduceACP(m map[string]any) bool {
	reduced := false
	if m["method"] == "session/update" {
		params, _ := m["params"].(map[string]any)
		update, _ := params["update"].(map[string]any)
		if cmds, ok := update["availableCommands"]; ok {
			update["availableCommands"] = fmt.Sprintf("[dropped: %d commands]", collectionLen(cmds))
			reduced = true
		}
		if meta, ok := update["_meta"].(map[string]any); ok {
			if rl, ok := meta["_claude/rateLimit"].(map[string]any); ok {
				meta["_claude/rateLimit"] = pick(rl, "status", "rateLimitType", "resetsAt")
				reduced = true
			}
		}
	}
	if result, ok := m["result"].(map[string]any); ok {
		for _, k := range []string{"models", "modes", "configOptions"} {
			inner, present := result[k]
			if !present {
				continue
			}
			delete(result, k)
			reduced = true
			im, _ := inner.(map[string]any)
			for _, id := range []string{"currentModeId", "currentModelId"} {
				if v, ok := im[id]; ok {
					result[id] = v
				}
			}
		}
	}
	return reduced
}

func reduceInit(m map[string]any) map[string]any {
	out := pick(m, "type", "subtype", "session_id", "model", "permissionMode", "claude_code_version", "capabilities")
	if tools, ok := m["tools"].([]any); ok {
		out["tools_count"] = len(tools)
	}
	return out
}

// initReplyPayload finds the initialize reply's body, which claude nests one
// level inside the control_response's response.
func initReplyPayload(m map[string]any) (map[string]any, bool) {
	resp, ok := m["response"].(map[string]any)
	if !ok {
		return nil, false
	}
	payload := resp
	if inner, ok := resp["response"].(map[string]any); ok {
		payload = inner
	}
	_, hasAccount := payload["account"]
	_, hasCommands := payload["commands"]
	return payload, hasAccount || hasCommands
}

func reduceInitReply(m map[string]any, payload map[string]any) map[string]any {
	resp := m["response"].(map[string]any)
	out := pick(m, "type")
	out["response"] = pick(resp, "subtype", "request_id")
	commands, _ := payload["commands"].([]any)
	out["commands_count"] = len(commands)
	return out
}

func pick(m map[string]any, keys ...string) map[string]any {
	out := make(map[string]any, len(keys))
	for _, k := range keys {
		if v, ok := m[k]; ok {
			out[k] = v
		}
	}
	return out
}

// marshal encodes compactly without HTML escaping and without the trailing newline.
func marshal(v any) (json.RawMessage, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, fmt.Errorf("scrub: encode frame: %w", err)
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte{'\n'}), nil
}
