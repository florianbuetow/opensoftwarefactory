# Open Software Factory

A thin software factory on a single-node KIND cluster. Temporal workflows chain steps, every handover is a file in a blobstore, agents run against an LLM gateway with MCP tools, and people decide at gates in one factory UI.

```text
                             ┌──────────────────┐
                             │     browser      │
                             └────────┬─────────┘
                                      │ http://localhost:8082
                             ┌────────▼─────────┐         ┌──────────────────┐
                             │    factory-ui    ├────────▶│  Kubernetes API  │
                             └────────┬─────────┘         └──────────────────┘
                                      │
         ┌────────────────────────────┼────────────────────────────┐
┌────────▼─────────┐         ┌────────▼─────────┐         ┌────────▼─────────┐
│   orchestrator   │         │  agent-registry  │         │ policy-registry  │
└────────┬─────────┘         └────────▲─────────┘         └────────▲─────────┘
         │ workflows                  │ definitions                │ policies
┌────────▼─────────┐  polls  ┌────────┴─────────┐   MCP   ┌────────┴─────────┐        ┌──────────────────┐
│     temporal     │◀────────┤      agent       ├────────▶│   mcp-gateway    ├───────▶│ external MCP     │
└──────────────────┘         └────────┬─────────┘         └──────────────────┘        │ servers/programs │
                                      │                                               └──────────────────┘
                                      │ model calls
┌──────────────────┐  traces ┌────────▼─────────┐         ┌──────────────────┐
│   openobserve    │◀────────┤   llm-gateway    ├────────▶│ model providers  │
└──────────────────┘         └──────────────────┘         └──────────────────┘

╔════════════════════════════════════════════════════════════════════════════╗
║      blobstore: every run artifact, agent, policy, task, and template      ║
║    read and written by factory-ui, orchestrator, agent, the registries,    ║
║                   tool-gateway, and mcp-gateway                            ║
╚════════════════════════════════════════════════════════════════════════════╝
```

| Service | Role |
| --- | --- |
| `temporal` | Workflow engine. |
| `orchestrator` | Runs the workflows: calls steps, holds gates, routes to other workflows. |
| `blobstore` | Every artifact, with creator and trace ID. |
| `agent-registry`, `policy-registry` | Versioned agents and policies. |
| `agent` | Runs an agent: model calls through the LLM gateway, tools through the MCP gateway. |
| `tool-gateway` | MCP server for the tools in `services/tool-gateway/tools/tools.py`. |
| `mcp-gateway` | Routes MCP `2025-11-25` to configured HTTP or stdio servers and enforces tool allow/deny lists. |
| `llm-gateway` | OpenAI-compatible proxy to the model providers; the UI reads its model list. |
| `factory-ui` | The UI at <http://localhost:8082>. |
| `openobserve` | Logs, metrics, and traces. |
| `evaluation` | Searches completed and failed runs and stores named evaluation-set snapshots. |
| `log-collector` | Tails this namespace's container logs, redacts credential fields, buffers on disk, and forwards them to OpenObserve. |

## Repository Structure

```
services/
  agent/              Runs agent steps: models via the LLM gateway, tools via MCP (Go)
  blobstore/          Stores every artifact with its creator and trace ID (Go)
  connectors/         Git repositories, dev pods, and the rules for agents working in them (Go)
  factory-ui/         The web UI and the proxy to the service APIs (Go; the UI is in web/)
  llm-gateway/        OpenAI-compatible proxy to the model providers (Go)
  mcp-gateway/        MCP passthrough routing, tool filtering, and stdio process management (Go)
  openobserve/        Configuration template for OpenObserve (runs the upstream image)
  orchestrator/       Runs the workflows on Temporal: steps, gates, and routes (Go)
  registry/           Agent and policy versions; deployed twice, as both registries (Go)
  tool-gateway/       MCP server for the functions in tools/tools.py (Python)
infra/
  kind/               KIND cluster configuration
  kubernetes/
    base/             One directory per deployment, temporal included
    overlays/local/   Puts the factory in the opensoftwarefactory namespace and adds the dev pod namespace
scripts/              Node scripts that create the local and cluster configuration
justfile              The commands below and the list of services
package.json          Node settings for scripts/ (ES modules, Node 24+)
```

