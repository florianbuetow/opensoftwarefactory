import assert from "node:assert/strict";
import fs from "node:fs";
import test from "node:test";
import vm from "node:vm";

const app = fs.readFileSync(new URL("web/app.js", import.meta.url), "utf8");
const helpers = app.slice(app.indexOf("function isJSONDocument"), app.indexOf("// Times are shown relative"));

function node(tag, props = {}, ...children) {
  return {
    tag, ...props, children: children.flat().filter((value) => value !== null && value !== undefined && value !== false),
    append(...items) { this.children.push(...items); },
    replaceChildren(...items) { this.children = items.flat(); },
    setAttribute(name, value) { this[name] = value; },
  };
}

function setup(storage = new Map()) {
  const toggles = [];
  const context = {
    document: {
      createDocumentFragment: () => node("fragment"),
      querySelectorAll: (selector) => selector === ".text-format-toggle" ? toggles : [],
    },
    localStorage: {
      getItem: (key) => storage.get(key) ?? null,
      setItem: (key, value) => storage.set(key, value),
    },
    requestAnimationFrame(callback) { callback(); },
    iconButton(name, label, onclick) {
      return node("button", { type: "button", className: "icon-button", title: label, "aria-label": label, onclick }, name);
    },
    h(tag, props = {}, ...children) {
      const result = node(tag, props, ...children);
      if (result.className === "text-format-toggle") toggles.push(result);
      return result;
    },
    pretty(text) { try { return JSON.stringify(JSON.parse(text), null, 2); } catch { return text; } },
  };
  vm.runInNewContext(`${helpers}\nglobalThis.markdownUI = { isJSONDocument, renderMarkdown, textView, markdownEditorSection, memoryObjectHash, blobReferences, promptEvidenceLinks, blobReferencePanel };`, context);
  return context.markdownUI;
}

const all = (root, tag) => {
  if (!root || typeof root !== "object") return [];
  return [...(root.tag === tag ? [root] : []), ...(root.children || []).flatMap((child) => all(child, tag))];
};

test("Markdown is the default and one toggle preserves plain whitespace", () => {
  const { textView } = setup();
  const source = "# Result\n\n- first\n  - nested\n\n| Name | Score |\n| --- | ---: |\n| Ada | 10 |\n\n~~~js\nconst x = 1;\n~~~";
  const view = textView(source);
  assert.equal(view.toggle.textContent, "TXT");
  assert.equal(view.toggle["aria-label"], "Switch to plain text");
  assert.equal(all(view.body, "h1").length, 1);
  assert.equal(all(view.body, "ul").length, 2, "nested lists stay nested");
  assert.equal(all(view.body, "table").length, 1);
  assert.equal(all(view.body, "pre").length, 1);
  view.toggle.onclick();
  assert.equal(view.toggle.textContent, "MD");
  assert.equal(view.toggle["aria-label"], "Switch to Markdown");
  assert.deepEqual(view.body.children, [source], "plain text is retained byte-for-byte");
});

test("Markdown rendering treats HTML as text and rejects unsafe links", () => {
  const { renderMarkdown } = setup();
  const rendered = renderMarkdown("<img src=x onerror=alert(1)> [open](javascript:alert(1)) [safe](https://example.test)");
  assert.equal(all(rendered, "img").length, 0);
  assert.equal(all(rendered, "a").length, 1);
  assert.equal(all(rendered, "a")[0].href, "https://example.test");
  assert.match(JSON.stringify(rendered), /<img src=x/);
});

test("JSON documents remain code without a toggle", () => {
  const { isJSONDocument, textView } = setup();
  assert.equal(isJSONDocument('{"ok":true}'), true);
  assert.equal(isJSONDocument("{prose}"), false);
  const view = textView('{"ok":true}');
  assert.equal(view.toggle.hidden, true);
  assert.match(view.body.className, /code/);
  assert.equal(view.body.children[0], '{\n  "ok": true\n}');
});

test("one global preference synchronizes viewers and survives a reload", () => {
  const storage = new Map();
  const firstPage = setup(storage);
  const agent = firstPage.textView("# Agent");
  const memory = firstPage.textView("# Memory");
  agent.toggle.onclick();
  assert.equal(agent.toggle.textContent, "MD");
  assert.equal(memory.toggle.textContent, "MD", "other visible viewers synchronize");
  assert.deepEqual(memory.body.children, ["# Memory"]);

  const reloadedPage = setup(storage);
  const decision = reloadedPage.textView("# Decision");
  assert.equal(decision.toggle.textContent, "MD", "explicit text choice survives reload");
  decision.toggle.onclick();
  const nextAgent = reloadedPage.textView("# Next agent");
  assert.equal(nextAgent.toggle.textContent, "TXT", "agent navigation uses the global choice");
});

