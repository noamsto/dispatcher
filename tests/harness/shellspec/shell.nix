{pkgs ? import <nixpkgs> {}}:
pkgs.mkShell {
  packages = with pkgs; [
    shellspec
    bats
    bash
    coreutils
    findutils
    git
    gh
    gtrash
    jq
    gnugrep
    gnused
    time
  ];
}
