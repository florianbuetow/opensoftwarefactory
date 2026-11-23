import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";

function setup(page, responder) {
  const elements = new Map(), nodes = [], requests = [], timers = [];
  function h(tag, props = {}, ...children) {
    const node = { tag, children: [], hidden: false, disabled: false, checked: false, textContent: "", className: "", attrs: {}, parentElement: null,
      append(...items) { for (const item of items.flat().filter(x => x !== null && x !== undefined && x !== false)) { this.children.push(item); if (typeof item === "object") item.parentElement = this; } },
      prepend(...items) { const previous = this.children; this.children = []; this.append(...items, ...previous); },
      insertBefore(item, before) { const index = this.children.indexOf(before); this.children.splice(index < 0 ? this.children.length : index, 0, item); item.parentElement = this; },
      replaceChildren(...items) { this.children = []; this.append(...items); },
      setAttribute(k, v) { this.attrs[k] = v; },
      remove() { if (this.parentElement) this.parentElement.children = this.parentElement.children.filter(x => x !== this); },
      insertAdjacentElement() {}, focus() {}, addEventListener(k, v) { this[`on${k}`] = v; },
      classList: { add() {}, toggle() {} }, scrollHeight: 100, scrollTop: 0, clientHeight: 100, offsetTop: 0,
    };
    let value = "";
    Object.defineProperty(node, "value", { get() { if (tag !== "select") return value; return this.options.find(o => o.selected)?.value || this.options[0]?.value || ""; }, set(v) { value = String(v); if (tag === "select") for (const option of this.options) option.selected = option.value === value; } });
    Object.defineProperty(node, "options", { get() { return this.children.filter(n => n?.tag === "option"); } });
    Object.defineProperty(node, "selectedOptions", { get() { return this.options.filter(n => n.selected); } });
    Object.defineProperty(node, "selectedIndex", { get() { return Math.max(0, this.options.findIndex(n => n.selected)); } });
    Object.assign(node, props); node.append(...children);
    if (props.id) elements.set(props.id, node);
    nodes.push(node); return node;
  }
  for (const id of ["tab-evaluations", "tab-events", "tab-intake", "tab-pricing", "tab-metrics", "tab-judges", "evaluation-new-slot"]) h("div", { id });
  elements.get("evaluation-new-slot").parentElement = h("div");
  const context = { h, $: id => elements.get(id), tabs: {}, URLSearchParams, Date, Set, Map, JSON, Number, crypto: { randomUUID: () => "test-id" },
    localStorage: { getItem: () => null, setItem() {} },
    document: { createElementNS: (_, tag) => h(tag) },
    iconButton: (_, label, onclick) => h("button", { title: label, onclick }, label),
    emptyPanel: text => h("empty", {}, text), errorPanel: text => h("error", {}, text), dataTable: (headers, rows) => h("table", { headers }, rows),
    notify() {}, creatorHeaders: () => ({}), agentFields: () => h("div"), memoryObjectHash: id => `#artifacts/${id}`,
    setInterval: fn => { timers.push(fn); return timers.length; }, clearInterval() {},
    api: async (...args) => { requests.push(args); return responder(...args); },
  };
  vm.createContext(context);
  for (const file of ["measurement-controls", page]) vm.runInContext(fs.readFileSync(new URL(`web/${file}.js`, import.meta.url), "utf8"), context);
  return { context, elements, nodes, requests, timers };
}
const settle = async () => { for (let i = 0; i < 5; i++) await new Promise(resolve => setImmediate(resolve)); };

test("event viewer bounds retained rows and closes only its viewer session", async () => {
  const f = setup("events", (method, path) => method === "POST" ? { id: "viewer" } : method === "DELETE" ? {} : { events: Array.from({ length: 1500 }, (_, i) => ({ id: String(i), workflow: "group/work", status: "completed" })), next_sequence: 1500 });
  f.context.tabs.events.show(); await settle();
  const feed = f.nodes.find(n => n.className === "event-feed");
  assert.equal(feed.children.length, 1000);
  assert.equal(feed.children[0].children[1].children[0], "group/work");
  f.context.tabs.events.hide(); await settle();
  assert.ok(f.requests.some(([m, p]) => m === "DELETE" && p.endsWith("/viewer")));
  assert.ok(f.requests.every(([, p]) => !p.includes("intake")));
});

