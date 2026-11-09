package rate

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/drift"
)

// The two drift guards for the ported admission table:
//
//   - TestAdmitSeedMatchesShippedDefaults: admit.json is the shipped
//     defaults.json's .modelMap and .classes copied by hand, so the two must
//     stay equal; every glob and regex in the table must also compile.
//   - TestAdmitPatternsMatchModelResolve: classify's class-of logic and its
//     gated/retired verdict are ports of adapters/core/model-resolve.sh, so the
//     bash that still ships is the oracle for them.
//
// Both read repo files outside crew/, so drift.Missing skips them when the Nix
// build sandbox (src = ./crew) lacks the file and fails under CREW_DRIFT_REQUIRED.
// The second also needs bash and jq, and routes those two through drift.Missing
// for the same reason: under CREW_DRIFT_REQUIRED a missing tool must fail, not
// silently drop the only oracle for the ported rules.

var (
	admitDefaultsPath = filepath.Join("..", "..", "..", "adapters", "core", "defaults.json")
	admitScriptPath   = filepath.Join("..", "..", "..", "adapters", "core", "model-resolve.sh")
)

// TestAdmitSeedMatchesShippedDefaults keeps the embedded seed honest: a model
// release that edits adapters/core/defaults.json without regenerating
// admit.json would otherwise make every admission verdict wrong.
func TestAdmitSeedMatchesShippedDefaults(t *testing.T) {
	shipped, err := os.ReadFile(admitDefaultsPath)
	if err != nil {
		drift.Missing(t, admitDefaultsPath, err)
	}
	var want, got map[string]any
	if err := json.Unmarshal(shipped, &want); err != nil {
		t.Fatalf("%s: %v", admitDefaultsPath, err)
	}
	if err := json.Unmarshal(admitJSON, &got); err != nil {
		t.Fatalf("admit.json: %v", err)
	}

	for _, sub := range []string{"modelMap", "classes"} {
		if reflect.DeepEqual(want[sub], got[sub]) {
			continue
		}
		t.Errorf("admit.json .%s differs from %s .%s; first differing key: %s",
			sub, admitDefaultsPath, sub, firstDifferingKey(want[sub], got[sub]))
	}

	// Every glob and regex must compile; loadAdmit drops the ones that do not,
	// which would silently narrow the row instead of admitting the id.
	var raw struct {
		ModelMap map[string]map[string]struct {
			Models []string `json:"models"`
			Regex  []string `json:"regex"`
		} `json:"modelMap"`
		Classes map[string]struct {
			Premium  []admitPattern `json:"premium"`
			Standard []admitPattern `json:"standard"`
			Cheap    []admitPattern `json:"cheap"`
		} `json:"classes"`
	}
	if err := json.Unmarshal(admitJSON, &raw); err != nil {
		t.Fatalf("admit.json: %v", err)
	}
	for engine, tiers := range raw.ModelMap {
		for tier, row := range tiers {
			for _, m := range row.Models {
				if globRegexp(m) == nil {
					t.Errorf("admit.json .modelMap.%s.%s.models %q does not compile", engine, tier, m)
				}
			}
			for _, re := range row.Regex {
				if _, err := regexp.Compile(re); err != nil {
					t.Errorf("admit.json .modelMap.%s.%s.regex %q does not compile: %v", engine, tier, re, err)
				}
			}
		}
	}
	for engine, cl := range raw.Classes {
		for class, pats := range map[string][]admitPattern{
			"premium": cl.Premium, "standard": cl.Standard, "cheap": cl.Cheap,
		} {
			for _, p := range pats {
				if globRegexp(p.Glob) == nil {
					t.Errorf("admit.json .classes.%s.%s %q does not compile", engine, class, p.Glob)
				}
			}
		}
	}
}

