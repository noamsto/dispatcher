package wtgit

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/drift"
)

// The bash helpers stay in use until their callers are ported, so the two
// config tables must not drift apart.
func TestTablesMatchShell(t *testing.T) {
	const path = "../../../adapters/core/worktree-git.sh"
	data, err := os.ReadFile(path)
	if err != nil {
		drift.Missing(t, path, err)
	}
	src := string(data)

	block := regexp.MustCompile(`(?s)_wt_neutral_cfg=\((.*?)\)`).FindStringSubmatch(src)
	if block == nil {
		t.Fatal("no _wt_neutral_cfg=( … ) block in worktree-git.sh")
	}
	if got := strings.Fields(block[1]); !slices.Equal(got, NeutralCfg) {
		t.Errorf("_wt_neutral_cfg = %q, NeutralCfg = %q", got, NeutralCfg)
	}

	loop := regexp.MustCompile(`for kv in "\$\{_wt_neutral_cfg\[@\]\}" ([^;]+); do _wt_argv\+=\(-c "\$kv"\); done`).FindStringSubmatch(src)
	if loop == nil {
		t.Fatal("no _wt_common_argv loop in worktree-git.sh")
	}
	if got := strings.Fields(loop[1]); !slices.Equal(got, CommonExtra) {
		t.Errorf("_wt_common_argv extras = %q, CommonExtra = %q", got, CommonExtra)
	}
}

// PlainBranchName and FetchOriginBranch are the Go copies of two helpers that
// stay in the lib for dispatch.sh's wrappers, so the security-relevant parts of
// them — the argv, the refspec, the porcelain flags and the two pre-git guards —
// are pinned to the bash text.
func TestFetchHelpersMatchShell(t *testing.T) {
	const path = "../../../adapters/core/worktree-git.sh"
	data, err := os.ReadFile(path)
	if err != nil {
		drift.Missing(t, path, err)
	}
	src := string(data)

	fetch := regexp.MustCompile(`_wt_git_common "\$common" (fetch [^"]+) "(\+refs/heads/\$name:refs/remotes/origin/\$name)"`).FindStringSubmatch(src)
	if fetch == nil {
		t.Fatal(`no _wt_git_common "$common" fetch … "+refs/heads/$name:refs/remotes/origin/$name" in worktree-git.sh`)
	}
	if got := strings.Fields(fetch[1]); !slices.Equal(got, fetchArgs) {
		t.Errorf("the bash fetch argv = %q, fetchArgs = %q", got, fetchArgs)
	}
	if got := strings.ReplaceAll(fetch[2], "$name", "%s"); got != fetchSpec {
		t.Errorf("the bash refspec = %q, fetchSpec = %q", got, fetchSpec)
	}

	flags := regexp.MustCompile(`case \$flag in ([^)]*)\)`).FindStringSubmatch(src)
	if flags == nil {
		t.Fatal("no `case $flag in … )` in _wt_fetch_origin_branch")
	}
	got := map[byte]bool{}
	for _, tok := range strings.Split(flags[1], "|") {
		tok = strings.Trim(strings.TrimSpace(tok), "'")
		if tok != "" {
			got[tok[0]] = true
		}
	}
	if len(got) != len(fetchFlags) {
		t.Errorf("the bash porcelain flags = %v, fetchFlags = %v", got, fetchFlags)
	}
	for f := range got {
		if !fetchFlags[f] {
			t.Errorf("the bash porcelain flag %q is not in fetchFlags", f)
		}
	}

	guards := regexp.MustCompile(`(?s)_wt_plain_branch_name\(\) \{(.*?)\n\}`).FindStringSubmatch(src)
	if guards == nil {
		t.Fatal("no _wt_plain_branch_name in worktree-git.sh")
	}
	if !strings.Contains(guards[1], "*:*") || !strings.Contains(guards[1], "+*") {
		t.Errorf("_wt_plain_branch_name no longer guards a colon and a leading +: %s", guards[1])
	}
	if !strings.Contains(guards[1], `git check-ref-format --branch "$1"`) {
		t.Errorf("_wt_plain_branch_name no longer asks git check-ref-format --branch: %s", guards[1])
	}
}
