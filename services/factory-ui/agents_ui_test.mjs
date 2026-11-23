import assert from "node:assert/strict";
import fs from "node:fs";
import test from "node:test";
import vm from "node:vm";

const source = fs.readFileSync(new URL("web/agents.js", import.meta.url), "utf8");

function setup() {
  const requests = [];
  const nodes = [];
  const h = (tag, props = {}, ...children) => {
    const node = {
      tag, props, children: children.flat().filter(Boolean), value: props.value || "", checked: !!props.checked, disabled: !!props.disabled,
      style: {}, isConnected: true, events: {}, textContent: "", title: props.title || "", classList: { add() {} },
      append(...items) { this.children.push(...items); },
      replaceChildren(...items) { this.children = items.flat().filter(Boolean); },
      addEventListener(name, handler) { this.events[name] = handler; },
      dispatchEvent(event) { this.events[event.type]?.(); },
      focus() { this.focused = true; },
    };
    nodes.push(node);
    return node;
  };
  const context = {
    h, registryTab() {},
    policyReferences(text) { return [...text.matchAll(/\{\{policy::([a-z0-9][a-z0-9/-]*)\}\}/g)].map((match) => match[1]); },
    setTextViewMode(mode) { context.textMode = mode; },
    iconButton: (name, label, onclick) => h("button", { title: label, "aria-label": label, onclick }, h("span", { className: "icon" }, name)),
    openDialog(config) {
      context.dialog = config;
      return { close() { context.dialog = null; config.onClose?.(); } };
    },
    openPopover(anchor, body, onClose) {
      context.popover = { anchor, body, closed: false, close() { this.closed = true; onClose?.(); } };
      return context.popover;
    },
    api(_method, path) {
      requests.push(path);
      if (path === "/api/policies/policies") return Promise.resolve({ entries: context.policies.map((name) => ({ name })) });
      return Promise.resolve(path.endsWith("/api/services") ? { services: context.services } : { data: context.data });
    },
    window: { discoverMcpServiceTools: (name) => Promise.resolve(context.tools[name] || []) },
    statusChip: (_state, label) => h("span", {}, label),
    requestAnimationFrame(callback) { callback(); },
    fitWidth() {}, document: { activeElement: null }, Event: class { constructor(type) { this.type = type; } },
    AbortController, setTimeout, clearTimeout,
  };
  context.data = [{ id: "local", upstream: "publisher/complete-long-model-name", owned_by: "lmstudio", available: true }];
  context.services = [{ name: "docs" }];
  context.policies = ["existing", "quality"];
  context.tools = { docs: [{ name: "search", description: "Search documents" }, { name: "read", description: "Read a document" }] };
  vm.runInNewContext(source, context);
  const area = h("textarea", { value: JSON.stringify({ models: ["local"] }) });
  const prompt = h("textarea", { value: "A\n{{policy::existing}}\nB" });
  const fields = context.agentFields(area, { "systemprompt.md": prompt });
  return { context, requests, nodes, fields, area, prompt };
}

const settle = async () => { for (let i = 0; i < 5; i++) await new Promise(setImmediate); };

test("agents show the full provider model name and refresh the list without editing JSON", async () => {
  const { context, requests, nodes, area } = setup();
  await settle();
  const select = nodes.find((node) => node.tag === "select" && node.id === "agent-model");
  const refresh = nodes.find((node) => node.tag === "button" && node.title === "Refresh models");
  assert.ok(refresh, "refresh control is visible in the model field");
  assert.ok(select.children.some((option) => JSON.stringify(option.children).includes("publisher/complete-long-model-name")));
  const name = nodes.find((node) => node.props.className === "model-name");
  assert.equal(name, undefined, "no duplicate model name below selector");
  context.data = [
    { id: "local", upstream: "publisher/new-complete-model-name", owned_by: "lmstudio", available: true },
    { id: "new-local", upstream: "publisher/newly-downloaded-model", owned_by: "lmstudio", available: true },
  ];
  const original = area.value;
  await refresh.onclick();
  assert.equal(requests.filter((path) => path === "/api/llm/v1/models").length, 3, "initial and field requests plus an explicit refresh");
  assert.ok(select.children.some((option) => JSON.stringify(option.children).includes("publisher/new-complete-model-name")));
  assert.ok(select.children.some((option) => option.value === "new-local" && JSON.stringify(option.children).includes("publisher/newly-downloaded-model")));
  assert.equal(refresh.props["aria-label"], "Refresh models");
  assert.equal(area.value, original);
});

