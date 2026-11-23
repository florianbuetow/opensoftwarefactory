import assert from "node:assert/strict";
import fs from "node:fs";
import test from "node:test";
import vm from "node:vm";

const read = (name) => fs.readFileSync(new URL(`web/${name}`, import.meta.url), "utf8");
const teams = read("teams.js");

// ---- Logic without the DOM ----

function logic() {
  const context = {};
  vm.runInNewContext(`${read("workflowref.js")}\n${teams}\nglobalThis.out = TeamsLogic;`, context); // index.html loads workflowref.js first
  return context.out;
}
const plain = (value) => JSON.parse(JSON.stringify(value)); // across the vm realm

test("a team is addressed by its blob path without teams/, optionally on one channel", () => {
  const { teamHash, teamFromHash, teamPath, traceOf, stepOf, validTeam } = logic();
  assert.equal(teamPath("t-1", "build/team"), "teams/t-1/build/team");
  assert.equal(teamHash("teams/t-1/build/team"), "#teams/t-1/build/team");
  assert.equal(teamHash("teams/t-1/team", "coder--reviewer"), "#teams/t-1/team?channel=coder--reviewer");
  assert.deepEqual(plain(teamFromHash("/t-1/build/team")), { team: "teams/t-1/build/team", channel: "" });
  assert.deepEqual(plain(teamFromHash("/t-1/team?channel=coder--reviewer")), { team: "teams/t-1/team", channel: "coder--reviewer" });
  assert.deepEqual(plain(teamFromHash("/t-1/team/?x=1&channel=a%2D%2Db")), { team: "teams/t-1/team", channel: "a--b" });
  for (const rest of ["", "/", "/t-1", "/t-1/../x", "/t-1/a b", "?channel=x"]) assert.equal(teamFromHash(rest).team, "", rest);
  assert.equal(traceOf("teams/t-1/build/team"), "t-1");
  assert.equal(stepOf("teams/t-1/build/team"), "build/team");
  for (const bad of ["teams/t-1", "user-sessions/t-1/team", "teams//x/y", null, 7, `teams/t/${"x".repeat(400)}`]) assert.equal(validTeam(bad), false, String(bad));
});

test("a team result manifest is recognised; any other gate input is not", () => {
  const { manifest } = logic();
  const doc = { summary: "Built it.", team: "teams/t-1/build/team", files: [{ name: "main.py", artifact: "teams/t-1/build/team/files/main.py" }, { name: "", artifact: "x" }, null, { name: "notes.md" }] };
  assert.deepEqual(plain(manifest(JSON.stringify(doc))), {
    summary: "Built it.", team: "teams/t-1/build/team", files: [{ name: "main.py", artifact: "teams/t-1/build/team/files/main.py" }],
  });
  assert.deepEqual(plain(manifest('{"team":"teams/t/s","files":[]}')), { summary: "", team: "teams/t/s", files: [] });
  for (const other of ["plain text", "# Title", "[1,2]", "null", '{"summary":"x"}', '{"team":"teams/t/s"}', '{"team":"runs/t/s","files":[]}', '{"team":"teams/t/s","files":"a"}']) {
    assert.equal(manifest(other), null, other);
  }
});

test("a result file is Markdown, JSON, or plain text by its extension, and code otherwise", () => {
  const { documentKind } = logic();
  assert.equal(documentKind("README.md"), "markdown");
  assert.equal(documentKind("data.JSON"), "json");
  assert.equal(documentKind("notes.txt"), "text");
  for (const name of ["main.py", "main_test.go", "app.tsx", "schema.sql", "Makefile"]) assert.equal(documentKind(name), "code", name);
});

test("channels come from team.json and the stored logs, with message counts, the task channel first", () => {
  const { channels, channelLabel } = logic();
  const team = { lead: "lead", channels: [{ id: "coder--reviewer", members: ["coder", "reviewer"] }, { id: "coder--lead", members: ["coder", "lead"] }, { id: "lead--tester", members: ["lead", "tester"] }] };
  const logs = [
    { id: "coder--lead", members: ["coder", "lead"], messages: [{ seq: 1 }, { seq: 2 }] },
    { id: "lead--workflow", members: ["lead", "workflow"], messages: [{ seq: 1 }] },
    { id: "coder--reviewer", messages: "broken" },
    null,
  ];
  assert.deepEqual(plain(channels(team, logs)), [
    { id: "lead--workflow", members: ["workflow", "lead"], count: 1, label: "workflow ↔ lead" },
    { id: "coder--lead", members: ["lead", "coder"], count: 2, label: "lead ↔ coder" },
    { id: "lead--tester", members: ["lead", "tester"], count: 0, label: "lead ↔ tester" },
    { id: "coder--reviewer", members: ["coder", "reviewer"], count: 0, label: "coder ↔ reviewer" },
  ]);
  // Without team.json the logs alone name the channels; a log without members is named by its ID.
  assert.deepEqual(plain(channels(null, [{ id: "a--b", messages: [] }])), [{ id: "a--b", members: ["a", "b"], count: 0, label: "a ↔ b" }]);
  assert.deepEqual(plain(channels(null, [])), []);
  assert.equal(channelLabel({ id: "coder--lead" }, "lead"), "lead ↔ coder");
});

test("members are listed with the lead first and marked", () => {
  const { members } = logic();
  const team = { lead: "lead", members: [{ name: "coder", kind: "agent", ref: "demo/coder@v1", description: "Writes code" }, { name: "lead", kind: "agent", ref: "demo/lead@v1" }, { name: "docs-team", kind: "workflow", ref: "docs-team" }, { kind: "agent" }] };
  assert.deepEqual(plain(members(team)), [
    { name: "lead", kind: "agent", ref: "demo/lead@v1", description: "", lead: true },
    { name: "coder", kind: "agent", ref: "demo/coder@v1", description: "Writes code", lead: false },
    { name: "docs-team", kind: "workflow", ref: "docs-team", description: "", lead: false },
  ]);
  assert.deepEqual(plain(members(null)), []);
});

test("a channel log's messages are shown in the order they were sent", () => {
  const { messages } = logic();
  const log = { messages: [{ seq: 2, from: "coder", text: "done", sent: "2026-01-01T00:00:02Z" }, { seq: 1, from: "lead", text: "go", sent: "2026-01-01T00:00:01Z" }, null, { seq: 3, from: "lead" }] };
  assert.deepEqual(plain(messages(log)), [
    { seq: 1, from: "lead", text: "go", sent: "2026-01-01T00:00:01Z" },
    { seq: 2, from: "coder", text: "done", sent: "2026-01-01T00:00:02Z" },
    { seq: 3, from: "lead", text: "", sent: "" },
  ]);
  assert.deepEqual(plain(messages(null)), []);
  assert.deepEqual(plain(messages({ messages: "x" })), []);
});

