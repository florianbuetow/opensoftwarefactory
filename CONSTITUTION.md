# Constitution

A stored version stays as it was saved. When you want to compare an old run with a new model or a new prompt, you start that old version again with the same input.

## A save writes the next version

You edit the coder agent and change the model from mini to gpt4o. Save does not overwrite v1. The registry stores v2 beside it. A step that says `coder@v1` still loads v1.

Re-run on the Runs page does the same thing with a whole workflow. If the run recorded v3, Re-run starts v3 with that run's original input. A run from before versions existed starts the newest version.

The description is the field you can change on an existing version. Rename changes the name on every version of that workflow. Delete removes an entry. A version that is still stored is not rewritten.

Edits are new versions.

```
┌──────────┐    ┌──────────┐
│ coder v1 ├─▶  │ coder v2 │
│ mini     │    │ gpt4o    │
└──────────┘    └──────────┘
```

Same rule for agents, policies, skills, and workflows.

- `services/registry/` stores agent prompts and settings, policies, and skill folders. A posted save is the next `v<N>`.
- `services/orchestrator/store.go` stores `workflows/<name>/v<N>/workflow.json`.
- `services/orchestrator/workflows.go` assigns the next version in `postWorkflowVersion`. `patchWorkflowDescription` edits the description of the version you have open.
- `services/factory-ui/web/agents.js`, `policies.js`, `workflows.js`, and `runs.js` are the screens. Re-run is in `runs.js`.

## A run keeps the prompt it used

During an attempt, the agent copies the system prompt, the compiled prompt, and each policy into the run folder. Open that run later and you see the text from that attempt. If the copy is missing, the run says so. The page does not fill the gap with whatever the registry holds today.

For a step named plan, attempt 1, the copies are:

- `runs/t1/_evidence/plan/attempt-1/systemprompt.source.md`
- `runs/t1/_evidence/plan/attempt-1/systemprompt.effective.md`
- `runs/t1/_evidence/plan/attempt-1/policies/security/review/v2/policy.md`

```
┌──────────────┐
│ registry     │
│ coder v1     │
└──────┬───────┘
       │
       ▼
┌──────────────┐
│ run evidence │
│ source prompt│
│ policy v2    │
└──────────────┘
```

- `services/agent/agent.go` writes those files. The paths are covered in `agent_test.go`.
- The blobs live under `runs/<trace>/_evidence/` in `services/blobstore/`.
- `services/factory-ui/web/runs.js` links to the copies on the selected model call.

## A score is a normal workflow

A judge is an agent step in a workflow you start like any other. When a workflow finishes, Intake can start the scoring workflow and pin the version. The judge writes a JSON artifact. The Judges screen maps fields in that JSON to named metrics. If a mapping fails, evaluation reads the same artifact again. The judge agent does not get another model call.

```
┌────────────┐
│ workflow   │
│ finishes   │
└─────┬──────┘
      │
      ▼
┌────────────┐
│ judge      │
│ workflow   │
└─────┬──────┘
      │
      ▼
┌────────────┐
│ graph      │
│ one metric │
└────────────┘
```

- `services/agent/output_contract.go` checks the JSON and records the extraction.
- `services/evaluation/extraction.go` turns the artifact into a metric. The request names the producer run, the target run, the artifact, the agent version, and the mapping version.
- `services/factory-ui/web/judges.js` edits the agent and the field mapping.
- `services/connectors/intake.go` starts the workflow from an event. The graph is `services/factory-ui/web/metrics.js`.

## The usage record stays, and the cost is calculated again

The gateway records the token counts a provider reported. A missing count stays missing. The cost you see is that usage multiplied by the price row and the EUR/USD rate that apply today. Change a price and evaluation writes a new cost. The usage row is unchanged. Posting the same usage twice does not add a second charge.

```
┌───────────┐ ┌───────────┐
│ usage row │ │ price row │
└─────┬─────┘ └─────┬─────┘
      │             │
      └──────┬──────┘
             │
             ▼
       ┌───────────┐
       │ cost today│
       └───────────┘
```

- `services/llm-gateway/usage.go` records provider usage.
- `services/evaluation/pricing.go`, `recalculations.go`, and `projector.go` calculate the current cost and refresh the graph data.
- `services/evaluation/measurements.go` keeps usage, execution, and judge measurements as separate records.
- `services/factory-ui/web/pricing.js` and `metrics.js` are the screens.

## One run is one trace of blobs

The orchestrator uses the trace id as the Temporal workflow id. The input, each step output, and the decision are objects under `runs/<trace>/`. A nested workflow stays on that same trace. The judge has its own trace and stores the id of the run it scored.

```
trace 7dcf3045
┌────────┐
│ input  │
└───┬────┘
    │
    ▼
┌────────┐
│ plan   │
└───┬────┘
    │
    ▼
┌────────┐
│ answer │
└────────┘
```

