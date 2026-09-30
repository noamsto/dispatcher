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

# commit_conv_driver <gitattributes-content> — commit a benign tools/conv.sh
# (cats its arg, or stdin), file `a` (no trailing newline), and .gitattributes
# with the given content (#578).
commit_conv_driver() {
  mkdir -p tools
  printf '#!/bin/sh\nif [ $# -gt 0 ]; then cat "$1"; else cat; fi\n' >tools/conv.sh
  chmod +x tools/conv.sh
  printf a >a
  printf '%s' "$1" >.gitattributes
  git add tools/conv.sh a .gitattributes
  git commit -q -m fixture
}

# rewrite_conv_and_commit — after add_worktree, rewrite $WT/tools/conv.sh so
# it touches $SENTINEL before behaving as before, and commit it on the
# worktree's own branch so `reset --hard HEAD` keeps the rewrite (#578).
rewrite_conv_and_commit() {
  printf '#!/bin/sh\ntouch %q\nif [ $# -gt 0 ]; then cat "$1"; else cat; fi\n' "$SENTINEL" >"$WT/tools/conv.sh"
  git -C "$WT" commit -qam rewrite
  rm -f "$SENTINEL"
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
  [[ $stderr == *"list drift with \`crew git-baseline\`"* ]]
  [[ $stderr == *"crew git-baseline --accept"* ]]
  [[ $stderr == *"deleting $BASELINE instead"* ]]
  [[ $stderr != *planted-value* ]]
}

@test "deleting the baseline is the recovery: the next dispatch re-records and the guard passes (#557)" {
  _wt_cfg_baseline_init "$COMMON"
  git config filter.x.clean cat
  run --separate-stderr _wt_cfg_guard "$COMMON"
  [ "$status" -eq 1 ]
  [[ $stderr == *"deleting $BASELINE instead"* ]]
  rm "$BASELINE"
  run --separate-stderr _wt_cfg_guard "$COMMON"
  [ "$status" -eq 1 ]
  [[ $stderr == *"no git-config baseline at $BASELINE — the next dispatch records it"* ]]
  _wt_cfg_baseline_init "$COMMON"
  grep -q filter.x.clean "$BASELINE"
  _wt_cfg_guard "$COMMON"
}

