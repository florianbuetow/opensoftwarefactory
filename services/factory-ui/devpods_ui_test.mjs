import assert from "node:assert/strict";
import fs from "node:fs";
import test from "node:test";
import vm from "node:vm";

// Dev pods and rejections that go back: the editor logic for the
// dev pod, command and stop steps and for a gate's back edge, and what Runs reads about a
// run's dev pod, its pull request, the Blocked state and the rounds of a step.

const read = (name) => fs.readFileSync(new URL(`web/${name}`, import.meta.url), "utf8");
const plain = (value) => JSON.parse(JSON.stringify(value)); // across the vm realm
const workflows = read("workflows.js"), runs = read("runs.js"), app = read("app.js"), agents = read("agents.js"), chat = read("chat.js"), style = read("style.css");

function editor() {
  const context = {};
  vm.runInNewContext(`${read("workflowref.js")}\n${workflows.slice(0, workflows.indexOf("// ---- Page ----"))}\nglobalThis.out = WorkflowLogic;`, context);
  return context.out;
}

function runsLogic() {
  const context = {};
  const pass = app.slice(app.indexOf("const PASS_PART ="), app.indexOf("// runIndex knows every run"));
  const logic = runs.slice(runs.indexOf("const RunsLogic ="), runs.indexOf("(() => {\n  const MAX_ROWS"));
  vm.runInNewContext(`${pass}\n${logic}\nglobalThis.out = { ...RunsLogic, stepPass, runExtras };`, context);
  return context.out;
}

// The run index of app.js, with the blobstore and the orchestrator answering as given.
function runIndex({ objects = [], status = {} }) {
  const context = {
    api: async (_method, path) => (path.startsWith("/api/blobs/trace/") ? { objects } : path.startsWith("/api/orchestrator/runs/") ? status : {}),
    loadBlob: async () => ({ text: "{}" }),
  };
  vm.runInNewContext(`${app.slice(app.indexOf("async function pool"), app.indexOf("// Pending gates"))}\nglobalThis.out = runIndex;`, context);
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

const W = 220, H = 64, SIZE = { w: W, h: H };

// ---- Editor: the three kinds ----

test("a dev pod is a hexagon, a command a slanted box and a stop step a notched box, each inside the same box", () => {
  const { tileOutline, POD_KINDS } = editor();
  assert.deepEqual(plain(POD_KINDS), ["devpod", "command", "devpod-stop"]);
  const kinds = ["agent", "gate", "interactive", "route", "workflow", "merge", "team", ...POD_KINDS];
  assert.equal(new Set(kinds.map((kind) => tileOutline(kind, W, H))).size, kinds.length - 1, "every kind has its own outline; a team shares the agent's");
  for (const kind of POD_KINDS) {
    const outline = points(tileOutline(kind, W, H));
    for (const [x, y] of outline) assert.ok(x >= 0 && x <= W && y >= 0 && y <= H, `${kind} stays inside the box`);
    // The ports sit at the middle of the top and bottom edges: both edges span the middle.
    for (const edge of [0, H]) {
      const xs = outline.filter((p) => p[1] === edge).map((p) => p[0]);
      assert.ok(Math.min(...xs) < W / 2 && Math.max(...xs) > W / 2, `${kind} has its ${edge ? "bottom" : "top"} port on the outline`);
    }
  }
  const middle = (kind) => points(tileOutline(kind, W, H)).filter((p) => p[1] === H / 2).map((p) => p[0]).sort((a, b) => a - b);
  assert.deepEqual(middle("devpod"), [0, W], "a pod's points reach the sides");
  assert.deepEqual(middle("devpod-stop"), [14, W - 14], "a stop step's points turn inwards");
  assert.match(workflows, /devpod: "Dev Pod", "devpod-stop": "Stop Dev Pod", command: "Command" \}/);
  assert.match(workflows, /devpod: DEVPOD_ICON, command: COMMAND_ICON, "devpod-stop": DEVPOD_STOP_ICON \}/);
});

test("the tiles say what the step works on: repository and work branch, the command, or that the pod is deleted", () => {
  const { podSubtitle, DEFAULT_WORK_BRANCH } = editor();
  assert.equal(DEFAULT_WORK_BRANCH, "agent/{run}");
  assert.equal(podSubtitle({ service: "devpod", repository: "claims-build", work_branch: "agent/pay-{run}" }), "claims-build · agent/pay-{run}");
  assert.equal(podSubtitle({ service: "devpod", repository: "claims-build" }), "claims-build · agent/{run}", "the default work branch");
  assert.equal(podSubtitle({ service: "devpod" }), "No repository");
  assert.equal(podSubtitle({ service: "command", command: " ./verify.sh " }), "./verify.sh");
  assert.equal(podSubtitle({ service: "command" }), "No command");
  assert.equal(podSubtitle({ service: "devpod-stop" }), "Deletes the dev pod");
  assert.match(workflows, /case "devpod": case "command": case "devpod-stop": return L\.podSubtitle\(s\);/);
});

