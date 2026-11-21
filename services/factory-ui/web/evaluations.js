"use strict";
// Evaluation sets are immutable run snapshots grouped for later comparisons. This page only
// curates datasets; scoring and executing evaluations belong to later evaluation-service work.
(() => {
  const PAGE_SIZE = 50;
  const state = { sets: [], active: "", set: null, runs: [], selected: new Set(), cursor: "", next: "", pages: [""], incomplete: 0, historyOmitted: 0, runRequest: 0 };
  const list = $("evaluation-sets"), results = $("evaluation-runs"), detail = $("evaluation-detail");
  const form = $("evaluation-filters"), actions = $("evaluation-actions"), title = $("evaluation-detail-title");

  const field = (label, input) => h("div", { className: "field" }, h("label", { htmlFor: input.id }, label), input);
  const workflow = h("select", { id: "evaluation-workflow" }, h("option", { value: "" }, "All"));
  const agent = h("select", { id: "evaluation-agent" }, h("option", { value: "" }, "All"));
  const setOptions = (select, values) => {
    const current = select.value;
    const options = [...new Set([...values, ...(current ? [current] : [])])].sort();
    select.replaceChildren(h("option", { value: "", selected: !current }, "All"), ...options.map((value) => h("option", { value, selected: value === current }, value)));
    select.value = current;
  };
  const status = h("select", { id: "evaluation-status" },
    ...[["", "All"], ["Completed", "Completed"], ["Failed", "Failed"], ["TimedOut", "Timed out"], ["Canceled", "Canceled"], ["Terminated", "Terminated"], ["Running", "Running"]].map(([value, label]) => h("option", { value }, label)));
  const startedFrom = h("input", { id: "evaluation-started-from", type: "datetime-local" });
  const startedTo = h("input", { id: "evaluation-started-to", type: "datetime-local" });
  const durationMin = h("input", { id: "evaluation-duration-min", "aria-label": "Runtime From in seconds", type: "text", inputMode: "decimal", pattern: "[0-9]+([.][0-9]+)?", placeholder: "0", title: "End-to-end runtime in seconds" });
  const durationMax = h("input", { id: "evaluation-duration-max", "aria-label": "Runtime To in seconds", type: "text", inputMode: "decimal", pattern: "[0-9]+([.][0-9]+)?", placeholder: "0", title: "End-to-end runtime in seconds" });

  const iso = (value) => value ? new Date(value).toISOString() : "";
  const params = (cursor = "") => {
    const p = new URLSearchParams({ limit: String(PAGE_SIZE) });
    for (const [key, value] of [["workflow", workflow.value.trim()], ["agent", agent.value.trim()], ["status", status.value],
      ["started_from", iso(startedFrom.value)], ["started_to", iso(startedTo.value)]]) if (value) p.set(key, value);
    if (durationMin.value !== "") p.set("duration_min_ms", String(Math.round(Number(durationMin.value) * 1000)));
    if (durationMax.value !== "") p.set("duration_max_ms", String(Math.round(Number(durationMax.value) * 1000)));
    if (cursor) p.set("cursor", cursor);
    return p;
  };

  const duration = (ms) => ms == null ? "—" : ms < 1000 ? `${ms} ms` : ms < 60000 ? `${(ms / 1000).toFixed(ms < 10000 ? 1 : 0)} s` : `${Math.floor(ms / 60000)} min ${Math.round(ms % 60000 / 1000)} s`;
  const stateName = (run) => !run.status || run.status === "Unknown" ? "idle" : runState(run.status, false);
  const runLabel = (run) => `${titleCase(run.workflow || "Run")} · ${midTruncate(run.trace_id, 13)}`;

  async function loadRuns(cursor = "", resetPages = false) {
    const request = ++state.runRequest;
    results.replaceChildren(emptyPanel("Loading runs…"));
    try {
      const data = await api("GET", `/api/evaluations/runs?${params(cursor)}`);
      if (request !== state.runRequest) return;
      state.runs = data.runs || [];
      state.incomplete = data.incomplete_runs || 0;
      state.historyOmitted = data.history_unavailable_omitted || 0;
      state.cursor = cursor;
      state.next = data.next_cursor || "";
      if (resetPages) state.pages = [""];
      renderRuns();
    } catch (error) { if (request === state.runRequest) results.replaceChildren(errorPanel(`Cannot list runs: ${error.message}`)); }
  }

  function renderRuns() {
    const warning = () => {
      const messages = [];
      if (state.incomplete) messages.push(`${state.incomplete} run${state.incomplete === 1 ? "" : "s"} could not be fully indexed and may not match these filters.`);
      if (state.historyOmitted) messages.push(`${state.historyOmitted} run${state.historyOmitted === 1 ? " has" : "s have"} no workflow history, so status or runtime could not be evaluated.`);
      return messages.length ? h("p", { className: "evidence-warning evaluation-index-warning" }, messages.join(" ")) : null;
    };
    if (!state.runs.length) { fill(results, warning(), emptyPanel("No matching runs"), pager()); return; }
    const rows = state.runs.map((run) => {
      const failed = stateName(run) === "failed";
      const check = h("input", { type: "checkbox", checked: state.selected.has(run.trace_id), "aria-label": `Select ${runLabel(run)}` });
      check.onchange = () => { if (check.checked) state.selected.add(run.trace_id); else state.selected.delete(run.trace_id); renderActions(); };
      return h("tr", { className: failed ? "run-failed" : "" }, h("td", { className: "evaluation-select-cell" }, check),
        h("td", {}, statusChip(stateName(run), run.history_unavailable ? "History unavailable" : runStateWord(run.status), run.history_unavailable ? "Temporal history is no longer available; artifacts and agent records remain" : undefined)),
        h("td", { className: "name-cell", dataset: { full: run.workflow || "" } }, titleCase(run.workflow || "")),
        h("td", { className: "opt", dataset: { full: (run.agents || []).join(", ") }, title: run.agents_complete === false ? "Agent index is incomplete" : undefined }, (run.agents || []).join(", ") || "—"),
        h("td", { className: "fit" }, timeEl(run.started)), h("td", { className: "num", title: run.duration_known === false ? "End-to-end runtime is unavailable" : undefined }, duration(run.duration_known === false ? null : run.duration_ms)),
        h("td", { className: "fit" }, h("a", { href: `#runs/${run.trace_id}` }, "Open")));
    });
    fill(results, warning(),
      dataTable(["", "Status", "Workflow", "Agents", "Started", "Runtime", ""], rows, { numeric: [5], optional: [3], name: 2 }), pager());
  }

  function pager() {
    const page = state.pages.indexOf(state.cursor);
    return h("div", { className: "evaluation-pager" },
      h("button", { type: "button", disabled: page <= 0, onclick: () => loadRuns(state.pages[page - 1]) }, "Previous"),
      h("span", {}, `Page ${Math.max(1, page + 1)}`),
      h("button", { type: "button", disabled: !state.next, onclick: () => { if (!state.pages.includes(state.next)) state.pages.push(state.next); loadRuns(state.next); } }, "Next"));
  }

  function renderActions() {
    const count = state.selected.size;
    actions.replaceChildren(
      h("button", { type: "button", onclick: () => { for (const run of state.runs) state.selected.add(run.trace_id); renderRuns(); renderActions(); } }, "Select page"),
      h("button", { type: "button", disabled: !count, onclick: () => { state.selected.clear(); renderRuns(); renderActions(); } }, "Clear selection"),
      h("button", { type: "button", className: "primary", disabled: !count, onclick: newSet }, `Create set${count ? ` (${count})` : ""}`));
  }

  async function loadSets(select = state.active) {
    try { state.sets = (await api("GET", "/api/evaluations/sets")).sets || []; }
    catch (error) { list.replaceChildren(h("li", { className: "error-line" }, `Cannot list sets: ${error.message}`)); return; }
    list.replaceChildren(...state.sets.map((set) => {
      const row = h("li", { role: "option", tabIndex: 0, "aria-selected": String(set.id === select), title: set.description || set.name },
        h("span", { className: "row-title" }, set.name), h("span", { className: "tree-sub" }, String(set.run_count)));
      const open = () => openSet(set.id);
      row.onclick = open;
      row.onkeydown = (event) => { if (event.key === "Enter" || event.key === " ") { event.preventDefault(); open(); } };
      return row;
    }));
    if (!state.sets.length) list.replaceChildren(h("li", { className: "evaluation-summary" }, "No evaluation sets yet"));
  }

  async function loadAgentOptions() {
    try {
      const data = await api("GET", "/api/agents/agents");
      const refs = (data.entries || []).flatMap((entry) => (entry.versions || []).map((version) => `${entry.name}@${version.version || version}`)).sort();
      setOptions(agent, refs);
    } catch { /* retain the last available choices */ }
  }

  async function loadWorkflowOptions() {
    try {
      const data = await api("GET", "/api/orchestrator/workflows");
      setOptions(workflow, (data.workflows || []).map((item) => item.name));
    } catch { /* retain the last available choices */ }
  }

  async function openSet(id) {
    state.active = id;
    await loadSets(id);
    try { state.set = await api("GET", `/api/evaluations/sets/${encodeURIComponent(id)}`); renderSet(); }
    catch (error) { detail.replaceChildren(errorPanel(`Cannot load evaluation set: ${error.message}`)); }
  }

  function renderSet() {
    const set = state.set;
    title.textContent = set?.name || "";
    if (!set) { detail.replaceChildren(); return; }
    const runs = set.runs || [];
    fill(detail, set.description ? h("p", { className: "evaluation-summary" }, set.description) : null,
      h("p", { className: "evaluation-summary" }, `${runs.length} run${runs.length === 1 ? "" : "s"} · saved ${formatTime(set.updated)}`),
      h("p", { className: "evaluation-summary" }, h("a", {
        className: "step-action", href: `/api/evaluations/sets/${encodeURIComponent(set.id)}/export`, download: "",
        title: "Downloads the set as a zip file: every run with its input, step outputs, agent records, sessions and teams, and a manifest that says what each file is",
      }, "Download zip")),
      ...runs.map((run) => h("div", { className: "evaluation-member" },
        h("a", { href: `#runs/${run.trace_id}`, title: run.source_present === false ? "The source run was removed; this saved snapshot remains" : "Open run" }, runLabel(run)),
        h("button", { type: "button", className: "danger", title: "Remove from set", onclick: () => updateSet(runs.filter((item) => item.trace_id !== run.trace_id).map((item) => item.trace_id)) }, "Remove"))),
      runs.some((run) => run.source_present === false) ? h("p", { className: "evidence-warning" }, "Some source runs were removed. Their saved snapshots remain in this set.") : null);
  }

  async function updateSet(runIDs) {
    try {
      state.set = await api("PUT", `/api/evaluations/sets/${encodeURIComponent(state.set.id)}`, {
        name: state.set.name, description: state.set.description || "", run_ids: runIDs, expected_revision: state.set.revision,
      }, creatorHeaders());
      setSaveState("Saved evaluation set"); await loadSets(state.set.id); renderSet();
    } catch (error) { setStatus(`Not saved: ${error.message}`, "error"); }
  }

  function newSet() {
    const name = h("input", { id: "evaluation-set-name", autocomplete: "off", autofocus: true });
    const description = h("textarea", { id: "evaluation-set-description", rows: 3 });
    const problem = h("p", { className: "error-text", role: "alert" });
    const create = h("button", { type: "button", className: "primary" }, "Create set");
    const cancel = h("button", { type: "button" }, "Cancel");
    const dlg = openDialog({ title: "Create evaluation set", body: h("div", {}, field("Name", name), field("Description", description), problem), footer: [cancel, create] });
    cancel.onclick = () => dlg.close();
    create.onclick = async () => {
      if (!name.value.trim()) { problem.textContent = "Enter a name."; name.focus(); return; }
      create.disabled = true;
      try {
        const set = await api("POST", "/api/evaluations/sets", { name: name.value.trim(), description: description.value.trim(), run_ids: [...state.selected] }, creatorHeaders());
        state.selected.clear(); dlg.close(); setSaveState("Created evaluation set"); await loadSets(set.id); await openSet(set.id); renderRuns(); renderActions();
      } catch (error) { problem.textContent = error.message; create.disabled = false; }
    };
  }

  form.replaceChildren(field("Workflow", workflow), field("Agent", agent), field("Status", status), field("Started after", startedFrom), field("Started before", startedTo),
    h("div", { className: "field evaluation-runtime", role: "group", "aria-labelledby": "evaluation-runtime-label" }, h("label", { id: "evaluation-runtime-label", htmlFor: durationMin.id }, "Runtime From/To"), h("div", { className: "evaluation-runtime-inputs" }, h("div", { className: "runtime-input" }, durationMin, h("span", { "aria-hidden": "true" }, "s")), h("div", { className: "runtime-input" }, durationMax, h("span", { "aria-hidden": "true" }, "s")))), h("button", { type: "submit", className: "primary" }, "Apply"));
  form.onsubmit = (event) => { event.preventDefault(); loadRuns("", true); };
  $("evaluation-new-slot").append(iconButton("plus", "Create evaluation set", newSet));

  tabs.evaluations = {
    show(rest) { const id = decodeURIComponent(rest.replace(/^\/+/, "")); loadAgentOptions(); loadWorkflowOptions(); loadSets(id); if (id) openSet(id); else { state.set = null; title.textContent = ""; renderSet(); } loadRuns("", true); renderActions(); },
    hide() {},
  };
})();
