package entry

import (
	"bytes"
	"strings"
	"testing"
)

func runHelp(args ...string) (string, string, int) {
	var out, errOut bytes.Buffer
	code := RunHelp(args, &out, &errOut)
	return out.String(), errOut.String(), code
}

func TestHelpTopLevel(t *testing.T) {
	for _, args := range [][]string{nil, {""}, {"", ""}} {
		stdout, stderr, code := runHelp(args...)
		if code != 0 || stderr != "" {
			t.Fatalf("help %v = %d, stderr %q", args, code, stderr)
		}
		if !strings.HasPrefix(stdout, "crew — file-based coordination bus for dispatcher and worker sessions\n") {
			t.Fatalf("help %v printed %q", args, firstLine(stdout))
		}
		if !strings.HasSuffix(stdout, "engine-cmd      Whether a tmux pane command is an engine\n") {
			t.Fatalf("help %v does not end the grouped list the way crew.sh did", args)
		}
	}
}

// TestHelpEveryScreenIsReachable walks the embedded screens and checks each one
// prints to stdout, whole, with exit 0 — the shape `_crew_help` had for
// `crew <sub> --help`, which the script routes here as `help <sub>`.
func TestHelpEveryScreenIsReachable(t *testing.T) {
	for _, name := range screens() {
		if name == "top" || strings.HasPrefix(name, "hold/") {
			continue
		}
		stdout, stderr, code := runHelp(name)
		want, _ := screen(name)
		if code != 0 || stderr != "" {
			t.Fatalf("help %s = %d, stderr %q", name, code, stderr)
		}
		if stdout != want {
			t.Errorf("help %s printed %d bytes, want the %d of its screen", name, len(stdout), len(want))
		}
		if !strings.HasPrefix(stdout, "usage: crew "+name+"\n") && !strings.HasPrefix(stdout, "usage: crew "+name+" ") {
			t.Errorf("help %s printed %q", name, firstLine(stdout))
		}
	}
}

func TestHelpHoldActions(t *testing.T) {
	for _, action := range []string{"add", "list", "due", "park", "release"} {
		stdout, stderr, code := runHelp("hold", action)
		want, _ := screen("hold/" + action)
		if code != 0 || stderr != "" || stdout != want {
			t.Fatalf("help hold %s = %d, out %q, stderr %q", action, code, firstLine(stdout), stderr)
		}
		if !strings.HasPrefix(stdout, "usage: crew hold "+action) {
			t.Errorf("help hold %s printed %q", action, firstLine(stdout))
		}
	}

	// The bare `crew hold` screen is the function's `''` arm.
	stdout, stderr, code := runHelp("hold")
	if want, _ := screen("hold/default"); code != 0 || stderr != "" || stdout != want {
		t.Fatalf("help hold = %d, out %q, stderr %q", code, firstLine(stdout), stderr)
	}
}

func TestHelpUnknownSubcommand(t *testing.T) {
	top, _ := screen("top")
	stdout, stderr, code := runHelp("nosuchcmd")
	if code != 1 {
		t.Fatalf("help nosuchcmd exit %d, want 1", code)
	}
	if stdout != "" {
		t.Errorf("help nosuchcmd wrote %q to stdout, want nothing", firstLine(stdout))
	}
	if want := "crew: no help for nosuchcmd\n\n" + top; stderr != want {
		t.Errorf("help nosuchcmd stderr = %q, want the refusal, a blank line and the grouped list", firstLine(stderr))
	}
}

func TestHelpUnknownHoldAction(t *testing.T) {
	stdout, stderr, code := runHelp("hold", "bogus")
	if code != 1 {
		t.Fatalf("help hold bogus exit %d, want 1", code)
	}
	if stdout != "" {
		t.Errorf("help hold bogus wrote %q to stdout", stdout)
	}
	if stderr != "crew: no help for hold bogus\n" {
		t.Errorf("help hold bogus stderr = %q, want the one-line refusal", stderr)
	}
}

// TestHelpNameIsNoPath: the arm's `*)` case answered every name that was not a
// case label with the refusal, whatever it looked like — including one that
// names another screen's file.
func TestHelpNameIsNoPath(t *testing.T) {
	top, _ := screen("top")
	for _, name := range []string{"../texts/top", "..", ".", "top.txt", "TOP", "hold/add", "hold//add"} {
		stdout, stderr, code := runHelp(name)
		if code != 1 || stdout != "" {
			t.Errorf("help %q = %d with stdout %q, want 1 and none", name, code, firstLine(stdout))
		}
		if want := "crew: no help for " + name + "\n\n" + top; stderr != want {
			t.Errorf("help %q stderr = %q, want the refusal and the grouped list", name, firstLine(stderr))
		}
	}
}

// TestHelpIgnoresTrailingArgs: the function took two arguments and no more, so
// `crew --help status extra` answered for status alone.
func TestHelpIgnoresTrailingArgs(t *testing.T) {
	stdout, _, code := runHelp("status", "extra", "--help")
	want, _ := screen("status")
	if code != 0 || stdout != want {
		t.Fatalf("help status extra --help = %d, out %q", code, firstLine(stdout))
	}
}

// TestHelpInternalNamesAreNoSubcommand: top and default are file names this
// package invented — the grouped list, and bare `crew hold`’s screen, whose
// label in the script was the empty pattern. Neither name was ever a case label,
// so both stay refused even though both name a real screen.
func TestHelpInternalNamesAreNoSubcommand(t *testing.T) {
	top, _ := screen("top")
	for _, args := range [][]string{{"top"}, {"default"}} {
		stdout, stderr, code := runHelp(args...)
		if code != 1 || stdout != "" {
			t.Errorf("help %v = %d with stdout %q, want 1 and none", args, code, firstLine(stdout))
		}
		if want := "crew: no help for " + args[0] + "\n\n" + top; stderr != want {
			t.Errorf("help %v stderr = %q, want the refusal and the grouped list", args, firstLine(stderr))
		}
	}
	for _, args := range [][]string{{"hold", "default"}, {"hold", "top"}} {
		stdout, stderr, code := runHelp(args...)
		if code != 1 || stdout != "" {
			t.Errorf("help %v = %d with stdout %q, want 1 and none", args, code, firstLine(stdout))
		}
		if want := "crew: no help for hold " + args[1] + "\n"; stderr != want {
			t.Errorf("help %v stderr = %q, want the one-line refusal", args, stderr)
		}
	}
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}
