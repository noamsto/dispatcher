package remote

import (
	"bytes"
	"cmp"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
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
	data, err := BuildRequest("/r/widget", "acme/widget", []string{"crew", "where", "x"}, []string{"CREW_ID=c1"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"v":1,"repo":"/r/widget","slug":"acme/widget","argv":["crew","where","x"],"env":{"CREW_ID":"c1"}}` {
		t.Fatalf("request = %s", data)
	}
	spec := "line $(x)\n"
	data, err = BuildRequest("/r/widget", "acme/widget", []string{"dispatch"}, nil, &spec, nil)
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
		"DISPATCH_TMUX_SESSION=-x",
		"DISPATCH_TMUX_SESSION=a:b",
		"DISPATCH_TMUX_SESSION=a.b",
		"DISPATCH_TMUX_SESSION=a;b",
		"DISPATCH_TMUX_SESSION=a\nb",
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
		if _, err := BuildRequest("/r", "a/b", []string{"dispatch"}, []string{env}, nil, nil); err == nil {
			t.Errorf("env %q accepted", env)
		}
	}
	ok := []string{
		"CREW_ID=c_1-x", "DISPATCH_TMUX_SESSION=crew-c_1", "DISPATCH_TMUX_SESSION=dispatcher", "DISPATCH_EMIT_ROW=1",
		"DISPATCH_SESSION_ID=s170-42", "CREW_IDENTITY_AVOID=ada,bo-b", "DISPATCH_ORIGIN_HOST=halo",
	}
	if _, err := BuildRequest("/r", "a/b", []string{"dispatch"}, ok, nil, nil); err != nil {
		t.Fatalf("valid env refused: %v", err)
	}
}

func TestBuildRequestNeedsArgv(t *testing.T) {
	if _, err := BuildRequest("/r", "a/b", nil, nil, nil, nil); err == nil {
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

// sshShim puts a fake ssh first on PATH: it records its argv and stdin under
// dir, prints to both streams, and exits with code.
func sshShim(t *testing.T, code string) string {
	t.Helper()
	return sshShimStderr(t, code, "remote-err")
}

// sshShimStderr is sshShim writing errLine (no single quotes) to stderr.
func sshShimStderr(t *testing.T, code, errLine string) string {
	t.Helper()
	dir := t.TempDir()
	writeScript(t, filepath.Join(dir, "ssh"), `printf '%s\0' "$@" > '`+dir+`/argv'
cat > '`+dir+`/stdin'
echo remote-out
printf '%s\n' '`+errLine+`' >&2
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

const (
	testTip     = "0123456789abcdef0123456789abcdef01234567"
	testSession = "0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0"
)

func testMeta() map[string]any {
	return map[string]any{
		"v": 1, "branch": "feat/x", "tip": testTip, "from_host": "laptop",
		"mode": "coop", "session": testSession, "handoff": "carry on $(x)\n",
	}
}

func writeBundle(t *testing.T, meta map[string]any, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	if meta != nil {
		data, err := json.Marshal(meta)
		if err != nil {
			t.Fatal(err)
		}
		files["meta.json"] = string(data)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func gunzipB64(t *testing.T, s string) string {
	t.Helper()
	gz, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// runMigrate runs a dispatch call carrying dir and returns the request ssh got.
func runMigrate(t *testing.T, repo, dir string) (int, string, string) {
	t.Helper()
	shim := sshShim(t, "0")
	var stderr bytes.Buffer
	rc := Run(context.Background(), Call{
		Host: "mini", Cwd: repo, MigrateDir: dir, Argv: []string{"dispatch", "standard", "opus", "--", "t"},
	}, io.Discard, &stderr)
	stdin, _ := os.ReadFile(filepath.Join(shim, "stdin"))
	return rc, string(stdin), stderr.String()
}

func TestRunMigrateDir(t *testing.T) {
	repo := cloneOf(t, "git@github.com:acme/widget.git")
	t.Setenv("DISPATCH_CONFIG_BIN", configBin(t, mini))
	patch, transcript := "diff --git a/x b/x\n", `{"type":"user"}`+"\n"
	dir := writeBundle(t, testMeta(), map[string]string{"patch": patch, "transcript.jsonl": transcript})
	rc, sent, stderr := runMigrate(t, repo, dir)
	if rc != 0 {
		t.Fatalf("rc %d, stderr %q", rc, stderr)
	}
	var req struct {
		Argv    []string                   `json:"argv"`
		Migrate map[string]json.RawMessage `json:"migrate"`
	}
	if err := json.Unmarshal([]byte(sent), &req); err != nil {
		t.Fatalf("request %s: %v", sent, err)
	}
	if want := `{"v":1,"branch":"feat/x","tip":"` + testTip + `","from_host":"laptop","mode":"coop","session":"` + testSession + `","handoff":"carry on $(x)\n"}`; string(req.Migrate["meta"]) != want {
		t.Fatalf("meta = %s, want %s", req.Migrate["meta"], want)
	}
	var patchGz, transcriptGz string
	if err := json.Unmarshal(req.Migrate["patch_gz"], &patchGz); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(req.Migrate["transcript_gz"], &transcriptGz); err != nil {
		t.Fatal(err)
	}
	if gunzipB64(t, patchGz) != patch || gunzipB64(t, transcriptGz) != transcript {
		t.Fatalf("blobs do not round-trip: %s", sent)
	}

	meta := testMeta()
	delete(meta, "session")
	rc, sent, stderr = runMigrate(t, repo, writeBundle(t, meta, map[string]string{}))
	if rc != 0 {
		t.Fatalf("meta only: rc %d, stderr %q", rc, stderr)
	}
	if strings.Contains(sent, "patch_gz") || strings.Contains(sent, "transcript_gz") || !strings.Contains(sent, `"migrate":{"meta":{`) {
		t.Fatalf("meta only request = %s", sent)
	}
}

func TestRunMigrateDirRefusals(t *testing.T) {
	repo := cloneOf(t, "git@github.com:acme/widget.git")
	t.Setenv("DISPATCH_CONFIG_BIN", configBin(t, mini))
	badMode := testMeta()
	badMode["mode"] = "later"
	for _, tc := range []struct {
		name string
		dir  string
		want string
	}{
		{"no meta.json", writeBundle(t, nil, map[string]string{"patch": "p"}), "crew: remote-run: --migrate-dir: "},
		{"bad meta", writeBundle(t, badMode, map[string]string{"transcript.jsonl": "{}\n"}), "mode"},
		{"session without transcript", writeBundle(t, testMeta(), map[string]string{}), "session"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rc, sent, stderr := runMigrate(t, repo, tc.dir)
			if rc != 2 || sent != "" || !strings.Contains(stderr, tc.want) {
				t.Fatalf("rc %d, sent %q, stderr %q, want rc 2 naming %q", rc, sent, stderr, tc.want)
			}
		})
	}
	_, _, stderr := runMigrate(t, repo, writeBundle(t, nil, map[string]string{}))
	if !strings.Contains(stderr, "meta.json") {
		t.Fatalf("missing meta.json refusal %q does not name it", stderr)
	}
}

// A bundle file that is a symlink (or anything but a regular file) is refused
// before ssh: the bundle must not carry what a link points at.
func TestRunMigrateDirRefusesNonRegular(t *testing.T) {
	repo := cloneOf(t, "git@github.com:acme/widget.git")
	t.Setenv("DISPATCH_CONFIG_BIN", configBin(t, mini))
	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	meta := testMeta()
	delete(meta, "session")
	for _, name := range []string{"meta.json", "patch", "transcript.jsonl"} {
		t.Run(name, func(t *testing.T) {
			var dir string
			if name == "meta.json" {
				dir = writeBundle(t, nil, map[string]string{})
			} else {
				dir = writeBundle(t, meta, map[string]string{})
			}
			if err := os.Symlink(secret, filepath.Join(dir, name)); err != nil {
				t.Fatal(err)
			}
			rc, sent, stderr := runMigrate(t, repo, dir)
			if want := filepath.Join(dir, name) + " is not a regular file"; rc != 2 || sent != "" || !strings.Contains(stderr, want) {
				t.Fatalf("rc %d, sent %q, stderr %q, want rc 2 naming %q", rc, sent, stderr, want)
			}
		})
	}
	dir := writeBundle(t, meta, map[string]string{})
	if err := os.Mkdir(filepath.Join(dir, "patch"), 0o700); err != nil {
		t.Fatal(err)
	}
	if rc, sent, stderr := runMigrate(t, repo, dir); rc != 2 || sent != "" || !strings.Contains(stderr, "patch is not a regular file") {
		t.Fatalf("dir patch: rc %d, sent %q, stderr %q", rc, sent, stderr)
	}
}

func TestBuildHostRequest(t *testing.T) {
	data, err := BuildHostRequest([]string{"crew", "overseer", "status", "--origin", "halo"})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"v":1,"scope":"host","repo":"","slug":"","argv":["crew","overseer","status","--origin","halo"],"env":{}}`; string(data) != want {
		t.Fatalf("request = %s, want %s", data, want)
	}
	for _, argv := range [][]string{nil, {"crew", "overseer"}, {"crew", "overseer", "msg"}, {"crew", "roster", "--live"}, {"dispatch"}} {
		if _, err := BuildHostRequest(argv); err == nil {
			t.Errorf("argv %q accepted", argv)
		}
	}
}

func TestRunHost(t *testing.T) {
	t.Setenv("DISPATCH_CONFIG_BIN", configBin(t, mini))
	shim := sshShim(t, "7")
	argv := []string{"crew", "overseer", "deliver", "--origin", "halo"}
	var stdout, stderr bytes.Buffer
	if rc := RunHost(context.Background(), "mini", argv, &stdout, &stderr); rc != 7 {
		t.Fatalf("rc = %d, stderr %q", rc, stderr.String())
	}
	if stdout.String() != "remote-out\n" || stderr.String() != "remote-err\n" {
		t.Fatalf("stdout %q stderr %q", stdout.String(), stderr.String())
	}
	gotArgv, _ := os.ReadFile(filepath.Join(shim, "argv"))
	if want := strings.Join(SSHArgv("me@mini"), "\x00") + "\x00"; string(gotArgv) != want {
		t.Fatalf("ssh argv = %q", gotArgv)
	}
	stdin, _ := os.ReadFile(filepath.Join(shim, "stdin"))
	want, err := BuildHostRequest(argv)
	if err != nil {
		t.Fatal(err)
	}
	if string(stdin) != string(want) {
		t.Fatalf("request = %s, want %s", stdin, want)
	}
}

func TestRunHostSSHFailure(t *testing.T) {
	t.Setenv("DISPATCH_CONFIG_BIN", configBin(t, mini))
	sshShim(t, "255")
	var stderr bytes.Buffer
	if rc := RunHost(context.Background(), "mini", []string{"crew", "overseer", "status"}, io.Discard, &stderr); rc != 255 {
		t.Fatalf("rc = %d", rc)
	}
	if want := "remote-err\nssh to host 'mini' (me@mini) failed (exit 255)\n"; stderr.String() != want {
		t.Fatalf("stderr = %q, want %q", stderr.String(), want)
	}
}

func TestRunHostRefusesBeforeSSH(t *testing.T) {
	t.Setenv("DISPATCH_CONFIG_BIN", configBin(t, mini))
	shim := sshShim(t, "0")
	for _, tc := range []struct {
		host string
		argv []string
		want string
	}{
		{"maxi", []string{"crew", "overseer", "status"}, "unknown host 'maxi' (no hosts.maxi in settings)\n"},
		{"mini", []string{"crew", "overseer", "msg"}, "argv not allowed in host scope: [\"crew\" \"overseer\" \"msg\"]\n"},
	} {
		var stderr bytes.Buffer
		if rc := RunHost(context.Background(), tc.host, tc.argv, io.Discard, &stderr); rc != 2 || stderr.String() != tc.want {
			t.Errorf("RunHost(%s, %q) = %d, stderr %q, want %q", tc.host, tc.argv, rc, stderr.String(), tc.want)
		}
		if strings.Contains(stderr.String(), "remote-run") {
			t.Errorf("refusal %q carries the remote-run prefix", stderr.String())
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

func TestBuildRequestLayout(t *testing.T) {
	data, err := BuildRequest("/r/widget", "acme/widget", []string{"dispatch"}, []string{"CREW_ID=c1", "CREW_LAYOUT=2"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"v":2,"repo":"/r/widget","slug":"acme/widget","argv":["dispatch"],"env":{"CREW_ID":"c1","CREW_LAYOUT":"2"}}`; string(data) != want {
		t.Fatalf("request = %s, want %s", data, want)
	}
	for _, v := range []string{"1", "x", "", "2\n", "22"} {
		if err := CheckEnv("CREW_LAYOUT", v); err == nil {
			t.Errorf("CREW_LAYOUT=%q accepted", v)
		}
	}
	if err := CheckEnv("CREW_LAYOUT", "2"); err != nil {
		t.Fatalf("CREW_LAYOUT=2 refused: %v", err)
	}
}

var crewEnv = []string{"CREW_ID=c1", "DISPATCH_ORIGIN_HOST=halo"}

// crewVerb is a crew-scope verb with its own flags and positionals, as the
// origin sends them.
type crewVerb struct {
	name       string
	flags, pos []string
}

var crewVerbs = []crewVerb{
	{"reply", []string{"--no-nudge"}, []string{"worker:b#s1-1", "body"}},
	{"nudge", []string{"--wait", "30"}, []string{"b"}},
	{"where", nil, []string{"b"}},
}

type namedArgv struct {
	name string
	argv []string
}

func (v crewVerb) argv(parts ...[]string) []string {
	return slices.Concat(append([][]string{{"crew", v.name}}, parts...)...)
}

// accepted are argvs the verb grammar must admit for crew c1.
func (v crewVerb) accepted() []namedArgv {
	crew, sep := []string{"--crew", "c1"}, []string{"--"}
	rows := []namedArgv{
		{"good", v.argv(crew, v.flags, sep, v.pos)},
		{"no flags", v.argv(crew, sep, v.pos)},
		{"--crew=", v.argv([]string{"--crew=c1"}, v.flags, sep, v.pos)},
		{"flags first", v.argv(v.flags, crew, sep, v.pos)},
	}
	if v.name == "nudge" {
		rows = append(rows, namedArgv{"--wait at the cap", v.argv(crew, []string{"--wait", "86400"}, sep, v.pos)})
	}
	if v.name == "reply" {
		rows = append(rows, namedArgv{"body that reads --crew=", v.argv(crew, sep, []string{v.pos[0], "--crew=zzz"})})
	}
	return rows
}

// refusals are argvs the verb grammar must refuse for crew c1.
func (v crewVerb) refusals() []namedArgv {
	crew, sep := []string{"--crew", "c1"}, []string{"--"}
	rows := []namedArgv{
		{"no --crew", v.argv(v.flags, sep, v.pos)},
		{"--crew mismatch", v.argv([]string{"--crew", "far"}, v.flags, sep, v.pos)},
		{"--crew= mismatch", v.argv([]string{"--crew=far"}, v.flags, sep, v.pos)},
		{"duplicate --crew", v.argv(crew, crew, v.flags, sep, v.pos)},
		{"duplicate --crew=", v.argv(crew, []string{"--crew=c1"}, v.flags, sep, v.pos)},
		{"--crew then --", v.argv([]string{"--crew"}, sep, v.pos)},
		{"no --", v.argv(crew, v.flags, v.pos)},
		{"too few positionals", v.argv(crew, v.flags, sep, v.pos[:len(v.pos)-1])},
		{"too many positionals", v.argv(crew, v.flags, sep, v.pos, []string{"extra"})},
	}
	for _, extra := range [][]string{
		{"--log"}, {"--dir"}, {"--json"}, {"--home"}, {"--crew-scope"}, {"--spec-file", "x"}, {"--pane", "%1"},
		{"--wait", "30"}, {"--no-nudge"},
	} {
		if len(v.flags) > 0 && v.flags[0] == extra[0] {
			continue
		}
		rows = append(rows, namedArgv{"extra " + extra[0], v.argv(crew, extra, v.flags, sep, v.pos)})
	}
	switch v.name {
	case "reply":
		rows = append(rows,
			namedArgv{"target not worker:", v.argv(crew, v.flags, sep, []string{"b#s1-1", "body"})},
			namedArgv{"duplicate --no-nudge", v.argv(crew, v.flags, v.flags, sep, v.pos)})
	case "nudge":
		rows = append(rows,
			namedArgv{"--wait abc", v.argv(crew, []string{"--wait", "abc"}, sep, v.pos)},
			namedArgv{"--wait empty", v.argv(crew, []string{"--wait", ""}, sep, v.pos)},
			namedArgv{"--wait=30", v.argv(crew, []string{"--wait=30"}, sep, v.pos)},
			namedArgv{"--wait over the cap", v.argv(crew, []string{"--wait", "86401"}, sep, v.pos)},
			namedArgv{"--wait overflowing", v.argv(crew, []string{"--wait", "99999999999999999999999"}, sep, v.pos)},
			namedArgv{"--wait no value", v.argv(crew, []string{"--wait"}, sep, v.pos)},
			namedArgv{"duplicate --wait", v.argv(crew, v.flags, v.flags, sep, v.pos)})
	}
	return rows
}

func TestBuildCrewRequest(t *testing.T) {
	data, err := BuildCrewRequest([]string{"crew", "dispatch-row", "--crew", "c1", "--session", "s1-1"}, crewEnv)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"v":2,"scope":"crew","repo":"","slug":"","argv":["crew","dispatch-row","--crew","c1","--session","s1-1"],"env":{"CREW_ID":"c1","DISPATCH_ORIGIN_HOST":"halo"}}`; string(data) != want {
		t.Fatalf("request = %s, want %s", data, want)
	}
	if _, err := BuildCrewRequest([]string{"crew", "relay", "--since", "0", "--crew=c1"}, crewEnv); err != nil {
		t.Fatalf("relay --crew=c1 refused: %v", err)
	}
	for _, v := range crewVerbs {
		for _, tc := range v.accepted() {
			if _, err := BuildCrewRequest(tc.argv, crewEnv); err != nil {
				t.Errorf("%s %s refused: %v", v.name, tc.name, err)
			}
		}
		for _, tc := range v.refusals() {
			if data, err := BuildCrewRequest(tc.argv, crewEnv); err == nil {
				t.Errorf("%s %s accepted: %s", v.name, tc.name, data)
			}
		}
	}
	relay := []string{"crew", "relay", "--crew", "c1"}
	for _, tc := range []struct {
		name string
		argv []string
		env  []string
	}{
		{"no CREW_ID", relay, []string{"DISPATCH_ORIGIN_HOST=halo"}},
		{"no DISPATCH_ORIGIN_HOST", relay, []string{"CREW_ID=c1"}},
		{"extra env", relay, append([]string{"DISPATCH_EMIT_ROW=1"}, crewEnv...)},
		{"layout env", relay, append([]string{"CREW_LAYOUT=2"}, crewEnv...)},
		{"bad origin", relay, []string{"CREW_ID=c1", "DISPATCH_ORIGIN_HOST=-x"}},
		{"crew reply", []string{"crew", "reply", "--crew", "c1"}, crewEnv},
		{"dispatch", []string{"dispatch", "--crew", "c1"}, crewEnv},
		{"bare crew", []string{"crew"}, crewEnv},
		{"empty argv", nil, crewEnv},
		{"no --crew", []string{"crew", "relay", "--since", "0"}, crewEnv},
		{"two --crew", []string{"crew", "relay", "--crew", "c1", "--crew=c1"}, crewEnv},
		{"--crew other", []string{"crew", "relay", "--crew", "other"}, crewEnv},
		{"--crew=other", []string{"crew", "relay", "--crew=other"}, crewEnv},
		{"--crew last", []string{"crew", "relay", "--crew"}, crewEnv},
	} {
		if data, err := BuildCrewRequest(tc.argv, tc.env); err == nil {
			t.Errorf("%s: accepted: %s", tc.name, data)
		}
	}
}

func TestRunCrewScope(t *testing.T) {
	t.Setenv("DISPATCH_CONFIG_BIN", configBin(t, mini))
	shim := sshShim(t, "0")
	argv := []string{"crew", "dispatch-row", "--crew", "c1", "--session", "s1-1"}
	var stdout, stderr bytes.Buffer
	rc := Run(context.Background(), Call{Host: "mini", Cwd: t.TempDir(), CrewScope: true, Env: crewEnv, Argv: argv}, &stdout, &stderr)
	if rc != 0 || stdout.String() != "remote-out\n" || stderr.String() != "remote-err\n" {
		t.Fatalf("rc = %d, stdout %q, stderr %q", rc, stdout.String(), stderr.String())
	}
	gotArgv, _ := os.ReadFile(filepath.Join(shim, "argv"))
	if want := strings.Join(SSHArgv("me@mini"), "\x00") + "\x00"; string(gotArgv) != want {
		t.Fatalf("ssh argv = %q", gotArgv)
	}
	want, err := BuildCrewRequest(argv, crewEnv)
	if err != nil {
		t.Fatal(err)
	}
	if stdin, _ := os.ReadFile(filepath.Join(shim, "stdin")); string(stdin) != string(want) {
		t.Fatalf("request = %s, want %s", stdin, want)
	}
}

func TestRunCrewScopeRefusesBeforeSSH(t *testing.T) {
	t.Setenv("DISPATCH_CONFIG_BIN", configBin(t, mini))
	shim := sshShim(t, "0")
	cwd := t.TempDir()
	relay := []string{"crew", "relay", "--crew", "c1"}
	for _, tc := range []struct {
		call Call
		want string
	}{
		{Call{Host: "mini", Cwd: cwd, CrewScope: true, Env: crewEnv, SpecFile: "spec.md", Argv: relay}, "crew: remote-run: --spec-file is not allowed with --crew-scope\n"},
		{Call{Host: "mini", Cwd: cwd, CrewScope: true, Env: crewEnv, MigrateDir: "bundle", Argv: relay}, "crew: remote-run: --migrate-dir is not allowed with --crew-scope\n"},
		{Call{Host: "maxi", Cwd: cwd, CrewScope: true, Env: crewEnv, Argv: relay}, "crew: remote-run: unknown host 'maxi' (no hosts.maxi in settings)\n"},
		{Call{Host: "mini", Cwd: cwd, CrewScope: true, Env: crewEnv, Argv: []string{"crew", "reply", "--crew", "c1"}}, "crew: remote-run: crew scope: crew reply needs -- before its positionals\n"},
		{Call{Host: "mini", Cwd: cwd, CrewScope: true, Env: crewEnv, Argv: []string{"crew", "reap", "--crew", "c1"}}, "crew: remote-run: argv not allowed in crew scope: [\"crew\" \"reap\"]\n"},
		{Call{Host: "mini", Cwd: cwd, CrewScope: true, Env: crewEnv[:1], Argv: relay}, "crew: remote-run: crew scope needs env CREW_ID and DISPATCH_ORIGIN_HOST\n"},
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

const oldRemoteLine = "crew: remote-run: host 'mini' runs a crew remote-exec that speaks request v1 only, which predates layout 2 — upgrade crew on mini, or dispatch from a layout-1 crew\n"

func TestRunOldRemote(t *testing.T) {
	repo := cloneOf(t, "git@github.com:acme/widget.git")
	t.Setenv("DISPATCH_CONFIG_BIN", configBin(t, mini))
	const refusal = "crew: remote-exec: unsupported request version 2"
	v2 := Call{Host: "mini", Cwd: repo, Env: []string{"CREW_LAYOUT=2"}, Argv: []string{"dispatch", "--engines"}}
	crew := Call{Host: "mini", Cwd: t.TempDir(), CrewScope: true, Env: crewEnv, Argv: []string{"crew", "relay", "--crew", "c1"}}
	v1 := Call{Host: "mini", Cwd: repo, Argv: []string{"dispatch", "--engines"}}
	for _, tc := range []struct {
		name, code, errLine string
		call                Call
		want                string
	}{
		{"v2 old remote", "2", refusal, v2, refusal + "\n" + oldRemoteLine},
		{"crew scope old remote", "2", refusal, crew, refusal + "\n" + oldRemoteLine},
		{"v1 same shim", "2", refusal, v1, refusal + "\n"},
		{"v2 other refusal", "2", "crew: remote-exec: argv not allowed", v2, "crew: remote-exec: argv not allowed\n"},
		{"v2 other rc", "1", refusal, v2, refusal + "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sshShimStderr(t, tc.code, tc.errLine)
			var stderr bytes.Buffer
			rc := Run(context.Background(), tc.call, io.Discard, &stderr)
			if fmt.Sprint(rc) != tc.code || stderr.String() != tc.want {
				t.Fatalf("rc = %d, stderr %q, want %s / %q", rc, stderr.String(), tc.code, tc.want)
			}
		})
	}
}

func TestTailBufferCaps(t *testing.T) {
	var b tailBuffer
	_, _ = b.Write(bytes.Repeat([]byte("a"), 5000))
	_, _ = b.Write([]byte("end"))
	if got := b.String(); len(got) != maxStderrTail || !strings.HasSuffix(got, "aend") {
		t.Fatalf("tail len %d, suffix %q", len(got), got[max(0, len(got)-4):])
	}
}

func TestBranchLayout(t *testing.T) {
	row := func(crew, branch, extra string) string {
		return `{"kind":"dispatch","crew_id":"` + crew + `","branch":"` + branch + `","host":"mini","remote":true` + extra + `}`
	}
	for _, tc := range []struct {
		name  string
		lines []string
		want  int
	}{
		{"layout 2", []string{row("c", "b", `,"crew_layout":2`)}, 2},
		{"newest wins: 2 then 1", []string{row("c", "b", `,"crew_layout":2`), row("c", "b", "")}, 1},
		{"newest wins: 1 then 2", []string{row("c", "b", ""), row("c", "b", `,"crew_layout":2`)}, 2},
		{"absent", []string{row("c", "b", "")}, 1},
		{"layout 1", []string{row("c", "b", `,"crew_layout":1`)}, 1},
		{"string 2", []string{row("c", "b", `,"crew_layout":"2"`)}, 1},
		{"other branch ignored", []string{row("c", "b", ""), row("c", "x", `,"crew_layout":2`)}, 1},
		{"other crew ignored", []string{row("c", "b", ""), row("d", "b", `,"crew_layout":2`)}, 1},
		{"no row", nil, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := BranchLayout(busRows(t, tc.lines...), "b", "c"); got != tc.want {
				t.Fatalf("BranchLayout = %d, want %d", got, tc.want)
			}
		})
	}
}

// WindowBound trusts a window only as far as the crew's newest dispatch, resume or
// window row for the branch: its recorded window id, else (a row an older binary
// wrote) its repo plus /crew, compared by realpath when the spellings differ.
func TestWindowBound(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{real, link} {
		if err := os.MkdirAll(filepath.Join(d, "crew"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	row := func(crew, branch, repo string) bus.Event {
		return bus.Event{Kind: bus.KindDispatch, CrewID: crew, Branch: branch, Doc: map[string]any{"repo": repo}}
	}
	resume := row("c1", "b", real)
	resume.Kind = bus.KindResume
	withID := func(e bus.Event, id string) bus.Event {
		doc := map[string]any{"window_id": id}
		for k, v := range e.Doc.(map[string]any) {
			doc[k] = v
		}
		e.Doc = doc
		return e
	}
	nullID := func(e bus.Event) bus.Event {
		doc := map[string]any{"window_id": nil}
		for k, v := range e.Doc.(map[string]any) {
			doc[k] = v
		}
		e.Doc = doc
		return e
	}
	winRow := func(crew, branch, repo, id string) bus.Event {
		e := withID(row(crew, branch, repo), id)
		e.Kind = bus.KindWindow
		return e
	}
	for _, tc := range []struct {
		name string
		rows []bus.Event
		win  string
		want bool
		id   string
	}{
		{"row repo's crew dir", []bus.Event{row("c1", "b", real)}, real + "/crew", true, ""},
		{"same dir through a symlink", []bus.Event{row("c1", "b", real)}, link + "/crew", true, ""},
		{"another dir", []bus.Event{row("c1", "b", real)}, "/elsewhere/crew", false, ""},
		{"repo itself, not its crew dir", []bus.Event{row("c1", "b", real)}, real, false, ""},
		{"no row", nil, real + "/crew", false, ""},
		{"row of another crew", []bus.Event{row("c2", "b", real)}, real + "/crew", false, ""},
		{"row of another branch", []bus.Event{row("c1", "x", real)}, real + "/crew", false, ""},
		{"row with no repo", []bus.Event{row("c1", "b", "")}, "/crew", false, ""},
		{"a newer resume row in this dir", []bus.Event{row("c1", "b", "/moved"), resume}, real + "/crew", true, ""},
		{"a newer resume row in another dir", []bus.Event{row("c1", "b", real), func() bus.Event { r := row("c1", "b", "/moved"); r.Kind = bus.KindResume; return r }()}, real + "/crew", false, ""},
		{"the newest row wins", []bus.Event{row("c1", "b", real), row("c1", "b", "/moved")}, real + "/crew", false, ""},
		{"the recorded id", []bus.Event{withID(row("c1", "b", real), "@3")}, real + "/crew", true, "@3"},
		{"a forged window copying the dir, not the id", []bus.Event{withID(row("c1", "b", real), "@3")}, real + "/crew", false, "@9"},
		{"a recorded id binds without the dir", []bus.Event{withID(row("c1", "b", real), "@3")}, "/elsewhere/crew", true, "@3"},
		{"a window row after the dispatch row", []bus.Event{row("c1", "b", real), winRow("c1", "b", real, "@3")}, real + "/crew", true, "@3"},
		{"a window row refuses a forged dir copy", []bus.Event{row("c1", "b", real), winRow("c1", "b", real, "@3")}, real + "/crew", false, "@9"},
		{"a newer window row replaces an older id", []bus.Event{winRow("c1", "b", real, "@3"), winRow("c1", "b", real, "@4")}, real + "/crew", false, "@3"},
		{"a window row of another crew", []bus.Event{winRow("c2", "b", real, "@3")}, real + "/crew", false, "@3"},
		{"a window row of another branch", []bus.Event{winRow("c1", "x", real, "@3")}, real + "/crew", false, "@3"},
		{"a resume row's id", []bus.Event{winRow("c1", "b", real, "@3"), withID(resume, "@5")}, real + "/crew", true, "@5"},
		{"a resume row's id replaces the window row's", []bus.Event{winRow("c1", "b", real, "@3"), withID(resume, "@5")}, real + "/crew", false, "@3"},
		{"a dispatch row whose window is not open yet binds nothing", []bus.Event{nullID(row("c1", "b", real))}, real + "/crew", false, "@3"},
		{"a dispatch row pending its window, even for its dir", []bus.Event{nullID(row("c1", "b", real))}, real + "/crew", false, ""},
		{"a newer pending dispatch row replaces an older window row", []bus.Event{winRow("c1", "b", real, "@3"), nullID(row("c1", "b", real))}, real + "/crew", false, "@3"},
		{"a window row with no id binds nothing", []bus.Event{winRow("c1", "b", real, "")}, real + "/crew", false, ""},
		{"a resume row with an empty id binds nothing", []bus.Event{withID(resume, "")}, real + "/crew", false, ""},
		{"a legacy resume row after a window row keeps the dir binding", []bus.Event{winRow("c1", "b", real, "@3"), resume}, real + "/crew", true, "@9"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := WindowBound(tc.rows, "b", "c1", tc.id, tc.win); got != tc.want {
				t.Errorf("WindowBound = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestBoundRows(t *testing.T) {
	d := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(d, name)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	const row = `{"ts":1,"crew_id":"c1","kind":"dispatch","branch":"b"}` + "\n"
	if rows, err := BoundRows(filepath.Join(d, "absent")); err != nil || len(rows) != 0 {
		t.Errorf("missing log: %v, %v; want no rows and no error", rows, err)
	}
	if rows, err := BoundRows(write("ok", row)); err != nil || len(rows) != 1 {
		t.Errorf("whole log: %v, %v; want one row", rows, err)
	}
	if _, err := BoundRows(write("torn", row+"{\"ts\":\n"+row)); err == nil {
		t.Error("a log torn before its last row was read as whole")
	}
}
