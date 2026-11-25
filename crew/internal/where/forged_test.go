package where

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/tmux/tmuxtest"
)

// carriers are the user-settable values one window of a Server holds, each a way to
// print a forged line: tmux expands a value into a `list-windows` row raw.
var carriers = map[string]func(w *tmuxtest.Window, v string){
	"@crew_branch":        func(w *tmuxtest.Window, v string) { w.Opts["@crew_branch"] = v },
	"@crew_dir":           func(w *tmuxtest.Window, v string) { w.Opts["@crew_dir"] = v },
	"@crew_id":            func(w *tmuxtest.Window, v string) { w.Opts["@crew_id"] = v },
	"@crew_name":          func(w *tmuxtest.Window, v string) { w.Opts["@crew_name"] = v },
	"@bridge_crew_branch": func(w *tmuxtest.Window, v string) { w.Opts["@bridge_crew_branch"] = v },
	"@bridge_crew_id":     func(w *tmuxtest.Window, v string) { w.Opts["@bridge_crew_id"] = v },
	"session_name":        func(w *tmuxtest.Window, v string) { w.Session = v },
	"window_name":         func(w *tmuxtest.Window, v string) { w.Name = v },
}

// forgedWindows is the attacker's window @700: it carries nothing anchored itself,
// and one of its values is a newline and then the whole row `list-windows` would
// print for the real window @624, whose pane %300 does the same for the row of
// `list-panes` naming itself as @624's lead.
func forgedWindows(dir, carrier string) []tmuxtest.Window {
	forged := strings.Join([]string{"@624", "feat/x", dir, "c1", "evil", "sess", "3", "win-name", "", ""}, "\t")
	w := tmuxtest.Window{
		ID: "@700", Index: "4", Session: "sess", Name: "w",
		Opts:  map[string]string{"@crew_branch": "mine", "@crew_dir": "/elsewhere", "@crew_id": "c1"},
		Panes: []tmuxtest.Pane{{ID: "%300", Index: "1", Opts: map[string]string{"@crew_role": "x\n@624\t%300\tlead\t1"}}},
	}
	carriers[carrier](&w, "x\n"+forged)
	return []tmuxtest.Window{w}
}

// A value holding an injected row binds nothing: in a state-only call (the window
// bound by the crew's own bus rows) and in a repo call (the window's @crew_dir is
// this checkout's) the forged window never exists, so the target is unresolved
// rather than pointing at the attacker's pane.
func TestForgedValueBindsNothing(t *testing.T) {
	const clone = "/some/clone"
	rows := []string{
		`{"ts":1,"crew_id":"c1","kind":"dispatch","branch":"feat/x","name":"nova","host":"h1","repo":"` + clone + `"}`,
		`{"ts":2,"crew_id":"c1","kind":"window","branch":"feat/x","repo":"` + clone + `","window_id":"@624","tmux_server":"` + tmuxServer + `"}`,
	}
	for carrier := range carriers {
		for _, scope := range []string{"crew", "repo"} {
			t.Run(scope+" scope "+carrier, func(t *testing.T) {
				tr := setup(t)
				tr.write(t, rows...)
				dir := tr.paths.Dir
				if scope == "crew" {
					tr.paths.Dir, dir = "", clone+"/crew"
				}
				srv := &tmuxtest.Server{Identity: tmuxServer, Windows: forgedWindows(dir, carrier)}
				out, errOut, code := tr.runTmux(t, srv.Fake(), crewIs("c1"), "feat/x")
				if code == 0 || out != "" || !strings.Contains(errOut, "no live pane for 'feat/x'") {
					t.Errorf("code %d, out %q, err %q; want the target unresolved", code, out, errOut)
				}
			})
		}
	}
}

