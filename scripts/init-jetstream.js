import { mkdir, writeFile, readFile, chmod, lstat } from "node:fs/promises";
import { randomBytes } from "node:crypto";
import path from "node:path";
import { fileURLToPath } from "node:url";

export async function initializeEventCredentials(directory) {
  await mkdir(directory, { recursive: true, mode: 0o700 });
  const file = path.join(directory, "auth.json");
  try {
    await writeFile(file, JSON.stringify({ NATS_USER: "factory", NATS_PASSWORD: randomBytes(32).toString("hex") }), { flag: "wx", mode: 0o600 });
  } catch (error) { if (error.code !== "EEXIST") throw error; }
  const credentialStat = await lstat(file);
  if (!credentialStat.isFile() || credentialStat.isSymbolicLink()) throw new Error("Event credentials must be a regular file");
  const auth = JSON.parse(await readFile(file, "utf8"));
  if (!auth.NATS_USER || !auth.NATS_PASSWORD) throw new Error("Incomplete local event credentials");
  if (!auth.NATS_CONSUMER_USER || !auth.NATS_CONSUMER_PASSWORD) {
    auth.NATS_CONSUMER_USER = "factory-consumer";
    auth.NATS_CONSUMER_PASSWORD = randomBytes(32).toString("hex");
    await writeFile(file, JSON.stringify(auth), { mode: 0o600 });
  }
  const makeSecret = (name, stringData) => ({ apiVersion: "v1", kind: "Secret", metadata: { name, namespace: "opensoftwarefactory" }, type: "Opaque", stringData });
  const secret = { apiVersion: "v1", kind: "List", items: [
    makeSecret("factory-events-publisher", { NATS_USER: auth.NATS_USER, NATS_PASSWORD: auth.NATS_PASSWORD }),
    makeSecret("factory-events-consumer", { NATS_USER: auth.NATS_CONSUMER_USER, NATS_PASSWORD: auth.NATS_CONSUMER_PASSWORD }),
    makeSecret("factory-events-server", { NATS_PUBLISHER_USER: auth.NATS_USER, NATS_PUBLISHER_PASSWORD: auth.NATS_PASSWORD, NATS_CONSUMER_USER: auth.NATS_CONSUMER_USER, NATS_CONSUMER_PASSWORD: auth.NATS_CONSUMER_PASSWORD }),
  ] };
  const manifest = path.join(directory, "secret.json");
  try {
    const existing = await lstat(manifest);
    if (!existing.isFile() || existing.isSymbolicLink()) throw new Error("Event secret manifest must be a regular file");
  } catch (error) { if (error.code !== "ENOENT") throw error; }
  await writeFile(manifest, JSON.stringify(secret), { mode: 0o600 });
  await chmod(manifest, 0o600);
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
  await initializeEventCredentials(path.join(root, "scratch/measurements/jetstream"));
  console.log("Local event credentials initialized.");
}
