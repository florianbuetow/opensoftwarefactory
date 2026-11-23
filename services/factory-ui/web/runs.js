"use strict";

// Workflow descriptions belong to one stored version, but unlike the graph they may change
// in place. Runs therefore load the exact version on demand instead of keeping definitions as
// an immutable source for hover text. A run that did not record a version has no safe
// description to show: using the newest one would describe something other than what ran.
const RunDescriptions = (() => {
  const values = new Map(); // "name@vN" -> description
  const pending = new Map(); // concurrent hover/focus events share one request
  const revisions = new Map(); // an editor update wins over an older request still in flight
  const refOf = (run) => {
    const name = String(run?.workflow || "");
    const version = Number(run?.workflow_version) || 0;
    return name && version > 0 ? WorkflowRef.format(name, version) : "";
  };
  const current = (run) => values.get(refOf(run)) || "";
  async function refresh(run) {
    const ref = refOf(run);
    if (!ref) return "";
    if (pending.has(ref)) return pending.get(ref);
    const { name, version } = WorkflowRef.parse(ref);
    const revision = revisions.get(ref) || 0;
    const request = api("GET", `/api/orchestrator/workflows/${encodeURIComponent(name)}/versions/${version}`)
      .then((definition) => String(definition?.description || ""))
      .catch(() => "")
      .then((description) => {
        if ((revisions.get(ref) || 0) !== revision) return values.get(ref) || "";
        values.set(ref, description);
        return description;
      })
      .finally(() => pending.delete(ref));
    pending.set(ref, request);
    return request;
  }
  function update(detail) {
    const name = String(detail?.name || "");
    const version = Number(detail?.version) || 0;
    if (!name || version < 1 || typeof detail?.description !== "string") return false;
    const ref = WorkflowRef.format(name, version);
    revisions.set(ref, (revisions.get(ref) || 0) + 1);
    values.set(ref, detail.description);
    return true;
  }
  return { current, refresh, update, refOf };
})();
// Runs page: every run from runs/<trace>/run.json, newest first, filtered by workflow and
// status, with a detail panel for the selected run. Hash: #runs/<trace>?workflow=&status=.

// ---- Logic without the DOM (covered by merge_batch_ui_test.mjs) ----

function skillToolWarning(event) {
  return event.kind === "tool_call" && event.warning
    ? h("p", { className: "tool-warning" }, h("strong", {}, "Warning"), " ", event.warning)
    : null;
}

function runSkillVersions(metadata) {
  const entries = Object.entries(metadata?.agents || {}).flatMap(([agent, settings]) => {
    const skills = Object.entries(settings?.skills || {});
    return skills.length ? [
      h("dt", {}, agent),
      h("dd", {}, skills.map(([name, version]) => `${name}@${version}`).sort().join(", ")),
    ] : [];
  });
  return entries.length ? h("div", { className: "section" },
    h("h3", { className: "section-title" }, "Skills"),
    h("dl", { className: "prompt-evidence-links" }, ...entries)) : null;
}

