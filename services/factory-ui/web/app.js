"use strict";
// Shell of the factory UI: page routing, the header menu, the "Acting as" chip, the notice,
// dialogs, and the helpers every page uses. Pages register in `tabs`.

const $ = (id) => document.getElementById(id);
const CREATOR_KEY = "factory.creator";

// tabs maps a page name to { show(rest), hide() }. Each page file registers itself.
// rest is the part of the hash after the page name, such as "/runs/x" or "?trace=y".
const tabs = {};
let activeTab = null;

// Routable pages and their navigation metadata.
const PAGES = [
  { name: "intake", title: "Intake", icon: "connector", tip: "Triggers that start workflows from connector events" },
  { name: "workflows", title: "Workflows", icon: "workflow", tip: "Workflow graphs" },
  { name: "runs", title: "Runs", icon: "run", tip: "Every run, newest first" },
  { name: "devpods", title: "Dev Pods", icon: "cluster", tip: "The dev pods of runs: their files, commit log, and diff" },
  { name: "events", title: "Events", icon: "history", tip: "Workflow event history" },
  { name: "decisions", title: "Decisions", icon: "gate", tip: "Gates waiting for a person" },
  { name: "sessions", title: "Sessions", icon: "chat", tip: "Interactive sessions waiting for a person" },
  { name: "agents", title: "Agents", icon: "agent", tip: "Versioned agent definitions" },
  { name: "policies", title: "Policies", icon: "policy", tip: "Versioned policies" },
  { name: "skills", title: "Skills", icon: "skill", tip: "Versioned skills" },
  { name: "mcp-gateway", title: "MCP Gateway", icon: "gateway", tip: "MCP passthrough services" },
  { name: "tools", title: "Tools", icon: "gateway", tip: "Every tool the MCP services offer" },
  { name: "artifacts", title: "Blobstore", icon: "memory", tip: "Runs, tasks, templates, and artifacts" },
  { name: "overview", title: "Overview", icon: "metrics", tip: "The factory now: what waits for you, what runs, and the last 24 hours" },
  { name: "metrics", title: "Metrics", icon: "metrics", tip: "Factory costs, tokens, runtime, and quality metrics over a period" },
  { name: "evaluations", title: "Evals", icon: "evaluation", tip: "Evaluation sets and judges" },
  { name: "connectors", title: "Connectors", icon: "connector", tip: "Git repositories and dev pods" },
  { name: "cluster", title: "Cluster", icon: "cluster", tip: "Pods, logs, and services" },
  { name: "system-diagram", title: "System Diagram", icon: "diagram", tip: "Factory components and connections" },
];
// Pages without a menu item: opened from another page, whose menu item stays the active one.
const SUBPAGES = { teams: { name: "teams", title: "Team", menu: "runs" } };
Object.assign(SUBPAGES, { judges: { name: "judges", title: "Judges", menu: "evaluations" }, pricing: { name: "pricing", title: "Prices", menu: "metrics" } });

// Top-level navigation groups and their destination pages.
const NAV_GROUPS = [
  { name: "Main", icon: "metrics", pages: ["overview", "metrics", "cluster", "system-diagram"] },
  { name: "Factory", icon: "workflow", pages: ["intake", "workflows", "agents", "runs", "devpods", "evaluations", "events"] },
  { name: "Tasks", icon: "gate", pages: ["decisions", "sessions"] },
  { name: "Connectors", icon: "connector", pages: ["connectors", "mcp-gateway", "tools"] },
  { name: "Context", icon: "memory", pages: ["policies", "skills", "artifacts"], planned: [{ before: "artifacts", title: "Memory", note: "Planned feature", icon: "memory" }] },
];

// Icons: 16px, 1.5px stroke, currentColor. Static markup only.
const ICONS = {
  metrics: '<path d="M2 2v12h12M5 11V8M9 11V5M13 11V2"/>',
  diagram: '<rect x="2" y="3" width="4" height="4" rx="1"/><rect x="10" y="3" width="4" height="4" rx="1"/><rect x="6" y="10" width="4" height="4" rx="1"/><path d="M6 5h4M4 7v2l4 1M12 7v2l-4 1"/>',
  workflow: '<rect x="2" y="2" width="5" height="4" rx="1"/><rect x="9" y="10" width="5" height="4" rx="1"/><path d="M4.5 6v2.5a1.5 1.5 0 0 0 1.5 1.5h3"/>',
  run: '<circle cx="8" cy="8" r="6"/><path d="M6.5 5.5v5l4-2.5z"/>',
  evaluation: '<path d="M3 3h10v10H3z"/><path d="M5.5 7.5l1.5 1.5 3.5-4M5.5 11h5"/>',
  gate: '<path d="M3 13V5l5-3 5 3v8"/><path d="M6 13V9h4v4"/>',
  agent: '<rect x="3" y="5" width="10" height="8" rx="2"/><path d="M8 2v3M6 9h.01M10 9h.01"/>',
  policy: '<path d="M8 2l5 2v4c0 3-2.2 5-5 6-2.8-1-5-3-5-6V4z"/>',
  skill: '<path d="M8 2l6 3-6 3-6-3zM2 8l6 3 6-3M2 11l6 3 6-3"/>',
  gateway: '<path d="M2 4h4M10 4h4M6 2v4M10 2v4M4 12h8M8 6v6"/>',
  memory: '<ellipse cx="8" cy="4" rx="5" ry="2"/><path d="M3 4v8c0 1.1 2.2 2 5 2s5-.9 5-2V4M3 8c0 1.1 2.2 2 5 2s5-.9 5-2"/>',
  connector: '<path d="M6 2v3M10 2v3M4.5 5h7v3a3.5 3.5 0 0 1-7 0zM8 11.5V14"/>',
  download: '<path d="M8 3v8M5 8l3 3 3-3M3 13h10"/>',
  cluster: '<rect x="2" y="2" width="12" height="5" rx="1"/><rect x="2" y="9" width="12" height="5" rx="1"/><path d="M5 4.5h.01M5 11.5h.01"/>',
  plus: '<path d="M8 3v10M3 8h10"/>',
  more: '<path d="M3.5 8h.01M8 8h.01M12.5 8h.01" stroke-width="2.5" stroke-linecap="round"/>',
  person: '<circle cx="8" cy="5.5" r="2.5"/><path d="M3 14c.6-2.6 2.6-4 5-4s4.4 1.4 5 4"/>',
  copy: '<rect x="5" y="5" width="8" height="9" rx="1.5"/><path d="M3 11V3.5A1.5 1.5 0 0 1 4.5 2H10"/>',
  triangle: '<path d="M5.5 3.5l5.5 4.5-5.5 4.5z" fill="currentColor" stroke="none"/>',
  upload: '<path d="M8 11V3M5 6l3-3 3 3M3 13h10"/>',
  folder: '<path d="M2 5a1 1 0 0 1 1-1h3l1.5 1.5H13a1 1 0 0 1 1 1V12a1 1 0 0 1-1 1H3a1 1 0 0 1-1-1z"/>',
  panel: '<rect x="2" y="3" width="12" height="10" rx="1.5"/><path d="M6 3v10"/>',
  panelRight: '<rect x="2" y="3" width="12" height="10" rx="1.5"/><path d="M10 3v10"/>',
  history: '<circle cx="8" cy="8" r="6"/><path d="M8 4.5V8l2.5 1.5"/>',
  refresh: '<path d="M13 6a5 5 0 1 0 .1 3M13 2v4H9"/>',
  edit: '<path d="M10.5 2.5l3 3-7.5 7.5-3.5.5.5-3.5zM9 4l3 3"/>',
  step: '<rect x="3" y="3" width="10" height="10" rx="2"/>',
  chat: '<path d="M3 3h10a1 1 0 0 1 1 1v6a1 1 0 0 1-1 1H7.5l-3 2.5V11H3a1 1 0 0 1-1-1V4a1 1 0 0 1 1-1z"/>',
  up: '<path d="M4 10l4-4 4 4"/>',
  down: '<path d="M4 6l4 4 4-4"/>',
  close: '<path d="M4.5 4.5l7 7M11.5 4.5l-7 7"/>',
  expand: '<path d="M9.5 2.5h4v4M6.5 13.5h-4v-4M13.5 2.5L9 7M2.5 13.5L7 9"/>',
};

// icon returns an inline SVG element for a name in ICONS.
function icon(name, size = 16) {
  const span = document.createElement("span");
  span.className = "icon";
  span.setAttribute("aria-hidden", "true");
  span.innerHTML = `<svg width="${size}" height="${size}" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round">${ICONS[name] || ""}</svg>`;
  return span;
}

// iconButton is a square button showing only an icon; label names it for tooltips and screen readers.
function iconButton(name, label, onclick, extra = {}) {
  const b = h("button", { type: "button", className: "icon-button", "aria-label": label, title: label, onclick, ...extra });
  b.append(icon(name));
  return b;
}

// notify shows one short line at the bottom centre: a confirmation fades out after 2 s,
// an error stays for 6 s in red. There are no other notifications.
function notify(text, kind = "") {
  const box = $("saved-notice");
  box.textContent = text;
  box.classList.toggle("error", kind === "error");
  box.setAttribute("role", kind === "error" ? "alert" : "status");
  box.classList.remove("show", "show-long");
  void box.offsetWidth; // restart the animation
  box.classList.add(kind === "error" ? "show-long" : "show");
}

// setStatus reports a result. An Undo action is kept for Cmd/Ctrl+Z instead of a button,
// on the page where it happened and until the next edit there (dropUndo).
let undoAction = null;
function setStatus(text, kind = "", action) {
  if (!text) return;
  if (action && /undo/i.test(action.label)) undoAction = { run: action.run, tab: activeTab };
  notify(text, kind);
}

function dropUndo() { undoAction = null; }

function setSaveState(text = "Saved") { notify(text); }

document.addEventListener("keydown", (ev) => {
  if (!(ev.metaKey || ev.ctrlKey) || ev.shiftKey || ev.key.toLowerCase() !== "z" || undoAction?.tab !== activeTab) return;
  if (ev.target.closest?.("input, textarea, select, [contenteditable]")) return;
  ev.preventDefault();
  const { run } = undoAction;
  undoAction = null;
  run();
});

// The state vocabulary: a filled dot plus a word, with fixed colors.
// blocked is a run an agent ended with report_blocked: a person has to clear the way, so its
// chip is in the waiting colour family, never the failed one.
const STATE_WORDS = { running: "Running", done: "Completed", failed: "Failed", waiting: "Waiting", blocked: "Blocked", idle: "Idle" };

function statusChip(state, word = STATE_WORDS[state], title = "") {
  // A chip has a tooltip only when it adds a reason; the word is already visible.
  return h("span", { className: `chip chip-${state}`, title: title || undefined }, h("span", { className: "dot", "aria-hidden": "true" }), word);
}

// runState maps a Temporal run status to a state; a running run with a pending gate is waiting.
function runState(status, waiting = false) {
  if (!status) return "idle";
  if (status === "Running") return waiting ? "waiting" : "running";
  if (status === "Completed") return "done";
  if (status === "Blocked") return "blocked";
  return "failed";
}

function runStateWord(status, waiting) {
  const state = runState(status, waiting);
  if (state === "failed" && status && status !== "Failed") return status; // Terminated, TimedOut, Canceled
  return STATE_WORDS[state];
}
// api calls a backend through the factory UI's proxy and returns the parsed JSON body.
async function api(method, path, body, headers = {}) {
  const init = { method, headers: { ...headers } };
  if (body !== undefined) {
    init.headers["Content-Type"] = typeof body === "string" ? "text/plain" : "application/json";
    init.body = typeof body === "string" ? body : JSON.stringify(body);
  }
  const res = await fetch(path, init);
  const text = await res.text();
  if (!res.ok) {
    const error = new Error(errorMessage(text, res));
    error.status = res.status;
    throw error;
  }
  return text ? JSON.parse(text) : {};
}

// errorMessage extracts {"error": "..."} from a response body, or falls back to its text.
function errorMessage(text, res) {
  try {
    const data = JSON.parse(text);
    if (data && data.error) return data.error;
  } catch { /* not JSON */ }
  return text.trim() || `${res.status} ${res.statusText}`;
}

