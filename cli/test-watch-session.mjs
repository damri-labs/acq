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
//  4. State is owner-only: the session file holds a live seat bearer token,
//     so the state directory is 0700 and both files 0600 - created that way,
//     and pre-existing loose state tightened on access (cases 11-13).
//  5. State files are written atomically (per-writer temp + rename), and a
//     CORRUPT state file fails visibly instead of being silently treated as
//     empty: a silent cursor reset would void replay-safety and could turn a
//     recoverable lost response into an unrecoverable loss (cases 14-17).
//  6. Unknown --flags fail fast and loud before any state or network
//     action: silently ignoring one lets a typo (--wiat 5) or an MCP-only
//     flag (--since) run with defaults - demonstrated as a recv that
//     parked the full 120s default on an unknown flag (case 18). `--`
//     ends flag parsing for dash-leading text.
// Run: node test-watch-session.mjs
import { spawn } from "node:child_process";
import { createServer } from "node:http";
import { mkdtempSync, writeFileSync, readFileSync, mkdirSync, existsSync, statSync, chmodSync, rmSync } from "node:fs";
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
const dirPath = home => join(home, ".config", "acq");
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
    if (req.method === "POST" && u.pathname === "/room") {
      // Minimal POST /room: default joinBy mirrors the real server's 2h
      // unclaimed window; set st.joinBy = null to omit the field entirely.
      const body = { roomId: "R", token: "tok", expiresAt: 0 };
      const jb = st.joinBy === undefined ? Math.floor(Date.now() / 1000) + 7200 : st.joinBy;
      if (jb !== null) body.joinBy = jb;
      res.writeHead(201, { "Content-Type": "application/json" });
      res.end(JSON.stringify(body));
      return;
    }
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

// Case 11: pre-existing loose state (0644 files, 0755 directory - e.g.
// written by an older version or copied in with a permissive umask) is
// tightened on access by any command, without touching content.
{
  const home = makeHome();
  const session = JSON.stringify({ room: "R", token: "tok", seat: "A" });
  writeFileSync(sessPath(home), session, { mode: 0o644 });
  writeFileSync(cursPath(home), JSON.stringify({ R: 3 }), { mode: 0o644 });
  chmodSync(dirPath(home), 0o755);
  const stub = await startStub();
  const r = await run(home, stub.port, ["recv", "--wait", "0"]);
  const mode = p => statSync(p).mode & 0o777;
  check("loose state: recv still works", r.code === 0, `exit=${r.code} err=${r.err}`);
  check("loose state: session file tightened to 0600", mode(sessPath(home)) === 0o600, mode(sessPath(home)).toString(8));
  check("loose state: cursors file tightened to 0600", mode(cursPath(home)) === 0o600, mode(cursPath(home)).toString(8));
  check("loose state: state directory tightened to 0700", mode(dirPath(home)) === 0o700, mode(dirPath(home)).toString(8));
  check("loose state: session content untouched", readSessionRaw(home) === session, readSessionRaw(home));
  stub.close();
}

// Case 12: state files created fresh are owner-only from the start (0600
// file, 0700 directory).
{
  const home = makeHome();
  writeFileSync(sessPath(home), JSON.stringify({ room: "R", token: "tok", seat: "A" }), { mode: 0o600 });
  chmodSync(dirPath(home), 0o700);
  const stub = await startStub();
  const r = await run(home, stub.port, ["recv", "--wait", "0"]);
  const mode = p => statSync(p).mode & 0o777;
  check("fresh state: recv works", r.code === 0, `exit=${r.code} err=${r.err}`);
  check("fresh state: cursors file created 0600", existsSync(cursPath(home)) && mode(cursPath(home)) === 0o600, existsSync(cursPath(home)) ? mode(cursPath(home)).toString(8) : "missing");
  stub.close();
}

// Case 13: an unwritable state path fails honestly - the command exits
// non-zero with an error line instead of silently succeeding while leaving
// state it could not persist or tighten.
{
  const home = makeHome();
  writeSession(home, { room: "R", token: "tok", seat: "A" });
  mkdirSync(cursPath(home)); // corrupt: cursors path occupied by a directory
  const stub = await startStub();
  stub.addMsg(7, "x");
  const r = await run(home, stub.port, ["recv", "--wait", "0"]);
  check("corrupt cursors path: recv exits non-zero", r.code !== 0, `exit=${r.code}`);
  check("corrupt cursors path: error surfaced on stderr", r.err.length > 0, `stderr empty, out=${r.out}`);
  stub.close();
}

