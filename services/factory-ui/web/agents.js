"use strict";
// Agents page: the agent registry. Every version holds systemprompt.md and agent.json.

// A new agent starts with the first available gateway model.
let gatewayModels = [];
let latestModelRequest = 0;
async function loadGatewayModels() {
  const request = ++latestModelRequest;
  const response = await api("GET", "/api/llm/v1/models");
  const models = Array.isArray(response.data) ? response.data : [];
  if (request === latestModelRequest) gatewayModels = models;
  return models;
}
loadGatewayModels().catch(() => {});

// modelSetting reads a typed temperature from 0 to 2.
// null means the field is empty or holds something else, and the setting is left out.
function modelSetting(typed) {
  const text = String(typed ?? "").trim().replace(",", ".");
  if (!/^(\d+\.?\d*|\.\d+)$/.test(text)) return null;
  const n = Number(text);
  return n >= 0 && n <= 2 ? n : null;
}

// commandLines reads the command patterns of an agent: the stored list, or the typed text with
// one pattern per line. Empty lines and anything that is not text are left out.
function commandLines(value) {
  const lines = Array.isArray(value) ? value : String(value ?? "").split("\n");
  return lines.filter((line) => typeof line === "string").map((line) => line.trim()).filter(Boolean);
}

function chooseAgentSkills(current, save) {
  const selected = new Map(current.map((ref) => ref.split("@")));
  const skill = h("select", { id: "agent-skill-name", disabled: true });
  const version = h("select", { id: "agent-skill-version", disabled: true });
  const add = h("button", { type: "button", disabled: true }, "Add skill");
  const list = h("ul", { className: "skill-picks", "aria-label": "Selected skills" });
  const problem = h("p", { className: "error-text", role: "alert" });
  const endpoint = "/api/skills/skills";
  let entries = [];
  let request = 0;
  let closed = false;
  const field = (input, label) => h("div", { className: "field" }, h("label", { htmlFor: input.id }, label), input);
  const apply = h("button", { type: "button", className: "primary", onclick: () => {
    save([...selected].map(([name, v]) => `${name}@${v}`));
    dialog.close();
  } }, "Apply");
  const dialog = openDialog({
    title: "Choose skills",
    body: h("div", { className: "skill-picker" }, list,
      h("div", { className: "row" }, field(skill, "Skill"), field(version, "Version"), add), problem),
    footer: [h("button", { type: "button", onclick: () => dialog.close() }, "Cancel"), apply],
    onClose: () => { closed = true; },
  });
  const option = (value, label = value) => h("option", { value }, label);
  const render = (placeholder) => {
    const available = entries.filter((entry) => !selected.has(entry));
    skill.replaceChildren(option("", placeholder || (available.length ? "Select skill" : "No skills available")), ...available.map((name) => option(name)));
    version.replaceChildren(option("", "Select version"));
    skill.disabled = !available.length;
    version.disabled = add.disabled = true;
    list.replaceChildren(...[...selected].map(([name, v]) =>
      h("li", {}, h("span", {}, `${name} ${v || ""}`),
        h("button", { type: "button", onclick: async () => {
          if (await confirmAction(`Remove ${name}?`, "Remove") && !closed) {
            request++;
            selected.delete(name);
            render();
            skill.focus();
          }
        } }, "Remove"))));
  };
  skill.onchange = async () => {
    const id = ++request;
    const name = skill.value;
    version.disabled = add.disabled = true;
    version.replaceChildren(option("", name ? "Loading…" : "Select version"));
    problem.textContent = "";
    if (!name) return;
    try {
      const data = await api("GET", `${endpoint}/${name.split("/").map(encodeURIComponent).join("/")}`);
      if (closed || id !== request) return;
      const versions = [...data.versions].reverse();
      version.replaceChildren(...(versions.length ? versions.map(({ version: v }) => option(v)) : [option("", "No versions")]));
      version.disabled = add.disabled = !versions.length;
    } catch (error) {
      if (closed || id !== request) return;
      version.replaceChildren(option("", "Select version"));
      problem.textContent = error.message;
    }
  };
  add.onclick = () => {
    if (!skill.value || !version.value || selected.has(skill.value)) return;
    selected.set(skill.value, version.value);
    render();
    skill.focus();
  };
  render("Loading…");
  api("GET", endpoint).then((data) => {
    if (closed) return;
    entries = data.entries.map(({ name }) => name);
    render();
  }, (error) => {
    if (closed) return;
    render();
    problem.textContent = error.message;
  });
}

