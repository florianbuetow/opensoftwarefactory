import { test } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";

const read = (name) => fs.readFileSync(new URL(`web/${name}`, import.meta.url), "utf8");

// A minimal SVG document: elements with attributes, children and text.
function fakeDocument() {
  const make = (tag) => {
    const node = {
      tag, attrs: {}, children: [], listeners: {}, textContent: "", innerHTML: "", parentElement: { clientWidth: 800 },
      setAttribute(k, v) { this.attrs[k] = String(v); },
      getAttribute(k) { return this.attrs[k]; },
      appendChild(c) { this.children.push(c); return c; },
      replaceChildren(...c) { this.children = c.filter((x) => x && typeof x === "object"); },
      addEventListener(type, fn) { (this.listeners[type] ||= []).push(fn); },
    };
    return node;
  };
  return { createElementNS: (_ns, tag) => make(tag), make };
}

const all = (n) => [n, ...n.children.flatMap(all)];

function graph() {
  const document = fakeDocument();
  const app = read("app.js");
  const context = { document, console };
  const icons = app.slice(app.indexOf("const ICONS = {"), app.indexOf("};", app.indexOf("const ICONS = {")) + 2);
  const workflows = read("workflows.js");
  vm.runInNewContext(`${icons}\n${app.slice(app.indexOf("const titleCase"), app.indexOf("// midTruncate"))}\n${read("workflowref.js")}\n${workflows.slice(0, workflows.indexOf("// ---- Page ----"))}\n${read("graph.js")}\nglobalThis.G = WorkflowGraph;`, context);
  return { G: context.G, document };
}

const demo = {
  name: "demo",
  steps: [
    { id: "plan", service: "agent", agent: "planner@v1", position: { x: 0, y: 40 } },
    { id: "approve", service: "gate", question: "Ship it?", depends_on: ["plan"], on_reject: "plan", position: { x: 0, y: 176 } },
    { id: "build", service: "agent", agent: "builder@v1", depends_on: ["approve"], position: { x: 0, y: 312 } },
  ],
};

test("drawGraph draws tiles and arrows without editing, and a run's states as dot and word", () => {
  const { G, document } = graph();
  const svg = document.make("svg");
  const states = { plan: "done", approve: "waiting", build: "idle" };
  G.drawGraph(svg, demo, { stateOf: (id) => states[id], idPrefix: "run-" });
  const nodes = all(svg);
  const tiles = nodes.filter((n) => /\btile\b/.test(n.attrs.class || ""));
  assert.deepEqual(tiles.map((t) => t.attrs["data-step"]), ["plan", "approve", "build"]);
  assert.match(tiles[1].attrs.class, /kind-gate/);
  assert.match(tiles[1].attrs.class, /state-waiting/, "the tile carries its state for the state colour");
  assert.equal(tiles[1].attrs["aria-label"], "Step Approve: Ship it?, Waiting");
  // The state is a dot and a word on the second line, never colour alone.
  const words = nodes.filter((n) => n.attrs.class === "sub state-word").map((n) => n.textContent);
  assert.deepEqual(words, ["Completed", "Waiting", "Idle"], "the words of the Steps list");
  assert.equal(nodes.filter((n) => /^state-dot/.test(n.attrs.class || "")).length, 3);
  // Two arrows and the gate's back edge; markers named by the prefix, so two graphs on one page never share them.
  assert.equal(nodes.filter((n) => n.attrs.class === "edge").length, 2);
  assert.equal(nodes.filter((n) => n.attrs.class === "back-edge").length, 1);
  assert.ok(nodes.some((n) => n.tag === "marker" && n.attrs.id === "run-arrow"));
  // Nothing edits: no listeners anywhere, no Input node unless asked for.
  assert.ok(nodes.every((n) => !Object.keys(n.listeners).length && !n.onclick && !n.onpointerdown));
  assert.ok(!nodes.some((n) => /input-tile/.test(n.attrs.class || "")));
});

test("drawGraph hands each tile and its ports to onTile, and fitView keeps the whole graph in view", () => {
  const { G, document } = graph();
  const seen = [];
  G.drawGraph(document.make("svg"), demo, { onTile: (s, parts) => seen.push([s.id, !!parts.out, !!parts.rejected]) });
  assert.deepEqual(seen, [["plan", true, false], ["approve", true, true], ["build", true, false]]);
  const view = G.fitView(demo, 400, { input: false, height: 300, minScale: 0.1 });
  const b = G.bounds(demo, false);
  assert.ok(b.minX * view.scale + view.ox >= 0 && b.maxX * view.scale + view.ox <= 400, "fits the width");
  assert.ok(b.minY * view.scale + view.oy >= 0 && b.maxY * view.scale + view.oy <= 300, "fits the height");
});

