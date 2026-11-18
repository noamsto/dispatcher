// Package remote is the ssh transport to a worker on another host: the
// `hosts` settings, the route a local dispatch row names, the `crew
// remote-run` client and the `crew remote-exec` server.
//
// This is trust-boundary code. The remote command line is always the literal
// `crew remote-exec`; every caller-supplied byte travels as one JSON request on
// ssh's stdin, and the server execs an allowlisted argv, never through a shell.
// The ssh target and the remote path come only from settings, never from a
// bus row.
package remote

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"syscall"

	"github.com/noamsto/dispatcher/crew/internal/bus"
	"github.com/noamsto/dispatcher/crew/internal/claimindex"
	"github.com/noamsto/dispatcher/crew/internal/git"
	"github.com/noamsto/dispatcher/crew/internal/gitguard"
	"github.com/noamsto/dispatcher/crew/internal/tmux"
	"github.com/noamsto/dispatcher/crew/internal/wtgit"
)

// The settings and request patterns, shared by the client and the server.
// \A..\z rather than ^..$, so a trailing newline never passes.
var (
	HostNameRe = regexp.MustCompile(`\A[A-Za-z0-9][A-Za-z0-9._-]*\z`)
	// SSHRe can never start with '-', so the target is never read as an ssh option.
	SSHRe      = regexp.MustCompile(`\A[A-Za-z0-9_][A-Za-z0-9._@%+-]*\z`)
	RepoKeyRe  = regexp.MustCompile(`\A[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+\z`)
	RepoPathRe = regexp.MustCompile(`\A/[^\x00-\x1f\x7f]*\z`)
	SessionRe  = regexp.MustCompile(`\As[0-9]+-[0-9]+\z`)
)

// EnvAllow is every env key a request may set, with the pattern its value must
// match. CREW_WORKER_ID and CREW_ROLE_ID are deliberately absent: the server
// neither sets nor clears them, so a worker's own env still holds it to the
// single-host sender tripwire.
var EnvAllow = map[string]*regexp.Regexp{
	"CREW_ID":               regexp.MustCompile(`\A[A-Za-z0-9_-]+\z`),
	"DISPATCH_TMUX_SESSION": regexp.MustCompile(`\A[A-Za-z0-9_][A-Za-z0-9_-]*\z`),
	"DISPATCH_ORIGIN_HOST":  regexp.MustCompile(`\A[A-Za-z0-9][A-Za-z0-9_-]*\z`),
	"DISPATCH_EMIT_ROW":     regexp.MustCompile(`\A1\z`),
	"DISPATCH_SESSION_ID":   SessionRe,
	"CREW_IDENTITY_AVOID":   regexp.MustCompile(`\A[A-Za-z0-9-]+(,[A-Za-z0-9-]+)*\z`),
}

// CrewSubcommands are the `crew` subcommands a request may run.
var CrewSubcommands = []string{"reply", "nudge", "where", "roster", "relay", "reap", "dispatch-row", "peer-sync"}

// HostArgv are the argv prefixes a host-scope request may run. Host scope
// reaches nothing else, and these are not in CrewSubcommands, so a repo-scope
// request never reaches them.
var HostArgv = [][]string{{"crew", "overseer", "status"}, {"crew", "overseer", "deliver"}}

// scopeHost marks a request that names no clone and runs a HostArgv command.
const scopeHost = "host"

func allowedHostArgv(argv []string) bool {
	return slices.ContainsFunc(HostArgv, func(p []string) bool {
		return len(argv) >= len(p) && slices.Equal(argv[:len(p)], p)
	})
}

const exitUsage = 2

// Host is one `hosts.<name>` settings entry.
type Host struct {
	SSH   string            `json:"ssh"`
	Repos map[string]string `json:"repos"`
	Peer  bool              `json:"peer"`
}

// Target is where a call lands: the ssh destination and the remote clone.
type Target struct {
	SSH  string
	Repo string
}

