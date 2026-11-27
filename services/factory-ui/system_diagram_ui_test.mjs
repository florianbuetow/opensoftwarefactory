import assert from "node:assert/strict";
import fs from "node:fs";
import test from "node:test";
import vm from "node:vm";

const read = (name) => fs.readFileSync(new URL(`web/${name}`, import.meta.url), "utf8");
const source = read("system-diagram.mmd");

// A stand-in page: elements that keep their attributes, children and listeners.
function page({ width = 1400, height = 800, stored = null } = {}) {
  const made = [];
  const element = (tag) => {
    const node = {
      tag, attrs: {}, children: [], listeners: {}, dataset: {}, classes: new Set(), style: {}, hidden: false,
      setAttribute(k, v) { this.attrs[k] = String(v); if (k.startsWith("data-")) this.dataset[k.slice(5)] = String(v); },
      append(...items) { this.children.push(...items); },
      replaceChildren(...items) { this.children = items.filter((item) => item !== null && item !== undefined); },
      addEventListener(type, fn) { this.listeners[type] = fn; },
      getBoundingClientRect: () => ({ left: 0, top: 0, bottom: 0, width, height }),
      setPointerCapture() {},
      classList: { add: (...names) => names.forEach((n) => node.classes.add(n)), remove: (...names) => names.forEach((n) => node.classes.delete(n)) },
      closest(selector) { return selector.split(",").map((s) => s.trim().slice(1)).some((name) => String(node.attrs.class || "").split(" ").includes(name)) ? node : null; },
    };
    made.push(node);
    return node;
  };
  const body = element("div"), actions = element("span");
  const store = new Map(stored ? [["factory.systemDiagram.positions", JSON.stringify(stored)]] : []);
  const context = {
    $: (id) => (id === "system-diagram" ? body : actions), tabs: {},
    h: (tag, props = {}, ...children) => Object.assign(element(tag), props, { children: children.filter((child) => child !== null && child !== undefined) }),
    document: { createElementNS: (_ns, tag) => element(tag) },
    localStorage: { getItem: (k) => store.get(k) ?? null, setItem: (k, v) => store.set(k, v), removeItem: (k) => store.delete(k) },
    emptyPanel: (message) => ({ message }), errorPanel: (message) => ({ message }),
    fetch: async (url) => { assert.equal(url, "system-diagram.mmd"); return { ok: true, text: async () => source }; },
    Math, JSON, Object, Number, String, Map, Set,
  };
  vm.runInNewContext(`${read("system-diagram.js")}\nglobalThis.logic = SystemDiagramLogic;`, context);
  const svg = () => body.children[0], card = () => body.children[1];
  // What is on the canvas now: the scene's children, since a redraw replaces them.
  const shown = (name) => svg().children[0].children.filter((n) => String(n.attrs.class || "").split(" ").includes(name));
  const open = async () => { context.tabs["system-diagram"].show(); await new Promise((resolve) => setImmediate(resolve)); };
  return { context, logic: context.logic, body, actions, store, svg, card, shown, open };
}
const plain = (value) => JSON.parse(JSON.stringify(value));
const overlap = (a, b) => a.x < b.x + b.w && b.x < a.x + a.w && a.y < b.y + b.h && b.y < a.y + a.h;
const textOf = (node) => (typeof node === "string" ? node : (node.children || []).map(textOf).join(" "));

test("the page is titled System Diagram, never Components", () => {
  const html = read("index.html");
  const section = html.slice(html.indexOf('id="tab-system-diagram"'), html.indexOf("</section>", html.indexOf('id="tab-system-diagram"')));
  assert.match(section, /<h2>System Diagram<\/h2>/);
  assert.doesNotMatch(section, /Components/);
});

