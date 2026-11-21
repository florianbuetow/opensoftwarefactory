# Factory measurements and event intake

- **Author:** Florian
- **Status:** Draft — Overview, Goals, Technical Plan, and Alternatives approved; Detailed Implementation awaiting review
- **Date:** 2026-11-21
- **Predecessor:** [Concept brief](concepts_brief.md)
- **Review evidence:** [Goldfish report](goldfish_review_report.md)
- **Implementation baseline:** [Current plan](docs/plans/factory-measurements-and-event-intake.md)

## Overview

Give factory operators a connected view of elapsed time, input and output tokens, cache usage, calculated cost, and quality across runs, workflows, agents, and models. Include the configuration and graphs needed to use those measurements, not just a collection backend.

Compose existing capabilities: JetStream retains workflow lifecycle events; connectors deliver them to configured intake; Temporal runs ordinary workflows; ordinary judge agents inspect execution evidence and produce JSON artifacts. The judges UI maps selected output fields to named, typed measurements. Blob storage retains evidence and current canonical measurements; OpenObserve provides queryable projections and existing logs/traces. Costs use configurable effective-dated prices and an editable EUR/USD conversion table.

All agreed capabilities belong to this delivery. Implementation order follows dependencies and does not defer graphs or configuration interfaces. The later janitor and tool-charge features remain explicitly separate future work.

## The Problem

Existing execution traces and run artifacts provide investigation detail, but do not yet give a complete accounting and quality view. Agent usage decoding discards detailed cache fields, old integer fields can conflate missing values with zero, and judge output extraction is not implemented. There is no general durable workflow-event intake path for automatically evaluating completed executions.

Operators need to understand which runs consume time and money and inspect quality results alongside that evidence. Calculations must remain traceable to observed usage, applicable prices, and configured exchange rates. Unknown evidence must remain visibly unknown. Changing prices must allow historical costs to be replaced without disrupting normal factory operation.

The concept review found implementation gaps in crash recovery, writer ownership, log capture, and migration. The technical sections will close those gaps inside the agreed architecture. They are not additional product requirements or reasons to introduce another database or scheduler.

## Goals

1. Record provider-reported input/output/cache usage with source evidence and explicit missing-data states. Compute cost as soon as the required evidence is available. Never infer remote cache reuse merely from repeated prompt text.
2. Measure each run's total elapsed time and preserve the identity needed to relate root and nested executions, their agents, model requests, and generated outputs.
3. Configure vendor/model/effort/mode prices with FROM/TO intervals and cached/uncached input and output rates, including explicit zero prices. Support EUR and USD, editable conversion rates, and selectable display currency without exchange-rate freshness enforcement.
4. Recalculate historical costs in background work and replace old current values. Preserve original usage; expose eventual consistency rather than presenting partial refreshes as complete totals.
5. Publish durable workflow-only lifecycle events. Provide a consumer-based Events view with rewind, string search, autoscroll, and at most 1,000 events displayed/in browser memory.
6. Configure event-to-workflow intake. Enabled triggers automatically submit workflows through the direct JetStream-to-Temporal handoff; disabled triggers do not submit work. Recover delivery without accidental repeated business execution.
7. Configure judges as ordinary agents in ordinary workflows, with read-only factory trace inspection, separate producer traces, and explicit links to evaluated executions. Store complete numeric/textual JSON outputs as generated blob artifacts.
8. Configure output structures and JSON-to-named-typed-metric mappings in the judges UI. Handle changing fields, invalid outputs, and extraction retries without silently rerunning paid judges.
9. Deliver switchable graphs with multiple lines by applicable model, workflow, agent, and run dimensions. Display text findings and source evidence in detail views. Keep missing values distinct from zero and avoid attributing a whole-workflow value to an arbitrary participating model.
10. Retain all logs indefinitely on the host filesystem, including records currently excluded from search ingestion. Preserve evidence through deployment and recovery; report storage failures explicitly.
11. Verify complete event-to-intake-to-judge-to-graph operation, cost replacement, restart recovery, legacy-data handling, and compatibility with existing installations. Deploy locally using project justfile targets.

## Non-Goals

- Special scorer nodes, hidden terminal scorers, lasso scopes, or scorer attachments on evaluated workflow definitions.
- Duplicating node, model, tool, artifact, or configuration trace detail into the workflow event feed.
- Another scheduler, custom broker consumer-progress service, agent registry, or database architecture.
- A/B testing, automatic winner selection, or invented score/confidence thresholds.
- Guessed provider usage, cache counts, prices, exchange rates, or historical configuration values.
- Retained competing cost histories instead of replacing current calculations.
- External exchange-rate acquisition or rejecting an old configured conversion rate.
- Implementing tool charges or `just janitor` now. Both remain future TODOs; the latter will roll up/compress retained logs without making current retention depend on its existence.

## Open Questions

No additional product interview is required for these goals. The technical drafting step must resolve the review's concrete engineering contracts: recoverable projection obligations, writer ownership, bounded catch-up, evidence/deletion semantics, and compatible migration/rollback. Their findings are preserved in the linked Goldfish report and must not be represented as already implemented or tested.

## Critical User Journeys

### Observe and evaluate a completed workflow

- **Given:** a configured factory-event connector, an enabled intake binding, and an ordinary scoring workflow containing judge agents.
- **When:** the source workflow finishes.
- **Then:** its durable event appears in Events and starts the scoring workflow. The scorer receives the target identity, reads its scoped trace through read-only tools, and saves JSON under its own execution. Mapped results become graphable and link to both executions and the original artifact.

### Correct historical costs

- **Given:** retained usage and edited price or currency-conversion rows.
- **When:** the operator starts recalculation for the affected records.
- **Then:** background work replaces their current calculated costs. Existing runs continue, graphs indicate affected ranges while refreshing, and completed refreshes contain each measurement once.

### Investigate without losing evidence

- **Given:** retained workflow events, traces, artifacts, and host logs.
- **When:** the operator rewinds Events, searches, or switches graph series.
- **Then:** the viewer remains bounded to 1,000 displayed events; graph queries are bounded by the requested range; missing evidence is explicit; and selected results link to their evidence. Reading does not advance an intake consumer.

## Technical Plan (Architecture)

### Component boundaries

