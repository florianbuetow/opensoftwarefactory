#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
ci_project="$repo_root/config/ci/python"
python_sources=(
    "$repo_root/services/tool-gateway"
    "$repo_root/services/devpod"
    "$repo_root/services/git-host"
)
production_sources=(
    "$repo_root/services/tool-gateway/factory.py"
    "$repo_root/services/tool-gateway/server.py"
    "$repo_root/services/tool-gateway/tools"
    "$repo_root/services/devpod/githelper.py"
    "$repo_root/services/devpod/podhttp.py"
    "$repo_root/services/devpod/toolserver.py"
    "$repo_root/services/git-host/server.py"
)

run_ci_tool() {
    uv run --project "$ci_project" --locked "$@"
}

check_format() {
    run_ci_tool ruff format --config "$ci_project/ruff.toml" --check "${python_sources[@]}"
}

check_style() {
    run_ci_tool ruff check --config "$ci_project/ruff.toml" "${python_sources[@]}"
}

check_mypy() {
    (
        cd "$repo_root/services/tool-gateway"
        run_ci_tool mypy --config-file "$ci_project/mypy.ini" factory.py server.py tools
    )
    (
        cd "$repo_root/services/devpod"
        run_ci_tool mypy --config-file "$ci_project/mypy.ini" githelper.py podhttp.py toolserver.py
    )
    (
        cd "$repo_root/services/git-host"
        run_ci_tool mypy --config-file "$ci_project/mypy.ini" server.py
    )
}

check_pyright() {
    run_ci_tool pyright --project "$ci_project/pyrightconfig.json"
}

check_security() {
    run_ci_tool bandit --severity-level medium --confidence-level medium --recursive \
        "${production_sources[@]}"
}

check_deps() {
    (
        cd "$repo_root/services/tool-gateway"
        run_ci_tool deptry .
    )
}

check_audit() {
    local requirements
    requirements=$(mktemp)
    trap 'rm -f "$requirements"' RETURN
    uv export --project "$repo_root/services/tool-gateway" --locked --no-dev \
        --no-emit-project --no-hashes --output-file "$requirements" >/dev/null
    run_ci_tool pip-audit --requirement "$requirements" --no-deps --disable-pip
}

check_test() {
    uv run --project "$repo_root/services/tool-gateway" --locked pytest -q "$repo_root/services/tool-gateway/tests"
    run_ci_tool python -m unittest discover -b -s "$repo_root/services/devpod/tests"
    run_ci_tool python -m unittest discover -b -s "$repo_root/services/git-host" -p 'test_*.py'
}

check_architecture() {
    run_ci_tool pytest -q "$ci_project/test_architecture.py"
}

check_coverage() {
    local failed=0
    mkdir -p "$repo_root/reports/python"
    COVERAGE_FILE="$repo_root/reports/python/tool-gateway.coverage" \
        uv run --project "$repo_root/services/tool-gateway" --locked --with pytest-cov pytest -q \
        --cov=factory --cov=server --cov=tools \
        --cov-branch --cov-report=term-missing --cov-fail-under=80 \
        "$repo_root/services/tool-gateway/tests" || failed=1
    if ! (
        cd "$repo_root/services/devpod" &&
            COVERAGE_FILE="$repo_root/reports/python/devpod.coverage" \
                run_ci_tool coverage run --branch --source=. --omit='tests/*' -m unittest discover -b -s tests &&
            COVERAGE_FILE="$repo_root/reports/python/devpod.coverage" \
                run_ci_tool coverage report --show-missing --fail-under=80
    ); then
        failed=1
    fi
    if ! (
        cd "$repo_root/services/git-host" &&
            COVERAGE_FILE="$repo_root/reports/python/git-host.coverage" \
                run_ci_tool coverage run --branch --source=. --omit='test_*.py' -m unittest discover -b -s . -p 'test_*.py' &&
            COVERAGE_FILE="$repo_root/reports/python/git-host.coverage" \
                run_ci_tool coverage report --show-missing --fail-under=80
    ); then
        failed=1
    fi
    return "$failed"
}

stage=${1-}
case "$stage" in
    format) check_format ;;
    style) check_style ;;
    mypy) check_mypy ;;
    pyright) check_pyright ;;
    security) check_security ;;
    deps) check_deps ;;
    audit) check_audit ;;
    test) check_test ;;
    architecture) check_architecture ;;
    coverage) check_coverage ;;
    *)
        printf 'usage: %s {format|style|mypy|pyright|security|deps|audit|test|architecture|coverage}\n' "$0" >&2
        exit 2
        ;;
esac
