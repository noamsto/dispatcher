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
  [[ $stderr == *"crew git-baseline --accept"* ]]
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

@test "_wt_cfg_pairs lists config-defined hooks and gc.recentObjectsHook (#557)" {
  git config hook.x.command "$HIT"
  git config gc.recentObjectsHook "$HIT"
  run pairs "$COMMON"
  [ "$status" -eq 0 ]
  grep -Fx "hook.x.command=$HIT" <<<"$output"
  grep -Fx "gc.recentobjectshook=$HIT" <<<"$output"
}

@test "a planted config-defined hook is refused and never runs (#557)" {
  # hook.<name>.command fires on its event even under core.hooksPath=/dev/null.
  add_worktree w
  _wt_cfg_baseline_init "$COMMON"
  git config hook.x.event reference-transaction
  git config hook.x.command "touch $SENTINEL"
  run --separate-stderr _wt_cfg_guard "$COMMON"
  [ "$status" -eq 1 ]
  [[ $stderr == *hook.x.command* ]]
  run --separate-stderr _wt_git "$ADMIN" "$WT" reset -q --hard HEAD
  [ "$status" -eq 1 ]
  [ ! -e "$SENTINEL" ]
}

@test "_wt_git suppresses baselined config hooks on the events it triggers (#557)" {
  : >f
  git add f
  git commit -q -m f
  add_worktree w
  for event in reference-transaction post-checkout post-index-change; do
    git config --add hook.x.event "$event"
  done
  git config hook.x.command "touch $SENTINEL"
  _wt_cfg_baseline_init "$COMMON"
  echo x >"$WT/f"
  _wt_git "$ADMIN" "$WT" reset -q --hard HEAD
  _wt_git "$ADMIN" "$WT" checkout -q -b w2
  [ ! -e "$SENTINEL" ]
  # Control: the same reset without _wt_git does fire it.
  git -C "$WT" -c core.hooksPath=/dev/null reset -q --hard HEAD
  [ -e "$SENTINEL" ]
}

@test "_wt_cfg_guard_cwd also checks the caller's own worktree config (#557)" {
  add_worktree b
  git config extensions.worktreeConfig true
  _wt_cfg_baseline_init "$COMMON"
  git -C "$WT" config --worktree core.fsmonitor "$HIT"
  _wt_cfg_guard_cwd "$COMMON"
  cd "$WT"
  _wt_cfg_guard "$COMMON"
  run --separate-stderr _wt_cfg_guard_cwd "$COMMON"
  [ "$status" -eq 1 ]
  [[ $stderr == *core.fsmonitor* ]]
}

@test "guard escapes worker-controlled key and origin bytes (#557)" {
  _wt_cfg_baseline_init "$COMMON"
  git config "filter.a"$'\e'"[2Kb.clean" "$HIT"
  inc="$BATS_TEST_TMPDIR/inc"$'\e'"[2K"
  printf '[diff]\n\texternal = %s\n' "$HIT" >"$inc"
  git config include.path "$inc"
  run --separate-stderr _wt_cfg_guard "$COMMON"
  [ "$status" -eq 1 ]
  [[ $stderr == *filter.a* ]]
  [[ $stderr == *diff.external* ]]
  [[ $stderr != *$'\e'* ]]
}

@test "pairs dedupe bytewise whatever the locale (#557)" {
  export LC_ALL=en_US.UTF-8
  [ "$(printf 'a\376\0a\377\0' | sort -z -u | tr -cd '\0' | wc -c)" -eq 1 ] ||
    skip "no locale here collates distinct bytes equal"
  git config filter.x.clean "cat #"$'\376'
  _wt_cfg_baseline_init "$COMMON"
  git config --add filter.x.clean "cat #"$'\377'
  run --separate-stderr _wt_cfg_guard "$COMMON"
  [ "$status" -eq 1 ]
  [[ $stderr == *filter.x.clean* ]]
}
