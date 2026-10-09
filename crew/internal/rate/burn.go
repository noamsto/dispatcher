// Burn pricing: the port of crew.sh's `_burn_weight`. The table is settings
// data (`burnClasses`, resolved through dispatch-config), each rule a shell
// glob matched in order, first match wins; opus is the one rule whose class
// follows effort through `byEffort`, with a `default` for an effort the map
// does not name. An unclassed model prices as nothing, which the run fold
// reads as a null cost class.
package rate

import (
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/noamsto/dispatcher/crew/internal/jsonv"
)

// burnRule is one burnClasses row. class/weight are the row's own values,
// which a byEffort row leaves empty because its price lives in the map.
type burnRule struct {
	match    string
	class    string
	weight   jsonv.Value
	byEffort []jsonv.Member
	hasBy    bool
}

// burnRules reads `.burnClasses` out of the resolved settings. A missing key
// or a non-array value is the arm's empty rule list: every model unclassed.
func burnRules(settings jsonv.Value) []burnRule {
	list, ok := settings.Get("burnClasses")
	if !ok || list.Kind() != jsonv.KindArray {
		return nil
	}
	var rules []burnRule
	for _, row := range list.Elems() {
		if row.Kind() != jsonv.KindObject {
			continue
		}
		var rule burnRule
		if m, ok := row.Get("match"); ok {
			rule.match, _ = m.AsString()
		}
		if c, ok := row.Get("class"); ok {
			rule.class = jqText(c)
		}
		rule.weight = jsonv.Null()
		if w, ok := row.Get("weight"); ok {
			rule.weight = argjson(jqText(w))
		}
		if by, ok := row.Get("byEffort"); ok && by.Kind() == jsonv.KindObject {
			rule.hasBy = true
			rule.byEffort = by.Members()
		}
		if rule.match == "" {
			continue
		}
		rules = append(rules, rule)
	}
	return rules
}

// burnWeight prices one model+effort, reporting false for an unclassed model.
// A byEffort map naming neither the effort nor a `default` prices as jq's
// string interpolation does — the literal class "null", a null weight —
// because that is what the arm wrote into the store.
func burnWeight(rules []burnRule, model, effort string) (string, jsonv.Value, bool) {
	for _, r := range rules {
		if !globMatch(r.match, model) {
			continue
		}
		if !r.hasBy {
			return r.class, r.weight, true
		}
		entry := byEffortEntry(r.byEffort, effort)
		class := "null"
		weight := jsonv.Null()
		if entry.Kind() == jsonv.KindObject {
			if c, ok := entry.Get("class"); ok {
				class = jqText(c)
			}
			if w, ok := entry.Get("weight"); ok {
				weight = argjson(jqText(w))
			}
		}
		return class, weight, true
	}
	return "", jsonv.Null(), false
}

// byEffortEntry is jq's `.[$e] // .default`: an absent, null or false effort
// entry falls through to `default`, which may itself be absent.
func byEffortEntry(members []jsonv.Member, effort string) jsonv.Value {
	for _, m := range members {
		if m.Key == effort && m.Val.Truthy() {
			return m.Val
		}
	}
	for _, m := range members {
		if m.Key == "default" {
			return m.Val
		}
	}
	return jsonv.Null()
}

// jqText is jq's scalar rendering (`@tsv`'s own rule for the two columns
// _burn_weight printed): strings unchanged, everything else its JSON text. The
// arm reached only string, number and null — a class is a string and a weight a
// number, and `@tsv` would have errored on anything else — so the boolean and
// container arms exist for the switch, not for the data.
func jqText(v jsonv.Value) string {
	switch v.Kind() {
	case jsonv.KindString:
		s, _ := v.AsString()
		return s
	case jsonv.KindNull:
		return "null"
	case jsonv.KindTrue:
		return "true"
	case jsonv.KindFalse:
		return "false"
	case jsonv.KindNumber:
		if text := v.NumberText(); text != "" {
			return text
		}
		f, _ := v.AsFloat()
		return strconv.FormatFloat(f, 'g', -1, 64)
	case jsonv.KindArray, jsonv.KindObject:
		return string(jsonv.Append(nil, v, jsonv.Options{}))
	}
	return ""
}

// argjson is bash's `--argjson w "$text"`: the text is parsed as JSON, so the
// class/weight round-trip through the arm's TSV comes back a number. Text the
// arm would have died on (`--argjson` rejects it under `set -e`) prices as
// null rather than killing the sweep over a malformed table row.
func argjson(text string) jsonv.Value {
	vals, err := jsonv.DecodeStream(strings.NewReader(text))
	if err != nil || len(vals) != 1 {
		return jsonv.Null()
	}
	return vals[0]
}

