package remote

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/claimindex"
)

// MaxRequest is the most stdin remote-exec reads.
const MaxRequest = 1 << 20

// scratchTTL bounds the scratch dirs a hung-up remote-exec leaves behind.
const scratchTTL = 24 * time.Hour

// requestKeys are the exact top-level keys a request may carry. encoding/json
// matches field names case-insensitively, so DisallowUnknownFields alone would
// let `"Repo"` through.
var requestKeys = []string{"v", "repo", "slug", "argv", "env", "spec", "stdin"}

// ServeOptions is how remote-exec finds the programs it runs.
type ServeOptions struct {
	LookPath func(string) (string, error) // `dispatch` and `permission-check`
	Self     func() (string, error)       // this crew binary, for `crew …`
	// ViaTmux runs `dispatch` through the running tmux server, so it gets that
	// server's login session: on macOS a bare ssh session cannot read the
	// login keychain.
	ViaTmux bool
}

// DefaultServeOptions resolves programs on PATH and `crew` as this executable.
func DefaultServeOptions() ServeOptions {
	return ServeOptions{LookPath: exec.LookPath, Self: os.Executable, ViaTmux: runtime.GOOS == "darwin"}
}

// Serve is `crew remote-exec`: it reads one request from stdin, checks it, and
// runs its argv in the requested clone, returning the child's status (128+n
// for signal n) or 2 for a refused request. It never depends on its own cwd:
// sshd starts it in $HOME.
func Serve(stdin io.Reader, stdout, stderr io.Writer, o ServeOptions) int {
	ctx := context.Background()
	req, common, err := admit(ctx, stdin)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "crew: remote-exec: %v\n", err)
		return exitUsage
	}
	scratch, err := newScratch(common + "/crew/remote-exec")
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "crew: remote-exec: %v\n", err)
		return 1
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.RemoveAll(scratch)
		}
	}()

	cmd, err := command(req.Argv, o)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "crew: remote-exec: %v\n", err)
		return 127
	}
	cmd.Dir = req.Repo
	cmd.Env = os.Environ()
	keys := make([]string, 0, len(req.Env))
	for k := range req.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		cmd.Env = append(cmd.Env, k+"="+req.Env[k])
	}
	if req.Spec != nil {
		spec := scratch + "/spec"
		if err := writePrivate(spec, []byte(*req.Spec)); err != nil {
			_, _ = fmt.Fprintf(stderr, "crew: remote-exec: %v\n", err)
			return 1
		}
		cmd.Env = append(cmd.Env, "DISPATCH_SPEC="+spec)
	}

	if req.Argv[0] == "dispatch" {
		if o.ViaTmux {
			rc, ok, k := runInTmux(cmd, scratch, o, stdout, stderr)
			if ok {
				keep = k
				return rc
			}
		}
		return runDetached(cmd, scratch, stdout, stderr)
	}
	if req.Stdin != nil {
		cmd.Stdin = strings.NewReader(*req.Stdin)
	}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return reportStatus(cmd.Run(), stderr, "crew: remote-exec: "+req.Argv[0])
}

// admit decodes and checks a request, returning it and its clone's git common
// dir. Nothing runs before every check has passed.
func admit(ctx context.Context, stdin io.Reader) (Request, string, error) {
	req, err := decode(stdin)
	if err != nil {
		return Request{}, "", err
	}
	common, err := checkRepo(ctx, req.Repo, req.Slug)
	if err != nil {
		return Request{}, "", err
	}
	if !allowedArgv(req.Argv) {
		return Request{}, "", fmt.Errorf("argv not allowed: %q", req.Argv[:min(len(req.Argv), 2)])
	}
	if req.Stdin != nil && !slices.Equal(req.Argv, []string{"crew", "peer-sync"}) {
		return Request{}, "", errors.New("stdin is only allowed with crew peer-sync")
	}
	for k, v := range req.Env {
		if err := CheckEnv(k, v); err != nil {
			return Request{}, "", err
		}
	}
	return req, common, nil
}

