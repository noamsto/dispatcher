package rolewatch

import "testing"

func TestSenderAllowed(t *testing.T) {
	tests := []struct {
		name, from, branch, crew string
		want                     bool
	}{
		{"lead worker", "worker:feat/9-x#s1-1", "feat/9-x", "c1", true},
		{"crew dispatcher", "dispatcher:c1", "feat/9-x", "c1", true},
		{"branch holding a hash", "worker:feat/a#b#s1-1", "feat/a#b", "c1", true},
		{"other branch worker", "worker:feat/8-other#s1-1", "feat/9-x", "c1", false},
		{"other branch role", "role:feat/8-other:reviewer", "feat/9-x", "c1", false},
		{"other crew dispatcher", "dispatcher:c2", "feat/9-x", "c1", false},
		{"sibling role", "role:feat/9-x:plan-critic", "feat/9-x", "c1", false},
		{"hash after the branch", "worker:feat/9-x#y#s1-1", "feat/9-x", "c1", false},
		{"hash in the session suffix", "worker:feat/9-x#s1#1", "feat/9-x", "c1", false},
		{"no crew refuses dispatcher", "dispatcher:c1", "feat/9-x", "", false},
		{"no crew refuses bare dispatcher", "dispatcher:", "feat/9-x", "", false},
		{"no crew still allows the worker", "worker:feat/9-x#s1-1", "feat/9-x", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := SenderAllowed(tc.from, tc.branch, tc.crew); got != tc.want {
				t.Errorf("SenderAllowed(%q, %q, %q) = %v, want %v", tc.from, tc.branch, tc.crew, got, tc.want)
			}
		})
	}
}

func TestAssignmentSafe(t *testing.T) {
	tests := []struct {
		name, body string
		want       bool
	}{
		{"ascii", `{"role":"reviewer","q":"go"}`, true},
		{"empty", "", true},
		{"utf8 accent", "café", true},
		{"utf8 prompt glyph", "❯ go", true},
		{"nbsp", "a b", true},
		{"tab", "a\tb", false},
		{"newline", "a\nb", false},
		{"carriage return", "a\rb", false},
		{"escape", "a\x1bb", false},
		{"delete", "a\x7fb", false},
		{"nul", "a\x00b", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := AssignmentSafe(tc.body); got != tc.want {
				t.Errorf("AssignmentSafe(%q) = %v, want %v", tc.body, got, tc.want)
			}
		})
	}
}
