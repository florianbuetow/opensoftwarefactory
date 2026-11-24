import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { chmod, cp, mkdir, mkdtemp, readFile, rm, stat, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

const sourceRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const just = execFileSync("which", ["just"], { encoding: "utf8" }).trim();

async function fixture(options = {}) {
  const root = await mkdtemp(path.join(tmpdir(), "factory-auxiliary-"));
  const bin = path.join(root, "bin");
  const log = path.join(root, "calls.log");
  await mkdir(bin);
  await cp(path.join(sourceRoot, "justfile"), path.join(root, "justfile"));
  for (const directory of [
    "scripts", "infra/kubernetes/overlays/local", "services/factory-ui/web",
    "services/blobstore/data/objects", "services/blobstore/data/meta",
    "services/openobserve/data",
  ]) await mkdir(path.join(root, directory), { recursive: true });
  await writeFile(path.join(root, "services/factory-ui/web/app.js"), "'use strict';\n");
  await writeFile(path.join(root, "scripts/ci-go.sh"), '#!/bin/sh\nprintf "ci-go.sh %s\\n" "$*" >> "$AUX_LOG"\n');
  await chmod(path.join(root, "scripts/ci-go.sh"), 0o755);
  await writeFile(path.join(root, "scripts/ci-python.sh"), '#!/bin/sh\nprintf "ci-python.sh %s\\n" "$*" >> "$AUX_LOG"\n');
  await chmod(path.join(root, "scripts/ci-python.sh"), 0o755);
  for (const module of ["llm-gateway", "blobstore", "mcp-gateway", "orchestrator", "registry", "evaluation", "agent", "factory-ui"])
    await mkdir(path.join(root, "services", module), { recursive: true });

  const command = `#!/bin/sh
set -eu
name=$(basename "$0")
printf '%s %s\\n' "$name" "$*" >> "$AUX_LOG"
case "$name" in
  curl) [ "\${MOCK_CURL:-ok}" = ok ] ;;
  open) [ "\${MOCK_OPEN:-ok}" = ok ] ;;
  kubectl)
    case " $* " in
      *" get namespace "*) [ "\${MOCK_API:-up}" = up ] ;;
      *" get deployments "*) printf 'factory-ui=1\\nagent=0\\n' ;;
      *" get services "*) printf 'factory-ui=8080\\n' ;;
      *" get deployment temporal "*) printf '%s\\n' "\${MOCK_TEMPORAL_REPLICAS:-1}" ;;
      *" get deployment blobstore "*) printf '%s\\n' "\${MOCK_BLOBSTORE_REPLICAS:-0}" ;;
      *" get deployment orchestrator "*) printf '1\\n' ;;
      *" get deployment agent-registry "*) printf '1\\n' ;;
      *" get deployment policy-registry "*) printf '1\\n' ;;
      *" port-forward "*) [ "\${MOCK_FORWARD:-ok}" = ok ] ;;
      *" kustomize "*) printf 'apiVersion: v1\\nkind: List\\n' ;;
    esac ;;
  docker)
    case "$1" in
      info) [ "\${MOCK_RUNTIME:-up}" = up ] ;;
      ps) [ "\${MOCK_NODE:-stopped}" = running ] && printf 'node-id\\n' ;;
    esac ;;
esac
exit 0
`;
  for (const name of ["clear", "colima", "curl", "docker", "gofmt", "go", "kind", "kubectl", "node", "open", "uv"]) {
    const file = path.join(bin, name);
    await writeFile(file, command);
    await chmod(file, 0o755);
  }
  const env = {
    ...process.env,
    PATH: `${bin}:${process.env.PATH}`,
    AUX_LOG: log,
    MOCK_API: options.api || "up",
    MOCK_BLOBSTORE_REPLICAS: String(options.blobstoreReplicas ?? 0),
    MOCK_CURL: options.curl || "ok",
    MOCK_FORWARD: options.forward || "ok",
    MOCK_NODE: options.node || "stopped",
    MOCK_OPEN: options.open || "ok",
    MOCK_RUNTIME: options.runtime || "up",
    MOCK_TEMPORAL_REPLICAS: String(options.temporalReplicas ?? 1),
  };
  const run = (args, succeeds = true) => {
    try {
      const output = execFileSync(just, args, { cwd: root, env, encoding: "utf8", stdio: "pipe" });
      assert.equal(succeeds, true, `${args.join(" ")} unexpectedly succeeded`);
      return output;
    } catch (error) {
      if (succeeds) throw error;
      return `${error.stdout || ""}${error.stderr || ""}`;
    }
  };
  const calls = async () => { try { return await readFile(log, "utf8"); } catch { return ""; } };
  return { calls, env, root, run };
}

test("init runs check, invokes each idempotent configuration generator, and initializes CI tools", async () => {
  const f = await fixture();
  try {
    const out = f.run(["init"]);
    assert.match(out, /Required tools available/);
    assert.match(out, /Local configuration initialized/);
    const calls = await f.calls();
    for (const script of ["init-llm-gateway.js", "init-openobserve.js", "init-orchestrator.js", "generate-kind-config.js"])
      assert.match(calls, new RegExp(`node scripts/${script.replaceAll(".", "\\.")}`));
    assert.match(calls, /ci-go\.sh init/);
    assert.match(calls, /uv sync --project config\/ci\/python --locked/);
  } finally { await rm(f.root, { recursive: true, force: true }); }
});

test("help lists every public target and does not mutate state", async () => {
  const f = await fixture();
  try {
    const output = f.run(["help"]);
    for (const target of ["cluster-create", "cluster-delete", "start", "stop", "build", "deploy", "update-ui", "reset", "reset-observe", "status", "info", "ui", "gateway", "observe", "temporal", "render", "check"])
      assert.match(output, new RegExp(`just ${target}\\b`));
    assert.doesNotMatch(await f.calls(), /docker |kind |kubectl |node /);
  } finally { await rm(f.root, { recursive: true, force: true }); }
});

test("render uses only the local Kustomize source", async () => {
  const f = await fixture();
  try {
    const output = f.run(["render"]);
    assert.match(output, /kind: List/);
    assert.match(await f.calls(), /kubectl kustomize infra\/kubernetes\/overlays\/local/);
  } finally { await rm(f.root, { recursive: true, force: true }); }
});

test("check verifies required workstation and CI tools", async () => {
  const f = await fixture();
  try {
    assert.match(f.run(["check"]), /Required tools available/);
  } finally { await rm(f.root, { recursive: true, force: true }); }
});

test("test invokes JS/UI, manifest, Go, and Python suites", async () => {
  const f = await fixture();
  try {
    f.run(["test"]);
    const calls = await f.calls();
    assert.match(calls, /node --test .*auxiliary-targets\.test\.js/);
    assert.match(calls, /kubectl kustomize/);
    assert.match(calls, /ci-go\.sh test/);
    assert.match(calls, /ci-python\.sh test/);
  } finally { await rm(f.root, { recursive: true, force: true }); }
});

for (const [target, title, url] of [
  ["ui", "factory UI", "http://localhost:8082"],
  ["observe", "OpenObserve", "http://localhost:5080"],
  ["temporal", "Temporal UI", "http://localhost:8233"],
]) {
  test(`${target} checks reachability and opens the exact URL`, async () => {
    const f = await fixture();
    try {
      const output = f.run([target]);
      assert.match(output, new RegExp(`${title} is reachable`, "i"));
      const calls = await f.calls();
      assert.match(calls, new RegExp(`curl -sf -m 3 ${url.replaceAll("/", "\\/")}`));
      assert.match(calls, new RegExp(`open ${url.replaceAll("/", "\\/")}`));
    } finally { await rm(f.root, { recursive: true, force: true }); }
  });
  test(`${target} fails when its URL is unreachable`, async () => {
    const f = await fixture({ curl: "fail" });
    try {
      assert.match(f.run([target], false), new RegExp(`${title} is not reachable`, "i"));
      assert.doesNotMatch(await f.calls(), /^open /m);
    } finally { await rm(f.root, { recursive: true, force: true }); }
  });
  test(`${target} fails instead of claiming success when the browser cannot open`, async () => {
    const f = await fixture({ open: "fail" });
    try {
      assert.match(f.run([target], false), /could not open|cannot open|failed to open/i);
    } finally { await rm(f.root, { recursive: true, force: true }); }
  });
}

test("gateway forwards the exact service port and propagates failure", async () => {
  const ok = await fixture({ node: "running" });
  const bad = await fixture({ node: "running", forward: "fail" });
  try {
    assert.match(ok.run(["gateway"]), /Gateway forwarding stopped/);
    assert.match(await ok.calls(), /kubectl --context kind-opensoftwarefactory --namespace opensoftwarefactory port-forward service\/llm-gateway 8081:8080/);
    assert.match(bad.run(["gateway"], false), /gateway failed/);
  } finally {
    await rm(ok.root, { recursive: true, force: true });
    await rm(bad.root, { recursive: true, force: true });
  }
});

test("update-ui deploys the Git route without restarting other services", async () => {
  const f = await fixture();
  try {
    f.run(["update-ui"]);
    const calls = await f.calls();
    assert.match(calls, /docker build --tag opensoftwarefactory-factory-ui:local services\/factory-ui/);
    assert.match(calls, /kind load docker-image opensoftwarefactory-factory-ui:local --name opensoftwarefactory/);
    assert.match(calls, /kubectl .* apply -f infra\/kubernetes\/base\/git-host\/service.yaml/);
    assert.match(calls, /kubectl .* rollout restart deployment\/factory-ui/);
    assert.doesNotMatch(calls, /rollout restart deployment\/(?!factory-ui)/);
  } finally { await rm(f.root, { recursive: true, force: true }); }
});

test("reset refuses to partially delete data when Temporal history is unreachable", async () => {
  const f = await fixture({ api: "down", node: "running" });
  const object = path.join(f.root, "services/blobstore/data/objects/keep");
  try {
    await writeFile(object, "keep");
    assert.match(f.run(["reset"], false), /cluster.*reachable|cannot reset/i);
    assert.equal(await readFile(object, "utf8"), "keep");
  } finally { await rm(f.root, { recursive: true, force: true }); }
});

test("reset deletes disposable factory data and restores original replica counts", async () => {
  const f = await fixture({ node: "running", temporalReplicas: 1, blobstoreReplicas: 0 });
  const object = path.join(f.root, "services/blobstore/data/objects/workflows/remove");
  try {
    await mkdir(path.dirname(object), { recursive: true });
    await writeFile(object, "remove");
    f.run(["reset"]);
    await assert.rejects(readFile(object), { code: "ENOENT" });
    const calls = await f.calls();
    assert.match(calls, /docker exec opensoftwarefactory-control-plane find \/var\/lib\/opensoftwarefactory\/temporal -mindepth 1 -delete/);
    assert.match(calls, /scale deployment temporal --replicas=1/);
    assert.match(calls, /scale deployment blobstore --replicas=0/);
  } finally { await rm(f.root, { recursive: true, force: true }); }
});

test("reset-observe clears hidden data while preserving credentials", async () => {
  const f = await fixture({ node: "running" });
  const visible = path.join(f.root, "services/openobserve/data/visible");
  const hidden = path.join(f.root, "services/openobserve/data/.hidden");
  const config = path.join(f.root, "services/openobserve/config/openobserve.env");
  try {
    await mkdir(path.dirname(config), { recursive: true });
    await writeFile(config, "credential=keep");
    await writeFile(visible, "remove");
    await writeFile(hidden, "remove");
    const mounted = (await stat(path.dirname(visible))).ino;
    f.run(["reset-observe"]);
    // The KIND node bind-mounts this directory; a recreated one would be invisible to it.
    assert.equal((await stat(path.dirname(visible))).ino, mounted);
    await assert.rejects(readFile(visible), { code: "ENOENT" });
    await assert.rejects(readFile(hidden), { code: "ENOENT" });
    assert.equal(await readFile(config, "utf8"), "credential=keep");
  } finally { await rm(f.root, { recursive: true, force: true }); }
});

test("default target shows help without starting services", async () => {
  const f = await fixture();
  try {
    assert.match(f.run([]), /Available commands shown/);
    assert.doesNotMatch(await f.calls(), /docker |kind |kubectl |node /);
  } finally { await rm(f.root, { recursive: true, force: true }); }
});

test("info reads readiness and service URLs without changing state", async () => {
  const f = await fixture();
  try {
    const out = f.run(["info"]);
    assert.match(out, /factory-ui\s+factory-ui:8080/);
    assert.match(out, /http:\/\/localhost:8082/);
    assert.doesNotMatch(await f.calls(), /docker start|docker stop|apply|rollout restart|kind load/);
  } finally { await rm(f.root, { recursive: true, force: true }); }
});
