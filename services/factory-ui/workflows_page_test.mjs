import assert from "node:assert/strict";
import fs from "node:fs";
import test from "node:test";
import vm from "node:vm";

// The Workflows page runs here against a small stand-in for the DOM and for the helpers of
// app.js, with the orchestrator answering versioned workflow requests. It covers
// what the logic tests cannot: that the page code loads, saves versions, switches between
// them, and asks before unsaved edits are lost.

const read = (name) => fs.readFileSync(new URL(`web/${name}`, import.meta.url), "utf8");
const settle = async () => { for (let i = 0; i < 8; i++) await new Promise((done) => setTimeout(done, 0)); };
const plain = (value) => JSON.parse(JSON.stringify(value));

class FakeNode {
  constructor(tag) {
    this.tag = tag; this.children = []; this.attrs = {}; this.listeners = {}; this.style = {}; this.dataset = {};
    this.hidden = false; this.value = ""; this.text = "";
    const names = new Set();
    this.classList = {
      add: (...n) => n.forEach((x) => names.add(x)), remove: (...n) => n.forEach((x) => names.delete(x)),
      toggle: (n, on = !names.has(n)) => { if (on) names.add(n); else names.delete(n); return on; }, contains: (n) => names.has(n),
    };
  }
  adopt(items) { return items.flat().filter((c) => c !== null && c !== undefined && c !== false && c !== "").map((c) => { if (c instanceof FakeNode) c.parentNode = this; return c; }); }
  append(...items) { this.children.push(...this.adopt(items)); }
  prepend(...items) { this.children.unshift(...this.adopt(items)); }
  appendChild(child) { this.append(child); return child; }
  replaceChildren(...items) { this.children = this.adopt(items); }
  remove() { if (this.parentNode) this.parentNode.children = this.parentNode.children.filter((c) => c !== this); }
  setAttribute(name, value) { this.attrs[name] = String(value); }
  getAttribute(name) { return name in this.attrs ? this.attrs[name] : null; }
  removeAttribute(name) { delete this.attrs[name]; }
  addEventListener(type, fn) { (this.listeners[type] ||= []).push(fn); }
  querySelector() { return null; }
  querySelectorAll() { return []; }
  closest() { return null; }
  contains(node) { return node === this || this.children.some((child) => child instanceof FakeNode && child.contains(node)); }
  focus() {} select() {} setPointerCapture() {}
  getBoundingClientRect() { return { left: 0, top: 0, right: 800, bottom: 600, width: 800, height: 600 }; }
  get parentElement() { return this.parentNode || (this.auto ||= new FakeNode("div")); }
  get clientWidth() { return 800; }
  get offsetWidth() { return 100; }
  get options() { return this.children; }
  get isConnected() { return true; }
  get textContent() { return this.text || this.children.map((c) => (c instanceof FakeNode ? c.textContent : String(c))).join(""); }
  set textContent(value) { this.text = String(value); this.children = []; }
  set innerHTML(value) { this.html = value; }
}

const all = (node) => (node instanceof FakeNode ? [node, ...node.children.flatMap(all)] : []);
const find = (root, test) => all(root).find(test);
const fire = async (node, type, ev = {}) => {
  const event = { target: node, button: 0, key: "", preventDefault() {}, stopPropagation() {}, ...ev };
  if (typeof node[`on${type}`] === "function") node[`on${type}`](event);
  for (const fn of node.listeners[type] || []) fn(event);
  await settle();
};

// The orchestrator as the contract describes it, holding every version of every workflow.
function orchestrator() {
  const step = (id, extra = {}) => ({ id, service: "agent", agent: "demo/coder@v1", ...extra });
  const store = {
    demo: [1, 2, 3].map((version) => ({ name: "demo", version, created: `2026-11-0${version}T10:00:00Z`, created_by: "ada",
      steps: [step("plan", { position: { x: 0, y: 40 } }), ...(version > 1 ? [{ id: "build", service: "workflow", workflow: "inner@v1", depends_on: ["plan"], position: { x: 0, y: 176 } }] : [])] })),
    inner: [1, 2].map((version) => ({ name: "inner", version, created: "2026-11-01T10:00:00Z", created_by: "ada", steps: [step(version === 1 ? "old-write" : "write")] })),
  };
  const calls = [];
  // crew is added by the team test, so the other tests keep their two workflows.
  const crew = { name: "crew", version: 1, steps: [{ id: "team", service: "team", agent: "demo/coder@v1", workflows: ["inner@v1"], channels: [["coder", "inner"]], position: { x: 0, y: 40 } }] };
  const withList = (name, def) => ({ ...structuredClone(def), versions: store[name].map((d) => d.version) });
  const fail = (status, message) => { const e = new Error(message); e.status = status; return e; };
  async function api(method, path, body, headers = {}) {
    calls.push({ method, path, body: body === undefined ? undefined : plain(body), headers });
    if (path === "/api/agents/agents") return { entries: [{ name: "demo/coder", versions: ["v1"] }] };
    if (path.startsWith("/api/agents/agents/")) return { files: { "systemprompt.md": "Write code.", "agent.json": { models: ["local"] } } };
    // The connector service lists the Git repositories as a bare array.
    if (path === "/api/connectors/git/repositories") return [
      { name: "claims-build", kind: "plain", url: "http://git-host:8080/claims-build.git", default_branch: "main", setup_command: "", idle_minutes: 30, approve_publish: false, credential: "none" },
      { name: "site", kind: "github", url: "https://github.com/acme/site.git", default_branch: "trunk", setup_command: "", idle_minutes: 30, approve_publish: true, credential: "read-write" },
    ];
    const rest = path.replace("/api/orchestrator/workflows", "");
    const [, name, kind, n] = rest.split("/").map(decodeURIComponent);
    if (method === "GET" && !rest) return { workflows: Object.keys(store).map((key) => withList(key, store[key].at(-1))) };
    if (method === "GET" && kind === "versions") {
      const def = store[name]?.find((d) => d.version === Number(n));
      if (!def) throw fail(404, "not found");
      return withList(name, def);
    }
    if (method === "PATCH" && kind === "versions" && rest.endsWith("/description")) {
      if (serverDescriptionFailure) throw fail(503, "description store unavailable");
      const def = store[name]?.find((d) => d.version === Number(n));
      if (!def) throw fail(404, "not found");
      def.description = body.description;
      return withList(name, def);
    }
    if (method === "POST" && kind === "versions") {
      if (!body.steps?.length) throw fail(422, `workflow "${name}" has no steps`);
      const list = (store[name] ||= []);
      list.push({ ...structuredClone(body), name, version: list.length + 1, created: "2026-11-15T10:00:00Z", created_by: headers["X-Creator"] || "" });
      return withList(name, list.at(-1));
    }
    if (method === "POST" && kind === "rename") {
      if (store[body.name]) throw fail(409, `workflow "${body.name}" exists`);
      store[body.name] = store[name].map((d) => ({ ...d, name: body.name }));
      delete store[name];
      return {};
    }
    if (method === "DELETE" && name && !kind) { delete store[name]; return {}; }
    throw fail(404, `${method} ${path}`);
  }
  let serverDescriptionFailure = false;
  return { api, calls, store, crew, failDescriptions(value) { serverDescriptionFailure = value; } };
}

