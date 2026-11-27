import { test } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";

const read = (name) => fs.readFileSync(new URL(`web/${name}`, import.meta.url), "utf8");
const source = read("shortcuts.js");

function setup() {
  const listeners = [];
  const notices = [], dialogs = [];
  const run = { hidden: false, disabled: false, title: "Opens Select Inputs to start runs of v3", clicks: 0, click() { this.clicks++; } };
  const jump = { focused: 0, focus() { this.focused++; }, select() {} };
  const context = {
    location: { hash: "#overview" },
    activeTab: "overview",
    menu: { hidden: true },
    decisionsState: { pending: [{ workflow_id: "w1", step: "accept" }, { workflow_id: "w2", step: "review" }] },
    notify: (text) => notices.push(text),
    openDialog: (config) => { dialogs.push(config); return { close() {} }; },
    h: (tag, props, ...children) => ({ tag, props, children }),
    $: (id) => (id === "trace-jump" ? jump : null),
    Date,
    document: {
      addEventListener: (type, fn) => { if (type === "keydown") listeners.push(fn); },
      querySelector: (sel) => (sel === "#tab-workflows .wf-run" ? run : null),
    },
  };
  vm.runInNewContext(`${source}\nglobalThis.S = ShortcutsLogic;`, context);
  const press = (key, extra = {}) => {
    let prevented = false;
    const target = { closest: (sel) => (extra.inField && sel.includes("input") ? {} : null) };
    for (const fn of listeners) fn({ key, shiftKey: key === "?" || !!extra.shift, metaKey: false, ctrlKey: false, altKey: false, defaultPrevented: false, target, preventDefault: () => { prevented = true; } });
    return prevented;
  };
  return { context, press, notices, dialogs, run, jump };
}

test("G then a letter goes to a page, and only within the moment after G", () => {
  const { context, press } = setup();
  press("g"); press("r");
  assert.equal(context.location.hash, "#runs");
  press("g"); press("d");
  assert.equal(context.location.hash, "#decisions");
  press("g"); press("x"); // no page: nothing
  assert.equal(context.location.hash, "#decisions");
  press("g", { inField: true }); press("o", { inField: true });
  assert.equal(context.location.hash, "#decisions", "typing in a field is never a shortcut");
});

test("J and K open the next and previous waiting decision, in the order Decisions lists them", () => {
  const { context, press, notices } = setup();
  press("j");
  assert.equal(context.location.hash, `#decisions/${encodeURIComponent("p:w1|accept")}`);
  press("j");
  assert.equal(context.location.hash, `#decisions/${encodeURIComponent("p:w2|review")}`);
  press("j");
  assert.equal(context.location.hash, `#decisions/${encodeURIComponent("p:w1|accept")}`, "wraps around");
  press("k");
  assert.equal(context.location.hash, `#decisions/${encodeURIComponent("p:w2|review")}`);
  context.decisionsState.pending = [];
  press("j");
  assert.deepEqual(notices, ["No decision waits"]);
});

test("R runs the workflow on the canvas, or says why it cannot; / jumps to a run; ? lists the shortcuts", () => {
  const { context, press, run, jump, dialogs, notices } = setup();
  press("r");
  assert.equal(run.clicks, 0, "only on Workflows");
  context.activeTab = "workflows";
  press("r");
  assert.equal(run.clicks, 1, "presses the visible Run…");
  run.disabled = true; run.title = "Needs the workflow saved first: a run uses a saved version";
  press("r");
  assert.equal(run.clicks, 1);
  assert.equal(notices.at(-1), "Not started: needs the workflow saved first: a run uses a saved version");
  assert.equal(press("/"), true);
  assert.equal(jump.focused, 1);
  press("?");
  assert.equal(dialogs.at(-1).title, "Keyboard shortcuts");
  // Nothing fires over an open menu.
  context.menu.hidden = false;
  context.location.hash = "#runs";
  press("g"); press("o");
  assert.equal(context.location.hash, "#runs");
  assert.match(read("index.html"), /<script src="shortcuts\.js"><\/script>/);
});

