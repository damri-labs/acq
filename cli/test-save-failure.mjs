// cli/test-save-failure.mjs - local session-storage failure handling in
// `acq new` / `acq join`. Isolated mock server + isolated HOME dirs only.
//
//   A. preflight refusal: unwritable state dir -> exit 1, ZERO remote calls,
//      clear message, pre-existing session untouched.
//   B. post-commit residual: the server chmods the state dir to read-only
//      when it receives the request (a real change between probe and save) -
//      the CLI must admit the remote commitment, name the consequence, give
//      honest guidance, and print NO token/credential.
//   C. positive control: writable dir -> success, session saved, no probe
//      file left behind.
//
// The mock server runs in a CHILD copy of this file, because spawnSync in
// the parent would deadlock an in-process server. The child reads the
// chmod-target path from a control file on each request; the parent
// writes/clears that file around the runs that need it.
import { spawnSync, spawn } from "node:child_process";
import http from "node:http";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

const PORT = 8124;
const SELF = fileURLToPath(import.meta.url);

if (process.env.SF_SERVER_CHILD) {
  // ---- child: mock ACQ server ----
  const ctl = process.env.SF_SERVER_CHILD;
  let creates = 0;
  const rooms = new Map();
  http.createServer((req, res) => {
    let b = ""; req.on("data", c => b += c); req.on("end", () => {
      const j = (code, obj) => { res.writeHead(code, { "content-type": "application/json" }); res.end(JSON.stringify(obj)); };
      // chmod only on the mutating calls the tests target - never on the
      // parent's own GET /counts bookkeeping reads.
      let target = null;
      if (req.method === "POST") {
        try { target = fs.readFileSync(ctl, "utf8").trim() || null; } catch {}
        if (target) { try { fs.chmodSync(target, 0o500); } catch {} }
      }
      if (req.method === "POST" && req.url === "/room") {
        creates++;
        const uin = `SFRoom${creates}-AAAAA-AAAAA`;
        rooms.set(uin, { joined: false });
        return j(200, { roomId: uin, token: `SECRET_tokA_${creates}`, pairCode: "654321",
          credential: "acq1_" + Buffer.from(JSON.stringify({ uin, token: `SECRET_tokA_${creates}`, pairCode: "654321", seat: "A" })).toString("base64url"),
          joinBy: Math.floor(Date.now() / 1000) + 7200, expiresAt: Math.floor(Date.now() / 1000) + 86400, share: "s" });
      }
      const m = req.url.match(/^\/room\/([^/]+)\/join$/);
      if (req.method === "POST" && m) {
        const r = rooms.get(m[1]);
        if (!r) return j(404, { detail: "unknown room" });
        if (r.joined) return j(409, { detail: "room full" });
        r.joined = true;
        return j(200, { roomId: m[1], token: `SECRET_tokB_${creates}`, recoveryCode: "112233",
          credential: "acq1_" + Buffer.from(JSON.stringify({ uin: m[1], token: `SECRET_tokB_${creates}`, recoveryCode: "112233", seat: "B" })).toString("base64url"),
          expiresAt: Math.floor(Date.now() / 1000) + 86400 });
      }
      if (req.method === "GET" && req.url === "/counts") {
        return j(200, { creates, joins: [...rooms.values()].filter(r => r.joined).length });
      }
      j(404, { detail: "nope" });
    });
  }).listen(PORT);
} else {
  // ---- parent: tests ----
  const CLI = path.join(path.dirname(SELF), "acq.js");
  const CTL = `/tmp/acq-sf-chmod-${process.pid}`;
  const child = spawn("node", [SELF], { env: { ...process.env, SF_SERVER_CHILD: CTL }, stdio: "ignore" });
  const counts = async () => (await (await fetch(`http://127.0.0.1:${PORT}/counts`)).json());
  await new Promise(r => setTimeout(r, 700));

  let failures = 0;
  const check = (name, ok, detail) => { if (ok) console.log(`ok - ${name}`); else { failures++; console.log(`FAIL - ${name}: ${detail}`); } };
  const mkhome = () => fs.mkdtempSync(path.join(os.tmpdir(), "acq-sf-"));
  const run = (home, args) => spawnSync("node", [CLI, ...args],
    { env: { ...process.env, HOME: home, ACQ_SERVER: `http://127.0.0.1:${PORT}` }, encoding: "utf8" });
  const noSecrets = s => !/SECRET_tok|acq1_|654321|112233/.test(s);

  // A1: preflight refusal on `new` - unwritable dir, pre-existing session preserved
  {
    const h = mkhome(), dir = path.join(h, ".config", "acq");
    fs.mkdirSync(dir, { recursive: true, mode: 0o700 });
    const prior = JSON.stringify({ room: "PRIOR-ROOM-AAAAA", token: "prior", seat: "A" });
    fs.writeFileSync(path.join(dir, "session.json"), prior, { mode: 0o600 });
    fs.chmodSync(dir, 0o500);
    // --replace passes the session-replace guard (stacked trees run it
    // first) so this suite reaches the storage probe it targets.
    const before = (await counts()).creates;
    const r = run(h, ["new", "--replace"]);
    fs.chmodSync(dir, 0o700);
    check("A1: new refuses pre-remote on unwritable dir, exit 1", r.status === 1, `code=${r.status} err=${(r.stderr || "").slice(0, 200)}`);
    check("A1: message names the storage problem and that no room was created", /cannot save a session locally/.test(r.stderr) && /no room was created/.test(r.stderr), r.stderr.slice(0, 300));
    check("A1: zero remote creates", (await counts()).creates === before, `creates=${(await counts()).creates} before=${before}`);
    check("A1: pre-existing session untouched", fs.readFileSync(path.join(dir, "session.json"), "utf8") === prior, "");
    check("A1: no raw stack trace", !/node:fs|at writePrivate/.test(r.stderr), r.stderr.slice(0, 200));
  }
  // A2: preflight refusal on `join` - zero remote joins
  {
    const h = mkhome(), dir = path.join(h, ".config", "acq");
    fs.mkdirSync(dir, { recursive: true }); fs.chmodSync(dir, 0o500);
    const bj = (await counts()).joins;
    const r = run(h, ["join", "SFRoom1-AAAAA-AAAAA"]);
    fs.chmodSync(dir, 0o700);
    check("A2: join refuses pre-remote, exit 1, says no join was attempted", r.status === 1 && /no join was attempted/.test(r.stderr), `code=${r.status} err=${(r.stderr || "").slice(0, 200)}`);
    check("A2: zero remote joins", (await counts()).joins === bj, "");
  }
  // B1: post-commit save failure on `new` (dir turns read-only when the request lands)
  {
    const h = mkhome(), dir = path.join(h, ".config", "acq");
    fs.mkdirSync(dir, { recursive: true });
    fs.writeFileSync(CTL, dir);
    const before = (await counts()).creates;
    const r = run(h, ["new"]);
    fs.unlinkSync(CTL); fs.chmodSync(dir, 0o700);
    check("B1: new post-commit failure exits 1 with honest guidance", r.status === 1 && /was created, but the session could not be saved/.test(r.stderr), `code=${r.status} err=${(r.stderr || "").slice(0, 300)}`);
    check("B1: guidance names the orphaned UIN and no-room-recovery", /SFRoom1-AAAAA-AAAAA/.test(r.stderr) && /expires on its own/.test(r.stderr) && /fresh room/.test(r.stderr), r.stderr.slice(0, 400));
    check("B1: exactly one remote create happened", (await counts()).creates === before + 1, "");
    check("B1: NO secret in stdout/stderr", noSecrets(r.stdout + r.stderr), (r.stdout + r.stderr).slice(0, 300));
    // retry after fixing storage: succeeds and creates a fresh room
    const r2 = run(h, ["new"]);
    check("B1: retry after fix succeeds", r2.status === 0 && /Room created/.test(r2.stdout), `code=${r2.status} out=${(r2.stdout || "").slice(0, 150)}`);
    check("B1: retry created exactly one more room (orphan documented, not hidden)", (await counts()).creates === before + 2, "");
  }
  // B2: post-commit save failure on `join` - seat burned, honest 409-ahead warning
  {
    const h = mkhome(), dir = path.join(h, ".config", "acq");
    fs.mkdirSync(dir, { recursive: true });
    const uin = `SFRoom${(await counts()).creates}-AAAAA-AAAAA`; // room from B1 retry, unjoined
    fs.writeFileSync(CTL, dir);
    const bj = (await counts()).joins;
    const r = run(h, ["join", uin]);
    fs.unlinkSync(CTL); fs.chmodSync(dir, 0o700);
    check("B2: join post-commit failure exits 1 with honest guidance", r.status === 1 && /you joined room/.test(r.stderr) && /could not be saved locally/.test(r.stderr), `code=${r.status} err=${(r.stderr || "").slice(0, 300)}`);
    check("B2: guidance warns re-join 409s against this seat and names the remedy", /room full/.test(r.stderr) && /earlier join holds the seat/.test(r.stderr) && /creator/.test(r.stderr), r.stderr.slice(0, 400));
    check("B2: exactly one remote join happened", (await counts()).joins === bj + 1, "");
    check("B2: NO secret in stdout/stderr", noSecrets(r.stdout + r.stderr), (r.stdout + r.stderr).slice(0, 300));
    // the predicted 409 actually happens on retry
    const r2 = run(h, ["join", uin]);
    check("B2: retry join gets the predicted 409 room full", r2.status === 1 && /409/.test(r2.stderr) && /room full/.test(r2.stderr), `code=${r2.status} err=${(r2.stderr || "").slice(0, 150)}`);
  }
  // C: positive control - success path unchanged, no probe litter
  {
    const h = mkhome();
    const r = run(h, ["new"]);
    const dir = path.join(h, ".config", "acq");
    const files = fs.readdirSync(dir).sort();
    check("C: new succeeds on writable dir", r.status === 0 && /Room created/.test(r.stdout), `code=${r.status}`);
    check("C: session saved with seat A credential", JSON.parse(fs.readFileSync(path.join(dir, "session.json"), "utf8")).seat === "A", "");
    check("C: no probe file left behind", files.every(f => !f.startsWith(".probe.")), files.join(","));
  }
  child.kill();
  console.log(failures ? `\n${failures} FAILURES` : "\nall save-failure tests passed");
  process.exit(failures ? 1 : 0);
}
