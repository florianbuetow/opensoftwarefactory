import assert from "node:assert/strict";
import fs from "node:fs";
import test from "node:test";
import vm from "node:vm";

const source = fs.readFileSync(new URL("web/runs.js", import.meta.url), "utf8");
const app = fs.readFileSync(new URL("web/app.js", import.meta.url), "utf8");
const fillSource = app.slice(app.indexOf("function fill("), app.indexOf("// createTree"));
// The timeline names a step through RunsLogic, which reads the pass of a step with stepPass.
const passSource = app.slice(app.indexOf("const PASS_PART ="), app.indexOf("// runIndex knows every run"));
const logicSource = source.slice(source.indexOf("function skillToolWarning("), source.indexOf("(() => {\n  const MAX_ROWS"));
const helpers = fillSource + passSource + logicSource + source.slice(source.indexOf("  async function executionRecords"), source.indexOf("  async function renderDetail"));

function h(tag, props = {}, ...children) {
  const node = {
    tag, props, children: [],
    append(...items) { attach(this, items.flat().filter((item) => item !== null && item !== undefined)); },
    // Native semantics: null and undefined are not skipped, they become the text "null"/"undefined".
    replaceChildren(...items) { this.children = []; attach(this, items.map((item) => (item === null || item === undefined ? String(item) : item))); },
    setAttribute(name, value) { this.props[name] = value; },
    closest(selector) { for (let item = this; item; item = item.parent) if (selector === item.tag) return item; return null; },
    querySelectorAll(selector) {
      const [className, attr] = selector.replace(/^\./, "").split("[");
      return allNodes(this).filter((item) => String(item.props?.className || "").split(" ").includes(className)
        && (!attr || String(item.props?.[attr.split("=")[0]]) === attr.split("=")[1].replace(/]$/, "")));
    },
  };
  node.append(...children);
  return node;
}
function attach(parent, children) { for (const child of children) { if (child && typeof child === "object") child.parent = parent; parent.children.push(child); } }
function allNodes(root) { return !root || typeof root !== "object" ? [] : [root, ...(root.children || []).flatMap(allNodes)]; }
function setup(loadBlob = async () => ({ text: "{}" }), api = async () => ({ logs: [] })) {
  const context = {
    h, loadBlob, api, URLSearchParams,
    statusChip: (state) => h("chip", { state }),
    titleCase: (value) => value[0]?.toUpperCase() + value.slice(1),
    rootError: (value) => value,
    pretty: (value) => value,
    promptEvidenceLinks: (record) => record.prompt_evidence ? h("section", { className: "prompt-evidence-links" }) : null,
  };
  vm.runInNewContext(`${helpers}\nglobalThis.timeline = { executionRecords, executionTimeline, telemetryWarning };`, context);
  return context.timeline;
}
const nodes = (root, className) => {
  if (!root || typeof root !== "object") return [];
  return [...(root.props?.className === className ? [root] : []), ...(root.children || []).flatMap((child) => nodes(child, className))];
};

test("execution timeline stays compact and shows attempts only when retries exist", async () => {
  const { executionTimeline } = setup();
  const timeline = executionTimeline([{ step: "research", attempts: [
    { attempt: 1, status: "failed", events: [{ kind: "tool_call", name: "lookup", status: "failed", duration_ms: 1250, error: "upstream closed", error_category: "transport" }] },
    { attempt: 2, status: "completed", events: [{ kind: "model_call", model: "qwen", status: "completed", duration_ms: 900 }] },
  ] }], { trace: "11111111-1111-1111-1111-111111111111", artifacts: {} });
  assert.equal(nodes(timeline, "timeline-attempt").map((node) => node.children[0]).join(","), "Attempt 1,Attempt 2");
  assert.equal(nodes(timeline, "timeline-duration").length, 0);
  assert.equal(nodes(timeline, "timeline-error").length, 0);
  assert.equal(nodes(timeline, "timeline-select")[0].props.title, "upstream closed");
  nodes(timeline, "timeline-select")[0].onclick(); await new Promise(setImmediate);
  assert.ok(nodes(timeline, "evidence-metrics").length);
  assert.ok(nodes(timeline, "error-line").some((node) => node.children[0] === "upstream closed"));
});