test("the source keeps every group and connects the LLM gateway to its LLM providers", () => {
  const { logic } = page();
  const model = logic.parse(source);
  assert.deepEqual(plain(model.groups.map((g) => g.title)), ["Control room", "Workflow execution", "Agent runtime", "LLM providers", "State and catalogs", "Git and development", "Observability"]);
  const label = (id) => model.nodes.find((n) => n.id === id).label;
  const fromGateway = model.edges.filter((e) => e.from === "llm" && model.nodes.find((n) => n.id === e.to).group === "providers");
  assert.deepEqual(plain(fromGateway.map((e) => [label(e.to), e.dashed, e.label])), [["LM Studio", false, ""], ["OpenAI-compatible APIs", true, "when configured"]]);
  assert.equal(model.nodes.find((n) => n.id === "blobs").shape, "store");
  assert.equal(model.nodes.find((n) => n.id === "operator").group, "", "a component outside every group stays outside");
  assert.ok(model.edges.some((e) => e.from === "orchestrator" && e.to === "temporal" && e.both), "a two-way connection is read as one");
  for (const edge of model.edges) for (const id of [edge.from, edge.to]) assert.ok(model.nodes.some((n) => n.id === id), `${id} is a component`);
});

test("every component and every group says what it is, in one sentence without a period", () => {
  const { logic } = page();
  const model = logic.parse(source);
  for (const item of [...model.nodes, ...model.groups]) {
    assert.ok(item.description.length > 20, `${item.id} has a description`);
    assert.doesNotMatch(item.description, /\.$/, `${item.id}: a tooltip has no trailing period`);
  }
});

test("the layout is layered: the flow reads from the left, groups stand in columns, nothing overlaps", () => {
  const { logic } = page();
  const model = logic.parse(source), positions = logic.layout(model, 1400 / 800);
  const nodes = Object.values(positions);
  assert.equal(nodes.length, model.nodes.length, "every component is placed");
  for (let i = 0; i < nodes.length; i++) for (let j = i + 1; j < nodes.length; j++) assert.ok(!overlap(nodes[i], nodes[j]), "two components overlap");
  const box = (id) => logic.groupRect(model, positions, id), groups = model.groups.map((g) => box(g.id));
  for (let i = 0; i < groups.length; i++) for (let j = i + 1; j < groups.length; j++) assert.ok(!overlap(groups[i], groups[j]), "two groups overlap");
  // Every call from one group into another goes to the right, never back: that is the flow.
  const group = (id) => model.nodes.find((n) => n.id === id).group;
  for (const e of model.edges.filter((x) => !x.dashed && group(x.from) && group(x.to) && group(x.from) !== group(x.to))) {
    assert.ok(box(group(e.to)).x > box(group(e.from)).x, `${e.from} calls ${e.to}, which stands to its right`);
  }
  assert.ok(positions.operator.x < Math.min(...groups.map((g) => g.x)), "the operator, who starts everything, is leftmost");
  // Groups of one layer share a column; the page is used in both directions.
  const columns = new Set(groups.map((g) => Math.round(g.x + g.w / 2)));
  assert.ok(columns.size >= 4 && columns.size < groups.length, `${columns.size} columns for ${groups.length} groups`);
  assert.equal(Math.round(box("providers").x + box("providers").w / 2), Math.round(box("state").x + box("state").w / 2), "the groups the agent runtime calls stand in one column");
  const all = logic.bounds(model, positions);
  assert.ok(all.w / all.h < 2.6 && all.w / all.h > 1, `the drawing is ${Math.round(all.w)} by ${Math.round(all.h)}, close to the panel's shape`);
  // A tall, narrow panel turns the flow: from the top down.
  const turned = logic.layout(model, 0.6), tall = logic.bounds(model, turned);
  assert.ok(tall.h > tall.w, "in a tall panel the layers run from the top down");
  assert.ok(turned.operator.y < Math.min(...model.groups.map((g) => logic.groupRect(model, turned, g.id).y)));
});

