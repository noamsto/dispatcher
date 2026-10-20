package speclint

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/git"
	"github.com/noamsto/dispatcher/crew/internal/git/gittest"
)

// world is the repo the fake git answers for: origin/main with these files and
// these `log` results, keyed by whether the grep is the closing-keyword one.
type world struct {
	noRef    bool
	tree     []string
	files    map[string]string
	mentions string
	closes   string
	logErr   bool // the mention search fails
	closeErr bool // the closing-keyword search fails
	lsErr    bool
	showErr  bool
}

func (w world) git() *gittest.Fake {
	return &gittest.Fake{Func: func(_ string, args []string) (string, error) {
		fail := &git.ExitError{Code: 128}
		switch args[0] {
		case "rev-parse":
			if args[1] == "--show-toplevel" {
				return "/repo", nil
			}
			if w.noRef {
				return "", fail
			}
			return "abc", nil
		case "symbolic-ref":
			return "", fail
		case "ls-tree":
			if w.lsErr {
				return "", fail
			}
			return strings.Join(w.tree, "\n"), nil
		case "show":
			if w.showErr {
				return "", fail
			}
			return w.files[strings.TrimPrefix(args[1], "origin/main:")], nil
		case "log":
			if strings.Contains(args[len(args)-1], "close") {
				if w.closeErr {
					return "", fail
				}
				return w.closes, nil
			}
			if w.logErr {
				return "", fail
			}
			return w.mentions, nil
		}
		return "", fail
	}}
}

const head = "Summary line.\n\n## Acceptance\n\n"

func lookIn(names ...string) func(string, string) bool {
	return func(_, name string) bool { return slices.Contains(names, name) }
}

