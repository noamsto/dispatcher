package nudge

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/tmux/tmuxtest"
)

// forgedServer is a tmux server whose only window is an attacker's: it carries no
// anchored stamps of its own, and one of its options holds a newline and then the
// whole row `list-windows` would print for window @7 of feat/x, while its pane's
// @crew_role does the same for the `list-panes` row naming the attacker's own pane
// %9 as @7's lead. Tmux expands a value raw, so a positional parse of those lists
// reads both rows as real.
func forgedServer(dir, option string) *tmuxtest.Server {
	forgedWindow := "@7\tfeat/x\t" + dir + "\tc1\tevil"
	forgedPane := "@7\t%9\tlead\tclaude"
	opts := map[string]string{"@crew_branch": "mine", "@crew_dir": "/elsewhere", "@crew_id": "c1", "@crew_name": "mine"}
	opts[option] = "x\n" + forgedWindow
	return &tmuxtest.Server{Identity: tmuxServer, Windows: []tmuxtest.Window{{
		ID: "@9", Index: "1", Opts: opts,
		Panes: []tmuxtest.Pane{{ID: "%9", Index: "1", Command: "claude", Opts: map[string]string{"@crew_role": "x\n" + forgedPane}}},
	}}}
}

// A stamp holding an injected row binds nothing, in the crew-scope arm (state only,
// the window bound by the crew's own bus rows) and the repo-scope arm (the window's
// @crew_dir is this checkout's): the forged window never exists, so no pane is typed.
func TestForgedStampBindsNothing(t *testing.T) {
	const clone = "/some/clone"
	for _, option := range []string{"@crew_branch", "@crew_dir", "@crew_id", "@crew_name"} {
		for _, scope := range []string{"crew", "repo"} {
			t.Run(scope+" scope "+option, func(t *testing.T) {
				f := newArm(t)
				dir := f.paths.Dir
				if scope == "crew" {
					var store string
					f.paths, store = pinCrew(t, f.paths, "c1")
					f.paths.Dir = ""
					dir = clone + "/crew"
					rows := `{"ts":1,"crew_id":"c1","kind":"dispatch","branch":"feat/x","repo":"` + clone + `"}` + "\n" +
						`{"ts":2,"crew_id":"c1","kind":"window","branch":"feat/x","repo":"` + clone + `","window_id":"@7","tmux_server":"` + tmuxServer + `"}` + "\n"
					if err := os.WriteFile(filepath.Join(store, "events.jsonl"), []byte(rows), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				f.pane.srv = forgedServer(dir, option)
				// "no live pane" / "no worker matches": the target never resolved
				// to a window, rather than failing at a later gate.
				if rc := f.run(Request{Target: featX}); rc == 0 || !strings.HasPrefix(f.errOut(), "no ") {
					t.Fatalf("rc = %d, stderr %q, want the target unresolved", rc, f.errOut())
				}
				if got := keys(f.pane); len(got) != 0 {
					t.Errorf("typed into a forged pane: %q", got)
				}
			})
		}
	}
}

// A pane whose @crew_role carries a forged `list-panes` row for itself, ahead of its
// real one, is not the lead of the window the forged row names.
func TestForgedPaneRoleDoesNotAnchor(t *testing.T) {
	srv := &tmuxtest.Server{Identity: tmuxServer, Windows: []tmuxtest.Window{
		{
			ID: "@9", Index: "1", Opts: map[string]string{},
			Panes: []tmuxtest.Pane{
				{ID: "%8", Index: "1", Command: "zsh", Opts: map[string]string{"@crew_role": "x\n@7\t%9\tlead\tclaude"}},
				{ID: "%9", Index: "2", Command: "claude", Opts: map[string]string{}},
			},
		},
		{
			ID: "@7", Index: "2", Opts: map[string]string{"@crew_branch": "feat/x", "@crew_id": "c1"},
			Panes: []tmuxtest.Pane{{ID: "%7", Index: "1", Command: "claude", Opts: map[string]string{"@crew_role": "lead"}}},
		},
	}}
	p := &fakePane{frames: acceptedFrames(t), srv: srv}
	b := &fakeBus{rows: sessionRows(t, idleReq, "working")}
	out, rc := Pane(t.Context(), req("claude"), newDeps(p, b, nil))
	if rc != exitRefused || !strings.HasPrefix(out, "anchor: window @9 of %9 is not anchored to crew c1") {
		t.Errorf("Pane = (%q, %d), want the anchor refusal", out, rc)
	}
	if got := keys(p); len(got) != 0 {
		t.Errorf("typed into a forged pane: %q", got)
	}
}

// A value holding a tab or a newline is refused wherever it is read, even when no
// forged row is in it: the pane's own role and command match nothing.
func TestUncleanPaneValuesAreNoLead(t *testing.T) {
	for name, pane := range map[string]tmuxtest.Pane{
		"role":    {ID: "%9", Index: "1", Command: "claude", Opts: map[string]string{"@crew_role": "lead\t"}},
		"command": {ID: "%9", Index: "1", Command: "claude\nzsh", Opts: map[string]string{"@crew_role": "lead"}},
	} {
		t.Run(name, func(t *testing.T) {
			srv := &tmuxtest.Server{Identity: tmuxServer, Windows: []tmuxtest.Window{{
				ID: "@7", Index: "1", Opts: map[string]string{"@crew_branch": "feat/x", "@crew_id": "c1"},
				Panes: []tmuxtest.Pane{pane},
			}}}
			p := &fakePane{frames: acceptedFrames(t), srv: srv}
			b := &fakeBus{rows: sessionRows(t, idleReq, "working")}
			out, rc := Pane(t.Context(), req("claude"), newDeps(p, b, nil))
			if rc != exitRefused || !strings.HasPrefix(out, "anchor:") {
				t.Errorf("Pane = (%q, %d), want an anchor refusal", out, rc)
			}
			if got := keys(p); len(got) != 0 {
				t.Errorf("typed anyway: %q", got)
			}
		})
	}
}

// The lead pane of a real, bound window is read from that window's own panes: a
// pane of another window whose @crew_role carries a forged row naming itself as the
// lead is never picked, so nothing is typed into it.
func TestForgedPaneRoleIsNotTheLead(t *testing.T) {
	f := newArm(t)
	f.pane.srv = &tmuxtest.Server{Identity: tmuxServer, Windows: []tmuxtest.Window{
		{
			ID: "@9", Index: "1", Opts: map[string]string{},
			Panes: []tmuxtest.Pane{
				{ID: "%8", Index: "1", Command: "zsh", Opts: map[string]string{"@crew_role": "x\n@7\t%9\tlead\tclaude"}},
				{ID: leadID, Index: "2", Command: "claude", Opts: map[string]string{}},
			},
		},
		{
			ID: "@7", Index: "2",
			Opts:  map[string]string{"@crew_branch": featX, "@crew_dir": f.paths.Dir, "@crew_id": "c1", "@crew_name": "reef"},
			Panes: []tmuxtest.Pane{{ID: "%7", Index: "1", Command: "claude", Opts: map[string]string{"@crew_role": "reviewer"}}},
		},
	}}
	// The real window's own pane %7 is the lead the arm picks: the capture fixture
	// answers only for %9, so the gate reads %7 as unreadable and nothing is typed.
	if rc := f.run(Request{Target: featX}); rc != exitRefused || f.errOut() != "refused — pane unreadable" {
		t.Errorf("rc = %d, stderr %q, want the real window's pane refused as unreadable", rc, f.errOut())
	}
	if got := keys(f.pane); len(got) != 0 {
		t.Errorf("typed into the forged lead: %q", got)
	}
}

// The call count is bounded: an accepted nudge of one window of one pane reads the two
// lists, the window's four stamps, the pane's role for the lead pick, and the anchor
// gate's two lists, two window stamps, role and command.
func TestNudgeStampReadsAreBounded(t *testing.T) {
	f := newArm(t)
	f.run(Request{Target: featX})
	reads := 0
	for _, c := range f.pane.tmux {
		if strings.HasPrefix(c, "list-") || strings.HasPrefix(c, "show-options") || strings.HasPrefix(c, "display-message") {
			reads++
		}
	}
	if want := 2 + 4 + 1 + 2 + 2 + 1 + 1; reads != want {
		t.Errorf("%d tmux reads, want %d:\n%s", reads, want, strings.Join(f.pane.tmux, "\n"))
	}
}

// A tmux call that fails after the lists were read is a failure to read, not a worker
// whose window is gone.
func TestFailedStampReadIsNotGone(t *testing.T) {
	f := newArm(t)
	f.pane.failShow = true
	if rc := f.run(Request{Target: featX}); rc == 0 || !strings.Contains(f.errOut(), "cannot read tmux window stamps") {
		t.Errorf("rc = %d, stderr %q, want a read failure", rc, f.errOut())
	}
	if got := keys(f.pane); len(got) != 0 {
		t.Errorf("typed after a failed read: %q", got)
	}
}