test("a connection leaves at the side that faces the other component, and fitting shows the whole drawing", () => {
  const { logic } = page();
  const beside = logic.edgeRoute({ x: 0, y: 0, w: 100, h: 40 }, { x: 300, y: 0, w: 100, h: 40 });
  assert.equal(beside.d, "M100.0 20.0 L300.0 20.0", "two in one row: a straight line from side to side");
  assert.equal(beside.endAngle, 0, "the arrowhead points the way the line arrives");
  const lower = logic.edgeRoute({ x: 0, y: 0, w: 100, h: 40 }, { x: 300, y: 200, w: 100, h: 40 });
  assert.match(lower.d, /^M100\.0 20\.0 C[\d. -]+ 300\.0 220\.0$/, "another row: one curve from the right side to the left side");
  assert.equal(lower.endAngle, 0);
  const below = logic.edgeRoute({ x: 0, y: 0, w: 100, h: 40 }, { x: 0, y: 52, w: 100, h: 40 });
  assert.equal(below.d, "M50.0 40.0 L50.0 52.0", "the next one in a column: straight down");
  assert.equal(below.endAngle, Math.PI / 2);
  const view = logic.fit({ x: 100, y: 50, w: 2000, h: 500 }, 1000, 600);
  assert.ok(view.k < 1, "a drawing wider than the view is made smaller");
  assert.ok(100 * view.k + view.x >= 31 && (100 + 2000) * view.k + view.x <= 969, "all of it is inside the view, with a margin");
});

test("a connection goes around what is in its way, and none crosses a component in the opening layout", () => {
  const { logic } = page();
  const a = { x: 0, y: 0, w: 100, h: 40 }, b = { x: 400, y: 0, w: 100, h: 40 }, between = { x: 200, y: 0, w: 100, h: 40 };
  const around = logic.edgeRoute(a, b, [between]);
  assert.equal(around.shape, "lane", "it takes a lane beside the component");
  assert.equal(around.crossings, 0);
  assert.ok(Math.abs(around.mid.y - 20) > 26, "its middle passes beside the component, not through it");
  assert.match(around.d, / Q/, "its corners are rounded");
  // Two in one column with one between them: a small bracket beside the column.
  const top = { x: 0, y: 0, w: 100, h: 40 }, middle = { x: 0, y: 52, w: 100, h: 40 }, bottom = { x: 0, y: 104, w: 100, h: 40 };
  const bracket = logic.edgeRoute(top, bottom, [middle]);
  assert.equal(bracket.crossings, 0);
  assert.match(bracket.d, /^M100\.0 20\.0 C[\d. -]+ 100\.0 124\.0$/, "out of the right side and back into the right side");
  // The factory's own diagram, as the page opens it.
  const model = logic.parse(source), positions = logic.layout(model, 1400 / 800);
  const crossing = model.edges.filter((e) => logic.edgeRoute(positions[e.from], positions[e.to], Object.entries(positions).filter(([id]) => id !== e.from && id !== e.to).map(([, r]) => r)).crossings > 0);
  assert.deepEqual(plain(crossing.map((e) => `${e.from} to ${e.to}`)), []);
});

test("the card says what a component, a group and a connection are, and what they connect to", () => {
  const { logic } = page();
  const model = logic.parse(source);
  const llm = logic.describe(model, { kind: "node", id: "llm" });
  assert.equal(llm.title, "LLM gateway");
  assert.match(llm.text, /^The one door to language models/);
  assert.deepEqual(plain(llm.lines), ["In: Agent runtime", "Calls: LM Studio", "Called by: Agent service", "Also reaches: OpenAI-compatible APIs (when configured), Evaluation (token usage), Telemetry collector (traces)"]);
  const temporal = logic.describe(model, { kind: "node", id: "temporal" });
  assert.ok(temporal.lines.includes("Calls: Orchestrator") && temporal.lines.includes("Called by: Orchestrator"), "a two-way connection counts both ways");
  const group = logic.describe(model, { kind: "group", id: "providers" });
  assert.deepEqual(plain(group), { title: "LLM providers", text: "Where the language models run", lines: ["Holds: LM Studio, OpenAI-compatible APIs"] });
  const index = (from, to) => model.edges.findIndex((e) => e.from === from && e.to === to);
  assert.deepEqual(plain(logic.describe(model, { kind: "edge", index: index("llm", "lmstudio") })), { title: "LLM gateway → LM Studio", text: "LLM gateway calls LM Studio", lines: [] });
  assert.deepEqual(plain(logic.describe(model, { kind: "edge", index: index("llm", "evaluation") })), { title: "LLM gateway → Evaluation", text: "LLM gateway reaches Evaluation: token usage", lines: ["Dashed: beside the main flow of calls"] });
  assert.equal(logic.describe(model, { kind: "edge", index: index("orchestrator", "temporal") }).title, "Orchestrator ↔ Temporal");
  for (const item of [llm, group]) for (const line of [item.text, ...item.lines]) assert.doesNotMatch(line, /\.$/);
});

