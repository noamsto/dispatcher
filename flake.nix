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
            # ^adapters/: all vendored payload or generator output, never
            # hand-authored here. Reformatting it broke two things — it rewrote
            # nested code fences inside a teammate prompt template (these files
            # are instructions a model reads, so their bytes are content), and
            # it would deadlock CI's drift gate, which regenerates the adapters
            # and asserts no diff.
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
          # @protocolRev@ (#184, #193) is a content hash of the same directory
          # baked into dispatch.sh / dispatch-resume.sh; their runtime guard
          # recomputes that hash from the files actually in $PROTOCOL_DIR and
          # refuses a mismatch, so a stale DISPATCHER_PROTOCOL_DIR export can
          # no longer launch workers against an old protocol contract. There is
          # no committed PROTOCOL_REV file to read or regenerate — the guard is
          # definitionally fresh, which is what lets two PRs editing different
          # protocol files merge in either order without conflict. readDir/
          # attrNames sort byte-wise and hashFile reads the store copy, so this
          # is reproducible, pure, and free of timestamps or git calls; the
          # runtime bash copy of the rule lives in _check_protocol_rev and is
          # pinned to this one by tests/module.bats.
          protocolFiles = builtins.attrNames (builtins.readDir protocols);
          protocolRev = builtins.substring 0 16 (builtins.hashString "sha256"
            (builtins.concatStringsSep "" (map
              (n: "${n}:${builtins.hashFile "sha256" (protocols + "/${n}")};")
              protocolFiles)));
          # @skillsDir@ is the build-time default for the env-overridable
          # DISPATCHER_SKILLS_DIR that pi workers are handed via --skill.
          # Only pi needs it: claude loads these as plugin skills and codex /
          # cursor get copies projected by gen-adapters.sh, so pi is the one
          # engine that would otherwise be pointed at a body it cannot open.
          # Delivered by path rather than as a fourth generated copy —
          # adapters/core/skills is the single source the other three project
          # from, and a copy would be one more tree to keep in sync.
          skills = ./adapters/core/skills;
          sub =
            builtins.replaceStrings
            ["@protocolDir@" "@protocolRev@" "@skillsDir@"]
            ["${protocols}" "${protocolRev}" "${skills}"];
        in rec {
          # Its own binary, not a crew subcommand: the primitive is standalone by
          # design (no crew, no bus, no dispatcher) and `crew pr-watch` only
          # wraps it to post the event.
          pr-watch = pkgs.writeShellApplication {
            name = "pr-watch";
            runtimeInputs = with pkgs; [gh git jq gnused gnugrep coreutils];
            text = builtins.readFile ./adapters/core/pr-watch.sh;
          };

          crew = pkgs.writeShellApplication {
            name = "crew";
            # gh + gtrash are for `reap`: it reads PR state via gh and trashes
            # the finished worker's task doc via gtrash so a post-mortem can
            # still recover it. `wt` stays ambient, as in dispatch — reap checks
            # for it and degrades to a notice when absent.
            runtimeInputs = (with pkgs; [git jq coreutils gnugrep tmux gh gtrash]) ++ [pr-watch];
            # No substitution: crew never references the protocols.
            text = builtins.readFile ./adapters/core/crew.sh;
          };

          dispatch = pkgs.writeShellApplication {
            name = "dispatch";
            # direnv: pre-allows the freshly scaffolded worktree's .envrc (#40).
            runtimeInputs = (with pkgs; [gh git jq gnused coreutils tmux direnv]) ++ [crew dispatch-resume];
            text = sub (builtins.readFile ./adapters/core/dispatch.sh);
          };

          # `dispatch` is deliberately NOT in runtimeInputs: the dispatch
          # package above lists dispatch-resume so its `resume` subcommand can
          # exec this one, and naming dispatch here would close that into an
          # eval-time cycle. dispatch-resume resolves `dispatch` from the
          # ambient PATH instead — the same ambient-tool pattern dispatch
          # itself uses for `wt`.
          dispatch-resume = pkgs.writeShellApplication {
            name = "dispatch-resume";
            runtimeInputs = (with pkgs; [gh git jq gnused gnugrep coreutils tmux]) ++ [crew];
            text = sub (builtins.readFile ./adapters/core/dispatch-resume.sh);
          };

          dispatcher = pkgs.writeShellApplication {
            name = "dispatcher";
            runtimeInputs = (with pkgs; [git jq coreutils tmux]) ++ [crew];
            text = sub (builtins.readFile ./adapters/core/dispatcher.sh);
          };

          refresh-scores = pkgs.writeShellApplication {
            name = "refresh-scores";
            runtimeInputs = with pkgs; [curl jq coreutils];
            text = builtins.readFile ./adapters/core/refresh-scores.sh;
          };

          refresh-budget = pkgs.writeShellApplication {
            name = "refresh-budget";
            runtimeInputs = with pkgs; [curl jq coreutils];
            text = builtins.readFile ./adapters/core/refresh-budget.sh;
          };

          refresh-models = pkgs.writeShellApplication {
            name = "refresh-models";
            runtimeInputs = with pkgs; [jq gnugrep gnused coreutils];
            text = builtins.readFile ./adapters/core/refresh-models.sh;
          };

          # yq-go is a runtime input, not just a devShell one: the resolver
          # parses the harness reviewers' frontmatter with it and its preflight
          # rejects any other yq, so it must not depend on whatever yq happens
          # to be ambient on a caller's PATH.
          reviewer-roster = pkgs.writeShellApplication {
            name = "reviewer-roster";
            runtimeInputs = with pkgs; [git jq yq-go coreutils gawk];
            text = builtins.replaceStrings ["@reviewersDir@"] ["${./adapters/core/reviewers}"] (builtins.readFile ./adapters/core/reviewers/resolve-roster.sh);
          };

          default = pkgs.symlinkJoin {
            name = "dispatcher-all";
            paths = [crew dispatch dispatch-resume dispatcher refresh-scores refresh-budget refresh-models pr-watch reviewer-roster];
          };
        };

        devShells.default = pkgs.mkShell {
          inherit (config.pre-commit) shellHook;
          packages =
            config.pre-commit.settings.enabledPackages
            ++ [
              config.treefmt.build.wrapper
              pkgs.bats
              pkgs.parallel
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
