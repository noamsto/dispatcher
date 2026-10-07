setup() {
  load helpers
  RESUME="$BATS_TEST_DIRNAME/../adapters/core/dispatch-resume.sh"
  export CREW_REAL="$BATS_TEST_DIRNAME/../adapters/core/crew.sh"
  run_resume() { bash -euo pipefail "$RESUME" "$@"; }
  setup_repo
  export HOME="$TEST_REPO"
  unset DISPATCH_PROFILE CREW_ID TMUX_PANE CLAUDE_CONFIG_DIR
  stub_tmux_no_pane
  stub_bin crew
  # engine-cmd needs real matching (mirrors crew.sh's own _is_engine_cmd,
  # #111): everything else in dispatch-resume.sh only cares that the call was
  # made, so it keeps the generic log-and-succeed behaviour stub_bin gave it.
  cat >"$STUB_DIR/crew" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
if [ "$1" = pi-agent-dir ]; then exec bash -euo pipefail "$CREW_REAL" pi-agent-dir; fi
if [ "$1" = resolve-target ]; then exec bash -euo pipefail "$CREW_REAL" "$@"; fi
if [ "$1" = where ]; then
  [ -z "${STUB_WHERE_LIVE:-}" ] || { echo "iris — main:3.1 (lead pane)   jump: ! tmux switch-client -t %9"; exit 0; }
  exit 1
fi
if [ "$1" = engine-cmd ]; then
  c="${2#.}"
  c="${c%-wrapped}"
  case "$c" in
  claude | codex | cursor-agent | node | pi) exit 0 ;;
  esac
  exit 1
fi
exit 0
EOF
  chmod +x "$STUB_DIR/crew"
  stub_bin gh
  cat >"$STUB_DIR/dispatch" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
[ -n "${DISPATCH_PRECHECK:-}" ] || exit 0
[ -z "${STUB_PRECHECK_FAIL:-}" ] || {
  echo "dispatch: --effort ultra is codex-only; claude tops out at max" >&2
  exit 1
}
exit 0
EOF
  chmod +x "$STUB_DIR/dispatch"
  export DISPATCHER_PROTOCOL_DIR="$TEST_REPO/protocols"
  mkdir -p "$DISPATCHER_PROTOCOL_DIR"
  touch "$DISPATCHER_PROTOCOL_DIR"/{WORKER_PROTOCOL.md,WORKER_PROTOCOL.claude.md,EVIDENCE_REVIEW.md,GRID_PROTOCOL.md,REVIEW_TASK.md}
  # Stands in for the store path flake.nix bakes as @skillsDir@ (#225).
  export DISPATCHER_SKILLS_DIR="$TEST_REPO/harness-skills"
  mkdir -p "$DISPATCHER_SKILLS_DIR/spec-plan-critic"
  printf -- '---\nname: spec-plan-critic\ndescription: seeded\n---\n' \
    >"$DISPATCHER_SKILLS_DIR/spec-plan-critic/SKILL.md"
  # The cross-repo lane hint is a sourced shared lib; raw runs point the
  # override at the repo copy (flake.nix bakes the store path for builds).
  export CROSS_REPO_HINT_LIB="$BATS_TEST_DIRNAME/../adapters/core/cross-repo-hint.sh"
  export CLAUDE_WORKER_SETTINGS_LIB="$BATS_TEST_DIRNAME/../adapters/core/claude-worker-settings.sh"
  git commit --allow-empty -qm init
}

teardown() { teardown_repo; }

# wait_for_log <pattern> — poll $STUB_LOG for a line written by a backgrounded
# stub (the nohup'd stall-watch). Fails the test after ~2s.
wait_for_log() {
  local i
  for i in $(seq 1 40); do
    grep -qE "$1" "$STUB_LOG" && return 0
    sleep 0.05
  done
  echo "wait_for_log: never saw '$1' in $STUB_LOG" >&2
  cat "$STUB_LOG" >&2
  return 1
}

# Default tmux stub: no pane sits at the worktree, so placement resolution
# takes the create-a-window path. Individual tests override $STUB_DIR/tmux
# when they need the reuse path instead.
stub_tmux_no_pane() {
  stub_bin tmux
  cat >"$STUB_DIR/tmux" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$1" in
list-panes) : ;;
new-window) printf '%s %s\n' '%99' '%99' ;;
display-message) printf '%s\n' '80 24 on' ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/tmux"
}

# anchor_path_for <wt> — the dispatcher-owned anchor path for <wt>, as
# dispatch-resume.sh's _worktree_anchor_path computes it.
anchor_path_for() {
  local key
  key="$(printf %s "$(realpath -e "$1")" | sha256sum | cut -c1-64)"
  printf '%s/crew/worktrees/%s\n' "$XDG_DATA_HOME" "$key"
}

# write_anchor <wt> [crew_dir] [branch] [gitdir] — write the dispatcher-owned
# anchor entry that dispatch-resume.sh's #518 check reads, keyed by <wt>'s own
# realpath. Defaults mirror a genuine worktree of $TEST_REPO.
write_anchor() {
  local wt="$1" cdir="${2:-}" br="${3:-}" gdir="${4:-}" path
  [ -n "$cdir" ] || cdir="$(realpath -m "$TEST_REPO/.git/crew")"
  [ -n "$br" ] || br="$(git -C "$wt" branch --show-current)"
  [ -n "$gdir" ] || gdir="$(realpath -e "$(git -C "$wt" rev-parse --absolute-git-dir)")"
  path="$(anchor_path_for "$wt")"
  mkdir -p "$(dirname "$path")"
  printf '%s\n%s\n%s\n%s\n' "$(realpath -e "$wt")" "$cdir" "$br" "$gdir" >"$path"
}

# A worktree that looks like a live worker's: its own branch, its own
# directory, and a task document with a full header.
setup_worker_wt() { # [extra header lines...]
  git -C "$TEST_REPO" worktree add -q -b feat/7-a-thing "$TEST_REPO/wt" HEAD
  WT="$TEST_REPO/wt"
  {
    printf 'tier: standard\nkind: implement\ndraft: false\n'
    printf 'engine: claude\nmodel: sonnet\neffort: medium\nmcp: \n'
    printf 'plan: required\ntitle: a thing\nCloses #7\n'
    printf 'dispatcher_pane: %%3\ncrew_dir: %s/.git/crew\ncrew_id: c1\n' "$TEST_REPO"
    printf 'agent_name: iris\nworker_id: worker:feat/7-a-thing#s1-99\n'
    for extra in "$@"; do printf '%s\n' "$extra"; done
    printf '\n## Task\n\nthe original body\n'
  } >"$WT/WORKER_TASK.md"
  export WT
  write_anchor "$WT"
  seed_git_baseline
}

# #817: `dispatch resume --help` is a grouped screen, not the one-line synopsis.
# The flag list comes from this file's own parse arms, and each flag has to own
# a line of the help — anchored, because the synopsis repeats every flag.
@test "resume --help and -h list every flag it parses and exit 0 (#817)" {
  local form flag missing
  for form in --help -h; do
    run run_resume "$form"
    [ "$status" -eq 0 ] || { echo "$form: status $status"; return 1; }
    [[ "$output" == *"usage: dispatch resume"* ]] || { echo "$form: no synopsis"; return 1; }
    missing=()
    while IFS= read -r flag; do
      grep -Eq "^  ${flag}([ =]|\$)" <<<"$output" || missing+=("$flag")
    done < <(grep -E '^  --[a-z-]+\)' "$RESUME" | sed -E 's/^  (--[a-z-]+)\).*/\1/' | sort -u)
    [ "${#missing[@]}" -eq 0 ] || { echo "$form missing: ${missing[*]}"; return 1; }
  done
}

@test "resume --help lists the target forms and an example (#817)" {
  run run_resume --help
  [ "$status" -eq 0 ]
  local need
  for need in 'worker:<branch>#<session>' 'a codename' 'a branch' 'dispatch resume' 'dispatch-wt'; do
    grep -Fq -- "$need" <<<"$output" || { echo "help missing: $need"; return 1; }
  done
}

# The help path must stay above every lookup a broken environment can fail — a
# bare directory, no crew id, no settings file.
@test "resume --help works with no repo, crew id or settings file (#817)" {
  local sandbox
  sandbox="$BATS_TEST_TMPDIR/norepo"
  mkdir -p "$sandbox"
  (
    cd "$sandbox" || exit 1
    env -u CREW_ID -u CREW_WORKER_ID -u CREW_REAL \
      HOME="$sandbox/home" XDG_CONFIG_HOME="$sandbox/config" XDG_DATA_HOME="$sandbox/data" \
      bash -euo pipefail "$(realpath "$RESUME")" --help
  ) >"$sandbox/out" 2>"$sandbox/err"
  [ -s "$sandbox/out" ]
  grep -q '^usage: dispatch resume' "$sandbox/out"
  if [ -s "$sandbox/err" ]; then
    echo "stderr: $(cat "$sandbox/err")"
    return 1
  fi
}

@test "refuses outside a worktree carrying a task document" {
  run run_resume
  [ "$status" -eq 1 ]
  [[ "$output" == *"no WORKER_TASK.md"* ]]
  [[ "$output" == *"dispatch <tier> <model>"* ]]
}

@test "refuses in the primary worktree even with a task document" {
  printf 'engine: claude\nmodel: sonnet\neffort: medium\ncrew_id: c1\n' >"$TEST_REPO/WORKER_TASK.md"
  run run_resume
  [ "$status" -eq 1 ]
  [[ "$output" == *"primary worktree"* ]]
}

@test "warns when WORKER_TASK.md is tracked on the branch" {
  setup_worker_wt
  git -C "$WT" add -f WORKER_TASK.md
  git -C "$WT" commit -qm 'a worker tracked its task doc'
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  [[ "$output" == *"WORKER_TASK.md is tracked on this branch"* ]]
  [[ "$output" == *"git rm --cached WORKER_TASK.md"* ]]
}

@test "warns when WORKER_TASK.md is tracked, even from a subdirectory" {
  setup_worker_wt
  git -C "$WT" add -f WORKER_TASK.md
  git -C "$WT" commit -qm 'a worker tracked its task doc'
  mkdir -p "$WT/sub"
  cd "$WT/sub"
  run run_resume
  [ "$status" -eq 0 ]
  [[ "$output" == *"WORKER_TASK.md is tracked on this branch"* ]]
}

@test "does not warn when WORKER_TASK.md is untracked on the branch" {
  setup_worker_wt
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  [[ "$output" != *"WORKER_TASK.md is tracked"* ]]
}

@test "resume excludes WORKER_TASK.md in the anchored common dir, not a worker-swapped .git (#633)" {
  setup_worker_wt
  git init -q "$BATS_TEST_TMPDIR/fake"
  # `crew identity` runs after the record check and before the exclude append;
  # the stub swaps the worktree's gitfile for a standalone repo in that window.
  cat >"$STUB_DIR/crew" <<EOF
#!/usr/bin/env bash
printf '%s\n' "\$*" >>"\$STUB_LOG"
if [ "\$1" = identity ] && [ ! -e "$BATS_TEST_TMPDIR/swapped" ]; then
  : >"$BATS_TEST_TMPDIR/swapped"
  rm "$WT/.git"
  mv "$BATS_TEST_TMPDIR/fake/.git" "$WT/.git"
fi
exit 0
EOF
  cd "$WT"
  run run_resume
  [ -e "$BATS_TEST_TMPDIR/swapped" ]
  [ "$status" -eq 0 ]
  [ "$(grep -cxF WORKER_TASK.md "$TEST_REPO/.git/info/exclude")" -eq 1 ]
  ! grep -qxF WORKER_TASK.md "$WT/.git/info/exclude"
}

@test "resume refuses a worktree whose admin dir has a config.worktree (#539)" {
  # #539: per-worktree config lives in the admin dir and is still read under
  # an anchored gitdir; it can carry keys no -c list enumerates, so resume
  # must refuse outright rather than try to filter it.
  setup_worker_wt
  cat >"$BATS_TEST_TMPDIR/hit.sh" <<EOF
#!/usr/bin/env bash
touch "$BATS_TEST_TMPDIR/SENTINEL"
EOF
  chmod +x "$BATS_TEST_TMPDIR/hit.sh"
  git -C "$TEST_REPO" config extensions.worktreeConfig true
  git -C "$WT" config --worktree core.fsmonitor "$BATS_TEST_TMPDIR/hit.sh"
  cd "$WT"
  run run_resume
  [ "$status" -eq 1 ]
  [[ "$output" == *"config.worktree"* ]]
  [ ! -e "$BATS_TEST_TMPDIR/SENTINEL" ]
}

@test "resume guards the worktree's config.worktree at entry (#585)" {
  setup_worker_wt
  git -C "$TEST_REPO" config extensions.worktreeConfig true
  seed_git_baseline
  git -C "$WT" config --worktree core.fsmonitor /evil
  before="$(cksum "$WT/WORKER_TASK.md")"
  cd "$WT"
  run run_resume
  [ "$status" -eq 1 ]
  [[ "$output" == *core.fsmonitor* ]]
  [[ "$output" != *"could not read the index"* ]]
  [ "$(cksum "$WT/WORKER_TASK.md")" = "$before" ]
}

@test "resume accepts a relative .git/hooks against an absolute baseline from its linked worktree (#638)" {
  setup_worker_wt
  git -C "$TEST_REPO" config core.hooksPath "$(realpath -e "$TEST_REPO/.git")/hooks"
  seed_git_baseline
  git -C "$TEST_REPO" config core.hooksPath .git/hooks
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  [[ "$output" != *"refusing git"* ]]
}

@test "resume still refuses a drifted key under a relative .git/hooks (#638)" {
  setup_worker_wt
  git -C "$TEST_REPO" config core.hooksPath "$(realpath -e "$TEST_REPO/.git")/hooks"
  seed_git_baseline
  git -C "$TEST_REPO" config core.hooksPath .git/hooks
  git -C "$TEST_REPO" config core.fsmonitor /evil
  cd "$WT"
  run run_resume
  [ "$status" -eq 1 ]
  [[ "$output" == *core.fsmonitor* ]]
  run ! grep -qi 'refusing git: core.hookspath' <<<"$output"
}

@test "resume refuses on a worker-planted include (#557)" {
  # #557: a worker's `git config include.path <file>` in the worktree writes
  # the COMMON config that dispatcher-run git (here, resume's own ls-files
  # and status calls) reads; a key planted after the baseline was seeded
  # must refuse resume outright.
  setup_worker_wt
  cat >"$BATS_TEST_TMPDIR/hit.sh" <<EOF
#!/usr/bin/env bash
touch "$BATS_TEST_TMPDIR/SENTINEL"
EOF
  chmod +x "$BATS_TEST_TMPDIR/hit.sh"
  cat >"$BATS_TEST_TMPDIR/include.gitconfig" <<EOF
[filter "x"]
	smudge = $BATS_TEST_TMPDIR/hit.sh
EOF
  git -C "$TEST_REPO" config include.path "$BATS_TEST_TMPDIR/include.gitconfig"
  cd "$WT"
  run run_resume
  [ "$status" -eq 1 ]
  [[ "$output" == *"include.path"* ]]
}

