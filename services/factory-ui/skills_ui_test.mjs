import assert from "node:assert/strict";
import fs from "node:fs";
import test from "node:test";
import vm from "node:vm";

const source = (name) => fs.readFileSync(new URL(`web/${name}.js`, import.meta.url), "utf8");
const section = (text, start, end) => text.slice(text.indexOf(start), text.indexOf(end));
const app = source("app");

function setup(api = async () => { throw new Error("Unexpected API request"); }) {
  const h = (tag, props = {}, ...children) => ({
    tag, ...props, children: children.flat().filter((child) => child != null),
    replaceChildren(...items) {
      this.children = items;
      if (tag === "select") this.value = items[0]?.value;
    },
    focus() {},
    addEventListener() {},
  });
  const browser = { h, api, markdownEditorSection: (path, area) => h("section", {}, path, area), emptyPanel: (text) => h("p", {}, text),
    registryTab: (config) => { browser.registry = config; },
    openDialog: (config) => { browser.dialog = config; return { close: () => config.onClose?.() }; },
    openMenu: (_, menu) => { browser.menu = menu; },
    confirmAction: async () => true,
    createTree(list, cfg) {
      browser.tree = { ...cfg, setRoots(roots) { list.replaceChildren(...roots); }, async reveal() {}, async select() {}, async expand() {} };
      return browser.tree;
    },
  };
  const editor = (file, text, change) => h("textarea", { label: file.label, value: text, input: change });
  vm.runInNewContext(`
    ${section(app, "const groupOf", "// openDialog")}
    ${source("skills")}
    ${section(source("agents"), "function chooseAgentSkills", "// agentFields")}
    ${section(source("runs"), "function skillToolWarning", "const RunsLogic")}
  `, browser);
  return { browser, editor };
}

function find(node, match) {
  if (node == null || typeof node !== "object") return undefined;
  if (match(node)) return node;
  return (node.children || []).map((child) => find(child, match)).find(Boolean);
}

const button = (node, text) => find(node, (el) => el.tag === "button" && el.children.includes(text));
const byID = (node, id) => find(node, (el) => el.id === id);
const settle = () => new Promise((resolve) => setImmediate(resolve));

test("skill file tree edits and collects the whole draft for the next version", async () => {
  const { browser, editor } = setup();
  const original = { "SKILL.md": browser.skillTemplate("") };
  const tree = browser.registry.fileTree.render(original, { onChange() {} }, editor, () => {});
  await settle();
  tree.syncName("git");
  assert.match(tree.collect()["SKILL.md"], /^---\nname: git\n/);
  button(tree.element, "Add folder…").onclick();
  byID(browser.dialog.body, "skill-file-name").value = "release";
  await browser.dialog.footer[1].onclick();
  browser.tree.onContext({ id: "release" }, {});
  browser.menu.find(([label]) => label === "Add file…")[1]();
  byID(browser.dialog.body, "skill-file-name").value = "SKILL.md";
  await browser.dialog.footer[1].onclick();
  const area = find(tree.element, (el) => el.tag === "textarea");
  area.value += "Release instructions";
  area.input();
  browser.tree.onContext({ id: "release" }, {});
  browser.menu.find(([label]) => label === "Rename folder…")[1]();
  byID(browser.dialog.body, "skill-file-name").value = "publish";
  await browser.dialog.footer[1].onclick();
  assert.match(tree.collect()["publish/SKILL.md"], /name: publish\n[\s\S]*Release instructions/);
  assert.equal(tree.collect()["release/SKILL.md"], undefined);
  browser.tree.onContext({ id: "publish/SKILL.md" }, {});
  await browser.menu.find(([label]) => label === "Delete file")[1]();
  assert.deepEqual(Object.keys(tree.collect()), ["SKILL.md"]);
  assert.equal(original["SKILL.md"], browser.skillTemplate(""));
  const registrySource = source("registry");
  assert.match(registrySource, /await api\("POST", `\$\{base\}\/\$\{enc\(name\)\}`, body/);
  assert.match(registrySource, /c\.version !== state\.versions\.get\(c\.name\)\?\.\[0\]\?\.version/);
});

test("older skill versions expose a read-only file tree and text", async () => {
  const { browser, editor } = setup();
  const files = { "SKILL.md": browser.skillTemplate("git"), "script.sh": "#!/bin/sh\necho test" };
  const tree = browser.registry.fileTree.render(files, { readOnly: true, onChange() {} }, editor, () => {});
  await settle();
  assert.equal(button(tree.element, "Add file…"), undefined);
  assert.equal(find(tree.element, (el) => el.tag === "textarea").readOnly, true);
  browser.tree.onSelect({ id: "script.sh" });
  assert.equal(find(tree.element, (el) => el.tag === "textarea").value, files["script.sh"]);
  browser.tree.onContext({ id: "script.sh" }, {});
  assert.equal(browser.menu, undefined);
});

test("agent skill picker defaults to the newest version and excludes an assigned name", async () => {
  const { browser } = setup(async (_, path) => path.endsWith("/git")
    ? { versions: [{ version: "v1" }, { version: "v2" }] }
    : { entries: [{ name: "git" }, { name: "review" }] });
  let result;
  browser.chooseAgentSkills(["review@v1"], (refs) => { result = Array.from(refs); });
  await settle();
  const { body, footer } = browser.dialog;
  const skill = byID(body, "agent-skill-name");
  const version = byID(body, "agent-skill-version");
  assert.deepEqual(skill.children.map((el) => el.value), ["", "git"]);
  skill.value = "git";
  await skill.onchange();
  assert.equal(version.value, "v2");
  button(body, "Add skill").onclick();
  assert.deepEqual(skill.children.map((el) => el.value), [""]);
  skill.value = "git";
  version.value = "v1";
  button(body, "Add skill").onclick();
  footer[1].onclick();
  assert.deepEqual(result, ["review@v1", "git@v2"]);
});

test("Runs labels a saved tool warning and displays pinned skill versions", () => {
  const { browser } = setup();
  const warning = browser.skillToolWarning({ kind: "tool_call", warning: "Output truncated" });
  assert.equal(warning.className, "tool-warning");
  assert.equal(warning.children[0].children[0], "Warning");
  assert.equal(warning.children.at(-1), "Output truncated");
  assert.equal(browser.skillToolWarning({ kind: "model_call", warning: "Output truncated" }), null);
  const metadata = browser.runSkillVersions({ agents: { "tools/operator@v1": { skills: { git: "v3" } } } });
  assert.equal(find(metadata, (el) => el.tag === "dd").children[0], "git@v3");
});
