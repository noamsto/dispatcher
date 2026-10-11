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