// loadBlob fetches a blobstore object with the metadata the blobstore returns in headers.
async function loadBlob(id) {
  const res = await fetch(`/api/blobs/blobs/${id}`);
  const text = await res.text();
  if (!res.ok) throw new Error(errorMessage(text, res));
  return {
    id,
    text,
    size: Number(res.headers.get("Content-Length")) || new Blob([text]).size,
    creator: res.headers.get("X-Creator") || "",
    trace: res.headers.get("X-Trace-Id") || "",
    created: res.headers.get("X-Created") || "",
  };
}

// downloadBlob saves a blobstore object as a file with the given name. The bytes are handed to
// the browser as a download of no particular type, so an HTML file is stored, never rendered
// inside the factory.
async function downloadBlob(id, name) {
  try {
    const res = await fetch(`/api/blobs/blobs/${id}`);
    if (!res.ok) throw new Error(errorMessage(await res.text(), res));
    const url = URL.createObjectURL(new Blob([await res.arrayBuffer()], { type: "application/octet-stream" }));
    const link = h("a", { href: url, download: name, hidden: true });
    document.body.append(link);
    link.click();
    link.remove();
    setTimeout(() => URL.revokeObjectURL(url), 10000);
  } catch (e) { setStatus(`Cannot download ${name}: ${e.message}`, "error"); }
}

// pretty indents text that parses as JSON and returns any other text unchanged.
function pretty(text) {
  try { return JSON.stringify(JSON.parse(text), null, 2); } catch { /* not one JSON document */ }
  // Line-delimited JSON, such as structured logs: pretty-print each JSON line.
  if (!/^\s*[{[]/m.test(text)) return text;
  return text.split("\n").map((line) => {
    const t = line.trim();
    if (!t.startsWith("{") && !t.startsWith("[")) return line;
    try { return JSON.stringify(JSON.parse(t), null, 2); } catch { return line; }
  }).join("\n");
}

// JSON stays in the code view and never gets a Markdown toggle.
function isJSONDocument(text) {
  try { JSON.parse(text); return true; } catch { return false; }
}

// Markdown is built as DOM nodes rather than HTML. Raw HTML remains text, and links are limited
// to safe browser schemes.
function markdownInline(text) {
  const out = document.createDocumentFragment();
  // An underscore inside a word (list_session_files) is part of a name, never emphasis.
  const token = /(`[^`]+`|!\[[^\]]*\]\([^)]+\)|\[[^\]]+\]\([^)]+\)|\*\*[^*]+\*\*|(?<![\p{L}\p{N}_])__[^_]+__(?![\p{L}\p{N}_])|~~[^~]+~~|\*[^*]+\*|(?<![\p{L}\p{N}_])_[^_]+_(?![\p{L}\p{N}_]))/gu;
  let at = 0;
  for (const match of text.matchAll(token)) {
    if (match.index > at) out.append(text.slice(at, match.index));
    const value = match[0];
    if (value.startsWith("`")) out.append(h("code", {}, value.slice(1, -1)));
    else if (value.startsWith("![")) {
      const parts = value.match(/^!\[([^\]]*)\]\(([^)]+)\)$/);
      out.append(parts ? h("span", { className: "markdown-image" }, parts[1] || "Image", ` (${parts[2]})`) : value);
    } else if (value.startsWith("[")) {
      const parts = value.match(/^\[([^\]]+)\]\(([^)]+)\)$/);
      const href = parts?.[2]?.trim() || "";
      if (parts && /^(https?:|mailto:|#|\/)/i.test(href)) out.append(h("a", { href }, parts[1]));
      else out.append(parts?.[1] || value);
    } else if (value.startsWith("**") || value.startsWith("__")) out.append(h("strong", {}, value.slice(2, -2)));
    else if (value.startsWith("~~")) out.append(h("del", {}, value.slice(2, -2)));
    else out.append(h("em", {}, value.slice(1, -1)));
    at = match.index + value.length;
  }
  if (at < text.length) out.append(text.slice(at));
  return out;
}

function renderMarkdown(text) {
  const root = h("div", { className: "markdown-body" });
  const lines = String(text).replace(/\r\n?/g, "\n").split("\n");
  const tableDivider = (line) => /^\s*\|?\s*:?-{3,}:?\s*(\|\s*:?-{3,}:?\s*)+\|?\s*$/.test(line);
  const startsBlock = (line, next = "") => /^\s*(```|~~~|#{1,6}\s|[-*_](?:\s*[-*_]){2,}\s*$|>\s?|[-+*]\s+|\d+[.)]\s+)/.test(line)
    || (line.includes("|") && tableDivider(next));
  const cells = (line) => line.trim().replace(/^\||\|$/g, "").split("|").map((cell) => cell.trim());
  const listAt = (start, indent, ordered) => {
    const list = h(ordered ? "ol" : "ul");
    let at = start;
    while (at < lines.length) {
      const item = lines[at].match(/^(\s*)([-+*]|\d+[.)])\s+(.+)$/);
      if (!item || item[1].length !== indent || /^\d/.test(item[2]) !== ordered) break;
      const li = h("li", {}, markdownInline(item[3]));
      at++;
      const child = lines[at]?.match(/^(\s*)([-+*]|\d+[.)])\s+(.+)$/);
      if (child && child[1].length > indent) {
        const nested = listAt(at, child[1].length, /^\d/.test(child[2]));
        li.append(nested.node); at = nested.at;
      }
      list.append(li);
    }
    return { node: list, at };
  };
  for (let i = 0; i < lines.length;) {
    const line = lines[i];
    if (!line.trim()) { i++; continue; }
    const fence = line.match(/^\s*(```|~~~)([\w-]*)\s*$/);
    if (fence) {
      const code = [];
      const end = new RegExp(`^\\s*${fence[1]}\\s*$`);
      for (i++; i < lines.length && !end.test(lines[i]); i++) code.push(lines[i]);
      if (i < lines.length) i++;
      root.append(h("pre", {}, h("code", { className: fence[2] ? `language-${fence[2]}` : "" }, code.join("\n"))));
      continue;
    }
    const heading = line.match(/^\s*(#{1,6})\s+(.+?)\s*#*\s*$/);
    if (heading) { root.append(h(`h${heading[1].length}`, {}, markdownInline(heading[2]))); i++; continue; }
    if (/^\s*[-*_](?:\s*[-*_]){2,}\s*$/.test(line)) { root.append(h("hr")); i++; continue; }
    if (/^\s*>/.test(line)) {
      const quote = [];
      while (i < lines.length && /^\s*>/.test(lines[i])) quote.push(lines[i++].replace(/^\s*>\s?/, ""));
      root.append(h("blockquote", {}, renderMarkdown(quote.join("\n"))));
      continue;
    }
    if (line.includes("|") && tableDivider(lines[i + 1] || "")) {
      const headings = cells(line), dividers = cells(lines[i + 1]);
      const align = dividers.map((cell) => /^:-+:$/.test(cell) ? "center" : /-+:$/.test(cell) ? "right" : "left");
      const table = h("table", {}, h("thead", {}, h("tr", {}, ...headings.map((cell, n) => h("th", { style: `text-align:${align[n] || "left"}` }, markdownInline(cell))))));
      const tbody = h("tbody");
      for (i += 2; i < lines.length && lines[i].includes("|") && lines[i].trim(); i++) {
        tbody.append(h("tr", {}, ...cells(lines[i]).map((cell, n) => h("td", { style: `text-align:${align[n] || "left"}` }, markdownInline(cell)))));
      }
      table.append(tbody); root.append(table); continue;
    }
    const item = line.match(/^(\s*)([-+*]|\d+[.)])\s+(.+)$/);
    if (item) {
      const list = listAt(i, item[1].length, /^\d/.test(item[2]));
      root.append(list.node); i = list.at;
      continue;
    }
    const paragraph = [line.trim()];
    for (i++; i < lines.length && lines[i].trim() && !startsBlock(lines[i], lines[i + 1]); i++) paragraph.push(lines[i].trim());
    root.append(h("p", {}, markdownInline(paragraph.join(" "))));
  }
  return root;
}

// The fixed-width control names the view it opens. One persisted preference applies to every
// non-JSON text view; Markdown is the default until the operator explicitly chooses text.
const TEXT_VIEW_MODE_KEY = "factory.text-view-mode";
let textViewMode = (() => {
  try { return localStorage.getItem(TEXT_VIEW_MODE_KEY) === "text" ? "text" : "markdown"; }
  catch { return "markdown"; }
})();

function setTextViewMode(mode) {
  textViewMode = mode === "text" ? "text" : "markdown";
  try { localStorage.setItem(TEXT_VIEW_MODE_KEY, textViewMode); } catch { /* storage unavailable */ }
  // Render callbacks live on their own controls. Querying the current DOM avoids retaining
  // viewers after navigation while keeping every visible toggle synchronized.
  for (const toggle of document.querySelectorAll(".text-format-toggle")) toggle.renderTextView?.(true);
}

function textView(text = "", { detectJSON = true, className = "file-block" } = {}) {
  const body = h("div"), toggle = h("button", { type: "button", className: "text-format-toggle" });
  let value = String(text), modeHandler = () => {};
  const render = (notify = false) => {
    const json = detectJSON && isJSONDocument(value);
    const markdown = !json && textViewMode === "markdown";
    toggle.hidden = json;
    toggle.textContent = markdown ? "TXT" : "MD";
    toggle.title = markdown ? "Switch to plain text" : "Switch to Markdown";
    toggle.setAttribute("aria-label", toggle.title);
    body.className = `${className} ${json ? "code" : markdown ? "markdown" : "prose"}`;
    body.replaceChildren(markdown ? renderMarkdown(value) : (json ? pretty(value) : value));
    if (notify) modeHandler(markdown);
  };
  toggle.renderTextView = render;
  toggle.onclick = () => setTextViewMode(textViewMode === "markdown" ? "text" : "markdown");
  render();
  return {
    body, toggle,
    setText(next) { value = String(next); render(); },
    onMode(handler) { modeHandler = handler; modeHandler(textViewMode === "markdown"); },
  };
}

// A registry Markdown preview hides, but never replaces, the live textarea. Unsaved prompt and
// policy drafts therefore survive both directions of the toggle.
function markdownEditorSection(label, area, resize = () => {}) {
  const preview = textView(area.value, { detectJSON: false });
  const edit = iconButton("edit", `Edit ${label}`, () => {
    setTextViewMode("text");
    requestAnimationFrame(() => { resize(); area.focus(); });
  });
  preview.body.hidden = true;
  preview.onMode((markdown) => {
    if (markdown) preview.setText(area.value);
    area.hidden = markdown;
    preview.body.hidden = !markdown;
    edit.hidden = !markdown || area.readOnly;
    if (!markdown) resize();
  });
  area.addEventListener("input", () => preview.setText(area.value));
  const section = h("section", { className: "section" },
    h("div", { className: "file-head" }, h("h3", { className: "section-title" }, label),
      h("div", { className: "file-head-actions" }, edit, preview.toggle)),
    area, preview.body);
  section.editorArea = area;
  section.previewBody = preview.body;
  return section;
}