// globMatch is bash's `case` pattern match, not path.Match: `*` and `?` cross
// `/` (a model id like `openrouter/anthropic/claude-opus-4.1` is priced by
// `*opus*`), `[...]` is a set with ranges and `[!…]` negation, and `\` escapes
// the next character. `|` is literal — the helper matched an *expanded*
// pattern (`case "$model" in $pat)`), where bash performs no alternation.
func globMatch(pattern, s string) bool {
	return matchFrom(pattern, s, 0, 0)
}

func matchFrom(pat, s string, pi, si int) bool {
	for pi < len(pat) {
		switch pat[pi] {
		case '*':
			for pi < len(pat)-1 && pat[pi+1] == '*' {
				pi++
			}
			if pi == len(pat)-1 {
				return true
			}
			for rest := si; rest <= len(s); rest++ {
				if matchFrom(pat, s, pi+1, rest) {
					return true
				}
			}
			return false
		case '?':
			if si >= len(s) {
				return false
			}
			_, n := utf8.DecodeRuneInString(s[si:])
			pi++
			si += n
		case '[':
			if end := setRange(pat, pi); end >= 0 {
				if si >= len(s) {
					return false
				}
				c, n := utf8.DecodeRuneInString(s[si:])
				if !matchSet(pat, c, pi) {
					return false
				}
				pi = end
				si += n
				continue
			}
			// Unterminated: bash's pattern grammar leaves it a literal `[`, so
			// `[x` matches the three characters `[x` and nothing else.
			if si >= len(s) || s[si] != '[' {
				return false
			}
			pi++
			si++
		case '\\':
			// A trailing `\` has nothing to escape and matches itself.
			lit := pi + 1
			if lit >= len(pat) {
				lit = pi
			}
			if si >= len(s) || s[si] != pat[lit] {
				return false
			}
			pi = lit + 1
			si++
		default:
			if si >= len(s) || s[si] != pat[pi] {
				return false
			}
			pi++
			si++
		}
	}
	return si == len(s)
}

// setRange returns the index just past pat's `[` set, or -1 when it is
// unterminated — which the caller matches as a literal `[`, the way bash's
// pattern grammar does.
func setRange(pat string, pi int) int {
	i := pi + 1
	if i < len(pat) && (pat[i] == '!' || pat[i] == '^') {
		i++
	}
	if i < len(pat) && pat[i] == ']' {
		i++
	}
	for i < len(pat) {
		switch pat[i] {
		case '\\':
			i += 2
			continue
		case '[':
			if _, next := posixClass(pat[i:]); next > 0 {
				i += next
				continue
			}
		case ']':
			return i + 1
		}
		i++
	}
	return -1
}

// posixClass parses a leading `[:name:]` in s, returning the name and its
// length, or 0 when s does not start with one.
func posixClass(s string) (string, int) {
	if !strings.HasPrefix(s, "[:") {
		return "", 0
	}
	end := strings.Index(s[2:], ":]")
	if end < 0 {
		return "", 0
	}
	return s[2 : 2+end], end + 4
}

func inClass(name string, c rune) bool {
	switch name {
	case "alpha":
		return unicode.IsLetter(c)
	case "digit":
		return c >= '0' && c <= '9'
	case "alnum":
		return unicode.IsLetter(c) || unicode.IsDigit(c)
	case "upper":
		return unicode.IsUpper(c)
	case "lower":
		return unicode.IsLower(c)
	case "space":
		return unicode.IsSpace(c)
	case "blank":
		return c == ' ' || c == '\t'
	case "punct":
		return unicode.IsPunct(c) || unicode.IsSymbol(c)
	case "print":
		return unicode.IsPrint(c)
	case "graph":
		return unicode.IsGraphic(c) && !unicode.IsSpace(c)
	case "cntrl":
		return unicode.IsControl(c)
	case "xdigit":
		return unicode.Is(unicode.ASCII_Hex_Digit, c)
	}
	return false
}

func matchSet(pat string, c rune, pi int) bool {
	end := setRange(pat, pi)
	if end < 0 {
		return false
	}
	body := pat[pi+1 : end-1]
	neg := false
	if body != "" && (body[0] == '!' || body[0] == '^') {
		neg = true
		body = body[1:]
	}
	return inSet([]rune(body), c) != neg
}

func inSet(body []rune, c rune) bool {
	for i := 0; i < len(body); i++ {
		if body[i] == '[' {
			if name, n := posixClass(string(body[i:])); n > 0 {
				if inClass(name, c) {
					return true
				}
				i += utf8.RuneCountInString(string(body[i:])[:n]) - 1
				continue
			}
		}
		ch := body[i]
		if ch == '\\' && i+1 < len(body) {
			i++
			ch = body[i]
		}
		if i+2 < len(body) && body[i+1] == '-' {
			if c >= ch && c <= body[i+2] {
				return true
			}
			i += 2
			continue
		}
		if c == ch {
			return true
		}
	}
	return false
}
