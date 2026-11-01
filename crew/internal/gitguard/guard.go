package gitguard

import (
	"io"
	"os"
	"strings"
)

// Check refuses when gitdir's ("" = common) exec-capable or, once marked,
// endpoint-redirecting config, or common's legacy remotes/branches entries,
// drifted from common's baseline. It prints nothing when it passes.
func Check(common, gitdir string, stderr io.Writer) error {
	if gitdir == "" {
		gitdir = common
	}
	if err := Pin(common, gitdir, stderr); err != nil {
		return err
	}
	return check(common, gitdir, "", stderr)
}

// check is Check after the pin; dir is the cwd git and realpath run in.
func check(common, gitdir, dir string, stderr io.Writer) error {
	file := BaselinePath(common)
	if fi, err := os.Stat(file); err != nil || !fi.Mode().IsRegular() {
		if armed, _ := isArmed(common); armed {
			say(stderr, "refusing git: no git-config baseline at %s, but this repo had one — a dispatch never re-records it: run `crew git-baseline --accept` from your own terminal\n", file)
			return ErrRefused
		}
		say(stderr, "refusing git: no git-config baseline at %s — the next dispatch records it, or run `crew git-baseline --accept` from your own terminal\n", file)
		return ErrRefused
	}
	r, err := realpath(dir, common)
	if err != nil {
		r = ""
	}
	recs, err := ReadBaseline(file)
	if err != nil {
		return err
	}
	base := map[string]bool{}
	redirect := false
	for _, rec := range recs {
		if rec == "" {
			continue
		}
		redirect = redirect || rec == RedirectMark
		base[Canon(r, rec)] = true
	}
	got, err := pairs(gitdir, dir, stderr)
	if err != nil {
		say(stderr, "refusing git: cannot list the git config of %s\n", gitdir)
		return ErrRefused
	}
	if legacy := LegacyDrift(common, base); len(legacy) > 0 {
		for _, e := range legacy {
			if e.Unsafe {
				say(stderr, "refusing git: unsafe legacy %s entry %s in %s\n", e.Kind, e.Shown, Quote(common))
			} else {
				say(stderr, "refusing git: legacy %s entry %s is new or changed from the git-config baseline %s\n", e.Kind, e.Shown, Quote(file))
			}
		}
		say(stderr, "refusing git: inspect and remove the legacy entry before re-running; the dispatcher never follows or repairs a legacy entry\n")
		return ErrRefused
	}
	var drift []string
	bad := map[string]bool{}
	seen := map[string]bool{}
	for _, rec := range got {
		if base[Canon(r, rec)] {
			continue
		}
		k, v := keyValue(rec)
		// A pre-#678 baseline guards the exec class only until dispatch's init migrates it.
		if !redirect && IsRedirect(k, v) {
			continue
		}
		if !seen[k] {
			drift = append(drift, k)
		}
		seen[k] = true
		bad[rec] = true
	}
	if len(drift) == 0 {
		return nil
	}
	reportDrift(gitdir, dir, file, drift, bad, stderr)
	return ErrRefused
}

// reportDrift names each drifted key with the file it came from. A failed
// origin listing leaves every key without one; the caller refuses regardless.
func reportDrift(gitdir, dir, file string, drift []string, bad map[string]bool, stderr io.Writer) {
	out, _ := git(dir, stderr, "--git-dir="+gitdir, "config", "--list", "--show-origin", "--show-scope", "-z")
	listing := splitNUL(out)
	for _, key := range drift {
		kd, withheld := showKey(key)
		found := map[string]bool{}
		for i := 0; i+2 < len(listing); i += 3 {
			if scope := listing[i]; scope != "local" && scope != "worktree" {
				continue
			}
			rec := listing[i+2]
			if !strings.Contains(rec, "\n") {
				rec += "\n"
			}
			if !strings.HasPrefix(rec, key+"\n") || !bad[rec] {
				continue
			}
			origin := strings.TrimPrefix(listing[i+1], "file:")
			if found[origin] {
				continue
			}
			found[origin] = true
			say(stderr, "refusing git: %s (from %s) is not in the git-config baseline %s\n", kd, Quote(origin), file)
			// Unsetting a replaced remote.origin.url would delete origin.
			if kd != Quote(key) || IsRedirect(key, strings.TrimPrefix(rec, key+"\n")) {
				say(stderr, "  restore the baselined value or remove it: edit %s\n", Quote(origin))
			} else {
				say(stderr, "  remove it: git config --file %s --unset-all %s\n", Quote(origin), Quote(key))
			}
		}
		if len(found) == 0 {
			say(stderr, "refusing git: %s is not in the git-config baseline %s\n", kd, file)
		}
		if withheld {
			say(stderr, "  %s%s\n", kd, ownTerminal(key))
		}
	}
	say(stderr, "  to clear it: list drift with `crew git-baseline`, remove any key you did not set (git config --unset-all), then accept the rest from your own terminal: `crew git-baseline --accept`\n")
}
