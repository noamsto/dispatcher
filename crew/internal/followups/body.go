package followups

import (
	"fmt"
	"regexp"
	"strings"
)

// Link is one filed follow-up as the PR body lists it.
type Link struct {
	N     int
	Title string
	Note  string
}

const (
	untrackedHeading = "## Follow-ups (untracked)"
	trackedHeading   = "## Follow-ups"
	ledgerOpen       = "<details><summary>Agent ledger</summary>"
)

// Rewrite swaps the PR body's follow-up section for links to the filed
// issues, leaving every byte outside that section (the ledger block above
// all) untouched. Lines split on "\n" only, so a CRLF body keeps its "\r".
func Rewrite(body string, links []Link) string {
	if len(links) == 0 {
		return body
	}
	lines := strings.Split(body, "\n")
	cr := ""
	if strings.Contains(body, "\r\n") {
		cr = "\r"
	}
	u := findHeading(lines, untrackedHeading)
	f := findHeading(lines, trackedHeading)

	switch {
	case f >= 0:
		end := sectionEnd(lines, f)
		// Insert after the last non-blank line so the gap before the next
		// section survives.
		at := end
		for at > f+1 && strings.TrimSpace(lines[at-1]) == "" {
			at--
		}
		have := strings.Join(lines[f:end], "\n")
		var add []string
		for _, l := range links {
			if !regexp.MustCompile(fmt.Sprintf(`#%d\b`, l.N)).MatchString(have) {
				add = append(add, renderLink(l, cr))
			}
		}
		lines = splice(lines, at, at, add)
		if u = findHeading(lines, untrackedHeading); u >= 0 {
			lines = splice(lines, u, sectionEnd(lines, u), nil)
		}
	case u >= 0:
		end := sectionEnd(lines, u)
		section := append([]string{trackedHeading + cr}, renderLinks(links, cr)...)
		if end == len(lines) {
			// The last element is the file's terminating newline.
			section = append(section, "")
		} else {
			section = append(section, cr)
		}
		lines = splice(lines, u, end, section)
	default:
		section := append([]string{trackedHeading + cr}, renderLinks(links, cr)...)
		at := len(lines)
		if i := findPrefix(lines, ledgerOpen); i >= 0 {
			at = i
			section = append(section, cr)
		} else if at > 0 && lines[at-1] == "" {
			at--
		}
		if at > 0 && strings.TrimSpace(lines[at-1]) != "" {
			section = append([]string{cr}, section...)
		}
		lines = splice(lines, at, at, section)
	}
	return strings.Join(lines, "\n")
}

func renderLink(l Link, cr string) string {
	s := fmt.Sprintf("- #%d — %s", l.N, l.Title)
	if l.Note != "" {
		s += " (" + l.Note + ")"
	}
	return s + cr
}

func renderLinks(links []Link, cr string) []string {
	out := make([]string, len(links))
	for i, l := range links {
		out[i] = renderLink(l, cr)
	}
	return out
}

func findHeading(lines []string, heading string) int {
	for i, l := range lines {
		if strings.TrimRight(l, " \t\r") == heading {
			return i
		}
	}
	return -1
}

func findPrefix(lines []string, prefix string) int {
	for i, l := range lines {
		if strings.HasPrefix(l, prefix) {
			return i
		}
	}
	return -1
}

// sectionEnd is the index of the first line after the section that opens at
// heading: the next "## " heading or <details> block, else the end.
func sectionEnd(lines []string, heading int) int {
	for i := heading + 1; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], "## ") || strings.HasPrefix(lines[i], "<details>") {
			return i
		}
	}
	return len(lines)
}

// splice returns lines with [from, to) replaced by repl.
func splice(lines []string, from, to int, repl []string) []string {
	out := make([]string, 0, len(lines)-(to-from)+len(repl))
	out = append(out, lines[:from]...)
	out = append(out, repl...)
	return append(out, lines[to:]...)
}
