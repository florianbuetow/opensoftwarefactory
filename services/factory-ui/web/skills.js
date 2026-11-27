"use strict";

const skillRoot = "SKILL.md";
const skillsRegistry = "/api/skills/skills";

function skillTemplate(name) {
  return `---\nname: ${name}\ndescription: \n---\n\n`;
}

function skillNameInHeader(text, from, to) {
  const header = /^(---\r?\n)([\s\S]*?)(\r?\n---(?:\r?\n|$))/.exec(text || "");
  if (!header) return text;
  const name = /^name:[ \t]*(?:(['"])([^\r\n]*?)\1|([^\s#]*))([ \t]*(?:#[^\r\n]*)?)\r?$/m;
  const updated = header[2].replace(name, (line, quote, quoted, plain, suffix) => {
    if ((quote ? quoted : plain) !== from) return line;
    const q = quote || "";
    return `name: ${q}${to}${q}${suffix}${line.endsWith("\r") ? "\r" : ""}`;
  });
  return header[1] + updated + header[3] + text.slice(header[0].length);
}

function renameSkillEntry(files, from, to) {
  return { ...files, [skillRoot]: skillNameInHeader(files[skillRoot], leafOf(from), leafOf(to)) };
}

function skillFileTree(initial, { readOnly = false, onChange }, editor, resize) {
  let files = new Map(Object.entries(initial || {}));
  let folders = new Set();
  let selection = skillRoot;
  let currentArea;
  let entryName = "";
  const original = new Map(files);
  const list = h("ul", { "aria-label": "Skill files" });
  const pane = h("div", { className: "skill-file-editor" });
  const changed = () => onChange(files.size !== original.size || [...files].some(([path, text]) => original.get(path) !== text));
  const tree = createTree(list, {
    onSelect(node) {
      selection = node.id;
      renderEditor();
    },
    onContext(node, ev) {
      if (readOnly || node.id === skillRoot) return;
      const isFolder = !files.has(node.id);
      openMenu(ev, [
        [isFolder ? "Rename folder…" : "Rename file…", () => nameDialog(isFolder, node.id), "", ""],
        [isFolder ? "Delete folder" : "Delete file", () => remove(node.id), "danger", ""],
        ...(isFolder ? [
          ["Add file…", () => nameDialog(false, "", node.id), "", ""],
          ["Add folder…", () => nameDialog(true, "", node.id), "", ""],
        ] : []),
      ]);
    },
  });

  function renderEditor() {
    currentArea = null;
    if (!files.has(selection)) {
      pane.replaceChildren(emptyPanel("Select a file"));
      return;
    }
    const path = selection;
    const markdown = /\.md$/i.test(path);
    const area = editor({ mono: !markdown, label: path }, files.get(path), () => {
      files.set(path, area.value);
      changed();
    });
    area.readOnly = readOnly;
    currentArea = area;
    pane.replaceChildren(markdown
      ? markdownEditorSection(path, area, () => resize(area))
      : h("div", {}, h("h3", { className: "section-title" }, path), area));
  }

  async function render(path) {
    selection = path;
    tree.setRoots(groupNodes(groupPaths(files.keys(), folders), [...files.keys()], (p) => p,
      (id, children) => ({ id, label: leafOf(id), full: id, children }),
      (id) => ({ id, label: leafOf(id), full: id })));
    await tree.reveal(groupIds(path, ""));
    await tree.select(path);
    renderEditor();
  }

  function nameDialog(isFolder, oldPath = "", directory = "") {
    const kind = isFolder ? "folder" : "file";
    const action = oldPath ? "Rename" : "Add";
    const parent = oldPath ? groupOf(oldPath) : directory;
    const input = h("input", { id: "skill-file-name", value: leafOf(oldPath), autocomplete: "off", spellcheck: false });
    const problem = h("p", { className: "error-text", role: "alert" });
    const apply = h("button", { type: "button", className: "primary" }, `${action} ${kind}`);
    const cancel = h("button", { type: "button" }, "Cancel");
    const dialog = openDialog({
      title: `${action} ${kind}`,
      body: h("div", { className: "field" }, h("label", { htmlFor: input.id }, "Name"), input, problem),
      footer: [cancel, apply],
    });
    cancel.onclick = () => dialog.close();
    apply.onclick = async () => {
      const name = input.value;
      if (!/^[A-Za-z0-9._-]+$/.test(name) || name === "." || name === "..") {
        problem.textContent = "Use letters, digits, dots, dashes, and underscores for the name.";
        return;
      }
      const path = parent ? `${parent}/${name}` : name;
      if (path === oldPath) { dialog.close(); return; }
      if (files.has(path) || groupPaths(files.keys(), folders).has(path)) {
        problem.textContent = `${path} already exists.`;
        return;
      }
      if (oldPath) {
        const moved = (id) => id === oldPath || id.startsWith(oldPath + "/") ? path + id.slice(oldPath.length) : id;
        files = new Map([...files].map(([id, text]) => [
          moved(id),
          isFolder && id === `${oldPath}/${skillRoot}` ? skillNameInHeader(text, leafOf(oldPath), name) : text,
        ]));
        folders = new Set([...folders].map(moved));
      } else if (isFolder) {
        folders.add(path);
      } else {
        files.set(path, name === skillRoot ? skillTemplate(leafOf(parent)) : "");
      }
      dialog.close();
      changed();
      await render(path);
      if (isFolder) await tree.expand(path);
    };
    input.addEventListener("keydown", (ev) => { if (ev.key === "Enter") apply.click(); });
  }

  async function remove(path) {
    if (!(await confirmAction(`Delete ${path}?`, "Delete"))) return;
    const inside = (id) => id === path || id.startsWith(path + "/");
    const parent = groupOf(path);
    const groups = groupPaths(files.keys(), folders);
    const siblings = [...groups, ...files.keys()].filter((p) => groupOf(p) === parent).sort();
    const index = siblings.indexOf(path);
    for (const group of groups) {
      if (inside(group)) folders.delete(group);
    }
    if (parent) folders.add(parent);
    for (const file of files.keys()) {
      if (inside(file)) files.delete(file);
    }
    changed();
    await render(inside(selection) ? siblings[index + 1] || siblings[index - 1] || parent || skillRoot : selection);
  }

  const actions = readOnly ? null : h("div", { className: "row" },
    h("button", { type: "button", onclick: () => nameDialog(false) }, "Add file…"),
    h("button", { type: "button", onclick: () => nameDialog(true) }, "Add folder…"));
  // Two columns that start on one line: the file tree under its heading, and the chosen file
  // under its name, so both headings and both bodies line up.
  const element = h("section", { className: "section skill-files" },
    h("div", { className: "skill-file-layout" },
      h("div", { className: "skill-file-list" }, h("div", { className: "file-head" }, h("h3", { className: "section-title" }, "Files"), actions), list),
      pane));
  render(skillRoot);
  return {
    element,
    collect: () => Object.fromEntries(files),
    focus: () => list.focus(),
    syncName(name) {
      const text = files.get(skillRoot);
      const updated = skillNameInHeader(text, entryName, name);
      entryName = name;
      if (updated === text) return;
      files.set(skillRoot, updated);
      if (selection === skillRoot) renderEditor();
    },
  };
}

registryTab({
  tab: "skills",
  base: skillsRegistry,
  noun: "skill",
  files: [{ name: skillRoot, template: skillTemplate }],
  fileTree: { render: skillFileTree, renameEntry: renameSkillEntry },
});