// agentFields shows the settings of agent.json as fields beside it: a changed field rewrites
// the JSON, and an edit of the JSON updates the fields. The models, with whether each is
// available now, come from the LLM gateway, which maps every model name to a provider's model.
function agentFields(area, areas = {}) {
  let writing = false;
  let models = null; // the gateway's models once loaded, or the reason they could not be
  let latestFieldRequest = 0;
  let catalog = null;
  let toolErrors = [];
  let latestToolsRequest = 0;
  let renderPicker = null;
  let policyPopover = null;
  const read = () => {
    try { const d = JSON.parse(area.value); return d && typeof d === "object" && !Array.isArray(d) ? d : null; } catch { return null; }
  };
  const write = (change) => {
    const d = read();
    if (!d) return;
    change(d);
    writing = true;
    area.value = JSON.stringify(d, null, 2);
    area.dispatchEvent(new Event("input")); // resizes the editor and shows the Save row
    writing = false;
  };
  // Availability is part of each model option, including the selected value.
  const field = (label, control, tip, name, beside = null) => {
    control.id = `agent-${name}`;
    return h("div", { className: "field" }, h("label", { htmlFor: control.id, title: tip }, label),
      beside ? h("div", { className: "row" }, control, ...(Array.isArray(beside) ? beside : [beside])) : control);
  };

  const model = h("select", { className: "model-select" });
  const modelDot = h("span", { className: "model-dot unknown", "aria-hidden": "true" }, "●");
  const modelControl = h("div", { className: "model-select-control" }, modelDot, model);
  const refreshButton = iconButton("refresh", "Refresh models");
  const effort = h("select");
  const retries = h("input", { type: "number", min: 0, max: 100, placeholder: "0", autocomplete: "off" });
  // Temperature is an optional model setting: a plain digit field, empty when not set.
  const temperature = h("input", { inputMode: "decimal", placeholder: "Not set", autocomplete: "off", maxLength: 6 });
  const tools = h("button", { type: "button", className: "agent-tool-picker-trigger", "aria-haspopup": "dialog" }, "No tools");
  const policies = h("button", { type: "button", className: "agent-policy-picker-trigger", "aria-haspopup": "dialog" }, "No policies");
  const skills = h("button", { type: "button", "aria-haspopup": "dialog" }, "No skills");
  const scriptTimeout = h("input", { inputMode: "numeric", autocomplete: "off", placeholder: "180" });
  const selectedSkills = (config) => Array.isArray(config?.skills) ? config.skills.filter((ref) => typeof ref === "string") : [];
  skills.onclick = () => chooseAgentSkills(selectedSkills(read()), (refs) => {
    write((config) => { if (refs.length) config.skills = refs; else delete config.skills; });
    refresh();
  });
  const promptArea = areas["systemprompt.md"];
  const description = h("input", { autocomplete: "off", placeholder: "No description" });
  // unpack_inputs is required in agent.json: ticked, the first message holds the inputs' content.
  const unpack = h("input", { type: "checkbox" });
  // commands is the optional list of command patterns the agent may run in a dev pod, one per line.
  const commands = h("textarea", { rows: 2, placeholder: "No commands", spellcheck: false, autocomplete: "off" });
  const texts = [description];

  const selectedPolicies = () => {
    if (!promptArea) return [];
    try { return policyReferences(promptArea.value); } catch { return []; }
  };
  const renderPolicies = () => {
    if (!promptArea) { policies.disabled = true; policies.title = "System prompt is unavailable"; return; }
    try {
      const selected = policyReferences(promptArea.value);
      policies.textContent = selected.length ? `${selected.length} polic${selected.length === 1 ? "y" : "ies"} selected` : "No policies";
      policies.title = selected.length ? selected.join("\n") : "Choose policies";
    } catch (error) {
      policies.textContent = "Invalid policy reference";
      policies.title = error.message;
    }
  };

  const renderModel = (current) => {
    const known = Array.isArray(models) ? models : [];
    const ids = known.map((m) => m.id);
    if (current && !ids.includes(current)) ids.push(current);
    ids.sort((a, b) => Number(!!known.find((m) => m.id === b)?.available) - Number(!!known.find((m) => m.id === a)?.available) || a.localeCompare(b, undefined, { sensitivity: "base" }));
    model.replaceChildren(...(current ? [] : [h("option", { value: "", selected: true }, "No model")]), ...ids.map((id) => {
      const m = known.find((x) => x.id === id);
      const state = !Array.isArray(models) ? "⚪ Unknown" : !m ? "🔴 Not configured" : m.available ? "🟢 Available" : "🔴 Unavailable";
      const label = m && m.upstream && m.upstream !== id ? `${id} — ${m.upstream}` : id;
      const availability = !Array.isArray(models) ? "unknown" : m?.available ? "available" : "unavailable";
      return h("option", { value: id, selected: id === current, className: availability, title: `${state} — ${label}`, "aria-label": `${state} — ${label}` }, label);
    }));
    const selected = known.find((m) => m.id === current);
    modelDot.className = `model-dot ${!Array.isArray(models) ? "unknown" : selected?.available ? "available" : "unavailable"}`;
    modelDot.hidden = !current;
  };
  const renderEffort = (current) => {
    const values = ["", "none", "low", "medium", "high"]; // none: the model answers without thinking first
    if (current && !values.includes(current)) values.push(current);
    effort.replaceChildren(...values.map((v) => h("option", { value: v, selected: v === (current || "") }, v || "Not set")));
  };

  // refresh shows what the JSON holds; while it does not parse, the fields wait for it.
  const box = h("div", { className: "file-fields" });
  function refresh() {
    const d = read();
    for (const c of [model, effort, temperature, retries, tools, policies, skills, scriptTimeout, unpack, commands, ...texts]) { c.disabled = !d; c.title = d ? "" : "Needs valid JSON in agent.json"; }
    if (!d) return;
    renderModel(Array.isArray(d.models) ? d.models[0] || "" : "");
    renderEffort(typeof d.effort === "string" ? d.effort : "");
    if (document.activeElement !== retries) retries.value = d.max_retries ?? "";
    if (document.activeElement !== temperature) temperature.value = d.temperature ?? "";
    const selectedTools = Array.isArray(d.tools) ? d.tools : [];
    tools.textContent = selectedTools.length ? `${selectedTools.length} tool${selectedTools.length === 1 ? "" : "s"} selected` : "No tools";
    tools.title = selectedTools.length ? selectedTools.join("\n") : "Choose tools";
    renderPolicies();
    const skillRefs = selectedSkills(d);
    skills.textContent = skillRefs.length ? `${skillRefs.length} skill${skillRefs.length === 1 ? "" : "s"} selected` : "No skills";
    skills.title = skillRefs.join("\n");
    if (document.activeElement !== scriptTimeout) scriptTimeout.value = d.skill_script_timeout_seconds ?? "";
    unpack.checked = d.unpack_inputs === true;
    if (document.activeElement !== description) description.value = d.description ?? "";
    if (document.activeElement !== commands) { commands.value = commandLines(d.commands).join("\n"); growCommands(); }
    requestAnimationFrame(() => { for (const c of [model, effort, ...texts]) if (c.isConnected) fitWidth(c); });
  }

  model.addEventListener("change", () => {
    write((d) => { const rest = Array.isArray(d.models) ? d.models.slice(1) : []; d.models = model.value ? [model.value, ...rest] : rest; });
    renderModel(model.value);
    fitWidth(model);
  });
  effort.addEventListener("change", () => { write((d) => { if (effort.value) d.effort = effort.value; else delete d.effort; }); fitWidth(effort); });
  // max_retries replaced max_turns, which the agent no longer reads.
  retries.addEventListener("input", () => write((d) => { const n = parseInt(retries.value, 10); delete d.max_turns; if (n >= 0) d.max_retries = n; else delete d.max_retries; }));
  // Stored as a number and left out when empty or not a number in range.
  temperature.addEventListener("input", () => write((d) => { const n = modelSetting(temperature.value); if (n === null) delete d.temperature; else d.temperature = n; }));
  scriptTimeout.addEventListener("input", () => write((d) => {
    if (!scriptTimeout.value.trim()) delete d.skill_script_timeout_seconds;
    else d.skill_script_timeout_seconds = /^\d+$/.test(scriptTimeout.value) && Number.isSafeInteger(Number(scriptTimeout.value))
      ? Number(scriptTimeout.value) : scriptTimeout.value;
  }));
  description.addEventListener("input", () => write((d) => { d.description = description.value; }));
  unpack.addEventListener("change", () => write((d) => { d.unpack_inputs = unpack.checked; }));
  // One pattern per line; a list without patterns is left out, which allows no command.
  const growCommands = () => { commands.rows = Math.max(2, commands.value.split("\n").length); };
  commands.addEventListener("input", () => {
    growCommands();
    write((d) => { const list = commandLines(commands.value); if (list.length) d.commands = list; else delete d.commands; });
  });
  for (const t of texts) t.addEventListener("input", () => fitWidth(t));
  area.addEventListener("input", () => { if (!writing) refresh(); });
  promptArea?.addEventListener("input", renderPolicies);
  const fetchModels = async () => {
    const request = ++latestFieldRequest;
    refreshButton.disabled = true;
    let result;
    try { result = await loadGatewayModels(); }
    catch (e) { result = e.message; }
    if (request !== latestFieldRequest) return;
    models = result;
    refreshButton.disabled = false;
    const d = read();
    renderModel(d && Array.isArray(d.models) ? d.models[0] || "" : "");
    if (model.isConnected) fitWidth(model);
  };
  refreshButton.onclick = fetchModels;
  const fetchTools = async () => {
    const request = ++latestToolsRequest;
    catalog = null;
    toolErrors = [];
    renderPicker?.();
    let next = [], errors = [];
    try {
      const response = await api("GET", "/api/mcp-gateway/api/services");
      const catalogServices = (response.services || []).filter((service) => service.name !== "skills");
      const services = catalogServices.filter((service) => service.name && !service.error);
      errors = catalogServices.filter((service) => service.error).map((service) => service.name);
      const results = await Promise.allSettled(services.map(async (service) => {
        const controller = new AbortController();
        const timeout = setTimeout(() => controller.abort(), 10000);
        try { return (await window.discoverMcpServiceTools(service.name, controller.signal)).map((tool) => ({
          ref: `${service.name}/${tool.name}`, description: tool.description || "",
        })); }
        finally { clearTimeout(timeout); }
      }));
      results.forEach((result, index) => {
        if (result.status === "fulfilled") next.push(...result.value);
        else errors.push(services[index].name);
      });
    } catch { errors = ["the MCP gateway"]; }
    if (request !== latestToolsRequest) return;
    catalog = next.sort((a, b) => a.ref.localeCompare(b.ref));
    toolErrors = errors;
    renderPicker?.();
  };
  tools.onclick = () => {
    const d = read();
    if (!d) return;
    const selected = new Set(Array.isArray(d.tools) ? d.tools : []);
    const search = h("input", { type: "search", placeholder: "Find a tool", "aria-label": "Find a tool", autocomplete: "off" });
    const list = h("div", { className: "agent-tool-picker-list", role: "group", "aria-label": "Available tools" });
    const status = h("p", { className: "agent-tool-picker-status", role: "status" });
    const reload = iconButton("refresh", "Refresh tools", () => fetchTools());
    renderPicker = () => {
      const offered = catalog || [];
      const known = new Set(offered.map((tool) => tool.ref));
      const options = [...offered, ...[...selected].filter((ref) => !known.has(ref)).map((ref) => ({ ref, description: "Not in the current tool catalog" }))];
      const query = search.value.trim().toLowerCase();
      const visible = options.filter((tool) => `${tool.ref} ${tool.description}`.toLowerCase().includes(query));
      list.replaceChildren(...visible.map((tool) => {
        const check = h("input", { type: "checkbox", value: tool.ref, checked: selected.has(tool.ref) });
        check.addEventListener("change", () => { if (check.checked) selected.add(tool.ref); else selected.delete(tool.ref); });
        return h("label", { className: "agent-tool-picker-item", title: tool.description || tool.ref },
          check, h("span", { className: "agent-tool-picker-name" }, tool.ref));
      }));
      status.textContent = toolErrors.length ? `Cannot list tools from ${toolErrors.join(", ")}`
        : catalog === null ? "Loading tools…" : !options.length ? "No tools from the MCP services"
          : !visible.length ? "No matching tools" : "";
      reload.disabled = catalog === null;
    };
    search.addEventListener("input", () => renderPicker());
    const apply = h("button", { type: "button", className: "primary", onclick: () => {
      write((config) => { config.tools = [...selected]; });
      refresh();
      dialog.close();
    } }, "Apply");
    const dialog = openDialog({ title: "Choose tools", width: 560,
      body: h("div", { className: "agent-tool-picker" },
        h("div", { className: "row" }, search, reload), status, list),
      footer: [h("button", { type: "button", onclick: () => dialog.close() }, "Cancel"), apply],
      onClose: () => { renderPicker = null; },
    });
    renderPicker();
    fetchTools();
  };

  policies.onclick = async () => {
    if (!promptArea) return;
    if (policyPopover) { policyPopover.close(); return; }
    let current;
    try { current = new Set(policyReferences(promptArea.value)); }
    catch (error) { setStatus(`Cannot choose policies: ${error.message}`, "error"); return; }
    const list = h("div", { className: "agent-tool-picker-list", role: "group", "aria-label": "Policies" });
    const status = h("p", { className: "agent-tool-picker-status", role: "status" }, "Loading policies…");
    const apply = h("button", { type: "button", className: "primary", disabled: true }, "Apply");
    const selected = new Set(current);
    const render = (catalog = []) => {
      const known = new Set(catalog);
      const options = [...catalog, ...[...selected].filter((id) => !known.has(id))].sort();
      list.replaceChildren(...options.map((id) => {
        const missing = !known.has(id);
        const check = h("input", { type: "checkbox", value: id, checked: selected.has(id) });
        check.addEventListener("change", () => { if (check.checked) selected.add(id); else selected.delete(id); });
        return h("label", { className: "agent-tool-picker-item", title: missing ? "This referenced policy no longer exists" : id },
          check, h("span", { className: "agent-tool-picker-name" }, missing ? `${id} (missing)` : id));
      }));
      status.textContent = options.length ? "" : "No policies yet";
      apply.disabled = !catalog.length && !selected.size;
    };
    apply.onclick = () => {
      let text = promptArea.value;
      for (const id of current) if (!selected.has(id)) text = text.split(`{{policy::${id}}}`).join("");
      for (const id of selected) if (!current.has(id)) text += `${text && !text.endsWith("\n") ? "\n" : ""}{{policy::${id}}}`;
      promptArea.value = text;
      promptArea.dispatchEvent(new Event("input", { bubbles: true }));
      setTextViewMode("text");
      popover.close();
      requestAnimationFrame(() => { promptArea.selectionStart = promptArea.selectionEnd = promptArea.value.length; promptArea.focus(); });
    };
    const cancel = h("button", { type: "button" }, "Cancel");
    const popover = openPopover(policies, h("div", { className: "agent-policy-popover" },
      h("div", { className: "agent-policy-popover-list" }, status, list),
      h("div", { className: "picker-actions" }, cancel, apply)), () => { policyPopover = null; });
    policyPopover = popover;
    cancel.onclick = () => popover.close();
    try {
      const response = await api("GET", "/api/policies/policies");
      render((response.entries || []).map((entry) => entry.name).filter(Boolean));
    } catch (error) { status.textContent = `Cannot list policies: ${error.message}`; render([...current]); apply.disabled = true; }
  };

  model.id = "agent-model";
  const modelField = h("div", { className: "field model-field" },
    h("label", { htmlFor: model.id, title: "The model the agent calls, by its name in the LLM gateway" }, "Model"),
    h("div", { className: "row" }, modelControl, refreshButton));
  const toolsField = field("Tools", tools, "Choose the tools this agent can call", "tools");
  toolsField.classList.add("agent-tools");
  const policiesField = field("Policies", policies, "Choose policies to inject into the system prompt", "policies");
  policiesField.classList.add("agent-policies");
  // A tickbox carries its label beside it; the tooltip says what each state means.
  unpack.id = "agent-unpack-inputs";
  const unpackField = h("div", { className: "field check-field" },
    h("label", { htmlFor: unpack.id, title: "Ticked, the first message holds the content of every input; unticked, the agent gets only the object IDs and reads them with get_artifact" },
      unpack, "Unpack inputs into the first message"));
  const commandsField = field("Commands", commands, "The commands this agent may run in a dev pod, one pattern per line; * stands for any text, and a command must match one pattern completely. Nothing listed means no command", "commands");
  commandsField.classList.add("agent-commands");
  box.append(
    modelField,
    field("Effort", effort, "How much the model thinks before it answers. None skips the thinking; the others are sent to the model as reasoning_effort", "effort"),
    field("Temperature", temperature, "Sent to the model as temperature, from 0 to 2; the model's default when empty", "temperature"),
    field("Max retries", retries, "How many more model calls a run may make after the first, such as after a tool call", "retries"),
    toolsField,
    policiesField,
    field("Skills", skills, "Choose skills and their pinned versions", "skills"),
    field("Script timeout", scriptTimeout, "Time limit for each skill script, in seconds", "skill-script-timeout", h("span", { "aria-hidden": "true" }, "s")),
    field("Description", description, "What the agent is for, in a few words", "description"),
    unpackField,
    commandsField,
  );
  refresh();
  fetchModels();
  return box;
}

registryTab({
  tab: "agents",
  base: "/api/agents/agents",
  noun: "agent",
  files: [
    {
      name: "systemprompt.md",
      label: "System prompt (systemprompt.md)",
      json: false,
      mono: false,
      policyReferences: true,
      template: () => "",
    },
    {
      name: "agent.json",
      label: "Config (agent.json)",
      json: true,
      mono: true,
      fields: agentFields,
      template: (name) => JSON.stringify({
        schema_version: 1,
        name,
        description: "",
        models: [(gatewayModels.find((m) => m.available) || gatewayModels[0])?.id].filter(Boolean),
        effort: "medium",
        tools: [],
        max_retries: 0,
        unpack_inputs: true,
      }, null, 2),
    },
  ],
});
