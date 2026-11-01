package gitguard

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Coverage marks: a baseline guards redirect-class pairs, and legacy entries,
// only once it carries the mark (pre-#678 baselines guard the exec class only).
const (
	RedirectMark = "#covers\nredirect"
	LegacyMark   = "#covers\nlegacy"
)

// BaselinePath is common's NUL-separated git-config baseline.
func BaselinePath(common string) string {
	return common + "/crew/git-config-baseline"
}

// ReadBaseline reads a baseline's records. Inner empty records are kept;
// consumers skip them.
func ReadBaseline(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return splitNUL(b), nil
}

// WriteBaseline replaces path with recs, byte-sorted, unique and
// NUL-terminated, through a temp file renamed over it.
func WriteBaseline(path string, recs []string) (err error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = os.Remove(tmp.Name())
		}
	}()
	var b strings.Builder
	for _, rec := range sortUnique(slices.Clone(recs)) {
		b.WriteString(rec)
		b.WriteByte(0)
	}
	_, werr := tmp.WriteString(b.String())
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return werr
	}
	return os.Rename(tmp.Name(), path)
}

// Init records common's baseline once, trust on first use: it never
// overwrites one, and only adds a coverage class an older baseline lacks —
// never exec-class pairs, which would launder drift. Once common is armed, a
// missing baseline refuses: only `crew git-baseline --accept` writes a new one.
func Init(common string, stderr io.Writer) error {
	if err := pinMain(common, stderr); err != nil {
		return err
	}
	file := BaselinePath(common)
	fi, err := os.Lstat(file)
	switch {
	case err == nil:
		if err := arm(common); err != nil {
			return err
		}
		if !fi.Mode().IsRegular() {
			return nil
		}
		return migrate(common, file, stderr)
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}
	union, err := Union(common, stderr)
	if err != nil {
		return err
	}
	aliases, err := aliasValues(common, stderr)
	if err != nil {
		return err
	}
	// Union runs the linked-admin pins, whose refusal names a planted commondir
	// and must win over the armed refusal.
	armed, err := isArmed(common)
	if armed {
		if err != nil {
			return err
		}
		marker, _ := ArmedPath(common)
		say(stderr, "refusing git-config baseline: %s is missing, but this repo had one (armed at %s) — a dispatch never re-records it: inspect the git config, then run `crew git-baseline --accept` from your own terminal\n", file, marker)
		return ErrRefused
	}
	if err := os.MkdirAll(common+"/crew", 0o777); err != nil {
		return err
	}
	recs := append(union, RedirectMark, LegacyMark)
	for _, rec := range LegacyRecords(common) {
		if _, kind, _, _ := legacyFields(rec); kind == "file" {
			recs = append(recs, rec)
		}
	}
	recs = sortUnique(recs)
	if err := arm(common); err != nil {
		return err
	}
	if err := WriteBaseline(file, recs); err != nil {
		return err
	}
	noteKeys(stderr, file, file, recs, aliases)
	return nil
}

func migrate(common, file string, stderr io.Writer) error {
	recs, err := ReadBaseline(file)
	if err != nil {
		return err
	}
	redirectCovered, legacyCovered := false, false
	for _, rec := range recs {
		redirectCovered = redirectCovered || rec == RedirectMark
		legacyCovered = legacyCovered || rec == LegacyMark
	}
	if redirectCovered && legacyCovered {
		return nil
	}
	var add, aliases []string
	if !redirectCovered {
		union, err := Union(common, stderr)
		if err != nil {
			return err
		}
		if aliases, err = aliasValues(common, stderr); err != nil {
			return err
		}
		for _, rec := range union {
			if IsRedirect(keyValue(rec)) {
				add = append(add, rec)
			}
		}
	}
	if !legacyCovered && len(LegacyRecords(common)) > 0 {
		say(stderr, "refusing git-config baseline migration: legacy remotes or branches entries exist — inspect and remove them before re-running dispatch\n")
		return ErrRefused
	}
	all := append(append(slices.Clone(recs), add...), RedirectMark, LegacyMark)
	if err := WriteBaseline(file, all); err != nil {
		return err
	}
	noteKeys(stderr, file, "redirect keys in "+file, add, aliases)
	return nil
}

