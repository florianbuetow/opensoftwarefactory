import { test } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";

const read = (name) => fs.readFileSync(new URL(`web/${name}`, import.meta.url), "utf8");
const overview = read("overview.js"), app = read("app.js"), html = read("index.html"), style = read("style.css");

function logic() {
  const context = {};
  vm.runInNewContext(`${overview.slice(0, overview.indexOf("\n(() => {"))}\nglobalThis.O = OverviewLogic;`, context);
  return context.O;
}
const plain = (v) => JSON.parse(JSON.stringify(v));
const HOUR = 3600000;
const now = Date.parse("2026-11-23T12:30:00Z");

test("Overview is home: the live factory, with the measurement explorer as Metrics beside it", () => {
  assert.match(app, /\{ name: "overview", title: "Overview", icon: "metrics", tip: "The factory now: what waits for you, what runs, and the last 24 hours" \}/);
  assert.match(app, /location\.replace\("#overview"\);/, "an unknown page opens Overview");
  assert.match(html, /<a class="product" href="#overview"/, "the logo goes home");
  assert.match(html, /<section class="page page-overview" id="tab-overview" hidden><\/section>/);
  assert.ok(html.indexOf('src="graph.js"') < html.indexOf('src="overview.js"'));
  // No selectors and no periods: the page has no form and refreshes itself every 5 s while open.
  assert.doesNotMatch(overview, /h\("select"|h\("form"|measurementUI\.select|type: "datetime-local"/);
  assert.match(overview, /state\.timer = setInterval\(\(\) => \{ if \(!document\.hidden\) refresh\(\); \}, 5000\);/);
  assert.match(overview, /hide\(\) \{ state\.active = false; clearInterval\(state\.timer\); state\.timer = null; \}/);
});

test("runs are counted per hour of the last 24, by state, and the failure rate counts finished runs only", () => {
  const O = logic();
  const runs = [
    { started: new Date(now - 10 * 60000).toISOString(), s: "failed" },
    { started: new Date(now - 20 * 60000).toISOString(), s: "done" },
    { started: new Date(now - 3 * HOUR).toISOString(), s: "running" },
    { started: new Date(now - 30 * HOUR).toISOString(), s: "done" }, // older than 24 h
  ];
  const buckets = O.hours(runs, (r) => r.s, now);
  assert.equal(buckets.length, 24);
  assert.deepEqual(plain(buckets.at(-1)), { start: Date.parse("2026-11-23T12:00:00Z"), done: 1, failed: 1, running: 0, total: 2 });
  assert.equal(buckets.reduce((n, b) => n + b.total, 0), 3);
  assert.deepEqual(plain(O.failureRate(buckets)), { failed: 1, finished: 2, percent: 50 });
  assert.equal(O.failureRate(O.hours([], () => "done", now)).percent, null, "no finished run is no rate, not 0 %");
});

test("the cost of the last 24 hours is added up per hour from the cost series", () => {
  const O = logic();
  const data = { series: [{ points: [{ time: new Date(now - 5 * 60000).toISOString(), value: "0.25" }, { time: new Date(now - 2 * HOUR).toISOString(), value: 1 }, { time: "bad", value: 9 }] },
    { points: [{ time: new Date(now - 6 * 60000).toISOString(), value: 0.5 }, { time: new Date(now - 48 * HOUR).toISOString(), value: 7 }] }] };
  const { sums, total } = O.costHours(data, now);
  assert.equal(sums.at(-1), 0.75);
  assert.equal(sums.at(-3), 1);
  assert.equal(total, 1.75);
  assert.equal(O.costHours(null, now).total, 0);
});

test("a step of a running run on its live graph is done, waiting for a person, running, or not started", () => {
  const O = logic();
  const def = { steps: [{ id: "plan" }, { id: "approve", depends_on: ["plan"] }, { id: "build", depends_on: ["approve"] }, { id: "chat", depends_on: ["plan"] }] };
  const st = { artifacts: { plan: "runs/t/plan" } };
  const decisions = [{ trace_id: "t", step: "approve" }], sessions = [{ session: "user-sessions/t/chat" }];
  const at = (id, d = decisions, s = sessions) => O.stepState(def, st, "t", d, s, id);
  assert.equal(at("plan"), "done");
  assert.equal(at("approve"), "waiting", "a gate that asks a person");
  assert.equal(at("chat"), "waiting", "a session that waits");
  assert.equal(at("build"), "idle");
  assert.equal(at("approve", [], []), "running", "its dependencies are done");
  assert.equal(O.stepState(def, st, "other", decisions, sessions, "approve"), "running", "another run's gate is not this one's");
});

test("Needs you counts what waits for a person or is broken, each linking to where it is fixed", () => {
  const O = logic();
  const items = O.needsYou({ decisions: [{}, {}], sessions: [], failed: 1, services: [{ state: "ready" }, { state: "down" }, { state: "starting" }] });
  assert.deepEqual(plain(items.map((i) => [i.label, i.count, i.state, i.href])), [
    ["Decisions wait", 2, "waiting", "#decisions"],
    ["Sessions wait", 0, "waiting", "#sessions"],
    ["Run failed in 24 h", 1, "failed", "#runs?status=failed"],
    ["Services not ready", 2, "failed", "#cluster"],
  ]);
  // A count of 0 reads "None"; a count shows its state as a dot and a word.
  assert.match(overview, /h\("span", \{ className: "ov-need-count" \}, n\.count \? String\(n\.count\) : "None"\)/);
  assert.match(overview, /n\.count \? statusChip\(n\.state, n\.label\) : n\.label/);
  // The waiting gates open their decision; deciding stays in its detail.
  assert.match(overview, /href: `#decisions\/\$\{encodeURIComponent\(`p:\$\{d\.workflow_id\}\|\$\{d\.step\}`\)\}`/);
  assert.doesNotMatch(overview, /"approved"|"rejected"/);
});

test("the services are read from the cluster's deployments and polled with the gates", () => {
  const context = {};
  vm.runInNewContext(`${app.slice(app.indexOf("function serviceStates("), app.indexOf("async function refreshClusterHealth("))}\nglobalThis.f = serviceStates;`, context);
  const deployments = [
    { metadata: { name: "tool-gateway" }, spec: { replicas: 1 }, status: {} },
    { metadata: { name: "agent" }, spec: { replicas: 2 }, status: { readyReplicas: 1 } },
    { metadata: { name: "blobstore" }, spec: { replicas: 1 }, status: { readyReplicas: 1 } },
  ];
  assert.deepEqual(plain(context.f(deployments)), [
    { name: "agent", ready: 1, desired: 2, state: "starting" },
    { name: "blobstore", ready: 1, desired: 1, state: "ready" },
    { name: "tool-gateway", ready: 0, desired: 1, state: "down" },
  ]);
  assert.match(app, /refreshDecisions\(\); refreshSessions\(\); refreshClusterHealth\(\);/);
  // Each service reads as a dot and a word, never colour alone.
  assert.match(overview, /s\.state === "ready" \? "Ready" : s\.state === "starting" \? "Starting" : "Not ready"/);
  assert.match(style, /\.ov-needs \{ display: grid; grid-template-columns: repeat\(4, minmax\(0, 1fr\)\);/);
});

test("the top bar marks a service that is not ready with its name, linking to Cluster", () => {
  const context = {};
  vm.runInNewContext(`${app.slice(app.indexOf("function serviceMarker("), app.indexOf("// showServiceMarker"))}\nglobalThis.f = serviceMarker;`, context);
  assert.deepEqual(plain(context.f([{ name: "agent", state: "ready", ready: 1, desired: 1 }])), { text: "", title: "" }, "nothing while all are ready");
  assert.deepEqual(plain(context.f([{ name: "tool-gateway", state: "down", ready: 0, desired: 1 }])), { text: "tool-gateway not ready", title: "Not ready: tool-gateway (0/1). Opens Cluster" });
  assert.equal(context.f([{ name: "a", state: "down", ready: 0, desired: 1 }, { name: "b", state: "starting", ready: 1, desired: 2 }]).text, "2 services not ready");
  // A chip, dot and word, beside Acting as; refreshed with every poll.
  assert.match(html, /<div class="topbar-end">\s+<a id="service-marker" class="service-marker" href="#cluster" hidden><\/a>\s+<form id="trace-jump-form"[\s\S]*?<\/form>\s+<label id="acting-as"/);
  assert.match(app, /marker\.replaceChildren\(\.\.\.\(text \? \[statusChip\("failed", text\)\] : \[\]\)\);/);
  assert.match(app, /showServiceMarker\(\);\n  document\.dispatchEvent\(new Event\("clusterchange"\)\);/);
});

test("the Sidebar and Detail toggles show the focus ring to the keyboard, never after a click", () => {
  assert.match(style, /\.page-toggles \.icon-button:focus:not\(:focus-visible\) \{ outline: none; box-shadow: none; \}/);
  assert.match(style, /\.page-toggles \.icon-button:focus-visible \{ outline: 2px solid var\(--accent\); outline-offset: -2px; \}/);
  assert.doesNotMatch(style, /\.page-toggles \.icon-button:focus-visible \{ outline: none/);
});

test("Shift+F10 and the ContextMenu key open the right-click menu of what has focus, under it", () => {
  const start = app.indexOf("// The keyboard opens the right-click menu");
  const code = app.slice(start, app.indexOf("// setPageActions fills", start));
  let listener = null, sent = null, prevented = false;
  const row = { getBoundingClientRect: () => ({ left: 100.4, bottom: 240 }), dispatchEvent: (e) => { sent = e; return true; } };
  const context = {
    menu: { hidden: true },
    document: { body: {}, activeElement: row, addEventListener: (type, fn) => { if (type === "keydown") listener = fn; } },
    MouseEvent: class { constructor(type, init) { Object.assign(this, { type }, init); } },
  };
  vm.runInNewContext(code, context);
  const key = (init) => { sent = null; prevented = false; listener({ ...init, preventDefault: () => { prevented = true; } }); };
  key({ key: "F10", shiftKey: true });
  assert.equal(sent.type, "contextmenu");
  assert.deepEqual([sent.bubbles, sent.clientX, sent.clientY], [true, 108, 240], "anchored under the focused row's left edge");
  assert.ok(prevented, "the browser's own menu does not open as well");
  key({ key: "ContextMenu" });
  assert.equal(sent?.type, "contextmenu");
  key({ key: "F10" });
  assert.equal(sent, null, "F10 alone does nothing");
  context.menu.hidden = false;
  key({ key: "ContextMenu" });
  assert.equal(sent, null, "never over an open menu");
  // The canvas takes focus, so the keyboard reaches its menu (Run…, Add …) too.
  assert.match(html, /<svg id="canvas" aria-label="Workflow graph" tabindex="0"><\/svg>/);
});

test("a row with cut-off text shows that text in full under it when the keyboard focuses it", () => {
  const start = app.indexOf("// The keyboard cannot hover");
  const code = app.slice(start, app.indexOf("// dataTable builds a table", start));
  const listeners = {};
  class Element {
    constructor(attrs = {}, cells = []) { Object.assign(this, { attrs: { ...attrs }, cells, dataset: attrs.dataset || {}, scrollWidth: attrs.scrollWidth || 0, clientWidth: attrs.clientWidth || 0 }); }
    matches(sel) { return sel === ":focus-visible" ? !!this.attrs.keyboard : sel === "[data-full]" ? "full" in this.dataset : false; }
    querySelectorAll() { return this.cells; }
    hasAttribute(k) { return k in this.attrs; }
    setAttribute(k, v) { this.attrs[k] = v; }
    removeAttribute(k) { delete this.attrs[k]; }
    getBoundingClientRect() { return { left: 40, top: 100, bottom: 140, width: 300, height: 40 }; }
  }
  const tip = { hidden: true, style: {}, isConnected: true, children: [], replaceChildren(...c) { this.children = c; }, getBoundingClientRect: () => ({ width: 200, height: 24 }) };
  let described = null;
  const context = {
    Element, window: { innerWidth: 1200, innerHeight: 800 },
    h: (tag, props, text) => (tag === "div" && props?.id === "focus-tip" ? tip : { text }),
    document: {
      body: { append() {} },
      addEventListener: (type, fn) => { (listeners[type] ||= []).push(fn); },
      querySelector: () => described,
    },
  };
  vm.runInNewContext(code, context);
  const cut = new Element({ dataset: { full: "Accept the ECC review report? Reject unresolved blockers…" }, scrollWidth: 400, clientWidth: 230 });
  const fits = new Element({ dataset: { full: "ECC Review" }, scrollWidth: 80, clientWidth: 120 });
  const row = new Element({ keyboard: true }, [fits, cut]);
  listeners.focusin[0]({ target: row });
  assert.equal(tip.hidden, false);
  assert.deepEqual(tip.children.map((c) => c.text), ["Accept the ECC review report? Reject unresolved blockers…"], "only what is cut off");
  assert.equal(row.attrs["aria-describedby"], "focus-tip", "a screen reader reads it with the row");
  assert.deepEqual([tip.style.left, tip.style.top], ["48px", "144px"]);
  described = row;
  listeners.focusout[0]();
  assert.equal(tip.hidden, true);
  assert.equal(row.attrs["aria-describedby"], undefined);
  // A click is no keyboard focus: the hover tooltip covers the mouse.
  listeners.focusin[0]({ target: new Element({}, [cut]) });
  assert.equal(tip.hidden, true);
});

test("the Cluster sidebar says in a word when a deployment or node is not ready, the counts in its tooltip", () => {
  const cluster = read("cluster.js");
  assert.match(cluster, /sub: short \? \(ready \? "Starting" : "Not ready"\) : ""/);
  assert.match(cluster, /title: short \? `\$\{x\.metadata\.name\}: \$\{ready\} of \$\{desired\} replicas ready` : x\.metadata\.name/);
  assert.match(cluster, /sub: nodeReady\(n\) \? "" : "Not ready"/);
  assert.doesNotMatch(cluster, /sub: ready < desired \? `\$\{ready\}\/\$\{desired\}`/, "never a bare count");
});

test("a running run's card reads the outputs its steps stored, and a waiting session makes the run Waiting", () => {
  // Found in a live run: Step 1 had finished, but the card read the status alone and showed it Running.
  assert.match(overview, /pool\(live, 3, \(r\) => runIndex\.status\(r\.trace_id, 3000, "overview"\)\)/);
  assert.match(overview, /sessionsState\.pending\.some\(\(s\) => String\(s\.session \|\| ""\)\.startsWith\(`user-sessions\/\$\{trace\}\/`\)\)/);
  assert.match(overview, /const waits = waitsForPerson\(r\.trace_id\);/);
  assert.match(read("graph.js"), /idle: "Idle" \}/, "a tile says what its row in Steps says");
});
