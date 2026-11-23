import assert from "node:assert/strict";
import fs from "node:fs";
import test from "node:test";
import vm from "node:vm";

const read = (name) => fs.readFileSync(new URL(`web/${name}`, import.meta.url), "utf8");
const chat = read("chat.js");
const app = read("app.js");

// ---- Logic without the DOM ----

function logic() {
  const context = {};
  vm.runInNewContext(`${chat}\nglobalThis.out = ChatLogic;`, context);
  return context.out;
}
const plain = (value) => JSON.parse(JSON.stringify(value)); // across the vm realm

test("tab IDs follow the contract and are named after the agent, numbered when taken", () => {
  const { newTabId, validTabId } = logic();
  assert.equal(newTabId("chat-triage@v3"), "chat-triage");
  assert.equal(newTabId("chat-triage@v3", ["chat-triage"]), "chat-triage-2");
  assert.equal(newTabId("chat-triage@v1", ["chat-triage", "chat-triage-2"]), "chat-triage-3");
  assert.equal(newTabId("Team/Review_Bot@v1"), "team-review-bot");
  assert.equal(newTabId(""), "chat");
  assert.equal(newTabId("@@@", ["chat"]), "chat-2");
  const long = newTabId(`${"x".repeat(80)}@v1`, []);
  assert.equal(long.length, 32);
  for (const id of [long, newTabId(`${"x".repeat(80)}@v1`, [long]), newTabId("-a--b-@v1"), newTabId("Ünïcode agent@v2")]) {
    assert.match(id, /^[a-z0-9][a-z0-9-]{0,39}$/);
    assert.ok(validTabId(id));
  }
  for (const id of ["", "Main", "-main", "a_b", "a".repeat(41), "a/b", "a.json"]) assert.equal(validTabId(id), false, id);
});

test("chat tabs are labelled by agent, with a number for a second tab of the same agent", () => {
  const { tabLabels } = logic();
  const labels = tabLabels([{ id: "triage", agent: "triage@v1" }, { id: "plain", agent: "plain@v2" }, { id: "triage-2", agent: "triage@v1" }, { id: "x", agent: "" }]);
  assert.deepEqual(plain([...labels]), [["triage", "triage"], ["plain", "plain"], ["triage-2", "triage 2"], ["x", "x"]]);
});

test("file names follow the contract; only Markdown, text, and JSON are accepted", () => {
  const { validFileName, storedFileName, uploadProblem, fileKind, MAX_UPLOAD_BYTES } = logic();
  for (const name of ["notes.md", "a.txt", "data.json", "input-intake.md", "A1._-x.md", `${"a".repeat(100)}.md`]) assert.ok(validFileName(name), name);
  for (const name of ["", ".md", "-a.md", "_a.md", "a.yaml", "a.MD", "a.md.exe", "a b.md", "a/b.md", "../a.md", "a.png", `${"a".repeat(101)}.md`, "notes"]) {
    assert.equal(validFileName(name), false, name);
  }
  assert.equal(storedFileName("notes.md"), "notes.md");
  assert.equal(storedFileName("My Notes (final).MD"), "My-Notes-final.md");
  assert.equal(storedFileName("  données çà.txt "), "donn-es.txt");
  assert.equal(storedFileName("...hidden.json"), "hidden.json");
  assert.equal(storedFileName(`${"b".repeat(150)}.txt`), `${"b".repeat(100)}.txt`);
  for (const name of ["picture.png", "config.yaml", "archive.tar.gz", "README", ".md", "???.md", "script.md.exe"]) assert.equal(storedFileName(name), "", name);
  for (const name of ["My Notes (final).MD", "x y.json", `${"b".repeat(150)}.txt`]) assert.ok(validFileName(storedFileName(name)), name);

  assert.equal(uploadProblem("notes.md", 10, "text"), "");
  assert.equal(uploadProblem("picture.png", 10), "only .md, .txt, and .json files");
  assert.equal(uploadProblem("notes.md", MAX_UPLOAD_BYTES + 1), "larger than 1 MB");
  assert.equal(uploadProblem("notes.md", MAX_UPLOAD_BYTES, "ok"), "");
  assert.equal(uploadProblem("notes.txt", 4, "a\u0000b"), "not a text file");
  assert.deepEqual(["a.md", "a.json", "a.txt"].map(fileKind), ["markdown", "json", "text"]);
});

test("search finds every match, ignoring case, without overlaps, and steps around", () => {
  const { findMatches, stepMatch } = logic();
  assert.deepEqual(plain(findMatches("Alpha beta ALPHA alphabet", "alpha")), [0, 11, 17]);
  assert.deepEqual(plain(findMatches("aaaa", "aa")), [0, 2]);
  assert.deepEqual(plain(findMatches("nothing here", "xyz")), []);
  assert.deepEqual(plain(findMatches("anything", "")), []);
  assert.deepEqual(plain(findMatches("a.b a*b", ".")), [1], "the search text is literal, not a pattern");
  // A character whose lowercase form is longer must not shift the offsets.
  const text = "İstanbul and istanbul";
  for (const at of findMatches(text, "stanbul")) assert.equal(text.slice(at, at + 7), "stanbul");
  assert.equal(findMatches(text, "stanbul").length, 2);
  assert.equal(stepMatch(0, 3, 1), 1);
  assert.equal(stepMatch(2, 3, 1), 0);
  assert.equal(stepMatch(0, 3, -1), 2);
  assert.equal(stepMatch(-1, 3, 1), 0);
  assert.equal(stepMatch(0, 0, 1), -1);
});