// Request is the JSON object remote-run writes to ssh's stdin and remote-exec
// reads. Field order is the wire order.
type Request struct {
	V       int               `json:"v"`
	Scope   string            `json:"scope,omitempty"` // "" for repo scope, or scopeHost
	Repo    string            `json:"repo"`
	Slug    string            `json:"slug"`
	Argv    []string          `json:"argv"`
	Env     map[string]string `json:"env"`
	Spec    *string           `json:"spec,omitempty"`
	Stdin   *string           `json:"stdin,omitempty"`
	Migrate *Migrate          `json:"migrate,omitempty"`

	unpacked *migrateFiles // Migrate decoded and checked by admit
}

// Migrate is the bundle `dispatch migrate` moves a worker with. Each blob is
// a file gzipped then base64-encoded, "" when the file is absent.
type Migrate struct {
	Meta         json.RawMessage `json:"meta"`
	PatchGz      string          `json:"patch_gz,omitempty"`
	TranscriptGz string          `json:"transcript_gz,omitempty"`
}

// MigrateMeta is a migrate bundle's meta.json.
type MigrateMeta struct {
	V        int    `json:"v"`
	Branch   string `json:"branch"`
	Tip      string `json:"tip"`
	FromHost string `json:"from_host"`
	Mode     string `json:"mode"`
	Session  string `json:"session,omitempty"`
	Handoff  string `json:"handoff"`
}

const maxHandoff = 64 << 10

var (
	migrateKeys = []string{"meta", "patch_gz", "transcript_gz"}
	metaKeys    = []string{"v", "branch", "tip", "from_host", "mode", "session", "handoff"}
	tipRe       = regexp.MustCompile(`\A[0-9a-f]{40}([0-9a-f]{24})?\z`)
	uuidRe      = regexp.MustCompile(`\A[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\z`)
)

// checkKeys refuses a key of the JSON object raw outside keys, for the reason
// requestKeys gives.
func checkKeys(raw []byte, keys []string, what string) error {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return fmt.Errorf("invalid %s: %v", what, err)
	}
	for k := range obj {
		if !slices.Contains(keys, k) {
			return fmt.Errorf("unknown %s key %q", what, k)
		}
	}
	return nil
}

// DecodeMigrateMeta strictly decodes and checks a migrate bundle's meta.json;
// a session is allowed only with a transcript to resume it from.
func DecodeMigrateMeta(raw []byte, transcript bool) (MigrateMeta, error) {
	if err := checkKeys(raw, metaKeys, "migrate meta"); err != nil {
		return MigrateMeta{}, err
	}
	var m MigrateMeta
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return MigrateMeta{}, fmt.Errorf("invalid migrate meta: %v", err)
	}
	switch {
	case m.V != 1:
		return MigrateMeta{}, fmt.Errorf("unsupported migrate meta version %d", m.V)
	case !refSafe(m.Branch):
		return MigrateMeta{}, fmt.Errorf("migrate meta branch %q is not a safe branch name", m.Branch)
	case !tipRe.MatchString(m.Tip):
		return MigrateMeta{}, fmt.Errorf("migrate meta tip %q is not a commit id", m.Tip)
	case !HostNameRe.MatchString(m.FromHost):
		return MigrateMeta{}, fmt.Errorf("migrate meta from_host %q is not a host name", m.FromHost)
	case m.Mode != "coop" && m.Mode != "now":
		return MigrateMeta{}, fmt.Errorf("migrate meta mode %q is not coop or now", m.Mode)
	case m.Session != "" && !uuidRe.MatchString(m.Session):
		return MigrateMeta{}, fmt.Errorf("migrate meta session %q is not a UUID", m.Session)
	case m.Session != "" && !transcript:
		return MigrateMeta{}, errors.New("migrate meta session needs a transcript")
	case len(m.Handoff) > maxHandoff:
		return MigrateMeta{}, errors.New("migrate meta handoff over 64 KiB")
	case strings.ContainsRune(m.Handoff, 0):
		return MigrateMeta{}, errors.New("migrate meta handoff contains NUL")
	}
	return m, nil
}

