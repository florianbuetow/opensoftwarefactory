"use strict";
(() => {
  const body = measurementUI.panel("judges", true), state = { files: null, name: "", version: "", active: false, dirty: false, request: 0 };
  const page = $("tab-judges");
  page.classList.add("page-judge-editor");
  const selection = h("div", { className: "panel-body judge-selection" });
  page.insertBefore(h("div", { className: "panel list-panel" }, h("div", { className: "panel-head" }, h("h2", {}, "Judges")), selection), body.parentElement);
  const agent = h("select", { id: "judge-agent" }), version = h("select", { id: "judge-version" });
  const editor = h("form", { className: "measurement-editor judge-editor", hidden: true }), settings = h("div");
  const prompt = h("textarea", { id: "judge-prompt", rows: 8 });
  const config = h("textarea", { id: "judge-config", rows: 8, className: "measurement-json" });
  const mappings = h("div", { className: "judge-mappings" }), problem = h("p", { className: "error-text", role: "alert" });
  const save = h("button", { type: "submit", className: "primary" }, "Save new version");
  const link = h("a", { href: "#agents" }, "Agent registry");
  const add = h("button", { type: "button", onclick: () => { mappingRow(); syncMappingConfig(); } }, "Add metric");
  selection.append(measurementUI.field("Agent", agent), measurementUI.field("Version", version), link);
  const empty = emptyPanel("No judge selected");
  body.append(empty, editor);
  editor.append(settings, measurementUI.field("System prompt", prompt), h("h2", {}, "JSON output metrics"), mappings, add,
    h("details", {}, h("summary", {}, "Agent configuration"), measurementUI.field("Config (agent.json)", config)), problem,
    h("div", { className: "measurement-actions" }, h("button", { type: "button", onclick: () => { state.dirty = false; loadAgent(); } }, "Cancel"), save));
  prompt.oninput = () => { state.dirty = true; };
  config.oninput = () => {
    state.dirty = true;
    try {
      const definition = JSON.parse(config.value);
      if (Array.isArray(definition.output_contract?.mappings)) {
        mappings.replaceChildren();
        for (const mapping of definition.output_contract.mappings) mappingRow(mapping);
      }
    } catch { /* Keep the mapping draft while JSON is incomplete. */ }
  };
  function syncMappingConfig() {
    state.dirty = true;
    try {
      const definition = JSON.parse(config.value);
      definition.output_contract = { format: "json", mappings: [...mappings.children].map(row => row.mappingValue()) };
      config.value = JSON.stringify(definition, null, 2);
    } catch { problem.textContent = "Fix the agent JSON before changing mappings."; }
  }
  function mappingRow(mapping = {}) {
    const path = h("input", { value: mapping.path ?? "", "aria-label": "JSON field path", placeholder: "/score" });
    const name = h("input", { value: mapping.name ?? "", required: true, "aria-label": "Metric name" });
    const type = h("select", { "aria-label": "Value type" }, h("option", { value: "" }, "Not set"), h("option", { value: "number", selected: mapping.type === "number" }, "Number"), h("option", { value: "string", selected: mapping.type === "string" }, "Text"));
    const row = h("div", { className: "judge-mapping-row" }, path, name, type);
    row.mappingValue = () => ({ path: path.value, name: name.value.trim(), type: type.value });
    row.append(iconButton("close", "Remove metric", () => { row.remove(); syncMappingConfig(); }));
    for (const control of [path, name, type]) control.oninput = syncMappingConfig;
    mappings.append(row);
  }
  async function loadAgent() {
    if (!agent.value || !version.value) { editor.hidden = true; return; }
    const request = ++state.request;
    try {
      const data = await api("GET", `/api/agents/agents/${agent.value.split("/").map(encodeURIComponent).join("/")}/${encodeURIComponent(version.value)}`);
      if (request !== state.request || !state.active) return;
      state.name = agent.value; state.version = version.value; state.files = data.files;
      const raw = data.files["agent.json"], definition = typeof raw === "string" ? JSON.parse(raw) : raw;
      config.value = JSON.stringify(definition, null, 2); prompt.value = data.files["systemprompt.md"] || "";
      settings.replaceChildren(agentFields(config, {}));
      mappings.replaceChildren(); for (const item of definition.output_contract?.mappings || []) mappingRow(item);
      empty.hidden = true; editor.hidden = false; state.dirty = false; problem.textContent = "";
      link.href = `#agents/${state.name}/${state.version}`;
    } catch (error) { problem.textContent = `Cannot load judge: ${error.message}`; editor.hidden = false; }
  }
  async function loadVersions() {
    try {
      const data = await api("GET", `/api/agents/agents/${agent.value.split("/").map(encodeURIComponent).join("/")}`);
      version.replaceChildren(h("option", { value: "" }, "Select version"), ...(data.versions || []).slice().reverse().map(v => h("option", { value: v.version }, v.version)));
      editor.hidden = true; empty.hidden = false;
    } catch (error) { problem.textContent = `Cannot load versions: ${error.message}`; }
  }
  agent.onchange = () => {
    if (state.dirty) { agent.value = state.name; notify("Save or cancel the current edit", "error"); return; }
    loadVersions();
  };
  version.onchange = () => {
    if (state.dirty) { version.value = state.version; notify("Save or cancel the current edit", "error"); return; }
    loadAgent();
  };
  editor.onsubmit = async event => {
    event.preventDefault(); if (!state.files) return;
    save.disabled = true;
    try {
      const definition = JSON.parse(config.value), values = [...mappings.children].map(row => row.mappingValue());
      if (values.some(m => !m.name || !["number", "string"].includes(m.type) || (m.path && !m.path.startsWith("/")))) throw new Error("Each metric needs a JSON pointer, name, and type.");
      if (new Set(values.map(m => m.name)).size !== values.length) throw new Error("Metric names must be unique.");
      definition.output_contract = { format: "json", mappings: values };
      const result = await api("POST", `/api/agents/agents/${state.name.split("/").map(encodeURIComponent).join("/")}`, { files: { ...state.files, "agent.json": definition, "systemprompt.md": prompt.value } }, creatorHeaders());
      state.dirty = false; notify(`Saved ${result.version}`); await loadVersions(); version.value = result.version; await loadAgent();
    } catch (error) { problem.textContent = `Not saved: ${error.message}`; }
    finally { save.disabled = false; }
  };
  tabs.judges = { async show() {
    state.active = true; if (state.files || state.dirty) return;
    try {
      const data = await api("GET", "/api/agents/agents");
      agent.replaceChildren(h("option", { value: "" }, "Select agent"), ...(data.entries || []).map(e => h("option", { value: e.name }, e.name)));
      version.replaceChildren(h("option", { value: "" }, "Select version"));
    } catch (error) { body.append(errorPanel(`Cannot load agents: ${error.message}`)); }
  }, hide() { state.active = false; ++state.request; } };
})();
