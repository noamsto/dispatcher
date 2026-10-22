package main

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
)

// `crew occupants` reads tmux and the path it is handed, so neither its refusal
// nor its answer needs a repo around it.
func TestOccupantsRunsOutsideARepo(t *testing.T) {
	outside, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(outside)
	t.Setenv("TMUX_PANE", "")
	calls, _ := stubTmuxLogged(t, "@1\tsage\t/wt/a\n", "@1\t%1\tclaude\n")
	e := env{getwd: func() (string, error) { return outside, nil }}

	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"occupants"}, &stdout, &stderr, e); code != exitFailure ||
		stdout.Len() != 0 || stderr.String() != "crew: occupants <worktree-path>\n" {
		t.Errorf("no path: code %d stdout %q stderr %q, want 1 and %q",
			code, stdout.String(), stderr.String(), "crew: occupants <worktree-path>\n")
	}
	if got := calls(); got != "" {
		t.Errorf("a missing path read tmux before refusing: %s", got)
	}

	stdout.Reset()
	stderr.Reset()
	if code := run(context.Background(), []string{"occupants", "/wt/a"}, &stdout, &stderr, e); code != 0 {
		t.Fatalf("occupants /wt/a: code %d stderr %q", code, stderr.String())
	}
	want := `[{"window":"@1","name":"sage","pane":"%1","command":"claude","engine":true,` +
		`"panes":[{"pane":"%1","command":"claude"}]}]` + "\n"
	if stdout.String() != want {
		t.Errorf("stdout = %q, want %q", stdout.String(), want)
	}
}