test("text judge findings retain target, producer and artifact links", async () => {
  const f = setup("metrics", (_, route) => route.includes("/measurements?") ? { items: [{ metric_name: "explanation", text_value: "The output preserves evidence.", target_run_id: "target", producer_run_id: "producer", artifact_id: "runs/producer/output.json", status: "derived" }] } : { series: [], coverage: { known: 0, missing: 0 } });
  f.context.tabs.metrics.show(); f.elements.get("metric-name").value = "input_tokens"; f.elements.get("metric-name").onchange(); f.elements.get("metric-group").value = "model";
  f.elements.get("metric-from").value = "2026-11-21T09:00"; f.elements.get("metric-to").value = "2026-11-21T11:00";
  f.nodes.find(n => n.tag === "form").onsubmit({ preventDefault() {} }); await settle();
  assert.ok(f.nodes.some(n => n.tag === "p" && n.children.includes("The output preserves evidence.")));
  for (const href of ["#runs/target", "#runs/producer", "#artifacts/runs/producer/output.json"]) assert.ok(f.nodes.some(n => n.href === href));
});

test("event viewer discards a late session response after navigation", async () => {
  let resolve;
  const f = setup("events", method => method === "POST" ? new Promise(r => { resolve = r; }) : {});
  f.context.tabs.events.show(); await settle(); f.context.tabs.events.hide();
  resolve({ id: "late" }); await settle();
  assert.ok(f.requests.some(([m, p]) => m === "DELETE" && p.endsWith("/late")));
  assert.equal(f.requests.filter(([m]) => m === "GET").length, 0);
});

test("paused Events still receives arrivals and reports the retained range and cap", async () => {
  let count = 0;
  const f = setup("events", method => method === "POST" ? { id: "viewer" } : method === "DELETE" ? {} : { events: [{ id: String(++count), workflow: "group/work" }], stream_first_sequence: 1, stream_last_sequence: 9, next_sequence: count + 1 });
  f.context.tabs.events.show(); await settle();
  f.nodes.find(n => n.tag === "button" && n.children.includes("Pause")).onclick();
  await f.timers[0](); await settle();
  assert.equal(f.nodes.find(n => n.className === "event-feed").children.length, 2);
  assert.ok(f.nodes.some(n => n.textContent.includes?.("2 / 1,000 displayed · Stream 1–9")));
});

test("runtime and quality graphs offer only supported attribution", () => {
  const f = setup("metrics", () => ({})), metric = f.elements.get("metric-name"), group = f.elements.get("metric-group");
  metric.value = "duration_ms"; metric.onchange();
  assert.deepEqual(group.options.map(o => o.value), ["", "workflow", "run"]);
  metric.value = "quality"; metric.onchange();
  assert.deepEqual(group.options.map(o => o.value), ["", "run", "agent"]);
});

test("judge mapping fields and agent JSON stay synchronized", async () => {
  const definition = { schema_version: 1, models: ["fixture"], output_contract: { format: "json", mappings: [{ path: "/score", name: "quality", type: "number" }] } };
  const f = setup("judges", (_, route) => route.endsWith("/agents") ? { entries: [{ name: "group/judge" }] } : route.endsWith("/v1") ? { files: { "agent.json": definition, "systemprompt.md": "Return JSON" } } : { versions: [{ version: "v1" }] });
  await f.context.tabs.judges.show();
  const agent = f.elements.get("judge-agent"); agent.value = "group/judge"; agent.onchange(); await settle();
  const version = f.elements.get("judge-version"); version.value = "v1"; version.onchange(); await settle();
  const config = f.elements.get("judge-config");
  const path = f.nodes.find(n => n["aria-label"] === "JSON field path"); path.value = "/changed"; path.oninput();
  assert.equal(JSON.parse(config.value).output_contract.mappings[0].path, "/changed");
  const edited = JSON.parse(config.value); edited.output_contract.mappings[0].path = "/from-json"; config.value = JSON.stringify(edited); config.oninput();
  const mappings = f.nodes.find(n => n.className === "judge-mappings");
  assert.equal(mappings.children[0].mappingValue().path, "/from-json");
});

test("currency conversion editing preserves decimal strings and explicit currency", async () => {
  const f = setup("pricing", method => method === "GET" ? [] : {});
  f.context.tabs.pricing.show(); await settle();
  const kind = f.elements.get("pricing-kind"); kind.value = "conversions"; kind.onchange(); await settle();
  f.nodes.find(n => n.title === "New conversion").onclick();
  f.elements.get("price-from_currency").value = "USD"; f.elements.get("price-to_currency").value = "EUR"; f.elements.get("price-rate").value = "0.923456789";
  await f.nodes.find(n => n.className === "measurement-editor").onsubmit({ preventDefault() {} });
  const put = f.requests.find(([m]) => m === "PUT");
  assert.equal(put[2].rate, "0.923456789"); assert.equal(put[2].from_currency, "USD"); assert.equal(put[3]["If-None-Match"], "*");
});

