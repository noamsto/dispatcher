package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/remote"
)

const remoteDispatchRow = `{"kind":"dispatch","crew_id":"c1","session":"s1-1","seq":1,"ts":1}`

// newRemoteCLI is lifecycleCLI with one layout-2 crew, c1, whose log holds a
// dispatch row.
func newRemoteCLI(t *testing.T) *lifecycleCLI {
	t.Helper()
	c := newLifecycleCLI(t)
	if err := bus.CreateMeta(c.state, bus.Meta{ID: "c1", Created: 1700000000000, HomeHost: "halo", CreatedBy: "register"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(c.store("c1")+"/events.jsonl", []byte(remoteDispatchRow+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	unsetenv(t, "CREW_ID", "CREW_WORKER_ID", "CREW_ROLE_ID")
	return c
}

func TestDispatchRowOutsideRepoCLI(t *testing.T) {
	c := newRemoteCLI(t)
	out, _ := c.expectIn(c.outside, 0, "dispatch-row", "--crew", "c1", "--session", "s1-1")
	if strings.TrimSpace(out) != remoteDispatchRow {
		t.Fatalf("stdout %q, want the dispatch row", out)
	}
}

func TestOutsideRepoLegacyCrewRefusedCLI(t *testing.T) {
	c := newRemoteCLI(t)
	for _, args := range [][]string{
		{"dispatch-row", "--crew", "legacy", "--session", "s1-1"},
		{"dispatch-row", "--session", "s1-1"},
		{"relay", "--crew", "legacy", "--since", "0"},
		{"relay", "--since", "0"},
		{"remote-run", "--host", "mini", "--env", "CREW_ID=c1", "--", "crew", "where", "x"},
	} {
		_, errOut := c.expectIn(c.outside, 1, args...)
		if errOut != "crew: not in a git repo\n" {
			t.Fatalf("%v: stderr %q", args, errOut)
		}
	}
}

// oneLineWriter is a stdout whose first write succeeds and every later one fails,
// which ends relay.
type oneLineWriter struct {
	buf bytes.Buffer
	n   int
}

func (w *oneLineWriter) Write(p []byte) (int, error) {
	if w.n++; w.n > 1 {
		return 0, errors.New("closed")
	}
	return w.buf.Write(p)
}

func TestRelayOutsideRepoCLI(t *testing.T) {
	c := newRemoteCLI(t)
	e := env{getwd: func() (string, error) { return c.outside, nil }, proc: c.proc}
	var out oneLineWriter
	var stderr bytes.Buffer
	code := run(context.Background(), []string{"relay", "--crew", "c1", "--since", "0", "--heartbeat", "1", "--interval", "1"}, &out, &stderr, e)
	if code != 0 {
		t.Fatalf("code %d, stderr %q", code, stderr.String())
	}
	if !strings.HasPrefix(out.buf.String(), "{") || !strings.HasSuffix(out.buf.String(), "\n") {
		t.Fatalf("stdout %q, want one JSON line", out.buf.String())
	}
}

func TestRemoteRunCrewScopeOutsideRepoCLI(t *testing.T) {
	c := newRemoteCLI(t)
	bash, err := exec.LookPath("bash")
	if err != nil {
		bash = "/bin/sh"
	}
	dir := t.TempDir()
	stdin := filepath.Join(dir, "stdin")
	script := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("#!"+bash+"\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	script("ssh", "cat > '"+stdin+"'\necho remote-out\n")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DISPATCH_CONFIG_BIN", script("dispatch-config", `echo '{"hosts":{"mini":{"ssh":"mini.lan"}}}'`+"\n"))

	argv := []string{"crew", "dispatch-row", "--crew", "c1", "--session", "s1-1"}
	kvs := []string{"CREW_ID=c1", "DISPATCH_ORIGIN_HOST=halo"}
	args := []string{"remote-run", "--host", "mini", "--crew-scope", "--env", kvs[0], "--env", kvs[1], "--"}
	out, errOut := c.expectIn(c.outside, 0, append(args, argv...)...)
	if out != "remote-out\n" {
		t.Fatalf("stdout %q, stderr %q", out, errOut)
	}
	want, err := remote.BuildCrewRequest(argv, kvs)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(stdin)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("ssh stdin %q, want %q", got, want)
	}

	noScope := append([]string{"remote-run", "--host", "mini", "--env", kvs[0], "--env", kvs[1], "--"}, argv...)
	_, errOut = c.expectIn(c.outside, 1, noScope...)
	if errOut != "crew: not in a git repo\n" {
		t.Fatalf("without --crew-scope: stderr %q", errOut)
	}
}
