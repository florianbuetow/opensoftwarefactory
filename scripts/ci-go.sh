#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
tool_dir="$repo_root/reports/go/tools"
report_dir="$repo_root/reports/go"
golangci_config="$repo_root/config/ci/go/golangci.yml"
arch_config="$repo_root/config/ci/go/arch-go.yml"

modules=(
    llm-gateway
    blobstore
    mcp-gateway
    connectors
    orchestrator
    registry
    evaluation
    agent
    factory-ui
)

all_stages=(format vet staticcheck security deps audit lint architecture test coverage)

install_tool() {
    local binary=$1
    local package=$2
    local version=${package##*@}
    local marker="$tool_dir/$binary.version"
    if [[ ! -x "$tool_dir/$binary" || ! -f "$marker" || $(<"$marker") != "$version" ]]; then
        mkdir -p "$tool_dir"
        GOBIN="$tool_dir" go install "$package"
        printf '%s\n' "$version" > "$marker"
    fi
}

for_modules() {
    local command_name=$1
    shift
    local module
    for module in "${modules[@]}"; do
        printf 'Go %s [%s]\n' "$command_name" "$module"
        "$@" "$repo_root/services/$module"
    done
}

check_format() {
    install_tool gofumpt mvdan.cc/gofumpt@v0.12.0
    local output
    output=$(
        "$tool_dir/gofumpt" -l \
            "${modules[@]/#/$repo_root/services/}"
    )
    if [[ -n "$output" ]]; then
        printf 'The following Go files require gofumpt:\n%s\n' "$output" >&2
        return 1
    fi
}

run_go_vet() {
    local module_path=$1
    go -C "$module_path" vet ./...
}

check_vet() {
    for_modules vet run_go_vet
}

run_staticcheck() {
    local module_path=$1
    mkdir -p "$report_dir/staticcheck-cache"
    (cd "$module_path" && STATICCHECK_CACHE="${STATICCHECK_CACHE:-$report_dir/staticcheck-cache}" "$tool_dir/staticcheck" ./...)
}

check_staticcheck() {
    install_tool staticcheck honnef.co/go/tools/cmd/staticcheck@v0.8.2
    for_modules staticcheck run_staticcheck
}

run_gosec() {
    local module_path=$1
    local module_name=${module_path##*/}
    mkdir -p "$report_dir/security"
    if ! (cd "$module_path" && "$tool_dir/gosec" -fmt text -out "$report_dir/security/$module_name.txt" ./...); then
        printf 'gosec report [%s]: %s\n' "$module_name" "$report_dir/security/$module_name.txt" >&2
        cat "$report_dir/security/$module_name.txt" >&2
        return 1
    fi
}

check_security() {
    install_tool gosec github.com/securego/gosec/v2/cmd/gosec@v2.30.0
    for_modules gosec run_gosec
}

run_deps() {
    local module_path=$1
    go -C "$module_path" mod tidy -diff
}

check_deps() {
    for_modules dependencies run_deps
}

run_govulncheck() {
    local module_path=$1
    (cd "$module_path" && "$tool_dir/govulncheck" ./...)
}

check_audit() {
    install_tool govulncheck golang.org/x/vuln/cmd/govulncheck@v1.10.0
    for_modules vulnerability-audit run_govulncheck
}

run_golangci() {
    local module_path=$1
    mkdir -p "$report_dir/golangci-cache"
    (cd "$module_path" && GOLANGCI_LINT_CACHE="${GOLANGCI_LINT_CACHE:-$report_dir/golangci-cache}" "$tool_dir/golangci-lint" run --config "$golangci_config" ./...)
}

check_lint() {
    install_tool golangci-lint github.com/golangci/golangci-lint/cmd/golangci-lint@v1.64.8
    for_modules golangci-lint run_golangci
}

arch_files=()

cleanup_arch_files() {
    local path
    for path in "${arch_files[@]}"; do
        if [[ -L "$path" && $(readlink "$path") == "$arch_config" ]]; then
            rm -f "$path"
        fi
    done
}

run_architecture() {
    local module_path=$1
    local local_config="$module_path/arch-go.yml"
    if [[ -e "$local_config" ]]; then
        printf 'Refusing to replace existing architecture configuration: %s\n' "$local_config" >&2
        return 1
    fi
    ln -s "$arch_config" "$local_config"
    arch_files+=("$local_config")
    (cd "$module_path" && "$tool_dir/arch-go" --color no)
    cleanup_arch_files
}

check_architecture() {
    install_tool arch-go github.com/arch-go/arch-go/v2@v2.1.2
    trap cleanup_arch_files EXIT
    for_modules architecture run_architecture
    trap - EXIT
}

run_tests() {
    local module_path=$1
    go -C "$module_path" test -race -v ./...
}

check_test() {
    for_modules tests run_tests
}

check_coverage() {
    mkdir -p "$report_dir/coverage"
    local module profile profile_from_module coverage
    local failed=0
    for module in "${modules[@]}"; do
        profile="$report_dir/coverage/$module.out"
        profile_from_module="../../reports/go/coverage/$module.out"
        printf 'Go coverage [%s]\n' "$module"
        go -C "$repo_root/services/$module" test -race -covermode=atomic -coverprofile="$profile_from_module" ./...
        go -C "$repo_root/services/$module" tool cover \
            -html="$profile" \
            -o "$report_dir/coverage/$module.html"
        coverage=$(
            go -C "$repo_root/services/$module" tool cover -func="$profile" \
                | awk '/^total:/ {gsub(/%/, "", $3); print $3}'
        )
        printf 'Go statement coverage [%s]: %s%%\n' "$module" "$coverage"
        if ! awk -v coverage="$coverage" 'BEGIN { exit !(coverage >= 80) }'; then
            printf 'Go coverage [%s] %s%% is below the required 80%%\n' "$module" "$coverage" >&2
            failed=1
        fi
    done
    return "$failed"
}

run_stage() {
    local stage=$1
    case "$stage" in
        format) check_format ;;
        vet) check_vet ;;
        staticcheck) check_staticcheck ;;
        security) check_security ;;
        deps) check_deps ;;
        audit) check_audit ;;
        lint) check_lint ;;
        architecture) check_architecture ;;
        test) check_test ;;
        coverage) check_coverage ;;
        *)
            printf 'Unknown Go CI stage: %s\nValid stages: %s\n' "$stage" "${all_stages[*]}" >&2
            return 2
            ;;
    esac
}

if [[ $# -gt 1 ]]; then
    printf 'Usage: %s [stage]\n' "$0" >&2
    exit 2
fi

if [[ $# -eq 1 ]]; then
    run_stage "$1"
else
    for stage in "${all_stages[@]}"; do
        run_stage "$stage"
    done
fi
