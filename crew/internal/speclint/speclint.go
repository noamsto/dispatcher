// Package speclint is `crew spec-lint`: it judges a dispatcher task spec before
// a worker is created, so a spec that is already done on origin, names files
// that are not there, or asks for commands the host cannot run is caught while
// it is cheap to fix.
//
// The checks are heuristics tuned for few false positives: only a missing
// summary or Acceptance section is an error, everything else a warning. Git is
// read through git.Client and never fetched, so the origin refs the dispatcher
// already has are what is judged.
package speclint

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"

	"github.com/noamsto/dispatcher/crew/internal/git"
)

const (
	exitErrors = 1
	exitUsage  = 2

	maxCommits = 5
)

// Request is one spec-lint run: the spec file, the issue numbers (digits only)
// the premise check looks for, and whether the host the worker will run on is
// not this one, which skips the runnable check.
type Request struct {
	File         string
	Issues       []string
	NoHostChecks bool
}

// Options are the seams: git, the executable lookup (name or repo-relative
// path, resolved from dir) and the platform the platform-tool check compares.
type Options struct {
	Git      git.Client
	LookPath func(dir, name string) bool
	GOOS     string
}

// DefaultOptions is the real git, the real lookup and this host's platform.
func DefaultOptions() Options {
	return Options{Git: git.Exec{}, LookPath: lookPath, GOOS: runtime.GOOS}
}

// Finding is one line of output: `<level> <check>: <message>`.
type Finding struct {
	Level   string
	Check   string
	Message string
}

func (f Finding) String() string { return f.Level + " " + f.Check + ": " + f.Message }

var issueRe = regexp.MustCompile(`^#?([0-9]+)$`)

// ParseIssue is an --issue value, `N` or `#N`, as its digits.
func ParseIssue(s string) (string, bool) {
	m := issueRe.FindStringSubmatch(s)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// Run lints req.File and prints the findings and a summary line to stderr. It
// exits 1 when any finding is an error, 0 otherwise, and 2 for an unreadable file.
func Run(ctx context.Context, req Request, cwd string, stderr io.Writer, o Options) int {
	spec, err := os.ReadFile(req.File)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "crew: spec-lint: %v\n", err)
		return exitUsage
	}
	var errs, warns int
	for _, f := range Lint(ctx, spec, req, cwd, o) {
		_, _ = fmt.Fprintln(stderr, f)
		if f.Level == "error" {
			errs++
		} else {
			warns++
		}
	}
	_, _ = fmt.Fprintf(stderr, "spec-lint: %d error(s), %d warning(s)\n", errs, warns)
	if errs > 0 {
		return exitErrors
	}
	return 0
}

var (
	listRe      = regexp.MustCompile(`^ {0,3}(?:[-*+]|[0-9]+[.)])\s+(.*)$`)
	labelRe     = regexp.MustCompile(`^([A-Za-z]+[0-9][0-9-]*)[*_]*:`)
	spanRe      = regexp.MustCompile("`([^`]+)`")
	extRe       = regexp.MustCompile(`.\.[A-Za-z][A-Za-z0-9]*$`)
	pathRe      = regexp.MustCompile(`^(\./)?([A-Za-z0-9._-]+(?:/[A-Za-z0-9._-]+)+)/?(:[0-9]+(-[0-9]+)?)?$`)
	newRe       = regexp.MustCompile(`(?i)\b(new|create|creates|created|creating|add|adds|added|introduce|introduces|generate|generated|write|writes)\b`)
	commandRe   = regexp.MustCompile(`^[A-Za-z0-9_.][A-Za-z0-9_./+-]*$`)
	envAssignRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)
	ciRe        = regexp.MustCompile(`\bCI\b|(?i:\b(?:ci|pre-push|fast|deterministic) gates?\b|\bworkflows?\b)`)
	statusRe    = regexp.MustCompile(`(?i)\bgreen\b|\brun id\b|actions/runs`)
	splitRe     = regexp.MustCompile(`&&|\|\||;`)
	spaceRe     = regexp.MustCompile(`\s+`)
	workflowRe  = regexp.MustCompile(`^\.github/workflows/[^/]+\.ya?ml$`)
)

var builtins = map[string]bool{
	"cd": true, "export": true, "source": true, ".": true, "test": true, "true": true, "false": true,
	"set": true, "unset": true, "eval": true, "exec": true, "env": true, "time": true,
}

// platformTools maps a tool to the only GOOS it runs on.
var platformTools = map[string]string{
	"xcodebuild": "darwin", "xcrun": "darwin", "simctl": "darwin", "codesign": "darwin",
	"systemctl": "linux", "journalctl": "linux", "apt-get": "linux",
}