// firstDifferingKey names the first key (sorted) whose value differs between
// the repo subtree a and the embedded subtree b, or which only one side holds.
func firstDifferingKey(a, b any) string {
	am, aok := a.(map[string]any)
	bm, bok := b.(map[string]any)
	if !aok || !bok {
		return "(not a JSON object)"
	}
	keys := make([]string, 0, len(am)+len(bm))
	seen := make(map[string]bool, len(am)+len(bm))
	for k := range am {
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	for k := range bm {
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		av, aok := am[k]
		bv, bok := bm[k]
		switch {
		case !aok:
			return k + " (only in admit.json)"
		case !bok:
			return k + " (only in " + admitDefaultsPath + ")"
		case !reflect.DeepEqual(av, bv):
			return k
		}
	}
	return "(no key differs)"
}

// admitDriftFixtures is the id set per engine. Every id is shape_ok for its
// engine, so model-resolve's class_of is decided by the class patterns, not by
// the shape filter. Between them the ids cover: seed aliases ("opus",
// "kimi-k3-high", "gpt-5.6-sol"), glob-matched full ids ("claude-opus-5-5",
// "openrouter/qwen/qwen3.8-flash"), a dated snapshot
// ("claude-opus-4-5-20251101"), the cursor regex row
// ("claude-opus-5-5-high"), gated provider ids ("anthropic/claude-opus-5-5",
// "anthropic/claude-sonnet-5-5"), a route variant
// ("openrouter/anthropic/claude-opus-5.5") and ids no pattern knows.
var admitDriftFixtures = map[string][]string{
	"claude": {
		"opus",
		"claude-opus-5-5",
		"claude-sonnet-5-5",
		"claude-haiku-4-5",
		"claude-opus-4-5-20251101",
		"claude-zzz-1",
	},
	"codex": {
		"gpt-5.6-sol",
		"gpt-5.6-terra",
		"gpt-5.6-luna",
		"gpt-5.5",
		"gpt-5.6-orion",
	},
	"cursor": {
		"kimi-k3-high",
		"grok-4.7-medium",
		"grok-4.7-low",
		"claude-opus-5-5-high",
		"composer-2.5",
		"cursor-unknown-9",
	},
	"pi": {
		"openrouter/deepseek/deepseek-v4.1-flash",
		"openrouter/qwen/qwen3.8-flash",
		"anthropic/claude-opus-5-5",
		"anthropic/claude-sonnet-5-5",
		"openrouter/anthropic/claude-opus-5.5",
		"zzz/unknown-model-1",
	},
}

var (
	admitDriftEngines = []string{"claude", "codex", "cursor", "pi"}
	admitDriftTiers   = []string{"trivial", "standard", "deep"}
)

type admitCatalogModel struct {
	ID string `json:"id"`
}

type admitCatalogEntry struct {
	FetchedEpoch int64               `json:"fetched_epoch"`
	Source       string              `json:"source"`
	Models       []admitCatalogModel `json:"models"`
}

type admitProbe struct {
	ClassOf *string `json:"class_of"`
	// Admitted is model-resolve's admitted list: the seed row's models plus the
	// unconditional catalog candidates the row does not already match.
	Admitted    []string `json:"admitted"`
	Conditional []struct {
		ID       string `json:"id"`
		Requires string `json:"requires"`
	} `json:"conditional"`
}

// TestAdmitPatternsMatchModelResolve runs the shipped bash oracle for every
// (engine, tier, fixture id) triple and checks the ported rules against it:
// admitClassOf must equal model-resolve's class_of, and every id
// model-resolve reports as conditional must classify as gated(its requires)
// where the repo tier admits that class and retired where it does not. The
// admitted/regex strings model-resolve prints are NOT compared: they hold the
// seed row's globs, so a concrete seed-matched id is absent from them by design.
func TestAdmitPatternsMatchModelResolve(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		drift.Missing(t, "bash (on PATH)", err)
	}
	if _, err := exec.LookPath("jq"); err != nil {
		drift.Missing(t, "jq (on PATH, model-resolve.sh needs it)", err)
	}
	defaults, err := os.ReadFile(admitDefaultsPath)
	if err != nil {
		drift.Missing(t, admitDefaultsPath, err)
	}
	if _, err := os.Stat(admitScriptPath); err != nil {
		drift.Missing(t, admitScriptPath, err)
	}

	// The repo tier admits are the oracle for retire-vs-gate, read here from
	// adapters/core/defaults.json rather than from the embedded table.
	var repo struct {
		Classes map[string]struct {
			Tiers map[string]struct {
				Class  string   `json:"class"`
				Admits []string `json:"admits"`
			} `json:"tiers"`
		} `json:"classes"`
	}
	if err := json.Unmarshal(defaults, &repo); err != nil {
		t.Fatalf("%s: %v", admitDefaultsPath, err)
	}
	admits := func(engine, tier, class string) bool {
		cl, ok := repo.Classes[engine]
		if !ok {
			return false
		}
		tr, ok := cl.Tiers[tier]
		if !ok {
			return false
		}
		list := tr.Admits
		if list == nil {
			list = []string{tr.Class}
		}
		for _, a := range list {
			if a == class {
				return true
			}
		}
		return false
	}

	// One catalog object keyed by engine, listing every fixture id so class_of
	// is decided by the patterns and not by catalog absence.
	catalog := make(map[string]admitCatalogEntry, len(admitDriftEngines))
	for _, engine := range admitDriftEngines {
		entry := admitCatalogEntry{FetchedEpoch: 1700000000, Source: "admit_drift_test"}
		for _, id := range admitDriftFixtures[engine] {
			entry.Models = append(entry.Models, admitCatalogModel{ID: id})
		}
		catalog[engine] = entry
	}
	dir := t.TempDir()
	catalogPath := filepath.Join(dir, "model-catalog.json")
	catalogJSON, err := json.Marshal(catalog)
	if err != nil {
		t.Fatalf("marshalling temp catalog: %v", err)
	}
	if err := os.WriteFile(catalogPath, catalogJSON, 0o600); err != nil {
		t.Fatalf("writing temp catalog: %v", err)
	}
	statePath := filepath.Join(dir, "model-class-state.json")
	if err := os.WriteFile(statePath, []byte("{}"), 0o600); err != nil {
		t.Fatalf("writing temp class-state: %v", err)
	}

	// A gated id's class and requires do not vary by tier; collect every one
	// model-resolve reports so the same id is checked at all three tiers.
	conditionalRequires := make(map[string]map[string]string, len(admitDriftEngines))
	for _, engine := range admitDriftEngines {
		conditionalRequires[engine] = make(map[string]string)
		for _, tier := range admitDriftTiers {
			probe := runModelResolve(t, defaults, engine, tier, catalogPath, statePath, "")
			for _, c := range probe.Conditional {
				if c.Requires == "" {
					continue
				}
				conditionalRequires[engine][c.ID] = c.Requires
			}
		}
	}

	compared := 0
	for _, engine := range admitDriftEngines {
		for _, tier := range admitDriftTiers {
			for _, id := range admitDriftFixtures[engine] {
				compared++
				probe := runModelResolve(t, defaults, engine, tier, catalogPath, statePath, id)
				classOf := "null"
				if probe.ClassOf != nil {
					classOf = *probe.ClassOf
				}

				// 1. The ported class-of must equal model-resolve's class_of.
				class, _, ok := admitClassOf(engine, id)
				switch {
				case probe.ClassOf == nil && ok:
					t.Errorf("%s %s %q: admitClassOf = %q, model-resolve class_of = null", engine, tier, id, class)
				case probe.ClassOf != nil && !ok:
					t.Errorf("%s %s %q: admitClassOf = null, model-resolve class_of = %q", engine, tier, id, *probe.ClassOf)
				case probe.ClassOf != nil && ok && class != *probe.ClassOf:
					t.Errorf("%s %s %q: admitClassOf = %q, model-resolve class_of = %q", engine, tier, id, class, *probe.ClassOf)
				}

				// 2. bash's admitted list holds the seed row's models (globs, not
				// concrete ids) plus the unconditional catalog candidates, so a
				// seed-matched id is compared nowhere. A candidate the oracle
				// lists as admitted must classify as admitted.
				listed := false
				for _, a := range probe.Admitted {
					if a == id {
						listed = true
						break
					}
				}
				if listed && !admitSeedMatch(engine, tier, id) {
					if state, requires := classify(engine, tier, id); state != admitted || requires != "" {
						t.Errorf("%s %s %q: model-resolve lists it as an unconditional candidate, classify = (%s,%q), want (admitted,\"\")",
							engine, tier, id, state, requires)
					}
				}

				// 3. Every gated id: gated(requires) when the repo tier admits
				// its class, retired when it does not.
				requires, gatedID := conditionalRequires[engine][id]
				if !gatedID {
					continue
				}
				wantState, wantRequires := retired, ""
				if probe.ClassOf != nil && admits(engine, tier, *probe.ClassOf) {
					wantState, wantRequires = gated, requires
				}
				gotState, gotRequires := classify(engine, tier, id)
				if gotState != wantState || gotRequires != wantRequires {
					t.Errorf("%s %s %q: classify = (%s,%q), want (%s,%q); model-resolve class_of = %s, gated requires %q",
						engine, tier, id, gotState, gotRequires, wantState, wantRequires, classOf, requires)
				}
			}
		}
	}
	t.Logf("cross-checked %d (engine,tier,id) triples over %d engines x %d tiers and %d fixture ids",
		compared, len(admitDriftEngines), len(admitDriftTiers), len(admitDriftFixtures["claude"])+
			len(admitDriftFixtures["codex"])+len(admitDriftFixtures["cursor"])+len(admitDriftFixtures["pi"]))
}