// Case 14: a truncated cursors.json (process killed mid-write) must NOT be
// silently treated as "no cursors" - that reset would drop already-generated
// but unprinted messages with no trace. recv must fail visibly, naming the
// file, before any server read.
{
  const home = makeHome();
  writeSession(home, { room: "R", token: "tok", seat: "A" });
  writeFileSync(cursPath(home), '{"R": 5, "oth'); // truncated mid-write
  const stub = await startStub();
  stub.addMsg(6, "must-not-lose");
  const r = await run(home, stub.port, ["recv", "--wait", "0"]);
  check("corrupt cursors: recv exits non-zero", r.code !== 0, `exit=${r.code} out=${r.out}`);
  check("corrupt cursors: error names the file and the consequence", r.err.includes("corrupt state file") && r.err.includes("cursors.json") && r.err.includes("replay-safety"), r.err);
  check("corrupt cursors: no server read on a guessed cursor", !stub.saw("/room/R/msg"), stub.queries.join(","));
  check("corrupt cursors: nothing printed as if healthy", !r.out.includes("[no new messages]"), r.out);
  stub.close();
}

// Case 15: a corrupt session.json (room/seat context unreadable) fails
// visibly instead of the silent "no active room" misdirection.
{
  const home = makeHome();
  writeFileSync(sessPath(home), '{"room": "R", "to');
  const stub = await startStub();
  const r = await run(home, stub.port, ["watch"]);
  check("corrupt session: watch exits non-zero", r.code !== 0, `exit=${r.code}`);
  check("corrupt session: error names the file", r.err.includes("corrupt state file") && r.err.includes("session.json"), r.err);
  check("corrupt session: no server contact on guessed state", stub.queries.length === 0, stub.queries.join(","));
  stub.close();
}

// Case 16: normal saves leave valid JSON and no temp-file debris (the
// atomic write completes the rename).
{
  const home = makeHome();
  writeSession(home, { room: "R", token: "tok", seat: "A" });
  const stub = await startStub();
  stub.addMsg(4, "x");
  const r = await run(home, stub.port, ["recv", "--wait", "0"]);
  const { readdirSync } = await import("node:fs");
  const leftovers = readdirSync(dirPath(home)).filter(f => f.endsWith(".tmp"));
  check("atomic writes: recv works", r.code === 0, `exit=${r.code} err=${r.err}`);
  check("atomic writes: cursors valid JSON after save", readCursors(home).R === 4, JSON.stringify(readCursors(home)));
  check("atomic writes: no .tmp debris left behind", leftovers.length === 0, leftovers.join(","));
  stub.close();
}