func TestLint(t *testing.T) {
	workflow := map[string]string{".github/workflows/ci.yml": "steps:\n  - run: nix flake check\n  - run: bash scripts/help-parity.sh\n"}
	wfTree := []string{".github/workflows/ci.yml"}
	const noCIWords = "CI without naming its exact command in backticks"
	for _, tc := range []struct {
		name string
		spec string
		req  Request
		w    world
		look []string
		goos string
		want []string
	}{
		{name: "clean spec", spec: head + "- AC1: it works\n"},
		{
			name: "no summary", spec: "# Title\n\n## Acceptance\n- one\n",
			want: []string{`error structure: no summary paragraph before the first "## " heading`},
		},
		{name: "summary after title", spec: "# Title\n\nWhy.\n\n## Acceptance\n- one\n"},
		{name: "no acceptance", spec: "Summary.\n\n## Task\n- one\n", want: []string{`error structure: no "## Acceptance" section`}},
		{name: "acceptance criteria heading", spec: "Summary.\n\n## Acceptance criteria\n1. one\n"},
		{
			name: "acceptance empty", spec: "Summary.\n\n## Acceptance\n\nprose only\n\n## Notes\n- not an acceptance item\n",
			want: []string{"error structure: Acceptance section has no list items"},
		},
		{
			name: "premise commit with closes",
			spec: head + "- one\n", req: Request{Issues: []string{"1188"}},
			w:    world{mentions: "abc1234 feat: spec lint (#1188)\ndef5678 docs: note #1188", closes: "abc1234"},
			want: []string{"warn premise: #1188 is already referenced on origin/main: abc1234 feat: spec lint (#1188) (closes); def5678 docs: note #1188"},
		},
		{
			name: "premise truncates", spec: head + "- one\n", req: Request{Issues: []string{"7"}},
			w:    world{mentions: "a1 s\nb2 s\nc3 s\nd4 s\ne5 s\nf6 s\ng7 s"},
			want: []string{"warn premise: #7 is already referenced on origin/main: a1 s; b2 s; c3 s; d4 s; e5 s; … and 2 more"},
		},
		{name: "premise no match", spec: head + "- one\n", req: Request{Issues: []string{"1188"}}},
		{name: "premise skipped without issues", spec: head + "- one\n", w: world{mentions: "abc1234 x #1188"}},
		{
			name: "no origin ref", spec: head + "- see `crew/foo.go`\n", w: world{noRef: true},
			want: []string{"warn premise: no origin default branch; premise, path and CI checks skipped"},
		},
		{
			name: "path absent", spec: head + "- edit `crew/foo.go`\n", w: world{tree: []string{"crew/bar.go"}},
			want: []string{`warn path: crew/foo.go is not on origin/main (say "new" on its line if the spec creates it)`},
		},
		{name: "path package-relative", spec: head + "- edit `status/status.go`\n", w: world{tree: []string{"crew/internal/status/status.go"}}},
		{
			name: "path suffix needs a segment boundary", spec: head + "- edit `tus/status.go`\n", w: world{tree: []string{"crew/internal/status/status.go"}},
			want: []string{`warn path: tus/status.go is not on origin/main (say "new" on its line if the spec creates it)`},
		},
		{name: "path model id ignored", spec: head + "- use `openrouter/z-ai/glm-5.3-flash`\n"},
		{name: "path present", spec: head + "- edit `crew/foo.go`\n", w: world{tree: []string{"crew/foo.go"}}},
		{name: "path absent but new", spec: head + "- add `crew/foo.go`\n", w: world{tree: []string{"crew/bar.go"}}},
		{name: "path dotless ignored", spec: head + "- on `origin/main` and `feat/x` in `crew/internal/git`\n"},
		{name: "path directory ignored", spec: head + "- under `docs/v1.2/`\n"},
		{name: "path non-candidates ignored", spec: head + "- `<dir>/a.go` `src/*.go` `/abs/a.go` `~/a/b.go` `$HOME/a.go` `https://x.io/a.go`\n"},
		{
			name: "path line suffix stripped", spec: head + "- see `crew/foo.go:813` and `crew/foo.go:1-9` and `./crew/foo.go`\n",
			w: world{tree: []string{"crew/foo.go"}},
		},
		{
			name: "path suffix absent", spec: head + "- see `crew/foo.go:813`\n",
			want: []string{`warn path: crew/foo.go is not on origin/main (say "new" on its line if the spec creates it)`},
		},
		{name: "path in fence ignored", spec: head + "- one\n\n```\nsee `crew/foo.go`\n```\n"},
		{
			name: "runnable unresolved", spec: head + "- AC2: `mytool t/x.in` passes\n",
			want: []string{`warn runnable: AC2: "mytool" is not on PATH here`},
		},
		{name: "runnable resolved", spec: head + "- AC2: `mytool t/x.in` passes\n", look: []string{"mytool"}},
		{name: "runnable env prefix dropped", spec: head + "- `CI=1 mytool x`\n", look: []string{"mytool"}},
		{
			name: "runnable env prefix unresolved", spec: head + "- `CI=1 mytool x`\n",
			want: []string{`warn runnable: item 1: "mytool" is not on PATH here`},
		},
		{name: "runnable relative script", spec: head + "- `scripts/x.sh --check`\n", look: []string{"scripts/x.sh"}},
		{
			name: "runnable relative script missing", spec: head + "- `scripts/x.sh --check`\n",
			want: []string{`warn runnable: item 1: "scripts/x.sh" is not on PATH here`},
		},
		{
			name: "platform tool behind wrapper", spec: head + "- AC1: `nix develop -c xcodebuild -scheme X`\n", look: []string{"nix"},
			want: []string{`warn runnable: AC1: "xcodebuild" is a macOS tool; this host is linux`},
		},
		{
			name: "platform tool behind assignment wrapper", spec: head + "- AC1: `env FOO=1 xcodebuild build`\n", look: []string{"env"},
			want: []string{`warn runnable: AC1: "xcodebuild" is a macOS tool; this host is linux`},
		},
		{
			name: "platform tool in quoted sh -c", spec: head + "- AC1: `sh -c 'xcodebuild -scheme X'`\n", look: []string{"sh"},
			want: []string{`warn runnable: AC1: "xcodebuild" is a macOS tool; this host is linux`},
		},
		{name: "platform tool as argument ignored", spec: head + "- `rg -l xcodebuild src/`\n", look: []string{"rg"}},
		{name: "linux-capable tools not platform tools", spec: head + "- `swift build` `pod install` `fastlane test`\n", look: []string{"swift", "pod", "fastlane"}},
		{name: "runnable quoted output not a command", spec: head + "- prints `warn premise: x` and `error structure: y`\n"},
		{name: "runnable builtin", spec: head + "- `cd crew && true`\n"},
		{name: "runnable not command shaped", spec: head + "- `## Task` `--issue N` `<level> x` `single`\n"},
		{name: "runnable no host checks", spec: head + "- `mytool x`\n", req: Request{NoHostChecks: true}},
		{name: "runnable outside acceptance ignored", spec: "Summary.\n\n## Task\n- run `mytool x`\n\n## Acceptance\n- one\n"},
		{
			name: "platform tool on linux", spec: head + "- AC1: `xcodebuild -scheme X` builds\n", goos: "linux",
			want: []string{`warn runnable: AC1: "xcodebuild" is a macOS tool; this host is linux`},
		},
		{name: "platform tool on darwin", spec: head + "- AC1: `xcodebuild -scheme X` builds\n", goos: "darwin", look: []string{"xcodebuild"}},
		{name: "ci prose without command", spec: head + "- AC4: CI passes\n", want: []string{"warn ci: AC4 mentions " + noCIWords}},
		{name: "ci status item exempt", spec: head + "- AC4: the CI run is green (run id pasted)\n"},
		{name: "ci not mentioned", spec: head + "- AC4: tests pass\n"},
		{name: "ci gate phrase without command", spec: head + "- AC4: the fast gate passes\n", want: []string{"warn ci: AC4 mentions " + noCIWords}},
		{name: "bare gate ignored", spec: head + "- AC4: the gate works\n"},
		{
			name: "ci command absent from workflows", spec: head + "- AC3: CI runs `scripts/other.sh --x`\n",
			w: world{tree: wfTree, files: workflow}, look: []string{"scripts/other.sh"},
			want: []string{`warn ci: AC3: "scripts/other.sh --x" is not run by .github/workflows on origin/main`},
		},
		{
			name: "ci command present", spec: head + "- AC3: CI runs `nix flake check`\n",
			w: world{tree: wfTree, files: workflow}, look: []string{"nix"},
		},
		{
			name: "ci command split on and", spec: head + "- AC3: CI runs `cd crew && nix flake check && scripts/x.sh --y`\n",
			w: world{tree: wfTree, files: workflow}, look: []string{"scripts/x.sh"},
			want: []string{`warn ci: AC3: "scripts/x.sh --y" is not run by .github/workflows on origin/main`},
		},
		{
			name: "ci quoted shell part", spec: head + "- AC3: CI runs `nix develop -c sh -c 'cd crew && go vet ./...'`\n",
			w: world{tree: wfTree, files: map[string]string{".github/workflows/ci.yml": "run: nix develop -c sh -c 'cd crew && go vet ./...'"}}, look: []string{"nix"},
		},
		{
			name: "ci gh part skipped", spec: head + "- AC3: CI is green: `gh pr checks && nix flake check`\n",
			w: world{tree: wfTree, files: workflow}, look: []string{"gh", "nix"},
		},
		{
			name: "ci workflow unreadable", spec: head + "- AC3: CI runs `nix flake check` and `scripts/x.sh --y`\n",
			w: world{tree: wfTree, files: workflow, showErr: true}, look: []string{"nix", "scripts/x.sh"},
			want: []string{"warn ci: cannot read workflows on origin/main: exit status 128: "},
		},
		{
			name: "premise closing search fails", spec: head + "- one\n", req: Request{Issues: []string{"7"}},
			w:    world{mentions: "a1 s", closeErr: true},
			want: []string{"warn premise: #7 is already referenced on origin/main: a1 s (could not check closing keywords: exit status 128: )"},
		},
		{
			name: "premise search fails", spec: head + "- one\n", req: Request{Issues: []string{"7"}}, w: world{logErr: true},
			want: []string{"warn premise: cannot search origin/main for #7: exit status 128: "},
		},
		{
			name: "tree unlistable warns once and skips ci commands", spec: head + "- see `a/b.go`\n- AC3: CI runs `nix flake check`\n",
			w: world{lsErr: true, files: workflow}, look: []string{"nix"},
			want: []string{"warn path: cannot list origin/main: exit status 128: ; path and CI command checks skipped"},
		},
		{
			name: "ci command found via flake.nix", spec: head + "- AC3: CI runs `statix check`\n",
			w: world{tree: append([]string{"flake.nix"}, wfTree...), files: map[string]string{
				".github/workflows/ci.yml": "run: nix flake check", "flake.nix": "statix  check",
			}}, look: []string{"statix"},
		},
		{
			name: "ci without workflows skips command half", spec: head + "- AC3: CI runs `nix flake check`\n",
			w: world{tree: []string{"a.go"}}, look: []string{"nix"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.goos == "" {
				tc.goos = "linux"
			}
			o := Options{Git: tc.w.git(), LookPath: lookIn(tc.look...), GOOS: tc.goos}
			var got []string
			for _, f := range Lint(context.Background(), []byte(tc.spec), tc.req, "/repo/sub", o) {
				got = append(got, f.String())
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("findings:\n got %q\nwant %q", got, tc.want)
			}
		})
	}
}

