package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/bus"
)

// lifecycleCLI is crewsCLI with its own state root and a directory no repo
// owns, for the repo-optional lifecycle commands.
type lifecycleCLI struct {
	*crewsCLI
	state   string // the crew state root: $XDG_STATE_HOME/crew
	outside string
}

func newLifecycleCLI(t *testing.T) *lifecycleCLI {
	t.Helper()
	c := newCrewsCLI(t)
	xdg := t.TempDir()
	t.Setenv("XDG_STATE_HOME", xdg)
	unsetenv(t, "CREW_LAYOUT")
	outside, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &lifecycleCLI{crewsCLI: c, state: filepath.Join(xdg, "crew"), outside: outside}
}

// runIn is run with dir as the working directory.
func (c *lifecycleCLI) runIn(dir string, args ...string) (string, string, int) {
	c.t.Helper()
	e := env{getwd: func() (string, error) { return dir, nil }, proc: c.proc}
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), args, &stdout, &stderr, e)
	return stdout.String(), stderr.String(), code
}

func (c *lifecycleCLI) expectIn(dir string, want int, args ...string) (string, string) {
	c.t.Helper()
	out, errOut, code := c.runIn(dir, args...)
	if code != want {
		c.t.Fatalf("%v in %s: code %d, want %d (stdout %q, stderr %q)", args, dir, code, want, out, errOut)
	}
	return out, errOut
}

func (c *lifecycleCLI) store(id string) string { return bus.StoreDir(c.state, id) }

func (c *lifecycleCLI) meta(id string) map[string]any {
	c.t.Helper()
	data, err := os.ReadFile(c.store(id) + "/meta.json")
	if err != nil {
		c.t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		c.t.Fatal(err)
	}
	return m
}

func (c *lifecycleCLI) archive(id string) {
	c.t.Helper()
	if err := os.MkdirAll(bus.ArchiveDir(c.state, id), 0o755); err != nil {
		c.t.Fatal(err)
	}
	if err := os.WriteFile(bus.ArchiveDir(c.state, id)+"/meta.json", []byte(`{"layout":2,"id":"`+id+`"}`), 0o644); err != nil {
		c.t.Fatal(err)
	}
}

func (c *lifecycleCLI) noTmux() {
	c.t.Helper()
	if got := stubLog(c.t, os.Getenv("TMUX_LOG")); got != "" {
		c.t.Errorf("tmux was called: %s", got)
	}
}

func missing(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Errorf("%s exists (%v)", path, err)
	}
}

func TestRegisterLayout2CLI(t *testing.T) {
	t.Run("in a repo", func(t *testing.T) {
		c := newLifecycleCLI(t)
		c.as("c2")
		t.Setenv("CREW_LAYOUT", "2")
		c.mustRun("register", "4242")
		m := c.meta("c2")
		if m["home_repo"] != filepath.Join(c.repo, ".git") || m["created_by"] != "register" || m["visiting"] != false {
			t.Errorf("meta = %v", m)
		}
		if host, _ := m["home_host"].(string); host == "" || strings.Contains(host, ".") {
			t.Errorf("home_host = %q, want the short hostname", host)
		}
		eq(t, "pid", strings.TrimSpace(readText(t, c.store("c2")+"/pid")), "4242")
		missing(t, c.crewDir("c2"))
		missing(t, filepath.Join(c.busDir(), "homes", "c2"))
	})
	t.Run("outside a repo", func(t *testing.T) {
		c := newLifecycleCLI(t)
		c.as("c2")
		_, errOut := c.expectIn(c.outside, 1, "register", "4242")
		eq(t, "layout 1 stderr", errOut, "crew: not in a git repo\n")
		missing(t, c.store("c2"))

		t.Setenv("CREW_LAYOUT", "2")
		c.expectIn(c.outside, 0, "register", "4242")
		if m := c.meta("c2"); m["home_repo"] != nil {
			t.Errorf("home_repo = %v, want null", m["home_repo"])
		}
		eq(t, "pid", strings.TrimSpace(readText(t, c.store("c2")+"/pid")), "4242")

		c.expectIn(c.outside, 0, "deregister")
		missing(t, c.store("c2")+"/pid")
		readText(t, c.store("c2")+"/meta.json")
		readText(t, c.store("c2")+"/pidfile.log")
	})
	t.Run("refusals", func(t *testing.T) {
		c := newLifecycleCLI(t)
		c.as("c2")
		t.Setenv("CREW_LAYOUT", "7")
		_, errOut := c.expect(1, "register", "4242")
		eq(t, "invalid value", errOut, "crew: register: CREW_LAYOUT must be 1 or 2 (got '7')\n")

		t.Setenv("CREW_LAYOUT", "2")
		c.archive("c2")
		_, errOut = c.expect(1, "register", "4242")
		eq(t, "archived", errOut, "crew: crew c2 is archived\n")
		missing(t, c.store("c2"))
		missing(t, c.crewDir("c2"))

		unsetenv(t, "XDG_STATE_HOME", "HOME")
		_, errOut = c.expectIn(c.outside, 1, "register", "4242")
		eq(t, "no state root", errOut, "crew: register: CREW_LAYOUT=2 needs XDG_STATE_HOME or HOME\n")
	})
}

