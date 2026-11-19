// Integration tests for the acq CLI's send uncertainty handling.
//
// Invariants under test (a dropped POST /msg response is ambiguous: the
// server stores before answering, so the send may have committed):
//  1. Every send carries a unique cid; verification matches THAT cid only -
//     never text or timing - so a prior identical message cannot produce a
//     false "sent" (controller-reproduced bug in the pre-cid revision).
//  2. Verification outcomes are exactly two: cid present proves acceptance
//     (exit 0, "verified after an ambiguous failure"); anything else is
//     honest outcome-unknown (exit 1). Absence is NEVER reported as
//     safe-to-rerun: the original request may still land later (case 4's
//     delayed commit lands between verification reads).
//  3. ANY answer that is not a clean success - including a JSON body with
//     a detail field - goes through the spaced cid verification reads: the
//     detail text is kept for the report, but its shape alone cannot
//     establish where the answer came from or that nothing was stored
//     (case 7: an intermediary's JSON error after a real commit).
//  4. `acq log --self` exposes message ids for the manual check the
//     outcome-unknown message points to.
// Run: node test-send-verify.mjs
import { spawn } from "node:child_process";
import { createServer } from "node:http";
import { mkdtempSync, mkdirSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

const ACQ_JS = join(fileURLToPath(new URL(".", import.meta.url)), "acq.js");
const sleep = ms => new Promise(r => setTimeout(r, ms));

function makeHome() {
  const home = mkdtempSync(join(tmpdir(), "acq-sendtest-"));
  mkdirSync(join(home, ".config", "acq"), { recursive: true });
  return home;
}

// Stub ACQ server with scriptable send faults. POST /room/ROOM/msg stores
// {seq, from, ts, text, end, cid} exactly like the real server (append
// under a lock, then answer); fault modes decide whether and when the
// append and the answer happen.
function startStub() {
  const st = {
    msgs: [],
    nextSeq: 1,
    mode: "normal",     // normal | commitloss | hangup | delayed | badgateway | ended | jsonerrAfterCommit | trunc2xx
    delayMs: 800,       // delayed mode: commit this long AFTER the client saw failure
    getCounts: { selfPeek: 0 },
  };
  const srv = createServer((req, res) => {
    const u = new URL(req.url, "http://x");
    if (req.method === "GET" && u.pathname.endsWith("/msg")) {
      if (u.searchParams.get("self") === "1" && u.searchParams.get("peek") === "1") st.getCounts.selfPeek++;
      const since = parseInt(u.searchParams.get("since") || "0", 10) || 0;
      const out = st.msgs.filter(m => m.seq > since);
      const cursor = st.msgs.length ? st.msgs[st.msgs.length - 1].seq : 0;
      res.writeHead(200, { "Content-Type": "application/json" });
      res.end(JSON.stringify({ messages: out, cursor, peerJoined: true, expiresAt: 0, ended: false }));
      return;
    }
    if (req.method === "POST" && u.pathname.endsWith("/msg")) {
      let raw = "";
      req.on("data", d => { raw += d; });
      req.on("end", () => {
        const b = JSON.parse(raw);
        const commit = () => {
          const m = { seq: st.nextSeq++, from: "A", ts: Date.now() / 1000, text: b.text, end: !!b.end, cid: b.cid || "" };
          st.msgs.push(m);
          return m;
        };
        switch (st.mode) {
          case "normal": {
            const m = commit();
            res.writeHead(200, { "Content-Type": "application/json" });
            res.end(JSON.stringify({ seq: m.seq, from: m.from, cid: m.cid }));
            return;
          }
          case "commitloss":
            commit();               // stored...
            req.socket.destroy();   // ...but the answer never arrives
            return;
          case "hangup":
            req.socket.destroy();   // never reached the "server"
            return;
          case "delayed":
            req.socket.destroy();   // client fails now...
            setTimeout(commit, st.delayMs); // ...server commits later
            return;
          case "badgateway":
            res.writeHead(502, { "Content-Type": "text/html" });
            res.end("<html>bad gateway</html>");
            return;
          case "jsonerrAfterCommit":
            commit(); // server committed...
            res.writeHead(500, { "Content-Type": "application/json" });
            res.end(JSON.stringify({ detail: "internal proxy failure" })); // ...but the intermediary reports a JSON error anyway
            return;
          case "trunc2xx": {
            const m = commit(); // committed...
            res.writeHead(200, { "Content-Type": "application/json" });
            res.end(JSON.stringify({ seq: m.seq, from: m.from, cid: m.cid }).slice(0, 12)); // ...but the 2xx body arrives cut
            return;
          }
          case "ended":
            res.writeHead(410, { "Content-Type": "application/json" });
            res.end(JSON.stringify({ detail: "room ended" }));
            return;
          // Typed storage outcomes (the SPEC's 500 contract):
          case "unknownBound": {
            // Committed, and the 500 carries this request's exact cid +
            // seq: the typed field binds to the request and verifies it.
            const m = commit();
            res.writeHead(500, { "Content-Type": "application/json" });
            res.end(JSON.stringify({ detail: "sent, but the copy cannot be confirmed; it may be deleted when the server restarts", stored: "unknown", seq: m.seq, cid: m.cid }));
            return;
          }
          case "unknownBare":
            // Committed, but the 500's stored:"unknown" has no cid/seq:
            // no binding, so the transcript check stays the arbiter.
            commit();
            res.writeHead(500, { "Content-Type": "application/json" });
            res.end(JSON.stringify({ detail: "sent, but the copy cannot be confirmed", stored: "unknown" }));
            return;
          case "unknownBareNoCommit":
            // Same unbound claim, but NOTHING committed: the claim alone
            // must never become a definite answer either way.
            res.writeHead(500, { "Content-Type": "application/json" });
            res.end(JSON.stringify({ detail: "sent, but the copy cannot be confirmed", stored: "unknown" }));
            return;
          case "definiteFalse":
            // Definite pre-commit rejection, nothing committed.
            res.writeHead(500, { "Content-Type": "application/json" });
            res.end(JSON.stringify({ detail: "the message was not stored and re-sending as-is is safe", stored: false }));
            return;
          case "definiteFalseAfterCommit":
            // An intermediary fakes a definite rejection over a real
            // commit: the transcript check must correct the typed field.
            commit();
            res.writeHead(500, { "Content-Type": "application/json" });
            res.end(JSON.stringify({ detail: "the message was not stored and re-sending as-is is safe", stored: false }));
            return;
        }
      });
      return;
    }
    res.writeHead(404).end();
  });
  return new Promise(res => srv.listen(0, "127.0.0.1", () => res({ port: srv.address().port, close: () => srv.close(), st })));
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

async function freshRoom() {
  const home = makeHome();
  const stub = await startStub();
  const room = "ROOM-0000-00000";
  const { writeFileSync: wf } = await import("node:fs");
  wf(join(home, ".config", "acq", "session.json"), JSON.stringify({ room, token: "tok-a", seat: "A" }));
  return { home, stub, room };
}

// 1. Normal send: cid round-trips; log --self shows it.
{
  const { home, stub, room } = await freshRoom();
  const r = await run(home, stub.port, ["send", "hello"]);
  check("normal send exits 0 with seq", r.code === 0 && /sent seq=1\b/.test(r.out), `code=${r.code} out=${r.out}`);
  const log = await run(home, stub.port, ["log", "--self"]);
  check("log --self labels own message and shows its id", /\[1\] you: hello \(id [0-9a-f-]{36}\)/.test(log.out), log.out);
  check("stored message carries the cid", /^[0-9a-f-]{36}$/.test(stub.st.msgs[0].cid), stub.st.msgs[0].cid);
  stub.close();
}

// 2. Stored-but-dropped (commitloss): verified via cid, exactly one copy.
{
  const { home, stub } = await freshRoom();
  stub.st.mode = "commitloss";
  const r = await run(home, stub.port, ["send", "dropped"]);
  check("commitloss verifies as sent, exit 0", r.code === 0 && /sent seq=1 \(verified after an ambiguous failure\)/.test(r.out), `code=${r.code} out=${r.out} err=${r.err}`);
  check("commitloss stored exactly one copy", stub.st.msgs.length === 1, JSON.stringify(stub.st.msgs));
  stub.close();
}

// 3. Controller repro: exactly one prior identical message + next send never
// forwarded - must NOT claim the old seq; honest outcome-unknown.
{
  const { home, stub } = await freshRoom();
  const first = await run(home, stub.port, ["send", "same-text"]);
  check("prior identical send lands", first.code === 0, first.err);
  stub.st.mode = "hangup";
  const r = await run(home, stub.port, ["send", "same-text"]);
  check("never-forwarded send is outcome-unknown, exit 1", r.code === 1 && /send outcome unknown/.test(r.err), `code=${r.code} err=${r.err}`);
  check("no false verified-sent claim", !/verified after an ambiguous failure/.test(r.out) && !/^sent seq=/m.test(r.out), r.out);
  check("unknown message carries the request id for manual checking", /\(id [0-9a-f-]{36}\)/.test(r.err), r.err);
  check("no raw fetch stack trace", !/TypeError: fetch failed/.test(r.err), r.err.slice(0, 120));
  check("still exactly one copy stored", stub.st.msgs.length === 1, JSON.stringify(stub.st.msgs));
  const log = await run(home, stub.port, ["log", "--self"]);
  check("log --self still shows only the original", (log.out.match(/same-text/g) || []).length === 1, log.out);
  stub.close();
}

// 4. Delayed commit: the send "fails" client-side, the server commits 800ms
// later - an early verification read misses, a spaced re-read catches it.
{
  const { home, stub } = await freshRoom();
  stub.st.mode = "delayed";
  const t0 = Date.now();
  const r = await run(home, stub.port, ["send", "slow-landing"]);
  check("delayed commit verified once it lands, exit 0", r.code === 0 && /verified after an ambiguous failure/.test(r.out), `code=${r.code} out=${r.out} err=${r.err}`);
  check("verification took more than one read", stub.st.getCounts.selfPeek >= 2, `reads=${stub.st.getCounts.selfPeek} elapsed=${Date.now() - t0}ms`);
  check("delayed commit stored exactly one copy", stub.st.msgs.length === 1, JSON.stringify(stub.st.msgs));
  stub.close();
}

// 5. Non-JSON gateway error (intermediary 502): ambiguous; with a negative
// transcript the received error is reported with storage unconfirmed.
{
  const { home, stub } = await freshRoom();
  stub.st.mode = "badgateway";
  const r = await run(home, stub.port, ["send", "via-dead-proxy"]);
  check("gateway 502 without detail is reported unconfirmed, exit 1", r.code === 1 && /error 502/.test(r.err) && /unconfirmed \(id [0-9a-f-]{36}\)/.test(r.err), `code=${r.code} err=${r.err}`);
  stub.close();
}

// 6. A JSON detail error is reported as received, but shape cannot prove
// origin or non-commit: the same spaced verification runs (negative here),
// and the report keeps the outcome honestly unconfirmed.
{
  const { home, stub } = await freshRoom();
  stub.st.mode = "ended";
  const r = await run(home, stub.port, ["send", "too-late"]);
  check("JSON detail error reported as received, exit 1", r.code === 1 && /error 410: room ended/.test(r.err), `code=${r.code} err=${r.err}`);
  check("JSON error gets the full spaced verification", stub.st.getCounts.selfPeek === 3, `reads=${stub.st.getCounts.selfPeek}`);
  check("report keeps storage unconfirmed with id + arbiter", /unconfirmed \(id [0-9a-f-]{36}\)/.test(r.err) && /acq log --self/.test(r.err), r.err);
  check("no verified-sent claim on the error path", !/verified after an ambiguous failure/.test(r.out), r.out);
  stub.close();
}

// 7. An intermediary CAN return a JSON error after the server committed:
// the cid check corrects the false error instead of trusting its shape.
{
  const { home, stub } = await freshRoom();
  stub.st.mode = "jsonerrAfterCommit";
  const r = await run(home, stub.port, ["send", "committed-anyway"]);
  check("JSON error after commit is corrected to verified-sent, exit 0", r.code === 0 && /verified after an ambiguous failure/.test(r.out), `code=${r.code} out=${r.out} err=${r.err}`);
  check("exactly one copy behind the false error", stub.st.msgs.length === 1, JSON.stringify(stub.st.msgs));
  stub.close();
}

// 8. A 2xx whose body arrives truncated is the same lost-response problem:
// the server committed before answering, the cid check verifies it.
{
  const { home, stub } = await freshRoom();
  stub.st.mode = "trunc2xx";
  const r = await run(home, stub.port, ["send", "cut-answer"]);
  check("truncated 2xx body verified as sent, exit 0", r.code === 0 && /verified after an ambiguous failure/.test(r.out), `code=${r.code} out=${r.out} err=${r.err}`);
  check("exactly one copy behind the cut body", stub.st.msgs.length === 1, JSON.stringify(stub.st.msgs));
  check("no raw parse crash on the truncated body", !/SyntaxError/.test(r.err), r.err.slice(0, 120));
  stub.close();
}

// 9. Server unreachable: outcome unknown, no crash artifacts.
{
  const home = makeHome();
  const { writeFileSync: wf } = await import("node:fs");
  wf(join(home, ".config", "acq", "session.json"), JSON.stringify({ room: "ROOM-0000-00000", token: "tok-a", seat: "A" }));
  const r = await run(home, 1, ["send", "into-the-void"]); // port 1: refused
  check("unreachable server is outcome-unknown, exit 1", r.code === 1 && /send outcome unknown/.test(r.err), `code=${r.code} err=${r.err}`);
  check("no raw fetch stack trace when unreachable", !/TypeError: fetch failed/.test(r.err), r.err.slice(0, 120));
}

// 10. `--` ends flag parsing: dash-leading message text sends verbatim,
// and an unknown flag anywhere is rejected before any request (paired
// with watch-session case 18).
{
  const { home, stub } = await freshRoom();
  const r = await run(home, stub.port, ["send", "--", "--leading-dashes"]);
  check("dash-text: send -- stores the text verbatim", r.code === 0 && stub.st.msgs.some(m => m.text === "--leading-dashes"), `code=${r.code} err=${r.err}`);
  stub.close();
}
{
  const { home, stub } = await freshRoom();
  const r = await run(home, stub.port, ["send", "never-sent", "--unknownflag"]);
  check("unknown flag on send: exit 1 before any request", r.code === 1 && /--unknownflag/.test(r.err) && stub.st.msgs.length === 0, `code=${r.code} err=${r.err} stored=${stub.st.msgs.length}`);
  stub.close();
}

// 11. Typed outcome: stored:"unknown" carrying THIS request's exact cid
// and a valid seq verifies the send on its own (no poll needed).
{
  const { home, stub } = await freshRoom();
  stub.st.mode = "unknownBound";
  const r = await run(home, stub.port, ["send", "bound-unknown"]);
  check("bound stored:unknown verifies the send, exit 0", r.code === 0 && /verified after an ambiguous failure/.test(r.out), `code=${r.code} out=${r.out} err=${r.err}`);
  check("exactly one copy behind the bound unknown", stub.st.msgs.length === 1, JSON.stringify(stub.st.msgs));
  stub.close();
}

// 12. stored:"unknown" with NO cid/seq is no binding: ignored as typed
// evidence, and the transcript check still verifies a real commit.
{
  const { home, stub } = await freshRoom();
  stub.st.mode = "unknownBare";
  const r = await run(home, stub.port, ["send", "bare-unknown-committed"]);
  check("unbound stored:unknown over a commit still verifies via transcript", r.code === 0 && /verified after an ambiguous failure/.test(r.out), `code=${r.code} out=${r.out} err=${r.err}`);
  stub.close();
}

// 13. The same unbound claim over NOTHING committed: generic unconfirmed,
// never a definite "safe to re-send" and never a false verified.
{
  const { home, stub } = await freshRoom();
  stub.st.mode = "unknownBareNoCommit";
  const r = await run(home, stub.port, ["send", "bare-unknown-empty"]);
  check("unbound stored:unknown over nothing: unconfirmed, exit 1", r.code === 1 && /unconfirmed/.test(r.err), `code=${r.code} err=${r.err}`);
  check("no definite safe-to-resend phrasing on an unbound claim", !/not stored and re-sending as-is is safe/.test(r.err), r.err.slice(0, 200));
  check("nothing stored behind the bare claim", stub.st.msgs.length === 0, JSON.stringify(stub.st.msgs));
  stub.close();
}

// 14. Definite stored:false over an empty transcript: the send did not
// happen and re-sending is safe (the SPEC's pre-commit rejection).
{
  const { home, stub } = await freshRoom();
  stub.st.mode = "definiteFalse";
  const r = await run(home, stub.port, ["send", "definite-noop"]);
  check("stored:false reported definite-not-stored, exit 1", r.code === 1 && /not stored and re-sending as-is is safe/.test(r.err), `code=${r.code} err=${r.err}`);
  check("nothing stored behind the definite rejection", stub.st.msgs.length === 0, JSON.stringify(stub.st.msgs));
  stub.close();
}

// 15. An intermediary can fake stored:false over a real commit: the
// transcript check corrects the typed definite into verified-sent.
{
  const { home, stub } = await freshRoom();
  stub.st.mode = "definiteFalseAfterCommit";
  const r = await run(home, stub.port, ["send", "fake-definite"]);
  check("fake stored:false over a commit corrected to verified, exit 0", r.code === 0 && /verified after an ambiguous failure/.test(r.out), `code=${r.code} out=${r.out} err=${r.err}`);
  check("exactly one copy behind the fake rejection", stub.st.msgs.length === 1, JSON.stringify(stub.st.msgs));
  stub.close();
}

if (failures) { console.error(`${failures} FAIL`); process.exit(1); }
console.log("all send-verification tests passed");
