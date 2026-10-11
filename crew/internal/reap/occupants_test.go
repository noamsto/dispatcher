package reap

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/jsonv"
)

// occTmux answers the three reads `_occupants` makes and records every argv.
func occTmux(self, wins, panes string, calls *[]string) func(context.Context, ...string) string {
	return func(_ context.Context, args ...string) string {
		*calls = append(*calls, strings.Join(args, " "))
		switch args[0] {
		case "display-message":
			return self
		case "list-windows":
			return wins
		case "list-panes":
			return panes
		}
		return ""
	}
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
	var calls []string
	p := Probes{Tmux: occTmux("@5", wins, panes, &calls)}

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
	if !slices.Equal(calls, wantCalls) {
		t.Fatalf("tmux calls:\n got %q\nwant %q", calls, wantCalls)
	}
}

func TestOccupantsNoTmuxPaneSkipsSelfLookup(t *testing.T) {
	var calls []string
	p := Probes{Tmux: occTmux("@1", "@1\tw\t/x", "", &calls)}
	got := Occupants(context.Background(), p, "/x", "")
	if len(got) != 1 || got[0].Window != "@1" {
		t.Fatalf("got %+v", got)
	}
	if len(calls) != 2 || strings.HasPrefix(calls[0], "display-message") {
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
	got := OccupantsJSON([]Occupant{
		{Window: "@1", Name: "w", Pane: "%2", Command: "claude", Engine: true, Panes: []PaneCmd{{"%2", "claude"}}},
		{Window: "@6", Name: "s"},
	})
	want, err := jsonv.ParseOne(`[
	  {"window":"@1","name":"w","pane":"%2","command":"claude","engine":true,"panes":[{"pane":"%2","command":"claude"}]},
	  {"window":"@6","name":"s","pane":null,"command":"","engine":false,"panes":[]}]`)
	if err != nil {
		t.Fatal(err)
	}
	if g, w := string(jsonv.Append(nil, got, jsonv.Options{})), string(jsonv.Append(nil, want, jsonv.Options{})); g != w {
		t.Fatalf("json:\n got %s\nwant %s", g, w)
	}
	if empty := string(jsonv.Append(nil, OccupantsJSON(nil), jsonv.Options{})); empty != "[]" {
		t.Fatalf("empty = %s", empty)
	}
}

func TestRunOccupants(t *testing.T) {
	var calls []string
	p := Probes{Tmux: occTmux("", "@1\tw\t/x", "@1\t%1\tclaude", &calls)}

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
	got, err := jsonv.ParseOne(strings.TrimSuffix(out.String(), "\n"))
	if err != nil || !strings.HasSuffix(out.String(), "\n") {
		t.Fatalf("stdout %q (%v)", out.String(), err)
	}
	wantV, _ := jsonv.ParseOne(strings.TrimSuffix(want, "\n"))
	if string(jsonv.Append(nil, got, jsonv.Options{})) != string(jsonv.Append(nil, wantV, jsonv.Options{})) {
		t.Fatalf("stdout %q", out.String())
	}
	if strings.Count(out.String(), "\n") != 1 {
		t.Fatalf("stdout not one compact line: %q", out.String())
	}
}

func TestProcLineSkipsUnrepresentableCwd(t *testing.T) {
	cases := []struct{ cwd, want string }{
		{"/wt/feat-x", "42\t/wt/feat-x\n"},
		{"", ""},
		{"/wt/evil\n1\t/wt/feat-x", ""},
		{"/wt/a\tb", ""},
	}
	for _, c := range cases {
		if got := procLine("42", c.cwd); got != c.want {
			t.Errorf("procLine(42, %q) = %q, want %q", c.cwd, got, c.want)
		}
	}
}
