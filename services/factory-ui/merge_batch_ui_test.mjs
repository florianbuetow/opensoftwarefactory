import assert from "node:assert/strict";
import fs from "node:fs";
import test from "node:test";
import vm from "node:vm";

// Merge and batch nodes: the editor logic, the one input shape
// (an ordered list of items), and what Runs shows for a node and for the steps after it.

const read = (name) => fs.readFileSync(new URL(`web/${name}`, import.meta.url), "utf8");
const plain = (value) => JSON.parse(JSON.stringify(value)); // across the vm realm
const workflows = read("workflows.js"), runs = read("runs.js"), app = read("app.js"), decisions = read("decisions.js"), style = read("style.css");

function editor() {
  const context = {};
  vm.runInNewContext(`${read("workflowref.js")}\n${workflows.slice(0, workflows.indexOf("// ---- Page ----"))}\nglobalThis.out = WorkflowLogic;`, context);
  return context.out;
}

function runsLogic() {
  const context = {};
  const pass = app.slice(app.indexOf("const PASS_PART ="), app.indexOf("// runIndex knows every run"));
  const logic = runs.slice(runs.indexOf("const RunsLogic ="), runs.indexOf("(() => {\n  const MAX_ROWS"));
  vm.runInNewContext(`${pass}\n${logic}\nglobalThis.out = { ...RunsLogic, stepPass };`, context);
  return context.out;
}

function decisionsLogic() {
  const context = {};
  vm.runInNewContext(`${decisions.slice(decisions.indexOf("const DecisionsLogic = {"), decisions.indexOf("(() => {"))}\nglobalThis.out = DecisionsLogic;`, context);
  return context.out;
}

// points reads the corner points of an outline made of straight lines only.
function points(path) {
  const out = [];
  let x = 0, y = 0;
  for (const [, cmd, args] of path.matchAll(/([MLHV])([^MLHVz]*)/g)) {
    const n = args.split(/[ ,]+/).filter(Boolean).map(Number);
    if (cmd === "H") x = n[0]; else if (cmd === "V") y = n[0]; else [x, y] = n;
    out.push([x, y]);
  }
  return out;
}

// ---- Editor ----

test("a merge is a funnel and a batch a tray, each its own outline inside the same box", () => {
  const { tileOutline } = editor();
  const W = 220, H = 64;
  const merge = points(tileOutline("merge", W, H));
  const xsAt = (y) => merge.filter((p) => p[1] === y).map((p) => p[0]);
  assert.deepEqual([Math.min(...xsAt(0)), Math.max(...xsAt(0))], [0, W], "the funnel is as wide as the box at its top");
  assert.ok(Math.min(...xsAt(H)) > 0 && Math.max(...xsAt(H)) < W, "and narrower at its bottom");
  assert.equal(Math.min(...xsAt(H)), W - Math.max(...xsAt(H)), "it narrows by the same amount on both sides");
  // The ports sit at the middle of the top and bottom edges: both are on the outline.
  assert.ok(Math.min(...xsAt(H)) < W / 2 && Math.max(...xsAt(H)) > W / 2);
  for (const [x, y] of merge) assert.ok(x >= 0 && x <= W && y >= 0 && y <= H, "inside the box");
  const outlines = ["agent", "gate", "interactive", "route", "workflow", "merge"].map((kind) => tileOutline(kind, W, H));
  assert.equal(new Set(outlines).size, outlines.length, "every kind has its own outline");
  // The tray is a plain box; its second line under the top edge is drawn by the page.
  assert.equal(tileOutline("batch", W, H), tileOutline("workflow", W, H));
  assert.match(workflows, /if \(s\.service === "batch"\) el\("line", \{ class: "tray-line", x1: 1, y1: TRAY_LINE_Y, x2: W - 1, y2: TRAY_LINE_Y \}, g\);/);
  assert.match(workflows, /const outline = L\.tileOutline\(kind\(s\), W, H\);/);
});