type line struct {
	text string
	span []string
}

type item struct {
	label string
	text  string
}

// Lint is every finding for spec, in check order.
func Lint(ctx context.Context, spec []byte, req Request, cwd string, o Options) []Finding {
	lines := parse(string(spec))
	items, structure := structureFindings(lines)
	out := structure

	top := git.Toplevel(ctx, o.Git, cwd)
	ref := defaultRef(ctx, o.Git, top)
	var files map[string]bool
	if ref == "" {
		out = append(out, Finding{"warn", "premise", "no origin default branch; premise, path and CI checks skipped"})
	} else {
		out = append(out, premise(ctx, o.Git, top, ref, req.Issues)...)
		var err error
		if files, err = tree(ctx, o.Git, top, ref); err != nil {
			out = append(out, Finding{"warn", "path", fmt.Sprintf("cannot list %s: %s; path and CI command checks skipped", ref, firstLine(err))})
		} else {
			out = append(out, paths(ref, files, lines)...)
		}
	}
	if !req.NoHostChecks {
		out = append(out, runnable(top, items, o)...)
	}
	if ref != "" {
		out = append(out, ci(ctx, o.Git, top, ref, files, items)...)
	}
	return out
}

// parse drops fenced code blocks, whose content is illustration rather than a
// claim, and indexes each remaining line's code spans.
func parse(spec string) []line {
	var lines []line
	fenced := false
	for _, l := range strings.Split(spec, "\n") {
		l = strings.TrimRight(l, "\r")
		if t := strings.TrimSpace(l); strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			fenced = !fenced
			continue
		}
		if !fenced {
			lines = append(lines, line{text: l, span: spans(l)})
		}
	}
	return lines
}

func spans(s string) []string {
	var out []string
	for _, m := range spanRe.FindAllStringSubmatch(s, -1) {
		out = append(out, m[1])
	}
	return out
}

func structureFindings(lines []line) ([]item, []Finding) {
	var out []Finding
	summary := false
	for _, l := range lines {
		if strings.HasPrefix(l.text, "## ") {
			break
		}
		if t := strings.TrimSpace(l.text); t != "" && !strings.HasPrefix(t, "#") {
			summary = true
		}
	}
	if !summary {
		out = append(out, Finding{"error", "structure", `no summary paragraph before the first "## " heading`})
	}
	acceptance := slices.IndexFunc(lines, func(l line) bool {
		return strings.HasPrefix(l.text, "## ") && isAcceptance(l.text)
	})
	if acceptance < 0 {
		return nil, append(out, Finding{"error", "structure", `no "## Acceptance" section`})
	}
	items := acceptanceItems(lines[acceptance+1:])
	if len(items) == 0 {
		out = append(out, Finding{"error", "structure", "Acceptance section has no list items"})
	}
	return items, out
}

func isAcceptance(heading string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(strings.TrimPrefix(heading, "## "))), "acceptance")
}

// acceptanceItems are the list items up to the next "## " heading; a non-blank
// line that is not itself a list line continues the item above it.
func acceptanceItems(lines []line) []item {
	var items []item
	cont := false
	for _, l := range lines {
		if strings.HasPrefix(l.text, "## ") {
			break
		}
		if m := listRe.FindStringSubmatch(l.text); m != nil {
			items = append(items, item{label: label(m[1], len(items)+1), text: m[1]})
			cont = true
			continue
		}
		if strings.TrimSpace(l.text) == "" {
			cont = false
			continue
		}
		if cont {
			items[len(items)-1].text += " " + strings.TrimSpace(l.text)
		}
	}
	return items
}

func label(text string, n int) string {
	if m := labelRe.FindStringSubmatch(strings.TrimLeft(text, "*_")); m != nil {
		return m[1]
	}
	return fmt.Sprintf("item %d", n)
}

// defaultRef is origin's default branch as a local ref, "" when there is none.
func defaultRef(ctx context.Context, g git.Client, top string) string {
	if ref := git.Output(ctx, g, top, "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); ref != "" {
		return ref
	}
	if _, err := g.Run(ctx, top, "rev-parse", "--verify", "--quiet", "origin/main^{commit}"); err == nil {
		return "origin/main"
	}
	return ""
}

