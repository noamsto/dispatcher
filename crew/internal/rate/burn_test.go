package rate

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/drift"
)

// defaultRules is the shipped burn table, the same file dispatch-config
// resolves: the oracle below is only as good as the data it reads, so a table
// that moved without a price moving here is what this test is for.
func defaultRules(t *testing.T) []burnRule {
	t.Helper()
	path := filepath.Join("..", "..", "..", "adapters", "core", "defaults.json")
	data, err := os.ReadFile(path)
	if err != nil {
		drift.Missing(t, path, err)
	}
	return burnRules(string(data))
}

// TestBurnTableOracle crosses every classed model with every effort the
// dispatcher can pass and asserts the class and weight the bash helper's
// `case` produced. It replaces the bats row that sed-extracted
// `_burn_weight`; the table moved to Go with the sweep, and so did the oracle.
func TestBurnTableOracle(t *testing.T) {
	rules := defaultRules(t)

	// Opus is the one rung whose class follows effort; "" is an absent effort.
	for _, c := range []struct {
		model, effort, class string
		weight               float64
	}{
		{"opus", "low", "standard", 2},
		{"opus", "medium", "premium", 4},
		{"opus", "high", "premium", 4},
		{"opus", "xhigh", "premium", 6},
		{"opus", "max", "premium", 8},
		{"opus", "", "premium", 4},
		{"opus", "bogus", "premium", 4},
		{"claude-opus-5", "low", "standard", 2},
	} {
		class, weight, ok := burnWeight(rules, c.model, c.effort)
		if !ok || class != c.class || !weightTruth(weight, c.weight) {
			t.Errorf("burnWeight(%q, %q) = (%q, %v, %v), want (%q, %v, true)",
				c.model, c.effort, class, weight, ok, c.class, c.weight)
		}
	}

	// Every other classed model is effort-independent: cross it with each
	// effort and require the same class, so a stray effort key cannot leak in.
	efforts := []string{"low", "medium", "high", "xhigh", "max", ""}
	for _, c := range []struct {
		model, class string
		weight       float64
	}{
		{"sonnet", "standard", 2},
		{"claude-sonnet-4-5", "standard", 2},
		{"haiku", "cheap", 1},
		{"claude-haiku-4-5", "cheap", 1},
		{"claude-fable-5", "fable", 8},
		{"fable", "fable", 8},
		{"gpt-5.6-sol", "premium", 4},
		{"gpt-5.6-terra", "standard", 2},
		{"gpt-5.6-luna", "cheap", 1},
		{"composer-2.5", "free", 0},
		{"composer-2.5-fast", "free", 0},
		{"cursor-grok-4.6-low", "cheap", 1},
		{"cursor-grok-4.6-medium", "standard", 2},
		{"cursor-grok-4.6-high", "premium", 4},
		{"cursor-grok-4.6-xhigh", "premium", 6},
		{"cursor-grok-4.5-low", "cheap", 1},
		{"cursor-grok-4.5-medium", "standard", 2},
		{"cursor-grok-4.5-high", "premium", 4},
		{"cursor-grok-4.6-low-fast", "standard", 2},
		{"cursor-grok-4.6-medium-fast", "premium", 4},
		{"cursor-grok-4.6-high-fast", "premium", 8},
		{"cursor-grok-4.6-xhigh-fast", "premium", 12},
		{"grok-4.7-low", "cheap", 1},
		{"grok-4.7-medium", "standard", 2},
		{"grok-4.7-high", "premium", 4},
		{"grok-4.7-xhigh", "premium", 6},
		{"grok-4.7-low-fast", "standard", 2},
		{"grok-4.7-medium-fast", "premium", 4},
		{"grok-4.7-high-fast", "premium", 8},
		{"grok-4.7-xhigh-fast", "premium", 12},
	} {
		for _, effort := range efforts {
			class, weight, ok := burnWeight(rules, c.model, effort)
			if !ok || class != c.class || !weightTruth(weight, c.weight) {
				t.Errorf("burnWeight(%q, %q) = (%q, %v, %v), want (%q, %v, true)",
					c.model, effort, class, weight, ok, c.class, c.weight)
			}
		}
	}

	// kimi-k3-high and an unknown id stay unclassed rather than guessed.
	for _, model := range []string{"kimi-k3-high", "some-unknown-model"} {
		if class, _, ok := burnWeight(rules, model, "high"); ok {
			t.Errorf("burnWeight(%q) = %q, want unclassed", model, class)
		}
	}
}