test("model options include colored availability and sort available first alphabetically", async () => {
  const { context, nodes, area } = setup();
  await settle();
  context.data = [
    { id: "z-off", available: false }, { id: "z-on", available: true },
    { id: "a-off", available: false }, { id: "a-on", available: true },
    { id: "local", available: false },
  ];
  const original = area.value;
  await nodes.find((n) => n.tag === "button" && n.title === "Refresh models").onclick();
  const options = nodes.find((n) => n.id === "agent-model").children.filter((n) => n.tag === "option");
  assert.deepEqual(Array.from(options, (o) => o.value), ["a-on", "z-on", "a-off", "local", "z-off"]);
  assert.equal(options[0].props.className, "available");
  assert.equal(options[3].props.className, "unavailable");
  assert.equal(options[3].children[0], "local");
  const selectedDot = nodes.find((n) => n.props.className === "model-dot unknown");
  assert.equal(selectedDot.className, "model-dot unavailable");
  // The dot is never colour alone: the state stands beside the select as a chip.
  assert.equal(nodes.some((n) => n.props.className === "model-state"), true);
  assert.equal(area.value, original);
});

test("agent tools come from MCP discovery and update agent.json without losing missing refs", async () => {
  const { context, nodes, area, requests } = setup();
  await settle();
  assert.equal(nodes.some((node) => node.tag === "input" && node.value === "docs/search"), false, "tool list stays out of the page");
  const choose = nodes.find((node) => node.id === "agent-tools");
  assert.equal(choose.tag, "button", "the Tools field itself is the picker");
  assert.equal(choose.textContent, "No tools");
  assert.equal(nodes.some((node) => node.tag === "input" && node.id === "agent-tools"), false);
  choose.onclick();
  await settle();
  assert.ok(requests.includes("/api/mcp-gateway/api/services"));
  const search = nodes.findLast((node) => node.tag === "input" && node.value === "docs/search");
  assert.ok(search, "discovered MCP tool is selectable");
  search.checked = true;
  search.events.change();
  assert.equal(JSON.parse(area.value).tools, undefined, "changes wait for Apply");
  const apply = nodes.findLast((node) => node.tag === "button" && node.children.includes("Apply"));
  apply.props.onclick();
  assert.deepEqual(JSON.parse(area.value).tools, ["docs/search"]);
  assert.equal(choose.textContent, "1 tool selected");

  area.value = JSON.stringify({ models: ["local"], tools: ["docs/search", "old/missing"] });
  area.dispatchEvent(new context.Event("input"));
  choose.onclick();
  await settle();
  const missing = nodes.findLast((node) => node.tag === "input" && node.value === "old/missing");
  assert.equal(missing.checked, true, "saved refs remain visible even when absent from the catalog");
  missing.checked = false;
  missing.events.change();
  nodes.findLast((node) => node.tag === "button" && node.children.includes("Apply")).props.onclick();
  assert.deepEqual(JSON.parse(area.value).tools, ["docs/search"]);
});

test("long tool descriptions stay inside the modal and Cancel leaves the agent unchanged", async () => {
  const { context, nodes, area } = setup();
  context.tools.docs[0].description = "Detailed usage instructions. ".repeat(30);
  const choose = nodes.find((node) => node.id === "agent-tools");
  choose.onclick();
  await settle();
  assert.equal(nodes.some((node) => node.props.className === "agent-tool-list"), false);
  const entry = nodes.findLast((node) => node.props.className === "agent-tool-picker-item" && node.title.startsWith("Detailed usage"));
  assert.ok(entry, "long instructions remain available as a tooltip");
  assert.equal(entry.children.length, 2, "only checkbox and tool name are displayed");
  const check = nodes.findLast((node) => node.tag === "input" && node.value === "docs/search");
  check.checked = true;
  check.events.change();
  context.dialog.footer[0].props.onclick();
  assert.equal(JSON.parse(area.value).tools, undefined);
});

test("every control uses the Safari-safe fixed-height select treatment", () => {
  const css = fs.readFileSync(new URL("web/style.css", import.meta.url), "utf8");
  // Global, so no panel (node panel, agents, evaluations) falls back to Safari's native select.
  assert.match(css, /^input, select \{ height: 32px; \}$/m);
  assert.match(css, /^button \{[^}]*height: 32px;/m);
  assert.match(css, /^select \{[^}]*-webkit-appearance: none; appearance: none;/ms);
  assert.match(css, /^input\[type=number\]::-webkit-inner-spin-button/m);
  assert.doesNotMatch(css, /base-select|selectedcontent/);
});

test("Policies picker edits systemprompt templates and follows manual source changes", async () => {
  const { context, nodes, prompt } = setup();
  await settle();
  const trigger = nodes.find((node) => node.id === "agent-policies");
  assert.equal(trigger.textContent, "1 policy selected");
  await trigger.onclick();
  await settle();
  const checks = nodes.filter((node) => node.tag === "input" && node.props.type === "checkbox" && ["existing", "quality"].includes(node.value)).slice(-2);
  const existing = checks.find((node) => node.value === "existing");
  const quality = checks.find((node) => node.value === "quality");
  existing.checked = false; existing.events.change();
  quality.checked = true; quality.events.change();
  const apply = nodes.find((node) => node.tag === "button" && node.children.includes("Apply") && node !== undefined);
  apply.onclick();
  assert.equal(prompt.value, "A\n\nB\n{{policy::quality}}", "only the exact removed token changes existing prose");
  assert.equal(trigger.textContent, "1 policy selected");
  assert.equal(context.popover.closed, true);
  assert.equal(context.dialog, undefined, "Policies uses an anchored popover, not a modal dialog");
  prompt.value += "\n{{policy::existing}}";
  prompt.dispatchEvent(new context.Event("input"));
  assert.equal(trigger.textContent, "2 policies selected", "manual source edits remain the source of truth");
});