test("the page draws every group, component and connection as its own drawing, not as Mermaid", async () => {
  const ui = page();
  await ui.open();
  const model = ui.logic.parse(source);
  assert.equal(ui.svg().tag, "svg");
  assert.equal(ui.shown("sd-group").length, 7);
  assert.equal(ui.shown("sd-node").length, model.nodes.length);
  assert.equal(ui.shown("sd-edge").length, model.edges.length);
  assert.match(ui.svg().children[0].attrs.transform, /^translate\(.+\) scale\(.+\)$/, "it opens fitted to the panel");
  assert.deepEqual(ui.actions.children.map((b) => b["aria-label"]), ["Zoom out", "Zoom in", "Fit", "Reset layout"]);
  assert.ok(ui.shown("sd-edge").every((edge) => edge.children[0].attrs.class === "sd-hit"), "every connection has a wide line for the pointer to find");
  assert.match(ui.shown("sd-node").find((n) => n.dataset.node === "llm").attrs["aria-label"], /^LLM gateway\. The one door to language models/, "a screen reader hears what the card shows");
});

test("the pointer over a component, a connection or a group shows its card and marks what belongs to it", async () => {
  const ui = page();
  await ui.open();
  const model = ui.logic.parse(source);
  const move = (target, x = 500, y = 300) => ui.svg().listeners.pointermove({ clientX: x, clientY: y, target });
  const classOf = (part) => part.attrs.class.split(" ");
  assert.equal(ui.card().hidden, true, "no card until the pointer is on something");
  // A component: its card, its connections lit, every other connection dimmed.
  const llm = ui.shown("sd-node").find((n) => n.dataset.node === "llm"), before = ui.shown("sd-node");
  move(llm);
  assert.equal(ui.card().hidden, false);
  assert.match(textOf(ui.card()), /^LLM gateway The one door to language models.* Calls: LM Studio /);
  assert.equal(ui.card().style.left, "516px");
  assert.deepEqual(ui.shown("sd-node"), before, "nothing is redrawn under the pointer, so the keyboard focus stays");
  const edges = ui.shown("sd-edge");
  model.edges.forEach((e, i) => assert.ok(classOf(edges[i]).includes(e.from === "llm" || e.to === "llm" ? "sd-lit" : "sd-dim"), `${e.from} to ${e.to}`));
  assert.ok(classOf(ui.shown("sd-node").find((n) => n.dataset.node === "lmstudio")).includes("sd-lit"), "the other end is marked too");
  // A connection: found through its wide line, with its two ends marked.
  const call = model.edges.findIndex((e) => e.from === "agent" && e.to === "mcp");
  move(edges[call]);
  assert.match(textOf(ui.card()), /^Agent service → MCP gateway Agent service calls MCP gateway/);
  assert.deepEqual(ui.shown("sd-node").filter((n) => classOf(n).includes("sd-lit")).map((n) => n.dataset.node).sort(), ["agent", "mcp"]);
  // A group: what it is and what it holds.
  move(ui.shown("sd-group").find((g) => g.dataset.group === "observability"));
  assert.match(textOf(ui.card()), /^Observability The logs and traces of everything that runs Holds: Telemetry collector, Log collector, OpenObserve|^Observability .* Holds: /);
  // The empty canvas, and leaving it: no card, nothing dimmed.
  move({ closest: () => null });
  assert.equal(ui.card().hidden, true);
  assert.ok(ui.shown("sd-edge").every((edge) => !classOf(edge).includes("sd-dim")));
  move(llm);
  ui.svg().listeners.pointerleave({});
  assert.equal(ui.card().hidden, true);
  // The keyboard: a focused component shows the same card.
  ui.svg().listeners.focusin({ target: llm });
  assert.equal(ui.card().hidden, false);
  assert.match(textOf(ui.card()), /^LLM gateway /);
  ui.svg().listeners.focusout({});
  assert.equal(ui.card().hidden, true);
});