test("the trace of an event is searched in the time its attempt ran, or its run for a record without times", async () => {
  const requested = [];
  const { executionTimeline } = setup(undefined, async (_method, path) => { requested.push(path); return path.includes("/spans?") ? { spans: [], partial: true } : { logs: [] }; });
  const spansOf = async (record, context) => {
    requested.length = 0;
    const timeline = executionTimeline([{ step: "research", attempts: [record] }], { trace: "11111111-1111-1111-1111-111111111111", artifacts: {}, ...context });
    nodes(timeline, "timeline-select")[0].onclick(); await new Promise(setImmediate); await new Promise(setImmediate);
    return { query: new URLSearchParams(requested.find((path) => path.includes("/spans?")).split("?")[1]), timeline };
  };
  const event = { kind: "model_call", status: "completed", trace_id: "a".repeat(32) };
  const timed = await spansOf({ status: "completed", trace_id: "a".repeat(32), started: "2026-11-26T23:53:22Z", finished: "2026-11-26T23:53:42Z", events: [event] }, {});
  assert.deepEqual([timed.query.get("trace_id"), timed.query.get("start"), timed.query.get("end")], ["a".repeat(32), "2026-11-26T23:53:22Z", "2026-11-26T23:53:42Z"]);
  const old = await spansOf({ status: "completed", trace_id: "a".repeat(32), events: [event] }, { runStarted: "2026-11-20T10:00:00Z", runFinished: "2026-11-20T10:05:00Z" });
  assert.deepEqual([old.query.get("start"), old.query.get("end")], ["2026-11-20T10:00:00Z", "2026-11-20T10:05:00Z"], "an old record uses the time of its run");
  // A search that could not cover everything says so, instead of claiming there is no trace.
  assert.ok(nodes(old.timeline, "evidence-warning").some((node) => /could be searched/.test(node.children[0])));
});

test("an opened event stays open when the run redraws itself", async () => {
  const { executionTimeline } = setup();
  const records = () => [{ step: "research", attempts: [
    { attempt: 1, status: "failed", events: [{ kind: "tool_call", name: "lookup", status: "failed", error: "upstream closed" }] },
    { attempt: 2, status: "failed", error: "gave up", events: [] },
  ] }];
  const context = { trace: "11111111-1111-1111-1111-111111111111", artifacts: {} };
  const first = executionTimeline(records(), context);
  nodes(first, "timeline-select")[1].onclick(); await new Promise(setImmediate);
  const opened = nodes(first, "execution-evidence")[0];
  // The poll rebuilds the timeline from freshly read records every few seconds.
  const redrawn = executionTimeline(records(), context);
  assert.equal(nodes(redrawn, "execution-evidence")[0], opened, "the same panel is shown again, with what it had loaded");
  assert.equal(opened.hidden, false);
  assert.ok(nodes(redrawn, "error-line").some((node) => node.children[0] === "gave up"));
  assert.deepEqual(nodes(redrawn, "timeline-select").map((node) => node.props["aria-selected"]), ["false", "true"]);
  // Another event can still be chosen afterwards, and it replaces the first one.
  nodes(redrawn, "timeline-select")[0].onclick(); await new Promise(setImmediate);
  assert.ok(nodes(redrawn, "error-line").some((node) => node.children[0] === "upstream closed"));
  const other = executionTimeline(records(), { trace: "22222222-2222-2222-2222-222222222222", artifacts: {} });
  assert.equal(nodes(other, "execution-evidence")[0].props.hidden, true, "another run opens with nothing selected");
});

test("token estimates and processing time appear once in selected evidence", async () => {
  const { executionTimeline } = setup();
  const timeline = executionTimeline([{ step: "write", attempts: [{ status: "completed", estimated_input_tokens: 120, estimated_output_tokens: 40, processing_time_ms: 2100,
    events: [{ kind: "model_call", status: "completed", estimated_input_tokens: 100, estimated_output_tokens: 32 }] }],
  }], { trace: "11111111-1111-1111-1111-111111111111", artifacts: {} });
  assert.equal(nodes(timeline, "timeline-estimate").length, 0);
  assert.equal(nodes(timeline, "timeline-duration").length, 0);
  nodes(timeline, "timeline-select")[0].onclick(); await new Promise(setImmediate);
  assert.deepEqual(nodes(timeline, "evidence-metrics")[0].children.map((node) => node.children[0]), ["Approx. input", "≈100 tokens", "Approx. output", "≈32 tokens"]);
});