```text
Workflow execution (Temporal)
       |
       +-- lifecycle evidence -> durable publication -> JetStream
       |                                                |
       |                          +---------------------+------------------+
       |                          |                                        |
       |                    Events consumer                     Factory connector
       |                    search / rewind                             |
       |                                                enabled intake binding
       |                                                               |
       |                                                   Temporal acceptance
       |                                                               |
       |                                                   ordinary scoring run
       |                                                               |
       +-- scoped trace <--- read-only trace tool <--- ordinary judge agents
                                                                       |
                                                                JSON blob output
                                                                       |
                                                        judges UI field mappings
                                                                       |
LLM gateway -> usage evidence -> current measurements in blobstore <-----+
                                         |
                               evaluation service projector
                                         |
                               OpenObserve derived streams
                                         |
                              Factory UI multi-line graphs
```

The existing orchestrator owns workflow submission and lifecycle evidence. The existing connector service adds a factory-event source adapter. JetStream owns consumer progress and acknowledgement. The evaluation service owns current measurement writes, deterministic cost calculation, result extraction, and OpenObserve projection. Registry/agent services retain ordinary agent definitions and gain the judge output contract. The tool gateway gains a small read-only trace adapter. The UI gains Events, Intake, judge configuration/mapping, pricing/conversion controls, and graphs using shared components.

Do not introduce another database, dispatcher, or agent engine. OpenObserve is a query projection; canonical evidence and current calculated values remain in blobstore. This distinction allows correction of derived data without replacing the agreed architecture.

### Identity and record contracts

| Record | Identity and required content |
| --- | --- |
| Workflow event | Deterministic ID from Temporal execution and lifecycle transition; schema version; business run ID; Temporal Workflow ID and Run ID; parent execution; workflow/version; actual trace ID when known; occurrence time; outcome |
| Intake | Binding ID plus event ID; immutable source payload and selected binding/workflow versions; deterministic UUID compatible with existing run APIs; accepted Temporal execution reference |
| Usage | Distinct actual upstream-attempt ID, logical call identity, execution/step/agent/version, trace/span references, requested/resolved model and provider, effort/modes, request time, optional reported counts, raw evidence reference |
| Current cost | Usage ID; source amount/currency; selected price and conversion revisions; current converted amount; calculation generation; availability reason |
| Execution time | Temporal execution identity and parent, start/close timestamps, final duration only when known |
| Judge metric | Producer execution/artifact/mapping/metric identity; target execution; judge/version; numeric or textual value; validation/evidence status |

Use optional values and explicit statuses such as reported, derived, estimated, missing, and not applicable. A value of zero is only known zero when source evidence or explicit configuration establishes it. Legacy integer zeros are ambiguous unless retained raw responses prove presence. Do not fabricate historical trace IDs, model settings, or price matches.

Root and child workflows may share distributed traces and business run IDs. Their Temporal execution identities and parent links distinguish scope. Model-call identity includes execution, activity attempt, and turn; each actual upstream HTTP attempt receives a distinct accounting identity. Retries of measurement persistence reuse that identity, while another potentially billable provider request does not.

### Workflow event publication and intake

Publish workflow-start and terminal-outcome events only. Preserve the existing completed, failed, cancelled, and blocked vocabulary and report Temporal termination/timeout accurately when present. Record occurrence time independently of publication time.

The orchestrator writes one idempotent pending publication record per transition outside removable run folders. A publisher sends it with a stable JetStream message ID and records completion after broker acknowledgement. Temporal remains the authority for terminal execution state; an early finalization activity alone does not establish that the execution has closed. Reconciliation discovers missing records and repairs failed hooks using overlapping visibility scans plus revisiting known open executions. It must not skip delayed results at a pagination or timestamp boundary.

For intake, submit directly to Temporal using the deterministic UUID derived from binding/event identity. The accepted workflow input contains the immutable event payload and pinned configuration, or a reference to an already durably stored immutable blob. Acknowledge JetStream only after acceptance or confirmation of the matching execution. Ambiguous timeouts retry the same identity. Explicit duplicate-ID policies must reject a fresh business execution for the same intake.

The workflow's first initialization activity persists input artifacts and an atomic create-if-absent receipt before graph nodes execute. The receipt records the Temporal Run ID: the same execution can retry initialization; another execution finding that receipt exits without repeating business work. Receipts remain outside run deletion and extend protection beyond Temporal history. This is duplicate prevention, not a separate scheduler.

Disabling intake prevents further submissions while accepted executions continue. As specified in the implementation baseline, the consumer pauses and catches up on retained backlog when re-enabled; the UI states that behavior. Source workflow/outcome filters are explicit. Example scoring bindings exclude their own output workflows. Reject automatic trigger cycles in configured source/destination relationships; internal workflow routing remains ordinary workflow behavior.

Recovery health reports oldest pending work, last successful progress, current error and remaining evidence-retention horizon. A reachable Temporal server or broker alone does not establish progress. If historical evidence is already unavailable, report the gap; do not fabricate a completion. Durability assumes intact host storage, not recovery from destruction of the single host disk.

### Usage capture, pricing, and currency

The gateway preserves complete provider-reported usage in compatible responses and durable evidence, including native and synthetic streaming. Missing terminal stream usage remains unknown. It does not infer remote caching from prompt equality. The agent decoder retains detailed usage rather than only three integer totals. Provider cost extensions are retained separately from factory calculations.

Price lookup uses request-start time and half-open effective intervals `[from, to)`, with explicit open-ended rows allowed. Match configured vendor/model/effort/modes and rate categories; reject ambiguous overlaps. Preserve decimal precision and rate units; round only for presentation. Cached/uncached components must not double-count a total input count that already includes cached tokens. Explicit zero prices are valid but do not fill unknown token counts.

EUR/USD conversion uses configured table rows regardless of age. A missing row is unavailable, not parity. Retain the source amount/currency and rate revision used. The UI lets the operator edit prices/conversion rows, select display currency, and start recalculation.

Immediate and background calculations call the same deterministic evaluation-service function. A recalculation job pins its selected configuration revisions, works in bounded batches, and persists progress. An older job may not overwrite a record already calculated by a newer generation. Canonical cost writes replace the current value; there is no competing cost-history table.

### Recoverable canonical writes and projection ownership

The evaluation service is the sole writer of canonical calculated costs, extracted metric records, and their derived streams. Gateway/orchestrator writers supply evidence; background workflows request calculations rather than directly changing cost objects.

Before a canonical change, persist an operation record containing its measurement identity, expected revision, replacement content, and affected hour. Apply the canonical replacement, then complete the operation only after its projection obligation is durable. Recovery scans unfinished operations and compares canonical/projection revisions. This closes the gap between overwriting a value and marking its hour dirty. Canonical writes and operation-state writes use revision checks; completing generation N cannot clear dirty work from N+1.

