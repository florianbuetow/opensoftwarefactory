import assert from "node:assert/strict";
import fs from "node:fs";
import test from "node:test";
import vm from "node:vm";

// The Connectors page and the dev pod view: the adapters that read
// the connector service's answers, and the page running against a small stand-in for the DOM
// and for the helpers of app.js.

const read = (name) => fs.readFileSync(new URL(`web/${name}`, import.meta.url), "utf8");
const plain = (value) => JSON.parse(JSON.stringify(value)); // across the vm realm
const source = read("connectors.js"), app = read("app.js"), html = read("index.html"), style = read("style.css");
const settle = async () => { for (let i = 0; i < 8; i++) await new Promise((done) => setTimeout(done, 0)); };

function logic() {
  const context = { URL, URLSearchParams };
  vm.runInNewContext(`${source.slice(0, source.indexOf("// ---- Connectors page ----"))}\nglobalThis.out = ConnectorsLogic;`, context);
  return context.out;
}

// The answers of the connector service, as its contract gives them.
const REPOS = [
  { name: "site", kind: "github", url: "https://github.com/acme/site.git", default_branch: "trunk", setup_command: "npm ci", idle_minutes: 45, approve_publish: true, credential: "read-write" },
  { name: "claims-build", kind: "plain", url: "http://git-host:8080/claims-build.git", default_branch: "main", setup_command: "", idle_minutes: 30, approve_publish: false, credential: "none" },
];
const PODS = [
  { id: "pod-1", repository: "claims-build", name: "", source_branch: "main", work_branch: "agent/pay-1a2b3c4d", runs: ["trace-1"], last_activity: "2026-11-17T10:00:00Z", expires: "2026-11-17T10:30:00Z", phase: "Running" },
  { id: "pod-2", repository: "site", name: "nightly", source_branch: "trunk", work_branch: "agent/nightly", runs: [], last_activity: "2026-11-17T10:00:00Z", expires: "2026-11-17T10:30:00Z", phase: "Pending" },
];

// ---- Adapters ----

test("the repositories are read in one place, sorted by name, without any token", () => {
  const { repositories, accessWord, kindWord } = logic();
  const list = repositories(REPOS);
  assert.deepEqual(plain(list), [
    { name: "claims-build", kind: "plain", url: "http://git-host:8080/claims-build.git", defaultBranch: "main", setupCommand: "", idleMinutes: 30, approvePublish: false, credential: "none" },
    { name: "site", kind: "github", url: "https://github.com/acme/site.git", defaultBranch: "trunk", setupCommand: "npm ci", idleMinutes: 45, approvePublish: true, credential: "read-write" },
  ]);
  assert.deepEqual(plain(repositories({ repositories: REPOS })), plain(list), "wrapped in an object as well");
  assert.deepEqual(plain(repositories([{ name: "bare", url: "u", token: "secret" }])), [{ name: "bare", kind: "plain", url: "u", defaultBranch: "main", setupCommand: "", idleMinutes: 30, approvePublish: false, credential: "none" }], "defaults, and a token is never taken over");
  assert.deepEqual(plain(repositories(null)), []);
  assert.deepEqual(plain(repositories([null, {}, "x"])), []);
  assert.deepEqual([accessWord("none"), accessWord("read"), accessWord("read-write"), accessWord(undefined)], ["No credential", "Read", "Read and write", "No credential"]);
  assert.deepEqual([kindWord("github"), kindWord("plain")], ["GitHub", "Plain"]);
});

test("the dev pods are read in one place: the runs that hold one, and when it expires", () => {
  const { devPods, timeLeft, phaseState } = logic();
  const pods = devPods(PODS);
  assert.deepEqual(plain(pods[0]), { id: "pod-1", repository: "claims-build", name: "", sourceBranch: "main", workBranch: "agent/pay-1a2b3c4d", runs: ["trace-1"], lastActivity: "2026-11-17T10:00:00Z", expires: "2026-11-17T10:30:00Z", phase: "Running" });
  assert.deepEqual(plain(devPods({ devpods: PODS })), plain(pods));
  assert.deepEqual(plain(devPods([{ id: "p", expires: "0001-01-01T00:00:00Z" }])), [{ id: "p", repository: "", name: "", sourceBranch: "", workBranch: "", runs: [], lastActivity: "", expires: "", phase: "Unknown" }], "Go's zero time is no time");
  assert.deepEqual(plain(devPods(undefined)), []);
  const now = Date.parse("2026-11-17T10:00:30Z");
  assert.equal(timeLeft(pods[0], now), "Held by a run", "kept while a run that uses it is open");
  assert.equal(timeLeft({ ...pods[0], runs: ["a", "b"] }, now), "Held by 2 runs");
  assert.equal(timeLeft(pods[1], now), "29 min");
  assert.equal(timeLeft(pods[1], Date.parse("2026-11-17T10:29:20Z")), "Under 1 min");
  assert.equal(timeLeft(pods[1], Date.parse("2026-11-17T10:31:00Z")), "Expires now");
  assert.equal(timeLeft({ runs: [], expires: "" }, now), "");
  assert.deepEqual(["Running", "Pending", "Creating", "Terminating", "Missing", "Failed"].map(phaseState), ["done", "running", "running", "idle", "failed", "failed"], "the chips of Cluster");
});

