# Factory measurements and event intake

- **Author:** Florian
- **Date:** 2026-11-21
- **Status:** Seven-lens brainstorm complete; implementation readiness not established.
- **Anchored concept:** [Factory measurements and event intake](docs/plans/factory-measurements-and-event-intake.md).
- **Review detail:** [Goldfish review report](goldfish_review_report.md).

## Overview

Factory operators need to see execution time, token and cache usage, cost, and quality together, and automatically start ordinary workflows from workflow lifecycle events. This delivery includes durable JetStream workflow events, an Events viewer, connector-driven Intake, ordinary scoring workflows, generated JSON artifacts with typed mappings in the judges UI, configurable prices and EUR/USD conversion, replacement recalculation, and selectable multi-line graphs. Logs remain on the host indefinitely.

The existing blobstore and OpenObserve remain the storage architecture. JetStream owns delivery and replay; Temporal owns workflow execution. Judges remain ordinary agents with ordinary tools. Every requested capability belongs to this delivery; staging below establishes dependency order, not optional releases.

## Analysis

The main risks are at the boundaries between durable records. Writing a current cost and marking its query partition dirty are separate operations unless the implementation joins them with a durable obligation or repairs them through revision reconciliation. Immediate pricing, resumed recalculation, and projection rebuilding also need one explicit ownership/revision rule so an older worker cannot restore stale costs. Preserve the selected hourly delete/replay procedure; close these boundaries within that architecture.

The review also exposed concrete migration concerns. Existing workflows must replay safely after lifecycle and initialization commands change. Existing installations need a supported host-mount migration that preserves stored evidence. Old agent records use integer token fields, so an old zero can mean an omitted provider field. Rollback must preserve indefinite retention and must not leave partially rebuilt hours looking complete. Acceptance receipts and accounting evidence need storage/deletion rules distinct from removable run artifacts.

Operational health must show progress, not merely reachable dependencies. A stalled reconciler can outlive Temporal history even while the broker and workflow server remain healthy. Use the existing health surface for oldest pending work, last successful progress, projection phase, and retention-risk status. Retained history and scoring catch-up also require bounded execution and projection work using the existing JetStream/Temporal controls. These are engineering defaults, not a request for a new quota product.

A trace query that returned every indexed row does not prove that every execution span arrived. The new read-only evidence contract should distinguish query pagination/truncation, terminal execution status, and unavailable or unknown source evidence. Reuse one backend query boundary for UI and tools; scope it by execution so nested siblings cannot contaminate a judge's evidence. Do not add a trace-certification subsystem.

For host logs, the current collector excludes its own logs, skips long lines, and filters ingestion access logs. A host sink placed after those filters would preserve those omissions. Establish the redacted archival route before search-specific exclusions, with explicit handling of collector failures and oversized records.

The inventive lens found **no false compromise detected**. The concept already uses separation of evidence from projections (at the cost of visible projection lag), automatic reconciliation (at the cost of durable recovery state and a retention horizon), and an intermediary broker (at the cost of acknowledgement ambiguity and receipt maintenance). Its ideal result is evidence written once, with publication, initialization, extraction, and projection recovering independently without rerunning paid judges or blocking ordinary execution. These patterns do not justify another coordinator or storage service.

UX recommendations remain bounded by the UI guide: show failures and incomplete coverage in selected details, preserve drafts and filters, and avoid duplicate telemetry summaries. Additional extraction-repair buttons or event-search continuation controls are reviewer suggestions, not settled requirements. The required minimum is honest failure/cap status and clear separation of retrying extraction from running a paid judge again.

### Reviewer coverage

| Reviewer | Status | Contribution |
| --- | --- | --- |
| egm-design | completed | Durability boundaries, rebuild cost, catch-up limits, recovery horizon |
| egm-codebase-details | completed | Trace evidence uncertainty, recovery health, collector exclusions |
| egm-zoom-out | completed | Shared ownership and existing query/domain reuse |
| egm-comprehension | completed | Failure clarity, graph freshness, capped search semantics |
| egm-implementation-readiness | completed | Workflow history, cluster migration, legacy zeros, rollback |
| egm-structural | completed | Durable obligations, stale writers, deletion ownership |
| egm-inventive-catalyst | completed | No false compromise; existing patterns and their costs |