test("the kinds are offered in the canvas menu and the Service suggestions", () => {
  assert.match(workflows, /\["Add dev pod", \(\) => addStep\("devpod"\), "", "[^"]+"\]/);
  assert.match(workflows, /\["Add command", \(\) => addStep\("command"\), "", "[^"]+"\]/);
  assert.match(workflows, /\["Add stop dev pod", \(\) => addStep\("devpod-stop"\), "", "[^"]+"\]/);
  assert.match(workflows, /devpod starts a dev pod with a clone of a repository on a work branch; command runs one command in the dev pod without a model; devpod-stop deletes the dev pod/);
});

test("a step keeps only the fields of its kind", () => {
  const { dropForeignFields } = editor();
  const all = { id: "x", depends_on: ["a"], position: { x: 1, y: 2 }, agent: "demo/a@v1", question: "Go?", timeout: "5m", max_attempts: 2, size: 3,
    repository: "r", source_branch: "main", work_branch: "agent/{run}", name: "n", protected: ["a"], rules: [{ tool: "write_file", message: "m" }], command: "ls", allow_failure: true };
  const kept = { id: "x", depends_on: ["a"], position: { x: 1, y: 2 } };
  assert.deepEqual(plain(dropForeignFields({ ...all, service: "devpod" })), { ...kept, timeout: "5m", max_attempts: 2,
    repository: "r", source_branch: "main", work_branch: "agent/{run}", name: "n", protected: ["a"], rules: [{ tool: "write_file", message: "m" }], service: "devpod" });
  assert.deepEqual(plain(dropForeignFields({ ...all, service: "command" })), { ...kept, timeout: "5m", command: "ls", allow_failure: true, service: "command" });
  assert.deepEqual(plain(dropForeignFields({ ...all, service: "devpod-stop" })), { ...kept, service: "devpod-stop" });
  assert.deepEqual(plain(dropForeignFields({ ...all, service: "agent" })), { ...kept, agent: "demo/a@v1", question: "Go?", timeout: "5m", max_attempts: 2, service: "agent" });
  assert.deepEqual(plain(dropForeignFields({ ...all, service: "merge" })), { ...kept, service: "merge" });
});

test("protected paths are read one per line, and a rule is stored with what it holds", () => {
  const { linesOf, ruleWith, RULE_TOOLS } = editor();
  assert.deepEqual(plain(linesOf(" tests/** \n\nverify.sh\n")), ["tests/**", "verify.sh"]);
  assert.deepEqual(plain(linesOf("")), []);
  assert.deepEqual(plain(RULE_TOOLS), ["write_file", "edit_file", "run_command"]);
  const rule = { tool: "write_file", path: "src/**", matches: "x", message: "Blocked" };
  assert.deepEqual(plain(ruleWith(rule, "path", "  ")), { tool: "write_file", matches: "x", message: "Blocked" });
  assert.deepEqual(plain(ruleWith(rule, "tool", "run_command")), { tool: "run_command", path: "src/**", matches: "x", message: "Blocked" });
  assert.deepEqual(plain(ruleWith({ tool: "nope" }, "message", " Blocked by CL-7 ")), { tool: "write_file", message: "Blocked by CL-7" });
});

test("what the orchestrator would refuse about the dev pod steps shows as problems", () => {
  const { stepProblems } = editor();
  const pod = { id: "pod", service: "devpod", repository: "claims-build", source_branch: "main", work_branch: "agent/{run}" };
  const w = { steps: [pod,
    { id: "build", service: "agent", depends_on: ["pod"] },
    { id: "verify", service: "command", depends_on: ["build"], command: "./verify.sh" },
    { id: "stop", service: "devpod-stop", depends_on: ["verify"] },
    { id: "loose", service: "command", command: "ls" },
    { id: "loose-stop", service: "devpod-stop" },
  ] };
  const of = (s, flow = w) => plain(stepProblems(flow, s, (id) => id.toUpperCase()));
  for (const s of w.steps.slice(0, 4)) assert.deepEqual(of(s), [], s.id);
  assert.deepEqual(of(w.steps[4]), ["A command step must come after the dev pod step"]);
  assert.deepEqual(of(w.steps[5]), ["A stop step must come after the dev pod step"]);
  assert.deepEqual(of({ id: "c", service: "command", depends_on: ["pod"], command: "  " }), ["A command step needs a command"]);
  assert.deepEqual(of({ id: "p", service: "devpod" }, { steps: [{ id: "p", service: "devpod" }] }), ["A dev pod needs a repository", "A dev pod needs a source branch", "A dev pod needs a work branch"]);
  assert.deepEqual(of({ ...pod, work_branch: "main" }), ["The work branch must differ from the source branch"]);
  // A second dev pod step, named as the page names steps.
  const second = { id: "pod-2", service: "devpod", repository: "r", source_branch: "main", work_branch: "agent/x" };
  assert.deepEqual(of(second, { steps: [...w.steps, second] }), ["A workflow has only one dev pod step, and POD is another"]);
  // A dev pod after a merge or batch would be created once per message.
  const after = { steps: [{ id: "a", service: "agent" }, { id: "collect", service: "merge", depends_on: ["a"] }, { ...pod, depends_on: ["collect"] }] };
  assert.deepEqual(of(after.steps[2], after), ["A dev pod cannot come after the merge step COLLECT"]);
  const rules = { ...pod, rules: [{ tool: "write_file", message: "ok" }, { tool: "delete", message: " " }, { tool: "run_command", matches: "(", message: "m" }] };
  assert.deepEqual(of(rules), ["Rule 2 needs a tool", "Rule 2 needs a message that names the rule, the reason and the fix", "Rule 3 has a pattern that is not a regular expression"]);
  // No new sentence ends in ": <text>", which the panel would read as a step ID.
  for (const problem of [...of(w.steps[4]), ...of(second, { steps: [...w.steps, second] }), ...of(rules)]) assert.doesNotMatch(problem, /: /);
  assert.match(workflows, /L\.stepProblems\(w, s, titleCase\)/);
});

// ---- Editor: a gate's back edge ----

const flow = () => ({ steps: [
  { id: "pod", service: "devpod" },
  { id: "build", service: "agent", depends_on: ["pod"] },
  { id: "docs", service: "interactive", depends_on: ["pod"] },
  { id: "check", service: "command", depends_on: ["build"] },
  { id: "review", service: "gate", depends_on: ["check", "docs"] },
  { id: "later", service: "agent", depends_on: ["review"] },
  { id: "aside", service: "agent" },
] });

test("a rejection goes back to an agent, interactive, team or workflow step the gate depends on, directly or through others", () => {
  const { rejectTargets, rejectRefusal, ancestors, REJECT_KINDS } = editor();
  assert.deepEqual(plain(REJECT_KINDS), ["agent", "interactive", "team", "workflow"]);
  const w = flow(), gate = w.steps[4];
  assert.deepEqual(plain(ancestors(w, "review")), ["check", "docs", "build", "pod"], "nearest first");
  assert.deepEqual(plain(rejectTargets(w, gate)), ["docs", "build"], "build through check; the command and the dev pod are no targets");
  const why = (id) => rejectRefusal(w, gate, id, (x) => x.toUpperCase());
  assert.equal(why("build"), "");
  assert.equal(why("check"), "A rejection cannot go back to CHECK, a command step");
  assert.equal(why("pod"), "A rejection cannot go back to POD, a dev pod step");
  assert.equal(why("later"), "A rejection goes back to a step REVIEW depends on, and it does not depend on LATER");
  assert.equal(why("aside"), "A rejection goes back to a step REVIEW depends on, and it does not depend on ASIDE");
  assert.equal(why("review"), "A rejection goes back to an earlier step");
  assert.equal(why("gone"), "A rejection goes back to an earlier step");
  // A workflow step and a team are targets as well.
  const nested = { steps: [{ id: "sub", service: "workflow" }, { id: "crew", service: "team", depends_on: ["sub"] }, { id: "g", service: "gate", depends_on: ["crew"] }] };
  assert.deepEqual(plain(rejectTargets(nested, nested.steps[2])), ["crew", "sub"]);
});

test("a back edge is refused across a merge or batch, and when a dev pod step would run again", () => {
  const { rejectRefusal, rejectTargets } = editor();
  const name = (id) => id;
  // A merge between the target and the gate.
  const across = { steps: [{ id: "a", service: "agent" }, { id: "b", service: "agent" }, { id: "collect", service: "merge", depends_on: ["a", "b"] }, { id: "sum", service: "agent", depends_on: ["collect"] }, { id: "g", service: "gate", depends_on: ["sum"] }] };
  assert.equal(rejectRefusal(across, across.steps[4], "a", name), "A rejection cannot go back across the merge step collect");
  assert.equal(rejectRefusal(across, across.steps[4], "sum", name), "", "both after the merge");
  assert.deepEqual(plain(rejectTargets(across, across.steps[4])), ["sum"]);
  // A batch that collects a step that runs again, beside the gate.
  const collects = { steps: [{ id: "a", service: "agent" }, { id: "g", service: "gate", depends_on: ["a"] }, { id: "gather", service: "batch", depends_on: ["a"], size: 2 }] };
  assert.equal(rejectRefusal(collects, collects.steps[1], "a", name), "A rejection cannot go back across the batch step gather");
  // After a team, any merge or batch that follows it.
  const team = { steps: [{ id: "crew", service: "team" }, { id: "g", service: "gate", depends_on: ["crew"] }, { id: "x", service: "agent", depends_on: ["g"] }, { id: "gather", service: "batch", depends_on: ["x"], size: 2 }] };
  assert.equal(rejectRefusal(team, team.steps[1], "crew", name), "A rejection cannot go back across the batch step gather");
  // The dev pod is created once: a step before it cannot run again through it.
  const pod = { steps: [{ id: "plan", service: "agent" }, { id: "pod", service: "devpod", depends_on: ["plan"] }, { id: "build", service: "agent", depends_on: ["pod"] }, { id: "g", service: "gate", depends_on: ["build"] }] };
  assert.equal(rejectRefusal(pod, pod.steps[3], "plan", name), "The dev pod step pod lies between plan and g and cannot run again");
  assert.equal(rejectRefusal(pod, pod.steps[3], "build", name), "");
});

test("a back edge's tooltip says what goes back", () => {
  const { backEdgeTip } = editor();
  const name = (id) => id[0].toUpperCase() + id.slice(1);
  assert.equal(backEdgeTip({ id: "review", on_reject: "build" }, { id: "build", service: "agent" }, name),
    "A rejection at Review goes back to Build with the reason, its previous result and the reviewed documents, and it runs again with the steps after it; a click removes it");
  assert.equal(backEdgeTip({ id: "review", on_reject: "crew" }, { id: "crew", service: "team" }, name),
    "A rejection at Review goes to the lead of Crew as a message with the reason, and the team works on; a click removes it");
});

// crossings lists the tiles a route's lines run across or touch; the first line may leave the
// gate and the last one enter the target.
function crossings(route, gate, target, others) {
  const hit = [];
  const pts = route.points;
  for (let i = 1; i < pts.length; i++) {
    const a = pts[i - 1], b = pts[i];
    assert.ok(a.x === b.x || a.y === b.y, "every line is horizontal or vertical");
    const boxes = [...others.map((box, n) => ({ ...box, name: `tile ${n}` })), ...(i > 1 ? [{ ...gate, name: "gate" }] : []), ...(i < pts.length - 1 ? [{ ...target, name: "target" }] : [])];
    for (const box of boxes) {
      if (Math.min(a.x, b.x) <= box.x + W && Math.max(a.x, b.x) >= box.x && Math.min(a.y, b.y) <= box.y + H && Math.max(a.y, b.y) >= box.y) hit.push(box.name);
    }
  }
  return hit;
}
const at = (col, row) => ({ x: col * 268, y: 40 + row * 136 }); // the editor's grid: columns 268 apart, rows 136

test("a back edge leaves the Rejected port and runs up the channel beside the column into the target's side", () => {
  const { backEdgeRoute, REJECT_PORT_Y } = editor();
  assert.equal(REJECT_PORT_Y, 20);
  const gate = at(0, 3), target = at(0, 1), others = [at(0, 0), at(0, 2), at(1, 2)];
  const route = backEdgeRoute(gate, target, others, SIZE);
  assert.equal(route.clear, true);
  assert.deepEqual(plain(route.points), [{ x: 220, y: 468 }, { x: 248, y: 468 }, { x: 248, y: 196 }, { x: 229, y: 196 }], "it ends 9 px before the tile, where the arrowhead touches it");
  assert.equal(route.d, "M220,468 L240,468 Q248,468 248,460 L248,204 Q248,196 240,196 L229,196");
  assert.deepEqual(crossings(route, gate, target, others), []);
  // A team's stack reaches 8 px past its box: the edge ends before it.
  assert.equal(backEdgeRoute(gate, target, others, SIZE, 8).points.at(-1).x, 237);
});

test("a back edge to a step in another column goes through the gap between two rows, never across a tile", () => {
  const { backEdgeRoute } = editor();
  // The target has a neighbour between it and the gate's column.
  const gate = at(1, 2), target = at(0, 0), others = [at(1, 0), at(0, 1), at(1, 1), at(2, 1)];
  const route = backEdgeRoute(gate, target, others, SIZE);
  assert.equal(route.clear, true);
  assert.deepEqual(crossings(route, gate, target, others), []);
  assert.deepEqual(plain(route.points.at(-1)), { x: 229, y: 60 }, "into the target's right side");
  assert.ok(route.points.length > 4, "it turns through a gap between rows");
  // A target right of the gate is entered from its left side.
  const left = backEdgeRoute(at(0, 2), at(2, 0), [at(1, 0), at(1, 1), at(0, 1), at(1, 2)], SIZE);
  assert.equal(left.clear, true);
  assert.deepEqual(plain(left.points.at(-1)), { x: 2 * 268 - 9, y: 60 });
  // A target beside the gate, in the same row: over the gate, not through it.
  const beside = backEdgeRoute(at(1, 1), at(0, 1), [at(0, 0), at(1, 0)], SIZE);
  assert.equal(beside.clear, true);
  assert.deepEqual(crossings(beside, at(1, 1), at(0, 1), [at(0, 0), at(1, 0)]), []);
});

test("on the editor's grid a back edge never crosses or touches a tile, wherever the gate and its target stand", () => {
  const { backEdgeRoute } = editor();
  let seed = 7;
  const random = (n) => { seed = (seed * 1103515245 + 12345) % 2147483648; return seed % n; };
  for (let round = 0; round < 400; round++) {
    const cells = [];
    for (let col = 0; col < 4; col++) for (let row = 0; row < 5; row++) if (random(3)) cells.push([col, row]);
    if (cells.length < 2) continue;
    const g = cells.splice(random(cells.length), 1)[0], t = cells.splice(random(cells.length), 1)[0];
    const gate = at(...g), target = at(...t), others = cells.map((c) => at(...c));
    const route = backEdgeRoute(gate, target, others, SIZE, round % 5 ? 0 : 8);
    assert.equal(route.clear, true, `gate ${g} target ${t} tiles ${JSON.stringify(cells)}`);
    assert.deepEqual(crossings(route, gate, target, others), [], `gate ${g} target ${t} tiles ${JSON.stringify(cells)}`);
    assert.deepEqual(plain(route.points[0]), { x: gate.x + W, y: gate.y + 20 }, "from the Rejected port");
    assert.equal(route.points.at(-1).y, target.y + 20);
  }
});

test("where tiles leave no way, the back edge still runs: around everything, and says it is not clear", () => {
  const { backEdgeRoute } = editor();
  // Tiles packed without gaps around the gate.
  const gate = { x: 0, y: 200 }, target = { x: 0, y: 0 };
  const route = backEdgeRoute(gate, target, [{ x: 225, y: 150 }, { x: 225, y: 230 }, { x: 225, y: 60 }, { x: 450, y: 180 }], SIZE);
  assert.equal(route.clear, false);
  assert.ok(route.d.startsWith("M220,220 "));
  assert.equal(route.points.at(-1).y, 20);
});

test("the editor draws the back edge dashed in the failed colour and the Rejected port on the gate", () => {
  assert.match(style, /\.back-edge \{ stroke: var\(--failed\); stroke-width: 1\.5; stroke-dasharray: 6 4; fill: none; \}/);
  assert.match(workflows, /el\("path", \{ d: route\.d, class: "back-edge", "marker-end": "url\(#arrow-reject\)" \}, g\);/);
  assert.match(workflows, /el\("circle", \{ class: "port port-reject", cx: W, cy: L\.REJECT_PORT_Y, r: 5 \}, g\);/);
  assert.match(workflows, /g\.onclick = \(\) => removeBackEdge\(s\);/);
  assert.match(workflows, /const refusal = L\.rejectRefusal\(workflow\(\), d\.from, target\.id, titleCase\);\n\s+if \(refusal\) setStatus\(refusal, "error"\);/);
  assert.match(workflows, /field\("Rejections go to", "on_reject", onReject,/);
  assert.match(workflows, /h\("option", \{ value: "" \}, "Nobody: a rejection ends the run"\)/);
  assert.match(workflows, /onReject && s\.on_reject \? field\("Max rejections", "max_rejections", digits\("max_rejections", "5"\), true,/);
});

// ---- Runs: rounds, the dev pod, the pull request, Blocked ----

test("a round is read from a name, after its pass, and is never a step", () => {
  const { stepPass, stepLabel } = runsLogic();
  assert.deepEqual(plain(stepPass("build.round-1")), { step: "build", pass: 0, round: 1 });
  assert.deepEqual(plain(stepPass("build.pass-2.round-3")), { step: "build", pass: 2, round: 3 });
  assert.deepEqual(plain(stepPass("flow/build.round-2")), { step: "flow/build", pass: 0, round: 2 });
  assert.deepEqual(plain(stepPass("flow.round-1/inner")), { step: "flow/inner", pass: 0, round: 1 });
  assert.deepEqual(plain(stepPass("build.round-0")), { step: "build.round-0", pass: 0, round: 0 }, "rounds start at 1");
  assert.deepEqual(plain(stepPass("review.2")), { step: "review.2", pass: 0, round: 0 }, "an earlier decision of a gate is not such a round");
  const title = (id) => id[0].toUpperCase() + id.slice(1);
  assert.equal(stepLabel("build.round-1", title), "Build · round 1");
  assert.equal(stepLabel("build.pass-2.round-1", title), "Build · pass 2 · round 1");
});

test("the objects of an earlier round belong to their step and round, not to steps of their own", async () => {
  const o = (name, created) => ({ id: `runs/t/${name}`, created });
  const index = runIndex({ objects: [
    o("input", "01"), o("run.json", "01"), o("metadata.json", "01"),
    o("build.round-1", "02"), o("build.round-1.run.json", "02"), o("build.round-1.attempt-1.run.json", "02"),
    o("check.round-1", "03"),
    o("review.1", "04"), // the gate's first decision, a rejection
    o("build.round-2", "05"), o("build.round-2.run.json", "05"),
    o("build", "06"), o("build.run.json", "06"), o("check", "07"), o("review", "08"),
    o("flow.round-1/inner", "02"), o("flow.round-1", "02"), o("flow/inner", "06"), o("flow", "06"),
    o("pull-request.json", "07"), o("pull-request.patch", "07"),
  ] });
  const out = await index.outputs("t", { owner: "runs", final: true });
  assert.deepEqual(Object.keys(out.artifacts).sort(), ["build", "check", "flow", "review", "review.1"], "no round and no pull request file is a step");
  assert.equal(out.artifacts.build, "runs/t/build", "the newest result keeps the plain name");
  assert.deepEqual(plain(out.order.filter((id) => /round-|pull-request/.test(id))), []);
  assert.deepEqual(plain(out.rounds), {
    build: [{ round: 1, artifact: "runs/t/build.round-1", record: "build.round-1" }, { round: 2, artifact: "runs/t/build.round-2", record: "build.round-2" }],
    check: [{ round: 1, artifact: "runs/t/check.round-1", record: "" }],
    flow: [{ round: 1, artifact: "runs/t/flow.round-1", record: "" }],
  });
  // The records of a round stay records, for the execution list; they are named by their key.
  assert.deepEqual([...out.records].sort(), ["build", "build.round-1", "build.round-2"]);
  assert.deepEqual(plain(out.attemptRecords.get("build.round-1")), ["runs/t/build.round-1.attempt-1.run.json"]);
  assert.deepEqual([...out.nested], ["flow"], "the nested run of an earlier round is not a workflow node of its own");
  assert.deepEqual(plain(out.nestedArtifacts), { "flow/inner": "runs/t/flow/inner" });
  assert.deepEqual(plain(out.passes), {});
});

test("a round of a pass is kept under its pass, and the pass keeps its newest result", async () => {
  const o = (name, created) => ({ id: `runs/t/${name}`, created });
  const index = runIndex({ objects: [o("compare.pass-1.round-1", "01"), o("compare.pass-1.round-1.run.json", "01"), o("compare.pass-1", "02"), o("compare.pass-1.run.json", "02")] });
  const out = await index.outputs("t", { owner: "runs", final: true });
  assert.deepEqual(plain(out.passes), { compare: [{ pass: 1, artifact: "runs/t/compare.pass-1", record: "compare.pass-1" }] });
  assert.deepEqual(plain(out.rounds), { "compare.pass-1": [{ round: 1, artifact: "runs/t/compare.pass-1.round-1", record: "compare.pass-1.round-1" }] });
  assert.equal(out.artifacts.compare, "runs/t/compare.pass-1");
});

test("the run status about dev pods, pull requests, rounds and Blocked is read in one place", async () => {
  const { runExtras } = runsLogic();
  const status = { status: "Blocked", error: "The test service is down", dev_pod: { id: "pod-1a2b", repository: "claims-build", source_branch: "main", work_branch: "agent/pay-1a2b3c4d" },
    pull_request: "runs/t/pull-request.json", rounds: { build: 2, check: 2, review: 1 } };
  assert.deepEqual(plain(runExtras(status)), {
    devPod: { id: "pod-1a2b", repository: "claims-build", sourceBranch: "main", workBranch: "agent/pay-1a2b3c4d" },
    pullRequest: "runs/t/pull-request.json", roundCounts: { build: 2, check: 2 }, blocked: "The test service is down",
  });
  assert.deepEqual(plain(runExtras({ status: "Failed", error: "boom" })), { devPod: null, pullRequest: "", roundCounts: {}, blocked: "" }, "a failed run is not blocked");
  assert.deepEqual(plain(runExtras({ dev_pod: {}, pull_request: 7, rounds: [] })), { devPod: null, pullRequest: "", roundCounts: {}, blocked: "" });
  assert.deepEqual(plain(runExtras(undefined)), { devPod: null, pullRequest: "", roundCounts: {}, blocked: "" });
  // The run index keeps it with the status.
  const index = runIndex({ status });
  const entry = await index.status("t", 3000, "");
  assert.equal(entry.status, "Blocked");
  assert.equal(entry.devPod.id, "pod-1a2b");
  assert.equal(entry.blocked, "The test service is down");
  const adapter = app.slice(app.indexOf("function runExtras("), app.indexOf("// stepTitle names a step"));
  assert.equal(app.split(".dev_pod").length, adapter.split(".dev_pod").length, "dev_pod is read in runExtras alone");
  assert.equal(app.split(".pull_request").length, adapter.split(".pull_request").length, "and so is pull_request");
  assert.doesNotMatch(runs, /\.dev_pod|\.pull_request\b/, "Runs reads the status through the adapter only");
});

test("Blocked is a state of its own, in the waiting colour and never failed", () => {
  const logic = {};
  vm.runInNewContext(`${app.slice(app.indexOf("const STATE_WORDS ="), app.indexOf("function statusChip("))}\n${app.slice(app.indexOf("function runState("), app.indexOf("// api calls a backend"))}\nglobalThis.out = { runState, runStateWord };`, logic);
  assert.equal(logic.out.runState("Blocked"), "blocked");
  assert.equal(logic.out.runStateWord("Blocked"), "Blocked");
  assert.equal(logic.out.runState("Failed"), "failed");
  assert.equal(logic.out.runStateWord("TimedOut"), "TimedOut");
  assert.match(style, /\.chip-blocked \{ background: rgb\(124 58 237 \/ \.12\); \}/, "the waiting tint");
  assert.match(runs, /!\["Running", "Completed", "Blocked"\]\.includes\(st\.status\)/, "no step of a blocked run is marked failed");
  assert.match(runs, /st\.status === "Blocked" \? \(st\.blocked \? h\("div", \{ className: "section" \}, h\("p", \{ className: "blocked-line" \}, st\.blocked\)\) : null\)/);
});

test("the rounds of a step are listed oldest first, the newest with the step's own result", () => {
  const { roundRows } = runsLogic();
  assert.deepEqual(plain(roundRows(undefined)), [], "a step that ran once has no rounds");
  assert.deepEqual(plain(roundRows([])), []);
  const kept = [{ round: 1, artifact: "runs/t/build.round-1", record: "build.round-1" }, { round: 2, artifact: "runs/t/build.round-2", record: "" }];
  assert.deepEqual(plain(roundRows(kept)), [
    { round: 1, artifact: "runs/t/build.round-1", current: false }, { round: 2, artifact: "runs/t/build.round-2", current: false }, { round: 3, artifact: "", current: true },
  ]);
  assert.equal(roundRows(kept, 3).at(-1).round, 3);
  assert.equal(roundRows([kept[0]], 4).at(-1).round, 4, "the status knows how often the step ran");
  assert.match(runs, /const rounds = passes\.length \? \[\] : RunsLogic\.roundRows\(st\.rounds\?\.\[id\], st\.roundCounts\?\.\[id\]\);/);
  assert.match(runs, /h\("span", \{ className: "step-name" \}, `Round \$\{r\.round\}`\), link\)/);
});

test("a pull request record is read in one place", () => {
  const { pullRequest } = runsLogic();
  const record = { repository: "claims-build", source_branch: "main", work_branch: "agent/pay-1a2b3c4d", title: "Show the payment date", body: "Adds the date.",
    commits: [{ hash: "0123456789abcdef", subject: "Show the payment date", author: "course/feature-builder@v3" }, null], patch: "runs/t/pull-request.patch", pushed: true, created: "2026-11-17T10:00:00Z", created_by_agent: "x" };
  assert.deepEqual(plain(pullRequest(JSON.stringify(record))), {
    repository: "claims-build", sourceBranch: "main", workBranch: "agent/pay-1a2b3c4d", title: "Show the payment date", body: "Adds the date.",
    commits: [{ hash: "0123456789abcdef", subject: "Show the payment date", author: "course/feature-builder@v3" }], patch: "runs/t/pull-request.patch", pushed: true, url: "",
  });
  assert.equal(pullRequest(JSON.stringify({ ...record, url: "https://github.com/acme/site/pull/7" })).url, "https://github.com/acme/site/pull/7");
  assert.equal(pullRequest(JSON.stringify({ ...record, url: "javascript:alert(1)" })).url, "", "only a web address becomes a link");
  assert.deepEqual(plain(pullRequest("{}")), { repository: "", sourceBranch: "", workBranch: "", title: "", body: "", commits: [], patch: "", pushed: false, url: "" });
  assert.equal(pullRequest("not json"), null);
  assert.equal(pullRequest("[]"), null);
  assert.match(runs, /href: `\/api\/blobs\/blobs\/\$\{record\.patch\}`, download: file,/, "the patch is downloaded through the blob API");
});

test("a dev pod, command or stop step shows its document: the command with its exit code and output", () => {
  const { stepDocument } = runsLogic();
  assert.deepEqual(plain(stepDocument("devpod", JSON.stringify({ pod: "pod-1", repository: "claims-build", source_branch: "main", work_branch: "agent/pay-1a2b3c4d", start_commit: "0123456789abcdef", reused: false }))),
    { lines: ["claims-build · agent/pay-1a2b3c4d", "From main at 01234567"], command: "", output: "" });
  assert.deepEqual(plain(stepDocument("devpod", JSON.stringify({ repository: "r", source_branch: "main", work_branch: "w", reused: true })).lines), ["r · w", "From main, continued in the live dev pod"]);
  assert.deepEqual(plain(stepDocument("command", JSON.stringify({ command: "./verify.sh", exit_code: 1, stdout: "2 tests\n", stderr: "FAILED\n" }))),
    { lines: ["Exit code 1"], command: "./verify.sh", output: "2 tests\nFAILED\n" });
  assert.deepEqual(plain(stepDocument("command", JSON.stringify({ command: "true", exit_code: 0, stdout: "", stderr: "" }))), { lines: ["Exit code 0"], command: "true", output: "" });
  assert.deepEqual(plain(stepDocument("devpod-stop", JSON.stringify({ pod: "pod-1", stopped: true })).lines), ["Dev pod deleted"]);
  assert.equal(stepDocument("agent", "{}"), null);
  assert.equal(stepDocument("command", "plain text"), null);
  assert.match(runs, /\["devpod", "command", "devpod-stop"\]\.includes\(service\) && artifactOf\(id\) \? RunsLogic\.stepDocument\(service, await stored\(artifactOf\(id\), final\)\) : null/);
});

test("Runs shows the run's dev pod with View files, and says when the pod is gone", () => {
  assert.match(runs, /podSection\(st\.devPod, live\),/);
  assert.match(runs, /gone \? h\("span", \{ className: "muted" \}, "No longer exists"\)/);
  assert.match(runs, /href: ConnectorsLogic\.podHash\(pod\.id\)/);
  assert.match(runs, /pullRequestSection\(pullText \? RunsLogic\.pullRequest\(pullText\) : null, st\.pullRequest\),/);
});

// ---- Agents and Sessions ----

test("an agent's commands are one pattern per line", () => {
  const context = {};
  vm.runInNewContext(`${agents.slice(agents.indexOf("function commandLines("), agents.indexOf("// agentFields shows"))}\nglobalThis.out = commandLines;`, context);
  const commandLines = context.out;
  assert.deepEqual(plain(commandLines("./verify.sh\n\n  git status*  \npython3 -m unittest *\n")), ["./verify.sh", "git status*", "python3 -m unittest *"]);
  assert.deepEqual(plain(commandLines(["git add *", " ", 7, "git commit *"])), ["git add *", "git commit *"]);
  assert.deepEqual(plain(commandLines(undefined)), []);
});

test("a session file can be downloaded from its menu and from the viewer, as a file and never as a page", () => {
  assert.match(chat, /\["Download", \(\) => downloadBlob\(fileId\(f\.name\), f\.name\), "", "[^"]+"\],/);
  assert.match(chat, /t\.download = iconButton\("download", "Download file", \(\) => downloadBlob\(fileId\(t\.name\), t\.name\)\);/);
  assert.match(chat, /h\("span", \{ className: "viewer-actions" \}, t\.download, t\.edit, t\.cancel, t\.save\)/);
  // The bytes are handed over without a type the browser would render, under the file's name.
  assert.match(app, /new Blob\(\[await res\.arrayBuffer\(\)\], \{ type: "application\/octet-stream" \}\)/);
  assert.match(app, /const link = h\("a", \{ href: url, download: name, hidden: true \}\);/);
  assert.match(app, /download: '<path /, "the icon exists");
});

// ---- Publish decisions and the dev pod of a session ----

test("a publish decision is read in one place and is never a step of the run", async () => {
  const decisions = read("decisions.js");
  const context = {};
  vm.runInNewContext(`${decisions.slice(decisions.indexOf("const DecisionsLogic = {"), decisions.indexOf("(() => {"))}\nglobalThis.out = DecisionsLogic;`, context);
  const { publish } = context.out;
  const entry = { step: "deliver.publish-2", question: "Open the pull request?", publish: { step: "deliver", pod: "pod-1", n: 2, kind: "pull_request", title: "Show the payment date", body: "Adds the date.", repository: "claims-build", branch: "agent/pay-1a2b3c4d" } };
  assert.deepEqual(plain(publish(entry)), { step: "deliver", kind: "pull_request", word: "Pull request", title: "Show the payment date", body: "Adds the date.", repository: "claims-build", branch: "agent/pay-1a2b3c4d" });
  assert.deepEqual(plain(publish({ step: "deliver.publish-1", publish: { kind: "push", repository: "r", branch: "b" } })), { step: "deliver", kind: "push", word: "Push", title: "", body: "", repository: "r", branch: "b" });
  assert.equal(publish({ step: "review" }), null, "an ordinary gate");
  assert.equal(publish(undefined), null);
  assert.match(decisions, /const publish = DecisionsLogic\.publish\(c\.kind === "pending" \? c\.d : c\.rec\);/);
  assert.match(decisions, /h\("p", \{ className: "decision-where" \}, \[publish\.repository, publish\.branch\]\.filter\(Boolean\)\.join\(" · "\)\),/);
  // Its record, runs/<run>/<step>.publish-<n>, belongs to the step that asked.
  const helpers = {};
  vm.runInNewContext(`${app.slice(app.indexOf("const PUBLISH_PART ="), app.indexOf("// runExtras is the one place"))}\nglobalThis.out = publishStep;`, helpers);
  assert.equal(helpers.out("deliver.publish-2"), "deliver");
  assert.equal(helpers.out("deliver"), "");
  assert.equal(helpers.out("deliver.publish-0"), "");
  const o = (name) => ({ id: `runs/t/${name}`, created: "01" });
  const out = await runIndex({ objects: [o("deliver"), o("deliver.publish-1"), o("deliver.publish-2")] }).outputs("t", { owner: "runs", final: true });
  assert.deepEqual(Object.keys(out.artifacts), ["deliver"]);
  assert.deepEqual(plain(out.order), ["deliver"]);
  // While it waits, the step that asked shows Waiting and links to the decision.
  assert.match(runs, /publishStep\(d\.step\) === id \|\| \(d\.artifact \?/);
  assert.match(runs, /if \(st\.status === "Running" && waitingPublish\(id\)\) return "waiting";/);
});

test("a chat message identifies its waiting workflow and leaves runtime settings to the server", () => {
  assert.match(chat, /workflow_id: o\.workflowId,/);
  assert.doesNotMatch(chat, /dev_pod|skill_script_timeout_seconds|skills:/);
  assert.doesNotMatch(read("sessions.js"), /devPod:|agentSettings:/);
});
