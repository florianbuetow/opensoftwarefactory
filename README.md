# Open Software Factory

A thin software factory on a Kubernetes in Docker (KIND) cluster. Temporal workflows chain steps, every handover is a file in a blobstore, agents run against an LLM gateway with MCP tools, and people decide at gates in one factory UI.

```text
                             ┌──────────────────┐
                             │     browser      │
                             └────────┬─────────┘
                                      │
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

## Repository Structure

```
services/
  agent/              Runs agent steps: models via the LLM gateway, tools via MCP (Go)
  blobstore/          Stores every artifact with its creator and trace ID (Go)
  connectors/         Git repositories, dev pods, and the rules for agents working in them (Go)
  devpod/             Image of a dev pod: the sandbox an agent works in (Python)
  evaluation/         Evaluation sets, measurements, prices, and judge extraction (Go)
  factory-ui/         The web UI and the proxy to the service APIs (Go; the UI is in web/)
  git-host/           Demo Git host for local repositories
  llm-gateway/        OpenAI-compatible proxy to the model providers (Go)
  mcp-gateway/        MCP passthrough routing, tool filtering, and stdio process management (Go)
  openobserve/        Configuration template for OpenObserve: logs, metrics, and traces
  orchestrator/       Runs the workflows on Temporal: steps, gates, and routes (Go)
  registry/           Agent, policy, and skill versions, each with its own registry deployment (Go)
  tool-gateway/       MCP server for the functions in tools/tools.py (Python)
infra/
  kind/               KIND cluster configuration
  kubernetes/
    base/             One directory per deployment, including Temporal, JetStream, and the log and telemetry collectors
    overlays/local/   Puts the factory in the opensoftwarefactory namespace and adds the dev pod namespace
scripts/              Node scripts that create the local and cluster configuration
justfile              The commands and the list of services
package.json          Node settings for scripts/ (ES modules, Node 24+)
```

## Prerequisites

- Colima
- Docker CLI
- KIND
- `kubectl`
- `just`
- Node.js 24 or later
- Go 1.26 or later
- `uv`
- A local LLM runtime with an OpenAI-compatible API. The default is LM Studio with the model `qwen3.5-9b-mlx` loaded; for another one, see [Using another LLM runtime](#using-another-llm-runtime)

## Commands

Everything is done through the justfile. Do not start, stop or deploy anything by hand.

```
just start            Brings the factory up from any state
just help             Lists every command and what it does
just info             Shows each service's address and whether it is up
just status           Shows the pods and services
```

## Local configuration and data

These folders are on your machine, are mounted into the cluster, and are ignored by Git. `just init` creates the configuration files.

```
services/
  llm-gateway/
    config/           config.yaml: model providers, credentials, and the model names agents use
    logs/             Logs of the LLM gateway
  openobserve/
    config/           openobserve.env: the OpenObserve login and its generated password
    data/             Logs, metrics, and traces
  blobstore/
    data/             Every artifact, agent, policy, workflow version, and MCP gateway setting
  tool-gateway/
    tools/            tools.py: the functions the tool gateway serves
scratch/
  seeds/              Optional seed data; `just sync-seeds` copies it into the cluster
```

Mounts are fixed when the cluster is created. After adding one, delete and create the cluster again.

### Using another LLM runtime

`just init` creates `services/llm-gateway/config/config.yaml` for LM Studio. For another runtime, replace its `providers:` block with one like this, then run `just deploy` if the cluster is already running:

```yaml
providers:
  - name: ollama
    base_url: http://host.lima.internal:11434/v1
    auth:
      type: none
    streaming: true
    models:
      "qwen3:8b": "qwen3:8b"
```

The gateway runs inside the cluster and reaches your machine as `host.lima.internal`, never as `localhost` or `127.0.0.1`. Leave out `model_discovery`; it works only with LM Studio. Under `models:`, list each model by the ID the runtime reports at `/v1/models`, and select it for the agents in the Factory UI.

Default ports:

```
LM Studio    1234
Ollama       11434
llama.cpp    8080
vLLM         8000
Jan          1337
```

## Contributing

We don't accept code changes. We accept natural language requests for changes that specify behaviors, functionality and features. A request may contain suggestions on how to implement them.

All code changes are performed by the repository maintainers after reviewing your feature request.

## License

PolyForm Noncommercial License 1.0.0. See [LICENSE](LICENSE).
