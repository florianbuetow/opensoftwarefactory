import { chmod, lstat, mkdir, readFile, writeFile } from "node:fs/promises";
import { randomBytes } from "node:crypto";
import { fileURLToPath } from "node:url";
import path from "node:path";

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const service = path.join(root, "services", "openobserve");
const configDirectory = path.join(service, "config");
const config = path.join(configDirectory, "openobserve.env");
const collectorOutput = path.join(configDirectory, "fluent-bit-output.conf");
const data = path.join(service, "data");

await mkdir(configDirectory, { recursive: true });
// Protect credentials and telemetry from other host users. Bind mounts expose
// only the child directories to the container, whose UID differs from the host.
await chmod(service, 0o700);
await mkdir(data, { recursive: true });
// KIND's container UID differs from the host owner, as with gateway logs.
await chmod(data, 0o1777);

try {
  const template = await readFile(path.join(service, "config.template"), "utf8");
  await writeFile(config, template.replace("__GENERATE_PASSWORD__", `Oo1!${randomBytes(24).toString("hex")}`), {
    flag: "wx",
    mode: 0o644, // Readable by the non-root container through the host mount.
  });
  console.log(`Created ${path.relative(root, config)} with a generated local password`);
} catch (error) {
  if (error.code !== "EEXIST") throw error;
  const existing = await lstat(config);
  if (!existing.isFile() || existing.isSymbolicLink()) {
    throw new Error(`${path.relative(root, config)} must be a regular file, not a directory or symlink`);
  }
  console.log(`Kept existing ${path.relative(root, config)}`);
}

// Apply the requested retained-history policy without changing credentials or other settings.
let retainedConfig = await readFile(config, "utf8");
for (const [key, value] of Object.entries({ ZO_COMPACT_DATA_RETENTION_DAYS: "0", ZO_INGEST_ALLOWED_UPTO: "100000000", ZO_MAX_FILE_RETENTION_TIME: "10", ZO_CALCULATE_STATS_INTERVAL: "10", ZO_USE_STREAM_SETTINGS_FOR_PARTITIONS_ENABLED: "false" })) {
  const pattern = new RegExp(`^${key}=.*$`, "m");
  retainedConfig = pattern.test(retainedConfig) ? retainedConfig.replace(pattern, `${key}=${value}`) : `${retainedConfig.trimEnd()}\n${key}=${value}\n`;
}
await writeFile(config, retainedConfig);
const values = Object.fromEntries(retainedConfig
  .split(/\r?\n/)
  .filter((line) => line && !line.startsWith("#") && line.includes("="))
  .map((line) => [line.slice(0, line.indexOf("=")), line.slice(line.indexOf("=") + 1)]));
const email = values.ZO_ROOT_USER_EMAIL;
const password = values.ZO_ROOT_USER_PASSWORD;
if (!email || !password || /[\r\n]/.test(email + password)) {
  throw new Error(`${path.relative(root, config)} must define single-line ZO_ROOT_USER_EMAIL and ZO_ROOT_USER_PASSWORD values`);
}
// Fluent Bit's official image is shell-free. Generate an ignored include file instead of
// exposing credentials in a Kubernetes Secret, manifest, command line, or process environment.
await writeFile(collectorOutput, `[OUTPUT]
    Name                     http
    Alias                    openobserve
    Match                    kube.*
    Host                     openobserve
    Port                     5080
    URI                      /api/default/factory/_json
    Format                   json
    Json_Date_Key            _timestamp
    Json_Date_Format         iso8601
    HTTP_User                ${email}
    HTTP_Passwd              ${password}
    Retry_Limit              False
    storage.total_limit_size 100M
`, { mode: 0o600 });
await chmod(collectorOutput, 0o600);

// Trace exporters send only to this internal collector; authentication remains here.
const traceCollector = path.join(configDirectory, "otel-collector.yaml");
const authorization = Buffer.from(`${email}:${password}`).toString("base64");
await writeFile(traceCollector, `receivers:
  otlp:
    protocols:
      http:
        endpoint: 0.0.0.0:4318
processors:
  memory_limiter:
    check_interval: 1s
    limit_mib: 192
    spike_limit_mib: 32
  batch:
    timeout: 1s
    send_batch_size: 512
extensions:
  health_check:
    endpoint: 0.0.0.0:13133
  file_storage:
    directory: /buffers
exporters:
  otlp_http/openobserve:
    endpoint: http://openobserve:5080/api/default
    headers:
      Authorization: "Basic ${authorization}"
    retry_on_failure:
      enabled: true
      max_elapsed_time: 0s
    sending_queue:
      enabled: true
      queue_size: 1000
      storage: file_storage
service:
  extensions: [health_check, file_storage]
  telemetry:
    metrics:
      readers:
        - pull:
            exporter:
              prometheus:
                host: 0.0.0.0
                port: 8888
  pipelines:
    traces:
      receivers: [otlp]
      processors: [memory_limiter, batch]
      exporters: [otlp_http/openobserve]
`, { mode: 0o600 });
await chmod(traceCollector, 0o600);
