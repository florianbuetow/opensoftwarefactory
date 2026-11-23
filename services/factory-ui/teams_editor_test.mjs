import assert from "node:assert/strict";
import fs from "node:fs";
import test from "node:test";
import vm from "node:vm";

// The editor draws its graph through WorkflowGraph (graph.js): the drawing is read in both.
const source = fs.readFileSync(new URL("web/workflows.js", import.meta.url), "utf8") + fs.readFileSync(new URL("web/graph.js", import.meta.url), "utf8");
const refSource = fs.readFileSync(new URL("web/workflowref.js", import.meta.url), "utf8"); // loaded before workflows.js
const style = fs.readFileSync(new URL("web/style.css", import.meta.url), "utf8");

// ---- Logic without the DOM: everything above the page code ----

function logic() {
  const end = source.indexOf("// ---- Page ----");
  assert.ok(end > 0, "workflows.js keeps its logic above the page code");
  const context = {};
  vm.runInNewContext(`${refSource}\n${source.slice(0, end)}\nglobalThis.out = WorkflowLogic; globalThis.ref = WorkflowRef;`, context);
  return { ...context.out, WorkflowRef: context.ref };
}
const plain = (value) => JSON.parse(JSON.stringify(value)); // across the vm realm

const team = () => ({
  id: "team", service: "team", agent: "demo/lead@v1",
  agents: ["demo/coder@v1", "demo/tester@v1", "demo/reviewer@v1"], workflows: ["docs-team@v2"],
});

test("a member is named by the last path segment of its agent, without the version", () => {
  const { memberName, teamMembers } = logic();
  assert.equal(memberName("demo/lead@v1"), "lead");
  assert.equal(memberName("lead@v12"), "lead");
  assert.equal(memberName("a/b/c/reviewer@v3"), "reviewer");
  assert.equal(memberName("plain"), "plain");
  assert.equal(memberName(""), "");
  assert.equal(memberName(undefined), "");
  assert.deepEqual(plain(teamMembers(team())), [
    { name: "lead", kind: "agent", ref: "demo/lead@v1", lead: true },
    { name: "coder", kind: "agent", ref: "demo/coder@v1", lead: false },
    { name: "tester", kind: "agent", ref: "demo/tester@v1", lead: false },
    { name: "reviewer", kind: "agent", ref: "demo/reviewer@v1", lead: false },
    { name: "docs-team", kind: "workflow", ref: "docs-team@v2", lead: false },
  ]);
  // A workflow member is named by its workflow, whatever version it is pinned to.
  assert.deepEqual(plain(teamMembers({ service: "team", agent: "demo/lead@v1", workflows: ["docs-team", "review@v12"] }).map((m) => [m.name, m.ref])), [["lead", "demo/lead@v1"], ["docs-team", "docs-team"], ["review", "review@v12"]]);
  assert.deepEqual(plain(teamMembers({ service: "team" })), []);
});

test("a channel ID is the two names in alphabetical order joined by --", () => {
  const { channelId } = logic();
  assert.equal(channelId("lead", "coder"), "coder--lead");
  assert.equal(channelId("coder", "lead"), "coder--lead");
  assert.equal(channelId("coder", "reviewer"), "coder--reviewer");
  assert.equal(channelId("lead", "workflow"), "lead--workflow");
});

test("without stored channels the lead has a channel to every member", () => {
  const { defaultChannels, teamChannels } = logic();
  assert.deepEqual(plain(defaultChannels(team())), [["lead", "coder"], ["lead", "tester"], ["lead", "reviewer"], ["lead", "docs-team"]]);
  assert.deepEqual(plain(defaultChannels({ service: "team", agents: ["demo/coder@v1"] })), [], "no lead, no default");
  assert.deepEqual(plain(teamChannels(team())), { pairs: plain(defaultChannels(team())), isDefault: true });
  assert.equal(teamChannels({ ...team(), channels: [] }).isDefault, true);
  const stored = teamChannels({ ...team(), channels: [["lead", "coder"], ["coder", "lead"], ["coder", "reviewer"]] });
  assert.deepEqual(plain(stored), { pairs: [["lead", "coder"], ["coder", "reviewer"]], isDefault: false }, "each channel once");
});

test("the first drawn channel keeps the default ones; removing the last returns to the default", () => {
  const { toggleChannel } = logic();
  const added = toggleChannel(team(), "coder", "reviewer");
  assert.deepEqual(plain(added), [["lead", "coder"], ["lead", "tester"], ["lead", "reviewer"], ["lead", "docs-team"], ["coder", "reviewer"]]);
  const removed = toggleChannel({ ...team(), channels: added }, "reviewer", "coder");
  assert.deepEqual(plain(removed), [["lead", "coder"], ["lead", "tester"], ["lead", "reviewer"], ["lead", "docs-team"]]);
  assert.deepEqual(plain(toggleChannel(team(), "tester", "lead")), [["lead", "coder"], ["lead", "reviewer"], ["lead", "docs-team"]], "a default channel is removed");
  assert.equal(toggleChannel({ ...team(), channels: [["lead", "coder"]] }, "coder", "lead"), undefined);
  const same = [["lead", "coder"]];
  assert.equal(toggleChannel({ ...team(), channels: same }, "coder", "coder"), same, "no channel with oneself");
});

test("channels of a member that left are dropped", () => {
  const { pruneChannels, removeMember } = logic();
  const s = { ...team(), channels: [["lead", "coder"], ["coder", "reviewer"], ["lead", "tester"]] };
  assert.deepEqual(plain(removeMember(s, "coder")), { agents: ["demo/tester@v1", "demo/reviewer@v1"], workflows: ["docs-team@v2"] });
  assert.deepEqual(plain(removeMember(s, "docs-team")), { agents: s.agents, workflows: [] });
  assert.deepEqual(plain(pruneChannels({ ...s, ...removeMember(s, "coder") })), [["lead", "tester"]]);
  assert.equal(pruneChannels({ ...s, agents: [], workflows: [] }), undefined);
  assert.equal(pruneChannels(team()), undefined);
});

