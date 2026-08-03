setup() {
  ROOT="$BATS_TEST_DIRNAME/.."
}

@test "all four packages build" {
  run nix build --no-link "$ROOT#crew" "$ROOT#dispatch" "$ROOT#dispatcher" "$ROOT#refresh-scores"
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

@test "the substituted protocol dir actually contains the protocols" {
  # A substituted-but-wrong path would leave every dispatched worker unable to
  # find its protocol, and nothing else would notice until a live run.
  out="$(nix build --no-link --print-out-paths "$ROOT#dispatch")"
  dir="$(grep -o '/nix/store/[^"}]*' "$out/bin/dispatch" | grep -i protocol | head -1)"
  [ -n "$dir" ]
  [ -f "$dir/WORKER_PROTOCOL.md" ]
  [ -f "$dir/DISPATCHER_PROTOCOL.md" ]
}

@test "crew is not substituted — it never references the protocols" {
  out="$(nix build --no-link --print-out-paths "$ROOT#crew")"
  run grep -c 'PROTOCOL' "$out/bin/crew"
  [ "$output" = "0" ]
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
  # importJSON path, or a broken interpolation.
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
        \"\${c.home.sessionVariables.DISPATCH_PROFILE}|\${c.home.sessionVariables.DISPATCHER_PROTOCOL_DIR}\"
  "
  [ "$status" -eq 0 ]
  # Assert the wiring, not the flavour of path it resolves to: whether `self`
  # lands in the store or stays a source path depends on how the flake was
  # evaluated (a CI checkout differs from a local dev tree), and that is not the
  # behaviour under test. Removing either export still fails here — a missing
  # attribute makes the eval itself error, so $status catches it.
  [[ "$output" == work\|* ]]
  [[ "$output" == */adapters/core/protocols ]]
}

@test "the codex plugin is copied as a real dir, never symlinked" {
  # Codex loads plugins only from a real directory under ~/.codex/plugins/cache.
  # A symlinked tree reports "installed, enabled" in `codex plugin list` while
  # its skills never reach the model — so cp -rL is load-bearing.
  run grep -F 'cp -rL' "$ROOT/nix/hm-module.nix"
  [ "$status" -eq 0 ]
  run grep -cE 'mkOutOfStoreSymlink|ln -s' "$ROOT/nix/hm-module.nix"
  [ "$output" = "0" ]
}
