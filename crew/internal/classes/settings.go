package classes

import (
	"encoding/json"
	"fmt"
	"sort"
)

// tierOrder is the only tier vocabulary there is; TiersOf walks it so callers
// get a stable order instead of Go's map order.
var tierOrder = []string{"trivial", "standard", "deep"}

// Settings is the slice of dispatch-config's merged JSON the class machinery
// reads. Raw is kept verbatim because model-resolve takes the whole document on
// stdin and applies its own checks to it.
type Settings struct {
	Raw     []byte                   // the merged settings JSON, fed to model-resolve on stdin
	Classes map[string]EngineClasses // engine → class config
}

// EngineClasses is classes.<engine>. The cheap/standard/premium match lists are
// not decoded: their shape varies (globs, objects with a requirement) and
// model-resolve owns them.
type EngineClasses struct {
	AutoPromote *bool                `json:"autoPromote"`
	Tiers       map[string]TierClass `json:"tiers"`
}

// TierClass maps a tier onto the class whose default it takes.
type TierClass struct {
	Class string `json:"class"`
}

// ParseSettings decodes the merged settings JSON. It errors only when the
// document is not an object of the expected shape.
func ParseSettings(raw []byte) (Settings, error) {
	var doc struct {
		Classes map[string]EngineClasses `json:"classes"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return Settings{}, fmt.Errorf("parse settings: %w", err)
	}
	return Settings{Raw: raw, Classes: doc.Classes}, nil
}

// TiersOf lists the tiers that take their model from class, in trivial,
// standard, deep order.
func (s Settings) TiersOf(engine, class string) []string {
	var out []string
	tiers := s.Classes[engine].Tiers
	for _, tier := range tierOrder {
		if tc, ok := tiers[tier]; ok && tc.Class == class {
			out = append(out, tier)
		}
	}
	return out
}

// AutoPromote reports whether the engine may switch a class default on its own.
// Absent means yes: promotion is opt-out.
func (s Settings) AutoPromote(engine string) bool {
	p := s.Classes[engine].AutoPromote
	return p == nil || *p
}

// floorWeights is the burn weight an unpriced model takes from its selection
// class, in the resolver's most-expensive-first order.
var floorWeights = []struct {
	Class  string
	Weight int
}{{"premium", 4}, {"standard", 2}, {"cheap", 1}}

// FloorClass returns the most expensive class whose patterns match model for
// engine, with that class's floor burn weight. ok is false when no pattern
// matches. Patterns follow model-resolve's glob: `*` and `?` wildcards, every
// other character literal.
func FloorClass(raw []byte, engine, model string) (class string, weight int, ok bool) {
	var doc struct {
		Classes map[string]map[string]json.RawMessage `json:"classes"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return "", 0, false
	}
	cfg := doc.Classes[engine]
	for _, fw := range floorWeights {
		var pats []json.RawMessage
		if json.Unmarshal(cfg[fw.Class], &pats) != nil {
			continue
		}
		for _, p := range pats {
			var glob string
			var obj struct {
				Glob string `json:"glob"`
			}
			switch {
			case json.Unmarshal(p, &glob) == nil:
			case json.Unmarshal(p, &obj) == nil:
				glob = obj.Glob
			}
			if glob != "" && wildMatch(glob, model) {
				return fw.Class, fw.Weight, true
			}
		}
	}
	return "", 0, false
}

func wildMatch(pat, s string) bool {
	for len(pat) > 0 {
		switch pat[0] {
		case '*':
			for i := 0; i <= len(s); i++ {
				if wildMatch(pat[1:], s[i:]) {
					return true
				}
			}
			return false
		case '?':
			if s == "" {
				return false
			}
		default:
			if s == "" || s[0] != pat[0] {
				return false
			}
		}
		pat, s = pat[1:], s[1:]
	}
	return s == ""
}

// Engines lists the engines that have class config, sorted.
func (s Settings) Engines() []string {
	out := make([]string, 0, len(s.Classes))
	for e := range s.Classes {
		out = append(out, e)
	}
	sort.Strings(out)
	return out
}
