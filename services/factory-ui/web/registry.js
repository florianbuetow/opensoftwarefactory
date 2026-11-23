"use strict";
// registryTab builds a page over a versioned registry (agents or policies): a tree
// sidebar of folders, entries, and their versions, and the files of the selected
// version. Every edit is saved as a new version from the action row under the files.
// Names are paths: "translation/en-2-de" is the entry en-2-de in the folder translation.
// The sidebar's right-click menu creates folders and entries, duplicates, and deletes.
//
//   tab    the page name and hash, such as "agents"
//   base   the API path of the entries, such as "/api/agents/agents"
//   noun   what one entry is called, such as "agent"
//   files  [{ name, json, template(name), label, mono }] the files of every version

// Registry paths are intentionally portable: lowercase ASCII letters, digits, dashes, and
// slashes between group segments. Normalize at the input boundary so invalid characters never
// remain in the field after typing or paste. Composition is left alone until the IME commits.
function constrainRegistryName(input, allowSlash = false) {
  let composing = false;
  const sanitize = (value) => String(value).toLowerCase().replace(allowSlash ? /[^a-z0-9\-/]/g : /[^a-z0-9-]/g, "");
  const apply = () => {
    const value = input.value;
    const start = input.selectionStart ?? value.length;
    const end = input.selectionEnd ?? start;
    const next = sanitize(value);
    if (next === value) return;
    const nextStart = sanitize(value.slice(0, start)).length;
    const nextEnd = sanitize(value.slice(0, end)).length;
    input.value = next;
    input.setSelectionRange?.(nextStart, nextEnd);
  };
  input.addEventListener("compositionstart", () => { composing = true; });
  input.addEventListener("compositionend", () => { composing = false; apply(); });
  input.addEventListener("input", () => { if (!composing) apply(); });
  apply();
  return input;
}

const policySegment = "[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?";
const policyIDPattern = new RegExp(`^${policySegment}(?:/${policySegment}){0,3}$`);

function policyReferences(text) {
  const refs = [];
  let at = 0;
  while ((at = text.indexOf("{{policy::", at)) >= 0) {
    const end = text.indexOf("}}", at + 10);
    if (end < 0) throw new Error("A policy reference is missing }}.");
    const id = text.slice(at + 10, end);
    if (!policyIDPattern.test(id)) throw new Error(`Invalid policy reference ${text.slice(at, end + 2)}.`);
    if (!refs.includes(id)) refs.push(id);
    at = end + 2;
  }
  return refs;
}

function insertPolicyReference(area, id, start = area.selectionStart, end = area.selectionEnd) {
  const reference = `{{policy::${id}}}`;
  const from = Number.isInteger(start) ? start : 0;
  const to = Number.isInteger(end) ? end : from;
  area.setRangeText(reference, from, to, "end");
  area.dispatchEvent(new Event("input", { bubbles: true }));
  return reference;
}

async function validatePolicyPrompt(text, fetchPolicy) {
  for (const id of policyReferences(text)) {
    try { await fetchPolicy(id); }
    catch (error) {
      if (error.status === 404) throw new Error(`Required policy ${id} does not exist.`);
      throw new Error(`Cannot validate required policy ${id}: ${error.message}`);
    }
  }
}

function openPolicyReferencePicker(area, selection, loadIDs, afterInsert = () => {}) {
  const select = h("select", { id: "agent-policy-reference", disabled: true }, h("option", {}, "Loading…"));
  const problem = h("p", { className: "error-text", role: "alert" });
  const add = h("button", { type: "button", className: "primary", disabled: true }, "Add policy");
  const cancel = h("button", { type: "button" }, "Cancel");
  const dlg = openDialog({ title: "Add policy", body: h("div", { className: "field" }, h("label", { htmlFor: select.id }, "Policy"), select, problem), footer: [cancel, add] });
  cancel.onclick = () => dlg.close();
  add.onclick = () => {
    if (!select.value) return;
    setTextViewMode("text");
    insertPolicyReference(area, select.value, selection.start, selection.end);
    dlg.close();
    afterInsert();
  };
  Promise.resolve(loadIDs()).then((values) => {
    const ids = [...new Set(values.filter(Boolean))].sort();
    select.replaceChildren(h("option", { value: "" }, ids.length ? "Select policy" : "No policies"), ...ids.map((id) => h("option", { value: id }, id)));
    select.disabled = !ids.length;
    add.disabled = !ids.length;
  }, (error) => { problem.textContent = `Cannot list policies: ${error.message}`; });
  return dlg;
}