test("the pod's files, a file, and git's text are read in one place", () => {
  const { fileList, fileContent, gitOutput, childPath, diffLines } = logic();
  const list = fileList({ path: ".", truncated: false, entries: [{ name: "verify.sh", type: "file", size: 120 }, { name: "src", type: "dir" }, { name: "README.md", type: "file", size: 9 }, { name: "tests", type: "dir" }] });
  assert.deepEqual(plain(list.entries), [{ name: "src", dir: true, size: 0 }, { name: "tests", dir: true, size: 0 }, { name: "README.md", dir: false, size: 9 }, { name: "verify.sh", dir: false, size: 120 }], "folders first, each by name");
  assert.deepEqual(plain(fileList(null)), { path: "", entries: [], truncated: false });
  assert.deepEqual(plain(fileContent({ path: "src/panel.py", content: "print(1)\n", start_line: 1, end_line: 1, total_lines: 1 })), { path: "src/panel.py", content: "print(1)\n", note: "" });
  assert.equal(fileContent({ content: "x", note: "The content was cut after 20000 of 30000 characters." }).note, "The content was cut after 20000 of 30000 characters.");
  assert.equal(gitOutput({ output: "commit abc\n" }), "commit abc\n");
  assert.equal(gitOutput({}), "");
  assert.equal(childPath("", "src"), "src");
  assert.equal(childPath(".", "src"), "src");
  assert.equal(childPath("src/panel", "view.py"), "src/panel/view.py");
  assert.deepEqual(plain(diffLines("diff --git a/x b/x\n--- a/x\n+++ b/x\n@@ -1 +1 @@\n-old\n+new\n same\n")).map((line) => line.kind), ["head", "head", "head", "hunk", "del", "add", ""]);
});

test("a saved repository sends the contract's fields, and the token only when one was typed", () => {
  const { repositoryBody } = logic();
  const form = { name: "site", kind: "github", url: " https://github.com/acme/site.git ", defaultBranch: "", setupCommand: " npm ci ", idleMinutes: "", approvePublish: true, credential: "read-write", token: "" };
  assert.deepEqual(plain(repositoryBody(form)), { kind: "github", url: "https://github.com/acme/site.git", default_branch: "main", setup_command: "npm ci", idle_minutes: 30, approve_publish: true, credential: "read-write" },
    "an empty token field leaves the stored token as it is");
  assert.equal(repositoryBody({ ...form, token: " ghp_x " }).token, "ghp_x");
  assert.equal("token" in repositoryBody({ ...form, credential: "none", token: "ghp_x" }), false, "no credential, no token");
  assert.equal(repositoryBody({ ...form, idleMinutes: "45" }).idle_minutes, 45);
  assert.equal(repositoryBody({ ...form, kind: "other" }).kind, "plain");
});

test("the repository form names what is missing, by field", () => {
  const { formProblems } = logic();
  const form = { name: "site", kind: "plain", url: "http://git-host:8080/site.git", idleMinutes: "", credential: "none", token: "" };
  assert.deepEqual(plain(formProblems(form, null, [])), {});
  assert.deepEqual(plain(formProblems({ ...form, name: " ", url: "" }, null, [])), { name: "Enter a name.", url: "Enter a URL." });
  assert.deepEqual(plain(formProblems({ ...form, name: "My Repo" }, null, [])), { name: "Use lower-case letters, digits, dashes and dots." });
  assert.deepEqual(plain(formProblems(form, null, ["site"])), { name: "site already exists." });
  assert.deepEqual(plain(formProblems({ ...form, idleMinutes: "0" }, null, [])), { idleMinutes: "Enter whole minutes, 1 or more." });
  // A credential needs a token once: a new repository, or one that had none.
  assert.deepEqual(plain(formProblems({ ...form, credential: "read" }, null, [])), { token: "Enter a token." });
  assert.deepEqual(plain(formProblems({ ...form, credential: "read" }, { name: "site", credential: "none" }, ["site"])), { token: "Enter a token." });
  assert.deepEqual(plain(formProblems({ ...form, credential: "read-write" }, { name: "site", credential: "read" }, ["site"])), {}, "the stored token stays");
});

test("the dev pod view travels in the hash: a file, the commit log, or the diff", () => {
  const { podHash, parsePodHash } = logic();
  assert.equal(podHash("pod-1"), "#devpods/pod-1");
  assert.equal(podHash("pod-1", { view: "log" }), "#devpods/pod-1?view=log");
  assert.equal(podHash("pod-1", { path: "src/a b.py" }), "#devpods/pod-1?path=src%2Fa%20b.py");
  assert.deepEqual(plain(parsePodHash("/pod-1")), { id: "pod-1", path: "", view: "diff" }, "the diff is shown first");
  assert.deepEqual(plain(parsePodHash("/pod-1?view=log")), { id: "pod-1", path: "", view: "log" });
  assert.deepEqual(plain(parsePodHash("/pod-1?path=src%2Fa%20b.py")), { id: "pod-1", path: "src/a b.py", view: "file" });
  assert.deepEqual(plain(parsePodHash("")), { id: "", path: "", view: "diff" });
});

