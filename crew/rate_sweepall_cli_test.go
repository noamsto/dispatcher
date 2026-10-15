package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// ghOpenPR answers every reconcile query with an open PR, so a sweep never
// reaches the network and never upgrades a run.
const ghOpenPR = `#!/bin/sh
printf '%s\n' "$*" >>"$GH_LOG"
case "$1" in
pr) echo '{"state":"OPEN","closedAt":null,"mergedAt":null,"mergeCommit":null,"commits":[],"reviews":[]}' ;;
api)
  for a in "$@"; do
    if [ "$a" = graphql ]; then
      echo '{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[]}}}}}'
      exit 0
    fi
  done
  echo '{"workflow_runs":[]}'
  ;;
esac
exit 0
`

// sweepAllCLI is a machine with a ratings store and a registry under a temp
// XDG root, gh stubbed on PATH, and no repo of its own: --sweep-all must not
// need the caller to be inside one.
type sweepAllCLI struct {
	t     *testing.T
	root  string // the --root the sweeps walk
	data  string // XDG_DATA_HOME
	ghLog string
}

func newSweepAllCLI(t *testing.T) *sweepAllCLI {
	t.Helper()
	unsetenv(t, "CREW_ID", "CREW_SWEEP_ROOTS", "CREW_WORKER_ID", "CREW_ROLE_ID", "CREW_CLOCK")
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c := &sweepAllCLI{t: t, root: filepath.Join(tmp, "root"), data: filepath.Join(tmp, "data"), ghLog: filepath.Join(tmp, "gh.log")}
	if err := os.MkdirAll(c.root, 0o755); err != nil {
		t.Fatal(err)
	}
	stubs := filepath.Join(tmp, "stubs")
	if err := os.Mkdir(stubs, 0o755); err != nil {
		t.Fatal(err)
	}
	writeStub(t, stubs, "gh", ghOpenPR)
	t.Setenv("PATH", stubs+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GH_LOG", c.ghLog)
	t.Setenv("HOME", filepath.Join(tmp, "home"))
	t.Setenv("XDG_DATA_HOME", c.data)
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CEILING_DIRECTORIES", tmp)
	t.Setenv("DISPATCH_CONFIG_BIN", filepath.Join(tmp, "no-such-dispatch-config"))
	return c
}

// repo is a git repo with a one-run bus whose PR url targets its own slug (the
// reconcile gate spends gh credentials on the dispatching repo only). Without a
// slug it has no origin remote.
func (c *sweepAllCLI) repo(dir, slug string) string {
	c.t.Helper()
	if out, err := exec.Command("git", "init", "-q", "-b", "main", dir).CombinedOutput(); err != nil {
		c.t.Fatalf("git init: %v\n%s", err, out)
	}
	if slug != "" {
		if out, err := exec.Command("git", "-C", dir, "remote", "add", "origin", "https://github.com/"+slug+".git").CombinedOutput(); err != nil {
			c.t.Fatalf("git remote add: %v\n%s", err, out)
		}
	}
	pr := "acme/none"
	if slug != "" {
		pr = slug
	}
	busDir := filepath.Join(dir, ".git", "crew")
	if err := os.MkdirAll(busDir, 0o755); err != nil {
		c.t.Fatal(err)
	}
	rows := `{"ts":1000,"crew_id":"c1","kind":"dispatch","branch":"feat/x","engine":"claude","model":"sonnet","tier":"standard","effort":"medium","title":"t"}` + "\n" +
		`{"ts":1500,"crew_id":"c1","from":"worker:feat/x","to":"dispatcher:c1","kind":"status","body":{"state":"pr_open","pr_url":"https://github.com/` + pr + `/pull/1"}}` + "\n"
	if err := os.WriteFile(filepath.Join(busDir, "events.jsonl"), []byte(rows), 0o644); err != nil {
		c.t.Fatal(err)
	}
	return dir
}

// runIn runs argv with cwd as the caller's directory.
func (c *sweepAllCLI) runIn(cwd string, args ...string) (stdout, stderr string, code int) {
	c.t.Helper()
	e := env{getwd: func() (string, error) { return cwd, nil }}
	var out, errb bytes.Buffer
	code = run(context.Background(), args, &out, &errb, e)
	return out.String(), errb.String(), code
}

// sweepAll runs `crew rate --sweep-all --root <root>` from outside any repo.
func (c *sweepAllCLI) sweepAll(extra ...string) (string, string, int) {
	c.t.Helper()
	return c.runIn(c.t.TempDir(), append([]string{"rate", "--sweep-all"}, extra...)...)
}

func (c *sweepAllCLI) storePath() string { return filepath.Join(c.data, "crew", "ratings.jsonl") }

// storeRepos is the sorted set of repos with a row in the shared store.
func (c *sweepAllCLI) storeRepos() string {
	c.t.Helper()
	data, err := os.ReadFile(c.storePath())
	if err != nil {
		c.t.Fatal(err)
	}
	seen := map[string]bool{}
	for line := range strings.SplitSeq(strings.TrimSpace(string(data)), "\n") {
		var row struct {
			Repo string `json:"repo"`
		}
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			c.t.Fatalf("store row %q: %v", line, err)
		}
		seen[row.Repo] = true
	}
	repos := make([]string, 0, len(seen))
	for r := range seen {
		repos = append(repos, r)
	}
	slices.Sort(repos)
	return strings.Join(repos, ",")
}