func TestClosingPattern(t *testing.T) {
	re := regexp.MustCompile("(?i)" + closingPattern("7"))
	for _, s := range []string{"Closes #7", "fixes: #7", "Resolved #7", "fix #7."} {
		if !re.MatchString(s) {
			t.Errorf("%q not matched", s)
		}
	}
	for _, s := range []string{"Closes #70", "see #7", "#7"} {
		if re.MatchString(s) {
			t.Errorf("%q matched", s)
		}
	}
	g := world{mentions: "a1 s"}.git()
	o := Options{Git: g, LookPath: lookIn(), GOOS: "linux"}
	Lint(context.Background(), []byte(head+"- one\n"), Request{Issues: []string{"7"}}, "/repo", o)
	want := "--grep=" + closingPattern("7")
	if !slices.ContainsFunc(g.Calls(), func(c string) bool { return strings.HasSuffix(c, want) }) {
		t.Errorf("calls %q lack %q", g.Calls(), want)
	}
}

func TestLintGitRunsAtToplevel(t *testing.T) {
	g := world{tree: []string{"a.go"}}.git()
	o := Options{Git: g, LookPath: lookIn(), GOOS: "linux"}
	Lint(context.Background(), []byte(head+"- see `x/a.go`\n"), Request{}, "/repo/sub", o)
	want := "/repo: ls-tree -r --full-tree --name-only origin/main"
	if !slices.Contains(g.Calls(), want) {
		t.Errorf("calls %q lack %q", g.Calls(), want)
	}
}

