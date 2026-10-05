self: {
  config,
  lib,
  pkgs,
  ...
}: let
  cfg = config.programs.dispatcher;
  ccPlugin = "${self}/adapters/claude-code/plugin";
  codexPlugin = "${self}/adapters/codex/plugin";
  codexVersion = (lib.importJSON "${codexPlugin}/.codex-plugin/plugin.json").version;
  codexCache = ".codex/plugins/cache/dispatcher/dispatcher";
  hasEngine = e: cfg.engines == null || lib.elem e cfg.engines;
  # `github` or `linear:TEAM`. TEAM is `[A-Z][A-Z0-9]*`.
  trackerValue = lib.types.strMatching "github|linear:[A-Z][A-Z0-9]*";

  # The locked settings layer baked into dispatch-config: always
  # profile+grantRoots, the rest only when set here, so an unset option leaves
  # the key to the out-of-store user settings file or the base default.
  openrouterLocked = lib.filterAttrs (_: v: v != null) {
    inherit (cfg.openrouter) keyFile;
    monthlyUsd = cfg.openrouter.monthlyTarget;
  };
  lockedSettings =
    {inherit (cfg) profile grantRoots;}
    // lib.optionalAttrs (cfg.engines != null) {inherit (cfg) engines;}
    // lib.optionalAttrs (cfg.repoTrackers != null) {inherit (cfg) repoTrackers;}
    // lib.optionalAttrs (cfg.orgTrackers != null) {inherit (cfg) orgTrackers;}
    // lib.optionalAttrs (openrouterLocked != {}) {openrouter = openrouterLocked;}
    // lib.optionalAttrs (cfg.localModels != null) {
      localModels = lib.mapAttrs (_: lib.filterAttrs (_: v: v != null)) cfg.localModels;
    };
  lockedFile = pkgs.writeText "dispatcher-locked-settings.json" (builtins.toJSON lockedSettings);
  pkgsFor = self.legacyPackages.${pkgs.stdenv.hostPlatform.system}.mkPackages lockedFile;