test("the tiles read Merge and Batch, with what they do as their second line", () => {
  const { nodeSubtitle, isNode, NODE_KINDS } = editor();
  assert.deepEqual(plain(NODE_KINDS), ["merge", "batch"]);
  assert.equal(nodeSubtitle({ service: "merge" }), "Waits for all");
  assert.equal(nodeSubtitle({ service: "batch", size: 3 }), "Collects 3");
  assert.equal(nodeSubtitle({ service: "batch" }), "Collects", "a batch without a size names none");
  assert.equal(isNode({ service: "merge" }) && isNode({ service: "batch" }), true);
  assert.equal(isNode({ service: "agent" }) || isNode(undefined), false);
  assert.match(workflows, /merge: "Merge", batch: "Batch", /);
  assert.match(workflows, /const KIND_ICON = \{ team: TEAM_ICON, merge: MERGE_ICON, batch: BATCH_ICON, /);
  assert.match(workflows, /case "merge": case "batch": return L\.nodeSubtitle\(s\);/);
});

test("the kinds are offered where a step's kind is chosen: the Service suggestions and the canvas menu", () => {
  assert.match(workflows, /for \(const value of \["workflow", "team", \.\.\.L\.NODE_KINDS, \.\.\.L\.POD_KINDS\]\)/);
  assert.match(workflows, /\["Add merge", \(\) => addStep\("merge"\), "", "[^"]+"\]/);
  assert.match(workflows, /\["Add batch", \(\) => addStep\("batch"\), "", "[^"]+"\]/);
  assert.match(workflows, /merge waits for a result from every source and forwards them together; batch collects results/);
});

test("a step that becomes a merge or batch keeps only its parameters, and loses them when it becomes something else", () => {
  const { dropForeignFields } = editor();
  const agent = { id: "collect", service: "merge", depends_on: ["a", "b"], position: { x: 1, y: 2 }, agent: "demo/a@v1", timeout: "2m", max_attempts: 3, question: "Go?", size: 4, capacity: 8, input_timeout: 60 };
  assert.deepEqual(plain(dropForeignFields(agent)), { id: "collect", service: "merge", depends_on: ["a", "b"], position: { x: 1, y: 2 }, capacity: 8, input_timeout: 60 });
  const batch = { id: "gather", service: "batch", depends_on: ["a"], capacity: 8, size: 2, input_timeout: 30, agents: ["x@v1"], channels: [] };
  assert.deepEqual(plain(dropForeignFields(batch)), { id: "gather", service: "batch", depends_on: ["a"], size: 2, input_timeout: 30 });
  const back = { id: "gather", service: "agent", depends_on: ["a"], agent: "demo/a@v1", timeout: "2m", size: 2, capacity: 16, input_timeout: 30 };
  assert.deepEqual(plain(dropForeignFields(back)), { id: "gather", service: "agent", depends_on: ["a"], agent: "demo/a@v1", timeout: "2m" });
  assert.match(workflows, /L\.dropForeignFields\(s\);/, "the Service field applies it");
});

test("a typed parameter is stored as a number, left out when empty or not a number", () => {
  const { paramValue } = editor();
  assert.equal(paramValue("16"), 16);
  assert.equal(paramValue(" 300 "), 300);
  assert.equal(paramValue("0"), 0, "a wrong number is kept, so its problem shows");
  assert.equal(paramValue("-2"), -2);
  assert.equal(paramValue(""), undefined);
  assert.equal(paramValue("many"), undefined);
});

test("what the orchestrator would refuse shows as problems: no source, no size, bad numbers, a router after a node", () => {
  const { stepProblems } = editor();
  const w = { steps: [
    { id: "a", service: "agent" },
    { id: "collect", service: "merge", depends_on: ["a"] },
    { id: "gather", service: "batch", depends_on: ["a"], size: 2 },
    { id: "next", service: "route", depends_on: ["collect"] },
    { id: "plain", service: "route", depends_on: ["a"] },
  ] };
  const of = (s) => plain(stepProblems(w, s));
  assert.deepEqual(of(w.steps[1]), []);
  assert.deepEqual(of(w.steps[2]), []);
  assert.deepEqual(of({ id: "m", service: "merge" }), ["A merge needs at least one source"]);
  assert.deepEqual(of({ id: "b", service: "batch", depends_on: [] }), ["A batch needs at least one source", "A batch needs a size"]);
  assert.deepEqual(of({ id: "b", service: "batch", depends_on: ["a"], size: 0 }), ["Size must be a whole number of 1 or more"]);
  assert.deepEqual(of({ id: "b", service: "batch", depends_on: ["a"], size: 2, input_timeout: -5 }), ["Input timeout must be a whole number of 1 or more"]);
  assert.deepEqual(of({ id: "m", service: "merge", depends_on: ["a"], capacity: 1.5, input_timeout: 0 }),
    ["Capacity must be a whole number of 1 or more", "Input timeout must be a whole number of 1 or more"]);
  assert.deepEqual(of(w.steps[3]), ["A router cannot follow a merge: collect"]);
  assert.deepEqual(of({ id: "next", service: "route", depends_on: ["gather"] }), ["A router cannot follow a batch: gather"]);
  assert.deepEqual(of(w.steps[4]), []);
  assert.deepEqual(of(w.steps[0]), []);
  // An arrow from a node onto a route step is refused at once, with the reason.
  assert.match(workflows, /target\.service === "route" && L\.isNode\(d\.from\)\) setStatus\(`A route step cannot follow a \$\{d\.from\.service\}`, "error"\)/);
});

test("the styles of the nodes sit in their own marked block", () => {
  const at = style.indexOf("/* workflow editor: merge and batch nodes */");
  assert.ok(at > 0);
  for (const selector of [".tile .tray-line", ".node-sources", '[data-field="capacity"]', '[data-field="size"]', '[data-field="input_timeout"]']) {
    assert.ok(style.slice(at).includes(selector), selector);
  }
});

// ---- One input shape ----

test("a gate shows one document per input item, in order, and names the round when a source repeats", () => {
  const { inputDocs } = decisionsLogic();
  const inputs = [
    { source: "input", artifact: "runs/t/input", round: 1 },
    { source: "short", artifact: "runs/t/short", round: 1 },
    { source: "a", artifact: "runs/t/a.pass-1", round: 1 },
    { source: "a", artifact: "runs/t/a.pass-2", round: 2 },
  ];
  assert.deepEqual(plain(inputDocs(inputs)), [
    { source: "short", artifact: "runs/t/short", round: 1, repeated: false },
    { source: "a", artifact: "runs/t/a.pass-1", round: 1, repeated: true },
    { source: "a", artifact: "runs/t/a.pass-2", round: 2, repeated: true },
  ]);
  assert.deepEqual(plain(inputDocs(undefined)), []);
  assert.deepEqual(plain(inputDocs({ short: "runs/t/short" })), [], "the map form no longer exists");
  assert.deepEqual(plain(inputDocs([null, { source: "a" }, { artifact: "x" }])), [], "an item needs a source and an artifact");
  assert.match(decisions, /for \(const \{ source, artifact, round, repeated \} of DecisionsLogic\.inputDocs\(c\.inputs\)\)/);
  assert.match(decisions, /repeated \? `\$\{titleCase\(source\)\} · round \$\{round\}` : titleCase\(source\)/);
});

test("no page reads step inputs as a map", () => {
  for (const name of ["decisions.js", "runs.js", "sessions.js", "teams.js", "chat.js", "app.js", "artifacts.js"]) {
    assert.doesNotMatch(read(name), /Object\.(entries|keys|values)\([^)]*inputs/, name);
  }
  assert.match(read("chat.js"), /const o = \{ agents: \[\], inputs: \[\], \.\.\.options \};/);
});

// ---- Runs ----

test("the pass of a step is read from its name, also through workflow nodes", () => {
  const { stepPass } = runsLogic();
  assert.deepEqual(plain(stepPass("compare.pass-2")), { step: "compare", pass: 2, round: 0 });
  assert.deepEqual(plain(stepPass("compare")), { step: "compare", pass: 0, round: 0 });
  assert.deepEqual(plain(stepPass("build/compare.pass-12")), { step: "build/compare", pass: 12, round: 0 });
  assert.deepEqual(plain(stepPass("build.pass-3/write")), { step: "build/write", pass: 3, round: 0 });
  assert.deepEqual(plain(stepPass("review.1")), { step: "review.1", pass: 0, round: 0 }, "a gate round is not a pass");
  assert.deepEqual(plain(stepPass("compare.pass-0")), { step: "compare.pass-0", pass: 0, round: 0 }, "passes start at 1");
});

// The shapes below are what the orchestrator answers: "nodes" keyed by step ID.
const item = (source, round = 1) => ({ source, artifact: `runs/t/${source}`, round });

test("the run status of a merge is read in one place: waiting entries with filled and open sources, and how many were forwarded", () => {
  const { nodeView, nodeLines, nodeState } = runsLogic();
  const step = { id: "collect", service: "merge", depends_on: ["short", "long-b"] };
  const status = { nodes: {
    gather: { kind: "batch", sources: ["a"], collected: [], size: 2, released: 0, input_timeout: 60, releases: [] },
    collect: { kind: "merge", sources: ["short", "long-b"], waiting: [{ filled: ["short"] }, { filled: ["long-b", "short"] }], forwarded: 1, expires: "2026-11-15T10:04:12Z",
      capacity: 16, input_timeout: 1200, releases: [{ pass: 1, items: [item("short"), item("long-b")], at: "2026-11-15T09:00:00Z" }] },
  } };
  const view = nodeView(status, "collect", step);
  assert.deepEqual(plain(view), {
    kind: "merge", sources: ["short", "long-b"], forwarded: 1, expires: "2026-11-15T10:04:12Z",
    waiting: [{ filled: ["short"], open: ["long-b"] }, { filled: ["short", "long-b"], open: [] }],
  });
  const now = Date.parse("2026-11-15T10:00:00Z");
  const title = (id) => id.toUpperCase();
  assert.deepEqual(plain(nodeLines(view, now, title)), [
    "Entry 2: has SHORT, waits for LONG-B, fails in 4:12",
    "Entry 3: has SHORT, LONG-B, forwarded next",
    "1 entry forwarded",
  ]);
  assert.equal(nodeState(view, "Running"), "running");
  // Without a timer there is no time; the sources come from the definition when the status has none.
  const bare = nodeView({ nodes: { collect: { kind: "merge", waiting: [{ filled: ["long-b"] }], forwarded: 0 } } }, "collect", step);
  assert.deepEqual(plain(bare.sources), ["short", "long-b"]);
  assert.deepEqual(plain(bare.waiting), [{ filled: ["long-b"], open: ["short"] }]);
  assert.equal(bare.expires, "");
  assert.deepEqual(plain(nodeLines(bare, now)), ["Entry 1: has long-b, waits for short", "0 entries forwarded"]);
  // Nothing waits any more: done once something was forwarded, idle before the first result.
  const idle = nodeView({ nodes: { collect: { kind: "merge", sources: ["short"], waiting: [], forwarded: 0 } } }, "collect", step);
  assert.equal(nodeState(idle, "Running"), "idle");
  assert.equal(nodeState({ ...idle, forwarded: 2 }, "Running"), "done");
  assert.equal(nodeState(null, "Completed"), "done");
  assert.equal(nodeState(null, "Running"), "idle");
  assert.equal(nodeView({}, "collect", step), null, "a status without nodes");
  assert.equal(nodeView({ nodes: { other: { kind: "merge" } } }, "collect", step), null, "a status that does not name the step");
  assert.deepEqual(plain(nodeLines(null)), []);
});

test("the run status of a batch reads as '3 of 5 collected, releases in 4:12', the batches released, and those released by timeout", () => {
  const { nodeView, nodeLines, nodeState, countdown, forwardedBy } = runsLogic();
  const step = { id: "gather", service: "batch", depends_on: ["a", "b", "c"], size: 5 };
  const status = { nodes: { gather: { kind: "batch", sources: ["a", "b", "c"], collected: [item("a"), item("b"), item("a", 2)], size: 5, released: 2, expires: "2026-11-15T10:04:12Z", input_timeout: 60,
    releases: [{ pass: 1, items: [item("a"), item("b")], at: "2026-11-15T09:00:00Z" }, { pass: 2, items: [item("c")], at: "2026-11-15T09:30:00Z", timed: true }] } } };
  const view = nodeView(status, "gather", step);
  const now = Date.parse("2026-11-15T10:00:00Z");
  assert.equal(view.collected.length, 3);
  assert.deepEqual(plain(view.collected[2]), { source: "a", artifact: "runs/t/a", round: 2 });
  assert.equal(view.timed, 1);
  assert.deepEqual(plain(nodeLines(view, now)), ["3 of 5 collected, releases in 4:12", "2 batches released, 1 by timeout"]);
  assert.equal(nodeState(view, "Running"), "running");
  assert.equal(forwardedBy(view), 2);
  // A full batch that only waits for the steps after it has no timer, and neither has an empty one.
  const full = nodeView({ nodes: { gather: { kind: "batch", collected: [item("a"), item("b")], size: 2, released: 1, releases: [{ pass: 1, items: [item("c")] }] } } }, "gather", step);
  assert.deepEqual(plain(nodeLines(full, now)), ["2 of 2 collected", "1 batch released"]);
  const empty = nodeView({ nodes: { gather: { kind: "batch", collected: [], released: 1, releases: [{ pass: 1, items: [item("a")] }], expires: "0001-01-01T00:00:00Z" } } }, "gather", step);
  assert.equal(empty.expires, "", "Go's zero time is no timer");
  assert.deepEqual(plain(nodeLines(empty, now)), ["0 of 5 collected", "1 batch released"], "the size comes from the definition when the status has none");
  assert.equal(nodeState(empty, "Running"), "done");
  assert.equal(countdown("2026-11-15T10:00:09Z", now), "0:09");
  assert.equal(countdown("2026-11-15T09:59:00Z", now), "0:00", "a timer that ran out");
  assert.equal(countdown("2026-11-15T11:01:05Z", now), "61:05");
});

test("a step is named with its pass where a person reads it: Sessions, Decisions, Complete", () => {
  const context = { titleCase: (id) => String(id).split(/[-_\s]+/).filter(Boolean).map((p) => p[0].toUpperCase() + p.slice(1)).join(" ") };
  vm.runInNewContext(`${app.slice(app.indexOf("const PASS_PART ="), app.indexOf("// runIndex knows every run"))}\nglobalThis.out = { stepTitle };`, context);
  const { stepTitle } = context.out;
  assert.equal(stepTitle("talk.pass-2"), "Talk · pass 2");
  assert.equal(stepTitle("talk", 3), "Talk · pass 3");
  assert.equal(stepTitle("build/code-review"), "Code Review");
  assert.equal(stepTitle("talk"), "Talk");
  assert.match(read("sessions.js"), /stepTitle\(e\.step, e\.pass \|\| stepPass\(e\.session \|\| ""\)\.pass\)/);
  assert.match(decisions, /stepTitle\(c\.step, pass\)/);
  assert.match(decisions, /const roundKey = \(d\) => `\$\{pendingId\(d\)\}#\$\{d\.pass \|\| 0\}\.\$\{d\.round \|\| 1\}`;/, "the same gate waits again in every pass");
  // The run status keeps what the nodes hold; its artifacts name a step, never a pass.
  assert.match(app, /artifacts: st\.artifacts \|\| \{\}, nodes: st\.nodes && typeof st\.nodes === "object" \? st\.nodes : \{\},/);
});

test("the gate, session and team of a pass are found under <step>.pass-<n>", () => {
  const { teamPaths, stepPass } = runsLogic();
  const base = "teams/t/build";
  assert.deepEqual(plain(teamPaths(base, undefined, [])), [{ team: base, pass: 0 }], "a team step that runs once");
  assert.deepEqual(plain(teamPaths(base, [{ pass: 1 }, { pass: 2 }], [{ team: "teams/t/build.pass-3", pass: 3 }, { team: "teams/x/build.pass-9" }, { team: "teams/t/builder.pass-4" }])),
    [{ team: "teams/t/build.pass-1", pass: 1 }, { team: "teams/t/build.pass-2", pass: 2 }, { team: "teams/t/build.pass-3", pass: 3 }]);
  // A rejection round of a gate in a pass is <gate>.pass-<n>.<round>: the round first, then the pass.
  const teams = {};
  vm.runInNewContext(`${read("workflowref.js")}\n${read("teams.js").slice(0, read("teams.js").indexOf("// ---- Running teams"))}\nglobalThis.out = TeamsLogic;`, teams);
  const round = teams.out.roundOf("runs/t/review.pass-1.2");
  assert.deepEqual(plain(round), { artifact: "runs/t/review.pass-1", round: 2 });
  assert.deepEqual(plain(stepPass(round.artifact)), { step: "runs/t/review", pass: 1, round: 0 });
  assert.equal(teams.out.validTeam("teams/t/build.pass-2"), true);
  assert.match(runs, /const gate = decisionsState\.pending\.find\(\(d\) => d\.trace_id === trace && d\.artifact === `runs\/\$\{trace\}\/\$\{key\}`\);/);
  assert.match(runs, /const session = `user-sessions\/\$\{trace\}\/\$\{key\}`, team = `teams\/\$\{trace\}\/\$\{key\}`;/);
  assert.match(runs, /href: TeamsLogic\.teamHash\(p\.team\)/);
  assert.match(runs, /href: ChatLogic\.sessionHash\(p\.session\)/);
});

test("a step after a merge or batch runs in passes: the node above it counts them", () => {
  const { passSource, passRows, forwardedBy } = runsLogic();
  const def = { steps: [
    { id: "intake", service: "agent" },
    { id: "short", service: "agent", depends_on: ["intake"] },
    { id: "collect", service: "merge", depends_on: ["short", "intake"] },
    { id: "compare", service: "agent", depends_on: ["collect"] },
    { id: "polish", service: "agent", depends_on: ["compare"] },
    { id: "gather", service: "batch", depends_on: ["polish"], size: 2 },
    { id: "summarise", service: "agent", depends_on: ["gather"] },
  ] };
  assert.equal(passSource(def, "intake"), "");
  assert.equal(passSource(def, "short"), "");
  assert.equal(passSource(def, "compare"), "collect");
  assert.equal(passSource(def, "polish"), "collect", "also further down");
  assert.equal(passSource(def, "summarise"), "gather", "the nearest node above");
  assert.equal(passSource(def, "collect"), "", "a node itself runs in no pass of its own");
  assert.equal(passSource(def, "missing"), "");
  assert.equal(forwardedBy({ kind: "merge", forwarded: 3 }), 3);
  assert.equal(forwardedBy(null), 0);

  const stored = [{ pass: 1, artifact: "runs/t/compare.pass-1", record: "compare.pass-1" }, { pass: 2, artifact: "runs/t/compare.pass-2", record: "compare.pass-2" }];
  assert.deepEqual(plain(passRows(stored, 2, "Running")), [
    { pass: 1, artifact: "runs/t/compare.pass-1", state: "done" }, { pass: 2, artifact: "runs/t/compare.pass-2", state: "done" },
  ]);
  // The node forwarded a third message: the third pass runs, or waits for the steps before it.
  assert.deepEqual(plain(passRows(stored, 3, "Running").at(-1)), { pass: 3, artifact: "", state: "running" });
  assert.deepEqual(plain(passRows(stored, 3, "Running", false).at(-1)), { pass: 3, artifact: "", state: "idle" });
  assert.equal(passRows(stored, 3, "Completed").length, 2, "a finished run starts no pass");
  // A pass with a record and no output yet runs; in a failed run it is the one that failed.
  const open = [{ pass: 1, artifact: "", record: "compare.pass-1" }];
  assert.equal(passRows(open, 1, "Running")[0].state, "running");
  assert.equal(passRows(open, 1, "Failed")[0].state, "failed");
  assert.equal(passRows(open, 1, "Failed", true, false)[0].state, "idle", "the run failed at another step");
  assert.deepEqual(plain(passRows([], 1, "Running")), [{ pass: 1, artifact: "", state: "running" }]);
  assert.deepEqual(plain(passRows(undefined, 0, "Running")), []);
});

test("a pass is named with its step in the timeline, and its record is found through the step", () => {
  const { stepLabel, recordKey } = runsLogic();
  const title = (id) => id[0].toUpperCase() + id.slice(1);
  assert.equal(stepLabel("compare.pass-2", title), "Compare · pass 2");
  assert.equal(stepLabel("build/compare.pass-2", title), "Build / Compare · pass 2");
  assert.equal(stepLabel("build/write", title), "Build / Write");
  const st = { records: new Set(["intake", "compare.pass-1", "compare.pass-2"]), passes: { compare: [{ pass: 1, artifact: "a", record: "compare.pass-1" }, { pass: 2, artifact: "", record: "compare.pass-2" }] } };
  assert.equal(recordKey(st, "intake"), "intake");
  assert.equal(recordKey(st, "compare"), "compare.pass-2", "the newest pass");
  assert.equal(recordKey(st, "other"), "");
  assert.match(runs, /const displayStep = \(step\) => RunsLogic\.stepLabel\(step \|\| "", titleCase\);/);
});

test("a failed merge reads as a sentence in the page's words", () => {
  const context = { titleCase: (id) => String(id).split(/[-_\s]+/).filter(Boolean).map((p) => p[0].toUpperCase() + p.slice(1)).join(" ") };
  vm.runInNewContext(`${runs.slice(runs.indexOf("  function rootError("), runs.indexOf("  // existingAgents lists"))}\nglobalThis.out = rootError;`, context);
  const rootError = context.out;
  assert.equal(rootError('workflow failed (type: wf): merge "collect" timed out: no result from "long-b"', "t"), "Collect timed out: no result from Long B");
  assert.equal(rootError('merge "collect" is full: capacity 16', "t"), "Collect is full: capacity 16");
  assert.equal(rootError('step "plan": model failed', "t"), "Plan: model failed", "a step failure reads as before");
  // The run names the merge as the place it failed.
  assert.match(runs, /\/\(\?:step\|merge\|batch\) "\(\[\^"\]\+\)"\/\.exec\(st\.error \|\| ""\)/);
});

test("Runs draws a node's lines under its name and one entry per pass under a step", () => {
  assert.match(runs, /const notes = RunsLogic\.nodeLines\(nodeOf\(id\), now, titleCase\);/);
  assert.match(runs, /h\("span", \{ className: "step-notes" \}/);
  assert.match(runs, /h\("span", \{ className: "step-name" \}, `Pass \$\{p\.pass\}`\), link\)/);
  assert.match(runs, /if \(passes\.length\) return null; \/\/ every pass links to its own decision, session, team or output below/);
  // The run status is read through the adapter only.
  assert.equal(runs.split(".nodes").length, 2, "status.nodes is read in nodeView alone");
  assert.match(style, /\.step-list li > \.step-notes \{ grid-column: 2 \/ -1;/);
});
