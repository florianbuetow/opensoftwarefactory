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
    titleCase: id => `Title(${id})`, midTruncate: (text, max) => text.length <= max ? text : `${text.slice(0, 4)}…`,
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
  const first = feed.children[0];
  assert.equal(first.children[1].children[0], "Title(group/work)", "the workflow is named as the factory names it");
  assert.equal(first.children[1].title, "group/work", "its stored name is the tooltip");
  assert.equal(first.children[2].children[0], "Title(completed)");
  assert.deepEqual(feed.parentElement.children[0].children.map(n => n.children[0]), ["Time", "Workflow", "Outcome"], "the feed has a heading row");
  first.onclick();
  assert.equal(first.attrs["aria-pressed"], "true", "the chosen event is marked");
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

test("currency conversion editing preserves decimal strings and saves by currency pair without an id", async () => {
  const stored = { revision: "c1", from_currency: "USD", to_currency: "EUR", rate: "0.92" };
  const f = setup("pricing", method => method === "GET" ? { conversions: [{ ...stored }] } : {});
  f.context.crypto.randomUUID = () => assert.fail("a conversion must not get a generated id");
  f.context.tabs.pricing.show(); await settle();
  const kind = f.elements.get("pricing-kind"); kind.value = "conversions"; kind.onchange(); await settle();
  const editor = () => f.nodes.find(n => n.className === "measurement-editor");
  const submit = () => editor().onsubmit({ preventDefault() {} });

  f.nodes.find(n => n.title === "New conversion").onclick();
  f.elements.get("price-from_currency").value = "EUR"; f.elements.get("price-to_currency").value = "USD"; f.elements.get("price-rate").value = "1.087654321";
  await submit(); await settle();
  const post = f.requests.find(([m]) => m === "POST");
  assert.equal(post[1], "/api/evaluations/conversions");
  assert.equal("id" in post[2], false, "a new conversion carries no id");
  assert.equal(post[2].rate, "1.087654321"); assert.equal(post[2].from_currency, "EUR"); assert.equal(post[2].to_currency, "USD");

  f.requests.length = 0;
  f.nodes.filter(n => n.tag === "table").pop().children[0].onclick();
  f.elements.get("price-rate").value = "0.923456789";
  await submit(); await settle();
  const put = f.requests.find(([m]) => m === "PUT");
  const query = new URLSearchParams(put[1].split("?")[1]);
  assert.equal(put[1].split("?")[0], "/api/evaluations/conversions");
  assert.deepEqual(Object.fromEntries(query), { from_currency: "USD", to_currency: "EUR" });
  assert.equal(put[3]["If-Match"], '"c1"');
  assert.equal(put[2].rate, "0.923456789");
  assert.equal("id" in put[2] || "revision" in put[2], false);

  f.requests.length = 0;
  f.nodes.filter(n => n.tag === "table").pop().children[0].onclick();
  f.elements.get("price-from_currency").value = "EUR"; f.elements.get("price-to_currency").value = "USD"; f.elements.get("price-rate").value = "1.1";
  await submit(); await settle();
  const writes = f.requests.filter(([m]) => m !== "GET");
  assert.deepEqual(writes.map(([m]) => m), ["DELETE", "POST"], "a changed currency pair deletes the old row and creates the new one");
  assert.deepEqual(Object.fromEntries(new URLSearchParams(writes[0][1].split("?")[1])), { from_currency: "USD", to_currency: "EUR" });
  assert.equal(writes[0][3]["If-Match"], '"c1"');
  assert.equal(writes[1][2].from_currency, "EUR"); assert.equal(writes[1][2].to_currency, "USD");
});