// noteKeys prints the records' keys, then each redirect pair through
// showRecorded. An exec value is never printed: a credential.helper value may
// carry a token.
func noteKeys(stderr io.Writer, file, what string, recs, aliases []string) {
	var keys []string
	seen := map[string]bool{}
	for _, rec := range recs {
		if rec == RedirectMark || rec == LegacyMark {
			continue
		}
		var raw, disp string
		if strings.HasPrefix(rec, legacyPrefix) {
			ns, _, name, _ := legacyFields(rec)
			raw = ns + "/" + name
			disp = Quote(raw)
		} else {
			raw, _ = keyValue(rec)
			disp = ShowKey(raw)
		}
		if seen[raw] {
			continue
		}
		seen[raw] = true
		keys = append(keys, disp)
	}
	if len(keys) == 0 {
		return
	}
	say(stderr, "git-config baseline: recorded %s: %s\n", what, strings.Join(keys, ", "))
	shown := false
	pointed := map[string]bool{}
	for _, rec := range recs {
		if rec == RedirectMark || rec == LegacyMark || strings.HasPrefix(rec, legacyPrefix) {
			continue
		}
		k, v := keyValue(rec)
		if !IsRedirect(k, v) {
			if kd, withheld := showKey(k); withheld && !pointed[k] {
				pointed[k] = true
				say(stderr, "  %s%s\n", kd, ownTerminal(k))
			}
			continue
		}
		say(stderr, "  %s\n", showRecorded(k, v, aliases))
		shown = true
	}
	if shown {
		say(stderr, "  human: check each value above is yours — a planted one stays trusted: restore it in its config file, then rebuild the baseline from your own terminal before any dispatch: move %s aside and run `crew git-baseline --accept` (dispatch refuses until you do)\n", Quote(file))
	}
}

// showValue is ShowPair, or the key and why its value is withheld: an exec
// value may carry a token, and a value with userinfo or a query prints only
// masked.
func showValue(k, v string, aliases []string) (string, bool) {
	kd := ShowKey(k)
	if IsExec(k, v) {
		return kd + ": value withheld (exec value)", true
	}
	shown := ShowPair(k, v, aliases)
	if !strings.ContainsAny(v, "@?") || shown != kd+"="+Quote(v) {
		return shown, false
	}
	reason := "outside the mask grammar"
	if globAny(k, "remote.*.url", "remote.*.pushurl") && Rewritten(v, aliases) {
		reason = "an insteadOf alias rewrites it"
	}
	return kd + ": value withheld (" + reason + ")", true
}

// showRecorded is showValue, plus a pointer to read the value from one's own
// terminal when it or the key is withheld: the note reaches the dispatcher,
// not the human's terminal.
func showRecorded(k, v string, aliases []string) string {
	shown, withheld := showValue(k, v, aliases)
	if _, keyWithheld := showKey(k); withheld || keyWithheld {
		return shown + ownTerminal(k)
	}
	return shown
}

// ownTerminal points at the command that reads k's value in the human's own
// terminal.
func ownTerminal(k string) string {
	cmd := "git config -z --show-origin --get-all " + Quote(k)
	// A URL-subsection key can carry userinfo ShowKey cannot mask either.
	if match(urlSubsectionKeys, k, "") && strings.ContainsAny(k, "@?") {
		sec, _, _ := strings.Cut(k, ".")
		cmd = "git config -z --show-origin --get-regexp " + Quote("^"+sec+`\.`)
	}
	// git prints values raw, so a planted CR or ESC could redraw the terminal and
	// a planted LF or TAB forge another origin and value; -z ends each entry with
	// NUL, which no value can hold, and in the C locale od -c prints every byte in
	// its own cell, as an ASCII character or an escape (another locale may print a
	// multibyte character as its glyph), so no value can spell an entry's end.
	return ` — check it in your own terminal (od shows each byte as a character or an escape; each entry ends at \0), from each checkout or worktree that may set it: ` + cmd + " | env LC_ALL=C od -An -c -v"
}