func decode(stdin io.Reader) (Request, error) {
	data, err := io.ReadAll(io.LimitReader(stdin, MaxRequest+1))
	if err != nil {
		return Request{}, fmt.Errorf("reading the request: %v", err)
	}
	if len(data) > MaxRequest {
		return Request{}, errors.New("request over 1 MiB")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	var top map[string]json.RawMessage
	if err := dec.Decode(&top); err != nil {
		return Request{}, fmt.Errorf("invalid request: %v", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return Request{}, errors.New("request must be exactly one JSON object")
	}
	for k := range top {
		if !slices.Contains(requestKeys, k) {
			return Request{}, fmt.Errorf("unknown request key %q", k)
		}
	}
	var req Request
	strict := json.NewDecoder(bytes.NewReader(data))
	strict.DisallowUnknownFields()
	if err := strict.Decode(&req); err != nil {
		return Request{}, fmt.Errorf("invalid request: %v", err)
	}
	if req.V != 1 {
		return Request{}, fmt.Errorf("unsupported request version %d", req.V)
	}
	if hasNUL(req) {
		return Request{}, errors.New("request string contains NUL")
	}
	return req, nil
}

func hasNUL(req Request) bool {
	strs := append([]string{req.Repo, req.Slug}, req.Argv...)
	for k, v := range req.Env {
		strs = append(strs, k, v)
	}
	if req.Spec != nil {
		strs = append(strs, *req.Spec)
	}
	if req.Stdin != nil {
		strs = append(strs, *req.Stdin)
	}
	return slices.ContainsFunc(strs, func(s string) bool { return strings.ContainsRune(s, 0) })
}

// checkRepo verifies repo is a checkout whose origin derives to slug, and
// returns its git common dir.
func checkRepo(ctx context.Context, repo, slug string) (string, error) {
	if !filepath.IsAbs(repo) {
		return "", fmt.Errorf("repo %q is not absolute", repo)
	}
	if st, err := os.Stat(repo); err != nil || !st.IsDir() {
		return "", fmt.Errorf("%s is not a directory", repo)
	}
	paths, err := bus.Locate(ctx, repo)
	if err != nil {
		return "", fmt.Errorf("%s is not inside a git checkout", repo)
	}
	url := originURL(ctx, paths.Common)
	got := claimindex.Slug(url)
	if got == "" || !RepoKeyRe.MatchString(slug) || got != strings.ToLower(slug) {
		if url == "" {
			url = "none"
		}
		return "", fmt.Errorf("%s is not a clone of %s (origin: %s)", repo, slug, url)
	}
	return paths.Common, nil
}

// allowedArgv is the argv allowlist, matched on the first one or two words.
func allowedArgv(argv []string) bool {
	if len(argv) == 0 {
		return false
	}
	switch argv[0] {
	case "dispatch", "permission-check":
		return true
	case "crew":
		return len(argv) >= 2 && slices.Contains(CrewSubcommands, argv[1])
	}
	return false
}

// command builds the child's exec.Cmd: argv is exec'd directly, never through
// a shell.
func command(argv []string, o ServeOptions) (*exec.Cmd, error) {
	if argv[0] == "crew" {
		self, err := o.Self()
		if err != nil {
			return nil, fmt.Errorf("crew: %v", err)
		}
		return exec.Command(self, argv[1:]...), nil
	}
	path, err := o.LookPath(argv[0])
	if err != nil {
		return nil, fmt.Errorf("%s: %v", argv[0], err)
	}
	return exec.Command(path, argv[1:]...), nil
}

// newScratch sweeps root's entries older than scratchTTL, then makes this
// request's own `<pid>-<rand>` dir under it.
func newScratch(root string) (string, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", err
	}
	cutoff := time.Now().Add(-scratchTTL)
	for _, e := range entries {
		if info, err := e.Info(); err == nil && info.ModTime().Before(cutoff) {
			_ = os.RemoveAll(filepath.Join(root, e.Name()))
		}
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	dir := filepath.Join(root, fmt.Sprintf("%d-%s", os.Getpid(), hex.EncodeToString(b[:])))
	if err := os.Mkdir(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

func createPrivate(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
}

func writePrivate(path string, data []byte) error {
	f, err := createPrivate(path)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// runDetached runs a dispatch so a dropped link cannot kill it halfway: its own
// session, stdin /dev/null, and output to files, copied out once it exits. If
// this process dies first, the dispatch still runs to completion and the sweep
// later removes its files.
func runDetached(cmd *exec.Cmd, scratch string, stdout, stderr io.Writer) int {
	outPath, errPath := scratch+"/stdout", scratch+"/stderr"
	outF, err := createPrivate(outPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "crew: remote-exec: %v\n", err)
		return 1
	}
	errF, err := createPrivate(errPath)
	if err != nil {
		_ = outF.Close()
		_, _ = fmt.Fprintf(stderr, "crew: remote-exec: %v\n", err)
		return 1
	}
	cmd.Stdout = outF
	cmd.Stderr = errF
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	runErr := cmd.Run()
	_ = outF.Close()
	_ = errF.Close()
	copyOut(stdout, outPath)
	copyOut(stderr, errPath)
	return reportStatus(runErr, stderr, "crew: remote-exec: dispatch")
}

var envKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// tmuxStartTimeout bounds the wait for a tmux job to start; tmuxPoll is how
// often runInTmux looks for its progress files.
var (
	tmuxStartTimeout = 10 * time.Second
	tmuxPoll         = 100 * time.Millisecond
)

// runInTmux runs a dispatch as a job of the running tmux server, which keeps
// it alive past a dropped link much as runDetached does. ok is false only when
// the job could not be started, before anything in scratch that runDetached
// creates exists, so the caller can fall back to it. keep is true when the job
// may still be running, so scratch must outlive this process.
func runInTmux(cmd *exec.Cmd, scratch string, o ServeOptions, stdout, stderr io.Writer) (rc int, ok, keep bool) {
	fallback := func(reason string) (int, bool, bool) {
		_, _ = fmt.Fprintf(stderr, "crew: remote-exec: not running dispatch in the tmux server (%s); running it in this ssh session\n", reason)
		return 0, false, false
	}
	tmux, err := o.LookPath("tmux")
	if err != nil {
		return fallback(err.Error())
	}
	if !runShellSafe(scratch) {
		return fallback(fmt.Sprintf("scratch path %q cannot be quoted for run-shell", scratch))
	}
	envPath, reason := safeLookPath("env")
	if reason != "" {
		return fallback(reason)
	}
	shPath, reason := safeLookPath("sh")
	if reason != "" {
		return fallback(reason)
	}
	rmPath, reason := safeLookPath("rm")
	if reason != "" {
		return fallback(reason)
	}
	runPath, startedPath := scratch+"/run", scratch+"/started"
	outPath, errPath, statusPath := scratch+"/stdout", scratch+"/stderr", scratch+"/status"
	old := syscall.Umask(0)
	syscall.Umask(old)

	// Everything that can kill a shell (an export of a readonly variable, a
	// failed cd) runs in the subshell, so the outer one always records the
	// status. The outer one has no PATH: builtins and absolute paths only. It
	// unlinks the script first, as that holds the whole environment.
	var b strings.Builder
	b.WriteString("umask 077\n")
	fmt.Fprintf(&b, "%s -f %s\n", shQuote(rmPath), shQuote(runPath))
	fmt.Fprintf(&b, "printf '%%s\\n' \"$$\" >%s\n", shQuote(startedPath))
	fmt.Fprintf(&b, "( umask %04o\n", old)
	for _, kv := range cmd.Env {
		k, v, _ := strings.Cut(kv, "=")
		if envKeyRe.MatchString(k) {
			fmt.Fprintf(&b, "export %s=%s\n", k, shQuote(v))
		}
	}
	fmt.Fprintf(&b, "cd %s && exec %s", shQuote(cmd.Dir), shQuote(cmd.Path))
	for _, a := range cmd.Args[1:] {
		b.WriteByte(' ')
		b.WriteString(shQuote(a))
	}
	fmt.Fprintf(&b, " ) </dev/null >%s 2>%s\n", shQuote(outPath), shQuote(errPath))
	fmt.Fprintf(&b, "printf '%%s\\n' \"$?\" >%s\n", shQuote(statusPath))
	// A non-zero exit would have tmux print "returned N" into a pane.
	b.WriteString("exit 0\n")
	if err := writePrivate(runPath, []byte(b.String())); err != nil {
		return fallback(err.Error())
	}

	// Never run-shell without -b: a keychain dialog would wedge the server's
	// command queue until it is answered.
	var runErr bytes.Buffer
	start := exec.Command(tmux, "run-shell", "-b",
		fmt.Sprintf(`exec "%s" -i "%s" "%s" >/dev/null 2>&1`, envPath, shPath, runPath))
	start.Env = cmd.Env
	start.Stderr = &runErr
	if err := start.Run(); err != nil {
		_ = os.Remove(runPath)
		reason := strings.TrimSpace(runErr.String())
		if reason == "" {
			reason = err.Error()
		}
		return fallback("tmux run-shell: " + reason)
	}

	pid, started := readInt(startedPath)
	for deadline := time.Now().Add(tmuxStartTimeout); !started; pid, started = readInt(startedPath) {
		if time.Now().After(deadline) {
			_ = os.Remove(runPath)
			_, _ = fmt.Fprintln(stderr, "crew: remote-exec: dispatch: the tmux job never started (check the tmux server's default-shell); if it starts late, recover with dispatch --host … --recover")
			return 255, true, true
		}
		time.Sleep(tmuxPoll)
	}
	for {
		// Checked before the status, so a job that wrote it and then exited
		// is never taken for one that died without it.
		gone := errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
		if rc, ok := readInt(statusPath); ok {
			copyOut(stdout, outPath)
			copyOut(stderr, errPath)
			return rc, true, false
		}
		if gone {
			_, _ = fmt.Fprintln(stderr, "crew: remote-exec: dispatch: the tmux job ended without an exit status; the dispatch may still be running — recover it")
			return 255, true, true
		}
		time.Sleep(tmuxPoll)
	}
}

// readInt reads the integer a job wrote to path, false while it has not.
func readInt(path string) (int, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	rc, err := strconv.Atoi(strings.TrimSpace(string(data)))
	return rc, err == nil
}

// safeLookPath resolves name on PATH to a path run-shell can take, or says
// why it cannot.
func safeLookPath(name string) (string, string) {
	path, err := exec.LookPath(name)
	if err != nil {
		return "", err.Error()
	}
	if !runShellSafe(path) {
		return "", fmt.Sprintf("%s path %q cannot be quoted for run-shell", name, path)
	}
	return path, ""
}

// runShellSafe reports whether s may go double-quoted into a run-shell
// command: tmux expands #{} formats in it and may hand it to the server's
// default-shell (fish, say) rather than sh, and both shells read a
// double-quoted string alike unless it holds one of these.
func runShellSafe(s string) bool {
	return !strings.ContainsFunc(s, func(r rune) bool {
		return strings.ContainsRune("$\"\\`#", r) || r < 0x20 || r == 0x7f
	})
}

// shQuote single-quotes s for POSIX sh.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// copyOut relays a finished child's output file; a hung-up reader is no
// reason to fail the child's status.
func copyOut(w io.Writer, path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	_, _ = io.Copy(w, f)
}