test("temperature is an optional number in agent.json, left out when empty", async () => {
  const { context, nodes, area } = setup();
  await settle();
  const temperature = nodes.find((node) => node.id === "agent-temperature");
  assert.ok(temperature, "the field is in the form");
  assert.equal(temperature.props.type, undefined, "a plain digit field, no spinner");
  assert.equal(temperature.props.placeholder, "Not set");
  const config = () => JSON.parse(area.value);
  const type = (field, value) => { field.value = value; field.events.input(); };
  assert.equal("temperature" in config(), false);
  type(temperature, "0.2");
  assert.equal(config().temperature, 0.2);
  assert.equal(typeof config().temperature, "number");
  type(temperature, "0,7");
  assert.equal(config().temperature, 0.7, "a decimal comma is a decimal point");
  assert.deepEqual(config().models, ["local"], "the other settings stay");
  // Empty, out of range, or not a number: the setting is left out, never stored as text.
  for (const bad of ["", "2.5", "-1", "warm", "1e1"]) { type(temperature, "1"); type(temperature, bad); assert.equal("temperature" in config(), false, `temperature ${bad}`); }
  type(temperature, "0");
  assert.equal(config().temperature, 0, "zero is a value");
  // An edit of the JSON shows in the field.
  context.document.activeElement = null;
  area.value = JSON.stringify({ models: ["local"], temperature: 1.5 });
  area.events.input();
  assert.equal(String(temperature.value), "1.5");
  const { modelSetting } = context;
  assert.equal(modelSetting("2"), 2);
  assert.equal(modelSetting(" .5 "), 0.5);
  assert.equal(modelSetting("2.01"), null);
});

test("Unpack inputs is a tickbox bound to the boolean unpack_inputs in agent.json; a new agent starts ticked", async () => {
  const { context, nodes, area } = setup();
  await settle();
  const unpack = nodes.find((node) => node.id === "agent-unpack-inputs");
  assert.ok(unpack, "the tickbox is in the form");
  assert.equal(unpack.props.type, "checkbox");
  const label = nodes.find((node) => node.tag === "label" && node.props.htmlFor === "agent-unpack-inputs");
  assert.ok(label.children.includes("Unpack inputs into the first message"));
  assert.match(label.props.title, /unticked, the agent gets only the object IDs and reads them with get_artifact/);
  const config = () => JSON.parse(area.value);
  // A definition without the field shows unticked; a tick writes true, clearing it writes false.
  assert.equal(unpack.checked, false);
  unpack.checked = true;
  unpack.events.change();
  assert.equal(config().unpack_inputs, true);
  unpack.checked = false;
  unpack.events.change();
  assert.equal(config().unpack_inputs, false, "stored as false, never left out");
  assert.deepEqual(config().models, ["local"], "the other settings stay");
  // An edit of the JSON shows in the tickbox.
  area.value = JSON.stringify({ models: ["local"], unpack_inputs: true });
  area.events.input();
  assert.equal(unpack.checked, true);
  area.value = JSON.stringify({ models: ["local"], unpack_inputs: "yes" });
  area.events.input();
  assert.equal(unpack.checked, false, "only the boolean true ticks it");
  // A new agent starts with true.
  assert.match(source, /max_retries: 0,\n        unpack_inputs: true,\n      \}, null, 2\)/);
});

test("Commands lists the command patterns of agent.json one per line and leaves an empty list out", async () => {
  const { nodes, area } = setup();
  await settle();
  const commands = nodes.find((node) => node.tag === "textarea" && node.id === "agent-commands");
  assert.ok(commands, "the Commands field exists");
  const label = nodes.find((node) => node.tag === "label" && node.children.includes("Commands"));
  assert.match(label.props.title, /may run in a dev pod/);
  assert.match(label.props.title, /\* stands for any text/);
  assert.match(label.props.title, /Nothing listed means no command/);
  assert.equal(commands.props.placeholder, "No commands");
  assert.equal(commands.value, "", "an agent without commands");
  commands.value = "./verify.sh\n\n  git status*  \npython3 -m unittest *\n";
  commands.events.input();
  assert.deepEqual(JSON.parse(area.value).commands, ["./verify.sh", "git status*", "python3 -m unittest *"]);
  assert.deepEqual(JSON.parse(area.value).models, ["local"], "the other settings stay");
  // An edit of the JSON shows in the field.
  area.value = JSON.stringify({ models: ["local"], commands: ["git diff*", "git log*"] });
  area.events.input();
  assert.equal(commands.value, "git diff*\ngit log*");
  commands.value = "  \n";
  commands.events.input();
  assert.equal("commands" in JSON.parse(area.value), false, "nothing listed: no command is allowed");
});
