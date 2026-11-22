"use strict";
// Intake page: event triggers. A trigger starts one workflow version whenever a run of one of
// its source workflows ends with one of its outcomes. Hash: #intake. The sidebar lists the
// triggers, the content panel edits the selected one. Everything goes through
// /api/connectors/intake/..., the factory UI's proxy to the connector service.

// ---- Logic without the DOM (covered by intake_ui_test.mjs) ----

const IntakeLogic = (() => {
  const OUTCOMES = [["completed", "Completed"], ["failed", "Failed"], ["cancelled", "Cancelled"], ["blocked", "Blocked"], ["terminated", "Terminated"], ["timed_out", "Timed out"]];
  // The input connectors a trigger can listen to. Only the factory's own event feed exists so
  // far; the others are listed so that the choice is visible, and cannot be chosen yet.
  const SOURCES = [
    { id: "factory", name: "Factory events", tip: "Runs of the factory's workflows, as they end", available: true },
    { id: "slack", name: "Slack", tip: "Not available yet", available: false },
    { id: "linear", name: "Linear", tip: "Not available yet", available: false },
    { id: "jira", name: "Jira", tip: "Not available yet", available: false },
  ];
  const sourceName = (id) => SOURCES.find((s) => s.id === id)?.name || String(id || "");
  const list = (value) => (Array.isArray(value) ? value.filter((x) => typeof x === "string" && x) : []);

  // triggers reads GET /api/connectors/intake/bindings: an array (or { bindings: [...] }) of
  //   { id, name, source, workflow, workflow_version, source_workflows, outcomes, enabled, revision }
  // Sorted by name.
  function triggers(answer) {
    const rows = Array.isArray(answer) ? answer : answer?.bindings || answer?.items || [];
    return rows.filter((b) => b && b.id).sort((a, b) => String(a.name || "").localeCompare(String(b.name || "")));
  }

  // workflows reads GET /api/orchestrator/workflows into [{ name, versions: [newest first] }], by name.
  function workflows(answer) {
    const rows = Array.isArray(answer) ? answer : answer?.workflows || [];
    const byName = new Map();
    for (const w of rows) {
      if (!w?.name) continue;
      const versions = (w.versions?.length ? w.versions : [w.version]).map((v) => Number(v?.version ?? v)).filter((v) => v > 0);
      byName.set(w.name, [...new Set([...(byName.get(w.name) || []), ...versions])].sort((a, b) => b - a));
    }
    return [...byName].map(([name, versions]) => ({ name, versions })).sort((a, b) => a.name.localeCompare(b.name));
  }

  // draft is what the form edits: the fields of a trigger as plain values.
  const draft = (t = {}) => ({
    name: t.name || "", sources: list(t.source_workflows), outcomes: list(t.outcomes),
    workflow: t.workflow || "", version: Number(t.workflow_version) || 0, enabled: !!t.enabled,
  });
  const same = (a, b) => JSON.stringify(a) === JSON.stringify(b);

  // problems says what Save would be refused for, one sentence each; empty when fine.
  function problems(d) {
    const out = [];
    if (!d.name.trim()) out.push("Enter a name.");
    if (!d.sources.length) out.push("Choose a source workflow.");
    if (!d.outcomes.length) out.push("Choose an outcome.");
    if (!d.workflow || !d.version) out.push("Choose the workflow to start.");
    return out;
  }

  // body is what PUT /api/connectors/intake/bindings/<id> takes.
  const body = (t, d) => ({
    ...t, name: d.name.trim(), source: t.source || "factory", workflow: d.workflow, workflow_version: d.version,
    source_workflows: d.sources, outcomes: d.outcomes, enabled: d.enabled,
  });

  // chosen is the text of a multi-select: the one choice by its name, more as a count.
  function chosen(values, label, noun, none, options = []) {
    if (!values.length) return none;
    if (options.length && options.every((value) => values.includes(value))) return `All ${noun}`;
    return values.length === 1 ? label(values[0]) : `${values.length} ${noun}`;
  }

  // statusParts reads GET .../bindings/<id>/status ({ pending, last_error, last_accepted }) into
  // the words of the status line.
  function statusParts(status = {}) {
    const pending = Number(status.pending) || 0;
    const accepted = typeof status.last_accepted === "string" && !status.last_accepted.startsWith("0001") ? status.last_accepted : "";
    return { pending: pending ? `${pending} event${pending === 1 ? "" : "s"} waiting` : "", accepted, error: status.last_error || "" };
  }

  return { OUTCOMES, SOURCES, sourceName, triggers, workflows, draft, same, problems, body, chosen, statusParts };
})();