## Prerequisites

Colima, the Docker CLI, KIND, kubectl, Node.js 24+, Go 1.26+, `uv`, and `just`.

## Setup

This guide creates a local, single-node factory. It does not use a hosted
Kubernetes service: KIND runs in Colima and the factory’s local data stays in
this checkout.

### 1. Install prerequisites

Install and make available on your `PATH`:

- [Colima](https://github.com/abiosoft/colima) and the Docker CLI
- [KIND](https://kind.sigs.k8s.io/) and `kubectl`
- [just](https://github.com/casey/just)
- Node.js 24 or later, Go 1.26 or later, and [`uv`](https://docs.astral.sh/uv/)
- [LM Studio](https://lmstudio.ai/) running locally

On macOS, Colima is the expected container runtime. The project uses its
default Docker socket automatically.

### 2. Configure a local model

Run `just llm` (needs `fzf` and `jq`). It detects well-known local runtimes
(LM Studio, Ollama, llama.cpp, MLX LM, vLLM, Jan), offers to start one that is
installed but not serving, lists its models, and writes your selection to
`services/llm-gateway/config/config.yaml`. The seeded agents expect
`qwen3.5-9b-mlx`; select that model, or choose another one for the relevant
agents in the Factory UI. Run `just deploy` afterwards if the cluster is
already running.

### 3. Initialize, build, and run the factory

From the repository root, run:

```sh
just init             # generate ignored local configuration and KIND settings
just cluster-create   # create the empty single-node KIND cluster
just update           # build local images, load them, and deploy every service
just status           # confirm the pods are running
just info             # print service URLs and readiness
```

`just init` never overwrites existing configuration or credentials. It creates
the ignored `services/llm-gateway/config/config.yaml` from its template, plus
the local OpenObserve and orchestrator configuration. The first `just update`
can take several minutes because it builds all service images.

Open the factory at <http://localhost:8082>, or run `just ui`. OpenObserve is
available at <http://localhost:5080> and the Temporal UI at
<http://localhost:8233>.

### Day-to-day lifecycle

- After source or manifest changes, run `just update` to rebuild and deploy.
- Use `just stop` to stop the project cluster without deleting its data, then
  `just start` to bring it back with the already-built images.
- Use `just check` before contributing to run manifest checks plus Go and
  Python tests.
- `just cluster-delete` removes only the KIND cluster and preserves host data.
  Run `just cluster-create` followed by `just update` to recreate it.

`just reset` deletes workflows, artifacts, and run history; `just
reset-observe` deletes OpenObserve data. Do not use either if you need to keep
local factory data.

## Usage

### The UI

- **Workflows:** edit a workflow as a graph. The Input node opens Select Inputs, which starts runs from typed text, a task collection (one run per file), or a previous input, optionally wrapped in a template.
- **Runs:** every run with its status and steps.
- **Decisions:** gates waiting for Approve or Reject, with the input and step outputs they are based on, and past decisions.
- **Agents and Policies:** versioned definitions in groups (such as `course/triage`). Editing saves a new version; right-click New version starts one. An agent's model, effort, max retries, tools, and description are fields above its files. The Tools picker opens a searchable dialog of allowed tools from configured MCP services; Apply saves `service/tool` references, and Refresh tools updates the dialog's list.
- **MCP Gateway:** add, edit, and delete MCP passthrough services; test discovery and permitted tool calls.
- **Memory:** the blobstore: runs, agents, policies, task collections, and templates.
- **Cluster:** deployments, pods with their logs, services, and the node.
- **Evaluations:** filter runs by workflow, participating agent, start range, status, or end-to-end duration, then save selected runs as a named set. Sets preserve selection-time metadata and flag source runs that were later removed.

### Commands

| Command | Action |
| --- | --- |
| `just cluster-create` | Create the empty KIND cluster |
| `just start` / `just stop` | Start or stop the existing cluster and its services, keeping data |
| `just deploy` | Deploy already-built images to the running cluster |
| `just update` | Build local images, then deploy them to the running cluster |
| `just cluster-delete` | Delete the cluster; all data remains |
| `just llm` | Detect local LLM runtimes and pick gateway models with fzf |
| `just init` | Create local configuration that does not exist yet |
| `just reset` | Delete workflows, artifacts, and run history, then re-initialize |
| `just reset-observe` | Delete OpenObserve data, keeping its configuration |
| `just status` / `just info` | Show pods and services / each service's URLs and health |
| `just ui`, `just observe`, `just temporal` | Open the factory UI, OpenObserve (:5080), or Temporal (:8233) |
| `just gateway` | Forward the LLM gateway to localhost:8081 |
| `just check` | Check scripts and manifests, run the Go and Python tests |
| `just render` | Render the Kubernetes manifests |

## Configuration

Local configuration and data are host-mounted and ignored by Git:

- `services/llm-gateway/config/config.yaml`: model providers, credentials, and the model names agents use, each mapped to a provider's model.
- `mcp-gateway/<service>/` in the blobstore: `mcpconfig.json`, `allowlist.json`, and `denylist.json` for each MCP passthrough. The MCP Gateway page edits these objects. Saving through the gateway applies changes immediately; direct Blobstore edits are loaded on gateway restart.
- `services/orchestrator/config/config.yaml`: the workflows (the Workflows page edits it).
- `services/openobserve/config/openobserve.env`: the OpenObserve login (`admin@example.com`) and its generated password.
- Fluent Bit collects the namespace's container stdout/stderr into the OpenObserve `factory` log stream with Kubernetes pod and container metadata. Its delivery queue survives pod restarts on the KIND node.
- Fluent Bit's internal HTTP API is reachable only inside the cluster at `http://log-collector:2020`: `/api/v1/health` reports output health, `/api/v1/metrics/prometheus` exposes `fluentbit_output_retries_total`, `fluentbit_output_retried_records_total`, `fluentbit_output_retries_failed_total`, `fluentbit_output_errors_total`, `fluentbit_output_dropped_records_total`, and `fluentbit_output_proc_records_total`; `/api/v1/storage` reports buffered chunk counts. The health check turns unhealthy after more than five output errors or retry failures in 60 seconds. Any increase in dropped records or retry failures indicates loss; retries alone indicate delivery trouble, even though this output retries indefinitely. The configured output storage limit is 100 MB. OpenObserve's `services/openobserve/config.template` sets `ZO_COMPACT_DATA_RETENTION_DAYS=7`; this is the available retention-policy source, so report evidence expiry as unknown when that metadata is unavailable rather than deriving a date from log timestamps.
- `services/blobstore/data/`: every artifact, agent, and policy.

Host mounts are fixed when the cluster is created: after adding one, run `just cluster-delete` and `just cluster-create` from the permanent checkout.

## Investigating a run

The Runs execution timeline links each attempt and event to its input/output artifacts, recorded conversation or tool exchange, correlated logs, and distributed spans. Missing artifacts, expired search windows, partial/truncated searches, and unavailable telemetry are shown explicitly. Run UUIDs identify business runs; `distributed_trace_id` and event trace/span IDs identify OpenTelemetry traces.

Agents save an attempt checkpoint before and after each discovery, model, and tool operation. A failed pre-operation checkpoint stops execution before the external call. If execution is interrupted, the persisted running event identifies the unfinished operation; a terminal workflow can label that event interrupted. Cancellation uses a bounded final write. No checkpoint can guarantee persistence while the blobstore itself is unavailable.

The internal `telemetry-collector` receives OTLP/HTTP on port 4318 and exports spans to OpenObserve's `default` trace stream. Its persistent queue holds at most 1,000 batches; exporter failures, refused spans, and queue pressure are exposed to the UI. `just init` generates its mode-0600 credential-bearing configuration under the existing ignored OpenObserve config directory. Services receive only the internal collector endpoint. Neither collector is publicly exposed. External service internals remain outside the trace boundary.

## Development

### Add a service

1. Add the code and a Dockerfile under `services/<name>/`.
2. Add its Deployment and Service under `infra/kubernetes/base/<name>/` with a `kustomization.yaml`, and include the directory in `infra/kubernetes/base/kustomization.yaml`.
3. Add the Deployment name to the `services` list in `justfile`.
4. Run `just update`.
