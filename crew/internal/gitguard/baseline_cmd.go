package gitguard

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"unicode/utf8"
)

// BaselineOptions is what `crew git-baseline` reads from the process.
type BaselineOptions struct {
	// IsTTY reports whether stdin is a terminal; --accept refuses otherwise.
	IsTTY func() bool
	// In is where --accept reads the typed answer.
	In io.Reader
}

// ctxEntry is one `config --list` triple of a listed context.
type ctxEntry struct {
	label, scope, origin, rec string
}

// RunBaseline is `crew git-baseline [--accept]`: it lists — or, with --accept,
// merges into — the baseline Check enforces. Values print masked or withheld
// behind their fingerprint, and --accept writes exactly the pairs this run
// printed.
func RunBaseline(accept bool, cwd string, o BaselineOptions, stdout, stderr io.Writer) int {
	if accept && !o.IsTTY() {
		say(stderr, "crew: git-baseline --accept needs your own terminal (not Claude Code's ! prefix)\n")
		return 1
	}

	out, err := git(cwd, io.Discard, "rev-parse", "--path-format=absolute", "--git-common-dir")
	common := strings.TrimSuffix(string(out), "\n")
	if err != nil || common == "" {
		say(stderr, "crew: not in a git repo\n")
		return 1
	}
	if PinCwd(common, cwd, stderr) != nil {
		return 1
	}
	ctxs := []string{common}
	for _, admin := range linkedAdmins(common) {
		if PinAdmin(common, admin, stderr) != nil {
			return 1
		}
		ctxs = append(ctxs, admin)
	}

	file := BaselinePath(common)
	fi, err := os.Stat(file)
	exists := err == nil && fi.Mode().IsRegular()
	var recs []string
	switch {
	case exists:
		if recs, err = ReadBaseline(file); err != nil {
			say(stderr, "crew: cannot read the git-config baseline %s\n", file)
			return 1
		}
	case !accept:
		if armed, _ := isArmed(common); armed {
			say(stderr, "git-config baseline %s: missing, but this repo had one — a dispatch refuses until you run `crew git-baseline --accept` from your own terminal\n", file)
			return 1
		}
		say(stderr, "git-config baseline %s: none yet — the next dispatch records it, or run `crew git-baseline --accept` from your own terminal\n", file)
		return 1
	}

	r, err := realpath("", common)
	if err != nil {
		r = ""
	}
	base := map[string]bool{}
	marked := false
	var aliases []string
	for _, rec := range recs {
		if rec == "" {
			continue
		}
		marked = marked || rec == RedirectMark
		// A baselined alias the worker unset now can come back without drift.
		if k, v, ok := strings.Cut(rec, "\n"); ok && isAliasKey(k) {
			aliases = append(aliases, v)
		}
		base[Canon(r, rec)] = true
	}

	if exists && !marked {
		say(stdout, "git-config baseline %s predates redirect-key coverage — the next dispatch records the redirect keys present then, or --accept does\n", file)
	}

	// Every context's aliases apply to every pair: git in a linked worktree
	// may rewrite the main checkout's url through an alias only it sees.
	var all []ctxEntry
	for _, ctx := range ctxs {
		label := "main checkout"
		if ctx != common {
			label = "worktree " + Quote(ctx[strings.LastIndexByte(ctx, '/')+1:])
		}
		out, err := git(cwd, stderr, "--git-dir="+ctx, "config", "--list", "--show-origin", "--show-scope", "-z")
		if err != nil {
			say(stderr, "crew: cannot list the git config of %s\n", ctx)
			return 1
		}
		listing := splitNUL(out)
		for i := 0; i+2 < len(listing); i += 3 {
			rec := listing[i+2]
			all = append(all, ctxEntry{label: label, scope: listing[i], origin: listing[i+1], rec: rec})
			// A human's global insteadOf rewrites a local remote url too.
			if k, v, ok := strings.Cut(rec, "\n"); ok && isAliasKey(k) {
				aliases = append(aliases, v)
			}
		}
	}

	var shown []string
	var entries []listed
	var cmds []string
	seen := map[string]bool{}
	for _, e := range all {
		if e.scope != "local" && e.scope != "worktree" {
			continue
		}
		rec := e.rec
		if !strings.Contains(rec, "\n") {
			rec += "\n"
		}
		key, value := keyValue(rec)
		if !Guarded(key, value) || base[Canon(r, rec)] {
			continue
		}
		origin := strings.TrimPrefix(e.origin, "file:")
		if seen[origin+"\n"+rec] {
			continue
		}
		seen[origin+"\n"+rec] = true
		entries, cmds = addListed(entries, cmds, key, value, aliases, e.label, origin, "")
		shown = append(shown, rec)
		if isAliasKey(key) {
			entries, cmds = addRewritten(entries, cmds, all, key, value)
		}
	}
	root := strings.TrimSuffix(common, "/.git")
	if len(entries) > 0 {
		since := "since the last baseline"
		if !exists {
			since = "no baseline yet"
		}
		renderListing(stdout, root, len(shown), since, entries, cmds)
	}

	legacy := LegacyDrift(common, base)
	if len(legacy) > 0 && len(entries) > 0 {
		say(stdout, "\n")
	}
	for _, e := range legacy {
		if e.Unsafe {
			say(stdout, "legacy %s entry %s: unsafe — inspect and remove it; --accept never records a legacy entry\n", e.Kind, e.Shown)
		} else {
			say(stdout, "legacy %s entry %s: new or changed from the baseline — inspect and remove it; --accept never records a legacy entry\n", e.Kind, e.Shown)
		}
	}

	if len(shown) == 0 && len(legacy) == 0 && exists && marked {
		say(stdout, "git-config baseline %s: no drift\n", file)
		return 0
	}
	if !accept {
		switch {
		case len(legacy) > 0 && len(shown) > 0:
			say(stdout, "\nRemove the legacy entries above first (--accept refuses while they are listed), then record the rest: crew git-baseline --accept\n")
		case len(legacy) > 0:
			say(stdout, "\nRemove the legacy entries above, then re-run: crew git-baseline\n")
		default:
			say(stdout, "\nIf they're yours, record them: crew git-baseline --accept\n")
		}
		return 1
	}
	if len(legacy) > 0 {
		say(stderr, "crew: git-baseline --accept refuses while legacy remotes/branches entries are listed — remove them, then re-run\n")
		return 1
	}

	switch {
	case len(shown) == 0:
		say(stdout, "No guarded git config yet. Record an empty baseline as trusted? Type yes: ")
	case slices.ContainsFunc(entries, func(e listed) bool { return e.reason != "" }):
		say(stdout, "\nRecord %s as trusted? The sha256 values should match what you checked. Type yes: ", these(len(shown)))
	default:
		say(stdout, "\nRecord %s as trusted? Type yes: ", these(len(shown)))
	}
	if readAnswer(o.In) != "yes" {
		say(stderr, "crew: baseline unchanged\n")
		return 1
	}

	if err := os.MkdirAll(common+"/crew", 0o777); err != nil {
		say(stderr, "crew: %v\n", err)
		return 1
	}
	if err := arm(common); err != nil {
		say(stderr, "crew: %v\n", err)
		return 1
	}
	// Merge: this run shows only the drift. The marker is always written, so an
	// empty set is never a 0-byte file or a lone NUL (an empty record).
	var merged []string
	for _, rec := range append(append(recs, shown...), RedirectMark) {
		if rec != "" {
			merged = append(merged, rec)
		}
	}
	if err := WriteBaseline(file, merged); err != nil {
		say(stderr, "crew: %v\n", err)
		return 1
	}
	say(stdout, "git-config baseline %s: accepted\n", file)
	return 0
}

