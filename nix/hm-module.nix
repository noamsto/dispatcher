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
in {
  options.programs.dispatcher = {
    enable = lib.mkEnableOption "the dispatcher agent fan-out harness";

    profile = lib.mkOption {
      type = lib.types.enum ["work" "personal"];
      default = "personal";
      description = ''
        Gates the codex and cursor engines, which are work-profile only.
        Replaces the DISPATCH_PROFILE environment variable.
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
      packages = [pkgsFor.crew pkgsFor.dispatch pkgsFor.dispatcher];

      sessionVariables = {
        DISPATCH_PROFILE = cfg.profile;
        # Exported, not merely baked into the CLIs. The `dispatcher` slash
        # command and the cursor rule are markdown an agent reads live and
        # resolves through its Bash tool, which a build-time substitution into
        # the shell scripts cannot reach. Each plugin also ships a protocols/
        # copy as the fallback when this is unset (a non-Nix install). Override
        # it in your shell to iterate on a checkout without rebuilding.
        DISPATCHER_PROTOCOL_DIR = "${self}/adapters/core/protocols";
      };

      # Cursor has no plugin format — loose files are the only channel. The .mdc
      # rule is silently ignored without `alwaysApply: true` frontmatter, which
      # the shipped file provides.
      file = {
        ".cursor/rules/dispatcher.mdc".source = "${self}/adapters/cursor/rules/dispatcher.mdc";
        ".cursor/commands" = {
          source = "${self}/adapters/cursor/commands";
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
      activation.dispatcherCodexPlugin = lib.hm.dag.entryAfter ["writeBoundary"] ''
        run rm -rf "$HOME/${codexCache}"
        run mkdir -p "$HOME/${codexCache}"
        run cp -rL ${codexPlugin} "$HOME/${codexCache}/${codexVersion}"
        run chmod -R u+w "$HOME/${codexCache}"
      '';
    };
  };
}
