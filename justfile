# Justfile conventions:
# - Use printf for ANSI colors: blue headers, yellow groups/warnings,
#   green success (✓), red failure (✗); always reset colors.
# - Wrap recipe output in blank echo lines.
# - Default to help; clear the screen and group commands in usage order.
# - Keep help and recipe ordering aligned; document each recipe.
# - Fail fast in composite recipes and end with a clear status message.
# - Use uv run for Python commands, if any.

set shell := ["bash", "-eu", "-o", "pipefail", "-c"]

# Central deployment list, in rollout order. Images are built from every services/*/Dockerfile.
services := "jetstream llm-gateway openobserve telemetry-collector temporal blobstore agent-registry policy-registry skill-registry tool-gateway mcp-gateway git-host connectors orchestrator evaluation agent factory-ui"
cluster := "opensoftwarefactory"
context := "kind-" + cluster
namespace := "opensoftwarefactory"
export DOCKER_HOST := "unix://" + env_var("HOME") + "/.colima/default/docker.sock"

# Default recipe: show available commands.
_default:
    @just help


# Initialize local configuration and KIND host mounts without overwriting credentials.
init:
    #!/usr/bin/env bash
    set -euo pipefail
    echo ""
    trap 'printf "\033[0;31m✗ init failed\033[0m\n" >&2' ERR
    printf "\033[0;34m=== Initializing local configuration ===\033[0m\n"
    node scripts/init-llm-gateway.js
    node scripts/init-openobserve.js
    node scripts/init-orchestrator.js
    node scripts/init-jetstream.js
    node scripts/generate-kind-config.js
    printf "\033[0;32m✓ Local configuration initialized\033[0m\n"
    echo ""

# Create the project KIND cluster only. Build with `just build` and deploy with `just deploy`.
cluster-create:
    #!/usr/bin/env bash
    set -euo pipefail
    echo ""
    trap 'printf "\033[0;31m✗ cluster-create failed\033[0m\n" >&2' ERR
    printf "\033[0;34m=== Creating the project cluster ===\033[0m\n"
    if ! docker info >/dev/null 2>&1; then colima start; fi
    just _vm-limits
    if [ ! -f infra/kind/cluster.local.yaml ]; then
        node scripts/generate-kind-config.js
    fi
    if docker ps -aq --filter label=io.x-k8s.kind.cluster={{cluster}} | grep -q .; then
        trap - ERR
        printf "\033[0;31m✗ Cluster {{cluster}} already exists\033[0m\n" >&2
        exit 1
    fi
    # Public images need no credentials. Avoid a stale Docker Desktop helper.
    export DOCKER_CONFIG=$(mktemp -d)
    trap 'rm -rf "$DOCKER_CONFIG"' EXIT
    kind create cluster --name {{cluster}} --config infra/kind/cluster.local.yaml --wait 120s
    docker exec {{cluster}}-control-plane install -d -o 1000 -g 1000 /var/lib/opensoftwarefactory/temporal
    printf "\033[0;32m✓ Project cluster created; deploy services with just deploy\033[0m\n"
    echo ""

# Raise the Colima VM inotify limit, which a VM restart resets to 128; at 128,
# kube-proxy fails with "too many open files" and cluster DNS stops working.
_vm-limits:
    @colima ssh -- sudo sysctl -q -w fs.inotify.max_user_instances=512

# Delete only this project cluster. Factory data on the host and the Colima VM disk remains.
cluster-delete:
    #!/usr/bin/env bash
    set -euo pipefail
    echo ""
    trap 'printf "\033[0;31m✗ cluster-delete failed\033[0m\n" >&2' ERR
    printf "\033[0;34m=== Deleting project cluster ===\033[0m\n"
    kind delete cluster --name {{cluster}}
    printf "\033[0;32m✓ Project cluster deleted; data retained\033[0m\n"
    echo ""

