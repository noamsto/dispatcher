package main

import (
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/alecthomas/kong"
)

// fishHelpers are the dynamic sources. Each is capped at a second and silent
// on any failure, so a slow bus or a missing jq costs the user a completion and
// never a stray error in the prompt. __crew_at and __crew_is match the leading
// non-flag words after `crew` positionally, so a nested command never fires its
// namesake's lines (`fleet stream` vs `stream`).
const fishHelpers = `function __crew_targets
    timeout 1 crew roster --live 2>/dev/null | jq -r '.[]? | .name, .branch' 2>/dev/null
end

function __crew_ids
    timeout 1 crew crews 2>/dev/null | tail -n +2 | cut -f1
end

function __crew_at
    set -l want "$argv"
    test -z "$want"; and return 0
    string match -q -- "$want *" (string join ' ' -- (__crew_words))" "
end

function __crew_is
    test "$argv" = "$(string join ' ' -- (__crew_words))"
end
`

// positionalTargets are the subcommands whose first word names a worker.
var positionalTargets = map[string]bool{"where": true, "nudge": true, "reply": true, "identity": true}

// pathPositionals are the subcommands whose first word is a filesystem path.
var pathPositionals = map[string]bool{"spec-lint": true, "occupants": true}

// pathFlags take a filesystem path as their value.
var pathFlags = map[string]bool{"spec-file": true, "pi-agent-dir": true}

// skippedSubs are callable top-level commands the help leaves undocumented.
var skippedSubs = map[string]bool{"git-guard": true, "reap-autosweep": true}

// writeFishCompletions renders the grammar as fish `complete` lines. Hidden
// commands and flags stay out of it, as they stay out of the help.
func writeFishCompletions(model *kong.Application, w io.Writer) {
	say(w, "complete -c crew -f\n\n%s\n%s\n", fishHelpers, fishWords(model))
	fishNode(w, model.Node, nil)
}

// fishWords is __crew_words: the leading non-flag words of the command line.
// A value-taking flag's value is the word after it, so the flags that take one
// are listed here and their value skipped (`crew fleet --crew c1 stream` is
// `fleet stream`).
func fishWords(model *kong.Application) string {
	names := map[string]bool{}
	valueFlagNames(model.Node, names)
	list := make([]string, 0, len(names))
	for name := range names {
		list = append(list, name)
	}
	slices.Sort(list)
	return fmt.Sprintf(`function __crew_words
    set -l w (commandline -xpc)
    set -e w[1]
    set -l valued %s
    set -l skip 0
    for t in $w
        if test $skip = 1
            set skip 0
            continue
        end
        if string match -q -- '--*' $t; and contains -- (string replace -r '^--' '' -- $t) $valued
            set skip 1
            continue
        end
        if not string match -q -- '-*' $t
            echo $t
        end
    end
end
`, strings.Join(list, " "))
}

func valueFlagNames(n *kong.Node, names map[string]bool) {
	for _, f := range n.Flags {
		if !f.IsBool() {
			names[f.Name] = true
		}
	}
	for _, c := range n.Children {
		valueFlagNames(c, names)
	}
	if n.DefaultCmd != nil {
		valueFlagNames(n.DefaultCmd, names)
	}
}

func fishNode(w io.Writer, n *kong.Node, path []string) {
	cond := fishCondition(path)
	fishFlags(w, cond, n)
	// A hidden default child carries its parent's bare-invocation flags.
	if d := n.DefaultCmd; d != nil && d.Hidden {
		fishFlags(w, cond, d)
	}
	if len(path) == 1 && positionalTargets[path[0]] {
		say(w, "complete -c crew -n %s -f -a '(__crew_targets)'\n", fishQuote(cond))
	}
	if len(path) == 1 && pathPositionals[path[0]] {
		say(w, "complete -c crew -n %s -F\n", fishQuote(cond))
	}
	listCond := fishQuote("__crew_is " + strings.Join(path, " "))
	for _, c := range n.Children {
		if c.Hidden || len(path) == 0 && skippedSubs[c.Name] {
			continue
		}
		line := fmt.Sprintf("complete -c crew -n %s -a %s", listCond, c.Name)
		if help := firstLine(c.Help); help != "" {
			line += " -d " + fishQuote(help)
		}
		say(w, "%s\n", line)
		fishNode(w, c, append(slices.Clip(path), c.Name))
	}
}

// fishCondition is true once every word of path has been typed as a leading
// non-flag word.
func fishCondition(path []string) string {
	return "__crew_at " + strings.Join(path, " ")
}

func fishFlags(w io.Writer, cond string, n *kong.Node) {
	for _, f := range n.Flags {
		if !f.Hidden {
			fishFlag(w, cond, f)
		}
	}
}

func fishFlag(w io.Writer, cond string, f *kong.Flag) {
	line := fmt.Sprintf("complete -c crew -n %s -l %s", fishQuote(cond), f.Name)
	if f.Short != 0 {
		line += " -s " + string(f.Short)
	}
	if !f.IsBool() {
		line += " -r"
		switch {
		case pathFlags[f.Name]:
			line += " -F"
		case f.Name == "crew":
			line += " -f -a '(__crew_ids)'"
		case f.Enum != "":
			line += " -f -a " + fishQuote(strings.Join(f.EnumSlice(), " "))
		default:
			line += " -f"
		}
	}
	if help := firstLine(f.Help); help != "" {
		line += " -d " + fishQuote(help)
	}
	say(w, "%s\n", line)
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return strings.TrimSpace(line)
}

func fishQuote(s string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s) + "'"
}

// runCompletion prints the completion script for the one shell crew supports.
func runCompletion(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 || args[0] != "fish" {
		say(stderr, "usage: crew completion fish\n")
		return exitUsage
	}
	var (
		r       root
		capture passthroughCmd
	)
	p, err := newCrewParser(&r, &capture)
	if err != nil {
		panic("crew: kong grammar: " + err.Error())
	}
	writeFishCompletions(p.Model, stdout)
	return 0
}
