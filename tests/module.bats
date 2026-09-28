bats_require_minimum_version 1.5.0 # `run --separate-stderr`

# Every test here runs a real `nix build`/`nix eval`/`nix store` against the
# shared local flake -- including one `nix build` of 9 outputs at once and a
# fresh <nixpkgs> resolution. On a cold cache (true on every fresh CI runner)
# that races internally on the git-fetcher cache, independent of bats-level
# concurrency -- it fails the same way run alone as run in parallel with
# itself, and only stops once the cache is warm. CI gives this file its own
# lane, run once before anything else, to guarantee that (see ci.yml).
# BATS_NO_PARALLELIZE_WITHIN_FILE stays here as a second layer for any
# direct/ad-hoc `bats --jobs` invocation that includes this file alongside
# others.
export BATS_NO_PARALLELIZE_WITHIN_FILE=true

# setup_file runs once per file, before any test in it. Every test below used
# to `nix build`/`nix eval` on its own -- each invocation is a full flake
# evaluation, and the nine-way build alone dominates a worker's edit loop.
# Build and evaluate exactly once here instead, and have every test read the
# results out of $BATS_FILE_TMPDIR (the one directory bats keeps alive for
# the whole file, unlike $BATS_TEST_TMPDIR which is per-test).
setup_file() {
  local root="$BATS_TEST_DIRNAME/.."

  nix build --no-link --print-out-paths \
    "$root#crew" "$root#dispatch" "$root#dispatch-resume" "$root#dispatcher" \
    "$root#refresh-scores" "$root#refresh-budget" "$root#refresh-models" "$root#pr-watch" \
    "$root#reviewer-roster" "$root#permission-check" \
    >"$BATS_FILE_TMPDIR/out-paths"

  # The module's own eval, through a real `lib.evalModules` (not a bare call
  # of the module function) so option defaults, merging and the mkIf'd config
  # body apply exactly as they would for a real home-manager user. A stub
  # module declares only the handful of home-manager options this module
  # touches (home.*, xdg.configFile, config.lib.file.mkOutOfStoreSymlink) and
  # a stand-in for lib.hm.dag.entryAfter, so evaluating needs no home-manager
  # input. Emitted as one JSON object -- five config shapes plus the option
  # list all fit in one `nix eval`, which is what keeps this to a single
  # invocation.
  cat >"$BATS_FILE_TMPDIR/eval-expr.nix" <<'NIXEOF'
let
  root = @ROOT@;
  self = builtins.getFlake (toString root);
  nixlib = (import <nixpkgs> {}).lib;
  # hm.dag.entryAfter is the only home-manager lib helper the module calls;
  # stub it to the DAG-free shape the module's own comments already treat it
  # as ({ inherit data; }) rather than pull in home-manager as a dependency.
  extLib = nixlib.extend (_: _: {hm.dag.entryAfter = _: data: {inherit data;};});
  pkgs = import <nixpkgs> {};

  # Declares just enough of the home-manager option surface for the module
  # to write into for real: home.packages/sessionVariables/file/activation,
  # xdg.configFile, and the config.lib.file.mkOutOfStoreSymlink helper.
  stub = {lib, ...}: {
    options = {
      home.packages = lib.mkOption {
        type = lib.types.listOf lib.types.package;
        default = [];
      };
      home.sessionVariables = lib.mkOption {
        type = lib.types.attrsOf lib.types.str;
        default = {};
      };
      home.file = lib.mkOption {
        type = lib.types.attrsOf lib.types.raw;
        default = {};
      };
      home.activation = lib.mkOption {
        type = lib.types.attrsOf lib.types.raw;
        default = {};
      };
      xdg.configFile = lib.mkOption {
        type = lib.types.attrsOf lib.types.raw;
        default = {};
      };
      lib = lib.mkOption {
        type = lib.types.attrsOf lib.types.raw;
        default = {};
      };
    };
    config.lib.file.mkOutOfStoreSymlink = p: "out-of-store:" + p;
  };

  eval = cfg:
    (extLib.evalModules {
      modules = [
        stub
        self.homeManagerModules.default
        {
          _module.args.pkgs = pkgs;
          programs.dispatcher = cfg;
        }
      ];
    }).config;

  optionNames = builtins.attrNames
    (extLib.evalModules {
      modules = [
        stub
        self.homeManagerModules.default
        {
          _module.args.pkgs = pkgs;
          programs.dispatcher.enable = false;
        }
      ];
    }).options.programs.dispatcher;

  findPkg = name: pkgList: nixlib.findFirst (p: p.name == name) null pkgList;

  # sessionVariables/fileNames/activationNames/packageNames/xdgConfigFile are
  # forced (not just referenced) so a typo'd option, a bad importJSON path or
  # a broken interpolation fails the eval here, not on a real rebuild.
  mkResult = cfg: let
    r = eval cfg;
  in
    builtins.deepSeq [r.home.sessionVariables r.home.file r.home.activation r.xdg.configFile] {
      sessionVariables = r.home.sessionVariables;
      fileNames = builtins.attrNames r.home.file;
      activationNames = builtins.attrNames r.home.activation;
      packageNames = map (p: p.name) r.home.packages;
      xdgConfigFile = r.xdg.configFile;
      locked = builtins.fromJSON (builtins.readFile r.programs.dispatcher.lockedSettingsFile);
    };

  mkDrvs = cfg: let
    r = eval cfg;
  in {
    dispatchConfigDrv = (findPkg "dispatch-config" r.home.packages).drvPath;
    dispatchDrv = (findPkg "dispatch" r.home.packages).drvPath;
  };

  fullCfg = {
    enable = true;
    profile = "work";
    engines = ["claude" "codex" "cursor" "pi"];
    grantRoots = ["/a/git" "/b/src"];
    repoTrackers."factify-inc/mono" = "linear:ENG";
    orgTrackers."factify-inc" = "linear:ENG";
    openrouter = {
      monthlyTarget = 50;
      keyFile = "/run/agenix/openrouter";
    };
    userSettings = "/home/u/cfg/settings.json";
  };

  minimalCfg = {enable = true;};

  cursorlessCfg = {
    enable = true;
    engines = ["claude" "pi"];
    grantRoots = [];
    openrouter = {
      monthlyTarget = null;
      keyFile = null;
    };
  };

  floatCfg = {
    enable = true;
    engines = ["claude" "pi"];
    grantRoots = [];
    openrouter = {
      monthlyTarget = 12.5;
      keyFile = null;
    };
  };

  codexOnlyCfg = {
    enable = true;
    engines = ["codex"];
  };
in {
  options = optionNames;
  full =
    mkResult fullCfg
    // {cursorSkills = builtins.replaceStrings ["\n"] [" "] (eval fullCfg).home.activation.dispatcherCursorSkills.data;};
  minimal = mkResult minimalCfg // mkDrvs minimalCfg;
  cursorless = mkResult cursorlessCfg;
  float = mkResult floatCfg;
  codexOnly = mkResult codexOnlyCfg // mkDrvs codexOnlyCfg;
  # `engines = []` must be a type error, not a silently-accepted value that
  # bakes `"engines": []` into the locked settings file (dispatch-config dies
  # on that at runtime).
  emptyEnginesRejected =
    !(builtins.tryEval
      (builtins.deepSeq (eval {
          enable = true;
          engines = [];
        })
        .programs
        .dispatcher
        .engines
        true))
    .success;
}
NIXEOF
  sed -i "s|@ROOT@|$root|" "$BATS_FILE_TMPDIR/eval-expr.nix"
  nix eval --impure --json --file "$BATS_FILE_TMPDIR/eval-expr.nix" \
    >"$BATS_FILE_TMPDIR/eval-out.json"

  # Build the module's own baked dispatch-config/dispatch for two engine
  # shapes (engines left unset vs. set to ["codex"]) so the integration tests
  # below run the real binaries, not just inspect the eval.
  nix build --no-link --print-out-paths \
    "$(jq -r '.minimal.dispatchConfigDrv' "$BATS_FILE_TMPDIR/eval-out.json")^out" \
    "$(jq -r '.minimal.dispatchDrv' "$BATS_FILE_TMPDIR/eval-out.json")^out" \
    "$(jq -r '.codexOnly.dispatchConfigDrv' "$BATS_FILE_TMPDIR/eval-out.json")^out" \
    "$(jq -r '.codexOnly.dispatchDrv' "$BATS_FILE_TMPDIR/eval-out.json")^out" \
    >"$BATS_FILE_TMPDIR/baked-out-paths"
}

