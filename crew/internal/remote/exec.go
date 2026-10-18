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
	"slices"
	"sort"
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
var requestKeys = []string{"v", "repo", "slug", "argv", "env", "spec"}

// ServeOptions is how remote-exec finds the programs it runs.
type ServeOptions struct {
	LookPath func(string) (string, error) // `dispatch` and `permission-check`
	Self     func() (string, error)       // this crew binary, for `crew …`
}

// DefaultServeOptions resolves programs on PATH and `crew` as this executable.
func DefaultServeOptions() ServeOptions {
	return ServeOptions{LookPath: exec.LookPath, Self: os.Executable}
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
	defer func() { _ = os.RemoveAll(scratch) }()

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
		return runDetached(cmd, scratch, stdout, stderr)
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