# Delete all workflows, stored artifacts, and Temporal run history, then re-initialize.
reset:
    #!/usr/bin/env bash
    set -euo pipefail
    echo ""
    trap 'printf "\033[0;31m✗ reset failed\033[0m\n" >&2' ERR
    printf "\033[0;34m=== Resetting factory data ===\033[0m\n"
    printf "\033[0;33mThis deletes every workflow, stored artifact, and run history on this host.\033[0m\n"
    just _reset-data "temporal blobstore orchestrator agent-registry policy-registry skill-registry" \
        "services/blobstore/data/objects services/blobstore/data/meta" \
        "/var/lib/opensoftwarefactory/temporal"
    printf "\033[0;32m✓ Factory data reset\033[0m\n"
    echo ""

# Delete OpenObserve telemetry data while keeping its configuration and credentials.
reset-observe:
    #!/usr/bin/env bash
    set -euo pipefail
    echo ""
    trap 'printf "\033[0;31m✗ reset-observe failed\033[0m\n" >&2' ERR
    printf "\033[0;34m=== Resetting OpenObserve data ===\033[0m\n"
    printf "\033[0;33mThis deletes all stored logs, metrics, traces, and dashboards on this host.\033[0m\n"
    just _reset-data "openobserve" "services/openobserve/data" ""
    printf "\033[0;32m✓ OpenObserve data reset\033[0m\n"
    echo ""

# Stop the given deployments if they run, delete the given host paths and
# cluster volume claims, re-initialize, and restart.
_reset-data deployments paths node_paths:
    #!/usr/bin/env bash
    set -euo pipefail
    if ! docker info >/dev/null 2>&1 || ! docker ps -q --filter label=io.x-k8s.kind.cluster={{cluster}} | grep -q . || \
       ! kubectl --context {{context}} --request-timeout=5s get namespace {{namespace}} >/dev/null 2>&1; then
        printf "\033[0;31m✗ Cannot reset data while cluster {{cluster}} is unavailable; run just start first\033[0m\n" >&2
        exit 1
    fi
    original_deployments=()
    original_replicas=()
    running=()
    for deployment in {{deployments}}; do
        replicas=$(kubectl --context {{context}} --namespace {{namespace}} get deployment "$deployment" \
            -o jsonpath='{.spec.replicas}')
        original_deployments+=("$deployment")
        original_replicas+=("${replicas:-0}")
        if [ "${replicas:-0}" -gt 0 ]; then running+=("$deployment"); fi
    done
    scaled=0
    restore_replicas() {
        for index in "${!original_deployments[@]}"; do
            deployment=${original_deployments[$index]}
            kubectl --context {{context}} --namespace {{namespace}} scale deployment "$deployment" \
                --replicas="${original_replicas[$index]}" >/dev/null
        done
        for deployment in "${running[@]}"; do
            kubectl --context {{context}} --namespace {{namespace}} rollout status "deployment/$deployment" --timeout=300s
        done
    }
    cleanup() {
        status=$?
        if [ "$scaled" = 1 ]; then
            set +e
            restore_replicas
            set -e
        fi
        exit "$status"
    }
    trap cleanup EXIT
    if [ "${#running[@]}" -gt 0 ]; then
        printf "\033[0;33mStopping {{deployments}} so their files are not in use.\033[0m\n"
        kubectl --context {{context}} --namespace {{namespace}} scale deployment "${running[@]}" --replicas=0
        scaled=1
        selector=$(IFS=,; printf '%s' "${running[*]}")
        kubectl --context {{context}} --namespace {{namespace}} wait --for=delete pod \
            -l "app.kubernetes.io/name in ($selector)" --timeout=300s
    fi
    # A directory is emptied, not removed: the KIND node bind-mounts some of them, and a
    # recreated directory would stay invisible to the node until it restarts.
    for path in {{paths}}; do
        if [ -d "$path" ]; then find "$path" -mindepth 1 -delete; else rm -f -- "$path"; fi
    done
    for path in {{node_paths}}; do
        docker exec {{cluster}}-control-plane find "$path" -mindepth 1 -delete
    done
    just init
    scaled=0
    restore_replicas
    trap - EXIT