test("the round is named from the second review on", () => {
  const { roundLabel } = logic();
  for (const first of [undefined, null, 0, 1, "1", "x"]) assert.equal(roundLabel(first), "", String(first));
  assert.equal(roundLabel(2), "Round 2");
  assert.equal(roundLabel("3"), "Round 3");
});

test("earlier decisions of a gate are the records <artifact>.<n>, oldest first", () => {
  const { roundRecords, roundOf, isRoundRecord } = logic();
  const ids = ["runs/t/build/review.2", "runs/t/build/review", "runs/t/build/review.1", "runs/t/build/team", "runs/t/build/review.run.json", "runs/t/build/reviewer.1", "runs/t/build/review.0"];
  assert.deepEqual(plain(roundRecords("runs/t/build/review", ids)), [{ id: "runs/t/build/review.1", round: 1 }, { id: "runs/t/build/review.2", round: 2 }]);
  assert.deepEqual(plain(roundRecords("runs/t/build/review", ids, 2)), [{ id: "runs/t/build/review.1", round: 1 }]);
  assert.deepEqual(plain(roundRecords("runs/t/build/review", [])), []);
  assert.deepEqual(plain(roundOf("runs/t/review.3")), { artifact: "runs/t/review", round: 3 });
  assert.deepEqual(plain(roundOf("runs/t/review")), { artifact: "runs/t/review", round: 0 });
  assert.equal(isRoundRecord("review.1"), true);
  for (const step of ["review", "step-1", "review.0", "review.run.json"]) assert.equal(isRoundRecord(step), false, step);
});

const DEFS = [
  { name: "team-demo", steps: [{ id: "restructure", service: "agent", agent: "demo/restructure@v1" }, { id: "clarify", service: "interactive" }, { id: "build", service: "workflow", workflow: "team-build" }, { id: "pick", service: "route" }] },
  { name: "team-build", steps: [{ id: "team", service: "team", agent: "demo/lead@v1" }, { id: "review", service: "gate", depends_on: ["team"], on_reject: "team" }, { id: "docs", service: "workflow", workflow: "docs" }] },
  { name: "docs", steps: [{ id: "write", service: "agent", agent: "demo/writer@v1" }] },
];

test("a step is found by its path through workflow nodes", () => {
  const { stepAt } = logic();
  assert.equal(stepAt(DEFS, "team-demo", "build").service, "workflow");
  assert.equal(stepAt(DEFS, "team-demo", "build/team").service, "team");
  assert.equal(stepAt(DEFS, "team-demo", "build/review").on_reject, "team");
  assert.equal(stepAt(DEFS, "team-demo", "build/docs/write").agent, "demo/writer@v1");
  for (const path of ["missing", "build/missing", "restructure/team", "pick/team", "build/team/x"]) assert.equal(stepAt(DEFS, "team-demo", path), null, path);
  assert.equal(stepAt(DEFS, "unknown", "build"), null);
  assert.equal(stepAt(null, "team-demo", "build"), null);
});

test("steps of a nested workflow are listed under their workflow node; route children and round records are not", () => {
  const { stepRows } = logic();
  const top = ["restructure", "clarify", "build", "pick", "review.1"];
  const observed = ["build/team", "build/review.1", "build/docs/write", "build/extra", "pick/research", "build/team/deep"];
  // A finished run: what left an object, in the order of the nested definition, then the rest.
  assert.deepEqual(plain(stepRows(top, observed, DEFS, "team-demo")), [
    { id: "restructure", depth: 0 }, { id: "clarify", depth: 0 }, { id: "build", depth: 0 },
    { id: "build/team", depth: 1 }, { id: "build/docs", depth: 1 }, { id: "build/docs/write", depth: 2 }, { id: "build/extra", depth: 1 },
    { id: "pick", depth: 0 },
  ]);
  // A running run also lists the nested steps that have not run yet.
  assert.deepEqual(plain(stepRows(["build"], [], DEFS, "team-demo", true)), [
    { id: "build", depth: 0 }, { id: "build/team", depth: 1 }, { id: "build/review", depth: 1 }, { id: "build/docs", depth: 1 }, { id: "build/docs/write", depth: 2 },
  ]);
  // Without a definition the list is the top-level steps, as before.
  assert.deepEqual(plain(stepRows(["a", "b"], ["a/x"], [], "gone")), [{ id: "a", depth: 0 }, { id: "b", depth: 0 }]);
});

test("a failed run names the nested steps its error passes through", () => {
  const { failedPaths } = logic();
  assert.deepEqual(plain(failedPaths('step "build": child workflow failed: step "team": team stalled')), ["build", "build/team"]);
  assert.deepEqual(plain(failedPaths("activity timeout")), []);
  assert.deepEqual(plain(failedPaths("")), []);
});

// ---- Wiring ----