@test "resume refuses without a git-config baseline and never records one (#557)" {
  # #557: only a dispatch records the baseline. Resume runs later, once
  # workers exist, so a baseline it recorded could trust a worker's key.
  setup_worker_wt
  rm "$TEST_REPO/.git/crew/git-config-baseline"
  cd "$WT"
  run run_resume
  [ "$status" -eq 1 ]
  [[ "$output" == *"no git-config baseline"* ]]
  [ ! -e "$TEST_REPO/.git/crew/git-config-baseline" ]
}

@test "refuses on a detached HEAD" {
  setup_worker_wt
  git -C "$WT" checkout -q --detach
  cd "$WT"
  run run_resume
  [ "$status" -eq 1 ]
  [[ "$output" == *"detached HEAD"* ]]
}

@test "resume passes the anchor check when a tag shares the branch name (#688)" {
  setup_worker_wt
  git -C "$TEST_REPO" tag feat/7-a-thing
  stub_tmux_with_pane_at_wt '@4' '%8' '' fish
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  grep -q 'send-keys' "$STUB_LOG"
}

# _assert_refused_before_discovery <status var already run> — no precheck, no
# window/pane action happened: the anchor check must run before all of them.
_assert_refused_before_discovery() {
  [ "$status" -eq 1 ]
  [ -f "$STUB_LOG" ] || return 0
  if grep -qE '^standard sonnet |new-window|set-window-option|send-keys' "$STUB_LOG"; then
    return 1
  fi
}

@test "refuses resume with no dispatcher anchor for the worktree (#518)" {
  setup_worker_wt
  rm -f "$(anchor_path_for "$WT")"
  cd "$WT"
  run run_resume
  _assert_refused_before_discovery
  [[ "$output" == *"no dispatcher record for $WT"* ]]
  [[ "$output" == *"re-dispatch the task"* ]]
}

@test "refuses when the gitlink points at a worker-built admin dir (crew dir mismatch, #518)" {
  setup_worker_wt
  # A fake repo with the same branch name, so HEAD still resolves — the point
  # under test is a crew-dir mismatch, not a broken HEAD.
  git init -q -b main "$BATS_TEST_TMPDIR/fake"
  git -C "$BATS_TEST_TMPDIR/fake" config user.email test@example.com
  git -C "$BATS_TEST_TMPDIR/fake" config user.name test
  git -C "$BATS_TEST_TMPDIR/fake" commit -q --allow-empty -m init
  git -C "$BATS_TEST_TMPDIR/fake" branch feat/7-a-thing
  admin="$(git -C "$WT" rev-parse --absolute-git-dir)"
  cp -r "$admin" "$BATS_TEST_TMPDIR/fake-admin"
  realpath -e "$BATS_TEST_TMPDIR/fake/.git" >"$BATS_TEST_TMPDIR/fake-admin/commondir"
  printf 'gitdir: %s\n' "$BATS_TEST_TMPDIR/fake-admin" >"$WT/.git"
  cd "$WT"
  run run_resume
  _assert_refused_before_discovery
  [[ "$output" == *"this worktree's crew dir"* ]]
  [[ "$output" == *"does not match the dispatcher's record"* ]]
}

@test "refuses when the gitlink points at another genuine worktree's admin dir (#518)" {
  setup_worker_wt
  git -C "$TEST_REPO" worktree add -q -b feat/8-other "$TEST_REPO/wt2" HEAD
  admin2="$(git -C "$TEST_REPO/wt2" rev-parse --absolute-git-dir)"
  printf 'gitdir: %s\n' "$admin2" >"$WT/.git"
  cd "$WT"
  run run_resume
  _assert_refused_before_discovery
  [[ "$output" == *"does not match the dispatcher's record"* ]]
  [[ "$output" == *"this worktree's branch"* || "$output" == *"this worktree's git dir"* ]]
}

@test "refuses when HEAD is switched to another branch (#518)" {
  setup_worker_wt
  git -C "$TEST_REPO" branch other-branch
  git -C "$WT" symbolic-ref HEAD refs/heads/other-branch
  cd "$WT"
  run run_resume
  _assert_refused_before_discovery
  [[ "$output" == *"this worktree's branch (other-branch)"* ]]
  [[ "$output" == *"does not match the dispatcher's record (feat/7-a-thing)"* ]]
}

@test "refuses when the anchor file is a symlink (#518)" {
  setup_worker_wt
  path="$(anchor_path_for "$WT")"
  mv "$path" "$BATS_TEST_TMPDIR/anchor-real"
  ln -s "$BATS_TEST_TMPDIR/anchor-real" "$path"
  cd "$WT"
  run run_resume
  _assert_refused_before_discovery
  [[ "$output" == *"no dispatcher record for $WT"* ]]
}

@test "refuses when the header lacks the launch tuple" {
  setup_worker_wt
  printf 'tier: standard\ncrew_id: c1\n' >"$WT/WORKER_TASK.md"
  cd "$WT"
  run run_resume
  [ "$status" -eq 1 ]
  [[ "$output" == *"header is missing"* ]]
  [[ "$output" == *"engine"* ]]
}

@test "aborts before scaffolding when a required protocol file is missing" {
  setup_worker_wt
  cd "$WT"
  export DISPATCHER_PROTOCOL_DIR="$TEST_REPO/protocols-incomplete"
  mkdir -p "$DISPATCHER_PROTOCOL_DIR"
  touch "$DISPATCHER_PROTOCOL_DIR/WORKER_PROTOCOL.md"
  run run_resume
  [ "$status" -eq 1 ]
  [[ "$output" == *"EVIDENCE_REVIEW.md"* ]]
  [[ "$output" == *"$DISPATCHER_PROTOCOL_DIR"* ]]
  if [ -f "$STUB_LOG" ]; then run grep -q 'new-window' "$STUB_LOG"; [ "$status" -ne 0 ]; fi

  # Only a claude lead appends the claude render, so only it requires that file.
  touch "$DISPATCHER_PROTOCOL_DIR/EVIDENCE_REVIEW.md"
  run run_resume
  [ "$status" -eq 1 ]
  [[ "$output" == *"WORKER_PROTOCOL.claude.md"* ]]
  [[ "$output" == *"$DISPATCHER_PROTOCOL_DIR"* ]]
  if [ -f "$STUB_LOG" ]; then run grep -q 'send-keys' "$STUB_LOG"; [ "$status" -ne 0 ]; fi
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  sed -i -e "s/^engine: .*/engine: codex/" -e "s|^model: .*|model: gpt-5.6-terra|" -e "s/^effort: .*/effort: medium/" "$WT/WORKER_TASK.md"
  DISPATCH_PROFILE=work run run_resume
  [ "$status" -eq 0 ]
  grep -q 'send-keys' "$STUB_LOG"
}

@test "refuses a protocol dir whose content hashes to a different revision than the script marker" {
  setup_worker_wt
  cd "$WT"
  # Baked-marker simulation, mirroring flake.nix's replaceStrings (see
  # _substituted_dispatch in dispatch.bats).
  sed 's/@protocolRev@/0123456789abcdef/' "$RESUME" >"$BATS_TEST_TMPDIR/resume-subst.sh"
  export DISPATCHER_PROTOCOL_DIR="$TEST_REPO/protocols-mismatch"
  mkdir -p "$DISPATCHER_PROTOCOL_DIR"
  touch "$DISPATCHER_PROTOCOL_DIR/WORKER_PROTOCOL.md" "$DISPATCHER_PROTOCOL_DIR/WORKER_PROTOCOL.claude.md" "$DISPATCHER_PROTOCOL_DIR/EVIDENCE_REVIEW.md"
  rev_dir="$(_protocol_dir_rev "$DISPATCHER_PROTOCOL_DIR")"
  run bash -euo pipefail "$BATS_TEST_TMPDIR/resume-subst.sh"
  [ "$status" -eq 1 ]
  [[ "$output" == *"protocol directory version mismatch"* ]]
  [[ "$output" == *"0123456789abcdef"* ]]
  [[ "$output" == *"$rev_dir"* ]]
  [[ "$output" == *"$DISPATCHER_PROTOCOL_DIR"* ]]
  if [ -f "$STUB_LOG" ]; then run grep -q 'new-window' "$STUB_LOG"; [ "$status" -ne 0 ]; fi
}

@test "resume proceeds when the protocol dir hashes to the script marker" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' '' fish
  cd "$WT"
  sed 's/@protocolRev@/0123456789abcdef/' "$RESUME" >"$BATS_TEST_TMPDIR/resume-subst.sh"
  export DISPATCHER_PROTOCOL_DIR="$TEST_REPO/protocols-matching"
  mkdir -p "$DISPATCHER_PROTOCOL_DIR"
  touch "$DISPATCHER_PROTOCOL_DIR/WORKER_PROTOCOL.md" "$DISPATCHER_PROTOCOL_DIR/WORKER_PROTOCOL.claude.md" "$DISPATCHER_PROTOCOL_DIR/EVIDENCE_REVIEW.md"
  rev="$(_protocol_dir_rev "$DISPATCHER_PROTOCOL_DIR")"
  sed "s/@protocolRev@/$rev/" "$RESUME" >"$BATS_TEST_TMPDIR/resume-subst.sh"
  run bash -euo pipefail "$BATS_TEST_TMPDIR/resume-subst.sh"
  [ "$status" -eq 0 ]
  grep -q 'send-keys' "$STUB_LOG"
}

# #303: a fake Nix store under $TEST_REPO/store; the scratch copy of the script
# bakes the "new" build's projected dirs the way flake.nix's replaceStrings
# does, and the setup() overrides are cleared so the baked default is in play.
_store_resume() {
  STORE="$TEST_REPO/store"
  BAKED_PROTOCOLS="$STORE/h-new-protocols"
  BAKED_SKILLS="$STORE/h-new-skills"
  BAKED_REVIEWERS="$STORE/h-new-reviewers"
  BAKED_CRITICS="$STORE/h-new-critics"
  BAKED_HINT="$STORE/h-new-cross-repo-hint.sh"
  mkdir -p "$BAKED_REVIEWERS" "$BAKED_CRITICS"
  cp "$CROSS_REPO_HINT_LIB" "$BAKED_HINT"
  printf 'new\n' >"$BAKED_REVIEWERS/r.md"
  printf 'new\n' >"$BAKED_CRITICS/c.md"
  _store_protocols "$BAKED_PROTOCOLS" new
  mkdir -p "$BAKED_SKILLS"
  sed "s|@protocolDir@|$BAKED_PROTOCOLS|; s|@protocolRev@|$(_protocol_dir_rev "$BAKED_PROTOCOLS")|; s|@skillsDir@|$BAKED_SKILLS|; s|@reviewersDir@|$BAKED_REVIEWERS|; s|@criticsDir@|$BAKED_CRITICS|; s|@crossRepoHintLib@|$BAKED_HINT|" "$RESUME" >"$BATS_TEST_TMPDIR/resume-store.sh"
  unset DISPATCHER_PROTOCOL_DIR DISPATCHER_SKILLS_DIR DISPATCHER_REVIEWERS_DIR DISPATCHER_CRITICS_DIR CROSS_REPO_HINT_LIB
}

_store_protocols() { # <dir> <content>
  local f
  mkdir -p "$1"
  for f in WORKER_PROTOCOL.md WORKER_PROTOCOL.claude.md EVIDENCE_REVIEW.md GRID_PROTOCOL.md REVIEW_TASK.md; do
    printf '%s %s\n' "$2" "$f" >"$1/$f"
  done
}

@test "resume ignores a stale store-path DISPATCHER_PROTOCOL_DIR with a notice and stamps the baked dir" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  _store_resume
  export DISPATCHER_PROTOCOL_DIR="$STORE/h-old-source/adapters/core/protocols"
  _store_protocols "$DISPATCHER_PROTOCOL_DIR" old
  run bash -euo pipefail "$BATS_TEST_TMPDIR/resume-store.sh"
  [ "$status" -eq 0 ]
  [[ "$output" == *"dispatch resume: ignoring stale DISPATCHER_PROTOCOL_DIR"* ]]
  grep -qx "protocol_dir: $BAKED_PROTOCOLS" "$WT/WORKER_TASK.md"
  grep -q -- "--append-system-prompt-file $BAKED_PROTOCOLS/WORKER_PROTOCOL.claude.md" <(launch_log)
  grep -q 'send-keys' "$STUB_LOG"
}

@test "resume refuses a relative DISPATCHER_*_DIR override" {
  setup_worker_wt
  cd "$WT"
  _store_resume
  export DISPATCHER_CRITICS_DIR="critics"
  run bash -euo pipefail "$BATS_TEST_TMPDIR/resume-store.sh"
  [ "$status" -ne 0 ]
  [[ "$output" == *"DISPATCHER_CRITICS_DIR must be an absolute path, got: critics"* ]]
}

@test "resume refuses a DISPATCHER_*_DIR override with shell metacharacters (#470)" {
  setup_worker_wt
  cd "$WT"
  _store_resume
  export DISPATCHER_CRITICS_DIR="$BATS_TEST_TMPDIR/c'; touch pwned; '"
  run bash -euo pipefail "$BATS_TEST_TMPDIR/resume-store.sh"
  [ "$status" -ne 0 ]
  [[ "$output" == *"DISPATCHER_CRITICS_DIR must not contain shell metacharacters"* ]]
}

@test "resume ignores stale reviewers/critics dirs and the launch env carries all four resolved dirs" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  _store_resume
  export DISPATCHER_REVIEWERS_DIR="$STORE/h-old-source/adapters/core/reviewers"
  export DISPATCHER_CRITICS_DIR="$STORE/h-old-source/adapters/core/critics"
  mkdir -p "$DISPATCHER_REVIEWERS_DIR" "$DISPATCHER_CRITICS_DIR"
  printf 'old\n' >"$DISPATCHER_REVIEWERS_DIR/r.md"
  printf 'old\n' >"$DISPATCHER_CRITICS_DIR/c.md"
  run bash -euo pipefail "$BATS_TEST_TMPDIR/resume-store.sh"
  [ "$status" -eq 0 ]
  [[ "$output" == *"dispatch resume: ignoring stale DISPATCHER_REVIEWERS_DIR"* ]]
  [[ "$output" == *"dispatch resume: ignoring stale DISPATCHER_CRITICS_DIR"* ]]
  grep -qF -- "DISPATCHER_PROTOCOL_DIR=$BAKED_PROTOCOLS DISPATCHER_SKILLS_DIR=$BAKED_SKILLS DISPATCHER_REVIEWERS_DIR=$BAKED_REVIEWERS DISPATCHER_CRITICS_DIR=$BAKED_CRITICS DISPATCH_GRANT_ROOTS=: GIT_EDITOR=true" <(launch_log)
  run grep -qF -- "h-old-source" <(launch_log)
  [ "$status" -ne 0 ]
}