function page() {
  const server = orchestrator();
  const ids = new Map(), selectors = new Map();
  const byId = (id) => { if (!ids.has(id)) ids.set(id, new FakeNode("div")); return ids.get(id); };
  const notices = [], questions = [], menus = [], inputs = [], dialogs = [];
  const windowListeners = {}; // the drags on the canvas end on the window
  const storage = new Map();
  const answers = { confirm: true };
  const h = (tag, props = {}, ...children) => {
    const node = new FakeNode(tag);
    for (const [key, value] of Object.entries(props || {})) {
      if (key === "dataset") Object.assign(node.dataset, value);
      else if (key.includes("-")) node.setAttribute(key, value);
      else node[key] = value;
    }
    node.append(...children);
    return node;
  };
  const context = {
    console, setTimeout, clearTimeout, structuredClone, h, $: byId,
    document: {
      querySelector: (selector) => { if (!selectors.has(selector)) selectors.set(selector, new FakeNode("div")); return selectors.get(selector); },
      createElementNS: (_ns, name) => new FakeNode(name), addEventListener() {}, dispatchEvent() {}, activeElement: null,
      createDocumentFragment: () => new FakeNode("fragment"),
    },
    window: { addEventListener(type, fn) { (windowListeners[type] ||= []).push(fn); } },
    location: { hash: "#workflows" },
    history: { pushState(_s, _t, url) { context.location.hash = url; }, replaceState(_s, _t, url) { context.location.hash = url; } },
    localStorage: { getItem: (key) => (storage.has(key) ? storage.get(key) : null), setItem: (key, value) => storage.set(key, String(value)), removeItem: (key) => storage.delete(key) },
    ResizeObserver: class { observe() {} },
    requestAnimationFrame: (fn) => fn(),
    CustomEvent: class { constructor(type, options) { this.type = type; this.detail = options?.detail; } },
    ICONS: new Proxy({}, { get: () => "" }),
    tabs: {}, pageToggles: {}, activeTab: "workflows",
    menu: { hidden: true }, closeMenu() {}, openMenu: (_ev, items) => menus.push(items),
    openPopover: () => ({ close() {} }), fitWidth() {},
    icon: () => h("span"),
    openDialog: (config) => { const dialog = { ...config, open: true, close() { dialog.open = false; } }; dialogs.push(dialog); return dialog; },
    iconButton: (name, label, onclick) => h("button", { title: label, onclick }),
    emptyPanel: (title, action) => h("div", { className: "empty", title, action }),
    errorPanel: (message) => h("div", { className: "error-panel", title: message }),
    textView: () => ({ toggle: h("button"), body: h("div") }),
    api: server.api,
    creatorHeaders: () => ({ "X-Creator": "ada" }),
    setStatus: (text, kind = "", action) => { notices.push({ text, kind }); if (action) context.undo = action.run; },
    setSaveState: (text = "Saved") => notices.push({ text, kind: "" }),
    dropUndo: () => { context.undo = null; },
    confirmAction: async (question, action) => { questions.push(`${question} [${action}]`); return answers.confirm; },
    Inputs: { setBlocker(fn) { context.blocker = fn; }, open: (...args) => inputs.push(args) },
  };
  // index.html loads connectors.js before workflows.js; the page needs its logic only. From
  // app.js it takes the sidebar tree, the group helpers, and titleCase as they are.
  const connectors = read("connectors.js"), app = read("app.js");
  const shared = [app.slice(app.indexOf("const titleCase"), app.indexOf("// midTruncate")), app.slice(app.indexOf("// createTree renders"), app.indexOf("// openDialog shows"))].join("\n");
  const markdown = app.slice(app.indexOf("function markdownInline("), app.indexOf("function renderMarkdown("));
  vm.runInNewContext(`${shared}\n${read("workflowref.js")}\n${markdown}\n${connectors.slice(0, connectors.indexOf("// ---- Connectors page ----"))}\n${read("graph.js")}\n${read("workflows.js")}`, context);
  const head = selectors.get("#tab-workflows .canvas-head");
  const ui = {
    context, server, notices, questions, inputs, answers, byId, dialogs,
    save: () => find(head, (n) => n.className === "primary wf-save"),
    runButton: () => find(head, (n) => n.className === "primary wf-run"),
    versions: () => find(head, (n) => n.tag === "select"),
    note: () => find(head, (n) => n.className === "wf-version-note"),
    shownVersion: () => ui.versions().children.find((o) => o.selected)?.textContent,
    // The sidebar tree: its visible rows (groups and workflows) by their labels.
    rows: () => all(byId("wf-list")).filter((n) => String(n.className || "").startsWith("tree-row")),
    names: () => ui.rows().map((row) => row.textContent),
    row: (label) => ui.rows().find((row) => row.textContent === label),
    selectedRow: () => ui.rows().find((row) => row.attrs["aria-selected"] === "true")?.textContent,
    // dialog is the open dialog; fill types into its Group and Name fields and submits it.
    dialog: () => dialogs.filter((d) => d.open).at(-1),
    field: (id) => find(ui.dialog().body, (n) => n.id === id),
    async submit(fields = {}) {
      for (const [id, value] of Object.entries(fields)) ui.field(id).value = value;
      await fire(ui.dialog().footer.find((b) => b.className === "primary"), "click");
    },
    tiles: () => all(byId("canvas")).filter((n) => n.attrs["data-step"]).map((n) => n.attrs["data-step"]),
    // menu opens the canvas menu and returns the item with that label: [label, run, class, tip, disabled].
    async menu(label, node = byId("canvas")) {
      menus.length = 0;
      await fire(node, "contextmenu");
      // An item may sit in a submenu ([label, null, …, children]), such as the Add items under Add step.
      const flat = (list) => list.flatMap((entry) => (Array.isArray(entry) && Array.isArray(entry[5]) ? [entry, ...flat(entry[5])] : [entry]));
      const item = flat(menus.at(-1)).find((entry) => Array.isArray(entry) && entry[0] === label);
      assert.ok(item, `menu item ${label}`);
      return item;
    },
    async run(label, node) { const item = await ui.menu(label, node); assert.ok(!item[4], `${label} is enabled`); await item[1](); await settle(); },
    posts: () => server.calls.filter((c) => c.method !== "GET"),
    unloadPrevented() {
      let prevented = false;
      for (const fn of windowListeners.beforeunload || []) fn({ preventDefault() { prevented = true; } });
      return prevented;
    },
    menus,
    // release ends a drag at a point of the graph (in the graph's own coordinates).
    async release(x, y) {
      const [, ox, oy, scale] = /translate\(([-\d.]+),([-\d.]+)\) scale\(([-\d.]+)\)/.exec(find(byId("canvas"), (n) => n.attrs.class === "graph").attrs.transform).map(Number);
      for (const fn of windowListeners.pointerup || []) fn({ clientX: x * scale + ox, clientY: y * scale + oy });
      await settle();
    },
    async open(rest = "") { context.tabs.workflows.show(rest); await settle(); },
  };
  return ui;
}

test("an older version's description saves in place without dirtying the graph", async () => {
  const ui = page();
  ui.server.store.demo[0].description = "Original first version";
  ui.server.store.demo[2].description = "Newest version";
  const before = plain(ui.server.store.demo);
  await ui.open("/demo?version=1");
  const box = ui.byId("wf-description");
  assert.match(box.textContent, /Original first version/);
  assert.equal(box.textContent, "Original first version");
  assert.equal(find(box, (n) => n.tag === "button" || /^h[1-6]$/.test(n.tag)), undefined, "read view is text only");
  await fire(find(box, (n) => n.className === "wf-description-text"), "dblclick");
  const area = find(box, (n) => n.tag === "textarea");
  assert.equal(area.rows, 1);
  assert.equal(area.value, "Original first version", "editable value excludes the Description prefix");
  area.value = "Revised first version\nWith another line";
  await fire(area, "input");
  assert.equal(area.value, "Revised first version With another line", "line breaks become spaces");
  assert.equal(ui.save().disabled, true);
  await fire(find(box, (n) => n.tag === "textarea"), "keydown", { key: "Enter" });
  assert.equal(ui.posts().length, 1);
  assert.equal(ui.posts()[0].method, "PATCH");
  assert.equal(ui.posts()[0].path, "/api/orchestrator/workflows/demo/versions/1/description");
  assert.equal(ui.server.store.demo.length, 3);
  assert.deepEqual(ui.server.store.demo[0], { ...before[0], description: area.value });
  assert.deepEqual(ui.server.store.demo.slice(1), before.slice(1));
  assert.equal(ui.shownVersion(), "v1");
  assert.equal(ui.save().disabled, true, "description does not enable a new-version save");
  assert.match(box.textContent, /Revised first version/);
});

test("saving a description preserves unsaved graph edits", async () => {
  const ui = page();
  await ui.open("/demo");
  await ui.run("Add agent");
  const box = ui.byId("wf-description");
  await fire(find(box, (n) => n.className === "wf-description-text"), "dblclick");
  const area = find(box, (n) => n.tag === "textarea");
  area.value = "Description only";
  await fire(area, "input");
  await fire(find(box, (n) => n.tag === "textarea"), "keydown", { key: "Enter" });
  assert.deepEqual(ui.tiles(), ["plan", "build", "step-1"]);
  assert.equal(ui.save().disabled, false, "graph edit still needs its own save");
  assert.equal(ui.server.store.demo.length, 3);
  assert.equal(ui.server.store.demo[2].steps.length, 2, "description save does not persist graph edits");
  await ui.save().onclick();
  await settle();
  assert.equal(ui.server.store.demo[3].description, "Description only");
  assert.equal(ui.server.store.demo[3].steps.length, 3);
});

test("description failures retain the edit for retry, and clearing needs no new version", async () => {
  const ui = page();
  ui.server.store.demo[2].description = "Clear me";
  await ui.open("/demo");
  const box = ui.byId("wf-description");
  await fire(find(box, (n) => n.className === "wf-description-text"), "dblclick");
  const area = find(box, (n) => n.tag === "textarea");
  area.value = "";
  await fire(area, "input");
  ui.server.failDescriptions(true);
  await fire(find(box, (n) => n.tag === "textarea"), "keydown", { key: "Enter" });
  assert.match(box.textContent, /Not saved: description store unavailable/);
  assert.equal(find(box, (n) => n.tag === "textarea").value, "");
  assert.equal(ui.server.store.demo[2].description, "Clear me");
  ui.server.failDescriptions(false);
  await fire(find(box, (n) => n.tag === "textarea"), "keydown", { key: "Enter" });
  assert.equal(ui.server.store.demo[2].description, "");
  assert.equal(ui.server.store.demo.length, 3);
  assert.match(box.textContent, /No description/);
});

test("the description footer saves on blur and Escape cancels without saving", async () => {
  const ui = page();
  await ui.open("/demo");
  const box = ui.byId("wf-description");
  await fire(find(box, (n) => n.className === "wf-description-text"), "dblclick");
  let area = find(box, (n) => n.tag === "textarea");
  area.value = "Single paragraph";
  await fire(area, "input");
  await fire(area.parentNode, "focusout", { relatedTarget: null });
  assert.equal(ui.server.store.demo[2].description, "Single paragraph");
  assert.equal(ui.server.store.demo.length, 3);
  await fire(find(box, (n) => n.className === "wf-description-text"), "dblclick");
  area = find(box, (n) => n.tag === "textarea");
  area.value = "Discard this edit";
  await fire(area, "input");
  await fire(area, "keydown", { key: "Escape" });
  await fire(area.parentNode, "focusout", { relatedTarget: null });
  assert.equal(ui.posts().length, 1);
  assert.equal(box.textContent, "Single paragraph");
});

