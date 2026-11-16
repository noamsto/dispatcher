package ui

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	vm "github.com/noamsto/dispatcher/dash/internal/view"
	"github.com/noamsto/dispatcher/dash/internal/view/viewtest"
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

// poisonPre and poisonPost wrap every string poison touches: an SGR, an OSC 8
// hyperlink, a C1 CSI and a bidi override.
const (
	poisonPre  = "\x1b[31m"
	poisonPost = "\x1b]8;;x\a\u009b\u202e"
)

// poison wraps every string in v, raw fields included, writing through
// pointers, slices and maps (whose keys it replaces).
func poison(v reflect.Value) {
	switch v.Kind() {
	case reflect.String:
		v.SetString(poisonPre + v.String() + poisonPost)
	case reflect.Pointer:
		if !v.IsNil() {
			poison(v.Elem())
		}
	case reflect.Struct:
		for i := range v.NumField() {
			if v.Type().Field(i).IsExported() {
				poison(v.Field(i))
			}
		}
	case reflect.Slice:
		for i := range v.Len() {
			poison(v.Index(i))
		}
	case reflect.Map:
		c := reflect.MakeMapWithSize(v.Type(), v.Len())
		for it := v.MapRange(); it.Next(); {
			k, e := reflect.New(v.Type().Key()).Elem(), reflect.New(v.Type().Elem()).Elem()
			k.Set(it.Key())
			e.Set(it.Value())
			poison(k)
			poison(e)
			c.SetMapIndex(k, e)
		}
		v.Set(c)
	}
}

// poisonedOverview is the fixture's Overview with every string poisoned.
func poisonedOverview() vm.OverviewView {
	ov := vm.BuildOverview(viewtest.Inputs())
	poison(reflect.ValueOf(&ov).Elem())
	return ov
}

// walkStrings calls visit with every string in v and whether it sits under a
// dash:"raw" field.
func walkStrings(v reflect.Value, raw bool, visit func(s string, raw bool)) {
	switch v.Kind() {
	case reflect.String:
		visit(v.String(), raw)
	case reflect.Pointer:
		if !v.IsNil() {
			walkStrings(v.Elem(), raw, visit)
		}
	case reflect.Struct:
		for i := range v.NumField() {
			if f := v.Type().Field(i); f.IsExported() {
				walkStrings(v.Field(i), raw || f.Tag.Get("dash") == "raw", visit)
			}
		}
	case reflect.Slice:
		for i := range v.Len() {
			walkStrings(v.Index(i), raw, visit)
		}
	case reflect.Map:
		for it := v.MapRange(); it.Next(); {
			walkStrings(it.Key(), raw, visit)
			walkStrings(it.Value(), raw, visit)
		}
	}
}

// controlRune reports C0 and C1 controls, DEL and the bidi controls.
func controlRune(r rune) bool {
	switch {
	case r < 0x20, r == 0x7f, r >= 0x80 && r <= 0x9f,
		r == 0x061c, r == 0x200e, r == 0x200f,
		r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069:
		return true
	}
	return false
}

func TestCleanViewCleansEveryFieldButArgv(t *testing.T) {
	raw := poisonedOverview()
	clean := cleanView(raw)
	var plain, kept int
	walkStrings(reflect.ValueOf(clean), false, func(s string, isRaw bool) {
		if isRaw {
			kept++
			if !strings.HasPrefix(s, poisonPre) {
				t.Errorf("raw field %q lost its bytes", s)
			}
			return
		}
		plain++
		if i := strings.IndexFunc(s, controlRune); i >= 0 {
			t.Errorf("field %q keeps control %U", s, []rune(s[i:])[0])
		}
		if strings.Contains(s, "]8;;") || strings.Contains(s, "[31m") {
			t.Errorf("field %q keeps an escape sequence's text", s)
		}
	})
	if plain == 0 || kept == 0 {
		t.Fatalf("walked %d cleaned and %d raw strings", plain, kept)
	}
	// cleanView copies: the poisoned value it read is untouched.
	walkStrings(reflect.ValueOf(raw), false, func(s string, _ bool) {
		if !strings.HasPrefix(s, poisonPre) {
			t.Errorf("cleanView wrote through to its input: %q", s)
		}
	})
}