test("execution records load immutable attempts instead of duplicating the latest summary", async () => {
  const loaded = [];
  const { executionRecords } = setup(async (id) => { loaded.push(id); return { text: JSON.stringify({ attempt: loaded.length }) }; });
  const records = await executionRecords("run-1", {
    records: new Set(["step"]),
    attemptRecords: new Map([["step", ["runs/run-1/step.attempt-1.run.json", "runs/run-1/step.attempt-2.run.json"]]]),
  }, "step");
  assert.deepEqual(Array.from(records, (record) => record.attempt), [1, 2]);
  assert.equal(loaded.includes("runs/run-1/step.run.json"), false);
});

test("legacy failed records still produce a useful timeline row", () => {
  const { executionTimeline } = setup();
  const timeline = executionTimeline([{ step: "build", attempts: [{ status: "failed", error: "compiler failed" }] }]);
  assert.equal(nodes(timeline, "timeline-event")[0].children[0], "Attempt failed");
  assert.equal(nodes(timeline, "timeline-select")[0].props.title, "compiler failed");
});

test("a terminal failure remains visible after earlier successful events", () => {
  const { executionTimeline } = setup();
  const timeline = executionTimeline([{ step: "build", attempts: [{ status: "failed", error: "artifact write failed", events: [
    { kind: "model_call", status: "completed", started: "2026-01-01T00:00:00Z", duration_ms: 10 },
  ] }] }]);
  assert.deepEqual(nodes(timeline, "timeline-event").map((node) => node.children[0]), ["Model call", "Attempt failed"]);
  assert.equal(nodes(timeline, "timeline-select")[1].props.title, "artifact write failed");
});

test("a running record is visible before its first event finishes", () => {
  const { executionTimeline } = setup();
  const timeline = executionTimeline([{ step: "research", attempts: [{ status: "running", attempt: 2, events: [] }] }]);
  assert.equal(nodes(timeline, "timeline-event")[0].children[0], "Attempt running");
  assert.equal(nodes(timeline, "timeline-attempt")[0].children[0], "Attempt 2");
});

test("concurrent and routed events are globally chronological", () => {
  const { executionTimeline } = setup();
  const timeline = executionTimeline([
    { step: "route/research", attempts: [{ attempt: 1, events: [
      { kind: "tool_call", name: "lookup", status: "completed", started: "2026-01-01T00:00:03Z" },
    ] }] },
    { step: "prepare", attempts: [{ attempt: 1, events: [
      { kind: "model_call", status: "completed", started: "2026-01-01T00:00:01Z" },
    ] }] },
  ]);
  assert.deepEqual(nodes(timeline, "timeline-step").map((node) => node.children[0]), ["Prepare", "Route / Research"]);
});

test("selecting an event shows its artifacts, recorded tool evidence and correlated logs", async () => {
  const requested = [];
  const { executionTimeline } = setup(undefined, async (_method, path) => {
    requested.push(path);
    if (path.includes("/spans?")) return { spans: [{ span_id: "span-a", operation: "mcp.call", service: "agent", duration: 1200 }] };
    return { logs: [{ container: "mcp-gateway", level: "INFO", message: "MCP request completed" }] };
  });
  const timeline = executionTimeline([{ step: "research", attempts: [{
    attempt: 1, status: "completed", _object_id: "runs/run-1/research.attempt-1.run.json", trace_id: "trace-a",
    messages: [
      { role: "assistant", tool_calls: [{ id: "call-1", function: { arguments: "{\"query\":\"safe\"}" } }] },
      { role: "tool", tool_call_id: "call-1", content: "result" },
    ],
    events: [{ kind: "tool_call", name: "lookup", status: "completed", tool_call_id: "call-1", trace_id: "trace-a", span_id: "span-a", input_artifacts: ["runs/run-1/input"], output_artifact: "runs/run-1/research" }],
  }] }], { trace: "11111111-1111-1111-1111-111111111111", artifacts: {}, retentionDays: 7 });
  const button = nodes(timeline, "timeline-select")[0];
  button.onclick();
  await new Promise(setImmediate);
  assert.equal(button.props["aria-selected"], "true");
  assert.match(requested[0], /run_id=11111111-1111-1111-1111-111111111111/);
  assert.match(requested[0], /step=research/);
  assert.match(requested[0], /attempt=1/);
  assert.deepEqual(allNodes(timeline).filter((item) => item.tag === "a").map((item) => item.children[0]), ["View input", "View output", "View execution record"]);
  assert.ok(allNodes(timeline).some((item) => item.children?.includes("MCP request completed")));
  assert.ok(allNodes(timeline).some((item) => item.children?.includes("result")));
  assert.ok(allNodes(timeline).some((item) => item.children?.includes("mcp.call")));
});