@test "guard skips empty baseline records (#585)" {
  mkdir -p "$COMMON/crew"
  printf '\0' >"$BASELINE"
  run --separate-stderr _wt_cfg_guard "$COMMON"
  [ "$status" -eq 0 ]
  [[ $stderr != *"bad array subscript"* ]]
  printf 'core.fsmonitor\nx\0\0' >"$BASELINE"
  git config core.fsmonitor x
  run --separate-stderr _wt_cfg_guard "$COMMON"
  [ "$status" -eq 0 ]
  [[ $stderr != *"bad array subscript"* ]]
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

# The value git-hooks.nix writes from a linked worktree (#628).
hookspath_abs() { printf '%s/hooks' "$(git -C "${1:-.}" rev-parse --path-format=absolute --git-common-dir)"; }

@test "guard accepts a relative .git/hooks when the absolute spelling is baselined (#628)" {
  add_worktree w
  git config core.hooksPath "$(hookspath_abs "$WT")"
  _wt_cfg_baseline_init "$COMMON"
  git config core.hooksPath .git/hooks
  _wt_cfg_guard "$COMMON"
  _wt_cfg_guard "$COMMON" "$ADMIN"
  _wt_cfg_guard_cwd "$COMMON"
  _wt_git "$ADMIN" "$WT" status --porcelain >/dev/null
}

@test "guard accepts the absolute spelling when relative .git/hooks is baselined (#628)" {
  add_worktree w
  git config core.hooksPath .git/hooks
  _wt_cfg_baseline_init "$COMMON"
  git config core.hooksPath "$(hookspath_abs "$WT")"
  _wt_cfg_guard "$COMMON"
  _wt_cfg_guard "$COMMON" "$ADMIN"
  (cd "$WT" && _wt_cfg_guard_cwd "$COMMON")
  _wt_git "$ADMIN" "$WT" status --porcelain >/dev/null
}

@test "guard still refuses a core.hooksPath naming a different dir (#628)" {
  git config core.hooksPath "$(hookspath_abs)"
  _wt_cfg_baseline_init "$COMMON"
  for v in "$BATS_TEST_TMPDIR/hooks" .husky .git/../evil ./.git/hooks .git//hooks .git/hooks/ .git/hooks-evil; do
    git config core.hooksPath "$v"
    run --separate-stderr _wt_cfg_guard "$COMMON"
    [ "$status" -eq 1 ]
    [[ $stderr == *core.hookspath* ]]
  done
  rm -f "$BASELINE"
  git config core.hooksPath "$(realpath "$TEST_REPO")/.husky"
  _wt_cfg_baseline_init "$COMMON"
  git config core.hooksPath .husky
  run --separate-stderr _wt_cfg_guard "$COMMON"
  [ "$status" -eq 1 ]
}

@test "a symlinked-prefix absolute baseline does not match relative .git/hooks (#628)" {
  ln -s "$TEST_REPO" "$BATS_TEST_TMPDIR/link"
  git config core.hooksPath "$BATS_TEST_TMPDIR/link/.git/hooks"
  _wt_cfg_baseline_init "$COMMON"
  git config core.hooksPath .git/hooks
  run --separate-stderr _wt_cfg_guard "$COMMON"
  [ "$status" -eq 1 ]
}

# hooks_rel_setup [<wt-name>] — baseline the absolute hooks dir, then switch
# the config to the relative spelling git-hooks.nix writes from the main root.
hooks_rel_setup() {
  if [ -n "${1:-}" ]; then
    add_worktree "$1"
    git config core.hooksPath "$(hookspath_abs "$WT")"
  else
    git config core.hooksPath "$(hookspath_abs)"
  fi
  _wt_cfg_baseline_init "$COMMON"
  git config core.hooksPath .git/hooks
}

plant_hook() { # <hooks-dir> — a reference-transaction hook that touches $SENTINEL
  mkdir -p "$1"
  printf '#!/bin/sh\ntouch %q\n' "$SENTINEL" >"$1/reference-transaction"
  chmod +x "$1/reference-transaction"
}

# guard_cwd_refuses <dir> — _wt_cfg_guard_cwd refuses from <dir>. Had it
# passed, the update-ref it guards would have run a planted hook.
guard_cwd_refuses() {
  cd "$1"
  run --separate-stderr _wt_cfg_guard_cwd "$COMMON"
  if [ "$status" -eq 0 ]; then git update-ref refs/heads/probe HEAD; fi
  [ ! -e "$SENTINEL" ]
  [ "$status" -eq 1 ]
  [[ $stderr == 'refusing git: '* ]]
}

@test "guard_cwd accepts relative .git/hooks against an absolute baseline from the main checkout (#628)" {
  hooks_rel_setup w
  _wt_cfg_guard_cwd "$COMMON"
  mkdir sub
  (cd sub && _wt_cfg_guard_cwd "$COMMON")
}

@test "guard_cwd refuses relative .git/hooks from a genuine linked worktree (#628)" {
  hooks_rel_setup w
  run git -C "$WT" rev-parse --path-format=absolute --git-path hooks
  [ "$status" -eq 128 ]
  guard_cwd_refuses "$WT"
  [[ $stderr == *"$(printf %q "$PWD")"* ]]
  [[ $stderr == *"$(realpath "$COMMON")/hooks"* ]]
}

@test "guard_cwd refuses relative .git/hooks from a worktree whose .git is a directory (#628)" {
  hooks_rel_setup w
  rm "$WT/.git"
  mkdir "$WT/.git"
  printf 'ref: refs/heads/w\n' >"$WT/.git/HEAD"
  realpath "$COMMON" >"$WT/.git/commondir"
  plant_hook "$WT/.git/hooks"
  [ "$(git -C "$WT" config --get core.hooksPath)" = .git/hooks ]
  guard_cwd_refuses "$WT"
}

@test "guard_cwd refuses relative .git/hooks through a .git symlink to its own admin dir (#628)" {
  hooks_rel_setup w
  rm "$WT/.git"
  ln -s "$ADMIN" "$WT/.git"
  plant_hook "$WT/.git/hooks"
  guard_cwd_refuses "$WT"
}

@test "guard_cwd refuses relative .git/hooks through a .git symlink to a fake admin dir (#628)" {
  hooks_rel_setup w
  cp -r "$ADMIN" "$COMMON/worktrees/evil"
  rm "$WT/.git"
  ln -s "$COMMON/worktrees/evil" "$WT/.git"
  plant_hook "$WT/.git/hooks"
  guard_cwd_refuses "$WT"
}

@test "guard_cwd refuses relative .git/hooks in a linked worktree made bare by config.worktree (#628)" {
  hooks_rel_setup w
  git config extensions.worktreeConfig true
  git -C "$WT" config --worktree core.bare true
  mkdir "$WT/sub"
  plant_hook "$WT/sub/.git/hooks"
  guard_cwd_refuses "$WT/sub"
}

@test "guard_cwd refuses relative .git/hooks from a main subdir when core.bare is set (#628)" {
  hooks_rel_setup
  git config core.bare true
  mkdir sub
  plant_hook sub/.git/hooks
  guard_cwd_refuses "$TEST_REPO/sub"
}

@test "guard_cwd refuses relative .git/hooks when core.worktree moves the work tree (#628)" {
  hooks_rel_setup
  mkdir "$BATS_TEST_TMPDIR/elsewhere"
  git config core.worktree "$BATS_TEST_TMPDIR/elsewhere"
  # From a subdir the value resolves against the cwd, not the moved work tree.
  mkdir sub
  plant_hook sub/.git/hooks
  guard_cwd_refuses "$TEST_REPO/sub"
}

@test "guard_cwd refuses relative .git/hooks under core.worktree from the main top level (#628)" {
  hooks_rel_setup
  W="$BATS_TEST_TMPDIR/worker"
  plant_hook "$W/.git/hooks"
  cp "$W/.git/hooks/reference-transaction" "$W/.git/hooks/post-checkout"
  git config core.worktree "$W"
  git config core.hooksPath .git/hooks
  cd "$TEST_REPO"
  run --separate-stderr _wt_cfg_guard_cwd "$COMMON"
  # Work-tree commands chdir into core.worktree and run $W/.git/hooks.
  if [ "$status" -eq 0 ]; then git checkout -q -b probe; fi
  [ ! -e "$SENTINEL" ]
  [ "$status" -eq 1 ]
  [[ $stderr == 'refusing git: '*core.worktree* ]]
  [[ $stderr == *"git config core.hooksPath $(printf %q "$(realpath "$COMMON")/hooks")"* ]]
}

@test "guard_cwd accepts relative .git/hooks when the baselined hooks dir is a symlink (#628)" {
  git config core.hooksPath "$(hookspath_abs)"
  _wt_cfg_baseline_init "$COMMON"
  mv "$COMMON/hooks" "$BATS_TEST_TMPDIR/realhooks"
  ln -s "$BATS_TEST_TMPDIR/realhooks" "$COMMON/hooks"
  git config core.hooksPath .git/hooks
  cd "$TEST_REPO"
  _wt_cfg_guard_cwd "$COMMON"
}

@test "guard_cwd advises the main checkout only from its own context (#628)" {
  hooks_rel_setup w
  cd "$WT"
  run --separate-stderr _wt_cfg_guard_cwd "$COMMON"
  [ "$status" -eq 1 ]
  [[ $stderr != *"run from the main checkout"* ]]
  [[ $stderr == *"set the absolute spelling: git config core.hooksPath"* ]]
  cd "$TEST_REPO"
  git config core.bare true
  mkdir sub
  cd sub
  run --separate-stderr _wt_cfg_guard_cwd "$COMMON"
  [ "$status" -eq 1 ]
  [[ $stderr == *"run from the main checkout, or set the absolute spelling: git config core.hooksPath"* ]]
}

@test "guard_cwd accepts relative .git/hooks from a worktree whose .git links to the common dir (#628)" {
  hooks_rel_setup w
  rm "$WT/.git"
  # Intended: git runs the common hooks dir from here, the baselined one.
  ln -s "$COMMON" "$WT/.git"
  cd "$WT"
  _wt_cfg_guard_cwd "$COMMON"
}

@test "guard_cwd accepts relative .git/hooks from a linked worktree when that spelling is baselined (#628)" {
  add_worktree w
  git config core.hooksPath .git/hooks
  _wt_cfg_baseline_init "$COMMON"
  cd "$WT"
  _wt_cfg_guard_cwd "$COMMON"
}

@test "guard_cwd refuses when reading core.hooksPath fails (#628)" {
  git config core.hooksPath "$(hookspath_abs)"
  _wt_cfg_baseline_init "$COMMON"
  git() {
    if [ "$*" = 'config --get core.hooksPath' ]; then return 2; fi
    command git "$@"
  }
  run --separate-stderr _wt_cfg_guard_cwd "$COMMON"
  [ "$status" -eq 1 ]
  [[ $stderr == 'refusing git: '* ]]
}

@test "_wt_cfg_canon maps only the exact relative .git/hooks (#628)" {
  _wt_cfg_canon /r $'core.hookspath\n.git/hooks' out
  [ "$out" = $'core.hookspath\n/r/hooks' ]
  _wt_cfg_canon "" $'core.hookspath\n.git/hooks' out
  [ "$out" = $'core.hookspath\n.git/hooks' ]
  _wt_cfg_canon /r $'core.fsmonitor\n.git/hooks' out
  [ "$out" = $'core.fsmonitor\n.git/hooks' ]
  for v in .git/hooks/ ./.git/hooks .git//hooks .git/x .husky .git/hooks/../x /x/hooks; do
    _wt_cfg_canon /r "core.hookspath"$'\n'"$v" out
    [ "$out" = "core.hookspath"$'\n'"$v" ]
  done
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
  [[ $stderr == *"no git-config baseline at $BASELINE — the next dispatch records it"* ]]
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

@test "_wt_git never runs a baselined worktree-relative textconv the worker rewrote (#578)" {
  commit_conv_driver 'a diff=myd
'
  add_worktree w
  git config diff.myd.textconv tools/conv.sh
  _wt_cfg_baseline_init "$COMMON"
  rewrite_conv_and_commit
  run --separate-stderr _wt_cfg_guard "$COMMON" "$ADMIN"
  [ "$status" -eq 0 ]
  echo b >"$WT/a"
  rm -f "$SENTINEL"
  run --separate-stderr _wt_git "$ADMIN" "$WT" diff
  [ "$status" -eq 0 ]
  [ ! -e "$SENTINEL" ]
  # Control: plain git in the worktree does run the rewritten driver.
  git -C "$WT" diff >/dev/null
  [ -e "$SENTINEL" ]
}

@test "_wt_git never runs a baselined worktree-relative filter the worker rewrote (#578)" {
  commit_conv_driver 'a filter=myf
'
  add_worktree w
  git config filter.myf.clean tools/conv.sh
  git config filter.myf.smudge tools/conv.sh
  _wt_cfg_baseline_init "$COMMON"
  rewrite_conv_and_commit
  # Same-size edit: stat-dirty, forces git to re-hash (and would run clean).
  printf b >"$WT/a"
  rm -f "$SENTINEL"
  run --separate-stderr _wt_status "$ADMIN" "$WT"
  [ "$status" -eq 0 ]
  rm -f "$SENTINEL"
  run --separate-stderr _wt_git "$ADMIN" "$WT" reset -q --hard HEAD
  [ "$status" -eq 0 ]
  [ ! -e "$SENTINEL" ]
  # Control: the same edit against plain git does run the rewritten filter.
  printf b >"$WT/a"
  git -C "$WT" reset -q --hard HEAD
  [ -e "$SENTINEL" ]
}

@test "_wt_git ignores a worker-planted core.attributesFile (#578)" {
  commit_conv_driver ''
  add_worktree w
  git config diff.myd.textconv tools/conv.sh
  _wt_cfg_baseline_init "$COMMON"
  rewrite_conv_and_commit
  printf 'a diff=myd\n' >"$WT/.evil"
  git -C "$WT" config core.attributesFile "$WT/.evil"
  run --separate-stderr _wt_cfg_guard "$COMMON" "$ADMIN"
  [ "$status" -eq 0 ]
  echo b >"$WT/a"
  rm -f "$SENTINEL"
  run --separate-stderr _wt_git "$ADMIN" "$WT" diff
  [ "$status" -eq 0 ]
  [ ! -e "$SENTINEL" ]
  # Control: plain git honors the planted attributesFile and runs the driver.
  git -C "$WT" diff >/dev/null
  [ -e "$SENTINEL" ]
}

@test "_wt_git refuses when the empty tree cannot be computed (#578)" {
  add_worktree w
  _wt_cfg_baseline_init "$COMMON"
  _wt_empty_tree() { return 1; }
  run --separate-stderr _wt_git "$ADMIN" "$WT" status --porcelain
  [ "$status" -eq 1 ]
  [[ $stderr == *"empty tree"* ]]
}
