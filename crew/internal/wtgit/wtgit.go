// Package wtgit builds the anchored git invocations the dispatcher runs against
// a worker's worktree, argv-identical to the bash helpers in
// adapters/core/worktree-git.sh (a drift test pins the shared tables).
//
// A worktree's `.git` gitlink is worker-writable, and git discovery through it
// reads whatever config the worker's chosen gitdir holds (#539). So the git dir
// comes from the main repo's own worktrees/*/gitdir back-pointer, never from
// discovery; the config that anchor still reads is checked against the
// baseline first (#557, gitguard); the -c overrides keep a baselined relative
// core.hooksPath and hook.<name>.command from running worker-tree programs, and
// --attr-source pinned to the empty tree keeps in-tree .gitattributes from
// selecting a filter driver (#578). The common-dir form anchors on --git-dir
// for the same reason (#633), and the trusted cwd keeps the dispatcher's own
// shell out of a worker's tree (#677).
//
// Command and CommonCommand return the unstarted *exec.Cmd so a caller can
// background it and keep its pid. Their env is scrubbed of the variables that
// would redirect git or inject config; the bash helpers inherit the environment
// instead, but no caller sets these, so the scrub only closes a hole.
package wtgit

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/noamsto/dispatcher/crew/internal/gitguard"
)

// NeutralCfg is `_wt_neutral_cfg`: the -c overrides that still matter past the
// config guard. hooksPath does not reach config-defined hooks, so those events
// are switched off; core.attributesFile is not a guarded key.
var NeutralCfg = []string{
	"core.fsmonitor=false", "core.hooksPath=/dev/null", "core.attributesFile=/dev/null",
	"hook.reference-transaction.enabled=false", "hook.post-checkout.enabled=false",
	"hook.post-index-change.enabled=false", "submodule.recurse=false",
}

// CommonExtra is what `_wt_common_argv` adds to NeutralCfg: a background gc or
// maintenance would run its own git outside these overrides.
var CommonExtra = []string{"gc.auto=0", "maintenance.auto=false"}

// Opts shapes the command Command and CommonCommand return.
type Opts struct {
	// Stderr receives refusals and the command's own stderr; nil is os.Stderr.
	Stderr io.Writer
	// IndexFile, when set, is GIT_INDEX_FILE for the command.
	IndexFile string
}

func (o Opts) stderr() io.Writer {
	if o.Stderr == nil {
		return os.Stderr
	}
	return o.Stderr
}

// dropped names the variables that redirect git's directories or objects or
// inject config; the prefixes are the GIT_CONFIG_KEY_<n>/VALUE_<n> pairs.
var (
	dropped = map[string]bool{
		"GIT_DIR": true, "GIT_WORK_TREE": true, "GIT_COMMON_DIR": true, "GIT_INDEX_FILE": true,
		"GIT_CONFIG": true, "GIT_CONFIG_PARAMETERS": true, "GIT_CONFIG_COUNT": true,
		"GIT_ATTR_SOURCE": true, "GIT_OBJECT_DIRECTORY": true, "GIT_ALTERNATE_OBJECT_DIRECTORIES": true,
	}
	droppedPrefixes = []string{"GIT_CONFIG_KEY_", "GIT_CONFIG_VALUE_"}
)

// Env is environ without the variables that would move git off its anchor or
// inject config past the guard, plus GIT_INDEX_FILE=index when index is set.
// GIT_CONFIG_GLOBAL, GIT_CONFIG_NOSYSTEM and HOME stay: they pick which user
// config is read, which the guard already covers.
func Env(environ []string, index string) []string {
	out := make([]string, 0, len(environ)+1)
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if dropped[name] || hasAnyPrefix(name, droppedPrefixes) {
			continue
		}
		out = append(out, kv)
	}
	if index != "" {
		out = append(out, "GIT_INDEX_FILE="+index)
	}
	return out
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// EmptyTree is the empty-tree oid of gitdir's repository (sha1 or sha256).
func EmptyTree(gitdir string) (string, error) {
	cmd := exec.Command("git", "--git-dir="+gitdir, "hash-object", "--no-filters", "-t", "tree", "/dev/null")
	cmd.Env = Env(os.Environ(), "")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(out), "\n"), nil
}

// guarded reports a gitguard error the way `crew-go git-guard` did for the
// bash wrappers: a refusal has said why already, anything else (an unreadable
// baseline) is printed here. Either way the caller gets a refusal.
func guarded(err error, stderr io.Writer) error {
	if err == nil || errors.Is(err, gitguard.ErrRefused) {
		return err
	}
	_, _ = fmt.Fprintf(stderr, "refusing git: %v\n", err)
	return fmt.Errorf("%w: %w", gitguard.ErrRefused, err)
}

func refuse(stderr io.Writer, format string, args ...any) error {
	_, _ = fmt.Fprintf(stderr, format, args...)
	return gitguard.ErrRefused
}

