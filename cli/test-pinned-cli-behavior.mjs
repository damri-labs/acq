// Behavioral test for the pinned download documented in cli/README.md.
//
// test-readme-pin.mjs proves INTEGRITY only: the bytes at the pinned URL
// hash to the documented digest and line count. Integrity is not
// behavior - a pin can be digest-perfect yet lack behavior the README
// advertises (the historical pre-guard pin was exactly that: correct
// digest, no session-replace guard, while the README showed --replace).
// This suite closes that gap for the session-replace guard:
//  1. Extract the documented pin, digest and line count (same block the
//     pin suite validates).
//  2. Download the pinned bytes and verify them against the documented
//     digest and line count. Integrity failure stops here.
//  3. Run the session-guard suite against the DOWNLOADED artifact (via
//     ACQ_CLI_UNDER_TEST), so the immutable CLI the README sends
//     newcomers to is the artifact whose guard behavior is proven - not
//     whatever sibling acq.js happens to be in the working tree.
//
// Offline policy: fail closed. If the pinned bytes cannot be fetched,
// the downloaded artifact's behavior is unverified and this suite says
// so with a FAIL and a nonzero exit; it never silently claims a pass.
// ACQ_PIN_BEHAVIOR_OFFLINE_SKIP=1 gives an explicit, loudly reported
// skip for offline development only: under CI (the CI variable is set)
// the skip is ignored and a fetch failure stays a FAIL, so a skipped
// run can never count as CI success.
// Run: node test-pinned-cli-behavior.mjs
import { createHash } from "node:crypto";
import { spawn } from "node:child_process";
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { get } from "node:https";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

const DIR = fileURLToPath(new URL(".", import.meta.url));
const readme = readFileSync(join(DIR, "README.md"), "utf8");

let failures = 0;
function check(name, cond, detail = "") {
  if (cond) { console.log(`ok - ${name}`); return true; }
  failures++;
  console.error(`FAIL - ${name}${detail ? ` :: ${String(detail).slice(0, 160)}` : ""}`);
  return false;
}
const sha256 = buf => createHash("sha256").update(buf).digest("hex");
const lineCount = buf => (buf.toString("utf8").match(/\n/g) || []).length;

const urlRe = /raw\.githubusercontent\.com\/damri-labs\/acq\/([0-9a-fA-F]+)\/cli\/acq\.js/g;
const pins = [...readme.matchAll(urlRe)].map(m => m[1]);
const digestMatch = readme.match(/should print\s*exactly\s*`([0-9a-fA-F]+)`/s);
const linesMatch = readme.match(/self-contained file - (\d+) lines/);

if (!pins.length && !digestMatch && !linesMatch) {
  console.log("ok - no pinned download documented in cli/README.md; nothing to check");
  process.exit(0);
}
if (!check("pinned download block is complete and unique",
  pins.length === 1 && !!digestMatch && !!linesMatch,
  `pins=${pins.length} digest=${!!digestMatch} lines=${!!linesMatch}`)) {
  console.error(`${failures} FAIL`);
  process.exit(1);
}

const pin = pins[0];
const digest = digestMatch[1];
const docLines = Number(linesMatch[1]);
const url = `https://raw.githubusercontent.com/damri-labs/acq/${pin}/cli/acq.js`;

function fetchPinned() {
  return new Promise((resolve, reject) => {
    const req = get(url, res => {
      if (res.statusCode !== 200) {
        res.resume();
        reject(Object.assign(new Error(`HTTP ${res.statusCode}`), { http: true }));
        return;
      }
      const parts = [];
      res.on("data", d => parts.push(d));
      res.on("end", () => resolve(Buffer.concat(parts)));
    });
    req.setTimeout(10000, () => req.destroy(new Error("timeout")));
    req.on("error", reject);
  });
}

let bytes;
try {
  bytes = await fetchPinned();
} catch (err) {
  if (process.env.ACQ_PIN_BEHAVIOR_OFFLINE_SKIP === "1" && !process.env.CI) {
    console.log(`skip - pinned CLI could not be fetched (${err.message}); downloaded behavior NOT verified (explicit offline skip)`);
    process.exit(0);
  }
  check("pinned CLI fetched for behavioral verification", false,
    `${err.message} - downloaded behavior unverified; fail-closed. Set ACQ_PIN_BEHAVIOR_OFFLINE_SKIP=1 for an explicit offline skip`);
  console.error(`${failures} FAIL`);
  process.exit(1);
}

// Integrity gate: the behavior below is only meaningful for the exact
// immutable bytes the README documents.
check("pinned bytes hash to the documented digest", sha256(bytes) === digest,
  `pinned sha256 ${sha256(bytes)} vs documented ${digest}`);
check("pinned bytes have the documented line count", lineCount(bytes) === docLines,
  `pinned ${lineCount(bytes)} lines vs documented ${docLines}`);
if (failures) { console.error(`${failures} FAIL`); process.exit(1); }

// The pinned file is ESM; outside the repo's package scope it needs an
// explicit .mjs name for the same runtime to load it as a module.
const work = mkdtempSync(join(tmpdir(), "acq-pinned-"));
const cliUnderTest = join(work, "acq.mjs");
try {
  writeFileSync(cliUnderTest, bytes);
  console.log(`ok - downloaded pin ${pin.slice(0, 7)} staged as the CLI under test`);
  // Bounded child: the fetch timeout bounds the download only, so the
  // guard suite itself gets its own 60s ceiling (normal run ~2s).
  const result = await new Promise((resolve) => {
    const child = spawn(process.execPath, [join(DIR, "test-session-guard.mjs")], {
      env: { ...process.env, ACQ_CLI_UNDER_TEST: cliUnderTest },
      stdio: "inherit",
    });
    const bound = setTimeout(() => { child.kill("SIGKILL"); resolve("timeout"); }, 60000);
    child.on("close", code => { clearTimeout(bound); resolve(code); });
  });
  check("session-guard suite passes against the downloaded pinned CLI", result === 0,
    result === "timeout" ? "guard suite exceeded the 60s bound" : `guard suite exited ${result} against pin ${pin}`);
} finally {
  rmSync(work, { recursive: true, force: true });
}

if (failures) { console.error(`${failures} FAIL`); process.exit(1); }
console.log("all pinned-cli-behavior tests passed");
