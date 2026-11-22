import test from "node:test";
import assert from "node:assert/strict";
import { mkdtemp, readFile, rm, stat, symlink, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { initializeEventCredentials } from "./init-jetstream.js";

test("initialization preserves private event credentials", async () => {
  const dir = await mkdtemp(path.join(tmpdir(), "factory-event-config-"));
  try {
    await initializeEventCredentials(dir);
    const before = await readFile(path.join(dir, "auth.json"), "utf8");
    await initializeEventCredentials(dir);
    assert.equal(await readFile(path.join(dir, "auth.json"), "utf8"), before);
    const secret = JSON.parse(await readFile(path.join(dir, "secret.json"), "utf8"));
    assert.equal(secret.items.find(s => s.metadata.name === "factory-events-publisher").stringData.NATS_PASSWORD, JSON.parse(before).NATS_PASSWORD);
    assert.notEqual(secret.items[0].stringData.NATS_PASSWORD, secret.items[1].stringData.NATS_PASSWORD);
    assert.equal((await stat(path.join(dir, "secret.json"))).mode & 0o777, 0o600);
  } finally { await rm(dir, { recursive: true, force: true }); }
});

test("event initialization does not follow a secret manifest symlink", async () => {
  const dir = await mkdtemp(path.join(tmpdir(), "factory-event-config-"));
  try {
    await writeFile(path.join(dir, "preserve"), "existing file");
    await symlink(path.join(dir, "preserve"), path.join(dir, "secret.json"));
    await assert.rejects(initializeEventCredentials(dir), /regular file/);
    assert.equal(await readFile(path.join(dir, "preserve"), "utf8"), "existing file");
  } finally { await rm(dir, { recursive: true, force: true }); }
});