# Show the available project commands.
help:
    @echo ""
    @clear
    @echo ""
    @printf "\033[0;34m=== opensoftwarefactory ===\033[0m\n"
    @echo ""
    @printf "\033[0;33mSetup & Lifecycle:\033[0m\n"
    @printf "  %-38s %s\n" "just init" "Initialize local configuration without overwriting it"
    @printf "  %-38s %s\n" "just sync-seeds" "Publish optional local scratch/seeds to the cluster"
    @printf "  %-38s %s\n" "just verify-measurements" "Test isolated real measurement service processes"
    @printf "  %-38s %s\n" "just verify-measurements-live" "Test real projection in a disposable OpenObserve stream"
    @printf "  %-38s %s\n" "just verify-workflow-events" "Test real isolated JetStream/Temporal intake and judges"
    @printf "  %-38s %s\n" "just verify-openobserve" "Verify replacement in a disposable telemetry stream"
    @printf "  %-38s %s\n" "just cluster-create" "Create the empty KIND cluster"
    @printf "  %-38s %s\n" "just cluster-delete" "Delete the project cluster, keeping all data"
    @printf "  %-38s %s\n" "just reset" "Delete workflows, artifacts, and run history, then re-init"
    @printf "  %-38s %s\n" "just reset-observe" "Delete OpenObserve telemetry data, keeping its config"
    @printf "  %-38s %s\n" "just help" "Show available commands"
    @echo ""
    @printf "\033[0;33mRun & Pipeline:\033[0m\n"
    @printf "  %-38s %s\n" "just start" "Bring the factory up: run init, cluster-create, build and deploy as needed"
    @printf "  %-38s %s\n" "just stop" "Stop the project cluster and all its services"
    @printf "  %-38s %s\n" "just build" "Build all local images"
    @printf "  %-38s %s\n" "just deploy" "Deploy existing images to the running cluster"
    @printf "  %-38s %s\n" "just update-ui" "Build and deploy only the factory UI"
    @printf "  %-38s %s\n" "just status" "Show project pods and services"
    @printf "  %-38s %s\n" "just info" "Show each service's URLs and whether it is up"
    @echo ""
    @printf "\033[0;33mAccess & Configuration:\033[0m\n"
    @printf "  %-38s %s\n" "just ui" "Open the factory UI at localhost:8082"
    @printf "  %-38s %s\n" "just gateway" "Forward the LLM gateway to localhost:8081"
    @printf "  %-38s %s\n" "just observe" "Open OpenObserve at localhost:5080"
    @printf "  %-38s %s\n" "just temporal" "Open the Temporal UI at localhost:8233"
    @printf "  %-38s %s\n" "just render" "Render the central Kubernetes configuration"
    @echo ""
    @printf "\033[0;33mCI & Testing:\033[0m\n"
    @printf "  %-38s %s\n" "just check" "Check scripts, manifests, Go race tests, and Python tests"
    @printf "  %-38s %s\n" "just ci" "Run Go and Python guardrails with summaries and failure output"
    @printf "  %-38s %s\n" "just ci-verbose" "Run the same guardrails and stream all output"
    @echo ""
    @printf "\033[0;32m✓ Available commands shown\033[0m\n"
    @echo ""


