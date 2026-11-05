# Copies `base` and the agent-browser skill stub into one tree of real files.
# A symlinkJoin would leave every plugin component a symlink out of the plugin
# folder, and Claude Code refuses to read those (the plugin's agents never
# register).
{pkgs}: name: base: subdir: skillTree:
pkgs.runCommandLocal name {} ''
  cp -rL ${base} $out
  chmod -R u+w $out
  mkdir -p $out/${subdir}
  cp -r ${skillTree}/agent-browser $out/${subdir}/
  chmod -R u+w $out/${subdir}/agent-browser
''
