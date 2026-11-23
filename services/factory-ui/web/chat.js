"use strict";
// Interactive sessions: a person chats with the agents of an interactive workflow step, works
// with the session's text files, and completes the step by choosing one file as its output.
//
//   InteractiveSession.mount(container, { session, traceId, workflowId, workflow, step, agent,
//     agents, artifact, inputs }) -> { destroy(), dirty(), setClosed(closed), refresh() }
//
// The team view (teams.js) mounts the same view read-only over a team's workspace: chip()
// gives its status chip, emptyTitle names the state without tabs, onActivate(id) follows the
// open tab, and openView(id, name, pane) adds a tab of its own (a channel's chat log);
// workspaceOnly says there are only files, so neither chat logs nor a completion record are
// asked for, and time() gives the time of the status line (the run's start).
//
// Storage (blobstore, through /api/blobs): <session>/files/<name> are the shared files,
// <session>/chats/<tab>.json is the history of one chat tab (written only by the agent service),
// and <session>/completed.json makes the session read-only. A message is one
// POST /api/agent/chat; the reply arrives when the whole turn has ended (no streaming).
// Chat is append-only: nothing here edits or removes a message.

// ---- Logic without the DOM (covered by chat_ui_test.mjs) ----

const ChatLogic = (() => {
  const TAB_ID = /^[a-z0-9][a-z0-9-]{0,39}$/;
  const FILE_NAME = /^[A-Za-z0-9][A-Za-z0-9._-]{0,99}\.(md|txt|json)$/;
  const SESSION = /^user-sessions(\/[A-Za-z0-9_-][A-Za-z0-9._-]*){2,}$/;
  const MAX_UPLOAD_BYTES = 1024 * 1024;

  const validTabId = (id) => TAB_ID.test(String(id));
  const validFileName = (name) => FILE_NAME.test(String(name));
  const validSession = (session) => String(session).length <= 400 && SESSION.test(String(session));

  // agentName is the name of a pinned agent reference: "triage@v2" is "triage".
  const agentName = (ref) => String(ref || "").split("@")[0];

  // newTabId names a chat tab after its agent, numbered when that name is taken, so the chat
  // log of a tab is recognisable in the blobstore: triage, triage-2, triage-3.
  function newTabId(agent, taken = []) {
    const used = new Set(taken);
    const base = agentName(agent).toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/^-+|-+$/g, "").slice(0, 32).replace(/-+$/, "") || "chat";
    if (!used.has(base)) return base;
    for (let n = 2; ; n++) if (!used.has(`${base}-${n}`)) return `${base}-${n}`;
  }

  // tabLabels names chat tabs by their agent; a second tab with the same agent gets a number.
  function tabLabels(tabs) {
    const seen = new Map(), labels = new Map();
    for (const t of tabs) {
      const name = agentName(t.agent) || t.id;
      const n = (seen.get(name) || 0) + 1;
      seen.set(name, n);
      labels.set(t.id, n > 1 ? `${name} ${n}` : name);
    }
    return labels;
  }

  // A session file is Markdown, JSON, or plain text, by its extension.
  const fileKind = (name) => (/\.md$/.test(name) ? "markdown" : /\.json$/.test(name) ? "json" : "text");

  // storedFileName turns the name of an uploaded file into a session file name: characters a
  // name may not hold become dashes and the extension is lowercase. "" when the file is not
  // Markdown, plain text, or JSON, or no name is left.
  function storedFileName(name) {
    const parts = /^(.*)\.([A-Za-z]+)$/.exec(String(name).trim());
    if (!parts || !["md", "txt", "json"].includes(parts[2].toLowerCase())) return "";
    const base = parts[1].replace(/[^A-Za-z0-9._-]+/g, "-").replace(/^[^A-Za-z0-9]+/, "").slice(0, 100).replace(/[-._]+$/, "");
    const stored = `${base}.${parts[2].toLowerCase()}`;
    return validFileName(stored) ? stored : "";
  }

  // uploadProblem says why a file cannot be added to the session, or "" when it can.
  function uploadProblem(name, size, text) {
    if (!storedFileName(name)) return "only .md, .txt, and .json files";
    if (size > MAX_UPLOAD_BYTES) return "larger than 1 MB";
    if (typeof text === "string" && text.includes("\u0000")) return "not a text file";
    return "";
  }

  // findMatches returns where query occurs in text: start offsets, ignoring case, not overlapping.
  function findMatches(text, query) {
    let hay = String(text), needle = String(query || "");
    if (!needle) return [];
    // Lowercasing changes the length of a few characters; offsets must stay those of the text.
    if (hay.toLowerCase().length === hay.length && needle.toLowerCase().length === needle.length) { hay = hay.toLowerCase(); needle = needle.toLowerCase(); }
    const found = [];
    for (let at = hay.indexOf(needle); at >= 0; at = hay.indexOf(needle, at + needle.length)) found.push(at);
    return found;
  }

  // stepMatch moves the current match forward or back and wraps around; -1 without matches.
  const stepMatch = (current, count, delta) => (count ? (((current + delta) % count) + count) % count : -1);

  // contentText reads the content of an OpenAI-format message: a string or a list of parts.
  function contentText(content) {
    if (typeof content === "string") return content;
    if (Array.isArray(content)) return content.map((part) => (typeof part === "string" ? part : part?.text || "")).join("");
    return content === null || content === undefined ? "" : String(content);
  }

  function prettyArguments(value) {
    if (value === null || value === undefined || value === "") return "";
    if (typeof value !== "string") return JSON.stringify(value, null, 2);
    try { return JSON.stringify(JSON.parse(value), null, 2); } catch { return value; }
  }

  // conversation maps a stored chat log (the exact OpenAI-format list sent to the model) to
  // what the history shows, in order: the system message, user messages, assistant answers,
  // and one compact entry per tool call with its arguments and, once it arrived, its result.
  function conversation(messages) {
    const items = [], calls = new Map();
    for (const m of Array.isArray(messages) ? messages : []) {
      if (!m || typeof m !== "object") continue;
      const text = contentText(m.content);
      if (m.role === "system") items.push({ kind: "system", text });
      else if (m.role === "user") items.push({ kind: "user", text });
      else if (m.role === "assistant") {
        if (text.trim()) items.push({ kind: "assistant", text });
        for (const call of Array.isArray(m.tool_calls) ? m.tool_calls : []) {
          const item = { kind: "tool", id: call?.id || "", name: call?.function?.name || "unknown", args: prettyArguments(call?.function?.arguments), result: null };
          items.push(item);
          if (item.id) calls.set(item.id, item);
        }
      } else if (m.role === "tool") {
        const call = calls.get(m.tool_call_id);
        if (call && call.result === null) call.result = text;
        else items.push({ kind: "tool", id: m.tool_call_id || "", name: m.name || "unknown", args: "", result: text });
      }
    }
    return items;
  }

  // mermaidBlocks returns the source of every fenced code block tagged mermaid, reading fences
  // the way renderMarkdown does.
  function mermaidBlocks(text) {
    const lines = String(text).replace(/\r\n?/g, "\n").split("\n"), blocks = [];
    for (let i = 0; i < lines.length; i++) {
      const fence = lines[i].match(/^\s*(```|~~~)([\w-]*)\s*$/);
      if (!fence) continue;
      const end = new RegExp(`^\\s*${fence[1]}\\s*$`), code = [];
      for (i++; i < lines.length && !end.test(lines[i]); i++) code.push(lines[i]);
      if (fence[2].toLowerCase() === "mermaid") blocks.push(code.join("\n"));
    }
    return blocks;
  }

  // maskTablePipes replaces every escaped pipe (\|) in the rows of a Markdown table by
  // PIPE_MARK, so renderMarkdown keeps the cell together; unmaskPipes turns the marks of the
  // rendered text into pipes. Code spans and fenced code blocks stay exactly as written.
  const PIPE_MARK = "";
  function maskTablePipes(text) {
    const divider = (line) => /^\s*\|?\s*:?-{3,}:?\s*(\|\s*:?-{3,}:?\s*)+\|?\s*$/.test(line);
    const lines = String(text).replace(/\r\n?/g, "\n").split("\n");
    let fence = null, table = false;
    for (let i = 0; i < lines.length; i++) {
      const line = lines[i];
      if (fence) { if (fence.test(line)) fence = null; continue; }
      const open = line.match(/^\s*(```|~~~)([\w-]*)\s*$/);
      if (open) { fence = new RegExp(`^\\s*${open[1]}\\s*$`); table = false; continue; }
      table = line.includes("|") && line.trim() !== "" && (table || divider(lines[i + 1] || ""));
      if (table) lines[i] = line.replace(/(`+)[\s\S]*?\1|\\\|/g, (found) => (found === "\\|" ? PIPE_MARK : found));
    }
    return lines.join("\n");
  }
  const unmaskPipes = (text) => String(text).replaceAll(PIPE_MARK, "|");

  // maskWordUnderscores replaces every underscore between two letters or digits by
  // UNDERSCORE_MARK, so renderMarkdown does not read the names in a message (send_message,
  // test_slugify.py) as emphasis; unmaskUnderscores turns the marks of the rendered text back.
  const UNDERSCORE_MARK = "\uE001";
  const maskWordUnderscores = (text) => String(text).replace(/([\p{L}\p{N}])(_+)(?=[\p{L}\p{N}])/gu, (_, before, run) => before + UNDERSCORE_MARK.repeat(run.length));
  const unmaskUnderscores = (text) => String(text).replaceAll(UNDERSCORE_MARK, "_");

  // A session file is shown as code unless it is Markdown, plain text, or JSON (a team's workspace holds code files).
  const codeFile = (name) => !/\.(md|txt|json)$/.test(String(name));

  // prettyJSON indents a JSON document; text that does not parse is returned as it is, with the reason.
  function prettyJSON(text) {
    try { return { ok: true, text: JSON.stringify(JSON.parse(text), null, 2), error: "" }; }
    catch (e) { return { ok: false, text: String(text), error: e.message }; }
  }
  // jsonProblem says why text is not a JSON document, or "" when it is one.
  const jsonProblem = (text) => prettyJSON(text).error;

  // A session opens at #sessions/<trace>/<step>: its blobstore path without user-sessions/.
  const sessionHash = (session) => `#sessions/${String(session).replace(/^user-sessions\//, "")}`;
  function sessionFromHash(rest) {
    const path = String(rest || "").split("?")[0].replace(/^\/+|\/+$/g, "");
    return path && validSession(`user-sessions/${path}`) ? `user-sessions/${path}` : "";
  }

  return {
    MAX_UPLOAD_BYTES, PIPE_MARK, validTabId, validFileName, validSession, agentName, newTabId, tabLabels, fileKind, storedFileName,
    uploadProblem, findMatches, stepMatch, contentText, conversation, mermaidBlocks, maskTablePipes, unmaskPipes, UNDERSCORE_MARK, maskWordUnderscores, unmaskUnderscores, codeFile, prettyJSON, jsonProblem, sessionHash, sessionFromHash,
  };
})();

// ---- The session view ----

const InteractiveSession = (() => {
  const MERMAID_VERSION = "12.3.0"; // the file in vendor/, see vendor/README.md
  const REFRESH_MS = 10000;
  let mermaidLoading = null, mermaidSeq = 0, fieldSeq = 0;

  // loadMermaid loads the vendored Mermaid file the first time a diagram is shown; it is served
  // by the factory UI itself, never by a CDN.
  function loadMermaid() {
    mermaidLoading ??= new Promise((resolve, reject) => {
      if (window.mermaid) { resolve(window.mermaid); return; }
      const script = h("script", { src: `vendor/mermaid.min.js?v=${MERMAID_VERSION}` });
      script.onload = () => (window.mermaid ? resolve(window.mermaid) : reject(new Error("the diagram library did not start")));
      script.onerror = () => { script.remove(); reject(new Error("the diagram library did not load")); };
      document.head.append(script);
    }).then((mermaid) => {
      mermaid.initialize({
        startOnLoad: false, securityLevel: "strict", suppressErrorRendering: true,
        theme: window.matchMedia?.("(prefers-color-scheme: dark)").matches ? "dark" : "default",
      });
      return mermaid;
    });
    mermaidLoading.catch(() => { mermaidLoading = null; }); // the next diagram tries again
    return mermaidLoading;
  }

  // drawDiagram replaces a mermaid code block with its diagram. When Mermaid cannot draw it,
  // the code block stays and a note under it says why.
  async function drawDiagram(pre, source, resized) {
    const id = `mermaid-diagram-${++mermaidSeq}`;
    try {
      const mermaid = await loadMermaid();
      await mermaid.parse(source);
      const { svg } = await mermaid.render(id, source);
      const figure = h("div", { className: "mermaid-diagram", role: "img", "aria-label": "Diagram" });
      figure.innerHTML = svg; // Mermaid's own SVG, produced at its strict security level
      pre.replaceWith(figure);
    } catch (e) {
      document.getElementById(`d${id}`)?.remove(); // what a failed render leaves behind
      document.getElementById(id)?.remove();
      const reason = String(e?.message || e || "unknown error");
      pre.after(h("p", { className: "error-line mermaid-error", title: reason }, `Diagram not rendered: ${reason.split("\n")[0]}`));
    }
    resized();
  }

  // renderRich is renderMarkdown plus diagrams: fenced mermaid blocks are drawn once the
  // library has loaded; resized is called whenever a diagram changed the height.
  function renderRich(text, resized = () => {}) {
    // An escaped pipe (\|) in a table row belongs to its cell and is shown as a pipe.
    // An underscore inside a word is part of a name, never emphasis.
    const masked = ChatLogic.maskWordUnderscores(ChatLogic.maskTablePipes(text));
    const root = renderMarkdown(masked);
    if (masked.includes(ChatLogic.PIPE_MARK) || masked.includes(ChatLogic.UNDERSCORE_MARK)) {
      const texts = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
      while (texts.nextNode()) texts.currentNode.data = ChatLogic.unmaskUnderscores(ChatLogic.unmaskPipes(texts.currentNode.data));
      for (const link of root.querySelectorAll("a[href]")) link.setAttribute("href", ChatLogic.unmaskUnderscores(link.getAttribute("href")));
    }
    // A table sits in its own scrolling box, so a wide one scrolls instead of squeezing its cells.
    for (const table of root.querySelectorAll("table")) {
      const box = h("div", { className: "table-scroll" });
      table.replaceWith(box);
      box.append(table);
    }
    for (const code of root.querySelectorAll("pre > code")) {
      if (code.className.toLowerCase() === "language-mermaid") drawDiagram(code.parentElement, code.textContent, resized);
    }
    return root;
  }

  // readBlob returns the text of a blobstore object, or null when there is none.
  async function readBlob(id) {
    const res = await fetch(`/api/blobs/blobs/${id}`);
    const text = await res.text();
    if (res.status === 404) return null;
    if (!res.ok) throw new Error(errorMessage(text, res));
    return text;
  }
  async function readJSON(id) {
    const text = await readBlob(id);
    if (text === null) return null;
    try { return JSON.parse(text); } catch { return null; }
  }
  // listObjects lists the objects directly under a prefix; a missing prefix holds none.
  async function listObjects(prefix) {
    try { return (await api("GET", `/api/blobs/list/${prefix}`)).objects || []; }
    catch (e) { if (e.status === 404 || /not found/i.test(e.message)) return []; throw e; }
  }

  function mount(container, options) {
    const o = { agents: [], inputs: [], ...options };
    const session = o.session;
    const agents = [...new Set([o.agent, ...(o.agents || [])].filter(Boolean))]; // the main agent first
    const s = {
      tabs: [], active: "", files: [], filesKey: null, filesError: "", loadError: "", started: false, starting: false,
      completed: null, completing: null, closed: !!o.closed, destroyed: false, uploading: false,
    };
    const locked = () => !!(s.completed || s.completing || s.closed);
    const chats = () => s.tabs.filter((t) => t.kind === "chat");
    const viewers = () => s.tabs.filter((t) => t.kind === "file");
    const tab = (id) => s.tabs.find((t) => t.id === id);
    const fileId = (name) => `${session}/files/${name}`;
    const writeHeaders = () => ({ ...creatorHeaders(), "Content-Type": "text/plain", ...(o.traceId ? { "X-Trace-Id": o.traceId } : {}) });

    // ---- Skeleton ----

    const tabList = h("div", { className: "chat-tablist", role: "tablist", "aria-label": "Chats and files" });
    const newChat = iconButton("plus", "New chat", (ev) => pickAgent(ev));
    const head = h("div", { className: "panel-head chat-tabs" }, tabList, newChat);
    const meta = h("div", { className: "run-meta session-meta" });
    const panes = h("div", { className: "chat-panes" });
    const main = o.filesContainer ? container : h("div", { className: "panel" });
    main.classList.add("chat-main");

    const picker = h("input", { type: "file", multiple: true, accept: ".md,.txt,.json", hidden: true });
    const upload = iconButton("upload", "Upload files", () => picker.click());
    const fileList = h("ul", { className: "rows", role: "listbox", "aria-label": "Session files" });
    const fileState = h("div");
    const complete = h("button", { type: "button", className: "primary", onclick: () => openComplete() }, "Complete…");
    const completeRow = h("div", { className: "section actions chat-complete" }, complete);
    const filesBox = o.filesContainer || h("aside", { className: "panel", "aria-label": "Session files" });
    filesBox.classList.add("chat-files");
    filesBox.replaceChildren(
      h("div", { className: "panel-head" }, h("h2", {}, "Files"), h("span", { className: "panel-actions" }, upload, picker)),
      h("div", { className: "panel-body" }, fileList, fileState),
      completeRow);
    main.replaceChildren(head, meta, panes);
    if (!o.filesContainer) container.replaceChildren(h("div", { className: "chat-layout" }, main, filesBox));

    // ---- Tabs ----

    function renderTabs() {
      const labels = ChatLogic.tabLabels(chats());
      tabList.replaceChildren(...s.tabs.map((t) => {
        const chat = t.kind === "chat";
        const label = chat ? labels.get(t.id) : t.name;
        const button = h("button", {
          type: "button", role: "tab", className: "chat-tab-button", "aria-selected": String(t.id === s.active), tabIndex: t.id === s.active ? 0 : -1,
          // A chat tab shows the agent's name; its tooltip adds the pinned version.
          title: chat && t.agent !== label ? t.agent : undefined, dataset: chat ? undefined : { full: label },
          "aria-label": !chat && t.dirty ? `${label}, unsaved edits` : undefined,
          onclick: () => activate(t.id, true),
        }, label, !chat && t.dirty ? h("span", { className: "chat-tab-dirty", "aria-hidden": "true" }, " •") : null);
        return h("div", { className: `chat-tab${t.id === s.active ? " active" : ""}`, role: "presentation" }, button,
          chat ? null : iconButton("close", `Close ${label}`, () => closeTab(t.id), { className: "icon-button chat-tab-close" }));
      }));
      tabList.querySelector(".chat-tab.active")?.scrollIntoView?.({ block: "nearest", inline: "nearest" });
    }

    tabList.addEventListener("keydown", (ev) => {
      if (ev.key !== "ArrowRight" && ev.key !== "ArrowLeft") return;
      const at = s.tabs.findIndex((t) => t.id === s.active);
      const next = s.tabs[at + (ev.key === "ArrowRight" ? 1 : -1)];
      if (!next) return;
      ev.preventDefault();
      activate(next.id);
      tabList.querySelector('[role="tab"][aria-selected="true"]')?.focus();
    });

    function activate(id, focus = false) {
      if (!tab(id)) id = s.tabs[0]?.id || "";
      s.active = id;
      for (const t of s.tabs) t.pane.hidden = t.id !== id;
      renderTabs();
      renderFiles(true);
      const t = tab(id);
      if (t?.kind === "chat") {
        if (t.atEnd) toEnd(t);
        if (focus && !locked()) t.input.focus();
      }
      renderEmpty();
      o.onActivate?.(s.active);
    }

    function addTab(t) {
      s.tabs.push(t);
      panes.append(t.pane);
    }

    async function closeTab(id) {
      const t = tab(id);
      if (!t) return;
      if (t.dirty && !(await confirmAction("Discard unsaved edits?", "Discard"))) return;
      const at = s.tabs.indexOf(t);
      if (at < 0) return;
      s.tabs.splice(at, 1);
      t.pane.remove();
      // The neighbour takes its place: the next tab, else the previous one.
      if (s.active === id) activate((s.tabs[at] || s.tabs[at - 1])?.id || "");
      else renderTabs();
    }

    // renderEmpty explains panes without a tab: a load error, or a session nobody chatted in.
    const emptyBox = h("div", { className: "chat-empty" });
    function renderEmpty() {
      // emptyTitle can be a function: "" while the page has not read what it shows, so nothing is claimed empty.
      const title = typeof o.emptyTitle === "function" ? o.emptyTitle() : o.emptyTitle || "No chats";
      const content = s.loadError ? errorPanel(s.loadError) : s.started && !s.tabs.length && title ? emptyPanel(title) : null;
      fill(emptyBox, content);
      if (content) panes.append(emptyBox); else emptyBox.remove();
    }

    // ---- Status line ----

    function renderMeta() {
      const sep = () => h("span", { className: "sep" }, "·");
      const done = s.completed;
      const chip = o.chip ? o.chip() : done ? statusChip("done") : s.completing ? statusChip("running", "Completing")
        : s.closed ? statusChip("idle", "Closed", "The run no longer waits at this step") : statusChip("waiting");
      const artifact = done?.artifact || (done ? o.artifact : "");
      const at = o.time?.() || "";
      fill(meta, chip,
        at ? [sep(), timeEl(at)] : null,
        done?.file ? [sep(), h("a", { href: location.hash || "#", title: "Opens the submitted file in a tab", onclick: (ev) => { ev.preventDefault(); openFile(done.file); } }, done.file)] : null,
        done?.completed_by ? [sep(), h("span", { dataset: { full: done.completed_by } }, done.completed_by)] : null,
        done?.completed_at ? [sep(), timeEl(done.completed_at)] : null,
        artifact && o.traceId ? [sep(), h("a", { href: `#artifacts?trace=${encodeURIComponent(o.traceId)}&object=${encodeURIComponent(artifact)}` }, "View output")] : null,
        o.traceId ? [sep(), h("a", { href: `#runs/${encodeURIComponent(o.traceId)}` }, "View run"),
          sep(), h("span", { className: "meta-pair" }, h("a", { href: `#artifacts?trace=${encodeURIComponent(o.traceId)}` }, "View trace"), copyButton(o.traceId, "trace ID"))] : null);
    }

    // applyLock shows or hides everything that changes the session. A completed session, one
    // being completed, and one whose run no longer waits are read-only.
    function applyLock() {
      const off = locked();
      newChat.hidden = off || !agents.length;
      upload.hidden = off;
      completeRow.hidden = off;
      for (const t of chats()) { t.composer.hidden = off; renderHistory(t); }
      for (const t of viewers()) renderViewer(t);
      renderMeta();
      renderComplete();
    }

    // ---- Chat ----

    function addChat(id, agent, messages = [], stored = false) {
      const name = ChatLogic.agentName(agent);
      const t = { id, kind: "chat", agent, messages, stored, busy: false, pending: "", atEnd: true, nodes: [] };
      t.history = h("div", { className: "chat-history", role: "log", tabIndex: 0, "aria-label": `Conversation with ${name}` });
      t.history.addEventListener("scroll", () => {
        if (t.history.clientHeight) t.atEnd = t.history.scrollHeight - t.history.scrollTop - t.history.clientHeight < 48;
      });
      t.input = h("textarea", { id: `chat-message-${++fieldSeq}`, className: "chat-input", rows: 1, autocomplete: "off" });
      t.sendButton = h("button", { type: "button", onclick: () => send(t) }, "Send");
      t.errorBox = h("p", { className: "error-line chat-error", role: "alert", hidden: true });
      t.composer = h("div", { className: "chat-composer" }, t.errorBox,
        h("div", { className: "actions" }, h("div", { className: "field" }, h("label", { htmlFor: t.input.id }, `Message to ${name}`), t.input), t.sendButton));
      t.input.addEventListener("input", () => { growInput(t); renderSend(t); });
      // Enter sends; Shift+Enter starts a new line. Enter that confirms an input-method choice does neither.
      t.input.addEventListener("keydown", (ev) => {
        if (ev.key !== "Enter" || ev.shiftKey || ev.isComposing || ev.keyCode === 229) return;
        ev.preventDefault();
        send(t);
      });
      t.pane = h("div", { className: "chat-pane", role: "tabpanel", hidden: true }, t.history, t.composer);
      t.composer.hidden = locked();
      addTab(t);
      renderHistory(t);
      renderSend(t);
      return t;
    }

    function growInput(t) {
      t.input.style.height = "auto";
      t.input.style.height = `${Math.min(200, t.input.scrollHeight + 2)}px`;
    }

    // renderSend: one message at a time per tab, and never an empty one; the tooltip says why.
    function renderSend(t) {
      const reason = t.busy ? "Waits for the answer" : t.input.value.trim() ? "" : "Needs a message";
      t.sendButton.disabled = !!reason;
      t.sendButton.title = reason || `Sends the message to ${t.agent}`;
    }

    function toEnd(t) {
      t.history.scrollTop = t.history.scrollHeight;
      t.atEnd = true;
    }

    function messageNode(t, item) {
      const resized = () => { if (t.atEnd) toEnd(t); };
      if (item.kind === "user") return h("div", { className: "chat-message user" },
        h("div", { className: "chat-author" }, "You"), h("div", { className: "chat-bubble" }, renderRich(item.text, resized)));
      if (item.kind === "assistant") return h("div", { className: "chat-message assistant" },
        h("div", { className: "chat-author" }, ChatLogic.agentName(t.agent), copyButton(item.text, "answer")), renderRich(item.text, resized));
      if (item.kind === "system") return h("details", { className: "chat-detail" }, h("summary", {}, "System message"),
        h("pre", { className: "file-block prose" }, item.text));
      const result = item.result === null ? null : isJSONDocument(item.result)
        ? h("pre", { className: "file-block code" }, pretty(item.result)) : h("pre", { className: "file-block prose" }, item.result);
      return h("details", { className: "chat-detail" }, h("summary", {}, "Tool call · ", h("span", { className: "mono" }, item.name)),
        item.args ? [h("h4", {}, "Arguments"), h("pre", { className: "file-block code" }, item.args)] : null,
        h("h4", {}, "Result"), result || h("p", { className: "muted" }, "No result stored"));
    }

    // renderHistory draws the stored messages, then the message being sent and the busy line.
    // A chat log only grows, so the nodes of messages already shown are kept: an open tool
    // call stays open and a drawn diagram is not drawn again. New content keeps the view at
    // the end when it was there.
    function renderHistory(t, toBottom = false) {
      const stick = toBottom || t.atEnd;
      const items = ChatLogic.conversation(t.messages);
      t.nodes = items.map((item, i) => {
        const key = JSON.stringify(item);
        return t.nodes[i]?.key === key ? t.nodes[i] : { key, node: messageNode(t, item) };
      });
      const rows = t.nodes.map((n) => n.node);
      if (t.pending) rows.push(h("div", { className: "chat-message user" }, h("div", { className: "chat-author" }, "You"), h("div", { className: "chat-bubble" }, renderRich(t.pending))));
      if (t.busy) rows.push(h("div", { className: "chat-busy", role: "status" }, statusChip("running"), h("span", {}, ChatLogic.agentName(t.agent))));
      if (!rows.length) rows.push(emptyPanel("No messages yet"));
      t.history.replaceChildren(...rows);
      if (stick) toEnd(t);
    }

    function showChatError(t, text) {
      t.errorBox.textContent = text;
      t.errorBox.hidden = !text;
    }

    // reloadLog reads the tab's history as the agent service stored it; false when there is none yet.
    async function reloadLog(t) {
      const log = await readJSON(`${session}/chats/${t.id}.json`);
      if (!log || !Array.isArray(log.messages)) return false;
      t.messages = log.messages;
      if (log.agent) t.agent = log.agent;
      t.stored = true;
      return true;
    }

    // send posts one message and waits for the turn to end. A second send while one is in
    // flight is ignored. A failed message goes back into the box, so it can be sent again.
    async function send(t) {
      const text = t.input.value.trim();
      if (!text || t.busy || locked() || s.destroyed) return;
      t.busy = true;
      t.pending = text;
      t.input.value = "";
      growInput(t);
      showChatError(t, "");
      renderSend(t);
      renderHistory(t, true);
      let reply = null, failure = "";
      try {
        reply = await api("POST", "/api/agent/chat", {
          workflow_id: o.workflowId, step_id: o.step, session, tab: t.id, agent: t.agent, message: text, created_by: creator(),
        });
      } catch (e) { failure = e.message; }
      // The stored log is the truth, also after a failure: the view shows what the agent kept.
      let stored = false;
      try { stored = await reloadLog(t); } catch { /* the reply below still shows */ }
      if (reply && !stored) t.messages = [...t.messages, { role: "user", content: text }, { role: "assistant", content: reply.reply || "" }];
      t.busy = false;
      t.pending = "";
      if (s.destroyed) return;
      if (failure) {
        showChatError(t, `Not sent: ${failure}`);
        if (!t.input.value.trim()) { t.input.value = text; growInput(t); }
      }
      renderSend(t);
      renderHistory(t, !failure);
      renderTabs(); // the stored log names the tab's agent
      refreshFiles(); // the agent may have written files
    }

    // pickAgent opens a further chat tab: at once when the step has one agent, else after a choice.
    function pickAgent(ev) {
      if (locked() || !agents.length) return;
      if (agents.length === 1) { openChat(agents[0]); return; }
      const at = newChat.getBoundingClientRect();
      openMenu({ preventDefault() {}, stopPropagation() {}, currentTarget: newChat, clientX: ev?.clientX || at.left, clientY: ev?.clientY || at.bottom + 4 },
        agents.map((ref) => [ref, () => openChat(ref)]));
    }

    function openChat(agent) {
      const t = addChat(ChatLogic.newTabId(agent, chats().map((c) => c.id)), agent);
      activate(t.id, true);
    }

    // ---- Files ----

    const version = (name) => { const f = s.files.find((x) => x.name === name); return f ? `${f.created}|${f.size}` : ""; };

    async function refreshFiles() {
      let objects;
      try { objects = await listObjects(`${session}/files`); s.filesError = ""; }
      catch (e) { if (s.destroyed) return; s.filesError = `Cannot list files: ${e.message}`; renderFiles(); return; }
      if (s.destroyed) return;
      s.filesListed = true; // only now can the list be called empty
      s.files = objects.map((x) => ({ name: x.id.split("/").pop(), size: x.size, created: x.created }))
        .sort((a, b) => a.name.localeCompare(b.name, undefined, { numeric: true, sensitivity: "base" }));
      renderFiles();
      // A viewer follows its file: gone with it, read again when it changed, unless it is being edited.
      for (const t of viewers()) {
        if (t.editing || t.loading || !t.loaded) continue;
        if (!version(t.name)) closeTab(t.id);
        else if (t.version !== version(t.name)) loadFile(t);
      }
    }

    function renderFiles(force = false) {
      const open = tab(s.active)?.kind === "file" ? tab(s.active).name : "";
      const key = JSON.stringify([s.files, s.filesError, open, locked(), !!s.filesListed]);
      if (!force && key === s.filesKey) return;
      if (!menu.hidden && menu.isConnected && fileList.querySelector(".menu-target")) return; // the outlined row stays while its menu is open
      s.filesKey = key;
      fill(fileState, s.filesError ? errorPanel(s.filesError) : s.files.length || !s.filesListed ? null : emptyPanel("No files"));
      fileList.replaceChildren(...(s.filesError ? [] : s.files).map((f) => {
        const li = h("li", { role: "option", tabIndex: 0, "aria-selected": String(f.name === open) }, h("div", { className: "row-title", dataset: { full: f.name } }, f.name));
        li.addEventListener("click", () => openFile(f.name));
        li.addEventListener("keydown", (ev) => {
          if (ev.key === "Enter" || ev.key === " ") { ev.preventDefault(); openFile(f.name); }
          if ((ev.key === "Delete" || ev.key === "Backspace") && !locked()) { ev.preventDefault(); deleteFile(f.name); }
          if (ev.key === "ArrowDown") li.nextElementSibling?.focus();
          if (ev.key === "ArrowUp") li.previousElementSibling?.focus();
        });
        li.addEventListener("contextmenu", (ev) => openMenu(ev, [
          ["Open", () => openFile(f.name), "", "Opens the file in a tab"],
          ["Download", () => downloadBlob(fileId(f.name), f.name), "", "Saves the file on this computer under its name"],
          ...(locked() ? [] : ["-", ["Delete", () => deleteFile(f.name), "danger", "Deletes the file from the session"]]),
        ]));
        return li;
      }));
      renderComplete();
    }

    async function deleteFile(name) {
      if (locked()) return;
      if (!(await confirmAction(`Delete ${name}?`, "Delete"))) return;
      try {
        const res = await fetch(`/api/blobs/blobs/${fileId(name)}`, { method: "DELETE", headers: creatorHeaders() });
        if (!res.ok && res.status !== 404) throw new Error(errorMessage(await res.text(), res));
        setSaveState("Deleted");
        const open = tab(`file:${name}`);
        if (open) { open.dirty = false; await closeTab(open.id); }
      } catch (e) { setStatus(`Not deleted: ${e.message}`, "error"); }
      refreshFiles();
    }

    // Upload: Markdown, plain text, and JSON files up to 1 MB; anything else is refused by name.
    // What was added opens. A file with the name of an existing one replaces it after a question.
    picker.onchange = async () => {
      const chosen = [...picker.files];
      picker.value = "";
      if (!chosen.length || locked() || s.uploading) return;
      s.uploading = true;
      const refused = [], stored = [];
      try {
        for (const file of chosen) {
          let text;
          let problem = ChatLogic.uploadProblem(file.name, file.size);
          if (!problem) {
            try { text = await file.text(); problem = ChatLogic.uploadProblem(file.name, file.size, text); }
            catch { problem = "cannot be read"; }
          }
          if (problem) { refused.push(`${file.name} (${problem})`); continue; }
          const name = ChatLogic.storedFileName(file.name);
          if (version(name) && !(await confirmAction(`Replace ${name}?`, "Replace"))) continue;
          try {
            const res = await fetch(`/api/blobs/blobs/${fileId(name)}`, { method: "PUT", headers: writeHeaders(), body: text });
            if (!res.ok) throw new Error(errorMessage(await res.text(), res));
            stored.push(name);
          } catch (e) { refused.push(`${file.name} (${e.message})`); }
        }
      } finally { s.uploading = false; }
      if (refused.length) setStatus(`Not added: ${refused.join(", ")}`, "error");
      else if (stored.length) setSaveState(stored.length === 1 ? "Added" : `Added ${stored.length} files`);
      await refreshFiles(); // an open viewer of a replaced file reads it again
      if (stored.length) openFile(stored[0]);
    };

    // ---- File viewer ----

    function openFile(name) {
      const id = `file:${name}`;
      if (!tab(id)) {
        const t = { id, kind: "file", name, text: "", loaded: false, loading: false, loadError: "", editing: false, dirty: false, saving: false, version: "", original: "", marks: [], offsets: [], current: -1 };
        t.search = h("input", { className: "viewer-search", placeholder: "Search", "aria-label": `Search in ${name}`, autocomplete: "off", spellcheck: false });
        t.count = h("span", { className: "viewer-count", role: "status" });
        t.prev = iconButton("up", "Previous match", () => stepSearch(t, -1));
        t.next = iconButton("down", "Next match", () => stepSearch(t, 1));
        // Download saves the stored file under its name; an HTML file is saved, never rendered here.
        t.download = iconButton("download", "Download file", () => downloadBlob(fileId(t.name), t.name));
        t.edit = iconButton("edit", "Edit file", () => startEdit(t));
        t.cancel = h("button", { type: "button", onclick: () => cancelEdit(t) }, "Cancel");
        t.save = h("button", { type: "button", onclick: () => saveFile(t) }, "Save");
        t.problem = h("p", { className: "error-text viewer-problem", role: "alert", hidden: true });
        t.body = h("div", { className: "viewer-body" });
        t.area = h("textarea", { className: "viewer-editor", hidden: true, spellcheck: false, autocomplete: "off", "aria-label": `Text of ${name}` });
        t.pane = h("div", { className: "chat-pane viewer-pane", role: "tabpanel", hidden: true },
          h("div", { className: "viewer-bar" }, t.search, t.count, t.prev, t.next, h("span", { className: "viewer-actions" }, t.download, t.edit, t.cancel, t.save)),
          t.problem, t.body, t.area);
        t.search.addEventListener("input", () => applySearch(t));
        t.search.addEventListener("keydown", (ev) => {
          // In the editor Enter only counts: moving the focus there would let a second Enter replace the match.
          if (ev.key === "Enter") { ev.preventDefault(); if (!t.editing) stepSearch(t, ev.shiftKey ? -1 : 1); }
          if (ev.key === "Escape" && t.search.value) { ev.stopPropagation(); t.search.value = ""; applySearch(t); }
        });
        t.area.addEventListener("input", () => {
          const dirty = t.area.value !== t.original;
          showProblem(t, "");
          if (dirty !== t.dirty) { t.dirty = dirty; renderTabs(); }
          renderViewerActions(t);
          applySearch(t, true);
        });
        addTab(t);
        renderViewer(t);
        loadFile(t);
      }
      activate(id);
    }

    async function loadFile(t) {
      t.loading = true;
      try {
        const text = await readBlob(fileId(t.name));
        if (text === null) throw new Error("it no longer exists");
        t.text = text;
        t.loaded = true;
        t.loadError = "";
        t.version = version(t.name);
      } catch (e) { t.loadError = `Cannot read ${t.name}: ${e.message}`; }
      t.loading = false;
      if (!s.destroyed && tab(t.id) && !t.editing) renderViewer(t);
    }

    function showProblem(t, text) {
      t.problem.textContent = text;
      t.problem.hidden = !text;
    }

    function renderViewerActions(t) {
      t.edit.hidden = t.editing || locked() || !t.loaded || !ChatLogic.validFileName(t.name);
      t.cancel.hidden = t.save.hidden = !t.editing;
      t.save.disabled = !t.dirty || t.saving;
      t.save.title = t.saving ? "Saving the file" : t.dirty ? `Stores ${t.name} and shows it again` : "Needs a change to save";
    }

    // renderViewer shows the file: Markdown rendered (diagrams drawn), JSON pretty-printed, plain
    // text as prose; or, while editing, the text in an editor.
    function renderViewer(t) {
      renderViewerActions(t);
      t.area.hidden = !t.editing;
      t.body.hidden = t.editing;
      if (!t.editing) {
        const kind = ChatLogic.fileKind(t.name);
        if (t.loadError) fill(t.body, errorState(t.loadError));
        else if (!t.loaded) fill(t.body);
        else if (kind === "markdown") fill(t.body, h("div", { className: "file-block markdown" }, renderRich(t.text, () => applySearch(t, true))));
        else if (kind === "json") {
          const shown = ChatLogic.prettyJSON(t.text);
          fill(t.body, shown.ok ? null : h("p", { className: "error-line" }, `Invalid JSON: ${shown.error}`), h("div", { className: "file-block code" }, shown.text));
        } else if (ChatLogic.codeFile(t.name)) fill(t.body, h("div", { className: "file-block code source" }, t.text)); // preformatted, in mono, scrolling sideways
        else fill(t.body, h("div", { className: "file-block prose" }, t.text));
      }
      applySearch(t, true);
    }

    // applySearch marks every match of the search text. In the rendered view the matches are
    // highlighted where they are shown; in the editor they are counted and selected one by one.
    // keep holds the current match where it was (a redraw); otherwise the first match is shown.
    function applySearch(t, keep = false) {
      for (const mark of t.body.querySelectorAll("mark.search-hit")) mark.replaceWith(mark.textContent);
      t.body.normalize();
      t.marks = [];
      t.offsets = [];
      const query = t.search.value;
      if (query && t.editing) t.offsets = ChatLogic.findMatches(t.area.value, query);
      else if (query) {
        const walker = document.createTreeWalker(t.body, NodeFilter.SHOW_TEXT);
        const nodes = [];
        while (walker.nextNode()) if (!walker.currentNode.parentElement?.closest("svg, .mermaid-diagram")) nodes.push(walker.currentNode); // a diagram is not text to mark
        for (const node of nodes) {
          const hits = ChatLogic.findMatches(node.data, query);
          if (!hits.length) continue;
          const parts = document.createDocumentFragment();
          let at = 0;
          for (const hit of hits) {
            const mark = h("mark", { className: "search-hit" }, node.data.slice(hit, hit + query.length));
            parts.append(node.data.slice(at, hit), mark);
            t.marks.push(mark);
            at = hit + query.length;
          }
          parts.append(node.data.slice(at));
          node.replaceWith(parts);
        }
      }
      const count = t.editing ? t.offsets.length : t.marks.length;
      t.current = count ? (keep && t.current >= 0 ? Math.min(t.current, count - 1) : 0) : -1;
      showMatch(t, !keep && !t.editing);
    }

    function showMatch(t, scroll) {
      const count = t.editing ? t.offsets.length : t.marks.length;
      t.count.textContent = !t.search.value ? "" : count ? `${t.current + 1} of ${count}` : "No matches";
      for (const b of [t.prev, t.next]) {
        b.disabled = !count;
        b.title = count ? b.getAttribute("aria-label") : "Needs a search with a match";
      }
      t.marks.forEach((mark, i) => mark.classList.toggle("current", i === t.current));
      if (scroll && t.marks[t.current]) t.marks[t.current].scrollIntoView({ block: "center" });
    }

    function stepSearch(t, delta) {
      const count = t.editing ? t.offsets.length : t.marks.length;
      if (!count) return;
      t.current = ChatLogic.stepMatch(t.current, count, delta);
      showMatch(t, true);
      if (!t.editing) return;
      // In the editor the match is selected, and the editor scrolls to its line.
      const at = t.offsets[t.current];
      t.area.focus();
      t.area.setSelectionRange(at, at + t.search.value.length);
      const line = t.area.value.slice(0, at).split("\n").length - 1;
      const lineHeight = parseFloat(getComputedStyle(t.area).lineHeight) || 20;
      t.area.scrollTop = Math.max(0, line * lineHeight - t.area.clientHeight / 2);
    }

    function startEdit(t) {
      if (locked() || !t.loaded) return;
      const shown = ChatLogic.fileKind(t.name) === "json" ? ChatLogic.prettyJSON(t.text) : null;
      t.original = shown?.ok ? shown.text : t.text;
      t.area.value = t.original;
      t.area.classList.toggle("code", ChatLogic.fileKind(t.name) === "json");
      t.editing = true;
      t.dirty = false;
      showProblem(t, "");
      renderViewer(t);
      t.area.focus();
    }

    async function cancelEdit(t) {
      if (t.dirty && !(await confirmAction("Discard unsaved edits?", "Discard"))) return;
      t.editing = false;
      t.dirty = false;
      showProblem(t, "");
      renderTabs();
      renderViewer(t);
      if (t.version !== version(t.name)) loadFile(t); // it changed while the editor was open
    }

    // saveFile writes the editor's text back and returns to the rendered view. JSON must parse;
    // a file that changed since it was opened (the agent wrote it) is replaced only after a question.
    async function saveFile(t) {
      if (t.saving || !t.editing) return;
      if (locked()) { setStatus("Not saved: the session is read-only", "error"); return; }
      const text = t.area.value;
      if (ChatLogic.fileKind(t.name) === "json") {
        const problem = ChatLogic.jsonProblem(text);
        if (problem) { showProblem(t, `Invalid JSON: ${problem}`); t.area.focus(); return; }
      }
      t.saving = true;
      renderViewerActions(t);
      try {
        await refreshFilesQuietly();
        if (version(t.name) && t.version && version(t.name) !== t.version && !(await confirmAction(`Replace the newer ${t.name}?`, "Replace"))) return;
        const res = await fetch(`/api/blobs/blobs/${fileId(t.name)}`, { method: "PUT", headers: writeHeaders(), body: text });
        if (!res.ok) throw new Error(errorMessage(await res.text(), res));
        t.text = text;
        t.editing = false;
        t.dirty = false;
        setSaveState("Saved");
        await refreshFilesQuietly();
        t.version = version(t.name);
        renderFiles();
      } catch (e) {
        setStatus(`Not saved: ${e.message}`, "error");
      } finally {
        t.saving = false;
        if (!s.destroyed) { renderTabs(); renderViewer(t); }
      }
    }

    // refreshFilesQuietly updates the listing without touching the viewers (a save is under way).
    async function refreshFilesQuietly() {
      try {
        s.files = (await listObjects(`${session}/files`)).map((x) => ({ name: x.id.split("/").pop(), size: x.size, created: x.created }))
          .sort((a, b) => a.name.localeCompare(b.name, undefined, { numeric: true, sensitivity: "base" }));
      } catch { /* the save itself reports a blobstore that cannot be reached */ }
    }

    // ---- Complete ----

    function renderComplete() {
      const reason = !creator() ? "Needs your name in the top bar" : !s.files.length ? "Needs a file in the session" : "";
      complete.disabled = !!reason;
      complete.title = reason || "Chooses the file that becomes the output of this step and lets the run continue";
    }

    function openComplete() {
      if (locked()) return;
      if (!creator()) { setStatus("Not completed: no name in the top bar", "error"); return; }
      const unsaved = viewers().find((t) => t.dirty);
      if (unsaved) { setStatus(`Not completed: ${unsaved.name} has unsaved edits`, "error"); return; }
      const open = tab(s.active)?.kind === "file" ? tab(s.active).name : "";
      let chosen = s.files.some((f) => f.name === open) ? open : s.files[0]?.name || "";
      let sending = false;
      const list = h("ul", { className: "pick-list", role: "radiogroup", "aria-label": "Output file" });
      const go = h("button", { type: "button", className: "primary" }, "Complete");
      const render = () => {
        list.replaceChildren(...s.files.map((f) => {
          const li = h("li", { role: "radio", tabIndex: 0, "aria-checked": String(f.name === chosen) }, h("span", { className: "grow" }, f.name));
          const pick = () => { chosen = f.name; render(); };
          li.addEventListener("click", pick);
          li.addEventListener("keydown", (ev) => { if (ev.key === "Enter" || ev.key === " ") { ev.preventDefault(); pick(); } });
          return li;
        }));
        go.disabled = !chosen || !creator() || sending;
        go.title = !creator() ? "Needs your name in the top bar" : !chosen ? "Needs a file" : `Submits ${chosen} as the output of ${stepTitle(o.step, stepPass(o.session || "").pass)}`;
      };
      const dialog = openDialog({
        title: "Complete session",
        body: h("div", {}, h("label", {}, "Output file"), list),
        footer: [h("button", { type: "button", onclick: () => dialog.close() }, "Cancel"), go],
      });
      go.onclick = async () => {
        if (sending || !chosen || !creator()) return; // a second click or Enter submits nothing twice
        sending = true;
        render();
        try {
          await api("POST", "/api/orchestrator/sessions/complete", { workflow_id: o.workflowId, step: o.step, file: chosen, completed_by: creator() });
        } catch (e) {
          sending = false;
          render();
          setStatus(`Not completed: ${e.message}`, "error");
          return;
        }
        dialog.close();
        setSaveState("Completed");
        s.completing = { file: chosen, until: Date.now() + 30000 };
        applyLock();
        o.onComplete?.(chosen);
        confirmCompletion();
      };
      render();
    }

    // The orchestrator answers before the run has taken the file. The session is read-only at
    // once and waits for completed.json; if that never appears the step is still pending
    // (copying the file failed), so the session opens again and says so.
    async function confirmCompletion() {
      while (!s.destroyed && s.completing) {
        if (await checkCompleted()) return;
        if (Date.now() > s.completing.until) {
          const { file } = s.completing;
          s.completing = null;
          applyLock();
          if (!s.closed) setStatus(`Not completed: the run did not take ${file}`, "error"); // a step that left the waiting list was taken
          return;
        }
        await new Promise((resolve) => setTimeout(resolve, 1500));
      }
    }

    // checkCompleted reads completed.json; its presence makes the session read-only.
    async function checkCompleted() {
      if (s.completed) return true;
      if (o.workspaceOnly) return false; // a team's workspace has no completion record
      let record = null;
      try { record = await readJSON(`${session}/completed.json`); } catch { return false; }
      if (!record || s.destroyed) return false;
      s.completed = record;
      s.completing = null;
      applyLock();
      return true;
    }

    // ---- Loading ----

    // start restores the session: its completion record, its files, and one chat tab per
    // stored chat log, oldest first. A session nobody chatted in yet opens with the main agent.
    async function start() {
      if (s.starting || s.started) return;
      s.starting = true;
      try {
        const [objects] = await Promise.all([o.workspaceOnly ? [] : listObjects(`${session}/chats`), checkCompleted(), refreshFiles()]);
        const ids = objects.map((x) => /\/([^/]+)\.json$/.exec(x.id)?.[1] || "").filter((id) => ChatLogic.validTabId(id));
        const logs = (await pool(ids, 4, async (id) => {
          try { return { id, log: await readJSON(`${session}/chats/${id}.json`) }; } catch { return { id, log: null }; }
        })).filter((x) => x.log && Array.isArray(x.log.messages))
          .sort((a, b) => String(a.log.created || "").localeCompare(String(b.log.created || "")) || a.id.localeCompare(b.id));
        if (s.destroyed) return;
        for (const { id, log } of logs) if (!tab(id)) addChat(id, log.agent || o.agent || "", log.messages, true);
        if (!chats().length && !locked() && agents.length) addChat(ChatLogic.newTabId(agents[0]), agents[0]);
        s.started = true;
        s.loadError = "";
        applyLock();
        activate(s.active || s.tabs[0]?.id || "");
      } catch (e) {
        if (s.destroyed) return;
        s.loadError = `Cannot load the session: ${e.message}`;
        renderEmpty();
      } finally { s.starting = false; }
    }

    // While shown, the session follows the blobstore: files the agent or someone else wrote,
    // and a completion recorded elsewhere. Nothing is redrawn unless it changed.
    async function refresh() {
      if (s.destroyed) return;
      if (!s.started) { await start(); return; }
      await Promise.all([refreshFiles(), s.completing ? null : checkCompleted()]);
    }
    const timer = setInterval(() => {
      if (document.hidden || main.offsetParent === null || !menu.hidden) return;
      refresh();
    }, REFRESH_MS);
    const onCreator = () => renderComplete();
    document.addEventListener("creatorchange", onCreator);

    renderTabs();
    applyLock();
    renderFiles(true);
    start();

    return {
      session,
      // dirty reports unsaved file edits; the page asks before it replaces the session.
      dirty: () => viewers().some((t) => t.dirty),
      // setClosed tells the session whether its run still waits at the step.
      setClosed(closed) {
        if (s.closed === !!closed) return;
        s.closed = !!closed;
        applyLock();
        if (s.closed) checkCompleted();
      },
      refresh,
      // For the team view: a tab of its own, the open tab, a file by name, and a new status chip.
      openView(id, name, pane) {
        if (!tab(id)) { pane.hidden = true; addTab({ id, kind: "view", name, pane }); }
        activate(id);
      },
      activeTab: () => s.active,
      openFile,
      renderMeta,
      renderEmpty,
      destroy() {
        s.destroyed = true;
        clearInterval(timer);
        document.removeEventListener("creatorchange", onCreator);
        main.classList.remove("chat-main");
        filesBox.classList.remove("chat-files");
        main.replaceChildren();
        filesBox.replaceChildren();
        if (!o.filesContainer) container.replaceChildren();
      },
    };
  }

  return { mount, renderMarkdown: renderRich };
})();