// Blobstore evidence links are limited to valid blob IDs. They never turn arbitrary URL-looking
// text into a link, and they always open the corresponding object on the Blobstore page.
const blobPartPattern = "[A-Za-z0-9_-](?:[A-Za-z0-9._-]*[A-Za-z0-9_-])?";
const blobIDPattern = new RegExp("(?:^|[\\s\"'(])((?:runs|agents|policies|skills|tasks|templates|evaluation-sets|mcp-gateway)/" + blobPartPattern + "(?:/" + blobPartPattern + ")*)", "g");
function validBlobID(id) {
  return String(id || "").length <= 512 && String(id || "").split("/").every((part) => /^[A-Za-z0-9_-][A-Za-z0-9._-]*$/.test(part));
}
function memoryObjectHash(id, trace = "") {
  if (!validBlobID(id)) return "";
  const run = /^runs\/([^/]+)\//.exec(id)?.[1] || (id.startsWith("runs/") ? trace : "");
  return run ? `#artifacts?trace=${encodeURIComponent(run)}&object=${encodeURIComponent(id)}` : `#artifacts/${id}`;
}
function memoryObjectLink(id, label = id, trace = "") {
  const href = memoryObjectHash(id, trace);
  return href ? h("a", { href, title: id }, label) : null;
}
function blobReferences(text) {
  const ids = new Set();
  for (const match of String(text).matchAll(blobIDPattern)) if (validBlobID(match[1])) ids.add(match[1]);
  return [...ids];
}
// Prompt provenance reads as a small table: what was used (label) and where to open it (links).
function promptEvidenceLinks(record) {
  const evidence = record?.prompt_evidence;
  if (!evidence) return null;
  const rows = [];
  const row = (label, ...pairs) => {
    const links = pairs.map(([text, id]) => memoryObjectLink(id, text, record.run_id)).filter(Boolean);
    if (links.length) rows.push(h("dt", {}, label), h("dd", {}, ...links.flatMap((link, i) => i ? [" · ", link] : [link])));
  };
  row("Source prompt", ["Recorded", evidence.source_prompt_artifact], ["Registry", evidence.agent_source_artifact]);
  row("Effective prompt", ["Recorded", evidence.effective_prompt_artifact]);
  for (const policy of evidence.policies || []) row(`Policy ${policy.id} ${policy.version}`, ["Recorded", policy.snapshot_artifact], ["Registry", policy.source_artifact]);
  for (const skill of evidence.skills || []) row(`Skill ${skill.name} ${skill.version}`, ["Registry", skill.source_artifact]);
  return rows.length ? h("dl", { className: "prompt-evidence-links" }, ...rows) : null;
}
function blobReferencePanel(text, excluded = []) {
  const skip = new Set(excluded.filter(Boolean));
  const links = blobReferences(text).filter((id) => !skip.has(id)).map((id) => h("li", {}, memoryObjectLink(id)));
  return links.length ? h("section", { className: "memory-reference-links" }, h("h3", { className: "section-title" }, "Blobstore references"), h("ul", {}, ...links)) : null;
}

// Times are shown relative ("3 min ago") with the full timestamp in a tooltip; timeEl
// elements refresh themselves every 30 seconds.
function formatTime(iso) {
  const d = new Date(iso);
  if (!iso || isNaN(d)) return iso || "";
  const s = (Date.now() - d) / 1000;
  if (s < 45) return "just now";
  if (s < 3600) return `${Math.max(1, Math.round(s / 60))} min ago`;
  if (s < 86400) return `${Math.round(s / 3600)} h ago`;
  if (s < 7 * 86400) return `${Math.round(s / 86400)} d ago`;
  return d.toLocaleDateString(undefined, { day: "numeric", month: "short", year: "numeric" });
}

// formatDuration is the time elapsed since iso, such as "4 min" or "2 h 5 min".
function formatDuration(iso) {
  const s = Math.max(0, (Date.now() - new Date(iso)) / 1000);
  if (isNaN(s)) return "";
  if (s < 60) return `${Math.round(s)} s`;
  if (s < 3600) return `${Math.floor(s / 60)} min`;
  if (s < 86400) return `${Math.floor(s / 3600)} h ${Math.floor((s % 3600) / 60)} min`;
  return `${Math.floor(s / 86400)} d ${Math.floor((s % 86400) / 3600)} h`;
}

const fullTime = (iso) => (iso && !isNaN(new Date(iso)) ? new Date(iso).toLocaleString() : "");

// timeEl shows a relative time ("ago") or an elapsed time ("for") with the full time as tooltip.
function timeEl(iso, kind = "ago") {
  return h("time", { dateTime: iso || "", title: fullTime(iso), dataset: { kind } }, kind === "for" ? formatDuration(iso) : formatTime(iso));
}

setInterval(() => {
  for (const t of document.querySelectorAll("time[data-kind]")) {
    t.textContent = t.dataset.kind === "for" ? formatDuration(t.dateTime) : formatTime(t.dateTime);
  }
}, 30000);

// titleCase shows an identifier such as "small-fix" or "step_1" as "Small Fix" / "Step 1"; a
// grouped name shows its groups before it ("ecc/ECC-plan" as "ECC / ECC Plan").
// The factory's acronyms stay in capitals ("ECC Plan", "LLM Gateway"), however the ID was typed.
const titleCase = (id) => String(id || "").split("/").map((part) => part.split(/[-_\s]+/).filter(Boolean).map((p) => (/^(ai|api|ci|cli|css|ecc|egm|html|http|id|json|llm|mcp|pr|qa|sql|ui|url|uuid|yaml)$/i.test(p) ? p.toUpperCase() : p[0].toUpperCase() + p.slice(1))).join(" ")).filter(Boolean).join(" / ");

// midTruncate shortens a long id in the middle, keeping its start and end readable.
function midTruncate(text, max = 40) {
  if (text.length <= max) return text;
  const head = Math.ceil((max - 1) / 2), tail = Math.floor((max - 1) / 2);
  return `${text.slice(0, head)}…${text.slice(text.length - tail)}`;
}

// idText shows an id, truncated in the middle, with the full id as tooltip.
function idText(id, max = 40) {
  return h("span", { className: "mono", title: id }, midTruncate(id, max));
}

// copyButton copies text to the clipboard; what names it for screen readers and the notice.
function copyButton(text, what) {
  return h("button", {
    className: "copy-button", type: "button", "aria-label": `Copy ${what}`, title: `Copy ${what}`,
    onclick: async (ev) => {
      ev.preventDefault();
      ev.stopPropagation();
      const value = typeof text === "function" ? text() : text;
      try {
        await navigator.clipboard.writeText(value);
      } catch {
        const area = h("textarea", { value, style: "position:fixed;opacity:0" });
        document.body.append(area);
        area.select();
        document.execCommand("copy");
        area.remove();
      }
      setSaveState("Copied");
    },
  }, icon("copy", 14));
}

// emptyPanel is the centered empty state of a whole panel: a title and at most one button.
function emptyPanel(title, action) {
  return h("div", { className: "empty-panel" },
    h("p", { className: "empty-title" }, title),
    action ? h("button", { type: "button", className: action.primary ? "primary" : "", onclick: action.run }, action.label) : null);
}

// errorState shows a server error in a panel, with a Retry button when retry is given.
function errorState(message, retry) {
  return h("div", { className: "error-state", role: "alert" },
    h("p", { className: "error-text" }, message),
    retry ? h("button", { onclick: retry }, "Retry") : null);
}

// errorPanel stands in for a panel that could not load, placed like an empty state. Retry only
// where the page does not refresh itself (the editors); live pages try again on their own.
function errorPanel(message, retry) {
  return h("div", { className: "empty-panel" }, errorState(message, retry));
}

// dataTable is the one table style: column headers with scope, numeric columns right-aligned.
// Text that may be cut off carries data-full: hovering it shows the full text as a tooltip,
// but only while it is cut off, so a tooltip never repeats what is visible.
document.addEventListener("mouseover", (ev) => {
  const el = ev.target.closest?.("[data-full]");
  // Only cut-off text gets its own tooltip; otherwise the row's tooltip, if any, shows.
  if (el) { if (el.scrollWidth > el.clientWidth + 1) el.title = el.dataset.full; else el.removeAttribute("title"); }
});

// The keyboard cannot hover: a row (or any control) that gets keyboard focus while text in it is
// cut off shows that text in full in a tooltip under it, which it is described by, until focus leaves.
const focusTip = h("div", { id: "focus-tip", className: "focus-tip", role: "tooltip", hidden: true });
// cutOffTexts lists the full texts of the cut-off [data-full] elements in el, el itself included.
function cutOffTexts(el) {
  const all = [...(el.matches?.("[data-full]") ? [el] : []), ...(el.querySelectorAll?.("[data-full]") || [])];
  return all.filter((e) => e.dataset.full && e.scrollWidth > e.clientWidth + 1).map((e) => e.dataset.full);
}
function hideFocusTip() {
  if (focusTip.hidden) return;
  focusTip.hidden = true;
  document.querySelector("[aria-describedby='focus-tip']")?.removeAttribute("aria-describedby");
}
document.addEventListener("focusin", (ev) => {
  hideFocusTip();
  const el = ev.target;
  if (!(el instanceof Element) || !el.matches(":focus-visible")) return;
  const texts = cutOffTexts(el);
  if (!texts.length) return;
  if (!focusTip.isConnected) document.body.append(focusTip);
  focusTip.replaceChildren(...texts.map((t) => h("div", {}, t)));
  focusTip.hidden = false;
  if (!el.hasAttribute("aria-describedby")) el.setAttribute("aria-describedby", "focus-tip");
  const r = el.getBoundingClientRect(), tip = focusTip.getBoundingClientRect();
  focusTip.style.left = `${Math.max(8, Math.min(r.left + 8, window.innerWidth - tip.width - 8))}px`;
  focusTip.style.top = `${r.bottom + tip.height + 12 > window.innerHeight ? r.top - tip.height - 4 : r.bottom + 4}px`;
});
document.addEventListener("focusout", hideFocusTip);
document.addEventListener("keydown", (ev) => { if (ev.key === "Escape") hideFocusTip(); });
document.addEventListener("scroll", hideFocusTip, true);

// dataTable builds a table; numeric columns align right, optional columns (cells marked "opt"
// too) give way when the middle column is narrow, and the name column (cells marked
// "name-cell") takes the rest of the width and truncates, its header included.
function dataTable(headings, rows, { numeric = [], optional = [], name = -1 } = {}) {
  return h("table", { className: "data-table" },
    h("thead", {}, h("tr", {}, ...headings.map((t, i) => h("th", { scope: "col", className: [numeric.includes(i) ? "num" : "", optional.includes(i) ? "opt" : "", i === name ? "name-cell" : ""].join(" ").trim(), dataset: i === name ? { full: t } : undefined }, t)))), // a name header can be cut off
    h("tbody", {}, ...rows));
}

// clickableRow makes a table row act like a button: click, Enter, and Space.
function clickableRow(tr, onActivate) {
  tr.classList.add("clickable");
  tr.tabIndex = 0;
  tr.addEventListener("click", onActivate);
  tr.addEventListener("contextmenu", (ev) => ev.preventDefault()); // no browser menu on rows
  tr.addEventListener("keydown", (ev) => {
    if (ev.key === "Enter" || ev.key === " ") { ev.preventDefault(); onActivate(ev); }
  });
  return tr;
}

// marks is the multi-selection of a table, beside its one selected row: Cmd/Ctrl+click or X on a
// focused row marks it (or unmarks it), Shift+click marks the rows from the last one marked, and
// Escape or a plain click clears them. The row menu then acts on every marked row at once.
function marks() {
  const set = new Set();
  let anchor = "";
  return {
    set,
    has: (id) => set.has(id),
    get size() { return set.size; },
    toggle(id) { if (set.has(id)) set.delete(id); else set.add(id); anchor = id; },
    // range marks the ids between the last one marked and id, in the order the table lists them.
    range(ids, id) {
      const from = ids.indexOf(anchor), to = ids.indexOf(id);
      if (from < 0 || to < 0) { this.toggle(id); return; }
      for (const x of ids.slice(Math.min(from, to), Math.max(from, to) + 1)) set.add(x);
      anchor = id;
    },
    clear() { const had = set.size > 0; set.clear(); anchor = ""; return had; },
    // keep drops the marks of rows no longer listed.
    keep(ids) { const listed = new Set(ids); for (const id of [...set]) if (!listed.has(id)) set.delete(id); },
  };
}

// markableRow is clickableRow for a table with marks: a modified click marks instead of opening,
// X marks the focused row, and Escape clears the marks. onMarks redraws the table.
function markableRow(tr, id, ids, rowMarks, onActivate, onMarks) {
  tr.classList.toggle("marked", rowMarks.has(id));
  if (rowMarks.has(id)) tr.firstChild?.append(h("span", { className: "sr-only" }, ", marked")); // read with the row
  clickableRow(tr, (ev) => {
    if (ev?.metaKey || ev?.ctrlKey) { rowMarks.toggle(id); onMarks(); return; }
    if (ev?.shiftKey && ev.type === "click") { rowMarks.range(ids(), id); onMarks(); return; }
    if (rowMarks.clear()) onMarks();
    onActivate(ev);
  });
  tr.addEventListener("keydown", (ev) => {
    if (ev.target !== tr || ev.metaKey || ev.ctrlKey || ev.altKey) return;
    if (ev.key === "x" || ev.key === "X") { ev.preventDefault(); rowMarks.toggle(id); onMarks(); }
    else if (ev.key === "Escape" && rowMarks.clear()) { ev.preventDefault(); onMarks(); }
  });
  return tr;
}