// admitSeedMatch reports whether the seed row's models globs or regex list name
// id — model-resolve's seedm for that row. Such an id is absent from the
// oracle's admitted list by design: the list holds the row's globs instead.
func admitSeedMatch(engine, tier, id string) bool {
	row := admitTableOnce().rows[engine][tier]
	if row == nil {
		return false
	}
	for _, re := range row.models {
		if re.MatchString(id) {
			return true
		}
	}
	for _, re := range row.regex {
		if re.MatchString(id) {
			return true
		}
	}
	return false
}

// runModelResolve invokes the shipped bash resolver with defaults.json on
// stdin and decodes its JSON object. A non-zero exit is fatal: the test only
// passes rows the script knows, and a non-zero exit there is a real failure.
func runModelResolve(t *testing.T, defaults []byte, engine, tier, catalogPath, statePath, id string) admitProbe {
	t.Helper()
	args := []string{admitScriptPath, engine, tier, catalogPath, statePath}
	if id != "" {
		args = append(args, id)
	}
	cmd := exec.Command("bash", args...)
	cmd.Stdin = bytes.NewReader(defaults)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			t.Fatalf("model-resolve.sh %s %s %q exited %d: %s",
				engine, tier, id, ee.ExitCode(), strings.TrimSpace(stderr.String()))
		}
		t.Fatalf("running model-resolve.sh %s %s %q: %v", engine, tier, id, err)
	}
	var probe admitProbe
	if err := json.Unmarshal(stdout.Bytes(), &probe); err != nil {
		t.Fatalf("model-resolve.sh %s %s %q: decoding output: %v\n%s", engine, tier, id, err, stdout.String())
	}
	return probe
}
