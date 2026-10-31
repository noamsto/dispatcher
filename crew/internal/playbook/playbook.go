// Package playbook maps a crew bus event to the dispatcher skill that holds
// its handling, so `crew watch`/`crew stream` output carries the pointer the
// dispatcher loads on demand (#1278).
package playbook

import "strings"

// The skill names, claude-canonical (`dispatcher:<x>`); the protocol index
// documents the bare-name form for codex/cursor/pi.
const (
	Watchdog       = "dispatcher:watchdog"
	BlockedReplies = "dispatcher:blocked-replies"
	PrWatch        = "dispatcher:pr-watch"
	FollowUps      = "dispatcher:follow-ups"
)

// watchdogPrefixes are the reserved `stall-watch` detail prefixes.
var watchdogPrefixes = []string{
	"prompt:", "quota:", "turn-stall:", "quiet:", "stalled:", "unread:",
	"load:", "runaway:", "truncated:", "provider-error:", "budget:", "dead:",
}

func hasPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// For returns the skill name for ev, or "" when its handling is not a moved
// playbook. It never mutates ev.
func For(ev map[string]any) string {
	kind, _ := ev["kind"].(string)
	switch kind {
	case "status":
		body, _ := ev["body"].(map[string]any)
		if body == nil {
			return ""
		}
		source, _ := body["source"].(string)
		detail, _ := body["detail"].(string)
		state, _ := body["state"].(string)
		if source == "watchdog" && hasPrefix(detail, watchdogPrefixes) {
			return Watchdog
		}
		if state == "pr_open" {
			return BlockedReplies
		}
		if state == "blocked" && source != "watchdog" &&
			(strings.HasPrefix(detail, "acceptance:") || strings.HasPrefix(detail, "permission:")) {
			return BlockedReplies
		}
	case "msg":
		from, _ := ev["from"].(string)
		if strings.HasPrefix(from, "pr-watch:") {
			return PrWatch
		}
		if body, _ := ev["body"].(string); strings.HasPrefix(from, "worker:") &&
			strings.HasPrefix(body, "follow-ups (untracked):") {
			return FollowUps
		}
	}
	return ""
}

// Annotate sets `playbook` on every event in a watch batch that has one. A
// malformed batch or event is skipped, never panics.
func Annotate(batch map[string]any) {
	events, _ := batch["events"].([]any)
	for _, e := range events {
		ev, ok := e.(map[string]any)
		if !ok {
			continue
		}
		if p := For(ev); p != "" {
			ev["playbook"] = p
		}
	}
}