@test "resume keeps a store-path DISPATCHER_PROTOCOL_DIR whose content matches the baked dir, silently" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  _store_resume
  export DISPATCHER_PROTOCOL_DIR="$STORE/h-cur-source/adapters/core/protocols"
  _store_protocols "$DISPATCHER_PROTOCOL_DIR" new
  run bash -euo pipefail "$BATS_TEST_TMPDIR/resume-store.sh"
  [ "$status" -eq 0 ]
  [[ "$output" != *"ignoring stale"* ]]
  grep -qx "protocol_dir: $DISPATCHER_PROTOCOL_DIR" "$WT/WORKER_TASK.md"
}

@test "resume still refuses a drifted non-store override and names the remedy" {
  setup_worker_wt
  cd "$WT"
  _store_resume
  export DISPATCHER_PROTOCOL_DIR="$TEST_REPO/checkout/protocols"
  _store_protocols "$DISPATCHER_PROTOCOL_DIR" drifted
  run bash -euo pipefail "$BATS_TEST_TMPDIR/resume-store.sh"
  [ "$status" -eq 1 ]
  [[ "$output" != *"ignoring stale"* ]]
  [[ "$output" == *"protocol directory version mismatch"* ]]
  [[ "$output" == *"unset DISPATCHER_PROTOCOL_DIR, or point it at a checkout matching this build"* ]]
  if [ -f "$STUB_LOG" ]; then run grep -q 'new-window' "$STUB_LOG"; [ "$status" -ne 0 ]; fi
}

@test "a raw (unsubstituted) resume script skips the revision check with a one-line warning" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' '' fish
  cd "$WT"
  export DISPATCHER_PROTOCOL_DIR="$TEST_REPO/protocols-no-rev"
  mkdir -p "$DISPATCHER_PROTOCOL_DIR"
  touch "$DISPATCHER_PROTOCOL_DIR/WORKER_PROTOCOL.md" "$DISPATCHER_PROTOCOL_DIR/WORKER_PROTOCOL.claude.md" "$DISPATCHER_PROTOCOL_DIR/EVIDENCE_REVIEW.md"
  run run_resume
  [ "$status" -eq 0 ]
  [[ "$output" == *"unsubstituted protocol revision"* ]]
  grep -q 'send-keys' "$STUB_LOG"
}

# #253: same guard, resume side. The guard runs at dispatch-resume.sh:242-243,
# before the engine precheck at :415, so it is engine-independent; parametrize
# over the engines the issue names as untested (codex, cursor, pi). The header
# rewrite uses anchored wildcards so every iteration actually changes the
# engine rather than no-opping after the first.
@test "resume aborts on a missing protocol file for codex, cursor and pi" {
  setup_worker_wt
  cd "$WT"
  export DISPATCHER_PROTOCOL_DIR="$TEST_REPO/protocols-incomplete"
  mkdir -p "$DISPATCHER_PROTOCOL_DIR"
  touch "$DISPATCHER_PROTOCOL_DIR/WORKER_PROTOCOL.md"
  n=0
  while IFS='|' read -r eng model effort profile _bin _marker; do
    n=$((n + 1))
    sed -i -e "s/^engine: .*/engine: $eng/" -e "s|^model: .*|model: $model|" -e "s/^effort: .*/effort: $effort/" "$WT/WORKER_TASK.md"
    DISPATCH_PROFILE="$profile" run run_resume
    [ "$status" -eq 1 ]
    [[ "$output" == *"EVIDENCE_REVIEW.md"* ]]
    [[ "$output" == *"$DISPATCHER_PROTOCOL_DIR"* ]]
    if [ -f "$STUB_LOG" ]; then run grep -q 'new-window' "$STUB_LOG"; [ "$status" -ne 0 ]; fi
    if [ -f "$STUB_LOG" ]; then run grep -q 'send-keys' "$STUB_LOG"; [ "$status" -ne 0 ]; fi
  done < <(protocol_engine_specs codex cursor pi)
  # A zero-iteration loop would pass vacuously; a mistyped/renamed filter must fail.
  [ "$n" -eq 3 ]
}

@test "resume refuses a stale protocol dir for codex, cursor and pi" {
  setup_worker_wt
  cd "$WT"
  sed 's/@protocolRev@/0123456789abcdef/' "$RESUME" >"$BATS_TEST_TMPDIR/resume-subst.sh"
  export DISPATCHER_PROTOCOL_DIR="$TEST_REPO/protocols-mismatch"
  mkdir -p "$DISPATCHER_PROTOCOL_DIR"
  touch "$DISPATCHER_PROTOCOL_DIR/WORKER_PROTOCOL.md" "$DISPATCHER_PROTOCOL_DIR/WORKER_PROTOCOL.claude.md" "$DISPATCHER_PROTOCOL_DIR/EVIDENCE_REVIEW.md"
  rev_dir="$(_protocol_dir_rev "$DISPATCHER_PROTOCOL_DIR")"
  n=0
  while IFS='|' read -r eng model effort profile _bin _marker; do
    n=$((n + 1))
    sed -i -e "s/^engine: .*/engine: $eng/" -e "s|^model: .*|model: $model|" -e "s/^effort: .*/effort: $effort/" "$WT/WORKER_TASK.md"
    DISPATCH_PROFILE="$profile" run bash -euo pipefail "$BATS_TEST_TMPDIR/resume-subst.sh"
    [ "$status" -eq 1 ]
    [[ "$output" == *"protocol directory version mismatch"* ]]
    [[ "$output" == *"0123456789abcdef"* ]]
    [[ "$output" == *"$rev_dir"* ]]
    [[ "$output" == *"$DISPATCHER_PROTOCOL_DIR"* ]]
    if [ -f "$STUB_LOG" ]; then run grep -q 'new-window' "$STUB_LOG"; [ "$status" -ne 0 ]; fi
    if [ -f "$STUB_LOG" ]; then run grep -q 'send-keys' "$STUB_LOG"; [ "$status" -ne 0 ]; fi
  done < <(protocol_engine_specs codex cursor pi)
  # A zero-iteration loop would pass vacuously; a mistyped/renamed filter must fail.
  [ "$n" -eq 3 ]
}

# Happy path: the resume launch prompt names the protocol dir. codex/cursor
# carry it in the prompt string; pi carries it in --append-system-prompt. All
# three also carry the protocol_note in the prompt. Assert each engine's actual
# carrier rather than assuming symmetry.
@test "resume names the protocol dir in the codex, cursor and pi launch" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  n=0
  while IFS='|' read -r eng model effort profile _bin _marker; do
    n=$((n + 1))
    sed -i -e "s/^engine: .*/engine: $eng/" -e "s|^model: .*|model: $model|" -e "s/^effort: .*/effort: $effort/" "$WT/WORKER_TASK.md"
    : >"$STUB_LOG"
    DISPATCH_PROFILE="$profile" run run_resume
    [ "$status" -eq 0 ]
    case "$eng" in
    pi) grep -q -- "--append-system-prompt $DISPATCHER_PROTOCOL_DIR/WORKER_PROTOCOL.md" <(launch_log) ;;
    *) grep -q -- "Read $DISPATCHER_PROTOCOL_DIR/WORKER_PROTOCOL.md and WORKER_TASK.md" <(launch_log) ;;
    esac
    grep -q -- "live in $DISPATCHER_PROTOCOL_DIR" <(launch_log)
  done < <(protocol_engine_specs codex cursor pi)
  # A zero-iteration loop would pass vacuously; a mistyped/renamed filter must fail.
  [ "$n" -eq 3 ]
}

# _assert_resume_bound <marker> — the resumed lead's send-keys line is only
# `bash '<launch script>'` (#298), under 512 bytes, and the script carries
# <marker> (the engine's continue/resume flag).
_assert_resume_bound() {
  local marker="$1" crew_launch_dir pattern line
  crew_launch_dir="$(git -C "$TEST_REPO" rev-parse --path-format=absolute --git-common-dir)/crew/launch"
  pattern="^send-keys -t %8 bash '${crew_launch_dir}/launch\.[A-Za-z0-9]{6}' Enter\$"
  line="$(grep '^send-keys' "$STUB_LOG")"
  [[ "$line" =~ $pattern ]]
  [ "${#line}" -lt 512 ]
  grep -q -F -- "$marker" <(launch_log)
}

# #298: a fresh pane's tty truncates typed-ahead input at 1024 bytes on macOS,
# so resume must type a short line for every engine, however long the prompt.
@test "bound: resume's send-keys line stays short and shaped for every engine" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  _assert_resume_bound 'claude --continue'

  n=0
  while IFS='|' read -r eng model effort profile _bin _marker; do
    n=$((n + 1))
    sed -i -e "s/^engine: .*/engine: $eng/" -e "s|^model: .*|model: $model|" -e "s/^effort: .*/effort: $effort/" "$WT/WORKER_TASK.md"
    : >"$STUB_LOG"
    DISPATCH_PROFILE="$profile" run run_resume
    [ "$status" -eq 0 ]
    case "$eng" in
    codex) _assert_resume_bound 'codex resume --last' ;;
    cursor) _assert_resume_bound 'cursor-agent --continue' ;;
    pi) _assert_resume_bound 'pi --continue' ;;
    esac
  done < <(protocol_engine_specs codex cursor pi)
  # A zero-iteration loop would pass vacuously; a mistyped/renamed filter must fail.
  [ "$n" -eq 3 ]
}

# dispatch-resume.sh is a standalone build, so it carries its own copies.
@test "shell_quote and write_launch_script are byte-identical between dispatch.sh and dispatch-resume.sh" {
  for fn in shell_quote write_launch_script _artifacts_dir_bad _protocol_dirs_record_bad _settings_env_names _settings_env_json _record_protocol_dirs launch_dir_args claude_lean_env _ensure_roster_render _recorded_pid_live _pid_alive _file_mtime_s _pid_recycled _ps_elapsed_s; do
    a="$(sed -n "/^${fn}() {/,/^}/p" "$BATS_TEST_DIRNAME/../adapters/core/dispatch.sh")"
    b="$(sed -n "/^${fn}() {/,/^}/p" "$BATS_TEST_DIRNAME/../adapters/core/dispatch-resume.sh")"
    [ -n "$a" ]
    [ "$a" = "$b" ]
  done
}

# _symlink_chain_hops, _git_config_files, _git_protected_dirs and _add_dir_ok
# live once in the shared grant-check lib, so dispatch, dispatch-resume and
# permission-check cannot drift. This pins the single-definition guarantee
# the byte-identical test used to supply for these four.
@test "the grant-check functions are defined only in the shared grant-check lib" {
  for fn in _symlink_chain_hops _git_config_files _git_protected_dirs _add_dir_ok; do
    run grep -q "^${fn}() {" "$BATS_TEST_DIRNAME/../adapters/core/dispatch.sh"
    [ "$status" -ne 0 ]
    run grep -q "^${fn}() {" "$BATS_TEST_DIRNAME/../adapters/core/dispatch-resume.sh"
    [ "$status" -ne 0 ]
    run grep -q "^${fn}() {" "$BATS_TEST_DIRNAME/../adapters/core/permission-check.sh"
    [ "$status" -ne 0 ]
    run grep -q "^${fn}() {" "$BATS_TEST_DIRNAME/../adapters/core/grant-check.sh"
    [ "$status" -eq 0 ]
  done
}

# _worktree_anchor_path lives once in the shared worktree-git lib, so dispatch
# (writer), dispatch-resume (reader) and crew reap (pruner) cannot drift. This
# pins the key format directly against the lib.
@test "_worktree_anchor_path derives the sha256(realpath) key from the shared lib" {
  # shellcheck source=/dev/null
  . "$BATS_TEST_DIRNAME/../adapters/core/worktree-git.sh"
  wt="$TEST_REPO/anchor-wt"
  mkdir -p "$wt"
  key="$(printf %s "$(realpath -e "$wt")" | sha256sum | cut -c1-64)"
  [ "$(_worktree_anchor_path "$wt")" = "$XDG_DATA_HOME/crew/worktrees/$key" ]
}

@test "_worktree_anchor_path is defined only in the shared worktree-git lib" {
  # The single definition is the anti-drift guarantee the byte-identical test
  # used to supply; a stray copy in either dispatcher script would fork it again.
  run grep -q '^_worktree_anchor_path() {' "$BATS_TEST_DIRNAME/../adapters/core/dispatch.sh"
  [ "$status" -ne 0 ]
  run grep -q '^_worktree_anchor_path() {' "$BATS_TEST_DIRNAME/../adapters/core/dispatch-resume.sh"
  [ "$status" -ne 0 ]
  run grep -q '^_worktree_anchor_path() {' "$BATS_TEST_DIRNAME/../adapters/core/worktree-git.sh"
  [ "$status" -eq 0 ]
}

# The lead-session helpers are duplicated for the same reason; one test diffs
# them so a fix in one file cannot silently miss the other.
@test "_uuid and the lead-record writers are byte-identical between dispatch.sh and dispatch-resume.sh" {
  for fn in _uuid _lead_record_safe _record_lead_session; do
    a="$(sed -n "/^${fn}() {/,/^}/p" "$BATS_TEST_DIRNAME/../adapters/core/dispatch.sh")"
    b="$(sed -n "/^${fn}() {/,/^}/p" "$BATS_TEST_DIRNAME/../adapters/core/dispatch-resume.sh")"
    [ -n "$a" ]
    [ "$a" = "$b" ]
  done
}

@test "_uuid emits a distinct lowercase v4-shaped uuid" {
  eval "$(sed -n '/^_uuid() {/,/^}/p' "$RESUME")"
  a="$(_uuid)"
  b="$(_uuid)"
  [[ "$a" =~ ^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$ ]]
  [ "$a" != "$b" ]
}

@test "--print reports the resolved launch and does not launch" {
  setup_worker_wt
  cd "$WT"
  run run_resume --print
  [ "$status" -eq 0 ]
  [[ "$output" == *"branch: feat/7-a-thing"* ]]
  [[ "$output" == *"engine: claude"* ]]
  [[ "$output" == *"model: sonnet"* ]]
  [[ "$output" == *"effort: medium"* ]]
  [[ "$output" == *"crew_id: c1"* ]]
  run cat "$STUB_LOG"
  [[ "$output" != *send-keys* ]]
}

@test "explicit flags override the recorded header" {
  setup_worker_wt
  cd "$WT"
  run run_resume --print --model opus --effort high
  [ "$status" -eq 0 ]
  [[ "$output" == *"model: opus"* ]]
  [[ "$output" == *"effort: high"* ]]
}

@test "the task body is left byte-identical" {
  setup_worker_wt
  cd "$WT"
  before="$(md5sum <"$WT/WORKER_TASK.md")"
  run run_resume --print
  [ "$status" -eq 0 ]
  [ "$(md5sum <"$WT/WORKER_TASK.md")" = "$before" ]
}