const RunsLogic = (() => {
  const isNode = (s) => s?.service === "merge" || s?.service === "batch";
  const count = (n) => Math.max(0, Math.floor(Number(n)) || 0);

  // nodeView is the one place that reads what the run status says about a merge or batch step.
  // The answer of GET /api/orchestrator/runs/<trace> has "nodes", keyed by step ID, for the merge
  // and batch steps of the run's own workflow (left out when it has none):
  //   merge: { "kind": "merge", "sources": ["a", "b"], "waiting": [{ "filled": ["a"] }], "forwarded": 1, "expires": "<time>" }
  //   batch: { "kind": "batch", "collected": [{ "source", "artifact", "round" }], "size": 5, "released": 2,
  //            "releases": [{ "pass", "items", "at", "timed" }], "expires": "<time>" }
  // waiting lists the entries not forwarded yet, oldest first; filled names the sources that
  // delivered for an entry. timed marks a batch the timeout released. expires is there only
  // while the input timeout runs. step is the step's definition, for what the status leaves
  // out. Returns null when the status does not name the step.
  function nodeView(status, id, step) {
    const raw = status?.nodes?.[id];
    if (!raw || typeof raw !== "object") return null;
    const kind = raw.kind === "merge" || raw.kind === "batch" ? raw.kind : step?.service;
    const expires = typeof raw.expires === "string" && !raw.expires.startsWith("0001") && !Number.isNaN(Date.parse(raw.expires)) ? raw.expires : "";
    if (kind === "merge") {
      const sources = (Array.isArray(raw.sources) && raw.sources.length ? raw.sources : step?.depends_on || []).map(String);
      const waiting = (Array.isArray(raw.waiting) ? raw.waiting : []).map((entry) => {
        const filled = (Array.isArray(entry?.filled) ? entry.filled : []).map(String);
        return { filled: sources.filter((s) => filled.includes(s)), open: sources.filter((s) => !filled.includes(s)) };
      });
      return { kind, sources, waiting, forwarded: count(raw.forwarded), expires };
    }
    if (kind === "batch") {
      const collected = (Array.isArray(raw.collected) ? raw.collected : []).filter((item) => item && typeof item === "object")
        .map((item) => ({ source: String(item.source || ""), artifact: String(item.artifact || ""), round: count(item.round) || 1 }));
      const timed = (Array.isArray(raw.releases) ? raw.releases : []).filter((release) => release && release.timed === true).length;
      return { kind, collected, size: count(raw.size) || count(step?.size), released: count(raw.released), timed, expires };
    }
    return null;
  }

  // countdown is the time left until a timer runs out, as minutes and seconds ("4:12").
  function countdown(expires, now = Date.now()) {
    const left = Math.max(0, Math.round((Date.parse(expires) - now) / 1000));
    return Number.isNaN(left) ? "" : `${Math.floor(left / 60)}:${String(left % 60).padStart(2, "0")}`;
  }

  const plural = (n, one, many) => `${n} ${n === 1 ? one : many}`;

  // nodeLines says what a merge or batch step holds, one line each. A merge lists its waiting
  // entries with the sources that delivered and those still open, then how many it forwarded. A
  // batch says how many results it collected and when it releases them, then how many batches
  // it released and how many of those the timeout released. name turns a step ID into what the page shows.
  function nodeLines(view, now = Date.now(), name = (id) => id) {
    if (!view) return [];
    const names = (ids) => ids.map(name).join(", ");
    if (view.kind === "merge") {
      const lines = view.waiting.map((entry, i) => {
        const number = view.forwarded + i + 1;
        if (!entry.open.length) return `Entry ${number}: has ${names(entry.filled)}, forwarded next`;
        // The timer belongs to the oldest waiting entry.
        const timer = i === 0 && view.expires ? `, fails in ${countdown(view.expires, now)}` : "";
        return `Entry ${number}: has ${names(entry.filled) || "nothing"}, waits for ${names(entry.open)}${timer}`;
      });
      return [...lines, `${plural(view.forwarded, "entry", "entries")} forwarded`];
    }
    const held = view.collected.length;
    const timer = held && view.expires ? `, releases in ${countdown(view.expires, now)}` : "";
    return [`${held}${view.size ? ` of ${view.size}` : ""} collected${timer}`, `${plural(view.released, "batch", "batches")} released${view.timed ? `, ${view.timed} by timeout` : ""}`];
  }

  // nodeState is the state of a merge or batch step: running while it holds something that
  // was not forwarded, done once it forwarded and holds nothing, idle before its first result.
  function nodeState(view, runStatus) {
    if (runStatus === "Completed") return "done";
    if (!view) return "idle";
    const holds = view.kind === "merge" ? view.waiting.length : view.collected.length;
    if (holds) return runStatus === "Running" ? "running" : "idle";
    return (view.kind === "merge" ? view.forwarded : view.released) ? "done" : "idle";
  }

  // forwardedBy is how many messages a node sent on: each one starts a pass of the steps after it.
  const forwardedBy = (view) => (!view ? 0 : view.kind === "merge" ? view.forwarded : view.released);

  // passSource names the merge or batch step whose messages make a step run in passes: the
  // nearest one above it in the workflow, or "" when there is none.
  function passSource(def, id) {
    const byId = new Map((def?.steps || []).map((s) => [s.id, s]));
    const seen = new Set([id]);
    let level = byId.get(id)?.depends_on || [];
    while (level.length) {
      const next = [];
      for (const dep of level) {
        if (seen.has(dep)) continue;
        seen.add(dep);
        const step = byId.get(dep);
        if (isNode(step)) return dep;
        next.push(...(step?.depends_on || []));
      }
      level = next;
    }
    return "";
  }

  // passRows lists the passes of a step after a merge or batch, oldest first: the stored ones
  // ([{ pass, artifact, record }]) and, while the run goes on and the node has forwarded more
  // messages than the step has passes, the next one. ready says whether that next pass has
  // what it depends on; it runs then and is idle until then. A pass without an output in a run
  // that ended early failed when the run failed at this step (failed), and is idle otherwise.
  function passRows(passes, forwarded, runStatus, ready = true, failed = true) {
    const rows = (passes || []).map((p) => ({
      pass: p.pass, artifact: p.artifact || "",
      state: p.artifact ? "done" : runStatus === "Running" ? "running" : runStatus === "Completed" ? "done" : failed ? "failed" : "idle",
    }));
    const last = rows.length ? rows[rows.length - 1].pass : 0;
    if (runStatus === "Running" && forwarded > last) rows.push({ pass: last + 1, artifact: "", state: ready ? "running" : "idle" });
    return rows;
  }

  // stepLabel names a step for the page: its path in title case, and its pass when it has one
  // ("compare.pass-2" is "Compare · pass 2").
  function stepLabel(key, title = (id) => id) {
    const { step, pass, round } = stepPass(key);
    return `${step.split("/").map(title).join(" / ")}${pass ? ` · pass ${pass}` : ""}${round ? ` · round ${round}` : ""}`;
  }

  // roundRows lists the rounds of a step that ran again after a rejection, oldest first: the
  // kept ones ([{ round, artifact, record }], stored as <step>.round-<n>) and the newest, whose
  // result has the step's own name (current). count is how often the run status says the step
  // ran; 0 when it does not say. A step that ran once has no rows. earlier is the step's own
  // result when its newest round failed before replacing it: that result is the round before's.
  function roundRows(kept, count = 0, earlier = "") {
    const rows = (kept || []).map((r) => ({ round: r.round, artifact: r.artifact || "", current: false }));
    if (earlier) rows.push({ round: Math.max(rows.length ? rows[rows.length - 1].round + 1 : 1, (Math.floor(Number(count)) || 0) - 1), artifact: earlier, current: false });
    if (!rows.length) return [];
    rows.push({ round: Math.max(rows[rows.length - 1].round + 1, Math.floor(Number(count)) || 0), artifact: "", current: true });
    return rows;
  }

  // runEndLine names a run that failed outside its steps, after the last one that finished
  // ("Run timed out after Review"). It is never shown beside a failed step.
  function runEndLine(status, last = "") {
    const how = { TimedOut: "timed out", Terminated: "was terminated", Canceled: "was canceled" }[status] || "failed";
    return `Run ${how}${last ? ` after ${last}` : ""}`;
  }

  // pullRequest is the one place that reads a pull request record, runs/<run>/pull-request.json:
  //   { repository, source_branch, work_branch, title, body, commits: [{ hash, subject, author }],
  //     patch: "<object ID of the patch>", pushed, url }
  // url is there only for a pull request opened on GitHub. Returns null for anything else.
  function pullRequest(text) {
    let raw;
    try { raw = JSON.parse(text); } catch { return null; }
    if (!raw || typeof raw !== "object" || Array.isArray(raw)) return null;
    const word = (value) => (typeof value === "string" ? value : "");
    return {
      repository: word(raw.repository), sourceBranch: word(raw.source_branch), workBranch: word(raw.work_branch),
      title: word(raw.title), body: word(raw.body),
      commits: (Array.isArray(raw.commits) ? raw.commits : []).filter((c) => c && typeof c === "object")
        .map((c) => ({ hash: word(c.hash), subject: word(c.subject), author: word(c.author) })),
      patch: word(raw.patch), pushed: raw.pushed === true, url: /^https?:\/\//.test(word(raw.url)) ? raw.url : "",
    };
  }

  // stepDocument reads the output document of a dev pod, command or stop step and says what
  // the step did: lines of text and, for a command, the command and its output.
  //   devpod:      { pod, repository, source_branch, work_branch, start_commit, reused }
  //   command:     { command, exit_code, stdout, stderr }
  //   devpod-stop: { pod, stopped }
  // Returns null for another kind of step or a document that is not such JSON.
  function stepDocument(service, text) {
    let raw;
    try { raw = JSON.parse(text); } catch { return null; }
    if (!raw || typeof raw !== "object" || Array.isArray(raw)) return null;
    const word = (value) => (typeof value === "string" ? value : "");
    if (service === "devpod") {
      const from = [word(raw.source_branch) ? `From ${raw.source_branch}` : "", word(raw.start_commit) ? `at ${raw.start_commit.slice(0, 8)}` : ""].filter(Boolean).join(" ");
      return { lines: [[word(raw.repository), word(raw.work_branch)].filter(Boolean).join(" · "), [from, raw.reused === true ? "continued in the live dev pod" : ""].filter(Boolean).join(", ")].filter(Boolean), command: "", output: "" };
    }
    if (service === "command") {
      const output = [word(raw.stdout), word(raw.stderr)].filter((part) => part.trim()).join(word(raw.stdout).endsWith("\n") ? "" : "\n");
      return { lines: [Number.isInteger(raw.exit_code) ? `Exit code ${raw.exit_code}` : ""].filter(Boolean), command: word(raw.command), output };
    }
    if (service === "devpod-stop") return { lines: [raw.stopped === false ? "Dev pod not deleted" : "Dev pod deleted"], command: "", output: "" };
    return null;
  }

  // recordKey is the key of a step's execution record: the step itself, or its newest pass.
  function recordKey(st, id) {
    if (st?.records?.has(id)) return id;
    return [...(st?.passes?.[id] || [])].reverse().find((p) => p.record)?.record || "";
  }

  // teamPaths lists the teams of a team step: one per pass when the step runs in passes
  // (teams/<trace>/<step>.pass-<n>), known from its stored passes and from the running teams;
  // otherwise the step's one team, with pass 0.
  function teamPaths(base, passes, running) {
    const found = new Set((passes || []).map((p) => p.pass));
    for (const t of running || []) {
      const at = stepPass(String(t?.team || ""));
      if (at.step === base && at.pass) found.add(at.pass);
    }
    return found.size ? [...found].sort((x, y) => x - y).map((pass) => ({ team: `${base}.pass-${pass}`, pass })) : [{ team: base, pass: 0 }];
  }

  return { isNode, nodeView, countdown, nodeLines, nodeState, forwardedBy, passSource, passRows, stepLabel, recordKey, teamPaths, roundRows, runEndLine, pullRequest, stepDocument };
})();

