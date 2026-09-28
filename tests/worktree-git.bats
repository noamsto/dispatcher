bats_require_minimum_version 1.5.0 # `run --separate-stderr`

setup() {
  load helpers
  setup_repo
  . "$WORKTREE_GIT_LIB"
  git commit -q --allow-empty -m init
  COMMON="$TEST_REPO/.git"
  BASELINE="$COMMON/crew/git-config-baseline"
  SENTINEL="$BATS_TEST_TMPDIR/SENTINEL"
  HIT="$BATS_TEST_TMPDIR/hit.sh"
  printf '#!/bin/sh\ntouch %q\ncat\n' "$SENTINEL" >"$HIT"
  chmod +x "$HIT"
}

teardown() {
  teardown_repo
}

# key=value lines, one per NUL-terminated key\nvalue pair.
pairs() { _wt_cfg_pairs "$@" | tr '\n\0' '=\n'; }

add_worktree() { # <name> -> $WT, $ADMIN
  WT="$BATS_TEST_TMPDIR/$1"
  git worktree add -q "$WT" -b "$1"
  ADMIN="$(_wt_admin_dir "$COMMON" "$WT")"
  [ "$ADMIN" = "$(realpath -e "$COMMON/worktrees/$1")" ]
}

@test "_wt_cfg_pairs lists local exec-capable keys only (#557)" {
  git config filter.X.clean "$HIT"
  git config alias.lg '!sh'
  git config alias.st status
  export GIT_CONFIG_GLOBAL="$BATS_TEST_TMPDIR/global"
  printf '[credential]\n\thelper = store\n' >"$GIT_CONFIG_GLOBAL"
  run pairs "$COMMON"
  [ "$status" -eq 0 ]
  grep -Fx "filter.X.clean=$HIT" <<<"$output"
  grep -Fx 'alias.lg=!sh' <<<"$output"
  [[ $output != *alias.st* ]]
  [[ $output != *credential.helper* ]]
}

@test "unparsable config fails closed in pairs, union and guard (#557)" {
  _wt_cfg_baseline_init "$COMMON"
  printf '[core\n' >>"$COMMON/config"
  run _wt_cfg_pairs "$COMMON"
  [ "$status" -ne 0 ]
  run _wt_cfg_union "$COMMON"
  [ "$status" -ne 0 ]
  run --separate-stderr _wt_cfg_guard "$COMMON"
  [ "$status" -eq 1 ]
  [[ $stderr == *"cannot list the git config"* ]]
}

@test "baseline captures pre-existing keys, is idempotent, and passes the guard (#557)" {
  git config core.hooksPath .husky
  git config filter.lfs.smudge 'git-lfs smudge -- %f'
  git config credential.helper 'store --file=/secret-token'
  run --separate-stderr _wt_cfg_baseline_init "$COMMON"
  [ "$status" -eq 0 ]
  [ -f "$BASELINE" ]
  [[ $stderr == *core.hookspath* ]]
  [[ $stderr == *filter.lfs.smudge* ]]
  [[ $stderr == *credential.helper* ]]
  [[ $stderr != *secret-token* ]]
  [[ $stderr != *git-lfs\ smudge* ]]
  cp "$BASELINE" "$BATS_TEST_TMPDIR/first"
  _wt_cfg_guard "$COMMON"
  _wt_cfg_baseline_init "$COMMON"
  cmp "$BASELINE" "$BATS_TEST_TMPDIR/first"
}

@test "guard refuses a planted filter and names key and origin, never the value (#557)" {
  _wt_cfg_baseline_init "$COMMON"
  git config filter.x.clean "$HIT --planted-value"
  run --separate-stderr _wt_cfg_guard "$COMMON"
  [ "$status" -eq 1 ]
  [[ $stderr == *filter.x.clean* ]]
  [[ $stderr == *"(from $COMMON/config)"* ]]
  [[ $stderr == *"--unset-all filter.x.clean"* ]]
  [[ $stderr == *"crew git-baseline --accept"* ]]
  [[ $stderr != *planted-value* ]]
}

