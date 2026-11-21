# Goldfish Review Report

**Target:** `docs/plans/factory-measurements-and-event-intake.md`, complete document.  
**Mode:** BRAINSTORM. **Date:** 2026-11-21.  
**Snapshot SHA-256:** `833ab975ec3abdda731fa9179a70595690c4d4aca4081fda4ea2a7ee448567b8`.

The target remains authoritative. Findings below refine implementation contracts within its scope. They are not additional user requirements, changes to the selected architecture, or an implementation-readiness verdict.

## Reviewer Status

| Reviewer | Status | Fresh context | Read evidence |
| --- | --- | --- | --- |
| egm-design | completed | `fork_turns: none` | Persona, target, Heilmeier/readiness standards, evaluation deployment |
| egm-codebase-details | completed | `fork_turns: none` | Persona, target, Heilmeier/readiness standards, telemetry/collector/query code |
| egm-zoom-out | completed | `fork_turns: none` | Persona, target, zoom-out standard, README, storage/query/workflow/tool code |
| egm-comprehension | completed | `fork_turns: none` | Persona, target, UI guide including antipatterns |
| egm-implementation-readiness | completed | `fork_turns: none` | Persona, target, readiness standard, workflow/deployment/usage code |
| egm-structural | completed | `fork_turns: none` | Persona, target, storage/evaluation/deletion code; no further brainstorm standard referenced |
| egm-inventive-catalyst | completed | `fork_turns: none` | Persona including inline catalog, complete target; no further standard referenced |

All seven reviewers returned actual findings or an explicit no-finding report. They ran read-only in capacity-limited waves, received no other reviewer's findings, and spawned no children. Fresh conversation isolation does not establish exclusion of host instruction files. Read evidence below is each child's reported actual reads with concrete excerpts; this synthesizer's interface does not expose child tool transcripts for independent read auditing.

## Prioritized Findings (Blockers -> Major -> Nits)

No concept-level blocker was established. Twelve deduplicated major implementation risks and two advisory items follow. Existing safeguards in the plan remain valid; recommendations close specific gaps or make their verification concrete.

### F1. Canonical updates can escape projection recovery — Severity: Major

- **Reviewer lenses:** egm-design, egm-zoom-out, egm-structural.
- **Affected scope:** target lines 173–179; `services/blobstore/main.go:244–250`.
- **Discrepancy:** overwriting a canonical cost and marking its hour dirty are separate writes. A crash between them can leave stale graphs indefinitely. Completing an old manifest must also not erase newer dirty work.
- **Recommendation:** persist a recoverable projection obligation with/before canonical mutation, or reconcile canonical revisions against completed manifests. Clear only the verified generation. Test the exact crash window and a concurrent arrival. Reuse the selected blobstore/projector.

### F2. Immediate and background writers need one ownership rule — Severity: Major

- **Reviewer lenses:** egm-zoom-out, egm-structural.
- **Affected scope:** target lines 157–179; `services/evaluation/main.go:82–87,340–342`.
- **Discrepancy:** immediate pricing, overlapping recalculation ranges, and hourly projection work do not yet share a specified stale-writer exclusion contract. Existing local mutexes do not fence an old instance after ownership changes.
- **Recommendation:** define canonical ownership/lock keys and revision checks for all current-cost writers. Choose a complete singleton/restart model or durable fencing; avoid adding a general coordinator. Test resumed older recalculation against new immediate pricing.

### F3. Whole-hour replacement can amplify work — Severity: Major

- **Reviewer lens:** egm-design.
- **Affected scope:** target lines 174–179; `infra/kubernetes/base/evaluation/deployment.yaml:44–45`.
- **Discrepancy:** repeated snapshots under sustained arrivals can cause repeated full-hour replay. The current evaluation limits are 300m CPU and 128Mi memory.
- **Recommendation:** choose bounded streaming batches, coalescing, concurrency, retry backoff, and fairness for current versus historical hours. Measure catch-up and freshness at realistic volume. This is a scheduling contract inside the selected projector, not another storage proposal.

### F4. Catch-up needs bounded execution and feedback protection — Severity: Major

- **Reviewer lens:** egm-design.
- **Affected scope:** target lines 116–122,193–199.
- **Discrepancy:** independent bindings and retained backlog can multiply scoring demand; implicit self-subscription avoidance does not establish protection against a multi-binding cycle.
- **Recommendation:** use existing broker/Temporal concurrency controls and bounded judge evidence/retry work. Validate the supplied example's workflow/outcome filters and bound feedback. Preserve backlog and expose throttling; do not add a separate quota-management product.

### F5. Recovery needs a measured horizon and progress health — Severity: Major

