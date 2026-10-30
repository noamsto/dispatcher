package once

import "testing"

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
