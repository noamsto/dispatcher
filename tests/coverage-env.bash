# shellcheck shell=bash
# BASH_ENV for child bash processes spawned by a coverage run.
# Inert unless BATS_COVERAGE_FILE is set. Each process opens the trace
# itself; nothing here depends on an inherited xtrace fd.
if [[ -n ${BATS_COVERAGE_FILE:-} ]]; then
  PS4='+${BASH_SOURCE[0]}:${LINENO}+ '
  exec {BASH_XTRACEFD}>>"$BATS_COVERAGE_FILE"
  set -x
fi
