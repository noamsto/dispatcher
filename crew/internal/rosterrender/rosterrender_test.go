package rosterrender

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/identity"
)

func TestInstalledCrewSkipsStorePaths(t *testing.T) {
	// The rows of the bats row this port moved: a box with crew installed keeps it
	// under /nix/store, so no daemon row can reach the skip without it being faked.
	dir := t.TempDir()
	noexec := filepath.Join(dir, "noexec")
	real := filepath.Join(dir, "real")
	dircrew := filepath.Join(dir, "dircrew")
	link := filepath.Join(dir, "link")
	for _, d := range []string{noexec, real, dircrew, link} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	body := []byte("#!/usr/bin/env bash\n")
	for _, d := range []string{noexec, real} {
		if err := os.WriteFile(filepath.Join(d, "crew"), body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(filepath.Join(real, "crew"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A directory named crew: `-f` refused it in the arm, `-x` would have accepted it.
	if err := os.MkdirAll(filepath.Join(dircrew, "crew"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(real, "crew"), filepath.Join(link, "crew")); err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(filepath.Join(real, "crew"))
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct{ name, path, want string }{
		{"store dir skipped", "/nix/store/fake-crew/bin:" + real, want},
		{"bare store dir skipped", "/nix/store:" + real, want},
		{"non-executable, then a directory, then the real one", noexec + ":" + dircrew + ":" + real, want},
		{"symlinked entry resolves", link, want},
		{"nothing executable on the path", noexec + ":" + dircrew, ""},
		{"only a store dir", "/nix/store/fake-crew/bin", ""},
		// A relative entry would resolve against the bus dir the daemon runs from.
		{"relative entry refused", ".", ""},
		{"empty PATH", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := InstalledCrew(tc.path); got != tc.want {
				t.Errorf("InstalledCrew(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

func TestTargetNamesEachCrewPerRepo(t *testing.T) {
	// The arm's `%(%m-%d-%H%M)T` of the crew id's epoch prefix, in the renderer's
	// zone: 1791360000 is 08:00 UTC on 10-07.
	fixed := time.Date(2026, 12, 25, 23, 59, 0, 0, time.UTC)
	for _, tc := range []struct{ name, common, crew, want string }{
		{"epoch prefix becomes a local time", "/work/myrepo/.git", "1791360000-abc",
			"/r/roster-myrepo-10-07-0800-" + hex("/work/myrepo/.git|1791360000-abc") + ".d2"},
		{"leading zero falls back to the crew id", "/work/myrepo/.git", "01791360000-abc",
			"/r/roster-myrepo-01791360000-abc-" + hex("/work/myrepo/.git|01791360000-abc") + ".d2"},
		{"13 digits is no epoch", "/work/myrepo/.git", "1791360000123-x",
			"/r/roster-myrepo-1791360000123-x-" + hex("/work/myrepo/.git|1791360000123-x") + ".d2"},
		{"a non-numeric id stands in for the time", "/work/myrepo/.git", "c1",
			"/r/roster-myrepo-c1-" + hex("/work/myrepo/.git|c1") + ".d2"},
		// Two repos sharing a directory name, and two crews of one repo, are separate.
		{"repo is the dir that owns the bus", "/other/myrepo/.git", "c1",
			"/r/roster-myrepo-c1-" + hex("/other/myrepo/.git|c1") + ".d2"},
		{"a space or slash in the id cannot steer the path", "/work/myrepo/.git", "../evil id",
			"/r/roster-myrepo-.._evil_id-" + hex("/work/myrepo/.git|../evil id") + ".d2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := target(tc.common, tc.crew, "/r", func() time.Time { return fixed })
			if got != tc.want {
				t.Errorf("target =\n %s\nwant\n %s", got, tc.want)
			}
		})
	}
}

// hex is the diagram name's 16-bit checksum, spelled the way the arm spelled it:
// `%04x` of `cksum % 65536`.
func hex(s string) string {
	return fmt.Sprintf("%04x", identity.CKsum([]byte(s))%65536)
}

func TestParseIsTheArmInItsOrder(t *testing.T) {
	for _, tc := range []struct {
		name  string
		args  []string
		msg   string
		code  int
		check func(*testing.T, call)
	}{
		{name: "crew alone is a full call", args: []string{"--crew", "c1"},
			check: func(t *testing.T, c call) {
				if c.crew != "c1" || c.interval != 2 || c.quiet != 1800 {
					t.Errorf("defaults drifted: %+v", c)
				}
				if c.intervalText != "2" || c.quietText != "1800" {
					t.Errorf("the daemon must be handed the caller's bytes: %q %q", c.intervalText, c.quietText)
				}
			}},
		{name: "the texts keep their spelling", args: []string{"--crew", "c1", "--interval", "02", "--quiet", "0"},
			check: func(t *testing.T, c call) {
				if c.interval != 2 || c.quiet != 0 {
					t.Errorf("values: %+v", c)
				}
			}},
		{name: "missing value", args: []string{"--crew"}, msg: "crew: --crew needs a value", code: exitUsage},
		{name: "empty value", args: []string{"--pane", ""}, msg: "crew: --pane needs a value", code: exitUsage},
		{name: "unknown arg", args: []string{"--nope"}, msg: "crew: roster-render: unknown arg '--nope'", code: exitUsage},
		// An unknown argument outranks a bad interval, as it does in the arm's loop.
		{name: "unknown beats interval", args: []string{"--nope", "--interval", "x"},
			msg: "crew: roster-render: unknown arg '--nope'", code: exitUsage},
		{name: "interval zero", args: []string{"--crew", "c1", "--interval", "0"}, msg: errInterval, code: exitUsage},
		{name: "interval negative", args: []string{"--crew", "c1", "--interval", "-1"}, msg: errInterval, code: exitUsage},
		{name: "interval not a number", args: []string{"--crew", "c1", "--interval", "2s"}, msg: errInterval, code: exitUsage},
		// bash's `[ "$v" -gt 0 ]` errors on a digit string it cannot read, so the
		// arm refused it with the same message; clamping it would be a daemon that
		// never wakes.
		{name: "interval overflows", args: []string{"--crew", "c1", "--interval", "99999999999999999999"},
			msg: errInterval, code: exitUsage},
		{name: "quiet overflows", args: []string{"--crew", "c1", "--quiet", "99999999999999999999"},
			msg: errQuiet, code: exitUsage},
		{name: "quiet negative", args: []string{"--crew", "c1", "--quiet", "-1"}, msg: errQuiet, code: exitUsage},
		{name: "no crew", args: []string{"--interval", "3"}, msg: errCrewRequired, code: exitUsage},
		{name: "once and detach", args: []string{"--crew", "c1", "--once", "--detach"}, msg: errOnceDetach, code: exitUsage},
		// The crew id check is the arm's last, after once/detach.
		{name: "bad crew id", args: []string{"--crew", "a/b", "--once", "--detach"}, msg: errOnceDetach, code: exitUsage},
		{name: "bad crew id alone", args: []string{"--crew", "a/b"}, msg: errCrewID, code: exitUsage},
		{name: "leading dash", args: []string{"--crew", "-x"}, msg: errCrewID, code: exitUsage},
		{name: "dot", args: []string{"--crew", "."}, msg: errCrewID, code: exitUsage},
		{name: "no-open", args: []string{"--crew", "c1", "--no-open"},
			check: func(t *testing.T, c call) {
				if !c.noOpen {
					t.Error("--no-open not recorded")
				}
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, msg, code := parse(tc.args)
			if tc.msg != "" {
				if msg != tc.msg || code != tc.code {
					t.Errorf("got (%q, %d), want (%q, %d)", msg, code, tc.msg, tc.code)
				}
				return
			}
			if msg != "" {
				t.Fatalf("unexpected refusal %q", msg)
			}
			if code != exitOK {
				t.Fatalf("code = %d", code)
			}
			tc.check(t, c)
		})
	}
}

func TestPutRefusesAnythingButARegularFile(t *testing.T) {
	dir := t.TempDir()
	t.Run("writes the content and nothing else", func(t *testing.T) {
		f := filepath.Join(dir, "diagram.d2")
		if err := put(f, "a: \"b\"\n", nilWriter{}); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != "a: \"b\"\n" {
			t.Errorf("wrote %q", got)
		}
	})
	t.Run("symlink", func(t *testing.T) {
		target := filepath.Join(dir, "elsewhere")
		if err := os.WriteFile(target, []byte("keep"), 0o644); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(dir, "link.d2")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		var msg strings.Builder
		if err := put(link, "slop\n", &msg); err == nil {
			t.Fatal("a symlinked target was accepted")
		}
		if got, _ := os.ReadFile(target); string(got) != "keep" {
			t.Errorf("the symlink's file was overwritten: %q", got)
		}
		if !strings.Contains(msg.String(), "not a regular file") {
			t.Errorf("no refusal line: %q", msg.String())
		}
	})
	t.Run("directory", func(t *testing.T) {
		d := filepath.Join(dir, "dir.d2")
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := put(d, "x\n", nilWriter{}); err == nil {
			t.Fatal("a directory was accepted as a target")
		}
	})
	t.Run("leaves no temp behind", func(t *testing.T) {
		before := countTemps(t, dir)
		if err := put(filepath.Join(dir, "ok.d2"), "x\n", nilWriter{}); err != nil {
			t.Fatal(err)
		}
		if after := countTemps(t, dir); after != before {
			t.Errorf("temp files left behind: %d before, %d after", before, after)
		}
	})
}

func countTemps(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".roster-render.") {
			n++
		}
	}
	return n
}

type nilWriter struct{}

func (nilWriter) Write(p []byte) (int, error) { return len(p), nil }

func TestRoleRowsAreThisCrewsLiveRolePanes(t *testing.T) {
	// The `-F` row order: dir, crew, branch, role, state, exited.
	panes := strings.Join([]string{
		"/bus/crew\tc1\tfeat/1-a\tspec-critic\tidle\t",
		"/bus/crew\tc1\tfeat/1-a\tlead\tworking\t",
		"/bus/crew\tc2\tfeat/1-a\tother-crew-role\tidle\t",
		"/other/crew\tc1\tfeat/2-b\tplan-critic\tworking\t",
		"/bus/crew\tc1\tfeat/3-c\treviewer\tidle\texited",
		"/bus/crew\tc1\t\trole-no-branch\tidle\t",
		"/bus/crew\tc1\tfeat/4-d\t\tidle\t",
		"short-row",
		"",
	}, "\n") + "\n"
	got := roleRows(panes, "/bus/crew", "c1")
	want := []string{"feat/1-a\tspec-critic\tidle"}
	if len(got) != len(want) || got[0] != want[0] {
		t.Errorf("roleRows = %q, want %q", got, want)
	}
}

func TestSanitizeIsTrsByteSet(t *testing.T) {
	// `tr -c 'A-Za-z0-9._-' '_'` is byte-wise: a multi-byte rune becomes one
	// underscore per byte, not per rune — `/` and the space one each, `é` two.
	if got := sanitize("aB9._-/ é"); got != "aB9._-____" {
		t.Errorf("sanitize = %q", got)
	}
}

func TestPanePIDMatchesIsTheArmsShapeCheck(t *testing.T) {
	for _, tc := range []struct{ pane, pid string }{
		{"%12", "400"},
		{"%0", "1"},
	} {
		if !panePIDMatches(tc.pane, tc.pid) {
			t.Errorf("%q/%q should match", tc.pane, tc.pid)
		}
	}
	for _, tc := range []struct{ pane, pid string }{
		{"0.1", "400"},
		{"%1a", "400"},
		{"%1", " 4a "},
		{"%1", ""},
		{"%1", " "},
	} {
		if panePIDMatches(tc.pane, tc.pid) {
			t.Errorf("%q/%q should not match", tc.pane, tc.pid)
		}
	}
}
