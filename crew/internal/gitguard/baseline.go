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
// never exec-class pairs, which would launder drift.
func Init(common string, stderr io.Writer) error {
	if err := pinMain(common, stderr); err != nil {
		return err
	}
	file := BaselinePath(common)
	fi, err := os.Lstat(file)
	switch {
	case err == nil && fi.Mode()&fs.ModeSymlink != 0:
		return nil
	case err == nil:
		if !fi.Mode().IsRegular() {
			return nil
		}
		return migrate(common, file, stderr)
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}
	if err := os.MkdirAll(common+"/crew", 0o777); err != nil {
		return err
	}
	union, err := Union(common, stderr)
	if err != nil {
		return err
	}
	recs := append(union, RedirectMark, LegacyMark)
	for _, rec := range LegacyRecords(common) {
		if _, kind, _, _ := legacyFields(rec); kind == "file" {
			recs = append(recs, rec)
		}
	}
	recs = sortUnique(recs)
	if err := WriteBaseline(file, recs); err != nil {
		return err
	}
	noteKeys(stderr, file, recs)
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
	var add []string
	if !redirectCovered {
		union, err := Union(common, stderr)
		if err != nil {
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
	noteKeys(stderr, "redirect keys in "+file, add)
	return nil
}

// noteKeys prints the records' keys, never their values: a credential.helper
// value may carry a token.
func noteKeys(stderr io.Writer, what string, recs []string) {
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
}