Use the existing single-instance deployment boundary for the evaluation writer, with a Recreate deployment strategy and a process-lifetime exclusive lock on the existing VM-native persistent filesystem. A second instance cannot begin writing while the first holds the lock. Do not put this lock on a filesystem lacking the required locking semantics. Drain outstanding external writes before ownership release; after an unclean restart, treat interrupted OpenObserve operations as ambiguous and rebuild the affected hour. This chooses a complete single-host ownership model rather than introducing distributed leases or a coordinator.

New measurement evidence continues to persist while projection work runs. Coalesce dirty hours, stream records in bounded batches, and alternate current/historical work so a hot hour cannot starve older work. Use configurable explicit operational bounds for batch size, outstanding ingestion requests, and worker concurrency; these are resource controls, not invented measurement values. Report backlog/freshness rather than claiming zero impact on finite shared resources.

### OpenObserve replacement and extraction

The selected pinned OpenObserve implementation accepts bulk `update` as ingestion, not per-record overwrite. Use dedicated derived streams, isolated from raw logs/traces, and replace complete aligned hourly ranges:

1. Persist a manifest of one current canonical record per identity and its revision. Mark the range refreshing; drain its projection writes.
2. Request aligned time-range deletion and persist the returned job ID. Wait for successful asynchronous completion before ingesting replacements.
3. Reingest every manifest record with its original occurrence timestamp and inspect each ingestion result. Configure historical ingestion to cover retained records; do not silently rewrite their timestamps.
4. Verify visible record identities/counts and totals. A crash, uncertain response, or partial ingestion restarts deletion/replay rather than blindly repeating append operations.
5. Publish the verified generation. Concurrent canonical arrivals remain dirty for a subsequent coalesced pass. Graphs distinguish verified but stale values from incomplete refreshes; incomplete partitions are not shown as complete totals.

The same projector handles judge extraction. Extraction first writes canonical typed records by stable identity; retries of identical content do not create another metric. If indexing fails, retry extraction/projection without paying for another judge run. Tests must exercise delayed/buffered ingestion during deletion as well as restarts; expected behavior is one current query contribution per identity.

### Judges and the read-only evidence contract

The judges tab edits ordinary agent references, tools/MCP, output structure, and typed field mappings. Version those definitions through existing registry/configuration patterns. The backend requests structured output where supported and always validates the actual artifact. Store invalid original output with its error rather than inventing JSON or silently rerunning the judge.

Scoring workflows run in a fresh trace and carry links to their targets. A shared backend evidence query powers both UI and tool access. It accepts target execution identity, trace identity, bounded pagination and time range; it filters nested subtrees and excludes siblings. The MCP adapter exposes read operations only and does not pass observability administrator credentials to agents.

Return execution outcome, query pagination/truncation, and evidence availability separately. A fully paginated query does not prove every span was exported. Missing/dropped/disabled tracing remains unknown or unavailable. Ordinary workflow retry logic can wait for delayed evidence, but cannot certify evidence that does not exist.

### Query and UI behavior

The Events view uses its own consumer. It initially displays up to the last 1,000 events, follows and autoscrolls live arrivals, pauses scrolling during inspection, and supports rewind and string filtering over retained history using bounded batches. Report the searched range and result cap. Moving between replay and live mode uses a stream position so no transition gap is hidden.

Intake shows connector/binding configuration, filters, destination, enabled state, and accepted runs. Selected detail distinguishes failed submission from accepted execution awaiting artifact initialization. Graphs provide time/metric/series selection and multiple lines, retaining filters while refreshing. Numeric metrics are plotted; text findings and source artifacts are inspected in details. Do not add A/B machinery or automatic winner classification.

Query OpenObserve over selected time ranges rather than scanning every blob/run for each plot. Carry completeness and refresh state alongside results. Model-call counts/costs have exact agent/model attribution. Whole-workflow duration and quality do not implicitly belong to each participating model; unsupported groupings are explicit. Parent cost rollups count unique upstream attempts once.

### Retention, deletion, and operational safety

Write redacted raw logs to host files before search-only filtering, including long lines, collector self-logs, and access logs currently excluded from indexing. Avoid collector feedback loops by separating archive ingestion from its own diagnostic output route. Neither container rotation nor the current capped search-output queue may delete the only retained copy. Log archive files have no expiry; `just janitor` compression remains a future task. Full storage produces visible write failure/backpressure, not a claim that finite disk is unlimited.

Keep usage/current measurements, receipts, publication obligations, and projector manifests in namespaces outside ordinary removable `runs/` artifacts. Existing run deletion removes the operational run but does not erase accounting or deduplication records. Keep the required evidence snapshot with the measurement, and show the original run link as removed. No new measurement-purge operation is introduced in this delivery.

For existing clusters, stage the host archival path through an available existing host mount before altering cluster topology. A migration requiring new KIND mounts must preserve and verify Temporal, blobstore, OpenObserve, and node-log data before an explicit cluster replacement procedure; ordinary deployment must never implicitly recreate the cluster. Keep a recoverable backup until post-migration validation passes.

Version new Temporal initialization/lifecycle command paths so in-flight legacy histories replay unchanged. Existing manual inputs remain valid; event-intake inputs use the new path. Retain compatible workers for already accepted executions during rollout or rollback. Do not restore expiring log configuration during code rollback. Stop incompatible writers and keep unfinished projection ranges visibly refreshing until a compatible worker recovers them.

### Review-finding disposition

| Review concern | Design treatment |
| --- | --- |
| Canonical-write/dirty-marker crash gap | Durable operation intent before replacement, revision reconciliation, generation-aware completion |
| Stale writers | One evaluation owner, exclusive lifetime lock, Recreate rollout, revision checks |
| Rebuild amplification | Coalesced hourly work, bounded streaming batches and fair scheduling |
| Catch-up and cycles | Existing consumer/worker limits and explicit source/destination cycle rejection |
| Recovery horizon | Progress/oldest-pending/retention-risk health and explicit unavailable evidence |
| Trace completeness | Shared execution-scoped query contract; pagination is not source certification |
| Collector omissions | Host archival before search filters, with oversized/self-log tests |
| Run deletion | Accounting and receipts retained separately from removable run artifacts |
| Temporal compatibility | Versioned command paths and retained compatible workers |
| Host-mount migration | Preserve/verify evidence before any explicit cluster replacement |
| Legacy zeros | Ambiguous unless raw presence is established |
| Rollback data safety | Retention independent of application rollback; preserve projector state |

