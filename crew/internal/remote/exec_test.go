package remote

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
)

const helperEnv = "CREW_REMOTE_TEST_HELPER"

// fakeBin is a dir holding a fake `dispatch`, `permission-check` and `crew`.
// Each dumps into $REMOTE_TEST_DUMP: its argv NUL-separated, its cwd, a copy
// of $DISPATCH_SPEC and of $DISPATCH_MIGRATE_DIR (modes kept) and their
// paths, and the allowlisted env plus
// CREW_WORKER_ID. With $FAKE_MARKER set it touches $FAKE_MARKER.started, sleeps
// 2 s, writes to stdout, then touches the marker.
func fakeBin(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	body := func(argv0 string) string {
		return `d="$REMOTE_TEST_DUMP"
if [ -n "${FAKE_MARKER:-}" ]; then : > "$FAKE_MARKER.started"; sleep 2; echo late; : > "$FAKE_MARKER"; exit 0; fi
printf '%s\0' ` + argv0 + ` "$@" > "$d/argv"
pwd -P > "$d/pwd"
umask > "$d/umask"
if [ -n "${DISPATCH_SPEC:-}" ]; then cp -p "$DISPATCH_SPEC" "$d/spec"; printf '%s' "$DISPATCH_SPEC" > "$d/spec-path"; ls "$(dirname "$DISPATCH_SPEC")" > "$d/scratch-ls"; fi
if [ -n "${DISPATCH_MIGRATE_DIR:-}" ]; then cp -Rp "$DISPATCH_MIGRATE_DIR" "$d/migrate"; printf '%s' "$DISPATCH_MIGRATE_DIR" > "$d/migrate-path"; fi
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
	return e.serveWith(t, stdin, binOptions(e.bin))
}

func (e serveEnv) serveWith(t *testing.T, stdin string, o ServeOptions) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	rc := Serve(strings.NewReader(stdin), &stdout, &stderr, o)
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
		{"version", valid(func(m map[string]any) { m["v"] = 3 }), "unsupported request version 3 (this remote-exec speaks v1-v2)"},
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
	started := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(marker + ".started"); err == nil {
			break
		}
		if time.Now().After(started) {
			t.Fatal("the dispatch never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
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

func TestAdmitPeerSyncStdin(t *testing.T) {
	e := newServeEnv(t)
	req := func(argv []string, stdin *string) string {
		m := map[string]any{"v": 1, "repo": e.repo, "slug": "acme/widget", "argv": argv, "env": map[string]string{}}
		if stdin != nil {
			m["stdin"] = *stdin
		}
		data, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	payload, nul := `{"v":1}`, "a\x00b"
	for _, tc := range []struct {
		name  string
		argv  []string
		stdin *string
		want  string
	}{
		{"peer-sync with stdin", []string{"crew", "peer-sync"}, &payload, ""},
		{"peer-sync without stdin", []string{"crew", "peer-sync"}, nil, ""},
		{"stdin with reply", []string{"crew", "reply"}, &payload, "stdin is only allowed with crew peer-sync"},
		{"stdin with peer-sync args", []string{"crew", "peer-sync", "x"}, &payload, "stdin is only allowed with crew peer-sync"},
		{"peer-syncx", []string{"crew", "peer-syncx"}, nil, "argv not allowed"},
		{"bogus", []string{"crew", "bogus"}, nil, "argv not allowed"},
		{"stdin nul", []string{"crew", "peer-sync"}, &nul, "NUL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := admit(context.Background(), strings.NewReader(req(tc.argv, tc.stdin)))
			if tc.want == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestServePeerSyncStdinReachesChild(t *testing.T) {
	e := newServeEnv(t)
	cat := filepath.Join(t.TempDir(), "crew")
	writeScript(t, cat, "cat\n")
	payload := "{\"v\":1,\"rows\":[]}\nsecond line"
	data, err := json.Marshal(map[string]any{"v": 1, "repo": e.repo, "slug": "acme/widget", "argv": []string{"crew", "peer-sync"}, "env": map[string]string{}, "stdin": payload})
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	o := ServeOptions{Self: func() (string, error) { return cat, nil }}
	if rc := Serve(bytes.NewReader(data), &stdout, &stderr, o); rc != 0 {
		t.Fatalf("rc %d, stderr %q", rc, stderr.String())
	}
	if stdout.String() != payload {
		t.Fatalf("child stdin = %q, want %q", stdout.String(), payload)
	}
}

// fakeTmux puts a `tmux` in e.bin that logs each call to $REMOTE_TEST_DUMP/tmux
// (argv NUL-separated, calls separated by \x1e). `run-shell -b CMD` runs CMD
// synchronously unless onRunShell says otherwise.
func (e serveEnv) fakeTmux(t *testing.T, onRunShell string) {
	t.Helper()
	if onRunShell == "" {
		onRunShell = `sh -c "$3"; exit 0`
	}
	writeScript(t, filepath.Join(e.bin, "tmux"), `{ printf '%s\0' "$@"; printf '\036'; } >> "$REMOTE_TEST_DUMP/tmux"
if [ "$1" = run-shell ]; then `+onRunShell+`; fi
exit 0
`)
}

func (e serveEnv) tmuxCalls(t *testing.T) [][]string {
	t.Helper()
	var calls [][]string
	for rec := range strings.SplitSeq(e.dumped(t, "tmux"), "\x1e") {
		if rec != "" {
			calls = append(calls, strings.Split(strings.TrimSuffix(rec, "\x00"), "\x00"))
		}
	}
	return calls
}

// scratchDirs lists the scratch dirs remote-exec left in e's clone.
func (e serveEnv) scratchDirs(t *testing.T) []string {
	t.Helper()
	dirs, err := filepath.Glob(filepath.Join(e.repo, ".git", "crew", "remote-exec", "*"))
	if err != nil {
		t.Fatal(err)
	}
	return dirs
}

func viaTmuxOptions(e serveEnv) ServeOptions {
	o := binOptions(e.bin)
	o.ViaTmux = true
	return o
}

func TestServeDispatchViaTmux(t *testing.T) {
	e := newServeEnv(t)
	e.fakeTmux(t, "")
	t.Setenv("FAKE_RC", "3")
	defer syscall.Umask(syscall.Umask(0o027))
	env := map[string]string{"CREW_ID": "c1", "DISPATCH_TMUX_SESSION": "crew-c1", "DISPATCH_EMIT_ROW": "1"}
	spec := "s"
	rc, stdout, stderr := e.serveWith(t, e.request(t, []string{"dispatch", "--title", "it's"}, env, &spec), viaTmuxOptions(e))
	if rc != 3 || !strings.Contains(stdout, "child-out") || !strings.Contains(stderr, "child-err") {
		t.Fatalf("rc %d, stdout %q, stderr %q", rc, stdout, stderr)
	}
	if got := e.dumped(t, "argv"); got != "dispatch\x00--title\x00it's\x00" {
		t.Fatalf("argv = %q", got)
	}
	calls := e.tmuxCalls(t)
	if len(calls) != 1 || len(calls[0]) != 3 || calls[0][0] != "run-shell" || calls[0][1] != "-b" {
		t.Fatalf("tmux calls %q, want one run-shell -b", calls)
	}
	if log := e.dumped(t, "tmux"); strings.Contains(log, "CREW_ID=") {
		t.Fatalf("an env value reached a tmux argv: %q", log)
	}
	if got := e.dumped(t, "env"); !strings.Contains(got, "CREW_ID=c1\n") {
		t.Fatalf("child env = %q", got)
	}
	if got := strings.TrimSpace(e.dumped(t, "umask")); got != "0027" {
		t.Fatalf("child umask = %s, want the caller's 0027", got)
	}
	if got := strings.Fields(e.dumped(t, "scratch-ls")); slices.Contains(got, "run") || !slices.Contains(got, "started") {
		t.Fatalf("scratch while the child ran = %q, want started and no run script", got)
	}
	repo, err := filepath.EvalSymlinks(e.repo)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(e.dumped(t, "pwd")); got != repo {
		t.Fatalf("child cwd = %q, want %q", got, repo)
	}
	if dirs := e.scratchDirs(t); len(dirs) != 0 {
		t.Fatalf("scratch left behind: %q", dirs)
	}
}

func TestServeDispatchViaTmuxSpacedPath(t *testing.T) {
	e := newServeEnv(t)
	e.fakeTmux(t, "")
	spaced := filepath.Join(filepath.Dir(e.repo), "my repo")
	if err := os.Rename(e.repo, spaced); err != nil {
		t.Fatal(err)
	}
	e.repo = spaced
	rc, stdout, stderr := e.serveWith(t, e.request(t, []string{"dispatch"}, nil, nil), viaTmuxOptions(e))
	if rc != 0 || stdout != "child-out\n" || stderr != "child-err\n" {
		t.Fatalf("rc %d, stdout %q, stderr %q", rc, stdout, stderr)
	}
	if got := strings.TrimSpace(e.dumped(t, "pwd")); got != spaced {
		t.Fatalf("child cwd = %q, want %q", got, spaced)
	}
}

func TestRunShellSafe(t *testing.T) {
	for _, s := range []string{"/a b/c", "/x/y-1.2_3", "/it's/(x);&|<>*?~{}"} {
		if !runShellSafe(s) {
			t.Errorf("runShellSafe(%q) = false", s)
		}
	}
	for _, s := range []string{"/a$b", `/a"b`, `/a\b`, "/a`b", "/a#{b}", "/a\nb", "/a\x7fb"} {
		if runShellSafe(s) {
			t.Errorf("runShellSafe(%q) = true", s)
		}
	}
}

func setTmuxStartTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	old := tmuxStartTimeout
	tmuxStartTimeout = d
	t.Cleanup(func() { tmuxStartTimeout = old })
}

func TestServeDispatchViaTmuxNeverStarts(t *testing.T) {
	e := newServeEnv(t)
	e.fakeTmux(t, "exit 0")
	setTmuxStartTimeout(t, 300*time.Millisecond)
	rc, _, stderr := e.serveWith(t, e.request(t, []string{"dispatch"}, nil, nil), viaTmuxOptions(e))
	if rc != 255 || !strings.Contains(stderr, "never started") || e.childRan() {
		t.Fatalf("rc %d, child ran %v, stderr %q", rc, e.childRan(), stderr)
	}
	dirs := e.scratchDirs(t)
	if len(dirs) != 1 {
		t.Fatalf("scratch dirs %q, want the one kept for a late start", dirs)
	}
	if _, err := os.Stat(filepath.Join(dirs[0], "run")); !os.IsNotExist(err) {
		t.Fatalf("the run script holding the environment is still there (%v)", err)
	}
}

// TestServeDispatchViaTmuxDiesWithoutStatus: the job records a pid that is
// gone and never writes a status, as when the server dies mid-dispatch.
func TestServeDispatchViaTmuxDiesWithoutStatus(t *testing.T) {
	e := newServeEnv(t)
	e.fakeTmux(t, `run=${3%'" >/dev/null 2>&1'}; run=${run##*'"'}
true & pid=$!; wait
printf '%s\n' "$pid" > "${run%/run}/started"; exit 0`)
	rc, _, stderr := e.serveWith(t, e.request(t, []string{"dispatch"}, nil, nil), viaTmuxOptions(e))
	if rc != 255 || !strings.Contains(stderr, "may still be running") {
		t.Fatalf("rc %d, stderr %q", rc, stderr)
	}
	if dirs := e.scratchDirs(t); len(dirs) != 1 {
		t.Fatalf("scratch dirs %q, want the one a live dispatch may still use", dirs)
	}
}

// TestServeDispatchViaTmuxReadonlyEnv: bash as /bin/sh exits on exporting a
// readonly variable such as PPID; that must not cost the exit status. dash
// exports it fine and the child runs.
func TestServeDispatchViaTmuxReadonlyEnv(t *testing.T) {
	e := newServeEnv(t)
	e.fakeTmux(t, "")
	t.Setenv("PPID", "1")
	rc, _, stderr := e.serveWith(t, e.request(t, []string{"dispatch"}, nil, nil), viaTmuxOptions(e))
	if strings.Contains(stderr, "without an exit status") {
		t.Fatalf("rc %d, stderr %q", rc, stderr)
	}
	if e.childRan() {
		if rc != 0 {
			t.Fatalf("child ran: rc %d, stderr %q", rc, stderr)
		}
		return
	}
	if rc == 0 || !strings.Contains(stderr, "readonly") {
		t.Fatalf("child did not run: rc %d, stderr %q", rc, stderr)
	}
}

func TestServeDispatchViaTmuxNoServer(t *testing.T) {
	e := newServeEnv(t)
	e.fakeTmux(t, `echo 'no server running on /tmp/tmux-1/default' >&2; exit 1`)
	rc, _, stderr := e.serveWith(t, e.request(t, []string{"dispatch"}, nil, nil), viaTmuxOptions(e))
	if rc != 0 || !e.childRan() {
		t.Fatalf("rc %d, child ran %v, stderr %q", rc, e.childRan(), stderr)
	}
	if !strings.Contains(stderr, "running it in this ssh session") || !strings.Contains(stderr, "no server running") {
		t.Fatalf("stderr %q", stderr)
	}
}

func TestServeDispatchViaRealTmux(t *testing.T) {
	tmux, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("no tmux")
	}
	e := newServeEnv(t)
	if !runShellSafe(e.repo) {
		t.Skipf("the test repo path %q has characters the tmux route refuses", e.repo)
	}
	dir, err := os.MkdirTemp("", "t")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMUX_TMPDIR", dir)
	// $TMUX would point the client at the server this test runs in.
	t.Setenv("TMUX", "")
	if out, err := exec.Command(tmux, "-f", "/dev/null", "new-session", "-d", "-s", "t", "sleep 60").CombinedOutput(); err != nil {
		t.Fatalf("tmux new-session: %v: %s", err, out)
	}
	t.Cleanup(func() {
		sock := filepath.Join(dir, fmt.Sprintf("tmux-%d", os.Getuid()), "default")
		_ = exec.Command(tmux, "-S", sock, "kill-server").Run()
		_ = os.RemoveAll(dir)
	})
	t.Setenv("FAKE_RC", "3")
	o := binOptions(e.bin)
	o.LookPath = func(name string) (string, error) {
		if name == "tmux" {
			return tmux, nil
		}
		return exec.LookPath(filepath.Join(e.bin, name))
	}
	o.ViaTmux = true
	spec := "s"
	req := e.request(t, []string{"dispatch"}, map[string]string{"CREW_ID": "c1"}, &spec)
	var rc int
	var stdout, stderr string
	done := make(chan struct{})
	go func() {
		defer close(done)
		var o1, o2 bytes.Buffer
		rc = Serve(strings.NewReader(req), &o1, &o2, o)
		stdout, stderr = o1.String(), o2.String()
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("remote-exec never returned")
	}
	if rc != 3 || !strings.Contains(stdout, "child-out") || !strings.Contains(stderr, "child-err") {
		t.Fatalf("rc %d, stdout %q, stderr %q", rc, stdout, stderr)
	}
	if strings.Contains(stderr, "ssh session") {
		t.Fatalf("fell back to the ssh session: %q", stderr)
	}
	if got := strings.Fields(e.dumped(t, "scratch-ls")); slices.Contains(got, "run") {
		t.Fatalf("scratch while the child ran = %q, want no run script", got)
	}
}

// hostRequest is a host-scope request for argv; mut edits it before encoding.
func hostRequest(t *testing.T, argv []string, mut func(m map[string]any)) string {
	t.Helper()
	m := map[string]any{"v": 1, "scope": "host", "repo": "", "slug": "", "argv": argv, "env": map[string]string{}}
	if mut != nil {
		mut(m)
	}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestServeHostScope(t *testing.T) {
	e := newServeEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := t.TempDir()
	t.Chdir(cwd)
	argv := []string{"crew", "overseer", "status", "--origin", "h"}
	rc, stdout, stderr := e.serve(t, hostRequest(t, argv, nil))
	if rc != 0 || stdout != "child-out\n" || stderr != "child-err\n" {
		t.Fatalf("rc %d, stdout %q, stderr %q", rc, stdout, stderr)
	}
	if got := e.dumped(t, "argv"); got != strings.Join(argv, "\x00")+"\x00" {
		t.Fatalf("argv = %q", got)
	}
	want, err := filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(e.dumped(t, "pwd")); got != want {
		t.Fatalf("child cwd = %q, want %q", got, want)
	}
	for _, dir := range []string{home, cwd} {
		if _, err := os.Stat(filepath.Join(dir, "crew")); !os.IsNotExist(err) {
			t.Fatalf("a scratch root was made under %s (%v)", dir, err)
		}
	}
	if dirs := e.scratchDirs(t); len(dirs) != 0 {
		t.Fatalf("scratch dirs %q", dirs)
	}

	t.Setenv("FAKE_RC", "4")
	if rc, _, _ := e.serve(t, hostRequest(t, []string{"crew", "overseer", "deliver"}, nil)); rc != 4 {
		t.Fatalf("deliver rc = %d, want 4", rc)
	}
}

func TestServeHostScopeRefusals(t *testing.T) {
	e := newServeEnv(t)
	t.Setenv("HOME", t.TempDir())
	status := []string{"crew", "overseer", "status"}
	for _, tc := range []struct {
		name, stdin, want string
	}{
		{"repo", hostRequest(t, status, func(m map[string]any) { m["repo"] = e.repo }), "host scope"},
		{"slug", hostRequest(t, status, func(m map[string]any) { m["slug"] = "acme/widget" }), "host scope"},
		{"env", hostRequest(t, status, func(m map[string]any) { m["env"] = map[string]string{"CREW_ID": "c1"} }), "host scope"},
		{"spec", hostRequest(t, status, func(m map[string]any) { m["spec"] = "s" }), "host scope"},
		{"empty spec", hostRequest(t, status, func(m map[string]any) { m["spec"] = "" }), "host scope"},
		{"stdin", hostRequest(t, status, func(m map[string]any) { m["stdin"] = "s" }), "host scope"},
		{"crew roster", hostRequest(t, []string{"crew", "roster", "--live"}, nil), "argv not allowed"},
		{"dispatch", hostRequest(t, []string{"dispatch"}, nil), "argv not allowed"},
		{"overseer msg", hostRequest(t, []string{"crew", "overseer", "msg"}, nil), "argv not allowed"},
		{"bare overseer", hostRequest(t, []string{"crew", "overseer"}, nil), "argv not allowed"},
		{"empty argv", hostRequest(t, []string{}, nil), "argv not allowed"},
		{"unknown scope", hostRequest(t, status, func(m map[string]any) { m["scope"] = "fleet" }), `unsupported request scope "fleet"`},
		{"repo scope overseer", e.request(t, status, nil, nil), "argv not allowed"},
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

func gzB64(t *testing.T, data []byte) string {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

// migrateRequest is a repo-scope request for argv carrying migrate, with spec
// as its spec when not "".
func (e serveEnv) migrateRequest(t *testing.T, argv []string, migrate any, spec string) string {
	t.Helper()
	m := map[string]any{"v": 1, "repo": e.repo, "slug": "acme/widget", "argv": argv, "env": map[string]string{}}
	if migrate != nil {
		m["migrate"] = migrate
	}
	if spec != "" {
		m["spec"] = spec
	}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

var migrateArgvOK = []string{"dispatch", "standard", "opus", "--title", "x"}

func TestServeMigrate(t *testing.T) {
	e := newServeEnv(t)
	patch, transcript := "diff --git a/x b/x\n", `{"type":"user"}`+"\n"
	payload := map[string]any{
		"meta":     testMeta(),
		"patch_gz": gzB64(t, []byte(patch)), "transcript_gz": gzB64(t, []byte(transcript)),
	}
	rc, _, stderr := e.serve(t, e.migrateRequest(t, migrateArgvOK, payload, ""))
	if rc != 0 {
		t.Fatalf("rc %d, stderr %q", rc, stderr)
	}
	dir := e.dumped(t, "migrate-path")
	if root := filepath.Join(e.repo, ".git", "crew", "remote-exec") + "/"; !strings.HasPrefix(dir, root) || filepath.Base(dir) != "migrate" {
		t.Fatalf("DISPATCH_MIGRATE_DIR = %q, want <scratch>/migrate under %s", dir, root)
	}
	if _, err := os.Stat(filepath.Dir(dir)); !os.IsNotExist(err) {
		t.Fatalf("scratch dir %s left behind (%v)", filepath.Dir(dir), err)
	}
	wantMeta := `{"v":1,"branch":"feat/x","tip":"` + testTip + `","from_host":"laptop","mode":"coop","session":"` + testSession + `","handoff":"carry on $(x)\n"}`
	for name, want := range map[string]string{"meta.json": wantMeta, "patch": patch, "transcript.jsonl": transcript} {
		path := filepath.Join(e.dump, "migrate", name)
		got, err := os.ReadFile(path)
		if err != nil || string(got) != want {
			t.Fatalf("%s = %q (%v), want %q", name, got, err, want)
		}
		if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %v (%v), want 0600", name, st.Mode(), err)
		}
	}
	if st, err := os.Stat(filepath.Join(e.dump, "migrate")); err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("migrate dir mode = %v (%v), want 0700", st.Mode(), err)
	}

	e2 := newServeEnv(t)
	meta := testMeta()
	delete(meta, "session")
	if rc, _, stderr := e2.serve(t, e2.migrateRequest(t, migrateArgvOK, map[string]any{"meta": meta}, "")); rc != 0 {
		t.Fatalf("meta only: rc %d, stderr %q", rc, stderr)
	}
	entries, err := os.ReadDir(filepath.Join(e2.dump, "migrate"))
	if err != nil || len(entries) != 1 || entries[0].Name() != "meta.json" {
		t.Fatalf("meta only migrate dir = %v (%v), want just meta.json", entries, err)
	}
}

func TestServeWithoutMigrateSetsNoDir(t *testing.T) {
	e := newServeEnv(t)
	if rc, _, stderr := e.serve(t, e.request(t, []string{"dispatch"}, nil, nil)); rc != 0 {
		t.Fatalf("rc %d, stderr %q", rc, stderr)
	}
	if _, err := os.Stat(filepath.Join(e.dump, "migrate-path")); !os.IsNotExist(err) {
		t.Fatalf("DISPATCH_MIGRATE_DIR set without a migrate payload (%v)", err)
	}
}

func TestAdmitMigrate(t *testing.T) {
	e := newServeEnv(t)
	with := func(mut func(meta, payload map[string]any)) map[string]any {
		meta := testMeta()
		payload := map[string]any{"meta": meta, "transcript_gz": gzB64(t, []byte("{}\n"))}
		mut(meta, payload)
		return payload
	}
	setMeta := func(k string, v any) map[string]any {
		return with(func(meta, _ map[string]any) { meta[k] = v })
	}
	ok := with(func(_, _ map[string]any) {})
	big := make([]byte, 15<<20)
	if _, err := rand.Read(big); err != nil {
		t.Fatal(err)
	}
	var bomb bytes.Buffer
	zw, err := gzip.NewWriterLevel(&bomb, gzip.BestSpeed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := zw.Write(make([]byte, maxMigrateBlob+1)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		stdin string
		want  string
	}{
		{"valid", e.migrateRequest(t, migrateArgvOK, ok, ""), ""},
		{"deep tier", e.migrateRequest(t, []string{"dispatch", "deep", "opus"}, ok, ""), ""},
		{"sha256 tip", e.migrateRequest(t, migrateArgvOK, setMeta("tip", strings.Repeat("ab", 32)), ""), ""},
		{"mode now", e.migrateRequest(t, migrateArgvOK, setMeta("mode", "now"), ""), ""},
		{"no session no transcript", e.migrateRequest(t, migrateArgvOK, with(func(meta, p map[string]any) {
			delete(meta, "session")
			delete(p, "transcript_gz")
		}), ""), ""},
		{"20 MiB", e.migrateRequest(t, migrateArgvOK, with(func(_, p map[string]any) { p["patch_gz"] = gzB64(t, big) }), ""), ""},

		{"dispatch --engines", e.migrateRequest(t, []string{"dispatch", "--engines"}, ok, ""), "migrate is only allowed with dispatch <tier>"},
		{"dispatch resume", e.migrateRequest(t, []string{"dispatch", "resume", "feat/x"}, ok, ""), "migrate is only allowed"},
		{"dispatch --migrate-check", e.migrateRequest(t, []string{"dispatch", "--migrate-check", "feat/x"}, ok, ""), "migrate is only allowed"},
		{"bare dispatch", e.migrateRequest(t, []string{"dispatch"}, ok, ""), "migrate is only allowed"},
		{"crew reply", e.migrateRequest(t, []string{"crew", "reply", "worker:feat/x", "hi"}, ok, ""), "migrate is only allowed"},
		{"permission-check", e.migrateRequest(t, []string{"permission-check"}, ok, ""), "migrate is only allowed"},

		{"2 MiB without migrate", e.migrateRequest(t, []string{"dispatch"}, nil, strings.Repeat("a", 2<<20)), "request over 1 MiB"},
		{"2 MiB with null migrate", strings.Replace(e.migrateRequest(t, []string{"dispatch"}, nil, strings.Repeat("a", 2<<20)), `{`, `{"migrate":null,`, 1), "request over 1 MiB"},
		{"33 MiB", e.migrateRequest(t, migrateArgvOK, with(func(_, p map[string]any) { p["patch_gz"] = strings.Repeat("A", 33<<20) }), ""), "request over 32 MiB"},
		{"bomb", e.migrateRequest(t, migrateArgvOK, with(func(_, p map[string]any) {
			p["patch_gz"] = base64.StdEncoding.EncodeToString(bomb.Bytes())
		}), ""), "128 MiB"},
		{"not base64", e.migrateRequest(t, migrateArgvOK, with(func(_, p map[string]any) { p["patch_gz"] = "!!" }), ""), "patch_gz"},
		{"not gzip", e.migrateRequest(t, migrateArgvOK, with(func(_, p map[string]any) {
			p["transcript_gz"] = base64.StdEncoding.EncodeToString([]byte("plain"))
		}), ""), "transcript_gz"},
		{"unknown migrate key", e.migrateRequest(t, migrateArgvOK, with(func(_, p map[string]any) { p["files"] = "x" }), ""), `"files"`},
		{"case-folded migrate key", e.migrateRequest(t, migrateArgvOK, with(func(_, p map[string]any) {
			p["Patch_gz"] = p["transcript_gz"]
		}), ""), `"Patch_gz"`},
		{"no meta", e.migrateRequest(t, migrateArgvOK, map[string]any{}, ""), "meta"},
		{"meta not an object", e.migrateRequest(t, migrateArgvOK, map[string]any{"meta": "x"}, ""), "meta"},

		{"version", e.migrateRequest(t, migrateArgvOK, setMeta("v", 2), ""), "version"},
		{"no version", e.migrateRequest(t, migrateArgvOK, with(func(meta, _ map[string]any) { delete(meta, "v") }), ""), "version"},
		{"unknown meta key", e.migrateRequest(t, migrateArgvOK, setMeta("extra", 1), ""), `"extra"`},
		{"case-folded meta key", e.migrateRequest(t, migrateArgvOK, with(func(meta, _ map[string]any) {
			delete(meta, "branch")
			meta["Branch"] = "feat/x"
		}), ""), `"Branch"`},
		{"bad tip", e.migrateRequest(t, migrateArgvOK, setMeta("tip", "abc"), ""), "tip"},
		{"upper tip", e.migrateRequest(t, migrateArgvOK, setMeta("tip", strings.ToUpper(testTip)), ""), "tip"},
		{"tip 41", e.migrateRequest(t, migrateArgvOK, setMeta("tip", testTip+"0"), ""), "tip"},
		{"tip newline", e.migrateRequest(t, migrateArgvOK, setMeta("tip", testTip+"\n"), ""), "tip"},
		{"bad from_host", e.migrateRequest(t, migrateArgvOK, setMeta("from_host", "-x"), ""), "from_host"},
		{"empty from_host", e.migrateRequest(t, migrateArgvOK, setMeta("from_host", ""), ""), "from_host"},
		{"bad mode", e.migrateRequest(t, migrateArgvOK, setMeta("mode", "later"), ""), "mode"},
		{"bad session", e.migrateRequest(t, migrateArgvOK, setMeta("session", "s1-2"), ""), "session"},
		{"upper session", e.migrateRequest(t, migrateArgvOK, setMeta("session", strings.ToUpper(testSession)), ""), "session"},
		{"session without transcript", e.migrateRequest(t, migrateArgvOK, with(func(_, p map[string]any) { delete(p, "transcript_gz") }), ""), "session"},
		{"handoff over 64 KiB", e.migrateRequest(t, migrateArgvOK, setMeta("handoff", strings.Repeat("h", 64<<10+1)), ""), "handoff"},
		{"handoff nul", e.migrateRequest(t, migrateArgvOK, setMeta("handoff", "a\x00b"), ""), "NUL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := admit(context.Background(), strings.NewReader(tc.stdin))
			if tc.want == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
	for _, b := range []string{"", "-x", "a..b", "a b", "a\tb", "a~b", "a^b", "a:b", "a?b", "a*b", "a[b", `a\b`, "a/", "a.lock", "a//b", "a@{b", "a\x7fb"} {
		t.Run("branch "+b, func(t *testing.T) {
			_, _, err := admit(context.Background(), strings.NewReader(e.migrateRequest(t, migrateArgvOK, setMeta("branch", b), "")))
			if err == nil || !strings.Contains(err.Error(), "branch") {
				t.Fatalf("branch %q: err = %v", b, err)
			}
		})
	}
	if e.childRan() {
		t.Fatal("admit ran a child")
	}
	rc, _, stderr := e.serve(t, hostRequest(t, []string{"crew", "overseer", "status"}, func(m map[string]any) { m["migrate"] = ok }))
	if rc != 2 || !strings.Contains(stderr, "host scope") {
		t.Fatalf("host scope with migrate: rc %d, stderr %q", rc, stderr)
	}
}

func TestAdmitRequestVersion(t *testing.T) {
	e := newServeEnv(t)
	repoReq := func(v int, env map[string]string) string {
		data, err := json.Marshal(map[string]any{"v": v, "repo": e.repo, "slug": "acme/widget", "argv": []string{"crew", "where", "x"}, "env": env})
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	layout := map[string]string{"CREW_ID": "c1", "CREW_LAYOUT": "2"}
	for _, tc := range []struct {
		name, stdin, want string
	}{
		{"v1 layout", repoReq(1, layout), "crew: remote-exec: env CREW_LAYOUT needs request v2\n"},
		{"v1 crew scope", crewRequest(t, []string{"crew", "relay", "--crew", "c1"}, func(m map[string]any) { m["v"] = 1 }), "crew: remote-exec: scope crew needs request v2\n"},
		{"v0", repoReq(0, nil), "crew: remote-exec: unsupported request version 0 (this remote-exec speaks v1-v2)\n"},
		{"v3", repoReq(3, layout), "crew: remote-exec: unsupported request version 3 (this remote-exec speaks v1-v2)\n"},
		{"v2 bad layout", repoReq(2, map[string]string{"CREW_LAYOUT": "1"}), "CREW_LAYOUT"},
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
	rc, stdout, stderr := e.serve(t, repoReq(2, layout))
	if rc != 0 || stdout != "child-out\n" || stderr != "child-err\n" {
		t.Fatalf("v2 layout: rc %d, stdout %q, stderr %q", rc, stdout, stderr)
	}
	if got := e.dumped(t, "argv"); got != "crew\x00where\x00x\x00" {
		t.Fatalf("argv = %q", got)
	}
}

func TestServeLayoutEnvComesOnlyFromTheRequest(t *testing.T) {
	e := newServeEnv(t)
	t.Setenv("CREW_LAYOUT", "2")
	self := filepath.Join(t.TempDir(), "crew")
	writeScript(t, self, `echo "${CREW_LAYOUT-unset}"`)
	o := ServeOptions{
		LookPath: func(name string) (string, error) { return "", fmt.Errorf("%s looked up", name) },
		Self:     func() (string, error) { return self, nil },
	}
	req := func(v int, env map[string]string) string {
		data, err := json.Marshal(map[string]any{"v": v, "repo": e.repo, "slug": "acme/widget", "argv": []string{"crew", "where", "x"}, "env": env})
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	for _, tc := range []struct {
		name, stdin, want string
	}{
		{"v1", req(1, map[string]string{"CREW_ID": "c1"}), "unset\n"},
		{"v2", req(2, map[string]string{"CREW_ID": "c1", "CREW_LAYOUT": "2"}), "2\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rc, stdout, stderr := e.serveWith(t, tc.stdin, o)
			if rc != 0 || stdout != tc.want {
				t.Fatalf("rc %d, stdout %q, stderr %q, want stdout %q", rc, stdout, stderr, tc.want)
			}
		})
	}
}

// crewRequest is a crew-scope request for argv from crew c1 of origin halo;
// mut edits it before encoding.
func crewRequest(t *testing.T, argv []string, mut func(m map[string]any)) string {
	t.Helper()
	m := map[string]any{"v": 2, "scope": "crew", "repo": "", "slug": "", "argv": argv, "env": map[string]string{"CREW_ID": "c1", "DISPATCH_ORIGIN_HOST": "halo"}}
	if mut != nil {
		mut(m)
	}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// visitingState points the state root at a fresh dir holding crew c1 visiting
// from halo, crew own (not visiting) and crew far visiting from elsewhere.
func visitingState(t *testing.T) string {
	t.Helper()
	xdg := t.TempDir()
	t.Setenv("XDG_STATE_HOME", xdg)
	state := filepath.Join(xdg, "crew")
	for _, m := range []bus.Meta{
		{ID: "c1", HomeHost: "halo", Visiting: true, CreatedBy: "remote-dispatch"},
		{ID: "own", HomeHost: "halo", CreatedBy: "register"},
		{ID: "far", HomeHost: "elsewhere", Visiting: true, CreatedBy: "remote-dispatch"},
	} {
		if err := bus.CreateMeta(state, m); err != nil {
			t.Fatal(err)
		}
	}
	return xdg
}

func TestServeCrewScopeRefusals(t *testing.T) {
	e := newServeEnv(t)
	t.Setenv("HOME", t.TempDir())
	visitingState(t)
	relay := []string{"crew", "relay", "--crew", "c1"}
	withEnv := func(env map[string]string) func(m map[string]any) {
		return func(m map[string]any) { m["env"] = env }
	}
	asCrew := func(id string) func(m map[string]any) {
		return withEnv(map[string]string{"CREW_ID": id, "DISPATCH_ORIGIN_HOST": "halo"})
	}
	migrate := map[string]any{"meta": testMeta()}
	badMeta := filepath.Join(os.Getenv("XDG_STATE_HOME"), "crew", "crews", "bad", "meta.json")
	if err := os.MkdirAll(filepath.Dir(badMeta), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(badMeta, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	var rows []struct{ name, stdin, want string }
	for _, v := range crewVerbs {
		for _, tc := range v.refusals() {
			rows = append(rows, struct{ name, stdin, want string }{v.name + " " + tc.name, crewRequest(t, tc.argv, nil), "crew scope"})
		}
		rows = append(rows,
			struct{ name, stdin, want string }{v.name + " own crew", crewRequest(t, v.argv([]string{"--crew", "own"}, v.flags, []string{"--"}, v.pos), asCrew("own")), "crew own is this host's own crew"},
			struct{ name, stdin, want string }{v.name + " other origin", crewRequest(t, v.argv([]string{"--crew", "far"}, v.flags, []string{"--"}, v.pos), asCrew("far")), "crew far is visiting from elsewhere"})
	}
	for _, tc := range append(rows, []struct {
		name, stdin, want string
	}{
		{"repo", crewRequest(t, relay, func(m map[string]any) { m["repo"] = e.repo }), "a crew scope request takes no repo, slug, spec, stdin or migrate"},
		{"slug", crewRequest(t, relay, func(m map[string]any) { m["slug"] = "acme/widget" }), "crew scope"},
		{"spec", crewRequest(t, relay, func(m map[string]any) { m["spec"] = "s" }), "crew scope"},
		{"empty spec", crewRequest(t, relay, func(m map[string]any) { m["spec"] = "" }), "crew scope"},
		{"stdin", crewRequest(t, relay, func(m map[string]any) { m["stdin"] = "s" }), "crew scope"},
		{"migrate", crewRequest(t, relay, func(m map[string]any) { m["migrate"] = migrate }), "crew scope"},
		{"no CREW_ID", crewRequest(t, relay, withEnv(map[string]string{"DISPATCH_ORIGIN_HOST": "halo"})), "crew scope needs env CREW_ID and DISPATCH_ORIGIN_HOST"},
		{"no origin", crewRequest(t, relay, withEnv(map[string]string{"CREW_ID": "c1"})), "crew scope needs env CREW_ID and DISPATCH_ORIGIN_HOST"},
		{"extra env", crewRequest(t, relay, withEnv(map[string]string{"CREW_ID": "c1", "DISPATCH_ORIGIN_HOST": "halo", "DISPATCH_EMIT_ROW": "1"})), "crew scope needs env CREW_ID and DISPATCH_ORIGIN_HOST"},
		{"bad origin", crewRequest(t, relay, withEnv(map[string]string{"CREW_ID": "c1", "DISPATCH_ORIGIN_HOST": "-x"})), "DISPATCH_ORIGIN_HOST"},
		{"bad id", crewRequest(t, []string{"crew", "relay", "--crew", "../c1"}, asCrew("../c1")), "CREW_ID"},
		{"crew reply", crewRequest(t, []string{"crew", "reply", "--crew", "c1"}, nil), "needs -- before its positionals"},
		{"crew reap", crewRequest(t, []string{"crew", "reap", "--crew", "c1"}, nil), "argv not allowed in crew scope"},
		{"peer-sync", crewRequest(t, []string{"crew", "peer-sync", "--crew", "c1"}, nil), "argv not allowed in crew scope"},
		{"dispatch", crewRequest(t, []string{"dispatch", "--crew", "c1"}, nil), "argv not allowed in crew scope"},
		{"permission-check", crewRequest(t, []string{"permission-check", "--crew", "c1"}, nil), "argv not allowed in crew scope"},
		{"overseer", crewRequest(t, []string{"crew", "overseer", "status", "--crew", "c1"}, nil), "argv not allowed in crew scope"},
		{"empty argv", crewRequest(t, []string{}, nil), "argv not allowed in crew scope"},
		{"no --crew", crewRequest(t, []string{"crew", "relay", "--since", "0"}, nil), "--crew"},
		{"two --crew", crewRequest(t, []string{"crew", "relay", "--crew", "c1", "--crew", "c1"}, nil), "--crew"},
		{"--crew mismatch", crewRequest(t, []string{"crew", "relay", "--crew", "far"}, nil), "--crew"},
		{"--crew= mismatch", crewRequest(t, []string{"crew", "dispatch-row", "--crew=far"}, nil), "--crew"},
		{"no meta", crewRequest(t, []string{"crew", "relay", "--crew", "ghost"}, asCrew("ghost")), "crew: remote-exec: crew ghost is not a crew on this host\n"},
		{"garbled meta", crewRequest(t, []string{"crew", "relay", "--crew", "bad"}, asCrew("bad")), "crew: remote-exec: crew bad: "},
		{"own crew", crewRequest(t, []string{"crew", "relay", "--crew", "own"}, asCrew("own")), "crew: remote-exec: crew own is this host's own crew, not a visiting one\n"},
		{"other origin", crewRequest(t, []string{"crew", "relay", "--crew", "far"}, asCrew("far")), "crew: remote-exec: crew far is visiting from elsewhere, not halo\n"},
	}...) {
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
	t.Run("no state root", func(t *testing.T) {
		t.Setenv("XDG_STATE_HOME", "")
		t.Setenv("HOME", "")
		rc, _, stderr := e.serve(t, crewRequest(t, relay, nil))
		if rc != 2 || stderr != "crew: remote-exec: crew c1: no crew state root on this host (XDG_STATE_HOME and HOME unset)\n" {
			t.Fatalf("rc %d, stderr %q", rc, stderr)
		}
		if e.childRan() {
			t.Fatal("the child ran despite the refusal")
		}
	})
	t.Run("v1 repo scope nudge still admits", func(t *testing.T) {
		argv := []string{"crew", "nudge", "--crew", "c1", "--wait", "5", "--", "b"}
		if rc, _, stderr := e.serve(t, e.request(t, argv, map[string]string{"CREW_ID": "c1"}, nil)); rc != 0 {
			t.Fatalf("rc %d, stderr %q", rc, stderr)
		}
	})
}

func TestServeCrewScope(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	xdg := visitingState(t)
	cwd := t.TempDir()
	t.Chdir(cwd)
	t.Setenv("CREW_ID", "stale")
	t.Setenv("DISPATCH_ORIGIN_HOST", "stale")
	bin := t.TempDir()
	self := filepath.Join(bin, "crew")
	writeScript(t, self, `pwd -P
printf '%s %s %s\n' "$CREW_ID" "$DISPATCH_ORIGIN_HOST" "$XDG_STATE_HOME"
printf '%s\n' "$*"
echo crew-err >&2
exit 3
`)
	o := ServeOptions{
		LookPath: func(name string) (string, error) { return "", fmt.Errorf("%s looked up", name) },
		Self:     func() (string, error) { return self, nil },
	}
	for _, argv := range [][]string{
		{"crew", "relay", "--since", "0", "--crew", "c1"},
		{"crew", "dispatch-row", "--crew=c1", "--session", "s1-1"},
		{"crew", "reply", "--crew", "c1", "--no-nudge", "--", "worker:b#s1-1", "--crew=zzz"},
		{"crew", "reply", "--crew=c1", "--", "worker:b#s1-1", "body"},
		{"crew", "nudge", "--crew", "c1", "--wait", "30", "--", "b"},
		{"crew", "where", "--crew", "c1", "--", "b"},
	} {
		var stdout, stderr bytes.Buffer
		rc := Serve(strings.NewReader(crewRequest(t, argv, nil)), &stdout, &stderr, o)
		want := "/\nc1 halo " + xdg + "\n" + strings.Join(argv[1:], " ") + "\n"
		if rc != 3 || stdout.String() != want || stderr.String() != "crew-err\n" {
			t.Fatalf("%q: rc %d, stdout %q, stderr %q, want rc 3, stdout %q", argv, rc, stdout.String(), stderr.String(), want)
		}
	}
	for _, dir := range []string{home, cwd} {
		if _, err := os.Stat(filepath.Join(dir, "crew")); !os.IsNotExist(err) {
			t.Fatalf("a scratch root was made under %s (%v)", dir, err)
		}
	}
	if _, err := os.Stat(filepath.Join(xdg, "crew", "remote-exec")); !os.IsNotExist(err) {
		t.Fatalf("a scratch root was made under the state root (%v)", err)
	}
}

// A $HOME that is a git work tree (a dotfiles checkout) must not leak into a
// crew-scope child: it still starts in "/", where no repo is found.
func TestServeCrewScopeIgnoresWorkTreeHome(t *testing.T) {
	home := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", home).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	t.Setenv("HOME", home)
	visitingState(t)
	self := filepath.Join(t.TempDir(), "crew")
	writeScript(t, self, "pwd -P\n")
	o := ServeOptions{
		LookPath: func(name string) (string, error) { return "", fmt.Errorf("%s looked up", name) },
		Self:     func() (string, error) { return self, nil },
	}
	var stdout, stderr bytes.Buffer
	rc := Serve(strings.NewReader(crewRequest(t, []string{"crew", "where", "--crew", "c1", "--", "b"}, nil)), &stdout, &stderr, o)
	if rc != 0 || stdout.String() != "/\n" || stderr.String() != "" {
		t.Fatalf("rc %d, stdout %q, stderr %q, want rc 0, stdout \"/\\n\"", rc, stdout.String(), stderr.String())
	}
}