func requireSweepAll(t *testing.T, c *sweepAllCLI, want int, args ...string) (string, string) {
	t.Helper()
	out, errb, code := c.sweepAll(args...)
	if code != want {
		t.Fatalf("rate --sweep-all %v: code %d, want %d\nstdout %q\nstderr %q", args, code, want, out, errb)
	}
	return out, errb
}

func wantContains(t *testing.T, label, got string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("%s = %q, want it to contain %q", label, got, w)
		}
	}
}

func TestSweepAllCLISweepsEveryRepoUnderRootNestedIncluded(t *testing.T) {
	c := newSweepAllCLI(t)
	c.repo(filepath.Join(c.root, "a"), "acme/alpha")
	c.repo(filepath.Join(c.root, "b"), "acme/beta")
	if err := os.MkdirAll(filepath.Join(c.root, "group"), 0o755); err != nil {
		t.Fatal(err)
	}
	c.repo(filepath.Join(c.root, "group", "c"), "acme/gamma")

	out, _ := requireSweepAll(t, c, 0, "--root", c.root)
	wantContains(t, "stdout", out,
		"acme/alpha: swept, 1 runs in store",
		"acme/beta: swept, 1 runs in store",
		"acme/gamma: swept, 1 runs in store")
	if got := c.storeRepos(); got != "acme/alpha,acme/beta,acme/gamma" {
		t.Errorf("store repos = %q", got)
	}
}

func TestSweepAllCLISkipsARepoWithNoOriginAndADirWithNoBus(t *testing.T) {
	c := newSweepAllCLI(t)
	c.repo(filepath.Join(c.root, "a"), "acme/alpha")
	c.repo(filepath.Join(c.root, "noremote"), "")
	if err := os.MkdirAll(filepath.Join(c.root, "nobus", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	out, errb := requireSweepAll(t, c, 0, "--root", c.root)
	wantContains(t, "stdout", out, "noremote: skipped (no origin remote)")
	if strings.Contains(out+errb, "nobus") {
		t.Errorf("output mentions nobus: stdout %q stderr %q", out, errb)
	}
	if got := c.storeRepos(); got != "acme/alpha" {
		t.Errorf("store repos = %q, want only acme/alpha", got)
	}
}

func TestSweepAllCLISecondRunLeavesTheStoreByteIdentical(t *testing.T) {
	c := newSweepAllCLI(t)
	c.repo(filepath.Join(c.root, "a"), "acme/alpha")
	c.repo(filepath.Join(c.root, "b"), "acme/beta")

	requireSweepAll(t, c, 0, "--root", c.root)
	before, err := os.ReadFile(c.storePath())
	if err != nil {
		t.Fatal(err)
	}
	requireSweepAll(t, c, 0, "--root", c.root)
	after, err := os.ReadFile(c.storePath())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("store changed on the second run:\nbefore %q\nafter  %q", before, after)
	}
}

func TestSweepAllCLIFindsARepoOutsideRootThroughTheRegistryOnceSwept(t *testing.T) {
	c := newSweepAllCLI(t)
	elsewhere := c.repo(filepath.Join(filepath.Dir(c.root), "elsewhere"), "acme/outside")
	if _, errb, code := c.runIn(elsewhere, "rate"); code != 0 {
		t.Fatalf("rate: code %d, stderr %q", code, errb)
	}
	registry, err := os.ReadFile(filepath.Join(c.data, "crew", "repos"))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(strings.Split(string(registry), "\n"), filepath.Join(elsewhere, ".git")) {
		t.Fatalf("registry = %q, want a line for %s", registry, filepath.Join(elsewhere, ".git"))
	}
	if err := os.Remove(c.storePath()); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(c.root, "empty")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatal(err)
	}

	out, _ := requireSweepAll(t, c, 0, "--root", empty)
	wantContains(t, "stdout", out, "acme/outside: swept, 1 runs in store")
}