setup() {
  ROOT="$BATS_TEST_DIRNAME/.."
  EVAL="$BATS_FILE_TMPDIR/eval-out.json"
  # `nix build --print-out-paths` prints one line per installable, in the same
  # order they were given on the command line (see setup_file) -- pinning that
  # assumption here since a future nix reordering them would go undetected: a
  # wrong OUT_* mapping still greps/deepSeqs against a real derivation.
  {
    read -r OUT_CREW
    read -r OUT_DISPATCH
    read -r OUT_DISPATCH_RESUME
    read -r OUT_DISPATCHER
    read -r OUT_REFRESH_SCORES
    read -r OUT_REFRESH_BUDGET
    read -r OUT_REFRESH_MODELS
    read -r OUT_PR_WATCH
    read -r OUT_REVIEWER_ROSTER
    read -r OUT_PERMISSION_CHECK
  } <"$BATS_FILE_TMPDIR/out-paths"
  {
    read -r OUT_MINIMAL_DISPATCH_CONFIG
    read -r OUT_MINIMAL_DISPATCH
    read -r OUT_CODEX_DISPATCH_CONFIG
    read -r OUT_CODEX_DISPATCH
  } <"$BATS_FILE_TMPDIR/baked-out-paths"
}

@test "every package builds" {
  # setup_file already ran the build; a failure there fails the whole file
  # before any test runs. This just proves every out path came back.
  for out in "$OUT_CREW" "$OUT_DISPATCH" "$OUT_DISPATCH_RESUME" "$OUT_DISPATCHER" \
    "$OUT_REFRESH_SCORES" "$OUT_REFRESH_BUDGET" "$OUT_REFRESH_MODELS" "$OUT_PR_WATCH" \
    "$OUT_REVIEWER_ROSTER" "$OUT_PERMISSION_CHECK" \
    "$OUT_MINIMAL_DISPATCH_CONFIG" "$OUT_MINIMAL_DISPATCH" \
    "$OUT_CODEX_DISPATCH_CONFIG" "$OUT_CODEX_DISPATCH"; do
    [ -n "$out" ]
    [ -e "$out" ]
  done
}