// trapFocus keeps Tab inside a dialog and returns a function that restores the earlier focus.
function trapFocus(container) {
  const before = document.activeElement;
  const onKey = (ev) => {
    if (ev.key !== "Tab") return;
    const items = [...container.querySelectorAll("a[href], button:not([disabled]), input:not([disabled]):not([type=hidden]), select, textarea, [tabindex='0']")]
      .filter((e) => e.offsetParent !== null);
    if (!items.length) return;
    const first = items[0], last = items[items.length - 1];
    if (ev.shiftKey && document.activeElement === first) { ev.preventDefault(); last.focus(); }
    else if (!ev.shiftKey && document.activeElement === last) { ev.preventDefault(); first.focus(); }
  };
  container.addEventListener("keydown", onKey);
  return () => {
    container.removeEventListener("keydown", onKey);
    if (before && before.isConnected) before.focus();
  };
}

function formatSize(n) {
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
  return `${(n / 1024 / 1024).toFixed(1)} MB`;
}

// h builds an element. Properties go on the element (className, onclick, ...);
// string children become text nodes, so content is never parsed as HTML.
function h(tag, props = {}, ...children) {
  const e = document.createElement(tag);
  for (const [k, v] of Object.entries(props)) {
    if (v === undefined || v === null) continue;
    if (k === "dataset") Object.assign(e.dataset, v);
    else if (k === "list") e.setAttribute(k, v); // input.list is a read-only DOM property
    else if (k in e) e[k] = v;
    else e.setAttribute(k, v);
  }
  for (const c of children.flat()) {
    if (c === null || c === undefined || c === false) continue;
    e.append(c instanceof Node ? c : String(c));
  }
  return e;
}

// fill replaces a parent's children like the native replaceChildren, but skips null, undefined,
// and false the way h() does: the native method would render them as the text "null".
function fill(parent, ...items) {
  parent.replaceChildren(...items.flat().filter((item) => item !== null && item !== undefined && item !== false));
}

// createTree renders a folder tree into a <ul> and returns its controller. Every tab's
// sidebar uses it. A node is { id, label, sub, title, mono, children, load }: a node
// with children (an array) or load (an async function returning nodes, called once
// when the folder is first opened) is a folder. Clicking a row selects it and opens it
// if it is a folder; the caret only opens or closes. Arrow keys move, open, and close.
function createTree(container, { onSelect = () => {}, onContext = null } = {}) {
  const t = { roots: [], expanded: new Set(), loaded: new Map(), selected: null, byId: new Map(), parent: new Map() };
  container.classList.add("tree");
  container.tabIndex = 0;
  container.setAttribute("role", "tree");
  const childrenOf = (n) => n.children || t.loaded.get(n.id) || null;
  const isFolder = (n) => !!(n.children || n.load);

  function render() {
    t.byId.clear();
    t.parent.clear();
    container.replaceChildren(...t.roots.map((n) => item(n, null)));
    container.querySelector(".tree-row.active")?.scrollIntoView({ block: "nearest" });
  }

  function item(n, parent) {
    t.byId.set(n.id, n);
    t.parent.set(n.id, parent);
    const kids = childrenOf(n);
    const open = t.expanded.has(n.id);
    // A folder keeps its triangle even when it turns out to hold no subfolders.
    const leaf = !isFolder(n);
    const row = h("div", {
      className: "tree-row" + (n.id === t.selected ? " active" : "") + (n.className ? " " + n.className : ""),
      title: n.title || "",
      role: "treeitem",
      "aria-selected": n.id === t.selected ? "true" : "false",
      "aria-expanded": isFolder(n) ? String(open) : undefined,
      // A click selects; a click on the open, selected folder folds it.
      onclick: () => (isFolder(n) && open && n.id === t.selected ? ctl.collapse(n.id) : ctl.select(n.id, true)),
      oncontextmenu: (ev) => { ev.preventDefault(); onContext?.(n, ev); }, // no browser menu on rows
    },
    h("span", { className: "caret" + (open ? " open" : "") + (leaf ? " leaf" : ""), onclick: (ev) => { ev.stopPropagation(); if (!leaf) ctl.toggle(n.id); } }, leaf ? null : icon("triangle", 12)),
    h("span", { className: "tree-label" + (n.mono ? " mono" : ""), dataset: n.full ? { full: n.full } : undefined }, n.label),
    n.sub ? h("span", { className: "tree-sub" }, n.sub) : null);
    const li = h("li", { role: "none" }, row);
    if (open && kids?.length) li.append(h("ul", { role: "group" }, ...kids.map((k) => item(k, n.id))));
    return li;
  }

  async function load(n) {
    if (!n.load || t.loaded.has(n.id)) return;
    try { t.loaded.set(n.id, await n.load()); } catch (e) { setStatus(`Cannot list ${n.label}: ${e.message}`, "error"); t.loaded.set(n.id, []); }
  }

  const ctl = {
    setRoots(nodes) { t.roots = nodes; render(); },
    get selected() { return t.selected; },
    node(id) { return t.byId.get(id); },
    // select marks a row; with notify (a click or key) it also opens a folder and calls onSelect.
    async select(id, notify = false) {
      t.selected = id;
      const n = t.byId.get(id);
      if (notify && n && isFolder(n) && !t.expanded.has(id)) { t.expanded.add(id); await load(n); }
      render();
      if (notify && n) onSelect(n);
    },
    async expand(id) {
      const n = t.byId.get(id);
      t.expanded.add(id);
      if (n) await load(n);
      render();
    },
    collapse(id) { t.expanded.delete(id); render(); },
    toggle(id) { return t.expanded.has(id) ? ctl.collapse(id) : ctl.expand(id); },
    // reveal opens each folder of a path of ids in turn, loading lazy folders on the way.
    async reveal(ids) {
      for (const id of ids) {
        const n = t.byId.get(id);
        if (!n) break;
        t.expanded.add(id);
        await load(n);
        render();
      }
    },
    // setChildren replaces the cached children of a lazy folder, such as after a refresh.
    setChildren(id, nodes) { t.loaded.set(id, nodes); render(); },
    invalidate() { t.loaded.clear(); },
  };

  container.addEventListener("keydown", (ev) => {
    const rows = [...container.querySelectorAll(".tree-row")];
    const i = rows.findIndex((r) => r.classList.contains("active"));
    const id = t.selected;
    if (ev.key === "ArrowDown" && i < rows.length - 1) rows[i + 1].click();
    else if (ev.key === "ArrowUp" && i > 0) rows[i - 1].click();
    else if (ev.key === "ArrowRight" && id != null) ctl.expand(id);
    else if (ev.key === "ArrowLeft" && id != null) {
      if (t.expanded.has(id)) ctl.collapse(id);
      else if (t.parent.get(id) != null) ctl.select(t.parent.get(id), true);
    } else return;
    ev.preventDefault();
  });
  return ctl;
}


// ---- Groups: names that are paths, such as "ecc/ECC-plan" (ECC-plan in the group ecc) ----
// The sidebars of Workflows, Agents and Policies show them as a tree of groups.

const groupOf = (path) => String(path).split("/").slice(0, -1).join("/");
const leafOf = (path) => String(path).split("/").pop();

// groupPaths returns every group that holds one of the names, at every depth, plus the extra ones.
function groupPaths(names, extra = []) {
  const out = new Set(extra);
  for (const name of names) {
    const parts = String(name).split("/");
    for (let i = 1; i < parts.length; i++) out.add(parts.slice(0, i).join("/"));
  }
  return out;
}

// groupNodes nests named items in their groups for createTree: in every group its subgroups
// first, then its items, each sorted by name. folder(path, children) and node(item) build them.
function groupNodes(groups, items, nameOf, folder, node, parent = "") {
  const depth = parent ? parent.split("/").length + 1 : 1;
  const inParent = (path) => path.split("/").length === depth && groupOf(path) === parent;
  return [
    ...[...groups].filter(inParent).sort().map((g) => folder(g, groupNodes(groups, items, nameOf, folder, node, g))),
    ...items.filter((item) => inParent(nameOf(item))).sort((a, b) => nameOf(a).localeCompare(nameOf(b))).map(node),
  ];
}

// groupIds are the tree ids (prefix and path) of the groups that hold a name, outermost first,
// in the order createTree's reveal opens them.
const groupIds = (name, prefix) => String(name).split("/").slice(0, -1).map((_, i, parts) => `${prefix}${parts.slice(0, i + 1).join("/")}`);

// openDialog shows a modal dialog: Escape and the backdrop close it, Tab stays inside,
// and focus returns to where it was. body and footer are nodes; returns { close, box }.
// Every small dialog (one field, one choice, or one question) is 440 px wide.
function openDialog({ title, body, footer, width = 440, onClose, labelledBy }) {
  const titleId = `dialog-title-${Date.now()}`;
  const box = h("div", { className: "dialog", tabIndex: -1, role: "dialog", "aria-modal": "true", "aria-labelledby": labelledBy || titleId, style: `width: min(${width}px, calc(100vw - 32px))` },
    h("div", { className: "dialog-head" }, h("h2", { id: titleId, className: "dialog-title" }, title)),
    h("div", { className: "dialog-body" }, body),
    footer ? h("div", { className: "dialog-foot" }, footer) : null);
  const backdrop = h("div", { className: "dialog-backdrop" }, box);
  document.body.append(backdrop);
  const release = trapFocus(box);
  const onKey = (ev) => { if (ev.key === "Escape") { ev.stopPropagation(); close(); } };
  backdrop.addEventListener("keydown", onKey);
  backdrop.addEventListener("pointerdown", (ev) => { if (ev.target === backdrop) close(); });
  window.addEventListener("hashchange", close, { once: true }); // navigating away closes it
  let closed = false;
  function close() {
    if (closed) return;
    closed = true;
    window.removeEventListener("hashchange", close);
    backdrop.remove();
    release();
    onClose?.();
  }
  // Focus starts on the first visible field or the enabled primary button, else the dialog itself.
  requestAnimationFrame(() => ([...box.querySelectorAll("[autofocus], textarea, input, select, button.primary")].find((e) => e.offsetParent !== null && !e.disabled) || box).focus());
  return { close, box };
}

// confirmAction asks before a destructive step in the app's own dialog: the question is
// the title, then Cancel and the action. It resolves true when the action is chosen.
function confirmAction(question, action) {
  return new Promise((resolve) => {
    let answered = false;
    const answer = (ok) => { if (!answered) { answered = true; resolve(ok); } };
    const go = h("button", { type: "button", className: "danger", onclick: () => { answer(true); dlg.close(); } }, action);
    const cancel = h("button", { type: "button", onclick: () => dlg.close() }, "Cancel");
    const dlg = openDialog({
      title: question,
      body: h("div"),
      footer: [cancel, go],
      onClose: () => answer(false),
    });
    // The safe choice has the focus, so Enter never deletes or discards by accident.
    requestAnimationFrame(() => cancel.focus());
  });
}

// pool runs fn over items with at most limit calls in flight.
async function pool(items, limit, fn) {
  const results = new Array(items.length);
  let next = 0;
  await Promise.all(Array.from({ length: Math.min(limit, items.length) }, async () => {
    while (next < items.length) {
      const i = next++;
      results[i] = await fn(items[i], i);
    }
  }));
  return results;
}

