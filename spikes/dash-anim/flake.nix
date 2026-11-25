{
  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixpkgs-unstable";

  outputs = {nixpkgs, ...}: let
    systems = ["x86_64-linux" "aarch64-linux" "aarch64-darwin" "x86_64-darwin"];
    forAll = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
  in {
    devShells = forAll (pkgs: {
      default = pkgs.mkShell {
        packages = with pkgs; [cargo rustc clippy rustfmt];
      };
    });
  };
}
