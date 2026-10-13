// Package piagentdir is crew.sh's `_pi_agent_dir` arm: seed the worker-scoped
// PI_CODING_AGENT_DIR (~/.pi/dispatcher-worker) from the user's ambient pi dir
// and print its path.
//
// auth.json is linked, not copied or filtered: pi writes it in place, so an
// oauth token refresh must land in the ambient file rather than strand a
// divergent copy in this shared dir. models-store.json is copied instead — it
// is a cache pi rewrites, and every worker shares this dir, so a link would aim
// N concurrent writers at the user's real catalog. models.json is generated
// from the dispatcher's localModels on every seed, with a dummy apiKey. This is
// trust-boundary code: every refusal keeps the bash arm's stderr text and exit.
package piagentdir

import (
	"bytes"
	"crypto/rand"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/noamsto/dispatcher/crew/internal/jqrun"
	"github.com/noamsto/dispatcher/crew/internal/jsonv"
)

var (
	//go:embed clash.jq
	clashProg string
	//go:embed models.jq
	modelsProg string
)

const (
	exitFailure = 1
	exitJQUsage = 2 // jq's exit for a bad --argjson / --slurpfile
	exitJQError = 5 // jq's exit for a runtime error
	bridgeRel   = "/bin/hookyard-bridge.ts"
	xOK         = 1 // access(2) X_OK
)

// Options is everything Run reads beyond the writers.
type Options struct {
	Home    string
	HasHome bool   // $HOME is set
	PIDir   string // $PI_CODING_AGENT_DIR, "" when unset or empty
	// ConfigBin is the dispatch-config command: $DISPATCH_CONFIG_BIN, or
	// `dispatch-config` found on PATH. Its stdin and stderr are inherited.
	ConfigBin string
}

// DefaultOptions reads the process environment.
func DefaultOptions() Options {
	home, ok := os.LookupEnv("HOME")
	bin := os.Getenv("DISPATCH_CONFIG_BIN")
	if bin == "" {
		bin = "dispatch-config"
	}
	return Options{Home: home, HasHome: ok, PIDir: os.Getenv("PI_CODING_AGENT_DIR"), ConfigBin: bin}
}

// refusal is a failure with the bash arm's exact message and exit status.
type refusal struct {
	msg  string
	code int
}

func (r *refusal) Error() string { return r.msg }

func refuse(format string, a ...any) error {
	return &refusal{msg: fmt.Sprintf(format, a...), code: exitFailure}
}

// Run seeds the worker dir and prints it, returning the arm's exit status.
func Run(stdout, stderr io.Writer, o Options) int {
	dir, err := seed(stderr, o)
	if err == nil {
		_, _ = fmt.Fprintf(stdout, "%s\n", dir)
		return 0
	}
	var r *refusal
	if errors.As(err, &r) {
		_, _ = fmt.Fprintf(stderr, "%s\n", r.msg)
		return r.code
	}
	_, _ = fmt.Fprintf(stderr, "crew: %v\n", err)
	return exitFailure
}

func seed(stderr io.Writer, o Options) (string, error) {
	if !o.HasHome {
		return "", refuse("crew: HOME is unset — refusing to seed pi worker dir")
	}
	home := o.Home
	dir := home + "/.pi/dispatcher-worker"
	ambient := ambientDir(home, dir, o.PIDir)

	// MkdirAll succeeds on a symlink to a dir, and every write would land in its target.
	if fi, err := os.Lstat(dir); err == nil && (fi.Mode()&fs.ModeSymlink != 0 || !fi.IsDir()) {
		return "", refuse("crew: %s is a symlink or not a directory — refusing to seed pi worker dir", dir)
	}
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return "", err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", err
	}

	if err := checkNesting(dir, ambient); err != nil {
		return "", err
	}

	// pi's only subprocess hook surface is the extension API, and
	// PI_CODING_AGENT_DIR replaces the ambient config dir rather than augmenting
	// it — so a worker would otherwise run no hook at all. Seed hookyard's
	// generated bridge (it holds absolute router paths, so the ambient copy is
	// the right one) and register it below. No ambient bridge means hookyard is
	// not installed; leave the worker unhooked rather than dangle an entry.
	bridgeEntry := ""
	if isRegular(ambient + bridgeRel) {
		if err := os.MkdirAll(dir+"/bin", 0o777); err != nil {
			return "", err
		}
		if err := copyIfChanged(dir+bridgeRel, ambient+bridgeRel); err != nil {
			return "", err
		}
		bridgeEntry = dir + bridgeRel
	}

	if err := seedSettings(dir, bridgeEntry); err != nil {
		return "", err
	}
	if err := seedAuth(dir, ambient); err != nil {
		return "", err
	}

	// A cold catalog costs pi the model's context window, cost and
	// thinkingLevelMap — the last is what --thinking resolves against.
	if isRegular(ambient + "/models-store.json") {
		if err := copyIfChanged(dir+"/models-store.json", ambient+"/models-store.json"); err != nil {
			return "", err
		}
	}

	if err := seedModels(stderr, dir, o.ConfigBin); err != nil {
		return "", err
	}
	return dir, nil
}

