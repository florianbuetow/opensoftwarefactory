#!/usr/bin/env bash
set -euo pipefail

mode=${1:?Usage: bash scripts/ci.sh quiet|verbose}
case "$mode" in
    quiet|verbose) ;;
    *) printf 'Invalid CI output mode: %s\n' "$mode" >&2; exit 2 ;;
esac

cd "$(dirname "${BASH_SOURCE[0]}")/.."

run_dir=$(mktemp -d)
trap 'rm -rf "$run_dir"' EXIT
log="$run_dir/check.log"

failed=0
run_check() {
    local name=$1 status=0
    shift
    if [[ $mode == verbose ]]; then
        printf '\n=== %s ===\n' "$name"
        "$@" || status=$?
    else
        "$@" >"$log" 2>&1 || status=$?
    fi
    if (( status != 0 )); then
        printf 'FAIL %s (exit %d)\n' "$name" "$status" >&2
        if [[ $mode == quiet ]]; then
            cat "$log" >&2
        fi
        failed=$((failed + 1))
    else
        printf 'PASS %s\n' "$name"
    fi
}

spellcheck() {
    git ls-files --cached --others --exclude-standard -z -- \
        '*.go' '*.py' '*.sh' '*.bash' '*.js' '*.mjs' '*.md' '*.toml' justfile \
        ':(exclude,glob)**/vendor/**' |
        xargs -0 codespell --check-filenames --ignore-words config/ci/codespell.txt
}

run_check 'Semgrep policies' env XDG_CONFIG_HOME="$run_dir" \
    semgrep scan --config config/ci/semgrep --error --metrics off --disable-version-check .
run_check 'Spelling' spellcheck

for stage in format vet staticcheck security deps audit lint architecture test coverage; do
    run_check "Go $stage" bash scripts/ci-go.sh "$stage"
done

for stage in format style mypy pyright security deps audit test architecture coverage; do
    run_check "Python $stage" bash scripts/ci-python.sh "$stage"
done

if (( failed != 0 )); then
    printf '\n%d CI checks failed\n' "$failed" >&2
    exit 1
fi
printf '\nAll CI checks passed\n'