// TestBurnSlashedModelID is the case path.Match would get wrong: a model id
// carrying `/` still prices through a `*`-only glob, because bash `case` globs
// do not treat `/` as a separator.
func TestBurnSlashedModelID(t *testing.T) {
	rules := defaultRules(t)
	for _, c := range []struct {
		model, effort, class string
		weight               float64
	}{
		{"openrouter/anthropic/claude-opus-4.1", "high", "premium", 4},
		{"openrouter/anthropic/claude-opus-4.1", "low", "standard", 2},
		{"openrouter/deepseek/deepseek-v4-pro", "high", "", 0},
	} {
		class, weight, ok := burnWeight(rules, c.model, c.effort)
		if c.class == "" {
			if ok {
				t.Errorf("burnWeight(%q) = %q, want unclassed", c.model, class)
			}
			continue
		}
		if !ok || class != c.class || !weightTruth(weight, c.weight) {
			t.Errorf("burnWeight(%q) = (%q, %v, %v), want (%q, %v, true)",
				c.model, class, weight, ok, c.class, c.weight)
		}
	}
}

func TestGlobMatch(t *testing.T) {
	for _, c := range []struct {
		pattern, s string
		want       bool
	}{
		{"*opus*", "openrouter/anthropic/claude-opus-4.1", true},
		{"*grok-4.[0-9]-medium", "cursor-grok-4.5-medium", true},
		{"*grok-4.[0-9]-medium", "cursor-grok-4.10-medium", false},
		{"composer-2.5*", "composer-2.5-fast", true},
		{"composer-2.5*", "composer-2.4", false},
		{"gpt-5.6-sol", "gpt-5.6-sol", true},
		{"gpt-5.6-sol", "gpt-5.6-sol-x", false},
		{"a?c", "abc", true},
		{"a?c", "ac", false},
		{"a?c", "a/c", true},
		{"[!a]*", "bbb", true},
		{"[!a]*", "abc", false},
		{"a|b", "a|b", true},
		{"a|b", "a", false},
		{`a\*b`, "a*b", true},
		{`a\*b`, "axb", false},
		{"*", "", true},
		{"", "", true},
		{"[a-z]*", "hello", true},
		{"[a-z]*", "Hello", false},
		// An unterminated set is a literal `[` in bash's pattern grammar, which
		// a user-refined burnClasses table can hit.
		{"[x", "[x", true},
		{"[x", "x", false},
		{"[x", "[abc", false},
		{"[x", "[", false},
		{"[abc", "a", false},
		{"[abc", "[abc", true},
		{"a[b", "a[b", true},
		{"a[b", "ab", false},
		// Expectations below were checked with `case "$s" in $p)` in bash.
		{"[[:alpha:]]x", "ax", true},
		{"[[:alpha:]]x", "1x", false},
		{"[[:alpha:]]x", "-x", false},
		{"[![:digit:]]", "5", false},
		{"[![:digit:]]", "a", true},
		{"[[:alpha:]]", "]", false},
		{"[[:alpha:][:digit:]]", "7", true},
		{"[[:alpha:]-]", "-", true},
		{"[[:alpha:]]", ":", false},
		{`a\`, `a\`, true},
		{`a\`, "a", false},
		{`a\`, `a\b`, false},
		{"?x", "éx", true},
		{"?x", "x", false},
		{"?", "é", true},
		{"?", "éé", false},
		{"[é]x", "éx", true},
		{"[a-z]x", "éx", false},
		{"[!a]x", "éx", true},
		{"*[!é]", "é", false},
		{"*[![:alpha:]]", "é", false},
	} {
		if got := globMatch(c.pattern, c.s); got != c.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v", c.pattern, c.s, got, c.want)
		}
	}
}

// TestBurnRulesEdgeCases covers the table shapes the shipped defaults.json
// does not carry: a byEffort map with no `default`, a rule with no class, and
// the first-match-wins order.
func TestBurnRulesEdgeCases(t *testing.T) {
	settings := decodeOne(t, `{"burnClasses":[
      {"match":"*x*","class":"first","weight":1},
      {"match":"x*","class":"second","weight":2},
      {"match":"*by*","byEffort":{"low":{"class":"l","weight":1}}},
      {"match":"*def*","byEffort":{"default":{"class":"d","weight":3}}},
      {"match":"","class":"skipped","weight":9},
      {"match":"*bare*"},
      {"match":"[weird","class":"weird","weight":5}
    ]}`)
	rules := burnRules(settings)
	if len(rules) != 6 {
		t.Fatalf("rules = %d, want 6 (the empty-match row is skipped)", len(rules))
	}
	for _, c := range []struct {
		model, effort, class string
		weight               any
	}{
		{"x", "high", "first", json.Number("1")},
		{"xy", "high", "first", json.Number("1")},
		{"by", "low", "l", json.Number("1")},
		{"by", "high", "null", nil},
		{"def", "anything", "d", json.Number("3")},
		{"bare", "high", "", nil},
		// The reviewer's repro: bash priced the unterminated set as a literal,
		// so `[weird` matches the model id `[weird` and nothing else — a table
		// with `[weird` must not price the plain id.
		{"[weird", "high", "weird", json.Number("5")},
	} {
		class, weight, ok := burnWeight(rules, c.model, c.effort)
		if !ok || class != c.class || (weight == nil) != (c.weight == nil) {
			t.Errorf("burnWeight(%q, %q) = (%q, %v, %v), want (%q, %v, true)",
				c.model, c.effort, class, weight, ok, c.class, c.weight)
			continue
		}
		if c.weight != nil && !weightTruth(weight, num(c.weight)) {
			t.Errorf("burnWeight(%q, %q) weight = %v, want %v", c.model, c.effort, weight, c.weight)
		}
	}
	if class, _, ok := burnWeight(rules, "weird", "high"); ok {
		t.Errorf("burnWeight(\"weird\") = %q, want unclassed", class)
	}
}

// TestBurnRulesNoTable is the arm's empty-settings path: every model unclassed.
func TestBurnRulesNoTable(t *testing.T) {
	for _, settings := range []string{`{}`, `{"burnClasses":[]}`, `{"burnClasses":null}`} {
		if rules := burnRules(decodeOne(t, settings)); len(rules) != 0 {
			t.Errorf("burnRules(%s) = %d rules, want 0", settings, len(rules))
		}
	}
}

func decodeOne(_ *testing.T, src string) string { return src }

func weightTruth(v any, want float64) bool {
	f, ok := asFloat(v)
	return ok && f == want
}

func num(v any) float64 {
	f, _ := asFloat(v)
	return f
}

// TestArgjsonLooseNumbers: the arm's `--argjson` accepted jq's looser number
// grammar, so a quoted weight like ".5" must still price.
func TestArgjsonLooseNumbers(t *testing.T) {
	nums := []struct {
		in   string
		want float64
	}{
		{".5", 0.5}, {"+2", 2}, {"5.", 5}, {"02", 2}, {"-.5e1", -5}, {"+1e400", math.Inf(1)},
	}
	for _, c := range nums {
		if got := argjson(c.in); !weightTruth(got, c.want) {
			t.Errorf("argjson(%q) = %#v, want %v", c.in, got, c.want)
		}
	}
	if got := argjson("1.50"); got != json.Number("1.50") {
		t.Errorf("argjson(%q) = %#v, want json.Number(\"1.50\")", "1.50", got)
	}
	for _, in := range []string{"nan", "Infinity", "inf", "0x10", "1_0", "abc", "", "1 2", ".", "+", "e5", " .5"} {
		if got := argjson(in); got != nil {
			t.Errorf("argjson(%q) = %#v, want nil", in, got)
		}
	}
}