@test "the protocol placeholder is substituted in dispatch" {
  run grep -c '@protocolDir@' "$OUT_DISPATCH/bin/dispatch"
  [ "$output" = "0" ]
}

@test "the protocol placeholder is substituted in dispatcher" {
  run grep -c '@protocolDir@' "$OUT_DISPATCHER/bin/dispatcher"
  [ "$output" = "0" ]
}

@test "the protocol placeholder is substituted in dispatch-resume" {
  run grep -c '@protocolDir@' "$OUT_DISPATCH_RESUME/bin/dispatch-resume"
  [ "$output" = "0" ]
}

@test "the substituted protocol dir actually contains the protocols" {
  # A substituted-but-wrong path would leave every dispatched worker unable to
  # find its protocol, and nothing else would notice until a live run.
  dir="$(grep -o '/nix/store/[^"}]*' "$OUT_DISPATCH/bin/dispatch" | grep -i protocol | head -1)"
  [ -n "$dir" ]
  [ -f "$dir/WORKER_PROTOCOL.md" ]
  [ -f "$dir/DISPATCHER_PROTOCOL.md" ]
  # dispatch --review resolves this one at dispatch time and aborts without it.
  [ -f "$dir/REVIEW_TASK.md" ]
  [ -f "$dir/EVIDENCE_REVIEW.md" ]
}

@test "the skills placeholder is substituted in dispatch and dispatch-resume" {
  # pi is handed this path with --skill; unsubstituted it is not a directory,
  # so the launch would silently drop the harness skills instead of failing.
  run grep -c '@skillsDir@' "$OUT_DISPATCH/bin/dispatch"
  [ "$output" = "0" ]
  run grep -c '@skillsDir@' "$OUT_DISPATCH_RESUME/bin/dispatch-resume"
  [ "$output" = "0" ]
}

@test "the substituted skills dir actually contains the harness skills" {
  dir="$(grep -o '/nix/store/[^"}]*' "$OUT_DISPATCH/bin/dispatch" | grep -- '-skills$' | head -1)"
  [ -n "$dir" ]
  [ -f "$dir/spec-plan-critic/SKILL.md" ]
  [ -f "$dir/deslop/SKILL.md" ]
}

@test "the cross-repo hint lib placeholder is substituted in dispatch and dispatch-resume" {
  # The lane hint is a sourced shared lib (#398/#420); an unsubstituted token is
  # not a readable path, so the guarded call site silently drops the hint.
  run grep -c '@crossRepoHintLib@' "$OUT_DISPATCH/bin/dispatch"
  [ "$output" = "0" ]
  run grep -c '@crossRepoHintLib@' "$OUT_DISPATCH_RESUME/bin/dispatch-resume"
  [ "$output" = "0" ]
}

@test "the substituted cross-repo hint lib exists and defines the helper" {
  lib="$(grep -o '/nix/store/[^"}]*cross-repo-hint[^"}]*' "$OUT_DISPATCH/bin/dispatch" | head -1)"
  [ -n "$lib" ]
  [ -f "$lib" ]
  grep -q '^cross_repo_hint() {' "$lib"
}

@test "the worktree-git lib placeholder is substituted in crew, dispatch and dispatch-resume" {
  # The anchored-git helper (#539) is a sourced shared lib; an unsubstituted
  # token is not a readable path, so reap/dispatch/resume would abort under
  # `set -e` the moment they source it.
  run grep -c '@worktreeGitLib@' "$OUT_CREW/bin/crew"
  [ "$output" = "0" ]
  run grep -c '@worktreeGitLib@' "$OUT_DISPATCH/bin/dispatch"
  [ "$output" = "0" ]
  run grep -c '@worktreeGitLib@' "$OUT_DISPATCH_RESUME/bin/dispatch-resume"
  [ "$output" = "0" ]
}