test("team channels cannot connect two workflow members, but an existing invalid channel can be removed", () => {
  const { toggleChannel, teamProblems } = logic();
  const s = { ...team(), workflows: ["docs-team@v2", "review-flow@v3"] };
  assert.equal(toggleChannel(s, "docs-team", "review-flow"), undefined, "refuses adding a workflow-to-workflow channel");
  assert.equal(toggleChannel(s, "review-flow", "docs-team"), undefined);
  for (const channel of [["docs-team", "review-flow"], ["docs-team", "review-flow", "one-way"], ["review-flow", "docs-team", "one-way"]]) {
    assert.match(teamProblems({ ...s, channels: [channel] }).join(" "), /Two workflows cannot/);
    assert.equal(toggleChannel({ ...s, channels: [channel] }, "docs-team", "review-flow"), undefined, "allows removing invalid stored channels");
  }
  assert.deepEqual(plain(teamProblems({ ...s, channels: [["coder", "docs-team"], ["review-flow", "tester"], ["coder", "tester"]] })), []);
});

test("a team with two members of one name, a reserved name, or a stray channel is refused with the reason", () => {
  const { teamProblems } = logic();
  assert.deepEqual(plain(teamProblems(team())), []);
  assert.deepEqual(plain(teamProblems({ ...team(), agents: ["demo/coder@v1", "other/coder@v2"] })),
    ["Two members would be named coder: demo/coder@v1 and other/coder@v2"]);
  assert.deepEqual(plain(teamProblems({ ...team(), agents: ["demo/coder@v1", "demo/coder@v2"] })),
    ["Two members would be named coder: demo/coder@v1 and demo/coder@v2"]);
  assert.deepEqual(plain(teamProblems({ ...team(), agents: ["other/lead@v1"] })), ["Two members would be named lead: demo/lead@v1 and other/lead@v1"]);
  assert.deepEqual(plain(teamProblems({ ...team(), agents: ["demo/docs-team@v1"] })), ["Two members would be named docs-team: demo/docs-team@v1 and docs-team@v2"]);
  assert.deepEqual(plain(teamProblems({ ...team(), agents: ["demo/workflow@v1"] })), ["A member cannot be named workflow: demo/workflow@v1"]);
  assert.deepEqual(plain(teamProblems({ service: "team", agents: ["demo/coder@v1"] })), ["A team needs a lead agent"]);
  assert.deepEqual(plain(teamProblems({ ...team(), channels: [["lead", "ghost"], ["coder", "coder"], ["lead"]] })), [
    "A channel names ghost, who is not a member",
    "A channel connects two different members, not coder with itself",
    "A channel connects exactly two members",
  ]);
});

test("a member that becomes the lead swaps places with the old lead", () => {
  const { setLead } = logic();
  assert.deepEqual(plain(setLead(team(), "demo/tester@v1")), { agent: "demo/tester@v1", agents: ["demo/coder@v1", "demo/lead@v1", "demo/reviewer@v1"] });
  assert.deepEqual(plain(setLead(team(), "demo/boss@v1")), { agent: "demo/boss@v1", agents: team().agents }, "an outsider replaces the lead");
  assert.deepEqual(plain(setLead({ service: "team", agents: ["demo/coder@v1"] }, "demo/coder@v1")), { agent: "demo/coder@v1", agents: [] });
});

test("only workflows that do not lead back are offered for a workflow node or a team", () => {
  const { reachesWorkflow, nestableWorkflows, nestedNames, nestedRefs } = logic();
  const workflows = [
    { name: "root", steps: [{ id: "a", service: "agent" }, { id: "build", service: "workflow", workflow: "team-build@v3" }] },
    { name: "team-build", steps: [{ id: "team", service: "team", agent: "demo/lead@v1", workflows: ["docs@v1"] }] },
    { name: "docs", steps: [{ id: "write", service: "agent" }] },
    { name: "other", steps: [] },
  ];
  assert.deepEqual(plain(nestedRefs(workflows[0])), ["team-build@v3"]);
  assert.deepEqual(plain(nestedNames(workflows[0])), ["team-build"], "by name, without the pinned version");
  assert.deepEqual(plain(nestedNames(workflows[1])), ["docs"]);
  assert.equal(reachesWorkflow(workflows, "root", "docs"), true);
  assert.equal(reachesWorkflow(workflows, "docs", "root"), false);
  assert.equal(reachesWorkflow(workflows, "root", "root"), true);
  assert.equal(reachesWorkflow(workflows, "gone", "root"), false);
  assert.deepEqual(plain(nestableWorkflows(workflows, "docs")), ["other"]);
  assert.deepEqual(plain(nestableWorkflows(workflows, "team-build")), ["docs", "other"]);
  assert.deepEqual(plain(nestableWorkflows(workflows, "root")), ["team-build", "docs", "other"]);
  // A loop that is stored already does not hang the check.
  const loop = [{ name: "a", steps: [{ service: "workflow", workflow: "b" }] }, { name: "b", steps: [{ service: "workflow", workflow: "a" }] }, { name: "c", steps: [] }];
  assert.deepEqual(plain(nestableWorkflows(loop, "c")), ["a", "b"]);
});