@test "resume preserves a stacked base: through the header rewrite" {
  # A real resume (not --print) so the _hdr_set rewrite actually runs; base: is
  # not in its field list, and must stay that way for a stacked worker.
  setup_worker_wt 'base: feat/parent'
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  grep -qx 'base: feat/parent' "$WT/WORKER_TASK.md"
}

# tmux stub that reports one pane sitting at $WT, so the reuse path fires.
# $4 (pane_current_command) defaults to empty — a plain shell, i.e. nothing an
# engine matcher would claim — so existing callers that omit it keep meaning
# "a human sitting there".
stub_tmux_with_pane_at_wt() { # $1=window id  $2=pane id  $3=@crew_name value  $4=pane_current_command
  cat >"$STUB_DIR/tmux" <<EOF
#!/usr/bin/env bash
printf '%s\n' "\$*" >>"\$STUB_LOG"
case "\$1" in
list-panes) printf '%s\t%s\t%s\t%s\t%s\n' '$1' '$2' '$WT' '${4:-}' '$3' ;;
new-window) printf '%s %s\n' '%99' '%99' ;;
display-message) printf '%s\n' '80 24 on' ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/tmux"
}

@test "--print names the existing pane at the worktree" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume --print
  [ "$status" -eq 0 ]
  [[ "$output" == *"window: @4"* ]]
  [[ "$output" == *"pane: %8"* ]]
  [[ "$output" == *"placement: reuse"* ]]
}

@test "--print reuses a pane with no worker identity (a human sitting there)" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' ''
  cd "$WT"
  run run_resume --print
  [ "$status" -eq 0 ]
  [[ "$output" == *"pane: %8"* ]]
  [[ "$output" == *"placement: reuse"* ]]
}

@test "refuses a pane still running a live claude engine" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris claude
  cd "$WT"
  run run_resume
  [ "$status" -eq 1 ]
  [[ "$output" == *"already alive there"* ]]
  [[ "$output" == *"tmux select-window -t @4"* ]]
  run grep -c send-keys "$STUB_LOG"
  [ "$status" -ne 0 ]
}

@test "refuses a pane running a nix-wrapped engine name" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris .claude-wrapped
  cd "$WT"
  run run_resume
  [ "$status" -eq 1 ]
  [[ "$output" == *"already alive there"* ]]
}

@test "refuses the guard on --print too, before any placement is reported" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris codex
  cd "$WT"
  run run_resume --print
  [ "$status" -eq 1 ]
  [[ "$output" != *"placement:"* ]]
}

@test "still resumes into a plain shell at the worktree (the primary use case)" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' '' fish
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  grep -q 'send-keys' "$STUB_LOG"
}

@test "--print reports a fresh window when nothing sits at the worktree" {
  setup_worker_wt
  cd "$WT"
  run run_resume --print
  [ "$status" -eq 0 ]
  [[ "$output" == *"placement: create"* ]]
  [[ "$output" == *"window: -"* ]]
  [[ "$output" == *"pane: -"* ]]
}

@test "--print on the create path opens no window and stamps nothing" {
  setup_worker_wt
  cd "$WT"
  run run_resume --print
  [ "$status" -eq 0 ]
  run cat "$STUB_LOG"
  [[ "$output" != *new-window* ]]
  [[ "$output" != *set-window-option* ]]
}

@test "--print on the reuse path stamps nothing" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume --print
  [ "$status" -eq 0 ]
  run cat "$STUB_LOG"
  [[ "$output" != *set-window-option* ]]
}

@test "creating a window stamps the crew identity on it" {
  setup_worker_wt
  cat >"$STUB_DIR/tmux" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$1" in
list-panes) : ;;
new-window) printf '%s %s\n' '%99' '%99' ;;
display-message) printf '%s\n' '80 24 on' ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/tmux"
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  grep -q 'set-window-option -t %99 @crew_name iris' "$STUB_LOG"
}

@test "reusing a pane does not open a window" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  run grep -c new-window "$STUB_LOG"
  [ "$status" -ne 0 ]
}

@test "runs the dispatch precheck with the recorded tuple" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  grep -qE 'standard sonnet .*--effort medium' "$STUB_LOG"
  grep -q -- '--agent claude' "$STUB_LOG"
  grep -q -- '--crew-id c1' "$STUB_LOG"
}

@test "suppresses the tier-model map when no model was named" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  grep -q -- '--ignore-map' "$STUB_LOG"
}

@test "re-arms the tier-model map when --model is passed" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume --model opus
  [ "$status" -eq 0 ]
  run grep -c -- '--ignore-map' "$STUB_LOG"
  [ "$status" -ne 0 ]
}

@test "forwards --ignore-budget to the precheck" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume --ignore-budget
  [ "$status" -eq 0 ]
  grep -q -- '--ignore-budget' "$STUB_LOG"
}

@test "a refused precheck aborts before any launch" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  STUB_PRECHECK_FAIL=1 run run_resume
  [ "$status" -eq 1 ]
  [[ "$output" == *"ultra is codex-only"* ]]
  run grep -c send-keys "$STUB_LOG"
  [ "$status" -ne 0 ]
}

@test "refuses an mcp profile on a non-claude engine" {
  setup_worker_wt
  sed -i -e 's/^engine: claude/engine: codex/' -e 's/^mcp: $/mcp: analytics/' "$WT/WORKER_TASK.md"
  cd "$WT"
  DISPATCH_PROFILE=work run run_resume
  [ "$status" -eq 1 ]
  [[ "$output" == *"claude-only"* ]]
}

@test "resume: profile from the settings file enables the work deep-claude codex MCP" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  sed -i 's/^tier: standard/tier: deep/' "$WT/WORKER_TASK.md"
  mkdir -p "$XDG_CONFIG_HOME/dispatcher"
  printf '{"profile":"work"}\n' >"$XDG_CONFIG_HOME/dispatcher/settings.json"
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  grep -F 'claude --continue' <(launch_log) | grep -qF 'mcp-codex.json'
}

@test "claude resume launches with --continue and the recorded tuple" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  DISPATCH_SESSION_ID=s2-100 run run_resume
  [ "$status" -eq 0 ]
  grep -qE 'send-keys -t %8 (-u DISPATCHER_[A-Z]+_DIR )*(DISPATCHER_[A-Z]+_DIR=[^ ]+ |DISPATCH_GRANT_ROOTS=[^ ]+ )*GIT_EDITOR=true GIT_SEQUENCE_EDITOR=: CREW_WORKER_ID=[^ ]+ CREW_ID=[^ ]+ ENABLE_CLAUDEAI_MCP_SERVERS=false claude --continue' <(launch_log)
  grep -q 'CREW_WORKER_ID=worker:feat/7-a-thing#s2-100 CREW_ID=c1 ENABLE_CLAUDEAI_MCP_SERVERS=false claude --continue' <(launch_log)
  grep -q -- '--model sonnet' <(launch_log)
  grep -q -- '--effort medium' <(launch_log)
  grep -q -- "--append-system-prompt-file $DISPATCHER_PROTOCOL_DIR/WORKER_PROTOCOL.claude.md" <(launch_log)
}

@test "claude resume disables only the worker's unused plugins" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  unset CLAUDE_WORKER_SETTINGS_LIB
  DISPATCH_SESSION_ID=s2-100 run run_resume
  [ "$status" -eq 0 ]
  line="$(grep -F 'claude --continue' <(launch_log))"
  settings='\{\"enabledPlugins\":\{\"superpowers@superpowers-dev\":false\,\"agent-smith@agent-smith\":false\,\"frontend-design@claude-plugins-official\":false\,\"refactoring-agent@xdg-claude\":false\,\"commit-commands@claude-code-plugins\":false\,\"resolved@resolved\":false\,\"context-efficient-tools@xdg-claude\":false\}\}'
  [[ "$line" == *"--settings $settings --add-dir "* ]]
  [[ "$line" != *disableAllHooks* ]]
  [[ "$line" != *gopls-lsp* ]]
}

@test "claude resume drops the claude.ai connectors unless DISPATCH_CLAUDE_CONNECTORS=1" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  DISPATCH_SESSION_ID=s2-100 run run_resume
  [ "$status" -eq 0 ]
  grep -qF 'ENABLE_CLAUDEAI_MCP_SERVERS=false claude --continue' <(launch_log)
}

@test "claude resume keeps the connectors under DISPATCH_CLAUDE_CONNECTORS=1" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  DISPATCH_CLAUDE_CONNECTORS=1 DISPATCH_SESSION_ID=s2-100 run run_resume
  [ "$status" -eq 0 ]
  ! grep -qF 'ENABLE_CLAUDEAI_MCP_SERVERS' <(launch_log)
  grep -qF "DISPATCH_CLAUDE_CONNECTORS=1 claude --continue" <(launch_log)
}

# _grant_record <line...> — write the branch's grant record, the only source a
# resumed claude launch reads its extra dirs from.
_grant_record() {
  mkdir -p "$TEST_REPO/.git/crew/grants/feat"
  printf '%s\n' "$@" >"$TEST_REPO/.git/crew/grants/feat/7-a-thing"
}

# _ro_rule <dir> — the read-only Edit deny-rule word launch_dir_args emits for
# a protocol dir, %q-quoted exactly as printf would (sets $r).
_ro_rule() { printf -v r ' %q' "Edit(/$1/**)"; }

@test "claude resume grants the mandated dirs and the recorded ones, terminated by an option" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  export DISPATCH_GRANT_ROOTS="$(realpath "$BATS_TEST_TMPDIR")"
  extra="$(realpath "$BATS_TEST_TMPDIR")/extra"
  mkdir -p "$extra"
  _grant_record "$extra"
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  line="$(grep -F 'claude --continue' <(launch_log))"
  [[ "$line" == *"--add-dir $DISPATCHER_PROTOCOL_DIR "* ]]
  [[ "$line" == *"--add-dir $DISPATCHER_SKILLS_DIR "* ]]
  [[ "$line" == *"--add-dir $TEST_REPO/.git/crew/artifacts/feat/7-a-thing "* ]]
  [[ "$line" == *"--add-dir $extra "* ]]
  [ -d "$TEST_REPO/.git/crew/artifacts/feat/7-a-thing" ]
  assert_add_dir_terminated "$line"
  _ro_rule "$DISPATCHER_PROTOCOL_DIR"
  [[ "$line" == *"$r"* ]]
}

@test "resume stamps the window for --spawn-role and rewrites the protocol-dirs record" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  rec="$TEST_REPO/.git/crew/protocol-dirs/feat/7-a-thing"
  mkdir -p "$(dirname "$rec")"
  printf '%s\n' /stale/protocols /stale/skills "" "" /stale/wt >"$rec"
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  grep -qxF -- "set-window-option -t @4 @crew_dir $TEST_REPO/.git/crew" "$STUB_LOG"
  grep -qxF -- 'set-window-option -t @4 @crew_branch feat/7-a-thing' "$STUB_LOG"
  grep -qxF -- 'set-window-option -t @4 @crew_id c1' "$STUB_LOG"
  grep -qxF -- 'set-option -p -t %8 @crew_model sonnet' "$STUB_LOG"
  mapfile -t lines <"$rec"
  [ "${#lines[@]}" -eq 6 ]
  [ "${lines[0]}" = "$DISPATCHER_PROTOCOL_DIR" ]
  [ "${lines[1]}" = "$DISPATCHER_SKILLS_DIR" ]
  [ "${lines[4]}" = "$(realpath "$WT")" ]
  jq -e --arg xdg "${XDG_CONFIG_HOME:-$HOME/.config}" 'type == "object" and .XDG_CONFIG_HOME == $xdg' <<<"${lines[5]}"
}

@test "resume on a new window stamps it for --spawn-role" {
  setup_worker_wt
  stub_tmux_no_pane
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  grep -qxF -- "set-window-option -t %99 @crew_dir $TEST_REPO/.git/crew" "$STUB_LOG"
  grep -qxF -- 'set-window-option -t %99 @crew_branch feat/7-a-thing' "$STUB_LOG"
  grep -qxF -- 'set-window-option -t %99 @crew_id c1' "$STUB_LOG"
}

@test "resume refuses a symlinked protocol-dirs record before launching" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  victim="$BATS_TEST_TMPDIR/victim"
  printf 'keep me\n' >"$victim"
  mkdir -p "$TEST_REPO/.git/crew/protocol-dirs/feat"
  ln -s "$victim" "$TEST_REPO/.git/crew/protocol-dirs/feat/7-a-thing"
  cd "$WT"
  run run_resume
  [ "$status" -eq 1 ]
  [[ "$output" == *"refusing to write the protocol-dirs record"* ]]
  [ "$(cat "$victim")" = "keep me" ]
  if [ -f "$STUB_LOG" ]; then run grep -q 'send-keys' "$STUB_LOG"; [ "$status" -ne 0 ]; fi
}

@test "claude resume refuses a symlinked parent of the artifacts dir and creates nothing under its target" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  victim="$BATS_TEST_TMPDIR/victim"
  mkdir -p "$victim" "$TEST_REPO/.git/crew/artifacts"
  ln -s "$victim" "$TEST_REPO/.git/crew/artifacts/feat"
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  [[ "$output" == *"is a symlink or not a directory — not granting"* ]]
  [ -z "$(ls -A "$victim")" ]
  line="$(grep -F 'claude --continue' <(launch_log))"
  [[ "$line" != *"crew/artifacts"* ]]
}

@test "claude resume never reads a grant from the task doc's add_dir: header" {
  evil="$(realpath "$BATS_TEST_TMPDIR")/evil"
  mkdir -p "$evil"
  setup_worker_wt "add_dir: $evil"
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  export DISPATCH_GRANT_ROOTS="$(realpath "$BATS_TEST_TMPDIR")"
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  line="$(grep -F 'claude --continue' <(launch_log))"
  [[ "$line" == *"--add-dir $TEST_REPO/.git/crew/artifacts/feat/7-a-thing "* ]]
  [[ "$line" != *"$evil"* ]]
}

@test "claude resume drops an invalid recorded grant with a warning and still launches" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  export DISPATCH_GRANT_ROOTS="$(realpath "$BATS_TEST_TMPDIR")"
  extra="$(realpath "$BATS_TEST_TMPDIR")/extra"
  mkdir -p "$extra"
  _grant_record / "$extra"
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  [[ "$output" == *"dispatch: dropping invalid grant '/' for feat/7-a-thing"* ]]
  line="$(grep -F 'claude --continue' <(launch_log))"
  [[ "$line" != *"--add-dir / "* ]]
  [[ "$line" == *"--add-dir $extra "* ]]
}

@test "claude resume drops a recorded grant that is a hooks dir via core.hooksPath" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  export DISPATCH_GRANT_ROOTS="$(realpath "$BATS_TEST_TMPDIR")"
  extra="$(realpath "$BATS_TEST_TMPDIR")/extra"
  git init -q "$extra"
  git -C "$extra" config core.hooksPath .husky
  mkdir -p "$extra/.husky" "$extra/docs"
  _grant_record "$extra/.husky" "$extra/docs"
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  [[ "$output" == *"dispatch: dropping invalid grant '$extra/.husky' for feat/7-a-thing"* ]]
  line="$(grep -F 'claude --continue' <(launch_log))"
  [[ "$line" != *"--add-dir $extra/.husky "* ]]
  [[ "$line" == *"--add-dir $extra/docs "* ]]
}

