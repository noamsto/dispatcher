package reap

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/tmux/tmuxtest"
)

// occTmux answers the three reads `_occupants` makes.
func occTmux(self, wins, panes string) *tmuxtest.Fake {
	return &tmuxtest.Fake{Out: map[string]string{"display-message": self, "list-windows": wins, "list-panes": panes}}
}

func TestOccupantsFiltersAndShape(t *testing.T) {
	wins := strings.Join([]string{
		"@1\tworker\t/wt/a",
		"@2\tdispatcher\t/wt/a",
		"@3\t\t/wt/a",
		"@4\tother\t/wt/b",
		"@5\tmine\t/wt/a",
		"@6\tshellonly\t/wt/a",
		"\tnoid\t/wt/a",
		"@7\ttwo\t/wt/a",
	}, "\n")
	panes := strings.Join([]string{
		"@1\t%1\tvim",
		"@1\t%2\t.claude-wrapped",
		"@1\t%3\tcodex",
		"@5\t%9\tclaude",
		"@6\t%6\tfish",
		"@7\t%7\tpi",
	}, "\n")
	tm := occTmux("@5", wins, panes)
	p := Probes{Tmux: tm}

	got := Occupants(context.Background(), p, "/wt/a", "%0")
	want := []Occupant{
		{Window: "@1", Name: "worker", Pane: "%2", Command: ".claude-wrapped", Engine: true,
			Panes: []PaneCmd{{"%2", ".claude-wrapped"}, {"%3", "codex"}}},
		{Window: "@6", Name: "shellonly"},
		{Window: "@7", Name: "two", Pane: "%7", Command: "pi", Engine: true, Panes: []PaneCmd{{"%7", "pi"}}},
	}
	if !equalOcc(got, want) {
		t.Fatalf("occupants:\n got %+v\nwant %+v", got, want)
	}
	wantCalls := []string{
		"display-message -p -t %0 #{window_id}",
		"list-windows -a -F #{window_id}\t#{@crew_name}\t#{pane_current_path}",
		"list-panes -a -F #{window_id}\t#{pane_id}\t#{pane_current_command}",
	}
	if calls := tm.Calls(); !slices.Equal(calls, wantCalls) {
		t.Fatalf("tmux calls:\n got %q\nwant %q", calls, wantCalls)
	}
}

// TestOccupantsRows is the stdout for each window shape the bats suite carried:
// one row per pane command an engine reports, a finished agent that dropped to a
// shell, windows none of which belong to the path, and the caller's own window.
func TestOccupantsRows(t *testing.T) {
	engineRow := func(cmd string) string {
		return `[{"window":"@1","name":"sage","pane":"%1","command":"` + cmd +
			`","engine":true,"panes":[{"pane":"%1","command":"` + cmd + `"}]}]`
	}
	tests := []struct {
		name        string
		wins, panes string
		selfWin     string // display-message's answer: the window the caller is in
		want        string
	}{
		{".claude-wrapped", "@1\tsage\t/wt/a", "@1\t%1\t.claude-wrapped", "", engineRow(".claude-wrapped")},
		{"node", "@1\tsage\t/wt/a", "@1\t%1\tnode", "", engineRow("node")},
		{"codex", "@1\tsage\t/wt/a", "@1\t%1\tcodex", "", engineRow("codex")},
		{"pi", "@1\tsage\t/wt/a", "@1\t%1\tpi", "", engineRow("pi")},
		{"a shell prompt is still an occupant", "@23\tsage\t/wt/a", "@23\t%33\tfish", "",
			`[{"window":"@23","name":"sage","pane":null,"command":"","engine":false,"panes":[]}]`},
		{"other paths, unnamed windows, the dispatcher", "@1\tsage\t/wt/b\n@2\t\t/wt/a\n@3\tdispatcher\t/wt/a",
			"@1\t%1\tclaude\n@3\t%3\tclaude", "", "[]"},
		{"the caller's own window", "@23\tsage\t/wt/a", "@23\t%33\tclaude", "@23", "[]"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// crew asks which window it is in only when it is in a pane at all.
			tmuxPane := ""
			if tc.selfWin != "" {
				tmuxPane = "%33"
			}
			p := Probes{Tmux: occTmux(tc.selfWin, tc.wins, tc.panes)}
			var out, errb bytes.Buffer
			rc := RunOccupants(context.Background(), []string{"/wt/a"}, &out, &errb, p, tmuxPane)
			if rc != 0 || errb.Len() != 0 || out.String() != tc.want+"\n" {
				t.Fatalf("rc %d stdout %q stderr %q, want 0 and %q", rc, out.String(), errb.String(), tc.want+"\n")
			}
		})
	}
}

func TestOccupantsNoTmuxPaneSkipsSelfLookup(t *testing.T) {
	tm := occTmux("@1", "@1\tw\t/x", "")
	got := Occupants(context.Background(), Probes{Tmux: tm}, "/x", "")
	if len(got) != 1 || got[0].Window != "@1" {
		t.Fatalf("got %+v", got)
	}
	if calls := tm.Calls(); len(calls) != 2 || strings.HasPrefix(calls[0], "display-message") {
		t.Fatalf("calls %q", calls)
	}
}

func equalOcc(a, b []Occupant) bool {
	return slices.EqualFunc(a, b, func(x, y Occupant) bool {
		return x.Window == y.Window && x.Name == y.Name && x.Pane == y.Pane &&
			x.Command == y.Command && x.Engine == y.Engine && slices.Equal(x.Panes, y.Panes)
	})
}

func TestOccupantsJSON(t *testing.T) {
	got := rowLine(occupantsJSON([]Occupant{
		{Window: "@1", Name: "w", Pane: "%2", Command: "claude", Engine: true, Panes: []PaneCmd{{"%2", "claude"}}},
		{Window: "@6", Name: "s"},
	}))
	sameJSON(t, got, `[
	  {"window":"@1","name":"w","pane":"%2","command":"claude","engine":true,"panes":[{"pane":"%2","command":"claude"}]},
	  {"window":"@6","name":"s","pane":null,"command":"","engine":false,"panes":[]}]`)
	if empty := rowLine(occupantsJSON(nil)); empty != "[]" {
		t.Fatalf("empty = %s", empty)
	}
}

func TestRunOccupants(t *testing.T) {
	p := Probes{Tmux: occTmux("", "@1\tw\t/x", "@1\t%1\tclaude")}

	var out, errb bytes.Buffer
	if rc := RunOccupants(context.Background(), nil, &out, &errb, p, ""); rc != 1 {
		t.Fatalf("rc = %d", rc)
	}
	if errb.String() != "crew: occupants <worktree-path>\n" || out.Len() != 0 {
		t.Fatalf("usage: stdout %q stderr %q", out.String(), errb.String())
	}

	out.Reset()
	errb.Reset()
	if rc := RunOccupants(context.Background(), []string{"/x"}, &out, &errb, p, ""); rc != 0 {
		t.Fatalf("rc = %d", rc)
	}
	want := `[{"window":"@1","name":"w","pane":"%1","command":"claude","engine":true,"panes":[{"pane":"%1","command":"claude"}]}]` + "\n"
	if !strings.HasSuffix(out.String(), "\n") {
		t.Fatalf("stdout %q", out.String())
	}
	sameJSON(t, strings.TrimSuffix(out.String(), "\n"), strings.TrimSuffix(want, "\n"))
	if strings.Count(out.String(), "\n") != 1 {
		t.Fatalf("stdout not one compact line: %q", out.String())
	}
}
