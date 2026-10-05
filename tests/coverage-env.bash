# shellcheck shell=bash
# BASH_ENV for child bash processes spawned by a coverage run.
# Inert unless BATS_COVERAGE_FILE is set. Each process reopens the trace
# by path rather than relying on the inherited xtrace fd (which points at
# the old inode once the janitor has unlinked it). The -e guard keeps a
# detached child that outlives its test from re-creating a trace the
# janitor already reduced.
if [[ -n ${BATS_COVERAGE_FILE:-} && -e $BATS_COVERAGE_FILE ]]; then
  PS4='+${BASH_SOURCE[0]}:${LINENO}+ '
  exec {BASH_XTRACEFD}>>"$BATS_COVERAGE_FILE"
  set -x
fi
