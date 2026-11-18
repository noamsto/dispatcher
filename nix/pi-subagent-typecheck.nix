# Type-checks the pi subagent extension against the types the pinned
# pi-coding-agent ships (dist/*.d.ts plus its node_modules), offline. Catches an
# import or API break a pi upgrade would cause. Evaluation fails when `pi` is not
# the version contract.txt pins, so a re-pin must bump the nixpkgs-pi input too.
{
  pkgs,
  pi,
  src,
  contract,
}: let
  pinned = builtins.head (builtins.match ".*\nversion\t([^\n]+)\n.*" (builtins.readFile contract));
  root = "${pi}/lib/node_modules/pi-monorepo";
  tsconfig = pkgs.writeText "tsconfig.json" (builtins.toJSON {
    compilerOptions = {
      target = "ES2022";
      module = "ESNext";
      moduleResolution = "Bundler";
      strict = true;
      noEmit = true;
      skipLibCheck = true;
      allowImportingTsExtensions = true;
      types = ["node"];
    };
    include = ["ext/*.ts"];
  });
in
  assert pi.version == pinned || throw "pi-subagent-typecheck: nixpkgs-pi has pi ${pi.version}, contract.txt pins ${pinned}";
    pkgs.runCommand "pi-subagent-typecheck" {nativeBuildInputs = [pkgs.typescript];} ''
      mkdir -p work/node_modules/@earendil-works work/ext
      cd work
      for e in ${root}/node_modules/*; do
        [ "$(basename "$e")" = @earendil-works ] || ln -s "$e" node_modules/
      done
      for e in ${root}/node_modules/@earendil-works/*; do
        ln -s "$e" node_modules/@earendil-works/
      done
      ln -s ${root} node_modules/@earendil-works/pi-coding-agent
      cp ${src}/*.ts ext/
      cp ${tsconfig} tsconfig.json
      tsc -p .
      touch $out
    ''
