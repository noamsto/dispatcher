package rate

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"regexp"
	"strings"
	"sync"
)

// admit.json is `.modelMap` and `.classes` copied verbatim from
// adapters/core/defaults.json (a drift test keeps them equal).
//
//go:embed admit.json
var admitJSON []byte

// admitState is what the shipped model map says about one model id.
type admitState string

const (
	// admitted: the row lists the id, or a class pattern admits it unconditionally.
	admitted admitState = "admitted"
	// gated: every class pattern matching the id carries a `requires` gate.
	gated admitState = "gated"
	// retired: the id is not in the shipped map for this engine and tier.
	retired admitState = "retired"
)

// globRegexp ports model-resolve.sh's `glob_re`: the whole string is anchored,
// `*` is `.*`, `?` is one character, and every other character is literal.
// A pattern that fails to compile matches nothing.
func globRegexp(pattern string) *regexp.Regexp {
	var b strings.Builder
	b.WriteString(`\A`)
	for _, r := range pattern {
		switch r {
		case '*':
			b.WriteString(`.*`)
		case '?':
			b.WriteString(`.`)
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	b.WriteString(`\z`)
	re, err := regexp.Compile(b.String())
	if err != nil {
		return nil
	}
	return re
}

// admitGlobMatch reports whether the model-resolve glob pattern matches id.
func admitGlobMatch(pattern, id string) bool {
	re := globRegexp(pattern)
	return re != nil && re.MatchString(id)
}

// admitPattern is one class pattern: either a bare glob string, or an object
// {"glob": ..., "requires": ...}. A bare string has Conditional false.
type admitPattern struct {
	Glob     string `json:"glob"`
	Requires string `json:"requires"`

	// Conditional records that the JSON entry was an object, not a string.
	Conditional bool `json:"-"`

	re *regexp.Regexp
}

func (p *admitPattern) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) > 0 && b[0] == '"' {
		p.Conditional = false
		return json.Unmarshal(b, &p.Glob)
	}
	var obj struct {
		Glob     string `json:"glob"`
		Requires string `json:"requires"`
	}
	if err := json.Unmarshal(b, &obj); err != nil {
		return err
	}
	p.Glob = obj.Glob
	p.Requires = obj.Requires
	p.Conditional = true
	return nil
}

type admitRow struct {
	// modelGlobs keeps the raw models entries in order (exact or trailing-*);
	// families derives aliases from them.
	modelGlobs []string
	models     []*regexp.Regexp
	regex      []*regexp.Regexp
}

type admitTier struct {
	class  string
	admits []string
}

type admitEngine struct {
	premium  []admitPattern
	standard []admitPattern
	cheap    []admitPattern
	tiers    map[string]admitTier
}

type admitTable struct {
	rows    map[string]map[string]*admitRow
	classes map[string]*admitEngine
}

// admitTableOnce decodes and compiles the seed once.
var admitTableOnce = sync.OnceValue(loadAdmit)

func loadAdmit() *admitTable {
	var raw struct {
		ModelMap map[string]map[string]struct {
			Models []string `json:"models"`
			Regex  []string `json:"regex"`
		} `json:"modelMap"`
		Classes map[string]struct {
			Premium  []admitPattern          `json:"premium"`
			Standard []admitPattern          `json:"standard"`
			Cheap    []admitPattern          `json:"cheap"`
			Tiers    map[string]admitTierRaw `json:"tiers"`
		} `json:"classes"`
	}
	if err := json.Unmarshal(admitJSON, &raw); err != nil {
		// The seed is a build-time asset; an unreadable one must not hide rows.
		return &admitTable{}
	}

	t := &admitTable{
		rows:    make(map[string]map[string]*admitRow, len(raw.ModelMap)),
		classes: make(map[string]*admitEngine, len(raw.Classes)),
	}
	for engine, tiers := range raw.ModelMap {
		rows := make(map[string]*admitRow, len(tiers))
		for tier, row := range tiers {
			r := &admitRow{modelGlobs: row.Models}
			for _, m := range row.Models {
				if re := globRegexp(m); re != nil {
					r.models = append(r.models, re)
				}
			}
			for _, m := range row.Regex {
				if re, err := regexp.Compile(m); err == nil {
					r.regex = append(r.regex, re)
				}
			}
			rows[tier] = r
		}
		t.rows[engine] = rows
	}
	for engine, cl := range raw.Classes {
		eng := &admitEngine{
			premium:  compilePatterns(cl.Premium),
			standard: compilePatterns(cl.Standard),
			cheap:    compilePatterns(cl.Cheap),
			tiers:    make(map[string]admitTier, len(cl.Tiers)),
		}
		for tier, tr := range cl.Tiers {
			eng.tiers[tier] = admitTier{class: tr.Class, admits: tr.Admits}
		}
		t.classes[engine] = eng
	}
	return t
}