func premise(ctx context.Context, g git.Client, top, ref string, issues []string) []Finding {
	var out []Finding
	for _, n := range issues {
		commits, err := g.Run(ctx, top, "log", ref, "--format=%h %s", "-E", "-i", "--grep="+mentionPattern(n))
		if err != nil {
			out = append(out, Finding{"warn", "premise", fmt.Sprintf("cannot search %s for #%s: %s", ref, n, firstLine(err))})
			continue
		}
		commits = strings.TrimSpace(commits)
		if commits == "" {
			continue
		}
		closing := map[string]bool{}
		closeNote := ""
		closed, err := g.Run(ctx, top, "log", ref, "--format=%h", "-E", "-i", "--grep="+closingPattern(n))
		if err != nil {
			closeNote = fmt.Sprintf(" (could not check closing keywords: %s)", firstLine(err))
		}
		for _, h := range strings.Fields(closed) {
			closing[h] = true
		}
		rows := strings.Split(commits, "\n")
		var shown []string
		for i, row := range rows {
			if i == maxCommits {
				shown = append(shown, fmt.Sprintf("… and %d more", len(rows)-maxCommits))
				break
			}
			if hash, _, _ := strings.Cut(row, " "); closing[hash] {
				row += " (closes)"
			}
			shown = append(shown, row)
		}
		out = append(out, Finding{"warn", "premise", fmt.Sprintf("#%s is already referenced on %s: %s%s", n, ref, strings.Join(shown, "; "), closeNote)})
	}
	return out
}

func mentionPattern(n string) string { return "#" + n + "([^0-9]|$)" }

func closingPattern(n string) string {
	return "(close[sd]?|fix(e[sd])?|resolve[sd]?):? " + mentionPattern(n)
}

func firstLine(err error) string {
	l, _, _ := strings.Cut(err.Error(), "\n")
	return l
}

// tree is every file on ref.
func tree(ctx context.Context, g git.Client, top, ref string) (map[string]bool, error) {
	out, err := g.Run(ctx, top, "ls-tree", "-r", "--full-tree", "--name-only", ref)
	if err != nil {
		return nil, err
	}
	files := map[string]bool{}
	for _, f := range strings.Split(out, "\n") {
		files[f] = true
	}
	return files, nil
}

func paths(ref string, files map[string]bool, lines []line) []Finding {
	// Specs often write package-relative paths, so a tracked file ending in
	// "/"+p counts too.
	tracked := func(p string) bool {
		if files[p] {
			return true
		}
		for f := range files {
			if strings.HasSuffix(f, "/"+p) {
				return true
			}
		}
		return false
	}
	var out []Finding
	seen := map[string]bool{}
	for _, l := range lines {
		for _, s := range l.span {
			p, ok := pathCandidate(s)
			if !ok || seen[p] || newRe.MatchString(l.text) || tracked(p) {
				continue
			}
			seen[p] = true
			out = append(out, Finding{"warn", "path", fmt.Sprintf(`%s is not on %s (say "new" on its line if the spec creates it)`, p, ref)})
		}
	}
	return out
}

// pathCandidate is span's repo path when it is file-like: names without a
// letter-led extension, such as branches, directories and model ids, are never
// judged.
func pathCandidate(span string) (string, bool) {
	m := pathRe.FindStringSubmatch(span)
	if m == nil || strings.HasSuffix(strings.TrimSuffix(span, m[3]), "/") {
		return "", false
	}
	if base := path.Base(m[2]); !extRe.MatchString(base) {
		return "", false
	}
	return m[2], true
}

// command is span's tokens after any NAME=value prefix, when span is
// command-shaped: it holds whitespace and starts like a program name. Quoted
// output such as `warn premise: x` has a `label:` in its first two words.
func command(span string) ([]string, bool) {
	f := strings.Fields(span)
	if len(f) < 2 {
		return nil, false
	}
	for len(f) > 0 && envAssignRe.MatchString(f[0]) {
		f = f[1:]
	}
	if len(f) == 0 || !commandRe.MatchString(f[0]) || slices.ContainsFunc(f[:min(2, len(f))], func(w string) bool { return strings.HasSuffix(w, ":") }) {
		return nil, false
	}
	return f, true
}

func runnable(top string, items []item, o Options) []Finding {
	var out []Finding
	for _, it := range items {
		for _, s := range spans(it.text) {
			tokens, ok := command(s)
			if !ok {
				continue
			}
			if tool, bad := foreignTool(tokens, o.GOOS); bad {
				out = append(out, Finding{"warn", "runnable", fmt.Sprintf("%s: %q is a %s tool; this host is %s", it.label, tool, osName(platformTools[tool]), o.GOOS)})
				continue
			}
			if name := tokens[0]; !builtins[name] && !o.LookPath(top, name) {
				out = append(out, Finding{"warn", "runnable", fmt.Sprintf("%s: %q is not on PATH here", it.label, name)})
			}
		}
	}
	return out
}