@test "claude resume drops a recorded grant outside the grant roots and still launches" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  unset DISPATCH_GRANT_ROOTS
  extra="$(realpath "$BATS_TEST_TMPDIR")/extra"
  mkdir -p "$extra"
  _grant_record "$extra"
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  [[ "$output" == *"dispatch: dropping invalid grant '$extra' for feat/7-a-thing"* ]]
  line="$(grep -F 'claude --continue' <(launch_log))"
  [[ "$line" != *"$extra"* ]]
  [[ "$line" == *"DISPATCH_GRANT_ROOTS=: "* ]]
  [[ "$line" != *"-u DISPATCH_GRANT_ROOTS"* ]]
}

@test "claude resume pins its grant roots into the launch" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  export DISPATCH_GRANT_ROOTS="$(realpath "$BATS_TEST_TMPDIR")"
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  line="$(grep -F 'claude --continue' <(launch_log))"
  [[ "$line" == *"DISPATCH_GRANT_ROOTS=$DISPATCH_GRANT_ROOTS "* ]]
}

@test "restamps protocol_dir into an older task doc and names it in the claude prompt" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  grep -qx "protocol_dir: $DISPATCHER_PROTOCOL_DIR" "$WT/WORKER_TASK.md"
  grep -q -- "live in $DISPATCHER_PROTOCOL_DIR" <(launch_log)
}

@test "--fresh drops the continue flag" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume --fresh
  [ "$status" -eq 0 ]
  grep -qE 'send-keys -t %8 (-u DISPATCHER_[A-Z]+_DIR )*(DISPATCHER_[A-Z]+_DIR=[^ ]+ |DISPATCH_GRANT_ROOTS=[^ ]+ )*GIT_EDITOR=true GIT_SEQUENCE_EDITOR=: CREW_WORKER_ID=[^ ]+ CREW_ID=[^ ]+ ENABLE_CLAUDEAI_MCP_SERVERS=false claude ' <(launch_log)
  run grep -c -- '--continue' <(launch_log)
  [ "$status" -ne 0 ]
  sid="$(grep -oE -- "--session-id $UUID_RE" <(launch_log) | head -1 | cut -d' ' -f2)"
  [ -n "$sid" ]
  [ "$(jq -r 'select(.kind == "resume") | .engine_session' "$(bus_log)" | tail -1)" = "$sid" ]
}

@test "codex resume launches resume --last" {
  setup_worker_wt
  sed -i -e 's/^engine: claude/engine: codex/' -e 's/^model: sonnet/model: gpt-5.6-sol/' "$WT/WORKER_TASK.md"
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  DISPATCH_PROFILE=work run run_resume
  [ "$status" -eq 0 ]
  grep -q 'codex resume --last' <(launch_log)
  grep -q -- '--profile worker' <(launch_log)
  [ "$(jq -c 'select(.kind == "resume") | .engine_session' "$(bus_log)" | tail -1)" = null ]
}

@test "codex --fresh drops the resume flag" {
  setup_worker_wt
  sed -i -e 's/^engine: claude/engine: codex/' -e 's/^model: sonnet/model: gpt-5.6-sol/' "$WT/WORKER_TASK.md"
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  DISPATCH_PROFILE=work run run_resume --fresh
  [ "$status" -eq 0 ]
  grep -qE 'send-keys -t %8 (-u DISPATCHER_[A-Z]+_DIR )*(DISPATCHER_[A-Z]+_DIR=[^ ]+ |DISPATCH_GRANT_ROOTS=[^ ]+ )*GIT_EDITOR=true GIT_SEQUENCE_EDITOR=: CREW_WORKER_ID=[^ ]+ CREW_ID=[^ ]+ codex ' <(launch_log)
  run grep -c -- 'resume --last' <(launch_log)
  [ "$status" -ne 0 ]
  [ "$(jq -c 'select(.kind == "resume") | .engine_session' "$(bus_log)" | tail -1)" = null ]
}

@test "cursor resume launches with --continue" {
  setup_worker_wt
  sed -i -e 's/^engine: claude/engine: cursor/' -e 's/^model: sonnet/model: composer-2.5/' "$WT/WORKER_TASK.md"
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  DISPATCH_PROFILE=work run run_resume
  [ "$status" -eq 0 ]
  grep -q 'cursor-agent --continue' <(launch_log)
}

@test "cursor --fresh drops the continue flag" {
  setup_worker_wt
  sed -i -e 's/^engine: claude/engine: cursor/' -e 's/^model: sonnet/model: composer-2.5/' "$WT/WORKER_TASK.md"
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  DISPATCH_PROFILE=work run run_resume --fresh
  [ "$status" -eq 0 ]
  grep -qE 'send-keys -t %8 (-u DISPATCHER_[A-Z]+_DIR )*(DISPATCHER_[A-Z]+_DIR=[^ ]+ |DISPATCH_GRANT_ROOTS=[^ ]+ )*GIT_EDITOR=true GIT_SEQUENCE_EDITOR=: CREW_WORKER_ID=[^ ]+ CREW_ID=[^ ]+ CURSOR_CLI_INDEXED_GREP=0 cursor-agent ' <(launch_log)
  run grep -c -- '--continue' <(launch_log)
  [ "$status" -ne 0 ]
}

@test "pi resume of a legacy grid lead relaunches fresh with a recorded id and reapplies the protocol" {
  setup_worker_wt 'roles: plan-critic,reviewer'
  sed -i -e 's/^engine: claude/engine: pi/' -e 's|^model: sonnet|model: openrouter/deepseek/deepseek-v4-flash|' "$WT/WORKER_TASK.md"
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  grep -qE "PI_CODING_AGENT_DIR=$HOME/.pi/dispatcher-worker pi +--name iris .*--thinking medium --session-id $UUID_RE --append-system-prompt" <(launch_log)
  run grep -c -- '--continue' <(launch_log)
  [ "$output" = 0 ]
  grep -q 'Read SPEC.md and PLAN.md' <(launch_log)
  grep -q -- "--append-system-prompt $DISPATCHER_PROTOCOL_DIR/WORKER_PROTOCOL.md" <(launch_log)
  grep -q -- '--no-approve' <(launch_log)
  grep -q 'role panes (plan-critic,reviewer) may still be parked' <(launch_log)
  [ "$(jq -r .defaultProjectTrust "$HOME/.pi/dispatcher-worker/settings.json")" = never ]
  [ "$(jq -r 'select(.kind == "resume") | .continued' "$(bus_log)" | tail -1)" = false ]
  sid="$(grep -oE -- "--session-id $UUID_RE" <(launch_log) | head -1 | cut -d' ' -f2)"
  [ "$(cat "$(lead_rec)")" = "pi $sid" ]
}

@test "pi resume of a legacy lead with no role grid still continues the latest session" {
  setup_worker_wt
  sed -i -e 's/^engine: claude/engine: pi/' -e 's|^model: sonnet|model: openrouter/deepseek/deepseek-v4-flash|' "$WT/WORKER_TASK.md"
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  grep -q "PI_CODING_AGENT_DIR=$HOME/.pi/dispatcher-worker pi --continue" <(launch_log)
  run grep -c -- '--session-id' <(launch_log)
  [ "$output" = 0 ]
  [ ! -e "$(lead_rec)" ]
}

@test "resuming a review lead with a grid points it at REVIEW_TASK's role-grid path" {
  setup_worker_wt 'roles: reviewer,refuter'
  sed -i -e 's/^kind: implement/kind: review/' -e 's/^engine: claude/engine: pi/' -e 's|^model: sonnet|model: openrouter/deepseek/deepseek-v4.1-flash|' "$WT/WORKER_TASK.md"
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  grep -q 'REVIEW_TASK.md Role-grid path' <(launch_log)
  run grep -c 'WORKER_PROTOCOL.md Grid mode' <(launch_log)
  [ "$output" = 0 ]
}

@test "pi resume passes the worktree's project skills and the harness skills with --skill" {
  setup_worker_wt
  mkdir -p "$WT/.agents/skills/preview"
  printf -- '---\nname: preview\ndescription: seeded\n---\n' >"$WT/.agents/skills/preview/SKILL.md"
  sed -i -e 's/^engine: claude/engine: pi/' -e 's|^model: sonnet|model: openrouter/deepseek/deepseek-v4-flash|' "$WT/WORKER_TASK.md"
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  grep -q -- "--no-approve --skill $WT/.agents/skills --skill $DISPATCHER_SKILLS_DIR " <(launch_log)
}

@test "pi resume passes only the harness skills when the worktree has none" {
  setup_worker_wt
  sed -i -e 's/^engine: claude/engine: pi/' -e 's|^model: sonnet|model: openrouter/deepseek/deepseek-v4-flash|' "$WT/WORKER_TASK.md"
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  grep -q -- "--no-approve --skill $DISPATCHER_SKILLS_DIR " <(launch_log)
  [ "$(grep -cF -- '--skill' <(launch_log))" -eq 1 ]
}

# A non-Nix install leaves @skillsDir@ unsubstituted, so the path is not a
# directory and the probe has to drop it rather than hand pi a literal.
@test "pi resume omits --skill when neither the worktree nor the harness dir exists" {
  setup_worker_wt
  export DISPATCHER_SKILLS_DIR="$TEST_REPO/no-such-skills"
  sed -i -e 's/^engine: claude/engine: pi/' -e 's|^model: sonnet|model: openrouter/deepseek/deepseek-v4-flash|' "$WT/WORKER_TASK.md"
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  run grep -F -- '--skill' <(launch_log)
  [ "$status" -ne 0 ]
}

@test "pi --fresh drops the continue flag" {
  setup_worker_wt
  sed -i -e 's/^engine: claude/engine: pi/' -e 's|^model: sonnet|model: openrouter/deepseek/deepseek-v4-flash|' "$WT/WORKER_TASK.md"
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume --fresh
  [ "$status" -eq 0 ]
  grep -qE 'send-keys -t %8 (-u DISPATCHER_[A-Z]+_DIR )*(DISPATCHER_[A-Z]+_DIR=[^ ]+ |DISPATCH_GRANT_ROOTS=[^ ]+ )*GIT_EDITOR=true GIT_SEQUENCE_EDITOR=: CREW_WORKER_ID=[^ ]+ CREW_ID=[^ ]+ PI_CODING_AGENT_DIR=[^ ]+ pi ' <(launch_log)
  run grep -c -- '--continue' <(launch_log)
  [ "$status" -ne 0 ]
}

@test "pi resume fails closed when the agent dir cannot be seeded" {
  setup_worker_wt
  sed -i -e 's/^engine: claude/engine: pi/' -e 's|^model: sonnet|model: openrouter/deepseek/deepseek-v4-flash|' "$WT/WORKER_TASK.md"
  cat >"$STUB_DIR/crew" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
if [ "$1" = pi-agent-dir ]; then exit 0; fi
if [ "$1" = engine-cmd ]; then
  c="${2#.}"
  c="${c%-wrapped}"
  case "$c" in
  claude | codex | cursor-agent | node | pi) exit 0 ;;
  esac
  exit 1
fi
exit 0
EOF
  chmod +x "$STUB_DIR/crew"
  cd "$WT"
  run run_resume
  [ "$status" -eq 1 ]
  [[ "$output" == *"could not seed the pi worker agent dir"* ]]
  run grep -c -- 'send-keys' "$STUB_LOG"
  [ "$status" -ne 0 ]
  run grep -c -- 'new-window' "$STUB_LOG"
  [ "$status" -ne 0 ]
}

@test "pi resume --print does not seed the agent dir" {
  setup_worker_wt
  sed -i -e 's/^engine: claude/engine: pi/' -e 's|^model: sonnet|model: openrouter/deepseek/deepseek-v4-flash|' "$WT/WORKER_TASK.md"
  cd "$WT"
  run run_resume --print
  [ "$status" -eq 0 ]
  [ ! -d "$HOME/.pi/dispatcher-worker" ]
}

@test "pi resume leaves the ambient ~/.pi/agent fixture untouched and does not leak secrets" {
  setup_worker_wt
  sed -i -e 's/^engine: claude/engine: pi/' -e 's|^model: sonnet|model: openrouter/deepseek/deepseek-v4-flash|' "$WT/WORKER_TASK.md"
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  mkdir -p "$HOME/.pi/agent"
  printf '{"opencode":{"type":"api_key","key":"SECRET-RESUME-FIXTURE"}}\n' >"$HOME/.pi/agent/auth.json"
  printf '{"defaultProjectTrust":"always"}\n' >"$HOME/.pi/agent/settings.json"
  before_auth=$(sha256sum "$HOME/.pi/agent/auth.json")
  before_settings=$(sha256sum "$HOME/.pi/agent/settings.json")
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  [ "$(sha256sum "$HOME/.pi/agent/auth.json")" = "$before_auth" ]
  [ "$(sha256sum "$HOME/.pi/agent/settings.json")" = "$before_settings" ]
  run grep -rF SECRET-RESUME-FIXTURE "$HOME/.pi/dispatcher-worker"
  [ "$status" -ne 0 ]
}

@test "the reorient prompt tells the worker not to trust its last plan" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  grep -q 'do not trust the last plan in your transcript' <(launch_log)
}

@test "plan provided resume prompt keeps the code review gate (#306)" {
  setup_worker_wt
  sed -i 's/^plan: required/plan: provided/' "$WT/WORKER_TASK.md"
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  grep -q 'Only planning is skipped' <(launch_log)
  grep -q 'Run your code review gate' <(launch_log)
}

@test "plan provided resume prompt for a kind: review worker has no code review gate to keep (#306)" {
  setup_worker_wt
  sed -i -e 's/^plan: required/plan: provided/' -e 's/^kind: implement/kind: review/' "$WT/WORKER_TASK.md"
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  grep -q 'plan of record' <(launch_log)
  run ! grep -q 'Only planning is skipped' <(launch_log)
}

@test "trailing arguments are appended to the prompt" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume the review comments are the priority
  [ "$status" -eq 0 ]
  grep -q 'the review comments are the priority' <(launch_log)
}

# The prompt lives in the launch script; the typed line carries only the
# script's one quoted path.
@test "no launch string contains an apostrophe" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  run grep -c "send-keys.*'.*'.*'" <(launch_log)
  [ "$status" -ne 0 ]
  raw="$(grep 'send-keys' "$STUB_LOG")"
  [ "$(grep -o "'" <<<"$raw" | wc -l)" -eq 2 ]
}

# #470: the task header and the branch name are worker-writable, and the launch
# script they are spliced into runs as the operator. A refused resume writes no
# launch script, types nothing into a pane, and runs nothing.
_assert_refused_unlaunched() { # <field> <marker>
  [ "$status" -eq 1 ]
  [[ "$output" == *"refusing to launch: $1"* ]]
  [ ! -e "$2" ]
  run ! grep -q 'send-keys' "$STUB_LOG"
  [ ! -e "$TEST_REPO/.git/crew/launch" ]
}

