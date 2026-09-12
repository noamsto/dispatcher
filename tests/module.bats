# Every test here runs a real `nix build`/`nix eval`/`nix store` against the
# shared local flake -- including one `nix build` of 8 outputs at once and a
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
# evaluation, and the eight-way build alone dominates a worker's edit loop.
# Build and evaluate exactly once here instead, and have every test read the
# results out of $BATS_FILE_TMPDIR (the one directory bats keeps alive for
# the whole file, unlike $BATS_TEST_TMPDIR which is per-test).
setup_file() {
  local root="$BATS_TEST_DIRNAME/.."

  nix build --no-link --print-out-paths \
    "$root#crew" "$root#dispatch" "$root#dispatch-resume" "$root#dispatcher" \
    "$root#refresh-scores" "$root#refresh-budget" "$root#refresh-models" "$root#pr-watch" \
    >"$BATS_FILE_TMPDIR/out-paths"

  # The two eval tests below force different config shapes (options only vs.
  # the full activated config), so they can't share one expression -- but
  # both still fit in one `nix eval`, so evaluate both here and split the
  # result on a newline. `--raw` just prints the string verbatim, so an
  # embedded "\n" is a safe separator: nix's own output never contains one.
  printf '%s\n' "
    let
      self = builtins.getFlake (toString $root);
      nixlib = (import <nixpkgs> {}).lib;
      lib = nixlib // { hm.dag.entryAfter = _: data: { inherit data; }; };
      pkgs = import <nixpkgs> {};

      optionsApplied = self.homeManagerModules.default {
        config = { programs.dispatcher = { enable = false; profile = \"personal\"; }; };
        inherit lib pkgs;
      };
      optionNames = builtins.concatStringsSep \",\" (builtins.attrNames optionsApplied.options.programs.dispatcher);

      # home-manager extends lib with lib.hm; stub the single helper the
      # module uses so config can be forced without taking a home-manager
      # dependency. Forcing sessionVariables + file + activation is what
      # catches a typo'd option, a bad importJSON path, or a broken
      # interpolation. The package list is read by name instead --
      # deepSeq on a derivation recurses through its self-referential
      # output attrs and never finishes.
      configApplied = self.homeManagerModules.default {
        config = { programs.dispatcher = { enable = true; profile = \"work\"; }; };
        inherit lib pkgs;
      };
      c = configApplied.config.content;
      configLine = builtins.deepSeq [c.home.sessionVariables c.home.file c.home.activation]
        \"\${c.home.sessionVariables.DISPATCH_PROFILE}|\${builtins.concatStringsSep \",\" (map (p: p.name) c.home.packages)}|\${c.home.sessionVariables.DISPATCHER_PROTOCOL_DIR}|\${c.home.sessionVariables.DISPATCHER_REVIEWERS_DIR}|\${c.home.sessionVariables.DISPATCHER_CRITICS_DIR}\";
    in optionNames + \"\n\" + configLine
  " >"$BATS_FILE_TMPDIR/eval-expr.nix"
  nix eval --impure --raw --file "$BATS_FILE_TMPDIR/eval-expr.nix" 2>/dev/null \
    >"$BATS_FILE_TMPDIR/eval-out"
}

setup() {
  ROOT="$BATS_TEST_DIRNAME/.."
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
  } <"$BATS_FILE_TMPDIR/out-paths"
  EVAL_OPTIONS="$(sed -n '1p' "$BATS_FILE_TMPDIR/eval-out")"
  EVAL_CONFIG="$(sed -n '2p' "$BATS_FILE_TMPDIR/eval-out")"
}

@test "every package builds" {
  # setup_file already ran the build; a failure there fails the whole file
  # before any test runs. This just proves every out path came back.
  for out in "$OUT_CREW" "$OUT_DISPATCH" "$OUT_DISPATCH_RESUME" "$OUT_DISPATCHER" \
    "$OUT_REFRESH_SCORES" "$OUT_REFRESH_BUDGET" "$OUT_REFRESH_MODELS" "$OUT_PR_WATCH"; do
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

@test "crew does not retain the protocols as a runtime closure reference" {
  # `crew` intentionally reads its source directly; unlike dispatch and
  # dispatcher it must not gain a runtime dependency on the protocol tree.
  protocols="$(nix store add-path "$ROOT/adapters/core/protocols")"
  run nix-store -q --requisites "$OUT_CREW"
  [ "$status" -eq 0 ]
  [[ "$output" != *"$protocols"* ]]
}

@test "the module declares its options" {
  [[ "$EVAL_OPTIONS" == *"enable"* ]]
  [[ "$EVAL_OPTIONS" == *"profile"* ]]
}

@test "the module's config body evaluates, and wires the protocol dir for real" {
  # `nix flake check` reports homeManagerModules as UNCHECKED, so an eval error
  # here would otherwise surface only in a consumer's rebuild. setup_file's
  # deepSeq forces the config body, not just the options -- that's what
  # actually catches a typo'd option, a bad importJSON path, or a broken
  # interpolation.
  #
  # Assert the wiring, not the flavour of path it resolves to: whether `self`
  # lands in the store or stays a source path depends on how the flake was
  # evaluated (a CI checkout differs from a local dev tree), and that is not the
  # behaviour under test. Removing either export still fails here — a missing
  # attribute makes setup_file's eval error, which fails the whole file.
  [[ "$EVAL_CONFIG" == work\|* ]]
  # Every CLI the module claims to install, resolved from the flake — a package
  # that isn't in `packages` fails the eval outright, not a grep.
  [[ "$EVAL_CONFIG" == *"crew,dispatch,dispatch-resume,dispatcher,refresh-scores,refresh-budget,refresh-models,pr-watch"* ]]
  [[ "$EVAL_CONFIG" == */adapters/core/protocols\|*/adapters/core/reviewers\|*/adapters/core/critics ]]
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
  codex_block="$(awk '/activation\.dispatcherCodexPlugin/{f=1} f{print; if (/^ *$/) exit}' "$ROOT/nix/hm-module.nix")"
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
  skills_block="$(awk '/activation\.dispatcherCursorSkills/{f=1} f{print; if (/^ *$/) exit}' "$ROOT/nix/hm-module.nix")"
  [ -n "$skills_block" ]
  run grep -F 'ln -sfn' <<<"$skills_block"
  [ "$status" -eq 0 ]
  run grep -F '.cursor/skills' <<<"$skills_block"
  [ "$status" -eq 0 ]
}
