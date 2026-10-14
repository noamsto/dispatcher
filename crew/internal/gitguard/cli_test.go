package gitguard

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func runGuard(cwd string, req GuardRequest) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := RunGuard(req, cwd, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestRunGuardValueOps(t *testing.T) {
	for _, tc := range []struct {
		req  GuardRequest
		want string
	}{
		{GuardRequest{Op: "show-key", Key: "core.pager"}, "core.pager"},
		{GuardRequest{Op: "show-key", Key: "url.https://u:p@h.example/.insteadof"}, ShowKey("url.https://u:p@h.example/.insteadof")},
		{GuardRequest{Op: "show-url", URL: "u:p@h.example:8080", Mode: "proxy"}, ShowURL("u:p@h.example:8080", true)},
		{GuardRequest{Op: "show-url", URL: "u:p@h.example:8080"}, ShowURL("u:p@h.example:8080", false)},
		{GuardRequest{Op: "show-url", URL: "u:p@h.example:8080", Mode: "other"}, ShowURL("u:p@h.example:8080", false)},
		{GuardRequest{Op: "show-pair", Key: "remote.origin.url", Value: "https://t@h.example/r", Aliases: []string{"https://t@h"}}, "remote.origin.url=https://t@h.example/r"},
		{GuardRequest{Op: "show-pair", Key: "remote.origin.url", Value: "https://t@h.example/r"}, ShowPair("remote.origin.url", "https://t@h.example/r", nil)},
		{GuardRequest{Op: "canon", Root: "/r", Rec: "core.hookspath\n.git/hooks"}, "core.hookspath\n/r/hooks"},
		{GuardRequest{Op: "canon", Rec: "core.hookspath\n.git/hooks"}, "core.hookspath\n.git/hooks"},
		{GuardRequest{Op: "canon", Root: "/r", Rec: "core.pager\nx\n"}, "core.pager\nx\n"},
	} {
		code, out, errOut := runGuard("", tc.req)
		if code != 0 || out != tc.want || errOut != "" {
			t.Errorf("%+v: exit %d, stdout %q, stderr %q; want %q", tc.req, code, out, errOut, tc.want)
		}
	}
}

func TestRunGuardRecordOps(t *testing.T) {
	r := newRepo(t)
	r.config(t, "core.pager", "less")
	r.config(t, "core.fsmonitor", "x")
	want := "core.fsmonitor\nx\x00core.pager\nless\x00"
	for _, op := range []string{"pairs", "union"} {
		if code, out, errOut := runGuard("", GuardRequest{Op: op, Dir: r.common}); code != 0 || out != want || errOut != "" {
			t.Errorf("%s: exit %d, stdout %q, stderr %q; want %q", op, code, out, errOut, want)
		}
	}
	shimGit(t, "--show-scope")
	if code, out, _ := runGuard("", GuardRequest{Op: "pairs", Dir: r.common}); code != 1 || out != "" {
		t.Errorf("pairs with a failing git: exit %d, stdout %q; want exit 1, nothing", code, out)
	}
}

func TestRunGuardChecks(t *testing.T) {
	r := newRepo(t)
	code, out, errOut := runGuard(r.main, GuardRequest{Op: "check", Dir: r.common})
	if want := "refusing git: no git-config baseline at " + r.baseline() + " — the next dispatch records it, or run `crew git-baseline --accept` from your own terminal\n"; code != 1 || out != "" || errOut != want {
		t.Fatalf("check without a baseline: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	if code, out, errOut := runGuard(r.main, GuardRequest{Op: "init", Dir: r.common}); code != 0 || out != "" {
		t.Fatalf("init: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	_, admin := r.addWorktree(t, "w")
	for _, req := range []GuardRequest{
		{Op: "check", Dir: r.common},
		{Op: "check", Dir: r.common, Gitdir: admin},
		{Op: "check-cwd", Dir: r.common},
		{Op: "commondir", Dir: r.common},
	} {
		if code, out, errOut := runGuard(r.main, req); code != 0 || out != "" || errOut != "" {
			t.Errorf("%+v: exit %d, stdout %q, stderr %q; want a silent pass", req, code, out, errOut)
		}
	}
	writeFile(t, r.common+"/commondir", "/elsewhere\n")
	for _, op := range []string{"check", "check-cwd", "init", "commondir"} {
		if code, out, errOut := runGuard(r.main, GuardRequest{Op: op, Dir: r.common}); code != 1 || out != "" || errOut != redirectLine(r.common) {
			t.Errorf("%s: exit %d, stdout %q, stderr %q; want the commondir refusal", op, code, out, errOut)
		}
	}
}

func TestRunGuardReportsErrors(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file modes")
	}
	r := newRepo(t)
	r.init(t)
	mustDo(t, os.Chmod(r.baseline(), 0))
	if code, _, errOut := runGuard(r.main, GuardRequest{Op: "check", Dir: r.common}); code != 1 || !strings.HasPrefix(errOut, "refusing git: ") || !strings.Contains(errOut, r.baseline()) {
		t.Errorf("check with an unreadable baseline: exit %d, stderr %q; want a refusal naming it", code, errOut)
	}

	r = newRepo(t)
	crew := r.common + "/crew"
	mustDo(t, os.Mkdir(crew, 0o555))
	t.Cleanup(func() { _ = os.Chmod(crew, 0o755) })
	if code, _, errOut := runGuard(r.main, GuardRequest{Op: "init", Dir: r.common}); code != 1 || !strings.HasPrefix(errOut, "refusing git: ") || !strings.Contains(errOut, crew) {
		t.Errorf("init with a read-only crew dir: exit %d, stderr %q; want a refusal naming it", code, errOut)
	}
}