(() => {
  const MAX_ROWS = 100;
  const STATUS_FILTERS = [["", "All"], ["running", "Running"], ["waiting", "Waiting"], ["done", "Completed"], ["failed", "Failed"]];
  const state = { workflow: "", status: "", selected: "", timer: null, definitions: null, error: "" };

  const filtersBox = $("runs-filters");
  const tableBox = $("runs-table");
  const detail = $("run-detail");

  // The workflow definitions, for the step list of a run (loaded once per visit): the newest
  // version of every workflow.
  async function definitions() {
    if (!state.definitions) {
      try { state.definitions = (await api("GET", "/api/orchestrator/workflows")).workflows || []; } catch { state.definitions = []; }
    }
    return state.definitions;
  }

  // versionDef loads one stored version of a workflow, once; null when it cannot be loaded.
  const versionDefs = new Map(); // "name@vN" -> a promise of the definition
  function versionDef(name, version) {
    const ref = WorkflowRef.format(name, version);
    if (!versionDefs.has(ref)) versionDefs.set(ref, api("GET", `/api/orchestrator/workflows/${encodeURIComponent(name)}/versions/${version}`).catch(() => null));
    return versionDefs.get(ref);
  }

  // runDefs adds the older versions a run used (its own and those of its nested workflows) to
  // the newest definitions, so its steps are read from what ran. The newest stay first.
  async function runDefs(defs, used) {
    const older = used.filter((u) => u && u.name && Number(u.version) > 0 && !defs.some((w) => w.name === u.name && Number(w.version) === Number(u.version)));
    const loaded = await Promise.all(older.map((u) => versionDef(u.name, Number(u.version))));
    return [...defs, ...loaded.filter(Boolean)];
  }

  // runMetadata reads runs/<trace>/metadata.json once the run's listing shows it: the workflow
  // version, the nested workflow versions and the agents the run used. null for a run without it.
  const metadataCache = new Map(); // trace -> metadata
  async function runMetadata(trace, st) {
    if (!st.metadata) return null;
    if (metadataCache.has(trace)) return metadataCache.get(trace);
    try {
      const value = JSON.parse((await loadBlob(`runs/${trace}/metadata.json`)).text);
      if (!value || typeof value !== "object") return null;
      metadataCache.set(trace, value);
      return value;
    } catch { return null; }
  }


  const stateOf = (trace) => {
    const st = runIndex.statuses.get(trace);
    return st ? runState(st.status, isWaiting(trace)) : "";
  };

  function hashFor(trace = state.selected) {
    const q = new URLSearchParams();
    if (state.workflow) q.set("workflow", state.workflow);
    if (state.status) q.set("status", state.status);
    const query = q.toString();
    return `#runs${trace ? `/${trace}` : ""}${query ? `?${query}` : ""}`;
  }

  function renderFilters() {
    const names = [...new Set(runIndex.list().map((r) => r.workflow).filter(Boolean))].sort();
    $("runs-workflows").replaceChildren(...[["", "All workflows"], ...names.map((n) => [n, titleCase(n)])].map(([value, label]) => {
      const li = h("li", { role: "option", tabIndex: 0, "aria-selected": String(state.workflow === value) }, h("div", { className: "row-title" }, label));
      const pick = () => { state.workflow = value; state.selected = ""; location.hash = hashFor(""); };
      li.addEventListener("click", pick);
      li.addEventListener("contextmenu", (ev) => openMenu(ev, [
        ["Clear runs", () => clearRuns(runIndex.list(value)), "danger", value ? `Removes every ${titleCase(value)} run with its artifacts` : "Removes every run with its artifacts"],
      ]));
      li.addEventListener("keydown", (ev) => { if (ev.key === "Enter" || ev.key === " ") { ev.preventDefault(); pick(); } });
      return li;
    }));
    const seg = h("div", { className: "segmented", role: "radiogroup", "aria-label": "Status" },
      ...STATUS_FILTERS.map(([value, label]) => h("button", {
        type: "button", role: "radio", "aria-checked": String(state.status === value),
        onclick: () => { state.status = value; location.hash = hashFor(); },
      }, label)));
    // A narrow column shows the same filter as a dropdown (see .status-select).
    const select = h("select", { className: "fit status-select", "aria-label": "Status", onchange: (ev) => { state.status = ev.target.value; location.hash = hashFor(); } },
      ...STATUS_FILTERS.map(([value, label]) => h("option", { value, selected: state.status === value }, label)));
    filtersBox.replaceChildren(seg, select);
  }

  function visibleRuns() {
    return runIndex.list(state.workflow)
      .filter((r) => !state.status || stateOf(r.trace_id) === state.status)
      .slice(0, MAX_ROWS);
  }

  function renderTable() {
    if (state.error) { tableBox.replaceChildren(errorPanel(state.error)); return; }
    const runs = visibleRuns();
    if (!runIndex.records.size) {
      tableBox.replaceChildren(emptyPanel("No runs yet", { label: "Open Workflows", run: () => { location.hash = "#workflows"; } }));
      return;
    }
    if (!runs.length) {
      tableBox.replaceChildren(emptyPanel("No matching runs", { label: "Clear filters", run: () => { state.workflow = state.status = ""; location.hash = hashFor(); } }));
      return;
    }
    // The workflow column is left out while the sidebar already names the workflow.
    const showWorkflow = !state.workflow;
    tableBox.replaceChildren(dataTable(["Status", ...(showWorkflow ? ["Workflow"] : []), "Started", "Failure"], runs.map((r) => {
      const st = runIndex.statuses.get(r.trace_id);
      const failed = stateOf(r.trace_id) === "failed";
      const failure = failed ? rootError(st?.error || "Run failed", r.trace_id) : "";
      const workflowName = showWorkflow ? h("span", {
        className: "run-workflow-name",
        title: RunDescriptions.current(r) || undefined,
        "aria-description": RunDescriptions.current(r) || undefined,
      }, runIndex.name(r.trace_id)) : null;
      const tr = h("tr", { "aria-selected": String(r.trace_id === state.selected), className: failed ? "run-failed" : "", title: failure || undefined },
        h("td", {}, runChip(r.trace_id, st)),
        showWorkflow ? h("td", { className: "name-cell", dataset: { full: runIndex.name(r.trace_id) } }, workflowName) : null,
        h("td", { className: "fit" }, timeEl(r.started)),
        h("td", { className: "run-failure opt", dataset: { full: failure } }, failure));
      const describe = async () => {
        if (!RunDescriptions.refOf(r)) return;
        const description = await RunDescriptions.refresh(r);
        const target = workflowName || tr;
        target.title = workflowName ? description : [description, failure].filter(Boolean).join("\n\n");
        if (description) target.setAttribute("aria-description", description);
        else target.removeAttribute("aria-description");
      };
      (workflowName || tr).addEventListener("mouseenter", describe);
      tr.addEventListener("focus", describe);
      // Worked out when the menu opens, since the definitions may arrive after the table is drawn.
      // A team step that runs in passes has one team per pass, named by its pass.
      const teamItems = () => TeamsLogic.teamSteps(state.definitions || [], WorkflowRef.format(r.workflow || st?.workflow || "", runVersion(r)))
        .flatMap((id, _, all) => RunsLogic.teamPaths(TeamsLogic.teamPath(r.trace_id, id), runIndex.statuses.get(r.trace_id)?.passes?.[id], teamsState.running).map(({ team, pass }) => {
          const which = [all.length > 1 ? id : "", pass ? `pass ${pass}` : ""].filter(Boolean).join(" · ");
          return [which ? `View messages: ${which}` : "View messages", () => { location.hash = TeamsLogic.teamHash(team); }, "", "Opens the messages between the team's agents and the files they wrote"];
        }));
      tr.addEventListener("contextmenu", (ev) => openMenu(ev, [
        ["Re-run", () => rerun(r), "", runVersion(r) ? `Starts v${runVersion(r)} of the workflow, the version this run used, with its original input` : "Starts the newest version of the workflow with this run’s original input"],
        ...teamItems(),
        ["View trace", () => { location.hash = `#artifacts?trace=${encodeURIComponent(r.trace_id)}`; }, "", "Opens the run's objects in Blobstore"],
        "-",
        ["Remove run", () => clearRuns([r]), "danger", "Removes this run with its artifacts"],
        "-",
        ["Clear runs", () => clearRuns(visibleRuns()), "danger", "Removes every run in this list with its artifacts"],
      ]));
      return clickableRow(tr, () => { location.hash = hashFor(r.trace_id); });
    }), { name: showWorkflow ? 1 : -1, optional: [showWorkflow ? 3 : 2] })); // Failure gives way as a column, header included
  }

  // The Workflows editor dispatches this after changing one stored version's description in
  // place. Update visible Runs immediately; a later hover still refetches to catch edits from
  // another browser tab.
  document.addEventListener("workflowdescriptionchange", (ev) => {
    if (RunDescriptions.update(ev.detail)) renderTable();
  });

  const rerunning = new Set();
  // runVersion is the workflow version a run used: from its metadata, else from run.json; 0 when unknown.
  const runVersion = (record, metadata) => Number(metadata?.workflow?.version) || Number(record?.workflow_version) || 0;
  async function rerun(run) {
    if (rerunning.has(run.trace_id)) return;
    if (!run.workflow) { setStatus("Not started: the run names no workflow", "error"); return; }
    rerunning.add(run.trace_id);
    try {
      const input = run.input_artifact || `runs/${run.trace_id}/input`;
      // A run that recorded its workflow version runs that version again; any other the newest.
      const version = runVersion(run) ? `&version=${runVersion(run)}` : "";
      const next = await api("POST", `/api/orchestrator/workflows/${encodeURIComponent(run.workflow)}/runs?input=${encodeURIComponent(input)}${version}`, undefined, creatorHeaders());
      setSaveState("Started");
      runIndex.refresh().catch(() => {});
      location.hash = `#runs/${encodeURIComponent(next.trace_id)}`;
    } catch (error) { setStatus(`Not started: ${error.message}`, "error"); }
    finally { rerunning.delete(run.trace_id); }
  }

  // clearRuns removes runs with their history and artifacts, after one confirmation.
  async function clearRuns(runs) {
    if (!runs.length) return;
    if (!(await confirmAction(runs.length === 1 ? "Remove this run with its artifacts?" : `Remove ${runs.length} runs with their artifacts?`, "Remove"))) return;
    // A removed selected run hands the selection to its neighbour: the next row, else the previous.
    const gone = new Set(runs.map((r) => r.trace_id));
    const list = visibleRuns();
    const at = list.findIndex((r) => r.trace_id === state.selected);
    const neighbour = at < 0 || !gone.has(state.selected) ? null
      : list.slice(at + 1).find((r) => !gone.has(r.trace_id)) || list.slice(0, at).reverse().find((r) => !gone.has(r.trace_id));
    try {
      for (const r of runs) {
        await api("DELETE", `/api/orchestrator/runs/${encodeURIComponent(r.trace_id)}`);
        runIndex.forget(r.trace_id);
        if (state.selected === r.trace_id) state.selected = neighbour?.trace_id || "";
      }
      setSaveState(runs.length === 1 ? "Removed" : `Removed ${runs.length} runs`);
    } catch (e) {
      setStatus(`Not removed: ${e.message}`, "error");
    }
    history.replaceState(null, "", hashFor());
    renderFilters();
    renderTable();
    renderDetail();
  }

  function runChip(trace, st) {
    const waiting = isWaiting(trace);
    if (st?.gone) return statusChip("idle", "Unknown", "Temporal has no history of this run; only its artifacts remain");
    // A blocked run gives the agent's reason as the chip's tooltip.
    return st && st.status ? statusChip(runState(st.status, waiting), runStateWord(st.status, waiting), st.blocked || "") : statusChip("idle", "…");
  }

  // stored reads a small JSON document of a run (a step's output, the pull request record),
  // once for a finished run and again every 10 s while the run goes on. "" when it cannot be read.
  const storedDocs = new Map(); // object ID -> { text, at, final }
  async function stored(id, final) {
    const cached = storedDocs.get(id);
    if (cached && (cached.final || Date.now() - cached.at < 10000)) return cached.text;
    let text = "";
    try { text = (await loadBlob(id)).text; } catch { return cached?.text || ""; }
    storedDocs.set(id, { text, at: Date.now(), final });
    return text;
  }

  // livePods lists the IDs of the dev pods that exist now, asked at most every 10 s; null
  // while the connector service does not answer.
  let podIds = null, podIdsAt = 0;
  async function livePods() {
    if (Date.now() - podIdsAt > 10000) {
      podIdsAt = Date.now();
      try { podIds = new Set(ConnectorsLogic.devPods(await api("GET", "/api/connectors/devpods")).map((p) => p.id)); } catch { podIds = null; }
    }
    return podIds;
  }

  // podSection shows the run's dev pod: its repository and work branch, and View files, which
  // opens the read-only view of the pod. A pod that is gone is said to be gone.
  function podSection(pod, live) {
    if (!pod) return null;
    const name = [pod.repository, pod.workBranch].filter(Boolean).join(" · ");
    const gone = live && !live.has(pod.id);
    return h("div", { className: "section" },
      h("h3", { className: "section-title" }, "Dev pod"),
      h("div", { className: "run-pod" },
        h("span", { className: "run-pod-name", dataset: { full: name } }, name),
        gone ? h("span", { className: "muted" }, "No longer exists")
          : h("a", { className: "step-output step-action", href: ConnectorsLogic.podHash(pod.id), title: "Opens the pod's files, its commit log and its diff against the source branch" }, "View files")));
  }

  // pullRequestSection shows the pull request record of the run: its title, its branch, its
  // commits, and the patch as a download.
  function pullRequestSection(record, id) {
    if (!record) return null;
    const branch = [record.workBranch && record.sourceBranch ? `${record.workBranch} into ${record.sourceBranch}` : record.workBranch, record.repository, record.pushed ? "pushed" : "not pushed"].filter(Boolean);
    const file = `${(record.workBranch || "pull-request").replace(/[^A-Za-z0-9._-]+/g, "-")}.patch`;
    return h("div", { className: "section" },
      h("div", { className: "file-head" }, h("h3", { className: "section-title" }, "Pull request"),
        h("a", { href: memoryObjectHash(id), title: "Opens the pull request record in Blobstore" }, "View record"),
        record.url ? h("a", { href: record.url, target: "_blank", rel: "noopener" }, "Open on GitHub") : null,
        record.patch && validBlobID(record.patch) ? h("a", { className: "step-action", href: `/api/blobs/blobs/${record.patch}`, download: file, title: "Downloads the commits as a patch that git am applies" }, "Download patch") : null),
      h("p", { className: "pull-request-title" }, record.title || "Untitled"),
      h("p", { className: "step-doc-line" }, branch.join(" · ")),
      record.commits.length ? h("ul", { className: "pull-request-commits" }, ...record.commits.map((c) => h("li", {},
        h("span", { className: "mono", title: c.hash }, c.hash.slice(0, 8)),
        h("span", { className: "subject", dataset: { full: c.subject } }, c.subject),
        h("span", { className: "author" }, c.author)))) : null);
  }

  // stepDoc shows what a dev pod, command or stop step did, under its name: a command step its
  // command, its exit code and, folded, its output.
  const openOutputs = new Set(); // the outputs unfolded by hand, so a redraw keeps them open
  function stepDoc(doc, key) {
    if (!doc) return null;
    const output = doc.output ? h("details", { open: openOutputs.has(key) }, h("summary", {}, "Output"), h("pre", { className: "file-block code source" }, doc.output)) : null;
    output?.addEventListener("toggle", () => { if (output.open) openOutputs.add(key); else openOutputs.delete(key); });
    return h("span", { className: "step-doc" },
      doc.command ? h("span", { className: "step-doc-line mono" }, doc.command) : null,
      ...doc.lines.map((line) => h("span", { className: "step-doc-line" }, line)),
      output);
  }

  // rootError reduces a Temporal error chain to its innermost message, in the page's words: a step
  // by its name, and Temporal's "activity StartToClose timeout" as "<Step> timed out".
  function rootError(error, trace) {
    const parts = String(error).split("): ");
    let msg = parts[parts.length - 1].replace(/(\s*\(type: [^)]*\))+\s*$/, "").split(`runs/${trace}/`).join("").trim();
    const steps = [...String(error).matchAll(/step "([^"]+)"/g)].map((m) => m[1]); // outermost first
    if (/^activity \w+ timeout$/i.test(msg)) msg = steps.length ? `${titleCase(steps[steps.length - 1])} timed out` : "timed out";
    msg = msg.replace(/step "([^"]+)"/g, (_, id) => titleCase(id)).replace(/:$/, ""); // a rejection without a comment
    // A merge or batch names itself and the sources that did not deliver: 'merge "collect" timed out: no result from "long-b"'.
    if (/^(merge|batch) "/.test(msg)) msg = msg.replace(/^(?:merge|batch) "([^"]+)"/, (_, id) => titleCase(id)).replace(/"([^"]+)"/g, (_, id) => titleCase(id));
    const text = msg || String(error);
    return text.charAt(0).toUpperCase() + text.slice(1); // a sentence, like every other line
  }

  // existingAgents lists the agent references the registry still has, refreshed every 30 s.
  let agentList = null, agentListAt = 0;
  async function existingAgents() {
    if (!agentList || Date.now() - agentListAt > 30000) {
      try {
        const d = await api("GET", "/api/agents/agents");
        agentList = new Set(d.entries.flatMap((e) => e.versions.map((v) => `${e.name}@${v.version || v}`)));
        agentListAt = Date.now();
      } catch { agentList ??= new Set(); }
    }
    return agentList;
  }

  // agentOf returns the agent an agent step ran (from its run record), or the one the
  // current definition pins for a step that has not run.
  const agentRefs = new Map();
  async function agentOf(trace, st, def, id, nestedStep = null) {
    const key = `${trace}/${id}`;
    if (agentRefs.has(key)) return agentRefs.get(key);
    let ref = "";
    const record = RunsLogic.recordKey(st, id); // of the newest pass for a step that runs in passes
    if (record) {
      try { ref = JSON.parse((await loadBlob(`runs/${trace}/${record}.run.json`)).text).agent || ""; } catch { /* no record */ }
    }
    const defined = nestedStep || def?.steps.find((s) => s.id === id); // a nested step's definition is handed in
    if (!ref && defined && (defined.service === "agent" || defined.service === "interactive")) ref = defined.agent || "";
    if (record) agentRefs.set(key, ref);
    return ref;
  }

  // waitingSessionOf finds the waiting session of a run's top-level step among the pending
  // ones; stepSession is the session a step links to: the waiting one, else the stored one.
  const waitingSessionOf = (pending, trace, id) => pending.find((p) => p.trace_id === trace && String(p.session).split("/")[2] === id);
  function stepSession(pending, folders, trace, id) {
    const session = waitingSessionOf(pending, trace, id)?.session || folders.get(id);
    return session && ChatLogic.validSession(session) ? session : "";
  }

  // sessionFolders finds the interactive sessions of a run that no longer wait, by listing
  // user-sessions/<trace>/ in the blobstore: top-level step -> session path. A folder that
  // holds no session itself is a route step; it leads to the first session of its child run.
  const sessionPaths = new Map(); // trace -> { at, paths }
  async function sessionFolders(trace, st, def) {
    // Only a workflow with an interactive or a route step can have sessions; an unknown one is asked.
    if (def && !def.steps.some((s) => s.service === "interactive" || s.service === "route")) return new Map();
    const cached = sessionPaths.get(trace);
    if (cached && (st.status !== "Running" || Date.now() - cached.at < 10000)) return cached.paths;
    const paths = new Map();
    const list = async (prefix) => {
      try { return await api("GET", `/api/blobs/list/${prefix}`); } catch { return { directories: [], objects: [] }; }
    };
    const isSession = (l) => (l.directories || []).some((d) => d === "files" || d === "chats") || (l.objects || []).some((x) => x.id.endsWith("/completed.json"));
    const root = await list(`user-sessions/${trace}`);
    for (const step of root.directories || []) {
      const folder = await list(`${root.prefix}/${step}`);
      if (isSession(folder)) { paths.set(step, folder.prefix); continue; }
      for (const child of folder.directories || []) {
        const inner = await list(`${folder.prefix}/${child}`);
        if (isSession(inner)) { paths.set(step, inner.prefix); break; }
      }
    }
    sessionPaths.set(trace, { at: Date.now(), paths });
    return paths;
  }

  async function executionRecords(trace, st, id) {
    if (!st.records?.has(id)) return null;
    const attempts = st.attemptRecords?.get(id) || [];
    const objectIDs = attempts.length ? attempts : [`runs/${trace}/${id}.run.json`];
    const loaded = await Promise.all(objectIDs.map(async (objectID) => {
      try { return { ...JSON.parse((await loadBlob(objectID)).text), _object_id: objectID }; } catch { return null; }
    }));
    return loaded.filter(Boolean);
  }

  function conversationEvidence(record, event) {
    const messages = Array.isArray(record.messages) ? record.messages : [];
    if (event.kind === "tool_call" && event.tool_call_id) {
      const call = messages.flatMap((message) => message?.tool_calls || []).find((item) => item?.id === event.tool_call_id);
      const result = messages.find((message) => message?.role === "tool" && message.tool_call_id === event.tool_call_id);
      return [
        call?.function?.arguments ? h("details", {}, h("summary", {}, "Tool arguments"), h("pre", { className: "file-block code" }, pretty(call.function.arguments))) : null,
        result?.content ? h("details", {}, h("summary", {}, "Tool result"), h("pre", { className: "file-block prose" }, result.content)) : null,
      ].filter(Boolean);
    }
    if (event.kind === "model_call") {
      return messages.length
        ? [h("details", {}, h("summary", {}, `Conversation · attempt ${record.attempt || 1}`), h("pre", { className: "file-block code" }, JSON.stringify(messages, null, 2)))]
        : [h("p", { className: "evidence-warning" }, "No conversation checkpoint was stored for this model call.")];
    }
    return [];
  }

  function metricEvidence(record, event) {
    const source = event.kind === "agent_execution" ? record : event;
    const values = [];
    if (source.estimated_input_tokens != null) values.push(["Approx. input", `≈${source.estimated_input_tokens} tokens`]);
    if (source.estimated_output_tokens != null) values.push(["Approx. output", `≈${source.estimated_output_tokens} tokens`]);
    if (source.processing_time_ms != null) values.push(["Processing time", formatDurationMs(source.processing_time_ms)]);
    else if (source.duration_ms != null) values.push(["Duration", formatDurationMs(source.duration_ms)]);
    if (!values.length) return null;
    return h("dl", { className: "evidence-metrics", title: "Token counts are crude estimates of approximately one token per four characters" },
      ...values.flatMap(([label, value]) => [h("dt", {}, label), h("dd", {}, value)]));
  }

  // Prompt provenance belongs to the model call: links to the recorded snapshots, or a plain
  // statement that a legacy record has none (today's registry prompt is never substituted).
  function promptEvidence(record, event, trace) {
    if (event.kind !== "model_call" && event.kind !== "agent_execution") return null;
    const links = promptEvidenceLinks({ ...record, run_id: trace });
    if (links) return links;
    const hasSystem = Array.isArray(record.messages) && record.messages.some((message) => message?.role === "system");
    return h("p", { className: "evidence-note" }, hasSystem
      ? "Prompt snapshots were not recorded; the effective prompt is the system message in the conversation."
      : "Prompt snapshots were not recorded for this attempt.");
  }

  async function showExecutionEvidence(panel, button, record, event, step, context) {
    for (const selected of button.closest("ul").querySelectorAll(".timeline-select[aria-selected=true]")) selected.setAttribute("aria-selected", "false");
    button.setAttribute("aria-selected", "true");
    panel.hidden = false;
    const interrupted = event.status === "running" && (record.status && record.status !== "running" || context.runStatus && context.runStatus !== "Running");
    const links = [];
    const inputs = [...new Set([...(event.input_artifacts || []), context.inputArtifact].filter(Boolean))];
    for (const [index, id] of inputs.entries()) links.push(h("a", { href: `#artifacts?trace=${encodeURIComponent(context.trace)}&object=${encodeURIComponent(id)}` }, inputs.length > 1 ? `View input ${index + 1}` : "View input"));
    const output = event.output_artifact || context.artifacts[step] || context.passArtifacts?.[step];
    if (output) links.push(h("a", { href: `#artifacts?trace=${encodeURIComponent(context.trace)}&object=${encodeURIComponent(output)}` }, "View output"));
    if (record._object_id) links.push(h("a", { href: `#artifacts?trace=${encodeURIComponent(context.trace)}&object=${encodeURIComponent(record._object_id)}` }, "View execution record"));
    const heading = event.kind === "tool_call" ? `Tool call · ${event.name || "unknown"}`
      : event.kind === "model_call" ? `Model call${event.model ? ` · ${event.model}` : ""}`
        : event.kind === "tool_discovery" ? `Tool discovery${event.service ? ` · ${event.service}` : ""}` : titleCase(event.name || event.kind || "Event");
    const logs = h("div", { className: "evidence-logs" }, h("p", { className: "evidence-note" }, "Loading logs…"));
    const spans = h("div", { className: "evidence-spans" }, h("p", { className: "evidence-note" }, "Loading trace…"));
    fill(panel,
      h("div", { className: "file-head" }, h("h3", { className: "section-title" }, heading), links.length ? h("span", { className: "evidence-links" }, ...links) : null),
      interrupted ? h("p", { className: "evidence-warning" }, "This event started, but its end was never recorded.") : null,
      !inputs.length ? h("p", { className: "evidence-warning" }, "This record names no input artifact.") : null,
      event.error ? h("p", { className: "error-line" }, event.error) : null,
      skillToolWarning(event),
      metricEvidence(record, event),
      promptEvidence(record, event, context.trace),
      ...conversationEvidence(record, event), spans, logs,
    );
    const traceID = event.trace_id || record.trace_id;
    const logsPromise = (async () => {
      const params = new URLSearchParams({ run_id: context.trace });
      if (traceID) params.set("trace_id", traceID);
      if (step) params.set("step", step);
      if (record.attempt) params.set("attempt", record.attempt);
      const started = event.started || record.started, finished = event.finished || record.finished;
      if (started && finished) { params.set("start", started); params.set("end", finished); }
      const result = await api("GET", `/api/observability/logs?${params}`);
      if (!result.logs?.length) {
        const started = new Date(record.started || 0).getTime();
        const expired = context.retentionDays && started && Date.now() - started > context.retentionDays * 86400000;
        logs.replaceChildren(h("p", { className: "evidence-warning" }, expired
          ? `No matching logs remain. Logs are kept for ${context.retentionDays} days.`
          : "No matching logs were collected for this event."));
        return;
      }
      fill(logs, result.partial || result.truncated ? h("p", { className: "evidence-warning" }, "Only part of the matching logs was returned.") : null,
        h("h4", {}, "Logs"), h("ul", { className: "evidence-log-list" }, ...result.logs.map((entry) =>
        h("li", {}, h("span", { className: "evidence-log-source" }, entry.container || "service"),
          h("span", { className: `evidence-log-level ${String(entry.level || "").toLowerCase()}` }, entry.level || ""),
          h("span", { className: "evidence-log-message", title: entry.error || entry.message }, entry.message || entry.error || "Log event")))));
    })().catch((error) => logs.replaceChildren(h("p", { className: "evidence-warning" }, `Cannot load the logs: ${error.message}`)));
    const spansPromise = (async () => {
      if (!traceID) {
        spans.replaceChildren(h("p", { className: "evidence-warning" }, "This older record has no trace ID."));
        return;
      }
      const result = await api("GET", `/api/observability/spans?trace_id=${encodeURIComponent(traceID)}`);
      if (!result.spans?.length) {
        spans.replaceChildren(h("p", { className: "evidence-warning" }, "No spans were found for this trace."));
        return;
      }
      const byID = new Map(result.spans.map((span) => [span.span_id, span]));
      const depth = (span) => { let count = 0, parent = byID.get(span.parent_span_id); while (parent && count < 8) { count++; parent = byID.get(parent.parent_span_id); } return count; };
      fill(spans, result.partial || result.truncated ? h("p", { className: "evidence-warning" }, "Only part of the trace was returned.") : null,
        h("h4", {}, "Trace"), h("ul", { className: "evidence-span-list" }, ...result.spans.map((span) =>
          h("li", { className: span.span_id === event.span_id ? "selected" : "", style: `padding-left:${depth(span) * 16}px` },
            h("span", { className: "evidence-span-service" }, span.service || "service"), h("span", { className: "evidence-span-operation" }, span.operation || span.span_id),
            span.duration != null ? h("span", { className: "timeline-duration" }, formatDurationMs(Number(span.duration) / 1000)) : null))));
    })().catch((error) => spans.replaceChildren(h("p", { className: "evidence-warning" }, `Cannot load the trace: ${error.message}`)));
    await Promise.allSettled([logsPromise, spansPromise]);
  }

  function executionTimeline(records, context = {}) {
    const entries = [];
    let ordinal = 0;
    const evidence = h("div", { className: "execution-evidence", "aria-live": "polite", hidden: true });
    const displayStep = (step) => RunsLogic.stepLabel(step || "", titleCase); // names the pass of a step that runs in passes
    for (const { step, attempts } of records) {
      const attemptList = attempts || [];
      const showAttempts = attemptList.length > 1 || attemptList.some((record) => (record.attempt || 1) > 1);
      for (const record of attemptList) {
      const events = Array.isArray(record.events) ? [...record.events].sort((a, b) => String(a.started || "").localeCompare(String(b.started || ""))) : [];
      for (const event of events) {
        const interrupted = event.status === "running" && (record.status && record.status !== "running" || context.runStatus && context.runStatus !== "Running");
        const state = event.status === "completed" ? "done" : event.status === "running" && !interrupted ? "running" : event.status === "canceled" ? "idle" : "failed";
        const label = event.kind === "model_call" ? "Model call"
          : event.kind === "tool_discovery" ? `Tool discovery${event.service ? ` · ${event.service}` : ""}`
            : event.kind === "tool_call" ? `Tool call · ${event.name || "unknown"}` : titleCase(event.name || event.kind || "Event");
        const button = h("button", { type: "button", className: "timeline-select", "aria-selected": "false", title: event.error || undefined }, statusChip(state),
          h("span", { className: "timeline-step" }, displayStep(step)),
          h("span", { className: "timeline-event-group" },
            showAttempts ? h("span", { className: "timeline-attempt" }, `Attempt ${record.attempt || 1}`) : null,
            h("span", { className: "timeline-event" }, interrupted ? `${label} · Interrupted` : label)));
        button.onclick = () => showExecutionEvidence(evidence, button, record, event, step, context);
        entries.push({ at: event.started || record.started || "", ordinal: ordinal++, node: h("li", {}, button) });
      }
      const recordInterrupted = record.status === "running" && context.runStatus && context.runStatus !== "Running";
      const lastFailed = [...events].reverse().find((event) => event.status === "failed" || event.status === "canceled");
      const terminalError = record.error || (recordInterrupted ? context.runError : "");
      const sameFailure = lastFailed && (!terminalError || lastFailed.error
        && (terminalError.includes(lastFailed.error) || lastFailed.error.includes(terminalError)));
      if (!events.length || recordInterrupted || ((record.status === "failed" || record.status === "canceled") && !sameFailure)) {
        const state = record.status === "failed" || recordInterrupted ? "failed" : record.status === "running" ? "running" : record.status === "completed" ? "done" : "idle";
        const terminal = recordInterrupted ? "Attempt interrupted" : record.status === "failed" ? "Attempt failed" : record.status === "canceled" ? "Attempt canceled" : record.status === "completed" ? "Attempt completed" : "Attempt running";
        const lastEvent = events.at(-1);
        const terminalAt = record.finished || lastEvent?.finished || lastEvent?.started || record.started || "";
        const terminalEvent = { kind: "agent_execution", name: terminal, status: recordInterrupted ? "failed" : record.status, started: record.started, finished: record.finished,
          error: terminalError, error_category: terminalError ? "execution" : "", trace_id: record.trace_id, span_id: record.span_id };
        const button = h("button", { type: "button", className: "timeline-select", "aria-selected": "false", title: terminalError || undefined }, statusChip(state),
          h("span", { className: "timeline-step" }, displayStep(step)),
          h("span", { className: "timeline-event-group" }, showAttempts ? h("span", { className: "timeline-attempt" }, `Attempt ${record.attempt || 1}`) : null,
            h("span", { className: "timeline-event" }, terminal)));
        button.onclick = () => showExecutionEvidence(evidence, button, record, terminalEvent, step, context);
        entries.push({ at: terminalAt, ordinal: ordinal++, node: h("li", {}, button) });
      }
      }
    }
    entries.sort((a, b) => String(a.at).localeCompare(String(b.at)) || a.ordinal - b.ordinal);
    const rows = entries.map((entry) => entry.node);
    return rows.length ? h("div", { className: "section run-timeline" },
      h("h3", { className: "section-title" }, "Execution"), h("ul", {}, ...rows), evidence) : null;
  }

  let observabilityHealth = null, observabilityHealthAt = 0;
  async function telemetryWarning() {
    if (!observabilityHealth || Date.now() - observabilityHealthAt > 30000) {
      try { observabilityHealth = await api("GET", "/api/observability/health"); }
      catch (error) { observabilityHealth = { status: "unavailable", components: {}, reason: error.message }; }
      observabilityHealthAt = Date.now();
    }
    if (observabilityHealth.status === "ok") return null;
    const reasons = Object.values(observabilityHealth.components || {}).filter((item) => item.status !== "ok").map((item) => item.reason).filter(Boolean);
    return h("p", { className: "telemetry-warning" }, `Telemetry ${observabilityHealth.status}: ${reasons.join("; ") || observabilityHealth.reason || "Some evidence may be unavailable"}`);
  }

  function formatDurationMs(ms) {
    const value = Math.max(0, Number(ms) || 0);
    return value < 1000 ? `${Math.round(value)} ms` : value < 60000 ? `${(value / 1000).toFixed(value < 10000 ? 1 : 0)} s` : `${Math.floor(value / 60000)} min ${Math.round((value % 60000) / 1000)} s`;
  }

  async function renderDetail() {
    const trace = state.selected;
    if (!trace) {
      const first = visibleRuns()[0];
      if (first) { state.selected = first.trace_id; history.replaceState(null, "", hashFor(first.trace_id)); renderTable(); return renderDetail(); }
      detail.replaceChildren();
      return;
    }
    const r = runIndex.records.get(trace);
    const st = await runIndex.status(trace);
    if (state.selected !== trace) return;
    // A removed run (no record, history, or artifacts left) hands the selection to the newest one.
    if (!r && st.gone && !Object.keys(st.artifacts || {}).length) { state.selected = ""; return renderDetail(); }
    // The steps are read from the versions the run used: its own and its nested workflows'.
    const metadata = await runMetadata(trace, st);
    const version = runVersion(r, metadata);
    const root = WorkflowRef.format(r?.workflow || st.workflow || "", version);
    const defs = await runDefs(await definitions(), [WorkflowRef.parse(root), ...(Array.isArray(metadata?.workflows) ? metadata.workflows : [])]);
    if (state.selected !== trace) return;
    const def = TeamsLogic.defOf(defs, root);
    const artifacts = st.artifacts || {};
    // A failed run names the step it failed at; a merge that timed out or ran full names itself.
    // A blocked run did not fail: its error is the reason the agent gave.
    const failedStep = st.status && !["Running", "Completed", "Blocked"].includes(st.status) ? /(?:step|merge|batch) "([^"]+)"/.exec(st.error || "")?.[1] : "";
    // The steps that ran, in order; then the failed one; while running, the rest of the definition.
    const stepIds = [...(st.order || [])];
    if (failedStep && !stepIds.includes(failedStep)) stepIds.push(failedStep);
    for (const key of st.records || []) { const id = stepPass(key).step; if (!id.includes("/") && !stepIds.includes(id)) stepIds.push(id); }
    if (st.status === "Running") for (const s of def?.steps || []) if (!stepIds.includes(s.id)) stepIds.push(s.id);
    // Under a workflow node come the steps of its nested workflow, "<node>/<step>", as rows of
    // their own; the earlier decision records of a gate (<gate>.<n>) are not steps.
    const nestedArtifacts = st.nestedArtifacts || {};
    const observed = [...Object.keys(nestedArtifacts), ...[...(st.records || [])].map((key) => stepPass(key).step).filter((id) => id.includes("/"))];
    const rows = TeamsLogic.stepRows(stepIds, observed, defs, root, st.status === "Running");
    const definedStep = (id) => TeamsLogic.stepAt(defs, root, id);
    const artifactOf = (id) => artifacts[id] || nestedArtifacts[id];
    const teamOf = (id) => TeamsLogic.teamPath(trace, id);
    const failedNested = new Set(failedStep ? TeamsLogic.failedPaths(st.error) : []);
    const failedAt = (id) => (id.includes("/") ? failedNested.has(id) : id === failedStep);
    // A gate waits at the step whose output it will write; a nested session at its own path.
    // A gate that runs in passes waits at the output of its pass, <step>.pass-<n>.
    // A publish decision (<step>.publish-<n>) waits at the step whose agent asked to publish.
    const waitingGateOf = (id) => decisionsState.pending.find((d) => d.trace_id === trace && (publishStep(d.step) === id || (d.artifact ? stepPass(d.artifact).step === `runs/${trace}/${id}` : d.step === id)));
    const waitingPublish = (id) => decisionsState.pending.some((d) => d.trace_id === trace && publishStep(d.step) === id);
    // A merge or batch step shows what it holds; the steps after it run once per message it
    // forwards, and show one entry per pass.
    const pathOf = (id, other) => id.slice(0, id.lastIndexOf("/") + 1) + other; // a step beside id, in the same workflow
    const defAround = (id) => (id.includes("/") ? TeamsLogic.defOf(defs, definedStep(id.slice(0, id.lastIndexOf("/")))?.workflow || "") : def);
    const nodeOf = (id) => { const s = definedStep(id); return RunsLogic.isNode(s) ? RunsLogic.nodeView(st, id, s) : null; };
    const passRowsOf = (id) => {
      const s = definedStep(id);
      if (RunsLogic.isNode(s)) return [];
      const failedHere = failedAt(id);
      const source = s ? RunsLogic.passSource(defAround(id), id.split("/").pop()) : "";
      const stored = st.passes?.[id] || [];
      if (!source) return RunsLogic.passRows(stored, 0, st.status, true, failedHere); // a step whose definition is not known still lists its stored passes
      const next = (stored.length ? stored[stored.length - 1].pass : 0) + 1;
      // The next pass can run once the steps it depends on have finished theirs.
      const ready = (s.depends_on || []).every((d) => RunsLogic.isNode(definedStep(pathOf(id, d))) || (st.passes?.[pathOf(id, d)] || []).some((p) => p.pass === next && p.artifact));
      return RunsLogic.passRows(stored, RunsLogic.forwardedBy(RunsLogic.nodeView(st, pathOf(id, source), definedStep(pathOf(id, source)))), st.status, ready, failedHere);
    };
    // passEntries adds to each pass what waits or runs under its own name: the gate, session and
    // team of pass n of a step are stored as <step>.pass-<n>.
    const passEntries = (id) => passRowsOf(id).map((p) => {
      const key = `${id}.pass-${p.pass}`;
      const gate = decisionsState.pending.find((d) => d.trace_id === trace && d.artifact === `runs/${trace}/${key}`);
      const session = `user-sessions/${trace}/${key}`, team = `teams/${trace}/${key}`;
      const sessionWaits = sessionsState.pending.some((x) => x.session === session);
      const live = st.status === "Running";
      const state = live && (gate || sessionWaits) ? "waiting" : live && teamRunning(team) ? "running" : p.state;
      return { ...p, state, gate, session, sessionWaits, team };
    });
    const nestedSession = (id) => `user-sessions/${trace}/${id}`;
    // A waiting session belongs to the top-level step in its path: the interactive step itself,
    // or the route step whose child run holds it.
    const waitingSession = (id) => waitingSessionOf(sessionsState.pending, trace, id);
    const stepState = (id) => {
      const child = id.includes("/"), parent = id.slice(0, id.lastIndexOf("/") + 1);
      if (st.status === "Running" && teamRunning(teamOf(id))) return "running"; // also in a later round, when its earlier result exists
      if (st.status === "Running" && waitingPublish(id)) return "waiting"; // its result exists; a person decides whether it is published
      if (RunsLogic.isNode(definedStep(id))) return (child ? failedNested.has(id) : id === failedStep) ? "failed" : RunsLogic.nodeState(nodeOf(id), st.status);
      const passStates = passEntries(id).map((p) => p.state); // of a later pass too, when its earlier output exists
      for (const found of ["waiting", "running", "failed"]) if (passStates.includes(found)) return found;
      if (failedAt(id)) return "failed"; // also when an earlier round of it left a result
      if (artifactOf(id)) return "done";
      if (st.status === "Completed") return "done";
      if (st.status === "Running") {
        if (waitingGateOf(id)) return "waiting";
        if (waitingSession(id)) return "waiting";
        if (child && sessionsState.pending.some((p) => p.session === nestedSession(id))) return "waiting";
        if (child ? observed.some((p) => p.startsWith(`${id}/`)) : st.nested?.has(id)) return "running";
        const s = definedStep(id);
        // A nested step can run only while its workflow node does.
        if (s && (s.depends_on || []).every((d) => artifactOf(parent + d)) && (!child || ["running", "waiting"].includes(stepState(parent.slice(0, -1))))) return "running";
      }
      return "idle";
    };
    const final = st.status !== "Running";
    const [agents, records, known, telemetry, sessions, docs, live, pullText] = await Promise.all([
      Promise.all(rows.map(({ id }) => agentOf(trace, st, def, id, id.includes("/") ? definedStep(id) : null))),
      Promise.all([...(st.records || [])].map(async (step) => ({ step, attempts: await executionRecords(trace, st, step) }))),
      existingAgents(),
      telemetryWarning(),
      sessionFolders(trace, st, def),
      // A dev pod, command or stop step shows its output document under its name.
      Promise.all(rows.map(async ({ id }) => {
        const service = definedStep(id)?.service;
        return ["devpod", "command", "devpod-stop"].includes(service) && artifactOf(id) ? RunsLogic.stepDocument(service, await stored(artifactOf(id), final)) : null;
      })),
      st.devPod ? livePods() : null,
      st.pullRequest && validBlobID(st.pullRequest) ? stored(st.pullRequest, final) : "",
    ]);
    if (state.selected !== trace) return;
    const pending = decisionsState.pending.filter((d) => d.trace_id === trace);
    // The output of each pass and of each kept round, by its record key ("compare.pass-2",
    // "build.round-1"), for the execution evidence.
    const passArtifacts = Object.fromEntries([...Object.values(st.passes || {}), ...Object.values(st.rounds || {})].flat().filter((p) => p.record && p.artifact).map((p) => [p.record, p.artifact]));
    const now = Date.now();
    const gates = new Set((def?.steps || []).filter((s) => s.service === "gate").map((s) => s.id));
    // A run that failed while none of its steps did (a run timeout, a termination) says so in a
    // last row of its own, so the error never stands above nothing but completed steps.
    const failedRun = st.status && !["Running", "Completed", "Blocked"].includes(st.status) && !st.gone;
    const lastDone = rows.filter(({ id, depth }) => !depth && stepState(id) === "done").at(-1);
    const runEnd = failedRun && !rows.some(({ id }) => stepState(id) === "failed")
      ? h("li", { className: "no-agent no-link step-end" }, statusChip("failed"), h("span", { className: "step-name" }, RunsLogic.runEndLine(st.status, lastDone ? titleCase(lastDone.id) : "")))
      : null;
    detail.replaceChildren(
      h("div", { className: "panel-head" }),
      h("div", { className: "panel-body" },
        h("div", { className: "section" },
          h("div", { className: "run-meta" },
            runChip(trace, st),
            r ? [h("span", { className: "sep" }, "·"), timeEl(r.started)] : null,
            version ? [h("span", { className: "sep" }, "·"), h("span", { className: "run-version" }, `Workflow v${version}`)] : null,
            h("span", { className: "sep" }, "·"), h("span", { className: "meta-pair" }, h("a", { href: `#artifacts?trace=${encodeURIComponent(trace)}` }, "View trace"), copyButton(trace, "trace ID")))),
        // A blocked run says why in the agent's words, as text: nothing failed.
        st.status === "Blocked" ? (st.blocked ? h("div", { className: "section" }, h("p", { className: "blocked-line" }, st.blocked)) : null)
          : st.error && st.status !== "Running" && !st.gone ? h("div", { className: "section" },
            h("p", { className: "error-line", title: st.error }, rootError(st.error, trace))) : null,
        podSection(st.devPod, live),
        runSkillVersions(metadata),
        pullRequestSection(pullText ? RunsLogic.pullRequest(pullText) : null, st.pullRequest),
        telemetry,
        executionTimeline(records, { trace, runStatus: st.status, runError: st.error, inputArtifact: r?.input_artifact, artifacts, passArtifacts, retentionDays: observabilityHealth?.retention_days || 0 }),
        h("div", { className: "section" },
          h("h3", { className: "section-title" }, "Steps"),
          h("ul", { className: "step-list" }, ...rows.flatMap(({ id, depth }, i) => {
            // A gate links to its decision; other steps link to their output.
            const stepLink = (step) => {
              if (passes.length) return null; // every pass links to its own decision, session, team or output below
              const child = step.includes("/"), service = definedStep(step)?.service || "", output = artifactOf(step);
              const waitingGate = waitingGateOf(step);
              if (waitingGate) return h("a", { className: "step-output", href: `#decisions/${encodeURIComponent(`p:${waitingGate.workflow_id}|${waitingGate.step}`)}` }, "View decision");
              // A team step links to its team: its channels and workspace, while it runs and afterwards.
              if (teamRunning(teamOf(step)) || (service === "team" && stepState(step) !== "idle")) return h("a", { className: "step-output step-action", href: TeamsLogic.teamHash(teamOf(step)), title: "Opens the messages between the team's agents and the files they wrote" }, "View messages");
              // An interactive step links to its session: open while it waits, read-only afterwards.
              const nested = child && (sessionsState.pending.some((p) => p.session === nestedSession(step)) || (service === "interactive" && output)) && ChatLogic.validSession(nestedSession(step));
              const session = child ? (nested ? nestedSession(step) : "") : stepSession(sessionsState.pending, sessions, trace, step);
              if (session) return h("a", { className: "step-output", href: ChatLogic.sessionHash(session) }, "View session");
              return failedAt(step) ? null : outputLink(step, output); // a failed step's result is an earlier round's
            };
            // outputLink leads to a gate's decision or to any other step's output.
            const outputLink = (step, output) => {
              if (!output) return null;
              if (step.includes("/") ? definedStep(step)?.service === "gate" : gates.has(step)) return h("a", { className: "step-output", href: `#decisions/${encodeURIComponent(`d:${output}`)}` }, "View decision");
              return h("a", { className: "step-output", href: `#artifacts?trace=${encodeURIComponent(trace)}&object=${encodeURIComponent(output)}` }, "View output");
            };
            const passes = passEntries(id);
            // passLink leads to what pass p of the step left or waits with.
            const passLink = (p) => {
              const service = definedStep(id)?.service || "";
              if (p.gate) return h("a", { className: "step-output", href: `#decisions/${encodeURIComponent(`p:${p.gate.workflow_id}|${p.gate.step}`)}` }, "View decision");
              if (service === "team" && (teamRunning(p.team) || p.state !== "idle")) return h("a", { className: "step-output step-action", href: TeamsLogic.teamHash(p.team), title: "Opens the messages between the team's agents and the files they wrote" }, "View messages");
              if ((p.sessionWaits || (service === "interactive" && p.artifact)) && ChatLogic.validSession(p.session)) return h("a", { className: "step-output", href: ChatLogic.sessionHash(p.session) }, "View session");
              return outputLink(id, p.artifact);
            };
            const notes = RunsLogic.nodeLines(nodeOf(id), now, titleCase);
            const ref = agents[i];
            const [name, version] = ref.split("@");
            // A step that ran again after a rejection has one entry per round under it; the
            // newest round carries the step's own link.
            const rounds = passes.length ? [] : RunsLogic.roundRows(st.rounds?.[id], st.roundCounts?.[id], failedAt(id) ? artifactOf(id) || "" : "");
            const ownLink = stepLink(id);
            const link = rounds.length ? null : ownLink;
            // A step without an agent or a link gives its name those columns (see .step-list).
            // A nested step is indented under its workflow node and named by its own ID.
            const row = h("li", { className: [ref ? "" : "no-agent", link ? "" : "no-link", depth ? "step-child" : ""].join(" ").trim(), style: depth ? `--step-depth:${depth}` : undefined }, statusChip(stepState(id)),
              h("span", { className: "step-name", dataset: { full: id.split("/").map(titleCase).join(" / ") } }, titleCase(id.split("/").pop())), // a cut-off name shows in full on hover
              // An agent moved or deleted since the run shows as text, not as a link to another agent.
              !ref ? null : known.has(ref) ? h("a", { className: "step-agent", href: `#agents/${name}/${version}`, title: "Opens the agent in Agents" }, ref) : h("span", { className: "step-agent" }, ref),
              link,
              // A merge or batch step says what it holds, under its name.
              notes.length ? h("span", { className: "step-notes" }, ...notes.map((line) => h("span", { className: "step-note" }, line))) : null,
              stepDoc(docs[i], `${trace}/${id}`));
            // A step after a merge or batch has one entry per pass, indented under it.
            return [row, ...passes.map((p) => {
              const link = passLink(p);
              return h("li", { className: `no-agent step-child step-pass${link ? "" : " no-link"}`, style: `--step-depth:${depth + 1}` }, statusChip(p.state),
                h("span", { className: "step-name" }, `Pass ${p.pass}`), link);
            }), ...rounds.map((r) => {
              // An earlier round links to what it left, kept as <step>.round-<n>.
              const link = r.current ? ownLink
                : r.artifact ? h("a", { className: "step-output", href: `#artifacts?trace=${encodeURIComponent(trace)}&object=${encodeURIComponent(r.artifact)}` }, "View output") : null;
              return h("li", { className: `no-agent step-child step-pass step-round${link ? "" : " no-link"}`, style: `--step-depth:${depth + 1}` }, statusChip(r.current ? stepState(id) : "done"),
                h("span", { className: "step-name" }, `Round ${r.round}`), link);
            })];
          }), runEnd)))));
  }

  // refresh loads new runs and the statuses of the visible ones; running runs are
  // asked again on every call (about every 3 seconds while the page is open).
  async function refresh(full = false) {
    try {
      if (full || !runIndex.records.size) await runIndex.refresh();
      state.error = "";
    } catch (e) {
      state.error = `Cannot list runs: ${e.message}`;
    }
    renderFilters();
    renderTable();
    const need = runIndex.list(state.workflow).slice(0, MAX_ROWS)
      .filter((r) => { const st = runIndex.statuses.get(r.trace_id); return !st || st.status === "Running"; });
    // A row needs the status alone, never the slow listing of the run's objects; the running teams are for View messages.
    await Promise.all([pool(need, 6, (r) => runIndex.status(r.trace_id, 2500, "")), refreshTeams()]);
    renderTable();
    renderDetail();
  }

  let ticks = 0;
  function startPolling() {
    clearInterval(state.timer);
    state.timer = setInterval(() => {
      if (document.hidden || !menu.hidden) return; // a redraw would drop the row an open menu outlines
      ticks++;
      refresh(ticks % 4 === 0); // new runs every 12 s, running statuses every 3 s
    }, 3000);
  }

  document.addEventListener("decisionschange", () => { if (activeTab === "runs") { renderTable(); if (state.selected) renderDetail(); } });
  document.addEventListener("sessionschange", () => { if (activeTab === "runs") { renderTable(); if (state.selected) renderDetail(); } });

  tabs.runs = {
    show(rest) {
      const [path, query = ""] = rest.split("?");
      const params = new URLSearchParams(query);
      const first = !state.timer;
      state.workflow = params.get("workflow") || "";
      state.status = params.get("status") || "";
      state.selected = path.replace(/^\/+/, "");
      renderFilters();
      renderTable();
      renderDetail();
      if (first) { state.definitions = null; refresh(true); startPolling(); }
    },
    hide() {
      clearInterval(state.timer);
      state.timer = null;
    },
  };
})();