@test "the grant-check lib placeholder is substituted in dispatch, dispatch-resume and permission-check" {
  # The --add-dir grant validator (#536) is a sourced shared lib; an
  # unsubstituted token is not a readable path, so any of the three would
  # abort under `set -e` the moment they source it.
  run grep -c '@grantCheckLib@' "$OUT_DISPATCH/bin/dispatch"
  [ "$output" = "0" ]
  run grep -c '@grantCheckLib@' "$OUT_DISPATCH_RESUME/bin/dispatch-resume"
  [ "$output" = "0" ]
  run grep -c '@grantCheckLib@' "$OUT_PERMISSION_CHECK/bin/permission-check"
  [ "$output" = "0" ]
}

@test "the protocol revision placeholder is substituted in dispatch and dispatch-resume" {
  run grep -c '@protocolRev@' "$OUT_DISPATCH/bin/dispatch"
  [ "$output" = "0" ]
  run grep -c '@protocolRev@' "$OUT_DISPATCH_RESUME/bin/dispatch-resume"
  [ "$output" = "0" ]
}

@test "the built scripts and the built protocol dir carry the same revision" {
  # The baked @protocolRev@ (guard marker, #184/#193) must equal the runtime
  # hash of the baked default protocol dir: sorted `name:sha256;` entries,
  # sha256 of the concatenation, first 16 hex chars — the rule flake.nix uses
  # to bake it and _check_protocol_rev uses to recompute it. One algorithm,
  # two implementations, pinned here: a protocol edit that drifts them apart
  # (or a checkout that no longer matches the build) fails this test. There is
  # no PROTOCOL_REV file to compare against any more — the guard hashes the
  # directory itself.
  dir="$(grep -o '/nix/store/[^"}]*' "$OUT_DISPATCH/bin/dispatch" | grep -i protocol | head -1)"
  [ -n "$dir" ]
  [ ! -f "$dir/PROTOCOL_REV" ]
  rev_dir="$(
    names=()
    shopt -s dotglob nullglob
    for f in "$dir"/*; do
      [ -f "$f" ] || continue
      names+=("$(basename "$f")")
    done
    shopt -u dotglob nullglob
    if [ ${#names[@]} -gt 0 ]; then
      mapfile -t names < <(printf '%s\n' "${names[@]}" | LC_ALL=C sort)
    fi
    entries=""
    for name in "${names[@]}"; do
      entries+="${name}:$(sha256sum "$dir/$name" | cut -d' ' -f1);"$'\n'
    done
    printf '%s' "$entries" | tr -d '\n' | sha256sum | cut -d' ' -f1 | cut -c1-16
  )"
  [ -n "$rev_dir" ]
  rev_dispatch="$(grep -oE 'stamped_rev="[0-9a-f]{16}"' "$OUT_DISPATCH/bin/dispatch" | head -1 | sed -n 's/stamped_rev="\([0-9a-f]\{16\}\)"/\1/p')"
  rev_resume="$(grep -oE 'stamped_rev="[0-9a-f]{16}"' "$OUT_DISPATCH_RESUME/bin/dispatch-resume" | head -1 | sed -n 's/stamped_rev="\([0-9a-f]\{16\}\)"/\1/p')"
  [ -n "$rev_dispatch" ]
  [ "$rev_dispatch" = "$rev_dir" ]
  [ "$rev_resume" = "$rev_dispatch" ]
}

@test "the runtime and Nix rules agree on an edge-case dir (dotfile, prefix names)" {
  # The two implementations must be byte-identical on more than the current
  # six-file tree: a naive line-sort or a non-dotglob glob silently diverges
  # the day a dotfile or a prefix-named pair lands in the protocol dir, making
  # the built-in default refuse every dispatch with an inexplicable hash
  # mismatch. Pin that here with the three shapes that break naive rules:
  # '.hidden' (glob '*/*' misses dotfiles), 'X'/'X1' (line-sort puts X1 first
  # because '1' < ':'; attrNames puts X first), and an EMPTY dir (a raw
  # `printf '%s\n' "${names[@]}"` with zero elements emits a blank line, so an
  # unsorted mapfile would inject an empty name — guards omitted, it hashes
  # `sha256sum "$dir/"` and diverges from Nix's sha256("")).
  scratch="$BATS_TEST_TMPDIR/edge-protocols"
  mkdir -p "$scratch"
  printf 'a' >"$scratch/.hidden"
  printf 'b' >"$scratch/X"
  printf 'c' >"$scratch/X1"
  printf 'd' >"$scratch/GRID"
  printf 'e' >"$scratch/GRID_PROTOCOL.md"
  empty="$BATS_TEST_TMPDIR/edge-protocols-empty"
  mkdir -p "$empty"

  for d in "$scratch" "$empty"; do
    rev_nix="$(nix eval --impure --raw --expr "
      let
        dir = builtins.toPath \"$d\";
        files = builtins.attrNames (builtins.readDir dir);
      in builtins.substring 0 16 (builtins.hashString \"sha256\"
        (builtins.concatStringsSep \"\" (map
          (n: \"\${n}:\${builtins.hashFile \"sha256\" (dir + \"/\${n}\")};\")
          files)))")"
    [ -n "$rev_nix" ]

    rev_bash="$(
      names=()
      shopt -s dotglob nullglob
      for f in "$d"/*; do
        [ -f "$f" ] || continue
        names+=("$(basename "$f")")
      done
      shopt -u dotglob nullglob
      if [ ${#names[@]} -gt 0 ]; then
        mapfile -t names < <(printf '%s\n' "${names[@]}" | LC_ALL=C sort)
      fi
      entries=""
      for name in "${names[@]}"; do
        entries+="${name}:$(sha256sum "$d/$name" | cut -d' ' -f1);"$'\n'
      done
      printf '%s' "$entries" | tr -d '\n' | sha256sum | cut -d' ' -f1 | cut -c1-16
    )"
    [ -n "$rev_bash" ]
    [ "$rev_nix" = "$rev_bash" ]
  done
}

@test "crew does not retain the protocols as a runtime closure reference" {
  # `crew` intentionally reads its source directly; unlike dispatch and
  # dispatcher it must not gain a runtime dependency on the protocol tree.
  protocols="$(nix store add-path "$ROOT/adapters/core/protocols")"
  run nix-store -q --requisites "$OUT_CREW"
  [ "$status" -eq 0 ]
  [[ "$output" != *"$protocols"* ]]
}

@test "the module declares its options" {
  run jq -e '(.options | index("enable")) != null' "$EVAL"
  [ "$status" -eq 0 ]
  run jq -e '(.options | index("profile")) != null' "$EVAL"
  [ "$status" -eq 0 ]
  run jq -e '(.options | index("lockedSettingsFile")) != null' "$EVAL"
  [ "$status" -eq 0 ]
  run jq -e '(.options | index("userSettings")) != null' "$EVAL"
  [ "$status" -eq 0 ]
}

@test "the module declares the engines option" {
  for name in engines grantRoots repoTrackers orgTrackers; do
    run jq -e --arg n "$name" '(.options | index($n)) != null' "$EVAL"
    [ "$status" -eq 0 ]
  done
}

@test "engines = [] is rejected by the option type" {
  run jq -e '.emptyEnginesRejected == true' "$EVAL"
  [ "$status" -eq 0 ]
}

@test "the module declares the openrouter option" {
  run jq -e '(.options | index("openrouter")) != null' "$EVAL"
  [ "$status" -eq 0 ]
}

@test "the module's config body evaluates, and wires the protocol dir for real" {
  # `nix flake check` reports homeManagerModules as UNCHECKED, so an eval error
  # here would otherwise surface only in a consumer's rebuild.
  run jq -e '.full.sessionVariables.DISPATCHER_PROTOCOL_DIR | endswith("/adapters/core/protocols")' "$EVAL"
  [ "$status" -eq 0 ]
  run jq -e '.full.sessionVariables.DISPATCHER_REVIEWERS_DIR | endswith("/adapters/core/reviewers")' "$EVAL"
  [ "$status" -eq 0 ]
  run jq -e '.full.sessionVariables.DISPATCHER_CRITICS_DIR | endswith("/adapters/core/critics")' "$EVAL"
  [ "$status" -eq 0 ]
  run jq -e '.full.sessionVariables.DISPATCHER_SKILLS_DIR | endswith("/adapters/core/skills")' "$EVAL"
  [ "$status" -eq 0 ]
  # Every CLI the module claims to install, resolved from the flake — a package
  # that isn't in `packages` fails the eval outright, not a grep.
  for pkg in crew dispatch dispatch-resume dispatcher refresh-scores refresh-budget refresh-models pr-watch reviewer-roster permission-check dispatch-config; do
    run jq -e --arg p "$pkg" '(.full.packageNames | index($p)) != null' "$EVAL"
    [ "$status" -eq 0 ]
  done
}

@test "the locked settings layer carries the routing options, only when set" {
  run jq -e '.full.locked.grantRoots == ["/a/git", "/b/src"]' "$EVAL"
  [ "$status" -eq 0 ]
  run jq -e '.full.locked.profile == "work"' "$EVAL"
  [ "$status" -eq 0 ]
  run jq -e '.full.locked.engines == ["claude", "codex", "cursor", "pi"]' "$EVAL"
  [ "$status" -eq 0 ]
  run jq -e '.full.locked.repoTrackers == {"factify-inc/mono": "linear:ENG"}' "$EVAL"
  [ "$status" -eq 0 ]
  run jq -e '.full.locked.orgTrackers == {"factify-inc": "linear:ENG"}' "$EVAL"
  [ "$status" -eq 0 ]
  run jq -e '.full.locked.openrouter.keyFile == "/run/agenix/openrouter"' "$EVAL"
  [ "$status" -eq 0 ]
  run jq -e '.full.locked.openrouter.monthlyUsd == 50' "$EVAL"
  [ "$status" -eq 0 ]

  # Every routing option left unset: the locked layer holds only the two
  # always-emitted keys, so the user settings file (or the base default)
  # governs everything else.
  run jq -e '.minimal.locked == {"grantRoots": [], "profile": "personal"}' "$EVAL"
  [ "$status" -eq 0 ]

  run jq -e '.float.locked.openrouter.monthlyUsd == 12.5' "$EVAL"
  [ "$status" -eq 0 ]
}

@test "no settings var is exported any more, only the four directory ones" {
  for cfgname in full minimal cursorless float; do
    run jq -e --arg c "$cfgname" '[.[$c].sessionVariables | keys[] | select(test("^DISPATCH_"))] | length == 0' "$EVAL"
    [ "$status" -eq 0 ]
    for var in DISPATCHER_PROTOCOL_DIR DISPATCHER_REVIEWERS_DIR DISPATCHER_CRITICS_DIR DISPATCHER_SKILLS_DIR; do
      run jq -e --arg c "$cfgname" --arg v "$var" '.[$c].sessionVariables | has($v)' "$EVAL"
      [ "$status" -eq 0 ]
    done
  done
}

@test "userSettings symlinks the settings file out of the store, unset installs no symlink" {
  run jq -e '.full.xdgConfigFile["dispatcher/settings.json"].source == "out-of-store:/home/u/cfg/settings.json"' "$EVAL"
  [ "$status" -eq 0 ]
  run jq -e '.minimal.xdgConfigFile | has("dispatcher/settings.json") | not' "$EVAL"
  [ "$status" -eq 0 ]
}

@test "engines left unset installs the codex and cursor artifacts" {
  run jq -e '(.minimal.activationNames | index("dispatcherCodexPlugin")) != null' "$EVAL"
  [ "$status" -eq 0 ]
  run jq -e '(.minimal.activationNames | index("dispatcherCursorSkills")) != null' "$EVAL"
  [ "$status" -eq 0 ]
  run jq -e '[.minimal.fileNames[] | select(startswith(".cursor/"))] | length > 0' "$EVAL"
  [ "$status" -eq 0 ]
}

@test "a roster without cursor installs no cursor artifacts" {
  run jq -e '[.cursorless.fileNames[] | select(startswith(".cursor/"))] | length == 0' "$EVAL"
  [ "$status" -eq 0 ]
  run jq -e '(.cursorless.activationNames | index("dispatcherCursorSkills")) == null' "$EVAL"
  [ "$status" -eq 0 ]
}

@test "a roster without codex installs no codex plugin activation" {
  run jq -e '(.cursorless.activationNames | index("dispatcherCodexPlugin")) == null' "$EVAL"
  [ "$status" -eq 0 ]
}

@test "the codex plugin is copied as a real dir, never symlinked" {
  # Codex loads plugins only from a real directory under ~/.codex/plugins/cache.
  # A symlinked tree reports "installed, enabled" in `codex plugin list` while
  # its skills never reach the model — so cp -rL is load-bearing. Scoped to the
  # codex activation block: the cursor-skills activation below it intentionally
  # uses ln -sfn, since ~/.cursor/skills is a shared namespace it must not
  # claim wholesale (#130).
  run grep -F 'cp -rL' "$ROOT/nix/hm-module.nix"
  [ "$status" -eq 0 ]
  # A renamed/moved activation attribute would make this extraction match
  # zero lines and silently disarm the guard below — assert it isn't empty
  # first, so that failure mode fails loudly instead of passing green.
  codex_block="$(awk '/dispatcherCodexPlugin =/{f=1} f{print; if (/^ *\/\//) exit}' "$ROOT/nix/hm-module.nix")"
  [ -n "$codex_block" ]
  run grep -cE 'mkOutOfStoreSymlink|ln -s' <<<"$codex_block"
  [ "$output" = "0" ]
}

@test "cursor skills are symlinked in, not claimed as a whole directory" {
  # ~/.cursor/skills is a shared namespace with other producers (#130) — a
  # whole-directory `home.file` source there conflicts the moment another
  # module also populates it. Individual skills must be linked in instead.
  run grep -cE '"\.cursor/skills"\s*=\s*\{' "$ROOT/nix/hm-module.nix"
  [ "$output" = "0" ]
  # Scoped to the dispatcherCursorSkills activation block (same extraction
  # style as the codex test above) so this can't pass on an unrelated ln -sfn
  # elsewhere while the actual symlink activation was dropped.
  skills_block="$(awk '/dispatcherCursorSkills =/{f=1} f{print; if (/^ *$/) exit}' "$ROOT/nix/hm-module.nix")"
  [ -n "$skills_block" ]
  run grep -F 'ln -sfn' <<<"$skills_block"
  [ "$status" -eq 0 ]
  run grep -F '.cursor/skills' <<<"$skills_block"
  [ "$status" -eq 0 ]
}

@test "cursor skill links are enumerated from the adapter dir, not hard-coded" {
  run jq -r '.full.cursorSkills' "$EVAL"
  [ "$status" -eq 0 ]
  [[ "$output" == *'/adapters/cursor/skills/spec-plan-critic" "$skills_dir/spec-plan-critic"'* ]]
  [[ "$output" == *'/adapters/cursor/skills/deslop" "$skills_dir/deslop"'* ]]
}

@test "the reviewers placeholder is substituted in reviewer-roster" {
  run grep -c '@reviewersDir@' "$OUT_REVIEWER_ROSTER/bin/reviewer-roster"
  [ "$output" = "0" ]
}

@test "the root placeholders are substituted in permission-check" {
  run grep -cE '@(protocol|skills|reviewers|critics)Dir@' "$OUT_PERMISSION_CHECK/bin/permission-check"
  [ "$output" = "0" ]
}

@test "reviewer-roster carries yq-go and jq in its closure" {
  yq_path="$(grep -o '/nix/store/[^"}]*' "$OUT_REVIEWER_ROSTER/bin/reviewer-roster" | grep 'yq-go' | head -1)"
  [ -n "$yq_path" ]
  jq_path="$(grep -o '/nix/store/[^"}]*' "$OUT_REVIEWER_ROSTER/bin/reviewer-roster" | grep -- '-jq-' | head -1)"
  [ -n "$jq_path" ]
}

@test "reviewer-roster resolves the harness roster with no env override" {
  export GIT_CONFIG_GLOBAL=/dev/null
  repo="$BATS_TEST_TMPDIR/smoke-repo"
  mkdir -p "$repo"
  git -C "$repo" init -q -b main
  git -C "$repo" config user.email test@example.com
  git -C "$repo" config user.name test
  echo seed >"$repo/seed.txt"
  git -C "$repo" add seed.txt
  git -C "$repo" commit -q -m seed

  run env -u DISPATCHER_REVIEWERS_DIR "$OUT_REVIEWER_ROSTER/bin/reviewer-roster" --base HEAD --repo "$repo" --default HEAD
  [ "$status" -eq 0 ]

  want="$(find "$ROOT/adapters/core/reviewers" -maxdepth 1 -name '*.md' | wc -l | tr -d ' ')"
  [ "$(jq '.reviewers | length' <<<"$output")" -eq "$want" ]
  [ "$(jq '[.reviewers[] | select(.source != "harness")] | length' <<<"$output")" -eq 0 ]
}

@test "built consumers bake the settings resolver" {
  for bin in "$OUT_DISPATCH/bin/dispatch" "$OUT_DISPATCH_RESUME/bin/dispatch-resume" \
    "$OUT_DISPATCHER/bin/dispatcher" "$OUT_REFRESH_BUDGET/bin/refresh-budget" \
    "$OUT_PERMISSION_CHECK/bin/permission-check"; do
    run grep -c '@dispatchConfig@' "$bin"
    [ "$output" = "0" ]
    run grep -q '/bin/dispatch-config' "$bin"
    [ "$status" -eq 0 ]
  done

  run env -u DISPATCH_CONFIG_BIN -u DISPATCH_ENGINES -u DISPATCH_LOCKED_SETTINGS \
    XDG_CONFIG_HOME="$BATS_TEST_TMPDIR/cfg" "$OUT_DISPATCH/bin/dispatch" --engines
  [ "$status" -eq 0 ]
}

# #606: the crew package carries dispatch-config, so `crew rate` resolves its
# burn table with DISPATCH_CONFIG_BIN unset and nothing on the inherited PATH.
@test "the crew package resolves its burn table with no dispatch-config on PATH" {
  repo="$BATS_TEST_TMPDIR/crew-606-repo"
  mkdir -p "$repo"
  git -C "$repo" init -q -b main
  git -C "$repo" config user.email test@example.com
  git -C "$repo" config user.name test
  echo seed >"$repo/seed.txt"
  git -C "$repo" add seed.txt
  git -C "$repo" commit -q -m seed

  logf="$(git -C "$repo" rev-parse --path-format=absolute --git-common-dir)/crew/events.jsonl"
  mkdir -p "$(dirname "$logf")"
  jq -nc '{ts:1000, crew_id:"c1", kind:"dispatch", branch:"b", engine:"claude", model:"opus", tier:"deep", effort:"high", title:"t"}' >"$logf"
  jq -nc '{ts:601000, crew_id:"c1", from:"worker:b", to:"dispatcher:c1", kind:"status", body:{state:"done"}}' >>"$logf"

  # `crew rate` sweeps the bus of the cwd, so run inside the fixture repo.
  cd "$repo"
  # PATH=/nonexistent so the only resolver available is the one the wrapper's
  # own runtimeInputs put on PATH; -u DISPATCH_CONFIG_BIN so nothing exported
  # can stand in for it. XDG_CONFIG_HOME/HOME are isolated so a host settings
  # file's burnClasses override cannot flip the asserted class (dispatch-config
  # merges ${XDG_CONFIG_HOME:-$HOME/.config}/dispatcher/settings.json).
  run --separate-stderr env -u DISPATCH_CONFIG_BIN -u _BURN_SETTINGS \
    PATH=/nonexistent XDG_DATA_HOME="$BATS_TEST_TMPDIR/data" \
    XDG_CONFIG_HOME="$BATS_TEST_TMPDIR/cfg" HOME="$BATS_TEST_TMPDIR/home" \
    "$OUT_CREW/bin/crew" rate
  [ "$status" -eq 0 ]
  [[ "$stderr" != *"dispatch-config unavailable"* ]]

  # The class is the real one the baked table assigns opus@high, not the
  # null an empty settings resolve would leave behind.
  run jq -s -c 'group_by(.run_id) | map(max_by(.swept_at)) | .[0].cost_class' \
    "$BATS_TEST_TMPDIR/data/crew/ratings.jsonl"
  [ "$status" -eq 0 ]
  [ "$output" = '"premium"' ]
}

# --- Integration: the module's baked dispatch-config/dispatch, run for real ---

@test "a module install with engines left unset resolves the user settings file's engines" {
  mkdir -p "$BATS_TEST_TMPDIR/cfg/dispatcher"
  echo '{"engines": ["cursor"]}' >"$BATS_TEST_TMPDIR/cfg/dispatcher/settings.json"

  run env -u DISPATCH_ENGINES -u DISPATCH_LOCKED_SETTINGS \
    XDG_CONFIG_HOME="$BATS_TEST_TMPDIR/cfg" "$OUT_MINIMAL_DISPATCH_CONFIG/bin/dispatch-config"
  [ "$status" -eq 0 ]
  [ "$(jq -c '.engines' <<<"$output")" = '["cursor"]' ]
}

@test "a module install with engines set wins over the user settings file's engines" {
  mkdir -p "$BATS_TEST_TMPDIR/cfg/dispatcher"
  echo '{"engines": ["cursor"]}' >"$BATS_TEST_TMPDIR/cfg/dispatcher/settings.json"

  run env -u DISPATCH_ENGINES -u DISPATCH_LOCKED_SETTINGS \
    XDG_CONFIG_HOME="$BATS_TEST_TMPDIR/cfg" "$OUT_CODEX_DISPATCH_CONFIG/bin/dispatch-config"
  [ "$status" -eq 0 ]
  [ "$(jq -c '.engines' <<<"$output")" = '["codex"]' ]
}

@test "a baked dispatch-config ignores DISPATCH_LOCKED_SETTINGS, with a stderr notice" {
  echo '{"engines": ["pi"]}' >"$BATS_TEST_TMPDIR/other-locked.json"

  run --separate-stderr env -u DISPATCH_ENGINES XDG_CONFIG_HOME="$BATS_TEST_TMPDIR/cfg" \
    DISPATCH_LOCKED_SETTINGS="$BATS_TEST_TMPDIR/other-locked.json" \
    "$OUT_CODEX_DISPATCH_CONFIG/bin/dispatch-config"
  [ "$status" -eq 0 ]
  [ "$(jq -c '.engines' <<<"$output")" = '["codex"]' ]
  [[ "$stderr" == *"ignoring DISPATCH_LOCKED_SETTINGS"* ]]
}

@test "a module install's dispatch bakes that same install's dispatch-config" {
  run grep -F "$OUT_CODEX_DISPATCH_CONFIG/bin/dispatch-config" "$OUT_CODEX_DISPATCH/bin/dispatch"
  [ "$status" -eq 0 ]
}
