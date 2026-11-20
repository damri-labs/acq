// cli/test-guard-plus-preflight.mjs - combined ordering checks for the two
// stacked pre-network refusals in `acq new`/`acq join`: the session-replace
// guard (stored credential protection) runs FIRST, the storage-writability
// probe SECOND, both before any remote call. Mock server in a child copy;
// isolated HOMEs only.
import { spawnSync, spawn } from "node:child_process";
import http from "node:http";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

const PORT = 8125;
const SELF = fileURLToPath(import.meta.url);

if (process.env.SF_SERVER_CHILD) {
  let creates = 0;
  const rooms = new Map();
  http.createServer((req, res) => {
    let b = ""; req.on("data", c => b += c); req.on("end", () => {
      const j = (code, obj) => { res.writeHead(code, { "content-type": "application/json" }); res.end(JSON.stringify(obj)); };
      if (req.method === "POST" && req.url === "/room") {
        creates++;
        const uin = `Combo${creates}-AAAAA-AAAAA`;
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
  const CLI = path.join(path.dirname(SELF), "acq.js");
  const CTL = `/tmp/acq-combo-${process.pid}`;
  const child = spawn("node", [SELF], { env: { ...process.env, SF_SERVER_CHILD: CTL }, stdio: "ignore" });
  const counts = async () => (await (await fetch(`http://127.0.0.1:${PORT}/counts`)).json());
  await new Promise(r => setTimeout(r, 700));

  let failures = 0;
  const check = (name, ok, detail) => { if (ok) console.log(`ok - ${name}`); else { failures++; console.log(`FAIL - ${name}: ${detail}`); } };
  const mkhome = () => fs.mkdtempSync(path.join(os.tmpdir(), "acq-combo-"));
  const run = (home, args) => spawnSync("node", [CLI, ...args],
    { env: { ...process.env, HOME: home, ACQ_SERVER: `http://127.0.0.1:${PORT}` }, encoding: "utf8" });
  const seedSession = dir => {
    fs.mkdirSync(dir, { recursive: true, mode: 0o700 });
    fs.writeFileSync(path.join(dir, "session.json"),
      JSON.stringify({ room: "STORED-ROOM-AAAAA", token: "STOREDSECRET_tok", seat: "A", credential: "acq1_STOREDSECRET" }), { mode: 0o600 });
  };
  const noSecrets = s => !/SECRET_tok|acq1_|654321|112233|STOREDSECRET/.test(s);

  // 1. stored session + unwritable disk, NO --replace: the GUARD wins
  //    (credential protection outranks the storage probe), zero remote.
  {
    const h = mkhome(), dir = path.join(h, ".config", "acq");
    seedSession(dir); fs.chmodSync(dir, 0o500);
    const before = (await counts()).creates;
    const r = run(h, ["new"]);
    fs.chmodSync(dir, 0o700);
    check("1: stored+unwritable without --replace -> guard refusal wins", r.status === 1 && /refusing to replace the stored session/.test(r.stderr), `code=${r.status} err=${(r.stderr||"").slice(0,200)}`);
    check("1: no probe message shown (guard ran first)", !/cannot save a session locally/.test(r.stderr), r.stderr.slice(0, 200));
    check("1: zero remote creates", (await counts()).creates === before, "");
    check("1: stored session untouched", /STORED-ROOM-AAAAA/.test(fs.readFileSync(path.join(dir, "session.json"), "utf8")), "");
    check("1: no secret in output", noSecrets(r.stdout + r.stderr), (r.stdout + r.stderr).slice(0, 200));
  }
  // 2. stored session + unwritable disk + --replace: guard passes with its
  //    warning, the PROBE refuses next, zero remote, stored session kept
  //    (refusal happens before any save, so --replace has not discarded).
  {
    const h = mkhome(), dir = path.join(h, ".config", "acq");
    seedSession(dir); fs.chmodSync(dir, 0o500);
    const before = (await counts()).creates;
    const r = run(h, ["new", "--replace"]);
    fs.chmodSync(dir, 0o700);
    check("2: --replace + unwritable -> probe refusal after guard warning", r.status === 1 && /warning: replacing the stored session/.test(r.stderr) && /cannot save a session locally/.test(r.stderr) && /no room was created/.test(r.stderr), `code=${r.status} err=${(r.stderr||"").slice(0,300)}`);
    check("2: zero remote creates", (await counts()).creates === before, "");
    check("2: stored session STILL intact (refusal preceded any save)", /STORED-ROOM-AAAAA/.test(fs.readFileSync(path.join(dir, "session.json"), "utf8")), "");
    check("2: no secret in output", noSecrets(r.stdout + r.stderr), (r.stdout + r.stderr).slice(0, 200));
  }
  // 3. stored session + WRITABLE disk + --replace: both pass, remote create
  //    happens, old session replaced by the new one.
  {
    const h = mkhome(), dir = path.join(h, ".config", "acq");
    seedSession(dir);
    const before = (await counts()).creates;
    const r = run(h, ["new", "--replace"]);
    const s = JSON.parse(fs.readFileSync(path.join(dir, "session.json"), "utf8"));
    check("3: --replace + writable -> create succeeds, session switched", r.status === 0 && /Room created/.test(r.stdout) && /^Combo\d+-AAAAA-AAAAA$/.test(s.room) && !/STORED-ROOM/.test(s.room), `code=${r.status} room=${s.room}`);
    check("3: exactly one remote create", (await counts()).creates === before + 1, "");
  }
  // 4. join with stored session, no --replace: guard wins, zero remote.
  {
    const h = mkhome(), dir = path.join(h, ".config", "acq");
    seedSession(dir);
    const bj = (await counts()).joins;
    const r = run(h, ["join", "Combo1-AAAAA-AAAAA"]);
    check("4: join with stored session -> guard refusal, zero remote joins", r.status === 1 && /refusing to replace the stored session/.test(r.stderr) && (await counts()).joins === bj, `code=${r.status} err=${(r.stderr||"").slice(0,150)}`);
    check("4: no secret in output", noSecrets(r.stdout + r.stderr), "");
  }
  child.kill();
  console.log(failures ? `\n${failures} FAILURES` : "\nall combined guard+preflight tests passed");
  process.exit(failures ? 1 : 0);
}