test("the wheel and the buttons zoom, and a drag on the empty canvas moves the view", async () => {
  const ui = page();
  await ui.open();
  const scale = () => Number(/scale\(([^)]+)\)/.exec(ui.svg().children[0].attrs.transform)[1]);
  const offset = () => /translate\(([^ ]+) ([^)]+)\)/.exec(ui.svg().children[0].attrs.transform).slice(1).map(Number);
  const before = scale();
  ui.svg().listeners.wheel({ deltaY: -200, clientX: 700, clientY: 400, preventDefault() {} });
  assert.ok(scale() > before, "wheel up zooms in");
  ui.actions.children[0].onclick();
  ui.actions.children[0].onclick();
  assert.ok(scale() < before, "Zoom out makes it smaller");
  ui.actions.children[2].onclick();
  assert.equal(scale(), before, "Fit shows the whole diagram again");
  const [x, y] = offset(), canvas = { closest: () => null };
  ui.svg().listeners.pointerdown({ button: 0, clientX: 100, clientY: 100, pointerId: 1, target: canvas, preventDefault() {} });
  ui.svg().listeners.pointermove({ clientX: 160, clientY: 130, target: canvas });
  ui.svg().listeners.pointerup({});
  assert.deepEqual(offset(), [x + 60, y + 30]);
});

test("a component can be dragged, a group moves with all it holds, and both are remembered", async () => {
  const ui = page();
  await ui.open();
  const rectOf = (id) => ui.shown("sd-node").find((n) => n.dataset.node === id).children[0].attrs;
  const scale = Number(/scale\(([^)]+)\)/.exec(ui.svg().children[0].attrs.transform)[1]);
  const drag = (target, dx, dy) => {
    ui.svg().listeners.pointerdown({ button: 0, clientX: 0, clientY: 0, pointerId: 1, target, preventDefault() {} });
    ui.svg().listeners.pointermove({ clientX: dx, clientY: dy, target });
    ui.svg().listeners.pointerup({});
  };
  const before = { ...rectOf("llm") };
  drag(ui.shown("sd-node").find((n) => n.dataset.node === "llm"), 50 * scale, 20 * scale);
  assert.ok(Math.abs(Number(rectOf("llm").x) - (Number(before.x) + 50)) <= 1);
  assert.ok(Math.abs(Number(rectOf("llm").y) - (Number(before.y) + 20)) <= 1);
  assert.equal(JSON.parse(ui.store.get("factory.systemDiagram.positions")).llm.x, Number(rectOf("llm").x), "the place is remembered");
  assert.equal(ui.card().hidden, true, "no card while something is dragged");
  const git = Number(rectOf("git").x), pods = Number(rectOf("pods").x), agent = Number(rectOf("agent").x);
  drag(ui.shown("sd-group").find((g) => g.dataset.group === "development"), 30 * scale, 0);
  assert.ok(Math.abs(Number(rectOf("git").x) - (git + 30)) <= 1 && Math.abs(Number(rectOf("pods").x) - (pods + 30)) <= 1, "the group's components move together");
  assert.equal(Number(rectOf("agent").x), agent, "another group stays where it is");
  // Reset layout puts everything back and forgets the remembered places.
  ui.actions.children[3].onclick();
  assert.equal(Number(rectOf("llm").x), Number(before.x));
  assert.equal(ui.store.has("factory.systemDiagram.positions"), false);
  // A remembered place is used the next time the page opens.
  const again = page({ stored: { llm: { x: 5, y: 7 } } });
  await again.open();
  const kept = again.shown("sd-node").find((n) => n.dataset.node === "llm").children[0].attrs;
  assert.deepEqual([Number(kept.x), Number(kept.y)], [5, 7]);
});