test("the team view has no menu item and keeps Runs active", () => {
  const app = read("app.js"), html = read("index.html");
  assert.match(app, /const SUBPAGES = \{ teams: \{ name: "teams", title: "Team", menu: "runs" \}, /);
  assert.doesNotMatch(app.slice(app.indexOf("const PAGES = ["), app.indexOf("];", app.indexOf("const PAGES = ["))), /teams/);
  assert.match(html, /<section class="page page-teams" id="tab-teams" hidden>/);
  assert.ok(html.indexOf('src="chat.js"') < html.indexOf('src="teams.js"') && html.indexOf('src="teams.js"') < html.indexOf('src="runs.js"'));
});

test("a gate that returns rejections to a team asks for a reason and does not say the run ends", () => {
  const decisions = read("decisions.js");
  assert.match(decisions, /back \? "Reason" : "Comment"/);
  assert.match(decisions, /"Needs a reason"/);
  assert.match(decisions, /Records your rejection and sends the reason back to the team/);
  // A gate without on_reject keeps its wording.
  assert.match(decisions, /Records your rejection and ends the run/);
});

test("a waiting gate is decided only in its detail, never from its row's menu", () => {
  const decisions = read("decisions.js");
  const menu = decisions.slice(decisions.indexOf("function menuFor("), decisions.indexOf("\n  }\n", decisions.indexOf("function menuFor(")));
  assert.match(menu, /if \(c\.kind === "pending"\) return \[run, trace, "-", clearItem\(state\.filter\)\];/);
  assert.doesNotMatch(menu, /"Approve"|"Reject"|decide\(/);
});

// ---- Regressions from the browser walk-through ----

test("another team in the hash is read at once: a load of the earlier team neither blocks nor overwrites it", async () => {
  // The loader of the page, run against stubs: the earlier team's read is still in flight when the next team opens.
  const source = teams.slice(teams.indexOf("  async function load(ask = true) {"), teams.indexOf("  // tick asks the orchestrator"));
  assert.doesNotMatch(source, /await refreshTeams\(\)/, "the orchestrator is not awaited before the blobstore read");
  const context = {};
  vm.runInNewContext(`
    ${teams.slice(0, teams.indexOf("// ---- Running teams"))}
    const gates = new Map(), asked = [], finishedTeams = new Set();
    const state = { generation: 1, team: "teams/a/team", loading: 0, loaded: false, logs: new Map(), docs: new Map(), info: null, finished: false, error: "", controller: null };
    const views = new Map();
    const refreshTeams = () => new Promise(() => {}); // an orchestrator that does not answer
    const readJSON = (id) => { asked.push(id); return new Promise((resolve) => gates.set(id, resolve)); };
    const listObjects = async (prefix) => (finishedTeams.has(prefix) ? [{ id: prefix + "/finished.json" }] : []);
    const listPrefix = async (prefix) => ({ directories: [], objects: await listObjects(prefix) });
    const pool = async (items, limit, fn) => Promise.all(items.map(fn));
    const runIndex = { statuses: new Map(), status: async () => ({}) };
    const renderList = () => {}, renderView = () => {}, openWanted = () => {};
    ${source}
    globalThis.out = { state, gates, asked, load, finishedTeams };`, context);
  const { state, gates, asked, load, finishedTeams } = context.out;
  finishedTeams.add("teams/a/team"); // the earlier team had finished; the new one has not
  const settle = () => new Promise((resolve) => setTimeout(resolve, 0));
  load(); // team a
  await settle();
  Object.assign(state, { generation: 2, loading: 0, team: "teams/b/team", loaded: false, info: null });
  load(); // team b, while a is in flight
  await settle();
  assert.ok(asked.includes("teams/b/team/team.json"), "the new team is read without waiting for the earlier load");
  gates.get("teams/b/team/team.json")({ lead: "b-lead" });
  await settle();
  assert.equal(state.loaded, true);
  assert.equal(state.info.lead, "b-lead");
  gates.get("teams/a/team/team.json")({ lead: "a-lead" });
  await settle();
  assert.equal(state.info.lead, "b-lead", "the earlier team's answer is dropped");
  assert.equal(state.finished, false);
  assert.equal(state.loading, 0);
});

test("underscores inside a word stay literal in chat and team messages; emphasis around words is kept", () => {
  const context = {};
  vm.runInNewContext(`${read("chat.js")}\nglobalThis.out = ChatLogic;`, context);
  const { maskWordUnderscores, unmaskUnderscores, UNDERSCORE_MARK, codeFile } = context.out;
  const text = "Use send_message or end the work with finish_team(...); see test_slugify.py and a__b, MAX_UPLOAD_BYTES, größe_1.";
  const masked = maskWordUnderscores(text);
  assert.equal(masked.includes("_"), false);
  assert.equal(masked.split(UNDERSCORE_MARK).length - 1, 8);
  assert.equal(unmaskUnderscores(masked), text);
  // Emphasis delimiters are not inside a word and stay as written.
  for (const kept of ["_emphasis_ and __strong__", "a _b_ c", "_x", "x_ y", "snake _case"]) assert.equal(maskWordUnderscores(kept), kept, kept);
  assert.equal(unmaskUnderscores(maskWordUnderscores("_file_name_")), "_file_name_");
  assert.equal(maskWordUnderscores("_file_name_"), `_file${UNDERSCORE_MARK}name_`);
  const chat = read("chat.js");
  assert.match(chat, /ChatLogic\.maskWordUnderscores\(ChatLogic\.maskTablePipes\(text\)\)/, "renderRich masks them");
  assert.doesNotMatch(read("app.js"), /UNDERSCORE_MARK|maskWordUnderscores/, "the shared renderMarkdown is unchanged");

  // Code files of a team workspace are shown as monospace preformatted text.
  for (const name of ["main.py", "main_test.go", "app.js", "schema.sql", "Makefile"]) assert.equal(codeFile(name), true, name);
  for (const name of ["notes.md", "a.txt", "data.json"]) assert.equal(codeFile(name), false, name);
  assert.match(chat, /ChatLogic\.codeFile\(t\.name\)\) fill\(t\.body, h\("div", \{ className: "file-block code source" \}, t\.text\)\)/);
});

test("a linked decision is shown or nothing is: no other item ever takes its place", () => {
  const decisions = read("decisions.js");
  const context = {};
  vm.runInNewContext(`${decisions.slice(decisions.indexOf("const DecisionsLogic = {"), decisions.indexOf("(() => {"))}\nglobalThis.out = DecisionsLogic;`, context);
  const { opened, select } = context.out;
  const linked = "p:711f/build|review", other = "p:3f60/build|review";
  // Not listed (yet): it stays selected, whatever else is listed and whichever lists have loaded.
  let s = select(opened(linked), false, undefined);
  assert.equal(s.selected, linked);
  s = select(s, false, other);
  assert.equal(s.selected, linked);
  s = select(s, false, "d:runs/t/review");
  assert.equal(s.selected, linked);
  // Once it is listed it is shown, beside another waiting gate of the same step.
  s = select(s, true, other);
  assert.deepEqual({ ...s }, { selected: linked, seen: linked, none: false });
  // A decided record likewise.
  assert.equal(select(opened("d:runs/t/review"), false, other).selected, "d:runs/t/review");
  // Only a bare #decisions selects the first item.
  assert.equal(select(opened(""), false, other).selected, other);
  assert.equal(select(opened(""), false, undefined).selected, "");
  assert.match(decisions, /const next = DecisionsLogic\.select\(state, listed, list\[0\]\?\.id\);/);
  assert.match(decisions, /Object\.assign\(state, DecisionsLogic\.opened\(decodeURIComponent\(path\.replace\(\/\^\\\/\+\/, ""\)\)\)\);/);
});

test("a gate rejected in this visit and linked again in its next round is the one shown, not another waiting gate", () => {
  const { opened, select } = decisionsLogic();
  const gate = "p:e46cd419|review", record = "d:runs/e46cd419/review.1", other = "p:3f6045c2/build|review";
  // 1. The gate is shown and rejected: the selection goes to the record of round 1.
  let s = select(opened(gate), true, gate);
  s = select({ ...s, selected: record }, true, other);
  assert.deepEqual({ ...s }, { selected: record, seen: record, none: false });
  // 2. The person leaves the page; the polls stop, so the waiting gates listed here go stale.
  // 3. A link opens the gate, waiting again in round 2, while another gate waits too. The stale
  //    list does not hold it yet: it stays selected (and the hash stays), the other gate is not taken.
  s = select(opened(gate), false, other);
  assert.deepEqual({ ...s }, { selected: gate, seen: "", none: false });
  s = select(s, false, other);
  assert.equal(s.selected, gate, "also at the next redraw before the poll answers");
  // 4. The poll lists round 2: the linked gate is shown.
  s = select(s, true, other);
  assert.deepEqual({ ...s }, { selected: gate, seen: gate, none: false });
  // Decided elsewhere afterwards: nothing is selected, and stays so at every poll.
  s = select(s, false, other);
  assert.deepEqual({ ...s }, { selected: "", seen: gate, none: true });
  assert.equal(select(s, false, other).selected, "");
  // A link to it once more is honoured again: selected while it does not wait, shown when it does.
  s = select(opened(gate), false, other);
  assert.equal(s.selected, gate);
  assert.equal(select(s, true, other).selected, gate);
});

test("a finished team that runs again is read at once, and a team that does not run only now and then", () => {
  const { reloadDue } = logic();
  assert.equal(reloadDue(false, true, true, 1), true, "it started running again (a rejection round)");
  assert.equal(reloadDue(true, true, true, 2), true, "it runs");
  assert.equal(reloadDue(true, false, true, 3), true, "it just stopped");
  assert.equal(reloadDue(false, false, false, 1), true, "it has not loaded yet");
  assert.deepEqual([1, 2, 3, 4, 5, 6, 10].map((t) => reloadDue(false, false, true, t)), [false, false, false, false, true, false, true]);
  // The page asks the orchestrator at every tick, whatever finished.json said.
  const tick = teams.slice(teams.indexOf("  async function tick() {"), teams.indexOf("  tabs.teams = {"));
  assert.match(tick, /await refreshTeams\(\)/);
  assert.doesNotMatch(tick, /state\.finished/);
  assert.match(tick, /visibilitychange/);
});

// ---- Regressions from the WebKit review ----

test("what a gate shows keeps underscores inside words, names the round of a decided record, and never wraps code", () => {
  const decisions = read("decisions.js"), chat = read("chat.js"), css = read("style.css");
  // Inputs, documents, and comments go through docView, which masks before rendering and unmasks what is shown.
  assert.doesNotMatch(decisions.replace(/function docView\(\) \{[\s\S]*?\n  \}\n/, ""), /textView\(/, "every text view of the page is a docView");
  assert.match(decisions, /view\.setText\(ChatLogic\.maskWordUnderscores\(text\)\); unmask\(\);/);
  assert.match(decisions, /view\.onMode\(unmask\)/);
  const context = {};
  vm.runInNewContext(`${chat}\nglobalThis.out = ChatLogic;`, context);
  const { maskWordUnderscores, unmaskUnderscores } = context.out;
  const input = "Write count_words(text) and test_wordcount.py; _really_ test it.";
  assert.equal(maskWordUnderscores(input).replace(/_really_/, "").includes("_"), false);
  assert.equal(unmaskUnderscores(maskWordUnderscores(input)), input);
  // Masked JSON is still JSON, so a JSON input is still pretty-printed.
  assert.deepEqual(JSON.parse(unmaskUnderscores(JSON.stringify(JSON.parse(maskWordUnderscores('{"file_name":"a_b.py"}'))))), { file_name: "a_b.py" });

  // The round of a decided record: from the record, else from its ID.
  const { roundLabel, roundOf } = logic();
  const label = (rec, id) => roundLabel(rec.round || roundOf(id).round);
  assert.equal(label({ round: 2 }, "runs/t/build/review"), "Round 2");
  assert.equal(label({}, "runs/t/build/review.3"), "Round 3");
  assert.equal(label({ round: 1 }, "runs/t/build/review"), "");
  assert.equal(label({}, "runs/t/build/review"), "");
  assert.match(decisions, /TeamsLogic\.roundLabel\(c\.kind === "pending" \? c\.d\.round : c\.rec\.round \|\| TeamsLogic\.roundOf\(c\.id\.slice\(2\)\)\.round\)/);

  // Code documents scroll sideways in their own box, in Decisions and in the file viewer.
  assert.match(css, /\.file-block\.code\.source \{ white-space: pre; overflow-wrap: normal; word-break: normal; overflow-x: auto;/);
  assert.match(decisions, /h\("pre", \{ className: "file-block code source" \}\)/);
  assert.match(chat, /className: "file-block code source" \}, t\.text\)/);
});

test("a team view asks for no completion record or chat logs, finds finished.json by listing, and shows the run's start time", () => {
  const chat = read("chat.js");
  assert.match(chat, /if \(o\.workspaceOnly\) return false;/);
  assert.match(chat, /o\.workspaceOnly \? \[\] : listObjects\(`\$\{session\}\/chats`\)/);
  assert.match(teams, /workspaceOnly: true/);
  assert.doesNotMatch(teams, /readJSON\(`\$\{team\}\/finished\.json`\)/);
  assert.match(teams, /time: \(\) => runIndex\.records\.get\(TeamsLogic\.traceOf\(team\)\)\?\.started \|\| ""/);
  assert.match(chat, /at \? \[sep\(\), timeEl\(at\)\] : null,/);
  const { hasFinished } = logic();
  assert.equal(hasFinished("teams/t/team", ["teams/t/team/team.json", "teams/t/team/finished.json"]), true);
  assert.equal(hasFinished("teams/t/team", ["teams/t/team/team.json"]), false);
  assert.equal(hasFinished("teams/t/team", ["teams/t/team2/finished.json"]), false);
  assert.equal(hasFinished("teams/t/team", undefined), false);
});

// ---- Regressions from the live-view review ----

function decisionsLogic() {
  const decisions = read("decisions.js");
  const context = {};
  vm.runInNewContext(`${decisions.slice(decisions.indexOf("const DecisionsLogic = {"), decisions.indexOf("(() => {"))}\nglobalThis.out = DecisionsLogic;`, context);
  return { ...context.out, decisions };
}

test("after a decision the selection goes to that decision's own record, never to another gate", () => {
  const { select, recordIds, decisions } = decisionsLogic();
  const gate = "runs/t/build/review";
  // A rejection that goes back to the team is the record of its round; the final record is the fallback
  // (the last rejection the orchestrator allows ends the run).
  assert.deepEqual(Array.from(recordIds(gate, 1, "rejected", true)), [`${gate}.1`, gate]);
  assert.deepEqual(Array.from(recordIds(gate, 3, "rejected", true)), [`${gate}.3`, gate]);
  assert.deepEqual(Array.from(recordIds(gate, undefined, "rejected", true)), [`${gate}.1`, gate]);
  // Every other decision is the gate's final record: never the Rejected record of an earlier round.
  assert.deepEqual(Array.from(recordIds(gate, 2, "approved", true)), [gate]);
  assert.deepEqual(Array.from(recordIds(gate, 1, "rejected", false)), [gate]);
  assert.deepEqual(Array.from(recordIds(gate, 1, "approved", false)), [gate]);

  // A selected item that was shown and is gone leaves nothing selected, whatever else waits.
  const other = "p:3f60/build|review", shown = { selected: "p:711f/build|review", seen: "p:711f/build|review", none: false };
  const gone = select(shown, false, other);
  assert.deepEqual({ ...gone }, { selected: "", seen: "p:711f/build|review", none: true });
  assert.equal(select(gone, false, other).selected, "", "and it stays empty at the next poll");
  assert.equal(select({ ...shown, none: true }, true, other).selected, shown.selected, "a listed item stays");

  // The page: the decided gate is listed at once from what was submitted and selected by its record ID.
  const decide = decisions.slice(decisions.indexOf("  async function decide("), decisions.indexOf("  // renderAll draws"));
  assert.match(decide, /DecisionsLogic\.recordIds\(/);
  assert.match(decide, /provisional: true/);
  assert.match(decide, /rec: \{ decision, comment, decided_by: by, decided_at: at,/);
  assert.match(decide, /if \(state\.selected === c\.id\) \{\s+state\.selected = id;/);
  assert.doesNotMatch(decisions, /state\.follow/, "no search for a record of the same step, which found an earlier round");
});

// Returning to the page (the window in front again, the polls running again) only redraws: it goes
// through select, never through opened, so these walk select the way the polls do.
test("a decision made while the page is away keeps the selection on its own record when the page is shown again", () => {
  const { opened, select, decisions } = decisionsLogic();
  const gate = "p:556fcbc0|review", record = "d:runs/556fcbc0/review.1", other = "p:e46cd419|review";
  let s = select(opened(gate), true, gate);
  // The decision is on its way: the gate stays listed, so a poll that no longer has it changes nothing.
  s = select(s, true, other);
  assert.deepEqual({ ...s }, { selected: gate, seen: gate, none: false });
  // The answer arrives: the selection goes to the record, listed from what was submitted.
  s = select({ ...s, selected: record }, true, other);
  assert.deepEqual({ ...s }, { selected: record, seen: record, none: false });
  // The page is shown again, with another gate waiting first, and at every poll after that.
  for (let poll = 0; poll < 3; poll++) s = select(s, true, other);
  assert.deepEqual({ ...s }, { selected: record, seen: record, none: false });

  // The page: a gate whose decision is on its way is not dropped by a poll, so decide still finds it selected.
  assert.match(decisions, /if \(!live\.has\(id\) && !deciding\.has\(roundKey\(c\.d\)\)\) pending\.delete\(id\);/);
  const decide = decisions.slice(decisions.indexOf("  async function decide("), decisions.indexOf("  // followRecord waits"));
  assert.match(decide, /if \(deciding\.has\(key\)\) return;\s+(\/\/.*\s+)*deciding\.add\(key\);\s+try \{\s+await api\("POST", API,/);
  assert.match(decide, /\} finally \{\s+deciding\.delete\(key\);/);
});

test("a page shown again with nothing selected selects nothing, with a stale list or while a different gate waits", () => {
  const { opened, select, decisions } = decisionsLogic();
  const gate = "p:556fcbc0|review", stale = "d:runs/e46cd419/review", fresh = "d:runs/556fcbc0/review.1", waiting = "p:e46cd419|review";
  // The gate was shown and left the list while the page was away (decided elsewhere).
  let s = select(select(opened(gate), true, gate), false, stale);
  assert.deepEqual({ ...s }, { selected: "", seen: gate, none: true });
  // Shown again with the stale list, then as the list catches up, then while a different gate waits first.
  for (const first of [stale, stale, fresh, waiting, waiting, undefined, waiting]) {
    s = select(s, false, first);
    assert.equal(s.selected, "", `nothing takes the empty selection (${first} is first)`);
  }
  // The same when nothing was ever selected by the hash but an item was shown and is gone.
  s = select(select(opened(""), false, stale), true, stale);
  assert.equal(s.selected, stale);
  s = select(s, false, waiting);
  assert.equal(select(s, false, waiting).selected, "");

  // The page: an emptied selection does not turn the hash into a bare #decisions, which would
  // select the first item when the page is loaded again; and nothing but the hash opens a selection.
  assert.match(decisions, /if \(moved && next\.selected && activeTab === "decisions"\) history\.replaceState\(null, "", hashFor\(\)\);/);
  assert.equal(decisions.match(/DecisionsLogic\.opened\(/g).length, 1, "only show(), which the hash drives, opens a selection");
  assert.doesNotMatch(decisions, /visibilitychange|pageshow|addEventListener\("focus"/);
});

test("approving round 2 of a gate whose round-1 rejection is already listed selects the approval, never the round-1 record", () => {
  const { opened, select, recordIds, decisions } = decisionsLogic();
  const artifact = "runs/556fcbc0/review", gate = "p:556fcbc0|review", first = `d:${artifact}.1`, approval = `d:${artifact}`;
  // A fresh page: the gate waits in round 2 and d:<artifact>.1 was read from the store.
  let s = select(opened(gate), true, gate);
  // The approval is recorded under the gate's own name only: no .n name is ever a candidate.
  const ids = Array.from(recordIds(artifact, 2, "approved", true));
  assert.deepEqual(ids, [artifact]);
  assert.equal(`d:${ids[0]}`, approval);
  // The gate leaves the list and the round-1 record is the first item until the approval is listed:
  // the selection is the approval at once, and stays there at every redraw after it.
  s = select({ ...s, selected: `d:${ids[0]}` }, true, first);
  assert.deepEqual({ ...s }, { selected: approval, seen: approval, none: false });
  for (const top of [first, approval, approval]) s = select(s, true, top);
  assert.equal(s.selected, approval);
  // Even while the approval is not listed (hidden by a filter, or its list reloads) the round-1 record is not taken.
  assert.equal(select({ ...select(opened(gate), true, gate), selected: approval }, false, first).selected, approval);
  assert.equal(select(s, false, first).selected, "");

  // The page: the selection after a decision is built from the submitted decision alone, and the
  // follow-up only looks at those names.
  const decide = decisions.slice(decisions.indexOf("  async function decide("), decisions.indexOf("  // renderAll draws"));
  assert.match(decide, /const ids = DecisionsLogic\.recordIds\(c\.d\.artifact \|\| `runs\/\$\{c\.trace\}\/\$\{c\.step\}`, c\.d\.round, decision, returns\(c\)\);\s+const id = `d:\$\{ids\[0\]\}`/);
  assert.match(decide, /state\.selected = id;/);
  assert.match(decide, /for \(const object of ids\) \{/);
  assert.equal(decide.match(/state\.selected = /g).length, 2, "the decision's record, and the same decision stored under its other name");
  assert.doesNotMatch(decide, /roundRecords|roundOf/);
});

test("Approve and Reject stay disabled, with the reason, until the gate's documents have loaded", () => {
  const { blocked, decisions } = decisionsLogic();
  assert.equal(blocked("", false), "Needs your name in the top bar");
  assert.equal(blocked("ada", false), "Needs the documents to load");
  assert.equal(blocked("ada", true), "");
  assert.match(decisions, /const blocked = DecisionsLogic\.blocked\(creator\(\), state\.ready === c\.id\);\s+approve\.disabled = !!blocked;/);
  assert.match(decisions, /const reason = blocked \|\| \(back && !comment\.value\.trim\(\) \? "Needs a reason" : ""\);\s+reject\.disabled = !!reason;/);
  // Ready only once every document settled, a team's result files included, and only for the detail that is drawn.
  assert.match(decisions, /box\.loaded = Promise\.all\(loads\);/);
  assert.match(decisions, /return Promise\.all\(documents\.map\(\(d\) => d\.loaded\)\);/);
  assert.match(decisions, /blocks\.loaded\.then\(\(\) => \{ if \(state\.blocks === blocks\) \{ state\.ready = c\.id; renderButtons\(\); \} \}\);/);
});

test("trace listings go through the shared queue on every page, and rows never ask for them", () => {
  const app = read("app.js"), decisions = read("decisions.js"), runs = read("runs.js"), memory = read("artifacts.js");
  const asked = (source) => (source.match(/\/api\/blobs\/trace\//g) || []).length;
  assert.equal(asked(app), 1, "only the queue's loader names the endpoint");
  assert.equal(asked(decisions) + asked(runs) + asked(memory) + asked(teams) + asked(read("chat.js")) + asked(read("sessions.js")), 0);
  assert.match(decisions, /traceListings\.ask\(trace, \{ owner: "decisions", final \}\)/);
  assert.match(memory, /traceListings\.ask\(trace, \{ owner: "artifacts" \}\)/);
  assert.match(runs, /runIndex\.status\(r\.trace_id, 2500, ""\)/, "a run row needs the status alone");
  assert.match(decisions, /runIndex\.status\(trace, 10000, ""\)/);
  assert.match(app, /traceListings\.show\(name\);/, "leaving a page drops what only it waited for");
  assert.match(decisions, /if \(state\.scanning\) return;/, "one scan at a time");
});

test("the team view claims no empty state before the team was read", () => {
  const chat = read("chat.js");
  assert.match(teams, /emptyTitle: \(\) => \(state\.loaded \? "No channels" : ""\)/);
  assert.match(teams, /state\.loaded && !list\.length \? emptyPanel\("No channels"\)/);
  assert.match(chat, /const title = typeof o\.emptyTitle === "function" \? o\.emptyTitle\(\) : o\.emptyTitle \|\| "No chats";/);
  assert.match(chat, /s\.started && !s\.tabs\.length && title \? emptyPanel\(title\) : null/);
  assert.match(teams, /state\.controller\?\.renderEmpty\(\);/);
});

// ---- Channels stored as one queue per sender ----

const at = (s) => `2026-01-01T00:00:${String(s).padStart(2, "0")}Z`;

test("the two queues of a channel are merged into one chat by the time sent, every message with its sender", () => {
  const { mergeQueues, channelLog, messages, channels } = logic();
  const lead = { channel: "coder--lead", from: "lead", to: "coder", messages: [{ seq: 1, text: "go", sent: at(1) }, { seq: 2, text: "and tests", sent: at(2) }, { seq: 3, text: "thanks", sent: at(9) }] };
  const coder = { channel: "coder--lead", from: "coder", to: "lead", messages: [{ seq: 2, text: "done", sent: at(7) }, { seq: 1, text: "on it", sent: at(3) }] };
  const chat = [
    { seq: 1, from: "lead", text: "go", sent: at(1) },
    { seq: 2, from: "lead", text: "and tests", sent: at(2) },
    { seq: 1, from: "coder", text: "on it", sent: at(3) },
    { seq: 2, from: "coder", text: "done", sent: at(7) },
    { seq: 3, from: "lead", text: "thanks", sent: at(9) },
  ];
  assert.deepEqual(plain(mergeQueues([coder, lead])), chat);
  assert.deepEqual(plain(mergeQueues([lead, coder])), chat);
  // Shown as one channel: named by its two members, with the merged count, in the merged order.
  const log = channelLog("coder--lead", [coder, lead]);
  assert.deepEqual(plain(messages(log)), chat, "seq counts per queue, so the merged order is kept");
  assert.deepEqual(plain(channels({ lead: "lead", channels: [{ id: "coder--lead", members: ["coder", "lead"] }] }, [log])), [{ id: "coder--lead", members: ["lead", "coder"], count: 5, label: "lead ↔ coder" }]);
  // Fractions of a second and time zones are times, not text.
  assert.deepEqual(plain(mergeQueues([
    { from: "a", messages: [{ seq: 1, text: "second", sent: "2026-01-01T01:00:00.5+01:00" }] },
    { from: "b", messages: [{ seq: 1, text: "first", sent: "2026-01-01T00:00:00.123456789Z" }] },
  ])).map((m) => m.text), ["first", "second"]);
});

test("messages sent at the same time keep their queue's order, the same way at every read", () => {
  const { mergeQueues } = logic();
  const a = { from: "a", messages: [{ seq: 1, text: "a1", sent: at(5) }, { seq: 2, text: "a2", sent: at(5) }, { seq: 3, text: "a3", sent: at(6) }] };
  const b = { from: "b", messages: [{ seq: 1, text: "b1", sent: at(5) }, { seq: 2, text: "b2", sent: at(5) }, { seq: 3, text: "b3", sent: at(6) }] };
  const texts = (list) => plain(list).map((m) => m.text);
  assert.deepEqual(texts(mergeQueues([a, b])), ["a1", "a2", "b1", "b2", "a3", "b3"]);
  assert.deepEqual(texts(mergeQueues([a, b])), texts(mergeQueues([a, b])));
  // A queue is never reordered: a clock that went back, or a missing time, leaves its seq order.
  const skew = { from: "c", messages: [{ seq: 1, text: "c1", sent: at(8) }, { seq: 2, text: "c2", sent: at(4) }, { seq: 3, text: "c3" }] };
  const merged = texts(mergeQueues([skew, a]));
  assert.deepEqual(merged.filter((t) => t.startsWith("c")), ["c1", "c2", "c3"]);
  assert.deepEqual(merged.filter((t) => t.startsWith("a")), ["a1", "a2", "a3"]);
});

test("a channel with one queue missing, or none yet, is still a channel", () => {
  const { mergeQueues, channelLog, channels, messages } = logic();
  const task = { channel: "lead--workflow", from: "workflow", to: "lead", messages: [{ seq: 1, text: "Build it", sent: at(1) }] };
  const log = channelLog("lead--workflow", [task]);
  assert.deepEqual(plain(messages(log)), [{ seq: 1, from: "workflow", text: "Build it", sent: at(1) }]);
  assert.deepEqual(plain(channels({ lead: "lead", channels: [] }, [log])), [{ id: "lead--workflow", members: ["workflow", "lead"], count: 1, label: "workflow ↔ lead" }]);
  // No queue yet: named by the channel's ID, with no messages.
  assert.deepEqual(plain(channels({ lead: "lead" }, [channelLog("coder--lead", [])])), [{ id: "coder--lead", members: ["lead", "coder"], count: 0, label: "lead ↔ coder" }]);
  assert.deepEqual(plain(mergeQueues([task, null, { from: "lead", messages: "broken" }])).length, 1);
  assert.deepEqual(plain(mergeQueues(undefined)), []);
});

test("a channel stored whole by an earlier run reads as before", () => {
  const { channelSources, messages, channels } = logic();
  const listing = { directories: [], objects: [{ id: "teams/t/team/channels/coder--lead.json", size: 120, created: "2026-01-01T00:00:00Z" }] };
  assert.deepEqual(plain(channelSources(listing)), { dirs: [], files: [{ id: "coder--lead", version: "2026-01-01T00:00:00Z|120", object: "teams/t/team/channels/coder--lead.json" }] });
  const log = { id: "coder--lead", members: ["coder", "lead"], messages: [{ seq: 2, from: "coder", text: "done", sent: at(2) }, { seq: 1, from: "lead", text: "go", sent: at(1) }] };
  assert.deepEqual(plain(messages(log)).map((m) => `${m.from}: ${m.text}`), ["lead: go", "coder: done"]);
  assert.equal(channels({ lead: "lead" }, [log])[0].count, 2);
});

test("a listing with both storage formats tells them apart, and a queue is read again only when it was written", () => {
  const { channelSources, queueSources } = logic();
  const listing = {
    directories: ["lead--workflow", "coder--lead/", "old--lead"],
    objects: [
      { id: "teams/t/team/channels/old--lead.json", size: 10, created: "c1" }, // also stored as queues: the queues are read
      { id: "teams/t/team/channels/lead--tester.json", size: 20, created: "c2" },
      { id: "teams/t/team/channels/notes.txt", size: 1, created: "c3" },
    ],
  };
  assert.deepEqual(plain(channelSources(listing)), {
    dirs: ["lead--workflow", "coder--lead", "old--lead"],
    files: [{ id: "lead--tester", version: "c2|20", object: "teams/t/team/channels/lead--tester.json" }],
  });
  assert.deepEqual(plain(channelSources(null)), { dirs: [], files: [] });
  const objects = [
    { id: "teams/t/team/channels/coder--lead/lead.json", size: 30, created: "c4" },
    { id: "teams/t/team/channels/coder--lead/coder.json", size: 40, created: "c5" },
    { id: "teams/t/team/channels/coder--lead/readme", size: 1, created: "c6" },
  ];
  assert.deepEqual(plain(queueSources("coder--lead", objects)), [
    { channel: "coder--lead", sender: "coder", version: "c5|40", object: "teams/t/team/channels/coder--lead/coder.json" },
    { channel: "coder--lead", sender: "lead", version: "c4|30", object: "teams/t/team/channels/coder--lead/lead.json" },
  ]);
  assert.deepEqual(plain(queueSources("coder--lead", undefined)), []);
});

test("the team view lists each channel directory once a tick and reads only the queues written since", async () => {
  const source = teams.slice(teams.indexOf("  async function load(ask = true) {"), teams.indexOf("  // tick asks the orchestrator"));
  const context = {};
  vm.runInNewContext(`
    ${teams.slice(0, teams.indexOf("// ---- Running teams"))}
    const T = "teams/t/team", store = new Map(), listed = [], readIds = [];
    const state = { generation: 1, team: T, loading: 0, loaded: false, logs: new Map(), docs: new Map(), info: null, finished: false, error: "", controller: null };
    const views = new Map();
    const refreshTeams = async () => {};
    const readJSON = async (id) => { readIds.push(id); return store.has(id) ? JSON.parse(store.get(id)) : null; };
    const listPrefix = async (prefix) => {
      listed.push(prefix);
      const directories = new Set(), objects = [];
      for (const [id, text] of store) {
        if (!id.startsWith(prefix + "/")) continue;
        const rest = id.slice(prefix.length + 1);
        if (rest.includes("/")) directories.add(rest.split("/")[0]); else objects.push({ id, size: text.length, created: "c" });
      }
      return { directories: [...directories], objects };
    };
    const listObjects = async (prefix) => (await listPrefix(prefix)).objects;
    const pool = async (items, limit, fn) => Promise.all(items.map(fn));
    const runIndex = { statuses: new Map(), status: async () => ({}) };
    const renderList = () => {}, renderView = () => {}, openWanted = () => {};
    ${source}
    globalThis.out = { state, store, listed, readIds, load, TeamsLogic };`, context);
  const { state, store, listed, readIds, load, TeamsLogic } = context.out;
  const T = "teams/t/team";
  const put = (id, doc) => store.set(`${T}/${id}`, JSON.stringify(doc));
  put("team.json", { lead: "lead", channels: [{ id: "coder--lead", members: ["coder", "lead"] }] });
  put("channels/coder--lead/lead.json", { channel: "coder--lead", from: "lead", to: "coder", messages: [{ seq: 1, text: "go", sent: at(1) }] });
  put("channels/lead--workflow/workflow.json", { channel: "lead--workflow", from: "workflow", to: "lead", messages: [{ seq: 1, text: "Build it", sent: at(0) }] });
  put("channels/lead--tester.json", { id: "lead--tester", members: ["lead", "tester"], messages: [{ seq: 1, from: "lead", text: "test", sent: at(2) }] });
  const shown = () => plain(TeamsLogic.channels(state.info, [...state.logs.values()].map((l) => l.log))).map((c) => `${c.label} ${c.count}`);
  const chat = (id) => plain(TeamsLogic.messages(state.logs.get(id).log)).map((m) => `${m.from}: ${m.text}`);

  await load(false);
  assert.equal(state.error, "");
  assert.deepEqual(shown(), ["workflow ↔ lead 1", "lead ↔ coder 1", "lead ↔ tester 1"]);
  assert.deepEqual(chat("lead--tester"), ["lead: test"]);

  // The other member answers: its queue appears, and only that queue is read.
  listed.length = readIds.length = 0;
  put("channels/coder--lead/coder.json", { channel: "coder--lead", from: "coder", to: "lead", messages: [{ seq: 1, text: "done", sent: at(5) }] });
  await load(false);
  assert.deepEqual(chat("coder--lead"), ["lead: go", "coder: done"]);
  assert.deepEqual(shown(), ["workflow ↔ lead 1", "lead ↔ coder 2", "lead ↔ tester 1"]);
  assert.deepEqual(plain(readIds).sort(), [`${T}/channels/coder--lead/coder.json`, `${T}/team.json`]);
  assert.deepEqual(plain(listed).sort(), [T, `${T}/channels`, `${T}/channels/coder--lead`, `${T}/channels/lead--workflow`], "one listing per channel directory");

  // Nothing written: nothing but team.json is read.
  listed.length = readIds.length = 0;
  await load(false);
  assert.deepEqual(plain(readIds), [`${T}/team.json`]);
  assert.deepEqual(chat("coder--lead"), ["lead: go", "coder: done"]);
});

test("a one-way channel is labelled sender → receiver", () => {
  const { channels, channelLabel } = logic();
  assert.equal(channelLabel({ id: "coder--lead", from: "coder" }, "lead"), "coder → lead");
  const listed = channels({ lead: "lead", channels: [{ id: "coder--lead", members: ["coder", "lead"], from: "lead" }, { id: "lead--tester", members: ["lead", "tester"] }] });
  assert.deepEqual([...listed.map((c) => c.label)], ["lead → coder", "lead ↔ tester"]);
});

test("a run's team steps are named from the definitions, nested ones by their path", () => {
  const { teamSteps } = logic();
  const defs = [
    { name: "outer", steps: [{ id: "prep", service: "agent" }, { id: "build", service: "workflow", workflow: "inner" }, { id: "crew", service: "team" }] },
    { name: "inner", steps: [{ id: "team", service: "team" }] },
  ];
  assert.deepEqual([...teamSteps(defs, "outer")], ["build/team", "crew"]);
  assert.deepEqual([...teamSteps(defs, "inner")], ["team"]);
  assert.deepEqual([...teamSteps(defs, "missing")], []);
  assert.deepEqual([...teamSteps(undefined, "outer")], []);
});

test("a nested workflow is found by its pinned reference: that version when it is known, else the newest", () => {
  const { defOf, teamSteps, stepAt, stepRows } = logic();
  // The newest definitions first, then the older versions a run used.
  const defs = [
    { name: "outer", version: 2, steps: [{ id: "build", service: "workflow", workflow: "inner@v1" }, { id: "docs", service: "workflow", workflow: "manual@v3" }] },
    { name: "inner", version: 2, steps: [{ id: "crew", service: "team" }, { id: "check", service: "gate" }] },
    { name: "manual", version: 3, steps: [{ id: "write", service: "agent" }] },
    { name: "inner", version: 1, steps: [{ id: "team", service: "team" }] },
    { name: "outer", version: 1, steps: [{ id: "old", service: "agent" }] },
  ];
  assert.equal(defOf(defs, "inner@v1"), defs[3]);
  assert.equal(defOf(defs, "inner@v2"), defs[1]);
  assert.equal(defOf(defs, "inner"), defs[1], "a bare name is the newest");
  assert.equal(defOf(defs, "inner@v7"), defs[1], "a version that is not loaded gives way to the newest");
  assert.equal(defOf(defs, "gone@v1"), undefined);
  assert.equal(defOf(undefined, "inner"), undefined);
  // The steps of a run are read from the versions it used.
  assert.deepEqual([...teamSteps(defs, "outer")], ["build/team"], "the pinned v1 of inner, not its newest");
  assert.deepEqual([...teamSteps(defs, "outer@v2")], ["build/team"]);
  assert.deepEqual([...teamSteps(defs, "outer@v1")], []);
  assert.equal(stepAt(defs, "outer", "build/team").service, "team");
  assert.equal(stepAt(defs, "outer", "build/crew"), null, "crew is a step of v2 only");
  assert.equal(stepAt(defs, "outer", "docs/write").service, "agent");
  assert.equal(stepAt(defs, "outer@v1", "old").service, "agent");
  assert.deepEqual(plain(stepRows(["build", "docs"], [], defs, "outer", true)), [
    { id: "build", depth: 0 }, { id: "build/team", depth: 1 }, { id: "docs", depth: 0 }, { id: "docs/write", depth: 1 },
  ]);
  assert.deepEqual(plain(stepRows(["old"], [], defs, "outer@v1", true)), [{ id: "old", depth: 0 }]);
});

test("a decision shows its gate's place in the workflow and says what Approve and Reject lead to", () => {
  const decisions = read("decisions.js");
  const context = {};
  vm.runInNewContext(`${decisions.slice(decisions.indexOf("const DecisionsLogic = {"), decisions.indexOf("(() => {"))}\nglobalThis.out = DecisionsLogic;`, context);
  const title = (id) => id.charAt(0).toUpperCase() + id.slice(1);
  const def = { steps: [{ id: "plan" }, { id: "accept", service: "gate", depends_on: ["plan"], on_reject: "plan" }, { id: "build", depends_on: ["accept"] }, { id: "docs", depends_on: ["accept"] }, { id: "end", service: "gate", depends_on: ["build"] }] };
  const accept = context.out.outcomes(def, "accept", title);
  assert.deepEqual([...accept.next], ["build", "docs"]);
  assert.equal(accept.approve, "Approve: Build and Docs run next.");
  assert.equal(accept.reject, "Reject: Plan runs again with your reason.");
  const end = context.out.outcomes(def, "end", title);
  assert.equal(end.approve, "Approve: the run completes.");
  assert.equal(end.reject, "Reject: the run ends.");
  assert.equal(context.out.outcomes(def, "gone", title), null);
  // Drawn read-only from the version the run used, under the question; never for a publish decision.
  assert.match(decisions, /if \(!publish\) gateMap\(c, \(box\) => detail\.querySelector\("\.panel-body > \.section"\)\?\.after\(box\)\);/);
  assert.match(decisions, /runWorkflow\(c\.workflow, version\),/);
  assert.match(decisions, /if \(out\.next\.includes\(s\.id\)\) g\.classList\.add\("next-approve"\);/);
});