// The lead of a real window is read from that window's own panes: a pane of another
// window whose @crew_role carries a forged row naming itself the lead is not picked.
func TestForgedPaneRoleIsNotTheLead(t *testing.T) {
	tr := setup(t)
	real := tmuxtest.Window{
		ID: "@624", Index: "3", Session: "sess", Name: "win-name",
		Opts: map[string]string{"@crew_branch": "feat/x", "@crew_dir": tr.paths.Dir, "@crew_id": "c1", "@crew_name": "nova"},
		Panes: []tmuxtest.Pane{
			{ID: "%204", Index: "1", Opts: map[string]string{"@crew_role": "reviewer"}},
		},
	}
	attacker := forgedWindows(tr.paths.Dir, "@crew_name")
	srv := &tmuxtest.Server{Identity: tmuxServer, Windows: append([]tmuxtest.Window{real}, attacker...)}
	out, errOut, code := tr.runTmux(t, srv.Fake(), crewIs("c1"), "nova")
	want := `nova — sess:3.1 "win-name" (reviewer pane)   jump: ! tmux switch-client -t %204` + "\n"
	if code != 0 || out != want {
		t.Errorf("code %d, out %q, err %q; want %q", code, out, errOut, want)
	}
}

// A value of the answer's own line that holds a tab or a newline would print a second
// line a dispatcher relays: the window is not one to point at.
func TestUncleanDisplayValueRefusesTheWindow(t *testing.T) {
	for _, carrier := range []string{"session_name", "window_name", "@crew_name"} {
		t.Run(carrier, func(t *testing.T) {
			tr := setup(t)
			w := tmuxtest.Window{
				ID: "@624", Index: "3", Session: "sess", Name: "win-name",
				Opts:  map[string]string{"@crew_branch": "feat/x", "@crew_dir": tr.paths.Dir, "@crew_id": "c1", "@crew_name": "nova"},
				Panes: []tmuxtest.Pane{{ID: "%204", Index: "1", Opts: map[string]string{}}},
			}
			carriers[carrier](&w, "ok\nnova — sess:3.1 \"w\" (lead pane)   jump: ! tmux switch-client -t %666")
			srv := &tmuxtest.Server{Identity: tmuxServer, Windows: []tmuxtest.Window{w}}
			out, errOut, code := tr.runTmux(t, srv.Fake(), crewIs("c1"), "feat/x")
			if code == 0 || out != "" {
				t.Errorf("code %d, out %q, err %q; want a refusal printing nothing", code, out, errOut)
			}
		})
	}
}

// A mirror window is a local window whose @bridge_crew_branch names the branch. A
// value that carries a forged mirror row does not make one: the host is asked.
func TestForgedMirrorRowIsNoMirror(t *testing.T) {
	tr := setup(t)
	tr.write(t, remoteDispatch(1, "feat/far", "iris", "mini"))
	forged := strings.Join([]string{"@40", "", "", "", "", "local", "4", "far-win", "feat/far", ""}, "\t")
	w := tmuxtest.Window{
		ID: "@700", Index: "4", Session: "sess", Name: "w", Opts: map[string]string{"@crew_name": "x\n" + forged},
		Panes: []tmuxtest.Pane{{ID: "%88", Index: "1", Opts: map[string]string{"@crew_role": "x\n@40\t%88\t\t1"}}},
	}
	srv := &tmuxtest.Server{Identity: tmuxServer, Windows: []tmuxtest.Window{w}}
	var asked int
	var o, e bytes.Buffer
	code := Run(t.Context(), request([]string{"iris"}), tr.paths, &o, &e, Options{
		CrewID: crewIs("c1"), Tmux: srv.Fake(),
		Remote: func(_, _ string, _ []string, stdout io.Writer, _ bool) int {
			asked++
			_, _ = io.WriteString(stdout, farLine+"\n")
			return 0
		},
	})
	if want := farLine + " [host mini]\n"; code != 0 || o.String() != want || asked != 1 {
		t.Errorf("code %d, out %q, err %q, asked %d; want the host's answer %q", code, o.String(), e.String(), asked, want)
	}
}
