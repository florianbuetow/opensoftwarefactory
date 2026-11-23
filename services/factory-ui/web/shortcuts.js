"use strict";
// Keyboard shortcuts for people who use the factory all day. None of them fires while a field,
// a dialog or a menu has the keyboard, and none needs a modifier key. "?" lists them.
const ShortcutsLogic = (() => {
  // G then a letter goes to a page.
  const GO = [
    ["o", "overview", "Overview"], ["m", "metrics", "Metrics"], ["c", "cluster", "Cluster"],
    ["w", "workflows", "Workflows"], ["a", "agents", "Agents"], ["r", "runs", "Runs"], ["e", "evaluations", "Evals"],
    ["d", "decisions", "Decisions"], ["s", "sessions", "Sessions"],
    ["n", "connectors", "Connectors"], ["p", "policies", "Policies"], ["k", "skills", "Skills"], ["b", "artifacts", "Blobstore"],
  ];
  const LIST = [
    ...GO.map(([key, , title]) => [`G then ${key.toUpperCase()}`, `Go to ${title}`]),
    ["R", "Run… the workflow on the canvas (Workflows)"],
    ["J / K", "Open the next or previous waiting decision"],
    ["/", "Jump to a run by its trace ID"],
    ["Shift+F10", "Open the menu of what has focus"],
    ["?", "Show these shortcuts"],
  ];

  // typing reports whether a key goes to a field, a dialog or a menu rather than to the page.
  const typing = (el) => !!el?.closest?.("input, textarea, select, [contenteditable], [contenteditable=''], .dialog, .ctx-menu, [role=menu]");

  // decisionId is the Decisions hash id of a waiting gate.
  const decisionId = (d) => `p:${d.workflow_id}|${d.step}`;

  // nextDecision is the id of the waiting gate after (step 1) or before (step -1) the one shown,
  // in the order Decisions lists them; the first (or last) when none of them is shown.
  function nextDecision(pending, shown, step) {
    if (!pending.length) return "";
    const ids = pending.map(decisionId);
    const at = ids.indexOf(shown);
    if (at < 0) return step > 0 ? ids[0] : ids[ids.length - 1];
    return ids[(at + step + ids.length) % ids.length];
  }

  // pageFor is the page that G then key goes to, or "".
  const pageFor = (key) => GO.find(([k]) => k === key)?.[1] || "";

  return { GO, LIST, typing, decisionId, nextDecision, pageFor };
})();

(() => {
  const S = ShortcutsLogic;
  let pendingG = 0; // when G was pressed, for G then a letter within 1.5 s

  function showList() {
    const rows = S.LIST.map(([keys, what]) => h("tr", {}, h("td", {}, h("kbd", {}, keys)), h("td", {}, what)));
    const close = h("button", { type: "button", className: "primary", onclick: () => dlg.close() }, "Close");
    const dlg = openDialog({ title: "Keyboard shortcuts", width: 520, body: h("table", { className: "data-table shortcut-table" }, h("tbody", {}, ...rows)), footer: [close] });
  }

  // shownDecision is the waiting gate Decisions shows, from the hash.
  function shownDecision() {
    const [page, rest = ""] = location.hash.slice(1).split("/");
    if (page !== "decisions") return "";
    try { return decodeURIComponent(rest.split("?")[0]); } catch { return ""; }
  }

  document.addEventListener("keydown", (ev) => {
    if (ev.metaKey || ev.ctrlKey || ev.altKey || ev.defaultPrevented) return;
    if (S.typing(ev.target) || document.querySelector(".dialog-backdrop") || !menu.hidden) return;
    const key = ev.key.length === 1 ? ev.key.toLowerCase() : ev.key;
    if (pendingG && Date.now() - pendingG < 1500) {
      pendingG = 0;
      const page = S.pageFor(key);
      if (page) { ev.preventDefault(); location.hash = `#${page}`; }
      return;
    }
    pendingG = 0;
    if (ev.key === "?") { ev.preventDefault(); showList(); return; }
    if (ev.shiftKey) return;
    if (key === "g") { pendingG = Date.now(); return; }
    if (key === "j" || key === "k") {
      const id = S.nextDecision(decisionsState.pending, shownDecision(), key === "j" ? 1 : -1);
      ev.preventDefault();
      if (id) location.hash = `#decisions/${encodeURIComponent(id)}`;
      else notify("No decision waits");
      return;
    }
    if (key === "r" && activeTab === "workflows") {
      const run = document.querySelector("#tab-workflows .wf-run");
      if (run && !run.hidden && !run.disabled) { ev.preventDefault(); run.click(); }
      else if (run && !run.hidden) { ev.preventDefault(); notify(`Not started: ${run.title.charAt(0).toLowerCase()}${run.title.slice(1)}`); }
      return;
    }
    if (key === "/") {
      const jump = $("trace-jump");
      if (jump) { ev.preventDefault(); jump.focus(); jump.select?.(); }
    }
  });
})();
