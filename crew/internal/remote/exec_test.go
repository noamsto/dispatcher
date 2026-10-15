package remote

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

const helperEnv = "CREW_REMOTE_TEST_HELPER"

// fakeBin is a dir holding a fake `dispatch`, `permission-check` and `crew`.
// Each dumps into $REMOTE_TEST_DUMP: its argv NUL-separated, its cwd, a copy
// of $DISPATCH_SPEC (mode kept) and its path, and the allowlisted env plus
// CREW_WORKER_ID. With $FAKE_MARKER set it first sleeps 2 s, writes to stdout,
// then touches the marker.
func fakeBin(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	body := func(argv0 string) string {
		return `d="$REMOTE_TEST_DUMP"
if [ -n "${FAKE_MARKER:-}" ]; then sleep 2; echo late; : > "$FAKE_MARKER"; exit 0; fi
printf '%s\0' ` + argv0 + ` "$@" > "$d/argv"
pwd -P > "$d/pwd"
if [ -n "${DISPATCH_SPEC:-}" ]; then cp -p "$DISPATCH_SPEC" "$d/spec"; printf '%s' "$DISPATCH_SPEC" > "$d/spec-path"; fi
env | grep -E '^(CREW_ID|DISPATCH_TMUX_SESSION|DISPATCH_EMIT_ROW|DISPATCH_SESSION_ID|CREW_IDENTITY_AVOID|CREW_WORKER_ID)=' | sort > "$d/env"
echo child-out
echo child-err >&2
if [ -n "${FAKE_SIGNAL:-}" ]; then kill -TERM $$; fi
exit "${FAKE_RC:-0}"
`
	}
	writeScript(t, filepath.Join(dir, "dispatch"), body("dispatch"))
	writeScript(t, filepath.Join(dir, "permission-check"), body("permission-check"))
	writeScript(t, filepath.Join(dir, "crew"), body("crew"))
	return dir
}

func binOptions(bin string) ServeOptions {
	return ServeOptions{
		LookPath: func(name string) (string, error) { return exec.LookPath(filepath.Join(bin, name)) },
		Self:     func() (string, error) { return filepath.Join(bin, "crew"), nil },
	}
}

type serveEnv struct {
	repo, dump, bin string
}

func newServeEnv(t *testing.T) serveEnv {
	t.Helper()
	e := serveEnv{repo: cloneOf(t, "git@github.com:Acme/Widget.git"), dump: t.TempDir(), bin: fakeBin(t)}
	t.Setenv("REMOTE_TEST_DUMP", e.dump)
	return e
}

func (e serveEnv) request(t *testing.T, argv []string, env map[string]string, spec *string) string {
	t.Helper()
	data, err := json.Marshal(Request{V: 1, Repo: e.repo, Slug: "acme/widget", Argv: argv, Env: env, Spec: spec})
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func (e serveEnv) serve(t *testing.T, stdin string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	rc := Serve(strings.NewReader(stdin), &stdout, &stderr, binOptions(e.bin))
	return rc, stdout.String(), stderr.String()
}

func (e serveEnv) dumped(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(e.dump, name))
	if err != nil {
		t.Fatalf("child dump %s: %v", name, err)
	}
	return string(data)
}

func (e serveEnv) childRan() bool {
	_, err := os.Stat(filepath.Join(e.dump, "argv"))
	return err == nil
}