test("a renamed workflow is followed by workflow nodes, teams, and channels", () => {
  const { renameWorkflowRefs } = logic();
  const workflows = [
    { name: "root", steps: [{ id: "b", service: "workflow", workflow: "docs@v2" }, { id: "r", service: "route", workflow: "docs" }, { id: "c", service: "workflow", workflow: "docs" }, { id: "d", service: "workflow", workflow: "docs-two@v1" }] },
    { name: "tb", steps: [{ id: "t", service: "team", agent: "demo/lead@v1", workflows: ["docs@v3", "x@v1"], channels: [["lead", "docs"], ["lead", "x"]] }] },
  ];
  renameWorkflowRefs(workflows, "docs", "manual");
  assert.equal(workflows[0].steps[0].workflow, "manual@v2", "the pinned version stays");
  assert.equal(workflows[0].steps[1].workflow, "docs", "only workflow steps");
  assert.equal(workflows[0].steps[2].workflow, "manual", "a reference without a version stays without one");
  assert.equal(workflows[0].steps[3].workflow, "docs-two@v1", "another workflow whose name starts alike");
  assert.deepEqual(plain(workflows[1].steps[0].workflows), ["manual@v3", "x@v1"]);
  assert.deepEqual(plain(workflows[1].steps[0].channels), [["lead", "manual"], ["lead", "x"]], "a channel names the member, without the version");
  const placed = [{ name: "tb", steps: [{ id: "t", service: "team", agent: "demo/lead@v1", workflows: ["docs@v1"], member_positions: { lead: { x: 50, y: 30 }, docs: { x: 20, y: 90 } } }] }];
  renameWorkflowRefs(placed, "docs", "manual");
  assert.deepEqual(plain(placed[0].steps[0].member_positions), { lead: { x: 50, y: 30 }, manual: { x: 20, y: 90 } }, "the member's place follows its name");
});

test("a grouped workflow member is named by the last part of its name, as the orchestrator names it", () => {
  const { memberName, teamMembers, removeMember, teamProblems, defaultChannels } = logic();
  assert.equal(memberName("ecc/ECC-plan@v1"), "ECC-plan");
  assert.equal(memberName("ecc/review/code-review"), "code-review");
  const s = { id: "team", service: "team", agent: "ecc/planner@v1", agents: ["ecc/coder@v1"], workflows: ["ecc/ECC-plan@v1", "ops/deploy@v3"] };
  assert.deepEqual(plain(teamMembers(s).map((m) => [m.name, m.kind, m.ref])), [
    ["planner", "agent", "ecc/planner@v1"], ["coder", "agent", "ecc/coder@v1"], ["ECC-plan", "workflow", "ecc/ECC-plan@v1"], ["deploy", "workflow", "ops/deploy@v3"],
  ]);
  assert.deepEqual(plain(defaultChannels(s)), [["planner", "coder"], ["planner", "ECC-plan"], ["planner", "deploy"]]);
  assert.deepEqual(plain(removeMember(s, "ECC-plan")), { agents: ["ecc/coder@v1"], workflows: ["ops/deploy@v3"] });
  // Two workflows of one name in different groups would be one member.
  assert.deepEqual(plain(teamProblems({ ...s, workflows: ["ecc/deploy@v1", "ops/deploy@v3"] })), ["Two members would be named deploy: ecc/deploy@v1 and ops/deploy@v3"]);
});

test("a workflow moved to another group keeps its channels and places; a new last part renames them", () => {
  const { renameWorkflowRefs } = logic();
  const team = () => ({ id: "t", service: "team", agent: "demo/lead@v1", workflows: ["ecc/ECC-plan@v2"], channels: [["lead", "ECC-plan"]], member_positions: { "ECC-plan": { x: 20, y: 90 } } });
  const moved = [{ name: "root", steps: [{ id: "n", service: "workflow", workflow: "ecc/ECC-plan@v2" }, team()] }];
  renameWorkflowRefs(moved, "ecc/ECC-plan", "plans/ECC-plan");
  assert.equal(moved[0].steps[0].workflow, "plans/ECC-plan@v2");
  assert.deepEqual(plain(moved[0].steps[1].workflows), ["plans/ECC-plan@v2"]);
  assert.deepEqual(plain(moved[0].steps[1].channels), [["lead", "ECC-plan"]], "the member keeps its name");
  assert.deepEqual(plain(moved[0].steps[1].member_positions), { "ECC-plan": { x: 20, y: 90 } });
  const renamed = [{ name: "root", steps: [team()] }];
  renameWorkflowRefs(renamed, "ecc/ECC-plan", "ecc/plan");
  assert.deepEqual(plain(renamed[0].steps[0].workflows), ["ecc/plan@v2"]);
  assert.deepEqual(plain(renamed[0].steps[0].channels), [["lead", "plan"]]);
  assert.deepEqual(plain(renamed[0].steps[0].member_positions), { plan: { x: 20, y: 90 } });
});

test("a grouped workflow travels in the hash as one part and comes back from it", () => {
  const { parseHash, formatHash } = logic();
  assert.equal(formatHash({ workflow: "ecc/ECC-plan", step: "plan" }), "#workflows/ecc%2FECC-plan/plan");
  assert.equal(formatHash({ workflow: "ecc/ECC-plan", version: 2, path: ["ops/deploy@v1"] }), "#workflows/ecc%2FECC-plan?version=2&path=ops%2Fdeploy%40v1");
  for (const value of [{ workflow: "ecc/review/code-review", step: "plan", version: 2, path: ["ecc/ECC-plan@v1", "root"] }, { workflow: "ecc/ECC-plan", step: "", version: 0, path: [] }]) {
    assert.deepEqual(plain(parseHash(formatHash(value).slice("#workflows".length))), value);
  }
  // A link typed with plain slashes is read by the names there are.
  const names = ["ecc/ECC-plan", "ecc/review/code-review", "demo"];
  assert.deepEqual(plain(parseHash("/ecc/ECC-plan/plan", names)), { workflow: "ecc/ECC-plan", step: "plan", version: 0, path: [] });
  assert.deepEqual(plain(parseHash("/ecc/review/code-review", names)), { workflow: "ecc/review/code-review", step: "", version: 0, path: [] });
  assert.deepEqual(plain(parseHash("/demo/plan", names)), { workflow: "demo", step: "plan", version: 0, path: [] });
  assert.deepEqual(plain(parseHash("/ecc%2FECC-plan/plan", names)), { workflow: "ecc/ECC-plan", step: "plan", version: 0, path: [] });
  assert.deepEqual(plain(parseHash("/ecc/ECC-plan/plan")), { workflow: "ecc", step: "ECC-plan", version: 0, path: [] }, "without the names, the first part is the workflow");
});