// ---- Intake page ----

(() => {
  const API = "/api/connectors/intake/bindings";
  const L = IntakeLogic;
  const enc = encodeURIComponent;
  const SELECTED_KEY = "factory.intake.selected";
  // current is the trigger being edited (a stored one, or a new one without a revision);
  // form is its draft and stored is what the draft started from.
  const state = { items: [], flows: [], current: null, form: null, stored: null, status: null, loaded: false, error: "", active: false, request: 0, saving: false, problems: [] };
  const rows = $("intake-list"), main = $("intake-main");

  const nameOf = (t) => t.name || "New trigger";
  const dirty = () => !!state.current && !L.same(state.form, state.stored);
  const outcomeWord = (value) => (L.OUTCOMES.find(([v]) => v === value) || [])[1] || value;
  const remember = (id) => { try { localStorage.setItem(SELECTED_KEY, id); } catch { /* storage unavailable */ } };
  const remembered = () => { try { return localStorage.getItem(SELECTED_KEY) || ""; } catch { return ""; } };

  // ---- Sidebar ----

  function renderList() {
    const listed = state.current && !state.current.revision ? [state.current, ...state.items] : state.items;
    const empty = $("intake-empty");
    if (state.error) { rows.replaceChildren(); fill(empty, errorPanel(state.error)); fill(main); return; }
    fill(empty, state.loaded && !listed.length ? emptyPanel("No event triggers yet") : null);
    rows.replaceChildren(...listed.map((t) => {
      const li = h("li", { role: "option", tabIndex: 0, "aria-selected": String(t.id === state.current?.id) }, h("div", { className: "row-title" }, nameOf(t)));
      li.onclick = () => select(t);
      li.onkeydown = (ev) => { if (ev.key === "Enter" || ev.key === " ") { ev.preventDefault(); select(t); } };
      li.oncontextmenu = (ev) => openMenu(ev, [
        ...(t.revision ? [["Delete", () => remove(t), "danger", `Removes ${nameOf(t)}; events that wait for it are dropped`], "-"] : []),
        ["New trigger", create, "", "Adds an event trigger"],
      ]);
      return li;
    }));
  }

  // ---- Content ----

  // picker is a compact multi-select like Policies on Agents: the trigger names the choice and
  // opens a list of tickboxes; Apply stores it.
  function picker(id, values, options, label, noun, none, apply) {
    const trigger = h("button", { type: "button", id, className: "multi-select-trigger", "aria-haspopup": "dialog" }, L.chosen(values, label, noun, none, options));
    if (values.length > 1) trigger.title = values.map(label).join("\n");
    trigger.onclick = () => {
      const selected = new Set(values);
      const all = [...new Set([...options, ...values])];
      const ok = h("button", { type: "button", className: "primary" }, "Apply"), cancel = h("button", { type: "button" }, "Cancel");
      const allCheck = h("input", { type: "checkbox", disabled: !all.length, "aria-label": `All ${noun}` });
      const checks = all.map((value) => {
        const check = h("input", { type: "checkbox", value, checked: selected.has(value) });
        check.addEventListener("change", () => {
          if (check.checked) selected.add(value); else selected.delete(value);
          syncAll();
        });
        return check;
      });
      const syncAll = () => {
        allCheck.checked = all.length > 0 && all.every((value) => selected.has(value));
        allCheck.indeterminate = selected.size > 0 && !allCheck.checked;
      };
      allCheck.addEventListener("change", () => {
        selected.clear();
        for (const check of checks) {
          check.checked = allCheck.checked;
          if (check.checked) selected.add(check.value);
        }
        syncAll();
      });
      syncAll();
      const popover = openPopover(trigger, h("div", {},
        h("div", { className: "agent-policy-popover-list" }, h("div", { className: "agent-tool-picker-list", role: "group" },
          h("label", { className: "agent-tool-picker-item" }, allCheck, h("span", { className: "agent-tool-picker-name" }, `All ${noun}`)),
          ...all.map((value, index) => h("label", { className: "agent-tool-picker-item" }, checks[index], h("span", { className: "agent-tool-picker-name" }, options.includes(value) ? label(value) : `${label(value)} (missing)`))))),
        h("div", { className: "picker-actions" }, cancel, ok)));
      cancel.onclick = () => popover.close();
      ok.onclick = () => { popover.close(); apply(all.filter((value) => selected.has(value))); renderForm(); $(id)?.focus(); };
    };
    return trigger;
  }

  const field = (label, control, tip) => h("div", { className: "field" }, h("label", { htmlFor: control.id, title: tip }, label), control);

  function statusLine() {
    const t = state.current, parts = L.statusParts(state.status || {});
    const sep = () => h("span", { className: "sep" }, "·");
    const source = h("span", { title: "The input connector this trigger listens to" }, L.sourceName(t.source));
    if (!t.revision) return h("div", { className: "run-meta" }, h("span", {}, "Not saved yet"), sep(), source);
    const chip = parts.error ? statusChip("failed", "Failing", parts.error) : t.enabled ? statusChip("done", "Enabled") : statusChip("idle", "Disabled");
    return h("div", { className: "run-meta" }, chip, sep(), source,
      ...(parts.pending ? [sep(), h("span", {}, parts.pending)] : []),
      ...(parts.accepted ? [sep(), h("span", {}, "Last start "), timeEl(parts.accepted)] : []));
  }

  function renderForm() {
    if (state.error) return;
    const t = state.current, d = state.form;
    if (!t) { fill(main); return; }
    const names = state.flows.map((w) => w.name);
    const name = h("input", { id: "intake-name", value: d.name, placeholder: "Not set" });
    name.style.minWidth = "200px";
    name.oninput = () => { d.name = name.value; fitWidth(name); sync(); };
    const sources = picker("intake-sources", d.sources, names.filter((value) => value !== d.workflow), titleCase, "workflows", "No workflows", (next) => { d.sources = next; });
    const outcomes = picker("intake-outcomes", d.outcomes, L.OUTCOMES.map(([v]) => v), outcomeWord, "outcomes", "No outcomes", (next) => { d.outcomes = next; });
    const gone = d.workflow && !names.includes(d.workflow);
    const workflow = h("select", { id: "intake-workflow" }, h("option", { value: "" }, "Not set"),
      ...names.map((value) => h("option", { value, selected: value === d.workflow }, titleCase(value))),
      gone ? h("option", { value: d.workflow, selected: true }, `${titleCase(d.workflow)} (missing)`) : null);
    const versions = state.flows.find((w) => w.name === d.workflow)?.versions || [];
    const lost = d.version && !versions.includes(d.version);
    const version = h("select", { id: "intake-version", disabled: !d.workflow, title: d.workflow ? undefined : "Needs a workflow" },
      ...versions.map((v, i) => h("option", { value: String(v), selected: v === d.version }, i ? `v${v}` : `v${v} (newest)`)),
      lost ? h("option", { value: String(d.version), selected: true }, `v${d.version} (missing)`) : null,
      d.workflow ? null : h("option", { value: "" }, "Not set"));
    // A newly chosen workflow starts at its newest version, as a workflow step does.
    workflow.onchange = () => {
      d.workflow = workflow.value;
      d.version = state.flows.find((w) => w.name === d.workflow)?.versions[0] || 0;
      d.sources = d.sources.filter((value) => value !== d.workflow);
      renderForm(); $("intake-workflow")?.focus();
    };
    version.onchange = () => { d.version = Number(version.value) || 0; fitWidth(version); sync(); };
    const enabled = h("input", { id: "intake-enabled", type: "checkbox", checked: d.enabled });
    enabled.onchange = () => { d.enabled = enabled.checked; sync(); };
    const cancel = h("button", { type: "button", id: "intake-cancel", onclick: discard }, "Cancel");
    const save = h("button", { type: "submit", id: "intake-save", className: "primary" }, "Save");
    const form = h("form", { className: "step-form intake-form", noValidate: true, onsubmit: (ev) => { ev.preventDefault(); store(); } },
      h("div", { className: "field-row" },
        field("Name", name),
        field("Source workflows", sources, "A matching run starts the selected workflow; that destination is excluded to prevent self-triggering"),
        field("Outcomes", outcomes, "The ways a source run can end that start the workflow"),
        field("Starts workflow", workflow, "The workflow a matching event starts"),
        field("Version", version, "The version that is started; it stays pinned when newer versions are saved"),
        h("div", { className: "field check-field" },
          h("label", { htmlFor: enabled.id, title: "Unticked, the trigger starts nothing and its events are kept; ticked again, it catches up on the events kept since" }, enabled, "Enabled"))),
      h("div", { className: "intake-problems error-text", id: "intake-problems", role: "alert" }),
      h("div", { className: "intake-actions" }, cancel, save));
    fill(main, h("div", { className: "section" }, statusLine(), form));
    for (const control of [name, workflow, version]) fitWidth(control);
    sync();
  }

  // sync shows the form's state without redrawing it: the problems Save found, and Save and
  // Cancel enabled only with unsaved edits.
  function sync() {
    const box = $("intake-problems"), save = $("intake-save"), cancel = $("intake-cancel");
    if (!box) return;
    if (state.problems.length) state.problems = L.problems(state.form); // a fixed field clears its sentence
    box.replaceChildren(...state.problems.map((text) => h("p", {}, text)));
    const edited = dirty();
    save.disabled = !edited || state.saving;
    save.title = edited ? "Stores the trigger" : "No unsaved edits";
    cancel.disabled = !edited && !!state.current.revision;
    cancel.title = cancel.disabled ? "No unsaved edits" : state.current.revision ? "Discards the unsaved edits" : "Discards the new trigger";
  }

  async function loadStatus() {
    const t = state.current;
    if (!t?.revision) return;
    try {
      const status = await api("GET", `${API}/${enc(t.id)}/status`);
      if (state.current?.id !== t.id || !state.active) return;
      const changed = !L.same(status, state.status);
      state.status = status;
      // Only the status line is redrawn, so a field being edited keeps its focus.
      if (changed) main.querySelector(".run-meta")?.replaceWith(statusLine());
    } catch { /* the status line keeps what it has */ }
  }

  // open shows a trigger without asking; select asks first when edits would be lost.
  function open(t) {
    state.current = t; state.form = t ? L.draft(t) : null; state.stored = t ? L.draft(t) : null;
    state.status = null; state.problems = [];
    if (t?.revision) remember(t.id);
    renderList(); renderForm(); loadStatus();
  }

  async function leave() {
    if (!dirty()) return true; // an untouched new trigger is gone without a question
    return confirmAction("Discard your unsaved changes?", "Discard");
  }

  async function select(t) {
    if (t.id === state.current?.id || !(await leave())) return;
    open(t);
  }

  // chooseSource asks which input connector a new trigger listens to. The connectors that do
  // not exist yet are listed greyed out; it resolves the chosen one, or "" when cancelled.
  function chooseSource() {
    return new Promise((resolve) => {
      let chosen = L.SOURCES.find((s) => s.available).id, answer = "";
      const items = L.SOURCES.map((s) => {
        const li = h("li", { role: "radio", tabIndex: s.available ? 0 : -1, "aria-checked": String(s.id === chosen), "aria-disabled": String(!s.available), title: s.tip, className: s.available ? "" : "muted unavailable" },
          h("div", { className: "row-title" }, s.name));
        if (s.available) {
          const pick = () => { chosen = s.id; for (const other of items) other.setAttribute("aria-checked", String(other === li)); };
          li.onclick = pick;
          li.onkeydown = (ev) => { if (ev.key === " " || ev.key === "Enter") { ev.preventDefault(); pick(); } };
        }
        return li;
      });
      const create = h("button", { type: "button", className: "primary", onclick: () => { answer = chosen; dlg.close(); } }, "Create");
      const dlg = openDialog({ title: "New trigger",
        body: h("div", {}, h("label", { id: "intake-source-label" }, "Input connector"), h("ul", { className: "pick-list", role: "radiogroup", "aria-labelledby": "intake-source-label" }, ...items)),
        footer: [h("button", { type: "button", onclick: () => dlg.close() }, "Cancel"), create],
        onClose: () => resolve(answer) });
    });
  }

  async function create() {
    if (state.current && !state.current.revision && !dirty()) { $("intake-name")?.focus(); return; }
    if (!(await leave())) return;
    const source = await chooseSource();
    if (!source) return;
    open({ id: crypto.randomUUID(), source, enabled: false });
    $("intake-name")?.focus();
  }

  function discard() {
    if (state.current?.revision) { open(state.current); return; }
    open(state.items.find((t) => t.id === remembered()) || state.items[0] || null);
  }

  async function store() {
    if (state.saving || !state.current || !dirty()) return;
    state.problems = L.problems(state.form);
    if (state.problems.length) { sync(); return; }
    const value = L.body(state.current, state.form);
    state.saving = true; sync();
    try {
      await measurementUI.write(`${API}/${enc(value.id)}`, value, value.revision);
      state.stored = L.draft(value); // nothing is unsaved while the list is read again
      remember(value.id);
      notify("Saved");
      await load(value.id);
    } catch (error) { notify(`Not saved: ${error.message}`, "error"); }
    finally { state.saving = false; sync(); }
  }

  async function remove(t) {
    if (!(await confirmAction(`Delete ${nameOf(t)}?`, "Delete"))) return;
    try {
      await api("DELETE", `${API}/${enc(t.id)}`);
      notify("Deleted");
      // Deleting the selected trigger selects its neighbour: the next one, else the previous one.
      const at = state.items.findIndex((x) => x.id === t.id);
      const next = state.current?.id === t.id ? (state.items[at + 1] || state.items[at - 1])?.id || "" : state.current?.id;
      if (state.current?.id === t.id) state.current = null;
      await load(next);
    } catch (error) { notify(`Not deleted: ${error.message}`, "error"); }
  }

  // load reads the triggers and the workflows; an edit in progress is never overwritten.
  async function load(want = "") {
    const request = ++state.request;
    try {
      const [bindings, flows] = await Promise.all([api("GET", API), api("GET", "/api/orchestrator/workflows")]);
      if (!state.active || request !== state.request) return;
      state.items = L.triggers(bindings); state.flows = L.workflows(flows); state.error = ""; state.loaded = true;
    } catch (error) {
      if (!state.active || request !== state.request) return;
      state.error = `Cannot load the event triggers: ${error.message}`; state.loaded = true;
      renderList(); return;
    }
    if (!want && dirty()) { renderList(); renderForm(); return; }
    const id = want || state.current?.id || remembered();
    open(state.items.find((t) => t.id === id) || (state.current && !state.current.revision ? state.current : state.items[0]) || null);
  }

  $("intake-new-slot").append(iconButton("plus", "New trigger", create));
  rows.parentElement.addEventListener("contextmenu", (ev) => {
    if (ev.target.closest?.("li")) return;
    openMenu(ev, [["New trigger", create, "", "Adds an event trigger"]]);
  });

  tabs.intake = { show() { state.active = true; load(); }, hide() { state.active = false; ++state.request; } };
})();
