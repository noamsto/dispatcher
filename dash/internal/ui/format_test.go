package ui

import (
	"encoding/json"
	"os"
	"testing"
)

func TestCleanText(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{"tab", "a\tb", "a b"},
		{"esc", "a\x1bb", "ab"},
		{"bidi override", "a\u202eb", "ab"},
		{"ALM", "a\u061cb", "ab"},
		{"mongolian vowel separator", "a\u180eb", "ab"},
		{"ZWSP", "a\u200bb", "ab"},
		{"RLM", "a\u200fb", "ab"},
		{"word joiner", "a\u2060b", "ab"},
		{"invisible plus", "a\u2064b", "ab"},
		{"BOM", "a\ufeffb", "ab"},
		{"tag block start", "a\U000e0000b", "ab"},
		{"tag A", "a\U000e0041b", "ab"},
		{"tag block end", "a\U000e007fb", "ab"},
		{"latin", "café", "café"},
		{"cjk", "日本語", "日本語"},
		{"emoji", "🚀 ok", "🚀 ok"},
		{"hair space kept", "a\u200ab", "a\u200ab"},
		{"hyphen kept", "a\u2010b", "a\u2010b"},
		{"after tag block kept", "a\U000e0080b", "a\U000e0080b"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := cleanText(tt.in); got != tt.want {
				t.Errorf("cleanText(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestCleanTextInvisibleFixture(t *testing.T) {
	data, err := os.ReadFile("../invisible/testdata/cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name    string `json:"name"`
		In      string `json:"in"`
		Dropped string `json:"dropped"`
	}
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			if got := cleanText(tc.In); got != tc.Dropped {
				t.Errorf("cleanText(%q) = %q, want %q", tc.In, got, tc.Dropped)
			}
		})
	}
}
