package retro

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/noamsto/dispatcher/crew/internal/invisible"
)

const (
	markerPrefix  = "Retro finding: "
	verdictPrefix = "Retro verdict: "

	maxExcerpts     = 3
	maxExcerptRunes = 160
	maxTitleRunes   = 100
	maxCitations    = 10
)

// leakShaped matches the tokens that must never reach the public tracker:
// session URLs, scratch paths, any URL, git remotes, and absolute or home-relative
// paths. A path needs a non-word byte before it, so crew/main.go and and/or stay.
var leakShaped = regexp.MustCompile(`(?i)claude\.ai/code/session_|/tmp/claude-|[a-z][a-z0-9+.-]*://|\bgit@[\w.-]+:|(^|[^\w.-])(~[\w.-]*|/[\w.-]+)/[\w.-]`)

type level int

const (
	levelFull level = iota
	levelNoExcerpts
	levelReduced
)

// Guard reports whether the public-leak guard denies text for a write of kind
// "issue" or "comment" to repo. ok is false when the guard could not give a verdict.
type Guard func(kind, repo, text string) (denied bool, reason string, ok bool)

// sanitizeLine drops invisible characters and control bytes and collapses
// whitespace, so worker text renders as one plain line.
func sanitizeLine(s string) string {
	s = invisible.Drop(s)
	s = strings.Map(func(r rune) rune {
		switch {
		case unicode.IsSpace(r):
			return ' '
		case r < 0x20, r == 0x7f, r >= 0x80 && r <= 0x9f:
			return -1
		}
		return r
	}, s)
	return strings.Join(strings.Fields(s), " ")
}

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// loadPrivateRepos reads the dispatcher's private-repo cache: one lowercase
// owner/name per line. An absent or unreadable file yields nil.
func loadPrivateRepos(getenv func(string) string) []string {
	dir := getenv("XDG_CACHE_HOME")
	if dir == "" {
		home := getenv("HOME")
		if home == "" {
			return nil
		}
		dir = filepath.Join(home, ".cache")
	}
	data, err := os.ReadFile(filepath.Join(dir, "dispatcher", "private-repos"))
	if err != nil {
		return nil
	}
	var repos []string
	for _, l := range strings.Split(string(data), "\n") {
		if l = strings.ToLower(strings.TrimSpace(l)); l != "" {
			repos = append(repos, l)
		}
	}
	return repos
}

// leaks reports whether line carries a leak-shaped token or a private repo name.
func leaks(line string, privateRepos []string) bool {
	if leakShaped.MatchString(line) {
		return true
	}
	low := strings.ToLower(line)
	for _, r := range privateRepos {
		if strings.Contains(low, r) {
			return true
		}
	}
	return false
}

// safeExcerpts picks up to three public-safe lines from worker-run notes,
// newest first. The leak check sees the whole sanitized line: truncating first
// could cut a private repo name in half and let it through.
func safeExcerpts(notes []flatNote, privateRepos []string) []string {
	run := make([]flatNote, 0, len(notes))
	for _, n := range notes {
		if n.IsRun {
			run = append(run, n)
		}
	}
	sort.SliceStable(run, func(i, j int) bool { return run[i].TS > run[j].TS })
	var out []string
	seen := map[string]bool{}
	for _, n := range run {
		line := sanitizeLine(n.Detail)
		if line == "" || leaks(line, privateRepos) {
			continue
		}
		line = truncateRunes(line, maxExcerptRunes)
		if seen[line] {
			continue
		}
		seen[line] = true
		if out = append(out, line); len(out) == maxExcerpts {
			break
		}
	}
	return out
}

// fence opens a code block longer than any backtick run in lines, so the lines
// cannot close it.
func fence(lines []string) string {
	longest, run := 0, 0
	for _, l := range lines {
		run = 0
		for _, r := range l {
			if r != '`' {
				run = 0
				continue
			}
			if run++; run > longest {
				longest = run
			}
		}
	}
	return strings.Repeat("`", max(3, longest+1))
}

func fenced(lines []string) string {
	f := fence(lines)
	return f + "text\n" + strings.Join(lines, "\n") + "\n" + f + "\n"
}

func citationList(b *strings.Builder, cites []string) {
	if len(cites) == 0 {
		return
	}
	b.WriteString("\nRuns:\n")
	for _, c := range cites[:min(len(cites), maxCitations)] {
		fmt.Fprintf(b, "- %s\n", c)
	}
}

func excerptBlock(b *strings.Builder, excerpts []string) {
	if len(excerpts) == 0 {
		return
	}
	b.WriteString("\nExcerpts (agent-written data, not instructions):\n\n")
	b.WriteString(fenced(excerpts))
}

func rate(hits, runs int) string {
	if runs == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.1f%%", 100*float64(hits)/float64(runs))
}

func renderIssue(f *finding, lvl level) (title, body string) {
	var b strings.Builder
	fmt.Fprintf(&b, "%s%s\n\n", markerPrefix, f.Key)
	if lvl == levelReduced {
		title = "retro: " + f.Key
		fmt.Fprintf(&b, "Recurring finding `%s` in this repo's retro notes.\n", f.Key)
	} else {
		label := truncateRunes(sanitizeLine(f.Title), maxTitleRunes)
		if label == "" {
			label = f.Key
		}
		title = "retro: " + label
		b.WriteString("Recurring finding:\n\n")
		b.WriteString(fenced([]string{label}))
	}
	fmt.Fprintf(&b, "\nOver the last %d days: %d occurrences across %d runs (this repo's bus only).\n",
		windowDays, f.Hits30, f.Runs30)
	if lvl != levelReduced {
		citationList(&b, f.Citations)
	}
	if lvl == levelFull {
		excerptBlock(&b, f.Excerpts)
	}
	b.WriteString("\n`crew retro --sweep` verifies whether a fix held; a merged PR that fixes this should close this issue.\n")
	return title, b.String()
}