// foreignTool checks the command word only: tokens[0] and the word a `-c` or
// `env` wrapper hands over to, so `rg xcodebuild src/` is not an xcodebuild run.
// The wrapper's own NAME=value arguments (and env's flags) are skipped, and the
// handed-over word may be quoted: `sh -c 'xcodebuild -scheme X'`.
func foreignTool(tokens []string, goos string) (string, bool) {
	cands := []string{tokens[0]}
	for i, t := range tokens[:len(tokens)-1] {
		if t != "-c" && t != "env" {
			continue
		}
		rest := tokens[i+1:]
		for len(rest) > 1 && (envAssignRe.MatchString(rest[0]) || t == "env" && strings.HasPrefix(rest[0], "-")) {
			rest = rest[1:]
		}
		cands = append(cands, strings.Trim(rest[0], `'"`))
	}
	for _, t := range cands {
		if want, ok := platformTools[t]; ok && want != goos {
			return t, true
		}
	}
	return "", false
}

func osName(goos string) string {
	if goos == "darwin" {
		return "macOS"
	}
	return goos
}

// lookPath resolves name as `command -v` would from dir: a name with a slash is
// an executable file there, anything else is searched on PATH.
func lookPath(dir, name string) bool {
	if !strings.Contains(name, "/") {
		_, err := exec.LookPath(name)
		return err == nil
	}
	if !filepath.IsAbs(name) {
		name = filepath.Join(dir, name)
	}
	st, err := os.Stat(name)
	return err == nil && st.Mode().IsRegular() && st.Mode()&0o111 != 0
}

// ci judges CI claims; files is the ref's tree, nil when it could not be listed.
func ci(ctx context.Context, g git.Client, top, ref string, files map[string]bool, items []item) []Finding {
	var out []Finding
	var corpus string
	loaded := false
	for _, it := range items {
		prose := spanRe.ReplaceAllString(it.text, "")
		if !ciRe.MatchString(prose) {
			continue
		}
		var cmds []string
		for _, s := range spans(it.text) {
			if _, ok := command(s); ok {
				cmds = append(cmds, s)
			}
		}
		if len(cmds) == 0 {
			if !statusRe.MatchString(it.text) {
				out = append(out, Finding{"warn", "ci", it.label + " mentions CI without naming its exact command in backticks"})
			}
			continue
		}
		if files == nil {
			continue
		}
		if !loaded {
			loaded = true
			c, err := workflowCorpus(ctx, g, top, ref, files)
			if err != nil {
				out = append(out, Finding{"warn", "ci", fmt.Sprintf("cannot read workflows on %s: %s", ref, firstLine(err))})
			}
			corpus = c
		}
		if corpus == "" {
			continue
		}
		for _, c := range cmds {
			for _, part := range splitRe.Split(c, -1) {
				part = strings.TrimSpace(strings.Trim(strings.TrimSpace(spaceRe.ReplaceAllString(part, " ")), `'"`))
				if first, _, _ := strings.Cut(part, " "); part == "" || builtins[first] || first == "gh" {
					continue
				}
				if !strings.Contains(corpus, part) {
					out = append(out, Finding{"warn", "ci", fmt.Sprintf("%s: %q is not run by .github/workflows on %s", it.label, part, ref)})
				}
			}
		}
	}
	return out
}

// workflowCorpus is the whitespace-collapsed text of the workflows on ref, plus
// the justfile and flake.nix when a workflow hands its commands to them; "" when
// the ref has no workflows. A workflow or helper that cannot be read is an error.
func workflowCorpus(ctx context.Context, g git.Client, top, ref string, files map[string]bool) (string, error) {
	var b strings.Builder
	for f := range files {
		if workflowRe.MatchString(f) {
			text, err := g.Run(ctx, top, "show", ref+":"+f)
			if err != nil {
				return "", err
			}
			b.WriteString(text)
			b.WriteByte('\n')
		}
	}
	if b.Len() == 0 {
		return "", nil
	}
	text := b.String()
	for _, extra := range []struct{ file, trigger string }{{"justfile", "just "}, {"flake.nix", "nix flake check"}} {
		if files[extra.file] && strings.Contains(text, extra.trigger) {
			text, err := g.Run(ctx, top, "show", ref+":"+extra.file)
			if err != nil {
				return "", err
			}
			b.WriteString(text)
			b.WriteByte('\n')
		}
	}
	return spaceRe.ReplaceAllString(b.String(), " "), nil
}