### Technical references

- [Selected replacement procedure and pinned sources](docs/plans/factory-measurements-and-event-intake.md#openobserve-replacement-procedure-selected)
- [Factory UI guide](docs/DESIGN.md)
- [Goldfish findings and repository evidence](goldfish_review_report.md)

This section defines proposed engineering contracts for review. It does not claim the code, migrations, resource bounds, or runtime acceptance tests have been implemented or validated.

## Alternatives Considered

These dispositions come from the concept brief and discussion. Engineering choices resolved in the approved Technical Plan are identified separately from author decisions.

### Special scorer nodes and workflow attachments

- **Benefit:** a scorer could appear directly on the evaluated workflow canvas.
- **Cost:** terminal barriers, node-range selection, trigger rules, and scorer exclusion would introduce a separate execution model.
- **Disposition:** rejected by author in favor of ordinary scoring workflows receiving workflow events through intake. Existing workflow composition already supplies branching, parallelism, tools, and execution records.

### A separate intake dispatcher or custom consumer-progress service

- **Benefit:** an application-owned queue could expose intermediate scheduling states.
- **Cost:** it would duplicate broker progress and Temporal scheduling while introducing another durable handoff to recover.
- **Disposition:** rejected by author in favor of direct JetStream-to-Temporal acceptance. The receipt serves only to prevent repeated business execution; it does not schedule work.

### Another database for mutable measurements

- **Benefit:** a database supporting row updates would simplify current-cost replacement.
- **Cost:** it changes the agreed storage architecture and introduces another service or persistence integration.
- **Disposition:** rejected by author as an unsolicited redesign. Retain blobstore and OpenObserve with the selected derived-range replacement procedure. Its replay workload and temporary query freshness limitations remain explicit.

### Updating individual OpenObserve records through bulk `update`

- **Benefit:** apparent compatibility with a familiar row-update operation.
- **Cost:** the pinned implementation appends the supplied record rather than replacing the existing one, so repeated identifiers do not prevent duplicate accounting.
- **Disposition:** technically unsuitable, based on the pinned-source investigation linked in the Technical Plan. Use isolated derived streams and aligned deletion/reingestion. This limitation is not attributed to an upstream maintainer statement.

### Retaining multiple historical calculated costs

- **Benefit:** old calculations could be queried without reconstruction.
- **Cost:** it adds selection/version semantics to every graph and does not satisfy replacement of old calculated data.
- **Disposition:** rejected by author: recalculation must overwrite current costs. Retain original usage and the configuration references used by the current calculation, not competing cost histories.

### Guessing missing usage, caching, or attribution

- **Benefit:** totals would appear complete even when providers omit evidence.
- **Cost:** the figures would conflate observations with assumptions. Matching prompt text does not establish remote cache reuse; a multi-model workflow does not identify one model responsible for its whole-run duration.
- **Disposition:** rejected by author: absent evidence must be explicit. Preserve separately labelled estimates already available, but do not use them as authoritative billing measurements.

### Per-scorer database tables

- **Benefit:** each scorer could have a custom physical schema.
- **Cost:** changing output fields would require database-schema changes and couple agent configuration to storage layout.
- **Disposition:** the author accepted JSON-field-to-named-typed-metric mappings instead. Configure them in the judges UI and use the common result record shape.

### External exchange-rate acquisition and freshness enforcement

- **Benefit:** rates could automatically follow a market source.
- **Cost:** another external dependency and stale-rate failure mode are unnecessary for the requested conversion behavior.
- **Disposition:** rejected by author in favor of an editable conversion table whose configured EUR/USD rates remain usable regardless of age. Missing rates remain explicit.

### A/B analysis and automatic winner classification

- **Benefit:** a specialized comparison product could present conclusions rather than observations.
- **Cost:** it requires scoring-comparability rules and statistical thresholds beyond the requested graph behavior.
- **Disposition:** rejected by author: provide selectable multi-line plots by applicable run, model, agent, and workflow dimensions. Do not infer winner or incubator thresholds.

### Additional event coverage duplicating traces

- **Benefit:** consumers could subscribe to individual model, tool, node, or artifact operations.
- **Cost:** it duplicates existing execution detail and expands the event contract beyond the automation target.
- **Disposition:** rejected by author: workflow events only. Detailed inspection continues through traces and artifacts.

### Reconciliation alone versus a durable projection obligation

- **Benefit of reconciliation alone:** fewer writes in the canonical mutation path.
- **Cost:** unnoticed divergence depends on later scans and can remain stale for longer.
- **Approved engineering choice:** persist mutation intent before the canonical change and reconcile unfinished/revision-mismatched operations. The two mechanisms complement each other; neither creates a new coordination service. This choice was resolved in the approved Technical Plan, not asserted as an earlier product requirement.

### Distributed writer fencing versus single-instance ownership

- **Benefit of distributed fencing:** multiple instances could coordinate ownership during failover.
- **Cost:** it adds a distributed ownership protocol to a single-host factory.
- **Approved engineering choice:** one evaluation writer, Recreate rollout, a process-lifetime exclusive filesystem lock, and revision checks. This deliberately does not promise multi-host availability. The Technical Plan requires appropriate filesystem semantics and recovery of ambiguous external writes.

### Additional repair and event-navigation controls

- **Benefit:** dedicated actions could streamline extraction repair or continuation beyond capped search results.
- **Cost:** they expand the interface before the established detail/action patterns have been used.
- **Disposition:** advisory suggestions, not requirements. Use existing detail views to expose failures, freshness, and caps; extra controls are not required for this delivery. Retrying extraction must remain distinguishable from rerunning a paid judge.

## Detailed Implementation Plan

This is one delivery. The numbered work packages establish dependencies; none is an optional release. Paths are relative to the repository root. Each named new file is to be created; each named existing file is to be modified as described. Do not create a second copy of an existing component. No production file is deleted by this plan.

### Backend contracts used by the file changes

Expose operations through the existing service boundaries and UI proxies. Mutation requests carry a schema version and expected revision where replacing configuration/current state; conflicts return an explicit conflict response. All list/query operations have bounded pagination. Missing values remain nullable with a reason, not numeric defaults.

| Owning service | Contract |
| --- | --- |
| Blobstore | Extend existing object PUT with conditional creation and revision checks. The object revision/contents, durable intent records, and successful write acknowledgement must agree after restart. Keep unconditional legacy PUT compatible. |
| Orchestrator | Internal event-intake submission accepts binding/event IDs, pinned workflow/version and immutable input. Return the existing matching execution for repeat submission; reject an identity/content mismatch. Existing manual start/status/delete endpoints remain usable. |
| Orchestrator | Internal workflow-event reconciliation and publication report progress, pending age and evidence gaps; they are not a new user-facing event taxonomy. |
| Connectors | Binding CRUD at `/api/intake/bindings`, per-binding enabled state and status; source adapter reads JetStream and calls orchestrator submission. Event-viewer sessions expose bounded fetch/search/rewind/close operations independently of intake consumers. |
| Evaluation | Idempotent usage/execution/result ingestion keyed by stable record identity; price/conversion configuration; calculation and extraction requests; recalculation start/status; bounded series query and measurement detail. The existing evaluation-set API continues to work. |
| Evaluation | Execution-scoped read-only evidence endpoint returns spans/artifact references, pagination, query completeness and source-evidence availability separately. UI and MCP call the same implementation. |
| Registry/agent | Optional versioned output contract and metric mapping metadata on ordinary agent configuration. Existing agents without that contract retain their current behavior. The judge editor owns these settings. |

Use existing `runs/` folders for working artifacts, but durable accounting records, intake receipts, publication obligations, mutation intents and projection manifests live under separate named prefixes. The evaluation service is their writer for calculations/results; the orchestrator owns lifecycle and intake records. Root metadata includes both business and distributed trace identities. Judge output remains a generated blob artifact; its canonical metric record points back to it.

No binary floating-point arithmetic is used for billed amounts. Parse rate and conversion decimal strings with standard-library exact arithmetic, retain source units/currency, sum before display rounding, and distinguish exact stored values from chart rendering. Validate configuration explicitly; do not seed fictitious prices, token counts, or exchange rates. Operational capacity settings are explicit configuration with documented units, not substitutes for missing observations.

### 1. Durable artifact primitives and retained namespaces

Implement these before enabling automatic intake or mutable measurements.

| File | Action and rationale |
| --- | --- |
| `services/blobstore/main.go` | Modify object handling to expose conditional creation/revision validation and durable acknowledgements; preserve existing routes, metadata and trace indexes. |
| `services/blobstore/conditional.go` | Create focused conditional-write helpers: per-object serialization, expected-revision checking, atomic publication and restart recovery. This supports receipts and mutation obligations without a database. |
| `services/blobstore/conditional_test.go` | Create concurrent create-if-absent, stale revision, interrupted write and recovery tests. Test object/metadata consistency. |
| `services/blobstore/main_test.go` | Extend legacy PUT/list/read regression coverage so new conditions do not change existing clients. |
| `services/orchestrator/pipeline/blob.go` | Extend the existing client for conditional receipts, mutation intents and durable record reads. |
| `services/orchestrator/server.go` | Keep accounting/receipt namespaces outside existing run deletion and return removed-run links without deleting retained measurements. |
| `services/orchestrator/server_test.go` | Test that deleting a run preserves durable receipts and independent accounting evidence. |

### 2. JetStream deployment and host persistence

JetStream is the only new service. Existing services retain their roles. Use a pinned image and explicit stream/consumer configuration; generated credentials stay outside tracked files. Keep JetStream storage off container layers. Stream history is retained independently of consumer acknowledgements.

| File | Action and rationale |
| --- | --- |
| `infra/kubernetes/base/jetstream/kustomization.yaml` | Create the JetStream resource list. |
| `infra/kubernetes/base/jetstream/deployment.yaml` | Create the single-host deployment with persistent storage, health checks, internal ports and resource settings. |
| `infra/kubernetes/base/jetstream/service.yaml` | Create the internal broker service; do not expose administration to the browser. |
| `services/jetstream/config.template` | Create the server configuration template, credential roles and durable storage location. |
| `scripts/init-jetstream.js` | Create idempotent configuration/credential initialization preserving existing data and operator settings. |
| `scripts/jetstream.test.js` | Create tests for generated configuration, stream retention, permissions and mount wiring. |
| `infra/kubernetes/base/kustomization.yaml` | Add JetStream to the base resource composition. |
| `scripts/generate-kind-config.js` | Extend existing mounts for event data and retained host logs. Preserve the VM-native lock/storage path and existing application mounts. |
| `.gitignore` | Ignore generated credentials, event data, lock files, host log archives and temporary migration exports; keep templates tracked. |
| `justfile` | Add initialization, image/deployment inclusion, status/info checks and the test commands below. Do not implement `just janitor` yet. |
| `scripts/lifecycle.test.js` | Extend service lifecycle tests, ensuring update/deploy does not recreate a cluster or discard persisted data. |
| `scripts/auxiliary-targets.test.js` | Extend status/info/render/check target coverage for the new service and verification commands. |

### 3. Workflow events and direct intake submission

Use the same pinned NATS Go client in orchestrator and connectors. Resolve and lock its version in their module files when implementing; do not introduce a wrapper service around it. The orchestrator uses it for publication; connectors use it for intake and viewer consumers.

| File | Action and rationale |
| --- | --- |
| `services/orchestrator/main.go` | Wire broker publication/recovery, intake submission and compatible workflow registration into startup and shutdown; set explicit duplicate-ID policies. |
| `services/orchestrator/go.mod`, `services/orchestrator/go.sum` | Add and lock the NATS client dependency needed for the agreed JetStream integration. |
| `services/orchestrator/config.go` | Add validated broker, publication/reconciliation and retention-horizon configuration. |
| `services/orchestrator/config.template` | Document explicit operational settings without fabricated business data. |
| `services/orchestrator/events.go` | Create versioned workflow-event records, stable IDs, pending publication and acknowledgement handling. Terminal records derive from closed execution state. |
| `services/orchestrator/events_test.go` | Create duplicate publish, lost acknowledgement, failure/cancel/block/terminate and delayed-publication tests. |
| `services/orchestrator/event_reconcile.go` | Create overlapping Temporal scans plus known-open execution tracking and recovery progress health. Persist pending work outside run cleanup. |
| `services/orchestrator/event_reconcile_test.go` | Create tests for delayed visibility, page boundaries, exhausted finalization retries and unavailable history. |
| `services/orchestrator/intake.go` | Create deterministic UUID submission, immutable accepted input, repeat matching and content-conflict handling. |
| `services/orchestrator/intake_test.go` | Create tests for concurrent duplicate submission, ambiguous acceptance, disabled submission boundaries and stable run API addressing. |
| `services/orchestrator/metadata.go` | Persist execution/version/parent/trace identities and Temporal start/close timestamps for measurements. |
| `services/orchestrator/pipeline/workflow.go` | Add versioned event-intake initialization before graph execution and compatible lifecycle paths. Preserve replay of existing histories. |
| `services/orchestrator/pipeline/activities.go` | Add initialization/receipt activities and lifecycle persistence calls; do not use activity execution time as run completion time. |
| `services/orchestrator/pipeline/team.go` | Carry child/team execution identity and parent links through nested invocation; retain ordinary team behavior. |
| `services/orchestrator/pipeline/intake_test.go` | Create initialization retry, receipt replay, fresh scoring trace and no-repeated-business-work tests. |
| `services/orchestrator/pipeline/workflow_test.go` | Extend top-level/nested lifecycle and old/manual input tests. |
| `services/orchestrator/pipeline/team_test.go` | Extend shared-trace/distinct-execution identity tests. |
| `services/orchestrator/pipeline/replay_test.go` | Create replay tests using sanitized pre-change and new intake histories. |
| `services/orchestrator/pipeline/testdata/legacy-history.json`, `services/orchestrator/pipeline/testdata/intake-history.json` | Create reproducible sanitized history fixtures; include nested execution and cancellation behavior. |
| `infra/kubernetes/base/orchestrator/deployment.yaml` | Wire broker/configuration and recovery-state access while retaining the existing Temporal connection. |

### 4. Factory connector, bindings and viewer consumers

The consumer acknowledgement transfers responsibility to Temporal. No separate pending-intake scheduler is added. Filter nonmatching workflow events without starting runs; preserve independent progress for each binding and viewer.

| File | Action and rationale |
| --- | --- |
| `services/connectors/main.go` | Wire factory event adapters and binding/viewer routes into the existing service without changing repository/devpod behavior. |
| `services/connectors/go.mod`, `services/connectors/go.sum` | Add/lock the same NATS client used by orchestrator. |
| `services/connectors/intake.go` | Create binding CRUD, pinned revisions, explicit filters, enabled state, source/destination cycle validation and status. |
| `services/connectors/intake_test.go` | Create binding persistence, revision conflict, fan-out, pause/catch-up and cycle tests. |
| `services/connectors/events.go` | Create durable consumer delivery and direct orchestrator submission; acknowledge only confirmed acceptance. |
| `services/connectors/events_test.go` | Create timeout/redelivery/replay tests, including independent bindings and no duplicate business execution. |
| `services/connectors/event_viewer.go` | Create bounded viewer session fetch, rewind, filtered search and cleanup; the server holds broker credentials. |
| `services/connectors/event_viewer_test.go` | Create capped-memory, search range/cursor, replay-to-live and viewer/intake independence tests. |
| `services/connectors/blob.go` | Extend existing persistence helpers for versioned binding documents. |
| `infra/kubernetes/base/connectors/deployment.yaml` | Add broker/orchestrator connection settings and internal credential mounts. |

### 5. Provider evidence and ordinary judge output contracts

Record measurements once per actual upstream attempt. Sending the same measurement twice must not create another charge. A second real provider request is another attempt even if it uses the same prompt. Provider omissions and interrupted responses stay explicit.

| File | Action and rationale |
| --- | --- |
| `services/llm-gateway/gateway.go` | Integrate request/attempt identity and response observation while preserving upstream status/body/streaming behavior. |
| `services/llm-gateway/usage.go` | Create optional usage normalization, raw-evidence persistence and native SSE observation; retain provider cost extensions separately. |
| `services/llm-gateway/usage_test.go` | Create cached/uncached, missing/zero, native/synthetic stream, partial-response and duplicate-persistence tests. |
| `services/llm-gateway/gateway_test.go` | Extend outward OpenAI-compatible response and no-added-latency-buffering regressions. |
| `services/llm-gateway/main.go`, `services/llm-gateway/config.go` | Wire blob/evaluation clients and durable pending measurement delivery. |
| `services/llm-gateway/config.template` | Document measurement destinations and operational buffering settings. |
| `services/llm-gateway/config_test.go` | Validate optional configuration and failure reporting. |
| `services/agent/llm.go` | Preserve detailed optional usage and output-format requests; do not decode missing counts into zero. |
| `services/agent/agent.go` | Propagate logical call/attempt context, preserve measurement references and read the optional output contract. |
| `services/agent/chat.go`, `services/agent/team.go` | Carry the same usage/output semantics through interactive and team execution paths. |
| `services/agent/output_contract.go` | Create JSON validation and mapping-contract interpretation for ordinary configured judges. Invalid output remains a stored artifact. |
| `services/agent/output_contract_test.go` | Create valid/invalid JSON, missing field, provider capability and no-silent-rerun tests. |
| `services/agent/measurements_test.go`, `services/agent/gateway_integration_test.go` | Extend evidence round-trip tests across the gateway/agent boundary. |
| `services/agent/chat_test.go`, `services/agent/team_test.go` | Extend non-pipeline path coverage without changing ordinary chat/team behavior. |
| `services/registry/main.go` | Invoke optional output/mapping validation when saving agent versions. |
| `services/registry/judge_contract.go` | Create validation for declared JSON structure, typed field mappings and version compatibility. |
| `services/registry/judge_contract_test.go`, `services/registry/main_test.go` | Verify invalid settings are rejected while existing agent definitions remain accepted. |
| `infra/kubernetes/base/llm-gateway/deployment.yaml`, `infra/kubernetes/base/agent/deployment.yaml` | Wire evidence/measurement endpoints without exposing their write credentials to agents. |

### 6. Measurements, deterministic calculations and OpenObserve projection

Implement inside the existing evaluation service. Canonical state stays in blobstore; query projections stay in OpenObserve. Short bounded work and fair scheduling keep historical jobs from monopolizing shared resources.

| File | Action and rationale |
| --- | --- |
| `services/evaluation/main.go` | Register new APIs, clients, writer lock, recovery and shutdown lifecycle while preserving evaluation sets. |
| `services/evaluation/config.go` | Create explicit configuration for batch bounds, query limits, upstream endpoints and persistent lock path. |
| `services/evaluation/measurements.go` | Create schema/versioned typed records, attribution, nullable evidence and stable identities. |
| `services/evaluation/measurements_test.go` | Create missing/zero/legacy ambiguity, attribution and duplicate-ingestion tests. |
| `services/evaluation/store.go` | Create conditional canonical writes, mutation intents, pending projection records and generation reconciliation using blobstore. |
| `services/evaluation/store_test.go` | Inject failure before/after intent, canonical write and dirty-state persistence. Ensure no lost refresh obligation. |
| `services/evaluation/ownership.go` | Create process-lifetime exclusive ownership and graceful drain on the VM-native lock path. |
| `services/evaluation/ownership_test.go` | Test second-process exclusion and restart recovery; never substitute an in-memory mutex for ownership. |
| `services/evaluation/pricing.go`, `services/evaluation/pricing_test.go` | Create effective-date matching, explicit category rates, exact monetary arithmetic and zero-price/missing-rate tests. |
| `services/evaluation/currency.go`, `services/evaluation/currency_test.go` | Create EUR/USD table validation/conversion and tests accepting old configured rates while rejecting missing/invalid rates. |
| `services/evaluation/recalculation.go`, `services/evaluation/recalculation_test.go` | Create bounded calculation batches, pinned configuration generations, checkpoints and stale-job exclusion. |
| `services/orchestrator/pipeline/recalculation.go`, `services/orchestrator/pipeline/recalculation_test.go` | Create an ordinary Temporal background workflow using evaluation activities, with resumable progress and fair bounded execution. No separate scheduler. |
| `services/evaluation/projector.go`, `services/evaluation/projector_test.go` | Create serialized hour manifests, dirty coalescing, delete/replay state, verified publication and restart tests. |
| `services/evaluation/openobserve.go`, `services/evaluation/openobserve_test.go` | Create pinned API client for hourly delete/status, bounded ingest/search and explicit historical-ingestion errors; test per-item failures and ambiguous responses. |
| `services/evaluation/extraction.go`, `services/evaluation/extraction_test.go` | Create idempotent JSON-to-typed-record extraction tied to judge/mapping/artifact versions; retry without rerunning agents. |
| `services/evaluation/series.go`, `services/evaluation/series_test.go` | Create bounded graph queries, applicable dimensions, exact cost aggregation, refresh/coverage status and no duplicate nested totals. |
| `services/evaluation/backfill.go`, `services/evaluation/backfill_test.go` | Create evidence-only import of existing runs and usage; classify legacy ambiguous zeros, preserve original times and resume safely. |
| `services/evaluation/main_test.go` | Add route, revision/error and existing evaluation-set compatibility tests. |
| `infra/kubernetes/base/evaluation/deployment.yaml` | Set Recreate ownership, writable persistent lock location, observability credential/configuration access and explicit resource settings. |
| `services/evaluation/Dockerfile` | Ensure required runtime paths and packaged configuration fit the existing non-root image; do not add another database runtime. |

### 7. Shared trace evidence, tool permissions and full host logs

Move the existing observability query responsibility into evaluation for reuse; factory-ui retains a compatibility proxy for existing routes. This is a responsibility move between existing services, not another service.

| File | Action and rationale |
| --- | --- |
| `services/evaluation/evidence.go`, `services/evaluation/evidence_test.go` | Create the shared execution-scoped evidence endpoint from existing query logic, including pagination, sibling exclusion and source/query completeness distinctions. |
| `services/factory-ui/observability.go`, `services/factory-ui/observability_test.go` | Delegate shared evidence reads while preserving current response contracts and collector-health presentation; remove implicit seven-day treatment of indefinitely archived logs. |
| `services/tool-gateway/factory.py` | Add a read-only HTTP client for evidence, with validated execution/trace identifiers and bounded responses. |
| `services/tool-gateway/tools/tools.py` | Expose only trace/evidence read functions; no mutation/admin or unrestricted query execution. |
| `services/tool-gateway/tests/test_tools.py`, `services/tool-gateway/tests/test_server.py` | Test schema/registration, read-only permissions, pagination and missing evidence. |
| `services/connectors/mcp.go` | Register the factory evidence adapter using the existing service-registration pattern, preserving operator allowlist configuration. |
| `services/connectors/mcp_test.go` | Create registration and permission-preservation tests. |
| `infra/kubernetes/base/tool-gateway/deployment.yaml` | Add only the internal evidence endpoint; no OpenObserve administrator secret in agent-facing tools. |
| `infra/kubernetes/base/log-collector/config.yaml` | Add durable redacted archival before search exclusions; preserve oversized/self/access records without recursive self-ingestion. |
| `infra/kubernetes/base/log-collector/daemonset.yaml` | Mount the persistent host archive and collector state; remove reliance on container-only paths. |
| `scripts/init-openobserve.js` | Generate archive/search routing and resilient buffering configuration; preserve credentials and existing operator data. |
| `services/openobserve/config.template` | Express retention and historical-ingestion policy explicitly; do not assume absent/zero values mean indefinite in the pinned server. Raw archive retention remains independent. |
| `infra/kubernetes/base/openobserve/deployment.yaml`, `infra/kubernetes/base/telemetry-collector/deployment.yaml` | Preserve durable observability/buffer mounts and expose meaningful failure health. |
| `scripts/log-collector.test.js`, `scripts/openobserve.test.js` | Test excluded/long/self/access logs, collector outage, saturation reporting, historical ingestion and retention preservation. |

### 8. Complete UI and working scoring example

Keep all new controls consistent with `docs/DESIGN.md`. The Evaluations area contains judges configuration and graph views; do not create a parallel agent registry. Pricing and conversion are measurement settings. Events and Intake are distinct navigable views. Use browser-native SVG/canvas for the requested line plots rather than adding a chart dependency.

| File | Action and rationale |
| --- | --- |
| `services/factory-ui/main.go`, `services/factory-ui/main_test.go` | Add/verify narrow proxy routes for Events, Intake, evidence and evaluation APIs; keep internal credentials server-side. |
| `services/factory-ui/web/app.js` | Register navigation/views and retain selection/filter state. |
| `services/factory-ui/web/events.js`, `services/factory-ui/events_ui_test.mjs` | Create the latest-1,000 viewer, autoscroll/pause/search/rewind/live modes and bounded-buffer tests. |
| `services/factory-ui/web/intake.js`, `services/factory-ui/intake_ui_test.mjs` | Create binding/source/destination/filter/enabled configuration and selected delivery detail, including explicit catch-up semantics. |
| `services/factory-ui/web/judges.js`, `services/factory-ui/judges_ui_test.mjs` | Create ordinary-agent selection, tools/output structure, field mapping and artifact validation detail in the responsible judges tab. |
| `services/factory-ui/web/pricing.js`, `services/factory-ui/pricing_ui_test.mjs` | Create effective-rate editing, currency table/display settings and recalculation controls/status. |
| `services/factory-ui/web/metrics.js`, `services/factory-ui/metrics_ui_test.mjs` | Create measurement/time/group/series selection, multi-line plots, text-result details and missing/refresh states. |
| `services/factory-ui/web/evaluations.js`, `services/factory-ui/evaluations_ui_test.mjs` | Integrate judges/settings/graphs with existing evaluation sets and preserve current workflows. |
| `services/factory-ui/web/agents.js`, `services/factory-ui/agents_ui_test.mjs` | Preserve new agent output-contract fields during ordinary agent edit/save; no duplicate mapping editor. |
| `services/factory-ui/web/runs.js`, `services/factory-ui/runs_timeline_ui_test.mjs` | Link measurement evidence and scoring/target runs in selected detail without adding duplicated telemetry summaries. |
| `services/factory-ui/web/inputs.js`, `services/factory-ui/inputs_ui_test.mjs` | Display event-origin intake artifacts and stable accepted-run links. |
| `services/factory-ui/web/style.css` | Add shared layout/state/plot styles, narrow-width behavior and Safari control compatibility. |
| `services/factory-ui/dom_ui_test.mjs` | Extend page registration and DOM behavior coverage. |
| `infra/kubernetes/base/factory-ui/deployment.yaml` | Wire new internal endpoint configuration through the existing UI service. |
| `examples/quality-scoring/README.md` | Create reproducible setup/run instructions for the complete event-to-graph example. |
| `examples/quality-scoring/agent.json`, `examples/quality-scoring/systemprompt.md` | Create an ordinary judge example with explicit output contract and read-only evidence tools; require the operator to choose a real configured model. |
| `examples/quality-scoring/workflow.yaml`, `examples/quality-scoring/intake.json` | Create the ordinary scoring graph and explicit source-workflow binding example. Do not seed guessed provider settings or prices. |

### 9. Upgrade, verification harness and documentation

Existing state must survive rollout. Use the existing host-mounted storage where possible. An installation requiring new immutable KIND mounts needs an explicit preserve/export/verify procedure before replacement; deployment must not silently destroy a cluster. Tests for migrations run against disposable fixtures.

| File | Action and rationale |
| --- | --- |
| `scripts/migrate-factory-measurements.js`, `scripts/migrate-factory-measurements.test.js` | Create idempotent preflight/state export/config migration/verification steps preserving existing artifacts, host logs and retained policy. Require an explicit destructive-migration invocation rather than embedding it in `just deploy`. |
| `scripts/verify-factory-measurements.js`, `scripts/verify-factory-measurements.test.js` | Create a bounded integration driver covering event intake, judge output extraction, graph queries and replacement against test identities/streams only. |
| `services/evaluation/projector_integration_test.go` | Create opt-in pinned-OpenObserve tests for hourly boundaries, historical timestamps, late/buffered writes, ambiguous acknowledgements and crash replay. |
| `services/connectors/jetstream_integration_test.go` | Create opt-in broker/restart tests for fan-out, rewind, duplicate delivery and stable Temporal acceptance. |
| `README.md` | Document all delivered features, configuration, host paths, ordinary deployment and explicit validation limits. |
| `docs/ARCHITECTURE.md` | Update existing component responsibilities and event/evidence/measurement boundaries. |
| `docs/PRODUCT.md` | Document Events, Intake, judges and multi-line measurements without introducing A/B scope. |
| `docs/DESIGN.md` | Record the approved UI placement/controls while preserving its existing progressive-disclosure rules. |
| `services/tool-gateway/README.md` | Document read-only evidence tool contracts and limitations. |
| `docs/operations/factory-measurements.md` | Create recovery, backlog, range-rebuild, full-storage, upgrade and rollback procedures, including the distinction between code rollback and retention policy. |
| `docs/plans/factory-measurements-and-event-intake.md` | Link to the finalized design and keep task status aligned; retain future `just janitor` and tool-charge TODOs. |

### Verification Plan

Run tests appropriate to each work package, then the complete project checks. Do not treat mocked HTTP tests as evidence that OpenObserve deletion and replay work.

| Verification | Required evidence |
| --- | --- |
| Formatting/static checks | `just check`: JavaScript syntax/tests, rendered Kubernetes configuration, Go formatting/vet/race tests, Python tool tests and existing project checks. Extend its lists to include every new test file above. |
| Backend behavior | Optional/zero/unknown usage; real request versus telemetry retry identities; date boundaries; exact monetary totals; missing/stale conversion; source-versus-producer attribution; legacy backfill. |
| Storage recovery | Crash before/after mutation intent, canonical replacement and dirty state; stale-job exclusion; exclusive ownership; receipt replay after history expiry; deletion preserving retained evidence. |
| Workflow compatibility | Replay recorded pre-change root/nested histories; manual runs and new intake inputs; completed/failed/cancelled/blocked/terminated outcomes; fresh scoring traces with target links. |
| Live JetStream/Temporal | Confirmed handoff before acknowledgement; ambiguous submission; broker restart; independent consumers; disabled/no-submission and re-enable catch-up; no repeated business execution. |
| Live OpenObserve | Replace the same hour twice; assert unique identities and sums; restart at each phase; include buffered/late writes and concurrent canonical arrivals; preserve historical timestamps. |
| Host retention | Verify actual host files for all service log classes after rotation/outage/restart, including formerly excluded records. Test saturation reporting with a bounded disposable volume. |
| UI | Safari and Chrome: page initialization, keyboard access, narrow panels, draft/filter preservation, capped Events memory, rewind/live transitions, several plot series and missing/refresh states. |
| Complete journey | A real configured source workflow finishes, enabled intake starts the ordinary judge workflow, saved JSON maps to a typed metric, and the graph links it to both executions. Prices/conversion change; recalculation replaces costs without rerunning the judge. |
| Deployment | `just render`, `just check`, then `just update`/`just deploy` as appropriate and `just status`/`just info`. Verify served UI assets match the tested version. |

Any failed recovery or destructive-range isolation test blocks implementation completion. Runtime checks are work to perform during implementation; this document does not report them as already run. The delivery is complete only when every agreed interface and journey above works, local deployment is verified, and no requested feature remains deferred except `just janitor` and tool charges.

*Drafted with EGM*
