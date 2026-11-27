import assert from "node:assert/strict";
import fs from "node:fs";
import test from "node:test";
import vm from "node:vm";

const source = fs.readFileSync(new URL("web/tools.js", import.meta.url), "utf8");
const elements = new Map();
const element = (tag = "div", props = {}, ...children) => {
  const node = {
    tag, props, children: children.flat().filter(Boolean), textContent: "",
    replaceChildren(...items) { this.children = items.flat().filter(Boolean); },
  };
  if (props.id) elements.set(props.id, node);
  return node;
};
const text = (node) => typeof node === "string" ? node : [node.textContent, ...node.children.map(text)].join(" ");
// The tree stand-in records what the page hands the real createTree of app.js.
const tree = { roots: [], revealed: [], selected: null, onSelect: null };
const context = {
  $: (id) => { if (!elements.has(id)) elements.set(id, element()); return elements.get(id); },
  h: element,
  emptyPanel: (title) => element("div", {}, title),
  dataTable: (headings, rows) => element("table", {}, ...headings, ...rows),
  createTree: (_container, { onSelect }) => {
    tree.onSelect = onSelect;
    return {
      setRoots(nodes) { tree.roots = nodes; },
      async reveal(ids) { tree.revealed = ids; },
      async select(id) { tree.selected = id; },
    };
  },
  api: async () => ({ services: ["factory", { name: "devpod" }, { name: "broken", error: "down" }, { name: "dead" }] }),
  location: { hash: "#tools" },
  activeTab: "tools",
  tabs: {},
  window: {
    discoverMcpServiceTools: async (service) => {
      if (service === "dead") throw new Error("unreachable");
      return service === "devpod"
        ? [{ name: "run_command", description: "Runs a command" }, { name: "read_file", description: "Reads a file", inputSchema: { type: "object" } }]
        : [{ name: "read_trace", description: "Reads a trace" }];
    },
  },
  AbortController, setTimeout, clearTimeout,
};
vm.runInNewContext(source, context);
const plain = (value) => JSON.parse(JSON.stringify(value));

test("the sidebar is a tree: every MCP service is a folder that holds its tools", async () => {
  await context.tabs.tools.show("");
  assert.deepEqual(plain(tree.roots.map((n) => [n.label, n.children.map((c) => c.label)])),
    [["devpod", ["read_file", "run_command"]], ["factory", ["read_trace"]]]);
  assert.match(text(context.$("tools-empty")), /Cannot list tools from broken, dead/);
});

test("without a tool in the URL the first service is selected and lists its tools", () => {
  assert.equal(context.location.hash, "#tools/devpod");
  assert.equal(tree.selected, "s:devpod");
  assert.equal(context.$("tools-detail-title").textContent, "devpod");
  const detail = text(context.$("tools-detail"));
  assert.match(detail, /read_file/);
  assert.match(detail, /Runs a command/);
});

test("a tool opens its service folder and shows its description and input schema", async () => {
  await context.tabs.tools.show("/devpod/read_file");
  assert.deepEqual(plain(tree.revealed), ["s:devpod"]);
  assert.equal(tree.selected, "t:devpod/read_file");
  assert.equal(context.$("tools-detail-title").textContent, "read_file");
  const detail = text(context.$("tools-detail"));
  assert.match(detail, /Reads a file/);
  assert.match(detail, /"type": "object"/);
});

test("a click in the tree moves the URL to the clicked service or tool", () => {
  tree.onSelect({ ref: "factory/read_trace" });
  assert.equal(context.location.hash, "#tools/factory/read_trace");
  tree.onSelect({ ref: "factory" });
  assert.equal(context.location.hash, "#tools/factory");
});

test("an unknown tool falls back to the first service", async () => {
  context.location.hash = "#tools/nope";
  await context.tabs.tools.show("/nope");
  assert.equal(context.location.hash, "#tools/devpod");
});

test("a tool without an input schema still renders", async () => {
  await context.tabs.tools.show("/factory/read_trace");
  const detail = text(context.$("tools-detail"));
  assert.match(detail, /Reads a trace/);
  assert.match(detail, /\{\}/);
});