// traceQueue limits a slow request that many rows ask for at once (the listing of a run's
// objects, which makes the blobstore scan every object): at most `limit` are in flight, so the
// browser's few connections per host stay free for the live polls. One request per key is
// shared by everyone who asks for it. Of those that wait, the page that is shown goes first,
// and within it the one asked last (the run just opened, before the rows behind it); what only
// a page that is no longer shown waits for is dropped. The answer for a finished run (final)
// does not change, so it is kept and never asked again.
function traceQueue(load, limit = 2) {
  const flying = new Map(); // key -> job, requests in flight
  const waiting = []; // jobs not started yet, the most recently asked last
  const kept = new Map(); // key -> the answer for a finished run
  let shown = "";
  const pump = () => {
    while (flying.size < limit && waiting.length) {
      let at = waiting.length - 1;
      for (let i = at; i >= 0; i--) if (waiting[i].owners.has(shown)) { at = i; break; }
      const job = waiting.splice(at, 1)[0];
      flying.set(job.key, job);
      const settle = (done, keep) => (value) => {
        flying.delete(job.key); // first, so whoever asks next gets a new request, not this answer
        if (keep && job.final) kept.set(job.key, value);
        done(value);
        pump();
      };
      Promise.resolve().then(() => load(job.key)).then(settle(job.resolve, true), settle(job.reject, false));
    }
  };
  // ask returns the answer for key. owner is the page that asks; final says the run has finished,
  // and fresh that the answer must be newer than this call: neither shares a request that started
  // earlier, since it may have read the run before its last objects were written.
  function ask(key, { owner = shown, final = false, fresh = false } = {}, again = false) {
    if (kept.has(key)) return Promise.resolve(kept.get(key));
    const flight = flying.get(key);
    if (flight) {
      if (again && final) flight.final = true;
      if (again || flight.final || !(final || fresh)) return flight.promise;
      return flight.promise.then(() => {}, () => {}).then(() => ask(key, { owner, final }, true));
    }
    const at = waiting.findIndex((j) => j.key === key);
    const job = at >= 0 ? waiting.splice(at, 1)[0] : { key, owners: new Set(), final: false };
    job.promise ??= new Promise((resolve, reject) => { job.resolve = resolve; job.reject = reject; });
    job.owners.add(owner);
    job.final ||= final;
    waiting.push(job);
    pump();
    return job.promise;
  }
  return {
    ask: (key, options) => ask(key, options),
    // show names the page that is shown now and drops what only other pages still wait for.
    show(page) {
      shown = page;
      for (let i = waiting.length - 1; i >= 0; i--) {
        if (waiting[i].owners.has(page)) continue;
        const [job] = waiting.splice(i, 1);
        job.reject(Object.assign(new Error("its page is no longer shown"), { dropped: true }));
      }
    },
    // forget drops what is kept for key; a request for it that is under way is not kept either.
    forget(key) {
      kept.delete(key);
      for (const job of [...flying.values(), ...waiting]) if (job.key === key) job.final = false;
    },
    size: () => ({ flying: flying.size, waiting: waiting.length, kept: kept.size }),
  };
}

// traceListings lists the objects of a run, {objects}, through the queue above. Every page asks here.
const traceListings = traceQueue((trace) => api("GET", `/api/blobs/trace/${encodeURIComponent(trace)}`));

// A step after a merge or batch node runs once for every message the node forwards. Pass n of
// such a step stores its output as <step>.pass-<n>, with its records beside it
// (<step>.pass-<n>.run.json, <step>.pass-<n>.attempt-<k>.run.json). A step that runs again after
// a rejection keeps what round n stored as <step>.round-<n> (with <step>.round-<n>.run.json and
// the objects of a nested run under it); the newest result keeps the plain name. stepPass
// splits such a name, also a path through workflow nodes, into the step, its pass and its
// round; pass and round are 0 without one. Neither a pass nor a round is a step of its own.
const PASS_PART = /^(.+)\.pass-([1-9][0-9]*)$/;
const ROUND_PART = /^(.+)\.round-([1-9][0-9]*)$/;
function stepPass(name) {
  let pass = 0, round = 0;
  const step = String(name).split("/").map((part) => {
    const kept = ROUND_PART.exec(part);
    if (kept) { round = Number(kept[2]); part = kept[1]; } // the round follows the pass: build.pass-2.round-1
    const found = PASS_PART.exec(part);
    if (found) pass = Number(found[2]); // the innermost pass is the step's own
    return found ? found[1] : part;
  }).join("/");
  return { step, pass, round };
}

// A publish decision (an agent's push or pull request that a person approves) waits and is
// recorded as <step>.publish-<n>. publishStep names the step that asked, "" for any other name;
// such a record is never a step of its own.
const PUBLISH_PART = /^(.+)\.publish-([1-9][0-9]*)$/;
const publishStep = (name) => PUBLISH_PART.exec(String(name))?.[1] || "";

// runExtras is the one place that reads what the run status says about dev pods and rounds.
// The answer of GET /api/orchestrator/runs/<trace> has, when they exist:
//   "dev_pod": { "id", "repository", "source_branch", "work_branch" }   the run's dev pod
//   "pull_request": "<object ID of pull-request.json>"                  the stored pull request record
//   "rounds": { "<step>": <how often the step ran> }                    steps that ran again after a rejection
// and the status value "Blocked" with the agent's reason in "error".
function runExtras(st) {
  const pod = st?.dev_pod && typeof st.dev_pod === "object" && st.dev_pod.id ? st.dev_pod : null;
  const roundCounts = {};
  for (const [step, n] of Object.entries(st?.rounds && typeof st.rounds === "object" ? st.rounds : {})) if (Number(n) > 1) roundCounts[step] = Math.floor(Number(n));
  return {
    devPod: pod ? { id: String(pod.id), repository: String(pod.repository || ""), sourceBranch: String(pod.source_branch || ""), workBranch: String(pod.work_branch || "") } : null,
    pullRequest: typeof st?.pull_request === "string" ? st.pull_request : "",
    roundCounts,
    blocked: st?.status === "Blocked" ? String(st.error || "") : "",
  };
}

// stepTitle names a step for a person: its own name in title case and, when it runs in passes,
// the pass ("talk.pass-2", or talk with pass 2, is "Talk · pass 2").
function stepTitle(name, pass = 0) {
  const at = stepPass(name);
  const n = Number(pass) || at.pass;
  return `${titleCase(at.step.split("/").pop())}${n ? ` · pass ${n}` : ""}${at.round ? ` · round ${at.round}` : ""}`;
}

// runIndex knows every run from runs/<trace>/run.json (cached, it never changes) and
// each run's status (final statuses cached, running ones refreshed when asked).
const runIndex = {
  records: new Map(), // trace -> { trace_id, workflow, workflow_version, started, started_by, input_artifact, source }
  statuses: new Map(), // trace -> { status, artifacts, error, at }
  generations: new Map(), // trace -> local invalidation generation for in-flight request guards
  revision: 0, // changes when a local removal makes an in-flight directory listing stale
  loading: null,

  // workflowNames are the stored workflows' names, for workflowOf; the pages that list runs
  // read them with loadWorkflowNames, and a failure keeps the last ones.
  workflowNames: [],
  async loadWorkflowNames() {
    try { this.workflowNames = ((await api("GET", "/api/orchestrator/workflows")).workflows || []).map((w) => w.name); } catch { /* keep the last */ }
  },
  // workflowOf names a run's workflow as it is stored now. A run from before its workflow moved
  // into a group recorded the bare name ("workflow-1"); the one grouped workflow of that name
  // (default/workflow-1) stands in, so the run is listed, filtered and named with the others.
  // run.json itself never changes.
  workflowOf(r) {
    const name = r?.workflow || "";
    if (!name || name.includes("/") || this.workflowNames.includes(name)) return name;
    return groupedName(this.workflowNames, name) || name;
  },

  // forget removes every cached part of a run and invalidates work that started before it.
  forget(trace) {
    this.generations.set(trace, (this.generations.get(trace) || 0) + 1);
    this.revision++;
    this.records.delete(trace);
    this.statuses.delete(trace);
    traceListings.forget(trace);
  },

  // refresh loads the run index from sqldb in a single query, removes cached runs no longer
  // present, and seeds terminal statuses so Overview and Runs do not fan out per run. A listing
  // that overlaps a local removal is asked again, so its older snapshot cannot restore that run.
  refresh() {
    this.loading ??= (async () => {
      let runs;
      while (runs === undefined) {
        const revision = this.revision;
        const answer = await api("GET", "/api/sqldb/runs");
        runs = Array.isArray(answer?.runs) ? answer.runs : [];
        if (revision !== this.revision) runs = undefined;
      }
      const present = new Set(runs.map((r) => r.trace_id));
      for (const trace of new Set([...this.records.keys(), ...this.statuses.keys()])) {
        if (!present.has(trace)) this.forget(trace);
      }
      const now = Date.now();
      for (const r of runs) {
        this.records.set(r.trace_id, r);
        if (r.status && r.status !== "Running") {
          const prev = this.statuses.get(r.trace_id);
          if (!prev || prev.status === "Running") {
            this.statuses.set(r.trace_id, {
              ...(prev || {}),
              status: r.status,
              workflow: r.workflow || prev?.workflow || "",
              started: r.started || prev?.started || "",
              artifacts: prev?.artifacts || {},
              error: r.error || "",
              blocked: r.status === "Blocked" ? String(r.error || "") : "",
              at: now,
            });
          }
        }
      }
    })().finally(() => { this.loading = null; });
    return this.loading;
  },

  // list returns the known runs, newest first, optionally of one workflow.
  list(workflow) {
    return [...this.records.values()]
      .filter((r) => !workflow || this.workflowOf(r) === workflow)
      .sort((a, b) => new Date(b.started) - new Date(a.started));
  },

  // status returns a run's status, fetching it unless a final one is cached or a fresh one is.
  // Finished steps come from the blobstore (runs/<trace>/<step>), so failed and running runs
  // show them too: artifacts maps step to object, order lists the steps by finish time,
  // agents maps an agent step to the agent it ran (from <step>.run.json). passes maps a step
  // after a merge or batch node to its passes, [{ pass, artifact, record }], oldest first;
  // artifacts then names the output of its newest pass. rounds maps a step that ran again after
  // a rejection to its kept rounds, [{ round, artifact, record }], oldest first; the newest
  // result is the step's own.
  // page names the page that shows the run's steps. Listing them is slow (see traceQueue), so a
  // caller that needs the status alone (a table row, a badge) passes "" and never waits for it;
  // a finished run is listed once, a running one again when its listing is older than maxAgeMs.
  async status(trace, maxAgeMs = 3000, page = "runs") {
    const cached = this.statuses.get(trace);
    const current = cached && (cached.status !== "Running" || Date.now() - cached.at < maxAgeMs);
    const listed = (st) => !!st.listedAt && (st.status !== "Running" || Date.now() - st.listedAt < maxAgeMs);
    if (current && (!page || listed(cached))) return cached;
    const generation = this.generations.get(trace) || 0;
    let entry = cached;
    if (!current) {
      try {
        const st = await api("GET", `/api/orchestrator/runs/${trace}`);
        entry = { status: st.status, workflow: st.workflow || "", started: st.started && !st.started.startsWith("0001") ? st.started : "", artifacts: st.artifacts || {}, nodes: st.nodes && typeof st.nodes === "object" ? st.nodes : {}, error: st.error || "", ...runExtras(st), at: Date.now() }; // nodes: what the merge and batch steps hold
      } catch (e) {
        entry = { status: "", artifacts: {}, error: e.message, gone: /not found/i.test(e.message), at: Date.now() };
        if (!entry.gone) return entry;
      }
    }
    if (page) {
      const out = await this.outputs(trace, { owner: page, final: entry.status !== "Running" });
      entry.artifacts = { ...out.artifacts, ...entry.artifacts };
      entry.order = out.order;
      entry.nested = out.nested;
      entry.nestedArtifacts = out.nestedArtifacts;
      entry.records = out.records;
      entry.attemptRecords = out.attemptRecords;
      entry.passes = out.passes;
      entry.rounds = out.rounds; // the kept rounds of the steps that ran again after a rejection
      entry.metadata = out.metadata; // whether runs/<trace>/metadata.json exists
      if (!out.failed) entry.listedAt = Date.now(); // a listing that failed or was dropped is asked again
    }
    if (generation === (this.generations.get(trace) || 0)) this.statuses.set(trace, entry);
    return entry;
  },

  async outputs(trace, listing) {
    const prefix = `runs/${trace}/`;
    const artifacts = {}, created = {}, nested = new Set(), records = new Set(), attemptRecords = new Map(), nestedArtifacts = {}, passes = {}, rounds = {};
    let failed = false, metadata = false;
    // roundOf finds the entry of one kept round of a step ("build.round-1", also of a pass or
    // of a nested step), adding it when it is new; null for a name without a round.
    const roundOf = (key) => {
      const at = stepPass(key);
      if (!at.round) return null;
      const step = key.split("/").map((part) => part.replace(ROUND_PART, "$1")).join("/"); // the step, with its pass
      const list = (rounds[step] ||= []);
      let entry = list.find((r) => r.round === at.round);
      if (!entry) list.push(entry = { round: at.round, artifact: "", record: "" });
      return entry;
    };
    // passOf finds the entry of one pass of a step, adding it when it is new. A record is
    // named by its key, the step with its pass ("compare.pass-2"), as in records.
    const passOf = (key) => {
      const { step, pass } = stepPass(key);
      if (!pass) return null;
      const list = (passes[step] ||= []);
      let entry = list.find((p) => p.pass === pass);
      if (!entry) list.push(entry = { pass, artifact: "", record: "" });
      return entry;
    };
    const record = (key) => {
      records.add(key);
      const kept = roundOf(key);
      if (kept) { kept.record = key; return; } // the record of a kept round, never of the newest pass
      const entry = passOf(key);
      if (entry) entry.record = key;
    };
    try {
      const t = await traceListings.ask(trace, listing);
      for (const o of t.objects || []) {
        if (!o.id.startsWith(prefix)) continue;
        const rest = o.id.slice(prefix.length);
        const step = rest.split("/")[0];
        if (step === "metadata.json") { metadata = true; continue; } // the run metadata, not a step
        if (!step || step === "input" || step === "run.json" || step === "_evidence") continue;
        if (step === "pull-request.json" || step === "pull-request.patch") continue; // the pull request record of the run, not a step
        if (rest.split("/").some(publishStep)) continue; // the record of a publish decision, not a step
        // Execution records can belong to a routed child workflow and therefore contain
        // slashes. Classify them before treating nested objects as ordinary route output.
        const attemptRecord = /^(.*)\.attempt-[1-9][0-9]*\.run\.json$/.exec(rest);
        if (attemptRecord) {
          record(attemptRecord[1]);
          const ids = attemptRecords.get(attemptRecord[1]) || [];
          ids.push(o.id);
          attemptRecords.set(attemptRecord[1], ids);
          continue;
        }
        if (rest.endsWith(".run.json")) { record(rest.slice(0, -".run.json".length)); continue; }
        // What an earlier round left belongs to its step and round: "build.round-1" is round 1 of
        // build, and "flow.round-1/inner" an object of the nested run of round 1; neither is a step.
        if (stepPass(rest).round) {
          const kept = rest.includes("/") && !stepPass(rest.split("/").pop()).round ? null : roundOf(rest);
          if (kept) kept.artifact = o.id;
          continue;
        }
        // The output of a pass belongs to its step: "compare.pass-2" is pass 2 of compare, not a step.
        const pass = passOf(rest);
        if (pass) pass.artifact = o.id;
        const path = stepPass(rest).step, top = path.split("/")[0];
        if (path.includes("/")) { nested.add(top); created[top] ??= o.created; if (!pass) nestedArtifacts[path] = o.id; continue; } // "<node step>/<step>": a child run's output
        if (!pass) artifacts[top] = o.id;
        if (!pass || !created[top] || String(o.created) < String(created[top])) created[top] = o.created; // a step with passes is ordered by its first
      }
      // A step with passes shows the output of its newest pass as its own.
      for (const [path, list] of Object.entries(passes)) {
        list.sort((a, b) => a.pass - b.pass);
        const newest = [...list].reverse().find((p) => p.artifact);
        if (newest) (path.includes("/") ? nestedArtifacts : artifacts)[path] = newest.artifact;
      }
    } catch { failed = true; /* the status still shows */ }
    const order = Object.keys(created).sort((a, b) => String(created[a]).localeCompare(String(created[b])));
    for (const ids of attemptRecords.values()) ids.sort((a, b) => Number(/\.attempt-([0-9]+)\.run\.json$/.exec(a)?.[1] || 0) - Number(/\.attempt-([0-9]+)\.run\.json$/.exec(b)?.[1] || 0));
    for (const list of Object.values(rounds)) list.sort((a, b) => a.round - b.round);
    return { artifacts, order, nested, records, attemptRecords, nestedArtifacts, passes, rounds, failed, metadata };
  },

  // name is what sidebars and tables call a run: its workflow; a run without one is named
  // by its shortened trace ID.
  name(trace) {
    return titleCase(this.workflowOf(this.records.get(trace))) || midTruncate(trace, 13);
  },
  // label adds the start time where a run is picked from a list (the trace field, tooltips).
  label(trace) {
    const r = this.records.get(trace);
    return r?.started ? `${this.name(trace)} · ${formatTime(r.started)}` : this.name(trace);
  },
};