func TestCrewsLayout2CLI(t *testing.T) {
	c := newLifecycleCLI(t)
	t.Setenv("CREW_LAYOUT", "2")
	c.as("c-here")
	c.mustRun("register", "4242")
	c.as("c-null")
	c.expectIn(c.outside, 0, "register", "4242")

	out := c.mustRun("crews")
	wantRow(t, out, "c-here")
	lacks(t, "repo table", out, "c-null")
	eq(t, "home", c.mustRun("crews", "--home"), crewsTableHeader+strings.Join(crewsRow(out, "c-here"), "\t")+"\n")

	out, _ = c.expectIn(c.outside, 0, "crews")
	wantRow(t, out, "c-null")
	lacks(t, "outside table", out, "c-here")

	unsetenv(t, "XDG_STATE_HOME", "HOME")
	_, errOut := c.expectIn(c.outside, 1, "crews")
	eq(t, "no state root outside a repo", errOut, "crew: not in a git repo\n")
}

func TestWhereLogDirCLI(t *testing.T) {
	c := newLifecycleCLI(t)
	c.as("")
	t.Setenv("CREW_LAYOUT", "2")
	c.as("c2")
	c.mustRun("register", "4242")
	c.as("")
	c.archive("c-arch")

	for _, tc := range []struct {
		name   string
		dir    string
		args   []string
		code   int
		stdout string
		stderr string
	}{
		{"layout 1 log", c.repo, []string{"where", "--log", "--crew", "c1"}, 0, filepath.Join(c.busDir(), "events.jsonl") + "\n", ""},
		{"layout 1 dir", c.repo, []string{"where", "--dir", "--crew", "c1"}, 0, c.crewDir("c1") + "\n", ""},
		{"layout 2 log", c.repo, []string{"where", "--log", "--crew", "c2"}, 0, c.store("c2") + "/events.jsonl\n", ""},
		{"layout 2 dir", c.repo, []string{"where", "--dir", "--crew=c2"}, 0, c.store("c2") + "\n", ""},
		{"layout 2 outside a repo", c.outside, []string{"where", "--log", "--crew", "c2"}, 0, c.store("c2") + "/events.jsonl\n", ""},
		{"layout 1 outside a repo", c.outside, []string{"where", "--dir", "--crew", "c1"}, 1, "", "crew: not in a git repo\n"},
		{"archived", c.repo, []string{"where", "--log", "--crew", "c-arch"}, 3, "", "crew: crew c-arch is archived\n"},
		{"archived outside a repo", c.outside, []string{"where", "--dir", "--crew", "c-arch"}, 3, "", "crew: crew c-arch is archived\n"},
		{"no crew", c.repo, []string{"where", "--log"}, 1, "", "crew: no crew context — pass --crew <id>\n"},
		{"invalid id", c.repo, []string{"where", "--log", "--crew", "../x"}, 1, "", "crew: invalid crew id \"../x\"\n"},
		{"both modes", c.repo, []string{"where", "--log", "--dir"}, 1, "", "crew: where: --log and --dir take no target (" + whereUsage + ")\n"},
		{"mode with a target", c.outside, []string{"where", "--log", "nova"}, 1, "", "crew: where: --log and --dir take no target (" + whereUsage + ")\n"},
		{"target mode outside a repo", c.outside, []string{"where", "nova"}, 1, "", "crew: not in a git repo\n"},
	} {
		out, errOut, code := c.runIn(tc.dir, tc.args...)
		if code != tc.code || out != tc.stdout || errOut != tc.stderr {
			t.Errorf("%s: code %d stdout %q stderr %q, want %d %q %q", tc.name, code, out, errOut, tc.code, tc.stdout, tc.stderr)
		}
	}
	c.noTmux()

	t.Setenv("CREW_ID", "c2")
	out, _ := c.expectIn(c.outside, 0, "where", "--dir")
	eq(t, "dir from CREW_ID", out, c.store("c2")+"\n")
}

