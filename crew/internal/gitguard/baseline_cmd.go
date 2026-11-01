package gitguard

import (
	"bufio"
	"io"
	"os"
	"strings"
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
	withheld := false
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
		withheld = listPair(stdout, "", key, value, aliases, e.label, origin) || withheld
		shown = append(shown, rec)
		if isAliasKey(key) {
			withheld = showRewritten(stdout, all, value) || withheld
		}
	}

	legacy := LegacyDrift(common, base)
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
		return 1
	}
	if len(legacy) > 0 {
		say(stderr, "crew: git-baseline --accept refuses while legacy remotes/branches entries are listed — remove them, then re-run\n")
		return 1
	}

	if len(shown) == 0 {
		say(stdout, "no guarded git config yet — yes records an empty baseline\n")
	}
	if withheld {
		say(stdout, "withheld above: type yes only if a plain `crew git-baseline` listing showed the same sha256 for each, and you then checked each value with the git config command under it\n")
	}
	say(stdout, "Accept these into the baseline %s? type yes: ", file)
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

// showRewritten lists the remote urls the drifted alias rewrites: one may have
// been accepted earlier while shown masked. It reports whether it printed an
// own-terminal pointer.
func showRewritten(stdout io.Writer, all []ctxEntry, alias string) bool {
	aliases := []string{alias}
	pointed := false
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
		pointed = listPair(stdout, "  rewritten by the alias above: ", k, v, aliases, e.label, origin) || pointed
	}
	return pointed
}

// listPair prints one listed pair, then the own-terminal pointer when its
// value or key is withheld; it reports whether it printed the pointer. A
// withheld value carries its fingerprint, so a value swapped between the
// listing the human checked and the --accept listing shows.
func listPair(stdout io.Writer, lead, k, v string, aliases []string, label, origin string) bool {
	disp, withheld := showValue(k, v, aliases)
	if withheld {
		disp += " [sha256:" + fingerprint(v) + "]"
	}
	say(stdout, "%s%s (%s, %s)\n", lead, disp, label, Quote(origin))
	kd, keyWithheld := showKey(k)
	if !withheld && !keyWithheld {
		return false
	}
	say(stdout, "  %s%s\n", kd, ownTerminal(k))
	return true
}

// readAnswer is bash's `read -r`: one line, a missing final newline still
// yielding its text, with IFS whitespace trimmed from both ends.
func readAnswer(in io.Reader) string {
	line, _ := bufio.NewReader(in).ReadString('\n')
	return strings.Trim(line, " \t\n")
}
