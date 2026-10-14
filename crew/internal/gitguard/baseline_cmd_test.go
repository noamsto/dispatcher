package gitguard

import (
	"bytes"
	"os"
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
}

func TestRunBaselineNotInRepo(t *testing.T) {
	root := gitEnv(t)
	t.Setenv("GIT_CEILING_DIRECTORIES", root)
	runBaseline(root, true, "", false).want(t, 1, "", "crew: not in a git repo\n")
}

func TestRunBaselineMissing(t *testing.T) {
	r := newRepo(t)
	runBaseline(r.main, false, "", false).want(t, 1, "",
		"git-config baseline "+r.baseline()+": none yet — the next dispatch records it, or run `crew git-baseline --accept` from your own terminal\n")
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
		driftLine("core.fsmonitor=x", "main checkout", r.common+"/config")+
			driftLine("core.pager=less", "worktree w", admin+"/config.worktree"), "")
}

func TestRunBaselinePredatesNoteBeforePairs(t *testing.T) {
	r := newRepo(t)
	writeFile(t, r.baseline(), "core.pager\nless\x00")
	r.config(t, "core.pager", "less")
	r.config(t, "core.fsmonitor", "x")
	runBaseline(r.main, false, "", false).want(t, 1,
		"git-config baseline "+r.baseline()+" predates redirect-key coverage — the next dispatch records the redirect keys present then, or --accept does\n"+
			driftLine("core.fsmonitor=x", "main checkout", r.common+"/config"), "")
}

func TestRunBaselineAliasRewrite(t *testing.T) {
	r := newRepo(t)
	r.config(t, "remote.origin.url", "https://t@h.example/r")
	r.init(t)
	r.config(t, "url.https://evil.example/.insteadof", "https://t@h")
	origin := r.common + "/config"
	runBaseline(r.main, false, "", false).want(t, 1,
		driftLine(ShowPair("url.https://evil.example/.insteadof", "https://t@h", nil), "main checkout", origin)+
			"  rewritten by the alias above: "+driftLine("remote.origin.url=https://t@h.example/r", "main checkout", origin), "")
}

func TestRunBaselineLegacy(t *testing.T) {
	r := newRepo(t)
	r.init(t)
	writeFile(t, r.common+"/remotes/foo", "URL: https://h.example/r\n")
	if err := os.MkdirAll(r.common+"/branches/d", 0o755); err != nil {
		t.Fatal(err)
	}
	runBaseline(r.main, false, "", false).want(t, 1,
		"legacy file entry "+Quote(r.common+"/remotes/foo")+": new or changed from the baseline"+legacyTail+
			"legacy directory entry "+Quote(r.common+"/branches/d")+": unsafe"+legacyTail, "")
}

func TestRunBaselineAcceptNo(t *testing.T) {
	r := newRepo(t)
	r.init(t)
	before := readFile(t, r.baseline())
	r.config(t, "core.fsmonitor", "x")
	runBaseline(r.main, true, "no\n", true).want(t, 1,
		driftLine("core.fsmonitor=x", "main checkout", r.common+"/config")+prompt(r.baseline()),
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
	note := "git-config baseline " + r.baseline() + " predates redirect-key coverage — the next dispatch records the redirect keys present then, or --accept does\n"
	origin := r.common + "/config"
	// read -r: surrounding blanks trimmed, a final line without a newline still read.
	runBaseline(r.main, true, " yes\t", true).want(t, 0,
		note+driftLine("core.fsmonitor=x", "main checkout", origin)+driftLine("core.editor=vi", "main checkout", origin)+
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
}

func TestRunBaselineAcceptRefusesLegacy(t *testing.T) {
	r := newRepo(t)
	r.init(t)
	before := readFile(t, r.baseline())
	r.config(t, "core.fsmonitor", "x")
	writeFile(t, r.common+"/remotes/foo", "URL: https://h.example/r\n")
	runBaseline(r.main, true, "yes\n", true).want(t, 1,
		driftLine("core.fsmonitor=x", "main checkout", r.common+"/config")+
			"legacy file entry "+Quote(r.common+"/remotes/foo")+": new or changed from the baseline"+legacyTail,
		"crew: git-baseline --accept refuses while legacy remotes/branches entries are listed — remove them, then re-run\n")
	if got := readFile(t, r.baseline()); got != before {
		t.Fatalf("baseline changed: %q, want %q", got, before)
	}
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
	plantRedirect(t, r, true)
	runBaseline(r.main, false, "", false).want(t, 1, "", redirectLine(r.common))
	runBaseline(r.main, true, "yes\n", true).want(t, 1, "", redirectLine(r.common))
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