// Pending gates, polled every 10 seconds for the menu badge and the waiting state.
const decisionsState = { pending: [], loaded: false };

async function refreshDecisions() {
  try {
    decisionsState.pending = (await api("GET", "/api/orchestrator/decisions")).decisions || [];
    decisionsState.loaded = true;
  } catch { /* the Decisions page shows errors; the badge keeps its last value */ }
  showDecisionsBadge();
}

// showDecisionsBadge shows how many gates wait on the menu item, and tells the Runs page.
function showDecisionsBadge() {
  updateNavCounts();
  const badge = $("decisions-badge");
  badge.textContent = decisionsState.pending.length || "";
  badge.hidden = !decisionsState.pending.length;
  badge.title = `${decisionsState.pending.length} gate${decisionsState.pending.length === 1 ? "" : "s"} waiting`;
  fitMenu();
  document.dispatchEvent(new Event("decisionschange"));
}

// Waiting interactive sessions, polled with the gates for the menu badge and the waiting state.
const sessionsState = { pending: [], loaded: false, error: "" };

async function refreshSessions() {
  try {
    sessionsState.pending = (await api("GET", "/api/orchestrator/sessions")).sessions || [];
    sessionsState.loaded = true;
    sessionsState.error = "";
  } catch (e) { sessionsState.error = e.message; /* the Sessions page shows it; the badge keeps its last value */ }
  showSessionsBadge();
}

// showSessionsBadge shows how many sessions wait on the menu item, and tells the pages.
function showSessionsBadge() {
  updateNavCounts();
  const badge = $("sessions-badge");
  badge.textContent = sessionsState.pending.length || "";
  badge.hidden = !sessionsState.pending.length;
  badge.title = `${sessionsState.pending.length} session${sessionsState.pending.length === 1 ? "" : "s"} waiting`;
  fitMenu();
  document.dispatchEvent(new Event("sessionschange"));
}

// The factory's services (the cluster's deployments), polled every 10 s with the gates: Overview
// shows them, and the top bar marks a service that is not ready.
const clusterHealth = { services: [], loaded: false, error: "" };

// serviceStates names each deployment with its ready and desired replicas, by name: ready when
// every desired replica is ready, starting while some are, not ready when none is.
function serviceStates(deployments) {
  return (deployments || []).map((x) => {
    const desired = x.spec?.replicas ?? 0, ready = x.status?.readyReplicas || 0;
    const state = ready >= desired ? "ready" : ready > 0 ? "starting" : "down";
    return { name: x.metadata?.name || "", ready, desired, state };
  }).filter((s) => s.name).sort((a, b) => a.name.localeCompare(b.name));
}


// serviceMarker names the services that are not ready, for the top bar: "" when all are.
function serviceMarker(services) {
  const down = (services || []).filter((s) => s.state !== "ready");
  if (!down.length) return { text: "", title: "" };
  return {
    text: down.length === 1 ? `${down[0].name} not ready` : `${down.length} services not ready`,
    title: `Not ready: ${down.map((s) => `${s.name} (${s.ready}/${s.desired})`).join(", ")}. Opens Cluster`,
  };
}

// runWorkflow loads the workflow version a run used, for its graph, once per version; null when
// it cannot. A run from before its workflow moved into a group names it without the group
// ("ECC-review" for ecc/ECC-review): the stored workflow of that name in a group stands in.
const runWorkflowCache = new Map();
function runWorkflow(name, version) {
  const key = `${name}@${version || 0}`;
  if (!runWorkflowCache.has(key)) runWorkflowCache.set(key, (async () => {
    const load = (n) => (version ? api("GET", `/api/orchestrator/workflows/${encodeURIComponent(n)}/versions/${version}`) : null);
    try { const def = await load(name); if (def?.steps) return def; } catch { /* moved or gone */ }
    try {
      const all = (await api("GET", "/api/orchestrator/workflows")).workflows || [];
      const moved = all.some((w) => w.name === name) ? name : groupedName(all.map((w) => w.name), name);
      if (!moved) return null;
      return version ? await load(moved) : all.find((w) => w.name === moved) || null;
    } catch { return null; }
  })());
  return runWorkflowCache.get(key);
}

// groupedName finds the one stored workflow in a group whose own name is name, or "".
function groupedName(names, name) {
  const found = names.filter((n) => n.includes("/") && n.split("/").pop().toLowerCase() === String(name).toLowerCase());
  return found.length === 1 ? found[0] : "";
}

// traceMatch finds the run a typed trace ID names: the one known run whose ID starts with it
// (case and dashes ignored, at least 4 characters), or a full UUID even before the run list knows
// it. count says how many known runs start with it, for "3 runs match".
function traceMatch(ids, text) {
  const want = String(text || "").trim().toLowerCase();
  const plain = (s) => s.toLowerCase().replace(/-/g, "");
  if (/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/.test(want)) return { trace: want, count: 1 };
  if (plain(want).length < 4) return { trace: "", count: 0 };
  const found = ids.filter((id) => plain(id).startsWith(plain(want)));
  return { trace: found.length === 1 ? found[0] : "", count: found.length };
}

// The top bar's trace ID jump opens the run the typed ID names, from any page.
function wireTraceJump() {
  const input = $("trace-jump");
  $("trace-jump-form").addEventListener("submit", async (ev) => {
    ev.preventDefault();
    if (!runIndex.records.size) { try { await runIndex.refresh(); } catch { /* the full ID still opens */ } }
    const { trace, count } = traceMatch([...runIndex.records.keys()], input.value);
    if (trace) { input.value = ""; input.blur(); location.hash = `#runs/${trace}`; return; }
    notify(count > 1 ? `${count} runs match: type more of the ID` : input.value.trim().length < 4 ? "Type at least 4 characters of the trace ID" : "No run has that trace ID", "error");
  });
  input.addEventListener("keydown", (ev) => { if (ev.key === "Escape") { input.value = ""; input.blur(); } });
}

// showServiceMarker shows the marker in the top bar while a service is not ready.
function showServiceMarker() {
  const marker = $("service-marker");
  const { text, title } = serviceMarker(clusterHealth.services);
  marker.hidden = !text;
  marker.title = title;
  marker.replaceChildren(...(text ? [statusChip("failed", text)] : []));
  fitMenu();
}

async function refreshClusterHealth() {
  try {
    clusterHealth.services = serviceStates((await api("GET", "/api/cluster/deployments")).items);
    clusterHealth.loaded = true;
    clusterHealth.error = "";
  } catch (e) { clusterHealth.error = e.message; /* Cluster shows the error; the marker keeps its last value */ }
  showServiceMarker();
  document.dispatchEvent(new Event("clusterchange"));
}

// A run waits while a gate or an interactive session of it waits for a person.
const isWaiting = (trace) => decisionsState.pending.some((d) => d.trace_id === trace) || sessionsState.pending.some((s) => s.trace_id === trace);

