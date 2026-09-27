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
  # `github` or `linear:TEAM`. TEAM is `[A-Z][A-Z0-9]*`.
  trackerValue = lib.types.strMatching "github|linear:[A-Z][A-Z0-9]*";
  trackerExport = attrs: lib.concatStringsSep " " (lib.mapAttrsToList (name: value: "${name}=${value}") attrs);
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

    grantRoots = lib.mkOption {
      type = lib.types.listOf (lib.types.strMatching "/[^:]*");
      default = [];
      example = ["/home/me/git"];
      description = ''
        Dirs under which `dispatch --add-dir` may grant a claude worker an extra
        directory. Exported as DISPATCH_GRANT_ROOTS (colon-separated). Empty, or
        unset at runtime, refuses every --add-dir. A root must not be / or $HOME
        or an ancestor of it; secrets dirs inside a root stay refused.
      '';
    };

    repoTrackers = lib.mkOption {
      type = lib.types.attrsOf trackerValue;
      default = {};
      example = {"factify-inc/mono" = "linear:ENG";};
      description = ''
        Tracker for one GitHub repo. Keys are `owner/repo`. A value is `github`
        or `linear:TEAM`. Exported as DISPATCH_REPO_TRACKERS (space-separated
        `key=value`). Empty exports an empty string. `dispatch` checks this
        before orgTrackers, then stamps `github`.
      '';
    };

    orgTrackers = lib.mkOption {
      type = lib.types.attrsOf trackerValue;
      default = {};
      example = {factify-inc = "linear:ENG";};
      description = ''
        Default tracker for every repo in a GitHub org. Keys are the org login.
        Values match repoTrackers. Exported as DISPATCH_ORG_TRACKERS
        (space-separated `key=value`). Empty exports an empty string. Used when
        the repo has no repoTrackers entry.
      '';
    };

    openrouter = {
      monthlyTarget = lib.mkOption {
        type = lib.types.nullOr lib.types.numbers.positive;
        default = null;
        example = 50;
        description = ''
          Monthly OpenRouter spend target in USD, tracked over the UTC
          calendar month. Exported as DISPATCH_OPENROUTER_MONTHLY_USD only
          when set (unset leaves the feature off: `refresh-budget` still
          records pi's month-to-date spend, but no window gates it).
        '';
      };

      keyFile = lib.mkOption {
        type = lib.types.nullOr (lib.types.strMatching "/.*");
        default = null;
        example = "/run/agenix/openrouter";
        description = ''
          Absolute path to a file whose first line is an OpenRouter API key
          (e.g. an agenix/sops secret path). A string, never `types.path` --
          a path would copy the secret into the Nix store. Exported as
          DISPATCH_OPENROUTER_KEY_FILE only when set; `refresh-budget` falls
          back to OPENROUTER_API_KEY when unset. `~/.pi/agent/auth.json` is
          never read.
        '';
      };
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
      packages = [pkgsFor.crew pkgsFor.dispatch pkgsFor.dispatch-resume pkgsFor.dispatcher pkgsFor.refresh-scores pkgsFor.refresh-budget pkgsFor.refresh-models pkgsFor.pr-watch pkgsFor.reviewer-roster pkgsFor.permission-check];

      sessionVariables =
        {
          DISPATCH_PROFILE = cfg.profile;
          DISPATCH_ENGINES = lib.concatStringsSep " " cfg.engines;
          DISPATCH_GRANT_ROOTS = lib.concatStringsSep ":" cfg.grantRoots;
          DISPATCH_REPO_TRACKERS = trackerExport cfg.repoTrackers;
          DISPATCH_ORG_TRACKERS = trackerExport cfg.orgTrackers;
          # Exported, not merely baked into the CLIs. The `dispatcher` slash
          # command and the cursor rule are markdown an agent reads live and
          # resolves through its Bash tool, which a build-time substitution into
          # the shell scripts cannot reach. Each plugin also ships a protocols/
          # copy as the fallback when this is unset (a non-Nix install). Override
          # it in your shell to iterate on a checkout without rebuilding.
          # #184/#193: dispatch / dispatch-resume recompute the built-in
          # protocol revision as a content hash of the files actually in
          # $PROTOCOL_DIR, so a checkout override that drifted from the build
          # aborts with an actionable message rather than silently running
          # workers against an old protocol contract. #303: a stale value held
          # by a long-lived shell or tmux server after a rebuild is a store path
          # from the previous build; dispatch / dispatch-resume / dispatcher
          # detect that by content, ignore it with a notice and use their baked
          # directory. The export stays because the markdown reads it. There is
          # no committed PROTOCOL_REV file to regenerate.
          DISPATCHER_PROTOCOL_DIR = "${self}/adapters/core/protocols";
          DISPATCHER_REVIEWERS_DIR = "${self}/adapters/core/reviewers";
          DISPATCHER_CRITICS_DIR = "${self}/adapters/core/critics";
          # pi only: the other three load the harness skills from their own
          # adapter trees, so pi is the one engine dispatch has to hand a path.
          DISPATCHER_SKILLS_DIR = "${self}/adapters/core/skills";
        }
        // lib.optionalAttrs (cfg.openrouter.monthlyTarget != null) {
          DISPATCH_OPENROUTER_MONTHLY_USD = toString cfg.openrouter.monthlyTarget;
        }
        // lib.optionalAttrs (cfg.openrouter.keyFile != null) {
          DISPATCH_OPENROUTER_KEY_FILE = cfg.openrouter.keyFile;
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
          # this links each of ours in under its own name rather than claiming
          # the whole directory. `ln -sfn` (not `home.file`) makes it idempotent
          # across activations and lets a store-path bump repoint an existing
          # link. Each link keeps the skill's literal directory name, since
          # that's the name the protocol docs and skill invocations resolve it
          # by (e.g. "spec-plan-critic", not "dispatcher-spec-plan-critic").
          dispatcherCursorSkills = lib.hm.dag.entryAfter ["writeBoundary"] ''
            skills_dir="$HOME/.cursor/skills"
            run mkdir -p "$skills_dir"
            ${lib.concatMapStrings (name: "run ln -sfn \"${self}/adapters/cursor/skills/${name}\" \"$skills_dir/${name}\"\n") (builtins.attrNames (lib.filterAttrs (_: t: t == "directory") (builtins.readDir "${self}/adapters/cursor/skills")))}'';
        };
    };
  };
}