@test "resume refuses a command substitution in the header's agent_name" {
  marker="$BATS_TEST_TMPDIR/pwned"
  setup_worker_wt
  sed -i "s|^agent_name: .*|agent_name: \$(touch $marker)|" "$WT/WORKER_TASK.md"
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume
  _assert_refused_unlaunched agent_name "$marker"
}

@test "resume refuses shell metacharacters in the header's crew_id" {
  marker="$BATS_TEST_TMPDIR/pwned"
  setup_worker_wt
  sed -i "s|^crew_id: .*|crew_id: c1;touch $marker|" "$WT/WORKER_TASK.md"
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume
  _assert_refused_unlaunched crew_id "$marker"
}

@test "resume refuses a quote-break in the header's roles" {
  marker="$BATS_TEST_TMPDIR/pwned"
  setup_worker_wt
  sed -i "/^roles: /d" "$WT/WORKER_TASK.md"
  sed -i "s|^agent_name: iris|agent_name: iris\nroles: x'; touch $marker; '|" "$WT/WORKER_TASK.md"
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume
  _assert_refused_unlaunched roles "$marker"
}

@test "resume refuses shell metacharacters in the header's model and effort" {
  marker="$BATS_TEST_TMPDIR/pwned"
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  cp WORKER_TASK.md "$BATS_TEST_TMPDIR/task.orig"
  sed -i "s|^model: .*|model: sonnet;touch $marker|" WORKER_TASK.md
  run run_resume
  _assert_refused_unlaunched model "$marker"
  cp "$BATS_TEST_TMPDIR/task.orig" WORKER_TASK.md
  sed -i "s|^effort: .*|effort: medium\$(touch $marker)|" WORKER_TASK.md
  run run_resume
  _assert_refused_unlaunched effort "$marker"
}

@test "resume refuses a branch name carrying a command substitution" {
  marker="$BATS_TEST_TMPDIR/pwned"
  git -C "$TEST_REPO" worktree add -q -b 'feat/$(touch${IFS}pwned)' "$TEST_REPO/evilwt" HEAD
  WT="$TEST_REPO/evilwt"
  printf 'tier: standard\nkind: implement\nengine: claude\nmodel: sonnet\neffort: medium\ncrew_id: c1\nagent_name: iris\n\n## Task\nx\n' >"$WT/WORKER_TASK.md"
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume
  [ "$status" -eq 1 ]
  [[ "$output" == *"refusing to launch: branch name"* ]]
  [ ! -e "$WT/pwned" ] && [ ! -e pwned ]
  run ! grep -q 'send-keys' "$STUB_LOG"
  [ ! -e "$TEST_REPO/.git/crew/launch" ]
}

@test "resume still launches a plain worker for claude and pi" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  DISPATCH_SESSION_ID=s2-100 run run_resume
  [ "$status" -eq 0 ]
  grep -q 'CREW_WORKER_ID=worker:feat/7-a-thing#s2-100 CREW_ID=c1 ENABLE_CLAUDEAI_MCP_SERVERS=false claude ' <(launch_log)
  grep -q -- '--name iris --model sonnet --effort medium' <(launch_log)
  sed -i -e 's|^engine: .*|engine: pi|' -e 's|^model: .*|model: openrouter/deepseek/deepseek-v4-flash|' WORKER_TASK.md
  : >"$STUB_LOG"
  DISPATCH_SESSION_ID=s3-101 run run_resume
  [ "$status" -eq 0 ]
  grep -q 'CREW_WORKER_ID=worker:feat/7-a-thing#s3-101 CREW_ID=c1 ' <(launch_log)
  grep -q -- '--name iris --model openrouter/deepseek/deepseek-v4-flash --thinking medium' <(launch_log)
}

bus_log() { printf '%s/.git/crew/events.jsonl' "$TEST_REPO"; }

UUID_RE='[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}'
LEAD_ID=aaaaaaaa-1111-4111-8111-aaaaaaaaaaaa
ROLE_ID=bbbbbbbb-2222-4222-8222-bbbbbbbbbbbb

lead_rec() { printf '%s/.git/crew/leads/feat/7-a-thing' "$TEST_REPO"; }

# claude_transcript <id> <age-seconds> — a transcript for the worktree's project
# dir, <age-seconds> old. Named claude_transcript so it cannot shadow a fixture.
claude_transcript() {
  local dir
  dir="$HOME/.claude/projects/$(git -C "$WT" rev-parse --show-toplevel | sed 's/[^a-zA-Z0-9]/-/g')"
  mkdir -p "$dir"
  printf '{}\n' >"$dir/$1.jsonl"
  touch -d "$2 seconds ago" "$dir/$1.jsonl"
}

# record_lead <line> — plant the lead's session record as a dispatch would.
record_lead() {
  mkdir -p "$(dirname "$(lead_rec)")"
  printf '%s\n' "$1" >"$(lead_rec)"
}

@test "claude resume attaches to the recorded lead session, not a newer role transcript" {
  setup_worker_wt 'roles: reviewer'
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  claude_transcript "$LEAD_ID" 600
  claude_transcript "$ROLE_ID" 5
  record_lead "claude $LEAD_ID"
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  grep -q "claude --resume $LEAD_ID --name iris" <(launch_log)
  run grep -c -- '--continue' <(launch_log)
  [ "$output" = 0 ]
  run grep -c -- '--session-id' <(launch_log)
  [ "$output" = 0 ]
  [ "$(jq -r 'select(.kind == "resume") | .continued' "$(bus_log)" | tail -1)" = true ]
  [ "$(cat "$(lead_rec)")" = "claude $LEAD_ID" ]
  [ "$(jq -r 'select(.kind == "resume") | .engine_session' "$(bus_log)" | tail -1)" = "$LEAD_ID" ]
}

@test "pi resume attaches to the recorded lead session by id" {
  setup_worker_wt 'roles: reviewer'
  sed -i -e 's/^engine: claude/engine: pi/' -e 's|^model: sonnet|model: openrouter/deepseek/deepseek-v4-flash|' "$WT/WORKER_TASK.md"
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  record_lead "pi $LEAD_ID"
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  grep -q -- "--thinking medium --session-id $LEAD_ID --append-system-prompt" <(launch_log)
  run grep -c -- '--continue' <(launch_log)
  [ "$output" = 0 ]
  grep -q 'this session has been resumed' <(launch_log)
  [ "$(jq -r 'select(.kind == "resume") | .continued' "$(bus_log)" | tail -1)" = true ]
  [ "$(cat "$(lead_rec)")" = "pi $LEAD_ID" ]
  [ "$(jq -r 'select(.kind == "resume") | .engine_session' "$(bus_log)" | tail -1)" = "$LEAD_ID" ]
}

@test "legacy claude worker with several transcripts relaunches fresh and records a new id" {
  setup_worker_wt 'roles: reviewer'
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  claude_transcript "$LEAD_ID" 600
  claude_transcript "$ROLE_ID" 5
  claude_transcript agent-a1b2c3 1
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  [[ "$output" == *"role panes"* ]]
  run grep -c -- '--continue' <(launch_log)
  [ "$output" = 0 ]
  grep -q 'Read SPEC.md and PLAN.md' <(launch_log)
  [ "$(jq -r 'select(.kind == "resume") | .continued' "$(bus_log)" | tail -1)" = false ]
  grep -qE -- "--effort medium --session-id $UUID_RE " <(launch_log)
  sid="$(grep -oE -- "--session-id $UUID_RE" <(launch_log) | head -1 | cut -d' ' -f2)"
  [ "$sid" != "$LEAD_ID" ]
  [ "$sid" != "$ROLE_ID" ]
  [ "$(cat "$(lead_rec)")" = "claude $sid" ]
}

@test "legacy solo claude worker continues even with one transcript" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  claude_transcript "$LEAD_ID" 600
  claude_transcript agent-a1b2c3 1
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  grep -q 'claude --continue --name iris' <(launch_log)
  run grep -c -- '--resume' <(launch_log)
  [ "$output" = 0 ]
  [ ! -e "$(lead_rec)" ]
}

@test "an engine switch to claude never adopts a lone transcript as the lead's" {
  setup_worker_wt 'roles: reviewer'
  sed -i -e 's/^engine: claude/engine: codex/' -e 's/^model: sonnet/model: gpt-5.6-sol/' "$WT/WORKER_TASK.md"
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  claude_transcript "$ROLE_ID" 600
  cd "$WT"
  run run_resume --agent claude --model sonnet
  [ "$status" -eq 0 ]
  run grep -c -- '--continue\|--resume' <(launch_log)
  [ "$output" = 0 ]
  grep -qE -- "--session-id $UUID_RE " <(launch_log)
  sid="$(grep -oE -- "--session-id $UUID_RE" <(launch_log) | head -1 | cut -d' ' -f2)"
  [ "$sid" != "$ROLE_ID" ]
  [ "$(cat "$(lead_rec)")" = "claude $sid" ]
}

@test "an engine switch to claude with no roles stamp and no roles.json never adopts a lone transcript" {
  setup_worker_wt
  sed -i -e 's/^engine: claude/engine: codex/' -e 's/^model: sonnet/model: gpt-5.6-sol/' "$WT/WORKER_TASK.md"
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  claude_transcript "$ROLE_ID" 600
  cd "$WT"
  run run_resume --agent claude --model sonnet
  [ "$status" -eq 0 ]
  run grep -c -- '--continue\|--resume' <(launch_log)
  [ "$output" = 0 ]
  sid="$(grep -oE -- "--session-id $UUID_RE" <(launch_log) | head -1 | cut -d' ' -f2)"
  [ -n "$sid" ]
  [ "$sid" != "$ROLE_ID" ]
  [ "$(cat "$(lead_rec)")" = "claude $sid" ]
}

@test "a roles.json beside the branch's artifacts marks role panes even without a roles stamp" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  mkdir -p "$TEST_REPO/.git/crew/artifacts/feat/7-a-thing"
  printf '{}\n' >"$TEST_REPO/.git/crew/artifacts/feat/7-a-thing/roles.json"
  claude_transcript "$ROLE_ID" 600
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  [[ "$output" == *"role panes"* ]]
  run grep -c -- '--continue\|--resume' <(launch_log)
  [ "$output" = 0 ]
  grep -qE -- "--session-id $UUID_RE " <(launch_log)
}

@test "a pending tombstone relaunches fresh and the launch overwrites it" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  claude_transcript "$ROLE_ID" 600
  record_lead 'pending -'
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  [[ "$output" == *"never launched"* ]]
  run grep -c -- '--continue\|--resume' <(launch_log)
  [ "$output" = 0 ]
  sid="$(grep -oE -- "--session-id $UUID_RE" <(launch_log) | head -1 | cut -d' ' -f2)"
  [ "$sid" != "$ROLE_ID" ]
  [ "$(cat "$(lead_rec)")" = "claude $sid" ]
}

@test "a grid lead with no record never resumes a lone transcript" {
  setup_worker_wt 'roles: reviewer'
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  claude_transcript "$ROLE_ID" 600
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  run grep -c -- '--continue\|--resume' <(launch_log)
  [ "$output" = 0 ]
  sid="$(grep -oE -- "--session-id $UUID_RE" <(launch_log) | head -1 | cut -d' ' -f2)"
  [ -n "$sid" ]
  [ "$sid" != "$ROLE_ID" ]
  [ "$(cat "$(lead_rec)")" = "claude $sid" ]
}

@test "a codex lead record names the engine and resumes --last for a solo worker" {
  setup_worker_wt
  sed -i -e 's/^engine: claude/engine: codex/' -e 's/^model: sonnet/model: gpt-5.6-sol/' "$WT/WORKER_TASK.md"
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  record_lead 'codex -'
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  grep -q 'codex resume --last' <(launch_log)
  [ "$(cat "$(lead_rec)")" = "codex -" ]
}

@test "a codex lead record with role panes relaunches fresh and keeps the record" {
  setup_worker_wt 'roles: reviewer'
  sed -i -e 's/^engine: claude/engine: codex/' -e 's/^model: sonnet/model: gpt-5.6-sol/' "$WT/WORKER_TASK.md"
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  record_lead 'codex -'
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  run grep -c -- 'resume --last' <(launch_log)
  [ "$output" = 0 ]
  [ "$(cat "$(lead_rec)")" = "codex -" ]
}

@test "a claude record with no id is malformed and relaunches fresh" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  claude_transcript "$ROLE_ID" 600
  record_lead 'claude -'
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  [[ "$output" == *"malformed"* ]]
  run grep -c -- '--continue\|--resume' <(launch_log)
  [ "$output" = 0 ]
  grep -qE "^claude $UUID_RE\$" "$(lead_rec)"
}

@test "legacy claude worker with no transcript still uses --continue and records nothing" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  grep -q 'claude --continue --name iris' <(launch_log)
  [ ! -e "$(lead_rec)" ]
}

@test "a malformed lead record relaunches fresh and never reaches the launch command" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  record_lead 'claude x;touch pwn'
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  [ ! -e "$WT/pwn" ]
  [ ! -e pwn ]
  run grep -c -F 'touch pwn' <(launch_log)
  [ "$output" = 0 ]
  run grep -c -- '--continue' <(launch_log)
  [ "$output" = 0 ]
  grep -q 'Read SPEC.md and PLAN.md' <(launch_log)
  grep -qE -- "--session-id $UUID_RE " <(launch_log)
  grep -qE "^claude $UUID_RE\$" "$(lead_rec)"
}

@test "a lead record for another engine relaunches fresh" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  claude_transcript "$LEAD_ID" 600
  record_lead "pi $LEAD_ID"
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  [[ "$output" == *"the lead ran pi, not claude"* ]]
  run grep -c -- '--continue\|--resume' <(launch_log)
  [ "$output" = 0 ]
  grep -q 'Read SPEC.md and PLAN.md' <(launch_log)
  grep -qE "^claude $UUID_RE\$" "$(lead_rec)"
}

@test "cross-engine resume records continued false and the new engine (#459)" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  sed -i -e 's/^engine: claude/engine: codex/' "$WT/WORKER_TASK.md"
  cd "$WT"
  run run_resume --agent claude
  [ "$status" -eq 0 ]
  [[ "$output" == *"the lead ran codex, not claude"* ]]
  row="$(jq -c 'select(.kind == "resume")' "$(bus_log)" | tail -1)"
  [ "$(jq -r .continued <<<"$row")" = false ]
  [ "$(jq -r .engine <<<"$row")" = claude ]
  [ "$(jq -r .prev_worker_id <<<"$row")" = 'worker:feat/7-a-thing#s1-99' ]
}

@test "a recorded claude session whose transcript is gone relaunches fresh" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  record_lead "claude $LEAD_ID"
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  run grep -c -- '--continue\|--resume' <(launch_log)
  [ "$output" = 0 ]
  grep -q 'Read SPEC.md and PLAN.md' <(launch_log)
  grep -qE -- "--session-id $UUID_RE " <(launch_log)
  [ "$(cat "$(lead_rec)")" != "claude $LEAD_ID" ]
}