# Bring the factory up from any state. This is the only target that runs other steps for
# you: it runs init, cluster-create and build only when they have not been done, starts a
# stopped cluster, and deploys.
start:
    #!/usr/bin/env bash
    set -euo pipefail
    echo ""
    trap 'printf "\033[0;31m✗ start failed\033[0m\n" >&2' ERR
    printf "\033[0;34m=== Starting the factory ===\033[0m\n"
    if ! docker info >/dev/null 2>&1; then colima start; fi
    just _vm-limits
    just init
    nodes=$(docker ps -aq --filter label=io.x-k8s.kind.cluster={{cluster}})
    if [ -z "$nodes" ]; then
        just cluster-create
        nodes=$(docker ps -aq --filter label=io.x-k8s.kind.cluster={{cluster}})
    fi
    running=$(docker ps -q --filter label=io.x-k8s.kind.cluster={{cluster}})
    for node in $nodes; do
        if ! printf '%s\n' "$running" | grep -Fxq "$node"; then docker start "$node" >/dev/null; fi
    done
    for attempt in $(seq 1 60); do
        if kubectl --context {{context}} --request-timeout=5s get nodes >/dev/null 2>&1; then break; fi
        if [ "$attempt" -eq 60 ]; then
            trap - ERR
            printf "\033[0;31m✗ Cluster {{cluster}} did not become ready\033[0m\n" >&2
            exit 1
        fi
        sleep 2
    done
    for dockerfile in services/*/Dockerfile; do
        image="opensoftwarefactory-$(basename "$(dirname "$dockerfile")"):local"
        if ! docker image inspect "$image" >/dev/null 2>&1; then just build; break; fi
    done
    just deploy
    printf "\033[0;32m✓ Factory started\033[0m\n"
    echo ""

# Stop this project's KIND node containers. Cluster state and data remain intact.
stop:
    #!/usr/bin/env bash
    set -euo pipefail
    echo ""
    trap 'printf "\033[0;31m✗ stop failed\033[0m\n" >&2' ERR
    printf "\033[0;34m=== Stopping the project cluster ===\033[0m\n"
    if ! docker info >/dev/null 2>&1; then
        trap - ERR
        printf "\033[0;31m✗ Container runtime is unavailable; no cluster state was changed\033[0m\n" >&2
        exit 1
    fi
    nodes=$(docker ps -q --filter label=io.x-k8s.kind.cluster={{cluster}})
    if [ -n "$nodes" ]; then docker stop $nodes >/dev/null; fi
    printf "\033[0;32m✓ Project cluster and all its services stopped; data retained\033[0m\n"
    echo ""

# Publish optional ignored local seeds; no seed payload is built into an image.
sync-seeds:
    node scripts/sync-seeds.js {{cluster}}-control-plane

# Test real local services with isolated temporary data; no cluster mutation.
verify-measurements:
    node scripts/verify-factory-measurements.js

# Exercise the actual projector against a unique disposable live OpenObserve stream.
verify-measurements-live:
    VERIFY_REAL_OPENOBSERVE=1 node scripts/verify-factory-measurements.js

# Test actual isolated broker/workflow processes with a deterministic model fixture.
verify-workflow-events:
    node scripts/verify-workflow-events.js

# Verify replacement only in a fresh disposable OpenObserve test stream.
verify-openobserve:
    node scripts/verify-openobserve.js

# Recovery only with explicit authorization to interrupt all workloads on default Colima.
_restart-shared-runtime approval:
    #!/usr/bin/env bash
    set -euo pipefail
    if [ "{{approval}}" != "shared-runtime-approved" ]; then
        echo "Explicit shared-runtime authorization is required." >&2
        exit 1
    fi
    running=()
    while IFS= read -r container; do
        [ -z "$container" ] || running+=("$container")
    done < <(docker ps -q)
    colima --profile default stop
    colima --profile default start
    if [ "${#running[@]}" -gt 0 ]; then docker start "${running[@]}"; fi
    just _vm-limits

# Prepare durable event storage and install generated local credentials.
_event-storage:
    docker exec {{cluster}}-control-plane install -d -o 1000 -g 1000 /var/lib/opensoftwarefactory/jetstream /var/lib/opensoftwarefactory/evaluation
    kubectl --context {{context}} apply -f infra/kubernetes/overlays/local/factory/namespace.yaml
    kubectl --context {{context}} apply -f scratch/measurements/jetstream/secret.json

# Deploy already-built local images and manifests to the running project cluster.
deploy:
    #!/usr/bin/env bash
    set -euo pipefail
    echo ""
    trap 'printf "\033[0;31m✗ deploy failed\033[0m\n" >&2' ERR
    printf "\033[0;34m=== Deploying existing images ===\033[0m\n"
    if ! docker info >/dev/null 2>&1 || ! docker ps -q --filter label=io.x-k8s.kind.cluster={{cluster}} | grep -q .; then
        trap - ERR
        printf "\033[0;31m✗ Cluster {{cluster}} is not running; run just start first\033[0m\n" >&2
        exit 1
    fi
    if ! kubectl --context {{context}} --request-timeout=5s get nodes >/dev/null 2>&1; then
        trap - ERR
        printf "\033[0;31m✗ Cluster {{cluster}} is not ready; run just start first\033[0m\n" >&2
        exit 1
    fi
    images=()
    missing=()
    for dockerfile in services/*/Dockerfile; do
        image="opensoftwarefactory-$(basename "$(dirname "$dockerfile")"):local"
        if docker image inspect "$image" >/dev/null 2>&1; then images+=("$image"); else missing+=("$image"); fi
    done
    if [ "${#missing[@]}" -gt 0 ]; then
        trap - ERR
        printf "\033[0;31m✗ Required images have not been built: %s; run just build\033[0m\n" "${missing[*]}" >&2
        exit 1
    fi
    just init
    just sync-seeds
    just _event-storage
    kind load docker-image "${images[@]}" --name {{cluster}}
    existing=""
    if kubectl --context {{context}} --namespace {{namespace}} get namespace {{namespace}} >/dev/null 2>&1; then
        existing=$(kubectl --context {{context}} --namespace {{namespace}} get deployment -o name 2>/dev/null || true)
    fi
    kubectl --context {{context}} apply -k infra/kubernetes/overlays/local
    for service in {{services}}; do
        if printf '%s\n' "$existing" | grep -Fxq "deployment.apps/$service" || printf '%s\n' "$existing" | grep -Fxq "deployment/$service"; then
            kubectl --context {{context}} --namespace {{namespace}} rollout restart "deployment/$service"
        fi
    done
    just _wait
    printf "\033[0;32m✓ Existing images deployed\033[0m\n"
    echo ""

