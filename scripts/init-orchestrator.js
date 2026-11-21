import { chmod, mkdir } from "node:fs/promises";
import { fileURLToPath } from "node:url";
import path from "node:path";

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");

// The orchestrator keeps its workflows in the blobstore. KIND's container UID differs from
// the host owner, so the blobstore data directory gets sticky shared-write permissions like
// the gateway logs.
const blobData = path.join(root, "services", "blobstore", "data");
await mkdir(blobData, { recursive: true });
await chmod(blobData, 0o1777);