@test "--fresh rewrites the lead record with a new id" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  claude_transcript "$LEAD_ID" 600
  record_lead "claude $LEAD_ID"
  cd "$WT"
  run run_resume --fresh
  [ "$status" -eq 0 ]
  grep -qE -- "--effort medium --session-id $UUID_RE " <(launch_log)
  sid="$(grep -oE -- "--session-id $UUID_RE" <(launch_log) | head -1 | cut -d' ' -f2)"
  [ "$sid" != "$LEAD_ID" ]
  [ "$(cat "$(lead_rec)")" = "claude $sid" ]
}

@test "--print reports the lead session and writes no record" {
  setup_worker_wt
  claude_transcript "$LEAD_ID" 600
  cd "$WT"
  run run_resume --print --fresh
  [ "$status" -eq 0 ]
  [ ! -e "$(lead_rec)" ]
  [ ! -e "$TEST_REPO/.git/crew/leads" ]
  record_lead "claude $LEAD_ID"
  run run_resume --print
  [ "$status" -eq 0 ]
  [[ "$output" == *"lead_session: $LEAD_ID"* ]]
  [[ "$output" == *"continue: true"* ]]
}

@test "codex --fresh replaces a stale lead record with one naming codex and no id" {
  setup_worker_wt
  sed -i -e 's/^engine: claude/engine: codex/' -e 's/^model: sonnet/model: gpt-5.6-sol/' "$WT/WORKER_TASK.md"
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  record_lead "claude $LEAD_ID"
  cd "$WT"
  run run_resume --fresh
  [ "$status" -eq 0 ]
  [ "$(cat "$(lead_rec)")" = "codex -" ]
}

@test "a symlinked leads dir is never written through" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  mkdir -p "$TEST_REPO/elsewhere"
  mkdir -p "$TEST_REPO/.git/crew"
  ln -s "$TEST_REPO/elsewhere" "$TEST_REPO/.git/crew/leads"
  cd "$WT"
  run run_resume --fresh
  [ "$status" -eq 0 ]
  [[ "$output" == *"symlink"* ]]
  [ -z "$(ls -A "$TEST_REPO/elsewhere")" ]
  grep -qE -- "--session-id $UUID_RE " <(launch_log)
}

# The reader must apply the writer's safety check: a symlinked leads dir is
# followed by a plain existence test, so a record planted in its target would
# attach resume to whatever session it names — here a role's.
@test "a symlinked leads dir is never read through: a role's session is not adopted" {
  setup_worker_wt 'roles: reviewer'
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  claude_transcript "$ROLE_ID" 600
  mkdir -p "$TEST_REPO/elsewhere/feat"
  printf 'claude %s\n' "$ROLE_ID" >"$TEST_REPO/elsewhere/feat/7-a-thing"
  mkdir -p "$TEST_REPO/.git/crew"
  ln -s "$TEST_REPO/elsewhere" "$TEST_REPO/.git/crew/leads"
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  [[ "$output" == *"malformed or unsafe"* ]]
  run grep -c -- "--resume $ROLE_ID" <(launch_log)
  [ "$output" = 0 ]
  grep -qE -- "--session-id $UUID_RE " <(launch_log)
}

# Same invariant when the symlink sits at the branch's parent component
# (leads/feat), not at leads/ itself.
@test "a symlinked leads parent component is never read through" {
  setup_worker_wt 'roles: reviewer'
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  claude_transcript "$ROLE_ID" 600
  mkdir -p "$TEST_REPO/.git/crew/leads"
  mkdir -p "$TEST_REPO/elsewhere"
  printf 'claude %s\n' "$ROLE_ID" >"$TEST_REPO/elsewhere/7-a-thing"
  ln -s "$TEST_REPO/elsewhere" "$TEST_REPO/.git/crew/leads/feat"
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  [[ "$output" == *"malformed or unsafe"* ]]
  run grep -c -- "--resume $ROLE_ID" <(launch_log)
  [ "$output" = 0 ]
  grep -qE -- "--session-id $UUID_RE " <(launch_log)
}

@test "writes a resume row naming both worker identities" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  DISPATCH_SESSION_ID=s2-100 run run_resume
  [ "$status" -eq 0 ]
  row="$(jq -c 'select(.kind == "resume")' "$(bus_log)" | tail -1)"
  [ "$(jq -r .crew_id <<<"$row")" = c1 ]
  [ "$(jq -r .branch <<<"$row")" = feat/7-a-thing ]
  [ "$(jq -r .worker_id <<<"$row")" = 'worker:feat/7-a-thing#s2-100' ]
  [ "$(jq -r .prev_worker_id <<<"$row")" = 'worker:feat/7-a-thing#s1-99' ]
  [ "$(jq -r .continued <<<"$row")" = true ]
  [ "$(jq -r .engine <<<"$row")" = claude ]
}

@test "--fresh records continued false" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume --fresh
  [ "$status" -eq 0 ]
  [ "$(jq -r 'select(.kind == "resume") | .continued' "$(bus_log)" | tail -1)" = false ]
}

@test "posts a working status under the NEW worker id" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  DISPATCH_SESSION_ID=s2-100 run run_resume
  [ "$status" -eq 0 ]
  grep -q 'status worker:feat/7-a-thing#s2-100 working resumed' "$STUB_LOG"
}

@test "updates worker_id in the task doc and leaves the body alone" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  DISPATCH_SESSION_ID=s2-100 run run_resume
  [ "$status" -eq 0 ]
  grep -qx 'worker_id: worker:feat/7-a-thing#s2-100' "$WT/WORKER_TASK.md"
  grep -qx 'the original body' "$WT/WORKER_TASK.md"
  grep -qx 'title: a thing' "$WT/WORKER_TASK.md"
}

@test "resume keeps every stamped Closes line of a bundled task (#615)" {
  setup_worker_wt 'Closes #8' 'Closes #9'
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  DISPATCH_SESSION_ID=s2-100 run run_resume
  [ "$status" -eq 0 ]
  run grep '^Closes ' "$WT/WORKER_TASK.md"
  [ "${lines[0]}" = 'Closes #7' ]
  [ "${lines[1]}" = 'Closes #8' ]
  [ "${lines[2]}" = 'Closes #9' ]
  [ "${#lines[@]}" -eq 3 ]
}

@test "stamps resume: true when the header has no resume field yet (#112)" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  grep -qx 'resume: true' "$WT/WORKER_TASK.md"
  run grep -c '^resume: ' "$WT/WORKER_TASK.md"
  [ "$output" = 1 ]
}

@test "rewrites an existing resume field to true rather than duplicating it" {
  setup_worker_wt 'resume: false'
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  grep -qx 'resume: true' "$WT/WORKER_TASK.md"
  run grep -c '^resume: ' "$WT/WORKER_TASK.md"
  [ "$output" = 1 ]
}

@test "inserting resume: true leaves a body line that happens to read resume: false alone" {
  setup_worker_wt
  printf 'the resume field is documented as\nresume: false\nby default\n' >>"$WT/WORKER_TASK.md"
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  # exactly one header-position "resume: true" line, and the body's own
  # "resume: false" line is untouched, not overwritten by the header rewrite.
  run grep -n '^resume: ' "$WT/WORKER_TASK.md"
  [ "$status" -eq 0 ]
  [ "$(printf '%s\n' "$output" | wc -l)" = 2 ]
  grep -qx 'resume: false' "$WT/WORKER_TASK.md"
}

@test "runs solo when the crew has no registered dispatcher" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  [[ "$output" == *"solo"* ]]
  run grep -c 'crew msg' "$STUB_LOG"
  [ "$status" -ne 0 ]
  grep -qx 'dispatcher_pane: %3' "$WT/WORKER_TASK.md"
}

@test "reattaches to a live dispatcher: retargets the pane and messages it" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  mkdir -p "$TEST_REPO/.git/crew/crews/c1"
  printf '%s\n' "$$" >"$TEST_REPO/.git/crew/crews/c1/pid"
  printf '%%77\n' >"$TEST_REPO/.git/crew/crews/c1/pane"
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  [[ "$output" == *"reattached"* ]]
  grep -qx 'dispatcher_pane: %77' "$WT/WORKER_TASK.md"
  grep -q 'msg .* dispatcher:c1' "$STUB_LOG"
}

@test "runs solo when the registered dispatcher pid is dead" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  mkdir -p "$TEST_REPO/.git/crew/crews/c1"
  printf '999999999\n' >"$TEST_REPO/.git/crew/crews/c1/pid"
  printf '%%77\n' >"$TEST_REPO/.git/crew/crews/c1/pane"
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  [[ "$output" == *"solo"* ]]
  grep -qx 'dispatcher_pane: %3' "$WT/WORKER_TASK.md"
}

# #461: the liveness probe was a bare `kill -0`. A live dispatcher owned by
# another uid makes that fail EPERM, which reads dead and strands the crew as
# solo. EPERM is proof the process exists, so resume must still reattach.
@test "reattaches when the recorded pid only signals EPERM (another uid) (#461)" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  mkdir -p "$TEST_REPO/.git/crew/crews/c1"
  printf '999999999\n' >"$TEST_REPO/.git/crew/crews/c1/pid"
  printf '%%77\n' >"$TEST_REPO/.git/crew/crews/c1/pane"
  # `kill` is a bash builtin, so a PATH stub cannot intercept it; an exported
  # function overriding the builtin is what the probe sees (#450 does the same).
  kill() {
    printf 'bash: kill: (%s) - Operation not permitted\n' "$2" >&2
    return 1
  }
  export -f kill
  cd "$WT"
  run run_resume
  unset -f kill
  [ "$status" -eq 0 ]
  [[ "$output" == *"reattached"* ]]
  grep -qx 'dispatcher_pane: %77' "$WT/WORKER_TASK.md"
  grep -q 'msg .* dispatcher:c1' "$STUB_LOG"
}

# #461: a recorded dead pid recycled by an unrelated process succeeds on
# `kill -0` and reads live. The pid file predates the process now holding its
# number, which cannot be the dispatcher; resume must treat it as dead.
@test "runs solo when the recorded pid is a later recycled process (#461)" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  mkdir -p "$TEST_REPO/.git/crew/crews/c1"
  sleep 30 &
  rec_pid=$!
  printf '%s\n' "$rec_pid" >"$TEST_REPO/.git/crew/crews/c1/pid"
  printf '%%77\n' >"$TEST_REPO/.git/crew/crews/c1/pane"
  touch -t 202001010000 "$TEST_REPO/.git/crew/crews/c1/pid"
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  [[ "$output" == *"solo"* ]]
  grep -qx 'dispatcher_pane: %3' "$WT/WORKER_TASK.md"
  kill "$rec_pid" 2>/dev/null || true
}

_resume_esc_seed() { # [failed-session] [failed-ts] [engine] [model] [tier]
  local fs="${1:-s1-99}" fts="${2:-200}" eng="${3:-claude}" mdl="${4:-sonnet}" tr="${5:-standard}"
  crew_dir="$TEST_REPO/.git/crew"
  mkdir -p "$crew_dir"
  jq -nc --arg b "feat/7-a-thing" --arg eng "$eng" --arg mdl "$mdl" --arg tr "$tr" '
    {ts: 100, kind:"dispatch", branch:$b, session:"s1-99",
     engine:$eng, model:$mdl, tier:$tr, effort:"medium",
     shape:"", task_kind:"implement", title:"a thing", plan:"required", resume:false}
  ' >>"$crew_dir/events.jsonl"
  jq -nc --arg b "feat/7-a-thing" --arg s "$fs" --argjson ts "$fts" '
    {ts: $ts, kind:"status",
     from:("worker:"+$b+"#"+$s),
     body:{state:"failed", detail:"test failure"}}
  ' >>"$crew_dir/events.jsonl"
}

# The dispatch stub accepts everything, so a refused escalation shows up as a
# precheck that was NOT handed --ignore-map (the real gate would exit 1).
_precheck_ignores_map() { grep 'resume precheck' "$STUB_LOG" | grep -q -- '--ignore-map'; }

@test "resume escalation: --model gpt-5.6-sol succeeds after prior failed with matching dispatch" {
  setup_worker_wt
  sed -i -e 's/^engine: claude/engine: codex/' -e 's/^model: sonnet/model: gpt-5.6-terra/' "$WT/WORKER_TASK.md"
  _resume_esc_seed s1-99 200 codex gpt-5.6-terra standard
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  DISPATCH_PROFILE=work DISPATCH_ENGINES="claude codex cursor pi" run run_resume --model gpt-5.6-sol
  [ "$status" -eq 0 ]
  _precheck_ignores_map
  run jq -r 'select(.kind == "resume") | .escalated_from' "$crew_dir/events.jsonl"
  [ "$output" = "terra" ]
  grep -qx 'model: gpt-5.6-sol' "$WT/WORKER_TASK.md"
  grep -qx 'escalated_from: terra' "$WT/WORKER_TASK.md"
  # The header now records the escalated model, so a later plain resume
  # relaunches on it rather than silently falling back to terra.
  run ! grep -qx 'model: gpt-5.6-terra' "$WT/WORKER_TASK.md"
}

@test "resume escalation: a failure posted only by the resumed session still escalates" {
  setup_worker_wt
  sed -i -e 's/^engine: claude/engine: codex/' -e 's/^model: sonnet/model: gpt-5.6-terra/' "$WT/WORKER_TASK.md"
  _resume_esc_seed s2-99 200 codex gpt-5.6-terra standard
  # s1 never failed; s2 exists only in a resume row.
  jq -c 'select(.kind == "status") | .from = "worker:feat/7-a-thing#s2-99"' "$crew_dir/events.jsonl" >"$crew_dir/x"
  jq -c 'select(.kind == "dispatch")' "$crew_dir/events.jsonl" >"$crew_dir/y"
  jq -nc '{ts:150, kind:"resume", branch:"feat/7-a-thing", session:"s2-99", engine:"codex", model:"gpt-5.6-terra"}' >>"$crew_dir/y"
  cat "$crew_dir/x" >>"$crew_dir/y"
  mv "$crew_dir/y" "$crew_dir/events.jsonl"
  rm -f "$crew_dir/x"
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  DISPATCH_PROFILE=work DISPATCH_ENGINES="claude codex cursor pi" run run_resume --model gpt-5.6-sol
  [ "$status" -eq 0 ]
  _precheck_ignores_map
  run jq -r 'select(.kind == "resume" and .session != null) | .escalated_from // "none"' "$crew_dir/events.jsonl"
  [[ "$output" == *"terra"* ]]
}