type admitTierRaw struct {
	Class  string   `json:"class"`
	Admits []string `json:"admits"`
}

func compilePatterns(pats []admitPattern) []admitPattern {
	out := make([]admitPattern, 0, len(pats))
	for _, p := range pats {
		p.re = globRegexp(p.Glob)
		out = append(out, p)
	}
	return out
}

// admitClassOf returns the most expensive class (premium, then standard, then
// cheap) of engine's class patterns that matches id, plus the matching
// patterns of that class. ok is false when the engine has no class table or no
// pattern matches — exactly when model-resolve.sh's class_of prints null. It is
// the class-of logic classify uses, exposed to the drift cross-check.
func admitClassOf(engine, id string) (class string, matches []admitPattern, ok bool) {
	t := admitTableOnce()
	eng, found := t.classes[engine]
	if !found {
		return "", nil, false
	}
	for _, c := range [3]struct {
		name string
		pats []admitPattern
	}{
		{"premium", eng.premium},
		{"standard", eng.standard},
		{"cheap", eng.cheap},
	} {
		var m []admitPattern
		for _, p := range c.pats {
			if p.re != nil && p.re.MatchString(id) {
				m = append(m, p)
			}
		}
		if len(m) > 0 {
			return c.name, m, true
		}
	}
	return "", nil, false
}

// classify ports model-resolve.sh's admission rules for one id. Unknown engine
// or tier returns admitted with no requires: advise must not hide a row it
// cannot classify.
func classify(engine, tier, id string) (admitState, string) {
	t := admitTableOnce()
	rows, ok := t.rows[engine]
	if !ok {
		return admitted, ""
	}
	row, ok := rows[tier]
	if !ok {
		return admitted, ""
	}
	eng, ok := t.classes[engine]
	if !ok {
		return admitted, ""
	}
	tr, ok := eng.tiers[tier]
	if !ok {
		return admitted, ""
	}

	// 1. Seed row: the models list (exact or trailing-* glob) or the regex list.
	for _, re := range row.models {
		if re.MatchString(id) {
			return admitted, ""
		}
	}
	for _, re := range row.regex {
		if re.MatchString(id) {
			return admitted, ""
		}
	}

	// 2. Most expensive matching class: premium, then standard, then cheap.
	class, matches, ok := admitClassOf(engine, id)
	if !ok {
		return retired, ""
	}

	admits := tr.admits
	if admits == nil {
		admits = []string{tr.class}
	}
	allowed := false
	for _, a := range admits {
		if a == class {
			allowed = true
			break
		}
	}
	if !allowed {
		return retired, ""
	}

	// 3. Every matching pattern carries a non-empty requires → gated; mixed or
	// bare-string patterns → admitted. The gate itself is not evaluated.
	requires := ""
	for _, m := range matches {
		if !m.Conditional || m.Requires == "" {
			return admitted, ""
		}
		if requires == "" {
			requires = m.Requires
		}
	}
	return gated, requires
}