- **Reviewer lenses:** egm-design, egm-codebase-details.
- **Affected scope:** target lines 87–92,120; `services/orchestrator/pipeline/workflow.go:366–369`; `services/factory-ui/observability.go:266–321`.
- **Discrepancy:** reachable dependencies do not reveal a stalled reconciler, while an outage can exceed Temporal evidence retention. Poll cadence alone does not specify recoverability.
- **Recommendation:** name configured retention and supported outage/catch-up bounds. Extend existing health with last progress, oldest unresolved work, pending publication/binding age, projector phase and last error. Expose expired evidence explicitly. Test healthy dependencies with stalled recovery. Keep high-cardinality identities in structured records rather than metric labels.

### F6. Query completeness is not trace-source completeness — Severity: Major

- **Reviewer lenses:** egm-codebase-details, egm-zoom-out.
- **Affected scope:** target lines 195–199; `services/factory-ui/observability.go:237–263`; `services/agent/telemetry.go:19–22`; `services/llm-gateway/telemetry.go:19–20,61–67`; `services/orchestrator/pipeline/workflow.go:1238–1251`.
- **Discrepancy:** current queries use a trace-ID filter and 500-row cap; their complete flag describes returned indexed hits. Tracing can be disabled, and children share trace IDs. A direct tool wrapper would inherit sibling contamination or unjustified evidence claims.
- **Recommendation:** extend one existing backend evidence contract for execution scope, pagination, query completeness and unknown/unavailable source evidence. Reuse it from UI and a small registered tool adapter. Do not promise complete-span certification; test terminal executions with absent/dropped spans.

### F7. Host archival must precede collector exclusions — Severity: Major

- **Reviewer lens:** egm-codebase-details.
- **Affected scope:** target lines 75–77; `infra/kubernetes/base/log-collector/config.yaml:32,40,106–111`.
- **Discrepancy:** collector self-logs, long lines and ingestion access lines are currently excluded. A sink after those filters cannot satisfy all-log retention.
- **Recommendation:** define a redacted durable host route independent of search-specific exclusions, with explicit long-line and collector-failure handling that avoids feedback. Test these precise sources; keep the settled indefinite-log requirement.

### F8. Ordinary run deletion can erase recovery/accounting evidence — Severity: Major

- **Reviewer lens:** egm-structural.
- **Affected scope:** target lines 87,120,144–151; `services/orchestrator/server.go:285–286`; `services/blobstore/main.go:311–335`.
- **Discrepancy:** the plan protects pending workflow events from removable run artifacts but does not give receipts, usage, costs, judge records and manifests equally explicit namespace/deletion rules. Current run deletion removes run artifacts and execution history.
- **Recommendation:** keep acceptance receipts outside ordinary cleanup; document retained accounting evidence and projection behavior after run deletion. Preserve current data until a deliberate deletion policy is defined. Make any authorized measurement removal recoverable across canonical and query records.

### F9. Temporal histories need upgrade and rollback compatibility — Severity: Major

- **Reviewer lens:** egm-implementation-readiness.
- **Affected scope:** target lines 81–90,119; `services/orchestrator/pipeline/workflow.go:314–323`; `services/orchestrator/main.go:77–79`.
- **Discrepancy:** adding initialization/lifecycle commands to the existing workflow name can affect histories already in flight; old workers may not understand accepted intake inputs after rollback.
- **Recommendation:** choose a compatible workflow version boundary or explicit drain/isolation cutover. Keep compatible workers for accepted intake work. Validate recorded pre-upgrade root/nested histories. This is a migration requirement, not evidence of a demonstrated replay failure.

### F10. Existing installations need a host-mount migration path — Severity: Major

- **Reviewer lens:** egm-implementation-readiness.
- **Affected scope:** target lines 75–77,228; `scripts/generate-kind-config.js:17–40`; `justfile:47–58`; `infra/kubernetes/base/log-collector/daemonset.yaml:69–88`.
- **Discrepancy:** generated KIND mounts are fixed, cluster-create refuses an existing cluster, and raw logs currently live under node `/var/log`. A fresh-install test does not prove preservation for the existing installation.
- **Recommendation:** specify a justfile-supported upgrade with refreshed configuration and verified evidence preservation before any cluster replacement. Include Temporal/blobstore/OpenObserve data and existing node logs. No destructive migration is authorized by this review.

### F11. Legacy zeros may encode absence — Severity: Major

- **Reviewer lens:** egm-implementation-readiness.
- **Affected scope:** target lines 54,256; `services/agent/llm.go:22–25`; `services/agent/agent.go:493,523–526`.
- **Discrepancy:** old integer usage fields conflate omitted counts with zero; accepting those values as authoritative would violate evidence-only backfill.
- **Recommendation:** version new usage evidence and classify legacy zeros as ambiguous unless raw retained evidence proves presence. Add a fixture where an omitted provider count became a stored zero. Preserve genuinely reported zero.

### F12. Application rollback must preserve data policy and rebuild state — Severity: Major

- **Reviewer lens:** egm-implementation-readiness.
- **Affected scope:** target lines 173–183; `services/openobserve/config/openobserve.env:20`.
- **Discrepancy:** prior configuration specifies seven-day retention. Restoring old configuration could reinstate expiry; old services cannot safely resume new projection phases or report freshness.
- **Recommendation:** separate code rollback from retention rollback. Preserve canonical records and indefinite retention, stop incompatible new writers/intake before downgrade, and complete or visibly quarantine unfinished hours. Define compatible schema readers/writers.