func TestServeRefusals(t *testing.T) {
	e := newServeEnv(t)
	other := cloneOf(t, "https://github.com/acme/other.git")
	plain := t.TempDir()
	valid := func(mut func(m map[string]any)) string {
		m := map[string]any{"v": 1, "repo": e.repo, "slug": "acme/widget", "argv": []string{"dispatch"}, "env": map[string]string{}}
		mut(m)
		data, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	for _, tc := range []struct {
		name, stdin, want string
	}{
		{"version", valid(func(m map[string]any) { m["v"] = 2 }), "version"},
		{"unknown key", valid(func(m map[string]any) { m["extra"] = 1 }), "extra"},
		{"case-folded key", valid(func(m map[string]any) { delete(m, "repo"); m["Repo"] = e.repo }), "Repo"},
		{"rm", valid(func(m map[string]any) { m["argv"] = []string{"rm", "-rf", "/"} }), "not allowed"},
		{"crew msg", valid(func(m map[string]any) { m["argv"] = []string{"crew", "msg", "x"} }), "not allowed"},
		{"bare crew", valid(func(m map[string]any) { m["argv"] = []string{"crew"} }), "not allowed"},
		{"empty argv", valid(func(m map[string]any) { m["argv"] = []string{} }), "not allowed"},
		{"env key", valid(func(m map[string]any) { m["env"] = map[string]string{"LD_PRELOAD": "/x.so"} }), "LD_PRELOAD"},
		{"env value", valid(func(m map[string]any) { m["env"] = map[string]string{"CREW_ID": "a;b"} }), "CREW_ID"},
		{"worker id", valid(func(m map[string]any) { m["env"] = map[string]string{"CREW_WORKER_ID": "x"} }), "CREW_WORKER_ID"},
		{"nul", valid(func(m map[string]any) { m["argv"] = []string{"dispatch", "a\x00b"} }), "NUL"},
		{"nul spec", valid(func(m map[string]any) { m["spec"] = "a\x00b" }), "NUL"},
		{"oversize", valid(func(m map[string]any) { m["spec"] = strings.Repeat("a", MaxRequest) }), "1 MiB"},
		{"trailing", valid(func(map[string]any) {}) + `{"v":1}`, "one JSON object"},
		{"not json", "dispatch --title x", "request"},
		{"relative repo", valid(func(m map[string]any) { m["repo"] = "repo" }), "absolute"},
		{"not a repo", valid(func(m map[string]any) { m["repo"] = plain }), "git checkout"},
		{"missing repo", valid(func(m map[string]any) { m["repo"] = plain + "/nope" }), "directory"},
		{"other clone", valid(func(m map[string]any) { m["repo"] = other }),
			"crew: remote-exec: " + other + " is not a clone of acme/widget (origin: https://github.com/acme/other.git)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rc, stdout, stderr := e.serve(t, tc.stdin)
			if rc != 2 || stdout != "" || !strings.Contains(stderr, tc.want) {
				t.Fatalf("rc %d, stdout %q, stderr %q, want rc 2 naming %q", rc, stdout, stderr, tc.want)
			}
			if e.childRan() {
				t.Fatal("the child ran despite the refusal")
			}
		})
	}
}

func TestServeArgvVerbatim(t *testing.T) {
	e := newServeEnv(t)
	marker := filepath.Join(t.TempDir(), "X")
	argv := []string{"dispatch", "--title", "$(touch " + marker + ")", "`touch " + marker + "`", "a; touch " + marker, `'"q"'`, "line\nbreak"}
	spec := "spec $(touch " + marker + ")\n`x`; 'q' \"q\"\n"
	env := map[string]string{
		"CREW_ID": "c1", "DISPATCH_TMUX_SESSION": "crew-c1", "DISPATCH_EMIT_ROW": "1",
		"DISPATCH_SESSION_ID": "s17-42", "CREW_IDENTITY_AVOID": "ada,bob",
	}
	t.Setenv("CREW_WORKER_ID", "worker:feat/x#s1-1")
	rc, stdout, stderr := e.serve(t, e.request(t, argv, env, &spec))
	if rc != 0 {
		t.Fatalf("rc %d, stderr %q", rc, stderr)
	}
	if stdout != "child-out\n" || stderr != "child-err\n" {
		t.Fatalf("stdout %q, stderr %q", stdout, stderr)
	}
	if got := e.dumped(t, "argv"); got != strings.Join(argv, "\x00")+"\x00" {
		t.Fatalf("argv = %q", got)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("an argv or spec string was run by a shell")
	}
	if got := strings.TrimSpace(e.dumped(t, "pwd")); got != e.repo {
		t.Fatalf("child cwd = %q, want %q", got, e.repo)
	}
	if got := e.dumped(t, "spec"); got != spec {
		t.Fatalf("spec = %q", got)
	}
	st, err := os.Stat(filepath.Join(e.dump, "spec"))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("spec mode = %v (%v), want 0600", st.Mode(), err)
	}
	specPath := e.dumped(t, "spec-path")
	if !strings.HasPrefix(specPath, filepath.Join(e.repo, ".git", "crew", "remote-exec")+"/") {
		t.Fatalf("spec file %q outside the scratch root", specPath)
	}
	if _, err := os.Stat(filepath.Dir(specPath)); !os.IsNotExist(err) {
		t.Fatalf("scratch dir %s left behind (%v)", filepath.Dir(specPath), err)
	}
	wantEnv := "CREW_ID=c1\nCREW_IDENTITY_AVOID=ada,bob\nCREW_WORKER_ID=worker:feat/x#s1-1\nDISPATCH_EMIT_ROW=1\nDISPATCH_SESSION_ID=s17-42\nDISPATCH_TMUX_SESSION=crew-c1\n"
	if got := e.dumped(t, "env"); got != wantEnv {
		t.Fatalf("child env = %q, want %q", got, wantEnv)
	}
}