- `services/orchestrator/` starts the workflow and writes `runs/<trace>/input`, `runs/<trace>/<step>`, and `runs/<trace>/metadata.json`.
- `services/blobstore/` stores each object with a creator and a trace id.
- `services/factory-ui/web/runs.js` and `artifacts.js` read them back in order.

## A person is a step

A gate is a step in the graph. The workflow stops there until someone approves or rejects on the Decisions page. That decision is the step's artifact. A rejection can end the run, or go back to an earlier step when the gate has a back edge. A publish approval waits in the same list.

```
┌──────────┐
│ agent    │
└────┬─────┘
     │
     ▼
┌──────────┐
│ gate     │
└────┬─────┘
     │
     ▼
┌──────────┐
│ a person │
└────┬─────┘
     │
     ▼
┌──────────┐
│ next step│
└──────────┘
```

- `services/orchestrator/pipeline/workflow.go` defines the gate and the decision signal.
- `services/factory-ui/web/decisions.js` is the page. The canvas for a gate is in `workflows.js`.

## The browser talks only to factory-ui

The page calls `/api/...` on factory-ui. That process forwards the call. Agents run in the agent service. From the browser, factory-ui forwards writes to the blobstore only for task files, task templates, and session files. The cluster view is read-only. The model list is the only call that reaches the LLM gateway.

- The UI never calls Claude Code or any agent CLI; agents run in the agent service.
- The UI proxies the backends; no other writes to the blobstore than tasks, templates, and session files.

```
               ┌──────────┐
               │ browser  │
               └────┬─────┘
                    │
                    ▼
               ┌──────────┐
               │factory-ui│
               └────┬─────┘
                    │
          ┌─────────┼─────────┐
          │         │         │
          ▼         ▼         ▼
     ┌─────────┐┌─────────┐┌─────────┐
     │orchestr.││ registry││blobstore│
     └─────────┘└─────────┘└─────────┘
```

- `services/factory-ui/main.go` is the proxy.
- `services/factory-ui/web/` is the page.
- The other services sit behind it: `services/orchestrator/`, `services/registry/`, `services/blobstore/`, `services/agent/`, `services/evaluation/`, `services/connectors/`, `services/mcp-gateway/`.

## A tool runs only when it is named

Each MCP service has three files in the blobstore. A tool call is sent on only when the name is in `allowlist.json` and is absent from `denylist.json`. A name on the denylist is refused even if it is also on the allowlist. A name in neither file is refused. The gateway does not register servers by itself.

```
┌────────────┐
│ tool call  │
└─────┬──────┘
      │
      ▼
┌────────────┐
│ allowlist  │
└─────┬──────┘
      │
      ├──────────┐
      │          │
      ▼          ▼
┌──────────┐ ┌──────────┐
│ refused  │ │ called   │
│ if denied│ │ if named │
└──────────┘ └──────────┘
```

- `services/mcp-gateway/config.go` is the check, in `allowed`.
- The files are `mcp-gateway/<service>/mcpconfig.json`, `allowlist.json`, and `denylist.json`, written through `services/mcp-gateway/store.go`.
- `services/factory-ui/web/mcp-gateway.js` edits them.
- Built-in tools live in `services/tool-gateway/tools/`.

## One event and one binding start one run

Intake hashes the event, the binding, and the pinned workflow version. That hash is the run id. A retry of the same pair returns the run already accepted. Save the binding against a newer workflow version and the hash changes, so the new version gets its own run. Turning the binding off stops new starts. Events already received stay queued.

```
┌────────────────┐
│ event+binding  │
└───────┬────────┘
        │
        ▼
┌────────────────┐
│ one run id     │
└───────┬────────┘
        │
        ▼
┌────────────────┐
│ the same run   │
└────────────────┘
```

- `services/orchestrator/pipeline/intake.go` builds the hash.
- `services/connectors/intake.go` stores the binding and submits the event.
- `services/factory-ui/web/intake.js` and `events.js` are the screens.

## A seed fills a gap

On startup the registry copies a seed version into the blobstore only when that version is not there. An agent you have already edited stays as you saved it. Connectors do the same with repositories.

```
┌────────────┐
│ seed file  │
└─────┬──────┘
      │
      ├──────────┐
      │          │
      ▼          ▼
┌──────────┐ ┌──────────┐
│ store it │ │ skip it  │
│ if absent│ │ if stored│
└──────────┘ └──────────┘
```

- `services/registry/main.go` stores a seed version only when that version is missing. Agent seeds are read from `scratch/seeds/registry/agents/`, mounted at `/seed-root/current/registry/agents`.
- `services/connectors/seed.go` does the same for repositories under `scratch/seeds/connectors/`.