test("a workflow reference pins a version; a bare name means the newest", () => {
  const { WorkflowRef, newestRef, versionWord } = logic();
  assert.deepEqual(plain(WorkflowRef.parse("team-build@v2")), { name: "team-build", version: 2 });
  assert.deepEqual(plain(WorkflowRef.parse("team-build@v12")), { name: "team-build", version: 12 });
  assert.deepEqual(plain(WorkflowRef.parse("team-build")), { name: "team-build", version: 0 });
  assert.deepEqual(plain(WorkflowRef.parse("")), { name: "", version: 0 });
  assert.deepEqual(plain(WorkflowRef.parse(undefined)), { name: "", version: 0 });
  // Only @vN with N from 1 pins a version: anything else is part of a bare name.
  for (const bare of ["a@v0", "a@2", "a@latest", "a@v1x", "a@v-1"]) assert.deepEqual(plain(WorkflowRef.parse(bare)), { name: bare, version: 0 }, bare);
  assert.equal(WorkflowRef.format("team-build", 2), "team-build@v2");
  assert.equal(WorkflowRef.format("team-build", 0), "team-build");
  assert.equal(WorkflowRef.format("team-build"), "team-build");
  assert.equal(WorkflowRef.nameOf("docs-team@v1"), "docs-team");
  for (const ref of ["a@v1", "a-b@v20", "plain"]) assert.equal(WorkflowRef.format(...Object.values(WorkflowRef.parse(ref))), ref, ref);
  // The newest version of a definition: the highest it lists, else its own, else none (a draft).
  assert.equal(WorkflowRef.newest({ name: "a", version: 2, versions: [1, 2, 3] }), 3);
  assert.equal(WorkflowRef.newest({ name: "a", version: 2 }), 2);
  assert.equal(WorkflowRef.newest({ name: "a", steps: [] }), 0);
  assert.equal(WorkflowRef.newest(undefined), 0);
  const defs = [{ name: "a", version: 3, versions: [1, 2, 3] }, { name: "b", version: 1, versions: [1] }];
  assert.equal(WorkflowRef.find(defs, "a"), defs[0]);
  assert.equal(WorkflowRef.find(defs, "a@v3"), defs[0], "a pin to the newest version is in the list");
  assert.equal(WorkflowRef.find(defs, "a@v2"), undefined, "an older version has to be loaded");
  assert.equal(WorkflowRef.find(defs, "c"), undefined);
  assert.equal(newestRef(defs, "a"), "a@v3");
  assert.equal(newestRef(defs, "b"), "b@v1");
  assert.equal(newestRef(defs, "gone"), "gone", "nothing to pin to");
  assert.equal(versionWord("a@v3"), "v3");
  assert.equal(versionWord("a"), "");
});

test("versions are listed newest first, the newest marked, a pin that is gone as missing", () => {
  const { versionOptions } = logic();
  assert.deepEqual(plain(versionOptions([1, 2, 3])), [{ version: 3, label: "v3 (newest)" }, { version: 2, label: "v2" }, { version: 1, label: "v1" }]);
  assert.deepEqual(plain(versionOptions([3, 1, 2, 2])), [{ version: 3, label: "v3 (newest)" }, { version: 2, label: "v2" }, { version: 1, label: "v1" }], "sorted, each once");
  assert.deepEqual(plain(versionOptions([1])), [{ version: 1, label: "v1 (newest)" }]);
  assert.deepEqual(plain(versionOptions([1, 3], 2)), [{ version: 3, label: "v3 (newest)" }, { version: 2, label: "v2 (missing)" }, { version: 1, label: "v1" }]);
  assert.deepEqual(plain(versionOptions([1, 2], 2)), [{ version: 2, label: "v2 (newest)" }, { version: 1, label: "v1" }], "a pin that exists adds nothing");
  assert.deepEqual(plain(versionOptions(undefined)), []);
  assert.deepEqual(plain(versionOptions([], 4)), [{ version: 4, label: "v4 (missing)" }]);
});

test("Save sends the definition without the orchestrator's fields, and edits are told from the stored version", () => {
  const { storable, fingerprint } = logic();
  const stored = { name: "demo", version: 3, versions: [1, 2, 3], created: "2026-11-01T10:00:00Z", created_by: "ada", description: "A demo", steps: [{ id: "a", service: "agent", position: { x: 0, y: 40 } }] };
  assert.deepEqual(plain(storable(stored)), { name: "demo", description: "A demo", steps: [{ id: "a", service: "agent", position: { x: 0, y: 40 } }] });
  assert.deepEqual(plain(storable(undefined)), {});
  const copy = structuredClone(stored);
  assert.equal(fingerprint(copy), fingerprint(stored));
  // A new version number or a new name is no edit; a moved node, a changed field, a new step are.
  assert.equal(fingerprint({ ...copy, version: 4, versions: [1, 2, 3, 4], created: "later", name: "renamed" }), fingerprint(stored));
  copy.steps[0].position.x = 12;
  assert.notEqual(fingerprint(copy), fingerprint(stored), "a moved node");
  copy.steps[0].position.x = 0;
  assert.equal(fingerprint(copy), fingerprint(stored), "moved back");
  assert.equal(fingerprint({ ...stored, description: "Another" }), fingerprint(stored), "description changes have an in-place save path");
  assert.notEqual(fingerprint({ ...stored, steps: [...stored.steps, { id: "b", service: "gate" }] }), fingerprint(stored));
});

