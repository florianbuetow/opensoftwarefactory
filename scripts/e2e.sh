#!/usr/bin/env bash
# End-to-end tests against the running factory: ./scripts/e2e.sh [test]
#   ui                 the test fixtures (fixtures/factory, group "test") through the factory UI, then sweep
#   ui-pages           every page has what it shows (files served, requests answered); starts no run
#   ui-sweep           remove every run of a test workflow; fail if one is left
#   openobserve       replacement only in a fresh disposable OpenObserve test stream
#   measurements-live  the real projector against a unique disposable live OpenObserve stream
# Without a test it runs ui, openobserve, and measurements-live. Every test first fails unless the
# factory is fully up. FACTORY_UI overrides the factory UI address (default http://localhost:8082).
set -euo pipefail
cd "$(dirname "$0")/.."

tests="ui openobserve measurements-live"

usage() {
  printf 'usage: %s [ui|ui-pages|ui-sweep|openobserve|measurements-live]\n' "$0" >&2
  exit 2
}

preflight() {
  if ! just status >/dev/null 2>&1; then
    printf 'The factory is not running: start it with just start\n' >&2
    exit 1
  fi
  node scripts/e2e-ui.js preflight
}

run() {
  case "$1" in
    ui) node scripts/e2e-ui.js ;;
    ui-pages) node scripts/e2e-ui.js pages ;;
    ui-sweep) node scripts/e2e-ui.js sweep ;;
    openobserve) node scripts/verify-openobserve.js ;;
    measurements-live) VERIFY_REAL_OPENOBSERVE=1 node scripts/verify-factory-measurements.js ;;
    *) usage ;;
  esac
}

case "${1:-}" in
  "")
    preflight
    for test in $tests; do
      printf '\n=== e2e: %s ===\n' "$test"
      run "$test"
    done
    ;;
  ui | ui-pages | ui-sweep | openobserve | measurements-live)
    preflight
    run "$1"
    ;;
  *) usage ;;
esac
