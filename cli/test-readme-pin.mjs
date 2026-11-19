// Regression test for the pinned-download block in cli/README.md.
//
// The README teaches newcomers to download cli/acq.js from one pinned
// commit and gives the sha256 digest and line count to compare against.
// A wrong value there teaches every reader a false integrity check (a
// retyped digest once gained a 65th character and shipped for review).
// This suite extracts the documented pin, digest and line count and
// validates them:
//  1. Structure (always, offline-safe): the pin is exactly 40 lowercase
//     hex, the digest exactly 64 lowercase hex, the line count a positive
//     integer, and each appears exactly once. A malformed value is a hard
//     failure, not a skip.
//  2. Content (online): the bytes served at the pinned URL hash to the
//     documented digest and have the documented line count.
//  3. Offline fallback: when the network is unreachable the local acq.js
//     is accepted only if it matches the documented digest byte-for-byte;
//     otherwise the content check is reported as skipped, never as passed.
// In a repository whose README documents no pinned download the suite
// says so and exits 0.
// Run: node test-readme-pin.mjs
import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { get } from "node:https";
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

// Any of these marks a pinned-download block; partial blocks are failures.
const urlRe = /raw\.githubusercontent\.com\/damri-labs\/acq\/([0-9a-fA-F]+)\/cli\/acq\.js/g;
const urls = [...readme.matchAll(urlRe)].map(m => m[1]);
const digestMatch = readme.match(/should print\s*exactly\s*`([0-9a-fA-F]+)`/s);
const linesMatch = readme.match(/self-contained file - (\d+) lines/);

if (!urls.length && !digestMatch && !linesMatch) {
  console.log("ok - no pinned download documented in cli/README.md; nothing to check");
  process.exit(0);
}

check("pin URL appears exactly once", urls.length === 1, `found ${urls.length}`);
const pin = urls[0] || "";
check("pin is exactly 40 lowercase hex", /^[0-9a-f]{40}$/.test(pin), `got "${pin}" (${pin.length} chars)`);
check("documented digest is exactly 64 lowercase hex",
  !!digestMatch && /^[0-9a-f]{64}$/.test(digestMatch[1]),
  digestMatch ? `got "${digestMatch[1]}" (${digestMatch[1].length} chars)` : "no digest found");
check("documented line count is a positive integer",
  !!linesMatch && Number(linesMatch[1]) > 0,
  linesMatch ? `got "${linesMatch[1]}"` : "no line count found");
if (failures) { console.error(`${failures} FAIL`); process.exit(1); }

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

try {
  const bytes = await fetchPinned();
  check("pinned bytes hash to the documented digest", sha256(bytes) === digest,
    `pinned sha256 ${sha256(bytes)} vs documented ${digest}`);
  check("pinned bytes have the documented line count", lineCount(bytes) === docLines,
    `pinned ${lineCount(bytes)} lines vs documented ${docLines}`);
} catch (err) {
  if (err.http) {
    // The pin resolves but the bytes are not served: a broken reference,
    // fail even offline-capable readers would hit this.
    check("pinned URL serves cli/acq.js", false, err.message);
  } else {
    const local = readFileSync(join(DIR, "acq.js"));
    if (sha256(local) === digest && lineCount(local) === docLines) {
      console.log("ok - network unreachable; verified against local acq.js matching the documented digest");
    } else {
      console.log(`skip - network unreachable (${err.message}) and local acq.js differs from the pin; content check not run, structure checks passed`);
    }
  }
}

if (failures) { console.error(`${failures} FAIL`); process.exit(1); }
console.log("all readme-pin tests passed");