// Resizable panels. A page's grid reads its column widths from CSS variables; a handle
// on a column edge sets the variable, kept per page and column in localStorage.
// Double-clicking a handle restores the default width.
const PANELS = {
  workflows: [
    { column: ".list-panel", prop: "--col-list", min: 200, max: 480 },
    { column: ".inspector", prop: "--col-inspector", min: 280, max: 720, edge: "left" },
  ],
  runs: [
    { column: ".list-panel", prop: "--col-list", min: 200, max: 480 },
    // A run's detail may take all the room the workflow column can spare, to read long evidence.
    { column: ".run-detail", prop: "--col-detail", min: 320, max: Infinity, edge: "left" },
  ],
  evaluations: [
    { column: ".list-panel", prop: "--col-list", min: 200, max: 480 },
    { column: ".evaluation-detail", prop: "--col-detail", min: 320, max: 720, edge: "left" },
  ],
  decisions: [
    { column: ".list-panel", prop: "--col-list", min: 200, max: 480 },
    { column: ".decision-detail", prop: "--col-detail", min: 320, max: 720, edge: "left" },
  ],
  sessions: [
    { column: ".list-panel", prop: "--col-list", min: 200, max: 480 },
    { column: ".session-files", prop: "--col-detail", min: 320, max: 720, edge: "left" },
  ],
  teams: [
    { column: ".list-panel", prop: "--col-list", min: 200, max: 480 },
    { column: ".teamview-files", prop: "--col-detail", min: 320, max: 720, edge: "left" },
  ],
  judges: [{ column: ".list-panel", prop: "--col-list", min: 200, max: 480 }],
  tools: [{ column: ".list-panel", prop: "--col-list", min: 200, max: 480 }],
  agents: [{ column: ".list-panel", prop: "--col-list", min: 200, max: 480 }],
  policies: [{ column: ".list-panel", prop: "--col-list", min: 200, max: 480 }],
  skills: [{ column: ".list-panel", prop: "--col-list", min: 200, max: 480 }],
  "mcp-gateway": [
    { column: ".list-panel", prop: "--col-list", min: 200, max: 480 },
    { column: ".mcp-test", prop: "--col-detail", min: 320, max: 720, edge: "left" },
  ],
  artifacts: [
    { column: ".list-panel", prop: "--col-list", min: 200, max: 480 },
    { column: ".content-panel", prop: "--col-detail", min: 320, max: 720, edge: "left" },
  ],
  cluster: [{ column: ".list-panel", prop: "--col-list", min: 200, max: 480 }],
  connectors: [{ column: ".list-panel", prop: "--col-list", min: 200, max: 480 }],
  intake: [{ column: ".list-panel", prop: "--col-list", min: 200, max: 480 }],
  devpods: [{ column: ".list-panel", prop: "--col-list", min: 200, max: 480 }],
};
const MIN_MAIN = 280;
const widthKey = (tab, prop) => `factory.width.${tab}.${prop}`;

const sidebarToggles = {};
// Pages whose right column can be hidden with the detail toggle.
const DETAIL_PANELS = { runs: ".run-detail", evaluations: ".evaluation-detail", decisions: ".decision-detail", sessions: ".session-files", teams: ".teamview-files", artifacts: ".content-panel", "mcp-gateway": ".mcp-test" };
const detailToggles = {}; // right-panel toggles for pages with a fixed right panel
const pageToggles = {}; // extra header toggles per page, registered by the page
// reserveToggles keeps the toggles from covering a panel header: the visible panel at the
// right edge of the page reserves the toggles' width.
function reserveToggles() {
  const box = $("page-toggles");
  const width = box.offsetWidth;
  document.querySelector(".content").style.setProperty("--toggles-w", `${width}px`);
  for (const p of document.querySelectorAll(".toggles-edge")) p.classList.remove("toggles-edge");
  const page = activeTab && $(`tab-${activeTab}`);
  box.style.top = page?.classList.contains("page-sectioned") ? "48px" : "0px";
  if (!page || !width) return;
  const right = page.getBoundingClientRect().right;
  for (const p of page.querySelectorAll(":scope > .panel, :scope > .canvas-area")) {
    const r = p.getBoundingClientRect();
    if (r.width && Math.abs(r.right - right) < 2) p.classList.add("toggles-edge");
  }
}

// revealPanel plays the panel reveal on a column its toggle has just shown: it arrives from
// its own edge of the window. Hiding a column is immediate.
function revealPanel(panel, fromRight = false) {
  if (!panel) return;
  panel.classList.remove("panel-reveal", "from-right");
  void panel.offsetWidth; // restart the animation
  panel.classList.add("panel-reveal");
  if (fromRight) panel.classList.add("from-right");
  panel.addEventListener("animationend", () => panel.classList.remove("panel-reveal", "from-right"), { once: true });
}

function setupPanels() {
  const watch = new ResizeObserver(() => reserveToggles());
  for (const p of document.querySelectorAll(".page > .panel, .page > .canvas-area")) watch.observe(p);
  for (const [tab, columns] of Object.entries(PANELS)) {
    const grid = $(`tab-${tab}`);
    if (columns.some((c) => c.column === ".list-panel")) {
      const key = `factory.sidebar.${tab}`;
      let open = true;
      try { open = localStorage.getItem(key) !== "closed"; } catch { /* storage unavailable */ }
      const b = iconButton("panel", "Sidebar", () => {
        open = !open;
        try { localStorage.setItem(key, open ? "open" : "closed"); } catch { /* storage unavailable */ }
        apply();
        if (open) revealPanel(grid.querySelector(":scope > .list-panel"));
      });
      const apply = () => { grid.classList.toggle("no-list", !open); b.classList.toggle("on", open); b.setAttribute("aria-pressed", String(open)); };
      apply();
      sidebarToggles[tab] = b;
    }
    if (DETAIL_PANELS[tab]) {
      const key = `factory.detail.${tab}`;
      let open = true;
      try { open = localStorage.getItem(key) !== "closed"; } catch { /* storage unavailable */ }
      const b = iconButton("panelRight", "Detail panel", () => {
        open = !open;
        try { localStorage.setItem(key, open ? "open" : "closed"); } catch { /* storage unavailable */ }
        apply();
        if (open) revealPanel(grid.querySelector(`:scope > ${DETAIL_PANELS[tab]}`), true);
      });
      const apply = () => { grid.classList.toggle("no-detail", !open); b.classList.toggle("on", open); b.setAttribute("aria-pressed", String(open)); };
      apply();
      detailToggles[tab] = b;
    }
    const placers = [];
    new ResizeObserver(() => placers.forEach((p) => p())).observe(grid);
    for (const c of columns) {
      try {
        const saved = Number(localStorage.getItem(widthKey(tab, c.prop)));
        if (saved) grid.style.setProperty(c.prop, `${Math.min(c.max, Math.max(c.min, saved))}px`);
      } catch { /* storage unavailable */ }
      const col = grid.querySelector(`:scope > ${c.column}`);
      if (!col) continue;
      const handle = h("div", { className: "resize-handle", role: "separator", "aria-orientation": "vertical", "aria-label": "Resize" });
      col.classList.add("resizable");
      grid.append(handle);
      const place = () => {
        handle.hidden = col.offsetParent === null;
        handle.style.left = `${(c.edge === "left" ? col.offsetLeft : col.offsetLeft + col.offsetWidth) - 4}px`;
      };
      placers.push(place);
      new ResizeObserver(() => placers.forEach((p) => p())).observe(col);
      handle.addEventListener("pointerdown", (ev) => {
        if (ev.button !== 0) return;
        ev.preventDefault();
        handle.setPointerCapture(ev.pointerId);
        const start = ev.clientX, width = col.getBoundingClientRect().width;
        const others = [...grid.children].filter((x) => x !== col && x.offsetParent !== null && x.classList.contains("resizable"))
          .reduce((sum, x) => sum + x.getBoundingClientRect().width, 0);
        const max = Math.min(c.max, grid.getBoundingClientRect().width - others - MIN_MAIN);
        document.body.classList.add("resizing");
        const move = (e) => {
          const dx = (e.clientX - start) * (c.edge === "left" ? -1 : 1);
          grid.style.setProperty(c.prop, `${Math.round(Math.min(Math.max(c.min, max), Math.max(c.min, width + dx)))}px`);
        };
        const up = () => {
          handle.removeEventListener("pointermove", move);
          document.body.classList.remove("resizing");
          try { localStorage.setItem(widthKey(tab, c.prop), parseInt(grid.style.getPropertyValue(c.prop), 10)); } catch { /* storage unavailable */ }
        };
        handle.addEventListener("pointermove", move);
        handle.addEventListener("pointerup", up, { once: true });
      });
      handle.addEventListener("dblclick", () => {
        grid.style.removeProperty(c.prop);
        try { localStorage.removeItem(widthKey(tab, c.prop)); } catch { /* storage unavailable */ }
      });
    }
  }
}

// Pages whose actions record the acting person: the chip turns red there when empty.
const NEEDS_NAME = new Set(["workflows", "decisions", "sessions", "agents", "policies", "skills", "artifacts"]);

function updateNameWarning() {
  const missing = NEEDS_NAME.has(activeTab) && !creator();
  const chip = $("acting-as");
  chip.classList.toggle("missing", missing);
  chip.title = missing
    ? "Needs your name: it is recorded as creator of runs, versions, decisions, and files"
    : "Recorded as creator of runs, versions, decisions, and files";
}

function creator() {
  return $("creator").value.trim();
}

// creatorHeaders returns the X-Creator header for writes when a name is set.
function creatorHeaders() {
  return creator() ? { "X-Creator": creator() } : {};
}

// ---- Context menus (shared by every page) ----

const menu = h("div", { className: "ctx-menu", role: "menu", hidden: true });
document.body.append(menu);

let menuReturn = null; // the element that had the focus before the menu opened

// openMenu shows a right-click menu of items: [label, run, class, tooltip, disabled, children],
// "-" for a separator. An item with children is a submenu: it opens in the menu's place (click,
// Enter or ArrowRight), with its own label first to go back (click or ArrowLeft).
function openMenu(ev, items) {
  ev.preventDefault();
  ev.stopPropagation();
  // The row the menu acts on is outlined while the menu is open; the selection stays put.
  for (const e of document.querySelectorAll(".menu-target")) e.classList.remove("menu-target");
  if (ev.currentTarget instanceof Element && ev.currentTarget.matches(".tree-row, tr, .rows li")) ev.currentTarget.classList.add("menu-target");
  menuReturn = document.activeElement;
  showMenuItems(items);
  menu.hidden = false;
  const r = menu.getBoundingClientRect();
  menu.style.left = `${Math.min(ev.clientX, window.innerWidth - r.width - 8)}px`;
  menu.style.top = `${Math.min(ev.clientY, window.innerHeight - r.height - 8)}px`;
  menu.querySelector("button:not(.disabled)")?.focus();
}

// showMenuItems fills the open menu with items; back is the item list a submenu returns to.
function showMenuItems(items, back = null, label = "") {
  const button = (item) => {
    const sub = Array.isArray(item[5]);
    return h("button", {
      type: "button", role: "menuitem", className: [item[2] || "", item[4] ? "disabled" : "", sub ? "has-submenu" : ""].filter(Boolean).join(" "),
      title: item[3] || undefined, "aria-disabled": item[4] ? "true" : undefined, "aria-haspopup": sub ? "menu" : undefined,
      dataset: sub ? { submenu: "1" } : undefined,
      // Focus goes back to what the menu acted on first, so a dialog the item opens returns it there.
      onclick: () => {
        if (item[4]) return;
        if (sub) { showMenuItems(item[5], items, item[0]); return; }
        closeMenu(); if (menuReturn?.isConnected) menuReturn.focus(); item[1]();
      },
    }, item[0]);
  };
  const head = back ? [h("button", { type: "button", role: "menuitem", className: "menu-back", dataset: { back: "1" }, title: "Shows the menu again", onclick: () => showMenuItems(back) }, label), h("hr", { role: "separator" })] : [];
  menu.replaceChildren(...head, ...items.map((item) => (item === "-" ? h("hr", { role: "separator" }) : button(item))));
  menu.querySelector(back ? "button:not(.disabled):not(.menu-back)" : "button:not(.disabled)")?.focus();
}

function closeMenu() {
  menu.hidden = true;
  for (const e of document.querySelectorAll(".menu-target")) e.classList.remove("menu-target");
}

