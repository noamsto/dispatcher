package panestate

import (
	"reflect"
	"strings"
	"testing"
)

func TestPublish(t *testing.T) {
	multi := strings.Repeat("é", 30) + strings.Repeat("✓", 20)
	cases := []struct {
		name                  string
		state, detail, source string
		want                  [][2]string
	}{
		{"short", "blocked", "quiet", "watchdog", [][2]string{{"@crew_state", "blocked"}, {"@crew_detail", "quiet"}, {"@crew_source", "watchdog"}}},
		{"empty source", "working", "d", "", [][2]string{{"@crew_state", "working"}, {"@crew_detail", "d"}, {"@crew_source", ""}}},
		{"exactly 40", "failed", strings.Repeat("x", 40), "", [][2]string{{"@crew_state", "failed"}, {"@crew_detail", strings.Repeat("x", 40)}, {"@crew_source", ""}}},
		{"41 cut", "failed", strings.Repeat("x", 41), "", [][2]string{{"@crew_state", "failed"}, {"@crew_detail", strings.Repeat("x", 40)}, {"@crew_source", ""}}},
		{"multibyte cut on runes", "blocked", multi, "watchdog", [][2]string{{"@crew_state", "blocked"}, {"@crew_detail", strings.Repeat("é", 30) + strings.Repeat("✓", 10)}, {"@crew_source", "watchdog"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got [][2]string
			Publish(func(o, v string) { got = append(got, [2]string{o, v}) }, tc.state, tc.detail, tc.source)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}