test("a stored chat log maps to the history: system first, tool calls compact with their results", () => {
  const { conversation } = logic();
  const items = plain(conversation([
    { role: "system", content: "You are helpful." },
    { role: "user", content: "List the files" },
    { role: "assistant", content: null, tool_calls: [
      { id: "c1", type: "function", function: { name: "list_session_files", arguments: "{\"session\":\"user-sessions/t/s\"}" } },
      { id: "c2", type: "function", function: { name: "read_session_file", arguments: "not json" } },
    ] },
    { role: "tool", tool_call_id: "c1", content: "{\"files\":[\"a.md\"]}" },
    { role: "tool", tool_call_id: "c2", content: "1: hello" },
    { role: "assistant", content: [{ type: "text", text: "There is " }, { type: "text", text: "one file." }] },
    { role: "user", content: "Thanks" },
    { role: "assistant", content: "  ", tool_calls: [{ id: "c3", function: { name: "write_session_file", arguments: { name: "b.md" } } }] },
    { role: "tool", tool_call_id: "orphan", name: "mystery", content: "late" },
    null, "noise",
  ]));
  assert.deepEqual(items, [
    { kind: "system", text: "You are helpful." },
    { kind: "user", text: "List the files" },
    { kind: "tool", id: "c1", name: "list_session_files", args: "{\n  \"session\": \"user-sessions/t/s\"\n}", result: "{\"files\":[\"a.md\"]}" },
    { kind: "tool", id: "c2", name: "read_session_file", args: "not json", result: "1: hello" },
    { kind: "assistant", text: "There is one file." },
    { kind: "user", text: "Thanks" },
    { kind: "tool", id: "c3", name: "write_session_file", args: "{\n  \"name\": \"b.md\"\n}", result: null },
    { kind: "tool", id: "orphan", name: "mystery", args: "", result: "late" },
  ]);
  assert.deepEqual(plain(conversation(undefined)), []);
  assert.deepEqual(plain(conversation({ messages: [] })), []);
});

test("mermaid code blocks are found the way renderMarkdown reads fences", () => {
  const { mermaidBlocks } = logic();
  const text = "# Plan\n\n```mermaid\ngraph TD\n  A --> B\n```\n\ntext\n\n~~~Mermaid\nsequenceDiagram\n~~~\n\n```js\nconst mermaid = 1;\n```\n\n```\n```mermaid\n```\n";
  assert.deepEqual(plain(mermaidBlocks(text)), ["graph TD\n  A --> B", "sequenceDiagram"]);
  assert.deepEqual(plain(mermaidBlocks("no diagrams, only the word mermaid")), []);
  assert.deepEqual(plain(mermaidBlocks("```mermaid\ngraph LR\n  unterminated")), ["graph LR\n  unterminated"]);

  // The view finds the same blocks in renderMarkdown's output: <pre><code class="language-mermaid">.
  const node = (tag, props = {}, ...children) => ({ tag, ...props, children: children.flat().filter((c) => c !== null && c !== undefined && c !== false), append(...items) { this.children.push(...items); } });
  const context = { document: { createDocumentFragment: () => node("fragment") }, h: node };
  vm.runInNewContext(`${app.slice(app.indexOf("function markdownInline"), app.indexOf("// The fixed-width control"))}\nglobalThis.render = renderMarkdown;`, context);
  const all = (root, tag) => [...(root.tag === tag ? [root] : []), ...(root.children || []).flatMap((child) => (typeof child === "object" ? all(child, tag) : []))];
  const codes = all(context.render(text), "pre").map((pre) => pre.children[0]).filter((code) => code.className.toLowerCase() === "language-mermaid");
  assert.deepEqual(codes.map((code) => code.children.join("")), ["graph TD\n  A --> B", "sequenceDiagram"]);

});

test("in the session views an escaped pipe stays in its table cell and shows as a pipe; code keeps it as written", () => {
  const { maskTablePipes, unmaskPipes, PIPE_MARK } = logic();
  const node = (tag, props = {}, ...children) => ({ tag, ...props, children: children.flat().filter((c) => c !== null && c !== undefined && c !== false), append(...items) { this.children.push(...items); } });
  const context = { document: { createDocumentFragment: () => node("fragment") }, h: node };
  vm.runInNewContext(`${app.slice(app.indexOf("function markdownInline"), app.indexOf("// The fixed-width control"))}\nglobalThis.render = renderMarkdown;`, context);
  const all = (root, tag) => [...(root.tag === tag ? [root] : []), ...(root.children || []).flatMap((child) => (typeof child === "object" ? all(child, tag) : []))];
  const textOf = (n) => (typeof n === "string" ? n : (n.children || []).map(textOf).join(""));
  const source = "| File | Line | Text |\n| --- | --- | --- |\n| spec.md | 10 | \\| CSV \\| High \\| |\n| a.md | 4 | `x \\| y` |\n\nprose \\| stays\n\n```\n| a \\| b |\n| --- | --- |\n| c \\| d | e |\n```\n\n`span \\| kept`";

  // renderRich's path: mask, renderMarkdown, unmask the rendered text.
  const masked = maskTablePipes(source);
  const root = context.render(masked);
  const rows = all(all(root, "table")[0], "tr");
  assert.deepEqual(rows.map((tr) => tr.children.length), [3, 3, 4], "only the escaped pipe of a code span still divides its row");
  assert.equal(unmaskPipes(textOf(rows[1].children[2])), "| CSV | High |");
  assert.equal(all(root, "table").length, 1);
  // Outside table rows, in code spans, and in fenced code nothing changes.
  assert.equal(masked.split(PIPE_MARK).length - 1, 3);
  assert.equal(unmaskPipes(masked).split("\n").slice(3).join("\n"), source.split("\n").slice(3).join("\n"));
  assert.equal(maskTablePipes("no table \\| here"), "no table \\| here");
  assert.ok(chat.includes("renderMarkdown(masked)"), "renderRich renders the masked text");

  // The shared renderMarkdown of the other pages is unchanged: there the escaped pipes divide the row.
  assert.deepEqual(all(all(context.render(source), "table")[0], "tr").map((tr) => tr.children.length), [3, 6, 4]);
});

test("JSON is pretty-printed for viewing and validated before saving", () => {
  const { prettyJSON, jsonProblem } = logic();
  assert.deepEqual(plain(prettyJSON("{\"a\":[1,2],\"b\":{\"c\":null}}")), { ok: true, text: "{\n  \"a\": [\n    1,\n    2\n  ],\n  \"b\": {\n    \"c\": null\n  }\n}", error: "" });
  const broken = prettyJSON("{\"a\": 1,}");
  assert.equal(broken.ok, false);
  assert.equal(broken.text, "{\"a\": 1,}", "text that does not parse is shown as it is");
  assert.ok(broken.error);
  assert.equal(jsonProblem("[1, 2, 3]"), "");
  assert.equal(jsonProblem("\"a string is a JSON document\""), "");
  assert.ok(jsonProblem(""));
  assert.ok(jsonProblem("{a: 1}"));
});