in {
  options.programs.dispatcher = {
    enable = lib.mkEnableOption "the dispatcher agent fan-out harness";

    profile = lib.mkOption {
      type = lib.types.enum ["work" "personal"];
      default = "personal";
      description = ''
        The machine's profile. Always emitted into the locked settings layer
        baked into the CLIs, read from there for the work+claude+deep rung
        and the work-only analytics MCP profile. Engine availability is
        `engines`, not this. A per-launch `DISPATCH_PROFILE` still overrides
        every layer.
      '';
    };

    engines = lib.mkOption {
      type = lib.types.nullOr (lib.types.nonEmptyListOf (lib.types.enum ["claude" "codex" "cursor" "pi"]));
      default = null;
      description = ''
        Engines this machine may dispatch, and gates the per-engine artifacts
        installed below. An engine must also be installed: the CLIs probe
        PATH before scaffolding. Set, it is emitted into the locked settings
        layer and wins over the user settings file's `engines`. Unset (the
        default), the locked layer omits the key, so the user settings file's
        value governs -- or, absent that too, all four engines are allowed at
        runtime. `null` also installs the codex and cursor artifacts below,
        since Nix cannot see which engines an out-of-store user file will
        enable. Set this to restrict the roster; the old default was
        `["claude" "pi"]`. An empty list is rejected by the type -- leave the
        option unset instead. A per-launch `DISPATCH_ENGINES` still overrides
        every layer.
      '';
    };

    grantRoots = lib.mkOption {
      type = lib.types.listOf (lib.types.strMatching "/[^:]*");
      default = [];
      example = ["/home/me/git"];
      description = ''
        Dirs under which `dispatch --add-dir` may grant a claude worker an
        extra directory. Always emitted into the locked settings layer baked
        into the CLIs (the user settings file cannot set this, only the
        locked layer or a per-launch `DISPATCH_GRANT_ROOTS` can). Empty, the
        default, refuses every --add-dir. A root must not be / or $HOME or an
        ancestor of it; secrets dirs inside a root stay refused.
      '';
    };

    repoTrackers = lib.mkOption {
      type = lib.types.nullOr (lib.types.attrsOf trackerValue);
      default = null;
      example = {"factify-inc/mono" = "linear:ENG";};
      description = ''
        Tracker for one GitHub repo. Keys are `owner/repo`, case-insensitive.
        A value is `github` or `linear:TEAM`. Set, it is emitted into the
        locked settings layer and merges per key with the user settings
        file's `repoTrackers` -- Nix wins for a key set here, the user file's
        entry for any other key still applies. Unset (the default), the
        locked layer omits the key and the user file governs alone. `dispatch`
        checks this before orgTrackers, then stamps `github`.
      '';
    };

    orgTrackers = lib.mkOption {
      type = lib.types.nullOr (lib.types.attrsOf trackerValue);
      default = null;
      example = {factify-inc = "linear:ENG";};
      description = ''
        Default tracker for every repo in a GitHub org. Keys are the org
        login, case-insensitive. Values match repoTrackers, and merge with
        the user settings file's `orgTrackers` the same way, per key. Used
        when the repo has no repoTrackers entry.
      '';
    };

    localModels = lib.mkOption {
      type = lib.types.nullOr (lib.types.attrsOf (lib.types.submodule {
        options = {
          baseUrl = lib.mkOption {
            type = lib.types.strMatching "https?://[^[:space:]]*[^/[:space:]]";
            example = "http://halo:13305/v1";
            description = "The endpoint's OpenAI-compatible base URL, without a trailing slash.";
          };
          contextWindow = lib.mkOption {
            type = lib.types.ints.positive;
            example = 131072;
            description = "The model's context window in tokens.";
          };
          maxConcurrent = lib.mkOption {
            type = lib.types.nullOr lib.types.ints.positive;
            default = null;
            description = "Concurrent dispatch workers allowed on this model. Null defaults to 1 at runtime.";
          };
          tiers = lib.mkOption {
            type = lib.types.nullOr (lib.types.nonEmptyListOf (lib.types.enum ["trivial" "standard" "deep"]));
            default = null;
            description = "Tiers this model may serve. Null defaults to trivial and standard at runtime.";
          };
        };
      }));
      default = null;
      example = {
        "lemonade/Qwen3.8-Flash-Next-MTP" = {
          baseUrl = "http://halo:13305/v1";
          contextWindow = 131072;
        };
      };
      description = ''
        Local pi models, served from your own endpoint. Keys are pi dispatch
        ids `<provider>/<model>`, validated by dispatch-config at runtime.
        Null `maxConcurrent` and `tiers` are omitted, so the runtime defaults
        apply (1; trivial and standard). Size `maxConcurrent` for the
        endpoint's other consumers (chat bots, interactive sessions), which
        dispatch does not count. Set, it lands in the locked settings layer
        and wins per field over the user settings file's `localModels`;
        unset, the key is left out and the user file governs alone.
      '';
    };

    openrouter = {
      monthlyTarget = lib.mkOption {
        type = lib.types.nullOr lib.types.numbers.positive;
        default = null;
        example = 50;
        description = ''
          Monthly OpenRouter spend target in USD, tracked over the UTC
          calendar month. Set, it lands as `openrouter.monthlyUsd` in the
          locked settings layer; unset leaves the key out there too
          (`refresh-budget` still records pi's month-to-date spend, but no
          window gates it unless the user settings file sets one).
        '';
      };

      keyFile = lib.mkOption {
        type = lib.types.nullOr (lib.types.strMatching "/.*");
        default = null;
        example = "/run/agenix/openrouter";
        description = ''
          Absolute path to a file whose first line is an OpenRouter API key
          (e.g. an agenix/sops secret path). A string, never `types.path` --
          a path would copy the secret into the Nix store. Set, it lands as
          `openrouter.keyFile` in the locked settings layer (the user
          settings file cannot set this key); `refresh-budget` falls back to
          OPENROUTER_API_KEY when unset, then to pi's own OpenRouter login
          (`~/.pi/agent/auth.json` read-only, `.openrouter.key` for
          `type: "api_key"`, `.openrouter.access` for `type: "oauth"`, else
          `pi auth print-api-key --provider openrouter`, pi 0.83+,
          timeout-bounded). A set keyFile is exclusive:
          pi's login is never consulted.
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

    lockedSettingsFile = lib.mkOption {
      type = lib.types.path;
      readOnly = true;
      default = lockedFile;
      description = ''
        The generated locked settings layer, baked into `dispatch-config` and
        every CLI that resolves settings through it. Read-only; consumers
        reference it rather than reconstructing the store path.
      '';
    };

    userSettings = lib.mkOption {
      type = lib.types.nullOr (lib.types.strMatching "/.*");
      default = null;
      example = "/home/me/nix-config/home/ai/dispatcher/settings.json";
      description = ''
        Path to the user settings file, symlinked to
        `''${XDG_CONFIG_HOME:-~/.config}/dispatcher/settings.json` via
        `mkOutOfStoreSymlink`. A string, never `types.path` -- a path would
        copy it into the Nix store. The target file is not created; point
        this at a file your own dotfiles already manage. Unset (the default),
        no symlink is made and `dispatch-config` reads whatever, if anything,
        already lives at that path.
      '';
    };
  };

  config = lib.mkIf cfg.enable {
    # One `home` attrset, not four `home.*` assignments — statix flags the
    # repeated key.
    home = {
      packages = [pkgsFor.crew pkgsFor.crew-dash pkgsFor.dispatch pkgsFor.dispatch-resume pkgsFor.dispatcher pkgsFor.refresh-scores pkgsFor.refresh-budget pkgsFor.refresh-models pkgsFor.pr-watch pkgsFor.reviewer-roster pkgsFor.permission-check pkgsFor.dispatch-config];

      sessionVariables = {
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

    xdg.configFile = lib.optionalAttrs (cfg.userSettings != null) {
      "dispatcher/settings.json".source = config.lib.file.mkOutOfStoreSymlink cfg.userSettings;
    };
  };
}
