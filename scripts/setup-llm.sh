#!/usr/bin/env bash
# Interactive setup of the LLM gateway's local model provider.
#
# Detects well-known local LLM runtimes (installed or already serving), offers to
# start one, lists its models, lets you pick with fzf, and writes the choice to
# services/llm-gateway/config/config.yaml. Compatible with bash 3.2 (macOS).
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
service="$root/services/llm-gateway"
config="$service/config/config.yaml"
template="$service/config.template"
seed_agents="$root/services/registry/seed/agents"
# The gateway runs in KIND inside Colima/Lima and reaches the host by this name.
host_alias="host.lima.internal"

blue() { printf '\033[0;34m%s\033[0m\n' "$*"; }
yellow() { printf '\033[0;33m%s\033[0m\n' "$*"; }
green() { printf '\033[0;32m%s\033[0m\n' "$*"; }
red() { printf '\033[0;31m%s\033[0m\n' "$*" >&2; }
die() { red "✗ $*"; exit 1; }

# --- Known runtimes: id|label|default port|executable candidates ---------------

RUNTIMES='lm-studio|LM Studio|1234|lms:$HOME/.lmstudio/bin/lms:/Applications/LM Studio.app
ollama|Ollama|11434|ollama:/Applications/Ollama.app
llama-cpp|llama.cpp (llama-server)|8080|llama-server
mlx-lm|MLX LM (mlx_lm.server)|8090|mlx_lm.server
vllm|vLLM|8000|vllm
jan|Jan|1337|/Applications/Jan.app'

field() { printf '%s' "$1" | cut -d'|' -f"$2"; }