test("descriptions always render Markdown, and the edit-only TXT/MD toggle preserves unsaved text", async () => {
  const ui = page();
  ui.context.localStorage.setItem("factory.text-view-mode", "text");
  ui.server.store.demo[2].description = "**Clear** [guide](https://example.test/guide)";
  await ui.open("/demo");
  const box = ui.byId("wf-description");
  assert.equal(find(box, (n) => n.tag === "strong").textContent, "Clear");
  const link = find(box, (n) => n.tag === "a");
  assert.equal(link.href, "https://example.test/guide");
  assert.equal(find(box, (n) => n.className === "text-format-toggle"), undefined);
  const text = find(box, (n) => n.className === "wf-description-text");
  link.closest = (selector) => selector === "a" ? link : null;
  await fire(text, "keydown", { key: "Enter", target: link });
  await fire(text, "dblclick", { target: link });
  assert.equal(find(box, (n) => n.tag === "textarea"), undefined, "links keep normal navigation behavior");
  await fire(text, "dblclick");
  const area = find(box, (n) => n.tag === "textarea");
  const editor = find(box, (n) => n.className === "wf-description-editor");
  const toggle = find(box, (n) => n.className === "text-format-toggle");
  const preview = find(box, (n) => n.className === "wf-description-preview");
  assert.equal(area.hidden, false);
  assert.equal(toggle.textContent, "MD");
  area.value = "**Changed** [guide](https://example.test/guide)";
  await fire(area, "input");
  await fire(editor, "focusout", { relatedTarget: toggle });
  await fire(toggle, "click");
  assert.equal(area.hidden, true);
  assert.equal(preview.hidden, false);
  assert.equal(find(preview, (n) => n.tag === "strong").textContent, "Changed");
  assert.equal(toggle.textContent, "TXT");
  assert.deepEqual(ui.posts(), [], "preview toggles never save or discard the draft");
  await fire(toggle, "click");
  assert.equal(area.hidden, false);
  assert.equal(area.value, "**Changed** [guide](https://example.test/guide)");
  await fire(editor, "focusout", { relatedTarget: null });
  assert.equal(ui.posts().length, 1);
  assert.equal(ui.posts()[0].method, "PATCH");
  assert.equal(ui.server.store.demo.length, 3);
  assert.equal(find(box, (n) => n.tag === "strong").textContent, "Changed");
  assert.equal(find(box, (n) => n.className === "text-format-toggle"), undefined);
});

test("description edits survive page visits and prevent unload and workflow-wide mutations", async () => {
  const ui = page();
  await ui.open("/demo");
  const box = ui.byId("wf-description");
  await fire(find(box, (n) => n.className === "wf-description-text"), "dblclick");
  const area = find(box, (n) => n.tag === "textarea");
  area.value = "Unfinished description";
  await fire(area, "input");
  assert.equal(ui.unloadPrevented(), true);
  ui.context.tabs.workflows.hide();
  await ui.open("/demo");
  assert.equal(find(box, (n) => n.tag === "textarea").value, "Unfinished description");
  for (const action of ["Rename workflow…", "Duplicate workflow", "Delete workflow"]) await ui.run(action);
  assert.deepEqual(ui.posts(), [], "editing a description blocks conflicting workflow mutations");
  ui.answers.confirm = false;
  await ui.open("/demo?version=1");
  assert.equal(ui.shownVersion(), "v3 (newest)");
  assert.equal(find(box, (n) => n.tag === "textarea").value, "Unfinished description");
  await fire(find(box, (n) => n.tag === "textarea"), "keydown", { key: "Escape" });
  assert.equal(ui.unloadPrevented(), false);
});

test("typed canvas additions have no dependencies or automatically drawn input arrows", async () => {
  const ui = page();
  await ui.open("/demo");
  const inputEdges = () => all(ui.byId("canvas")).filter((n) => (n.attrs.class || "").includes("input-edge")).length;
  assert.equal(inputEdges(), 1, "saved root receives an input arrow");
  for (const [label, service] of [["Add agent", "agent"], ["Add human gate", "gate"], ["Add interactive session", "interactive"], ["Add workflow", "workflow"], ["Add team", "team"], ["Add router", "route"], ["Add service", "service"], ["Add merge", "merge"], ["Add batch", "batch"]]) {
    await ui.run(label);
    assert.equal(panelField(ui, "service").value, service);
    assert.equal(inputEdges(), 1, "new draft nodes do not gain input arrows");
  }
  await ui.save().onclick();
  await settle();
  const added = ui.posts()[0].body.steps.slice(-9);
  assert.equal(added.length, 9);
  for (const step of added) assert.equal(step.depends_on, undefined, step.id);
  assert.equal(inputEdges(), 10, "saved roots show their runtime input connections");
});

test("the page opens the newest version, lists the versions, and saves nothing by itself", async () => {
  const ui = page();
  await ui.open();
  assert.deepEqual(ui.names(), ["Demo", "Inner"]);
  assert.equal(ui.selectedRow(), "Demo");
  assert.deepEqual(ui.versions().children.map((o) => o.textContent), ["v3 (newest)", "v2", "v1"]);
  assert.equal(ui.shownVersion(), "v3 (newest)");
  assert.deepEqual(ui.tiles(), ["plan", "build"]);
  assert.equal(ui.save().disabled, true);
  assert.equal(ui.save().title, "No unsaved edits");
  // Without unsaved edits the one primary button runs the version on the canvas.
  assert.equal(ui.save().hidden, true, "Save gives its place to Run…");
  assert.equal(ui.runButton().hidden, false);
  assert.equal(ui.runButton().disabled, false);
  assert.equal(ui.runButton().textContent, "Run…");
  assert.equal(ui.runButton().title, "Opens Select Inputs to start runs of v3");
  assert.equal(ui.note().hidden, true);
  assert.equal(ui.context.location.hash, "#workflows/demo/plan", "the newest version needs no version in the hash");
  assert.deepEqual(ui.posts(), [], "loading writes nothing");
  // A workflow tile names the workflow and the version it is pinned to.
  const build = find(ui.byId("canvas"), (n) => n.attrs["data-step"] === "build");
  assert.equal(build.attrs["aria-label"], "Step Build: Inner v1");
});

test("an edit stays unsaved until Save, which stores the next version and shows it", async () => {
  const ui = page();
  await ui.open();
  await ui.run("Add agent");
  assert.deepEqual(ui.tiles(), ["plan", "build", "step-1"]);
  assert.equal(ui.save().disabled, false);
  assert.equal(ui.save().title, "Stores the workflow as v4");
  assert.equal(ui.save().hidden, false, "an edit turns Run… into Save");
  assert.equal(ui.runButton().hidden, true);
  await settle();
  assert.deepEqual(ui.posts(), [], "no autosave");
  // Selecting another node keeps the unsaved edit.
  await fire(find(ui.byId("canvas"), (n) => n.attrs["data-step"] === "plan"), "keydown", { key: "Enter" });
  assert.deepEqual(ui.tiles(), ["plan", "build", "step-1"]);
  assert.equal(ui.save().disabled, false);
  // With unsaved edits a run is refused: it would use the stored version, not what is shown.
  assert.equal((await ui.menu("Run workflow…"))[4], true);
  assert.match(ui.context.blocker(), /saved first/);
  await ui.save().onclick();
  await settle();
  const [post] = ui.posts();
  assert.equal(post.method, "POST");
  assert.equal(post.path, "/api/orchestrator/workflows/demo/versions");
  assert.deepEqual(post.headers, { "X-Creator": "ada" });
  assert.deepEqual(Object.keys(post.body).sort(), ["name", "steps"], "without the fields the orchestrator sets");
  assert.deepEqual(post.body.steps.map((s) => s.id), ["plan", "build", "step-1"]);
  assert.equal(post.body.steps[1].workflow, "inner@v1", "the pin is stored as it is");
  assert.ok(post.body.steps[2].position, "positions are saved with the version");
  assert.deepEqual(ui.versions().children.map((o) => o.textContent), ["v4 (newest)", "v3", "v2", "v1"]);
  assert.equal(ui.shownVersion(), "v4 (newest)");
  assert.equal(ui.save().disabled, true);
  assert.equal(ui.notices.at(-1).text, "Saved v4");
  assert.equal(ui.context.blocker(), "");
  // The run now uses the version on the canvas.
  await ui.run("Run workflow…");
  assert.deepEqual(ui.inputs.at(-1), ["demo", 4]);
});

test("a refused save keeps the edits and names the reason", async () => {
  const ui = page();
  await ui.open();
  await ui.run("Add agent");
  const real = ui.context.api;
  ui.context.api = async (method, path, ...rest) => {
    if (method === "POST") { const e = new Error('step "step-1" names an agent that does not exist'); e.status = 422; throw e; }
    return real(method, path, ...rest);
  };
  await ui.save().onclick();
  await settle();
  assert.deepEqual(ui.notices.at(-1), { text: "Not saved: step Step 1 names an agent that does not exist", kind: "error" });
  assert.deepEqual(ui.tiles(), ["plan", "build", "step-1"]);
  assert.equal(ui.save().disabled, false);
  assert.equal(ui.shownVersion(), "v3 (newest)");
});

test("an older version is read-only until edited and is then saved as the next version", async () => {
  const ui = page();
  await ui.open();
  ui.versions().value = "2";
  await fire(ui.versions(), "change");
  assert.ok(ui.server.calls.some((c) => c.method === "GET" && c.path === "/api/orchestrator/workflows/demo/versions/2"));
  assert.equal(ui.shownVersion(), "v2");
  assert.equal(ui.note().hidden, false);
  assert.equal(ui.note().textContent, "Viewing v2");
  assert.equal(ui.save().disabled, true);
  assert.equal(ui.context.location.hash, "#workflows/demo/plan?version=2", "a reload returns to this version");
  // A run from here uses v2.
  await ui.run("Run workflow…");
  assert.deepEqual(ui.inputs.at(-1), ["demo", 2]);
  await ui.run("Add agent");
  assert.equal(ui.note().textContent, "Edited v2: saves as v4");
  assert.equal(ui.save().title, "Stores the workflow as v4");
  await ui.save().onclick();
  await settle();
  assert.equal(ui.posts()[0].path, "/api/orchestrator/workflows/demo/versions");
  assert.equal(ui.shownVersion(), "v4 (newest)");
  assert.equal(ui.note().hidden, true);
  assert.equal(ui.context.location.hash, "#workflows/demo/step-1", "the saved version is the newest: no version in the hash");
  assert.equal(ui.server.store.demo[1].steps.length, 2, "v2 itself never changes");
});

