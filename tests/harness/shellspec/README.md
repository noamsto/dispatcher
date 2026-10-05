# ShellSpec prototype

This bench-only suite ports the 26 canonical cases without adding ShellSpec to
the default development shell. Run it with:

```console
nix-shell tests/harness/shellspec/shell.nix --run tests/harness/shellspec/run.sh
```

`case-runner.sh` checks every case's executed assertion IDs against
`tests/harness/assertions.tsv`. The suite therefore fails if a mapped assertion
is omitted, duplicated, or assigned to the wrong case. The public script,
environment, temporary Git repository, XDG paths, and PATH-stub boundaries are
retained from the Bats originals.

`boundary-decisions.tsv` records why none of the three entry points can use
ShellSpec `Include` plus `When call`: each script executes its main path when
sourced, and two use `exit` as part of their public error contract. Each example
therefore uses `When run script`, preserving the black-box subprocess boundary.

The adapter emits the common ten-column H04 schema, one independently measured
row per manifest case:

```console
nix-shell tests/harness/shellspec/shell.nix --run \
  'HARNESS_BENCH_HARNESS=shellspec HARNESS_BENCH_MODE=prototype HARNESS_BENCH_REP=1 tests/harness/shellspec/adapter.sh > /tmp/shellspec.tsv'
tests/harness/shellspec/check-results.sh /tmp/shellspec.tsv
```

`measure.sh` separately compares 26 no-op ShellSpec examples with 26 no-op Bats
examples (and therefore `bats-exec-test` startup), plus 100 ShellSpec function
mock calls with 100 PATH-executable mock calls. Function mocks avoid a process
per call, but they cannot cross the retained fresh-Bash boundary. Consequently
they cannot replace the `gh`, `git`, or `tmux` executable fixtures in these
black-box cases without changing the boundary being evaluated; PATH fixtures
remain the parity-preserving choice. Run repeated measurements with:

```console
nix-shell tests/harness/shellspec/shell.nix --run \
  'tests/harness/shellspec/measure.sh 5'
```
