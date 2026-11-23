import { mkdir, readdir, lstat, realpath } from "node:fs/promises";
import { execFileSync } from "node:child_process";
import { randomUUID } from "node:crypto";
import { fileURLToPath } from "node:url";
import path from "node:path";

export const seedDirectories = ["registry/agents", "registry/policies", "registry/skills", "orchestrator", "git-host", "connectors", "factory-ui"];

// Seeds are operator data. Reject links rather than copying files outside this tree.
export async function prepareSeeds(directory) {
  await mkdir(directory, { recursive: true });
  if ((await lstat(directory)).isSymbolicLink()) throw new Error("Seed root must not be a symbolic link");
  const root = await realpath(directory);
  async function inspect(dir) {
    for (const entry of await readdir(dir, { withFileTypes: true })) {
      if (entry.isSymbolicLink()) throw new Error(`Seed symbolic link is not allowed: ${path.relative(root, path.join(dir, entry.name))}`);
      if (entry.isDirectory()) await inspect(path.join(dir, entry.name));
      else if (!entry.isFile()) throw new Error("Seed tree contains a non-regular file");
    }
  }
  await inspect(root);
  for (const relative of seedDirectories) await mkdir(path.join(root, relative), { recursive: true });
  return root;
}

// The factory's own test fixtures (fixtures/factory, tracked in git) hold only the group "test":
// agents under registry/agents/test/ and workflows in orchestrator/test.json.
export async function checkFixtures(directory) {
  const root = await realpath(directory);
  const files = [];
  async function walk(dir) {
    for (const entry of await readdir(dir, { withFileTypes: true })) {
      const full = path.join(dir, entry.name), relative = path.relative(root, full);
      if (entry.isSymbolicLink()) throw new Error(`Fixture symbolic link is not allowed: ${relative}`);
      if (entry.isDirectory()) await walk(full);
      else files.push(relative.split(path.sep).join("/"));
    }
  }
  await walk(root);
  const outside = files.filter((f) => f !== "README.md" && f !== "orchestrator/test.json" && !f.startsWith("registry/agents/test/"));
  if (outside.length) throw new Error(`Test fixtures may hold only the group "test": ${outside.join(", ")}`);
  return root;
}

export async function syncSeeds(directory, node, run = execFileSync, fixtures = "") {
  if (!/^[a-zA-Z0-9][a-zA-Z0-9_.-]*$/.test(node)) throw new Error("Invalid KIND node name");
  const root = await prepareSeeds(directory);
  const fixtureRoot = fixtures ? await checkFixtures(fixtures) : "";
  const base = "/var/lib/opensoftwarefactory/seeds";
  const generation = `snapshot-${randomUUID()}`;
  const destination = `${base}/${generation}`;
  const options = { stdio: "inherit" };
  run("docker", ["exec", node, "mkdir", "-p", destination], options);
  run("docker", ["cp", `${root}/.`, `${node}:${destination}`], options);
  // The test fixtures join the snapshot; they hold only the group "test", so they never replace an operator seed.
  if (fixtureRoot) run("docker", ["cp", `${fixtureRoot}/.`, `${node}:${destination}`], options);
  // Publish only a complete snapshot. Retain prior snapshots, never remove source data.
  run("docker", ["exec", node, "sh", "-eu", "-c",
    'chown -R 1000:1000 "$1"; chmod -R a+rX "$1"; ln -s "$2" "$3"; mv -Tf "$3" "$4"',
    "sync-seeds", destination, generation, `${base}/.next-${generation}`, `${base}/current`], options);
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
  await syncSeeds(path.join(root, "scratch/seeds"), process.argv[2] || "opensoftwarefactory-control-plane", execFileSync, path.join(root, "fixtures/factory"));
  console.log("Local seed snapshot published; existing stored entries remain unchanged.");
}
