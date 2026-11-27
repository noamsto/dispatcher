package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/spikes/headless-supervisor/internal/cases"
	"github.com/noamsto/dispatcher/spikes/headless-supervisor/internal/supervisor"
)

func init() {
	cases.Register(cases.Case{Name: "t-pass", Run: func(context.Context, *cases.CaseEnv) (string, error) { return "ok", nil }})
	cases.Register(cases.Case{Name: "t-fail", Engines: []string{"pi"}, Run: func(context.Context, *cases.CaseEnv) (string, error) {
		return "", errors.New("boom")
	}})
}

func runHsup(t *testing.T, args ...string) (int, string) {
	t.Helper()
	var out, errb strings.Builder
	code := run(args, &out, &errb)
	return code, out.String()
}

func TestCasesListsNamePairs(t *testing.T) {
	_, out := runHsup(t, "cases")
	for _, want := range []string{"t-pass claude\n", "t-pass pi\n", "t-fail pi\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in %q", want, out)
		}
	}
	if strings.Contains(out, "t-fail claude") {
		t.Errorf("t-fail must not list claude: %q", out)
	}
}

func TestCasePassFailExitCodes(t *testing.T) {
	d := t.TempDir()
	base := []string{"--work", filepath.Join(d, "work"), "--outside", filepath.Join(d, "out"), "--state", filepath.Join(d, "state"), "--transcript", filepath.Join(d, "t.jsonl")}

	code, out := runHsup(t, append([]string{"case", "t-pass", "--engine", "claude"}, base...)...)
	if code != 0 || out != "PASS t-pass claude: ok\n" {
		t.Fatalf("pass: %d %q", code, out)
	}
	code, out = runHsup(t, append([]string{"case", "t-fail", "--engine", "pi"}, base...)...)
	if code != 1 || out != "FAIL t-fail pi: boom\n" {
		t.Fatalf("fail: %d %q", code, out)
	}
	code, out = runHsup(t, append([]string{"case", "t-fail", "--engine", "claude"}, base...)...)
	if code != 1 || !strings.HasPrefix(out, "FAIL t-fail claude: case not defined") {
		t.Fatalf("wrong engine: %d %q", code, out)
	}
	if code, _ = runHsup(t, append([]string{"case", "nope", "--engine", "pi"}, base...)...); code != 1 {
		t.Fatalf("unknown case code = %d", code)
	}
}

func TestCtlRefusesOpenStateDir(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, supervisor.SessionFile), []byte(`{"engine":"pi"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := ctl([]string{state, "session"}, &out); err != nil {
		t.Fatalf("own 0700 state: %v", err)
	}
	if err := os.Chmod(state, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, cmd := range []string{"session", "status"} {
		if err := ctl([]string{state, cmd}, &out); err == nil || !strings.Contains(err.Error(), "open to group or others") {
			t.Fatalf("ctl %s on a 0755 state: %v", cmd, err)
		}
	}
}