test("a price is saved by its key, never by a generated id", async () => {
  const stored = { revision: "r1", vendor: "v", model: "m", effort: "none", modes: { tier: "batch" }, from: "2026-11-22T17:09:44.341018Z", to: null, currency: "USD", unit_tokens: 1000, input_uncached: "1", input_cached: "0", output: "2" };
  const f = setup("pricing", method => method === "GET" ? { prices: [{ ...stored }] } : {});
  f.context.crypto.randomUUID = () => assert.fail("a price must not get a generated id");
  f.context.tabs.pricing.show(); await settle();
  const editor = () => f.nodes.find(n => n.className === "measurement-editor");
  const submit = () => editor().onsubmit({ preventDefault() {} });

  f.nodes.find(n => n.title === "New price").onclick();
  for (const [key, value] of Object.entries({ vendor: "v", model: "n", effort: "low", currency: "EUR", unit_tokens: "1000", input_uncached: "1", input_cached: "0", output: "2", from: "2026-01-01T00:00:00Z" })) f.elements.get(`price-${key}`).value = value;
  f.elements.get("price-open").checked = true; f.elements.get("price-modes").value = "{}";
  await submit(); await settle();
  const post = f.requests.find(([m]) => m === "POST");
  assert.equal(post[1], "/api/evaluations/prices", "a new price is created with POST on the collection");
  assert.equal("id" in post[2], false, "a new price carries no id"); assert.equal(post[2].from, "2026-01-01T00:00:00.000Z");

  f.requests.length = 0;
  f.nodes.filter(n => n.tag === "table").pop().children[0].onclick();
  f.elements.get("price-output").value = "3";
  await submit(); await settle();
  const put = f.requests.find(([m]) => m === "PUT");
  const query = new URLSearchParams(put[1].split("?")[1]);
  assert.equal(put[1].split("?")[0], "/api/evaluations/prices");
  assert.deepEqual(Object.fromEntries(query), { vendor: "v", model: "m", effort: "none", modes: '{"tier":"batch"}', from: "2026-11-22T17:09:44.341018Z" }, "an existing price is addressed by its key");
  assert.equal(put[3]["If-Match"], '"r1"');
  assert.equal(put[2].output, "3"); assert.equal(put[2].from, stored.from, "an unchanged start date keeps its stored text");
  assert.equal("id" in put[2] || "revision" in put[2], false);

  f.requests.length = 0;
  f.nodes.filter(n => n.tag === "table").pop().children[0].onclick();
  f.elements.get("price-from").value = "2026-12-01T00:00:00Z";
  await submit(); await settle();
  const writes = f.requests.filter(([m]) => m !== "GET");
  assert.deepEqual(writes.map(([m]) => m), ["DELETE", "POST"], "a new start date deletes the old row and creates the new one");
  assert.equal(new URLSearchParams(writes[0][1].split("?")[1]).get("from"), stored.from);
  assert.equal(writes[0][3]["If-Match"], '"r1"');
  assert.equal(writes[1][2].from, "2026-12-01T00:00:00.000Z");
});