test("a reload returns to the version in the hash; one that is gone gives way to the newest", async () => {
  const ui = page();
  await ui.open("/demo/plan?version=1");
  assert.equal(ui.shownVersion(), "v1");
  assert.deepEqual(ui.tiles(), ["plan"]);
  assert.equal(ui.note().textContent, "Viewing v1");
  const other = page();
  await other.open("/demo?version=9");
  assert.equal(other.shownVersion(), "v3 (newest)");
  assert.equal(other.context.location.hash, "#workflows/demo/plan");
});

test("another workflow or version is opened over unsaved edits only after asking", async () => {
  const ui = page();
  await ui.open();
  await ui.run("Add agent");
  ui.answers.confirm = false;
  await fire(ui.rows()[1], "click");
  assert.deepEqual(ui.questions, ["Discard your unsaved changes? [Discard]"]);
  assert.equal(ui.selectedRow(), "Demo");
  assert.deepEqual(ui.tiles(), ["plan", "build", "step-1"], "the edits stay");
  // The version select and a link (the hash) ask as well; kept edits put the hash back.
  ui.versions().value = "1";
  await fire(ui.versions(), "change");
  assert.equal(ui.shownVersion(), "v3 (newest)");
  assert.equal(ui.questions.length, 2);
  ui.context.location.hash = "#workflows/inner";
  await ui.open("/inner");
  assert.equal(ui.questions.length, 3);
  assert.equal(ui.selectedRow(), "Demo");
  assert.equal(ui.context.location.hash, "#workflows/demo/step-1");
  // Leaving the page and coming back keeps the edits without loading over them.
  const gets = ui.server.calls.filter((c) => c.path === "/api/orchestrator/workflows").length;
  ui.context.tabs.workflows.hide();
  await ui.open("");
  assert.deepEqual(ui.tiles(), ["plan", "build", "step-1"]);
  assert.equal(ui.server.calls.filter((c) => c.path === "/api/orchestrator/workflows").length, gets);
  assert.equal(ui.questions.length, 3, "the same workflow asks nothing");
  // Discarding opens the other workflow and drops the edits.
  ui.answers.confirm = true;
  await fire(ui.rows()[1], "click");
  assert.equal(ui.selectedRow(), "Inner");
  assert.deepEqual(ui.tiles(), ["write"]);
  assert.equal(ui.save().disabled, true);
  await fire(ui.rows()[0], "click");
  assert.equal(ui.questions.length, 4, "nothing to ask without edits");
  assert.deepEqual(ui.tiles(), ["plan", "build"]);
  assert.deepEqual(ui.posts(), []);
});

test("undo puts a deleted step back and leaves it unsaved like any edit", async () => {
  const ui = page();
  await ui.open();
  const tile = find(ui.byId("canvas"), (n) => n.attrs["data-step"] === "plan");
  await ui.run("Delete step", tile);
  assert.deepEqual(ui.tiles(), ["build"]);
  assert.equal(ui.save().disabled, false);
  assert.equal(ui.notices.at(-1).text, "Deleted");
  ui.context.undo();
  await settle();
  assert.deepEqual(ui.tiles().sort(), ["build", "plan"]);
  assert.equal(ui.save().disabled, true, "back to the stored version: nothing to save");
  assert.deepEqual(ui.posts(), []);
});

test("a workflow node opens the version it is pinned to, and the path leads back", async () => {
  const ui = page();
  await ui.open();
  await fire(ui.byId("canvas"), "dblclick", { target: { closest: () => ({ getAttribute: () => "build" }) } });
  assert.ok(ui.server.calls.some((c) => c.path === "/api/orchestrator/workflows/inner/versions/1"), "the pinned v1 is loaded");
  assert.equal(ui.selectedRow(), "Inner");
  assert.deepEqual(ui.tiles(), ["old-write"], "v1 of inner, not its newest");
  assert.equal(ui.shownVersion(), "v1");
  assert.equal(ui.context.location.hash, "#workflows/inner/old-write?version=1&path=demo");
  const path = find(ui.context.document.querySelector("#tab-workflows .canvas-head"), (n) => n.className === "wf-path");
  assert.equal(path.hidden, false);
  assert.equal(path.children[0].textContent, "Demo");
  assert.equal(path.children[0].href, "#workflows/demo");
});

test("a new workflow asks for a group and a name, and is a draft until its first Save stores v1", async () => {
  const ui = page();
  await ui.open();
  const before = ui.notices.length;
  await ui.run("New workflow");
  assert.equal(ui.dialog().title, "New workflow");
  assert.equal(ui.field("wf-name-group").value, "", "an ungrouped workflow on the canvas suggests no group");
  assert.equal(ui.field("wf-name-name").value, "Workflow 1", "the lowest free number");
  // The group is required, as on Agents and Policies; the problem shows under the field.
  await ui.submit();
  assert.equal(find(ui.dialog().body, (n) => n.className === "error-text" && n.textContent).textContent, "Enter a group.");
  assert.deepEqual(ui.names(), ["Demo", "Inner"], "nothing is made before the dialog is done");
  await ui.submit({ "wf-name-group": "Notes", "wf-name-name": "Release Notes" });
  assert.equal(ui.dialog(), undefined);
  assert.deepEqual(ui.names(), ["Notes", "Release Notes", "Demo", "Inner"], "groups come first; the group of the draft opens");
  assert.equal(ui.selectedRow(), "Release Notes");
  assert.equal(ui.versions().hidden, true, "nothing stored yet");
  assert.equal(ui.note().textContent, "Not saved yet");
  assert.equal(ui.save().disabled, true);
  assert.equal(ui.save().title, "Needs a step");
  assert.deepEqual(ui.posts(), [], "a draft is not stored");
  assert.equal(ui.notices.length, before, "creating it shows no notice");
  await ui.run("Add agent");
  assert.equal(ui.save().disabled, false);
  assert.equal(ui.save().title, "Stores the workflow as v1");
  assert.equal((await ui.menu("Run workflow…"))[4], true, "a draft cannot run");
  await ui.save().onclick();
  await settle();
  assert.equal(ui.posts()[0].path, "/api/orchestrator/workflows/notes%2Frelease-notes/versions", "the grouped name is one part of the path");
  assert.equal(ui.posts()[0].body.name, "notes/release-notes");
  assert.equal(ui.versions().hidden, false);
  assert.equal(ui.shownVersion(), "v1 (newest)");
  assert.equal(ui.notices.at(-1).text, "Saved v1");
  assert.equal(ui.context.location.hash, "#workflows/notes%2Frelease-notes/step-1");
  // New in a group's menu starts in that group. A draft that is left without a step is gone.
  await ui.run("New workflow", ui.row("Notes"));
  assert.equal(ui.field("wf-name-group").value, "Notes");
  assert.equal(ui.field("wf-name-name").value, "Workflow 1");
  await ui.submit();
  assert.deepEqual(ui.names(), ["Notes", "Release Notes", "Workflow 1", "Demo", "Inner"]);
  await fire(ui.row("Demo"), "click");
  assert.deepEqual(ui.names(), ["Notes", "Release Notes", "Demo", "Inner"]);
  assert.equal(ui.questions.length, 0, "an empty draft holds no work");
});

