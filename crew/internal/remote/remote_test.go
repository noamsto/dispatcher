package remote

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/bus"
)

// shebang is the interpreter line for a stub script: `#!/usr/bin/env bash`
// does not resolve inside the nix sandbox.
func shebang() string {
	if b, err := exec.LookPath("bash"); err == nil {
		return "#!" + b + "\n"
	}
	return "#!/bin/sh\n"
}

func writeScript(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(shebang()+body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// configBin is a fake dispatch-config printing settings.
func configBin(t *testing.T, settings string) string {
	t.Helper()
	data := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(data, []byte(settings), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "dispatch-config")
	writeScript(t, bin, "cat '"+data+"'\n")
	return bin
}

// cloneOf is a fresh checkout whose origin is url; it returns the worktree.
func cloneOf(t *testing.T, url string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "repo")
	for _, args := range [][]string{
		{"init", "-q", "-b", "main", dir},
		{"-C", dir, "remote", "add", "origin", url},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return real
}

const mini = `{"engines":["claude"],"hosts":{"mini":{"ssh":"me@mini","repos":{"Acme/Widget":"/r/widget"}}}}`

func TestLoad(t *testing.T) {
	hosts, err := Load(context.Background(), configBin(t, mini), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]Host{"mini": {SSH: "me@mini", Repos: map[string]string{"Acme/Widget": "/r/widget"}}}
	if !reflect.DeepEqual(hosts, want) {
		t.Fatalf("hosts = %#v, want %#v", hosts, want)
	}
}

func TestLoadPeer(t *testing.T) {
	hosts, err := Load(context.Background(), configBin(t, `{"hosts":{"mini":{"ssh":"me@mini","peer":true,"repos":{}}}}`), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !hosts["mini"].Peer {
		t.Fatalf("hosts = %#v, want mini.peer true", hosts)
	}
}

func TestLoadNoHosts(t *testing.T) {
	hosts, err := Load(context.Background(), configBin(t, `{"engines":["claude"]}`), io.Discard)
	if err != nil || len(hosts) != 0 {
		t.Fatalf("hosts = %v, err = %v", hosts, err)
	}
}

func TestLoadRevalidates(t *testing.T) {
	for name, settings := range map[string]string{
		"ssh option":    `{"hosts":{"mini":{"ssh":"-oProxyCommand=x","repos":{}}}}`,
		"ssh space":     `{"hosts":{"mini":{"ssh":"me mini","repos":{}}}}`,
		"host name":     `{"hosts":{"-x":{"ssh":"me@mini","repos":{}}}}`,
		"repo key":      `{"hosts":{"mini":{"ssh":"me@mini","repos":{"widget":"/r/w"}}}}`,
		"relative path": `{"hosts":{"mini":{"ssh":"me@mini","repos":{"a/b":"r/w"}}}}`,
		"control char":  `{"hosts":{"mini":{"ssh":"me@mini","repos":{"a/b":"/r/w\n"}}}}`,
		"not object":    `{"hosts":["mini"]}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(context.Background(), configBin(t, settings), io.Discard); err == nil || !strings.Contains(err.Error(), "hosts") {
				t.Fatalf("err = %v, want a hosts refusal", err)
			}
		})
	}
}

func TestLoadConfigFails(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "dispatch-config")
	writeScript(t, bin, "echo boom >&2; exit 3\n")
	var stderr bytes.Buffer
	if _, err := Load(context.Background(), bin, &stderr); err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(stderr.String(), "boom") {
		t.Fatalf("dispatch-config stderr not passed through: %q", stderr.String())
	}
}

func TestLoadWithoutConfigBinIsNotFound(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := Load(context.Background(), "dispatch-config", io.Discard)
	if !errors.Is(err, exec.ErrNotFound) || !strings.HasPrefix(err.Error(), "could not resolve the dispatcher settings (dispatch-config: ") {
		t.Fatalf("err = %v, want exec.ErrNotFound without a command prefix", err)
	}
}

func TestRunSettingsFailure(t *testing.T) {
	repo := cloneOf(t, "https://github.com/acme/widget")
	bin := filepath.Join(t.TempDir(), "dispatch-config")
	writeScript(t, bin, "exit 3\n")
	t.Setenv("DISPATCH_CONFIG_BIN", bin)
	var stderr bytes.Buffer
	Run(context.Background(), Call{Host: "mini", Cwd: repo, Argv: []string{"crew", "where", "x"}}, io.Discard, &stderr)
	if want := "crew: remote-run: could not resolve the dispatcher settings (" + bin + ": exit status 3)\n"; stderr.String() != want {
		t.Fatalf("stderr = %q, want %q", stderr.String(), want)
	}
}

func TestResolve(t *testing.T) {
	hosts := map[string]Host{"mini": {SSH: "me@mini", Repos: map[string]string{"Acme/Widget": "/r/widget"}}}
	got, err := Resolve(hosts, "mini", "acme/widget")
	if err != nil || got != (Target{SSH: "me@mini", Repo: "/r/widget"}) {
		t.Fatalf("Resolve = %+v, %v", got, err)
	}
	if _, err := Resolve(hosts, "maxi", "acme/widget"); err == nil ||
		err.Error() != "crew: remote-run: unknown host 'maxi' (no hosts.maxi in settings)" {
		t.Fatalf("unknown host err = %v", err)
	}
	if _, err := Resolve(hosts, "mini", "acme/other"); err == nil ||
		err.Error() != "crew: remote-run: host 'mini' has no clone mapped for acme/other (settings hosts.mini.repos)" {
		t.Fatalf("no mapping err = %v", err)
	}
}

func TestSSHArgv(t *testing.T) {
	want := []string{
		"-T", "-o", "BatchMode=yes", "-o", "ConnectTimeout=15", "-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=3", "-o", "ForwardAgent=no", "-o", "ForwardX11=no",
		"-o", "ClearAllForwardings=yes", "-o", "PermitLocalCommand=no", "--", "me@mini", "crew", "remote-exec",
	}
	if got := SSHArgv("me@mini"); !reflect.DeepEqual(got, want) {
		t.Fatalf("SSHArgv = %q", got)
	}
}

func TestBuildRequest(t *testing.T) {
	data, err := BuildRequest("/r/widget", "acme/widget", []string{"crew", "where", "x"}, []string{"CREW_ID=c1"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"v":1,"repo":"/r/widget","slug":"acme/widget","argv":["crew","where","x"],"env":{"CREW_ID":"c1"}}` {
		t.Fatalf("request = %s", data)
	}
	spec := "line $(x)\n"
	data, err = BuildRequest("/r/widget", "acme/widget", []string{"dispatch"}, nil, &spec)
	if err != nil {
		t.Fatal(err)
	}
	var req Request
	if err := json.Unmarshal(data, &req); err != nil || req.Spec == nil || *req.Spec != spec || req.Env == nil {
		t.Fatalf("request = %s (%v)", data, err)
	}
}

func TestBuildRequestRefusesEnv(t *testing.T) {
	for _, env := range []string{
		"LD_PRELOAD=/x.so",
		"CREW_WORKER_ID=worker:x",
		"CREW_ID=a;b",
		"CREW_ID=",
		"DISPATCH_TMUX_SESSION=other",
		"DISPATCH_TMUX_SESSION=crew-a b",
		"DISPATCH_EMIT_ROW=2",
		"DISPATCH_ORIGIN_HOST=a b",
		"DISPATCH_ORIGIN_HOST=a;b",
		"DISPATCH_ORIGIN_HOST=-halo",
		"DISPATCH_SESSION_ID=s1",
		"DISPATCH_SESSION_ID=s1-2\n",
		"CREW_IDENTITY_AVOID=a,,b",
		"CREW_IDENTITY_AVOID=a b",
		"NOEQUALS",
	} {
		if _, err := BuildRequest("/r", "a/b", []string{"dispatch"}, []string{env}, nil); err == nil {
			t.Errorf("env %q accepted", env)
		}
	}
	ok := []string{
		"CREW_ID=c_1-x", "DISPATCH_TMUX_SESSION=crew-c_1", "DISPATCH_EMIT_ROW=1",
		"DISPATCH_SESSION_ID=s170-42", "CREW_IDENTITY_AVOID=ada,bo-b", "DISPATCH_ORIGIN_HOST=halo",
	}
	if _, err := BuildRequest("/r", "a/b", []string{"dispatch"}, ok, nil); err != nil {
		t.Fatalf("valid env refused: %v", err)
	}
}

func TestBuildRequestNeedsArgv(t *testing.T) {
	if _, err := BuildRequest("/r", "a/b", nil, nil, nil); err == nil {
		t.Fatal("empty argv accepted")
	}
}

// busRows reads lines as a bus log.
func busRows(t *testing.T, lines ...string) []bus.Event {
	t.Helper()
	log := filepath.Join(t.TempDir(), "events.jsonl")
	if err := os.WriteFile(log, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	evs, err := bus.ReadEvents(log)
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

func TestRoute(t *testing.T) {
	rows := busRows(t,
		`{"kind":"dispatch","crew_id":"c","branch":"feat/a","host":"mini","remote":true}`,
		`{"kind":"dispatch","crew_id":"c","branch":"feat/b","host":"mini","remote":true}`,
		`{"kind":"dispatch","crew_id":"c","branch":"feat/b","host":"laptop"}`,
		`{"kind":"dispatch","crew_id":"d","branch":"feat/a","host":"maxi","remote":true}`,
		`{"kind":"status","crew_id":"c","branch":"feat/a","host":"other","remote":true}`,
		`{"kind":"dispatch","crew_id":"c","branch":"feat/c","host":"mini","remote":"true"}`,
	)
	for _, tc := range []struct{ branch, crew, want string }{
		{"feat/a", "c", "mini"},
		{"feat/b", "c", ""}, // the newest row is local
		{"feat/a", "d", "maxi"},
		{"feat/c", "c", ""}, // remote must be the boolean true
		{"feat/z", "c", ""},
	} {
		if got := Route(rows, tc.branch, tc.crew); got != tc.want {
			t.Errorf("Route(%s, %s) = %q, want %q", tc.branch, tc.crew, got, tc.want)
		}
	}
}

func TestRouteAny(t *testing.T) {
	rows := busRows(t,
		`{"kind":"dispatch","crew_id":"c","branch":"feat/a","host":"mini","remote":true}`,
		`{"kind":"dispatch","crew_id":"c","branch":"feat/b","host":"mini","remote":true}`,
		`{"kind":"dispatch","crew_id":"c","branch":"feat/b","host":"laptop"}`,
		`{"kind":"dispatch","crew_id":"c","branch":"feat/d","host":"laptop"}`,
		`{"kind":"dispatch","crew_id":"d","branch":"feat/d","host":"maxi","remote":true}`,
		`{"kind":"dispatch","crew_id":"c","branch":"feat/l"}`,
		`{"kind":"dispatch","crew_id":"d","branch":"feat/l"}`,
	)
	for _, tc := range []struct{ branch, host, crew, err string }{
		{branch: "feat/a", host: "mini", crew: "c"},
		{branch: "feat/b"}, // the newest row is local
		{branch: "feat/d", err: "feat/d is dispatched in crews c, d, one of them on a remote host — pass --crew <id>"},
		{branch: "feat/l"}, // local in two crews: the local path's own cross-crew lookup decides
		{branch: "feat/z"},
	} {
		host, crew, err := RouteAny(rows, tc.branch)
		if host != tc.host || crew != tc.crew || fmt.Sprint(err) != cmp.Or(tc.err, "<nil>") {
			t.Errorf("RouteAny(%s) = %q, %q, %v; want %q, %q, %s", tc.branch, host, crew, err, tc.host, tc.crew, tc.err)
		}
	}
}

func TestRemoteHosts(t *testing.T) {
	rows := busRows(t,
		`{"kind":"dispatch","crew_id":"c","branch":"feat/a","host":"mini","remote":true}`,
		`{"kind":"dispatch","crew_id":"c","branch":"feat/b","host":"big","remote":true}`,
		`{"kind":"dispatch","crew_id":"c","branch":"feat/c","host":"mini","remote":true}`,
		`{"kind":"dispatch","crew_id":"c","branch":"feat/d","host":"laptop"}`,
		`{"kind":"dispatch","crew_id":"d","branch":"feat/e","host":"other","remote":true}`,
	)
	if got := RemoteHosts(rows, "c"); !reflect.DeepEqual(got, []string{"big", "mini"}) {
		t.Fatalf("RemoteHosts = %q", got)
	}
	if got := RemoteHosts(rows, "z"); len(got) != 0 {
		t.Fatalf("RemoteHosts(z) = %q", got)
	}
}

// sshShim puts a fake ssh first on PATH: it records its argv and stdin under
// dir, prints to both streams, and exits with code.
func sshShim(t *testing.T, code string) string {
	t.Helper()
	dir := t.TempDir()
	writeScript(t, filepath.Join(dir, "ssh"), `printf '%s\0' "$@" > '`+dir+`/argv'
cat > '`+dir+`/stdin'
echo remote-out
echo remote-err >&2
exit `+code+"\n")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

func TestRun(t *testing.T) {
	repo := cloneOf(t, "git@github.com:acme/widget.git")
	t.Setenv("DISPATCH_CONFIG_BIN", configBin(t, mini))
	shim := sshShim(t, "7")
	spec := filepath.Join(repo, "spec.md")
	if err := os.WriteFile(spec, []byte("do $(it)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	rc := Run(context.Background(), Call{
		Host: "mini", Cwd: repo, Env: []string{"CREW_ID=c1"}, SpecFile: "spec.md",
		Argv: []string{"dispatch", "--title", "a;b"},
	}, &stdout, &stderr)
	if rc != 7 {
		t.Fatalf("rc = %d, stderr %q", rc, stderr.String())
	}
	if stdout.String() != "remote-out\n" || stderr.String() != "remote-err\n" {
		t.Fatalf("stdout %q stderr %q", stdout.String(), stderr.String())
	}
	argv, _ := os.ReadFile(filepath.Join(shim, "argv"))
	if want := strings.Join(SSHArgv("me@mini"), "\x00") + "\x00"; string(argv) != want {
		t.Fatalf("ssh argv = %q", argv)
	}
	stdin, _ := os.ReadFile(filepath.Join(shim, "stdin"))
	if want := `{"v":1,"repo":"/r/widget","slug":"acme/widget","argv":["dispatch","--title","a;b"],"env":{"CREW_ID":"c1"},"spec":"do $(it)\n"}`; string(stdin) != want {
		t.Fatalf("request = %s", stdin)
	}
}

func TestRunSSHFailure(t *testing.T) {
	repo := cloneOf(t, "https://github.com/acme/widget")
	t.Setenv("DISPATCH_CONFIG_BIN", configBin(t, mini))
	sshShim(t, "255")
	var stderr bytes.Buffer
	if rc := Run(context.Background(), Call{Host: "mini", Cwd: repo, Argv: []string{"crew", "where", "x"}}, io.Discard, &stderr); rc != 255 {
		t.Fatalf("rc = %d", rc)
	}
	if !strings.HasSuffix(stderr.String(), "crew: remote-run: ssh to host 'mini' (me@mini) failed (exit 255)\n") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestRunRefusesBeforeSSH(t *testing.T) {
	repo := cloneOf(t, "https://github.com/acme/other.git")
	widget := cloneOf(t, "https://github.com/acme/widget.git")
	t.Setenv("DISPATCH_CONFIG_BIN", configBin(t, mini))
	shim := sshShim(t, "0")
	for _, tc := range []struct {
		call Call
		want string
	}{
		{Call{Host: "maxi", Cwd: repo, Argv: []string{"crew", "where", "x"}}, "crew: remote-run: unknown host 'maxi' (no hosts.maxi in settings)\n"},
		{Call{Host: "maxi", Cwd: t.TempDir(), Argv: []string{"crew", "where", "x"}}, "crew: remote-run: unknown host 'maxi' (no hosts.maxi in settings)\n"},
		{Call{Host: "mini", Cwd: repo, Argv: []string{"crew", "where", "x"}}, "crew: remote-run: host 'mini' has no clone mapped for acme/other (settings hosts.mini.repos)\n"},
		{Call{Host: "mini", Cwd: widget, Env: []string{"LD_PRELOAD=x"}, Argv: []string{"dispatch"}}, "crew: remote-run: --env key LD_PRELOAD is not allowed\n"},
	} {
		var stderr bytes.Buffer
		if rc := Run(context.Background(), tc.call, io.Discard, &stderr); rc != 2 || stderr.String() != tc.want {
			t.Errorf("Run(%+v) = %d, stderr %q, want %q", tc.call, rc, stderr.String(), tc.want)
		}
	}
	if _, err := os.Stat(filepath.Join(shim, "argv")); err == nil {
		t.Fatal("ssh ran despite a refusal")
	}
}

const chromeRefusal = "crew: remote-run: --chrome is dispatcher-only — a worker or role pane cannot grant a session the owner's browser\n"

func TestRunChromeTripwire(t *testing.T) {
	repo := cloneOf(t, "git@github.com:acme/widget.git")
	t.Setenv("DISPATCH_CONFIG_BIN", configBin(t, mini))
	chrome := []string{"dispatch", "standard", "opus", "--effort", "medium", "--chrome", "--", "t"}
	title := []string{"dispatch", "standard", "opus", "--", "fix the", "--chrome", "flag"}
	for _, tc := range []struct {
		name, worker, role, pane, branch string
		argv                             []string
		refused                          bool
	}{
		{name: "worker", worker: "worker:feat/x#s1-1", argv: chrome, refused: true},
		{name: "role", role: "role:reviewer", argv: chrome, refused: true},
		{name: "pane", pane: "%1", branch: "feat/1-x", argv: chrome, refused: true},
		{name: "dispatcher", argv: chrome},
		{name: "pane without branch", pane: "%1", argv: chrome},
		{name: "title only", worker: "worker:feat/x#s1-1", argv: title},
		{name: "not dispatch", worker: "worker:feat/x#s1-1", argv: []string{"crew", "reply", "--chrome"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CREW_WORKER_ID", tc.worker)
			t.Setenv("CREW_ROLE_ID", tc.role)
			t.Setenv("TMUX_PANE", tc.pane)
			old := paneBranch
			paneBranch = func(_ context.Context, pane string) string {
				if pane != "%1" {
					t.Errorf("pane = %q", pane)
				}
				return tc.branch
			}
			t.Cleanup(func() { paneBranch = old })
			shim := sshShim(t, "0")
			var stderr bytes.Buffer
			rc := Run(context.Background(), Call{Host: "mini", Cwd: repo, Argv: tc.argv}, io.Discard, &stderr)
			_, sshErr := os.Stat(filepath.Join(shim, "argv"))
			if tc.refused {
				if rc != 2 || stderr.String() != chromeRefusal || sshErr == nil {
					t.Fatalf("rc = %d, stderr %q, ssh ran %v", rc, stderr.String(), sshErr == nil)
				}
				return
			}
			if rc != 0 || sshErr != nil {
				t.Fatalf("rc = %d, stderr %q, ssh err %v", rc, stderr.String(), sshErr)
			}
		})
	}
}
