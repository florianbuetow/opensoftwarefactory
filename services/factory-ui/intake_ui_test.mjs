import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";

const read = (file) => fs.readFileSync(new URL(`web/${file}`, import.meta.url), "utf8");
const source = read("intake.js");
// The logic part of intake.js needs no DOM; the page part after it does.
const context = vm.createContext({});
vm.runInContext(`${source.slice(0, source.indexOf("// ---- Intake page ----"))}; globalThis.L = IntakeLogic;`, context);
const L = context.L;
const plain = (value) => JSON.parse(JSON.stringify(value));

test("workflows are listed by name with their versions newest first", () => {
  const flows = L.workflows({ workflows: [{ name: "b/two", version: 1, versions: [1] }, { name: "a/one", version: 3, versions: [1, 2, 3] }] });
  assert.deepEqual(plain(flows), [{ name: "a/one", versions: [3, 2, 1] }, { name: "b/two", versions: [1] }]);
  assert.deepEqual(plain(L.workflows([{ name: "solo", version: 2 }])), [{ name: "solo", versions: [2] }]);
});

test("a trigger's draft round-trips into the stored binding", () => {
  const stored = { id: "t1", revision: "r1", name: "Review failures", source: "factory", workflow: "ops/triage", workflow_version: 2, source_workflows: ["a/one"], outcomes: ["failed"], enabled: true };
  const draft = L.draft(stored);
  assert.deepEqual(plain(L.problems(draft)), []);
  assert.deepEqual(plain(L.body(stored, draft)), stored);
  assert.ok(L.same(draft, L.draft(stored)));
  draft.enabled = false;
  assert.ok(!L.same(draft, L.draft(stored)));
});

test("an incomplete trigger names each missing field in one sentence", () => {
  assert.deepEqual(plain(L.problems(L.draft({}))), ["Enter a name.", "Choose a source workflow.", "Choose an outcome.", "Choose the workflow to start."]);
});

test("a multi-select names one choice and counts more", () => {
  const label = (v) => v.toUpperCase();
  assert.equal(L.chosen([], label, "workflows", "No workflows"), "No workflows");
  assert.equal(L.chosen(["a"], label, "workflows", "No workflows"), "A");
  assert.equal(L.chosen(["a", "b"], label, "workflows", "No workflows"), "2 workflows");
  assert.equal(L.chosen(["a", "b"], label, "workflows", "No workflows", ["a", "b"]), "All workflows");
  assert.equal(L.chosen(["a"], label, "workflows", "No workflows", ["a", "b"]), "A");
  const outcomes = L.OUTCOMES.map(([value]) => value);
  assert.deepEqual(plain(outcomes), ["completed", "failed", "cancelled", "blocked", "terminated", "timed_out"]);
  assert.equal(L.chosen(outcomes, label, "outcomes", "No outcomes", outcomes), "All outcomes");
});

function pickerHarness(values, options, noun = "workflows") {
  let popup, applied;
  const h = (tag, props = {}, ...children) => ({ tag, ...props, children: children.flat(), listeners: {},
    addEventListener(type, fn) { this.listeners[type] = fn; } });
  const ctx = vm.createContext({ h, L, openPopover(_trigger, content) { popup = content; return { close() {} }; }, renderForm() {}, $() { return { focus() {} }; } });
  const start = source.indexOf("  function picker(");
  vm.runInContext(`${source.slice(start, source.indexOf("  const field =", start))}\nglobalThis.makePicker = picker;`, ctx);
  const trigger = ctx.makePicker("test-picker", values, options, (value) => value, noun, "None", (next) => { applied = plain(next); });
  trigger.onclick();
  const nodes = (node) => typeof node !== "object" || !node ? [] : [node, ...(node.children || []).flatMap(nodes)];
  const all = nodes(popup);
  return { trigger, master: all.find((n) => n.tag === "input" && n["aria-label"] === `All ${noun}`),
    checks: all.filter((n) => n.tag === "input" && n.value),
    apply: () => all.find((n) => n.tag === "button" && n.children[0] === "Apply").onclick(),
    cancel: () => all.find((n) => n.tag === "button" && n.children[0] === "Cancel").onclick(),
    applied: () => applied };
}

test("All workflows selects explicit choices, supports partial selection and clearing, and Cancel changes nothing", () => {
  const ui = pickerHarness(["one"], ["one", "two"]);
  assert.equal(ui.master.indeterminate, true);
  ui.master.checked = true;
  ui.master.listeners.change();
  assert.ok(ui.checks.every((check) => check.checked));
  assert.equal(ui.applied(), undefined, "selection remains a draft until Apply");
  ui.apply();
  assert.deepEqual(ui.applied(), ["one", "two"]);
  ui.checks[0].checked = false;
  ui.checks[0].listeners.change();
  assert.equal(ui.master.checked, false);
  assert.equal(ui.master.indeterminate, true);
  ui.apply();
  assert.deepEqual(ui.applied(), ["two"]);
  ui.master.checked = false;
  ui.master.listeners.change();
  ui.apply();
  assert.deepEqual(ui.applied(), []);
  const cancelled = pickerHarness([], ["one"]);
  cancelled.master.checked = true;
  cancelled.master.listeners.change();
  cancelled.cancel();
  assert.equal(cancelled.applied(), undefined);
  assert.equal(pickerHarness([], []).master.disabled, true);
});

test("All outcomes includes every supported terminal status without a wildcard payload", () => {
  const outcomes = plain(L.OUTCOMES.map(([value]) => value));
  const ui = pickerHarness([], outcomes, "outcomes");
  ui.master.checked = true;
  ui.master.listeners.change();
  ui.apply();
  assert.deepEqual(ui.applied(), outcomes);
  const draft = L.draft({ source_workflows: ["source"], outcomes: ui.applied() });
  assert.deepEqual(plain(L.body({}, draft).outcomes), outcomes);
});

test("the status line counts waiting events and ignores a zero time", () => {
  assert.deepEqual(plain(L.statusParts({ pending: 1, last_accepted: "0001-01-01T00:00:00Z" })), { pending: "1 event waiting", accepted: "", error: "" });
  assert.deepEqual(plain(L.statusParts({ pending: 3, last_accepted: "2026-11-21T10:00:00Z", last_error: "boom" })), { pending: "3 events waiting", accepted: "2026-11-21T10:00:00Z", error: "boom" });
});

test("a new trigger chooses its input connector; only Factory events can be chosen yet", () => {
  assert.deepEqual(plain(L.SOURCES.map((s) => [s.name, s.available])), [["Factory events", true], ["Slack", false], ["Linear", false], ["Jira", false]]);
  assert.equal(L.sourceName("factory"), "Factory events");
  assert.match(source, /title: "New trigger"[\s\S]*"Input connector"/);
  assert.match(read("app.js"), /const PAGES = \[\n  \{ name: "intake"/);
});

test("Intake follows the page structure and the shared controls", () => {
  const html = read("index.html");
  assert.match(html, /id="tab-intake"[^>]*>\s*<div class="panel list-panel">\s*<div class="panel-head"><h2>Event triggers<\/h2><span id="intake-new-slot">/);
  assert.match(read("app.js"), /\n  intake: \[\{ column: "\.list-panel"/);
  // No list boxes, raw JSON status, or sentence under the controls.
  assert.doesNotMatch(source, /multiple|measurement-json|className: "muted" \}, "/);
  assert.match(source, /"No event triggers yet"/);
  assert.match(source, /multi-select-trigger/);
  assert.match(source, /Unticked, the trigger starts nothing and its events are kept; ticked again, it catches up/);
});