// Case 17: the exact syscall sequence of acq.js writePrivate (temp file in
// the same directory, chmod 600, rename) is exercised by two real child
// PROCESSES whose steps are aligned by barrier files, so the interleaving
// is exact, not timing luck. 17a replicates the PRE-FIX shared-temp shape
// and pins the demonstrated tear: one writer renames the shared temp while
// the other still holds it open, and the second writer's late write lands
// in the published file. 17b replicates the shipped per-writer-temp shape:
// each rename publishes one complete payload, last renamer wins whole-file
// (the documented lost-update outcome), never a blend, no debris. The real
// CLI path is covered end-to-end by case 16; this harness exists because
// the two-CLI-process overlap cannot be forced deterministically from the
// command surface.
{
  const dir = mkdtempSync(join(tmpdir(), "acq-race-"));
  const barrier = join(dir, "b"); mkdirSync(barrier);
  const payload1 = JSON.stringify({ roomA: 1, pad: "x".repeat(80) });
  const payload2 = JSON.stringify({ roomB: 2, pad: "y".repeat(400) });
  writeFileSync(join(dir, "p1.json"), payload1);
  writeFileSync(join(dir, "p2.json"), payload2);
  const child = (mode, me, myTmp) => spawn(process.execPath, ["-e", `
    const fs = require("fs");
    const [, mode, target, myTmp, payloadFile, bdir, me] = process.argv; // node -e: argv is [execPath, ...args]
    const payload = fs.readFileSync(payloadFile, "utf8");
    const sig = n => fs.writeFileSync(bdir + "/" + n, "1");
    const nap = ms => Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, ms);
    const wait = n => { const t = Date.now(); while (!fs.existsSync(bdir + "/" + n)) { if (Date.now() - t > 10000) { console.error("timeout " + n); process.exit(3); } nap(5); } };
    if (mode === "shared") {
      if (me === "p1") {
        const fd = fs.openSync(myTmp, "w");          // p1 opens the shared temp first
        sig("p1-open"); wait("p2-renamed");
        fs.writeSync(fd, payload);                    // late write lands in the published inode
        fs.closeSync(fd);
      } else {
        wait("p1-open");
        fs.writeFileSync(myTmp, payload);             // truncate + complete write
        fs.renameSync(myTmp, target);                 // publishes the inode p1 still holds
        sig("p2-renamed");
      }
    } else {
      if (me === "p1") {
        fs.writeFileSync(myTmp, payload); sig("p1-written"); wait("p2-written");
        fs.renameSync(myTmp, target); sig("p1-renamed");
      } else {
        wait("p1-written");
        fs.writeFileSync(myTmp, payload); sig("p2-written"); wait("p1-renamed");
        fs.renameSync(myTmp, target);                 // last renamer wins whole-file
      }
    }
  `, mode, join(dir, "published.json"), myTmp, join(dir, me === "p1" ? "p1.json" : "p2.json"), barrier, me], { stdio: ["ignore", "pipe", "pipe"] });
  const run2 = (mode, tmp1, tmp2) => Promise.all([child(mode, "p1", tmp1), child(mode, "p2", tmp2)].map(
    p => new Promise((res, rej) => { let err = ""; p.stderr.on("data", d => err += d); p.on("error", rej); p.on("exit", c => res({ c, err })); })));
  const readPub = () => readFileSync(join(dir, "published.json"), "utf8");

  // 17a: pre-fix shared-temp shape - the tear, deterministically.
  {
    const shared = join(dir, "shared.tmp");
    const [r1, r2] = await run2("shared", shared, shared);
    const pub = readPub();
    let parses = true; try { JSON.parse(pub); } catch { parses = false; }
    check("shared-temp harness: both writers completed", r1.c === 0 && r2.c === 0, `${r1.c} ${r2.c} ${r1.err} ${r2.err}`);
    check("shared-temp shape (pre-fix): published file is torn, not the last complete payload", !parses && pub !== payload2, pub.slice(0, 120));
    rmSync(join(dir, "published.json"));
  }

  // 17b: shipped per-writer-temp shape - isolation, deterministically.
  {
    const [r1, r2] = await run2("unique", join(dir, "u.111.tmp"), join(dir, "u.222.tmp"));
    const pub = readPub();
    let parsed = null; try { parsed = JSON.parse(pub); } catch {}
    const { readdirSync } = await import("node:fs");
    const debris = readdirSync(dir).filter(f => f.endsWith(".tmp"));
    check("per-writer temps: both writers completed", r1.c === 0 && r2.c === 0, `${r1.c} ${r2.c} ${r1.err} ${r2.err}`);
    check("per-writer temps: published file is one COMPLETE payload (last renamer wins)", pub === payload2, pub.slice(0, 120));
    check("per-writer temps: published file parses", parsed && parsed.roomB === 2, pub.slice(0, 80));
    check("per-writer temps: no temp debris", debris.length === 0, debris.join(","));
  }
  rmSync(dir, { recursive: true, force: true });
}

// Case 18: unknown --flags are rejected before any state or network
// action. `recv --since 0` (a flag that exists only on the MCP tool) once
// parked the full 120s default wait; a typo like --wiat would too.
{
  const home = makeHome();
  const sess = { room: "R", token: "tok", seat: "A" };
  writeSession(home, sess);
  const stub = await startStub();
  const r1 = await run(home, stub.port, ["recv", "--since", "0"]);
  check("unknown flag: recv --since rejected fast, exit 1", r1.code === 1 && /--since/.test(r1.err), `code=${r1.code} err=${r1.err}`);
  const r2 = await run(home, stub.port, ["recv", "--wiat", "5"]);
  check("unknown flag: typo --wiat rejected, exit 1", r2.code === 1 && /--wiat/.test(r2.err), `code=${r2.code} err=${r2.err}`);
  const r3 = await run(home, stub.port, ["send", "never-sent", "--bogus"]);
  check("unknown flag: send --bogus rejected, exit 1", r3.code === 1 && /--bogus/.test(r3.err), `code=${r3.code} err=${r3.err}`);
  check("unknown flags: no request reached the server", stub.queries.length === 0, stub.queries.join(","));
  check("unknown flags: session file untouched", readSessionRaw(home) === JSON.stringify(sess), readSessionRaw(home));
  stub.close();
}

