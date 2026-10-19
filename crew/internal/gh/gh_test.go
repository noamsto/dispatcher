package gh_test

import (
	"context"
	"os/exec"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/gh"
	"github.com/noamsto/dispatcher/crew/internal/gh/ghtest"
)

func TestExec(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\necho out\necho err >&2\nexit 3\n"
	stub := dir + "/gh"
	if out, err := exec.Command("sh", "-c", "printf '%s' \"$1\" > \"$2\" && chmod +x \"$2\"", "sh", script, stub).CombinedOutput(); err != nil {
		t.Fatalf("stub: %v: %s", err, out)
	}
	t.Setenv("PATH", dir)
	ctx := context.Background()
	if got, err := (gh.Exec{}).Run(ctx); got != "out" || err == nil {
		t.Errorf("Run = %q, %v; want stdout kept alongside the failure", got, err)
	}
	if got, _ := (gh.Exec{}).Combined(ctx); got != "out\nerr" {
		t.Errorf("Combined = %q, want stderr merged", got)
	}
	if !gh.Installed(ctx, gh.Exec{}) {
		t.Error("Installed = false with a gh on PATH")
	}
}

func TestMissing(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := gh.Exec{}.Run(context.Background())
	if !gh.Missing(err) || gh.Installed(context.Background(), gh.Exec{}) {
		t.Errorf("err = %v, want not found", err)
	}
}

func TestFake(t *testing.T) {
	f := &ghtest.Fake{Out: map[string]string{"pr list": "x"}}
	if got, err := f.Run(context.Background(), "pr", "list"); got != "x" || err != nil {
		t.Errorf("Run = %q, %v", got, err)
	}
	if _, err := f.Combined(context.Background(), "nope"); err == nil {
		t.Error("unmatched call succeeded")
	}
	if c := f.Calls(); len(c) != 2 || c[1] != "nope" {
		t.Errorf("Calls = %v", c)
	}
}
