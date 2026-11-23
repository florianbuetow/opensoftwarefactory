import assert from "node:assert/strict";
import test from "node:test";
import { preflight, run, sweep } from "./e2e-ui.js";

// A fake factory: the test workflows exist, runs complete or wait at once. It starts with a test
// run someone started in a browser and a run of an operator's workflow.
function factory({ failOneStep = false, deleteFails = false } = {}) {
  const calls = [];
  const runs = new Map([["stray", "test/waits-for-person"], ["theirs", "default/workflow-1"]]);
  let next = 0;
  const call = async (method, route) => {
    calls.push(`${method} ${route}`);
    if (route === "/api/orchestrator/workflows") return { workflows: [{ name: "test/one-step" }, { name: "test/waits-for-person" }, { name: "default/workflow-1" }] };
    const start = /^\/api\/orchestrator\/workflows\/(.+)\/runs$/.exec(route);
    if (method === "POST" && start) { const trace = `t${++next}`; runs.set(trace, decodeURIComponent(start[1])); return { trace_id: trace }; }
    const status = /^\/api\/orchestrator\/runs\/(.+)$/.exec(route);
    if (method === "GET" && status) return { status: runs.get(status[1]) === "test/one-step" ? (failOneStep ? "Failed" : "Completed") : "Running" };
    if (method === "DELETE" && status) { if (!deleteFails) runs.delete(status[1]); return null; }
    if (route === "/api/orchestrator/sessions") return { sessions: [...runs].filter(([, w]) => w === "test/waits-for-person").map(([t]) => ({ session: `user-sessions/${t}/talk` })) };
    if (route === "/api/blobs/list/runs") return { directories: [...runs.keys()] };
    const record = /^\/api\/blobs\/blobs\/runs\/(.+)\/run\.json$/.exec(route);
    if (record) return { workflow: runs.get(record[1]) };
    const list = /^\/api\/blobs\/list\/runs\/(.+)$/.exec(route);
    if (list) return { objects: [{ id: `runs/${list[1]}/answer` }] };
    throw new Error(`unexpected ${method} ${route}`);
  };
  return { call, calls, runs };
}

test("the live check runs only the test workflows and leaves no test run behind", async () => {
  const f = factory();
  await run({ call: f.call, log: () => {} });
  const started = f.calls.filter((c) => c.startsWith("POST "));
  assert.deepEqual(started, ["POST /api/orchestrator/workflows/test%2Fone-step/runs", "POST /api/orchestrator/workflows/test%2Fwaits-for-person/runs"]);
  assert.deepEqual([...f.runs.keys()], ["theirs"], "its own runs and the stray test run are gone; the operator's run stays");
  assert.ok(!f.calls.some((c) => c.startsWith("POST ") && c.includes("default")), "it never starts another workflow");
  assert.ok(!f.calls.includes("DELETE /api/orchestrator/runs/theirs"), "it never removes another workflow's run");
});

test("a failing check still removes every test run", async () => {
  const f = factory({ failOneStep: true });
  await assert.rejects(run({ call: f.call, log: () => {} }), /test\/one-step ended Failed/);
  assert.deepEqual([...f.runs.keys()], ["theirs"]);
});

test("the sweep fails while a test run is still there", async () => {
  const f = factory({ deleteFails: true });
  await assert.rejects(sweep({ call: f.call, log: () => {} }), /test runs are left behind: stray/);
});

test("preflight fails unless the factory is fully up, naming what is missing", async () => {
  const up = (overrides = {}) => async (method, route) => {
    if (route in overrides) { const v = overrides[route]; if (v instanceof Error) throw v; return v; }
    if (route === "/healthz") return { status: "ok" };
    if (route === "/api/cluster/deployments") return { items: [{ metadata: { name: "orchestrator" }, spec: { replicas: 1 }, status: { readyReplicas: 1 } }] };
    if (route === "/api/orchestrator/workflows") return { workflows: [{ name: "test/one-step" }, { name: "test/waits-for-person" }] };
    throw new Error(`unexpected ${method} ${route}`);
  };
  await preflight({ call: up() });
  await assert.rejects(preflight({ call: up({ "/healthz": new Error("fetch failed") }) }), /does not answer \(fetch failed\): start the factory with just start/);
  await assert.rejects(preflight({ call: up({ "/api/cluster/deployments": { items: [] } }) }), /reports no deployments/);
  await assert.rejects(preflight({ call: up({ "/api/cluster/deployments": { items: [{ metadata: { name: "agent" }, spec: { replicas: 1 }, status: {} }] } }) }), /not fully up; not ready: agent \(0\/1\)/);
  await assert.rejects(preflight({ call: up({ "/api/orchestrator/workflows": { workflows: [{ name: "test/one-step" }] } }) }), /fixtures are not published \(test\/waits-for-person\): run just deploy/);
});
