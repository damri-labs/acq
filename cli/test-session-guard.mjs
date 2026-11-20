// Tests for the session-replace preflight guard: exactly one session is
// stored, so a successful new/join for a DIFFERENT room replaces the
// previous room's only local credential - possibly while that room is
// still live, or ended with its transcript still readable until expiry.
// Switching must be deliberate.
//
// Invariants under test:
//  1. new/join with a stored session for a different room REFUSE before
//     any network call (the mock server records zero requests), exit 1,
//     and the refusal names the stored room and --replace.
//  2. --replace opts in: the call proceeds, the warning names the loss
//     including the ended-room transcript case, and the session switches.
//  3. NO same-room exemption: joining the SAME stored room is a real
//     credential loss (the server accepts the join - seat B is free in an
//     owner-only room - and the owner token + recovery secret would be
//     discarded for a seat-B token), so it refuses without --replace and
//     proceeds with it. The guard cannot distinguish servers either:
//     same UIN against a DIFFERENT ACQ_SERVER is still a stored credential
//     being replaced, so the conservative refusal applies there too.
//  4. No stored session: new proceeds without any flag.
//  5. Reads/sends are never guarded: recv works with a stored session
//     and no --replace.
//  6. A corrupt session file still fails visibly before any network call
//     (pre-existing behavior the guard must not mask).
//  7. Refusal leaves the stored session byte-identical.
//  8. A tokenless session that still carries a credential (recovery
//     secret) is guarded exactly like a tokened one.
//
// The CLI under test defaults to the sibling acq.js; set
// ACQ_CLI_UNDER_TEST to an absolute path to run the same checks against
// another artifact (test-pinned-cli-behavior.mjs uses this to exercise
// the immutable pinned download instead of the working tree).
// Run: node test-session-guard.mjs
import { spawn } from "node:child_process";
import { createServer } from "node:http";
import { mkdtempSync, writeFileSync, readFileSync, mkdirSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

const CLI = process.env.ACQ_CLI_UNDER_TEST || join(fileURLToPath(new URL(".", import.meta.url)), "acq.js");
const UIN1 = "AAAA-BBBB-0001", UIN2 = "CCCC-DDDD-0002";

let requests = 0;
const server = createServer((req, res) => {
  requests++;
  const json = (code, obj) => { res.writeHead(code, { "content-type": "application/json" }); res.end(JSON.stringify(obj)); };
  if (req.method === "POST" && req.url === "/room") {
    return json(200, { roomId: UIN2, token: "tok-new-2", joinBy: Math.floor(Date.now() / 1000) + 7200 });
  }
  if (req.method === "POST" && req.url === `/room/${UIN2}/join`) {
    return json(200, { token: "tok-join-2", expiresAt: Math.floor(Date.now() / 1000) + 7200 });
  }
  if (req.method === "POST" && req.url === `/room/${UIN1}/join`) {
    return json(200, { token: "tok-join-1-again", expiresAt: Math.floor(Date.now() / 1000) + 7200 });
  }
  if (req.method === "GET" && req.url.startsWith(`/room/${UIN1}/msg`)) {
    return json(200, { messages: [], cursor: 0, peerJoined: false, expiresAt: Math.floor(Date.now() / 1000) + 7200 });
  }
  json(404, { detail: "nope" });
});

await new Promise(r => server.listen(0, "127.0.0.1", r));
const BASE = `http://127.0.0.1:${server.address().port}`;

let failures = 0;
function check(name, cond, extra) {
  if (cond) { console.log(`ok - ${name}`); }
  else { failures++; console.log(`FAIL - ${name}${extra ? " :: " + extra : ""}`); }
}

function run(home, args) {
  return new Promise((resolve) => {
    const p = spawn(process.execPath, [CLI, ...args], { env: { ...process.env, HOME: home, ACQ_SERVER: BASE } });
    let out = "", err = "";
    p.stdout.on("data", d => out += d);
    p.stderr.on("data", d => err += d);
    p.on("close", code => resolve({ code, out, err }));
  });
}

function freshHome() { return mkdtempSync(join(tmpdir(), "acq-guard-")); }
const sessFile = home => join(home, ".config", "acq", "session.json");
function seed(home) {
  mkdirSync(join(home, ".config", "acq"), { recursive: true });
  const s = { room: UIN1, token: "tok-room-1", seat: "A", credential: "acq1_dGhpcyBpcyBub3QgYSByZWFsIGNyZWRlbnRpYWw" };
  writeFileSync(sessFile(home), JSON.stringify(s, null, 2), { mode: 0o600 });
  return JSON.stringify(s, null, 2);
}

// 1+7. new refuses before any network call; stored session untouched.
{
  const home = freshHome(); const before = seed(home);
  const n0 = requests;
  const r = await run(home, ["new"]);
  check("new with stored session refuses (exit 1)", r.code === 1, r.out + r.err);
  check("refusal made NO network call", requests === n0, `requests went ${n0}->${requests}`);
  check("refusal names stored room and --replace", r.err.includes(UIN1) && r.err.includes("--replace"), r.err);
  check("refusal says nothing was created remotely", /nothing was created or claimed/i.test(r.err), r.err);
  check("stored session byte-identical after refusal", readFileSync(sessFile(home), "utf8") === before);
}

// 1b. join for a DIFFERENT room refuses the same way.
{
  const home = freshHome(); seed(home);
  const n0 = requests;
  const r = await run(home, ["join", UIN2]);
  check("join different room refuses (exit 1)", r.code === 1, r.out + r.err);
  check("join refusal made NO network call", requests === n0, `requests went ${n0}->${requests}`);
  check("join refusal names stored room and --replace", r.err.includes(UIN1) && r.err.includes("--replace"), r.err);
}

// 2. --replace proceeds with a complete loss warning.
{
  const home = freshHome(); seed(home);
  const r = await run(home, ["new", "--replace"]);
  check("new --replace proceeds (exit 0)", r.code === 0, r.out + r.err);
  check("warning names the replaced room", r.err.includes(UIN1), r.err);
  check("warning covers still-live loss", /still live/i.test(r.err), r.err);
  check("warning covers ended-room transcript utility", /transcript stays readable until the room expires/i.test(r.err), r.err);
  const now = JSON.parse(readFileSync(sessFile(home), "utf8"));
  check("session switched to the new room", now.room === UIN2 && now.token === "tok-new-2");
}

// 2b. join --replace switches too.
{
  const home = freshHome(); seed(home);
  const r = await run(home, ["join", UIN2, "--replace"]);
  check("join --replace proceeds (exit 0)", r.code === 0, r.out + r.err);
  const now = JSON.parse(readFileSync(sessFile(home), "utf8"));
  check("join --replace switched the session", now.room === UIN2 && now.token === "tok-join-2");
}

// 3a. SAME stored room: join refuses without --replace (joining your own
// owner room claims seat B and discards the owner credential - the loss
// this guard exists to prevent). No network call is made.
{
  const home = freshHome(); seed(home);
  const n0 = requests;
  const r = await run(home, ["join", UIN1]);
  check("same-room join refuses without --replace (exit 1)", r.code === 1, r.out + r.err);
  check("same-room refusal made NO network call", requests === n0, `requests went ${n0}->${requests}`);
  check("same-room refusal names stored room and --replace", r.err.includes(UIN1) && r.err.includes("--replace"), r.err);
}

// 3b. SAME stored room with --replace: proceeds with the loss warning.
{
  const home = freshHome(); seed(home);
  const r = await run(home, ["join", UIN1, "--replace"]);
  check("same-room join --replace proceeds (exit 0)", r.code === 0, r.out + r.err);
  check("same-room --replace warns about the discarded credential", /replacing the stored session/.test(r.err), r.err);
}

// 3c. Same UIN, DIFFERENT server (ACQ_SERVER changed): the guard cannot
// verify which server minted the stored room, so the conservative refusal
// applies - same UIN on another server is still a stored credential being
// replaced. Simulated by re-seeding and joining against this mock while
// the stored credential came from "elsewhere" (the CLI records no server,
// which is exactly why the refusal must not trust the UIN).
{
  const home = freshHome(); seed(home);
  const n0 = requests;
  const r = await run(home, ["join", UIN1.toLowerCase().replace(/-/g, "")]); // normalized same UIN
  check("same-UIN (normalized) join refuses without --replace", r.code === 1, r.out + r.err);
  check("normalized same-UIN refusal made NO network call", requests === n0, `requests went ${n0}->${requests}`);
}

// 8. Tokenless session carrying only a credential: still guarded.
{
  const home = freshHome();
  mkdirSync(join(home, ".config", "acq"), { recursive: true });
  writeFileSync(sessFile(home), JSON.stringify({ room: UIN1, seat: "A", credential: "acq1_dGVzdG9ubHljcmVkZW50aWFs" }, null, 2), { mode: 0o600 });
  const n0 = requests;
  const r = await run(home, ["new"]);
  check("tokenless credential-only session is guarded (exit 1)", r.code === 1, r.out + r.err);
  check("tokenless guard made NO network call", requests === n0, `requests went ${n0}->${requests}`);
}

// 4. No stored session: new proceeds bare.
{
  const home = freshHome();
  const r = await run(home, ["new"]);
  check("new with empty state proceeds (exit 0)", r.code === 0, r.out + r.err);
  check("no refusal text on first use", !/refusing to replace/.test(r.err), r.err);
}

// 5. recv with a stored session is never guarded.
{
  const home = freshHome(); seed(home);
  const r = await run(home, ["recv", "--wait", "0"]);
  check("recv unaffected by guard (exit 0)", r.code === 0, r.out + r.err);
}

// 6. Corrupt session fails visibly, before any network call.
{
  const home = freshHome();
  mkdirSync(join(home, ".config", "acq"), { recursive: true });
  writeFileSync(sessFile(home), "{ not json", { mode: 0o600 });
  const n0 = requests;
  const r = await run(home, ["new"]);
  check("corrupt session refuses (exit nonzero)", r.code !== 0, r.out + r.err);
  check("corrupt-session error is the visible one", /corrupt state file/.test(r.err), r.err);
  check("corrupt refusal made NO network call", requests === n0, `requests went ${n0}->${requests}`);
}

server.close();
console.log(failures === 0 ? "all session-guard checks passed" : `${failures} check(s) FAILED`);
process.exit(failures === 0 ? 0 : 1);