test("rename, duplicate and delete go to the orchestrator", async () => {
  const ui = page();
  await ui.open();
  // Duplicate stores the copy at once as v1 of a new name and opens it.
  await ui.run("Duplicate workflow");
  assert.equal(ui.posts()[0].path, "/api/orchestrator/workflows/demo-copy/versions");
  assert.equal(ui.posts()[0].body.name, "demo-copy");
  assert.equal(ui.posts()[0].body.steps.length, 2);
  assert.equal(ui.selectedRow(), "Demo Copy");
  assert.equal(ui.shownVersion(), "v1 (newest)");
  assert.equal(ui.notices.some((n) => n.text === "Duplicated"), true);
  // Rename asks for the group and the name; another group moves the workflow there. The
  // orchestrator follows the name in every workflow; the page follows too.
  await fire(ui.row("Inner"), "click");
  await ui.run("Rename workflow…");
  assert.equal(ui.dialog().title, "Rename workflow");
  assert.equal(ui.field("wf-name-group").value, "");
  assert.equal(ui.field("wf-name-name").value, "Inner");
  await ui.submit({ "wf-name-name": "Core" });
  assert.equal(find(ui.dialog().body, (n) => n.className === "error-text" && n.textContent).textContent, "Enter a group.", "Rename needs a group as well");
  await ui.submit({ "wf-name-group": "Ecc", "wf-name-name": "Core" });
  const rename = ui.posts().at(-1);
  assert.equal(rename.path, "/api/orchestrator/workflows/inner/rename");
  assert.deepEqual(rename.body, { name: "ecc/core" });
  assert.equal(ui.dialog(), undefined);
  assert.deepEqual(ui.names(), ["ECC", "Core", "Demo", "Demo Copy"]);
  assert.equal(ui.selectedRow(), "Core");
  assert.equal(ui.notices.at(-1).text, "Renamed");
  // A name taken already is refused under the field, names that differ only in case included.
  await ui.run("Rename workflow…", ui.row("Demo Copy"));
  await ui.submit({ "wf-name-group": "ECC", "wf-name-name": "core" });
  assert.equal(find(ui.dialog().body, (n) => n.className === "error-text" && n.textContent).textContent, "There is already a workflow named ECC / Core.");
  ui.dialog().close();
  await fire(ui.row("Demo"), "click");
  const build = find(ui.byId("canvas"), (n) => n.attrs["data-step"] === "build");
  assert.equal(build.attrs["aria-label"], "Step Build: ECC / Core v1", "the node follows the new name and keeps its version");
  assert.equal(ui.save().disabled, true, "following a rename is no unsaved edit");
  // Delete asks first, then removes every version; the next workflow in its group takes its place.
  ui.answers.confirm = false;
  await ui.run("Delete workflow");
  assert.equal(ui.questions.at(-1), "Delete Demo with all its versions? [Delete]");
  assert.equal(ui.posts().some((c) => c.method === "DELETE"), false);
  ui.answers.confirm = true;
  await ui.run("Delete workflow");
  assert.equal(ui.posts().at(-1).method, "DELETE");
  assert.equal(ui.posts().at(-1).path, "/api/orchestrator/workflows/demo");
  assert.deepEqual(ui.names(), ["ECC", "Core", "Demo Copy"]);
  assert.equal(ui.selectedRow(), "Demo Copy");
  assert.equal(ui.notices.at(-1).text, "Deleted");
  // A duplicate stays in the group of its original.
  await ui.run("Duplicate workflow", ui.row("Core"));
  assert.equal(ui.posts().at(-1).path, "/api/orchestrator/workflows/ecc%2Fcore-copy/versions");
  assert.equal(ui.posts().at(-1).body.name, "ecc/core-copy");
  assert.deepEqual(ui.names(), ["ECC", "Core", "Core Copy", "Demo Copy"]);
  assert.equal(ui.selectedRow(), "Core Copy");
  // Delete group asks first and deletes every workflow in it; the workflow after the group takes its place.
  await ui.run("Delete group", ui.row("ECC"));
  assert.equal(ui.questions.at(-1), "Delete ECC with its 2 workflows and all their versions? [Delete]");
  assert.deepEqual(ui.posts().filter((c) => c.method === "DELETE").slice(-2).map((c) => c.path).sort(), ["/api/orchestrator/workflows/ecc%2Fcore", "/api/orchestrator/workflows/ecc%2Fcore-copy"]);
  assert.deepEqual(ui.names(), ["Demo Copy"]);
  assert.equal(ui.selectedRow(), "Demo Copy");
  assert.deepEqual(Object.keys(ui.server.store).sort(), ["demo-copy"]);
  assert.equal(ui.posts().some((c) => c.method === "PUT"), false);
});

test("the sidebar is a tree of groups whose folders start closed, but the group of the workflow shown opens", async () => {
  const ui = page();
  const flow = (name) => [{ name, version: 1, created: "2026-11-01T10:00:00Z", created_by: "ada", steps: [{ id: "plan", service: "agent", agent: "demo/coder@v1", position: { x: 0, y: 40 } }] }];
  for (const name of ["ecc/ECC-plan", "ecc/review/code-review", "ops/deploy"]) ui.server.store[name] = flow(name);
  await ui.open("/ecc%2Freview%2Fcode-review/plan");
  // In every group its subgroups first, then its workflows; a row shows the last part in title case.
  assert.deepEqual(ui.names(), ["ECC", "Review", "Code Review", "ECC Plan", "Ops", "Demo", "Inner"]);
  assert.equal(ui.selectedRow(), "Code Review");
  assert.equal(ui.context.location.hash, "#workflows/ecc%2Freview%2Fcode-review/plan");
  assert.deepEqual(ui.rows().filter((r) => ["true", "false"].includes(r.attrs["aria-expanded"])).map((r) => [r.textContent, r.attrs["aria-expanded"]]), [["ECC", "true"], ["Review", "true"], ["Ops", "false"]], "every group has a triangle");
  // A click on a group opens it and keeps the workflow on the canvas.
  await fire(ui.row("Ops"), "click");
  assert.deepEqual(ui.names(), ["ECC", "Review", "Code Review", "ECC Plan", "Ops", "Deploy", "Demo", "Inner"]);
  assert.equal(ui.selectedRow(), "Ops");
  await ui.run("Add agent");
  assert.equal(ui.selectedRow(), "Ops", "an edit leaves the group picked");
  ui.answers.confirm = false;
  await fire(ui.row("Deploy"), "click");
  assert.equal(ui.selectedRow(), "Code Review", "kept edits mark the workflow on the canvas again");
  ui.answers.confirm = true;
  await fire(ui.row("Deploy"), "click");
  assert.equal(ui.context.location.hash, "#workflows/ops%2Fdeploy/plan");
  assert.equal(ui.selectedRow(), "Deploy");
  // A link typed with plain slashes finds the grouped name as well.
  await ui.open("/ecc/ECC-plan/plan");
  assert.equal(ui.selectedRow(), "ECC Plan");
  assert.equal(ui.context.location.hash, "#workflows/ecc%2FECC-plan/plan");
  // New group… adds an empty group in this visit; it is stored once it holds a workflow.
  await ui.run("New group…", ui.row("ECC"));
  assert.equal(ui.dialog().title, "New group");
  ui.field("wf-group-name").value = "Plans";
  await fire(ui.dialog().footer.find((b) => b.className === "primary"), "click");
  assert.equal(ui.selectedRow(), "Plans");
  assert.deepEqual(ui.names().slice(0, 4), ["ECC", "Plans", "Review", "Code Review"]);
  await ui.run("New workflow", ui.row("Plans"));
  assert.equal(ui.field("wf-name-group").value, "ECC / Plans");
  await ui.submit({ "wf-name-name": "Q3 Plan" });
  assert.equal(ui.selectedRow(), "Q3 Plan");
  assert.equal(ui.context.location.hash, "#workflows/ecc%2Fplans%2Fq3-plan");
  // The flat pickers name a grouped workflow with its groups.
  assert.equal(vm.runInContext("titleCase", ui.context)("ecc/review/code-review"), "ECC / Review / Code Review");
  await fire(ui.row("Demo"), "click");
  await ui.open("/demo/build");
  const nested = find(ui.byId("inspector"), (n) => n.dataset.field === "workflow");
  assert.deepEqual(nested.children.map((o) => o.textContent).sort(), ["ECC / ECC Plan", "ECC / Review / Code Review", "Inner", "Ops / Deploy"]);
});

test("the node panel of a workflow step has a Workflow and a Version select", async () => {
  const ui = page();
  await ui.open("/demo/build");
  const field = (name) => find(ui.byId("inspector"), (n) => n.dataset.field === name);
  assert.deepEqual(field("workflow").children.map((o) => [o.value, o.selected]), [["inner", true]]);
  assert.deepEqual(field("workflow_version").children.map((o) => [o.textContent, o.selected]), [["v2 (newest)", false], ["v1", true]]);
  // The steps card shows the pinned version, loaded on demand.
  assert.ok(ui.server.calls.some((c) => c.path === "/api/orchestrator/workflows/inner/versions/1"));
  assert.equal(find(ui.byId("inspector"), (n) => n.className === "nested-step-name").textContent, "Old Write");
  // Choosing the newest version changes the stored reference and is an unsaved edit.
  const form = find(ui.byId("inspector"), (n) => n.className === "step-form");
  const version = field("workflow_version");
  version.value = "2";
  await fire(form, "change", { target: version });
  assert.equal(ui.save().disabled, false);
  await ui.save().onclick();
  await settle();
  assert.equal(ui.posts()[0].body.steps[1].workflow, "inner@v2");
});

test("clicking or dragging between team workflows is refused without dirtying the graph", async () => {
  const ui = page();
  ui.server.crew.steps[0].workflows = ["inner@v1", "demo@v3"];
  ui.server.crew.steps[0].channels = [["coder", "inner"], ["coder", "demo"]];
  ui.server.store.crew = [ui.server.crew];
  await ui.open("/crew/team");
  const member = (name) => find(ui.byId("inspector"), (n) => n.dataset.member === name);
  await fire(member("inner"), "click");
  assert.match(member("demo").title, /Two workflows cannot/);
  await fire(member("demo"), "click");
  assert.match(ui.notices.at(-1).text, /Two workflows cannot/);
  assert.equal(ui.save().disabled, true);
  const target = member("demo");
  target.closest = () => target;
  ui.context.document.elementFromPoint = () => target;
  const port = find(member("inner"), (n) => n.className === "team-port");
  await fire(port, "pointerdown", { clientX: 10, clientY: 10, pointerId: 1 });
  await fire(port, "pointerup", { clientX: 20, clientY: 20, pointerId: 1 });
  assert.match(ui.notices.at(-1).text, /Two workflows cannot/);
  assert.equal(ui.save().disabled, true);
  assert.deepEqual(ui.posts(), []);
  // An agent/workflow channel is still editable.
  await fire(member("coder"), "click");
  await fire(member("demo"), "click");
  assert.equal(ui.save().disabled, false);
  await ui.save().onclick();
  await settle();
  assert.deepEqual(ui.posts()[0].body.steps[0].channels, [["coder", "inner"]]);
});