// ambientDir is the ambient pi dir: absolute, never the worker dir itself.
func ambientDir(home, dir, set string) string {
	fallback := home + "/.pi/agent"
	ambient := set
	if ambient == "" {
		ambient = fallback
	}
	if rest, ok := strings.CutPrefix(ambient, "~/"); ok {
		ambient = home + "/" + rest
	}
	ambient = strings.TrimSuffix(ambient, "/")
	// A relative dir would break the read-through once pi runs in another cwd;
	// an inherited worker dir would make the seed read itself.
	if !strings.HasPrefix(ambient, "/") || ambient == dir {
		return fallback
	}
	return ambient
}

// checkNesting refuses a worker dir inside the ambient one or the reverse: a
// symlinked parent or an aliased PI_CODING_AGENT_DIR defeats the literal check.
func checkNesting(dir, ambient string) error {
	dirReal, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return err
	}
	if !isDir(ambient) {
		return nil
	}
	// Bash resolved it with `cd`, which needs search permission.
	if syscall.Access(ambient, xOK) != nil {
		return refuse("crew: cd: %s: Permission denied", ambient)
	}
	ambientReal, err := filepath.EvalSymlinks(ambient)
	if err != nil {
		return err
	}
	if strings.HasPrefix(dirReal+"/", ambientReal+"/") {
		return refuse("crew: %s resolves inside the ambient pi dir %s — refusing to seed pi worker dir", dir, ambient)
	}
	if strings.HasPrefix(ambientReal+"/", dirReal+"/") {
		return refuse("crew: ambient pi dir %s resolves inside %s — refusing to seed pi worker dir", ambient, dir)
	}
	return nil
}

func seedSettings(dir, bridgeEntry string) error {
	base := jsonv.Object()
	if vs, err := readValues(dir + "/settings.json"); err == nil && len(vs) == 1 && vs[0].Kind() == jsonv.KindObject {
		base = vs[0]
	}
	// A non-array extensions value is hand-edited or future-schema; appending to
	// it would make jq die with an opaque `cannot be added`. Refuse legibly.
	ext, hasExt := base.Get("extensions")
	if bridgeEntry != "" && hasExt && ext.Kind() != jsonv.KindNull && ext.Kind() != jsonv.KindArray {
		return refuse("crew: %s/settings.json has a non-array extensions value — refusing to seed pi worker dir", dir)
	}
	base.Set("defaultProjectTrust", jsonv.Str("never"))
	// Append-if-absent is order-preserving: a sort would reorder a pi-written
	// multi-entry extensions list on every reseed.
	if bridgeEntry != "" {
		elems := ext.Elems() // null or absent: none
		registered := false
		for _, e := range elems {
			if s, ok := e.AsString(); ok && s == bridgeEntry {
				registered = true
			}
		}
		if !registered {
			base.Set("extensions", jsonv.Array(append(append([]jsonv.Value{}, elems...), jsonv.Str(bridgeEntry))...))
		}
	}
	return writeIfChanged(dir+"/settings.json", 0o644, pretty(base))
}

// seedAuth links the ambient auth.json, or seeds an empty object when there is
// genuinely none.
//
// Only a genuinely missing file means "no keys". Stat also fails for a dangling
// or looping symlink and behind an unsearchable dir, so "missing" needs its
// nearest existing ancestor to be a searchable directory; anything else must be
// a regular file, since reading a FIFO blocks forever.
func seedAuth(dir, ambient string) error {
	auth := ambient + "/auth.json"
	probe := auth
	// Strip with string ops, not filepath.Dir: Dir cleans the path, and a
	// component ending in a newline must survive to be seen as the link it is.
	// probe is always absolute, so it terminates at "/".
	for !exists(probe) && !isLink(probe) {
		probe = probe[:strings.LastIndex(probe, "/")]
		if probe == "" {
			probe = "/"
		}
	}
	if probe != auth && isDir(probe) && syscall.Access(probe, xOK) == nil {
		// No ambient file: seed an empty object so pi falls through to the env vars.
		return writeIfChanged(dir+"/auth.json", 0o600, "{}\n")
	}
	if !isRegular(auth) {
		return refuse("crew: %s is not a reachable regular file — refusing to seed pi credentials", auth)
	}
	// Linking hands pi the file unparsed, so validate here — a worker that dies
	// on a malformed auth.json reports it as "no API key", far from the cause.
	vs, err := readValues(auth)
	if err != nil || len(vs) == 0 || vs[len(vs)-1].Kind() != jsonv.KindObject {
		return refuse("crew: %s is unreadable or not a JSON object — refusing to seed pi credentials", auth)
	}
	return linkIfChanged(dir+"/auth.json", auth)
}