// ---- The pages ----

class FakeNode {
  constructor(tag) {
    this.tag = tag; this.children = []; this.attrs = {}; this.listeners = {}; this.style = {}; this.dataset = {};
    this.hidden = false; this.value = ""; this.text = ""; this.className = "";
    this.classList = { add: (...names) => { this.className = [this.className, ...names].filter(Boolean).join(" "); } };
  }
  adopt(items) { return items.flat().filter((c) => c !== null && c !== undefined && c !== false && c !== "").map((c) => { if (c instanceof FakeNode) c.parentNode = this; return c; }); }
  append(...items) { this.children.push(...this.adopt(items)); }
  replaceChildren(...items) { this.children = this.adopt(items); }
  setAttribute(name, value) { this.attrs[name] = String(value); }
  addEventListener(type, fn) { (this.listeners[type] ||= []).push(fn); }
  querySelector(selector) { const tags = selector.split(",").map((s) => s.trim()); return all(this).slice(1).find((n) => tags.includes(n.tag)) || null; }
  closest(tag) { for (let n = this; n; n = n.parentNode) if (n.tag === tag) return n; return null; }
  focus() { this.focused = true; }
  get textContent() { return this.text || this.children.map((c) => (c instanceof FakeNode ? c.textContent : String(c))).join(""); }
  set textContent(value) { this.text = String(value); this.children = []; }
}
const all = (node) => (node instanceof FakeNode ? [node, ...node.children.flatMap(all)] : []);
const find = (root, check) => all(root).find(check);
const fire = async (node, type, ev = {}) => {
  const event = { target: node, preventDefault() {}, stopPropagation() {}, ...ev };
  if (typeof node[`on${type}`] === "function") node[`on${type}`](event);
  for (const fn of node.listeners[type] || []) fn(event);
  await settle();
};

