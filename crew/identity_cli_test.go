package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The `identity:` bats cases that asserted the subcommand itself (#1044): what
// one run of `crew identity` prints, and what it prints when tmux is not there.
// The bus-driven cases — recorded identities, live workers, the codename grammar
// — are in crew/internal/identity, where the arm is.

// identityCLI runs `crew identity argv…` in a fresh git repo whose bus holds log
// (a nil log leaves no bus at all), and returns what the arm printed.
func identityCLI(t *testing.T, log *string, argv ...string) (stdout, stderr string, code int) {
	t.Helper()
	repo, _ := repoWithLog(t, log, 0)
	// A dispatching host sets this for the workers it holds elsewhere; the pool
	// answers these cases, so the list starts empty.
	t.Setenv("CREW_IDENTITY_AVOID", "")
	e := env{getwd: func() (string, error) { return repo, nil }, probes: noTouchProbes()}

	var out, errOut bytes.Buffer
	code = run(context.Background(), append([]string{"identity"}, argv...), &out, &errOut, e)
	return out.String(), errOut.String(), code
}

func TestIdentityCLIIsDeterministicOverTheBranch(t *testing.T) {
	first, errFirst, codeFirst := identityCLI(t, nil, "feat/foo")
	second, errSecond, codeSecond := identityCLI(t, nil, "feat/foo")

	if codeFirst != 0 || codeSecond != 0 {
		t.Fatalf("codes = %d %d, want 0 0 (stderr %q %q)", codeFirst, codeSecond, errFirst, errSecond)
	}
	if errFirst != "" || errSecond != "" {
		t.Errorf("stderr = %q %q, want empty", errFirst, errSecond)
	}
	if first != second {
		t.Errorf("identity feat/foo printed %q then %q, want the same line twice", first, second)
	}
	if strings.Count(first, "\n") != 1 {
		t.Errorf("identity feat/foo printed %q, want one line", first)
	}
}

func TestIdentityCLIEmitsNameColorAndTmuxKeys(t *testing.T) {
	out, errOut, code := identityCLI(t, nil, "feat/foo")
	if code != 0 || errOut != "" {
		t.Fatalf("identity feat/foo = %d %q, want 0 and no stderr", code, errOut)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("identity feat/foo printed %q, want one json object: %v", out, err)
	}
	for _, key := range []string{"name", "color", "tmux"} {
		if _, ok := doc[key]; !ok {
			t.Errorf("identity feat/foo printed %q, which has no %q key", out, key)
		}
	}
}

func TestIdentityCLIResolvesABranchWithNoTmuxAvailable(t *testing.T) {
	// The case's fake tmux: it is on PATH and every read fails, so no window
	// names reach the arm. calls records that the arm did ask it.
	bin := t.TempDir()
	calls := filepath.Join(bin, "calls")
	script := "#!/bin/sh\nprintf x >>" + calls + "\nexit 1\n"
	if err := os.WriteFile(filepath.Join(bin, "tmux"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	out, errOut, code := identityCLI(t, nil, "feat/9-new", "c1")
	if code != 0 || errOut != "" {
		t.Fatalf("identity feat/9-new = %d %q, want 0 and no stderr", code, errOut)
	}
	if _, err := os.Stat(calls); err != nil {
		t.Fatalf("the arm resolved the branch without reading tmux, so a failing read went untested: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("identity feat/9-new printed %q, want one json object: %v", out, err)
	}
	if name, _ := doc["name"].(string); name == "" {
		t.Errorf("identity feat/9-new printed %q, want a name", out)
	}
}
