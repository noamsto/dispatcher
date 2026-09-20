self: {
  config,
  lib,
  pkgs,
  ...
}: let
  cfg = config.programs.dispatcher;
  pkgsFor = self.packages.${pkgs.stdenv.hostPlatform.system};
  ccPlugin = "${self}/adapters/claude-code/plugin";
  codexPlugin = "${self}/adapters/codex/plugin";
  codexVersion = (lib.importJSON "${codexPlugin}/.codex-plugin/plugin.json").version;
  codexCache = ".codex/plugins/cache/dispatcher/dispatcher";
  hasEngine = e: lib.elem e cfg.engines;
in {
  options.programs.dispatcher = {
    enable = lib.mkEnableOption "the dispatcher agent fan-out harness";

    profile = lib.mkOption {
      type = lib.types.enum ["work" "personal"];
      default = "personal";
      description = ''
        The machine's profile. Read by the CLIs for the work+claude+deep rung
        and the work-only analytics MCP profile. Engine availability is
        `engines`, not this.
      '';
    };

    engines = lib.mkOption {
      type = lib.types.listOf (lib.types.enum ["claude" "codex" "cursor" "pi"]);
      default = ["claude" "pi"];
      description = ''
        Engines this machine may dispatch. Exported as DISPATCH_ENGINES and
        gates the per-engine artifacts installed below. An engine must also be
        installed: the CLIs probe PATH before scaffolding. Unset at runtime
        (a non-Nix checkout), the CLIs allow all four.

        Defaults to the two all-profile engines, which is the roster the
        removed work-profile gate produced on a personal machine.
      '';
    };

    claudePluginDir = lib.mkOption {
      type = lib.types.str;
      readOnly = true;
      default = ccPlugin;
      description = ''
        Pass to claude via --plugin-dir. Read-only; consumers reference it
        rather than reconstructing the store path.
      '';
    };
  };

  config = lib.mkIf cfg.enable {
    # One `home` attrset, not four `home.*` assignments — statix flags the
    # repeated key.
    home = {
      packages = [pkgsFor.crew pkgsFor.dispatch pkgsFor.dispatch-resume pkgsFor.dispatcher pkgsFor.refresh-scores pkgsFor.refresh-budget pkgsFor.refresh-models pkgsFor.pr-watch pkgsFor.reviewer-roster];

      sessionVariables = {
        DISPATCH_PROFILE = cfg.profile;
        DISPATCH_ENGINES = lib.concatStringsSep " " cfg.engines;
        # Exported, not merely baked into the CLIs. The `dispatcher` slash
        # command and the cursor rule are markdown an agent reads live and
        # resolves through its Bash tool, which a build-time substitution into
        # the shell scripts cannot reach. Each plugin also ships a protocols/
        # copy as the fallback when this is unset (a non-Nix install). Override
        # it in your shell to iterate on a checkout without rebuilding.
        # #184/#193: dispatch / dispatch-resume recompute the built-in
        # protocol revision as a content hash of the files actually in
        # $PROTOCOL_DIR, so a stale value held by a long-lived shell or tmux
        # server hashes differently and aborts with an actionable message
        # rather than silently running workers against an old protocol
        # contract. There is no committed PROTOCOL_REV file to regenerate.
        DISPATCHER_PROTOCOL_DIR = "${self}/adapters/core/protocols";
        DISPATCHER_REVIEWERS_DIR = "${self}/adapters/core/reviewers";
        DISPATCHER_CRITICS_DIR = "${self}/adapters/core/critics";
        # pi only: the other three load the harness skills from their own
        # adapter trees, so pi is the one engine dispatch has to hand a path.
        DISPATCHER_SKILLS_DIR = "${self}/adapters/core/skills";
      };

      # Cursor has no plugin format — loose files are the only channel. The .mdc
      # rule is silently ignored without `alwaysApply: true` frontmatter, which
      # the shipped file provides.
      file = lib.optionalAttrs (hasEngine "cursor") {
        ".cursor/rules/dispatcher.mdc".source = "${self}/adapters/cursor/rules/dispatcher.mdc";
        ".cursor/commands" = {
          source = "${self}/adapters/cursor/commands";
          recursive = true;
        };
        # ".cursor/skills" is NOT claimed here: it's a shared namespace with
        # other producers (like another module already installing aeye's
        # skills there). A whole-directory `source` would conflict with
        # theirs the moment ours is non-empty. Individual skills are
        # symlinked in by the activation script below instead.
        #
        # The protocols tell a cursor worker to fall back to the roster copy
        # beside commands/ whenever the exported variable is unset, so both
        # rosters have to exist there and not only in the store.
        ".cursor/reviewers" = {
          source = "${self}/adapters/cursor/reviewers";
          recursive = true;
        };
        ".cursor/critics" = {
          source = "${self}/adapters/cursor/critics";
          recursive = true;
        };
        ".cursor/protocols" = {
          source = "${self}/adapters/cursor/protocols";
          recursive = true;
        };
      };

      # Codex loads plugins ONLY from a real directory under
      # ~/.codex/plugins/cache. A symlinked tree reports "installed, enabled" in
      # `codex plugin list` while its skills never reach the model — hence
      # cp -rL. chmod lets the next switch replace the read-only store copy.
      #
      # The matching [marketplaces.dispatcher] /
      # [plugins."dispatcher@dispatcher"] stanzas in ~/.codex/config.toml are
      # hand-managed (no store path, so not Nix's) — see README. The adapter is
      # inert without them.
      activation =
        lib.optionalAttrs (hasEngine "codex") {
          dispatcherCodexPlugin = lib.hm.dag.entryAfter ["writeBoundary"] ''
            run rm -rf "$HOME/${codexCache}"
            run mkdir -p "$HOME/${codexCache}"
            run cp -rL ${codexPlugin} "$HOME/${codexCache}/${codexVersion}"
            run chmod -R u+w "$HOME/${codexCache}"
          '';
        }
        // lib.optionalAttrs (hasEngine "cursor") {
          # ~/.cursor/skills is a shared namespace another module also links
          # individual skills into (see the ".cursor/skills" comment above), so
          # this links each of ours in rather than claiming the whole directory.
          # `ln -sfn` (not `home.file`) makes it idempotent across activations and
          # lets a store-path bump repoint an existing link. Named "spec-plan-critic",
          # not "dispatcher-spec-plan-critic", because that's the literal name the
          # protocol docs and skill invocations resolve it by.
          dispatcherCursorSkills = lib.hm.dag.entryAfter ["writeBoundary"] ''
            skills_dir="$HOME/.cursor/skills"
            run mkdir -p "$skills_dir"
            run ln -sfn "${self}/adapters/cursor/skills/spec-plan-critic" "$skills_dir/spec-plan-critic"
          '';
        };
    };
  };
}
