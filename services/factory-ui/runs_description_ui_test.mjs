import assert from "node:assert/strict";
import fs from "node:fs";
import test from "node:test";
import vm from "node:vm";

const source = fs.readFileSync(new URL("web/runs.js", import.meta.url), "utf8");
const start = source.indexOf("const RunDescriptions");
const end = source.indexOf("\n(() => {", start);
assert.ok(start > 0 && end > start, "description logic is independently testable");
const code = `${source.slice(start, end)}\nglobalThis.RunDescriptions = RunDescriptions;`;

function harness(definitions = {}) {
  const calls = [];
  let release;
  const delayed = new Map();
  const context = {
    encodeURIComponent,
    WorkflowRef: {
      format: (name, version) => `${name}@v${version}`,
      parse: (ref) => { const [name, version] = ref.split("@v"); return { name, version: Number(version) }; },
    },
    api: async (method, path) => {
      calls.push([method, path]);
      if (delayed.has(path)) return delayed.get(path);
      if (!(path in definitions)) throw new Error("not found");
      return definitions[path];
    },
  };
  vm.runInNewContext(code, context);
  return {
    descriptions: context.RunDescriptions,
    calls,
    delay(path) {
      const promise = new Promise((done) => { release = done; });
      delayed.set(path, promise);
      return (value) => release(value);
    },
  };
}

test("loads the description of the exact workflow version and refetches on later hover", async () => {
  const path = "/api/orchestrator/workflows/demo/versions/2";
  const h = harness({ [path]: { name: "demo", version: 2, description: "Version two" } });
  const run = { workflow: "demo", workflow_version: 2 };
  assert.equal(h.descriptions.current(run), "");
  assert.equal(await h.descriptions.refresh(run), "Version two");
  assert.equal(h.descriptions.current(run), "Version two");
  assert.equal(await h.descriptions.refresh(run), "Version two");
  assert.deepEqual(h.calls, [["GET", path], ["GET", path]], "a later hover sees in-place edits from another tab");
});

test("never substitutes the newest description when a run has no recorded version", async () => {
  const h = harness();
  assert.equal(h.descriptions.refOf({ workflow: "demo" }), "");
  assert.equal(await h.descriptions.refresh({ workflow: "demo" }), "");
  assert.deepEqual(h.calls, []);
});

test("shares an in-flight hover request, accepts editor updates, and hides load failures", async () => {
  const path = "/api/orchestrator/workflows/demo/versions/1";
  const h = harness();
  const resolve = h.delay(path);
  const run = { workflow: "demo", workflow_version: 1 };
  const first = h.descriptions.refresh(run);
  const second = h.descriptions.refresh(run);
  assert.equal(h.calls.length, 1, "mouseenter and focus share their request");
  assert.equal(h.descriptions.update({ name: "demo", version: 1, description: "Edited while loading" }), true);
  resolve({ description: "Original" });
  assert.deepEqual(await Promise.all([first, second]), ["Edited while loading", "Edited while loading"], "a stale response cannot replace a saved edit");
  assert.equal(h.descriptions.update({ name: "demo", version: 1, description: "Edited in place" }), true);
  assert.equal(h.descriptions.current(run), "Edited in place");

  const missing = harness();
  assert.equal(await missing.descriptions.refresh(run), "");
  assert.equal(missing.descriptions.current(run), "");
});

test("the Runs table keeps description tooltip separate from truncated workflow-name text", () => {
  assert.match(source, /className: "run-workflow-name"/);
  assert.match(source, /\(workflowName \|\| tr\)\.addEventListener\("mouseenter", describe\)/, "filtered runs use the row when the workflow column is hidden");
  assert.match(source, /\[description, failure\]\.filter\(Boolean\)\.join\("\\n\\n"\)/, "the filtered row preserves its failure reason");
  assert.match(source, /document\.addEventListener\("workflowdescriptionchange"/);
});
