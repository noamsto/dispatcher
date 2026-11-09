package rate

import "testing"

// TestClassify pins the admission rules ported from adapters/core/model-resolve.sh
// against the real shipped defaults (crew/internal/rate/admit.json).
//
// Data note: the seed rows are not symmetric. claude deep lists
// opus/sonnet/fable but no haiku; claude standard/trivial list
// opus/sonnet/haiku but no fable. So fable is retired at claude standard and
// haiku is retired at claude deep — the class patterns below do not make up a
// seed row membership (classes.claude.premium matches only opus ids).
func TestClassify(t *testing.T) {
	cases := []struct {
		name     string
		engine   string
		tier     string
		id       string
		want     admitState
		requires string
	}{
		// The claude aliases, at the tiers whose row lists them.
		{"opus deep", "claude", "deep", "opus", admitted, ""},
		{"opus standard", "claude", "standard", "opus", admitted, ""},
		{"opus trivial", "claude", "trivial", "opus", admitted, ""},
		{"sonnet deep", "claude", "deep", "sonnet", admitted, ""},
		{"sonnet standard", "claude", "standard", "sonnet", admitted, ""},
		{"sonnet trivial", "claude", "trivial", "sonnet", admitted, ""},
		{"haiku deep", "claude", "deep", "haiku", retired, ""},
		{"haiku standard", "claude", "standard", "haiku", admitted, ""},
		{"haiku trivial", "claude", "trivial", "haiku", admitted, ""},
		{"fable deep", "claude", "deep", "fable", admitted, ""},
		{"fable standard", "claude", "standard", "fable", retired, ""},
		{"fable trivial", "claude", "trivial", "fable", retired, ""},

		// Trailing-* glob in the seed row.
		{"glob deep", "claude", "deep", "claude-opus-5-5", admitted, ""},
		{"glob standard", "claude", "standard", "claude-haiku-4-5", admitted, ""},

		// Class patterns, pi: conditional candidates and the class/tier admits list.
		{"anthropic opus pi standard", "pi", "standard", "anthropic/claude-opus-5-5", retired, ""},
		{"anthropic opus pi deep", "pi", "deep", "anthropic/claude-opus-5-5", gated, "anthropic_credits"},
		{"anthropic sonnet pi standard", "pi", "standard", "anthropic/claude-sonnet-5-5", gated, "anthropic_credits"},
		{"openrouter opus pi deep", "pi", "deep", "openrouter/anthropic/claude-opus-5.5", gated, "pi_month_target"},
		{"openrouter opus pi trivial", "pi", "trivial", "openrouter/anthropic/claude-opus-5.5", retired, ""},
		{"pi seed row", "pi", "standard", "openrouter/qwen/qwen3.8-flash", admitted, ""},
		// Reaches step 3 through a bare (ungated) class pattern: pi trivial's
		// seed row does not list it, and classes.pi.standard's
		// openrouter/qwen/qwen*-flash is a bare string, so the class matches
		// and the tier admits standard — admitted by the pattern alone.
		{"pi bare class pattern at trivial", "pi", "trivial", "openrouter/qwen/qwen3.8-flash", admitted, ""},

		// Matches no seed row and no class pattern: not in the shipped map.
		{"minimax claude", "claude", "deep", "opencode/minimax-m3", retired, ""},
		{"minimax codex", "codex", "standard", "opencode/minimax-m3", retired, ""},
		{"minimax pi", "pi", "standard", "opencode/minimax-m3", retired, ""},

		// The row's regex list admits effort-suffixed cursor ids.
		{"cursor regex", "cursor", "deep", "claude-opus-5-5-high", admitted, ""},
		{"cursor seed", "cursor", "deep", "kimi-k3-high", admitted, ""},
		{"cursor class cheap at deep", "cursor", "deep", "grok-4.7-low", retired, ""},
		{"cursor class cheap at trivial", "cursor", "trivial", "grok-4.7-low", admitted, ""},

		// Unknown engine or tier cannot be classified: admitted, unmarked.
		{"unknown engine", "mystery", "deep", "whatever", admitted, ""},
		{"unknown tier", "claude", "mystery", "opus", admitted, ""},
		{"unknown both", "mystery", "mystery", "whatever", admitted, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, req := classify(tc.engine, tc.tier, tc.id)
			if got != tc.want {
				t.Fatalf("classify(%q, %q, %q) state = %q, want %q", tc.engine, tc.tier, tc.id, got, tc.want)
			}
			if req != tc.requires {
				t.Fatalf("classify(%q, %q, %q) requires = %q, want %q", tc.engine, tc.tier, tc.id, req, tc.requires)
			}
		})
	}
}

// TestGlobMatch pins model-resolve's glob_re: anchored, * is .*, ? is one
// character, everything else is literal.
func TestAdmitGlob(t *testing.T) {
	cases := []struct {
		pattern string
		id      string
		want    bool
	}{
		{"claude-opus-*", "claude-opus-5-5", true},
		{"claude-opus-*", "claude-opus-", true},
		{"claude-opus-*", "claude-sonnet-5", false},
		{"*", "anything-at-all", true},
		{"gpt-?", "gpt-5", true},
		{"gpt-?", "gpt-55", false},
		{"gpt-5.5", "gpt-5.5", true},
		{"gpt-5.5", "gpt-5x5", false},
		{"a+b", "a+b", true},
		{"a+b", "aab", false},
		{"^claude$", "^claude$", true},
		{"^claude$", "claude", false},
	}
	for _, tc := range cases {
		if got := admitGlobMatch(tc.pattern, tc.id); got != tc.want {
			t.Errorf("admitGlobMatch(%q, %q) = %v, want %v", tc.pattern, tc.id, got, tc.want)
		}
	}
}
