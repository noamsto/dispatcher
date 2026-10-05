{pkgs ? import <nixpkgs> {}}:
pkgs.mkShell {
  packages = with pkgs; [
    python3Packages.pytest
    python3Packages.pytest-xdist
    bash
    coreutils
    findutils
    git
    gh
    jq
    gnugrep
    gnused
    time
  ];
}