// openPopover anchors a small interactive picker to a control. It follows the context-menu
// positioning and focus rules while allowing checkboxes and explicit Apply/Cancel actions.
function openPopover(anchor, body, onClose) {
  const box = h("div", { className: "ctx-menu picker-popover", role: "dialog" }, body);
  document.body.append(box);
  const release = trapFocus(box);
  let closed = false;
  const close = () => {
    if (closed) return;
    closed = true;
    document.removeEventListener("pointerdown", outside, true);
    window.removeEventListener("resize", close);
    box.remove();
    release();
    onClose?.();
  };
  const outside = (event) => { if (!box.contains(event.target) && event.target !== anchor) close(); };
  box.addEventListener("keydown", (event) => { if (event.key === "Escape") { event.preventDefault(); close(); } });
  const rect = anchor.getBoundingClientRect();
  const bounds = box.getBoundingClientRect();
  box.style.left = `${Math.max(8, Math.min(rect.left, window.innerWidth - bounds.width - 8))}px`;
  box.style.top = `${Math.min(rect.bottom + 4, window.innerHeight - bounds.height - 8)}px`;
  document.addEventListener("pointerdown", outside, true);
  window.addEventListener("resize", close);
  requestAnimationFrame(() => box.querySelector("input:not([disabled]), button:not([disabled])")?.focus());
  return { box, close };
}

document.addEventListener("pointerdown", (ev) => { if (!menu.contains(ev.target)) closeMenu(); }, true);
document.addEventListener("contextmenu", closeMenu, true);
document.addEventListener("scroll", closeMenu, true);
window.addEventListener("blur", closeMenu);
window.addEventListener("resize", closeMenu);
window.addEventListener("hashchange", closeMenu);
menu.addEventListener("keydown", (ev) => {
  const items = [...menu.querySelectorAll("button")];
  const i = items.indexOf(document.activeElement);
  if (ev.key === "ArrowDown") { ev.preventDefault(); items[(i + 1) % items.length].focus(); }
  if (ev.key === "ArrowUp") { ev.preventDefault(); items[(i - 1 + items.length) % items.length].focus(); }
  if (ev.key === "ArrowRight" && document.activeElement?.dataset?.submenu) { ev.preventDefault(); document.activeElement.click(); }
  if (ev.key === "ArrowLeft") { const back = menu.querySelector("[data-back]"); if (back) { ev.preventDefault(); back.click(); } }
  if (ev.key === "Escape") { ev.preventDefault(); ev.stopPropagation(); closeMenu(); if (menuReturn?.isConnected) menuReturn.focus(); }
});

// The keyboard opens the right-click menu of what has focus (a row, a tree row, a tile) with
// Shift+F10 or the ContextMenu key, anchored under its left edge, as a right-click would.
document.addEventListener("keydown", (ev) => {
  if (!(ev.key === "ContextMenu" || (ev.shiftKey && ev.key === "F10"))) return;
  const target = document.activeElement;
  if (!target || target === document.body || !menu.hidden) return;
  ev.preventDefault();
  const r = target.getBoundingClientRect();
  target.dispatchEvent(new MouseEvent("contextmenu", { bubbles: true, cancelable: true, clientX: Math.round(r.left + 8), clientY: Math.round(r.bottom) }));
});

// setPageActions fills the toggles row at the right end of the panel-header row.
function setPageActions(...nodes) {
  $("page-toggles").replaceChildren(...nodes.filter(Boolean));
}

const pageHrefs = new Map();
let navPopover = null, navTrigger = null;

// The pages of the Tasks menu that count what waits for a person. The menu item carries the
// counts, and so does each page's row in the open menu, so it is clear where the tasks wait.
const NAV_COUNTS = {
  decisions: { count: () => decisionsState.pending.length, noun: "gate" },
  sessions: { count: () => sessionsState.pending.length, noun: "session" },
};
function setNavCount(badge, name) {
  const n = NAV_COUNTS[name].count();
  badge.textContent = n ? String(n) : "";
  badge.hidden = !n;
  badge.title = `${n} ${NAV_COUNTS[name].noun}${n === 1 ? "" : "s"} waiting`;
  return badge;
}
// navCountBadge is the count on a page's row in the open menu, or null for a page that counts nothing.
const navCountBadge = (name) => (NAV_COUNTS[name] ? setNavCount(h("span", { className: "badge", dataset: { count: name } }), name) : null);
// updateNavCounts keeps the counts of an open menu in step with the menu item's.
function updateNavCounts() {
  for (const link of navPopover?.children || []) for (const badge of link.children || []) if (badge.dataset?.count) setNavCount(badge, badge.dataset.count);
}

function closeNavPopover(returnFocus = false) {
  if (!navPopover || navPopover.hidden) return;
  navPopover.hidden = true;
  navTrigger?.setAttribute("aria-expanded", "false");
  if (returnFocus) navTrigger?.focus();
  navTrigger = null;
}

function openNavPopover(trigger, group) {
  if (navTrigger === trigger && !navPopover.hidden) { closeNavPopover(true); return; }
  closeNavPopover();
  navTrigger = trigger;
  const entries = [];
  for (const name of group.pages) {
    for (const planned of (group.planned || []).filter((item) => item.before === name)) {
      entries.push(h("div", { className: "nav-planned", role: "menuitem", "aria-disabled": "true", title: planned.note },
        icon(planned.icon), h("span", {}, planned.title)));
    }
    const page = PAGES.find((item) => item.name === name);
    const label = name === "connectors" ? "Git" : page.title;
    const link = h("a", { href: pageHrefs.get(name) || `#${name}`, role: "menuitem", dataset: { page: name }, title: page.tip }, icon(page.icon), h("span", {}, label), navCountBadge(name));
    if (name === activeTab || SUBPAGES[activeTab]?.menu === name) link.setAttribute("aria-current", "page");
    link.onclick = () => closeNavPopover(true);
    entries.push(link);
  }
  navPopover.replaceChildren(...entries);
  navPopover.hidden = false;
  trigger.setAttribute("aria-expanded", "true");
  const anchor = trigger.getBoundingClientRect(), bounds = navPopover.getBoundingClientRect();
  navPopover.style.left = `${Math.max(8, Math.min(anchor.left, window.innerWidth - bounds.width - 8))}px`;
  navPopover.style.top = `${anchor.bottom + 4}px`;
  (navPopover.querySelector('[aria-current="page"]') || navPopover.querySelector("a"))?.focus();
}

function route() {
  let hash = location.hash.slice(1);
  try { hash = decodeURI(hash); } catch { /* keep it encoded */ }
  const name = hash.split(/[/?]/)[0];
  if (!tabs[name]) {
    location.replace("#overview");
    return;
  }
  if (activeTab !== name) {
    if (activeTab) {
      tabs[activeTab].hide?.();
      $(`tab-${activeTab}`).hidden = true;
    }
    activeTab = name;
    traceListings.show(name); // listings only the page left behind waits for are dropped
    setPageActions(sidebarToggles[name], ...(pageToggles[name] || []), detailToggles[name]);
    $(`tab-${name}`).hidden = false;
    reserveToggles();
    const page = PAGES.find((p) => p.name === name) || SUBPAGES[name];
    document.title = `${page.title} · Open Software Factory`;
    for (const button of $("rail").querySelectorAll(".nav-trigger")) {
      const group = NAV_GROUPS.find((item) => item.name === button.dataset.group);
      button.classList.toggle("active", group.pages.includes(page.menu || name));
    }
    fitMenu(); // Keep navigation within the available header width.
    updateNameWarning();
  }
  tabs[name].show(hash.slice(name.length));
}

function buildRail() {
  navPopover = h("div", { className: "nav-popover", role: "menu", hidden: true, "aria-label": "Pages" });
  document.body.append(navPopover);
  $("rail").replaceChildren(...NAV_GROUPS.map((group) => {
    const button = h("button", { type: "button", className: "nav-trigger", dataset: { group: group.name },
      "aria-haspopup": "menu", "aria-expanded": "false", onclick: () => openNavPopover(button, group) },
      icon(group.icon), h("span", { className: "rail-label" }, group.name), icon("down", 12));
    if (group.name === "Tasks") button.append(h("span", { id: "decisions-badge", className: "badge", hidden: true }), h("span", { id: "sessions-badge", className: "badge", hidden: true }));
    return button;
  }));
  document.addEventListener("pointerdown", (event) => {
    if (navPopover.hidden || navPopover.contains(event.target) || navTrigger?.contains(event.target)) return;
    closeNavPopover();
  });
  document.addEventListener("focusin", (event) => {
    if (navPopover.hidden || navPopover.contains(event.target) || navTrigger?.contains(event.target)) return;
    closeNavPopover();
  });
  document.addEventListener("keydown", (event) => {
    if (navPopover.hidden) return;
    if (event.key === "Escape") { event.preventDefault(); closeNavPopover(true); }
    if (event.key === "ArrowDown" || event.key === "ArrowUp") {
      event.preventDefault();
      const links = [...navPopover.querySelectorAll("a")], at = links.indexOf(document.activeElement);
      links[(at + (event.key === "ArrowDown" ? 1 : -1) + links.length) % links.length]?.focus();
    }
  });
  fitMenu();
}

// Keep group labels visible; the rail scrolls horizontally on narrow windows.
const MENU_FITS = [[], ["fit-tight"], ["fit-tight", "fit-logo"]];
function fitMenu() {
  const menu = $("rail"), bar = menu.parentElement;
  let step = 0;
  for (; step < MENU_FITS.length; step++) {
    bar.classList.remove("fit-tight", "fit-logo", "fit-labels", "fit-icons");
    bar.classList.add(...MENU_FITS[step]);
    if (menu.scrollWidth <= menu.clientWidth) break;
  }
}
window.addEventListener("resize", fitMenu);

// fitWidth sizes an input or select to the text it shows (its value, the chosen option,
// or the placeholder), so a field is never wider or narrower than its content.
const measure = h("span", { className: "measure", "aria-hidden": "true" });
function fitWidth(el) {
  if (!measure.isConnected) document.body.append(measure);
  const cs = getComputedStyle(el);
  measure.style.font = cs.font;
  measure.textContent = (el.tagName === "SELECT" ? el.selectedOptions[0]?.text : el.value) || el.placeholder || "";
  const box = parseFloat(cs.paddingLeft) + parseFloat(cs.paddingRight) + parseFloat(cs.borderLeftWidth) + parseFloat(cs.borderRightWidth);
  el.style.width = `${Math.ceil(measure.getBoundingClientRect().width + box) + (el.tagName === "SELECT" ? 24 : 2)}px`;
  // A suggestion list reserves room for its arrow inside the field; grow by what still overflows.
  if (el.tagName === "INPUT" && el.scrollWidth > el.clientWidth) el.style.width = `${parseFloat(el.style.width) + el.scrollWidth - el.clientWidth}px`;
}

function startApp() {
  buildRail();
  $("acting-as").prepend(icon("person"));
  try { $("creator").value = localStorage.getItem(CREATOR_KEY) || ""; } catch { /* storage unavailable */ }
  const fitCreator = () => fitWidth($("creator"));
  fitCreator();
  $("creator").addEventListener("input", () => {
    fitCreator();
    try { localStorage.setItem(CREATOR_KEY, creator()); } catch { /* storage unavailable */ }
    document.dispatchEvent(new Event("creatorchange"));
    updateNameWarning();
  });
  setupPanels();
  // A page's menu item returns to where that page was left (within the session); on the page
  // itself it opens the page plainly.
  window.addEventListener("hashchange", (ev) => {
    const pageOf = (hash) => hash.slice(1).split(/[/?]/)[0];
    const from = new URL(ev.oldURL).hash, to = location.hash;
    if (pageOf(from) !== pageOf(to)) {
      if (PAGES.some((p) => p.name === pageOf(from))) pageHrefs.set(pageOf(from), from);
      pageHrefs.delete(pageOf(to));
    }
    closeNavPopover();
    route();
  });
  route();
  refreshDecisions();
  refreshSessions();
  refreshClusterHealth();
  wireTraceJump();
  setInterval(() => { if (!document.hidden) { refreshDecisions(); refreshSessions(); refreshClusterHealth(); } }, 10000);
}

window.addEventListener("DOMContentLoaded", startApp);