@test "resume escalation: a spoofed failed status does not unlock --model gpt-5.6-sol" {
  setup_worker_wt
  sed -i -e 's/^engine: claude/engine: codex/' -e 's/^model: sonnet/model: gpt-5.6-terra/' "$WT/WORKER_TASK.md"
  _resume_esc_seed s-nonexistent 200 codex gpt-5.6-terra standard
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  DISPATCH_PROFILE=work DISPATCH_ENGINES="claude codex cursor pi" run run_resume --model gpt-5.6-sol
  [ "$status" -eq 0 ]
  run ! _precheck_ignores_map
  grep -qx 'model: gpt-5.6-terra' "$WT/WORKER_TASK.md"
}

@test "resume escalation: an already-escalated branch refuses a second --model gpt-5.6-sol" {
  setup_worker_wt
  sed -i -e 's/^engine: claude/engine: codex/' -e 's/^model: sonnet/model: gpt-5.6-terra/' "$WT/WORKER_TASK.md"
  _resume_esc_seed s1-99 200 codex gpt-5.6-terra standard
  jq -nc '{ts:250, kind:"resume", branch:"feat/7-a-thing", session:"s2-99", engine:"codex", model:"gpt-5.6-sol", escalated_from:"terra"}' >>"$crew_dir/events.jsonl"
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  DISPATCH_PROFILE=work DISPATCH_ENGINES="claude codex cursor pi" run run_resume --model gpt-5.6-sol
  [ "$status" -eq 0 ]
  run ! _precheck_ignores_map
}

@test "resume escalation: a branch that failed and later finished does not escalate" {
  setup_worker_wt
  sed -i -e 's/^engine: claude/engine: codex/' -e 's/^model: sonnet/model: gpt-5.6-terra/' "$WT/WORKER_TASK.md"
  _resume_esc_seed s1-99 200 codex gpt-5.6-terra standard
  jq -nc '{ts:300, kind:"status", from:"worker:feat/7-a-thing#s1-99", body:{state:"done"}}' >>"$crew_dir/events.jsonl"
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  DISPATCH_PROFILE=work DISPATCH_ENGINES="claude codex cursor pi" run run_resume --model gpt-5.6-sol
  [ "$status" -eq 0 ]
  run ! _precheck_ignores_map
}

@test "resume escalation: a trivial-tier worker cannot reach fable" {
  setup_worker_wt
  sed -i 's/^tier: standard/tier: trivial/' "$WT/WORKER_TASK.md"
  _resume_esc_seed
  sed -i 's/"tier":"standard"/"tier":"trivial"/' "$crew_dir/events.jsonl"
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume --model fable
  [ "$status" -eq 0 ]
  run ! _precheck_ignores_map
}

@test "resume escalation: --model opus after a failed standard sonnet is an in-row hop" {
  setup_worker_wt
  _resume_esc_seed
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume --model opus
  [ "$status" -eq 0 ]
  run ! _precheck_ignores_map
}

@test "resume escalation: a header claiming standard cannot borrow a trivial-tier failure" {
  setup_worker_wt
  sed -i -e 's/^engine: claude/engine: codex/' -e 's/^model: sonnet/model: gpt-5.6-terra/' "$WT/WORKER_TASK.md"
  _resume_esc_seed s1-99 200 codex gpt-5.6-luna trivial
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  DISPATCH_PROFILE=work DISPATCH_ENGINES="claude codex cursor pi" run run_resume --model gpt-5.6-sol
  [ "$status" -eq 0 ]
  run ! _precheck_ignores_map
}

@test "resume escalation: an opus id that only shares the target as a prefix is refused" {
  setup_worker_wt
  sed -i 's/^engine: claude/engine: cursor/; s/^model: sonnet/model: cursor-grok-4.6-medium/' "$WT/WORKER_TASK.md"
  _resume_esc_seed
  sed -i 's/"engine":"claude"/"engine":"cursor"/; s/"model":"sonnet"/"model":"cursor-grok-4.6-medium"/' "$crew_dir/events.jsonl"
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  DISPATCH_PROFILE=work DISPATCH_ENGINES="claude codex cursor pi" run run_resume --model cursor-grok-4.6-highfoo
  [ "$status" -eq 0 ]
  run ! _precheck_ignores_map
}

@test "re-arms the stall watchdog on the resumed pane" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  wait_for_log 'stall-watch worker:feat/7-a-thing#s[0-9]+-[0-9]+ --pane %8 --engine claude'
}

@test "resume --ignore-budget re-arms the stall watchdog with --no-budget" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume --ignore-budget
  [ "$status" -eq 0 ]
  wait_for_log 'stall-watch worker:feat/7-a-thing#s[0-9]+-[0-9]+ --pane %8 --engine claude --no-budget$'
}

@test "resume without --ignore-budget re-arms the stall watchdog without --no-budget" {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  run run_resume
  [ "$status" -eq 0 ]
  wait_for_log 'stall-watch worker:feat/7-a-thing#s[0-9]+-[0-9]+ --pane %8 --engine claude'
  run ! grep -q -- '--no-budget' "$STUB_LOG"
}

@test "roster-render: resume hands over its own TMUX_PANE, never dispatcher_pane" {
  setup_worker_wt
  sed -i 's/^dispatcher_pane: .*/dispatcher_pane: %99/' "$WT/WORKER_TASK.md"
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  cd "$WT"
  TMUX_PANE=%5 run run_resume
  [ "$status" -eq 0 ]
  wait_for_log 'roster-render --crew c1 --pane %5( --no-open)? --detach$'
  run ! grep -E 'roster-render.*%99' "$STUB_LOG"
}

# A live registered dispatcher owns the renderer's pane; a human resuming from a
# worker's own pane must not retarget it there.
_roster_live_dispatcher() {
  setup_worker_wt
  stub_tmux_with_pane_at_wt '@4' '%8' iris
  mkdir -p "$TEST_REPO/.git/crew/crews/c1"
  printf '%s\n' "$$" >"$TEST_REPO/.git/crew/crews/c1/pid"
  printf '%%3\n' >"$TEST_REPO/.git/crew/crews/c1/pane"
  cd "$WT"
}

@test "roster-render: resume with a live dispatcher drops a TMUX_PANE that is not its pane" {
  _roster_live_dispatcher
  TMUX_PANE=%5 run run_resume
  [ "$status" -eq 0 ]
  [[ "$output" == *"reattached"* ]]
  wait_for_log 'roster-render --crew c1( --no-open)? --detach$'
  run ! grep -E 'roster-render.*--pane' "$STUB_LOG"
}

@test "roster-render: resume from the live dispatcher's own pane hands it over" {
  _roster_live_dispatcher
  TMUX_PANE=%3 run run_resume
  [ "$status" -eq 0 ]
  wait_for_log 'roster-render --crew c1 --pane %3( --no-open)? --detach$'
}

# _seed_worker_stream <ts-ms> — a stream for crew c1 armed in the worker repo
# (TEST_REPO, where $WT is a linked worktree): a live pid plus a stream.tick,
# the exact state `crew stream --status` reads from <repo>/crew/crews/c1/.
_seed_worker_stream() {
  local cdir="$TEST_REPO/.git/crew/crews/c1"
  mkdir -p "$cdir/stream.lock.d"
  printf '%s\n' "$$" >"$cdir/stream.lock.d/pid"
  jq -nc --argjson pid "$$" --argjson ts "$1" '{pid:$pid, ts:$ts, park:300}' >"$cdir/stream.tick"
}

# _resume_cross_repo_fixture <dispatcher-pane-path> — a resume whose dispatcher
# pane reports <dispatcher-pane-path> as its cwd. `crew stream` is routed to the
# real crew.sh so the liveness predicate is exercised, and the tmux stub answers
# #{pane_current_path}; setup() unsets TMUX_PANE, so it is exported here.
_resume_cross_repo_fixture() {
  local pane_path="${1:-$TEST_REPO}"
  setup_worker_wt
  cd "$WT"
  git init -q -b main "$BATS_TEST_TMPDIR/other-repo"
  export TMUX_PANE=%9
  export STUB_PANE_PATH="$pane_path"
  cat >"$STUB_DIR/tmux" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
case "$*" in
*'#{pane_current_path}'*) printf '%s\n' "$STUB_PANE_PATH" ;;
*'#{client_width}'*) printf '%s\n' '80 24 on' ;;
esac
if [ "$1" = new-window ]; then
  printf '%s %s\n' '%1' '%1'
fi
exit 0
EOF
  chmod +x "$STUB_DIR/tmux"
  cat >"$STUB_DIR/crew" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
if [ "$1" = pi-agent-dir ]; then exec bash -euo pipefail "$CREW_REAL" pi-agent-dir; fi
if [ "$1" = engine-cmd ]; then
  c="${2#.}"
  c="${c%-wrapped}"
  case "$c" in
  claude | codex | cursor-agent | node | pi) exit 0 ;;
  esac
  exit 1
fi
if [ "$1" = stream ]; then exec bash -euo pipefail "$CREW_REAL" "$@"; fi
exit 0
EOF
  chmod +x "$STUB_DIR/crew"
}

@test "cross-repo: resume prints the worker-repo stream command (#420)" {
  _resume_cross_repo_fixture "$BATS_TEST_TMPDIR/other-repo"
  run run_resume
  [ "$status" -eq 0 ]
  worker_top="$(git -C "$WT" rev-parse --show-toplevel)"
  [[ "$output" == *"arm/adjust the lane: cd $worker_top && crew stream --crew c1"* ]]
}

@test "cross-repo: resume omits the hint when the dispatcher checkout is the worker repo (#420)" {
  _resume_cross_repo_fixture "$TEST_REPO"
  run run_resume
  [ "$status" -eq 0 ]
  [[ "$output" != *"arm/adjust the lane"* ]]
}

@test "cross-repo: resume omits the hint when a live stream is already armed (#420)" {
  _resume_cross_repo_fixture "$BATS_TEST_TMPDIR/other-repo"
  _seed_worker_stream "$(jq -nc 'now*1000|floor')"
  run run_resume
  [ "$status" -eq 0 ]
  [[ "$output" != *"arm/adjust the lane"* ]]
}

@test "cross-repo: a stale stream does not suppress the resume hint (#420)" {
  _resume_cross_repo_fixture "$BATS_TEST_TMPDIR/other-repo"
  _seed_worker_stream "$(jq -nc '(now-700)*1000|floor')"
  run run_resume
  [ "$status" -eq 0 ]
  worker_top="$(git -C "$WT" rev-parse --show-toplevel)"
  [[ "$output" == *"arm/adjust the lane: cd $worker_top && crew stream --crew c1"* ]]
}

@test "cross-repo: resume omits the hint outside tmux (#420)" {
  _resume_cross_repo_fixture "$BATS_TEST_TMPDIR/other-repo"
  unset TMUX_PANE
  run run_resume
  [ "$status" -eq 0 ]
  [[ "$output" != *"arm/adjust the lane"* ]]
}

# seed_dispatch_row <branch> <name> <host> [also_closes-json] — a bus row as
# dispatch writes it, in the repo $CREW_REAL resolves from the cwd.
seed_dispatch_row() {
  mkdir -p "$TEST_REPO/.git/crew"
  jq -nc --arg b "$1" --arg n "$2" --arg h "$3" --argjson a "${4:-[]}" \
    '{ts: 1, crew_id: "c1", kind: "dispatch", branch: $b, name: $n, host: $h, also_closes: $a}' \
    >>"$TEST_REPO/.git/crew/events.jsonl"
}

@test "target: #N, branch and codename resume the same worker as cwd-bound, from the main checkout" {
  setup_worker_wt
  seed_dispatch_row feat/7-a-thing iris "$(uname -n)"
  cd "$WT"
  run run_resume --print
  [ "$status" -eq 0 ]
  expected="$output"
  cd "$TEST_REPO"
  for t in '#7' 7 feat/7-a-thing iris 'worker:feat/7-a-thing#s1-99'; do
    run run_resume "$t" --print
    [ "$status" -eq 0 ]
    [ "$output" = "$expected" ]
  done
}

@test "target: a live worker is refused with its window and nothing launches" {
  setup_worker_wt
  seed_dispatch_row feat/7-a-thing iris "$(uname -n)"
  cd "$TEST_REPO"
  : >"$STUB_LOG"
  STUB_WHERE_LIVE=1 run run_resume '#7'
  [ "$status" -eq 1 ]
  [[ "$output" == *"still alive"* ]]
  [[ "$output" == *"tmux switch-client -t %9"* ]]
  ! grep -q '^new-window\|^send-keys' "$STUB_LOG"
}

@test "target: a record from another host is refused, naming the host" {
  setup_worker_wt
  seed_dispatch_row feat/7-a-thing iris otherbox
  cd "$TEST_REPO"
  run run_resume '#7'
  [ "$status" -eq 1 ]
  [[ "$output" == *"host 'otherbox'"* ]]
}

@test "target: an ambiguous target lists its candidates" {
  setup_worker_wt
  seed_dispatch_row feat/7-a-thing iris "$(uname -n)"
  seed_dispatch_row fix/7-another iris "$(uname -n)"
  cd "$TEST_REPO"
  run run_resume '#7'
  [ "$status" -eq 1 ]
  [[ "$output" == *"ambiguous"* ]]
  [[ "$output" == *"feat/7-a-thing"* ]]
  [[ "$output" == *"fix/7-another"* ]]
  run run_resume iris
  [ "$status" -eq 1 ]
  [[ "$output" == *"ambiguous"* ]]
}

@test "target: an unknown target is refused" {
  setup_worker_wt
  seed_dispatch_row feat/7-a-thing iris "$(uname -n)"
  cd "$TEST_REPO"
  run run_resume '#999'
  [ "$status" -eq 1 ]
  [[ "$output" == *"no worker matches '#999'"* ]]
}

@test "target: a plain word the bus does not know stays an extra prompt" {
  setup_worker_wt
  seed_dispatch_row feat/7-a-thing iris "$(uname -n)"
  cd "$WT"
  run run_resume continue --print
  [ "$status" -eq 0 ]
  [[ "$output" == *continue* ]]
}

@test "target: a worktree whose record no longer matches is still refused" {
  setup_worker_wt
  seed_dispatch_row feat/7-a-thing iris "$(uname -n)"
  git -C "$WT" checkout -q -b feat/other
  cd "$TEST_REPO"
  run run_resume '#7'
  [ "$status" -eq 1 ]
  [[ "$output" == *"does not match the dispatcher's record"* ]]
}

@test "target: a legacy row with no host or codename still resumes" {
  setup_worker_wt
  mkdir -p "$TEST_REPO/.git/crew"
  jq -nc '{ts: 1, crew_id: "c1", kind: "dispatch", branch: "feat/7-a-thing"}' >"$TEST_REPO/.git/crew/events.jsonl"
  cd "$TEST_REPO"
  run run_resume '#7' --print
  [ "$status" -eq 0 ]
}

@test "target: an unknown branch-shaped word is refused, not taken as a prompt" {
  setup_worker_wt
  seed_dispatch_row feat/7-a-thing iris "$(uname -n)"
  cd "$WT"
  run run_resume feat/other --print
  [ "$status" -eq 1 ]
  [[ "$output" == *"no worker matches 'feat/other'"* ]]
}