test("graphs keep missing values as gaps and permit multiple series", async () => {
  const f = setup("metrics", () => ({ series: [{ key: "model-a", points: [{ time: "2026-11-21T10:00:00Z", value: "1" }, { time: "2026-11-21T10:01:00Z", value: null }, { time: "2026-11-21T10:02:00Z", value: "3" }] }, { key: "model-b", points: [{ time: "2026-11-21T10:00:00Z", value: "2" }] }], coverage: { known: 3, missing: 1 } }));
  f.context.tabs.metrics.show(); f.elements.get("metric-name").value = "input_tokens"; f.elements.get("metric-name").onchange(); f.elements.get("metric-group").value = "model";
  f.elements.get("metric-from").value = "2026-11-21T09:00"; f.elements.get("metric-to").value = "2026-11-21T11:00";
  f.nodes.find(n => n.tag === "form").onsubmit({ preventDefault() {} }); await settle();
  const lines = f.nodes.filter(n => n.attrs.class?.startsWith("plot-line"));
  assert.equal(lines.length, 2); assert.equal((lines[0].attrs.d.match(/M/g) || []).length, 2);
  assert.ok(f.requests.some(([, path]) => path.includes("group_by=model")));
});

test("factory metrics and evaluations have separate navigation", () => {
  const f = setup("metrics", () => ({}));
  const navs = f.nodes.filter(n => n.tag === "nav");
  assert.deepEqual(navs.find(n => n["aria-label"] === "Metrics").children.map(n => n.href), ["#metrics", "#pricing"]);
  assert.deepEqual(navs.find(n => n["aria-label"] === "Evaluations").children.map(n => n.href), ["#evaluations", "#judges"]);
  const app = fs.readFileSync(new URL("web/app.js", import.meta.url), "utf8");
  const c = vm.createContext({});
  vm.runInContext(app.slice(app.indexOf("const PAGES ="), app.indexOf("// Icons:")) + ";globalThis.pages=PAGES;globalThis.subpages=SUBPAGES;globalThis.groups=NAV_GROUPS;", c);
  assert.equal(c.pages.find(p => p.name === "overview").title, "Overview", "home is the live factory");
  assert.equal(c.pages.find(p => p.name === "metrics").title, "Metrics", "the explorer with its periods is a page of its own");
  assert.equal(c.pages.find(p => p.name === "evaluations").title, "Evals");
  assert.deepEqual(Array.from(c.groups.find(g => g.name === "Main").pages), ["overview", "metrics", "cluster", "system-diagram"]);
  assert.equal(c.subpages.metrics, undefined);
  assert.equal(c.subpages.pricing.menu, "metrics");
  assert.equal(c.subpages.judges.menu, "evaluations");
});

test("metrics loads actual runtime on entry without date entry and exposes custom dates only on request", async () => {
  const f = setup("metrics", (_, route) => route.includes("/series?") ? {series: [{key:"workflow",points:[{time:new Date().toISOString(),value:938}]}],coverage:{known:1,missing:0}} : {items:[]});
  f.context.tabs.metrics.show(); await settle();
  assert.equal(f.elements.get("metric-range").value, "7d");
  assert.equal(f.elements.get("metric-name").value, "duration_ms");
  assert.equal(f.elements.get("metric-from").parentElement.hidden, true);
  assert.ok(f.requests.some(([,route]) => route.includes("/series?") && route.includes("metric=duration_ms") && route.includes("group_by=workflow")));
  assert.ok(f.nodes.find(n => n.className === "measurement-chart").children.some(n => n.tag === "svg"));
  const range = f.elements.get("metric-range"); range.value="custom"; range.onchange();
  assert.equal(f.elements.get("metric-from").parentElement.hidden, false);
  range.value="24h"; range.onchange(); await settle();
  assert.equal(f.elements.get("metric-from").parentElement.hidden, true);
});

test("Sets and Judges keep category navigation above headed panels", () => {
  const f = setup("judges", () => ({}));
  for (const id of ["tab-evaluations", "tab-judges"]) {
    const page = f.elements.get(id);
    assert.equal(page.children[0].tag, "nav");
    assert.deepEqual(page.children[0].children.map(n => n.href), ["#evaluations", "#judges"]);
  }
  const page = f.elements.get("tab-judges");
  assert.equal(page.children[1].className, "panel list-panel");
  assert.equal(page.children[2].className, "panel content-panel");
  assert.equal(page.children[1].children[0].className, "panel-head");
  assert.equal(page.children[2].children[0].className, "panel-head");
});