func isAliasKey(k string) bool {
	return globAny(k, "url.*.insteadof", "url.*.pushinsteadof")
}

// listed is one entry of the listing: a plain `key=value` text, or, when its
// value is withheld, the key, why, and the fingerprint of the value.
type listed struct {
	label, origin, via string
	text               string
	key, reason, fp    string
}

// addRewritten lists the remote urls the drifted alias rewrites: one may have
// been accepted earlier while shown masked.
func addRewritten(entries []listed, cmds []string, all []ctxEntry, aliasKey, alias string) ([]listed, []string) {
	aliases := []string{alias}
	via := ShowKey(aliasKey)
	seen := map[string]bool{}
	for _, e := range all {
		if e.scope != "local" && e.scope != "worktree" {
			continue
		}
		k, v, ok := strings.Cut(e.rec, "\n")
		if !ok || !globAny(k, "remote.*.url", "remote.*.pushurl") || !Rewritten(v, aliases) {
			continue
		}
		origin := strings.TrimPrefix(e.origin, "file:")
		if seen[origin+"\n"+e.rec] {
			continue
		}
		seen[origin+"\n"+e.rec] = true
		entries, cmds = addListed(entries, cmds, k, v, aliases, e.label, origin, via)
	}
	return entries, cmds
}

// addListed appends one pair, and the command that reads it in the human's own
// terminal when its value or key is withheld. A withheld value carries its
// fingerprint, so a value swapped between the listing the human checked and
// the --accept listing shows.
func addListed(entries []listed, cmds []string, k, v string, aliases []string, label, origin, via string) ([]listed, []string) {
	e := listed{label: label, origin: origin, via: via}
	var reason string
	e.text, reason = classify(k, v, aliases)
	if reason != "" {
		e.key, e.reason, e.fp = ShowKey(k), reason, fingerprint(v)
	}
	if _, keyWithheld := showKey(k); reason != "" || keyWithheld {
		if cmd := ownTerminalCmd(k); !slices.Contains(cmds, cmd) {
			cmds = append(cmds, cmd)
		}
	}
	return append(entries, e), cmds
}