test("prices are one sorted table that the filter narrows by model or vendor", async () => {
  const price = (vendor, model, effort, output) => ({ vendor, model, effort, currency: "USD", unit_tokens: 1000000, input_uncached: "3", input_cached: "0.30", output, from: "2026-11-22T17:09:44Z", to: null, modes: {} });
  const f = setup("pricing", method => method === "GET" ? { prices: [price("lm-studio", "qwen3", "high", "0"), price("anthropic", "claude-sonnet", "low", "15"), price("anthropic", "claude-sonnet", "none", "15")] } : {});
  f.context.tabs.pricing.show(); await settle();
  const cells = () => { const table = f.nodes.filter(n => n.tag === "table").pop(); return table.children.map(tr => tr.children.map(td => td.children[0])); };
  assert.deepEqual(Array.from(f.nodes.filter(n => n.tag === "table").pop().headers), ["Model", "Vendor", "Effort", "Input", "Cached input", "Output", "Per", "Currency", "Effective"]);
  assert.deepEqual(cells().map(row => row.slice(0, 3).join("/")), ["claude-sonnet/anthropic/none", "claude-sonnet/anthropic/low", "qwen3/lm-studio/high"], "sorted by vendor, model, then effort");
  assert.deepEqual(cells()[0].slice(3, 8), ["3", "0.30", "15", "1M tokens", "USD"], "rates stay the decimal strings that were stored");
  const filter = f.elements.get("pricing-filter");
  filter.value = "QWEN"; filter.oninput();
  assert.deepEqual(cells().map(row => row[0]), ["qwen3"], "the filter ignores case");
  filter.value = "anthropic"; filter.oninput();
  assert.equal(cells().length, 2, "the vendor is filtered too");
  filter.value = "nothing"; filter.oninput();
  assert.ok(f.nodes.some(n => n.role === "status" && n.children.includes("No model or vendor contains “nothing”")));
  filter.value = ""; filter.oninput();
  f.nodes.filter(n => n.tag === "table").pop().children[2].onclick();
  assert.equal(f.elements.get("price-model").value, "qwen3", "a click on a row edits that price");
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

test("metrics narrows every number to the runs of the chosen outcome", async () => {
  const f = setup("metrics", (_, route) => route.includes("/series?") ? { series: [], coverage: { known: 0, missing: 0 } } : { items: [] });
  f.context.tabs.metrics.show(); await settle();
  assert.ok(f.requests.every(([, route]) => !route.includes("outcome=")), "all runs count until an outcome is chosen");
  f.requests.length = 0;
  const outcome = f.elements.get("metric-outcome"); outcome.value = "failed"; outcome.onchange(); await settle();
  const series = f.requests.filter(([, route]) => route.includes("/series?"));
  assert.ok(series.length >= 5, "the chart and the four totals ask again");
  assert.ok(series.every(([, route]) => route.includes("outcome=failed")));
});

test("metrics loads actual runtime on entry without date entry and exposes custom dates only on request", async () => {
  const f = setup("metrics", (_, route) => route.includes("/series?") ? {series: [{key:"workflow",points:[{time:new Date().toISOString(),value:938}]}],coverage:{known:1,missing:0}} : {items:[]});
  f.context.tabs.metrics.show(); await settle();
  assert.equal(f.elements.get("metric-range").value, "7d");
  assert.equal(f.elements.get("metric-name").value, "duration_ms");
  assert.equal(f.elements.get("metric-from").parentElement.hidden, true);
  assert.ok(f.requests.some(([,route]) => route.includes("/series?") && route.includes("metric=duration_ms") && route.includes("group_by=workflow")));
  assert.equal(f.nodes.filter(n => n.className === "metric-bar-row").length, 1, "one measured workflow is one bar");
  const apply = f.nodes.find(n => n.tag === "button" && n.children.includes("Apply"));
  assert.equal(apply.hidden, true, "a preset reloads at once and needs no Apply");
  const range = f.elements.get("metric-range"); range.value="custom"; range.onchange();
  assert.equal(f.elements.get("metric-from").parentElement.hidden, false);
  assert.equal(apply.hidden, false, "typed dates wait for Apply");
  range.value="24h"; range.onchange(); await settle();
  assert.equal(f.elements.get("metric-from").parentElement.hidden, true);
  assert.equal(apply.hidden, true);
});

const textOf = node => typeof node === "string" ? node : [node.textContent, ...(node.children || []).map(textOf)].join(" ").replace(/\s+/g, " ").trim();
const factoryWeek = (_, route) => {
  if (!route.includes("/series?")) return { items: [] };
  const at = "2026-11-23T21:00:00Z", point = value => ({ time: at, value, status: "known" });
  if (route.includes("group_by=run")) return { series: [{ key: "run-a", points: [point(16952)] }, { key: "run-b", points: [point(4318)] }, { key: "run-c", points: [point(30621)] }], coverage: { known: 3, missing: 0 } };
  if (route.includes("metric=cost")) return { series: [], coverage: { known: 0, missing: 15 } };
  if (route.includes("metric=input_tokens")) return { series: [{ key: "<nil>", points: [point(567), point(11833)] }], coverage: { known: 2, missing: 9 } };
  if (route.includes("metric=output_tokens")) return { series: [{ key: "<nil>", points: [point(100)] }], coverage: { known: 1, missing: 0 } };
  return { series: [{ key: "default/workflow-1", points: [point(21270)] }, { key: "egm/full", points: [point(30621)] }], coverage: { known: 2, missing: 0 }, projection: { status: "refreshing" } };
};

test("metrics leads with the totals of the period, each with its unit, and says Unknown instead of zero", async () => {
  const f = setup("metrics", factoryWeek);
  f.context.tabs.metrics.show(); await settle();
  const tiles = f.nodes.filter(n => n.className === "metric-tile").map(textOf);
  assert.deepEqual(tiles.slice(-4), ["Unknown Cost 15 values missing", "3 Runs", "51.9 s Runtime", "12.5k Tokens 9 values missing"]);
  assert.ok(f.requests.some(([, route]) => route.includes("metric=cost") && route.includes("currency=EUR")), "the cost total asks in the display currency");
});

test("metrics compares the groups as ranked bars and draws no trend from single points", async () => {
  const f = setup("metrics", factoryWeek);
  f.context.tabs.metrics.show(); await settle();
  assert.deepEqual(f.nodes.filter(n => n.className === "metric-bar-row").map(textOf), ["Title(egm/full) 30.6 s", "Title(default/workflow-1) 21.3 s"]);
  assert.ok(f.nodes.some(n => n.tag === "h2" && n.textContent === "Runtime by workflow"));
  const bars = f.nodes.filter(n => n.className === "metric-bar").map(n => n.style);
  assert.equal(bars[0], "width:100%"); assert.ok(parseFloat(bars[1].slice(6)) < 100);
  assert.equal(f.nodes.find(n => n.className === "metric-trend").hidden, true, "one measured hour per workflow is no line");
  assert.equal(f.nodes.find(n => n.className === "measurement-chart").children.length, 0);
  assert.equal(f.nodes.filter(n => n.type === "checkbox").length, 0, "no legend of checkboxes");
});

test("the metrics status line names only what is still counting or missing", async () => {
  const f = setup("metrics", factoryWeek);
  f.context.tabs.metrics.show(); await settle();
  const status = f.nodes.find(n => n.role === "status" && n.className.includes("metric-status"));
  assert.equal(textOf(status), "Latest hours still counting");
  assert.equal(f.nodes.filter(n => n.className === "metric-warning").length, 0);
  const metric = f.elements.get("metric-name"); metric.value = "input_tokens"; metric.onchange();
  f.elements.get("metric-group").value = "workflow"; f.elements.get("metric-group").onchange(); await settle();
  assert.equal(textOf(status), "9 values missing");
  assert.equal(f.nodes.filter(n => n.className === "metric-warning").map(textOf).pop(), "9 values missing");
  assert.deepEqual(f.nodes.filter(n => n.className === "metric-bar-row").map(textOf).slice(-1), ["Unattributed 12.4k tokens"]);
});

test("the metrics trend has day ticks and values with units on the axis and in the hover text", async () => {
  const points = [{ time: "2026-11-21T10:00:00Z", value: 30621 }, { time: "2026-11-24T10:00:00Z", value: 12000 }];
  const f = setup("metrics", (_, route) => route.includes("/series?") ? { series: [{ key: "egm/full", points }], coverage: { known: 2, missing: 0 } } : { items: [] });
  f.context.tabs.metrics.show(); await settle();
  const range = f.elements.get("metric-range"); range.value = "custom"; range.onchange();
  f.elements.get("metric-from").value = "2026-11-20T00:00"; f.elements.get("metric-to").value = "2026-11-27T00:00";
  f.nodes.find(n => n.tag === "form").onsubmit({ preventDefault() {} }); await settle();
  assert.equal(f.nodes.find(n => n.className === "metric-trend").hidden, false);
  const labels = f.nodes.filter(n => n.attrs.class === "plot-label").map(n => n.textContent);
  const day = iso => new Date(iso).toLocaleDateString(undefined, { day: "numeric", month: "short" });
  for (const iso of ["2026-11-20T00:00", "2026-11-23T00:00", "2026-11-27T00:00"]) assert.ok(labels.includes(day(iso)), `a tick for ${day(iso)}`);
  assert.ok(labels.every(label => !/\d\d:\d\d:\d\d/.test(label)), "no seconds on the time axis");
  assert.ok(labels.includes("0.0 s") && labels.includes("40.0 s"), "the value axis carries the unit");
  const hover = f.nodes.filter(n => n.tag === "title").map(n => n.textContent);
  assert.ok(hover.some(text => text.startsWith("Title(egm/full): 30.6 s · ")));
});

test("Graphs and Prices are named by their tab alone", () => {
  for (const id of ["metrics", "pricing"]) {
    const f = setup(id, () => ({}));
    assert.ok(!f.nodes.some(n => n.className === "panel-head"), `${id} repeats no heading under its tab`);
  }
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
