# shellcheck shell=bash
# Line-trace hook, sourced while a bats file is being sourced.
# Collection and setup_file source the file with BATS_TEST_NUMBER unset or -1;
# only a real test (positive BATS_TEST_NUMBER) under BATS_COVERAGE_DIR traces.
# Child bash processes pick up tests/coverage-env.bash via BASH_ENV; this
# shell also traces, so production libs sourced in-process are covered.

_bats_coverage_on() {
  local env_dir
  BATS_COVERAGE_FILE="$BATS_COVERAGE_DIR/$(basename "${BATS_TEST_FILENAME%.bats}").${BATS_TEST_NUMBER}.trace"
  : >"$BATS_COVERAGE_FILE"
  export BATS_COVERAGE_FILE
  env_dir="$(dirname "${BASH_SOURCE[0]}")"
  env_dir="$(cd "$env_dir" && pwd)"
  export BASH_ENV="$env_dir/coverage-env.bash"
  PS4='+${BASH_SOURCE[0]}:${LINENO}+ '
  exec {BASH_XTRACEFD}>>"$BATS_COVERAGE_FILE"
  set -x
}

if [[ -n ${BATS_COVERAGE_DIR:-} && ${BATS_TEST_NUMBER:-} =~ ^[1-9][0-9]*$ ]]; then
  _bats_coverage_on
fi