func these(n int) string {
	if n == 1 {
		return "this entry"
	}
	return fmt.Sprintf("these %d entries", n)
}

// renderListing prints the entries grouped by the file that sets them, then
// what is hidden and how to read it.
func renderListing(w io.Writer, root string, n int, since string, entries []listed, cmds []string) {
	noun := "entries"
	if n == 1 {
		noun = "entry"
	}
	say(w, "git-config baseline: %d new %s in %s (%s)\n", n, noun, Quote(root), since)

	type group struct {
		in   string
		rows []listed
	}
	var groups []*group
	idx := map[string]*group{}
	keyW, noteW := 0, 0
	for _, e := range entries {
		if e.reason != "" {
			keyW = max(keyW, utf8.RuneCountInString(e.key))
			noteW = max(noteW, utf8.RuneCountInString(hiddenNote(e.reason)))
		}
		id := e.label + "\x00" + e.origin
		g := idx[id]
		if g == nil {
			rel := strings.TrimPrefix(e.origin, root+"/")
			g = &group{in: e.label + " (" + Quote(rel) + ")"}
			idx[id] = g
			groups = append(groups, g)
		}
		g.rows = append(g.rows, e)
	}
	for _, g := range groups {
		say(w, "\n")
		for _, e := range g.rows {
			line := "  " + e.text
			if e.reason != "" {
				line = fmt.Sprintf("  %-*s  %-*s  sha256:%s", keyW, e.key, noteW, hiddenNote(e.reason), e.fp)
			}
			if e.via != "" {
				line += "  (rewritten by " + e.via + ")"
			}
			say(w, "%s\n", line)
		}
		say(w, "  in: %s\n", g.in)
	}
	if len(cmds) == 0 {
		return
	}
	say(w, "\nHidden values stay off this screen: a planted value could carry a token or terminal escapes.\n")
	say(w, "See them safely (each byte prints as a character or an escape; each entry ends at \\0):\n")
	cmdW := 0
	for _, c := range cmds {
		cmdW = max(cmdW, utf8.RuneCountInString(c))
	}
	for _, c := range cmds {
		say(w, "  %-*s%s\n", cmdW, c, odPipe)
	}
	say(w, "  (run these from each checkout or worktree that may set them)\n")
}

func hiddenNote(reason string) string {
	if reason == "exec value" {
		return "runs a command — value hidden"
	}
	return "value hidden (" + reason + ")"
}

// readAnswer is bash's `read -r`: one line, a missing final newline still
// yielding its text, with IFS whitespace trimmed from both ends.
func readAnswer(in io.Reader) string {
	line, _ := bufio.NewReader(in).ReadString('\n')
	return strings.Trim(line, " \t\n")
}
