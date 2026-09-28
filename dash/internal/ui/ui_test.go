package ui

import "testing"

// TestColorForcedAscii matches finding 8: the interactive TUI must honor
// CREW_DASH_COLOR=never the same way it already honors NO_COLOR (the README
// promises both), while CREW_DASH_COLOR=always has no TUI effect (that
// override is --once's colorEnabled in main.go, not the interactive path).
func TestColorForcedAscii(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want bool
	}{
		{"no env vars", nil, false},
		{"NO_COLOR set", map[string]string{"NO_COLOR": "1"}, true},
		{"CREW_DASH_COLOR=never", map[string]string{"CREW_DASH_COLOR": "never"}, true},
		{"CREW_DASH_COLOR=always has no TUI effect", map[string]string{"CREW_DASH_COLOR": "always"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			getenv := func(k string) string { return tc.env[k] }
			if got := colorForcedAscii(getenv); got != tc.want {
				t.Errorf("colorForcedAscii(%v) = %v, want %v", tc.env, got, tc.want)
			}
		})
	}
}
