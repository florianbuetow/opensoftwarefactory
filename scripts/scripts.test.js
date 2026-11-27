import assert from "node:assert/strict";
import { existsSync, readFileSync } from "node:fs";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const read = (file) => readFileSync(path.join(root, file), "utf8");

// cases lists the tests a script dispatches and the node script each runs.
const cases = (script) => [...script.matchAll(/^ {4}([a-z-]+)\) (?:VERIFY_REAL_OPENOBSERVE=1 )?node (scripts\/[a-z0-9-]+\.js)(?: [a-z]+)? ;;$/gm)].map((m) => [m[1], m[2]]);

test("e2e.sh dispatches the end-to-end tests, each to a script that exists, after a preflight", () => {
  const script = read("scripts/e2e.sh");
  const found = cases(script);
  assert.deepEqual(found.map(([name]) => name), ["ui", "ui-pages", "ui-sweep", "openobserve", "measurements-live"]);
  for (const [, file] of found) assert.ok(existsSync(path.join(root, file)), `${file} exists`);
  assert.match(script, /tests="ui openobserve measurements-live"/, "all of them without a test");
  assert.match(script, /if ! just status >\/dev\/null 2>&1; then\n\s+printf 'The factory is not running: start it with just start\\n' >&2\n\s+exit 1/, "a stopped factory fails at once");
  assert.match(script, /node scripts\/e2e-ui\.js preflight/, "then it must be fully up");
  assert.equal((script.match(/^\s+preflight$/gm) || []).length, 2, "before all tests and before one");
});

test("integration.sh dispatches the integration tests, each to a script that exists, without the cluster", () => {
  const script = read("scripts/integration.sh");
  const found = cases(script);
  assert.deepEqual(found.map(([name]) => name), ["measurements", "workflow-events"]);
  for (const [, file] of found) assert.ok(existsSync(path.join(root, file)), `${file} exists`);
  assert.doesNotMatch(script, /just status|preflight/);
});

test("just e2e runs the end-to-end tests; the justfile has no other verification targets", () => {
  const justfile = read("justfile");
  assert.match(justfile, /\ne2e:\n {4}@\.\/scripts\/e2e\.sh\n/);
  assert.match(justfile, /"just e2e" "Run the end-to-end tests against the running factory"/);
  assert.deepEqual(justfile.match(/^verify[a-z-]*:/gm) || [], []);
  assert.doesNotMatch(justfile, /ci-checks := "[^"]*e2e/, "end-to-end tests stay out of just ci");
});