test("a session opens at #sessions/<trace>/<step> and only valid session paths are read from a hash", () => {
  const { sessionHash, sessionFromHash, validSession } = logic();
  assert.equal(sessionHash("user-sessions/abc-1/talk"), "#sessions/abc-1/talk");
  assert.equal(sessionFromHash("/abc-1/talk"), "user-sessions/abc-1/talk");
  assert.equal(sessionFromHash("/abc-1/route/talk?x=1"), "user-sessions/abc-1/route/talk");
  assert.equal(sessionFromHash(sessionHash("user-sessions/t/a/b").slice("#sessions".length)), "user-sessions/t/a/b");
  for (const rest of ["", "/", "/abc", "/abc/../talk", "/abc/.hidden", "/a b/c", "/abc//talk"]) assert.equal(sessionFromHash(rest), "", rest);
  assert.ok(validSession("user-sessions/t/s"));
  for (const s of ["user-sessions/t", "runs/t/s", "user-sessions/t/s/", "/user-sessions/t/s", `user-sessions/t/${"s".repeat(400)}`]) assert.equal(validSession(s), false, s);
});

// ---- The mounted session, on a small stand-in DOM ----

class El {
  constructor(tag) {
    Object.assign(this, {
      tagName: String(tag).toUpperCase(), children: [], attributes: {}, events: {}, dataset: {}, style: {}, parentElement: null,
      className: "", hidden: false, disabled: false, value: "", title: "", id: "", type: "", role: "", tabIndex: 0, rows: 0, src: "", htmlFor: "",
      placeholder: "", autocomplete: "", spellcheck: true, multiple: false, accept: "", checked: false, onclick: null, onchange: null,
      scrollTop: 0, scrollHeight: 0, clientHeight: 0, files: [],
    });
    const el = this;
    this.classList = {
      list: () => el.className.split(/\s+/).filter(Boolean),
      contains: (name) => this.classList.list().includes(name),
      add: (...names) => { el.className = [...new Set([...this.classList.list(), ...names])].join(" "); },
      remove: (...names) => { el.className = this.classList.list().filter((n) => !names.includes(n)).join(" "); },
      toggle: (name, on = !this.classList.contains(name)) => { if (on) this.classList.add(name); else this.classList.remove(name); },
    };
  }
  get isConnected() { return true; }
  get offsetParent() { return this; }
  get childNodes() { return this.children; }
  get textContent() { return this.children.map((c) => (typeof c === "string" ? c : c.textContent)).join(""); }
  set textContent(text) { this.replaceChildren(...(text ? [String(text)] : [])); }
  setAttribute(name, value) { this.attributes[name] = String(value); }
  getAttribute(name) { return this.attributes[name] ?? null; }
  addEventListener(type, fn) { (this.events[type] ||= []).push(fn); }
  removeEventListener() {}
  dispatch(type, event = {}) { for (const fn of this.events[type] || []) fn({ preventDefault() {}, stopPropagation() {}, target: this, ...event }); }
  click() { this.onclick?.({ preventDefault() {}, stopPropagation() {} }); }
  append(...items) {
    for (const item of items.flat()) {
      if (item && typeof item === "object") { item.remove?.(); item.parentElement = this; }
      this.children.push(item);
    }
  }
  replaceChildren(...items) {
    for (const c of this.children) if (c && typeof c === "object") c.parentElement = null;
    this.children = [];
    this.append(...items);
  }
  remove() {
    if (!this.parentElement) return;
    this.parentElement.children = this.parentElement.children.filter((c) => c !== this);
    this.parentElement = null;
  }
  focus() { El.focused = this; }
  normalize() {}
  scrollIntoView() {}
  setSelectionRange() {}
  getBoundingClientRect() { return { left: 0, top: 0, right: 0, bottom: 0, width: 0, height: 0 }; }
  all() { return this.children.flatMap((c) => (c && typeof c === "object" ? [c, ...c.all()] : [])); }
  // Only what the view asks for: ".class", "tag.class", and attribute pairs.
  querySelectorAll(selector) {
    if (/[\s>,]/.test(selector)) return [];
    const tag = /^[a-z]+/.exec(selector)?.[0];
    const classes = [...selector.matchAll(/\.([\w-]+)/g)].map((m) => m[1]);
    const attrs = [...selector.matchAll(/\[([\w-]+)="([^"]*)"\]/g)].map((m) => [m[1], m[2]]);
    return this.all().filter((e) => (!tag || e.tagName === tag.toUpperCase()) && classes.every((c) => e.classList.contains(c))
      && attrs.every(([k, v]) => String(e[k] ?? e.attributes[k]) === v));
  }
  querySelector(selector) { return this.querySelectorAll(selector)[0] || null; }
}

const flush = async () => { for (let i = 0; i < 20; i++) await new Promise((resolve) => setImmediate(resolve)); };
const SESSION = "user-sessions/trace-1/talk";

function setup({ blobs = {}, chatReply, creatorName = "ada" } = {}) {
  const store = new Map(Object.entries(blobs)); // object id -> text
  const calls = [], notices = [], dialogs = [];
  let name = creatorName, confirmAnswer = true;
  const listing = (prefix) => {
    const objects = [...store.keys()].filter((id) => id.startsWith(`${prefix}/`) && !id.slice(prefix.length + 1).includes("/"))
      .sort().map((id) => ({ id, size: store.get(id).length, created: `v${store.get(id).length}` }));
    if (!objects.length && ![...store.keys()].some((id) => id.startsWith(`${prefix}/`))) { const e = new Error("prefix not found"); e.status = 404; throw e; }
    return { prefix, directories: [], objects };
  };
  const context = {
    Node: El, console,
    document: {
      hidden: false, head: new El("head"), createElement: (tag) => new El(tag), getElementById: () => null,
      addEventListener() {}, removeEventListener() {}, createDocumentFragment: () => new El("fragment"),
    },
    location: { hash: "#sessions/trace-1/talk" },
    menu: { hidden: true, isConnected: true },
    setInterval: () => 1, clearInterval() {}, setTimeout: (fn) => { setImmediate(fn); return 1; },
    getComputedStyle: () => ({ lineHeight: "20px" }),
    creator: () => name,
    creatorHeaders: () => (name ? { "X-Creator": name } : {}),
    titleCase: (id) => id,
    isJSONDocument: (text) => { try { JSON.parse(text); return true; } catch { return false; } },
    pretty: (text) => text,
    errorMessage: (text) => text,
    setStatus: (text, kind) => notices.push([kind || "", text]),
    setSaveState: (text = "Saved") => notices.push(["", text]),
    confirmAction: async () => confirmAnswer,
    openMenu: (_ev, items) => { context.lastMenu = items; },
    openDialog: (dialog) => { dialogs.push(dialog); return { close() { dialog.closed = true; } }; },
    api: async (method, path, body) => {
      calls.push({ method, path, body: body === undefined ? undefined : plain(body) });
      if (method === "GET" && path.startsWith("/api/blobs/list/")) return listing(path.slice("/api/blobs/list/".length));
      if (path === "/api/agent/chat") return chatReply(body, store);
      if (path === "/api/orchestrator/sessions/complete") return body;
      throw new Error(`unexpected ${method} ${path}`);
    },
    fetch: async (path, init = {}) => {
      const id = path.slice("/api/blobs/blobs/".length);
      const method = init.method || "GET";
      if (method !== "GET") calls.push({ method, path, headers: init.headers, body: init.body });
      if (method === "PUT") store.set(id, init.body);
      if (method === "DELETE") store.delete(id);
      const found = store.has(id);
      return { ok: method !== "GET" || found, status: method !== "GET" || found ? 200 : 404, statusText: "", text: async () => (method === "GET" && found ? store.get(id) : "not found") };
    },
  };
  context.window = context;
  const dom = app.slice(app.indexOf("function h("), app.indexOf("// createTree"));
  const poolSource = app.slice(app.indexOf("async function pool"), app.indexOf("// runIndex knows"));
  vm.runInNewContext(`"use strict"; ${dom}\n${poolSource}
    const iconButton = (name, label, onclick, extra = {}) => h("button", { type: "button", className: "icon-button", "aria-label": label, title: label, onclick, ...extra }, name);
    const statusChip = (state, word = state) => h("span", { className: "chip chip-" + state }, word);
    const emptyPanel = (title) => h("div", { className: "empty-panel" }, title);
    const errorState = (message) => h("div", { className: "error-state" }, message);
    const errorPanel = errorState;
    const copyButton = (text, what) => h("button", { className: "copy-button", "aria-label": "Copy " + what, copies: text });
    const timeEl = (iso) => h("time", {}, iso);
    const renderMarkdown = (text) => h("div", { className: "markdown-body" }, String(text));
    ${chat}
    globalThis.mountSession = InteractiveSession.mount;`, context);
  const main = new El("div"), files = new El("aside");
  const mount = (options = {}) => context.mountSession(main, {
    session: SESSION, traceId: "trace-1", workflowId: "wf-run-1", workflow: "demo", step: "talk",
    agent: "triage@v1", agents: ["plain@v2", "triage@v1"], artifact: "runs/trace-1/talk", inputs: [], filesContainer: files, ...options,
  });
  const find = (root, selector) => root.querySelectorAll(selector);
  return {
    main, files, mount, calls, notices, dialogs, store, context, find,
    setCreator(value) { name = value; }, setConfirm(value) { confirmAnswer = value; },
    tabs: () => find(main, ".chat-tab-button").map((b) => b.textContent),
    activePane: () => find(main, ".chat-pane").find((p) => !p.hidden),
    posts: () => calls.filter((c) => c.path === "/api/agent/chat"),
  };
}

const logOf = (agent, messages, created = "2026-01-01T00:00:00Z") => JSON.stringify({ session: SESSION, agent, created, messages });

test("a new session opens one chat with the main agent; stored chat logs are restored as tabs", async () => {
  const fresh = setup();
  fresh.mount();
  await flush();
  assert.deepEqual(fresh.tabs(), ["triage"]);
  assert.equal(fresh.find(fresh.files, ".empty-panel")[0].textContent, "No files");

  const restored = setup({ blobs: {
    [`${SESSION}/chats/plain.json`]: logOf("plain@v2", [{ role: "system", content: "s" }, { role: "user", content: "second tab" }], "2026-01-02T00:00:00Z"),
    [`${SESSION}/chats/triage.json`]: logOf("triage@v1", [{ role: "system", content: "s" }, { role: "user", content: "hello" }, { role: "assistant", content: "hi there" }]),
    [`${SESSION}/chats/Not-A-Tab.json`]: logOf("triage@v1", []),
    [`${SESSION}/files/input-intake.md`]: "# Input",
  } });
  restored.mount();
  await flush();
  assert.deepEqual(restored.tabs(), ["triage", "plain"], "oldest log first; an invalid tab ID is not a tab");
  const pane = restored.activePane();
  assert.deepEqual(restored.find(pane, ".chat-message").map((m) => m.classList.list().join(" ")), ["chat-message user", "chat-message assistant"]);
  assert.equal(restored.find(pane, ".chat-detail").length, 1, "the system message is there, folded");
  assert.equal(restored.find(pane, ".chat-detail")[0].tagName, "DETAILS");
  assert.equal(restored.find(pane, ".copy-button")[0].attributes.copies, "hi there", "every answer can be copied");
  assert.deepEqual(restored.find(restored.files, ".row-title").map((r) => r.textContent), ["input-intake.md"]);
});

test("a message is posted once, the view shows the stored log afterwards, and the files are listed again", async () => {
  let release;
  const ui = setup({ chatReply: (body, store) => new Promise((resolve) => {
    release = () => {
      store.set(`${SESSION}/chats/${body.tab}.json`, logOf(body.agent, [{ role: "system", content: "s" }, { role: "user", content: body.message }, { role: "assistant", content: "stored answer" }]));
      store.set(`${SESSION}/files/summary.md`, "# Summary");
      resolve({ reply: "transient answer", chat: "x", messages: 3 });
    };
  }) });
  ui.mount();
  await flush();
  const pane = ui.activePane();
  const input = ui.find(pane, ".chat-input")[0], send = pane.all().find((e) => e.tagName === "BUTTON" && e.textContent === "Send");
  assert.equal(send.disabled, true, "nothing to send yet");
  input.value = "  What is in the files?  ";
  input.dispatch("input");
  assert.equal(send.disabled, false);

  let prevented = false;
  input.dispatch("keydown", { key: "Enter", shiftKey: true, preventDefault() { prevented = true; } });
  assert.equal(ui.posts().length, 0, "Shift+Enter is a new line");
  assert.equal(prevented, false);
  input.dispatch("keydown", { key: "Enter", shiftKey: false, preventDefault() { prevented = true; } });
  assert.equal(prevented, true);
  assert.equal(ui.posts().length, 1);
  assert.deepEqual(ui.posts()[0].body, { workflow_id: "wf-run-1", step_id: "talk", session: SESSION, tab: "triage", agent: "triage@v1", message: "What is in the files?", created_by: "ada" });

  // Busy: the message shows at once, the box is empty, and further sends do nothing.
  assert.equal(input.value, "");
  assert.equal(send.disabled, true);
  assert.equal(ui.find(pane, ".chat-busy").length, 1);
  assert.equal(ui.find(pane, ".chat-bubble")[0].textContent, "What is in the files?");
  input.value = "typed while waiting";
  input.dispatch("input");
  input.dispatch("keydown", { key: "Enter" });
  send.click();
  assert.equal(ui.posts().length, 1, "one message at a time");

  release();
  await flush();
  assert.equal(ui.find(pane, ".chat-busy").length, 0);
  assert.deepEqual(ui.find(pane, ".chat-message").map((m) => m.textContent), ["YouWhat is in the files?", "triagestored answer"], "the stored log, not the transient reply");
  assert.equal(input.value, "typed while waiting", "a draft typed meanwhile is kept");
  assert.equal(send.disabled, false);
  assert.deepEqual(ui.find(ui.files, ".row-title").map((r) => r.textContent), ["summary.md"], "files the agent wrote appear");
});

test("a failed message is reported and put back for another try", async () => {
  let fail = true;
  const ui = setup({ chatReply: async (body, store) => {
    if (fail) { const e = new Error("upstream model failed"); e.status = 502; throw e; }
    store.set(`${SESSION}/chats/${body.tab}.json`, logOf(body.agent, [{ role: "user", content: body.message }, { role: "assistant", content: "ok" }]));
    return { reply: "ok" };
  } });
  ui.mount();
  await flush();
  const pane = ui.activePane();
  const input = ui.find(pane, ".chat-input")[0], send = pane.all().find((e) => e.textContent === "Send");
  input.value = "try me";
  input.dispatch("input");
  send.click();
  await flush();
  const error = ui.find(pane, ".chat-error")[0];
  assert.equal(error.hidden, false);
  assert.equal(error.textContent, "Not sent: upstream model failed");
  assert.equal(input.value, "try me", "the text is kept");
  assert.equal(send.disabled, false);
  assert.equal(ui.find(pane, ".chat-message").length, 0, "nothing was stored, so nothing is shown as sent");

  fail = false;
  send.click();
  await flush();
  assert.equal(error.hidden, true);
  assert.equal(ui.posts().length, 2);
  assert.deepEqual(ui.find(pane, ".chat-message").map((m) => m.textContent), ["Youtry me", "triageok"]);
});

test("a new chat tab is opened for a chosen agent and has its own log", async () => {
  const ui = setup({ chatReply: async () => ({ reply: "hello" }) });
  ui.mount();
  await flush();
  ui.main.all().find((e) => e.attributes["aria-label"] === "New chat").click();
  assert.deepEqual(plain(ui.context.lastMenu.map((item) => item[0])), ["triage@v1", "plain@v2"], "the main agent first, each agent once");
  ui.context.lastMenu[1][1]();
  ui.context.lastMenu[0][1]();
  assert.deepEqual(ui.tabs(), ["triage", "plain", "triage 2"]);
  const pane = ui.activePane();
  const input = ui.find(pane, ".chat-input")[0];
  input.value = "second chat";
  input.dispatch("keydown", { key: "Enter" });
  await flush();
  assert.equal(ui.posts()[0].body.tab, "triage-2");
  assert.equal(ui.posts()[0].body.agent, "triage@v1");
  assert.deepEqual(ui.find(pane, ".chat-message").map((m) => m.textContent), ["Yousecond chat", "triagehello"], "without a stored log the reply itself is shown");
});

test("a completed session is read-only and names the submitted file", async () => {
  const ui = setup({ blobs: {
    [`${SESSION}/completed.json`]: JSON.stringify({ file: "summary.md", completed_by: "grace", completed_at: "2026-01-03T00:00:00Z", artifact: "runs/trace-1/talk" }),
    [`${SESSION}/chats/triage.json`]: logOf("triage@v1", [{ role: "user", content: "hello" }, { role: "assistant", content: "hi" }]),
    [`${SESSION}/files/summary.md`]: "# Summary",
  } });
  ui.mount();
  await flush();
  const hiddenControl = (label) => ui.main.all().concat(ui.files.all()).find((e) => e.attributes["aria-label"] === label).hidden;
  assert.equal(ui.find(ui.main, ".chat-composer")[0].hidden, true, "no composer");
  assert.equal(hiddenControl("New chat"), true);
  assert.equal(hiddenControl("Upload files"), true);
  assert.equal(ui.find(ui.files, ".chat-complete")[0].hidden, true);
  const meta = ui.find(ui.main, ".session-meta")[0];
  assert.match(meta.textContent, /done.*summary\.md.*grace.*2026-01-03T00:00:00Z.*View output.*View run.*View trace/);

  // The file opens for reading; it cannot be edited, and its row offers no Delete.
  ui.find(ui.files, ".rows")[0].children[0].dispatch("click");
  await flush();
  assert.deepEqual(ui.tabs(), ["triage", "summary.md"]);
  assert.equal(ui.activePane().all().find((e) => e.attributes["aria-label"] === "Edit file").hidden, true);
  ui.find(ui.files, ".rows")[0].children[0].dispatch("contextmenu");
  assert.deepEqual(plain(ui.context.lastMenu.map((item) => item[0])), ["Open", "Download"]);
});

test("Complete submits the chosen file once, as the acting person, and locks the session", async () => {
  const ui = setup({ blobs: { [`${SESSION}/files/a.md`]: "# A", [`${SESSION}/files/b.json`]: "{}" } });
  ui.setCreator("");
  ui.mount();
  await flush();
  const complete = ui.find(ui.files, ".chat-complete")[0].children[0];
  assert.equal(complete.disabled, true);
  assert.equal(complete.title, "Needs your name in the top bar");
  complete.click();
  assert.equal(ui.dialogs.length, 0);
  assert.deepEqual(ui.notices.at(-1), ["error", "Not completed: no name in the top bar"]);

  ui.setCreator("ada");
  complete.click();
  assert.equal(ui.dialogs.length, 1);
  const dialog = ui.dialogs[0];
  assert.equal(dialog.title, "Complete session");
  const rows = () => dialog.body.querySelectorAll(".pick-list")[0].children;
  assert.deepEqual(rows().map((li) => [li.textContent, li.attributes["aria-checked"]]), [["a.md", "true"], ["b.json", "false"]]);
  rows()[1].dispatch("click");
  assert.deepEqual(rows().map((li) => li.attributes["aria-checked"]), ["false", "true"]);
  const go = dialog.footer[1];
  const first = go.onclick(), second = go.onclick();
  await Promise.all([first, second]);
  const posts = ui.calls.filter((c) => c.path === "/api/orchestrator/sessions/complete");
  assert.equal(posts.length, 1, "a second click submits nothing twice");
  assert.deepEqual(posts[0].body, { workflow_id: "wf-run-1", step: "talk", file: "b.json", completed_by: "ada" });
  assert.equal(dialog.closed, true);
  assert.equal(ui.find(ui.files, ".chat-complete")[0].hidden, true, "read-only at once");
  assert.equal(ui.find(ui.main, ".chat-composer")[0].hidden, true);

  // The orchestrator's record confirms it.
  ui.store.set(`${SESSION}/completed.json`, JSON.stringify({ file: "b.json", completed_by: "ada", completed_at: "2026-01-03T00:00:00Z" }));
  await flush();
  assert.match(ui.find(ui.main, ".session-meta")[0].textContent, /done.*b\.json.*ada/);
});

test("a JSON file is pretty-printed, edited as text, validated, and written back with the creator", async () => {
  const ui = setup({ blobs: { [`${SESSION}/files/data.json`]: "{\"a\":1}" } });
  ui.mount();
  await flush();
  ui.find(ui.files, ".rows")[0].children[0].dispatch("click");
  await flush();
  const pane = ui.activePane();
  assert.equal(ui.find(pane, ".file-block")[0].textContent, "{\n  \"a\": 1\n}");
  const control = (label) => pane.all().find((e) => e.attributes["aria-label"] === label || e.textContent === label);
  const area = ui.find(pane, ".viewer-editor")[0], problem = ui.find(pane, ".viewer-problem")[0];
  assert.equal(area.hidden, true);
  control("Edit file").click();
  assert.equal(area.hidden, false);
  assert.equal(area.value, "{\n  \"a\": 1\n}");
  assert.equal(control("Save").disabled, true, "nothing changed yet");

  area.value = "{\"a\": 2,}";
  area.dispatch("input");
  assert.deepEqual(ui.tabs(), ["triage", "data.json •"], "unsaved edits are marked on the tab");
  await control("Save").onclick();
  assert.match(problem.textContent, /^Invalid JSON: /);
  assert.equal(ui.calls.filter((c) => c.method === "PUT").length, 0, "invalid JSON is not written");

  // Closing the tab or cancelling asks before the edits are dropped.
  ui.setConfirm(false);
  await control("Cancel").onclick();
  assert.equal(area.hidden, false, "the edits stay when the question is answered with Cancel");
  ui.setConfirm(true);

  area.value = "{\"a\": 2}";
  area.dispatch("input");
  assert.equal(problem.hidden, true);
  await control("Save").onclick();
  const put = ui.calls.find((c) => c.method === "PUT");
  assert.equal(put.path, `/api/blobs/blobs/${SESSION}/files/data.json`);
  assert.equal(put.body, "{\"a\": 2}");
  assert.equal(put.headers["X-Creator"], "ada");
  assert.equal(put.headers["X-Trace-Id"], "trace-1");
  assert.equal(area.hidden, true, "back in the rendered view");
  assert.equal(ui.find(pane, ".file-block")[0].textContent, "{\n  \"a\": 2\n}");
  assert.deepEqual(ui.tabs(), ["triage", "data.json"]);
  assert.deepEqual(ui.notices.at(-1), ["", "Saved"]);
});

test("deleting a file asks first and closes its tab", async () => {
  const ui = setup({ blobs: { [`${SESSION}/files/a.md`]: "# A", [`${SESSION}/files/b.md`]: "# B" } });
  ui.mount();
  await flush();
  const row = () => ui.find(ui.files, ".rows")[0].children[0];
  row().dispatch("click");
  await flush();
  row().dispatch("contextmenu");
  assert.deepEqual(plain(ui.context.lastMenu.map((item) => (Array.isArray(item) ? item[0] : item))), ["Open", "Download", "-", "Delete"]);
  ui.setConfirm(false);
  ui.context.lastMenu[3][1]();
  await flush();
  assert.equal(ui.calls.filter((c) => c.method === "DELETE").length, 0, "nothing is deleted without the confirmation");
  ui.setConfirm(true);
  ui.context.lastMenu[3][1]();
  await flush();
  assert.deepEqual(ui.calls.filter((c) => c.method === "DELETE").map((c) => c.path), [`/api/blobs/blobs/${SESSION}/files/a.md`]);
  assert.deepEqual(ui.find(ui.files, ".row-title").map((r) => r.textContent), ["b.md"]);
  assert.deepEqual(ui.tabs(), ["triage"]);
});

test("uploads take only small Markdown, text, and JSON files, and what was added opens", async () => {
  const ui = setup();
  ui.mount();
  await flush();
  const picker = ui.files.all().find((e) => e.type === "file");
  assert.equal(picker.accept, ".md,.txt,.json");
  const file = (name, text, size = text.length) => ({ name, size, text: async () => text });
  picker.files = [file("Meeting Notes.MD", "# Notes"), file("photo.png", "x"), file("big.txt", "x", 2 * 1024 * 1024), file("binary.txt", "a\u0000b")];
  await picker.onchange();
  await flush();
  const puts = ui.calls.filter((c) => c.method === "PUT");
  assert.deepEqual(puts.map((c) => [c.path, c.body]), [[`/api/blobs/blobs/${SESSION}/files/Meeting-Notes.md`, "# Notes"]]);
  assert.deepEqual(ui.notices.at(-1), ["error", "Not added: photo.png (only .md, .txt, and .json files), big.txt (larger than 1 MB), binary.txt (not a text file)"]);
  assert.deepEqual(ui.tabs(), ["triage", "Meeting-Notes.md"], "the uploaded file opens");
});

// ---- Sessions page, run steps, and workflow editor ----

const HOUR = 3600 * 1000;

function sessionsPage({ pending = [], stored = [], runs = {} } = {}) {
  const ids = { "session-list": new El("ul"), "session-list-state": new El("div"), "session-main": new El("div"), "session-files": new El("aside") };
  const mounts = [], hashes = [];
  const context = {
    Node: El, console,
    document: { hidden: false, createElement: (tag) => new El(tag), getElementById: (id) => ids[id] || null, addEventListener() {}, createDocumentFragment: () => new El("fragment") },
    addEventListener() {},
    location: { hash: "#sessions" },
    history: { replaceState: (_s, _t, hash) => { hashes.push(hash); context.location.hash = hash; } },
    localStorage: { getItem: () => null, setItem() {} },
    menu: { hidden: true },
    setInterval: () => 1, clearInterval() {}, setTimeout: () => 1,
    tabs: {}, activeTab: "sessions",
    sessionsState: { pending, loaded: true, error: "" },
    runIndex: { records: new Map(Object.entries(runs)), refresh: async () => {} },
    midTruncate: (text) => text,
    formatTime: (iso) => `${Math.round((Date.parse("2026-01-01T12:00:00Z") - Date.parse(iso)) / HOUR)} h ago`,
    fullTime: (iso) => `full ${iso}`,
    emptyPanel: (title) => title, errorPanel: (message) => message,
    openMenu() {}, confirmAction: async () => true, showSessionsBadge() {}, reserveToggles() {},
    refreshSessions: async () => {},
    api: async (_method, path) => {
      const session = path.slice("/api/blobs/list/".length);
      if (!stored.includes(session)) { const e = new Error("prefix not found"); e.status = 404; throw e; }
      return { directories: [], objects: [{ id: `${session}/notes.md` }] };
    },
  };
  context.window = context;
  const dom = app.slice(app.indexOf("function h("), app.indexOf("// createTree"));
  const titleCase = app.slice(app.indexOf("const titleCase ="), app.indexOf("\n", app.indexOf("const titleCase =")));
  // The sidebar names a step with its pass through stepTitle and stepPass of app.js.
  const passes = app.slice(app.indexOf("const PASS_PART ="), app.indexOf("// runIndex knows every run"));
  vm.runInNewContext(`${dom}\nconst $ = (id) => document.getElementById(id);\n${titleCase}\n${passes}\n${chat}
    InteractiveSession.mount = (_el, options) => {
      const controller = { options, closed: options.closed, destroyed: false, dirty: () => false, destroy() { this.destroyed = true; }, setClosed(value) { this.closed = value; } };
      globalThis.mounts.push(controller);
      return controller;
    };
    ${read("sessions.js")}`, Object.assign(context, { mounts }));
  const rows = () => ids["session-list"].children.map((li) => ({ title: li.querySelector(".row-title").textContent, time: li.querySelector(".session-time")?.textContent || "", tip: li.title, selected: li.getAttribute("aria-selected") }));
  return { context, mounts, hashes, rows, show: async (rest) => { context.tabs.sessions.show(rest); await flush(); }, changed: async () => { context.tabs.sessions.show(""); await flush(); } };
}

const waitingEntry = (trace, step = "talk", session = `user-sessions/${trace}/${step}`) => ({ session, trace_id: trace, workflow_id: `wf-${trace}`, workflow: "bug-triage", step, agent: "triage@v1", agents: [], artifact: `runs/${trace}/${step}`, inputs: [] });

test("two sessions of the same workflow step are told apart in the sidebar by their run's start time", async () => {
  const page = sessionsPage({
    pending: [waitingEntry("old"), waitingEntry("new")],
    runs: { old: { trace_id: "old", started: "2026-01-01T09:00:00Z" }, new: { trace_id: "new", started: "2026-01-01T11:00:00Z" } },
  });
  await page.show("");
  const rows = page.rows();
  assert.deepEqual(rows.map((r) => r.title), ["Bug Triage › Talk", "Bug Triage › Talk"]);
  assert.deepEqual(rows.map((r) => r.time), ["1 h ago", "3 h ago"], "newest run first, each with its own time");
  assert.deepEqual(rows.map((r) => r.tip), ["Started full 2026-01-01T11:00:00Z", "Started full 2026-01-01T09:00:00Z"]);
  // Without a hash the newest waiting session opens and the hash names it.
  assert.equal(page.mounts[0].options.session, "user-sessions/new/talk");
  assert.deepEqual(rows.map((r) => r.selected), ["true", "false"]);
  assert.equal(page.context.location.hash, "#sessions/new/talk");
});

test("the hash names the session, also one of a routed child run; a waiting one is open, a finished one read-only", async () => {
  const child = "user-sessions/t1/route/talk";
  const page = sessionsPage({ pending: [waitingEntry("t1", "talk", child), waitingEntry("t2")], stored: ["user-sessions/done/talk"], runs: { done: { trace_id: "done", workflow: "bug-triage", started: "2026-01-01T08:00:00Z" } } });
  await page.show("/t1/route/talk");
  assert.equal(page.mounts.length, 1);
  assert.equal(page.mounts[0].options.session, child);
  assert.equal(page.mounts[0].options.traceId, "t1");
  assert.equal(page.mounts[0].options.closed, false, "a waiting session can be worked in");
  assert.equal(page.context.location.hash, "#sessions/t1/route/talk");

  // A session that no longer waits, linked from a finished run: found in the blobstore, read-only.
  await page.show("/done/talk");
  assert.equal(page.mounts.length, 2);
  assert.ok(page.mounts[0].destroyed);
  assert.deepEqual(plain({ ...page.mounts[1].options, onComplete: undefined, filesContainer: undefined }), { session: "user-sessions/done/talk", traceId: "done", workflow: "bug-triage", step: "talk", closed: true });
  assert.deepEqual(page.rows().map((r) => r.title), ["Bug Triage › Talk", "Bug Triage › Talk", "Bug Triage › Talk"], "kept in the list while the page is open");

  // A session that does not exist gives way to the open one; nothing new is mounted.
  await page.show("/missing/talk");
  assert.equal(page.mounts.length, 2);
  assert.equal(page.context.location.hash, "#sessions/done/talk");

  // The open session stops waiting (completed elsewhere): the same view becomes read-only.
  await page.show("/t2/talk");
  assert.equal(page.mounts[2].closed, false);
  page.context.sessionsState.pending = page.context.sessionsState.pending.filter((p) => p.trace_id !== "t2");
  await page.changed();
  assert.equal(page.mounts.length, 3);
  assert.equal(page.mounts[2].closed, true);
  assert.equal(page.rows().length, 3, "it stays listed");
});

test("an interactive step of a run links to its session while it waits and after the run finished, and reads as waiting", () => {
  const runs = read("runs.js");
  const context = {};
  const helpers = runs.slice(runs.indexOf("  // waitingSessionOf finds"), runs.indexOf("  // sessionFolders finds"));
  const states = app.slice(app.indexOf("function runState("), app.indexOf("function runStateWord("));
  const waiting = app.slice(app.indexOf("const isWaiting ="), app.indexOf("\n", app.indexOf("const isWaiting =")));
  vm.runInNewContext(`${chat}\n${helpers}\n${states}\nconst decisionsState = { pending: [] }, sessionsState = { pending: [] };\n${waiting}
    globalThis.out = { waitingSessionOf, stepSession, runState, isWaiting, sessionsState, hash: ChatLogic.sessionHash };`, context);
  const { waitingSessionOf, stepSession, runState, isWaiting, sessionsState, hash } = context.out;
  const pending = [{ trace_id: "t1", step: "talk", session: "user-sessions/t1/talk" }, { trace_id: "t2", step: "talk", session: "user-sessions/t2/handoff/talk" }];
  const none = new Map();

  // While it waits: the step itself, or the route step whose child run holds the session.
  assert.equal(hash(stepSession(pending, none, "t1", "talk")), "#sessions/t1/talk");
  assert.equal(hash(stepSession(pending, none, "t2", "handoff")), "#sessions/t2/handoff/talk");
  assert.equal(stepSession(pending, none, "t2", "talk"), "", "the child's step is not a step of the parent run");
  assert.equal(stepSession(pending, none, "t1", "review"), "");
  assert.equal(stepSession(pending, none, "other", "talk"), "", "another run's session is not linked");
  assert.ok(waitingSessionOf(pending, "t1", "talk"));
  assert.equal(waitingSessionOf(pending, "t1", "review"), undefined);
  // After the run finished: the session stored in the blobstore.
  assert.equal(hash(stepSession([], new Map([["talk", "user-sessions/t1/talk"]]), "t1", "talk")), "#sessions/t1/talk");
  assert.equal(stepSession([], new Map([["talk", "../secrets"]]), "t1", "talk"), "", "only a valid session path is linked");
  // The waiting session wins over a stored folder of the same step.
  assert.equal(stepSession(pending, new Map([["handoff", "user-sessions/t2/handoff/old"]]), "t2", "handoff"), "user-sessions/t2/handoff/talk");
  assert.ok(runs.includes("if (waitingSession(id)) return \"waiting\";"), "the step reads as waiting");
  assert.ok(runs.includes("stepSession(sessionsState.pending, sessions, trace, step)"), "the step list links through stepSession");

  // The run reads as waiting while one of its sessions waits, and as completed afterwards.
  sessionsState.pending.push(pending[0]);
  assert.equal(runState("Running", isWaiting("t1")), "waiting");
  assert.equal(runState("Running", isWaiting("t3")), "running");
  assert.equal(runState("Completed", isWaiting("t1")), "done");
});

test("an interactive workflow step is stored as service, agent, and agents; other steps carry no agents", () => {
  const source = read("workflows.js");
  const fields = source.slice(source.indexOf("  const stepFields = {"), source.indexOf("  // stepForm builds"));
  const special = source.slice(source.indexOf("  const SPECIAL ="), source.indexOf("\n", source.indexOf("  const SPECIAL =")));
  const context = {};
  // The step fields reach the logic above the page code through L.
  const logic = `${read("workflowref.js")}\n${source.slice(0, source.indexOf("// ---- Page ----"))}\nconst L = WorkflowLogic;`;
  vm.runInNewContext(`${logic}\n${special}\nconst state = { agentRefs: ["triage@v2", "plain@v1", "review@v3"] };\n${fields}\nglobalThis.out = { stepFields, state };`, context);
  const { stepFields, state } = context.out;
  const stored = (s) => JSON.parse(JSON.stringify(s));

  // Switching a step to interactive preselects the first agent, like a new step.
  const s = { id: "talk", service: "gate", question: "Go on?", depends_on: ["intake"] };
  stepFields.service(s, "interactive");
  assert.deepEqual(stored(s), { id: "talk", service: "interactive", depends_on: ["intake"], agent: "triage@v2" });
  // Further agents: each once, never the main agent.
  stepFields.agents(s, ["plain@v1", "triage@v2", "plain@v1", "review@v3", ""]);
  assert.deepEqual(stored(s), { id: "talk", service: "interactive", depends_on: ["intake"], agent: "triage@v2", agents: ["plain@v1", "review@v3"] });
  // What is stored reads back the same.
  const again = stored(s);
  stepFields.agents(again, again.agents);
  stepFields.service(again, "interactive");
  assert.deepEqual(stored(again), stored(s));
  // A further agent that becomes the main agent is not listed twice; none left, no agents key.
  stepFields.agent(s, "plain@v1");
  assert.deepEqual([s.agent, stored(s.agents)], ["plain@v1", ["review@v3"]]);
  stepFields.agents(s, ["plain@v1"]);
  assert.equal("agents" in s, false);

  // An agent step that becomes interactive keeps its agent; back to agent, gate, or route no agents remain.
  const a = { id: "work", service: "agent", agent: "review@v3" };
  stepFields.service(a, "interactive");
  assert.equal(a.agent, "review@v3");
  stepFields.agents(a, ["plain@v1"]);
  stepFields.service(a, "agent");
  assert.deepEqual(stored(a), { id: "work", service: "agent", agent: "review@v3" });
  for (const service of ["gate", "route", "custom-queue"]) {
    const o = { id: "x", service: "interactive", agent: "triage@v2", agents: ["plain@v1"] };
    stepFields.service(o, service);
    assert.deepEqual(stored(o), { id: "x", service });
  }
  // Without any agent in the registry nothing is preselected.
  state.agentRefs = [];
  const empty = { id: "y", service: "gate" };
  stepFields.service(empty, "interactive");
  assert.deepEqual(stored(empty), { id: "y", service: "interactive" });
});
