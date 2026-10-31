package playbook

import "testing"

func TestFor(t *testing.T) {
	cases := []struct {
		name string
		ev   map[string]any
		want string
	}{
		{"watchdog prompt", map[string]any{"kind": "status", "body": map[string]any{"source": "watchdog", "state": "blocked", "detail": "prompt: trust"}}, Watchdog},
		{"watchdog permission dialog", map[string]any{"kind": "status", "body": map[string]any{"source": "watchdog", "state": "blocked", "detail": "prompt: permission — Bash: rm"}}, Watchdog},
		{"watchdog quota", map[string]any{"kind": "status", "body": map[string]any{"source": "watchdog", "state": "blocked", "detail": "quota: wait"}}, Watchdog},
		{"watchdog dead", map[string]any{"kind": "status", "body": map[string]any{"source": "watchdog", "state": "failed", "detail": "dead: gone"}}, Watchdog},
		{"worker acceptance", map[string]any{"kind": "status", "body": map[string]any{"state": "blocked", "detail": "acceptance: AC3 — no browser"}}, BlockedReplies},
		{"worker permission", map[string]any{"kind": "status", "body": map[string]any{"state": "blocked", "detail": "permission: needs a dir"}}, BlockedReplies},
		{"pr-watch", map[string]any{"kind": "msg", "from": "pr-watch:12", "body": "{}"}, PrWatch},
		{"follow-ups", map[string]any{"kind": "msg", "from": "worker:feat/x#s1-1", "body": "follow-ups (untracked): x"}, FollowUps},
		{"watchdog prefix on bad source", map[string]any{"kind": "status", "body": map[string]any{"source": "worker", "state": "blocked", "detail": "quota: wait"}}, ""},
		{"acceptance while watchdog-sourced", map[string]any{"kind": "status", "body": map[string]any{"source": "watchdog", "state": "blocked", "detail": "acceptance: x"}}, ""},
		{"acceptance in a non-blocked state", map[string]any{"kind": "status", "body": map[string]any{"state": "done", "detail": "acceptance: x"}}, ""},
		{"follow-ups from a non-worker", map[string]any{"kind": "msg", "from": "dispatcher:c1", "body": "follow-ups (untracked): x"}, ""},
		{"non-pr-watch msg", map[string]any{"kind": "msg", "from": "worker:feat/x#s1-1", "body": "{}"}, ""},
		{"non-string msg body", map[string]any{"kind": "msg", "from": "pr-watch:1", "body": map[string]any{}}, PrWatch},
		{"status with no body", map[string]any{"kind": "status"}, ""},
		{"empty detail", map[string]any{"kind": "status", "body": map[string]any{"source": "watchdog", "state": "blocked"}}, ""},
		{"unrelated kind", map[string]any{"kind": "log"}, ""},
	}
	for _, c := range cases {
		if got := For(c.ev); got != c.want {
			t.Errorf("%s: For = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestAnnotate(t *testing.T) {
	batch := map[string]any{"events": []any{
		map[string]any{"kind": "status", "body": map[string]any{"source": "watchdog", "state": "blocked", "detail": "load: busy"}},
		map[string]any{"kind": "status", "body": map[string]any{"state": "blocked", "detail": "need input"}},
		map[string]any{"kind": "msg", "from": "pr-watch:3", "body": "{}"},
		"not a map",
	}}
	Annotate(batch)
	events := batch["events"].([]any)
	if got := events[0].(map[string]any)["playbook"]; got != Watchdog {
		t.Errorf("watchdog playbook = %v", got)
	}
	if _, ok := events[1].(map[string]any)["playbook"]; ok {
		t.Error("an unmatched event must carry no playbook field")
	}
	if got := events[2].(map[string]any)["playbook"]; got != PrWatch {
		t.Errorf("pr-watch playbook = %v", got)
	}
	// The non-map entry and a batch with no events must not panic.
	Annotate(map[string]any{})
	Annotate(map[string]any{"events": "nope"})
}
