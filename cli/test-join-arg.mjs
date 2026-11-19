// Integration tests for the acq CLI's join-argument handling.
//
// Invariant under test: every invite form our own docs hand out must reach
// the same endpoint. The README, the examples repo and the room briefing all
// say "hand the other side the UIN or the invite link", but join used to
// splice its argument verbatim into the request path, so a pasted invite
// link (https://host/room/<UIN>) produced /room/https://host/.../join and a
// 404 - a demonstrated real-trial failure. UIN case/hyphen normalization is
// the SERVER's job (normalizeUIN) and stays there: for bare codes the CLI
// must pass the argument through byte-identical; only the URL wrapper
// (scheme, host, /room/ path, query, trailing slashes) is stripped.
// Run: node test-join-arg.mjs
import { spawn } from "node:child_process";
import { createServer } from "node:http";
import { mkdtempSync, mkdirSync, existsSync, readFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

const ACQ_JS = join(fileURLToPath(new URL(".", import.meta.url)), "acq.js");

function makeHome() {
  const home = mkdtempSync(join(tmpdir(), "acq-join-test-"));
  mkdirSync(join(home, ".config", "acq"), { recursive: true });
  return home;
}
const sessPath = home => join(home, ".config", "acq", "session.json");

// Stub ACQ server: records the exact request path of each join attempt and
// answers a fixed seat-B payload. It deliberately does NO UIN normalization
// of its own, so the path it records is exactly what the CLI sent.
const seen = [];
function startStub() {
  const srv = createServer((req, res) => {
    seen.push(req.url);
    const known = /^\/room\/[^/]+\/join$/.test(req.url) && !req.url.startsWith("/room/world/");
    if (req.method === "POST" && known) {
      res.writeHead(200, { "Content-Type": "application/json" });
      res.end(JSON.stringify({ token: "tokB", expiresAt: Date.now() + 86400000 }));
    } else {
      res.writeHead(404, { "Content-Type": "application/json" });
      res.end(JSON.stringify({ detail: "room not found" }));
    }
  });
  return new Promise(r => srv.listen(0, "127.0.0.1", () => r(srv)));
}

function runCli(home, base, args) {
  return new Promise(resolve => {
    const p = spawn("node", [ACQ_JS, ...args], {
      env: { ...process.env, HOME: home, ACQ_SERVER: base },
      stdio: ["ignore", "pipe", "pipe"],
    });
    let out = "", err = "";
    p.stdout.on("data", d => out += d);
    p.stderr.on("data", d => err += d);
    p.on("exit", code => resolve({ code, out, err }));
  });
}

let passed = 0, failed = 0;
function check(name, cond, detail) {
  if (cond) { passed++; console.log("ok -", name); }
  else { failed++; console.log("not ok -", name, detail === undefined ? "" : `(${detail})`); }
}

const UIN = "QGZ9-ZETS-ESBDP";
const stub = await startStub();
const base = `http://127.0.0.1:${stub.address().port}`;

async function joinCase(name, arg, expectPath) {
  const home = makeHome();
  const before = seen.length;
  const r = await runCli(home, base, ["join", arg]);
  const got = seen.slice(before);
  check(`${name}: exits 0`, r.code === 0, `code=${r.code} err=${r.err.trim()}`);
  check(`${name}: exactly one request, to the bare-UIN endpoint`,
    got.length === 1 && got[0] === expectPath, JSON.stringify(got));
  const sess = existsSync(sessPath(home)) ? JSON.parse(readFileSync(sessPath(home), "utf8")) : null;
  check(`${name}: session stores the UIN, not the paste`,
    sess && sess.room === UIN && sess.seat === "B", sess && `room=${sess.room}`);
}

// 1. Control: the bare UIN passes through byte-identical (server owns
//    UIN normalization; the CLI must not touch it).
await joinCase("bare UIN", UIN, `/room/${UIN}/join`);
// 2-3. The full invite link, with and without a trailing slash - the exact
//    forms the README and the room page hand out.
await joinCase("invite link", `https://acq.damri.org/room/${UIN}`, `/room/${UIN}/join`);
await joinCase("invite link, trailing slash", `https://acq.damri.org/room/${UIN}/`, `/room/${UIN}/join`);
// 4. Schemeless host/path paste.
await joinCase("schemeless link", `acq.damri.org/room/${UIN}`, `/room/${UIN}/join`);
// 5. Path-only paste.
await joinCase("path only", `/room/${UIN}`, `/room/${UIN}/join`);
// 6. Link carrying a tracking query - paste from a chat or a logged page.
await joinCase("link with query", `https://acq.damri.org/room/${UIN}?from=chat`, `/room/${UIN}/join`);
// 7. The join command URL copied whole from docs.
await joinCase("link ending /join", `https://acq.damri.org/room/${UIN}/join`, `/room/${UIN}/join`);
// 8. Lowercase bare UIN passes through UNCHANGED: case folding is the
//    server's documented job, doubling it client-side would fork the rules.
{
  const home = makeHome();
  const before = seen.length;
  const low = UIN.toLowerCase();
  const r = await runCli(home, base, ["join", low]);
  const got = seen.slice(before);
  check("lowercase UIN: passed through byte-identical",
    r.code === 0 && got.length === 1 && got[0] === `/room/${low}/join`, JSON.stringify(got));
}
// 9. Unhyphenated bare UIN also passes through unchanged.
{
  const home = makeHome();
  const before = seen.length;
  const flat = UIN.replace(/-/g, "");
  const r = await runCli(home, base, ["join", flat]);
  const got = seen.slice(before);
  check("unhyphenated UIN: passed through byte-identical",
    r.code === 0 && got.length === 1 && got[0] === `/room/${flat}/join`, JSON.stringify(got));
}
// 10. A URL with no /room/ segment falls back to its last segment and the
//     server stays the arbiter: here the stub 404s it, the CLI reports the
//     failure and - critically - writes NO session state.
{
  const home = makeHome();
  const before = seen.length;
  const r = await runCli(home, base, ["join", "https://example.com/hello/world"]);
  const got = seen.slice(before);
  check("non-room URL: one request, last segment as candidate",
    got.length === 1 && got[0] === "/room/world/join", JSON.stringify(got));
  check("non-room URL: nonzero exit, error printed",
    r.code !== 0 && /404|error/.test(r.err + r.out), `code=${r.code} err=${r.err.trim()}`);
  check("non-room URL: no session file written", !existsSync(sessPath(home)));
}
// 11. Missing argument still fails with usage, no request at all.
{
  const home = makeHome();
  const before = seen.length;
  const r = await runCli(home, base, ["join"]);
  check("no argument: usage error, zero requests",
    r.code !== 0 && /usage/.test(r.err) && seen.length === before,
    `code=${r.code} err=${r.err.trim()} reqs=${seen.length - before}`);
}

stub.close();
console.log(`${passed} passed, ${failed} failed`);
process.exit(failed ? 1 : 0);