func TestServeCrewRunsSelf(t *testing.T) {
	e := newServeEnv(t)
	for _, sub := range CrewSubcommands {
		rc, stdout, stderr := e.serve(t, e.request(t, []string{"crew", sub, "a b"}, nil, nil))
		if rc != 0 || stdout != "child-out\n" || stderr != "child-err\n" {
			t.Fatalf("crew %s: rc %d, stdout %q, stderr %q", sub, rc, stdout, stderr)
		}
		if got := e.dumped(t, "argv"); got != "crew\x00"+sub+"\x00a b\x00" {
			t.Fatalf("crew %s argv = %q", sub, got)
		}
	}
}

func TestServeExitStatus(t *testing.T) {
	e := newServeEnv(t)
	t.Setenv("FAKE_RC", "3")
	if rc, _, _ := e.serve(t, e.request(t, []string{"permission-check", "--pane", "%1"}, nil, nil)); rc != 3 {
		t.Fatalf("rc = %d, want 3", rc)
	}
	if rc, _, _ := e.serve(t, e.request(t, []string{"dispatch"}, nil, nil)); rc != 3 {
		t.Fatalf("detached rc = %d, want 3", rc)
	}
	t.Setenv("FAKE_SIGNAL", "1")
	if rc, _, _ := e.serve(t, e.request(t, []string{"permission-check"}, nil, nil)); rc != 128+15 {
		t.Fatalf("signalled rc = %d, want 143", rc)
	}
}

func TestServeMissingCommand(t *testing.T) {
	e := newServeEnv(t)
	if err := os.Remove(filepath.Join(e.bin, "permission-check")); err != nil {
		t.Fatal(err)
	}
	rc, _, stderr := e.serve(t, e.request(t, []string{"permission-check"}, nil, nil))
	if rc != 127 || !strings.Contains(stderr, "permission-check") {
		t.Fatalf("rc %d, stderr %q", rc, stderr)
	}
}

func TestServeSweepsStaleScratch(t *testing.T) {
	e := newServeEnv(t)
	root := filepath.Join(e.repo, ".git", "crew", "remote-exec")
	stale, fresh := filepath.Join(root, "1-old"), filepath.Join(root, "2-new")
	for _, d := range []string{stale, fresh} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "out"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-25 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	if rc, _, stderr := e.serve(t, e.request(t, []string{"crew", "where", "x"}, nil, nil)); rc != 0 {
		t.Fatalf("rc %d, stderr %q", rc, stderr)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale scratch dir kept (%v)", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("fresh scratch dir swept: %v", err)
	}
}

// TestHelperServe is `crew remote-exec` in its own process, started outside
// any repo, so a test can kill it mid-request as a dropped ssh link would.
func TestHelperServe(t *testing.T) {
	if os.Getenv(helperEnv) != "1" {
		t.Skip("helper process for the detached dispatch test")
	}
	os.Exit(Serve(os.Stdin, os.Stdout, os.Stderr, binOptions(os.Getenv("REMOTE_TEST_BIN"))))
}

func TestServeDetachedDispatchSurvivesCaller(t *testing.T) {
	e := newServeEnv(t)
	marker := filepath.Join(t.TempDir(), "marker")
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperServe$")
	cmd.Dir = t.TempDir()
	cmd.Env = append(os.Environ(), helperEnv+"=1", "REMOTE_TEST_BIN="+e.bin, "FAKE_MARKER="+marker)
	cmd.Stdin = strings.NewReader(e.request(t, []string{"dispatch", "--title", "t"}, nil, nil))
	// Its own process group stands in for sshd's session: killing the group
	// kills every process that did not leave it. The stdout pipe is closed by
	// Wait, so a child still writing to it would die of SIGPIPE.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if _, err := cmd.StdoutPipe(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the dispatch finished before the caller was killed; the test proves nothing")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the detached dispatch died with its caller")
		}
		time.Sleep(100 * time.Millisecond)
	}
}