# Build every services/*/Dockerfile and nothing else. `just deploy` loads the resulting images.
build:
    #!/usr/bin/env bash
    set -euo pipefail
    echo ""
    trap 'printf "\033[0;31m✗ build failed\033[0m\n" >&2' ERR
    printf "\033[0;34m=== Building local images ===\033[0m\n"
    if ! docker info >/dev/null 2>&1; then colima start; fi
    # Public images need no credentials. Avoid a stale Docker Desktop helper.
    export DOCKER_CONFIG=$(mktemp -d)
    trap 'rm -rf "$DOCKER_CONFIG"' EXIT
    for dockerfile in services/*/Dockerfile; do
        dir=$(dirname "$dockerfile")
        image="opensoftwarefactory-$(basename "$dir"):local"
        docker build --tag "$image" "$dir"
    done
    # Each rebuild leaves the previous :local images behind without a name; remove them.
    docker image prune -f >/dev/null
    printf "\033[0;32m✓ Local images built; deploy them with just deploy\033[0m\n"
    echo ""

# Remove only unused dangling image cache, never containers or volumes.
_prune-image-cache:
    docker image prune -f

# Clear unused rebuildable builder cache after builds, without deleting images or data.
_prune-build-cache:
    docker builder prune -f --keep-storage 512MB

# Rebuild the measurement backend after a focused integration correction.
_build-evaluation:
    docker build --tag opensoftwarefactory-evaluation:local services/evaluation

# Resume a build interrupted after the earlier service images were verified.
_build-final-services:
    #!/usr/bin/env bash
    set -euo pipefail
    export DOCKER_CONFIG=$(mktemp -d)
    trap 'rm -rf "$DOCKER_CONFIG"' EXIT
    docker build --tag opensoftwarefactory-registry:local services/registry
    docker build --tag opensoftwarefactory-tool-gateway:local services/tool-gateway

# Allow retained-artifact metadata migration to finish before liveness applies.
_deploy-blobstore:
    kubectl --context {{context}} --namespace {{namespace}} apply -f infra/kubernetes/base/blobstore/deployment.yaml
    kubectl --context {{context}} --namespace {{namespace}} rollout status deployment/blobstore --timeout=600s

# Load and roll out only the already-built evaluation image.
_deploy-evaluation:
    kind load docker-image opensoftwarefactory-evaluation:local --name {{cluster}}
    kubectl --context {{context}} --namespace {{namespace}} apply -f infra/kubernetes/base/evaluation/deployment.yaml
    kubectl --context {{context}} --namespace {{namespace}} rollout restart deployment/evaluation
    kubectl --context {{context}} --namespace {{namespace}} rollout status deployment/evaluation --timeout=300s

# Build and roll out only the orchestrator after a focused recovery correction.
_update-orchestrator:
    docker build --tag opensoftwarefactory-orchestrator:local services/orchestrator
    kind load docker-image opensoftwarefactory-orchestrator:local --name {{cluster}}
    kubectl --context {{context}} --namespace {{namespace}} apply -f infra/kubernetes/base/orchestrator/deployment.yaml
    kubectl --context {{context}} --namespace {{namespace}} rollout restart deployment/orchestrator
    kubectl --context {{context}} --namespace {{namespace}} rollout status deployment/orchestrator --timeout=300s

# Build and deploy only the factory UI, preserving the other running services.
update-ui:
    #!/usr/bin/env bash
    set -euo pipefail
    export DOCKER_CONFIG=$(mktemp -d)
    trap 'rm -rf "$DOCKER_CONFIG"' EXIT
    docker build --tag opensoftwarefactory-factory-ui:local services/factory-ui
    kind load docker-image opensoftwarefactory-factory-ui:local --name {{cluster}}
    kubectl --context {{context}} --namespace {{namespace}} apply -f infra/kubernetes/base/factory-ui/deployment.yaml
    kubectl --context {{context}} --namespace {{namespace}} apply -f infra/kubernetes/base/git-host/service.yaml
    kubectl --context {{context}} --namespace {{namespace}} rollout restart deployment/factory-ui
    kubectl --context {{context}} --namespace {{namespace}} rollout status deployment/factory-ui --timeout=300s

# Wait until every deployment in the service list has rolled out.
_wait:
    #!/usr/bin/env bash
    set -euo pipefail
    for service in {{services}}; do
        kubectl --context {{context}} --namespace {{namespace}} rollout status "deployment/$service" --timeout=300s
    done
    kubectl --context {{context}} --namespace {{namespace}} rollout status daemonset/log-collector --timeout=300s

# Show each service's in-cluster and port-forward URLs, marking ready deployments.
info:
    #!/usr/bin/env bash
    set -euo pipefail
    echo ""
    trap 'printf "\033[0;31m✗ info failed\033[0m\n" >&2' ERR
    printf "\033[0;34m=== Service information ===\033[0m\n"
    # Local URLs match the port-forward recipes below.
    local_url() {
        case "$1" in
            factory-ui) echo "http://localhost:8082 (always on)" ;;
            git-host) echo "http://localhost:8082/git/ (always on)" ;;
            llm-gateway) echo "http://localhost:8081/v1 (just gateway)" ;;
            openobserve) echo "http://localhost:5080 (always on)" ;;
            temporal) echo "http://localhost:8233 (always on)" ;;
            *) echo "-" ;;
        esac
    }
    ready=""
    ports=""
    if kubectl --context {{context}} get namespace {{namespace}} >/dev/null 2>&1; then
        ready=$(kubectl --context {{context}} --namespace {{namespace}} get deployments \
            -o jsonpath='{range .items[*]}{.metadata.name}={.status.readyReplicas}{"\n"}{end}')
        ports=$(kubectl --context {{context}} --namespace {{namespace}} get services \
            -o jsonpath='{range .items[*]}{.metadata.name}={range .spec.ports[*]}{.port} {end}{"\n"}{end}')
    else
        printf "\033[0;33mCluster {{cluster}} is not reachable; all services shown as down.\033[0m\n"
    fi
    printf "\n  %-3s %-14s %-32s %s\n" "" "SERVICE" "IN-CLUSTER" "LOCAL"
    for service in {{services}}; do
        count=$(printf '%s\n' "$ready" | sed -n "s/^$service=//p")
        if [ "${count:-0}" -ge 1 ] 2>/dev/null; then mark="\033[0;32m✓\033[0m"; else mark="\033[0;31m✗\033[0m"; fi
        cluster_urls=$(printf '%s\n' "$ports" | sed -n "s/^$service=//p" | awk -v s="$service" '{for (i = 1; i <= NF; i++) printf "%s%s:%s", (i > 1 ? ", " : ""), s, $i}')
        printf "  $mark   %-14s %-32s %s\n" "$service" "${cluster_urls:--}" "$(local_url "$service")"
    done
    echo ""
    printf "\033[0;32m✓ Service information shown\033[0m\n"
    echo ""

# Show pods and services in the project namespace.
status:
    #!/usr/bin/env bash
    set -euo pipefail
    echo ""
    trap 'printf "\033[0;31m✗ status failed\033[0m\n" >&2' ERR
    printf "\033[0;34m=== Local service status ===\033[0m\n"
    if ! docker info >/dev/null 2>&1; then
        trap - ERR
        printf "\033[0;31m✗ Container runtime is unavailable; run just start\033[0m\n" >&2
        exit 1
    fi
    nodes=$(docker ps -aq --filter label=io.x-k8s.kind.cluster={{cluster}})
    if [ -z "$nodes" ]; then
        trap - ERR
        printf "\033[0;31m✗ Cluster {{cluster}} has not been created; run just cluster-create first\033[0m\n" >&2
        exit 1
    fi
    running=$(docker ps -q --filter label=io.x-k8s.kind.cluster={{cluster}})
    if [ -z "$running" ]; then
        trap - ERR
        printf "\033[0;31m✗ Cluster {{cluster}} is stopped; run just start\033[0m\n" >&2
        exit 1
    fi
    if ! kubectl --context {{context}} --request-timeout=5s get nodes >/dev/null 2>&1; then
        trap - ERR
        printf "\033[0;31m✗ Cluster {{cluster}} node is running but the Kubernetes API is unreachable; run just stop, then just start\033[0m\n" >&2
        exit 1
    fi
    if ! output=$(kubectl --context {{context}} --namespace {{namespace}} --request-timeout=5s get pods,services 2>/dev/null); then
        trap - ERR
        printf "\033[0;31m✗ Cannot read project services; run just stop, then just start\033[0m\n" >&2
        exit 1
    fi
    printf '%s\n' "$output"
    printf "\n\033[0;33mWeb UIs\033[0m\n"
    kubectl --context {{context}} --namespace {{namespace}} --request-timeout=5s get services \
        -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.metadata.annotations.opensoftwarefactory\.dev/url}{"\n"}{end}' |
        awk -v login="$(just _openobserve-login)" 'NF == 2 {
            split("factory-ui=Factory UI,openobserve=OpenObserve,temporal=Temporal", pairs, ",")
            name = $1
            for (i in pairs) { split(pairs[i], kv, "="); if (kv[1] == $1) name = kv[2] }
            line = name ": " $2
            if ($1 == "openobserve" && login != "") line = line " " login
            print "  " line
        }'
    printf "\033[0;32m✓ Status retrieved\033[0m\n"
    echo ""

# Print the OpenObserve login that just init generated, if there is one.
_openobserve-login:
    #!/usr/bin/env bash
    set -euo pipefail
    env_file=services/openobserve/config/openobserve.env
    [ -f "$env_file" ] || exit 0
    printf '%s %s\n' "$(sed -n 's/^ZO_ROOT_USER_EMAIL=//p' "$env_file")" \
        "$(sed -n 's/^ZO_ROOT_USER_PASSWORD=//p' "$env_file")"

# Open the factory UI, which KIND exposes on localhost:8082 without a port-forward.
ui:
    @just _open "the factory UI" "http://localhost:8082"

# Forward the gateway to localhost:8081 until interrupted.
gateway:
    #!/usr/bin/env bash
    set -euo pipefail
    echo ""
    trap 'printf "\033[0;31m✗ gateway failed\033[0m\n" >&2' ERR
    printf "\033[0;34m=== Forwarding gateway to localhost:8081 ===\033[0m\n"
    if ! docker info >/dev/null 2>&1 || ! docker ps -q --filter label=io.x-k8s.kind.cluster={{cluster}} | grep -q . || \
       ! kubectl --context {{context}} --namespace {{namespace}} --request-timeout=5s get service llm-gateway >/dev/null 2>&1; then
        trap - ERR
        printf "\033[0;31m✗ LLM gateway is unavailable; run just start first\033[0m\n" >&2
        exit 1
    fi
    kubectl --context {{context}} --namespace {{namespace}} port-forward service/llm-gateway 8081:8080
    printf "\033[0;32m✓ Gateway forwarding stopped\033[0m\n"
    echo ""

# Open OpenObserve, which KIND exposes on localhost:5080 without a port-forward.
observe:
    @just _open "OpenObserve" "http://localhost:5080"

# Open the Temporal UI, which KIND exposes on localhost:8233 without a port-forward.
temporal:
    @just _open "the Temporal UI" "http://localhost:8233"

# Check that a KIND-exposed web UI answers, print its URL, and open it in the browser.
_open title url:
    #!/usr/bin/env bash
    set -euo pipefail
    echo ""
    printf "\033[0;34m=== {{title}} ===\033[0m\n"
    if curl -sf -m 3 "{{url}}" >/dev/null; then
        printf "{{url}}\n"
        if ! command -v open >/dev/null || ! open "{{url}}"; then
            printf "\033[0;31m✗ {{title}} is reachable, but the browser could not open it; use the URL above\033[0m\n" >&2
            exit 1
        fi
        printf "\033[0;32m✓ {{title}} is reachable\033[0m\n"
    else
        printf "\033[0;31m✗ {{title}} is not reachable at {{url}}; run just start\033[0m\n"
        exit 1
    fi
    echo ""

# Render the central Kubernetes configuration without a cluster connection.
render:
    #!/usr/bin/env bash
    set -euo pipefail
    echo ""
    trap 'printf "\033[0;31m✗ render failed\033[0m\n" >&2' ERR
    printf "\033[0;34m=== Rendering Kubernetes configuration ===\033[0m\n"
    kubectl kustomize infra/kubernetes/overlays/local
    printf "\033[0;32m✓ Configuration rendered\033[0m\n"
    echo ""

# Check scripts, manifests, formatting, static analysis, and race tests.
check:
    #!/usr/bin/env bash
    set -euo pipefail
    echo ""
    trap 'printf "\033[0;31m✗ check failed\033[0m\n" >&2' ERR
    printf "\033[0;34m=== Checking project ===\033[0m\n"
    node --check scripts/init-llm-gateway.js
    node --check scripts/generate-kind-config.js
    node --check scripts/init-openobserve.js
    node --check scripts/init-orchestrator.js
    node --check scripts/sync-seeds.js
    node --check scripts/init-jetstream.js
    node --check scripts/verify-factory-measurements.js
    node --check scripts/verify-workflow-events.js
    node --test scripts/jetstream.test.js
    node --test services/factory-ui/measurements_ui_test.mjs
    node --test services/factory-ui/skills_ui_test.mjs
    node --test scripts/seeds.test.js
    node --check scripts/auxiliary-targets.test.js
    node --check scripts/lifecycle.test.js
    node --check scripts/migrate-unpack-inputs.js
    node --check scripts/migrate-unpack-inputs.test.js
    for script in services/factory-ui/web/*.js; do node --check "$script"; done
    node --test scripts/auxiliary-targets.test.js scripts/lifecycle.test.js scripts/migrate-systemprompt.test.js scripts/migrate-unpack-inputs.test.js scripts/openobserve.test.js scripts/log-collector.test.js services/factory-ui/agents_ui_test.mjs services/factory-ui/chat_ui_test.mjs services/factory-ui/evaluations_ui_test.mjs services/factory-ui/dom_ui_test.mjs services/factory-ui/markdown_ui_test.mjs services/factory-ui/mcp_gateway_ui_test.mjs services/factory-ui/policy_templates_ui_test.mjs services/factory-ui/registry_name_ui_test.mjs services/factory-ui/run_index_ui_test.mjs services/factory-ui/rerun_ui_test.mjs services/factory-ui/runs_timeline_ui_test.mjs services/factory-ui/teams_editor_test.mjs services/factory-ui/teams_ui_test.mjs services/factory-ui/inputs_ui_test.mjs services/factory-ui/workflows_page_test.mjs services/factory-ui/merge_batch_ui_test.mjs services/factory-ui/devpods_ui_test.mjs services/factory-ui/connectors_ui_test.mjs
    kubectl kustomize infra/kubernetes/overlays/local >/dev/null
    for module in llm-gateway blobstore mcp-gateway connectors orchestrator registry evaluation agent factory-ui; do
        test -z "$(gofmt -l "services/$module")"
        go -C "services/$module" vet ./...
        go -C "services/$module" test -race ./...
    done
    uv run --project services/tool-gateway --locked pytest -q services/tool-gateway
    uv run --no-project python3 -m unittest discover -b -s services/devpod/tests
    printf "\033[0;32m✓ All checks passed\033[0m\n"
    echo ""

# Run the template guardrails, showing details when a check fails.
ci:
    @bash scripts/ci.sh quiet

# Run the same guardrails, streaming the tools' output.
ci-verbose:
    @bash scripts/ci.sh verbose
