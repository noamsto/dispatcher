// Package repos discovers the git repos on this machine that carry a crew bus:
// the registry dispatch appends to plus a bounded walk of the sweep roots.
package repos

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// Status says whether a discovered candidate can be read as a bus.
type Status string

const (
	StatusOK    Status = "ok"
	StatusGone  Status = "gone"
	StatusNoBus Status = "no bus"
)

// Repo is one candidate git common dir and its status.
type Repo struct {
	Common string
	Status Status
}

// Options carry every outside read, so tests use temp dirs.
type Options struct {
	// Registry overrides RegistryPath(Env).
	Registry string
	// Roots are the raw --root values; empty falls back to $CREW_SWEEP_ROOTS,
	// then $HOME.
	Roots  []string
	Getenv func(string) string
	Stderr io.Writer
	// Prefix leads every stderr note, e.g. "crew: rate --sweep-all".
	Prefix string
}

const (
	busSuffix = "/.git/crew/events.jsonl"
	maxDepth  = 6
)

// RegistryPath is `${XDG_DATA_HOME:-$HOME/.local/share}/crew/repos`, the file
// dispatch appends each repo's git common dir to.
func RegistryPath(lookup func(string) string) string {
	if lookup == nil {
		lookup = os.Getenv
	}
	base := lookup("XDG_DATA_HOME")
	if base == "" {
		base = lookup("HOME") + "/.local/share"
	}
	return filepath.Join(base, "crew", "repos")
}

// Register adds common to the registry the way dispatch's `_registry_add` does.
func Register(registry, common string) error {
	if slices.Contains(registryLines(registry), common) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(registry), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(registry, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(common + "\n"); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// Discover returns every registry or walk candidate, byte-sorted and
// deduplicated, each tagged with its status.
func Discover(o Options) []Repo {
	lookup := o.Getenv
	if lookup == nil {
		lookup = os.Getenv
	}
	stderr := o.Stderr
	if stderr == nil {
		stderr = io.Discard
	}
	registry := o.Registry
	if registry == "" {
		registry = RegistryPath(lookup)
	}

	roots := strings.Join(o.Roots, "\n")
	if roots == "" {
		roots = lookup("CREW_SWEEP_ROOTS")
	}
	if roots == "" {
		roots = lookup("HOME")
	}
	// The bash `$(…)` that built this list strips trailing newlines.
	roots = strings.TrimRight(strings.ReplaceAll(roots, ":", "\n"), "\n")
	rootList := strings.Split(roots, "\n")

	seen := map[string]bool{}
	var found []string
	add := func(c string) {
		if c == "" || seen[c] {
			return
		}
		seen[c] = true
		found = append(found, c)
	}
	for _, c := range registryLines(registry) {
		add(c)
	}
	for _, root := range rootList {
		if info, err := os.Stat(root); err != nil || !info.IsDir() {
			say(stderr, "%s: %s is not a directory\n", o.Prefix, root)
			continue
		}
		walk(root, add)
	}
	sort.Strings(found)

	if len(found) == 0 {
		say(stderr, "%s: no repo with a crew bus found (roots: %s)\n", o.Prefix, strings.Join(rootList, " "))
	}
	out := make([]Repo, 0, len(found))
	for _, c := range found {
		out = append(out, Repo{Common: c, Status: statusOf(c)})
	}
	return out
}

// Registered is every repo the registry lists, byte-sorted and deduplicated,
// each tagged with its status: the walk's roots are the caller's, the registry
// is the machine's. registry "" reads RegistryPath(getenv).
func Registered(registry string, getenv func(string) string) []Repo {
	if registry == "" {
		registry = RegistryPath(getenv)
	}
	commons := registryLines(registry)
	slices.Sort(commons)
	commons = slices.Compact(commons)
	out := make([]Repo, 0, len(commons))
	for _, c := range commons {
		out = append(out, Repo{Common: c, Status: statusOf(c)})
	}
	return out
}

// registryLines is the registry's entries in file order; a missing file has
// none.
func registryLines(registry string) []string {
	data, err := os.ReadFile(registry)
	if err != nil {
		return nil
	}
	var lines []string
	for line := range strings.SplitSeq(string(data), "\n") {
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func statusOf(common string) Status {
	if info, err := os.Stat(common); err != nil || !info.IsDir() {
		return StatusGone
	}
	if info, err := os.Stat(filepath.Join(common, "crew", "events.jsonl")); err != nil || !info.Mode().IsRegular() {
		return StatusNoBus
	}
	return StatusOK
}

// walk mirrors `find root -mindepth 1 -maxdepth 6 \( -name '.*' ! -name .git
// ! -name .worktrees -prune \) -o -path '*/.git/crew/events.jsonl' -print`,
// reporting each hit's git common dir. Like find it never follows symlinks,
// root included, and an unreadable directory is skipped, not fatal.
func walk(root string, add func(string)) {
	if info, err := os.Lstat(root); err != nil || !info.IsDir() {
		return
	}
	var visit func(dir string, depth int)
	visit = func(dir string, depth int) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			name := e.Name()
			if strings.HasPrefix(name, ".") && name != ".git" && name != ".worktrees" {
				continue
			}
			path := dir + "/" + name
			if strings.HasSuffix(dir, "/") {
				path = dir + name
			}
			if strings.HasSuffix(path, busSuffix) {
				add(strings.TrimSuffix(path, "/crew/events.jsonl"))
			}
			if e.IsDir() && depth < maxDepth {
				visit(path, depth+1)
			}
		}
	}
	visit(root, 1)
}

var originRE = regexp.MustCompile(`^(?:https?://[^/]+/|ssh://(?:[^@/]+@)?[^/]+/|[^@/:]+@[^:/]+:)([^/]+)/([^/]+?)(?:\.git)?/?$`)

// Name labels a repo `owner/name` from its origin URL, else by its checkout
// directory (or, for a bare common dir, the dir name minus `.git`).
func Name(common, originURL string) string {
	if m := originRE.FindStringSubmatch(originURL); m != nil {
		return m[1] + "/" + m[2]
	}
	base := filepath.Base(common)
	if base == ".git" {
		return filepath.Base(filepath.Dir(common))
	}
	return strings.TrimSuffix(base, ".git")
}

// senderCap bounds a sender, crew id, branch or repo label on an overseer line.
const senderCap = 120

// Sanitize keeps the sender charset of s, any other rune a `?`, at most
// senderCap bytes.
func Sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		if b.Len() == senderCap {
			break
		}
		if senderRune(r) {
			b.WriteRune(r)
		} else {
			b.WriteByte('?')
		}
	}
	return b.String()
}

// ValidIdent is whether s is an identifier Sanitize leaves as it is: not
// empty, at most senderCap bytes, all of the sender charset.
func ValidIdent(s string) bool {
	if s == "" || len(s) > senderCap {
		return false
	}
	for _, r := range s {
		if !senderRune(r) {
			return false
		}
	}
	return true
}

func senderRune(r rune) bool {
	return 'a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' || '0' <= r && r <= '9' || strings.ContainsRune(":/#._@-", r)
}

// Label is Name as an overseer line or status row shows it, so a repo is
// addressed by the label it displays.
func Label(common, originURL string) string {
	return Sanitize(Name(common, originURL))
}

// say writes to a stream whose failure is not worth failing the command over.
func say(w io.Writer, format string, args ...any) { _, _ = fmt.Fprintf(w, format, args...) }
