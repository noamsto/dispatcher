package entry

import (
	"bytes"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/roster"
)

func TestEngineCmd(t *testing.T) {
	cases := map[string]int{
		// The engines, as tmux reports them.
		"claude": 0, "codex": 0, "cursor-agent": 0, "node": 0, "pi": 0,
		// makeWrapper's hidden inner exec, and both at once.
		".claude": 0, ".node": 0, "pi-wrapped": 0, ".claude-wrapped": 0,
		// Everything else a pane can be running.
		"fish": 1, "bash": 1, "Claude": 1, "..claude": 1,
		"claude-wrapped-x": 1, "-wrapped": 1, "node ": 1, "": 1,
	}
	for cmd, want := range cases {
		var stderr bytes.Buffer
		if code := RunEngineCmd([]string{cmd}, &stderr); code != want {
			t.Errorf("engine-cmd %q = %d, want %d", cmd, code, want)
		}
		if stderr.Len() != 0 && cmd != "" {
			t.Errorf("engine-cmd %q wrote %q, want nothing", cmd, stderr.String())
		}
	}
}

// TestEngineCmdNeedsACommand: the arm refused an empty argument with its usage
// line rather than falling through the table, which is also what an empty
// pane_current_command must not do.
func TestEngineCmdNeedsACommand(t *testing.T) {
	for _, args := range [][]string{nil, {""}, {"", "extra"}} {
		var stdout, stderr bytes.Buffer
		if code := RunEngineCmd(args, &stderr); code != 1 {
			t.Fatalf("engine-cmd %v = %d, want 1", args, code)
		}
		if stdout.Len() != 0 {
			t.Fatalf("engine-cmd %v wrote %q to stdout", args, stdout.String())
		}
		if stderr.String() != "crew: engine-cmd <pane_current_command>\n" {
			t.Fatalf("engine-cmd %v stderr = %q", args, stderr.String())
		}
	}
}

// TestEngineCmdAgreesWithRoster: the table is roster's, so `crew engine-cmd`
// can never answer about a pane differently from how `crew roster` counts it.
func TestEngineCmdAgreesWithRoster(t *testing.T) {
	for _, cmd := range append(append([]string{}, roster.EngineCommands...), "fish", "", ".claude-wrapped", ".pi") {
		want := 1
		if roster.IsEngineCmd(cmd) {
			want = 0
		}
		var stderr bytes.Buffer
		if got := RunEngineCmd([]string{cmd}, &stderr); got != want {
			t.Errorf("engine-cmd %q = %d, roster.IsEngineCmd says %d", cmd, got, want)
		}
	}
}
