import assert from "node:assert/strict";
import fs from "node:fs";
import test from "node:test";
import vm from "node:vm";

const source = fs.readFileSync(new URL("web/inputs.js", import.meta.url), "utf8");

// Select Inputs with a remembered text input, so Run again starts a run at once.
function setup() {
  const calls = [];
  const context = {
    api: async (...args) => { calls.push(args); return { trace_id: "t1", workflow_version: 2 }; },
    loadBlob: async () => ({ text: "{{task}}" }),
    creatorHeaders: () => ({ "X-Creator": "ada" }),
    runIndex: { refresh: async () => {} },
    location: { hash: "#workflows/demo" },
    setSaveState() {}, setStatus(message) { context.error = message; },
    document: { dispatchEvent() {} }, CustomEvent: class { constructor(type, init) { Object.assign(this, { type }, init); } },
    localStorage: { getItem: () => JSON.stringify({ mode: "text", text: "Fix the bug", template: "default" }), setItem() {} },
  };
  vm.runInNewContext(`${source}\nglobalThis.out = Inputs;`, context);
  return { Inputs: context.out, calls, context };
}

test("a run from the editor names the version on the canvas; without one it runs the newest", async () => {
  const { Inputs, calls } = setup();
  await Inputs.runAgain("team demo", 2);
  assert.equal(calls[0][0], "POST");
  assert.equal(calls[0][1], "/api/orchestrator/workflows/team%20demo/runs?version=2");
  assert.equal(calls[0][2], "Fix the bug");
  assert.deepEqual({ ...calls[0][3] }, { "X-Creator": "ada" });
  await Inputs.runAgain("team demo");
  assert.equal(calls[1][1], "/api/orchestrator/workflows/team%20demo/runs");
  // A grouped name is one part of the path.
  await Inputs.runAgain("ecc/ECC-plan", 3);
  assert.equal(calls[2][1], "/api/orchestrator/workflows/ecc%2FECC-plan/runs?version=3");
});

test("a run is refused while the editor has unsaved edits", async () => {
  const { Inputs, calls, context } = setup();
  Inputs.setBlocker(() => "Needs the workflow saved first: a run uses a saved version");
  await Inputs.runAgain("demo", 3);
  assert.equal(calls.length, 0);
  assert.match(context.error, /saved first/);
});

test("the Text input takes a file from the file dialog or a drop, and only a text file", () => {
  assert.match(source, /textUpload = h\("button", \{ type: "button", title: "[^"]+" \}, icon\("upload"\), " Upload file"\)/);
  assert.match(source, /textUpload\.onclick = \(\) => textPicker\.click\(\)/, "the button opens the file dialog");
  assert.match(source, /text\.addEventListener\("drop", \(ev\) => \{\s*if \(!dragsFile\(ev\)\) return;\s*ev\.preventDefault\(\);/, "a dropped file is taken, dragged text is left alone");
  assert.match(source, /takeFile = async \(file\) => \{\s*if \(!file\) return;\s*if \(!isText\(file\)\) \{ setStatus\(`Not added: \$\{file\.name\} \(not a text file\)`, "error"\); return; \}/);
  const css = fs.readFileSync(new URL("web/style.css", import.meta.url), "utf8");
  assert.match(css, /\.inputs-text\.drop \{ outline: 2px dashed var\(--accent\)/);
});

test("a started run opens on Runs, as whatever is created opens", async () => {
  const { Inputs, context } = setup();
  await Inputs.runAgain("ecc/ECC-plan", 3);
  assert.equal(context.error, undefined);
  assert.equal(context.location.hash, "#runs/t1", "the new run, by the trace ID the orchestrator returned");
  assert.match(source, /location\.hash = started === 1 && last \? `#runs\/\$\{encodeURIComponent\(last\)\}` : `#runs\?workflow=\$\{encodeURIComponent\(workflow\)\}`;/, "several runs open as the workflow's runs");
});