test("the trace ID jump opens the run a typed ID names, from any page", () => {
  const app = read("app.js"), html = read("index.html");
  const context = {};
  vm.runInNewContext(`${app.slice(app.indexOf("function traceMatch("), app.indexOf("// The top bar's trace ID jump"))}\nglobalThis.f = traceMatch;`, context);
  const ids = ["a8a79b74-c4a3-425b-988c-13aea5d9099d", "a8a7ffff-0000-4000-8000-000000000000", "fa99a7ce-1111-4111-8111-111111111111"];
  assert.deepEqual({ ...context.f(ids, "fa99") }, { trace: "fa99a7ce-1111-4111-8111-111111111111", count: 1 });
  assert.deepEqual({ ...context.f(ids, "A8A7") }, { trace: "", count: 2 }, "two runs match: type more");
  assert.deepEqual({ ...context.f(ids, "a8a79b74c4a3") }, { trace: "a8a79b74-c4a3-425b-988c-13aea5d9099d", count: 1 }, "dashes ignored");
  assert.deepEqual({ ...context.f(ids, "a8a") }, { trace: "", count: 0 }, "too short to name a run");
  assert.equal(context.f([], "0b9e4c4e-2222-4222-8222-222222222222").trace, "0b9e4c4e-2222-4222-8222-222222222222", "a full ID opens before the list knows it");
  assert.match(html, /<form id="trace-jump-form" class="trace-jump" role="search">\s+<input id="trace-jump"[^>]*aria-label="Jump to a run by its trace ID"/);
  assert.match(app, /location\.hash = `#runs\/\$\{trace\}`;/);
  assert.match(app, /count > 1 \? `\$\{count\} runs match: type more of the ID`/);
});