// refSafe reports whether b is a branch name git takes as is and no git
// command reads as an option or a revision expression.
func refSafe(b string) bool {
	if b == "" || strings.HasPrefix(b, "-") || strings.HasSuffix(b, "/") || strings.HasSuffix(b, ".lock") {
		return false
	}
	for _, bad := range []string{"..", "//", "@{"} {
		if strings.Contains(b, bad) {
			return false
		}
	}
	return !strings.ContainsFunc(b, func(r rune) bool {
		return r <= ' ' || r == 0x7f || strings.ContainsRune(`~^:?*[\`, r)
	})
}

// ConfigBin is the dispatch-config command: $DISPATCH_CONFIG_BIN, else
// `dispatch-config` on PATH.
func ConfigBin() string {
	if bin := os.Getenv("DISPATCH_CONFIG_BIN"); bin != "" {
		return bin
	}
	return "dispatch-config"
}

// Load runs dispatch-config and returns its `hosts`, re-validated with the
// same patterns dispatch-config applies. Its stderr goes to stderr. Errors
// carry no command prefix: each caller adds its own.
func Load(ctx context.Context, bin string, stderr io.Writer) (map[string]Host, error) {
	cmd := exec.CommandContext(ctx, bin)
	cmd.Stderr = stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("could not resolve the dispatcher settings (%s: %w)", bin, err)
	}
	var settings struct {
		Hosts map[string]Host `json:"hosts"`
	}
	if err := json.Unmarshal(out, &settings); err != nil {
		return nil, fmt.Errorf("settings hosts: %v", err)
	}
	for name, h := range settings.Hosts {
		if err := validHost(name, h); err != nil {
			return nil, err
		}
	}
	return settings.Hosts, nil
}

func validHost(name string, h Host) error {
	if !HostNameRe.MatchString(name) {
		return fmt.Errorf("settings hosts: invalid host name %q", name)
	}
	if !SSHRe.MatchString(h.SSH) {
		return fmt.Errorf("settings hosts.%s.ssh: invalid ssh destination %q", name, h.SSH)
	}
	for key, path := range h.Repos {
		if !RepoKeyRe.MatchString(key) {
			return fmt.Errorf("settings hosts.%s.repos: invalid key %q (want owner/repo)", name, key)
		}
		if !RepoPathRe.MatchString(path) {
			return fmt.Errorf("settings hosts.%s.repos.%s: %q is not an absolute path without control characters", name, key, path)
		}
	}
	return nil
}

func lookupHost(hosts map[string]Host, name string) (Host, error) {
	h, ok := hosts[name]
	if !ok {
		return Host{}, fmt.Errorf("unknown host '%s' (no hosts.%s in settings)", name, name)
	}
	return h, nil
}

// Resolve finds the ssh destination for host name and its clone of slug.
// Repo keys match slug case-insensitively, as claimindex.Slug lowercases.
func Resolve(hosts map[string]Host, name, slug string) (Target, error) {
	h, err := lookupHost(hosts, name)
	if err != nil {
		return Target{}, fmt.Errorf("crew: remote-run: %w", err)
	}
	keys := make([]string, 0, len(h.Repos))
	for k := range h.Repos {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if strings.EqualFold(k, slug) {
			return Target{SSH: h.SSH, Repo: h.Repos[k]}, nil
		}
	}
	return Target{}, fmt.Errorf("crew: remote-run: host '%s' has no clone mapped for %s (settings hosts.%s.repos)", name, slug, name)
}

// SSHArgv is ssh's argv after the program name. Forwarding is forced off
// whatever ssh_config says, so no dispatcher-side credential reaches the
// remote, and the remote command is the fixed literal `crew remote-exec`.
func SSHArgv(dest string) []string {
	return []string{
		"-T",
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=15",
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=3",
		"-o", "ForwardAgent=no",
		"-o", "ForwardX11=no",
		"-o", "ClearAllForwardings=yes",
		"-o", "PermitLocalCommand=no",
		"--", dest, "crew", "remote-exec",
	}
}

// CheckEnv refuses a key outside EnvAllow or a value failing its pattern.
func CheckEnv(key, value string) error {
	re, ok := EnvAllow[key]
	if !ok {
		return fmt.Errorf("env key %s is not allowed", key)
	}
	if !re.MatchString(value) {
		return fmt.Errorf("env %s value %q does not match %s", key, value, re)
	}
	return nil
}

// BuildRequest encodes a request. env holds KEY=VALUE pairs; spec and migrate
// are nil when there is none.
func BuildRequest(repo, slug string, argv, env []string, spec *string, migrate *Migrate) ([]byte, error) {
	if len(argv) == 0 {
		return nil, errors.New("crew: remote-run: no command given after --")
	}
	vars := map[string]string{}
	for _, kv := range env {
		key, value, ok := strings.Cut(kv, "=")
		if !ok {
			return nil, fmt.Errorf("crew: remote-run: --env %q is not KEY=VALUE", kv)
		}
		if err := CheckEnv(key, value); err != nil {
			return nil, fmt.Errorf("crew: remote-run: --%v", err)
		}
		vars[key] = value
	}
	return json.Marshal(Request{V: 1, Repo: repo, Slug: slug, Argv: argv, Env: vars, Spec: spec, Migrate: migrate})
}

// BuildHostRequest encodes a host-scope request for argv, which must match
// HostArgv.
func BuildHostRequest(argv []string) ([]byte, error) {
	if !allowedHostArgv(argv) {
		return nil, fmt.Errorf("argv not allowed in host scope: %q", argv[:min(len(argv), 3)])
	}
	return json.Marshal(Request{V: 1, Scope: scopeHost, Argv: argv, Env: map[string]string{}})
}

// dispatchRow is the part of a bus row the route reads.
func dispatchRow(e bus.Event, crew string) (map[string]any, bool) {
	if e.Kind != bus.KindDispatch || e.CrewID != crew {
		return nil, false
	}
	doc, ok := e.Doc.(map[string]any)
	return doc, ok
}

func isRemote(doc map[string]any) (string, bool) {
	host, _ := doc["host"].(string)
	return host, doc["remote"] == true
}

// Route is the host a branch's calls go to: the host of its newest dispatch row
// in crew, when that row is remote:true, else "" (a local worker).
func Route(rows []bus.Event, branch, crew string) string {
	for i := len(rows) - 1; i >= 0; i-- {
		doc, ok := dispatchRow(rows[i], crew)
		if !ok || rows[i].Branch != branch {
			continue
		}
		if host, remote := isRemote(doc); remote {
			return host
		}
		return ""
	}
	return ""
}

// RouteAny is Route for a caller with no crew: the host and crew of the one
// crew whose newest dispatch row for branch is remote:true. When several crews
// dispatched the branch and one of them is remote, the target could be either
// crew's worker, so it is refused rather than guessed.
func RouteAny(rows []bus.Event, branch string) (host, crew string, err error) {
	newest := map[string]map[string]any{}
	for _, e := range rows {
		if e.CrewID == "" || e.Branch != branch {
			continue
		}
		if doc, ok := dispatchRow(e, e.CrewID); ok {
			newest[e.CrewID] = doc
		}
	}
	for c, doc := range newest {
		if h, remote := isRemote(doc); remote && h != "" {
			host, crew = h, c
		}
	}
	if host == "" {
		return "", "", nil
	}
	if len(newest) > 1 {
		crews := slices.Sorted(maps.Keys(newest))
		return "", "", fmt.Errorf("%s is dispatched in crews %s, one of them on a remote host — pass --crew <id>", branch, strings.Join(crews, ", "))
	}
	return host, crew, nil
}

// RemoteHosts is the sorted distinct hosts of crew's remote:true dispatch rows.
func RemoteHosts(rows []bus.Event, crew string) []string {
	var hosts []string
	for _, e := range rows {
		doc, ok := dispatchRow(e, crew)
		if !ok {
			continue
		}
		if host, remote := isRemote(doc); remote && host != "" && !slices.Contains(hosts, host) {
			hosts = append(hosts, host)
		}
	}
	sort.Strings(hosts)
	return hosts
}

// Call is one remote-run invocation. SpecFile and MigrateDir are "" for none;
// a relative path is read from Cwd.
type Call struct {
	Host       string
	Cwd        string
	Env        []string
	SpecFile   string
	MigrateDir string
	Argv       []string
}

// Run is `crew remote-run`: it sends c to its host over ssh and returns ssh's
// status, or 2 for a refusal before any ssh.
func Run(ctx context.Context, c Call, stdout, stderr io.Writer) int {
	target, req, err := prepare(ctx, c, stderr)
	if err != nil {
		if !errors.Is(err, gitguard.ErrRefused) {
			_, _ = fmt.Fprintln(stderr, err)
		}
		return exitUsage
	}
	return viaSSH(ctx, c.Host, target.SSH, req, stdout, stderr, "crew: remote-run: ")
}

// RunHost sends a host-scope request for argv to host over ssh and returns
// ssh's status, or 2 for a refusal before any ssh. Its own messages carry no
// command prefix, so a caller can print them as its own.
func RunHost(ctx context.Context, host string, argv []string, stdout, stderr io.Writer) int {
	hosts, err := Load(ctx, ConfigBin(), stderr)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return exitUsage
	}
	h, err := lookupHost(hosts, host)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return exitUsage
	}
	req, err := BuildHostRequest(argv)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return exitUsage
	}
	return viaSSH(ctx, host, h.SSH, req, stdout, stderr, "")
}

// viaSSH runs `crew remote-exec` at dest with req on its stdin; prefix leads
// each message it writes itself.
func viaSSH(ctx context.Context, host, dest string, req []byte, stdout, stderr io.Writer, prefix string) int {
	cmd := exec.CommandContext(ctx, "ssh", SSHArgv(dest)...)
	cmd.Stdin = bytes.NewReader(req)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	rc := reportStatus(cmd.Run(), stderr, prefix+"ssh")
	if rc == 255 {
		_, _ = fmt.Fprintf(stderr, "%sssh to host '%s' (%s) failed (exit 255)\n", prefix, host, dest)
	}
	return rc
}

// paneBranch is the @crew_branch of a tmux pane; a test seam.
var paneBranch = func(ctx context.Context, pane string) string {
	return tmux.Output(ctx, tmux.Exec{}, "display-message", "-p", "-t", pane, "#{@crew_branch}")
}

// chromeFromWorker reports a dispatch --chrome call made from a worker or role
// pane. A tripwire on the sanctioned path, not authentication (the env can be
// unset): it mirrors dispatch's local refusal, which the remote cannot repeat
// because it cannot see the caller's env. The scan stops at the first `--` so a
// title is never read as a flag; a `--` that is itself a flag value is not handled.
func chromeFromWorker(ctx context.Context, argv []string) bool {
	if len(argv) == 0 || argv[0] != "dispatch" {
		return false
	}
	end := slices.Index(argv, "--")
	if end < 0 {
		end = len(argv)
	}
	if !slices.Contains(argv[:end], "--chrome") {
		return false
	}
	if os.Getenv("CREW_WORKER_ID") != "" || os.Getenv("CREW_ROLE_ID") != "" {
		return true
	}
	pane := os.Getenv("TMUX_PANE")
	return pane != "" && paneBranch(ctx, pane) != ""
}

func prepare(ctx context.Context, c Call, stderr io.Writer) (Target, []byte, error) {
	if chromeFromWorker(ctx, c.Argv) {
		return Target{}, nil, errors.New("crew: remote-run: --chrome is dispatcher-only — a worker or role pane cannot grant a session the owner's browser")
	}
	hosts, err := Load(ctx, ConfigBin(), stderr)
	if err != nil {
		return Target{}, nil, fmt.Errorf("crew: remote-run: %w", err)
	}
	if _, err := lookupHost(hosts, c.Host); err != nil {
		return Target{}, nil, fmt.Errorf("crew: remote-run: %w", err)
	}
	paths, err := bus.Locate(ctx, c.Cwd)
	if err != nil {
		return Target{}, nil, fmt.Errorf("crew: remote-run: %s is not inside a git checkout", c.Cwd)
	}
	// A worker worktree's planted .git would pick the slug, and so the clone the
	// call lands in (#633).
	if _, err := wtgit.TrustedCwd(paths.Common, c.Cwd, stderr); err != nil {
		if errors.Is(err, gitguard.ErrRefused) {
			return Target{}, nil, err
		}
		return Target{}, nil, fmt.Errorf("crew: remote-run: %v", err)
	}
	slug, err := originSlug(ctx, paths.Common)
	if err != nil {
		return Target{}, nil, err
	}
	target, err := Resolve(hosts, c.Host, slug)
	if err != nil {
		return Target{}, nil, err
	}
	var spec *string
	if c.SpecFile != "" {
		data, err := os.ReadFile(fromCwd(c.Cwd, c.SpecFile))
		if err != nil {
			return Target{}, nil, fmt.Errorf("crew: remote-run: --spec-file: %v", err)
		}
		text := string(data)
		spec = &text
	}
	var migrate *Migrate
	if c.MigrateDir != "" {
		if migrate, err = readMigrateDir(fromCwd(c.Cwd, c.MigrateDir)); err != nil {
			return Target{}, nil, fmt.Errorf("crew: remote-run: --migrate-dir: %v", err)
		}
	}
	req, err := BuildRequest(target.Repo, slug, c.Argv, c.Env, spec, migrate)
	return target, req, err
}

func fromCwd(cwd, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(cwd, path)
}

// readMigrateDir packs a migrate bundle dir: meta.json, checked as the remote
// will check it, and the optional patch and transcript.jsonl.
func readMigrateDir(dir string) (*Migrate, error) {
	raw, err := readRegular(filepath.Join(dir, "meta.json"))
	if err != nil {
		return nil, err
	}
	patch, err := gzipFile(filepath.Join(dir, "patch"))
	if err != nil {
		return nil, err
	}
	transcript, err := gzipFile(filepath.Join(dir, "transcript.jsonl"))
	if err != nil {
		return nil, err
	}
	meta, err := DecodeMigrateMeta(raw, transcript != "")
	if err != nil {
		return nil, err
	}
	m := &Migrate{PatchGz: patch, TranscriptGz: transcript}
	if m.Meta, err = json.Marshal(meta); err != nil {
		return nil, err
	}
	return m, nil
}

// readRegular is the content of path, which must be a regular file: a
// symlink is not followed, so a bundle never carries what one points at.
func readRegular(path string) ([]byte, error) {
	notRegular := fmt.Errorf("%s is not a regular file", path)
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, notRegular
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	if fi, err = f.Stat(); err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, notRegular
	}
	return io.ReadAll(f)
}

// gzipFile is path's content gzipped then base64-encoded, "" when it does
// not exist.
func gzipFile(path string) (string, error) {
	data, err := readRegular(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(data); err != nil {
		return "", err
	}
	if err := zw.Close(); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}

// CloneSlug is the owner/repo of the checkout whose common dir is common,
// derived the way the claims index derives it; "" when its origin has none.
func CloneSlug(ctx context.Context, common string) string {
	return claimindex.Slug(originURL(ctx, common))
}

func originSlug(ctx context.Context, common string) (string, error) {
	slug := CloneSlug(ctx, common)
	if slug == "" {
		return "", fmt.Errorf("crew: remote-run: no owner/repo in this checkout's origin (%q)", originURL(ctx, common))
	}
	return slug, nil
}

// originURL is the configured remote.origin.url, "" when unset.
func originURL(ctx context.Context, common string) string {
	url, err := git.Origin(ctx, git.Exec{}, common)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(url)
}

// reportStatus maps a finished command to a shell-style status: its exit code,
// or 128+n when signal n killed it. A command that could not start is
// reported on stderr as 127 (not found) or 126.
func reportStatus(err error, stderr io.Writer, what string) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		return ee.ExitCode()
	}
	_, _ = fmt.Fprintf(stderr, "%s: %v\n", what, err)
	if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
		return 127
	}
	return 126
}