function pages({ repos = REPOS, pods = PODS, files = {}, fail = {} } = {}) {
  const ids = new Map();
  const byId = (id) => { if (!ids.has(id)) ids.set(id, new FakeNode("div")); return ids.get(id); };
  const calls = [], notices = [], questions = [], menus = [], dialogs = [];
  const answers = { confirm: true };
  const server = { repos: structuredClone(repos), pods: structuredClone(pods) };
  const h = (tag, props = {}, ...children) => {
    const node = new FakeNode(tag);
    for (const [key, value] of Object.entries(props || {})) {
      if (value === undefined || value === null) continue;
      if (key === "dataset") Object.assign(node.dataset, value);
      else if (key.includes("-")) node.setAttribute(key, value);
      else node[key] = value;
    }
    node.append(...children);
    // A select shows its selected option, else its first one, as in a browser.
    if (tag === "select") node.value = (node.children.find((o) => o.selected) || node.children[0])?.value ?? "";
    return node;
  };
  const refuse = (status, message) => Object.assign(new Error(message), { status });
  async function api(method, path, body) {
    calls.push({ method, path, body: body === undefined ? undefined : plain(body) });
    if (fail[path]) throw refuse(...fail[path]);
    const [, rest] = /^\/api\/connectors(.*)$/.exec(path);
    if (method === "GET" && rest === "/git/repositories") return server.repos;
    if (method === "GET" && rest === "/devpods") return server.pods;
    const repo = /^\/git\/repositories\/([^/]+)$/.exec(rest);
    if (repo && method === "PUT") {
      const { token, ...kept } = body; // never returned
      server.repos = [...server.repos.filter((r) => r.name !== repo[1]), { name: repo[1], ...kept }];
      return { name: repo[1], ...kept };
    }
    if (repo && method === "DELETE") { server.repos = server.repos.filter((r) => r.name !== repo[1]); return {}; }
    const pod = /^\/devpods\/([^/?]+)(?:\/(files|file|log|diff))?(?:\?path=(.*))?$/.exec(rest);
    if (pod && !server.pods.some((p) => p.id === pod[1])) throw refuse(404, "this dev pod does not exist");
    if (pod && method === "DELETE") { server.pods = server.pods.filter((p) => p.id !== pod[1]); return {}; }
    if (pod && pod[2] === "log") return { output: "commit 0123456789abcdef (HEAD -> agent/pay-1a2b3c4d)\n\n    Show the payment date\n" };
    if (pod && pod[2] === "diff") return { output: files["@diff"] ?? "diff --git a/src/panel.py b/src/panel.py\n@@ -1 +1 @@\n-old\n+new\n" };
    if (pod && pod[2] === "files") {
      const dir = decodeURIComponent(pod[3] || ".");
      return { path: dir, truncated: false, entries: dir === "." ? [{ name: "AGENTS.md", type: "file", size: 365 }, { name: "verify.sh", type: "file", size: 120 }, { name: "src", type: "dir" }] : [{ name: "panel.py", type: "file", size: 9 }] };
    }
    if (pod && pod[2] === "file") return { path: decodeURIComponent(pod[3]), content: "print(1)\n", start_line: 1, end_line: 1, total_lines: 1 };
    throw refuse(404, `${method} ${path}`);
  }
  const trees = [];
  const createTree = (container, { onSelect }) => {
    const tree = { container, roots: [], selected: null, loaded: new Map(), onSelect,
      setRoots(nodes) { tree.roots = nodes; },
      async select(id) { tree.selected = id; },
      async reveal(list) { for (const id of list) { const node = tree.node(id); if (node?.load) tree.loaded.set(id, await node.load()); } },
      invalidate() { tree.loaded.clear(); },
      node(id) { const deep = (n) => [n, ...(n.children || []).flatMap(deep)]; return [...tree.roots.flatMap(deep), ...[...tree.loaded.values()].flat()].find((n) => n.id === id); },
    };
    trees.push(tree);
    return tree;
  };
  const context = {
    console, setTimeout, clearTimeout, structuredClone, URL, URLSearchParams, h, $: byId, api, createTree,
    setInterval: () => 1, clearInterval() {},
    document: { hidden: false },
    location: { hash: "#connectors", replace(hash) { context.location.hash = hash; } },
    tabs: {}, menu: { hidden: true },
    openMenu: (_ev, items) => menus.push(items),
    openDialog: (config) => { const dialog = { ...config, closed: false, close() { dialog.closed = true; } }; dialogs.push(dialog); return dialog; },
    confirmAction: async (question, action) => { questions.push(`${question} [${action}]`); return answers.confirm; },
    setStatus: (text, kind = "") => notices.push({ text, kind }),
    setSaveState: (text = "Saved") => notices.push({ text, kind: "" }),
    iconButton: (name, label, onclick) => h("button", { title: label, onclick }),
    emptyPanel: (title) => h("div", { className: "empty-panel" }, title),
    errorPanel: (message) => h("div", { className: "error-panel" }, message),
    errorState: (message) => h("div", { className: "error-state" }, message),
    statusChip: (state, word) => h("span", { className: `chip chip-${state}` }, word),
    dataTable: (headings, rows) => h("table", {}, h("thead", {}, ...headings.map((t) => h("th", {}, t))), h("tbody", {}, ...rows)),
    clickableRow: (tr, run) => { tr.addEventListener("click", run); return tr; },
    fill: (parent, ...items) => parent.replaceChildren(...items.flat().filter((item) => item !== null && item !== undefined && item !== false)),
    textView: (text) => ({ toggle: h("button", {}, "TXT"), body: h("div", { className: "file-block markdown" }, text) }),
    pretty: (text) => text, fullTime: (iso) => iso, formatSize: (n) => `${n} B`,
    midTruncate: (text, max) => (text.length <= max ? text : `${text.slice(0, max - 1)}…`),
    runIndex: { records: new Map([["trace-1", {}]]), label: () => "Ex06 Implementation · 3 min ago", refresh: async () => {} },
  };
  vm.runInNewContext(source, context);
  const main = byId("connector-main"), detail = byId("devpod-detail");
  const ui = {
    context, calls, notices, questions, menus, dialogs, answers, server, main, detail, tree: trees[0],
    async open() { context.tabs.connectors.show(""); await settle(); },
    async openPod(rest) { context.tabs.devpods.show(rest); await settle(); },
    tables: () => all(main).filter((n) => n.tag === "table"),
    rows: (table) => all(ui.tables()[table]).filter((n) => n.tag === "tr").map((tr) => tr.children.map((td) => td.textContent)),
    row: (table, text) => all(ui.tables()[table]).find((n) => n.tag === "tr" && n.textContent.includes(text)),
    writes: () => calls.filter((c) => c.method !== "GET"),
    // field finds a control of the open repository form by its label.
    field(label) {
      const box = all(dialogs.at(-1).body).find((n) => n.className.split(" ").includes("field") && n.children[0]?.textContent === label);
      assert.ok(box, `field ${label}`);
      return box.querySelector("input, select");
    },
    problems: () => all(dialogs.at(-1).body).filter((n) => n.className.includes("field-problem") && !n.hidden).map((n) => n.textContent),
    async save() { await dialogs.at(-1).footer[1].onclick(); await settle(); },
  };
  return ui;
}

