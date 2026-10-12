package entry

import (
	"embed"
	"io"
	"io/fs"
	"regexp"
	"slices"
	"strings"
)

// helpTexts is every screen crew.sh's `_crew_help` cat'ed out of a heredoc, one
// file per screen: `top` for the grouped list, `hold/<action>` for hold's
// per-action screens (`default` for bare `crew hold`), and the subcommand name
// for the rest. The bodies were extracted out of the script mechanically rather
// than retyped, and scripts/help-parity.sh re-runs the two sides against each
// other from the pre-change crew.sh in git — this change deleted the script's
// copy, so no in-tree drift guard can hold the link.
//
//go:embed texts
var helpTexts embed.FS

// screenNameRe is the shape of a key: one screen name, optionally under one
// directory (hold's). It keeps a caller-supplied name from naming a path.
var screenNameRe = regexp.MustCompile(`^[a-z0-9-]+(/[a-z0-9-]+)?$`)

// segmentRe is one screen name and nothing else. `_crew_help` matched its case
// labels exactly, so `crew help hold/add` — a name with a slash in it, which no
// case label has — got the refusal rather than hold's add screen.
var segmentRe = regexp.MustCompile(`^[a-z0-9-]+$`)

// internalKey reports the two screen names this package invented rather than
// inherited from a case label: top, the grouped list the function printed for an
// empty subcommand, and default, bare `crew hold`’s screen (whose label in the
// script was the empty pattern, which is no name). Both match segmentRe and both
// name a real file, and neither was ever a subcommand — `crew help top` was
// refused, and so it still is.
func internalKey(key string) bool { return key == "top" || key == "default" }

// screen returns the named help screen's text.
func screen(key string) (string, bool) {
	if !screenNameRe.MatchString(key) {
		return "", false
	}
	b, err := helpTexts.ReadFile("texts/" + key + ".txt")
	if err != nil {
		return "", false
	}
	return string(b), true
}

// RunHelp is crew.sh's `_crew_help`, and the routing that called it lives in the
// script: `crew --help [sub [subsub]]`, `crew <sub> --help`, `crew hold <action>
// --help` and `crew fleet stream --help` all arrive here as the arguments the
// function took. A screen goes to stdout with exit 0, so `crew x --help | grep`
// works; a name with no screen goes to stderr with exit 1, as the function
// returned 1 to the arm's `exit $?`.
func RunHelp(args []string, stdout, stderr io.Writer) int {
	sub, subsub := "", ""
	if len(args) > 0 {
		sub = args[0]
	}
	if len(args) > 1 {
		subsub = args[1]
	}

	if sub == "" {
		top, _ := screen("top")
		_, _ = io.WriteString(stdout, top)
		return 0
	}

	if sub == "hold" {
		// hold takes an action, and its bare screen is the one its `''` arm held.
		key := ""
		switch {
		case subsub == "":
			key = "hold/default"
		case segmentRe.MatchString(subsub) && !internalKey(subsub):
			key = "hold/" + subsub
		}
		if key != "" {
			if text, ok := screen(key); ok {
				_, _ = io.WriteString(stdout, text)
				return 0
			}
		}
		say(stderr, "crew: no help for hold %s\n", subsub)
		return exitFailure
	}

	if segmentRe.MatchString(sub) && !internalKey(sub) {
		if text, ok := screen(sub); ok {
			_, _ = io.WriteString(stdout, text)
			return 0
		}
	}
	// The arm's fallback: the refusal, a blank line, and the whole grouped list,
	// all of it on stderr.
	say(stderr, "crew: no help for %s\n\n", sub)
	top, _ := screen("top")
	_, _ = io.WriteString(stderr, top)
	return exitFailure
}

// screens lists the embedded screen names, without the `texts/` prefix or the
// extension, holding the one directory the screens live in (hold's) as one
// segment: `hold/add`.
func screens() []string {
	var names []string
	for _, dir := range []string{"texts", "texts/hold"} {
		entries, err := fs.ReadDir(helpTexts, dir)
		if err != nil {
			panic("entry: unreadable embedded help texts: " + err.Error())
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			name := strings.TrimSuffix(e.Name(), ".txt")
			if dir != "texts" {
				name = "hold/" + name
			}
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}