test("registry preview keeps the same editable draft when toggled both ways", () => {
  const storage = new Map();
  const { markdownEditorSection } = setup(storage);
  const area = node("textarea", { value: "# Original", events: {} });
  area.focused = false;
  area.focus = () => { area.focused = true; };
  area.addEventListener = (type, listener) => { area.events[type] = listener; };
  let resized = 0;
  const section = markdownEditorSection("System prompt (systemprompt.md)", area, () => { assert.equal(area.hidden, false); resized++; });
  const [edit, toggle] = all(section, "button");
  assert.equal(area.hidden, true, "editor starts in the global Markdown mode");
  assert.equal(all(section, "h1")[0].children[0].children[0], "Original");
  assert.equal(edit.title, "Edit System prompt (systemprompt.md)");
  edit.onclick();
  assert.equal(area.hidden, false);
  assert.equal(area.focused, true, "Edit focuses the live prompt textarea");
  assert.equal(storage.get("factory.text-view-mode"), "text", "editing uses the shared text mode");
  assert.ok(resized > 0, "resize after revealing the textarea");
  area.value = "# Unsaved\n\nDraft text";
  area.events.input();
  toggle.onclick();
  assert.equal(area.hidden, true);
  assert.equal(all(section, "h1")[0].children[0].children[0], "Unsaved");
  toggle.onclick();
  assert.equal(area.hidden, false);
  assert.ok(resized > 0, "resize after revealing the textarea");
  assert.equal(area.value, "# Unsaved\n\nDraft text");
  assert.equal(section.children.includes(area), true, "the live textarea is never replaced");
});

test("all requested surfaces use the shared view and JSON editors remain excluded", () => {
  const files = ["registry.js", "workflows.js", "decisions.js", "artifacts.js"].map((name) => fs.readFileSync(new URL(`web/${name}`, import.meta.url), "utf8"));
  assert.match(files[0], /markdownEditorSection\(/);
  assert.match(files[0], /confirmAction\("Save these changes as a new version\?", "Save new version"\)/);
  for (const source of files.slice(1)) assert.match(source, /textView\(/);
  assert.match(files[0], /if \(f\.json\)/);
  assert.doesNotMatch(files[2], /replace\(\/\^```/, "decision output fences reach the Markdown renderer intact");
  const css = fs.readFileSync(new URL("web/style.css", import.meta.url), "utf8");
  assert.match(css, /\.text-format-toggle \{ width: 42px; min-width: 42px;/);
});

test("prompt evidence exposes exact snapshot and registry objects as Memory links", () => {
  const { promptEvidenceLinks } = setup();
  const panel = promptEvidenceLinks({ run_id: "run-1", prompt_evidence: {
    agent_source_artifact: "agents/build/v3/systemprompt.md",
    source_prompt_artifact: "runs/run-1/_evidence/build/attempt-2/systemprompt.source.md",
    effective_prompt_artifact: "runs/run-1/_evidence/build/attempt-2/systemprompt.effective.md",
    policies: [{ id: "security/review", version: "v4", source_artifact: "policies/security/review/v4/policy.md", snapshot_artifact: "runs/run-1/_evidence/build/attempt-2/policies/security/review/v4/policy.md" }],
  } });
  const links = all(panel, "a");
  assert.equal(links.length, 5);
  assert.equal(links[0].href, "#artifacts?trace=run-1&object=runs%2Frun-1%2F_evidence%2Fbuild%2Fattempt-2%2Fsystemprompt.source.md");
  assert.equal(links[1].href, "#artifacts/agents/build/v3/systemprompt.md");
  assert.match(links[3].href, /runs%2Frun-1.*policy\.md/);
  assert.equal(links[4].href, "#artifacts/policies/security/review/v4/policy.md");
});

test("Memory reference discovery links strict blob IDs but not paths inside external URLs", () => {
  const { blobReferences, blobReferencePanel } = setup();
  const text = 'input "runs/r1/input", policy policies/security/v2/policy.md; external https://example.test/runs/not-memory/input';
  assert.deepEqual(Array.from(blobReferences(text)), ["runs/r1/input", "policies/security/v2/policy.md"]);
  assert.equal(all(blobReferencePanel(text), "a").length, 2);
});

test("an underscore inside a word is part of a name, never emphasis", () => {
  const { renderMarkdown } = setup();
  const root = renderMarkdown("Call list_session_files, then __init__ and _this_ or __that__.");
  const text = (n) => typeof n === "string" ? n : (n.children || []).map(text).join("");
  assert.deepEqual(all(root, "em").map(text), ["this"]);
  assert.deepEqual(all(root, "strong").map(text), ["init", "that"]);
  assert.match(text(root), /list_session_files/);
});

test("the reduced-motion rules are the last rules of the stylesheet, so they win", () => {
  const css = fs.readFileSync(new URL("web/style.css", import.meta.url), "utf8");
  const query = "@media (prefers-reduced-motion: reduce)";
  const at = css.lastIndexOf(query);
  assert.ok(at > 0);
  assert.equal(css.indexOf(query), at, "one block only");
  const block = css.slice(at);
  for (const selector of [".chip-running .dot", ".ctx-menu", ".dialog", ".panel-reveal", ".caret .icon"]) assert.ok(block.includes(selector), selector);
  assert.doesNotMatch(block.slice(block.indexOf("}\n}") + 3), /\S/, "nothing follows the block");
});