All seven ran with fresh conversation contexts against the same unchanged target, in capacity-limited waves. Their reported reads and concrete excerpts are recorded in the review report. No failed or missing lens was counted as a clean review.

## Goals vs. Non-Goals

**In-scope goals:** durable workflow-only lifecycle events and replay; Events and Intake UI; automatic enabled bindings; root/nested execution identity and elapsed time; reported token/cache evidence with missing-data states; effective-dated decimal pricing; editable EUR/USD conversion and display currency; replacement cost recalculation; ordinary judge workflows with separate traces; JSON artifacts and versioned typed mappings; numerical graphs and inspectable text findings; indefinite host logs; recovery tests and local deployment through justfile targets.

**Out-of-scope non-goals:** special scorer node types or hidden terminal scorers; node-lasso scopes or workflow scorer attachments; non-workflow event coverage; another consumer-state service, scheduler, agent registry, or database; guessed cache usage; competing cost histories; external exchange-rate feeds or freshness rejection; A/B testing and automatic ranking. Tool-charge accounting and the future `just janitor` remain later work. No automatic log deletion is introduced.

## Alternatives Weighed

| Alternative | Disposition and reason |
| --- | --- |
| Ordinary scoring workflows versus special scoring machinery | Settled in the anchored plan: ordinary agents/workflows preserve existing composition and execution identity. |
| Existing blobstore/OpenObserve versus another database | Storage choice is already resolved in the anchored plan. Complete the selected hourly replacement protocol and its tests; do not reopen database selection. |
| Direct JetStream-to-Temporal submission versus a pending-intake dispatcher | Direct handoff is selected. Durable receipts prevent duplicates without owning scheduling. |
| Replace current costs versus keep competing cost versions | Replacement is settled. Original usage remains available, while graphs expose one current calculation. |
| Explicit unavailable evidence versus inferred usage/cache/attribution | Preserve unavailable/unknown states; fabricated values would undermine the requested visibility. |
| Durable projection obligation versus revision reconciliation | Engineering options still to select/document for the canonical-write crash gap. Both fit the selected storage; neither is attributed to a user decision. |
| Deployment singleton versus durable fenced ownership | Engineering options for excluding stale writers. Choose one complete model with restart guarantees; a process-local mutex alone is insufficient. |
| New repair/navigation controls versus existing detail/actions | Advisory UX choice. First satisfy failure, freshness, and search-cap clarity using established controls. Extra controls are not added to the settled scope by this brief. |

## Key Decisions & Staging for Next Steps

**User-settled scope** is the anchored plan's Settled requirements and explicit exclusions. **Selected architecture** includes existing blobstore/OpenObserve and hourly delete/replay. **Engineering conventions** such as nested workflow coverage, indefinite event retention, pause/resume catch-up, request-start pricing and half-open intervals remain labelled implementation interpretations/proposals in that plan; this brief does not relabel them as user decisions.

1. Record bounded implementation contracts: canonical mutation/projection recovery, writer ownership, deletion namespaces, recovery horizon, workload limits, compatible workflow histories, and upgrade/rollback sequencing. Preserve all requested capabilities.
2. Prove durable foundations using failure fixtures: canonical-write crash windows, concurrent/stale writers, acceptance/receipt replay, collector exclusions, host restarts, and existing-installation migration. Keep retention policy independent of application rollback.
3. Build on those foundations: provider evidence and deterministic pricing, ordinary scoring and read-only evidence, typed extraction, then the requested configuration/views/graphs. Use bounded OpenObserve queries rather than scanning all historical runs for each plot.
4. Run the anchored acceptance checks, including a real completion event launching a scoring workflow whose saved JSON metric appears in a graph, replacement without duplicate contributions, disabled/catch-up behavior, browser checks, and justfile deployment.

There are **no concept-level blockers established by this brainstorm**. Unresolved implementation contracts and unperformed runtime checks prevent a readiness or completion claim. No product code, deployment, or anchored plan was changed during this review; no timeline or resource estimate has been validated.
