"use strict";
// Select Inputs: the dialog that starts runs of a workflow. The input is either text, a
// task collection (one run per file under tasks/<collection>/), or a previous run's
// input. A template from templates/tasks/ wraps every input: {{task}} is replaced by the
// input, or the input is appended after a blank line when the template has no {{task}}.
// The last choice is remembered per workflow so "Run workflow" can repeat it.

const Inputs = (() => {
  const TASKS = "tasks";
  const TEMPLATES = "templates/tasks/";
  const DEFAULT_TEMPLATE = "default";
  const LAST_KEY = "factory.lastInput.";
  const TEXT_EXTENSIONS = /\.(txt|md|markdown|json|ya?ml|toml|ini|cfg|conf|csv|tsv|log|xml|html?|css|js|mjs|ts|tsx|jsx|go|py|rb|rs|java|kt|c|h|cpp|hpp|cs|sh|bash|zsh|sql|diff|patch|env|properties|gradle|tf|proto|graphql|rst|adoc|tex)$/i;

  // blocker is set by the Workflows page: a reason runs cannot start now, or "".
  let blocker = () => "";

  const sanitize = (name) => name.trim().replace(/[^A-Za-z0-9._-]/g, "-").replace(/^[.-]+/, "");
  const templateId = (name) => `${TEMPLATES}${name}.md`;

  function isText(file) {
    const t = file.type;
    return t.startsWith("text/") || /^application\/(json|.*\+json|xml|.*\+xml|x-yaml|yaml|javascript|x-sh|toml|sql)$/.test(t)
      || ((!t || t === "application/octet-stream") && TEXT_EXTENSIONS.test(file.name));
  }

  function last(workflow) {
    try { return JSON.parse(localStorage.getItem(LAST_KEY + workflow)) || null; } catch { return null; }
  }
  function remember(workflow, choice) {
    try { localStorage.setItem(LAST_KEY + workflow, JSON.stringify(choice)); } catch { /* storage unavailable */ }
  }

  // compose wraps one input in the template; both are trimmed.
  function compose(template, input) {
    const t = template.trim(), body = input.trim();
    if (!t) return body;
    if (t.includes("{{task}}")) return t.split("{{task}}").join(body).trim();
    return `${t}\n\n${body}`;
  }

  // collections lists tasks/<collection>/ folders with their files.
  async function collections() {
    let root;
    try { root = await api("GET", `/api/blobs/list/${TASKS}`); } catch (e) {
      if (/not found/i.test(e.message)) return [];
      throw e;
    }
    const out = await pool(root.directories, 6, async (name) => {
      const l = await api("GET", `/api/blobs/list/${TASKS}/${name}`).catch(() => ({ objects: [] }));
      return { name, files: l.objects.map((o) => o.id).sort() };
    });
    return out;
  }

  async function templates() {
    try {
      const l = await api("GET", `/api/blobs/list/${TEMPLATES.slice(0, -1)}`);
      return l.objects.map((o) => o.id.slice(TEMPLATES.length).replace(/\.md$/, ""))
        .sort((a, b) => (a === DEFAULT_TEMPLATE ? -1 : b === DEFAULT_TEMPLATE ? 1 : a.localeCompare(b)));
    } catch (e) {
      if (/not found/i.test(e.message)) return [DEFAULT_TEMPLATE];
      throw e;
    }
  }

  async function templateText(name) {
    try { return (await loadBlob(templateId(name))).text; } catch { return name === DEFAULT_TEMPLATE ? "{{task}}" : ""; }
  }

  // resolve turns a choice into the list of inputs to run: [{ text }].
  async function resolve(choice) {
    if (choice.mode === "text") return [{ text: choice.text || "" }];
    if (choice.mode === "previous") {
      const b = await loadBlob(`runs/${choice.previous}/input`);
      return [{ text: b.text }];
    }
    const all = await collections();
    const c = all.find((x) => x.name === choice.collection);
    if (!c) throw new Error(`There is no task collection named ${choice.collection}`);
    return pool(c.files, 6, async (id) => ({ text: (await loadBlob(id)).text }));
  }

  // start runs the workflow once per input, records how each run was started, and
  // remembers the choice. Returns the number of runs started.
  // version is the workflow version to run; 0 runs the newest.
  async function start(workflow, choice, template, version = 0) {
    const inputs = await resolve(choice);
    const bodies = inputs.map((i) => ({ ...i, body: compose(template, i.text) })).filter((i) => i.body);
    if (!bodies.length) throw new Error("The input is empty");
    const url = `/api/orchestrator/workflows/${encodeURIComponent(workflow)}/runs${version > 0 ? `?version=${version}` : ""}`;
    let started = 0;
    for (const i of bodies) {
      const run = await api("POST", url, i.body, creatorHeaders());
      started++;
    }
    remember(workflow, choice);
    runIndex.refresh().catch(() => {});
    setSaveState(started === 1 ? "Started" : `Started ${started} runs`);
    document.dispatchEvent(new CustomEvent("runsstarted", { detail: { workflow } }));
    return started;
  }

  // describe says what "Run workflow" will do, for its tooltip.
  function describe(workflow) {
    const c = last(workflow);
    if (!c) return "Opens Select Inputs";
    const what = c.mode === "collection" ? `collection ${c.collection}` : c.mode === "previous" ? "the same previous input" : "the same text";
    return `Runs again with ${what} and template ${c.template || DEFAULT_TEMPLATE}`;
  }

  // runAgain repeats the remembered choice, or opens the dialog when there is none.
  async function runAgain(workflow, version = 0) {
    const c = last(workflow);
    if (!c) { open(workflow, version); return; }
    if (blocker()) { setStatus(blocker(), "error"); return; }
    try {
      await start(workflow, c, await templateText(c.template || DEFAULT_TEMPLATE), version);
    } catch (e) { setStatus(`Not started: ${e.message}`, "error"); }
  }

  // uploadFiles asks which collection to add text files to, then stores them there.
  async function uploadFiles(files, existing, onDone, preselect = "") {
    const refused = files.filter((f) => !isText(f)).map((f) => f.name);
    const texts = files.filter(isText);
    if (refused.length) setStatus(`Not added: ${refused.map((n) => `${n} (not a text file)`).join(", ")}`, "error");
    if (!texts.length) return;
    const names = existing.map((c) => c.name);
    const NEW = "\u0000new";
    let chosen = names.includes(preselect) ? preselect : names[0] || NEW;
    const newName = h("input", { id: "new-collection-name", autocomplete: "off" });
    const problem = h("p", { className: "error-text", role: "alert" });
    const newField = h("div", { className: "field", hidden: chosen !== NEW }, h("label", { htmlFor: newName.id }, "Name"), newName, problem);
    const list = h("ul", { className: "pick-list", role: "radiogroup", "aria-label": "Collection" });
    const renderChoices = () => list.replaceChildren(...[...names, NEW].map((n) => {
      const li = h("li", { role: "radio", tabIndex: 0, "aria-checked": String(n === chosen) },
        h("span", { className: "grow" }, n === NEW ? "New collection…" : n));
      const pick = () => { chosen = n; newField.hidden = n !== NEW; renderChoices(); if (n === NEW) newName.focus(); };
      li.addEventListener("click", pick);
      li.addEventListener("keydown", (ev) => { if (ev.key === "Enter" || ev.key === " ") { ev.preventDefault(); pick(); } });
      return li;
    }));
    renderChoices();
    const upload = h("button", { type: "button", className: "primary", title: `Uploads ${texts.length} file${texts.length === 1 ? "" : "s"}` }, "Upload");
    const dlg = openDialog({
      title: "Add to collection",
      body: h("div", { className: "choose-collection" }, list, newField),
      footer: [h("button", { type: "button", onclick: () => dlg.close() }, "Cancel"), upload],
    });
    newName.addEventListener("keydown", (ev) => { if (ev.key === "Enter") upload.click(); });
    upload.onclick = async () => {
      const collection = chosen === NEW ? sanitize(newName.value) : chosen;
      if (!collection) { problem.textContent = "Enter a name."; newName.focus(); return; }
      upload.disabled = true;
      const failed = [], stored = [];
      for (const file of texts) {
        const id = `${TASKS}/${collection}/${sanitize(file.name) || "task.txt"}`;
        try {
          const res = await fetch(`/api/blobs/blobs/${id}`, { method: "PUT", headers: { ...creatorHeaders(), "Content-Type": "text/plain" }, body: file });
          if (!res.ok) throw new Error(errorMessage(await res.text(), res));
          stored.push(id);
        } catch (e) { failed.push(`${file.name} (${e.message})`); }
      }
      dlg.close();
      if (failed.length) setStatus(`Not added: ${failed.join(", ")}`, "error");
      else setSaveState(texts.length === 1 ? "Added" : `Added ${texts.length} files`);
      onDone(collection, stored);
    };
  }

  // open shows Select Inputs for a workflow; its runs use that version (0: the newest).
  function open(workflow, version = 0) {
    const remembered = last(workflow) || {};
    const st = {
      mode: remembered.mode || "text",
      text: remembered.mode === "text" ? remembered.text || "" : "",
      collection: remembered.collection || "",
      previous: remembered.previous || "",
      template: remembered.template || DEFAULT_TEMPLATE,
      collections: null,
      busy: false,
    };

    // Mode control.
    const modes = [["text", "Text"], ["collection", "Task collection"], ["previous", "Previous input"]];
    const segmented = h("div", { className: "segmented inputs-mode", role: "radiogroup", "aria-label": "Input" });
    const panes = {
      text: h("div"),
      collection: h("div"),
      previous: h("div"),
    };
    const renderMode = () => {
      segmented.replaceChildren(...modes.map(([m, label]) => h("button", {
        type: "button", role: "radio", "aria-checked": String(st.mode === m), onclick: () => { st.mode = m; renderMode(); segmented.querySelector("[aria-checked=true]").focus(); },
      }, label)));
      for (const [m, pane] of Object.entries(panes)) pane.hidden = m !== st.mode;
      update();
    };

    // Text.
    const text = h("textarea", { className: "inputs-text", "aria-label": "Input text", value: st.text });
    text.addEventListener("input", () => { st.text = text.value; update(); });
    // Upload file opens the file dialog and puts the text of the chosen file in the field; a
    // file dropped on the field does the same.
    const takeFile = async (file) => {
      if (!file) return;
      if (!isText(file)) { setStatus(`Not added: ${file.name} (not a text file)`, "error"); return; }
      try {
        text.value = st.text = await file.text();
        update();
        text.focus();
      } catch (e) { setStatus(`Cannot read ${file.name}: ${e.message}`, "error"); }
    };
    const textPicker = h("input", { type: "file", hidden: true });
    const textUpload = h("button", { type: "button", title: "Replaces the input text with the text of a file; a file dropped on the field does the same" }, icon("upload"), " Upload file");
    textUpload.onclick = () => textPicker.click();
    textPicker.onchange = () => { const file = textPicker.files[0]; textPicker.value = ""; takeFile(file); };
    // Only a dragged file is taken over: dragged text still drops into the field as usual.
    const dragsFile = (ev) => [...(ev.dataTransfer?.types || [])].includes("Files");
    text.addEventListener("dragover", (ev) => { if (!dragsFile(ev)) return; ev.preventDefault(); text.classList.add("drop"); });
    text.addEventListener("dragleave", () => text.classList.remove("drop"));
    text.addEventListener("drop", (ev) => {
      if (!dragsFile(ev)) return;
      ev.preventDefault();
      text.classList.remove("drop");
      takeFile(ev.dataTransfer.files[0]);
    });
    panes.text.append(text, h("div", { className: "row pane-actions" }, textUpload, textPicker));

    // Task collection.
    const collectionList = h("ul", { className: "pick-list", role: "radiogroup", "aria-label": "Task collections" });
    const files = h("div", { className: "pick-files" });
    const picker = h("input", { type: "file", multiple: true, hidden: true });
    const uploadButton = h("button", { type: "button", title: "Uploads text files into a collection under tasks/" }, icon("upload"), " Upload files");
    uploadButton.onclick = () => picker.click();
    const afterUpload = async (name) => { st.collection = name; await loadCollections(); };
    picker.onchange = () => { const list = [...picker.files]; picker.value = ""; uploadFiles(list, st.collections || [], afterUpload); };
    collectionList.addEventListener("dragover", (ev) => { ev.preventDefault(); collectionList.classList.add("drop"); });
    collectionList.addEventListener("dragleave", () => collectionList.classList.remove("drop"));
    collectionList.addEventListener("drop", (ev) => {
      ev.preventDefault();
      collectionList.classList.remove("drop");
      uploadFiles([...ev.dataTransfer.files], st.collections || [], afterUpload);
    });
    // The secondary action sits under what it adds to, left-aligned, like Save as… under the template.
    panes.collection.append(collectionList, files, h("div", { className: "row pane-actions" }, uploadButton, picker));

    const renderCollections = () => {
      if (!st.collections) { collectionList.replaceChildren(); return; }
      if (!st.collections.length) {
        collectionList.replaceChildren(h("li", { className: "muted" }, "No collections yet"));
        files.replaceChildren();
        return;
      }
      collectionList.replaceChildren(...st.collections.map((c) => {
        const li = h("li", { role: "radio", tabIndex: 0, "aria-checked": String(c.name === st.collection) },
          h("span", { className: "grow row-title" }, c.name));
        const pick = () => { st.collection = c.name; renderCollections(); update(); }; // a radio: a second click keeps it chosen
        li.addEventListener("click", pick);
        li.addEventListener("keydown", (ev) => { if (ev.key === "Enter" || ev.key === " ") { ev.preventDefault(); pick(); } });
        return li;
      }));
      const c = st.collections.find((x) => x.name === st.collection);
      files.replaceChildren(...(c ? c.files.map((id) => h("span", {}, id.split("/").pop())) : []));
    };
    async function loadCollections() {
      st.collections = null;
      renderCollections();
      try {
        st.collections = await collections();
        if (!st.collections.some((c) => c.name === st.collection)) st.collection = "";
      } catch (e) {
        st.collections = [];
        collectionList.replaceChildren(h("li", {}, errorState(e.message, loadCollections)));
        return;
      }
      renderCollections();
      update();
    }

    // Previous input.
    const previousList = h("ul", { className: "pick-list", role: "radiogroup", "aria-label": "Previous inputs" });
    panes.previous.append(previousList);
    const previews = new Map(); // trace -> preview text; null when the run has no input to repeat
    const inputs = new Map(); // trace -> its input once loaded: an input run again is listed once, at its newest run
    const renderPrevious = () => {
      const newest = new Map(); // input -> the newest listed run of it
      const runs = runIndex.list().slice(0, 50).filter((r) => {
        if (previews.get(r.trace_id) === null) return false;
        const input = inputs.get(r.trace_id);
        if (input === undefined) return true;
        if (newest.has(input)) { if (st.previous === r.trace_id) st.previous = newest.get(input); return false; }
        newest.set(input, r.trace_id);
        return true;
      });
      if (!runs.length) { previousList.replaceChildren(h("li", { className: "muted" }, "No runs yet")); return; }
      previousList.replaceChildren(...runs.map((r) => {
        // One line of the input; cut off, it shows in full on hover.
        const preview = h("span", { className: "meta preview", dataset: { trace: r.trace_id, full: previews.get(r.trace_id) ?? "" } }, previews.get(r.trace_id) ?? "");
        const li = h("li", { role: "radio", tabIndex: 0, "aria-checked": String(r.trace_id === st.previous) },
          h("span", { className: "grow" },
            h("span", { className: "row-title" }, titleCase(r.workflow) || midTruncate(r.trace_id, 13), h("span", { className: "meta" }, " · ", timeEl(r.started))),
            preview));
        const pick = () => { st.previous = r.trace_id; renderPrevious(); update(); };
        li.addEventListener("click", pick);
        li.addEventListener("keydown", (ev) => { if (ev.key === "Enter" || ev.key === " ") { ev.preventDefault(); pick(); } });
        if (!previews.has(r.trace_id)) {
          previews.set(r.trace_id, "");
          const drop = () => {
            previews.set(r.trace_id, null);
            if (st.previous === r.trace_id) st.previous = "";
            renderPrevious();
            update();
          };
          loadBlob(`runs/${r.trace_id}/input`).then((b) => {
            const t = b.text.replace(/\s+/g, " ").trim();
            if (!t) { drop(); return; } // an empty input cannot start a run
            previews.set(r.trace_id, t.length > 600 ? `${t.slice(0, 600)}…` : t); // a task file can be long
            const repeated = [...inputs.values()].includes(b.text);
            inputs.set(r.trace_id, b.text);
            if (repeated) { renderPrevious(); return; }
            // The list may have been redrawn while this loaded: fill the row that shows it now.
            const shown = [...previousList.querySelectorAll(".meta[data-trace]")].find((e) => e.dataset.trace === r.trace_id);
            if (shown) shown.textContent = shown.dataset.full = previews.get(r.trace_id);
          }, drop);
        }
        return li;
      }));
    };

    // Template.
    const templateToggle = h("button", { type: "button", className: "template-toggle", "aria-expanded": "false" }, icon("triangle", 12), h("span"));
    const templateSelect = h("select", { "aria-label": "Template", className: "fit" });
    const templateEditor = h("textarea", { "aria-label": "Template text", spellcheck: false });
    const saveAs = h("button", { type: "button", title: "Saves the template text under a new name" }, "Save as…");
    const templateBody = h("div", { className: "template-body", hidden: true }, templateSelect, templateEditor, h("div", { className: "row" }, saveAs));
    // Closed, the toggle names the template; open, the select below names it.
    const setToggleLabel = () => { templateToggle.lastChild.textContent = templateBody.hidden ? `Template: ${st.template}` : "Template"; };
    templateToggle.onclick = () => {
      const openNow = templateBody.hidden;
      templateBody.hidden = !openNow;
      templateToggle.setAttribute("aria-expanded", String(openNow));
      setToggleLabel();
    };
    const loadTemplate = async (name) => {
      st.template = name;
      setToggleLabel();
      templateEditor.value = await templateText(name);
    };
    templateSelect.onchange = () => loadTemplate(templateSelect.value);
    const refreshTemplates = async () => {
      let names = [DEFAULT_TEMPLATE];
      try { names = await templates(); } catch (e) { setStatus(`Cannot list templates: ${e.message}`, "error"); }
      if (!names.includes(st.template)) st.template = DEFAULT_TEMPLATE;
      templateSelect.replaceChildren(...names.map((n) => h("option", { value: n, selected: n === st.template }, n)));
      return names;
    };
    saveAs.onclick = () => {
      const name = h("input", { id: "template-name", autocomplete: "off", value: st.template === DEFAULT_TEMPLATE ? "" : st.template });
      const save = h("button", { type: "button", className: "primary" }, "Save");
      const problem = h("p", { className: "error-text", role: "alert" });
      const d = openDialog({ title: "Save template", body: h("div", { className: "field" }, h("label", { htmlFor: name.id }, "Name"), name, problem), footer: [h("button", { type: "button", onclick: () => d.close() }, "Cancel"), save] });
      name.addEventListener("keydown", (ev) => { if (ev.key === "Enter") save.click(); });
      save.onclick = async () => {
        const n = sanitize(name.value).replace(/\.md$/, "");
        if (!n) { problem.textContent = "Enter a name."; name.focus(); return; }
        const names = await refreshTemplates();
        if (names.includes(n)) { problem.textContent = `${n} already exists.`; name.focus(); return; }
        try {
          const res = await fetch(`/api/blobs/blobs/${templateId(n)}`, { method: "PUT", headers: { ...creatorHeaders(), "Content-Type": "text/plain" }, body: templateEditor.value.trim() });
          if (!res.ok) throw new Error(errorMessage(await res.text(), res));
        } catch (e) { setStatus(`Not saved: ${e.message}`, "error"); return; }
        d.close();
        st.template = n;
        await refreshTemplates();
        setToggleLabel();
        setSaveState("Saved");
      };
    };

    // Footer.
    const runButton = h("button", { type: "button", className: "primary" }, "Run");
    const cancel = h("button", { type: "button" }, "Cancel");

    function count() {
      if (st.mode === "text") return st.text.trim() ? 1 : 0;
      if (st.mode === "previous") return st.previous ? 1 : 0;
      return st.collections?.find((c) => c.name === st.collection)?.files.length || 0;
    }
    function update() {
      const n = count();
      const why = blocker()
        || (n ? "" : st.mode === "text" ? "Needs an input text"
          : st.mode === "collection" ? (st.collection ? "This collection has no files" : "Needs a task collection")
            : "Needs a previous input");
      runButton.disabled = !!why || st.busy;
      runButton.title = why || `Starts ${n} run${n === 1 ? "" : "s"}`;
    }

    const dialog = openDialog({
      title: "Select Inputs",
      width: 720,
      body: h("div", {}, segmented, panes.text, panes.collection, panes.previous, h("div", { className: "template-section" }, templateToggle, templateBody)),
      footer: [cancel, runButton],
      // Closed without running (Cancel, Escape, a click outside), the choice stays as a draft, so
      // typed text is never lost; it only fills the dialog the next time.
      onClose: () => { if (!st.busy) remember(workflow, { mode: st.mode, template: st.template, text: st.text, collection: st.collection, previous: st.previous }); },
    });
    cancel.onclick = () => dialog.close();
    runButton.onclick = async () => {
      if (runButton.disabled) return;
      st.busy = true;
      update();
      const choice = { mode: st.mode, template: st.template, text: st.mode === "text" ? st.text : undefined, collection: st.mode === "collection" ? st.collection : undefined, previous: st.mode === "previous" ? st.previous : undefined };
      try {
        await start(workflow, choice, templateEditor.value, version);
        dialog.close();
      } catch (e) {
        setStatus(`Not started: ${e.message}`, "error");
      } finally {
        st.busy = false;
        update();
      }
    };
    text.addEventListener("keydown", (ev) => { if (ev.key === "Enter" && (ev.metaKey || ev.ctrlKey)) runButton.click(); });

    setToggleLabel();
    renderMode();
    loadCollections();
    renderPrevious();
    runIndex.refresh().then(renderPrevious, () => {});
    refreshTemplates().then(() => loadTemplate(st.template));
  }

  return {
    open,
    runAgain,
    describe,
    compose,
    collections,
    uploadFiles,
    setBlocker(fn) { blocker = fn; },
  };
})();
