import { readFile, writeFile } from "node:fs/promises";
import { fileURLToPath } from "node:url";
import path from "node:path";

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const source = path.join(root, "infra", "kind", "cluster.yaml");
const destination = path.join(root, "infra", "kind", "cluster.local.yaml");
const configDirectory = path.join(root, "services", "llm-gateway", "config");
const logsDirectory = path.join(root, "services", "llm-gateway", "logs");
const marker = "  - role: control-plane\n";
const base = await readFile(source, "utf8");

if (!base.includes(marker)) {
  throw new Error(`${path.relative(root, source)} has no control-plane node`);
}

const mounts = [
  "    extraMounts:",
  `      - hostPath: ${JSON.stringify(configDirectory)}`,
  "        containerPath: /llm-gateway/config",
  "        readOnly: true",
  `      - hostPath: ${JSON.stringify(logsDirectory)}`,
  "        containerPath: /llm-gateway/logs",
  `      - hostPath: ${JSON.stringify(path.join(root, "services", "openobserve", "config"))}`,
  "        containerPath: /openobserve/config",
  "        readOnly: true",
  `      - hostPath: ${JSON.stringify(path.join(root, "services", "openobserve", "data"))}`,
  "        containerPath: /openobserve/data",
  `      - hostPath: ${JSON.stringify(path.join(root, "services", "blobstore", "data"))}`,
  "        containerPath: /blobstore/data",
  `      - hostPath: ${JSON.stringify(path.join(root, "services", "tool-gateway", "tools"))}`,
  "        containerPath: /tool-gateway/tools",
  "        readOnly: true",
  // A directory on the Colima VM disk, not the macOS host: SQLite needs POSIX
  // locks that virtiofs lacks, and the VM disk outlives `just cluster-delete`.
  "      - hostPath: /var/lib/opensoftwarefactory",
  "        containerPath: /var/lib/opensoftwarefactory",
  "",
].join("\n");

const generated = base.replace(marker, `${marker}${mounts}`);
await writeFile(destination, generated, "utf8");
console.log(`Generated ${path.relative(root, destination)}`);