// Command is `_wt_git`: git run against the linked worktree wt through its
// admin dir. Every error is a gitguard.ErrRefused whose reason is on o.Stderr.
func Command(ctx context.Context, admin, wt string, o Opts, args ...string) (*exec.Cmd, error) {
	stderr := o.stderr()
	// config.worktree is still read with an anchored --git-dir, and its keys
	// (include.path, filter drivers) cannot be enumerated away with -c.
	if _, err := os.Lstat(admin + "/config.worktree"); err == nil {
		return nil, refuse(stderr, "refusing git in %s: %s/config.worktree exists — per-worktree config is worker-writable\n", wt, admin)
	}
	// The shell test is `*/worktrees/?*` plus no `/` after the last `/worktrees/`.
	const sep = "/worktrees/"
	first, last := strings.Index(admin, sep), strings.LastIndex(admin, sep)
	if first < 0 || first+len(sep) == len(admin) || strings.Contains(admin[last+len(sep):], "/") {
		return nil, refuse(stderr, "refusing git in %s: %s is not a linked-worktree admin dir\n", wt, admin)
	}
	common := admin[:last]
	if err := guarded(gitguard.Check(common, admin, stderr), stderr); err != nil {
		return nil, err
	}
	empty, err := EmptyTree(admin)
	if err != nil {
		return nil, refuse(stderr, "refusing git: cannot compute the empty tree of %s\n", admin)
	}
	// -C: with the caller's cwd inside wt, git would resolve pathspecs against
	// that subdirectory rather than the work-tree root.
	argv := []string{"-C", wt, "--no-optional-locks"}
	argv = appendCfg(argv, NeutralCfg)
	argv = append(argv, "--attr-source="+empty, "--git-dir="+admin, "--work-tree="+wt)
	return build(ctx, o, append(argv, args...)), nil
}

// CommonCommand is `_wt_git_common`: git anchored on the common dir itself, so
// no worker's `.git` is consulted wherever the caller runs it.
func CommonCommand(ctx context.Context, common string, o Opts, args ...string) (*exec.Cmd, error) {
	stderr := o.stderr()
	if err := guarded(gitguard.Check(common, common, stderr), stderr); err != nil {
		return nil, err
	}
	argv := []string{"--git-dir=" + common}
	argv = appendCfg(argv, NeutralCfg)
	argv = appendCfg(argv, CommonExtra)
	return build(ctx, o, append(argv, args...)), nil
}

func appendCfg(argv, cfg []string) []string {
	for _, kv := range cfg {
		argv = append(argv, "-c", kv)
	}
	return argv
}

func build(ctx context.Context, o Opts, argv []string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", argv...)
	cmd.Env = Env(os.Environ(), o.IndexFile)
	cmd.Stderr = o.stderr()
	return cmd
}

// Status is `_wt_status`: the porcelain status of wt, trailing newlines
// stripped. A submodule's status would run by discovery through its own
// worker-writable .git; only the command-line flag beats a .gitmodules
// `ignore = none`.
func Status(ctx context.Context, admin, wt string, o Opts, extra ...string) (string, error) {
	cmd, err := Command(ctx, admin, wt, o, append([]string{"status", "--porcelain", "--ignore-submodules=all"}, extra...)...)
	if err != nil {
		return "", err
	}
	out, err := cmd.Output()
	return strings.TrimRight(string(out), "\n"), err
}

// GitlinkOK reports whether wt's `.git` is a regular file whose `gitdir:` line
// resolves to admin.
func GitlinkOK(admin, wt string) bool {
	gitlink := wt + "/.git"
	if fi, err := os.Lstat(gitlink); err != nil || !fi.Mode().IsRegular() {
		return false
	}
	data, err := os.ReadFile(gitlink)
	if err != nil {
		return false
	}
	line, _, _ := strings.Cut(string(data), "\n")
	target, ok := strings.CutPrefix(line, "gitdir: ")
	if !ok {
		return false
	}
	if !strings.HasPrefix(target, "/") {
		target = wt + "/" + target
	}
	real, err := filepath.EvalSymlinks(target)
	return err == nil && real == admin
}

// AdminDir is `_wt_admin_dir`: the realpath of <common>/worktrees/<id> whose
// own gitdir back-pointer names wt's `.git`.
func AdminDir(common, wt string) (string, error) {
	want, err := gitguard.RealpathMissing(wt + "/.git")
	if err != nil {
		return "", err
	}
	entries, err := os.ReadDir(common + "/worktrees")
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		adminDir := common + "/worktrees/" + e.Name()
		back, ok := backPointer(adminDir + "/gitdir")
		if !ok {
			continue
		}
		if !strings.HasPrefix(back, "/") {
			back = adminDir + "/" + back
		}
		if got, err := gitguard.RealpathMissing(back); err != nil || got != want {
			continue
		}
		real, err := filepath.EvalSymlinks(adminDir)
		if err != nil {
			return "", err
		}
		// A symlinked worktrees/ or worktrees/<id> moves the admin dir — and the
		// config it reads — outside the main repo.
		realCommon, err := filepath.EvalSymlinks(common)
		if err != nil || real != realCommon+"/worktrees/"+e.Name() {
			return "", fmt.Errorf("%s resolves outside %s/worktrees", adminDir, common)
		}
		return real, nil
	}
	return "", fmt.Errorf("no linked-worktree admin dir of %s points at %s/.git", common, wt)
}

// backPointer is the first line of a regular gitdir file.
func backPointer(path string) (string, bool) {
	fi, err := os.Stat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return "", false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	line, _, _ := strings.Cut(string(data), "\n")
	return line, true
}
