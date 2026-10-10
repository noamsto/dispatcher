# Bench shells must use the repo's locked nixpkgs, not the ambient channel:
# all harness legs of one benchmark run have to share one toolchain.
let
  lock = builtins.fromJSON (builtins.readFile ../../../flake.lock);
  locked = lock.nodes.nixpkgs.locked;
  nixpkgs = fetchTarball {
    url = "https://github.com/${locked.owner}/${locked.repo}/archive/${locked.rev}.tar.gz";
    sha256 = locked.narHash;
  };
in
  {pkgs ? import nixpkgs {}}:
    pkgs.mkShell {
      packages = with pkgs; [
        shellspec
        bats
        bash
        coreutils
        findutils
        git
        gh
        # go: the crew arms ported to Go exec crew-go through CREW_GO_BIN, which
        # case-runner.sh builds when the bench adapter has not exported it.
        go
        gtrash
        jq
        gnugrep
        gnused
        time
      ];
    }
