import assert from "node:assert/strict";
import { mkdir, mkdtemp, rm, writeFile, symlink, readFile, stat } from "node:fs/promises";
import { execFileSync } from "node:child_process";
import { tmpdir } from "node:os";
import path from "node:path";
import test from "node:test";
import { checkFixtures, prepareSeeds, seedDirectories, syncSeeds } from "./sync-seeds.js";

test("empty checkout seeds prepare empty optional directories", async () => {
  const dir = await mkdtemp(path.join(tmpdir(), "factory-seeds-"));
  try {
    await prepareSeeds(dir);
    for (const name of seedDirectories) assert.ok((await stat(path.join(dir, name))).isDirectory());
    await writeFile(path.join(dir, "operator.txt"), "keep");
    await prepareSeeds(dir);
    assert.equal(await readFile(path.join(dir, "operator.txt"), "utf8"), "keep");
  } finally { await rm(dir, { recursive: true, force: true }); }
});

test("seed snapshot is published only after successful copy", async () => {
  const dir = await mkdtemp(path.join(tmpdir(), "factory-seeds-"));
  const calls = [];
  try {
    await syncSeeds(dir, "factory-control-plane", (cmd, args) => calls.push([cmd, ...args]));
    assert.equal(calls.length, 3);
    assert.equal(calls[1][1], "cp");
    assert.match(calls[2].join(" "), /mv -Tf/);
    const failed = [];
    await assert.rejects(syncSeeds(dir, "factory-control-plane", (cmd, args) => {
      failed.push(args); if (args[0] === "cp") throw new Error("copy failed");
    }), /copy failed/);
    assert.equal(failed.length, 2);
  } finally { await rm(dir, { recursive: true, force: true }); }
});

test("links cannot import external files as seeds", async () => {
  const dir = await mkdtemp(path.join(tmpdir(), "factory-seeds-"));
  try {
    await symlink(tmpdir(), path.join(dir, "external"));
    await assert.rejects(prepareSeeds(dir), /symbolic link/);
  } finally { await rm(dir, { recursive: true, force: true }); }
});

test("operator seed payloads are not tracked", () => {
  const files = execFileSync("git", ["ls-files", "-z"], { encoding: "utf8" }).split("\0").filter(Boolean);
  assert.deepEqual(files.filter(p => p.startsWith("scratch/") || /(^|\/)seed\//.test(p) || /(^|\/)seeds\//.test(p)), []);
});

test("examples remain local-only", async () => {
  const files = execFileSync("git", ["ls-files", "-z"], { encoding: "utf8" }).split("\0").filter(Boolean);
  assert.deepEqual(files.filter(p => p.startsWith("examples/")), []);
  // The test writes its own file, so it does not depend on any file of the project.
  const root = execFileSync("git", ["rev-parse", "--show-toplevel"], { encoding: "utf8" }).trim();
  const dir = path.join(root, "examples");
  const created = await mkdir(dir, { recursive: true });
  const file = path.join(dir, `local-only-test-${process.pid}.txt`);
  await writeFile(file, "test\n");
  try {
    // git check-ignore exits 0 when the path is ignored and fails otherwise.
    execFileSync("git", ["check-ignore", "-q", file], { cwd: root });
  } finally {
    await rm(file, { force: true });
    if (created) await rm(dir, { recursive: true, force: true });
  }
});

test("the tracked test fixtures join the seed snapshot and hold only the group test", async () => {
  const seeds = await mkdtemp(path.join(tmpdir(), "factory-seeds-"));
  const fixtures = await mkdtemp(path.join(tmpdir(), "factory-fixtures-"));
  const calls = [];
  try {
    await mkdir(path.join(fixtures, "registry/agents/test/echo/v1"), { recursive: true });
    await writeFile(path.join(fixtures, "registry/agents/test/echo/v1/agent.json"), "{}");
    await mkdir(path.join(fixtures, "orchestrator"), { recursive: true });
    await writeFile(path.join(fixtures, "orchestrator/test.json"), "{}");
    await syncSeeds(seeds, "factory-control-plane", (cmd, args) => calls.push([cmd, ...args]), fixtures);
    const copies = calls.filter((c) => c[1] === "cp");
    assert.equal(copies.length, 2, "the operator's seeds, then the fixtures");
    assert.ok(copies[1][2].startsWith(await checkFixtures(fixtures)));
    assert.match(calls.at(-1).join(" "), /mv -Tf/, "published only after both copies");
    // Anything outside the group "test" is refused before anything is copied.
    await mkdir(path.join(fixtures, "registry/agents/course/x/v1"), { recursive: true });
    await writeFile(path.join(fixtures, "registry/agents/course/x/v1/agent.json"), "{}");
    calls.length = 0;
    await assert.rejects(syncSeeds(seeds, "factory-control-plane", (cmd, args) => calls.push([cmd, ...args]), fixtures), /only the group "test"/);
    assert.equal(calls.length, 0);
  } finally {
    await rm(seeds, { recursive: true, force: true });
    await rm(fixtures, { recursive: true, force: true });
  }
});

test("the repository's test fixtures hold only the group test", async () => {
  const root = execFileSync("git", ["rev-parse", "--show-toplevel"], { encoding: "utf8" }).trim();
  await checkFixtures(path.join(root, "fixtures/factory"));
  const workflows = JSON.parse(await readFile(path.join(root, "fixtures/factory/orchestrator/test.json"), "utf8")).workflows;
  assert.ok(workflows.length > 0 && workflows.every((w) => w.name.startsWith("test/")));
  assert.ok(workflows.flatMap((w) => w.steps).every((s) => !s.agent || s.agent.startsWith("test/")), "they use only test agents");
});
