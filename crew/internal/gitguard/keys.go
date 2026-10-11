// Package gitguard is the git-config guard: a worker can write the repo's
// common config, so the dispatcher refuses to run git when an exec-capable or
// endpoint-redirecting (key, value) pair has drifted from a trusted baseline.
package gitguard

import "strings"

// ExecKeys are the config keys whose value git may run as a program, or that
// pull in more config. Each entry is `<key-glob>` or `<key-glob> <value-glob>`,
// matched against git's canonical key (section and final key lowercase,
// subsection verbatim).
var ExecKeys = []string{
	"include.path", "includeif.*.path",
	"core.fsmonitor", "core.hookspath", "core.sshcommand", "core.gitproxy",
	"core.pager", "core.editor", "core.askpass", "core.alternaterefscommand",
	"sequence.editor", "pager.*", "hook.*.command", "hook.*.event", "gc.recentobjectshook",
	"filter.*.clean", "filter.*.smudge", "filter.*.process",
	"diff.external", "diff.*.textconv", "diff.*.command", "merge.*.driver",
	"difftool.*.cmd", "mergetool.*.cmd",
	"credential.helper", "credential.*.helper",
	"gpg.program", "gpg.*.program", "gpg.ssh.defaultkeycommand",
	"alias.* !*", "submodule.*.update !*",
	"remote.*.uploadpack", "remote.*.receivepack", "remote.*.vcs",
	"protocol.allow", "protocol.*.allow", "uploadpack.packobjectshook",
	"interactive.difffilter", "web.browser", "browser.*.cmd", "man.*.cmd",
	"tar.*.command", "sendemail.smtpserver", "sendemail.*cmd",
	"lfs.customtransfer.*.path", "lfs.extension.*.clean", "lfs.extension.*.smudge",
}

// RedirectKeys are the config keys that choose which endpoint git talks to, or
// whether it trusts it. Same format as ExecKeys.
var RedirectKeys = []string{
	"url.*.insteadof", "url.*.pushinsteadof",
	"remote.*.url", "remote.*.pushurl", "remote.*.proxy",
	"http.proxy", "http.*.proxy",
	"http.sslverify", "http.*.sslverify",
	"http.sslcainfo", "http.*.sslcainfo", "http.sslcapath", "http.*.sslcapath",
	"http.curloptresolve", "http.*.curloptresolve",
	"fetch.bundleuri",
}

// urlSubsectionKeys are the keys whose subsection is itself a URL; only these
// are masked in a key.
var urlSubsectionKeys = []string{"url.*.insteadof", "url.*.pushinsteadof", "http.*.*", "credential.*.*"}

// IsExec reports whether ExecKeys names the pair.
func IsExec(k, v string) bool { return match(ExecKeys, k, v) }

// IsRedirect reports whether RedirectKeys names the pair.
func IsRedirect(k, v string) bool { return match(RedirectKeys, k, v) }

// Guarded reports whether a baseline guards the pair.
func Guarded(k, v string) bool { return IsExec(k, v) || IsRedirect(k, v) }

// Canon maps the exact relative `.git/hooks` core.hookspath record (`key\nvalue`)
// to `<r>/hooks`, r the realpath of the caller's anchored common dir: git-hooks.nix
// spells the shared hooks path that way from the main checkout and `<common>/hooks`
// from a linked worktree. Any other spelling may name a worker-editable dir, so it
// stays as is.
func Canon(r, rec string) string {
	if rec != "core.hookspath\n.git/hooks" || r == "" {
		return rec
	}
	return "core.hookspath\n" + r + "/hooks"
}

func match(table []string, k, v string) bool {
	for _, entry := range table {
		keyGlob, valueGlob, hasValue := strings.Cut(entry, " ")
		if glob(keyGlob, k) && (!hasValue || glob(valueGlob, v)) {
			return true
		}
	}
	return false
}

// glob matches s against a pattern whose only wildcard is `*`, which spans any
// bytes including `.` and `/` — bash's `[[ s == pattern ]]`, unlike path.Match.
func glob(pattern, s string) bool {
	px, sx := 0, 0
	starPx, starSx := 0, 0
	for px < len(pattern) || sx < len(s) {
		if px < len(pattern) {
			if pattern[px] == '*' {
				starPx, starSx = px, sx+1
				px++
				continue
			}
			if sx < len(s) && pattern[px] == s[sx] {
				px++
				sx++
				continue
			}
		}
		if 0 < starSx && starSx <= len(s) {
			px, sx = starPx, starSx
			continue
		}
		return false
	}
	return true
}