test("Git and MCP share the Connectors menu, with the dev pod view attached to Git", () => {
  const menu = app.slice(app.indexOf("const PAGES = ["), app.indexOf("];", app.indexOf("const PAGES = [")));
  const names = [...menu.matchAll(/name: "([^"]+)"/g)].map((m) => m[1]);
  assert.ok(names.includes("connectors"));
  const groups = app.slice(app.indexOf("const NAV_GROUPS = ["), app.indexOf("];", app.indexOf("const NAV_GROUPS = [")));
  assert.match(groups, /name: "Connectors"[^\n]*pages: \["connectors", "mcp-gateway", "tools"\]/);
  assert.match(menu, /\{ name: "connectors", title: "Connectors", icon: "connector", tip: "[^"]+" \}/);
  assert.match(app, /connector: '<path /, "its icon, drawn like the others");
  assert.match(menu, /\{ name: "devpods", title: "Dev Pods", icon: "cluster", tip: "[^"]+" \}/, "the dev pods have a menu item of their own");
  assert.match(groups, /name: "Factory"[^\n]*pages: \[[^\]]*"runs", "devpods"/, "under Runs in the Factory menu");
  assert.doesNotMatch(app, /devpods: \{ name: "devpods"[^}]*menu:/, "no longer a sub-page of Connectors");
  assert.match(html, /<section class="page page-cluster page-connectors" id="tab-connectors" hidden>/);
  assert.match(html, /<section class="page page-cluster page-devpods" id="tab-devpods" hidden>/);
  assert.ok(html.indexOf('src="app.js"') < html.indexOf('src="connectors.js"') && html.indexOf('src="connectors.js"') < html.indexOf('src="workflows.js"'), "loaded before the pages that use its logic");
  for (const page of ["connectors", "devpods"]) assert.match(app, new RegExp(`\\n  ${page}: \\[\\{ column: "\\.list-panel"`), `${page} has a resizable sidebar`);
  assert.match(style, /\.connector-block \{/);
});

test("the page lists the Git repositories and the live dev pods", async () => {
  const ui = pages();
  await ui.open();
  assert.equal(ui.context.$("connector-kinds").textContent, "Git", "the one connector kind");
  assert.deepEqual(ui.rows(0), [
    ["claims-build", "Plain", "/git/browse/claims-build.git", "main", "No credential"],
    ["site", "GitHub", "https://github.com/acme/site", "trunk", "Read and write"],
  ]);
  const [held, idle] = ui.rows(1);
  // A pod is called by its name, in a column of its own; its repository is never part of it.
  assert.deepEqual(held.slice(0, 6), ["Running", "Unnamed", "claims-build", "agent/pay-1a2b3c4d", "Ex06 Implementation · 3 min ago", "Held by a run"]);
  assert.deepEqual(idle.slice(0, 5), ["Pending", "nightly", "site", "agent/nightly", "No runs"]);
  assert.equal(held[6], "View filesDestroy");
  const run = find(ui.row(1, "pod-1") || ui.row(1, "agent/pay"), (n) => n.tag === "a" && n.href.startsWith("#runs/"));
  assert.equal(run.href, "#runs/trace-1", "a run links to Runs");
  assert.equal(find(ui.row(1, "agent/pay"), (n) => n.tag === "a" && n.textContent === "View files").href, "#devpods/pod-1");
  assert.doesNotMatch(JSON.stringify(ui.rows(0)), /token/);
});

test("repository links open in the browser without opening the settings row", async () => {
  const ui = pages();
  await ui.open();
  const row = ui.row(0, "claims-build");
  const local = find(row, (node) => node.tag === "a");
  assert.equal(local.href, "/git/browse/claims-build.git");
  assert.equal(local.target, "_blank");
  assert.equal(local.rel, "noopener");
  let stops = 0;
  const stopPropagation = () => { stops++; };
  await fire(local, "click", { stopPropagation });
  await fire(local, "keydown", { key: "Enter", stopPropagation });
  assert.equal(stops, 2);
  assert.equal(ui.dialogs.length, 0);
  await fire(row, "click");
  assert.equal(ui.field("URL").value, "http://git-host:8080/claims-build.git");

  const { repositoryLink } = logic();
  assert.equal(repositoryLink("git@github.com:acme/site.git"), "https://github.com/acme/site");
  assert.equal(repositoryLink("ssh://git@github.com/acme/site.git"), "https://github.com/acme/site");
  assert.equal(repositoryLink("https://git.example.com/group/repo.git"), "https://git.example.com/group/repo.git");
  assert.equal(repositoryLink("https://user:secret@git.example.com/group/repo.git"), "https://git.example.com/group/repo.git");
  assert.equal(repositoryLink("javascript:alert(1)"), "");
});

test("empty lists and a service that does not answer are said in the page's words", async () => {
  const empty = pages({ repos: [], pods: [] });
  await empty.open();
  assert.deepEqual(all(empty.main).filter((n) => n.className === "empty-panel").map((n) => n.textContent), ["No repositories yet", "No dev pods"]);
  const down = pages({ fail: { "/api/connectors/git/repositories": [502, "connection refused"], "/api/connectors/devpods": [502, "connection refused"] } });
  await down.open();
  assert.deepEqual(all(down.main).filter((n) => n.className === "error-panel").map((n) => n.textContent),
    ["Cannot list the repositories: connection refused", "Cannot list the dev pods: connection refused"]);
});

test("a repository is added through the form; the token is sent once and never shown again", async () => {
  const ui = pages();
  await ui.open();
  await ui.context.$("connector-new-slot").children[0].onclick();
  assert.equal(ui.dialogs.at(-1).title, "New repository");
  assert.deepEqual(all(ui.dialogs.at(-1).body).filter((n) => n.tag === "label").map((n) => n.textContent),
    ["Name", "Kind", "URL", "Default branch", "Setup command", "Idle minutes", "Ask a person before publishing", "Access", "Token"]);
  assert.equal(ui.field("Token").type, "password");
  assert.equal(ui.field("Token").disabled, true, "no credential, no token");
  assert.equal(ui.field("Idle minutes").placeholder, "30", "the default");
  // Nothing is sent while a field is missing.
  await ui.save();
  assert.deepEqual(ui.problems(), ["Enter a name.", "Enter a URL."]);
  assert.equal(ui.writes().length, 0);
  ui.field("Name").value = "payments";
  ui.field("URL").value = "https://github.com/acme/payments.git";
  ui.field("Kind").value = "github";
  await fire(ui.field("Kind"), "change");
  const approve = find(ui.dialogs.at(-1).body, (n) => n.type === "checkbox");
  assert.equal(approve.checked, true, "a GitHub repository asks before publishing unless the box was changed by hand");
  ui.field("Access").value = "read-write";
  await fire(ui.field("Access"), "change");
  assert.equal(ui.field("Token").disabled, false);
  await ui.save();
  assert.deepEqual(ui.problems(), ["Enter a token."]);
  ui.field("Token").value = "ghp_secret";
  await ui.save();
  assert.deepEqual(ui.writes(), [{ method: "PUT", path: "/api/connectors/git/repositories/payments", body: {
    kind: "github", url: "https://github.com/acme/payments.git", default_branch: "main", setup_command: "", idle_minutes: 30, approve_publish: true, credential: "read-write", token: "ghp_secret" } }]);
  assert.equal(ui.dialogs.at(-1).closed, true);
  assert.equal(ui.notices.at(-1).text, "Saved");
  assert.deepEqual(ui.rows(0).map((r) => r[0]), ["claims-build", "payments", "site"]);

  // Editing it: the name is fixed, the token field is empty, and an empty field keeps the token.
  await fire(ui.row(0, "payments"), "click");
  assert.equal(ui.dialogs.at(-1).title, "Edit repository");
  assert.equal(ui.field("Name").disabled, true);
  assert.equal(ui.field("Token").value, "");
  assert.equal(ui.field("Token").placeholder, "Unchanged");
  ui.field("Default branch").value = "trunk";
  await ui.save();
  assert.deepEqual(ui.writes().at(-1).body, { kind: "github", url: "https://github.com/acme/payments.git", default_branch: "trunk", setup_command: "", idle_minutes: 30, approve_publish: true, credential: "read-write" });
  assert.doesNotMatch(JSON.stringify(all(ui.main).map((n) => n.textContent)), /ghp_secret/);
});

test("a refused save keeps the form open and names the reason", async () => {
  const ui = pages({ fail: { "/api/connectors/git/repositories/claims-build": [400, "url is required"] } });
  await ui.open();
  await fire(ui.row(0, "claims-build"), "click");
  await ui.save();
  assert.deepEqual(ui.notices.at(-1), { text: "Not saved: url is required", kind: "error" });
  assert.equal(ui.dialogs.at(-1).closed, false);
});

test("deleting a repository and destroying a dev pod ask first", async () => {
  const ui = pages();
  await ui.open();
  const item = async (row, label) => { ui.menus.length = 0; await fire(row, "contextmenu"); return ui.menus.at(-1).find((entry) => Array.isArray(entry) && entry[0] === label); };
  assert.deepEqual(plain((await item(ui.row(0, "site"), "Delete") && ui.menus.at(-1)).map((entry) => (Array.isArray(entry) ? entry[0] : entry))), ["Edit…", "-", "Delete", "-", "New repository"]);
  ui.answers.confirm = false;
  await (await item(ui.row(0, "site"), "Delete"))[1]();
  await settle();
  assert.deepEqual(ui.questions, ["Delete site? [Delete]"]);
  assert.equal(ui.writes().length, 0, "nothing is deleted without the confirmation");
  ui.answers.confirm = true;
  await (await item(ui.row(0, "site"), "Delete"))[1]();
  await settle();
  assert.deepEqual(ui.writes().at(-1), { method: "DELETE", path: "/api/connectors/git/repositories/site", body: undefined });
  assert.equal(ui.notices.at(-1).text, "Deleted");
  assert.deepEqual(ui.rows(0).map((r) => r[0]), ["claims-build"]);

  const destroy = find(ui.row(1, "agent/pay"), (n) => n.tag === "button" && n.textContent === "Destroy");
  ui.answers.confirm = false;
  await destroy.onclick();
  await settle();
  assert.equal(ui.questions.at(-1), "Destroy the dev pod on agent/pay-1a2b3c4d? Warning: a run still uses it (Ex06 Implementation · 3 min ago). Work that was not pushed is lost. [Destroy]");
  assert.equal(ui.writes().length, 1);
  ui.answers.confirm = true;
  await find(ui.row(1, "agent/pay"), (n) => n.tag === "button" && n.textContent === "Destroy").onclick();
  await settle();
  assert.deepEqual(ui.writes().at(-1), { method: "DELETE", path: "/api/connectors/devpods/pod-1", body: undefined });
  assert.equal(ui.notices.at(-1).text, "Destroyed");
  assert.deepEqual(ui.rows(1).map((r) => r[1]), ["nightly"]);
  assert.deepEqual(plain((await item(ui.row(1, "agent/nightly"), "Destroy") && ui.menus.at(-1)).map((entry) => (Array.isArray(entry) ? entry[0] : entry))), ["View files", "-", "Destroy"]);
});

test("the dev pods are read again every 10 s while the page is open", () => {
  assert.match(source, /state\.timer = setInterval\(async \(\) => \{ if \(document\.hidden\) return; await loadPods\(\); render\(\); \}, 10000\);/);
  assert.match(source, /hide\(\) \{ clearInterval\(state\.timer\); state\.timer = null; \},/);
});

test("the dev pod view shows the diff first, then the commit log and any file, read-only", async () => {
  const ui = pages();
  await ui.openPod("/pod-1");
  // Three groups say what is what: Git holds the two views of the history (not files), Agent the
  // files that instruct agents, Filesystem every folder and file of the pod.
  // The sidebar lists every live pod; the one on show holds the three groups.
  assert.deepEqual(plain(ui.tree.roots.map((n) => [n.id, n.label, n.kind, !!n.children])), [
    ["pod:pod-1", "agent/pay-1a2b3c4d", "pod", true], ["pod:pod-2", "nightly", "pod", false]], "a named pod by its name, an unnamed one by its work branch");
  assert.equal(ui.tree.roots[1].title, "Opens this dev pod · Repository site", "the repository is told apart from the name");
  const shown = ui.tree.roots[0].children;
  assert.deepEqual(plain(shown.map((n) => [n.id, n.label, n.kind])), [["@git", "Git", "dir"], ["@agent", "Agent", "dir"], ["@files", "Filesystem", "dir"]]);
  const [git, agent, files] = shown.map((n) => n.children);
  assert.deepEqual(plain(git.map((n) => [n.label, n.kind])), [["Diff against main", "view"], ["Commit log", "view"]]);
  assert.ok(git.every((n) => /not a file$/.test(n.title)));
  assert.deepEqual(plain(agent.map((n) => [n.id, n.label, n.path])), [["@agent/AGENTS.md", "AGENTS.md", "AGENTS.md"]]);
  assert.deepEqual(plain(files.map((n) => [n.label, n.kind])), [["src", "dir"], ["AGENTS.md", "file"], ["verify.sh", "file"]]);
  assert.equal(ui.tree.selected, "@diff");
  assert.match(ui.detail.textContent, /^Running·Repository claims-build·Work branch agent\/pay-1a2b3c4dDestroyDiff against main/, "the status line says which is the repository and which the branch");
  const lines = all(ui.detail).filter((n) => n.className.startsWith("diff-line"));
  assert.deepEqual(lines.map((n) => n.className), ["diff-line diff-head", "diff-line diff-hunk", "diff-line diff-del", "diff-line diff-add"]);
  // A click on a row goes through the hash, so Back returns.
  ui.tree.onSelect(git[1]);
  assert.equal(ui.context.location.hash, "#devpods/pod-1?view=log");
  await ui.openPod("/pod-1?view=log");
  assert.match(ui.detail.textContent, /Commit logcommit 0123456789abcdef/);
  ui.tree.onSelect(files[0]);
  ui.tree.onSelect(shown[0]);
  ui.tree.onSelect(ui.tree.roots[0]);
  assert.equal(ui.context.location.hash, "#devpods/pod-1?view=log", "a folder, a group or the pod on show only opens");
  // Another pod's row opens that pod.
  ui.tree.onSelect(ui.tree.roots[1]);
  assert.equal(ui.context.location.hash, "#devpods/pod-2");
  // A file picked under Agent opens as a file and stays marked there.
  ui.tree.onSelect(agent[0]);
  assert.equal(ui.context.location.hash, "#devpods/pod-1?path=AGENTS.md");
  await ui.openPod("/pod-1?path=AGENTS.md");
  assert.equal(ui.tree.selected, "@agent/AGENTS.md");
  ui.tree.onSelect(files[1]);
  await ui.openPod("/pod-1?path=AGENTS.md");
  assert.equal(ui.tree.selected, "AGENTS.md", "picked under Filesystem, it is marked there");
  // A file in a folder: the folder is opened in the tree and the file is read.
  await ui.openPod("/pod-1?path=src%2Fpanel.py");
  assert.equal(ui.tree.selected, "src/panel.py");
  assert.deepEqual(plain(ui.tree.loaded.get("src").map((n) => n.id)), ["src/panel.py"]);
  assert.match(ui.detail.textContent, /src\/panel\.pyprint\(1\)/);
  assert.deepEqual(ui.calls.filter((c) => c.method !== "GET"), [], "nothing is ever written");
  assert.ok(ui.calls.some((c) => c.path === "/api/connectors/devpods/pod-1/file?path=src%2Fpanel.py"));
  // An unchanged diff or log says so.
  const clean = pages({ files: { "@diff": "" } });
  await clean.openPod("/pod-1");
  assert.match(clean.detail.textContent, /No changes$/);
});

test("a dev pod that no longer exists is said to be gone, plainly", async () => {
  const ui = pages();
  await ui.openPod("/pod-9");
  assert.equal(ui.detail.textContent, "This dev pod no longer exists");
  assert.deepEqual(plain(ui.tree.roots), []);
  // A pod that goes away while it is looked at.
  const live = pages();
  await live.openPod("/pod-1");
  live.server.pods = [];
  await live.openPod("/pod-1?view=log");
  assert.equal(live.detail.textContent, "This dev pod no longer exists");
  // Without a pod in the hash (the Dev Pods menu item) the page lists the live pods: one row
  // each in the sidebar and in the table, and a row opens its pod.
  const list = pages();
  await list.openPod("");
  assert.equal(list.context.location.hash, "#connectors", "the hash is left alone");
  assert.deepEqual(plain(list.tree.roots.map((n) => [n.id, n.label, n.kind])), [
    ["pod:pod-1", "agent/pay-1a2b3c4d", "pod"], ["pod:pod-2", "nightly", "pod"]]);
  const table = all(list.detail).find((n) => n.tag === "table");
  const rows = all(table).filter((n) => n.tag === "tr").map((tr) => tr.children.map((td) => td.textContent));
  assert.deepEqual(rows[0], ["Running", "Unnamed", "claims-build", "agent/pay-1a2b3c4d", "Ex06 Implementation · 3 min ago", "Held by a run", "View filesDestroy"]);
  assert.deepEqual(rows[1].slice(1, 3), ["nightly", "site"]);
  assert.equal(find(all(table).filter((n) => n.tag === "tr")[0], (n) => n.tag === "a" && n.textContent === "View files").href, "#devpods/pod-1");
  list.tree.onSelect(list.tree.roots[1]);
  assert.equal(list.context.location.hash, "#devpods/pod-2");
  // No pods: the empty state.
  const none = pages();
  none.server.pods = [];
  await none.openPod("");
  assert.equal(none.detail.textContent, "No dev pods");
});

test("a dev pod is shut down from the Dev Pods list and from its own view, with a warning while a run uses it", async () => {
  const { destroyQuestion } = logic();
  assert.equal(destroyQuestion({ workBranch: "agent/x", runs: [] }), "Destroy the dev pod on agent/x? Work that was not pushed is lost.");
  assert.equal(destroyQuestion({ workBranch: "agent/x", runs: ["t1"] }, () => "Build · now"), "Destroy the dev pod on agent/x? Warning: a run still uses it (Build · now). Work that was not pushed is lost.");
  assert.equal(destroyQuestion({ workBranch: "agent/x", runs: ["t1", "t2"] }), "Destroy the dev pod on agent/x? Warning: 2 runs still use it. Work that was not pushed is lost.");
  // From the list: the pod a run holds warns; declined, nothing is deleted.
  const list = pages();
  await list.openPod("");
  const rowsOf = (ui) => all(all(ui.detail).find((n) => n.tag === "table")).filter((n) => n.tag === "tr");
  const destroy = (tr) => find(tr, (n) => n.tag === "button" && n.textContent === "Destroy");
  list.answers.confirm = false;
  destroy(rowsOf(list)[0]).onclick({ stopPropagation() {} });
  await settle();
  assert.match(list.questions.at(-1), /Warning: a run still uses it \(Ex06 Implementation · 3 min ago\)/);
  assert.deepEqual(list.writes(), []);
  // The pod no run uses asks without a warning; confirmed, it is deleted and the list is read again.
  list.answers.confirm = true;
  destroy(rowsOf(list)[1]).onclick({ stopPropagation() {} });
  await settle();
  assert.equal(list.questions.at(-1), "Destroy the dev pod on agent/nightly? Work that was not pushed is lost. [Destroy]");
  assert.deepEqual(list.writes().at(-1), { method: "DELETE", path: "/api/connectors/devpods/pod-2", body: undefined });
  assert.equal(list.notices.at(-1).text, "Destroyed");
  assert.equal(rowsOf(list).length, 1);
  // From the pod's own view: Destroy in its status line, then the list opens.
  const view = pages();
  await view.openPod("/pod-1");
  view.answers.confirm = true;
  find(view.detail, (n) => n.tag === "button" && n.textContent === "Destroy").onclick();
  await settle();
  assert.deepEqual(view.writes().at(-1), { method: "DELETE", path: "/api/connectors/devpods/pod-1", body: undefined });
  assert.equal(view.context.location.hash, "#devpods");
});