async function runExclusive(state, control, operation) {
  if (state.busy) return false;
  state.busy = true;
  if (control) control.disabled = true;
  try { await operation(); return true; }
  finally { state.busy = false; if (control?.isConnected) control.disabled = false; }
}

function registryTab({ tab, base, noun, files, fileTree }) {
  const LAST_KEY = `factory.last.${tab}`;
  const plural = `${noun[0].toUpperCase()}${noun.slice(1)}s`.replace("Policys", "Policies");
  // draft is a new version being written ({ name, version }): shown in the tree, stored when saved.
  const state = { entries: [], name: "", version: "", seq: 0, versions: new Map(), unsaved: false, folders: new Set(), draft: null };
  const page = $(`tab-${tab}`);
  const treeBox = h("ul");
  const detail = h("div", { className: "panel-body registry-detail" });
  page.append(
    h("aside", { className: "panel list-panel" },
      h("div", { className: "panel-head" }, h("h2", {}, plural), iconButton("plus", `New ${noun}`, () => showNewForm())),
      h("div", { className: "panel-body", oncontextmenu: (ev) => { if (!ev.target.closest(".tree-row")) openMenu(ev, createItems("")); } }, treeBox)),
    h("div", { className: "panel content-panel" }, h("div", { className: "panel-head" }), detail),
  );

  const remember = (name) => { try { localStorage.setItem(LAST_KEY, name); } catch { /* storage unavailable */ } };
  const lastUsed = () => { try { return localStorage.getItem(LAST_KEY) || ""; } catch { return ""; } };
  const enc = (name) => name.split("/").map(encodeURIComponent).join("/");
  const isVersion = (s) => /^v[1-9][0-9]*$/.test(s);
  const SEGMENT = "[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?";
  const validName = (n) => new RegExp(`^${SEGMENT}(/${SEGMENT}){0,3}$`).test(n) && !n.split("/").some((s) => isVersion(s) || s === "latest");

  // Tree ids: "f:<folder>", "n:<name>", "v:<name>@<version>", and "d:<name>" for a draft version.
  const nameOf = (id) => (id.startsWith("v:") ? id.slice(2).split("@")[0] : id.startsWith("n:") ? id.slice(2) : "");
  const versionOf = (id) => (id.startsWith("v:") ? id.slice(2).split("@")[1] : "");

  // ---- Sidebar tree: folders, entries, and versions (newest first) ----

  const tree = createTree(treeBox, {
    onSelect(n) { if (!n.id.startsWith("f:") && !n.id.startsWith("d:")) go(nameOf(n.id), versionOf(n.id)); },
    onContext(n, ev) {
      if (n.id.startsWith("d:")) return; // the draft has no actions; Cancel discards it
      if (n.id.startsWith("f:")) folderMenu(n.id.slice(2), ev);
      else entryMenu(nameOf(n.id), versionOf(n.id), ev);
    },
  });

  // folderPaths returns every folder that holds an entry, plus folders created in this session.
  const folderPaths = () => groupPaths(state.entries.map((e) => e.name), state.folders);

  function renderTree() {
    tree.setRoots(groupNodes(folderPaths(), state.entries, (e) => e.name,
      (f, children) => ({ id: `f:${f}`, label: leafOf(f), children }),
      (e) => ({
        id: `n:${e.name}`, label: leafOf(e.name),
        children: [
          ...(state.draft?.name === e.name ? [{ id: `d:${e.name}`, label: state.draft.version, title: "Not stored until saved", className: "draft" }] : []),
          ...(state.versions.get(e.name) || []).map((v) => ({ id: `v:${e.name}@${v.version}`, label: v.version, title: fullTime(v.created) })),
        ],
      })));
  }

  // createItems are the "New" actions inside a group ("" is the top level). Every menu ends
  // with them, so the actions on what was clicked come first and "New group…" is last.
  function createItems(folder) {
    return [
      [`New ${noun}`, () => showNewForm(folder ? { group: folder } : null), "", ""],
      ["New group…", () => newFolder(folder), "", ""],
    ];
  }

  function folderMenu(folder, ev) {
    const inside = state.entries.filter((e) => e.name.startsWith(`${folder}/`));
    openMenu(ev, [
      ["Delete group", () => removeFolder(folder, inside), "danger", inside.length ? `Deletes ${folder} with its ${inside.length} ${inside.length === 1 ? noun : plural.toLowerCase()}` : `Deletes ${folder}`],
      "-",
      ...createItems(folder),
    ]);
  }

  function entryMenu(name, version, ev) {
    const from = version || state.versions.get(name)?.[0]?.version || "";
    openMenu(ev, [
      ["New version", () => newVersion(name, from), "", `Starts the next version from ${from}; it is stored when saved`],
      [`Duplicate ${noun}`, () => duplicate(name, version), "", `Copies ${name}${version ? " " + version : ""} as v1 of a new ${noun}`],
      [`Rename ${noun}…`, () => move(name), "", "Renames it with every version, or moves it into another group"],
      "-",
      // On a version row, its own Delete comes first; Delete agent still removes every version.
      // An only version has none: deleting it would delete the agent, which Delete agent already says.
      ...(version && (state.versions.get(name) || []).length > 1 ? [["Delete version", () => removeVersion(name, version), "danger", `Deletes ${version} of ${name}; the other versions stay`]] : []),
      [`Delete ${noun}`, () => remove(name), "danger", `Deletes ${name} with every version`],
      "-",
      ...createItems(groupOf(name)),
    ]);
  }

  // usedBy names the workflows whose steps pin the entry (any version, or the given one).
  async function usedBy(name, version = "") {
    if (tab !== "agents") return [];
    try {
      const cfg = await api("GET", "/api/orchestrator/workflows");
      return cfg.workflows.filter((w) => w.steps.some((s) => {
        const [n, v] = (s.agent || "").split("@");
        return n === name && (!version || v === version);
      })).map((w) => titleCase(w.name));
    } catch { return []; }
  }

  async function removeVersion(name, version) {
    const users = await usedBy(name, version);
    if (!(await confirmAction(`Delete ${name} ${version}${users.length ? `, used by ${users.join(" and ")}` : ""}?`, "Delete"))) return;
    // The selection moves to the neighbour version: the next older, else the newer one.
    const versions = (state.versions.get(name) || []).map((v) => v.version);
    const at = versions.indexOf(version);
    const next = versions[at + 1] || versions[at - 1] || "";
    try {
      await api("DELETE", `${base}/${enc(name)}/${encodeURIComponent(version)}`);
      state.versions.delete(name);
      state.unsaved = false;
      setSaveState("Deleted");
      const target = state.name === name && state.version === version ? (next ? `#${tab}/${name}/${next}` : `#${tab}`) : location.hash;
      if (target !== location.hash) location.hash = target; else show(location.hash.slice(tab.length + 1));
    } catch (e) { setStatus(`Not deleted: ${e.message}`, "error"); }
  }

  // newFolder asks for a name and adds the folder; it is stored once it holds an entry.
  function newFolder(parent) {
    const input = constrainRegistryName(h("input", { autocomplete: "off", id: `${tab}-folder-name` }));
    const problem = h("p", { className: "error-text", role: "alert" });
    const create = h("button", { type: "button", className: "primary" }, "Create group");
    const dlg = openDialog({
      title: "New group",
      body: h("div", { className: "field" }, h("label", { htmlFor: input.id }, "Name"), input, problem),
      footer: [h("button", { type: "button", onclick: () => dlg.close() }, "Cancel"), create],
    });
    const submit = () => {
      const path = parent ? `${parent}/${input.value.trim()}` : input.value.trim();
      if (!validName(path)) { problem.textContent = "Use lowercase letters, digits, and dashes."; input.focus(); return; }
      if (state.entries.some((e) => e.name === path)) { problem.textContent = `${path} is a ${noun}.`; input.focus(); return; }
      state.folders.add(path);
      dlg.close();
      renderTree();
      tree.reveal(path.split("/").map((_, i, parts) => `f:${parts.slice(0, i + 1).join("/")}`)).then(() => tree.select(`f:${path}`));
    };
    create.onclick = submit;
    input.addEventListener("keydown", (ev) => { if (ev.key === "Enter") submit(); });
    input.focus();
  }

  // move renames an entry (into a folder, for example) with every version, keeping each
  // version's number and creator; workflows that pin an agent follow it.
  function move(name) {
    const id = nameFields(`${tab}-move`, name);
    const button = h("button", { type: "button", className: "primary" }, "Rename");
    const dlg = openDialog({ title: `Rename ${noun}`, body: id.row, footer: [h("button", { type: "button", onclick: () => dlg.close() }, "Cancel"), button] });
    const submit = async () => {
      if (button.disabled) return; // a rename is running: a second Enter would move the versions twice
      if (id.full() === name) { dlg.close(); return; }
      const target = id.check(name);
      if (!target) return;
      button.disabled = true;
      try {
        await moveEntry(name, target);
        dlg.close();
        setSaveState("Renamed");
        location.hash = `#${tab}/${target}`;
      } catch (e) { setStatus(`Not renamed: ${e.message}`, "error"); button.disabled = false; }
    };
    button.onclick = submit;
    for (const c of [id.group, id.name]) c.addEventListener("keydown", (ev) => { if (ev.key === "Enter") submit(); });
    // Renaming starts in the name with it selected, as Rename workflow does (after the dialog focuses its first field).
    requestAnimationFrame(() => { id.name.focus(); id.name.select(); });
  }

  // nameFields asks for a group and a name. The group is required (nested groups are separated
  // with /); the name has no slashes. Each field's problem shows under it.
  function nameFields(idPrefix, value = "") {
    const parts = value.split("/");
    const group = constrainRegistryName(h("input", { autocomplete: "off", id: `${idPrefix}-group`, value: parts.length > 1 ? parts.slice(0, -1).join("/") : "" }), true);
    group.setAttribute("list", `${idPrefix}-groups`); // a read-only property, so set as an attribute
    const name = constrainRegistryName(h("input", { autocomplete: "off", id: `${idPrefix}-name`, value: parts[parts.length - 1] }));
    const groupProblem = h("p", { className: "error-text", role: "alert" });
    const nameProblem = h("p", { className: "error-text", role: "alert" });
    const row = h("div", { className: "name-fields" },
      h("div", { className: "field" }, h("label", { htmlFor: group.id, title: "Nested groups are separated with /" }, "Group"), group, groupProblem,
        h("datalist", { id: `${idPrefix}-groups` }, ...[...folderPaths()].sort().map((f) => h("option", { value: f })))),
      h("div", { className: "field" }, h("label", { htmlFor: name.id }, "Name"), name, nameProblem));
    const groupValue = () => group.value.trim().replace(/^\/+|\/+$/g, "");
    const full = () => [groupValue(), name.value.trim()].filter(Boolean).join("/");
    // check returns the full name, or "" after showing what is wrong; except is the current name.
    const check = (except = "") => {
      groupProblem.textContent = nameProblem.textContent = "";
      const g = groupValue(), n = name.value.trim();
      const fail = (p, field, text) => { p.textContent = text; field.focus(); return ""; };
      if (!g && !fileTree) return fail(groupProblem, group, "Enter a group.");
      if (g && !validName(g)) return fail(groupProblem, group, "Use lowercase letters, digits, and dashes; separate nested groups with /.");
      if (!n) return fail(nameProblem, name, "Enter a name.");
      if (n.includes("/") || !validName(n)) return fail(nameProblem, name, "Use lowercase letters, digits, and dashes.");
      if (!validName(full())) return fail(groupProblem, group, "Groups nest at most three deep.");
      if (full() !== except && state.entries.some((e) => e.name === full())) return fail(nameProblem, name, `${full()} already exists.`);
      if (full() !== except && folderPaths().has(full())) return fail(nameProblem, name, `${full()} is a group.`);
      return full();
    };
    for (const c of [group, name]) { c.addEventListener("input", () => fitWidth(c)); requestAnimationFrame(() => { if (c.isConnected) fitWidth(c); }); }
    return { row, group, name, check, full };
  }

  async function moveEntry(name, target) {
    const versions = [...(await loadVersions(name))].reverse(); // oldest first
    const contents = [];
    for (const v of versions) contents.push({ v, c: await api("GET", `${base}/${enc(name)}/${encodeURIComponent(v.version)}`) });
    // The old entry goes first, so it can become the folder of the new one (intake → intake/intake).
    await api("DELETE", `${base}/${enc(name)}`);
    state.versions.delete(name);
    try {
      for (const { v, c } of contents) {
        const moved = fileTree ? fileTree.renameEntry(c.files, name, target) : c.files;
        await api("POST", `${base}/${enc(target)}`, { files: moved }, { "X-Creator": v.creator || creator() || "unknown" });
      }
    } catch (e) {
      for (const { v, c } of contents) await api("POST", `${base}/${enc(name)}`, { files: c.files }, { "X-Creator": v.creator || "unknown" }).catch(() => {});
      throw e;
    }
    if (tab === "agents") {
      const cfg = await api("GET", "/api/orchestrator/workflows");
      let changed = false;
      for (const w of cfg.workflows) for (const s of w.steps) {
        const [n, ver] = (s.agent || "").split("@");
        if (n === name && ver) { s.agent = `${target}@${ver}`; changed = true; }
      }
      if (changed) await api("PUT", "/api/orchestrator/workflows", cfg);
    }
    if (lastUsed() === name) remember(target);
  }

  // go navigates; unsaved edits are confirmed first.
  async function go(name, version) {
    if (!(await confirmLeave())) { tree.select(state.draft ? `d:${state.draft.name}` : state.version ? `v:${state.name}@${state.version}` : state.name ? `n:${state.name}` : null); return; }
    state.unsaved = false;
    const hash = `#${tab}/${name}${version ? "/" + version : ""}`;
    // Leaving a draft for the version it started from changes no hash, so show it directly.
    if (state.draft) { state.draft = null; if (location.hash === hash) { show(hash.slice(tab.length + 1)); return; } }
    location.hash = hash;
  }

  const confirmLeave = async () => !state.unsaved || confirmAction("Discard your unsaved changes?", "Discard");
  window.addEventListener("beforeunload", (ev) => { if (state.unsaved) ev.preventDefault(); });

  // ---- Files ----

  // fileText turns a file as the registry returns it into editable text.
  function fileText(f, value) {
    if (value === undefined) return "";
    return f.json && typeof value !== "string" ? JSON.stringify(value, null, 2) : String(value);
  }

  // collectFiles reads the editors into the request body, parsing the JSON files.
  function collectFiles(areas) {
    if (fileTree) return areas.collect();
    const out = {};
    for (const f of files) {
      const text = areas[f.name].value;
      if (!f.json) { out[f.name] = text; continue; }
      try { out[f.name] = JSON.parse(text); } catch (e) { throw new Error(`${f.name} is not valid JSON: ${e.message}`); }
    }
    return out;
  }

  async function validatePolicyReferences(storedFiles) {
    const promptFile = files.find((file) => file.policyReferences);
    if (!promptFile) return;
    await validatePolicyPrompt(String(storedFiles[promptFile.name] || ""), (id) => api("GET", `/api/policies/policies/${enc(id)}/latest`));
  }

  async function prepareFiles(areas, problem) {
    problem.textContent = "";
    try {
      const storedFiles = collectFiles(areas);
      await validatePolicyReferences(storedFiles);
      return { files: storedFiles };
    } catch (error) {
      problem.textContent = error.message;
      return null;
    }
  }

  const rowsFor = (text) => Math.max(4, String(text).split("\n").length + 1);

  // autosize grows a textarea to its content so the panel scrolls, not the box.
  // The panel keeps its scroll position while the box is measured.
  function autosize(area) {
    if (area.hidden || !area.getClientRects().length) return;
    const top = detail.scrollTop;
    area.style.height = "0";
    area.style.height = `${Math.max(96, area.scrollHeight + 2)}px`;
    detail.scrollTop = top;
  }

  function editor(f, value, onInput) {
    const area = h("textarea", { className: `file ${f.mono ? "code" : "prose"}`, spellcheck: false, value, rows: rowsFor(value), "aria-label": f.label });
    area.addEventListener("input", () => { autosize(area); onInput(); });
    requestAnimationFrame(() => autosize(area));
    return area;
  }

  // create posts the files as the next version of name.
  // A second click (or Enter) while a save runs would store a second, identical version.
  const saveState = { busy: false };
  async function create(name, areas, problem, confirmExisting = false, control = null) {
    return runExclusive(saveState, control, async () => {
      try {
        const body = await prepareFiles(areas, problem);
        if (!body) return;
        if (confirmExisting && !(await confirmAction("Save these changes as a new version?", "Save new version"))) return;
        const res = await api("POST", `${base}/${enc(name)}`, body, creatorHeaders());
        state.unsaved = false;
        state.draft = null;
        state.versions.delete(name);
        setSaveState(`Saved ${res.version}`);
        location.hash = `#${tab}/${res.name}/${res.version}`;
      } catch (e) {
        if (fileTree || /policy|skill/i.test(e.message)) problem.textContent = e.message;
        else setStatus(`Not saved: ${e.message}`, "error");
      }
    });
  }

  // ---- Content: the files of one version, editable; saving makes a new version ----

  // fileSection titles a file's editor; settings sections show a file's fields (agent.json) in
  // one row above all files.
  const fileSection = (f, area) => {
    if (f.json) return h("section", { className: "section" }, h("h3", { className: "section-title" }, f.label), area);
    const section = markdownEditorSection(f.label, area, () => autosize(area));
    if (f.policyReferences) addPolicyInsertion(section, area);
    return section;
  };

  function addPolicyInsertion(section, area) {
    let selection = { start: area.selectionStart || 0, end: area.selectionEnd || 0 };
    const remember = () => { selection = { start: area.selectionStart ?? selection.start, end: area.selectionEnd ?? selection.end }; };
    for (const event of ["click", "keyup", "select", "input"]) area.addEventListener(event, remember);

    const choose = async () => {
      openPolicyReferencePicker(area, selection, async () => {
        const response = await api("GET", "/api/policies/policies");
        return (response.entries || []).map((entry) => entry.name);
      }, () => {
        selection = { start: area.selectionStart, end: area.selectionEnd };
        requestAnimationFrame(() => { autosize(area); area.focus(); });
      });
    };
    const context = (event) => {
      if (event.currentTarget === area) remember();
      openMenu(event, [["Add policy", choose, "", "Inserts a policy reference at the cursor"]]);
    };
    area.addEventListener("contextmenu", context);
    section.previewBody.addEventListener("contextmenu", context);
  }
  const settingsSections = (areas) => files.filter((f) => f.fields).map((f) => h("section", { className: "section" }, f.fields(areas[f.name], areas)));

  // renderContent shows a version's files. The Save row appears with the first edit, or at
  // once for a draft version, which Cancel discards.
  function renderContent(c, draft = false) {
    let areas;
    const problem = h("p", { className: "error-text", role: "alert" });
    const actions = h("div", { className: "section actions", hidden: true });
    const save = h("button", { type: "button", className: "primary", onclick: async () => {
      await create(c.name, areas, problem, !draft, save);
    } }, "Save as new version");
    const cancel = h("button", { type: "button", onclick: () => {
      state.unsaved = false;
      if (draft) { state.draft = null; renderTree(); tree.select(`v:${state.name}@${state.version}`); }
      renderContent(c);
    } }, "Cancel");
    actions.append(cancel, save);
    if (fileTree) {
      const readOnly = !draft && c.version !== state.versions.get(c.name)?.[0]?.version;
      areas = fileTree.render(c.files, { readOnly, onChange(dirty) {
        state.unsaved = dirty;
        actions.hidden = !dirty && !draft;
      } }, editor, autosize);
      actions.hidden = readOnly || !draft;
      detail.replaceChildren(problem, areas.element, actions);
      if (draft) areas.focus();
      return;
    }
    areas = {};
    const original = {};
    const check = () => {
      state.unsaved = files.some((f) => areas[f.name].value !== original[f.name]);
      actions.hidden = !state.unsaved && !draft;
    };
    actions.hidden = !draft;
    const sections = files.map((f) => {
      original[f.name] = fileText(f, c.files?.[f.name]);
      areas[f.name] = editor(f, original[f.name], check);
      return fileSection(f, areas[f.name]);
    });
    detail.replaceChildren(problem, ...settingsSections(areas), ...sections, actions);
    if (draft) areas[files[0].name].focus();
  }

  // newVersion opens the next version of an entry as a draft, starting from the given version.
  async function newVersion(name, from) {
    if (!from || !(await confirmLeave())) return;
    try {
      const versions = await loadVersions(name);
      const c = await api("GET", `${base}/${enc(name)}/${encodeURIComponent(from)}`);
      const next = fileTree ? "Draft version" : `v${Math.max(0, ...versions.map((v) => parseInt(v.version.slice(1), 10) || 0)) + 1}`;
      state.unsaved = false;
      state.name = name;
      state.version = from;
      state.draft = { name, version: next };
      history.replaceState(null, "", `#${tab}/${name}/${from}`);
      remember(name);
      renderTree();
      const parts = name.split("/").slice(0, -1);
      await tree.reveal([...parts.map((_, i) => `f:${parts.slice(0, i + 1).join("/")}`), `n:${name}`]);
      tree.select(`d:${name}`);
      renderContent(c, true);
    } catch (e) { setStatus(`Cannot load ${name} ${from}: ${e.message}`, "error"); }
  }

  async function showNewForm(prefill = null) {
    if (!(await confirmLeave())) return;
    state.unsaved = false;
    state.name = state.version = "";
    tree.select(null);
    const id = nameFields(`${tab}-new`, prefill?.group ? `${prefill.group}/` : "");
    const areas = fileTree ? fileTree.render(Object.fromEntries(files.map((f) => [f.name, f.template("")])), {
      onChange() { state.unsaved = true; },
    }, editor, autosize) : {};
    if (!fileTree) {
      for (const f of files) areas[f.name] = editor(f, prefill?.files?.[f.name] ?? f.template(""), () => { state.unsaved = true; });
    }
    const problem = h("p", { className: "error-text", role: "alert" });
    const submit = async () => { const n = id.check(); if (n) await create(n, areas, problem); };
    // The JSON names the entry by its full name, kept in step with the two fields.
    const syncName = () => {
      if (fileTree) { areas.syncName(id.name.value.trim()); return; }
      for (const f of files.filter((x) => x.json)) {
        try {
          const obj = JSON.parse(areas[f.name].value);
          if ("name" in obj) { obj.name = id.full(); areas[f.name].value = JSON.stringify(obj, null, 2); }
        } catch { /* leave edited text alone */ }
      }
    };
    for (const c of [id.group, id.name]) {
      c.addEventListener("keydown", (ev) => { if (ev.key === "Enter") submit(); });
      c.addEventListener("input", () => { state.unsaved = true; syncName(); }); // typed names are kept like typed files
    }
    detail.replaceChildren(
      problem,
      h("section", { className: "section" }, id.row),
      ...(fileTree ? [areas.element] : [...settingsSections(areas), ...files.map((f) => fileSection(f, areas[f.name]))]),
      h("div", { className: "section actions" },
        h("button", { type: "button", onclick: () => { state.unsaved = false; show(""); } }, "Cancel"),
        h("button", { type: "button", className: "primary", onclick: submit }, `Create ${noun}`)),
    );
    syncName();
    (id.group.value ? id.name : id.group).focus();
  }

  // duplicate opens the New form filled with a version of an entry (its newest by default).
  // duplicate stores one version as v1 of a new entry beside it ("<name>-copy") and opens it,
  // like Duplicate on the Workflows and Blobstore pages.
  async function duplicate(source, version = "") {
    try {
      const versions = await loadVersions(source);
      const v = version || versions[0]?.version;
      if (!v) return;
      const c = await api("GET", `${base}/${enc(source)}/${encodeURIComponent(v)}`);
      let name = `${source}-copy`;
      for (let n = 2; state.entries.some((e) => e.name === name); n++) name = `${source}-copy-${n}`;
      const copy = fileTree ? fileTree.renameEntry(c.files, source, name) : { ...c.files };
      for (const f of files.filter((x) => x.json)) {
        const json = copy[f.name];
        if (json && typeof json === "object" && json.name === source) copy[f.name] = { ...json, name };
      }
      const res = await api("POST", `${base}/${enc(name)}`, { files: copy }, creatorHeaders());
      state.versions.delete(name);
      setSaveState("Duplicated");
      location.hash = `#${tab}/${res.name}/${res.version}`;
    } catch (e) { setStatus(`Not duplicated: ${e.message}`, "error"); }
  }

  async function remove(name) {
    const users = await usedBy(name);
    if (!(await confirmAction(`Delete ${name} with every version${users.length ? `, used by ${users.join(" and ")}` : ""}?`, "Delete"))) return;
    // The selection moves to a neighbour, in the same group when there is one.
    const names = state.entries.map((e) => e.name).filter((n) => n !== name).sort();
    const group = (n) => n.split("/").slice(0, -1).join("/");
    const siblings = names.filter((n) => group(n) === group(name));
    const next = siblings.find((n) => n > name) || siblings.reverse().find((n) => n < name) || names.find((n) => n > name) || names[names.length - 1];
    try {
      await api("DELETE", `${base}/${enc(name)}`);
      state.versions.delete(name);
      state.unsaved = false;
      setSaveState("Deleted");
      if (state.name === name) location.hash = next ? `#${tab}/${next}` : `#${tab}`; else show(location.hash.slice(tab.length + 1));
    } catch (e) { setStatus(`Not deleted: ${e.message}`, "error"); }
  }

  async function removeFolder(folder, inside) {
    if (inside.length && !(await confirmAction(`Delete ${folder} with its ${inside.length} ${inside.length === 1 ? noun : plural.toLowerCase()}?`, "Delete"))) return;
    try {
      for (const e of inside) { await api("DELETE", `${base}/${enc(e.name)}`); state.versions.delete(e.name); }
      for (const f of [...state.folders]) if (f === folder || f.startsWith(`${folder}/`)) state.folders.delete(f);
      setSaveState("Deleted");
      // The selection moves to the entry after the group, or the last one before it.
      const rest = state.entries.map((e) => e.name).filter((n) => !n.startsWith(`${folder}/`)).sort();
      const next = rest.find((n) => n > folder) || rest[rest.length - 1];
      if (state.name.startsWith(`${folder}/`)) location.hash = next ? `#${tab}/${next}` : `#${tab}`; else show(location.hash.slice(tab.length + 1));
    } catch (e) { setStatus(`Not deleted: ${e.message}`, "error"); }
  }

  // loadVersions caches an entry's versions, newest first.
  async function loadVersions(name) {
    if (state.versions.has(name)) return state.versions.get(name);
    const v = await api("GET", `${base}/${enc(name)}`);
    const items = [...v.versions].reverse();
    state.versions.set(name, items);
    return items;
  }

  // show loads the state named by the hash: "", "/<name>", or "/<name>/<version>", where
  // name may contain folders.
  async function show(rest) {
    const parts = rest.split("/").filter(Boolean).map(decodeURIComponent);
    const version = parts.length > 1 && isVersion(parts[parts.length - 1]) ? parts.pop() : "";
    const name = parts.join("/");
    // Unsaved edits survive a visit to another page: coming back shows the same draft,
    // and opening a different entry asks first.
    if (state.unsaved || state.draft) {
      const same = !name || (name === state.name && (!version || version === state.version));
      if (same || !(await confirmLeave())) {
        if (state.name && (!same || !name)) location.replace(`#${tab}/${enc(state.name)}/${encodeURIComponent(state.version)}`);
        return;
      }
      state.unsaved = false;
      state.draft = null;
    }
    const seq = ++state.seq;
    try {
      const data = await api("GET", base);
      if (seq !== state.seq) return;
      state.entries = data.entries;
      await pool(state.entries, 6, (e) => loadVersions(e.name).catch(() => {}));
      if (seq !== state.seq) return;
      renderTree();
      if (!name || !state.entries.some((e) => e.name === name)) {
        const pick = state.entries.find((e) => e.name === lastUsed()) || state.entries[0];
        state.name = state.version = "";
        if (pick) { location.replace(`#${tab}/${pick.name}`); return; }
        // Nothing stored yet: the empty state, like Workflows without a workflow.
        detail.replaceChildren(emptyPanel(`No ${plural.toLowerCase()} yet`, { label: `New ${noun}`, primary: true, run: () => showNewForm(null) }));
        return;
      }
      state.name = name;
      state.version = version;
      remember(name);
      const versions = await loadVersions(name);
      if (seq !== state.seq) return;
      // No version, or one that is gone, opens the newest: never a ghost of a deleted version.
      if (!version || !versions.some((v) => v.version === version)) {
        if (versions.length) location.replace(`#${tab}/${name}/${versions[0].version}`);
        return;
      }
      const folderParts = name.split("/").slice(0, -1);
      await tree.reveal([...folderParts.map((_, i) => `f:${folderParts.slice(0, i + 1).join("/")}`), `n:${name}`]);
      tree.select(`v:${name}@${version}`);
      const c = await api("GET", `${base}/${enc(name)}/${encodeURIComponent(version)}`);
      if (seq !== state.seq) return;
      state.unsaved = false;
      renderContent(c);
    } catch (e) {
      if (seq === state.seq) detail.replaceChildren(errorPanel(`Cannot load ${name || plural.toLowerCase()}: ${e.message}`, () => show(rest)));
    }
  }

  tabs[tab] = { show };
}