func TestParseIssue(t *testing.T) {
	for in, want := range map[string]string{"7": "7", "#1188": "1188"} {
		if got, ok := ParseIssue(in); !ok || got != want {
			t.Errorf("ParseIssue(%q) = %q, %v", in, got, ok)
		}
	}
	for _, in := range []string{"", "#", "x1", "1x", "##1", "1 2"} {
		if _, ok := ParseIssue(in); ok {
			t.Errorf("ParseIssue(%q) accepted", in)
		}
	}
}

func TestRunExit(t *testing.T) {
	dir := t.TempDir()
	n := 0
	write := func(spec string) string {
		n++
		p := filepath.Join(dir, fmt.Sprintf("spec%d.md", n))
		if err := os.WriteFile(p, []byte(spec), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	o := Options{Git: world{tree: []string{"a.go"}}.git(), LookPath: lookIn(), GOOS: "linux"}
	for _, tc := range []struct {
		name string
		file string
		code int
		want string
	}{
		{"errors", write("# T\n\n## Acceptance\n- one\n"), 1,
			"error structure: no summary paragraph before the first \"## \" heading\nspec-lint: 1 error(s), 0 warning(s)\n"},
		{"warnings only", write(head + "- `mytool x`\n"), 0,
			"warn runnable: item 1: \"mytool\" is not on PATH here\nspec-lint: 0 error(s), 1 warning(s)\n"},
		{"clean", write(head + "- one\n"), 0, "spec-lint: 0 error(s), 0 warning(s)\n"},
		{"unreadable", filepath.Join(dir, "missing.md"), 2, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stderr bytes.Buffer
			code := Run(context.Background(), Request{File: tc.file}, "/repo", &stderr, o)
			if code != tc.code {
				t.Errorf("exit %d, want %d (stderr %q)", code, tc.code, stderr.String())
			}
			if tc.want != "" && stderr.String() != tc.want {
				t.Errorf("stderr %q, want %q", stderr.String(), tc.want)
			}
			if tc.code == 2 && !strings.HasPrefix(stderr.String(), "crew: spec-lint: ") {
				t.Errorf("stderr %q", stderr.String())
			}
		})
	}
}