### A1. Failure and cap presentation should use established controls — Severity: Nit (advisory UX)

- **Reviewer lens:** egm-comprehension; raw lens severity was Major for four UX concerns, normalized here as advisory implementation guidance because the plan already requires visible failures/freshness and the proposed controls are not settled scope.
- **Affected scope:** target lines 119–132,174–179,189–209,270; `docs/DESIGN.md:72–73,118`.
- **Guidance:** selected binding detail should distinguish submission failure from accepted work awaiting artifacts. Mapping errors should name the saved artifact/version and make clear that extraction retry does not rerun the judge. Graphs should distinguish temporary rebuilding from missing evidence without losing filters. Capped search results should expose order/range/cap rather than imply a complete history search.
- **Boundary:** new repair buttons and navigation beyond the 1,000-match cap are optional design suggestions; no new product controls are mandated by this report. Preserve drafts, selected detail and compact default rows.

### A2. Reuse domain vocabulary, not unbounded catalog scans — Severity: Nit

- **Reviewer lens:** egm-zoom-out.
- **Affected scope:** target lines 144,207–213; `services/evaluation/main.go:457–488`.
- **Guidance:** the existing catalog scans every run and launches work per ID. Use bounded OpenObserve time-range queries for plots while reusing evaluation identities and existing query transport. No new cache, index or database is warranted.

## Inventive assessment

**egm-inventive-catalyst:** “No false compromise detected.” Separation of canonical data/projections, reconciliation, and JetStream as intermediary already address the relevant contradictions. Their costs are projection lag, durable recovery state, and ambiguous acknowledgements/receipt maintenance. Finite host storage versus indefinite retention is a physical limit already handled through explicit write failure, not a reason to invent another subsystem.

## Evidence of Reads

Persona and standard paths below are under `/Users/flo/.codex/plugins/cache/egm/egm/0.1.1/`. Every reviewer read the target; no external implementation-source claim was independently reverified in this brainstorm.

| Reviewer | Actual reads reported and concrete evidence |
| --- | --- |
| egm-design | `agents/design_critic.md` ("Peak QPS, storage footprint ... LLM token budgets"); `resources/document_standards/heilmeier-standard.md`; `implementation-readiness-standard.md`; evaluation deployment (300m CPU / 128Mi). |
| egm-codebase-details | `agents/codebase_details.md`; Heilmeier/readiness standards; agent and gateway telemetry; UI observability; orchestrator pipeline; log-collector config/kustomization; telemetry-collector deployment; `services/openobserve/config.template`; `scripts/init-openobserve.js`. Excerpts: complete flag uses `!result.Partial && result.Total <= len(result.Hits)`; collector has `Skip_Long_Lines On`. |
| egm-zoom-out | `agents/zoom_out.md`; zoom-out standard ("minimal number of components"); README ("thin software factory on a single-node KIND cluster"); blobstore, evaluation, UI observability, orchestrator pipeline/main, tool-gateway `tools.py`. Excerpt: `os.Rename` precedes `s.writeMeta`; tool module registers public functions. |
| egm-comprehension | `agents/comprehension.md` ("API / CLI Ergonomics", "Cognitive Friction"); target; `docs/DESIGN.md`, including "a refresh never overwrites an edit in progress" and selected-event evidence guidance. No additional standard referenced by the brainstorm persona. |
| egm-implementation-readiness | `agents/implementation_readiness.md`; readiness standard; target; orchestrator workflow/main, KIND generator, justfile, collector daemonset, agent usage/aggregation, OpenObserve env. Excerpts: `PromptTokens int`; `ZO_COMPACT_DATA_RETENTION_DAYS=7`; existing-cluster refusal. |
| egm-structural | `agents/structural.md` ("coupling risks", "architectural decoupling requirements"); target; blobstore writes/deletes; evaluation state/locking; orchestrator deletion. Excerpt: object rename then separate metadata write. No further brainstorm standard referenced. |
| egm-inventive-catalyst | `agents/inventive_catalyst.md` and its inline pattern catalog ("theoretical invariants are immutable system bounds"); full target with focused lines 94–185. No additional standard referenced. |

The synthesizer read `agents/synthesizer.md`, `docs/codex.md`, all seven persona files, the brainstorm skill, the target, relevant standards and UI guidance. Neither output artifact existed before this review.

## Unresolved Implementation Work and Limits

The crash/revision and writer-ownership contracts, deletion behavior, workload bounds, migration/rollback sequencing, and recovery horizon remain to be made concrete and tested. These are unresolved implementation concerns, not evidence that a new product interview or architecture replacement is needed.

No code was changed, no service deployed, and no runtime or browser test performed. The anchored plan's acceptance checklist remains unexecuted. This review establishes seven-lens coverage and grounded risks; it does not establish implementation readiness or successful delivery.
