// Integration tests for the acq CLI's read paths.
//
// Invariants under test:
//  1. watch/recv NEVER write the shared session file (owned by new/join) -
//     a concurrent room switch while a read is parked can never be clobbered.
//  2. Reads are replay-safe: new/join seed a per-room cursor, each read
//     passes it as `since` and saves the response cursor, so a response lost
//     after the server committed its delivery cursor is recovered by simply
//     re-running the command. watch/recv dedup through the same cursor file.
//  3. Legacy rooms (no seeded cursor, e.g. created before cursors existed)
//     get exactly one server-tracked read; that read's lost-response window
//     is documented (case 5 pins it as known behavior).
// Run: node test-watch-session.mjs
import { spawn } from "node:child_process";
import { createServer } from "node:http";
import { mkdtempSync, writeFileSync, readFileSync, mkdirSync, existsSync } from "node:fs";
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
const cursPath = home => join(home, ".config", "acq", "cursors.json");
const writeSession = (home, s) => writeFileSync(sessPath(home), JSON.stringify(s));
const readSessionRaw = home => readFileSync(sessPath(home), "utf8");
const readCursors = home => (existsSync(cursPath(home)) ? JSON.parse(readFileSync(cursPath(home), "utf8")) : {});

// Stub ACQ server: since-aware msg endpoint emulating the server's real
// semantics (a no-since read advances the server-side delivery cursor when
// the response is generated), a scriptable message list, a stream endpoint,
// gates, and an armed "drop the next fully-generated response" control.
function startStub() {
  const gates = {};   // path-prefix -> release fn
  const queries = []; // recorded request urls
  const st = {
    msgs: [],           // {seq, from, ts, text, end}
    delivered: 0,       // server-side no-since delivery cursor
    dropNext: 0,        // armed response drops
    streamBody: "event: peer_joined\ndata: {}\n\n",
    streamBodies: [],   // queued per-request stream bodies (override streamBody)
    msgStatuses: [],    // queued per-request msg HTTP statuses (override 200)
    ended: false,       // ended flag in msg responses
    times: { msg: [], stream: [] }, // request arrival times (ms)
  };
  const srv = createServer((req, res) => {
    queries.push(req.url);
    const u = new URL(req.url, "http://x");
    const isStream = u.pathname.endsWith("/stream");
    const key = isStream ? "stream" : "msg";
    st.times[key].push(Date.now());
    (gates[key] || Promise.resolve()).then(() => {
      if (isStream) {
        res.writeHead(200, { "Content-Type": "text/event-stream" });
        res.end(st.streamBodies.length ? st.streamBodies.shift() : st.streamBody);
        return;
      }
      if (st.msgStatuses.length) {
        const s = st.msgStatuses.shift();
        const detail = s === 410 ? "room expired" : s === 404 ? "room not found" : "boom";
        res.writeHead(s, { "Content-Type": "application/json" });
        res.end(JSON.stringify({ detail }));
        return;
      }
      const sinceP = u.searchParams.get("since");
      const from = sinceP === null ? st.delivered : parseInt(sinceP, 10) || 0;
      const out = st.msgs.filter(m => m.seq > from);
      const cursor = st.msgs.length ? st.msgs[st.msgs.length - 1].seq : 0;
      // Server semantics: the delivery cursor advances when the response is
      // generated - explicit-since replays included - before anything is
      // written to the socket.
      if (cursor > st.delivered) st.delivered = cursor;
      const body = JSON.stringify({ messages: out, cursor, peerJoined: true, expiresAt: 0, ended: st.ended });
      if (st.dropNext > 0) {
        st.dropNext--;
        req.socket.destroy(); // response lost after the server committed
        return;
      }
      res.writeHead(200, { "Content-Type": "application/json" });
      res.end(body);
    });
  });
  return new Promise(res => srv.listen(0, "127.0.0.1", () => res({
    port: srv.address().port,
    close: () => srv.close(),
    queries,
    st,
    addMsg: (seq, text) => { st.msgs.push({ seq, from: "B", ts: 1, text, end: false }); st.msgs.sort((a, b) => a.seq - b.seq); },
    dropNext: () => { st.dropNext++; },
    hold: key => { gates[key] = new Promise(() => {}); },
    arm: key => { let r; gates[key] = new Promise(res2 => { r = res2; }); gates[key].release = r; },
    release: key => { const g = gates[key]; gates[key] = null; g && g.release && g.release(); },
    saw: prefix => queries.some(u => u.startsWith(prefix)),
    sawMatch: re => queries.some(u => re.test(u)),
    setStreamBody: b => { st.streamBody = b; },
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
// writes NEW while watch is parked; on wake, watch must not touch the
// session file.
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
  check("watch cursor write went to OLD, not the session", readCursors(home).OLD !== undefined, JSON.stringify(readCursors(home)));
  stub.close();
}

// Case 2: watch wakes on a stream event and prints the message through the
// replay-safe read; a following recv dedups via the saved cursor (sends
// since=<cursor>, prints nothing new) and never writes the session file.
{
  const home = makeHome();
  const before = JSON.stringify({ room: "OLD", token: "tok-old", seat: "A" });
  writeSession(home, { room: "OLD", token: "tok-old", seat: "A" });
  const stub = await startStub();
  stub.arm("stream");
  const w = run(home, stub.port, ["watch"]);
  while (!stub.saw("/room/OLD/stream")) await sleep(10);
  stub.addMsg(8, "hi");
  stub.setStreamBody('data: {"seq":8,"from":"B","ts":1,"text":"hi","end":false}\n\n');
  stub.release("stream");
  const wr = await w;
  check("watch woke on message", wr.code === 0 && wr.out.includes("<acq-inbound") && wr.out.includes("hi"), `exit=${wr.code} ${wr.out}`);
  check("watch saved the cursor", readCursors(home).OLD === 8, JSON.stringify(readCursors(home)));
  const r = await run(home, stub.port, ["recv", "--wait", "0"]);
  const recvQuery = [...stub.queries].reverse().find(u => u.includes("/msg") && u.includes("wait=0") && u.includes("since=")); // the recv --wait 0 run above is the last msg request; earlier ones are watch's own reads
  check("recv passes the saved cursor as since", !!recvQuery && recvQuery.includes("since=8"), recvQuery || "(none)");
  check("recv dedups what watch already printed", !r.out.includes("<acq-inbound") && r.out.includes("[no new messages]"), r.out);
  check("recv wrote nothing over the session", readSessionRaw(home) === before, readSessionRaw(home));
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
  stub.addMsg(9, "late");
  stub.release("msg");
  const { code, out } = await w;
  check("recv printed the late message", out.includes("<acq-inbound") && out.includes("late"), out);
  check("recv during room switch wrote nothing over the session", readSessionRaw(home) === before, readSessionRaw(home));
  check("recv saved the cursor for OLD", readCursors(home).OLD === 9, JSON.stringify(readCursors(home)));
  check("recv exited 0", code === 0, `exit=${code}`);
  stub.close();
}

// Case 4 (the defect this change fixes): a recv response lost after the
// server committed delivery is recovered by re-running recv - the seeded
// cursor makes every read an explicit-since replay.
{
  const home = makeHome();
  writeSession(home, { room: "R", token: "tok", seat: "A" });
  writeFileSync(cursPath(home), JSON.stringify({ R: 0 })); // what acq new seeds
  const stub = await startStub();
  stub.addMsg(5, "lost-then-found");
  stub.dropNext();
  const r1 = await run(home, stub.port, ["recv", "--wait", "0"]);
  check("lost response: recv fails honestly", r1.code === 1 && r1.err.includes("nothing was marked read"), `exit=${r1.code} err=${r1.err}`);
  check("lost response: nothing printed", !r1.out.includes("<acq-inbound"), r1.out);
  check("lost response: cursor not advanced locally", readCursors(home).R === 0, JSON.stringify(readCursors(home)));
  const r2 = await run(home, stub.port, ["recv", "--wait", "0"]);
  check("retry re-reads and prints the message", r2.code === 0 && r2.out.includes("lost-then-found"), `exit=${r2.code} ${r2.out}`);
  check("retry saves the cursor", readCursors(home).R === 5, JSON.stringify(readCursors(home)));
  stub.close();
}

// Case 5 (documented residual window, pre-cursor rooms): a legacy room with
// no seeded cursor gets one server-tracked read; if THAT response is lost
// the messages are gone (server cursor already advanced). This pins the
// boundary the README and protocol text describe.
{
  const home = makeHome();
  writeSession(home, { room: "LEG", token: "tok", seat: "A" }); // no cursors.json entry
  const stub = await startStub();
  stub.addMsg(3, "legacy-window");
  stub.dropNext();
  const r1 = await run(home, stub.port, ["recv", "--wait", "0"]);
  check("legacy first read: lost response fails honestly", r1.code === 1, `exit=${r1.code}`);
  const r2 = await run(home, stub.port, ["recv", "--wait", "0"]);
  check("legacy first read: lost message is not recovered (documented window)", !r2.out.includes("legacy-window"), r2.out);
  check("legacy read then seeds the cursor", readCursors(home).LEG !== undefined, JSON.stringify(readCursors(home)));
  stub.close();
}

// Case 6 (the 04:10 defect, terminal): watch on a room the server answers
// 404 for must STOP - exit 2 with [room not found] after exactly one read,
// never poll a dead room, never write cursors or the session.
{
  const home = makeHome();
  const before = JSON.stringify({ room: "GONE", token: "tok", seat: "A" });
  writeSession(home, { room: "GONE", token: "tok", seat: "A" });
  const stub = await startStub();
  stub.st.msgStatuses = [404, 404, 404]; // more queued than may ever be consumed
  const { code, out, err } = await run(home, stub.port, ["watch"]);
  const msgReqs = stub.queries.filter(u => u.includes("/msg")).length;
  check("gone room (404): watch exits 2 (room over)", code === 2, `exit=${code}`);
  check("gone room (404): says [room not found]", out.includes("[room not found]"), out);
  check("gone room (404): no error-line, no retry polling", err === "" && msgReqs === 1, `msgReqs=${msgReqs} err=${err}`);
  check("gone room (404): stream never opened", !stub.saw("/room/GONE/stream"), "");
  check("gone room (404): no cursor write, session untouched", readCursors(home).GONE === undefined && readSessionRaw(home) === before, readSessionRaw(home));
  stub.close();
}

// Case 7: same for 410 (expired, tombstoned server-side).
{
  const home = makeHome();
  writeSession(home, { room: "EXP", token: "tok", seat: "A" });
  const stub = await startStub();
  stub.st.msgStatuses = [410, 410, 410];
  const { code, out, err } = await run(home, stub.port, ["watch"]);
  const msgReqs = stub.queries.filter(u => u.includes("/msg")).length;
  check("expired room (410): watch exits 2", code === 2, `exit=${code}`);
  check("expired room (410): says [room expired]", out.includes("[room expired]"), out);
  check("expired room (410): one read, no retry", err === "" && msgReqs === 1, `msgReqs=${msgReqs} err=${err}`);
  stub.close();
}

// Case 8 (the 04:10 defect, transient): two 500s then a healthy answer must
// NOT kill the watch - it retries and delivers the message.
{
  const home = makeHome();
  writeSession(home, { room: "FLAKY", token: "tok", seat: "A" });
  const stub = await startStub();
  stub.st.msgStatuses = [500, 500];
  stub.addMsg(4, "after the blip");
  const t0 = Date.now();
  const { code, out } = await run(home, stub.port, ["watch"]);
  const msgReqs = stub.queries.filter(u => u.includes("/msg")).length;
  check("transient 5xx: watch survives and exits 0 with the message", code === 0 && out.includes("after the blip"), `exit=${code} ${out}`);
  check("transient 5xx: retried (3 reads)", msgReqs === 3, `msgReqs=${msgReqs}`);
  check("transient 5xx: retries spaced ~5s apart", Date.now() - t0 >= 9000, `${Date.now() - t0}ms`);
  stub.close();
}

// Case 9: stream reports end/expired and the trailing read then answers
// 410 (room expired out from under a parked watch) - exit 2 with the
// stream's reason, not a raw error line.
{
  const home = makeHome();
  writeSession(home, { room: "MID", token: "tok", seat: "A" });
  const stub = await startStub();
  stub.arm("stream");
  const w = run(home, stub.port, ["watch"]);
  while (!stub.saw("/room/MID/stream")) await sleep(10);
  stub.st.msgStatuses = [410]; // only the trailing read gets this
  stub.setStreamBody('event: end\ndata: {"reason":"expired"}\n\n');
  stub.release("stream");
  const { code, out, err } = await w;
  check("mid-watch expiry: exit 2 with [room expired]", code === 2 && out.includes("[room expired]"), `exit=${code} ${out}`);
  check("mid-watch expiry: no raw error line", err === "", err);
  stub.close();
}

// Case 10: a stream that closes with nothing must reconnect after the retry
// delay, never hot-loop.
{
  const home = makeHome();
  writeSession(home, { room: "IDLE", token: "tok", seat: "A" });
  const stub = await startStub();
  stub.st.streamBodies = ["", "event: peer_joined\ndata: {}\n\n"];
  const { code, out } = await run(home, stub.port, ["watch"]);
  const gap = stub.st.times.stream.length >= 2 ? stub.st.times.stream[1] - stub.st.times.stream[0] : 0;
  check("empty stream close: reconnected and woke on peer_joined", code === 0 && out.includes("[peer joined]"), `exit=${code} ${out}`);
  check("empty stream close: reconnect waited ~5s (no hot loop)", gap >= 4000, `gap=${gap}ms`);
  stub.close();
}

process.exit(failures ? 1 : 0);
