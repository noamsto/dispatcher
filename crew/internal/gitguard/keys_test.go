package gitguard

import "testing"

func TestGlob(t *testing.T) {
	for _, tc := range []struct {
		pattern, s string
		want       bool
	}{
		{"", "", true},
		{"", "a", false},
		{"*", "", true},
		{"*", "a.b/c", true},
		{"a.*.b", "a.x/y.z.b", true},
		{"a.*.b", "a..b", true},
		{"a.*.b", "a.b", false},
		{"core.pager", "core.pagerx", false},
		{"core.pager", "core.page", false},
		{"sendemail.*cmd", "sendemail.tocmd", true},
		{"sendemail.*cmd", "sendemail.cmdx", false},
		{"!*", "!sh", true},
		{"!*", "status", false},
		{"*a*a", "aaa", true},
		{"*a*a", "ab", false},
	} {
		if got := glob(tc.pattern, tc.s); got != tc.want {
			t.Errorf("glob(%q, %q) = %v, want %v", tc.pattern, tc.s, got, tc.want)
		}
	}
}

func TestMatchValueGlob(t *testing.T) {
	for _, tc := range []struct {
		k, v string
		want bool
	}{
		{"alias.st", "!sh", true},
		{"alias.st", "status", false},
		{"alias.st", " !sh", false},
		{"submodule.a.update", "!x", true},
		{"submodule.a.update", "checkout", false},
	} {
		if got := IsExec(tc.k, tc.v); got != tc.want {
			t.Errorf("IsExec(%q, %q) = %v, want %v", tc.k, tc.v, got, tc.want)
		}
	}
}

func TestTables(t *testing.T) {
	for _, tc := range []struct {
		k, v           string
		exec, redirect bool
	}{
		{"core.fsmonitor", "", true, false},
		{"core.fsmonitorx", "", false, false},
		{"includeif.gitdir:**/x.path", "y", true, false},
		{"url.https://x/.insteadof", "z", false, true},
		{"http.https://h/.sslverify", "false", false, true},
		{"remote.origin.url", "x", false, true},
		{"remote.o.uploadpack", "x", true, false},
		{"lfs.customtransfer.a/b.path", "p", true, false},
		{"alias.st", "status", false, false},
		{"alias.st.command", "!sh", true, false},
		{"alias.st.command", "status", false, false},
		{"trailer.sign.cmd", "x", true, false},
		{"trailer.sign.command", "x", true, false},
		{"trailer.sign.key", "Signed-off-by", false, false},
		{"mergetool.vimdiff.path", "x", true, false},
		{"mergetool.vimdiff.trustexitcode", "true", false, false},
		{"difftool.vimdiff.path", "x", true, false},
		{"difftool.vimdiff.prompt", "false", false, false},
		{"sendemail.work.smtpserver", "/tmp/x", true, false},
		{"sendemail.work.smtpserverport", "25", false, false},
		{"sendemail.work.tocmd", "x", true, false},
		{"sendemail.work.sendmailcmd", "x", true, false},
		{"sendemail.work.smtpuser", "u", false, false},
		{"browser.firefox.path", "x", true, false},
		{"man.konqueror.path", "x", true, false},
		{"guitool.lint.cmd", "x", true, false},
		{"guitool.lint.needsfile", "true", false, false},
		{"imap.tunnel", "x", true, false},
		{"imap.host", "x", false, false},
		{"instaweb.httpd", "x", true, false},
		{"instaweb.browser", "x", true, false},
		{"instaweb.modulepath", "x", true, false},
		{"instaweb.port", "1234", false, false},
		{"help.browser", "x", true, false},
		{"help.format", "web", false, false},
	} {
		if got := IsExec(tc.k, tc.v); got != tc.exec {
			t.Errorf("IsExec(%q, %q) = %v, want %v", tc.k, tc.v, got, tc.exec)
		}
		if got := IsRedirect(tc.k, tc.v); got != tc.redirect {
			t.Errorf("IsRedirect(%q, %q) = %v, want %v", tc.k, tc.v, got, tc.redirect)
		}
		if got := Guarded(tc.k, tc.v); got != (tc.exec || tc.redirect) {
			t.Errorf("Guarded(%q, %q) = %v", tc.k, tc.v, got)
		}
	}
}

func TestCanon(t *testing.T) {
	for _, tc := range []struct{ r, rec, want string }{
		{"/repo/.git", "core.hookspath\n.git/hooks", "core.hookspath\n/repo/.git/hooks"},
		{"", "core.hookspath\n.git/hooks", "core.hookspath\n.git/hooks"},
		{"/repo/.git", "core.hookspath\n./.git/hooks", "core.hookspath\n./.git/hooks"},
		{"/repo/.git", "core.hookspath\n.husky", "core.hookspath\n.husky"},
		{"/repo/.git", "core.fsmonitor\n.git/hooks", "core.fsmonitor\n.git/hooks"},
	} {
		if got := Canon(tc.r, tc.rec); got != tc.want {
			t.Errorf("Canon(%q, %q) = %q, want %q", tc.r, tc.rec, got, tc.want)
		}
	}
}
