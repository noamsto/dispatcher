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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	"DISPATCH_TMUX_SESSION": regexp.MustCompile(`\Acrew-[A-Za-z0-9_-]+\z`),
	"DISPATCH_EMIT_ROW":     regexp.MustCompile(`\A1\z`),
	"DISPATCH_SESSION_ID":   SessionRe,
	"CREW_IDENTITY_AVOID":   regexp.MustCompile(`\A[A-Za-z0-9-]+(,[A-Za-z0-9-]+)*\z`),
}

// CrewSubcommands are the `crew` subcommands a request may run.
var CrewSubcommands = []string{"reply", "nudge", "where", "roster", "relay", "reap", "dispatch-row"}

const exitUsage = 2

// Host is one `hosts.<name>` settings entry.
type Host struct {
	SSH   string            `json:"ssh"`
	Repos map[string]string `json:"repos"`
}

// Target is where a call lands: the ssh destination and the remote clone.
type Target struct {
	SSH  string
	Repo string
}

// Request is the JSON object remote-run writes to ssh's stdin and remote-exec
// reads. Field order is the wire order.
type Request struct {
	V    int               `json:"v"`
	Repo string            `json:"repo"`
	Slug string            `json:"slug"`
	Argv []string          `json:"argv"`
	Env  map[string]string `json:"env"`
	Spec *string           `json:"spec,omitempty"`
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
// same patterns dispatch-config applies. Its stderr goes to stderr.
func Load(ctx context.Context, bin string, stderr io.Writer) (map[string]Host, error) {
	cmd := exec.CommandContext(ctx, bin)
	cmd.Stderr = stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("crew: remote-run: could not resolve the dispatcher settings (%s: %v)", bin, err)
	}
	var settings struct {
		Hosts map[string]Host `json:"hosts"`
	}
	if err := json.Unmarshal(out, &settings); err != nil {
		return nil, fmt.Errorf("crew: remote-run: settings hosts: %v", err)
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
		return fmt.Errorf("crew: remote-run: settings hosts: invalid host name %q", name)
	}
	if !SSHRe.MatchString(h.SSH) {
		return fmt.Errorf("crew: remote-run: settings hosts.%s.ssh: invalid ssh destination %q", name, h.SSH)
	}
	for key, path := range h.Repos {
		if !RepoKeyRe.MatchString(key) {
			return fmt.Errorf("crew: remote-run: settings hosts.%s.repos: invalid key %q (want owner/repo)", name, key)
		}
		if !RepoPathRe.MatchString(path) {
			return fmt.Errorf("crew: remote-run: settings hosts.%s.repos.%s: %q is not an absolute path without control characters", name, key, path)
		}
	}
	return nil
}

func lookupHost(hosts map[string]Host, name string) (Host, error) {
	h, ok := hosts[name]
	if !ok {
		return Host{}, fmt.Errorf("crew: remote-run: unknown host '%s' (no hosts.%s in settings)", name, name)
	}
	return h, nil
}

// Resolve finds the ssh destination for host name and its clone of slug.
// Repo keys match slug case-insensitively, as claimindex.Slug lowercases.
func Resolve(hosts map[string]Host, name, slug string) (Target, error) {
	h, err := lookupHost(hosts, name)
	if err != nil {
		return Target{}, err
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

// BuildRequest encodes a request. env holds KEY=VALUE pairs; spec is nil when
// there is none.
func BuildRequest(repo, slug string, argv, env []string, spec *string) ([]byte, error) {
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
	return json.Marshal(Request{V: 1, Repo: repo, Slug: slug, Argv: argv, Env: vars, Spec: spec})
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

// Call is one remote-run invocation. SpecFile is "" for none; a relative path
// is read from Cwd.
type Call struct {
	Host     string
	Cwd      string
	Env      []string
	SpecFile string
	Argv     []string
}

// Run is `crew remote-run`: it sends c to its host over ssh and returns ssh's
// status, or 2 for a refusal before any ssh.
func Run(ctx context.Context, c Call, stdout, stderr io.Writer) int {
	target, req, err := prepare(ctx, c, stderr)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return exitUsage
	}
	cmd := exec.CommandContext(ctx, "ssh", SSHArgv(target.SSH)...)
	cmd.Stdin = bytes.NewReader(req)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	rc := exitStatus(cmd.Run(), stderr, "crew: remote-run: ssh")
	if rc == 255 {
		_, _ = fmt.Fprintf(stderr, "crew: remote-run: ssh to host '%s' (%s) failed (exit 255)\n", c.Host, target.SSH)
	}
	return rc
}

func prepare(ctx context.Context, c Call, stderr io.Writer) (Target, []byte, error) {
	hosts, err := Load(ctx, ConfigBin(), stderr)
	if err != nil {
		return Target{}, nil, err
	}
	if _, err := lookupHost(hosts, c.Host); err != nil {
		return Target{}, nil, err
	}
	slug, err := originSlug(ctx, c.Cwd)
	if err != nil {
		return Target{}, nil, err
	}
	target, err := Resolve(hosts, c.Host, slug)
	if err != nil {
		return Target{}, nil, err
	}
	var spec *string
	if c.SpecFile != "" {
		path := c.SpecFile
		if !filepath.IsAbs(path) {
			path = filepath.Join(c.Cwd, path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return Target{}, nil, fmt.Errorf("crew: remote-run: --spec-file: %v", err)
		}
		text := string(data)
		spec = &text
	}
	req, err := BuildRequest(target.Repo, slug, c.Argv, c.Env, spec)
	return target, req, err
}

// originSlug is the owner/repo of the checkout containing cwd, derived the way
// the claims index derives it.
func originSlug(ctx context.Context, cwd string) (string, error) {
	paths, err := bus.Locate(ctx, cwd)
	if err != nil {
		return "", fmt.Errorf("crew: remote-run: %s is not inside a git checkout", cwd)
	}
	url := originURL(ctx, paths.Common)
	slug := claimindex.Slug(url)
	if slug == "" {
		return "", fmt.Errorf("crew: remote-run: no owner/repo in this checkout's origin (%q)", url)
	}
	return slug, nil
}

// originURL is the configured remote.origin.url, "" when unset.
func originURL(ctx context.Context, common string) string {
	out, err := exec.CommandContext(ctx, "git", "--git-dir="+common, "config", "--get", "remote.origin.url").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// exitStatus maps a finished command to a shell-style status: its exit code,
// or 128+n when signal n killed it. A command that could not start is
// reported on stderr as 127 (not found) or 126.
func exitStatus(err error, stderr io.Writer, what string) int {
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