test("an unfinished checkpoint in a failed attempt is shown as interrupted with missing evidence", async () => {
  const { executionTimeline } = setup(undefined, async () => ({ logs: [] }));
  const timeline = executionTimeline([{ step: "build", attempts: [{ status: "failed", error: "pod exited", events: [
    { kind: "model_call", status: "running", started: "2026-01-01T00:00:00Z" },
  ] }] }], { trace: "11111111-1111-1111-1111-111111111111", artifacts: {} });
  assert.equal(nodes(timeline, "timeline-event")[0].children[0], "Model call · Interrupted");
  const button = nodes(timeline, "timeline-select")[0];
  button.onclick();
  await new Promise(setImmediate);
  assert.ok(nodes(timeline, "evidence-warning").some((item) => String(item.children[0]).includes("its end was never recorded")));
  assert.ok(nodes(timeline, "evidence-warning").some((item) => String(item.children[0]).includes("No matching logs")));
});

test("a running checkpoint is interrupted when the workflow is already terminal", () => {
  const { executionTimeline } = setup();
  const timeline = executionTimeline([{ step: "research", attempts: [{ status: "running", events: [
    { kind: "tool_call", name: "lookup", status: "running" },
  ] }] }], { runStatus: "Failed" });
  assert.equal(nodes(timeline, "timeline-event")[0].children[0], "Tool call · lookup · Interrupted");
});

test("a terminal workflow marks an eventless running checkpoint as interrupted", () => {
  const { executionTimeline } = setup();
  const timeline = executionTimeline([{ step: "research", attempts: [{ status: "running", events: [] }] }], { runStatus: "Failed", runError: "worker exited" });
  assert.equal(nodes(timeline, "timeline-event")[0].children[0], "Attempt interrupted");
  assert.equal(nodes(timeline, "timeline-select")[0].props.title, "worker exited");
});

