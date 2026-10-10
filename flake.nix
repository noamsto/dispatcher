{
  description = "dispatcher — shell-based agent-orchestration harness";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    # Separate pin: `claude plugin test` landed after the main nixpkgs lock's
    # claude-code (2.1.220). Only the mod-tests runner reads it.
    nixpkgs-claude.url = "github:NixOS/nixpkgs/7a0f122f5090cf4c2ade2a13a0e229d4e19ba71f";
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
        system,
        ...
      }: let
        # `claude plugin test` is the only runner for the mod's
        # `claude-code/testing` kit (it ships inside the binary), and the
        # binary is unfree. Allow just that one package; the wrapped `claude`
        # on a dev machine rejects `plugin test`, so the pin is the way in.
        claudeCode =
          (import inputs.nixpkgs-claude {
            inherit system;
            config.allowUnfreePredicate = p: pkgs.lib.getName p == "claude-code";
          }).claude-code;
        # Runs the mod tests hermetically: HOME is a scratch dir (no login, no
        # config) and the function-hooks gate the test runner needs is set.
        modTests = pkgs.writeShellApplication {
          name = "mod-tests";
          runtimeInputs = [pkgs.git];
          text = ''
            home=$(mktemp -d)
            trap 'rm -rf "$home"' EXIT
            export HOME=$home CLAUDE_CODE_ENABLE_FUNCTION_HOOKS=1
            ${claudeCode}/bin/claude plugin test "''${1:-$(git rev-parse --show-toplevel)/adapters/claude-code/plugin}"
          '';
        };
        # The package set with `lockedSettings` (a JSON file, or null for none)
        # baked into dispatch-config as its locked layer. nix/hm-module.nix
        # calls it with the file it generates.
        mkPackages = lockedSettings: let
          protocols = ./adapters/core/protocols;
          # @protocolDir@ is the build-time default for the env-overridable
          # PROTOCOL_DIR in dispatch.sh / dispatcher.sh. Substituting a store
          # path here is what frees them from the old ~/nix-config literal.
          # @protocolRev@ (#184, #193) is a content hash of the same directory
          # baked into dispatch.sh / dispatch-resume.sh; their runtime guard
          # recomputes that hash from the files actually in $PROTOCOL_DIR and
          # refuses a mismatch, so a checkout override that drifted from this
          # build cannot launch workers against an old protocol contract. A
          # stale store-path DISPATCHER_PROTOCOL_DIR export (a long-lived shell
          # after a rebuild) never reaches that check: _resolve_dir ignores it
          # with a notice and uses @protocolDir@ (#303). There is
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
          # DISPATCHER_SKILLS_DIR that pi workers are handed via --skill. The
          # source tree itself, not a projection: gen-adapters.sh copies this
          # same directory into the other three adapter trees.
          skills = ./adapters/core/skills;
          sub =
            builtins.replaceStrings
            ["@protocolDir@" "@protocolRev@" "@skillsDir@" "@reviewersDir@" "@criticsDir@" "@crossRepoHintLib@" "@publicLeakGuard@" "@worktreeGitLib@" "@grantCheckLib@" "@claudeWorkerSettingsLib@" "@localModelsLib@" "@budgetGateLib@" "@laneProfilesDir@"]
            ["${protocols}" "${protocolRev}" "${skills}" "${./adapters/core/reviewers}" "${./adapters/core/critics}" "${./adapters/core/cross-repo-hint.sh}" "${./adapters/core/public-leak-guard.sh}" "${./adapters/core/worktree-git.sh}" "${./adapters/core/grant-check.sh}" "${./adapters/core/claude-worker-settings.sh}" "${./adapters/core/local-models.sh}" "${./adapters/core/budget-gate.sh}" "${./adapters/core/lane-profiles}"];
          # The settings resolver (#560), with defaults.json baked in as its base
          # layer. withConfig bakes its path into the consumers as the default for
          # their env-overridable DISPATCH_CONFIG_BIN, the WORKTREE_GIT_LIB idiom.
          dispatchConfig = pkgs.writeShellApplication {
            name = "dispatch-config";
            runtimeInputs = with pkgs; [jq coreutils];
            text =
              builtins.replaceStrings
              (["@defaultsJson@"] ++ pkgs.lib.optional (lockedSettings != null) "@lockedSettings@")
              (["${./adapters/core/defaults.json}"] ++ pkgs.lib.optional (lockedSettings != null) "${lockedSettings}")
              (builtins.readFile ./adapters/core/dispatch-config.sh);
          };
          withConfig = builtins.replaceStrings ["@dispatchConfig@"] ["${dispatchConfig}/bin/dispatch-config"];
          # @launcherRuntimePath@ (#936) is a launcher's own pinned tool PATH:
          # exactly the dirs writeShellApplication's preamble prepends, i.e.
          # `lib.makeBinPath` of the very list passed as its runtimeInputs, so the
          # dirs a launcher strips cannot drift from the dirs it was given. Its
          # script removes them from PATH for the engine it launches; a raw run
          # from a checkout never gets the substitution and leaves PATH alone.
          launcherPath = runtimeInputs:
            builtins.replaceStrings ["@launcherRuntimePath@"] [(pkgs.lib.makeBinPath runtimeInputs)];
        in rec {
          dispatch-config = dispatchConfig;

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
            # still recover it. crew-dash is for `dash`, which delegates to it.
            # dispatch-config (#606) is on PATH so `crew rate` / _burn_weight
            # resolve the burn table out of the box, with no DISPATCH_CONFIG_BIN
            # and no ambient resolver required.
            # stall-watch runs refresh-budget (rate-limited, host-wide) so its
            # budget: detector reads a fresh cache.
            runtimeInputs = (with pkgs; [git jq coreutils gnugrep tmux gh gtrash procps]) ++ [pr-watch dispatch-config crew-dash refresh-budget];
            # crew never references the protocols, but reap sources the
            # anchored-git lib (#539), so it still needs `sub`.
            # @crewGoBin@ is the Go port's binary, substituted for crew alone
            # (not in the shared `sub`): only crew.sh's ported-arm exec uses it.
            text =
              builtins.replaceStrings ["@crewGoBin@"] ["${crew-go}/bin/crew-go"]
              (sub (builtins.readFile ./adapters/core/crew.sh));
          };

          # The Go port of crew's ported subcommands; crew.sh execs it. It
          # references nothing in the flake, so crew listing it closes no
          # eval-time cycle. git and tmux come from crew's own PATH at run
          # time, and git is a check input because the bus tests run real
          # `git init`/`git worktree`. buildGoModule names the binary after the
          # module path's last element ("crew"), so postInstall renames it.
          crew-go = pkgs.buildGoModule {
            # `name`, not just pname/version, for the same reason as crew-dash.
            name = "crew-go";
            pname = "crew-go";
            version = "0.1.0";
            src = ./crew;
            vendorHash = "sha256-9H7aWvuqfuxsL3UIoxaqNEofuFXE2D9Dq3Hk32qSHmk=";
            nativeCheckInputs = [pkgs.git pkgs.ps];
            postInstall = "mv $out/bin/crew $out/bin/crew-go";
          };

          # crew is deliberately NOT a runtime input of crew-dash: crew lists
          # crew-dash so its `dash` subcommand can exec this one, and naming
          # crew here would close that into an eval-time cycle (the
          # dispatch-resume precedent above). `crew dash` resolves `crew` from
          # CREW_BIN, its own readlink'd path, instead. buildGoModule names the
          # binary after the module path's last element ("dash"), so
          # postInstall renames it before wrapping.
          crew-dash = pkgs.buildGoModule {
            # `name`, not just pname/version: the hm module installs it into
            # home.packages, and tests/module.bats checks p.name literally —
            # buildGoModule would otherwise default it to "crew-dash-0.1.0".
            name = "crew-dash";
            pname = "crew-dash";
            version = "0.1.0";
            src = ./dash;
            vendorHash = "sha256-0HYob9awE9cJYU/7J6WF7o6D6i4gmPMN0kyFuCpgpFA=";
            ldflags = ["-s" "-w" "-X main.dispatchConfigBin=${dispatchConfig}/bin/dispatch-config"];
            nativeBuildInputs = [pkgs.makeWrapper];
            postInstall = ''
              mv $out/bin/dash $out/bin/crew-dash
              wrapProgram $out/bin/crew-dash --prefix PATH : ${pkgs.lib.makeBinPath [refresh-budget pkgs.git]}
            '';
          };

          dispatch = let
            # One list, read twice: the preamble's runtimeInputs and the bake.
            # direnv: pre-allows the freshly scaffolded worktree's .envrc (#40).
            # curl, gnugrep, betterleaks: the public-leak guard a mint runs its
            # issue body through.
            rt = (with pkgs; [gh git jq gnused coreutils findutils diffutils tmux direnv curl gnugrep betterleaks procps]) ++ [crew dispatch-resume];
          in
            pkgs.writeShellApplication {
              name = "dispatch";
              runtimeInputs = rt;
              text = launcherPath rt (withConfig (sub (builtins.readFile ./adapters/core/dispatch.sh)));
            };

          # `dispatch` is deliberately NOT in runtimeInputs: the dispatch
          # package above lists dispatch-resume so its `resume` subcommand can
          # exec this one, and naming dispatch here would close that into an
          # eval-time cycle. dispatch-resume resolves `dispatch` from the
          # ambient PATH instead — the same ambient-tool pattern dispatch
          # itself uses for `wt`.
          dispatch-resume = let
            # One list, read twice: the preamble's runtimeInputs and the bake.
            rt = (with pkgs; [gh git jq gnused gnugrep coreutils findutils diffutils tmux]) ++ [crew];
          in
            pkgs.writeShellApplication {
              name = "dispatch-resume";
              runtimeInputs = rt;
              text = launcherPath rt (withConfig (sub (builtins.readFile ./adapters/core/dispatch-resume.sh)));
            };

          dispatcher = let
            rt = (with pkgs; [git jq coreutils diffutils tmux]) ++ [crew];
          in
            pkgs.writeShellApplication {
              name = "dispatcher";
              runtimeInputs = rt;
              text = launcherPath rt (withConfig (sub (builtins.readFile ./adapters/core/dispatcher.sh)));
            };

          refresh-scores = pkgs.writeShellApplication {
            name = "refresh-scores";
            runtimeInputs = with pkgs; [curl jq coreutils];
            text = builtins.readFile ./adapters/core/refresh-scores.sh;
          };

          refresh-budget = pkgs.writeShellApplication {
            name = "refresh-budget";
            runtimeInputs = with pkgs; [curl jq coreutils tmux];
            text = withConfig (sub (builtins.readFile ./adapters/core/refresh-budget.sh));
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
            runtimeInputs = with pkgs; [git jq yq-go coreutils gawk diffutils];
            text = builtins.replaceStrings ["@reviewersDir@"] ["${./adapters/core/reviewers}"] (builtins.readFile ./adapters/core/reviewers/resolve-roster.sh);
          };

          permission-check = pkgs.writeShellApplication {
            name = "permission-check";
            runtimeInputs = with pkgs; [jq coreutils findutils gnused tmux git procps];
            text = withConfig (sub (builtins.readFile ./adapters/core/permission-check.sh));
          };

          default = pkgs.symlinkJoin {
            name = "dispatcher-all";
            paths = [crew crew-dash dispatch dispatch-resume dispatch-config dispatcher refresh-scores refresh-budget refresh-models pr-watch reviewer-roster permission-check];
          };
        };
      in {
        treefmt = {
          projectRootFile = "flake.nix";
          # Vendored shell is payload, not our source: it must stay
          # byte-identical to upstream so this extraction can prove it changed
          # no behaviour. They happen to be shfmt-clean today, so nothing is
          # rewritten right now — the exclude is what keeps a future upstream
          # edit from being silently reformatted on its way in.
          # dispatcher.sh is NOT excluded: that one is ours, ported from fish.
          # dispatch-resume.sh carries byte-identical copies of crew.sh's
          # liveness helpers (#461); shfmt would reformat some of them and
          # sever the sync guarantee tests/adapters.bats pins.
          settings.global.excludes = [
            "adapters/core/crew.sh"
            "adapters/core/dispatch.sh"
            "adapters/core/dispatch-notify.sh"
            "adapters/core/dispatch-resume.sh"
            "adapters/claude-code/plugin/scripts/*"
            "adapters/codex/plugin/scripts/*"
          ];
          programs = {
            alejandra.enable = true;
            shfmt = {
              enable = true;
              indent_size = 2;
            };
            gofmt.enable = true;
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
            #
            # ^dash/.*/testdata/: golden ui View() frames are width-padded on
            # purpose and carry trailing spaces as part of the fixture.
            # ^crew/.*/testdata/: byte-exact jq output goldens (concatenated
            # JSON values, escapes, trailing blank lines) that jq produced.
            excludes = ["\\.bats$" "^adapters/" "^dash/.*/testdata/" "^crew/.*/testdata/"];
          };
          check-merge-conflicts.enable = true;
          trim-trailing-whitespace = {
            enable = true;
            # Same goldens reason as prettier's exclude above.
            excludes = ["^dash/.*/testdata/" "^crew/.*/testdata/"];
          };
        };

        checks = {
          # Fails `nix flake check` on doc drift even without a full checkout run
          # of gen-adapters.sh (#560) — mirrors the tier-map conformance test's
          # role for dispatch.sh, but for the generated doc regions.
          model-map-doc = pkgs.runCommand "model-map-doc" {nativeBuildInputs = with pkgs; [bash jq gawk diffutils coreutils gnused];} "bash ${./scripts/gen-model-map-doc.sh} --check ${./adapters/core/defaults.json} ${./adapters/core/protocols/dispatch-orchestration.md} && touch $out";

          # `nix flake check` only evaluates packages, not builds them — these
          # make it build, which runs buildGoModule's `go test ./...` (doCheck).
          inherit (config.packages) crew-dash crew-go;

          # Runs adapters/claude-code/plugin/hooks/*.test.ts, which nothing else
          # in CI executes.
          mod-tests = pkgs.runCommand "mod-tests" {} ''
            ${modTests}/bin/mod-tests ${./adapters/claude-code/plugin}
            touch $out
          '';
        };

        packages = mkPackages null;

        legacyPackages.mkPackages = mkPackages;

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
              # d2: tests/crew.bats compiles every roster palette color with the
              # real compiler, so a palette name d2 rejects (steel, rust, sky
              # before the renderer's mapping) fails CI instead of silently
              # skipping.
              pkgs.d2
              pkgs.git
              pkgs.tmux
              pkgs.gh
              pkgs.go
              pkgs.golangci-lint
              # mod-tests: runs the claude plugin mod tests the way the
              # `mod-tests` flake check does.
              modTests
              # mawk, nawk, and (on Linux) BusyBox awk: tests/secret-read-guard.bats
              # runs the credential-read rule under each awk implementation.
              # BusyBox is wrapped as busybox-awk because busybox on PATH would
              # shadow coreutils.
              pkgs.mawk
              pkgs.nawk
            ]
            ++ pkgs.lib.optionals pkgs.stdenv.isLinux [(pkgs.writeShellScriptBin "busybox-awk" ''exec ${pkgs.busybox}/bin/busybox awk "$@"'')];
        };
      };
    };
}