test("the Runs detail draws the run's workflow version above its steps, in the states of the Steps list", () => {
  const runs = read("runs.js"), html = read("index.html"), style = read("style.css");
  assert.ok(html.indexOf('src="graph.js"') < html.indexOf('src="workflows.js"') && html.indexOf('src="graph.js"') < html.indexOf('src="runs.js"'), "loaded before its users");
  // One source of truth: the graph's tiles take their state from the same stepState as the rows.
  assert.match(runs, /const graph = runGraph\(graphDef, stepState, \(id\) => \{/);
  assert.match(runs, /const wrap = WorkflowGraph\.viewer\(def, \{/, "drawn by the shared viewer");
  assert.match(runs, /stateOf, selected, onTile, onEnlarge: opts\.onEnlarge,/);
  // A waiting gate opens its decision; any other tile picks out its row, which keeps the pick across redraws.
  assert.match(runs, /if \(gate\) \{ location\.hash = `#decisions\/\$\{encodeURIComponent\(`p:\$\{gate\.workflow_id\}\|\$\{gate\.step\}`\)\}`; return; \}/);
  assert.match(runs, /dataset: \{ step: id \} \}, statusChip\(stepState\(id\)\)/);
  assert.match(runs, /focusedStep = \{ trace, id \};/);
  // Above the dev pod and the Steps, under the run's error line.
  assert.ok(runs.indexOf("        graph,\n        podSection(") > runs.indexOf('h("p", { className: "error-line"'));
  // The states read as a dot and a word in the state colour; a running dot pulses unless motion is reduced.
  assert.match(style, /\.tile\.state-failed \.body \{ stroke: var\(--failed\); stroke-width: 2; \}/);
  assert.match(style, /\.tile\.state-waiting \.state-dot \{ fill: var\(--waiting\); \}/);
  assert.match(style.slice(style.lastIndexOf("@media (prefers-reduced-motion: reduce)")), /\.tile\.state-running \.state-dot/);
});

test("the run's graph pans, zooms, marks the steps the run is at, and opens nearly full screen", () => {
  const runs = read("runs.js"), style = read("style.css"), app = read("app.js"), viewer = read("graph.js"), workflows = read("workflows.js");
  // One component for both pages: the viewer and the enlarged dialog live in graph.js, and
  // neither page has a pan, a zoom or an enlarged dialog of its own.
  assert.match(viewer, /\n  function viewer\(def, opts = \{\}\) \{/);
  assert.match(viewer, /\n  function openLarge\(\{ title, def, stateOf = null, onTile = \(\) => \{\}, viewKey = "", label = "", graph = \{\}, onOpen = null, openLabel = "", openTip = "" \}\) \{/);
  // A run leads to its workflow three ways (bug report, 26 Nov: the jump was only inside the
  // enlarged view): the workflow link in its status line, a double-click on a tile, and
  // Open workflow beside Close in the enlarged view. All open the version the run used.
  assert.match(runs, /const workflowHash = \(step = ""\) => WorkflowLogic\.formatHash\(\{ workflow: workflowName, step, version: version \|\| 0 \}\);/);
  assert.match(runs, /h\("a", \{ className: "run-version", href: workflowHash\(\),/);
  assert.match(runs, /onOpen: workflowName \? \(id\) => \{ location\.hash = workflowHash\(id\); \} : null \}\);/);
  assert.match(viewer, /if \(onOpen\) g\.addEventListener\("dblclick", \(ev\) => \{ ev\.stopPropagation\(\); onOpen\(s\.id\); \}\);/);
  assert.match(runs, /openLabel: "Open workflow", openTip: "Opens this version of the workflow on Workflows",/);
  assert.match(viewer, /footer: \[onOpen \? h\("button", \{ type: "button", title: openTip, onclick: \(\) => onOpen\(\) \}, openLabel\) : null,/);
  assert.match(runs, /WorkflowGraph\.openLarge\(\{/);
  assert.match(workflows, /G\.openLarge\(\{/);
  for (const page of [runs, workflows.slice(workflows.indexOf("G.openLarge({"))]) assert.doesNotMatch(page, /graph-dialog|run-graph-large/, "the dialog is the shared one");
  assert.doesNotMatch(runs, /addEventListener\("wheel"/);
  // Pan by a drag and zoom by the wheel, kept per run and size across the 3 s redraws.
  assert.match(viewer, /const views = new Map\(\);/);
  assert.match(viewer, /svg\.addEventListener\("wheel", \(ev\) => \{\s+ev\.preventDefault\(\);/);
  assert.match(viewer, /if \(!moved && Math\.hypot\(dx, dy\) < 4\) return;/, "a click is not a pan");
  assert.match(viewer, /if \(kept\) view = \{ \.\.\.view, \.\.\.kept \};/);
  assert.match(runs, /if \(WorkflowGraph\.dragging\(\)\) return; \/\/ a pan is in progress/, "no redraw under the pointer");
  // The steps the run is at now are active: red, with the state still a word on the tile.
  assert.match(viewer, /const ACTIVE_STATES = \["running", "waiting"\];/);
  assert.match(viewer, /if \(stateOf && ACTIVE_STATES\.includes\(stateOf\(s\.id\)\)\) g\.classList\.add\("active"\);/);
  assert.match(style, /\.run-detail-graph \.tile\.active \.body \{ stroke: var\(--failed\); stroke-width: 3; \}/);
  // A click on the empty graph, or its Enlarge button, opens it nearly full screen.
  assert.match(viewer, /if \(onEnlarge && !ev\.target\.closest\?\.\("\.tile"\)\) onEnlarge\(\);/);
  assert.match(viewer, /iconButton\("expand", "Enlarge graph"/);
  assert.match(app, /\n  expand: '<path /);
  assert.match(style, /\.dialog-backdrop\.graph-backdrop \{ align-items: stretch; padding: 24px; \}/);
  // Enlarged from a run: a tile opens its step on the Workflows page in the run's version, and says what it is on hover.
  assert.match(runs, /location\.hash = WorkflowLogic\.formatHash\(\{ workflow, step: id, version: version \|\| 0 \}\);/);
  assert.match(viewer, /large: true, stateOf: states, describe: describeStep,/);
  assert.match(viewer, /text = typeof config\?\.description === "string" \? config\.description : "";/);
  // Enlarged from the Workflows page: a double-click on the empty canvas; a tile closes it and selects that node.
  assert.match(workflows, /if \(!s && w\?\.steps\.length && !state\.teamOpen && !ev\.target\.closest\?\.\("\.tile, \.edge, \.edge-hit, \.back-edge, \.step-group-label"\)\) \{/);
  assert.match(workflows, /onTile: \(id, close\) => \{ close\(\); state\.selected = id; render\(\); \},/);
});

test("the Runs detail says a run is loading instead of going blank, and announces when it is ready", () => {
  const runs = read("runs.js");
  assert.match(runs, /if \(detail\.dataset\.trace !== trace\) \{\s+detail\.dataset\.trace = trace;\s+detail\.setAttribute\("aria-busy", "true"\);/);
  assert.match(runs, /h\("p", \{ className: "muted loading-line", role: "status" \}, `Loading \$\{titleCase\(r\?\.workflow \|\| "the run"\)\}…`\)/);
  // The loading line comes before the first await, so nothing is blank while the status loads.
  assert.ok(runs.indexOf('className: "muted loading-line"') < runs.indexOf("const st = await runIndex.status(trace);"));
  assert.match(runs, /detail\.setAttribute\("aria-busy", "false"\);\n[^]*?\n    detail\.replaceChildren\(/, "announced as ready, then drawn");
});

test("a tile is read by the name it shows, and the keyboard sees a ring around it, apart from the selection", () => {
  const { G, document } = graph();
  const svg = document.make("svg");
  G.drawGraph(svg, demo, {});
  const nodes = all(svg);
  const plan = nodes.find((n) => n.attrs["data-step"] === "plan");
  assert.equal(plan.attrs["aria-label"], "Step Plan: planner@v1", "title case, as on the tile");
  const ring = plan.children.find((n) => n.attrs.class === "focus-ring");
  assert.deepEqual([ring.attrs.x, ring.attrs.y, ring.attrs.width, ring.attrs.height], ["-4", "-4", "228", "72"]);
  const style = read("style.css");
  assert.match(style, /\.tile:focus-visible \.focus-ring \{ stroke: var\(--accent\); \}/);
  assert.match(style, /\.tile \.focus-ring \{ fill: none; stroke: transparent; stroke-width: 2; pointer-events: none; \}/);
  assert.match(read("workflows.js"), /t\.getAttribute\("data-step"\) === id/, "the editor finds a tile by its step, not its label");
});

test("an arrow to a step more than a column aside turns along the gap, and the editor's fit never cuts a tile off", () => {
  const { G } = graph();
  // ECC End To End: Baseline (top right) feeds Develop (next row, far left).
  const w = { steps: [
    { id: "plan", service: "agent", position: { x: 260, y: 30 } }, { id: "architecture", service: "agent", depends_on: ["plan"], position: { x: 528, y: 30 } },
    { id: "baseline", service: "command", position: { x: 1332, y: 30 } },
    { id: "develop", service: "team", depends_on: ["plan", "architecture", "baseline"], position: { x: 260, y: 210 } },
  ] };
  const step = (id) => w.steps.find((s) => s.id === id);
  const path = G.dependencyPath(w, step("baseline"), step("develop"));
  assert.doesNotMatch(path, /C/, "no curve across the gap");
  // Down from Baseline, a rounded turn, along the gap at one height, a rounded turn, down into Develop.
  const shape = /^M(\d+),94 L\1,\d+ Q\1,(\d+) \d+,\2 L\d+,\2 Q370,\2 370,\d+ L370,196$/.exec(path);
  assert.ok(shape, path);
  assert.ok(Number(shape[2]) > 30 + 64 && Number(shape[2]) < 210, "it runs along the gap between the rows");
  // Plan stands right above Develop: its arrow stays straight down.
  assert.match(G.dependencyPath(w, step("plan"), step("develop")), /^M370,94 C/);
  // The editor fits the whole graph however narrow the canvas, down to a quarter size.
  const view = G.fitView(w, 900, { minScale: 0.25 });
  const b = G.bounds(w);
  assert.ok(b.maxX * view.scale + view.ox <= 900, "the right-most tile is in view");
  assert.match(read("workflows.js"), /G\.fitView\(w, visible, \{ minScale: MIN_SCALE \}\)/);
});

test("a run from before its workflow moved into a group still finds its graph under the group's name", () => {
  const app = read("app.js"), runs = read("runs.js");
  const context = {};
  vm.runInNewContext(`${app.slice(app.indexOf("function groupedName("), app.indexOf("// traceMatch finds"))}\nglobalThis.f = groupedName;`, context);
  const names = ["ecc/ECC-review", "ecc/ECC-plan", "default/workflow-1", "other/workflow-1"];
  assert.equal(context.f(names, "ECC-review"), "ecc/ECC-review");
  assert.equal(context.f(names, "ecc-review"), "ecc/ECC-review", "case aside");
  assert.equal(context.f(names, "workflow-1"), "", "two groups hold that name: no guess");
  assert.equal(context.f(names, "gone"), "");
  assert.match(runs, /const graphDef = def\?\.steps\?\.length \? def : await runWorkflow\(r\?\.workflow \|\| st\.workflow \|\| "", version\);/);
  assert.match(read("overview.js"), /live\.map\(\(r\) => runWorkflow\(r\.workflow, Number\(r\.workflow_version\) \|\| 0\)\)/);
});

test("the run detail keeps its scroll position across the 3 s redraws", () => {
  // Bug report, 26 Nov: every redraw built a new scrolling panel, so the detail jumped to the top.
  const runs = read("runs.js");
  const at = runs.indexOf('const scrolledTo = detail.querySelector(":scope > .panel-body")?.scrollTop || 0;');
  assert.ok(at > 0, "reads the scroll position before the redraw");
  assert.ok(at < runs.indexOf("detail.replaceChildren(\n      h(\"div\", { className: \"panel-head\" }),\n      h(\"div\", { className: \"panel-body\" },\n        h(\"div\", { className: \"section\" },\n          h(\"div\", { className: \"run-meta\" },"), "before the full redraw");
  assert.match(runs, /if \(body && detail\.dataset\.trace === trace\) body\.scrollTop = scrolledTo;/, "puts it back for the same run only");
});
