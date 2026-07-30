{
  description = "dispatcher — shell-based agent-orchestration harness";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-parts.url = "github:hercules-ci/flake-parts";
    treefmt-nix = {
      url = "github:numtide/treefmt-nix";
      inputs.nixpkgs.follows = "nixpkgs";
    };
    git-hooks-nix = {
      url = "github:cachix/git-hooks.nix";
      inputs.nixpkgs.follows = "nixpkgs";
    };
  };

  outputs = inputs @ {flake-parts, ...}:
    flake-parts.lib.mkFlake {inherit inputs;} {
      systems = ["x86_64-linux" "aarch64-linux" "x86_64-darwin" "aarch64-darwin"];

      imports = [
        inputs.treefmt-nix.flakeModule
        inputs.git-hooks-nix.flakeModule
      ];

      # Consumed as `inputs.dispatcher.homeManagerModules.default`. Takes `self`
      # so it can reach the adapter trees and the per-system packages.
      flake.homeManagerModules.default = import ./nix/hm-module.nix inputs.self;

      perSystem = {
        pkgs,
        config,
        ...
      }: {
        treefmt = {
          projectRootFile = "flake.nix";
          # Vendored shell is payload, not our source: it must stay
          # byte-identical to upstream so this extraction can prove it changed
          # no behaviour. They happen to be shfmt-clean today, so nothing is
          # rewritten right now — the exclude is what keeps a future upstream
          # edit from being silently reformatted on its way in.
          # dispatcher.sh is NOT excluded: that one is ours, ported from fish.
          settings.global.excludes = [
            "adapters/core/crew.sh"
            "adapters/core/dispatch.sh"
            "adapters/core/dispatch-notify.sh"
            "adapters/claude-code/plugin/scripts/*"
            "adapters/codex/plugin/scripts/*"
          ];
          programs = {
            alejandra.enable = true;
            shfmt = {
              enable = true;
              indent_size = 2;
            };
          };
        };

        pre-commit.settings.hooks = {
          statix.enable = true;
          deadnix.enable = true;
          alejandra.enable = true;
          shellcheck = {
            enable = true;
            # .envrc: sourced by direnv, no shebang (SC2148).
            # .bats: a test DSL, not plain bash — shellcheck misparses `done`
            # as a loop keyword (SC1010), the `VAR= cmd` idiom (SC1007), and
            # bats-invoked helpers as dead (SC2329). CI lints core shell
            # explicitly via `shellcheck adapters/core/*.sh`, not the tests.
            excludes = ["^\\.envrc$" "\\.bats$"];
          };
          prettier = {
            enable = true;
            # .bats: prettier has no bats formatter and mangles the DSL.
            #
            # ^adapters/: nothing under it is hand-authored source in this repo —
            # it is all vendored payload (protocols, agents, skills, workflows)
            # or generator output (commands, codex skills, hook scripts). Two
            # reasons it must not be reformatted:
            #   1. It is content, not style. These files are fed to models as
            #      system prompts and instructions. Prettier rewrote *is* to
            #      _is_, re-padded tables, and — worse — rewrote nested code
            #      fences (``` -> ````), restructuring a teammate prompt
            #      template. Byte-identity with upstream is also how this
            #      extraction proves it changed no behaviour.
            #   2. It would deadlock the drift gate. CI regenerates the adapters
            #      and asserts `git diff --exit-code`; if prettier reformats the
            #      generated output after the generator writes it, committed
            #      output can never match a fresh run, and the gate fails
            #      forever.
            excludes = ["\\.bats$" "^adapters/"];
          };
          check-merge-conflicts.enable = true;
          trim-trailing-whitespace.enable = true;
        };

        packages = let
          protocols = ./adapters/core/protocols;
          # @protocolDir@ is the build-time default for the env-overridable
          # PROTOCOL_DIR in dispatch.sh / dispatcher.sh. Substituting a store
          # path here is what frees them from the old ~/nix-config literal.
          sub = builtins.replaceStrings ["@protocolDir@"] ["${protocols}"];
        in rec {
          crew = pkgs.writeShellApplication {
            name = "crew";
            runtimeInputs = with pkgs; [git jq coreutils gnugrep tmux];
            # No substitution: crew never references the protocols.
            text = builtins.readFile ./adapters/core/crew.sh;
          };

          dispatch = pkgs.writeShellApplication {
            name = "dispatch";
            runtimeInputs = (with pkgs; [gh git jq gnused coreutils tmux]) ++ [crew];
            text = sub (builtins.readFile ./adapters/core/dispatch.sh);
          };

          dispatcher = pkgs.writeShellApplication {
            name = "dispatcher";
            runtimeInputs = (with pkgs; [git jq coreutils tmux]) ++ [crew];
            text = sub (builtins.readFile ./adapters/core/dispatcher.sh);
          };

          default = pkgs.symlinkJoin {
            name = "dispatcher-all";
            paths = [crew dispatch dispatcher];
          };
        };

        devShells.default = pkgs.mkShell {
          inherit (config.pre-commit) shellHook;
          packages =
            config.pre-commit.settings.enabledPackages
            ++ [
              config.treefmt.build.wrapper
              pkgs.bats
              pkgs.shellcheck
              pkgs.jq
              # yq-go: tests/adapters.bats parses generated codex skill
              # frontmatter to prove it is valid YAML. Without it here the test
              # silently uses whatever yq leaks in from the user profile and
              # fails in CI.
              pkgs.yq-go
              pkgs.git
              pkgs.tmux
              pkgs.gh
            ];
        };
      };
    };
}
