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

export async function syncSeeds(directory, node, run = execFileSync) {
  if (!/^[a-zA-Z0-9][a-zA-Z0-9_.-]*$/.test(node)) throw new Error("Invalid KIND node name");
  const root = await prepareSeeds(directory);
  const base = "/var/lib/opensoftwarefactory/seeds";
  const generation = `snapshot-${randomUUID()}`;
  const destination = `${base}/${generation}`;
  const options = { stdio: "inherit" };
  run("docker", ["exec", node, "mkdir", "-p", destination], options);
  run("docker", ["cp", `${root}/.`, `${node}:${destination}`], options);
  // Publish only a complete snapshot. Retain prior snapshots, never remove source data.
  run("docker", ["exec", node, "sh", "-eu", "-c",
    'chown -R 1000:1000 "$1"; chmod -R a+rX "$1"; ln -s "$2" "$3"; mv -Tf "$3" "$4"',
    "sync-seeds", destination, generation, `${base}/.next-${generation}`, `${base}/current`], options);
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
  await syncSeeds(path.join(root, "scratch/seeds"), process.argv[2] || "opensoftwarefactory-control-plane");
  console.log("Local seed snapshot published; existing stored entries remain unchanged.");
}