# Print the first candidate that exists (executable on PATH, file, or app bundle).
find_executable() {
    local candidate expanded
    local IFS=':'
    for candidate in $1; do
        expanded=$(eval "printf '%s' \"$candidate\"")
        case "$expanded" in
            /*) [ -e "$expanded" ] && { printf '%s' "$expanded"; return 0; } ;;
            *) command -v "$expanded" 2>/dev/null && return 0 ;;
        esac
    done
    return 1
}

# Succeeds when an OpenAI-compatible server answers on the port.
serving() { curl -fsS --max-time 1 "http://127.0.0.1:$1/v1/models" >/dev/null 2>&1; }

wait_for_server() {
    local port=$1 i
    for i in $(seq 1 60); do
        serving "$port" && return 0
        sleep 0.5
    done
    return 1
}

# --- Preconditions --------------------------------------------------------------

[ -t 0 ] && [ -t 1 ] || die "This setup is interactive and needs a terminal"
command -v fzf >/dev/null 2>&1 || die "fzf is required: brew install fzf"
command -v curl >/dev/null 2>&1 || die "curl is required"
command -v jq >/dev/null 2>&1 || die "jq is required: brew install jq"
[ -f "$template" ] || die "Missing $template; run from a complete checkout"

echo ""
blue "=== Setting up the local LLM provider ==="

# --- 1. Detect runtimes ---------------------------------------------------------

menu=""
while IFS= read -r line; do
    id=$(field "$line" 1)
    label=$(field "$line" 2)
    port=$(field "$line" 3)
    exe=$(find_executable "$(field "$line" 4)" || true)
    if serving "$port"; then
        state="● serving"
    elif [ -n "$exe" ]; then
        state="○ installed"
    else
        continue
    fi
    menu+=$(printf '%s\t%-12s %-26s :%s\n' "$id" "$state" "$label" "$port")$'\n'
done <<EOF
$RUNTIMES
EOF
menu+=$(printf 'custom\t%-12s %-26s\n' "…" "Other OpenAI-compatible server")

pick=$(printf '%s\n' "$menu" | fzf --height=~50% --reverse --delimiter=$'\t' --with-nth=2 \
    --prompt='runtime> ' --header='Pick the local LLM runtime to wire into the gateway' \
    --no-multi) || die "Cancelled"
runtime=$(printf '%s' "$pick" | cut -f1)

# --- 2. Resolve the server: port, start if needed -------------------------------

if [ "$runtime" = custom ]; then
    printf 'Base URL of the server as reachable from this machine (e.g. http://127.0.0.1:9000): '
    read -r custom_url
    custom_url=${custom_url%/}
    custom_url=${custom_url%/v1}
    case "$custom_url" in http://*|https://*) ;; *) die "URL must start with http:// or https://" ;; esac
    local_base=$custom_url
    curl -fsS --max-time 3 "$local_base/v1/models" >/dev/null || die "No OpenAI-compatible server answers at $local_base/v1/models"
    label="custom"
    provider_name="local-llm"
    gateway_base="${custom_url/localhost/$host_alias}"
    gateway_base="${gateway_base/127.0.0.1/$host_alias}/v1"
else
    line=$(printf '%s\n' "$RUNTIMES" | grep "^$runtime|")
    label=$(field "$line" 2)
    port=$(field "$line" 3)
    exe=$(find_executable "$(field "$line" 4)" || true)
    provider_name=$runtime
    local_base="http://127.0.0.1:$port"
    gateway_base="http://$host_alias:$port/v1"

    if ! serving "$port"; then
        yellow "$label is not serving on port $port."
        log="${TMPDIR:-/tmp}/osf-llm-$runtime.log"
        case "$runtime" in
            lm-studio)
                [ -x "$exe" ] || die "LM Studio is installed but its lms CLI is not: open LM Studio once, then run ~/.lmstudio/bin/lms bootstrap"
                start=("$exe" server start --port "$port") ;;
            ollama)
                start=("$exe" serve) ;;
            mlx-lm)
                start=("$exe" --port "$port") ;;
            llama-cpp)
                die "llama-server serves one model: start it yourself, for example: llama-server -m model.gguf --port $port, then run this again" ;;
            vllm)
                die "vLLM serves one model: start it yourself, for example: vllm serve <model> --port $port, then run this again" ;;
            jan)
                open -a Jan 2>/dev/null || true
                die "Enable Jan's local API server in Settings, then run this again" ;;
        esac
        answer=$(printf 'Start %s now\nCancel\n' "$label" | fzf --height=~30% --reverse --prompt='start?> ' \
            --header="$label is installed but not serving") || die "Cancelled"
        [ "$answer" = Cancel ] && die "Cancelled"
        if [ "$runtime" = lm-studio ]; then
            "${start[@]}" >"$log" 2>&1 || { cat "$log" >&2; die "Could not start $label"; }
        else
            nohup "${start[@]}" >"$log" 2>&1 &
            disown 2>/dev/null || true
        fi
        printf 'Waiting for %s on port %s…\n' "$label" "$port"
        wait_for_server "$port" || die "$label did not start; see $log"
        green "✓ $label is serving on port $port"
    else
        green "✓ $label is serving on port $port"
    fi
fi

# --- 3. List models and choose --------------------------------------------------

list_models() {
    local rows=""
    if [ "$runtime" = lm-studio ]; then
        rows=$(curl -fsS --max-time 5 "$local_base/api/v0/models" 2>/dev/null |
            jq -r '.data[]? | select(.type != "embeddings") | [.id, (.state // ""), (.arch // ""), (.max_context_length // "")] | @tsv' 2>/dev/null || true)
    fi
    if [ -z "$rows" ]; then
        rows=$(curl -fsS --max-time 5 "$local_base/v1/models" |
            jq -r '.data[]?.id | select(test("embed|rerank"; "i") | not) | [., "", "", ""] | @tsv')
    fi
    printf '%s' "$rows"
}

models=$(list_models) || die "Could not list models from $local_base"
if [ -z "$models" ]; then
    case "$runtime" in
        lm-studio) die "LM Studio reports no language models: download one in LM Studio first" ;;
        ollama) die "Ollama has no models: ollama pull qwen3:8b" ;;
        *) die "$label reports no models" ;;
    esac
fi

selected=$(printf '%s\n' "$models" | column -t -s$'\t' | fzf --multi --height=~70% --reverse \
    --prompt='models> ' \
    --header=$'TAB marks models, ENTER confirms (at least one)\nmodel · state · architecture · context' ) || die "Cancelled"
ids=$(printf '%s\n' "$selected" | awk '{print $1}')
[ -n "$ids" ] || die "No model selected"

# --- 4. Write the gateway configuration -----------------------------------------

if [ -f "$config" ]; then
    head_part=$(sed '/^providers:/,$d' "$config")
else
    head_part=$(sed '/^# The factory uses only/,$d' "$template")
fi
discovery=""
if [ "$runtime" = lm-studio ]; then
    discovery="    model_discovery: lm-studio # lists and routes only the LLM instances LM Studio has loaded"
fi

new_config=$(
    printf '%s\n' "$head_part" | sed -e :a -e '/^\n*$/{$d;N;ba' -e '}'
    printf '\n# Generated by scripts/setup-llm.sh (just llm). The factory uses only these models.\n'
    printf 'providers:\n'
    printf '  - name: %s\n' "$provider_name"
    printf '    base_url: %s\n' "$gateway_base"
    printf '    auth:\n      type: none\n'
    printf '    streaming: true\n'
    [ -z "$discovery" ] || printf '%s\n' "$discovery"
    printf '    models:\n'
    printf '%s\n' "$ids" | while IFS= read -r id; do printf '      "%s": "%s"\n' "$id" "$id"; done
)

echo ""
yellow "New provider configuration:"
printf '%s\n' "$new_config" | sed -n '/^providers:/,$p'
echo ""
if [ -f "$config" ] && [ "$new_config" = "$(cat "$config")" ]; then
    green "✓ $config already matches"
else
    answer=$(printf 'Write %s\nCancel\n' "${config#"$root"/}" | fzf --height=~30% --reverse --prompt='write?> ') || die "Cancelled"
    [ "$answer" = Cancel ] && die "Cancelled"
    mkdir -p "$(dirname "$config")"
    if [ -f "$config" ]; then
        cp "$config" "$config.bak"
        yellow "Previous configuration saved as ${config#"$root"/}.bak"
    fi
    printf '%s\n' "$new_config" >"$config"
    green "✓ Wrote ${config#"$root"/}"
fi

# --- 5. Report agents that reference models not configured ----------------------

if [ -d "$seed_agents" ]; then
    missing=$(find "$seed_agents" -name agent.json -exec jq -r '.models[]?' {} + 2>/dev/null | sort -u |
        while IFS= read -r m; do printf '%s\n' "$ids" | grep -Fxq "$m" || printf '%s\n' "$m"; done)
    if [ -n "$missing" ]; then
        echo ""
        yellow "Seeded agents reference models you did not select:"
        printf '  %s\n' $missing
        yellow "Select a configured model for those agents in the Factory UI."
    fi
fi

echo ""
yellow "If the cluster is running, apply the change with: just deploy"
green "✓ Local LLM provider configured"
echo ""