@test "guard refuses include.path and the exec key it pulls in (#557)" {
  _wt_cfg_baseline_init "$COMMON"
  printf '[core]\n\tfsmonitor = %s\n' "$HIT" >"$BATS_TEST_TMPDIR/inc"
  git config include.path "$BATS_TEST_TMPDIR/inc"
  run --separate-stderr _wt_cfg_guard "$COMMON"
  [ "$status" -eq 1 ]
  [[ $stderr == *include.path* ]]
  [[ $stderr == *core.fsmonitor*"(from $BATS_TEST_TMPDIR/inc)"* ]]
}

@test "baseline unions per-worktree includeIf keys; new keys there are refused (#557)" {
  add_worktree w
  # No trailing slash: git appends `**` to one, which then needs a path
  # component below the admin dir itself.
  git config 'includeIf.gitdir:**/worktrees/w.path' "$BATS_TEST_TMPDIR/inc-w"
  printf '[core]\n\tpager = less\n' >"$BATS_TEST_TMPDIR/inc-w"
  run pairs "$COMMON"
  [[ $output != *core.pager* ]]
  run pairs "$ADMIN"
  grep -Fx core.pager=less <<<"$output"
  _wt_cfg_baseline_init "$COMMON"
  _wt_cfg_guard "$COMMON"
  _wt_cfg_guard "$COMMON" "$COMMON/worktrees/w"
  printf '[diff]\n\texternal = %s\n' "$HIT" >>"$BATS_TEST_TMPDIR/inc-w"
  _wt_cfg_guard "$COMMON"
  run --separate-stderr _wt_cfg_guard "$COMMON" "$COMMON/worktrees/w"
  [ "$status" -eq 1 ]
  [[ $stderr == *diff.external* ]]
}

@test "unsetting a baseline key is not drift (#557)" {
  git config filter.a.clean cat
  git config credential.helper store
  _wt_cfg_baseline_init "$COMMON"
  git config --unset filter.a.clean
  _wt_cfg_guard "$COMMON"
}

@test "_wt_git refuses an admin dir not shaped <common>/worktrees/<id> (#557)" {
  add_worktree w
  _wt_cfg_baseline_init "$COMMON"
  run --separate-stderr _wt_git "$COMMON" "$WT" status --porcelain
  [ "$status" -eq 1 ]
  [[ $stderr == *"is not a linked-worktree admin dir"* ]]
  run --separate-stderr _wt_git "$COMMON/worktrees/" "$WT" status --porcelain
  [ "$status" -eq 1 ]
  [[ $stderr == *"is not a linked-worktree admin dir"* ]]
}

@test "_wt_admin_dir refuses a worktrees/<id> symlinked to a copy elsewhere (#557)" {
  add_worktree w
  cp -a "$COMMON/worktrees/w" "$BATS_TEST_TMPDIR/copy"
  rm -rf "$COMMON/worktrees/w"
  ln -s "$BATS_TEST_TMPDIR/copy" "$COMMON/worktrees/w"
  run _wt_admin_dir "$COMMON" "$WT"
  [ "$status" -eq 1 ]
  [ -z "$output" ]
}

@test "_wt_git refuses without a baseline and does not create one (#557)" {
  add_worktree w
  run --separate-stderr _wt_git "$ADMIN" "$WT" status --porcelain
  [ "$status" -eq 1 ]
  [[ $stderr == *"no git-config baseline at $BASELINE"* ]]
  [ ! -e "$BASELINE" ]
  [ ! -e "$COMMON/crew" ]
}

@test "_wt_git passes a clean worktree once the baseline exists (#557)" {
  add_worktree w
  _wt_cfg_baseline_init "$COMMON"
  echo x >"$WT/new"
  run --separate-stderr _wt_git "$ADMIN" "$WT" status --porcelain
  [ "$status" -eq 0 ]
  [ "$output" = "?? new" ]
}

@test "_wt_git never runs a worker-planted clean filter (#557)" {
  : >f
  echo 'f filter=x' >.gitattributes
  git add f .gitattributes
  git commit -q -m attrs
  add_worktree w
  _wt_cfg_baseline_init "$COMMON"
  git -C "$WT" config filter.x.clean "$HIT"
  echo x >"$WT/f"
  run --separate-stderr _wt_git "$ADMIN" "$WT" status --porcelain
  [ "$status" -eq 1 ]
  [[ $stderr == *filter.x.clean* ]]
  [ ! -e "$SENTINEL" ]
}