test("missing logs beyond configured retention are identified as possibly expired", async () => {
  const { executionTimeline } = setup(undefined, async (_method, path) => path.includes("/spans?") ? { spans: [] } : { logs: [] });
  const timeline = executionTimeline([{ step: "old", attempts: [{ started: "2020-01-01T00:00:00Z", events: [
    { kind: "model_call", status: "completed", trace_id: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", span_id: "bbbbbbbbbbbbbbbb" },
  ] }] }], { trace: "11111111-1111-1111-1111-111111111111", artifacts: {}, retentionDays: 7 });
  nodes(timeline, "timeline-select")[0].onclick();
  await new Promise(setImmediate);
  assert.ok(nodes(timeline, "evidence-warning").some((item) => String(item.children[0]).includes("kept for 7 days")));
});

test("telemetry failures produce a visible warning while healthy telemetry stays quiet", async () => {
  const unhealthy = setup(undefined, async () => ({ status: "degraded", components: { traces: { status: "degraded", reason: "Trace delivery queue is near capacity" } } }));
  const warning = await unhealthy.telemetryWarning();
  assert.match(warning.children[0], /queue is near capacity/);
  const healthy = setup(undefined, async () => ({ status: "ok", components: {} }));
  assert.equal(await healthy.telemetryWarning(), null);
});

test("timeline CSS uses a compact responsive list and wraps long event names", () => {
  const css = fs.readFileSync(new URL("web/style.css", import.meta.url), "utf8");
  assert.match(css, /grid-template-columns: auto minmax\(0, 1fr\);\s*grid-template-areas: "status step" "status event"/);
  assert.match(css, /\.timeline-event-group \{[^}]*flex-wrap: wrap;/s);
  assert.doesNotMatch(css, /grid-template-columns:\s*auto minmax\(80px, \.6fr\) auto minmax\(160px, 1\.4fr\) auto auto/);
});

test("model call evidence links recorded prompt snapshots and states legacy absence plainly", async () => {
  const { executionTimeline } = setup();
  const show = async (record) => {
    const timeline = executionTimeline([{ step: "build", attempts: [record] }], { trace: "run-1", artifacts: {} });
    nodes(timeline, "timeline-select")[0].onclick(); await new Promise(setImmediate);
    return timeline;
  };
  const events = [{ kind: "model_call", status: "completed" }];
  const recorded = await show({ attempt: 1, status: "completed", events, prompt_evidence: { effective_prompt_artifact: "runs/run-1/_evidence/build/attempt-1/systemprompt.effective.md" } });
  assert.equal(nodes(recorded, "prompt-evidence-links").length, 1);
  const legacy = await show({ attempt: 1, status: "completed", events });
  assert.equal(nodes(legacy, "prompt-evidence-links").length, 0);
  assert.ok(nodes(legacy, "evidence-note").some((n) => /Prompt snapshots were not recorded/.test(n.children[0])));
});

const texts = (root) => allNodes(root).flatMap((node) => (node && typeof node === "object" ? node.children : [node])).filter((child) => typeof child === "string");

test("selected evidence, partial logs and partial spans never render null text under native replaceChildren", async () => {
  const api = async (_method, path) => path.includes("/spans?")
    ? { partial: false, spans: [{ span_id: "s1", operation: "op", service: "agent" }] }
    : { partial: false, logs: [{ container: "agent", level: "INFO", message: "ok" }] };
  const { executionTimeline } = setup(undefined, api);
  const timeline = executionTimeline([{ step: "build", attempts: [{ attempt: 1, status: "completed", trace_id: "t", started: "2026-01-01T00:00:00Z", finished: "2026-01-01T00:00:01Z",
    events: [{ kind: "model_call", status: "completed", span_id: "s1" }] }] }], { trace: "run-1", artifacts: {} });
  nodes(timeline, "timeline-select")[0].onclick(); await new Promise(setImmediate); await new Promise(setImmediate);
  assert.ok(nodes(timeline, "evidence-log-list").length && nodes(timeline, "evidence-span-list").length, "logs and spans rendered");
  assert.deepEqual(texts(timeline).filter((text) => text === "null" || text === "undefined"), []);
});

test("fill skips null, undefined and false where the native replaceChildren would print them", () => {
  const context = {};
  vm.runInNewContext(fillSource + "globalThis.fill = fill;", context);
  const parent = { children: [], replaceChildren(...items) { this.children = items.map(String); } };
  context.fill(parent, "a", null, undefined, false, [null, "b"]);
  assert.deepEqual(parent.children, ["a", "b"]);
});

test("no native replaceChildren call in the UI passes a top-level nullable argument", () => {
  const offenders = [];
  for (const name of fs.readdirSync(new URL("web/", import.meta.url)).filter((file) => file.endsWith(".js"))) {
    const text = fs.readFileSync(new URL(`web/${name}`, import.meta.url), "utf8");
    for (const match of text.matchAll(/\.replaceChildren\(/g)) {
      let depth = 1, at = match.index + match[0].length, top = "";
      for (; depth && at < text.length; at++) {
        const ch = text[at];
        if (ch === "(" || ch === "[" || ch === "{") depth++; else if (ch === ")" || ch === "]" || ch === "}") depth--;
        if (depth === 1) top += ch;
      }
      if (/:\s*null\b|\bnull\s*[,)]/.test(top) && !/\.filter\(/.test(text.slice(match.index, at))) offenders.push(`${name}:${text.slice(0, match.index).split("\n").length}`);
    }
  }
  assert.deepEqual(offenders, []);
});