func TestSweepAllCLIRegisteredRepoThatNoLongerExistsIsSkippedAsGone(t *testing.T) {
	c := newSweepAllCLI(t)
	if err := os.MkdirAll(filepath.Join(c.data, "crew"), 0o755); err != nil {
		t.Fatal(err)
	}
	vanished := filepath.Join(filepath.Dir(c.root), "vanished", ".git")
	if err := os.WriteFile(filepath.Join(c.data, "crew", "repos"), []byte(vanished+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, _ := requireSweepAll(t, c, 0, "--root", c.root)
	wantContains(t, "stdout", out, "vanished/.git: skipped (gone)")
}

func TestSweepAllCLIFailingRepoIsReportedTheRestStillSweepAndExitIsOne(t *testing.T) {
	c := newSweepAllCLI(t)
	c.repo(filepath.Join(c.root, "a"), "acme/alpha")
	bad := c.repo(filepath.Join(c.root, "bad"), "acme/broken")
	if err := os.WriteFile(filepath.Join(bad, ".git", "crew", "events.jsonl"), []byte("not json\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, _ := requireSweepAll(t, c, 1, "--root", c.root)
	wantContains(t, "stdout", out, "acme/broken: failed (rc=", "acme/alpha: swept, 1 runs in store")
	if got := c.storeRepos(); got != "acme/alpha" {
		t.Errorf("store repos = %q, want only acme/alpha", got)
	}
}

func TestSweepAllCLINoReposFoundIsExitZeroButSaysSo(t *testing.T) {
	c := newSweepAllCLI(t)
	out, errb := requireSweepAll(t, c, 0, "--root", c.root)
	if out != "" {
		t.Errorf("stdout = %q, want empty", out)
	}
	wantContains(t, "stderr", errb, "no repo with a crew bus found")
}

func TestSweepAllCLIRootThatIsNotADirectoryIsReported(t *testing.T) {
	c := newSweepAllCLI(t)
	typo := filepath.Join(c.root, "typo")
	_, errb := requireSweepAll(t, c, 0, "--root", typo)
	wantContains(t, "stderr", errb, typo+" is not a directory")
}

func TestSweepAllCLIUnreadableDirectoryUnderRootDoesNotAbortTheWalk(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-000 directory")
	}
	c := newSweepAllCLI(t)
	c.repo(filepath.Join(c.root, "alpha"), "acme/alpha")
	locked := filepath.Join(c.root, "locked")
	if err := os.Mkdir(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	out, _ := requireSweepAll(t, c, 0, "--root", c.root)
	wantContains(t, "stdout", out, "acme/alpha: swept")
}

func TestSweepAllCLIRunsOutsideAnyGitRepo(t *testing.T) {
	c := newSweepAllCLI(t)
	c.repo(filepath.Join(c.root, "alpha"), "acme/alpha")
	outside := t.TempDir()

	out, errb, code := c.runIn(outside, "rate", "--sweep-all", "--root", c.root)
	if code != 0 {
		t.Fatalf("sweep-all: code %d, stderr %q", code, errb)
	}
	wantContains(t, "stdout", out, "acme/alpha: swept")

	out, errb, code = c.runIn(outside, "rate")
	if code != 1 {
		t.Errorf("plain rate outside a repo: code %d, want 1", code)
	}
	wantContains(t, "output", out+errb, "not in a git repo")
}

func TestSweepAllCLIConflictingOrMalformedFlagsAreErrors(t *testing.T) {
	c := newSweepAllCLI(t)
	// --root without --sweep-all is refused after the repo is located.
	host := c.repo(filepath.Join(filepath.Dir(c.root), "host"), "acme/host")
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--sweep-all", "--report"}, "rate takes --report, --json, --pooled, and --sweep-all/--root"},
		{[]string{"--sweep-all", "--json"}, ""},
		{[]string{"--sweep-all", "--pooled"}, ""},
		{[]string{"--root", c.root}, "rate --root needs --sweep-all"},
		{[]string{"--sweep-all", "--root"}, "rate --root needs a directory"},
	} {
		out, errb, code := c.runIn(host, append([]string{"rate"}, tc.args...)...)
		if code != 1 {
			t.Errorf("rate %v: code %d, want 1 (stdout %q stderr %q)", tc.args, code, out, errb)
		}
		wantContains(t, "output of rate "+strings.Join(tc.args, " "), out+errb, tc.want)
	}
}
