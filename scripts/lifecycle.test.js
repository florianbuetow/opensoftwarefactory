import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { chmod, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const just = execFileSync("which", ["just"], { encoding: "utf8" }).trim();

async function fixture(options = {}) {
  const directory = await mkdtemp(path.join(tmpdir(), "factory-lifecycle-"));
  const bin = path.join(directory, "bin");
  const log = path.join(directory, "calls.log");
  const state = path.join(directory, "node-running");
  const runtime = path.join(directory, "runtime-running");
  await execFileSync("mkdir", ["-p", bin]);
  const command = `#!/bin/sh
set -eu
name=$(basename "$0")
printf '%s %s\\n' "$name" "$*" >> "$LIFECYCLE_LOG"
if [ "$name" = kubectl ]; then
  case " $* " in
    *" get nodes "*|*" get pods,services "*) [ "\${MOCK_API:-up}" = up ] || exit 1 ;;
  esac
  case " $* " in
    *" get deployment -o name "*) [ -n "\${MOCK_EXISTING_DEPLOYMENTS:-}" ] && printf '%s\\n' "$MOCK_EXISTING_DEPLOYMENTS" ;;
  esac
  exit 0
fi
case "$name:$1" in
  colima:start) : > "$LIFECYCLE_RUNTIME" ;;
  docker:info) [ -f "$LIFECYCLE_RUNTIME" ] && exit 0 || exit 1 ;;
  docker:ps)
    case " $* " in
      *" -a "*|*" -aq "*) { [ "\${MOCK_CLUSTER:-present}" = present ] || [ -f "$LIFECYCLE_CREATED" ]; } && printf 'node-id\\n' ;;
      *) [ -f "$LIFECYCLE_STATE" ] && printf 'node-id\\n' ;;
    esac ;;
  docker:start) : > "$LIFECYCLE_STATE" ;;
  docker:stop) rm -f "$LIFECYCLE_STATE" ;;
  docker:build) : > "$LIFECYCLE_BUILT" ;;
  docker:image)
    [ "\${MOCK_MISSING_IMAGE:-}" = "$3" ] && [ ! -f "$LIFECYCLE_BUILT" ] && exit 1 || exit 0 ;;
  kind:create) : > "$LIFECYCLE_CREATED"; : > "$LIFECYCLE_STATE" ;;
esac
exit 0
`;
  for (const name of ["colima", "docker", "kind", "kubectl", "node", "sleep"]) {
    const file = path.join(bin, name);
    await writeFile(file, command);
    await chmod(file, 0o755);
  }
  if (options.running) await writeFile(state, "running\n");
  if (!options.runtimeDown) await writeFile(runtime, "running\n");
  const env = {
    ...process.env,
    PATH: `${bin}:${process.env.PATH}`,
    LIFECYCLE_LOG: log,
    LIFECYCLE_STATE: state,
    LIFECYCLE_RUNTIME: runtime,
    LIFECYCLE_CREATED: path.join(directory, "cluster-created"),
    LIFECYCLE_BUILT: path.join(directory, "images-built"),
    MOCK_CLUSTER: options.cluster || "present",
    MOCK_MISSING_IMAGE: options.missingImage || "",
    MOCK_EXISTING_DEPLOYMENTS: options.existingDeployments || "",
    MOCK_API: options.apiDown ? "down" : "up",
  };
  const run = (target, succeeds = true) => {
    try {
      execFileSync(just, [target], { cwd: root, env, encoding: "utf8", stdio: "pipe" });
      assert.equal(succeeds, true, `${target} unexpectedly succeeded`);
    } catch (error) {
      if (succeeds) throw error;
      return `${error.stdout || ""}${error.stderr || ""}`;
    }
  };
  const calls = async () => { try { return await readFile(log, "utf8"); } catch { return ""; } };
  return { directory, run, calls };
}

test("cluster-create creates only the KIND cluster", async () => {
  const f = await fixture({ cluster: "missing" });
  try {
    f.run("cluster-create");
    const calls = await f.calls();
    assert.match(calls, /kind create cluster --name opensoftwarefactory/);
    assert.doesNotMatch(calls, /docker build|kind load|kubectl |node scripts\/init-/);
  } finally { await rm(f.directory, { recursive: true, force: true }); }
});

test("cluster-delete deletes only the project cluster", async () => {
  const f = await fixture();
  try {
    f.run("cluster-delete");
    const calls = await f.calls();
    assert.match(calls, /kind delete cluster --name opensoftwarefactory/);
    assert.doesNotMatch(calls, /docker |kubectl |node /);
  } finally { await rm(f.directory, { recursive: true, force: true }); }
});

test("start starts the existing node then deploys without building", async () => {
  const f = await fixture();
  try {
    f.run("start");
    const calls = await f.calls();
    assert.match(calls, /docker start node-id/);
    assert.match(calls, /kind load docker-image/);
    assert.match(calls, /kubectl --context kind-opensoftwarefactory apply -k infra\/kubernetes\/overlays\/local/);
    assert.doesNotMatch(calls, /docker build|kind create cluster|colima status|colima start/);
    assert.doesNotMatch(calls, /rollout restart deployment\/temporal/, "new deployments are not restarted during first initialization");
  } finally { await rm(f.directory, { recursive: true, force: true }); }
});

test("deploy restarts services that existed before apply", async () => {
  const f = await fixture({ running: true, existingDeployments: "deployment.apps/temporal" });
  try {
    f.run("deploy");
    assert.match(await f.calls(), /rollout restart deployment\/temporal/);
  } finally { await rm(f.directory, { recursive: true, force: true }); }
});

test("start initializes the configuration before anything else", async () => {
  const f = await fixture();
  try {
    f.run("start");
    const calls = await f.calls();
    assert.match(calls, /node scripts\/init-llm-gateway\.js/);
    assert.ok(calls.indexOf("node scripts/init-llm-gateway.js") < calls.indexOf("docker start node-id"));
  } finally { await rm(f.directory, { recursive: true, force: true }); }
});

test("start creates the cluster when it does not exist, then deploys", async () => {
  const f = await fixture({ cluster: "missing" });
  try {
    f.run("start");
    const calls = await f.calls();
    assert.match(calls, /kind create cluster --name opensoftwarefactory/);
    assert.ok(calls.indexOf("kind create cluster") < calls.indexOf("kind load docker-image"));
    assert.match(calls, /apply -k infra\/kubernetes\/overlays\/local/);
    assert.doesNotMatch(calls, /docker build/);
  } finally { await rm(f.directory, { recursive: true, force: true }); }
});

test("start builds the images when one is missing, then deploys", async () => {
  const f = await fixture({ missingImage: "opensoftwarefactory-agent:local" });
  try {
    f.run("start");
    const calls = await f.calls();
    assert.match(calls, /docker start node-id/);
    assert.match(calls, /docker build --tag opensoftwarefactory-agent:local services\/agent/);
    assert.ok(calls.indexOf("docker build") < calls.indexOf("kind load docker-image"));
    assert.doesNotMatch(calls, /kind create cluster/);
  } finally { await rm(f.directory, { recursive: true, force: true }); }
});

test("deploy fails before applying when a required local image is missing", async () => {
  const f = await fixture({ running: true, missingImage: "opensoftwarefactory-agent:local" });
  try {
    const output = f.run("deploy", false);
    assert.match(output, /Required images have not been built/);
    const calls = await f.calls();
    assert.doesNotMatch(calls, /kind load|kubectl apply|node /);
  } finally { await rm(f.directory, { recursive: true, force: true }); }
});

test("deploy fails when the existing cluster is not running", async () => {
  const f = await fixture();
  try {
    const output = f.run("deploy", false);
    assert.match(output, /is not running; run just start first/);
    assert.doesNotMatch(await f.calls(), /kind load|kubectl apply|docker build/);
  } finally { await rm(f.directory, { recursive: true, force: true }); }
});

test("stop touches only running nodes of the project cluster", async () => {
  const f = await fixture({ running: true });
  try {
    f.run("stop");
    const calls = await f.calls();
    assert.match(calls, /docker ps -q --filter label=io.x-k8s.kind.cluster=opensoftwarefactory/);
    assert.match(calls, /docker stop node-id/);
    assert.doesNotMatch(calls, /kubectl |kind |docker build|docker rm|colima /);
  } finally { await rm(f.directory, { recursive: true, force: true }); }
});

test("stop fails safely when the container runtime is unavailable", async () => {
  const f = await fixture({ runtimeDown: true });
  try {
    assert.match(f.run("stop", false), /Container runtime is unavailable; no cluster state was changed/);
    const calls = await f.calls();
    assert.match(calls, /^docker info$/m);
    assert.doesNotMatch(calls, /colima start|docker stop|kubectl |kind /);
  } finally { await rm(f.directory, { recursive: true, force: true }); }
});

function assertStatusReadOnly(calls) {
  assert.doesNotMatch(calls, /colima start|docker start|docker stop|docker build|kind create|kind delete|kind load|kubectl apply|kubectl delete|kubectl scale/);
}

test("status reports when the container runtime is unavailable without touching the cluster", async () => {
  const f = await fixture({ runtimeDown: true });
  try {
    assert.match(f.run("status", false), /Container runtime is unavailable; run just start/);
    assertStatusReadOnly(await f.calls());
  } finally { await rm(f.directory, { recursive: true, force: true }); }
});

test("status distinguishes a missing cluster from a stopped node", async () => {
  const missing = await fixture({ cluster: "missing" });
  const stopped = await fixture();
  try {
    assert.match(missing.run("status", false), /has not been created; run just cluster-create first/);
    assert.match(stopped.run("status", false), /is stopped; run just start/);
    assertStatusReadOnly(await missing.calls());
    assertStatusReadOnly(await stopped.calls());
  } finally {
    await rm(missing.directory, { recursive: true, force: true });
    await rm(stopped.directory, { recursive: true, force: true });
  }
});

test("status suppresses kubectl noise when the Kubernetes API is unreachable", async () => {
  const f = await fixture({ running: true, apiDown: true });
  try {
    const output = f.run("status", false);
    assert.match(output, /node is running but the Kubernetes API is unreachable/);
    assert.doesNotMatch(output, /memcache|connection refused|127\.0\.0\.1/);
    assertStatusReadOnly(await f.calls());
  } finally { await rm(f.directory, { recursive: true, force: true }); }
});

test("status reads project pods and services without lifecycle mutations", async () => {
  const f = await fixture({ running: true });
  try {
    f.run("status");
    const calls = await f.calls();
    assert.match(calls, /kubectl --context kind-opensoftwarefactory --namespace opensoftwarefactory --request-timeout=5s get pods,services/);
    assertStatusReadOnly(calls);
  } finally { await rm(f.directory, { recursive: true, force: true }); }
});

test("build builds the images and does nothing else", async () => {
  const f = await fixture({ running: true });
  try {
    f.run("build");
    const calls = await f.calls();
    assert.match(calls, /docker build --tag opensoftwarefactory-agent:local services\/agent/);
    assert.doesNotMatch(calls, /kind |kubectl |docker start|docker stop|node /);
  } finally { await rm(f.directory, { recursive: true, force: true }); }
});

test("update no longer exists", async () => {
  const f = await fixture({ running: true });
  try {
    f.run("update", false);
    assert.doesNotMatch(await f.calls(), /docker build|kind load|apply -k/);
  } finally { await rm(f.directory, { recursive: true, force: true }); }
});