test("the canvas head: Save only with unsaved edits, an older version read-only until edited", () => {
  const { headState } = logic();
  // The newest version without edits: nothing to save, no note.
  assert.deepEqual(plain(headState({ version: 3, newest: 3, steps: 2 })), { next: 4, note: "", canSave: false, saveTip: "No unsaved edits" });
  assert.deepEqual(plain(headState({ version: 3, newest: 3, steps: 2, dirty: true })), { next: 4, note: "", canSave: true, saveTip: "Stores the workflow as v4" });
  // An older version is read-only until edited; Save then stores the next version, not a new v2.
  assert.deepEqual(plain(headState({ version: 2, newest: 3, steps: 2 })), { next: 4, note: "Viewing v2", canSave: false, saveTip: "No unsaved edits" });
  assert.deepEqual(plain(headState({ version: 2, newest: 3, steps: 2, dirty: true })), { next: 4, note: "Edited v2: saves as v4", canSave: true, saveTip: "Stores the workflow as v4" });
  // A new workflow is a draft until its first Save, which stores v1; it needs a step.
  assert.deepEqual(plain(headState({ draft: true })), { next: 1, note: "Not saved yet", canSave: false, saveTip: "Needs a step" });
  assert.deepEqual(plain(headState({ draft: true, steps: 1, dirty: true })), { next: 1, note: "Not saved yet", canSave: true, saveTip: "Stores the workflow as v1" });
  // While a save is on its way a second one cannot start.
  assert.equal(headState({ version: 3, newest: 3, steps: 2, dirty: true, saving: true }).canSave, false);
  assert.equal(headState().canSave, false);
});