test("a team's workflow member is pinned, named by its workflow, and can move to the newest version", async () => {
  const ui = page();
  ui.server.store.crew = [ui.server.crew];
  await ui.open("/crew/team");
  assert.equal(find(ui.byId("inspector"), (n) => n.tag === "button" && ["Make lead", "Remove"].includes(n.textContent)), undefined, "member actions stay in the context menu only");
  const member = () => find(ui.byId("inspector"), (n) => n.dataset.member === "inner");
  assert.equal(member().textContent, "innerWorkflow v1", "the name without the version, then the pinned version");
  assert.equal(find(ui.byId("canvas"), (n) => n.attrs["data-step"] === "team").attrs["aria-label"], "Step Team: demo/coder@v1 · 2 members");
  // "+" offers the workflows that are not members yet, pinned to their newest version.
  const menus = [];
  ui.context.openMenu = (_ev, items) => menus.push(items);
  await fire(find(ui.byId("inspector"), (n) => n.title === "Add member"), "click");
  assert.deepEqual(plain(menus.at(-1).filter(Array.isArray).map((item) => item[0])), ["Demo v3 (workflow)"], "inner is a member already, at whatever version");
  // The member's menu opens the pinned version or moves the pin to the newest.
  await fire(member(), "contextmenu");
  const labels = plain(menus.at(-1).filter(Array.isArray).map((item) => item[0]));
  assert.deepEqual(labels, ["Open workflow", "Use v2", "Make lead", "Reset layout", "Remove member"]);
  await menus.at(-1).find((item) => item[0] === "Use v2")[1]();
  await settle();
  assert.equal(member().textContent, "innerWorkflow v2");
  assert.equal(ui.save().disabled, false);
  await ui.save().onclick();
  await settle();
  const stored = ui.posts()[0].body.steps[0];
  assert.deepEqual(stored.workflows, ["inner@v2"]);
  assert.deepEqual(stored.channels, [["coder", "inner"]], "the channel names the member and stays");
  // Adding the offered workflow stores its pinned reference.
  await fire(find(ui.byId("inspector"), (n) => n.title === "Add member"), "click");
  await menus.at(-1).find((item) => item[0] === "Demo v3 (workflow)")[1]();
  await settle();
  await ui.save().onclick();
  await settle();
  assert.deepEqual(ui.posts()[1].body.steps[0].workflows, ["inner@v2", "demo@v3"]);
  // Open workflow shows the pinned version.
  await fire(member(), "contextmenu");
  await menus.at(-1).find((item) => item[0] === "Open workflow")[1]();
  await settle();
  assert.equal(ui.selectedRow(), "Inner");
  assert.equal(ui.shownVersion(), "v2 (newest)");
  assert.equal(ui.context.location.hash, "#workflows/inner/write?path=crew");
});

// ---- Merge and batch nodes ----

// fan adds a workflow with a merge, a batch, and a route step after the merge.
function fan(ui) {
  const agent = (id, extra = {}) => ({ id, service: "agent", agent: "demo/coder@v1", ...extra });
  ui.server.store.fan = [{ name: "fan", version: 1, created: "2026-11-01T10:00:00Z", created_by: "ada", steps: [
    agent("a", { position: { x: 0, y: 40 } }), agent("b", { position: { x: 268, y: 40 } }),
    { id: "collect", service: "merge", depends_on: ["a", "b"], capacity: 4, position: { x: 0, y: 176 } },
    { id: "gather", service: "batch", depends_on: ["b"], size: 2, input_timeout: 30, position: { x: 268, y: 176 } },
    { id: "next", service: "route", depends_on: ["collect"], position: { x: 0, y: 312 } },
  ] }];
}
const panelField = (ui, name) => find(ui.byId("inspector"), (n) => n.dataset.field === name);
const panelProblems = (ui) => all(ui.byId("inspector")).filter((n) => n.className === "error-text team-problem").map((n) => n.textContent);
const panelLabels = (ui) => all(ui.byId("inspector")).filter((n) => n.tag === "label").map((n) => n.textContent);
const change = async (ui, name, value) => {
  const control = panelField(ui, name);
  control.value = value;
  await fire(find(ui.byId("inspector"), (n) => n.className === "step-form"), "change", { target: control });
};

test("a merge and a batch have their own tiles: a funnel and a tray, named by what they do", async () => {
  const ui = page();
  fan(ui);
  await ui.open("/fan/collect");
  const tile = (id) => find(ui.byId("canvas"), (n) => n.attrs["data-step"] === id);
  const body = (id) => find(tile(id), (n) => n.attrs.class === "body").attrs.d;
  const lines = (id) => all(tile(id)).filter((n) => n.tag === "text").map((n) => n.textContent);
  assert.equal(body("collect"), "M0,0 H220 V20 L192,64 H28 L0,20 z", "a funnel: wide at the top, narrow at the bottom");
  assert.deepEqual(lines("collect"), ["Collect", "Waits for all"]);
  assert.deepEqual(lines("gather"), ["Gather", "Collects 2"]);
  assert.ok(find(tile("gather"), (n) => n.attrs.class === "tray-line"), "a tray: a second line under the top edge");
  assert.equal(find(tile("collect"), (n) => n.attrs.class === "tray-line"), undefined);
  assert.notEqual(body("gather"), body("a"));
  // Both keep the ports of every tile, in and out.
  for (const id of ["collect", "gather"]) assert.equal(all(tile(id)).filter((n) => n.attrs.class === "port").length, 2, id);
  // The outline names the kind; no tile carries a coloured bar.
  assert.ok(tile("collect").attrs.class.includes("kind-merge") && tile("gather").attrs.class.includes("kind-batch"));
  for (const id of ["collect", "gather", "a"]) assert.equal(find(tile(id), (n) => /^bar-/.test(n.attrs.class || "")), undefined, id);
});

test("the node panel of a merge or batch has Node ID, its sources and its parameters, and no agent fields", async () => {
  const ui = page();
  fan(ui);
  await ui.open("/fan/collect");
  assert.equal(find(ui.byId("inspector"), (n) => n.tag === "h2").textContent, "Merge");
  assert.deepEqual(panelLabels(ui), ["Node ID", "Sources", "Service", "Capacity", "Input timeout"]);
  assert.equal(find(ui.byId("inspector"), (n) => n.className === "node-sources").textContent, "A, B", "the sources in their order, read-only");
  assert.equal(panelField(ui, "capacity").value, 4);
  assert.equal(panelField(ui, "capacity").placeholder, "16", "the default");
  assert.equal(panelField(ui, "input_timeout").value, "");
  assert.equal(panelField(ui, "input_timeout").placeholder, "300");
  assert.equal(panelField(ui, "capacity").type, undefined, "a plain digit field, no spinner");
  for (const gone of ["agent", "timeout", "attempts", "question", "activity", "size"]) assert.equal(panelField(ui, gone), undefined, gone);
  assert.deepEqual(panelProblems(ui), []);

  await ui.open("/fan/gather");
  assert.equal(find(ui.byId("inspector"), (n) => n.tag === "h2").textContent, "Batch");
  assert.deepEqual(panelLabels(ui), ["Node ID", "Sources", "Service", "Size", "Input timeout"]);
  assert.equal(find(ui.byId("inspector"), (n) => n.className === "node-sources").textContent, "B");
  assert.equal(panelField(ui, "size").value, 2);
  assert.equal(panelField(ui, "input_timeout").value, 30);
  assert.equal(panelField(ui, "capacity"), undefined);

  // The parameters are stored as numbers and left out when empty; Save sends them.
  await change(ui, "size", "3");
  await change(ui, "input_timeout", "");
  await ui.open("/fan/collect");
  await change(ui, "input_timeout", "120");
  await ui.save().onclick();
  await settle();
  const stored = ui.posts()[0].body.steps;
  assert.deepEqual(stored.find((s) => s.id === "gather"), { id: "gather", service: "batch", depends_on: ["b"], size: 3, position: { x: 268, y: 176 } });
  assert.deepEqual(stored.find((s) => s.id === "collect"), { id: "collect", service: "merge", depends_on: ["a", "b"], capacity: 4, input_timeout: 120, position: { x: 0, y: 176 } });
});

test("problems the orchestrator would refuse show in the node panel, like a team's", async () => {
  const ui = page();
  fan(ui);
  await ui.open("/fan/next");
  assert.deepEqual(panelProblems(ui), ["A router cannot follow a merge: Collect"]);
  await ui.open("/fan/gather");
  await change(ui, "size", "0");
  assert.deepEqual(panelProblems(ui), ["Size must be a whole number of 1 or more"]);
  await change(ui, "size", "");
  await change(ui, "input_timeout", "-1");
  assert.deepEqual(panelProblems(ui), ["A batch needs a size", "Input timeout must be a whole number of 1 or more"]);
  // A new node has no source yet; a batch has no size either.
  await ui.run("Add merge");
  assert.ok(ui.tiles().includes("merge-1"));
  assert.deepEqual(panelProblems(ui), ["A merge needs at least one source"]);
  assert.equal(find(ui.byId("inspector"), (n) => n.className === "node-sources none").textContent, "No sources");
  await ui.run("Add batch");
  assert.ok(ui.tiles().includes("batch-1"));
  assert.deepEqual(panelProblems(ui), ["A batch needs at least one source", "A batch needs a size"]);
  // The orchestrator stays the authority: Save is not blocked by a problem.
  assert.equal(ui.save().disabled, false);
});