test("rows are marked with Cmd/Ctrl+click, Shift+click or X, and the menu then acts on all of them", () => {
  const app = read("app.js"), runs = read("runs.js"), decisions = read("decisions.js"), style = read("style.css");
  const context = {};
  vm.runInNewContext(`${app.slice(app.indexOf("function marks("), app.indexOf("// markableRow is clickableRow"))}\nglobalThis.m = marks();`, context);
  const m = context.m, ids = ["a", "b", "c", "d"];
  m.toggle("b");
  m.range(ids, "d");
  assert.deepEqual([...m.set].sort(), ["b", "c", "d"], "Shift+click marks from the last one marked");
  m.toggle("c");
  assert.deepEqual([...m.set].sort(), ["b", "d"]);
  m.keep(["a", "b"]);
  assert.deepEqual([...m.set], ["b"], "a row that left the list is no longer marked");
  assert.equal(m.clear(), true);
  assert.equal(m.size, 0);
  // Runs: re-run or remove every marked run; Decisions: remove marked decided gates, never decide.
  assert.match(runs, /\[`Re-run \$\{runMarks\.size\} runs`, \(\) => rerunMany\(marked\(\)\),/);
  assert.match(runs, /\[`Remove \$\{runMarks\.size\} runs`, \(\) => clearRuns\(marked\(\)\), "danger"/);
  assert.match(runs, /setSaveState\(started === 1 \? "Started" : `Started \$\{started\} runs`\)/);
  assert.match(decisions, /return c\.kind === "decided" \? markableRow\(tr, c\.id, \(\) => decided\.map\(\(x\) => x\.id\), decidedMarks, open, renderTable\) : clickableRow\(tr, open\);/);
  assert.match(decisions, /\[`Remove \$\{marked\.length\} decisions`, \(\) => removeManyDecided\(marked\), "danger"/);
  assert.match(style, /\.data-table tbody tr\.marked \{ background: rgb\(47 111 222 \/ \.07\); box-shadow: inset 3px 0 0 var\(--accent\); \}/);
  assert.match(app, /if \(rowMarks\.has\(id\)\) tr\.firstChild\?\.append\(h\("span", \{ className: "sr-only" \}, ", marked"\)\);/, "a screen reader hears it");
});

test("names keep the factory's acronyms in capitals, the Agents tree reads in title case, and nothing moves for it", () => {
  const app = read("app.js"), registry = read("registry.js"), runs = read("runs.js"), workflows = read("workflows.js");
  const context = {};
  vm.runInNewContext(`${app.slice(app.indexOf("const titleCase"), app.indexOf("// midTruncate"))}\nglobalThis.t = titleCase;`, context);
  assert.equal(context.t("ecc/ECC-plan"), "ECC / ECC Plan", "one form for a group and a name");
  assert.equal(context.t("llm-gateway"), "LLM Gateway");
  assert.equal(context.t("api-review"), "API Review");
  assert.equal(context.t("egm/EGM-goldfish-style"), "EGM / EGM Goldfish Style");
  assert.equal(context.t("small-fix"), "Small Fix");
  assert.equal(context.t("review-elephant"), "Review Elephant");
  assert.match(runs, /return text\.replace\(\/\^\[A-Za-z\]\+\/, \(word\) => titleCase\(word\)\);/, '"LLM gateway: 404", never "Llm"');
  assert.match(registry, /label: titleCase\(leafOf\(e\.name\)\), title: e\.name,/);
  assert.match(registry, /label: titleCase\(leafOf\(f\)\), title: f, children/);
  // A group typed as shown is the existing one; a rename that only changes case keeps the ID.
  assert.match(workflows, /g = \[\.\.\.groupList\(\)\]\.find\(\(x\) => sameName\(x, typed\)\) \|\| typed;/);
  assert.match(workflows, /full = except && sameName\(`\$\{g\}\/\$\{n\}`, except\) \? except : `\$\{g\}\/\$\{n\}`;/);
});

test("tooltips say what happens or what is shown, never what to do", () => {
  const sources = ["app.js", "workflows.js", "runs.js", "decisions.js", "overview.js", "graph.js", "shortcuts.js", "registry.js", "artifacts.js", "cluster.js"].map(read).join("\n");
  const tips = [...sources.matchAll(/title: ["`]([^"`]+)["`]/g)].map((m) => m[1]);
  const imperative = tips.filter((t) => /^(Double-click|Click|Drag|Right-click|Press|Type|Select|Choose|Back to)\b/.test(t));
  assert.deepEqual(imperative, []);
  assert.match(read("workflows.js"), /title: `The description of v\$\{shownVersion\(\) \|\| 1\}, kept with that version; a double-click or Enter edits it`/);
});

test("Blobstore's references line up with the document under them, with a gap, never a second inset", () => {
  const style = read("style.css");
  assert.match(style, /#art-detail \{ padding: 24px; \}/);
  assert.match(style, /#art-detail \.prompt-evidence-links, #art-detail \.memory-reference-links \{ padding: 0; margin: 0 0 16px; \}/);
  assert.ok(style.indexOf("#art-detail .memory-reference-links") > style.indexOf(".content-panel .prompt-evidence-links, .content-panel .memory-reference-links"), "the later rule wins");
});


test("Agents names what is open in its header and groups its settings, at most four to a group", () => {
  const agents = read("agents.js"), registry = read("registry.js");
  assert.match(registry, /setHead\(`\$\{titleCase\(c\.name\)\} · \$\{c\.version\}`, draft \? "Not saved yet" : ""\);/);
  assert.match(registry, /setHead\(`New \$\{noun\}`\);/);
  assert.match(registry, /h\("div", \{ className: "panel content-panel" \}, contentHead, detail\)/);
  const groups = [...agents.matchAll(/group\("([^"]+)",/g)].map((m) => m[1]);
  assert.deepEqual(groups, ["Model", "Tools and context", "Purpose and input", "Dev pod"]);
  const block = agents.slice(agents.indexOf('group("Model",'), agents.indexOf('group("Dev pod"'));
  for (const g of block.split(/group\("/).slice(1)) assert.ok((g.match(/field\(|Field,|Field\)/g) || []).length <= 4, `at most four fields: ${g.slice(0, 20)}`);
});

test("Decisions gives the Question the room, and its middle header shows which decisions are listed", () => {
  const decisions = read("decisions.js"), style = read("style.css");
  assert.match(decisions, /const SHOW = \[\["", "All"\], \["pending", "Waiting"\], \["decided", "Decided"\]\];/);
  assert.match(decisions, /\(!state\.show \|\| c\.kind === state\.show\)/);
  assert.match(decisions, /shown\.forEach\(\(b, i\) => b\.setAttribute\("aria-checked", String\(state\.show === SHOW\[i\]\[0\]\)\)\); return; \}/, "a redraw keeps the focus");
  assert.match(decisions, /h\("td", \{ className: "opt dec-workflow", dataset: \{ full: titleCase\(c\.workflow\) \} \}/);
  assert.match(style, /\.decisions-panel \.dec-workflow \{ max-width: 160px; overflow: hidden; text-overflow: ellipsis; \}/);
});

test("a selected failed row stands out among failed rows: its red tint and the accent outline", () => {
  const style = read("style.css");
  const rule = '.data-table tbody tr.run-failed[aria-selected="true"] { background: color-mix(in srgb, var(--failed) 8%, var(--panel)); box-shadow: inset 0 0 0 2px var(--accent); }';
  assert.ok(style.includes(rule));
  assert.ok(style.indexOf(rule) > style.indexOf('.data-table tbody tr.active, .data-table tbody tr[aria-selected="true"]'), "after the selection rule, so it wins");
});

test("the Agents model says in a word beside its select whether the gateway serves it", () => {
  const agents = read("agents.js");
  assert.match(agents, /const word = !Array\.isArray\(models\) \? \["idle", "Unknown"\] : !selected \? \["failed", "Not configured"\] : selected\.available \? \["done", "Available"\] : \["failed", "Unavailable"\];/);
  assert.match(agents, /modelState\.replaceChildren\(\.\.\.\(current \? \[statusChip\(word\[0\], word\[1\]\)\] : \[\]\)\);/);
  assert.match(agents, /h\("div", \{ className: "row" \}, modelControl, refreshButton, modelState\)\)/);
});

test("a segmented control has room inside its edge: its options never touch it", () => {
  const style = read("style.css");
  assert.match(style, /\.segmented \{ display: inline-flex; padding: 3px; gap: 3px; border: 1px solid var\(--line\); border-radius: 9px;/);
  assert.match(style, /\.segmented button \{ height: 26px; padding: 0 12px; border: 0; border-radius: 6px;/);
});

test("rendered Markdown reads as prose, at most about 75 characters to a line", () => {
  assert.match(read("style.css"), /\.file-block\.markdown :is\(p, ul, ol, blockquote, h1, h2, h3, h4, h5, h6, dl\) \{ max-width: 64ch; \}/, "nested in .markdown-body too");
  assert.doesNotMatch(read("style.css"), /\.wf-description-text \{ max-width/, "the workflow description wraps at the width of its footer, not before");
});

test("each page clips itself, so nothing positioned over the content area is cut off", () => {
  const style = read("style.css");
  assert.match(style, /\n\.content \{ position: relative; min-width: 0; min-height: 0; \}/);
  assert.match(style, /\n\.page \{ position: relative; height: 100%; display: grid; min-height: 0; overflow: hidden; \}/);
  assert.match(style, /body \{\n  margin: 0; height: 100vh; overflow: hidden;/, "the window still never scrolls as a whole");
});

test("every page script parses", () => {
  // The source-pattern tests read scripts as text; this one proves each still loads in a browser.
  for (const name of fs.readdirSync(new URL("web/", import.meta.url)).filter((f) => f.endsWith(".js"))) {
    assert.doesNotThrow(() => new vm.Script(read(name), { filename: name }), name);
  }
});

test("the By column on Decisions gives way to the Question too", () => {
  assert.match(read("style.css"), /\.decisions-panel \.dec-by \{ max-width: 112px; overflow: hidden; text-overflow: ellipsis; \}/);
  assert.match(read("decisions.js"), /h\("td", \{ className: "opt dec-by", dataset: \{ full:/);
});

test("a Blobstore run folder shows its trace ID as stored, never a workflow name a person chose", () => {
  const artifacts = read("artifacts.js");
  assert.match(artifacts, /label, mono: !!traceOf\(prefix\), full: traceOf\(prefix\) \? label : undefined,/);
  assert.match(artifacts, /title: traceOf\(prefix\) \? runIndex\.label\(traceOf\(prefix\)\) : "",/, "the workflow and start time are only the tooltip");
  assert.doesNotMatch(artifacts, /label: known \? runIndex\.label\(trace\)/);
});