func TestIndexCLI(t *testing.T) {
	c := newLifecycleCLI(t)
	t.Setenv("CREW_LAYOUT", "2")
	c.as("c2")
	c.mustRun("register", "4242")
	c.as("")
	c.archive("c-arch")
	indexFile := bus.IndexPath(c.state, filepath.Join(c.repo, ".git"))

	c.expect(0, "index", "add", "--crew", "c1")
	missing(t, filepath.Join(c.state, "index"))

	c.expect(0, "index", "add", "--crew", "c2")
	c.expect(0, "index", "add", "--crew", "c2")
	got := readText(t, indexFile)
	lines := strings.Split(strings.TrimSuffix(got, "\n"), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], `{"v":1,"common":"`+filepath.Join(c.repo, ".git")+`"`) ||
		!strings.HasPrefix(lines[1], `{"crew":"c2","ts":`) {
		t.Errorf("index file = %q", got)
	}

	_, errOut := c.expect(3, "index", "add", "--crew", "c-arch")
	eq(t, "archived", errOut, "crew: crew c-arch is archived\n")
	_, errOut = c.expectIn(c.outside, 1, "index", "add", "--crew", "c2")
	eq(t, "outside a repo", errOut, "crew: not in a git repo\n")
	_, errOut = c.expect(1, "index", "add")
	eq(t, "no crew", errOut, "crew: index: --crew needs an id ("+indexUsage+")\n")
	_, errOut = c.expect(1, "index")
	eq(t, "bare", errOut, indexUsage+"\n")

	// rebuild keeps only what c2's own log backs: it never dispatched, so its
	// entry goes.
	c.expectIn(c.outside, 0, "index", "rebuild")
	missing(t, indexFile)

	unsetenv(t, "XDG_STATE_HOME", "HOME")
	_, errOut = c.expectIn(c.outside, 1, "index", "rebuild")
	eq(t, "no state root", errOut, "crew: index: neither XDG_STATE_HOME nor HOME is set\n")

	out, _ := c.expectIn(c.outside, 0, "index", "rebuild", "--help")
	if !strings.HasPrefix(out, "usage: crew index ") {
		t.Errorf("index rebuild --help = %q", out)
	}
}

func readText(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestRegisterVisitingCLI(t *testing.T) {
	c := newLifecycleCLI(t)
	c.as("c3")
	c.mustRun("register", "--visiting", "halo")
	m := c.meta("c3")
	if m["home_host"] != "halo" || m["visiting"] != true || m["created_by"] != "remote-dispatch" || m["home_repo"] != nil {
		t.Errorf("meta = %v", m)
	}
	missing(t, c.store("c3")+"/pid")

	_, errOut := c.expect(1, "register", "--visiting")
	if !strings.HasPrefix(errOut, "crew: register: --visiting needs an origin host") {
		t.Errorf("bare --visiting stderr = %q", errOut)
	}
	_, errOut = c.expect(1, "register", "--visiting", "halo", "4242")
	if !strings.HasPrefix(errOut, "crew: register: --visiting takes no pid") {
		t.Errorf("pid stderr = %q", errOut)
	}
}
