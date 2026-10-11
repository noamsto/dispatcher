package gitguard

import (
	"bytes"
	"testing"
)

func runGuard(cwd string, args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := RunGuard(args, cwd, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestRunGuardUsage(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{"nope"},
		{"check"},
		{"check", "a", "b", "c"},
		{"check-cwd"},
		{"init", "a", "b"},
		{"commondir"},
		{"pairs"},
		{"union", "a", "b"},
		{"show-url"},
		{"show-url", "a", "proxy", "x"},
		{"show-key", "a", "b"},
		{"show-pair", "k"},
		{"canon", "r"},
	} {
		code, out, errOut := runGuard("", args...)
		if code != 64 || out != "" || errOut != guardUsage {
			t.Errorf("%q: exit %d, stdout %q, stderr %q; want the usage exit", args, code, out, errOut)
		}
	}
}

func TestRunGuardValueOps(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"show-key", "core.pager"}, "core.pager"},
		{[]string{"show-key", "url.https://u:p@h.example/.insteadof"}, ShowKey("url.https://u:p@h.example/.insteadof")},
		{[]string{"show-url", "u:p@h.example:8080", "proxy"}, ShowURL("u:p@h.example:8080", true)},
		{[]string{"show-url", "u:p@h.example:8080"}, ShowURL("u:p@h.example:8080", false)},
		{[]string{"show-pair", "remote.origin.url", "https://t@h.example/r", "https://t@h"}, "remote.origin.url=https://t@h.example/r"},
		{[]string{"show-pair", "remote.origin.url", "https://t@h.example/r"}, ShowPair("remote.origin.url", "https://t@h.example/r", nil)},
		{[]string{"canon", "/r", "core.hookspath\n.git/hooks"}, "core.hookspath\n/r/hooks"},
		{[]string{"canon", "", "core.hookspath\n.git/hooks"}, "core.hookspath\n.git/hooks"},
		{[]string{"canon", "/r", "core.pager\nx\n"}, "core.pager\nx\n"},
	} {
		code, out, errOut := runGuard("", tc.args...)
		if code != 0 || out != tc.want || errOut != "" {
			t.Errorf("%q: exit %d, stdout %q, stderr %q; want %q", tc.args, code, out, errOut, tc.want)
		}
	}
}

func TestRunGuardRecordOps(t *testing.T) {
	r := newRepo(t)
	r.config(t, "core.pager", "less")
	r.config(t, "core.fsmonitor", "x")
	want := "core.fsmonitor\nx\x00core.pager\nless\x00"
	for _, op := range []string{"pairs", "union"} {
		if code, out, errOut := runGuard("", op, r.common); code != 0 || out != want || errOut != "" {
			t.Errorf("%s: exit %d, stdout %q, stderr %q; want %q", op, code, out, errOut, want)
		}
	}
	shimGit(t, "--show-scope")
	if code, out, _ := runGuard("", "pairs", r.common); code != 1 || out != "" {
		t.Errorf("pairs with a failing git: exit %d, stdout %q; want exit 1, nothing", code, out)
	}
}

func TestRunGuardChecks(t *testing.T) {
	r := newRepo(t)
	code, out, errOut := runGuard(r.main, "check", r.common)
	if want := "refusing git: no git-config baseline at " + r.baseline() + " — the next dispatch records it, or run `crew git-baseline --accept` from your own terminal\n"; code != 1 || out != "" || errOut != want {
		t.Fatalf("check without a baseline: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	if code, out, errOut := runGuard(r.main, "init", r.common); code != 0 || out != "" {
		t.Fatalf("init: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	_, admin := r.addWorktree(t, "w")
	for _, args := range [][]string{
		{"check", r.common},
		{"check", r.common, ""},
		{"check", r.common, admin},
		{"check-cwd", r.common},
		{"commondir", r.common},
	} {
		if code, out, errOut := runGuard(r.main, args...); code != 0 || out != "" || errOut != "" {
			t.Errorf("%q: exit %d, stdout %q, stderr %q; want a silent pass", args, code, out, errOut)
		}
	}
	writeFile(t, r.common+"/commondir", "/elsewhere\n")
	for _, args := range [][]string{{"check", r.common}, {"check-cwd", r.common}, {"init", r.common}, {"commondir", r.common}} {
		if code, out, errOut := runGuard(r.main, args...); code != 1 || out != "" || errOut != redirectLine(r.common) {
			t.Errorf("%q: exit %d, stdout %q, stderr %q; want the commondir refusal", args, code, out, errOut)
		}
	}
}