// Case 19: invalid --wait values are rejected before any request, and
// accepted ones reach the server as exact decimal text. Non-numeric,
// negative, exponent or blank input used to land as wait=0 - an instant
// non-blocking read printing "[no new messages]": a typo looking exactly
// like an empty room (reproduced 19.11 against a server built from current
// source: `abc`, `3.7`, `-5`, `1e21` serialized as "1e+21", "" and " "
// each returned immediately). The wire contract: 1-15 integer digits plus
// an optional fractional part that rounds down; 16+ digits are rejected
// rather than losing integer precision as a double (the server would
// saturate such overflow to its cap, silently interpreting a typo).
{
  const home = makeHome();
  const sess = { room: "R", token: "tok", seat: "A" };
  writeSession(home, sess);
  const stub = await startStub();
  const r1 = await run(home, stub.port, ["recv", "--wait", "abc"]);
  check("invalid --wait: non-numeric rejected, exit 1", r1.code === 1 && /--wait/.test(r1.err) && /abc/.test(r1.err), `code=${r1.code} err=${r1.err}`);
  const r2 = await run(home, stub.port, ["recv", "--wait", "-5"]);
  check("invalid --wait: negative rejected, exit 1", r2.code === 1 && /--wait/.test(r2.err), `code=${r2.code} err=${r2.err}`);
  const r3 = await run(home, stub.port, ["recv", "--wait"]);
  check("invalid --wait: missing value rejected, exit 1", r3.code === 1 && /--wait/.test(r3.err), `code=${r3.code} err=${r3.err}`);
  const r4 = await run(home, stub.port, ["recv", "--wait", "1e21"]);
  check("invalid --wait: exponent notation rejected, exit 1", r4.code === 1 && /--wait/.test(r4.err), `code=${r4.code} err=${r4.err}`);
  const r5 = await run(home, stub.port, ["recv", "--wait", ""]);
  check("invalid --wait: blank rejected, exit 1", r5.code === 1 && /--wait/.test(r5.err), `code=${r5.code} err=${r5.err}`);
  const r6 = await run(home, stub.port, ["recv", "--wait", " "]);
  check("invalid --wait: whitespace-only rejected, exit 1", r6.code === 1 && /--wait/.test(r6.err), `code=${r6.code} err=${r6.err}`);
  const r7 = await run(home, stub.port, ["recv", "--wait", "99999999999999999999"]);
  check("invalid --wait: 20-digit value rejected, exit 1", r7.code === 1 && /--wait/.test(r7.err), `code=${r7.code} err=${r7.err}`);
  check("invalid --wait: no request reached the server", stub.queries.length === 0, stub.queries.join(","));
  const r8 = await run(home, stub.port, ["recv", "--wait", "3.7"]);
  check("fractional --wait: floors to whole seconds, reaches server as wait=3", r8.code === 0 && stub.saw("/room/R/msg?wait=3"), `code=${r8.code} queries=${stub.queries.join(",")}`);
  const r9 = await run(home, stub.port, ["recv", "--wait", "999999999999999"]);
  check("15-digit --wait: reaches server as exact plain digits", r9.code === 0 && stub.saw("/room/R/msg?wait=999999999999999"), `code=${r9.code} queries=${stub.queries.join(",")}`);
  const r10 = await run(home, stub.port, ["recv", "--wait", "999999999999999.99"]);
  check("accepted boundary: 15-digit integer part + fraction truncates textually, never rounds up", r10.code === 0 && stub.saw("/room/R/msg?wait=999999999999999") && !stub.saw("/room/R/msg?wait=1000000000000000"), `code=${r10.code} queries=${stub.queries.join(",")}`);
  const r11 = await run(home, stub.port, ["recv", "--wait", "4503599627370495.9"]);
  check("16-integer-digit fraction is outside the grammar: rejected, exit 1", r11.code === 1 && /--wait/.test(r11.err), `code=${r11.code} err=${r11.err}`);
  check("--wait validation: session file untouched", readSessionRaw(home) === JSON.stringify(sess), readSessionRaw(home));
  stub.close();
}

// Case 20: `new` prints the join window from the response's joinBy, not a
// hardcoded number. The server grants 2 hours unclaimed while the CLI once
// printed "valid 10 min" regardless (found 19.11 against a locally built
// server: POST /room returned joinBy = now+120min, the CLI said 10 min).
// A missing/invalid joinBy must print no time claim at all.
{
  const home = makeHome();
  const stub = await startStub();
  const r1 = await run(home, stub.port, ["new"]);
  check("new: join window comes from joinBy (stub grants 120 min)", r1.code === 0 && /one join within ~120 min/.test(r1.out), `code=${r1.code} out=${r1.out}`);
  check("new: no stale 10-minute claim", !/10 min/.test(r1.out), r1.out);
  stub.st.joinBy = null; // server omits the field
  const home2 = makeHome();
  const r2 = await run(home2, stub.port, ["new"]);
  check("new: missing joinBy -> plain one-join text, no NaN/undefined/min", r2.code === 0 && /one join\)/.test(r2.out) && !/NaN|undefined|\d+ min/.test(r2.out), `code=${r2.code} out=${r2.out}`);
  stub.close();
}

process.exit(failures ? 1 : 0);