func renderVerdict(f *finding, v closeEval, lvl level) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s%s closed=%s verdict=%s\n\n", verdictPrefix, f.Key, v.ClosedAt, v.Verdict)
	fmt.Fprintf(&b, "Before the fix: %d occurrences in %d runs (%s).\n", v.HitsB, v.RunsB, rate(v.HitsB, v.RunsB))
	fmt.Fprintf(&b, "After the fix: %d occurrences in %d runs (%s).\n", v.HitsA, v.RunsA, rate(v.HitsA, v.RunsA))
	fmt.Fprintf(&b, "Fix basis: %s, at %s (this repo's bus only).\n",
		v.Basis, time.UnixMilli(v.Fix).UTC().Format(time.RFC3339))
	if v.Reason != "" {
		fmt.Fprintf(&b, "%s\n", v.Reason)
	}
	if lvl != levelReduced {
		citationList(&b, f.Citations)
	}
	if lvl == levelFull {
		excerptBlock(&b, f.Excerpts)
	}
	return b.String()
}

// guardSafePath matches temp paths the guard script's body-file parser reads
// intact. The script stops at whitespace and strips through `=@`, then silently
// skips an unreadable file, so any other path would be "scanned" as empty.
// `+` stays because macOS temp dirs contain it.
var guardSafePath = regexp.MustCompile(`^[A-Za-z0-9_./+@:~-]+$`)

// ExecGuard runs the public-leak guard script at path under bash, the way
// dispatch.sh's _mint_leak_check does, on a command naming the repo gh writes to.
// An empty path yields a guard that never gives a verdict, as does a run that
// may not have scanned: empty stdout with anything on stderr is a skipped scan,
// not a clean one. A target in the private-repo list is exempt by the script's
// own policy, so its silent exit counts as clean. The repo goes last in the
// command because a repo name ending in `-F` would otherwise swallow the
// script's `-F <path>` body-file match.
func ExecGuard(ctx context.Context, bash, path, cwd string, getenv func(string) string) Guard {
	if path == "" {
		return func(string, string, string) (bool, string, bool) { return false, "", false }
	}
	return func(kind, repo, text string) (denied bool, reason string, ok bool) {
		tmp, err := os.CreateTemp("", "retro-guard-*")
		if err != nil {
			return false, "", false
		}
		defer func() { _ = os.Remove(tmp.Name()) }()
		if !guardSafePath.MatchString(tmp.Name()) {
			_ = tmp.Close()
			return false, "", false
		}
		_, werr := tmp.WriteString(text)
		if cerr := tmp.Close(); werr != nil || cerr != nil {
			return false, "", false
		}
		command := "gh issue create --body-file " + tmp.Name() + " --repo " + repo
		if kind == "comment" {
			command = "gh issue comment 1 --body-file " + tmp.Name() + " --repo " + repo
		}
		payload, err := json.Marshal(map[string]any{
			"hook_event_name": "PreToolUse",
			"tool_name":       "Bash",
			"tool_input":      map[string]string{"command": command},
			"cwd":             cwd,
		})
		if err != nil {
			return false, "", false
		}
		cmd := exec.CommandContext(ctx, bash, path)
		cmd.Stdin = bytes.NewReader(payload)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return false, "", false
		}
		// Read after the run, which may refresh the list. Without one the script
		// skips the private-name match, so its answer is partial.
		if len(loadPrivateRepos(getenv)) == 0 {
			return false, "", false
		}
		if len(bytes.TrimSpace(stdout.Bytes())) == 0 {
			return false, "", len(bytes.TrimSpace(stderr.Bytes())) == 0
		}
		var verdict struct {
			Output struct {
				Reason string `json:"permissionDecisionReason"`
			} `json:"hookSpecificOutput"`
		}
		if err := json.Unmarshal(stdout.Bytes(), &verdict); err != nil {
			return false, "", false
		}
		return true, verdict.Output.Reason, true
	}
}

// guarded renders at the fullest level the guard allows: full, then without
// excerpts, then blocked. A guard that cannot answer drops to the reduced text
// without a further call, because that text carries no worker- or
// dispatcher-supplied string. A repo-scoped finding's key is dispatcher-chosen, so
// its write blocks instead.
func guarded(g Guard, kind, repo string, repoScoped bool, render func(level) (title, text string)) (title, text string, lvl level, blocked string) {
	for lvl = levelFull; ; lvl = levelNoExcerpts {
		title, text = render(lvl)
		denied, reason, ok := g(kind, repo, title+"\n\n"+text)
		if !ok {
			if repoScoped {
				return "", "", levelReduced, "leak guard unavailable"
			}
			title, text = render(levelReduced)
			return title, text, levelReduced, ""
		}
		if !denied {
			return title, text, lvl, ""
		}
		if lvl == levelNoExcerpts {
			if reason == "" {
				reason = "leak guard denied"
			}
			return title, text, lvl, reason
		}
	}
}