test("a run from the editor needs a saved version", () => {
  const { runBlocker } = logic();
  assert.equal(runBlocker({ steps: 2 }), "");
  assert.match(runBlocker({ steps: 2, dirty: true }), /saved first/);
  assert.match(runBlocker({ steps: 2, draft: true }), /saved first/);
  assert.equal(runBlocker({ steps: 0, draft: true }), "Needs a step");
  assert.notEqual(runBlocker({ steps: 2, saving: true }), "");
  // The page asks before it opens Select Inputs and passes the version on the canvas.
  assert.match(source, /if \(blocked\) \{ setStatus\(`Not started: /);
  assert.match(source, /Inputs\.open\(workflow\(\)\.name, shownVersion\(\)\);/);
});

test("nothing is saved automatically: Save posts a new version, and the page code sees what it uses", () => {
  assert.doesNotMatch(source, /api\("PUT"/);
  assert.doesNotMatch(source, /SAVE_DELAY|flushSave|saving\.timer/);
  assert.match(source, /api\("POST", `\$\{API\}\/workflows\/\$\{encodeURIComponent\(w\.name\)\}\/versions`, L\.storable\(w\), creatorHeaders\(\)\)/);
  assert.match(source, /api\("POST", `\$\{API\}\/workflows\/\$\{encodeURIComponent\(from\)\}\/rename`, \{ name: to \}/);
  assert.match(source, /api\("DELETE", `\$\{API\}\/workflows\/\$\{encodeURIComponent\(e\.name\)\}`/);
  assert.match(source, /api\("GET", `\$\{API\}\/workflows\/\$\{encodeURIComponent\(name\)\}\/versions\/\$\{version\}`\)/);
  assert.match(source, /confirmAction\("Discard your unsaved changes\?", "Discard"\)/);
  // The page reaches the logic only through L: every L.<name> it uses is exported.
  const exported = Object.keys(logic());
  const used = new Set([...source.slice(source.indexOf("// ---- Page ----")).matchAll(/\bL\.([A-Za-z_]+)/g)].map((m) => m[1]));
  for (const name of used) assert.ok(exported.includes(name), `WorkflowLogic exports ${name}`);
  // And it never names a variable of the logic closure directly.
  const page = source.slice(source.indexOf("// ---- Page ----"));
  for (const hidden of ["MEMBER_H", "ONE_WAY", "RESERVED_MEMBER"]) assert.doesNotMatch(page, new RegExp(`(?<![.\\w])${hidden}\\b`), hidden);
});

test("a gate can return rejections to the team and agent steps it depends on, never to others", () => {
  const { rejectTargets } = logic();
  const w = { name: "tb", steps: [
    { id: "team", service: "team" }, { id: "other-team", service: "team" }, { id: "agent", service: "agent" }, { id: "router", service: "route" },
    { id: "review", service: "gate", depends_on: ["agent", "team", "gone"] },
  ] };
  assert.deepEqual(plain(rejectTargets(w, w.steps[4])), ["agent", "team"], "a team as before, and now an agent step too");
  assert.deepEqual(plain(rejectTargets(w, { id: "g", service: "gate" })), []);
});

test("the path of a nested workflow travels in the hash", () => {
  const { parseHash, formatHash } = logic();
  assert.equal(formatHash({ workflow: "" }), "#workflows");
  assert.equal(formatHash({ workflow: "team-demo" }), "#workflows/team-demo");
  assert.equal(formatHash({ workflow: "team-demo", step: "build" }), "#workflows/team-demo/build");
  assert.equal(formatHash({ workflow: "docs", step: "write", path: ["team-demo", "team-build"] }), "#workflows/docs/write?path=team-demo,team-build");
  assert.equal(formatHash({ workflow: "docs", path: ["team-demo"] }), "#workflows/docs?path=team-demo");
  assert.equal(formatHash({ workflow: "a b", step: "c/d", path: ["x,y"] }), "#workflows/a%20b/c%2Fd?path=x%2Cy");
  assert.deepEqual(plain(parseHash("")), { workflow: "", step: "", version: 0, path: [] });
  assert.deepEqual(plain(parseHash("/team-demo")), { workflow: "team-demo", step: "", version: 0, path: [] });
  assert.deepEqual(plain(parseHash("/team-demo/build")), { workflow: "team-demo", step: "build", version: 0, path: [] });
  assert.deepEqual(plain(parseHash("/docs?path=team-demo")), { workflow: "docs", step: "", version: 0, path: ["team-demo"] });
  assert.deepEqual(plain(parseHash("/docs/write?path=team-demo,team-build")), { workflow: "docs", step: "write", version: 0, path: ["team-demo", "team-build"] });
  assert.deepEqual(plain(parseHash("/a%20b/c%2Fd?path=x%2Cy")), { workflow: "a b", step: "c/d", version: 0, path: ["x,y"] });
  assert.deepEqual(plain(parseHash("/bad%zz/step?path=,")), { workflow: "bad%zz", step: "step", version: 0, path: [] });
  for (const value of [{ workflow: "docs", step: "write", version: 0, path: ["team-demo", "team-build"] }, { workflow: "docs", step: "", version: 0, path: [] }]) {
    assert.deepEqual(plain(parseHash(formatHash(value).slice("#workflows".length))), value);
  }
});

test("the hash carries the version when it is not the newest, so a reload returns to it", () => {
  const { parseHash, formatHash } = logic();
  assert.equal(formatHash({ workflow: "team-demo", version: 2 }), "#workflows/team-demo?version=2");
  assert.equal(formatHash({ workflow: "team-demo", step: "build", version: 2 }), "#workflows/team-demo/build?version=2");
  assert.equal(formatHash({ workflow: "team-demo", version: 0 }), "#workflows/team-demo", "the newest version needs none");
  assert.equal(formatHash({ workflow: "docs", step: "write", version: 3, path: ["team-demo@v2", "team-build"] }), "#workflows/docs/write?version=3&path=team-demo%40v2,team-build");
  assert.deepEqual(plain(parseHash("/team-demo?version=2")), { workflow: "team-demo", step: "", version: 2, path: [] });
  assert.deepEqual(plain(parseHash("/docs/write?version=3&path=team-demo%40v2,team-build")), { workflow: "docs", step: "write", version: 3, path: ["team-demo@v2", "team-build"] });
  assert.deepEqual(plain(parseHash("/docs?path=a&version=4")), { workflow: "docs", step: "", version: 4, path: ["a"] }, "in either order");
  for (const bad of ["/docs?version=0", "/docs?version=x", "/docs?version=-1", "/docs?version=2x", "/docs?subversion=2"]) assert.equal(parseHash(bad).version, 0, bad);
  for (const value of [{ workflow: "docs", step: "write", version: 2, path: ["team-demo@v1"] }, { workflow: "docs", step: "", version: 7, path: [] }]) {
    assert.deepEqual(plain(parseHash(formatHash(value).slice("#workflows".length))), value);
  }
});

test("member tiles sit on a ring with the lead at the top and never overlap", () => {
  const { teamLayout } = logic();
  assert.deepEqual(plain(teamLayout(0)), { height: 52, points: [] });
  assert.deepEqual(plain(teamLayout(1).points), [{ x: 50, y: 26 }]);
  const two = teamLayout(2);
  assert.equal(two.points[0].x, 50);
  assert.equal(two.points[1].x, 50);
  assert.ok(two.points[1].y - two.points[0].y >= 44);
  for (let n = 1; n <= 14; n++) {
    const { height, points } = teamLayout(n);
    assert.equal(points.length, n);
    assert.equal(points[0].x, 50, "the lead is centred");
    assert.equal(points[0].y, Math.min(...points.map((p) => p.y)), "the lead is at the top");
    for (const p of points) {
      assert.ok(p.x >= 13 && p.x <= 87, `tile ${n} stays inside the panel`);
      assert.ok(p.y >= 22 && p.y <= height - 22, `tile ${n} stays inside the ring`);
    }
    // Tiles are 26% wide and 44 px high: two tiles are apart in one direction at least.
    for (let i = 0; i < n; i++) for (let j = i + 1; j < n; j++) {
      const apart = Math.abs(points[i].x - points[j].x) >= 26 || Math.abs(points[i].y - points[j].y) >= 44;
      assert.ok(apart, `${n} members: tiles ${i} and ${j} overlap`);
    }
  }
});

// ---- The page uses the logic and its styles exist ----

test("the editor knows the workflow and team kinds and keeps their fields when a step is duplicated", () => {
  assert.match(source, /const SPECIAL = \["agent", "gate", "interactive", "route", "workflow", "team", "merge", "batch", \.\.\.L\.POD_KINDS\]/);
  assert.match(source, /"max_attempts", "workflow", "workflows", "channels", "max_turns", "member_positions", "capacity", "size", "input_timeout",/);
  assert.match(source, /addEventListener\("dblclick"/);
  for (const field of ["workflow", "workflow_version", "on_reject", "max_turns", "lead"]) assert.match(source, new RegExp(`\\n    ${field}\\(s, v\\)`), field);
});

test("a workflow node stores its workflow and version as one pinned reference", () => {
  const fields = source.slice(source.indexOf("  const stepFields = {"), source.indexOf("  // stepForm builds"));
  const context = {};
  vm.runInNewContext(`${refSource}
    ${source.slice(0, source.indexOf("// ---- Page ----"))}
    const L = WorkflowLogic;
    const SPECIAL = ["agent", "gate", "interactive", "route", "workflow", "team", "merge", "batch"];
    const defs = [{ name: "team-build", version: 3, versions: [1, 2, 3] }, { name: "docs", version: 1, versions: [1] }];
    const state = { agentRefs: [] };
    const pinNewest = (name) => WorkflowRef.format(name, WorkflowRef.newest(defs.find((w) => w.name === name)));
    const nestable = () => defs.map((w) => w.name);
    ${fields}\nglobalThis.out = stepFields;`, context);
  const stepFields = context.out;
  const s = { id: "build", service: "agent", agent: "demo/a@v1", timeout: "2m", max_attempts: 2 };
  stepFields.service(s, "workflow");
  assert.deepEqual(plain(s), { id: "build", service: "workflow", workflow: "team-build@v3" }, "the first workflow, pinned to its newest version");
  stepFields.workflow_version(s, "2");
  assert.equal(s.workflow, "team-build@v2");
  stepFields.workflow(s, "docs");
  assert.equal(s.workflow, "docs@v1", "another workflow starts at its newest version");
  stepFields.workflow_version(s, "");
  assert.equal(s.workflow, "docs@v1", "no version is no change");
  stepFields.workflow(s, "");
  assert.equal("workflow" in s, false);
  stepFields.workflow_version(s, "2");
  assert.equal("workflow" in s, false, "a version alone names nothing");
  // The panel has both selects; the team picker and the add-member menu offer pinned references.
  assert.match(source, /field\("Workflow", "workflow", nested,/);
  assert.match(source, /field\("Version", "workflow_version", nestedVersion,/);
  assert.match(source, /nestable\(\)\.filter\(\(name\) => !chosen\.some\(\(ref\) => WorkflowRef\.nameOf\(ref\) === name\)\)\.map\(pinNewest\)/);
  assert.match(source, /nestable\(\)\.filter\(\(name\) => !members\.some\(\(m\) => m\.kind === "workflow" && WorkflowRef\.nameOf\(m\.ref\) === name\)\)\.map\(pinNewest\)/, "a member is named by its last part, so the workflow is compared by its reference");
});

test("a team keeps its channels, places and problems by member name when workflows are pinned", () => {
  const { teamMembers, teamChannels, defaultChannels, pruneChannels, teamProblems, removeMember } = logic();
  const s = { id: "team", service: "team", agent: "demo/lead@v1", agents: ["demo/coder@v1"], workflows: ["docs-team@v2"] };
  assert.deepEqual(plain(defaultChannels(s)), [["lead", "coder"], ["lead", "docs-team"]]);
  const drawn = { ...s, channels: [["lead", "docs-team"], ["coder", "docs-team", "one-way"]] };
  assert.deepEqual(plain(teamProblems(drawn)), []);
  assert.deepEqual(plain(pruneChannels(drawn)), drawn.channels);
  assert.deepEqual(plain(teamProblems({ ...s, channels: [["lead", "docs-team@v2"]] })), ["A channel names docs-team@v2, who is not a member"]);
  // The same workflow twice, at two versions, would give two members one name.
  assert.deepEqual(plain(teamProblems({ ...s, workflows: ["docs-team@v2", "docs-team@v1"] })), ["Two members would be named docs-team: docs-team@v2 and docs-team@v1"]);
  assert.deepEqual(plain(teamProblems({ ...s, workflows: ["workflow@v1"] })), ["A member cannot be named workflow: workflow@v1"]);
  assert.deepEqual(plain(removeMember(s, "docs-team")), { agents: ["demo/coder@v1"], workflows: [] });
  assert.equal(teamMembers(s)[2].name, "docs-team");
  assert.equal(teamChannels(s).isDefault, true);
});

test("the styles of the new nodes sit in one marked block at the end of the stylesheet", () => {
  const at = style.indexOf("/* workflow editor: team and workflow nodes */");
  assert.ok(at > 0);
  const block = style.slice(at);
  for (const selector of [".tile .band", ".tile .band-line", ".wf-path", ".team-member", ".team-channel.default", ".nested-steps"]) {
    assert.ok(block.includes(selector), selector);
  }
});

test("a member tile stays under the pointer while it is pressed, so the click arrives", () => {
  // Every button moves by a transform while pressed. A tile centred by its own transform lost it
  // then, jumped half its size away, and the click landed on a channel line instead.
  assert.match(style, /button:active \{ transform: translateY\(1px\); \}/, "the pressed state this guards against");
  const rule = /\n\.team-member \{([^}]*)\}/.exec(style)?.[1] || "";
  assert.match(rule, /position: absolute/);
  assert.match(rule, /margin: -22px 0 0 calc\(max\(26%, 96px\) \/ -2\)/, "centred by margins: half of 44 px and half of the width");
  assert.match(rule, /width: max\(26%, 96px\)/, "wide enough for a member name in a narrow panel");
  assert.match(rule, /height: 44px/);
  assert.doesNotMatch(rule, /transform/);
});

test("a workflow node has its own shape and the team panel says how a channel is drawn", () => {
  assert.match(source, /class: "band"/);
  assert.match(source, /class: "band-line"/);
});

test("a member tile's tooltip says what a click does; the panel shows no instruction line", () => {
  const { memberTip, teamMembers, teamChannels } = logic();
  const [lead, coder, , reviewer] = teamMembers(team());
  const { pairs } = teamChannels({ ...team(), channels: [["lead", "coder"], ["reviewer", "coder"]] });
  assert.equal(memberTip(coder, null, pairs), "Picks demo/coder@v1 to draw a channel to the member picked next");
  assert.equal(memberTip(coder, coder, pairs), "Ends the pick of coder");
  assert.equal(memberTip(reviewer, lead, pairs), "Adds a channel between lead and reviewer");
  assert.equal(memberTip(lead, coder, pairs), "Removes the channel between coder and lead");
  assert.equal(memberTip(coder, reviewer, pairs), "Removes the channel between reviewer and coder", "whichever way it was stored");
  assert.equal(memberTip(coder, lead, teamChannels(team()).pairs), "Removes the channel between lead and coder", "a default channel too");
  assert.doesNotMatch(source, /team-hint/);
  assert.doesNotMatch(style, /team-hint/);
});

test("Make lead and Remove end the pick, so the next click picks a member", () => {
  assert.match(source, /const makeLead = \(m\) => \{ teamPick = null; edit\(/);
  assert.match(source, /const remove = \(m\) => \{ teamPick = null; edit\(/);
});

test("a pick shows in the team opened on the canvas too, so its next click draws a channel there", () => {
  assert.match(source, /teamPick = on \? null : \{ step: s\.id, name: m\.name \};\s+renderTeams\(\);/);
  assert.match(source, /const renderTeams = \(\) => \{ renderInspector\(\); if \(state\.teamOpen\) renderCanvas\(\); \};/);
});

test("every member tile has a port; a drag from it onto another member adds their channel", () => {
  assert.match(source, /className: "team-port"/);
  assert.match(source, /closest\("\.team-member"\)[\s\S]{0,400}toggle\(m\.name, to\)/);
  assert.match(style, /\.team-port \{/);
});

test("an arrow to a step beside its source runs from side to side, and under the tiles in between", () => {
  const { edgeRoute } = logic();
  const size = { w: 220, h: 64 }, col = 268, row = 136;
  const at = (c, r) => ({ x: c * col, y: 40 + r * row });
  // Below the source: from the bottom port to the top port, as before, however many rows down or columns aside.
  assert.deepEqual(plain(edgeRoute(at(0, 0), at(0, 1), [], size)), { kind: "down" });
  assert.deepEqual(plain(edgeRoute(at(0, 0), at(2, 1), [at(1, 0)], size)), { kind: "down" });
  assert.deepEqual(plain(edgeRoute(at(1, 0), at(0, 3), [], size)), { kind: "down" });
  // Beside it in the same row: out of the facing side, to the right or to the left.
  assert.deepEqual(plain(edgeRoute(at(0, 0), at(1, 0), [at(2, 0), at(0, 1)], size)), { kind: "side", dir: 1 });
  assert.deepEqual(plain(edgeRoute(at(1, 0), at(0, 0), [], size)), { kind: "side", dir: -1 });
  // A tile dragged a little up or down is still beside its source; one that overlaps it sideways is not.
  assert.deepEqual(plain(edgeRoute(at(0, 0), { x: col, y: 40 + 30 }, [], size)), { kind: "side", dir: 1 });
  assert.deepEqual(plain(edgeRoute(at(0, 0), { x: col, y: 40 - 50 }, [], size)), { kind: "side", dir: 1 });
  assert.deepEqual(plain(edgeRoute(at(0, 0), { x: 100, y: 40 }, [], size)), { kind: "down" });
  // Other tiles in between: under them, along the gap below the row (24 px under the lowest tile).
  assert.deepEqual(plain(edgeRoute(at(0, 0), at(2, 0), [at(1, 0)], size)), { kind: "under", dir: 1, y: 40 + 64 + 24 });
  assert.deepEqual(plain(edgeRoute(at(3, 0), at(0, 0), [at(1, 0), { x: 2 * col, y: 40 + 10 }, at(1, 1)], size)), { kind: "under", dir: -1, y: 50 + 64 + 24 });
  assert.deepEqual(plain(edgeRoute(at(0, 0), at(2, 0), [at(1, 1), at(3, 0)], size)), { kind: "side", dir: 1 }, "tiles of other rows or beyond the target are not in the way");
  // The canvas draws every dependency by this decision.
  assert.match(source, /const path = dependencyPath\(w, from, s\);/);
  assert.match(source, /if \(route\.kind === "down"\) return edgePath\(outPort\(from\), inPort\(s\), bowsLeft\(w, from, s\)\);/);
});

test("a channel's direction is stored as its sender first and one-way third", () => {
  const { setChannelDirection, channelSender, teamChannels, teamProblems, pruneChannels } = logic();
  const oneWay = setChannelDirection(team(), "lead", "coder", "coder");
  assert.deepEqual(plain(oneWay[0]), ["coder", "lead", "one-way"], "the sender comes first");
  assert.equal(oneWay.length, 4, "the default channels are kept");
  assert.equal(channelSender(oneWay[0]), "coder");
  assert.equal(channelSender(oneWay[1]), "");
  const stored = { ...team(), channels: oneWay };
  assert.deepEqual(plain(teamChannels(stored).pairs[0]), ["coder", "lead", "one-way"]);
  assert.deepEqual(plain(teamProblems(stored)), []);
  assert.deepEqual(plain(pruneChannels(stored)[0]), ["coder", "lead", "one-way"]);
  assert.deepEqual(plain(setChannelDirection(stored, "coder", "lead", "lead")[0]), ["lead", "coder", "one-way"], "reversed");
  assert.deepEqual(plain(setChannelDirection(stored, "lead", "coder", "")[0]), ["coder", "lead"], "both directions again");
  assert.match(teamProblems({ ...team(), channels: [["lead", "coder", "sideways"]] })[0], /two-way or one-way/);
});
