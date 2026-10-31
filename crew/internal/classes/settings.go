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
	Raw      []byte                     // the merged settings JSON, fed to model-resolve on stdin
	ModelMap map[string]map[string]Seed // engine → tier → seed row
	Classes  map[string]EngineClasses   // engine → class config
}

// Seed is a modelMap row; only the default seeds a class.
type Seed struct {
	Default string `json:"default"`
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
		ModelMap map[string]map[string]Seed `json:"modelMap"`
		Classes  map[string]EngineClasses   `json:"classes"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return Settings{}, fmt.Errorf("parse settings: %w", err)
	}
	return Settings{Raw: raw, ModelMap: doc.ModelMap, Classes: doc.Classes}, nil
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

// Engines lists the engines that have class config, sorted.
func (s Settings) Engines() []string {
	out := make([]string, 0, len(s.Classes))
	for e := range s.Classes {
		out = append(out, e)
	}
	sort.Strings(out)
	return out
}
