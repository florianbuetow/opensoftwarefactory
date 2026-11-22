import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { cp, mkdir, mkdtemp, readFile, realpath, rm, stat, symlink, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import test from "node:test";

const sourceRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");

test("initialization preserves credentials/data and KIND mounts the host directories", async () => {
  const root = await realpath(await mkdtemp(path.join(tmpdir(), "openobserve setup ")));
  try {
    await mkdir(path.join(root, "scripts"));
    await mkdir(path.join(root, "services", "openobserve"), { recursive: true });
    await mkdir(path.join(root, "infra", "kind"), { recursive: true });
    for (const file of ["scripts/init-openobserve.js", "scripts/generate-kind-config.js", "services/openobserve/config.template", "infra/kind/cluster.yaml"]) {
      await cp(path.join(sourceRoot, file), path.join(root, file));
    }
    await writeFile(path.join(root, "package.json"), '{"type":"module"}\n');
    const run = (script) => execFileSync(process.execPath, [path.join(root, "scripts", script)], { encoding: "utf8", stdio: "pipe" });
    const configPath = path.join(root, "services/openobserve/config/openobserve.env");
    const dataPath = path.join(root, "services/openobserve/data");
    const collectorOutputPath = path.join(root, "services/openobserve/config/fluent-bit-output.conf");
    const firstOutput = run("init-openobserve.js");
    const config = await readFile(configPath, "utf8");
    const password = config.match(/^ZO_ROOT_USER_PASSWORD=(Oo1![a-f0-9]{48})$/m)?.[1];
    assert.ok(password, "generate a unique local password");
    assert.ok(!firstOutput.includes(password), "never print credentials");
    const collectorOutput = await readFile(collectorOutputPath, "utf8");
    assert.match(collectorOutput, /URI\s+\/api\/default\/factory\/_json/);
    assert.match(collectorOutput, /Alias\s+openobserve/);
    assert.match(collectorOutput, /Retry_Limit\s+False/);
    assert.match(collectorOutput, /HTTP_User\s+admin@example\.com/);
    assert.ok(collectorOutput.includes(`HTTP_Passwd              ${password}`));
    assert.ok(!firstOutput.includes(collectorOutput), "never print the collector's credential include");
    assert.equal((await stat(collectorOutputPath)).mode & 0o777, 0o600);
    const traceConfigPath = path.join(root, "services/openobserve/config/otel-collector.yaml");
    const traceConfig = await readFile(traceConfigPath, "utf8");
    const encoded = Buffer.from(`admin@example.com:${password}`).toString("base64");
    assert.ok(traceConfig.includes(`Authorization: "Basic ${encoded}"`));
    assert.ok(!firstOutput.includes(encoded));
    assert.equal((await stat(traceConfigPath)).mode & 0o777, 0o600);
    assert.match(traceConfig, /storage: file_storage/);
    assert.match(traceConfig, /endpoint: http:\/\/openobserve:5080\/api\/default/);

    assert.equal((await stat(path.join(root, "services/openobserve"))).mode & 0o777, 0o700);
    assert.equal((await stat(dataPath)).mode & 0o7777, 0o1777);
    await writeFile(configPath, config + "# local edit\n");
    await writeFile(path.join(dataPath, "preserve-me"), "existing telemetry");
    run("init-openobserve.js");
    assert.equal(await readFile(configPath, "utf8"), config + "# local edit\n");
    await writeFile(configPath, (config + "# local edit\n").replace("ZO_COMPACT_DATA_RETENTION_DAYS=0", "ZO_COMPACT_DATA_RETENTION_DAYS=7").replace(/^ZO_INGEST_ALLOWED_UPTO=.*\n/m, ""));
    run("init-openobserve.js");
    const migrated = await readFile(configPath, "utf8");
    assert.ok(migrated.includes(`ZO_ROOT_USER_PASSWORD=${password}`));
    assert.match(migrated, /^ZO_COMPACT_DATA_RETENTION_DAYS=0$/m);
    assert.match(migrated, /^ZO_INGEST_ALLOWED_UPTO=100000000$/m);
    assert.ok(migrated.includes("# local edit\n"));
    assert.equal(await readFile(path.join(dataPath, "preserve-me"), "utf8"), "existing telemetry");
    run("generate-kind-config.js");
    const generated = await readFile(path.join(root, "infra/kind/cluster.local.yaml"), "utf8");
    assert.ok(generated.includes(`hostPath: ${JSON.stringify(path.dirname(configPath))}\n        containerPath: /openobserve/config\n        readOnly: true`));
    assert.ok(generated.includes(`hostPath: ${JSON.stringify(dataPath)}\n        containerPath: /openobserve/data`));
    assert.ok(generated.includes("containerPath: /llm-gateway/config"));
    assert.ok(generated.includes("containerPath: /llm-gateway/logs"));
    await rm(configPath);
    await mkdir(configPath);
    assert.throws(() => run("init-openobserve.js"), /must be a regular file/);
    await rm(configPath, { recursive: true });
    await symlink(path.join(dataPath, "preserve-me"), configPath);
    assert.throws(() => run("init-openobserve.js"), /must be a regular file/);
  } finally {
    await rm(root, { recursive: true, force: true });
  }
});
