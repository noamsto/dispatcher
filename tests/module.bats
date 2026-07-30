setup() {
  ROOT="$BATS_TEST_DIRNAME/.."
}

@test "all three packages build" {
  run nix build --no-link "$ROOT#crew" "$ROOT#dispatch" "$ROOT#dispatcher"
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

@test "the home-manager module evaluates and declares its options" {
  # `nix flake check` reports homeManagerModules as UNCHECKED, so an eval error
  # in the module would otherwise surface only when a consumer imports it.
  run nix eval --impure --raw --expr "
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

@test "the module exports DISPATCHER_PROTOCOL_DIR, not just DISPATCH_PROFILE" {
  # The dispatcher slash command and the cursor rule read the protocol through
  # this variable at runtime; a build-time substitution into the shell scripts
  # cannot reach markdown an agent reads live.
  run grep -c 'DISPATCHER_PROTOCOL_DIR' "$ROOT/nix/hm-module.nix"
  [ "$output" -ge 1 ]
  run grep -c 'DISPATCH_PROFILE' "$ROOT/nix/hm-module.nix"
  [ "$output" -ge 1 ]
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