test("a step that changes its kind to or from merge and batch drops the fields of the other kind", async () => {
  const ui = page();
  fan(ui);
  await ui.open("/fan/a");
  await change(ui, "timeout", "5m");
  await change(ui, "service", "batch");
  assert.equal(find(ui.byId("inspector"), (n) => n.tag === "h2").textContent, "Batch");
  for (const gone of ["agent", "timeout", "attempts"]) assert.equal(panelField(ui, gone), undefined, gone);
  await ui.open("/fan/gather");
  await change(ui, "service", "merge");
  assert.equal(panelField(ui, "size"), undefined);
  assert.equal(panelField(ui, "input_timeout").value, 30, "the parameter both kinds have stays");
  await ui.open("/fan/collect");
  await change(ui, "service", "agent");
  await ui.save().onclick();
  await settle();
  const stored = ui.posts()[0].body.steps;
  assert.deepEqual(stored.find((s) => s.id === "a"), { id: "a", service: "batch", position: { x: 0, y: 40 } });
  assert.deepEqual(stored.find((s) => s.id === "gather"), { id: "gather", service: "merge", depends_on: ["b"], input_timeout: 30, position: { x: 268, y: 176 } });
  assert.deepEqual(stored.find((s) => s.id === "collect"), { id: "collect", service: "agent", depends_on: ["a", "b"], position: { x: 0, y: 176 } });
});

// ---- Dev pod, command and stop nodes, and a gate's back edge ----

// ship adds a workflow that works in a dev pod: pod, build, verify, review, stop, top to bottom.
function ship(ui) {
  ui.server.store.ship = [{ name: "ship", version: 1, created: "2026-11-01T10:00:00Z", created_by: "ada", steps: [
    { id: "pod", service: "devpod", repository: "claims-build", source_branch: "main", work_branch: "agent/pay-{run}", protected: ["tests/**"],
      rules: [{ tool: "write_file", path: "src/**", matches: "secret", message: "Blocked by R1" }], position: { x: 0, y: 40 } },
    { id: "build", service: "agent", agent: "demo/coder@v1", depends_on: ["pod"], position: { x: 0, y: 176 } },
    { id: "verify", service: "command", depends_on: ["build"], command: "./verify.sh", timeout: "5m", position: { x: 0, y: 312 } },
    { id: "review", service: "gate", depends_on: ["verify"], question: "Good?", position: { x: 0, y: 448 } },
    { id: "stop", service: "devpod-stop", depends_on: ["review"], position: { x: 0, y: 584 } },
  ] }];
}
const storedStep = (ui, id) => ui.posts().at(-1).body.steps.find((s) => s.id === id);
const saveNow = async (ui) => { await ui.save().onclick(); await settle(); };

test("a dev pod, a command and a stop step have their own tiles, named by what they do", async () => {
  const ui = page();
  ship(ui);
  await ui.open("/ship/pod");
  const tile = (id) => find(ui.byId("canvas"), (n) => n.attrs["data-step"] === id);
  const body = (id) => find(tile(id), (n) => n.attrs.class === "body").attrs.d;
  const lines = (id) => all(tile(id)).filter((n) => n.tag === "text").map((n) => n.textContent);
  assert.equal(body("pod"), "M14,0 H206 L220,32 L206,64 H14 L0,32 z", "a hexagon");
  assert.equal(body("verify"), "M12,0 H220 L208,64 H0 z", "a slanted box");
  assert.equal(body("stop"), "M0,0 H220 L206,32 L220,64 H0 L14,32 z", "a box notched on each side");
  assert.equal(new Set(["pod", "build", "verify", "review", "stop"].map(body)).size, 5, "every kind has its own outline");
  assert.deepEqual(lines("pod"), ["Pod", "claims-build · agent/pay-…"], "the repository and the work branch, cut where the tile ends");
  assert.equal(find(tile("pod"), (n) => n.tag === "title").textContent, "Pod: claims-build · agent/pay-{run}", "in full on hover");
  assert.deepEqual(lines("verify"), ["Verify", "./verify.sh"]);
  assert.deepEqual(lines("stop"), ["Stop", "Deletes the dev pod"]);
  for (const [id, kind] of [["pod", "kind-devpod"], ["verify", "kind-command"], ["stop", "kind-devpod-stop"]]) assert.ok(tile(id).attrs.class.includes(kind), kind);
  // They keep the ports of every tile; only a gate has the Rejected port.
  for (const id of ["pod", "verify", "stop"]) assert.equal(all(tile(id)).filter((n) => (n.attrs.class || "").startsWith("port")).length, 2, id);
  assert.equal(all(tile("review")).filter((n) => n.attrs.class === "port port-reject").length, 1);
  for (const [id, name] of [["pod", "Dev Pod"], ["verify", "Command"], ["stop", "Stop Dev Pod"]]) {
    await ui.open(`/ship/${id}`);
    assert.equal(find(ui.byId("inspector"), (n) => n.tag === "h2").textContent, name);
  }
});

test("the node panel of a dev pod has exactly its fields: repository, branches, name, protected paths and rules", async () => {
  const ui = page();
  ship(ui);
  await ui.open("/ship/pod");
  assert.deepEqual(panelLabels(ui), ["Node ID", "Repository", "Source branch", "Work branch", "Name", "Service", "Protected paths", "Rules", "Tool", "Path", "Matches", "Message"]);
  assert.deepEqual(panelField(ui, "repository").children.map((o) => o.textContent), ["claims-build", "site"], "filled from the connectors API");
  assert.equal(panelField(ui, "work_branch").value, "agent/pay-{run}");
  assert.equal(panelField(ui, "work_branch").placeholder, "agent/{run}", "the default");
  assert.equal(panelField(ui, "name").placeholder, "No name");
  assert.match(find(ui.byId("inspector"), (n) => n.tag === "label" && n.textContent === "Name").title, /continues in the live dev pod/);
  assert.equal(panelField(ui, "protected").value, "tests/**");
  assert.equal(panelField(ui, "rule-0-tool").children.find((o) => o.selected).textContent, "write_file");
  assert.equal(panelField(ui, "rule-0-message").value, "Blocked by R1");
  for (const gone of ["agent", "timeout", "attempts", "question", "activity", "command"]) assert.equal(panelField(ui, gone), undefined, gone);
  assert.deepEqual(panelProblems(ui), []);

  await change(ui, "name", "claims");
  await change(ui, "protected", "tests/**\n\n  verify.sh  \n");
  // A rule is edited field by field; an empty path or pattern is left out.
  const rule = async (field, value) => { const control = panelField(ui, field); control.value = value; await fire(control, "change"); };
  await rule("rule-0-path", "");
  await rule("rule-0-tool", "run_command");
  await find(ui.byId("inspector"), (n) => n.title === "Add rule").onclick();
  await settle();
  assert.deepEqual(panelProblems(ui), ["Rule 2 needs a message that names the rule, the reason and the fix"]);
  await rule("rule-1-message", "Blocked by R2");
  await rule("rule-1-matches", "(");
  assert.deepEqual(panelProblems(ui), ["Rule 2 has a pattern that is not a regular expression"]);
  await rule("rule-1-matches", "rm -rf");
  await saveNow(ui);
  assert.deepEqual(storedStep(ui, "pod"), {
    id: "pod", service: "devpod", repository: "claims-build", source_branch: "main", work_branch: "agent/pay-{run}", protected: ["tests/**", "verify.sh"],
    rules: [{ tool: "run_command", matches: "secret", message: "Blocked by R1" }, { tool: "write_file", matches: "rm -rf", message: "Blocked by R2" }],
    position: { x: 0, y: 40 }, name: "claims",
  });
  // Removing every rule and path stores neither.
  for (const n of [2, 1]) { await find(ui.byId("inspector"), (b) => b.title === `Remove rule ${n}`).onclick(); await settle(); }
  await change(ui, "protected", "");
  await saveNow(ui);
  assert.equal("rules" in storedStep(ui, "pod") || "protected" in storedStep(ui, "pod"), false);
});

test("a chosen repository brings its default branch, and a new dev pod starts with the default work branch", async () => {
  const ui = page();
  await ui.open("/demo");
  await ui.run("Add dev pod");
  assert.ok(ui.tiles().includes("dev-pod-1"), "named Dev Pod 1");
  assert.equal(panelField(ui, "repository").children.find((o) => o.selected).textContent, "claims-build", "the first repository, as a new agent step gets the first agent");
  assert.equal(panelField(ui, "source_branch").value, "main");
  assert.equal(panelField(ui, "work_branch").value, "agent/{run}");
  await change(ui, "source_branch", "");
  await change(ui, "repository", "site");
  assert.equal(panelField(ui, "source_branch").value, "trunk", "the repository's default branch");
  await change(ui, "work_branch", "trunk");
  assert.deepEqual(panelProblems(ui), ["The work branch must differ from the source branch"]);
  await change(ui, "work_branch", "");
  await change(ui, "repository", "");
  assert.deepEqual(panelProblems(ui), ["A dev pod needs a repository", "A dev pod needs a work branch"]);
});

test("the node panel of a command has Command, Timeout and Allow failure; a stop step nothing but its ID", async () => {
  const ui = page();
  ship(ui);
  await ui.open("/ship/verify");
  assert.deepEqual(panelLabels(ui), ["Node ID", "Service", "Timeout", "Allow failure", "Command"]);
  assert.equal(panelField(ui, "command").value, "./verify.sh");
  assert.equal(panelField(ui, "timeout").value, "5m");
  assert.equal(panelField(ui, "allow_failure").checked, false);
  for (const gone of ["agent", "attempts", "repository", "question"]) assert.equal(panelField(ui, gone), undefined, gone);
  const box = panelField(ui, "allow_failure");
  box.checked = true;
  await fire(find(ui.byId("inspector"), (n) => n.className === "step-form"), "change", { target: box });
  await change(ui, "command", "python3 -m unittest\n  discover");
  await saveNow(ui);
  assert.deepEqual(storedStep(ui, "verify"), { id: "verify", service: "command", depends_on: ["build"], command: "python3 -m unittest discover", timeout: "5m", position: { x: 0, y: 312 }, allow_failure: true });
  await change(ui, "command", "");
  assert.deepEqual(panelProblems(ui), ["A command step needs a command"]);
  await ui.open("/ship/stop");
  assert.deepEqual(panelLabels(ui), ["Node ID", "Service"]);
  assert.deepEqual(panelProblems(ui), []);
});

