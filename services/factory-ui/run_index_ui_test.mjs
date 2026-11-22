import assert from "node:assert/strict";
import fs from "node:fs";
import test from "node:test";
import vm from "node:vm";

const app = fs.readFileSync(new URL("web/app.js", import.meta.url), "utf8");
const helpers = app.slice(app.indexOf("async function pool"), app.indexOf("// Pending gates"));
const deferred = () => {
  let resolve, reject;
  const promise = new Promise((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
};
function setup({ api, loadBlob = async () => ({ text: "{}" }) }) {
  const context = { api, loadBlob };
  vm.runInNewContext(`${helpers}\nglobalThis.testRunIndex = runIndex;`, context);
  return context.testRunIndex;
}

test("refresh removes cached runs absent from a successful server listing", async () => {
  const index = setup({ api: async () => ({ directories: [] }) });
  index.records.set("gone", { trace_id: "gone" });
  index.statuses.set("gone", { status: "Completed" });
  await index.refresh();
  assert.equal(index.records.has("gone"), false);
  assert.equal(index.statuses.has("gone"), false);
});

test("a stale directory response overlapping removal is listed again", async () => {
  const first = deferred();
  let listings = 0, blobLoads = 0;
  const index = setup({
    api: async (_method, path) => {
      assert.equal(path, "/api/blobs/list/runs");
      listings++;
      return listings === 1 ? first.promise : { directories: [] };
    },
    loadBlob: async () => { blobLoads++; return { text: "{}" }; },
  });
  index.records.set("gone", { trace_id: "gone" });
  const refreshing = index.refresh();
  index.forget("gone");
  first.resolve({ directories: ["gone"] });
  await refreshing;
  assert.equal(listings, 2);
  assert.equal(blobLoads, 0);
  assert.equal(index.records.has("gone"), false);
});

test("a delayed failed record load cannot restore a locally removed run", async () => {
  const record = deferred(), loading = deferred();
  let fallbackRequests = 0;
  const index = setup({
    api: async (_method, path) => {
      if (path === "/api/blobs/list/runs") return { directories: ["gone"] };
      fallbackRequests++;
      throw new Error("gone");
    },
    loadBlob: async () => { loading.resolve(); return record.promise; },
  });
  const refreshing = index.refresh();
  await loading.promise;
  index.forget("gone");
  record.reject(new Error("not found"));
  await refreshing;
  assert.equal(fallbackRequests, 0);
  assert.equal(index.records.has("gone"), false);
  assert.equal(index.statuses.has("gone"), false);
});

test("a queued record load uses the listing-time generation", async () => {
  const traces = Array.from({ length: 9 }, (_, i) => `run-${i}`);
  const release = deferred(), started = deferred();
  const loads = [];
  const index = setup({
    api: async () => ({ directories: traces }),
    loadBlob: async (id) => {
      loads.push(id);
      if (loads.length === 8) started.resolve();
      await release.promise;
      return { text: "{}" };
    },
  });
  const refreshing = index.refresh();
  await started.promise;
  index.forget("run-8");
  release.resolve();
  await refreshing;
  assert.equal(loads.some((id) => id.includes("run-8")), false);
  assert.equal(index.records.has("run-8"), false);
});

test("a delayed status response cannot restore a locally removed status", async () => {
  const status = deferred(), requested = deferred();
  const index = setup({ api: async (_method, path) => {
    if (path.startsWith("/api/orchestrator/runs/")) { requested.resolve(); return status.promise; }
    return { objects: [] };
  } });
  const checking = index.status("gone");
  await requested.promise;
  index.forget("gone");
  status.resolve({ status: "Completed", artifacts: {} });
  await checking;
  assert.equal(index.statuses.has("gone"), false);
});

test("a failed server listing retains the last good run cache", async () => {
  const index = setup({ api: async () => { throw new Error("gateway unavailable"); } });
  index.records.set("known", { trace_id: "known" });
  index.statuses.set("known", { status: "Completed" });
  await assert.rejects(index.refresh(), /gateway unavailable/);
  assert.equal(index.records.has("known"), true);
  assert.equal(index.statuses.has("known"), true);
});

test("attempt records are trace metadata, not visible step artifacts", async () => {
  const index = setup({ api: async (_method, path) => {
    assert.equal(path, "/api/blobs/trace/run-1");
    return { objects: [
      { id: "runs/run-1/research.attempt-2.run.json", created: "2026-01-02" },
      { id: "runs/run-1/research.attempt-1.run.json", created: "2026-01-01" },
      { id: "runs/run-1/research.run.json", created: "2026-01-02" },
    ] };
  } });
  const out = await index.outputs("run-1");
  assert.equal(out.records.has("research"), true);
  assert.deepEqual(Array.from(out.attemptRecords.get("research")), [
    "runs/run-1/research.attempt-1.run.json",
    "runs/run-1/research.attempt-2.run.json",
  ]);
  assert.deepEqual(Object.keys(out.artifacts), []);
});

test("routed child execution records remain timeline metadata", async () => {
  const index = setup({ api: async () => ({ objects: [
    { id: "runs/run-1/route/research.attempt-2.run.json", created: "2026-01-02" },
    { id: "runs/run-1/route/research.attempt-1.run.json", created: "2026-01-01" },
    { id: "runs/run-1/route/research.run.json", created: "2026-01-02" },
    { id: "runs/run-1/route/research", created: "2026-01-02" },
  ] }) });
  const out = await index.outputs("run-1");
  assert.equal(out.records.has("route/research"), true);
  assert.deepEqual(Array.from(out.attemptRecords.get("route/research")), [
    "runs/run-1/route/research.attempt-1.run.json",
    "runs/run-1/route/research.attempt-2.run.json",
  ]);
  assert.equal(out.nested.has("route"), true);
  assert.equal("route/research.run.json" in out.artifacts, false);
});

// ---- The queue for trace listings ----

function queueSetup(limit = 2) {
  const context = { api: async () => ({}), loadBlob: async () => ({ text: "{}" }) };
  vm.runInNewContext(`${helpers}\nglobalThis.testQueue = traceQueue;`, context);
  const started = [], jobs = new Map();
  const queue = context.testQueue((key) => { started.push(key); const job = deferred(); jobs.set(key, job); return job.promise; }, limit);
  const tick = () => new Promise((done) => { setTimeout(done, 0); });
  return { queue, started, jobs, tick };
}

test("at most two trace listings are in flight, and one request per trace is shared", async () => {
  const { queue, started, jobs, tick } = queueSetup();
  const a = queue.ask("a"), again = queue.ask("a"), b = queue.ask("b"), c = queue.ask("c"), cAgain = queue.ask("c");
  await tick();
  assert.deepEqual(started, ["a", "b"]);
  assert.equal(a, again, "in flight: shared");
  assert.equal(c, cAgain, "waiting: shared");
  assert.deepEqual({ ...queue.size() }, { flying: 2, waiting: 1, kept: 0 });
  jobs.get("a").resolve({ objects: [1] });
  assert.deepEqual(await a, { objects: [1] });
  await tick();
  assert.deepEqual(started, ["a", "b", "c"]);
  jobs.get("b").reject(new Error("gateway"));
  await assert.rejects(b, /gateway/);
  jobs.get("c").resolve({});
  await c;
  await tick();
  assert.deepEqual({ ...queue.size() }, { flying: 0, waiting: 0, kept: 0 });
});

test("the listing asked for last is served first, and one asked again moves to the front", async () => {
  const { queue, started, jobs, tick } = queueSetup();
  for (const key of ["a", "b", "row-1", "row-2", "row-3", "opened"]) queue.ask(key);
  queue.ask("row-1"); // the person opened this run
  await tick();
  jobs.get("a").resolve({});
  await tick();
  jobs.get("b").resolve({});
  await tick();
  assert.deepEqual(started, ["a", "b", "row-1", "opened"]);
  jobs.get("row-1").resolve({});
  jobs.get("opened").resolve({});
  await tick();
  assert.deepEqual(started.slice(4), ["row-3", "row-2"]);
});

test("the shown page goes first, and what only a page left behind waits for is dropped", async () => {
  const { queue, started, jobs, tick } = queueSetup();
  queue.show("runs");
  queue.ask("a", { owner: "runs" });
  queue.ask("b", { owner: "runs" });
  const mine = queue.ask("shown", { owner: "runs" });
  queue.ask("background", { owner: "decisions" }); // asked later by a page that is not shown
  await tick();
  jobs.get("a").resolve({});
  await tick();
  assert.deepEqual(started, ["a", "b", "shown"], "the shown page's request before a later one of another page");

  const left = queue.ask("left", { owner: "runs" });
  const both = queue.ask("both", { owner: "runs" });
  queue.ask("both", { owner: "decisions" });
  queue.show("decisions");
  await assert.rejects(left, (e) => e.dropped === true);
  assert.deepEqual({ ...queue.size() }, { flying: 2, waiting: 2, kept: 0 }, "background and both stay");
  jobs.get("b").resolve({});
  jobs.get("shown").resolve({});
  await mine;
  await tick();
  assert.deepEqual(started.slice(3).sort(), ["background", "both"]);
  jobs.get("both").resolve({ objects: [] });
  assert.deepEqual(await both, { objects: [] });
});

test("the listing of a finished run is kept and never asked for again", async () => {
  const { queue, started, jobs, tick } = queueSetup();
  const first = queue.ask("done", { final: true });
  await tick();
  jobs.get("done").resolve({ objects: ["x"] });
  await first;
  assert.deepEqual(await queue.ask("done"), { objects: ["x"] });
  assert.deepEqual(await queue.ask("done", { fresh: true }), { objects: ["x"] });
  assert.deepEqual(started, ["done"]);
  // A running run is asked again each time.
  const running = queue.ask("running");
  await tick();
  jobs.get("running").resolve({});
  await running;
  queue.ask("running");
  await tick();
  assert.deepEqual(started, ["done", "running", "running"]);
  // A removed run is no longer kept.
  queue.forget("done");
  queue.ask("done");
  await tick();
  assert.equal(started.filter((k) => k === "done").length, 2);
});

test("a listing that started before the run finished is not taken for the final one", async () => {
  const { queue, started, jobs, tick } = queueSetup();
  const early = queue.ask("run"); // asked while the run was running
  await tick();
  const final = queue.ask("run", { final: true });
  const fresh = queue.ask("run", { fresh: true });
  assert.notEqual(final, early);
  jobs.get("run").resolve({ objects: ["old"] });
  assert.deepEqual(await early, { objects: ["old"] });
  await tick();
  assert.deepEqual(started, ["run", "run"], "asked once more, shared by both");
  jobs.get("run").resolve({ objects: ["old", "last"] });
  assert.deepEqual(await final, { objects: ["old", "last"] });
  assert.deepEqual(await fresh, { objects: ["old", "last"] });
  assert.deepEqual(await queue.ask("run"), { objects: ["old", "last"] });
  assert.equal(started.length, 2, "and that one is kept");
});

test("a row's status never asks for the trace listing; the opened run is listed once when finished", async () => {
  const asked = [];
  let status = "Running";
  const index = setup({ api: async (_method, path) => {
    asked.push(path);
    if (path.startsWith("/api/orchestrator/runs/")) return { status, artifacts: {} };
    return { objects: [{ id: "runs/run-1/build", created: "2026-01-01" }] };
  } });
  const listings = () => asked.filter((p) => p.startsWith("/api/blobs/trace/")).length;
  const row = await index.status("run-1", 0, "");
  assert.equal(listings(), 0);
  assert.equal(row.status, "Running");
  assert.equal(index.statuses.get("run-1").status, "Running", "the row's status is cached without the listing");
  // The detail lists the run; a running one again at the next poll.
  assert.deepEqual(Object.keys((await index.status("run-1", 0)).artifacts), ["build"]);
  await index.status("run-1", 0);
  assert.equal(listings(), 2);
  // Finished: the row learns it first, the detail then lists it once more, and never again.
  status = "Completed";
  await index.status("run-1", 0, "");
  assert.equal(listings(), 2);
  const done = await index.status("run-1", 0);
  assert.equal(listings(), 3);
  assert.deepEqual(Array.from(done.order), ["build"]);
  await index.status("run-1", 0);
  await index.status("run-1", 0, "");
  assert.equal(listings(), 3);
  assert.equal(asked.filter((p) => p.startsWith("/api/orchestrator/runs/")).length, 4);
});

test("a listing that fails is asked again instead of being cached as a run without steps", async () => {
  let fail = true, listings = 0;
  const index = setup({ api: async (_method, path) => {
    if (path.startsWith("/api/orchestrator/runs/")) return { status: "Completed", artifacts: {} };
    listings++;
    if (fail) throw new Error("gateway");
    return { objects: [{ id: "runs/run-1/build", created: "2026-01-01" }] };
  } });
  assert.deepEqual(Object.keys((await index.status("run-1")).artifacts), []);
  fail = false;
  assert.deepEqual(Object.keys((await index.status("run-1")).artifacts), ["build"]);
  await index.status("run-1");
  assert.equal(listings, 2);
});

test("the run metadata is noted, never listed as a step", async () => {
  const index = setup({ api: async () => ({ objects: [
    { id: "runs/run-1/metadata.json", created: "2026-01-01" },
    { id: "runs/run-1/run.json", created: "2026-01-01" },
    { id: "runs/run-1/input", created: "2026-01-01" },
    { id: "runs/run-1/research", created: "2026-01-02" },
  ] }) });
  const out = await index.outputs("run-1");
  assert.deepEqual(Object.keys(out.artifacts), ["research"]);
  assert.deepEqual(Array.from(out.order), ["research"]);
  assert.equal(out.metadata, true);
  const older = setup({ api: async () => ({ objects: [{ id: "runs/run-2/research", created: "2026-01-02" }] }) });
  assert.equal((await older.outputs("run-2")).metadata, false, "a run from before the metadata");
});

// A step after a merge or batch node runs once per message the node forwards.
test("the objects of a pass belong to their step and pass; they are no steps of their own", async () => {
  const index = setup({ api: async () => ({ objects: [
    { id: "runs/run-1/intake", created: "2026-01-01" },
    { id: "runs/run-1/compare.pass-2", created: "2026-01-04" },
    { id: "runs/run-1/compare.pass-1", created: "2026-01-02" },
    { id: "runs/run-1/compare.pass-1.run.json", created: "2026-01-02" },
    { id: "runs/run-1/compare.pass-2.run.json", created: "2026-01-04" },
    { id: "runs/run-1/compare.pass-2.attempt-2.run.json", created: "2026-01-04" },
    { id: "runs/run-1/compare.pass-2.attempt-1.run.json", created: "2026-01-03" },
    { id: "runs/run-1/compare.pass-3.run.json", created: "2026-01-05" },
    { id: "runs/run-1/review.1", created: "2026-01-06" },
  ] }) });
  const out = await index.outputs("run-1");
  assert.deepEqual(Object.keys(out.artifacts).sort(), ["compare", "intake", "review.1"], "compare.pass-2 is not a step");
  assert.equal(out.artifacts.compare, "runs/run-1/compare.pass-2", "the step shows the output of its newest pass");
  assert.deepEqual(Array.from(out.order), ["intake", "compare", "review.1"], "ordered by its first pass");
  assert.deepEqual(JSON.parse(JSON.stringify(out.passes)), { compare: [
    { pass: 1, artifact: "runs/run-1/compare.pass-1", record: "compare.pass-1" },
    { pass: 2, artifact: "runs/run-1/compare.pass-2", record: "compare.pass-2" },
    { pass: 3, artifact: "", record: "compare.pass-3" },
  ] });
  // Records keep the pass in their key, so each pass has its own execution record and attempts.
  assert.deepEqual([...out.records].sort(), ["compare.pass-1", "compare.pass-2", "compare.pass-3"]);
  assert.deepEqual(Array.from(out.attemptRecords.get("compare.pass-2")), [
    "runs/run-1/compare.pass-2.attempt-1.run.json", "runs/run-1/compare.pass-2.attempt-2.run.json",
  ]);
});

test("a pass of a nested step is attributed to its path under the workflow node", async () => {
  const index = setup({ api: async () => ({ objects: [
    { id: "runs/run-1/build/write.pass-1", created: "2026-01-02" },
    { id: "runs/run-1/build/write.pass-2", created: "2026-01-03" },
    { id: "runs/run-1/build/write.pass-2.run.json", created: "2026-01-03" },
    { id: "runs/run-1/build/plan", created: "2026-01-01" },
  ] }) });
  const out = await index.outputs("run-1");
  assert.deepEqual(Object.keys(out.artifacts), []);
  assert.deepEqual(JSON.parse(JSON.stringify(out.nestedArtifacts)), { "build/plan": "runs/run-1/build/plan", "build/write": "runs/run-1/build/write.pass-2" });
  assert.deepEqual(Array.from(out.passes["build/write"], (p) => p.pass), [1, 2]);
  assert.equal(out.passes["build/write"][1].record, "build/write.pass-2");
  assert.deepEqual([...out.nested], ["build"]);
  const status = await index.status("run-1");
  assert.equal(status.passes["build/write"].length, 2, "the status carries the passes");
});
