"use strict";
// Artifacts tab, three columns: a lazy folder tree of the blobstore, the objects of the
// selected folder or the artifacts of the selected trace, and the selected artifact.
// Hashes: #artifacts (root), #artifacts/<prefix>/ (folder), #artifacts/<id> (object),
// #artifacts?trace=<trace id>[&object=<id>] (the artifacts of a run, oldest first).
// Folders under runs/ are trace IDs; selecting one shows that trace.

(() => {
  const RUNS = "runs";
  const listings = new Map(); // prefix -> { directories, objects }, fetched once
  const state = { seq: 0, folder: null, selected: "", stale: true };

  const parentOf = (id) => id.split("/").slice(0, -1).join("/");
  const join = (prefix, name) => (prefix ? `${prefix}/${name}` : name);
  // traceOf returns the trace ID when prefix is a runs/<trace> folder.
  const traceOf = (prefix) => (/^runs\/[^/]+$/.test(prefix) ? prefix.slice(RUNS.length + 1) : "");
  const folderHash = (prefix) => (traceOf(prefix) ? `#artifacts?trace=${encodeURIComponent(traceOf(prefix))}` : prefix ? `#artifacts/${prefix}/` : "#artifacts");

  // list fetches a prefix listing; a missing prefix is an empty folder.
  async function list(prefix, fresh = false) {
    if (!fresh && listings.has(prefix)) return listings.get(prefix);
    let l;
    try {
      l = await api("GET", `/api/blobs/list/${prefix}`);
    } catch (e) {
      if (!/not found/i.test(e.message)) throw e;
      l = { directories: [], objects: [] };
    }
    listings.set(prefix, l);
    if (prefix === RUNS) await runIndex.refresh().catch(() => {}); // run folders are named by their run records
    if (fresh && prefix === "") tree.setRoots(childNodes("", l));
    else if (fresh) tree.setChildren(prefix, childNodes(prefix, l));
    return l;
  }

  // Tree (left column): the shared folder tree, loading each folder once when opened.

  const tree = createTree($("art-tree"), { onSelect: (n) => { location.hash = folderHash(n.id); }, onContext: folderMenu });

  // Task collections live under tasks/<collection>/; the tree menu creates, copies, and deletes them.
  const TASKS = "tasks";
  const collectionOf = (prefix) => (/^tasks\/[^/]+$/.test(prefix) ? prefix.slice(TASKS.length + 1) : "");
  function folderMenu(n, ev) {
    const trace = traceOf(n.id);
    if (trace) {
      // A run's folder offers what its row on the Runs page offers.
      openMenu(ev, [
        ["View run", () => { location.hash = `#runs/${trace}`; }, "", "Opens the run on Runs"],
        "-",
        ["Remove run", () => removeRun(trace), "danger", "Removes this run with its artifacts"],
      ]);
      return;
    }
    const c = collectionOf(n.id);
    if (n.id === TASKS) openMenu(ev, [["New collection…", () => newCollection(), "", "Uploads files into a new collection"]]);
    else if (c) openMenu(ev, [
      ["Add files…", () => newCollection(c), "", `Uploads files into ${c}`],
      ["Duplicate collection", () => duplicateCollection(c), "", `Copies every file of ${c}`],
      "-",
      ["Delete collection", () => deleteCollection(c), "danger", `Deletes every file of ${c}`],
      "-",
      ["New collection…", () => newCollection(), "", "Uploads files into a new collection"],
    ]);
  }
  let preselect = "";
  function newCollection(collection = "") { preselect = collection; picker.click(); }
  async function filesOf(c) {
    const l = await api("GET", `/api/blobs/list/${TASKS}/${c}`);
    return l.objects.map((o) => o.id);
  }
  async function duplicateCollection(c) {
    const existing = (await Inputs.collections().catch(() => [])).map((x) => x.name);
    let name = `${c}-copy`;
    for (let n = 2; existing.includes(name); n++) name = `${c}-copy-${n}`;
    try {
      for (const id of await filesOf(c)) {
        const b = await loadBlob(id);
        const res = await fetch(`/api/blobs/blobs/${TASKS}/${name}/${id.split("/").pop()}`, { method: "PUT", headers: { ...creatorHeaders(), "Content-Type": "text/plain" }, body: b.text });
        if (!res.ok) throw new Error(errorMessage(await res.text(), res));
      }
      setSaveState("Duplicated");
      refreshAll();
      location.hash = `#artifacts/${TASKS}/${name}/`;
    } catch (e) { setStatus(`Not duplicated: ${e.message}`, "error"); }
  }
  async function deleteCollection(c) {
    if (!(await confirmAction(`Delete the collection ${c} and all its files?`, "Delete"))) return;
    try {
      for (const id of await filesOf(c)) await deleteObject(id);
      setSaveState("Deleted");
      location.hash = `#artifacts/${TASKS}/`;
      refreshAll();
    } catch (e) { setStatus(`Not deleted: ${e.message}`, "error"); }
  }
  async function removeRun(trace) {
    if (!(await confirmAction("Remove this run with its artifacts?", "Remove"))) return;
    // A removed open run hands the selection to its neighbour folder, else to the runs folder.
    const open = state.rest?.includes(trace);
    const order = childNodes(RUNS, listings.get(RUNS) || { directories: [] }).map((n) => traceOf(n.id));
    const at = order.indexOf(trace);
    const next = at < 0 ? "" : order[at + 1] || order[at - 1] || "";
    try {
      await api("DELETE", `/api/orchestrator/runs/${encodeURIComponent(trace)}`);
      runIndex.forget(trace);
      setSaveState("Removed");
      if (open) location.hash = next ? `#artifacts?trace=${encodeURIComponent(next)}` : `#artifacts/${RUNS}/`;
      refreshAll();
    } catch (e) { setStatus(`Not removed: ${e.message}`, "error"); }
  }

  async function deleteObject(id) {
    const res = await fetch(`/api/blobs/blobs/${id}`, { method: "DELETE" });
    if (!res.ok && res.status !== 404) throw new Error(errorMessage(await res.text(), res));
  }
  // A run folder shows its trace ID as stored, in mono, newest first: the folder's real and
  // unchanging name, never a workflow name a person chose and may rename. The tooltip names its
  // workflow and start time.
  const folderNode = (prefix, label) => ({
    id: prefix,
    label, mono: !!traceOf(prefix), full: traceOf(prefix) ? label : undefined,
    title: traceOf(prefix) ? runIndex.label(traceOf(prefix)) : "",
    load: async () => childNodes(prefix, await list(prefix)),
  });
  const childNodes = (prefix, l) => {
    const dirs = [...l.directories];
    if (prefix === RUNS) {
      const started = (t) => new Date(runIndex.records.get(t)?.started || 0).getTime();
      dirs.sort((a, b) => started(b) - started(a));
    }
    return dirs.map((d) => folderNode(join(prefix, d), d));
  };
  // The top-level folders are the roots; there is no root row. The "Blobstore" heading
  // links to #artifacts, which selects nothing and lists the root in the middle column.
  const loadRoots = () => list("", true);

  // selectFolder opens the tree down to prefix and marks it selected.
  async function selectFolder(prefix) {
    state.folder = prefix;
    const parts = prefix ? prefix.split("/") : [];
    await tree.reveal(parts.map((_, i) => parts.slice(0, i + 1).join("/")));
    tree.select(prefix || null);
  }

  // Middle column

  // setList fills the middle column; the tree already names what is listed, so there is no title.
  function setList(_title, ...content) {
    $("art-list").replaceChildren(...content);
    markSelected();
    renderUpload();
  }

  // Under tasks/, files are uploaded into a collection; the open collection is preselected.
  const uploadButton = iconButton("upload", "Upload files into a task collection", () => picker.click());
  const picker = h("input", { type: "file", multiple: true, accept: ".txt,.md,.json,.yaml,.yml,.csv,text/*", hidden: true });
  picker.onchange = async () => {
    const files = [...picker.files];
    picker.value = "";
    const existing = await Inputs.collections().catch(() => []);
    const chosen = preselect || (state.folder?.startsWith("tasks/") ? state.folder.split("/")[1] : "");
    preselect = "";
    // What was added opens, like a duplicate or a new entry.
    Inputs.uploadFiles(files, existing, (collection, stored) => { if (stored?.length) location.hash = `#artifacts/${stored[0]}`; else refreshAll(); }, chosen);
  };
  $("art-upload-slot").append(uploadButton, picker);
  function renderUpload() {
    uploadButton.hidden = !(state.folder === "tasks" || state.folder?.startsWith("tasks/"));
  }

  function markSelected() {
    for (const tr of $("art-list").querySelectorAll("tr[data-id]")) tr.classList.toggle("active", tr.dataset.id === state.selected);
  }

  // Columns: name, creator, created, size; sizes are right-aligned.
  const table = (first, rows) => dataTable([first, "Creator", "Created", "Size"], rows, { numeric: [3], optional: [1], name: 0 });

  function row(o, name, open) {
    const tr = clickableRow(h("tr", { dataset: { id: o.id } },
      // The copy button is a row action: it shows on the selected or hovered row.
      h("td", { className: "name-cell" }, h("div", { className: "cell-line" }, h("span", { className: "cell-text", dataset: { full: name } }, name), copyButton(o.id, "object ID"))),
      h("td", { className: "opt", dataset: { full: o.creator && o.creator !== "unknown" ? o.creator : "" } }, o.creator && o.creator !== "unknown" ? o.creator : ""),
      h("td", {}, timeEl(o.created)),
      h("td", { className: "num" }, formatSize(o.size))), open);
    if (collectionOf(parentOf(o.id))) {
      tr.addEventListener("contextmenu", (ev) => openMenu(ev, [["Delete file", async () => {
        if (!(await confirmAction(`Delete ${name}?`, "Delete"))) return;
        // A deleted open file hands the selection to its neighbour row.
        const ids = [...$("art-list").querySelectorAll("tr[data-id]")].map((x) => x.dataset.id);
        const at = ids.indexOf(o.id);
        const next = state.selected === o.id ? ids[at + 1] || ids[at - 1] : null;
        try {
          await deleteObject(o.id);
          setSaveState("Deleted");
          if (next) location.hash = `#artifacts/${next}`; else refreshAll();
        } catch (e) { setStatus(`Not deleted: ${e.message}`, "error"); }
      }, "danger", `Deletes ${o.id}`]]));
    }
    return tr;
  }

  // listError shows why a listing failed; the page tries again every 10 seconds.
  function listError(title, message) {
    setList(title, errorPanel(message));
  }

  async function showFolder(prefix, seq) {
    let l;
    try { l = await list(prefix, true); } catch (e) {
      if (seq === state.seq) listError(prefix ? `${prefix}/` : "", `Cannot list ${prefix || "the blobstore"}: ${e.message}`);
      return;
    }
    if (seq !== state.seq) return;
    await selectFolder(prefix);
    if (seq !== state.seq) return;
    const title = prefix ? `${prefix}/` : "";
    if (prefix === RUNS && !l.directories.length) {
      setList(title, emptyPanel("No runs yet"));
      return;
    }
    if (unchanged(prefix, l.objects)) return;
    const base = prefix ? prefix + "/" : "";
    const objects = l.objects.map((o) => row(o, o.id.slice(base.length), () => { location.hash = `#artifacts/${o.id}`; }));
    setList(title, objects.length ? table("Name", objects) : emptyPanel("No files")); // its folders are in the tree
    if (l.objects.length && !l.objects.some((o) => o.id === state.selected)) showObject(newest(l.objects).id);
    else if (!l.objects.length) { state.selected = ""; $("art-detail").replaceChildren(); }
  }

  const newest = (objects) => objects.reduce((a, b) => (b.created > a.created ? b : a));

  // unchanged reports whether a self-refresh found the same listing as last time.
  function unchanged(where, objects) {
    const key = where + JSON.stringify(objects.map((o) => [o.id, o.size, o.created]));
    const same = state.quiet && key === state.listKey;
    state.listKey = key;
    return same;
  }

  async function showTrace(trace, seq) {
    const title = "";
    let t;
    try { t = await traceListings.ask(trace, { owner: "artifacts" }); } catch (e) {
      if (seq === state.seq) listError(title, `Cannot load trace ${trace}: ${e.message}`);
      return;
    }
    if (seq !== state.seq) return;
    await loadRoots(); // a new run must show up in the tree without a manual refresh
    await list(RUNS, true);
    await selectFolder(`${RUNS}/${trace}`).catch(() => {});
    if (seq !== state.seq) return;
    if (!t.objects.length) {
      setList(title, emptyPanel("No artifacts yet"));
      return;
    }
    if (unchanged(trace, t.objects)) return;
    const prefix = `${RUNS}/${trace}/`;
    setList(title, table("Stage",
      t.objects.map((o) => row(o, o.id.startsWith(prefix) ? o.id.slice(prefix.length) : o.id,
        () => { location.hash = `#artifacts?trace=${encodeURIComponent(trace)}&object=${encodeURIComponent(o.id)}`; }))));
    if (!t.objects.some((o) => o.id === state.selected)) showObject(newest(t.objects).id);
  }

  // Right column

  async function showObject(id) {
    state.selected = id;
    markSelected();
    const detail = $("art-detail");
    try {
      const b = await loadBlob(id);
      if (state.selected !== id) return;
      // Name, size, time, creator, and the copy action are in the selected row; the detail is the content.
      // JSON reads as code; any other text as prose, as on Decisions, Agents, and Policies. Blank
      // lines around it (models often start with some) are left out, as on Decisions.
      const text = b.text.replace(/^(\s*\n)+/, "").trimEnd();
      const view = textView(text);
      let parsed = null;
      try { parsed = JSON.parse(text); } catch { /* ordinary text */ }
      const evidence = promptEvidenceLinks(parsed);
      const excluded = parsed?.prompt_evidence ? [
        parsed.prompt_evidence.agent_source_artifact,
        parsed.prompt_evidence.source_prompt_artifact,
        parsed.prompt_evidence.effective_prompt_artifact,
        ...(parsed.prompt_evidence.policies || []).flatMap((policy) => [policy.source_artifact, policy.snapshot_artifact]),
        ...(parsed.prompt_evidence.skills || []).map((skill) => skill.source_artifact),
      ] : [];
      fill(detail, h("div", { className: "content-format" }, view.toggle), evidence, blobReferencePanel(text, excluded), view.body);
    } catch (e) {
      if (state.selected === id) { detail.replaceChildren(errorPanel(`Cannot load ${id}: ${e.message}`)); }
    }
  }

  // show routes the hash. An object directly under runs/<trace> opens in its trace list.
  const LAST_KEY = "factory.last.artifacts";
  async function show(rest, quiet = false) {
    if (!rest && !quiet) {
      let last = "";
      try { last = localStorage.getItem(LAST_KEY) || ""; } catch { /* storage unavailable */ }
      await runIndex.refresh().catch(() => {});
      // A remembered run that was removed since gives way to the newest one.
      const lastTrace = new URLSearchParams(last.startsWith("?") ? last.slice(1) : "").get("trace");
      if (lastTrace && !runIndex.list().some((r) => r.trace_id === lastTrace)) last = "";
      if (!last) {
        const newestRun = runIndex.list()[0];
        if (newestRun) last = `?trace=${encodeURIComponent(newestRun.trace_id)}`;
      }
      if (last) { location.replace(`#artifacts${last}`); return; }
    }
    if (rest) { try { localStorage.setItem(LAST_KEY, rest); } catch { /* storage unavailable */ } }
    state.quiet = quiet;
    const seq = ++state.seq;
    state.rest = rest;
    try {
      if (state.stale) { await loadRoots(); state.stale = false; } // on tab open, and again after a failed load
      if (rest.startsWith("?")) {
        const params = new URLSearchParams(rest.slice(1));
        const trace = (params.get("trace") || "").trim();
        if (params.get("object")) showObject(params.get("object"));
        if (trace) await showTrace(trace, seq);
        else await showFolder(RUNS, seq);
        return;
      }
      const path = rest.replace(/^\/+/, "");
      const isFolder = path === "" || path.endsWith("/");
      const folder = isFolder ? path.replace(/\/+$/, "") : parentOf(path);
      if (!isFolder) showObject(path);
      if (traceOf(folder)) await showTrace(traceOf(folder), seq);
      else await showFolder(folder, seq);
    } catch (e) {
      if (seq === state.seq) listError("", `Cannot read the blobstore: ${e.message}`);
    }
  }

  // The trace field drops down the 10 most recent runs, named by workflow and time.
  const recent = h("div", { className: "ctx-menu trace-menu", role: "listbox", hidden: true });
  document.body.append(recent);
  async function showRecent() {
    try { await runIndex.refresh(); } catch { /* shows what is known */ }
    const runs = runIndex.list().slice(0, 10);
    recent.replaceChildren(...runs.map((r) => h("button", {
      type: "button", role: "option", title: r.trace_id,
      onpointerdown: (ev) => ev.preventDefault(),
      onclick: () => { recent.hidden = true; $("trace-input").blur(); location.hash = `#artifacts?trace=${encodeURIComponent(r.trace_id)}`; },
    }, runIndex.label(r.trace_id))));
    if (!runs.length || document.activeElement !== $("trace-input")) { recent.hidden = true; return; }
    const box = $("trace-input").getBoundingClientRect();
    Object.assign(recent.style, { left: `${box.left}px`, top: `${box.bottom + 4}px`, minWidth: `${box.width}px` });
    recent.hidden = false;
  }
  $("trace-input").addEventListener("focus", showRecent);
  $("trace-input").addEventListener("blur", () => { recent.hidden = true; });
  $("trace-input").addEventListener("keydown", (ev) => {
    if (ev.key === "Escape") { recent.hidden = true; ev.target.blur(); }
    if (ev.key === "ArrowDown" && !recent.hidden) { ev.preventDefault(); recent.querySelector("button")?.focus(); }
  });
  recent.addEventListener("keydown", (ev) => {
    const items = [...recent.querySelectorAll("button")];
    const i = items.indexOf(document.activeElement);
    if (ev.key === "ArrowDown") { ev.preventDefault(); items[Math.min(i + 1, items.length - 1)]?.focus(); }
    if (ev.key === "ArrowUp") { ev.preventDefault(); if (i <= 0) $("trace-input").focus(); else items[i - 1].focus(); }
    if (ev.key === "Escape") { recent.hidden = true; $("trace-input").focus(); }
  });
  recent.addEventListener("focusout", (ev) => { if (!recent.contains(ev.relatedTarget) && ev.relatedTarget !== $("trace-input")) recent.hidden = true; });

  $("trace-form").addEventListener("submit", (ev) => {
    ev.preventDefault();
    const trace = $("trace-input").value.trim();
    if (trace) location.hash = `#artifacts?trace=${encodeURIComponent(trace)}`;
  });

  const refreshAll = () => {
    listings.clear();
    tree.invalidate();
    state.stale = true;
    const hash = location.hash.slice(1);
    show(hash.slice("artifacts".length));
  };

  tabs.artifacts = {
    show(rest) {
      show(rest);
      state.poll ??= setInterval(() => { if (!document.hidden && activeTab === "artifacts" && menu.hidden) show(state.rest, true); }, 10000);
    },
    hide() {
      state.stale = true;
      clearInterval(state.poll);
      state.poll = null;
    },
  };
})();