test("problems of the dev pod steps show in the node panel: a second dev pod, a step that does not come after it", async () => {
  const ui = page();
  ship(ui);
  await ui.open("/ship/pod");
  await ui.run("Add dev pod");
  assert.deepEqual(panelProblems(ui), ["A workflow has only one dev pod step, and Pod is another"]);
  await ui.run("Add command");
  assert.ok(ui.tiles().includes("command-1"));
  assert.deepEqual(panelProblems(ui), ["A command step needs a command", "A command step must come after the dev pod step"]);
  await ui.run("Add stop dev pod");
  assert.ok(ui.tiles().includes("stop-dev-pod-1"));
  assert.deepEqual(panelProblems(ui), ["A stop step must come after the dev pod step"]);
  assert.equal(ui.save().disabled, false, "the orchestrator stays the authority");
});

test("a step that changes its kind to or from the dev pod kinds drops the fields of the other kind", async () => {
  const ui = page();
  ship(ui);
  await ui.open("/ship/pod");
  await change(ui, "service", "agent");
  await ui.open("/ship/build");
  await change(ui, "timeout", "5m");
  await change(ui, "service", "command");
  assert.equal(panelField(ui, "timeout").value, "5m", "the field both kinds have stays");
  await ui.open("/ship/verify");
  await change(ui, "service", "devpod-stop");
  await saveNow(ui);
  assert.deepEqual(storedStep(ui, "pod"), { id: "pod", service: "agent", position: { x: 0, y: 40 } });
  assert.deepEqual(storedStep(ui, "build"), { id: "build", service: "command", depends_on: ["pod"], position: { x: 0, y: 176 }, timeout: "5m" });
  assert.deepEqual(storedStep(ui, "verify"), { id: "verify", service: "devpod-stop", depends_on: ["build"], position: { x: 0, y: 312 } });
});

test("dragging a gate's Rejected port onto an earlier step sets where its rejections go; a refused drop says why", async () => {
  const ui = page();
  ship(ui);
  await ui.open("/ship/review");
  const tile = (id) => find(ui.byId("canvas"), (n) => n.attrs["data-step"] === id);
  const port = () => find(tile("review"), (n) => n.attrs.class === "port port-reject");
  const backEdges = () => all(ui.byId("canvas")).filter((n) => n.attrs.class === "back-edge-group");
  const drop = async (x, y) => { port().onpointerdown({ button: 0, stopPropagation() {} }); await ui.release(x, y); };
  assert.equal(port().attrs.cx, "220", "on the gate's right edge");
  assert.equal(port().attrs.cy, "20", "below the corner: clear of the output port at the bottom");
  assert.match(port().children[0].textContent, /^Rejected: /);
  assert.deepEqual(panelField(ui, "on_reject").children.map((o) => o.textContent), ["Nobody: a rejection ends the run", "Build"]);
  assert.equal(panelField(ui, "max_rejections"), undefined, "only with a target");

  // The dev pod cannot run again, and Stop comes after the gate.
  await drop(110, 72);
  assert.deepEqual(ui.notices.at(-1), { text: "A rejection cannot go back to Pod, a dev pod step", kind: "error" });
  await drop(110, 616);
  assert.deepEqual(ui.notices.at(-1), { text: "A rejection cannot go back to Stop, a stop dev pod step", kind: "error" });
  assert.equal(backEdges().length, 0);

  await drop(110, 208);
  assert.equal(backEdges().length, 1);
  const edge = backEdges()[0];
  assert.equal(edge.children[0].textContent, "A rejection at Review goes back to Build with the reason, its previous result and the reviewed documents, and it runs again with the steps after it; a click removes it");
  const line = find(edge, (n) => n.attrs.class === "back-edge");
  assert.equal(line.attrs["marker-end"], "url(#arrow-reject)");
  assert.equal(line.attrs.d, "M220,468 L240,468 Q248,468 248,460 L248,204 Q248,196 240,196 L229,196", "out of the port, up the channel beside the column, into Build's side");
  assert.equal(panelField(ui, "on_reject").children.find((o) => o.selected).textContent, "Build");
  assert.equal(panelField(ui, "max_rejections").placeholder, "5", "the default");
  assert.deepEqual(panelLabels(ui), ["Node ID", "Service", "Rejections go to", "Max rejections", "Question"]);
  await change(ui, "max_rejections", "0");
  assert.deepEqual(panelProblems(ui), ["Max rejections must be a whole number of 1 or more"]);
  await change(ui, "max_rejections", "3");
  await saveNow(ui);
  assert.equal(storedStep(ui, "review").on_reject, "build");
  assert.equal(storedStep(ui, "review").max_rejections, 3);

  // A click removes the back edge, and Cmd/Ctrl+Z puts it back.
  await backEdges()[0].onclick();
  await settle();
  assert.equal(backEdges().length, 0);
  assert.equal(ui.notices.at(-1).text, "Removed");
  ui.context.undo();
  await settle();
  assert.equal(backEdges().length, 1);
  // The panel's select sets it as well; Nobody removes it with its limit.
  await change(ui, "on_reject", "");
  await saveNow(ui);
  assert.equal("on_reject" in storedStep(ui, "review") || "max_rejections" in storedStep(ui, "review"), false);
});

test("a back edge the orchestrator would refuse stays and shows as a problem, and a renamed target keeps it", async () => {
  const ui = page();
  ship(ui);
  ui.server.store.ship[0].steps[3].on_reject = "build";
  await ui.open("/ship/build");
  await change(ui, "id", "Make");
  await ui.open("/ship/review");
  assert.equal(panelField(ui, "on_reject").children.find((o) => o.selected).textContent, "Make");
  // Verify becomes a merge: a rejection cannot go back across it.
  await ui.open("/ship/verify");
  await change(ui, "service", "merge");
  await ui.open("/ship/review");
  assert.deepEqual(panelProblems(ui), ["A rejection cannot go back across the merge step Verify"]);
  assert.equal(panelField(ui, "on_reject").children.find((o) => o.selected).textContent, "Make (not allowed)");
  // A back edge whose target is deleted goes with it.
  await ui.run("Delete step", find(ui.byId("canvas"), (n) => n.attrs["data-step"] === "make"));
  await ui.open("/ship/review");
  assert.deepEqual(panelField(ui, "on_reject").children.map((o) => [o.textContent, !!o.selected]), [["Nobody: a rejection ends the run", false]], "nothing is chosen, so the first option shows");
});

test("Connect to… draws and removes a step's arrows from the keyboard, with the drop's rules", async () => {
  const ui = page();
  await ui.open();
  await ui.run("Add agent");
  const tile = (id) => find(ui.byId("canvas"), (n) => n.attrs["data-step"] === id);
  await ui.run("Connect to…", tile("plan"));
  const dialog = ui.dialog();
  assert.equal(dialog.title, "Connect Plan to");
  const options = all(dialog.body).filter((n) => n.type === "checkbox");
  assert.deepEqual(options.map((b) => [b.dataset.step, b.checked, !!b.disabled]), [["build", true, false], ["step-1", false, false]]);
  // Build stops receiving Plan's output; Step 1 starts.
  options[0].checked = false;
  options[1].checked = true;
  await fire(dialog.footer.find((b) => b.className === "primary"), "click");
  assert.equal(dialog.open, false);
  assert.equal(ui.save().disabled, false, "an edit like any other, kept until Save");
  await ui.save().onclick();
  await settle();
  const stored = ui.posts().at(-1).body.steps;
  assert.deepEqual(stored.find((s) => s.id === "build").depends_on || [], []);
  assert.deepEqual(stored.find((s) => s.id === "step-1").depends_on, ["plan"]);
  // A step that would close a cycle is offered disabled, with the reason beside it.
  await ui.run("Connect to…", tile("step-1"));
  const back = all(ui.dialog().body).filter((n) => n.type === "checkbox").find((b) => b.dataset.step === "plan");
  assert.equal(back.disabled, true);
  assert.ok(all(ui.dialog().body).some((n) => n.textContent === "That dependency would create a cycle"));
});

test("the canvas menu keeps its twelve kinds of step in one Add step submenu", async () => {
  const ui = page();
  await ui.open();
  ui.menus.length = 0;
  await fire(ui.byId("canvas"), "contextmenu");
  const top = ui.menus.at(-1).filter(Array.isArray);
  const labels = top.map((entry) => entry[0]);
  assert.ok(labels.includes("Add step"));
  assert.ok(!labels.some((label) => label.startsWith("Add ") && label !== "Add step"), "no kind of step at the top level");
  const kinds = top.find((entry) => entry[0] === "Add step")[5].map((entry) => entry[0]);
  assert.deepEqual([...kinds], ["Add agent", "Add human gate", "Add interactive session", "Add workflow", "Add team", "Add router", "Add service", "Add merge", "Add batch", "Add dev pod", "Add command", "Add stop dev pod"]);
  assert.ok(top.length <= 9, `a short menu: ${top.length} items`);
  // The menu opens a submenu in its place, its label first to go back, by click and by the arrow keys.
  const app = fs.readFileSync(new URL("web/app.js", import.meta.url), "utf8");
  assert.match(app, /if \(sub\) \{ showMenuItems\(item\[5\], items, item\[0\]\); return; \}/);
  assert.match(app, /if \(ev\.key === "ArrowRight" && document\.activeElement\?\.dataset\?\.submenu\)/);
  assert.match(app, /if \(ev\.key === "ArrowLeft"\) \{ const back = menu\.querySelector\("\[data-back\]"\);/);
});
