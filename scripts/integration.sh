#!/usr/bin/env bash
# Integration tests: real service processes with isolated temporary data, no cluster needed.
# ./scripts/integration.sh [test]
#   measurements      real blobstore/evaluation processes; no cluster mutation
#   workflow-events   real JetStream/Temporal and factory processes with a deterministic model fixture
# Without a test it runs both.
set -euo pipefail
cd "$(dirname "$0")/.."

tests="measurements workflow-events"

usage() {
  printf 'usage: %s [measurements|workflow-events]\n' "$0" >&2
  exit 2
}

run() {
  case "$1" in
    measurements) node scripts/verify-factory-measurements.js ;;
    workflow-events) node scripts/verify-workflow-events.js ;;
    *) usage ;;
  esac
}

case "${1:-}" in
  "")
    for test in $tests; do
      printf '\n=== integration: %s ===\n' "$test"
      run "$test"
    done
    ;;
  measurements | workflow-events) run "$1" ;;
  *) usage ;;
esac