func seedModels(stderr io.Writer, dir, bin string) error {
	cmd := exec.Command(bin)
	cmd.Stdin = os.Stdin
	cmd.Stderr = stderr
	out, err := cmd.Output()
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			_, _ = fmt.Fprintf(stderr, "crew: %s: command not found\n", bin)
		} else if _, exited := err.(*exec.ExitError); !exited {
			_, _ = fmt.Fprintf(stderr, "crew: %v\n", err)
		}
		return refuse("crew: could not resolve the dispatcher settings for the pi worker models.json — refusing to seed pi worker dir")
	}
	settings, err := jsonv.ParseOne(string(out))
	if err != nil {
		return &refusal{msg: "jq: Invalid JSON text passed to --argjson", code: exitJQUsage}
	}
	auths, err := readValues(dir + "/auth.json")
	if err != nil {
		return &refusal{msg: fmt.Sprintf("jq: Bad JSON in --slurpfile a %s/auth.json", dir), code: exitJQUsage}
	}

	// pi prefers a stored auth.json credential over a models.json apiKey, and
	// the worker's auth.json links the user's real one — a clashing provider
	// name would send that credential to the local endpoint.
	clash, err := jqrun.Run(clashProg, nil, 0, map[string]jsonv.Value{"s": settings, "a": jsonv.Array(auths...)})
	if err != nil {
		return &refusal{msg: "jq: error: " + err.Error(), code: exitJQError}
	}
	if name, _ := clash.AsString(); name != "" {
		return refuse("crew: localModels provider '%s' has a stored pi credential in auth.json — rename the provider so the credential is never sent to a local endpoint", name)
	}

	prog, err := jqrun.Compile(modelsProg, 0, nil)
	if err != nil {
		return err
	}
	models, err := prog.Run(settings)
	if err != nil {
		return &refusal{msg: "jq: error: " + err.Error(), code: exitJQError}
	}
	return writeIfChanged(dir+"/models.json", 0o644, pretty(models))
}

func pretty(v jsonv.Value) string {
	return string(jsonv.Append(nil, v, jsonv.Options{Indent: true})) + "\n"
}

func readValues(path string) ([]jsonv.Value, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return jsonv.DecodeStream(bytes.NewReader(b))
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func isLink(p string) bool {
	fi, err := os.Lstat(p)
	return err == nil && fi.Mode()&fs.ModeSymlink != 0
}

func isDir(p string) bool { fi, err := os.Stat(p); return err == nil && fi.IsDir() }

func isRegular(p string) bool { fi, err := os.Stat(p); return err == nil && fi.Mode().IsRegular() }

// tempName is a sibling `.seed.XXXXXX` name, like mktemp's.
func tempName(target string) string {
	const alnum = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	var r [6]byte
	_, _ = rand.Read(r[:])
	for i := range r {
		r[i] = alnum[int(r[i])%len(alnum)]
	}
	return filepath.Join(filepath.Dir(target), ".seed."+string(r[:]))
}

// replaceWith writes content to a fresh same-dir temp file, sets its mode and
// renames it over target, so a reader never sees a truncated file.
func replaceWith(target string, mode fs.FileMode, content []byte) error {
	for {
		tmp := tempName(target)
		f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return err
		}
		_, err = f.Write(content)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err == nil {
			err = os.Chmod(tmp, mode)
		}
		if err == nil {
			err = os.Rename(tmp, target)
		}
		if err != nil {
			_ = os.Remove(tmp)
		}
		return err
	}
}

func writeIfChanged(target string, mode fs.FileMode, content string) error {
	if fi, err := os.Stat(target); err == nil && fi.Mode().IsRegular() {
		if cur, err := os.ReadFile(target); err == nil && string(cur) == content {
			return nil
		}
	}
	return replaceWith(target, mode, []byte(content))
}

func copyIfChanged(target, source string) error {
	src, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	if cur, err := os.ReadFile(target); err == nil && bytes.Equal(cur, src) {
		return nil
	}
	return replaceWith(target, 0o644, src)
}

// linkIfChanged points target at source. pi resolves auth.json under
// PI_CODING_AGENT_DIR with no path override, so the worker needs its own entry.
func linkIfChanged(target, source string) error {
	// Stat also catches a symlinked dir, which would swallow the rename into it.
	if isDir(target) {
		return refuse("crew: %s is a directory — refusing to seed pi worker dir", target)
	}
	if cur, err := os.Readlink(target); err == nil && cur == source {
		return nil
	}
	for {
		tmp := tempName(target)
		err := os.Symlink(source, tmp)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return err
		}
		if err := os.Rename(tmp, target); err != nil {
			_ = os.Remove(tmp)
			return err
		}
		return nil
	}
}
