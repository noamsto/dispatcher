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

setup() {
  ROOT="$BATS_TEST_DIRNAME/.."
}

@test "every package builds" {
  run nix build --no-link "$ROOT#crew" "$ROOT#dispatch" "$ROOT#dispatch-resume" \
    "$ROOT#dispatcher" "$ROOT#refresh-scores" "$ROOT#refresh-budget" \
    "$ROOT#refresh-models" "$ROOT#pr-watch"
  [ "$status" -eq 0 ]
}

@test "the protocol placeholder is substituted in dispatch" {
  out="$(nix build --no-link --print-out-paths "$ROOT#dispatch")"
  run grep -c '@protocolDir@' "$out/bin/dispatch"
  [ "$output" = "0" ]
}

@test "the protocol placeholder is substituted in dispatcher" {
  out="$(nix build --no-link --print-out-paths "$ROOT#dispatcher")"
  run grep -c '@protocolDir@' "$out/bin/dispatcher"
  [ "$output" = "0" ]
}

@test "the protocol placeholder is substituted in dispatch-resume" {
  out="$(nix build --no-link --print-out-paths "$ROOT#dispatch-resume")"
  run grep -c '@protocolDir@' "$out/bin/dispatch-resume"
  [ "$output" = "0" ]
}

@test "the substituted protocol dir actually contains the protocols" {
  # A substituted-but-wrong path would leave every dispatched worker unable to
  # find its protocol, and nothing else would notice until a live run.
  out="$(nix build --no-link --print-out-paths "$ROOT#dispatch")"
  dir="$(grep -o '/nix/store/[^"}]*' "$out/bin/dispatch" | grep -i protocol | head -1)"
  [ -n "$dir" ]
  [ -f "$dir/WORKER_PROTOCOL.md" ]
  [ -f "$dir/DISPATCHER_PROTOCOL.md" ]
  # dispatch --review resolves this one at dispatch time and aborts without it.
  [ -f "$dir/REVIEW_TASK.md" ]
}

@test "crew does not retain the protocols as a runtime closure reference" {
  # `crew` intentionally reads its source directly; unlike dispatch and
  # dispatcher it must not gain a runtime dependency on the protocol tree.
  protocols="$(nix store add-path "$ROOT/adapters/core/protocols")"
  out="$(nix build --no-link --print-out-paths "$ROOT#crew")"
  run nix-store -q --requisites "$out"
  [ "$status" -eq 0 ]
  [[ "$output" != *"$protocols"* ]]
}

# Evaluate a nix expression from a file, returning stdout only.
#
# Two reasons not to inline `nix eval --expr`: bats' `run` merges stderr into
# $output, and `nix eval --impure` emits warnings in some environments (a CI
# checkout does, a clean local tree does not) that would prefix the value and
# break an anchored match. A file also avoids nesting three levels of quotes.
nix_eval() {
  printf '%s\n' "$1" >"$BATS_TEST_TMPDIR/expr.nix"
  nix eval --impure --raw --file "$BATS_TEST_TMPDIR/expr.nix" 2>/dev/null
}

@test "the module declares its options" {
  run nix_eval "
    let
      self = builtins.getFlake (toString $ROOT);
      lib = (import <nixpkgs> {}).lib;
      pkgs = import <nixpkgs> {};
      applied = self.homeManagerModules.default {
        config = { programs.dispatcher = { enable = false; profile = \"personal\"; }; };
        inherit lib pkgs;
      };
    in builtins.concatStringsSep \",\" (builtins.attrNames applied.options.programs.dispatcher)
  "
  [ "$status" -eq 0 ]
  [[ "$output" == *"enable"* ]]
  [[ "$output" == *"profile"* ]]
}

@test "the module's config body evaluates, and wires the protocol dir for real" {
  # `nix flake check` reports homeManagerModules as UNCHECKED, so an eval error
  # here would otherwise surface only in a consumer's rebuild. Forcing `options`
  # alone does NOT catch that — this forces the config body.
  #
  # home-manager extends lib with lib.hm; stub the single helper the module uses
  # so config can be forced without taking a home-manager dependency. Forcing
  # sessionVariables + file + activation is what catches a typo'd option, a bad
  # importJSON path, or a broken interpolation. The package list is read by name
  # instead — `deepSeq` on a derivation recurses through its self-referential
  # output attrs and never finishes.
  #
  # Returns the resolved value rather than grepping the source, so it proves the
  # variable is actually wired into sessionVariables — not merely that the token
  # appears somewhere in the file.
  run nix_eval "
    let
      self = builtins.getFlake (toString $ROOT);
      nixlib = (import <nixpkgs> {}).lib;
      lib = nixlib // { hm.dag.entryAfter = _: data: { inherit data; }; };
      pkgs = import <nixpkgs> {};
      applied = self.homeManagerModules.default {
        config = { programs.dispatcher = { enable = true; profile = \"work\"; }; };
        inherit lib pkgs;
      };
      c = applied.config.content;
    in
      builtins.deepSeq [c.home.sessionVariables c.home.file c.home.activation]
        \"\${c.home.sessionVariables.DISPATCH_PROFILE}|\${builtins.concatStringsSep \",\" (map (p: p.name) c.home.packages)}|\${c.home.sessionVariables.DISPATCHER_PROTOCOL_DIR}|\${c.home.sessionVariables.DISPATCHER_REVIEWERS_DIR}|\${c.home.sessionVariables.DISPATCHER_CRITICS_DIR}\"
  "
  [ "$status" -eq 0 ]
  # Assert the wiring, not the flavour of path it resolves to: whether `self`
  # lands in the store or stays a source path depends on how the flake was
  # evaluated (a CI checkout differs from a local dev tree), and that is not the
  # behaviour under test. Removing either export still fails here — a missing
  # attribute makes the eval itself error, so $status catches it.
  [[ "$output" == work\|* ]]
  # Every CLI the module claims to install, resolved from the flake — a package
  # that isn't in `packages` fails the eval outright, not a grep.
  [[ "$output" == *"crew,dispatch,dispatch-resume,dispatcher,refresh-scores,refresh-budget,refresh-models,pr-watch"* ]]
  [[ "$output" == */adapters/core/protocols\|*/adapters/core/reviewers\|*/adapters/core/critics ]]
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
