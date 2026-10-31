package classes

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/drift"
)

const (
	resolveScript = "../../../adapters/core/model-resolve.sh"
	resolveConfig = "../../../adapters/core/defaults.json"
	catalogJSON   = `{"codex":{"models":[{"id":"gpt-5.6-luna"},{"id":"gpt-5.6-terra"},{"id":"gpt-5.7-terra"},{"id":"gpt-5.6-sol"},{"id":"gpt-5.7-sol"}]}}`
)

// testResolver reads the shipped resolver and defaults; both live outside
// crew/, so the Nix sandbox skips.
func testResolver(t *testing.T) Resolver {
	t.Helper()
	if _, err := os.Stat(resolveScript); err != nil {
		drift.Missing(t, resolveScript, err)
	}
	settings, err := os.ReadFile(resolveConfig)
	if err != nil {
		drift.Missing(t, resolveConfig, err)
	}
	catalog := filepath.Join(t.TempDir(), "catalog.json")
	if err := os.WriteFile(catalog, []byte(catalogJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	return Resolver{Bash: "bash", Script: resolveScript, Settings: settings, Catalog: catalog}
}

func TestResolveNoState(t *testing.T) {
	r := testResolver(t)
	res, err := r.Resolve("codex", "standard", filepath.Join(t.TempDir(), "absent.json"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Default != "gpt-5.6-terra" {
		t.Errorf("Default = %q, want gpt-5.6-terra", res.Default)
	}
	if res.Trial == nil || *res.Trial != "gpt-5.7-terra" {
		t.Errorf("Trial = %v, want gpt-5.7-terra", res.Trial)
	}
}

func TestResolveRejectsCrossClassDefault(t *testing.T) {
	r := testResolver(t)
	state := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(state, []byte(`{"codex":{"cheap":{"default":"gpt-5.6-sol"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := r.Resolve("codex", "trivial", state)
	if err != nil {
		t.Fatal(err)
	}
	if res.Default != "gpt-5.6-luna" {
		t.Errorf("Default = %q, want the seed gpt-5.6-luna", res.Default)
	}
	if !containsWarning(res.Warnings, "is not valid for the class") {
		t.Errorf("Warnings = %q, want one naming the invalid class default", res.Warnings)
	}
}

func TestResolveValidDefault(t *testing.T) {
	r := testResolver(t)
	for _, tc := range []struct {
		name  string
		tier  string
		class string
		id    string
		want  bool
	}{
		{"same class", "standard", "standard", "gpt-5.7-terra", true},
		{"cross class", "standard", "standard", "gpt-5.7-sol", false},
		{"cheaper class", "deep", "premium", "gpt-5.7-terra", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _, err := r.ValidDefault("codex", tc.tier, tc.class, tc.id)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("ValidDefault(%s, %s, %s) = %v, want %v", tc.tier, tc.class, tc.id, got, tc.want)
			}
		})
	}
}

func TestResolveUnavailable(t *testing.T) {
	_, err := Resolver{}.Resolve("codex", "standard", "")
	if !errors.Is(err, ErrUnavailable) {
		t.Errorf("Resolve err = %v, want ErrUnavailable", err)
	}
	_, _, err = Resolver{}.ValidDefault("codex", "standard", "standard", "x")
	if !errors.Is(err, ErrUnavailable) {
		t.Errorf("ValidDefault err = %v, want ErrUnavailable", err)
	}
}

func TestResolveUnknownTier(t *testing.T) {
	r := testResolver(t)
	_, err := r.Resolve("codex", "huge", filepath.Join(t.TempDir(), "absent.json"))
	if err == nil || !strings.Contains(err.Error(), "no model row") {
		t.Errorf("err = %v, want the resolver's 'no model row' stderr", err)
	}
}

func containsWarning(warnings []string, sub string) bool {
	for _, w := range warnings {
		if strings.Contains(w, sub) {
			return true
		}
	}
	return false
}
