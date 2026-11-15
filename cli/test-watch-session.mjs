// Integration tests: `acq watch` and `acq recv` must NEVER write the shared
// session file (delivery state lives server-side per seat). Covers Atra's
// interleaved read-then-write repro shape plus watch->recv dedup and recv
// during a concurrent room switch. Run: node test-watch-session.mjs
import { spawn } from "node:child_process";
import { createServer } from "node:http";
import { mkdtempSync, writeFileSync, readFileSync, mkdirSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

const ACQ_JS = join(fileURLToPath(new URL(".", import.meta.url)), "acq.js");
const sleep = ms => new Promise(r => setTimeout(r, ms));

function makeHome() {
  const home = mkdtempSync(join(tmpdir(), "acq-test-"));
  mkdirSync(join(home, ".config", "acq"), { recursive: true });
  return home;
}
const sessPath = home => join(home, ".config", "acq", "session.json");
const writeSession = (home, s) => writeFileSync(sessPath(home), JSON.stringify(s));
const readSessionRaw = home => readFileSync(sessPath(home), "utf8");

// Stub ACQ server with scripted, gated responses and request recording.
function startStub() {
  const gates = {};   // path-prefix -> release fn
  const queries = []; // recorded request urls
  const body = { stream: "event: peer_joined\ndata: {}\n\n" };
  const srv = createServer((req, res) => {
    queries.push(req.url);
    const key = req.url.startsWith("/room/OLD/stream") ? "stream" : "msg";
    (gates[key] || Promise.resolve()).then(() => {
      if (key === "stream") {
        res.writeHead(200, { "Content-Type": "text/event-stream" });
        res.end(body.stream);
      } else {
        res.writeHead(200, { "Content-Type": "application/json" });
        res.end(JSON.stringify({ messages: [{ seq: 7, from: "B", ts: 1, text: "hi", end: false }], cursor: 7, peerJoined: true, expiresAt: 0, ended: false }));
      }
    });
  });
  return new Promise(res => srv.listen(0, "127.0.0.1", () => res({
    port: srv.address().port,
    close: () => srv.close(),
    queries,
    hold: key => { gates[key] = new Promise(() => {}); },        // never resolves until release
    arm: key => { let r; gates[key] = new Promise(res2 => { r = res2; }); gates[key].release = r; },
    release: key => { const g = gates[key]; gates[key] = null; g && g.release && g.release(); },
    saw: prefix => queries.some(u => u.startsWith(prefix)),
    setStreamBody: b => { body.stream = b; },
  })));
}

function run(home, port, argv) {
  const p = spawn(process.execPath, [ACQ_JS, ...argv], {
    env: { ...process.env, HOME: home, ACQ_SERVER: `http://127.0.0.1:${port}` },
    stdio: ["ignore", "pipe", "pipe"],
  });
  let out = "", err = "";
  p.stdout.on("data", d => { out += d; });
  p.stderr.on("data", d => { err += d; });
  return new Promise((res, rej) => { p.on("error", rej); p.on("exit", code => res({ code, out, err })); });
}

let failures = 0;
function check(name, cond, extra) {
  if (cond) console.log(`ok - ${name}`);
  else { failures++; console.error(`FAIL - ${name}${extra ? ": " + extra : ""}`); }
}

// Case 1 (Atra's interleave): watch blocks on OLD; a concurrent `acq new`
// writes NEW while watch is parked; on wake, watch must not touch the file.
{
  const home = makeHome();
  const before = JSON.stringify({ room: "NEW", token: "tok-new", seat: "A" });
  writeSession(home, { room: "OLD", token: "tok-old", seat: "A" });
  const stub = await startStub();
  stub.arm("stream");
  const w = run(home, stub.port, ["watch"]);
  while (!stub.saw("/room/OLD/stream")) await sleep(10);
  writeFileSync(sessPath(home), before);           // concurrent acq new
  stub.release("stream");
  const { code } = await w;
  check("watch wrote nothing over a concurrent session", readSessionRaw(home) === before, readSessionRaw(home));
  check("watch exited 0 on peer_joined", code === 0, `exit=${code}`);
  stub.close();
}

// Case 2: watch consumed a message server-side; a following recv must send
// NO `since` (server cursor owns dedup) and write nothing.
{
  const home = makeHome();
  const before = JSON.stringify({ room: "OLD", token: "tok-old", seat: "A" });
  writeFileSync(sessPath(home), before);
  const stub = await startStub();
  stub.setStreamBody('data: {"seq":7,"from":"B","ts":1,"text":"hi","end":false}\n\n');
  const w = await run(home, stub.port, ["watch"]);
  check("watch woke on message", w.code === 0 && w.out.includes("<acq-inbound"), `exit=${w.code} ${w.out}`);
  const r = await run(home, stub.port, ["recv", "--wait", "0"]);
  const recvQuery = stub.queries.find(u => u.includes("/msg"));
  check("recv sends no since param", recvQuery && !recvQuery.includes("since="), recvQuery);
  check("recv prints server-delivered messages", r.out.includes("<acq-inbound"), r.out);
  check("recv wrote nothing", readSessionRaw(home) === before, readSessionRaw(home));
  stub.close();
}

// Case 3: recv parked on a long wait while the session flips rooms; the
// late reply prints, and the NEW session is untouched.
{
  const home = makeHome();
  const before = JSON.stringify({ room: "NEW", token: "tok-new", seat: "A" });
  writeSession(home, { room: "OLD", token: "tok-old", seat: "A" });
  const stub = await startStub();
  stub.arm("msg");
  const w = run(home, stub.port, ["recv", "--wait", "120"]);
  while (!stub.saw("/room/OLD/msg")) await sleep(10);
  writeFileSync(sessPath(home), before);           // concurrent acq join
  stub.release("msg");
  const { code, out } = await w;
  check("recv printed the late message", out.includes("<acq-inbound"), out);
  check("recv during room switch wrote nothing", readSessionRaw(home) === before, readSessionRaw(home));
  check("recv exited 0", code === 0, `exit=${code}`);
  stub.close();
}

process.exit(failures ? 1 : 0);
