// End-to-end test of the running factory through the factory UI's API, with the factory's own test
// fixtures only (fixtures/factory: the group "test"). It starts its own runs, checks what the UI
// shows of them, and removes every run it started, also when a check fails. It never touches
// another workflow. preflight fails unless the factory is fully up.
import assert from "node:assert/strict";
import { fileURLToPath } from "node:url";
import path from "node:path";

const BASE = process.env.FACTORY_UI || "http://localhost:8082";
const CREATOR = "factory-test";

export async function api(method, route, body, base = BASE) {
  const res = await fetch(`${base}${route}`, {
    method, headers: { "Content-Type": "text/plain", "X-Creator": CREATOR }, body,
  });
  const text = await res.text();
  if (!res.ok) throw new Error(`${method} ${route}: ${res.status} ${text.slice(0, 200)}`);
  return text ? JSON.parse(text) : null;
}

const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

// preflight fails, naming what is missing, unless the factory is fully up: the UI answers, every
// deployment has all its replicas ready, and the test fixtures are published.
export async function preflight({ call = api, base = BASE } = {}) {
  try { await call("GET", "/healthz"); }
  catch (e) { throw new Error(`The factory UI at ${base} does not answer (${e.message}): start the factory with just start`); }
  const deployments = (await call("GET", "/api/cluster/deployments")).items || [];
  assert.ok(deployments.length > 0, "The cluster reports no deployments: start the factory with just start");
  const notReady = deployments.filter((d) => (d.status?.readyReplicas || 0) < (d.spec?.replicas ?? 1)).map((d) => `${d.metadata?.name} (${d.status?.readyReplicas || 0}/${d.spec?.replicas ?? 1})`);
  assert.deepEqual(notReady, [], `The factory is not fully up; not ready: ${notReady.join(", ")}`);
  const names = ((await call("GET", "/api/orchestrator/workflows")).workflows || []).map((w) => w.name);
  const missing = ["test/one-step", "test/waits-for-person"].filter((n) => !names.includes(n));
  assert.deepEqual(missing, [], `The test fixtures are not published (${missing.join(", ")}): run just deploy`);
}

// waitFor polls until check returns a value, or fails after timeoutMs with what it saw last.
async function waitFor(what, check, timeoutMs = 180000) {
  const until = Date.now() + timeoutMs;
  let last;
  while (Date.now() < until) {
    try { last = await check(); if (last?.done) return last; } catch (e) { last = e.message; }
    await sleep(2000);
  }
  throw new Error(`${what}: still ${JSON.stringify(last)} after ${timeoutMs / 1000} s`);
}

// sweep removes every run of a test workflow, however it was started (this check, a browser, a
// person), and fails unless none is left: a test never leaves a run behind.
export async function sweep({ call = api, log = console.log } = {}) {
  const traces = ((await call("GET", "/api/blobs/list/runs").catch((e) => { if (/404/.test(e.message)) return { directories: [] }; throw e; })).directories || []);
  const ofTest = async (trace) => {
    try { return String((await call("GET", `/api/blobs/blobs/runs/${trace}/run.json`))?.workflow || "").startsWith("test/"); } catch { return false; }
  };
  const found = [];
  for (const trace of traces) if (await ofTest(trace)) found.push(trace);
  for (const trace of found) { await call("DELETE", `/api/orchestrator/runs/${trace}`); log(`removed ${trace}`); }
  const left = [];
  for (const trace of ((await call("GET", "/api/blobs/list/runs")).directories || [])) if (await ofTest(trace)) left.push(trace);
  assert.deepEqual(left, [], `test runs are left behind: ${left.join(", ")}`);
  const sessions = ((await call("GET", "/api/orchestrator/sessions")).sessions || []).filter((s) => found.some((t) => String(s.session || "").includes(`/${t}/`)));
  assert.deepEqual(sessions, [], "a removed test run still has a waiting session");
  return found;
}

export async function run({ call = api, log = console.log } = {}) {
  const started = [];
  const start = async (workflow, input) => {
    const r = await call("POST", `/api/orchestrator/workflows/${encodeURIComponent(workflow)}/runs`, input);
    started.push(r.trace_id);
    log(`started ${workflow}: ${r.trace_id}`);
    return r.trace_id;
  };
  try {
    // The fixtures are there: published by just sync-seeds, never an operator seed.
    const names = ((await call("GET", "/api/orchestrator/workflows")).workflows || []).map((w) => w.name);
    for (const name of ["test/one-step", "test/waits-for-person"]) assert.ok(names.includes(name), `${name} is missing: run just deploy to publish the test fixtures`);

    // One agent step: the run completes and stores the step's output.
    const one = await start("test/one-step", "A cat who reviews pull requests.");
    const done = await waitFor("test/one-step", async () => {
      const st = await call("GET", `/api/orchestrator/runs/${one}`);
      return { done: st.status !== "Running", status: st.status, artifacts: st.artifacts };
    });
    assert.equal(done.status, "Completed", `test/one-step ended ${done.status}`);
    const listing = await call("GET", `/api/blobs/list/runs/${one}`);
    assert.ok((listing.objects || []).some((o) => o.id === `runs/${one}/answer`), "the answer step stored its output");
    log("test/one-step: Completed, answer stored");

    // An interactive step: the run waits for a person, with its session listed as waiting.
    const waits = await start("test/waits-for-person", "A cat who reviews pull requests.");
    const waiting = await waitFor("test/waits-for-person", async () => {
      const sessions = (await call("GET", "/api/orchestrator/sessions")).sessions || [];
      const mine = sessions.find((s) => String(s.session || "").startsWith(`user-sessions/${waits}/`));
      const st = await call("GET", `/api/orchestrator/runs/${waits}`);
      return { done: !!mine || st.status !== "Running", status: st.status, session: mine?.session || "" };
    });
    assert.equal(waiting.status, "Running", `test/waits-for-person ended ${waiting.status} instead of waiting`);
    assert.equal(waiting.session, `user-sessions/${waits}/talk`, "the talk step waits for a person");
    const answer = await call("GET", `/api/blobs/list/runs/${waits}`);
    assert.ok((answer.objects || []).some((o) => o.id === `runs/${waits}/answer`), "the step before the person finished first");
    log("test/waits-for-person: waits at talk after answer");
  } finally {
    // Tear down: every run this check started is removed, whatever happened above.
    for (const trace of started) {
      try { await call("DELETE", `/api/orchestrator/runs/${trace}`); log(`removed ${trace}`); }
      catch (e) { log(`could not remove ${trace}: ${e.message}`); }
    }
    // Then every other run of a test workflow, and the proof that none is left.
    await sweep({ call, log });
  }
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  const what = process.argv[2] || "";
  try {
    if (what === "preflight") { await preflight(); console.log("The factory is fully up."); }
    else if (what === "sweep") { await sweep(); console.log("No test run is left."); }
    else { await run(); console.log("End-to-end UI test passed; no test run is left."); }
  } catch (e) {
    // The reason alone, never a stack trace: it says what is wrong and what to do.
    console.error(`FAIL: ${e.message}`);
    process.exit(1);
  }
}
