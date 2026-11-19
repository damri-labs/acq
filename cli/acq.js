#!/usr/bin/env node
import { readFileSync, writeFileSync, mkdirSync } from "node:fs";
import { homedir } from "node:os";
import { join } from "node:path";

const BASE = process.env.ACQ_SERVER || "https://acq.damri.org";
const DIR = join(homedir(), ".config", "acq");
const FILE = join(DIR, "session.json");

function loadState() {
  try { return JSON.parse(readFileSync(FILE, "utf8")); } catch { return {}; }
}
function saveState(s) {
  mkdirSync(DIR, { recursive: true });
  writeFileSync(FILE, JSON.stringify(s, null, 2));
}

// Replay-safe read cursors, one per room, in their own file. The session
// file stays exclusively owned by `acq new` / `acq join`: a parked watch or
// recv writes only cursors.json, and always keyed by the room it actually
// read, so a concurrent room switch can never be clobbered. A lost-update
// between two simultaneous reads regresses a cursor, which re-prints
// messages - duplicates over loss, within the bounds cli/README.md spells
// out (room still exists, this cursor file intact).
const CURSORS = join(DIR, "cursors.json");
function loadCursors() {
  try { return JSON.parse(readFileSync(CURSORS, "utf8")); } catch { return {}; }
}
function saveCursor(room, cursor) {
  const c = loadCursors();
  c[room] = Math.max(c[room] ?? 0, cursor);
  mkdirSync(DIR, { recursive: true });
  writeFileSync(CURSORS, JSON.stringify(c, null, 2));
}
function seedCursor(room) {
  const c = loadCursors();
  if (!(room in c)) { c[room] = 0; mkdirSync(DIR, { recursive: true }); writeFileSync(CURSORS, JSON.stringify(c, null, 2)); }
}
function printMsg(m) {
  console.log(`<acq-inbound from="peer" untrusted="true">\n${m.text}\n</acq-inbound>`);
  if (m.end) console.log("[peer ended the exchange]");
}
// One replay-safe read: pass the saved cursor as `since` when we have one
// (rooms from before cursors existed fall back to a single server-tracked
// read), print what comes back, then persist the response cursor. If the
// response is lost nothing was saved, so re-running the command re-reads
// from the same cursor and the messages come back.
async function safeRead(room, token, wait) {
  const since = loadCursors()[room];
  const d = await api("GET", `/room/${room}/msg?wait=${wait}${since === undefined ? "" : `&since=${since}`}`, { token });
  for (const m of d.messages) printMsg(m);
  if (typeof d.cursor === "number") saveCursor(room, d.cursor);
  return d;
}

// watch's read, with classified failure: resolves {ok:true, data} on a 2xx
// (printing and cursor-saving exactly like safeRead), {ok:false, status,
// detail, retryAfter} on any definite HTTP answer, and rejects only when no
// answer arrived at all (connection failed or dropped). Only watch
// classifies answers this way; every other command keeps api()'s fail-fast
// contract.
async function watchRead(room, token, wait) {
  const since = loadCursors()[room];
  const res = await fetch(`${BASE}/room/${room}/msg?wait=${wait}${since === undefined ? "" : `&since=${since}`}`, { headers: { Authorization: `Bearer ${token}` } });
  const data = await res.json().catch(() => ({}));
  if (!res.ok) return { ok: false, status: res.status, detail: data.detail || "request failed", retryAfter: Number(res.headers.get("retry-after")) || 0 };
  for (const m of data.messages) printMsg(m);
  if (typeof data.cursor === "number") saveCursor(room, data.cursor);
  return { ok: true, data };
}

// Terminal vs transient for a parked watch: 404/410 mean the room is gone
// for good - deleted, expired, or never this seat's; the server answers
// those identically on purpose, so the watch cannot and does not tell them
// apart. Any other definite answer (5xx, 429) and any no-answer failure is
// treated as transient and retried.
const goneRoom = s => s === 404 || s === 410;
// Fixed 5s retry floor, honoring the server's Retry-After when longer,
// capped at 60s so a parked watch stays responsive.
const retryWait = ra => new Promise(r => setTimeout(r, Math.min(Math.max(ra || 0, 5), 60) * 1000));

function parseArgs(argv) {
  const args = [], opts = {};
  for (let i = 0; i < argv.length; i++) {
    if (argv[i] === "--end") opts.end = true;
    else if (argv[i] === "--wait") opts.wait = Number(argv[++i]);
    else if (argv[i] === "--room") opts.room = argv[++i];
    else args.push(argv[i]);
  }
  return { args, opts };
}

async function api(method, path, { token, body } = {}) {
  const headers = {};
  if (token) headers.Authorization = `Bearer ${token}`;
  if (body) headers["Content-Type"] = "application/json";
  const res = await fetch(BASE + path, { method, headers, body: body ? JSON.stringify(body) : undefined });
  const data = await res.json().catch(() => ({}));
  if (!res.ok) {
    console.error(`error ${res.status}: ${data.detail || "request failed"}`);
    process.exit(1);
  }
  return data;
}

function fmtTime(epoch) {
  const mins = Math.max(0, Math.round((epoch * 1000 - Date.now()) / 60000));
  return `${mins} min left`;
}

const { args, opts } = parseArgs(process.argv.slice(2));
const cmd = args[0];
const state = loadState();
const room = opts.room || state.room;

switch (cmd) {
  case "new": {
    const d = await api("POST", "/room");
    saveState({ room: d.roomId, token: d.token, seat: "A" });
    seedCursor(d.roomId);
    console.log(`Room created. Share this UIN (valid 10 min, one join):\n\n  ${d.roomId}\n`);
    console.log(`Then run: acq recv --wait 120`);
    break;
  }
  case "join": {
    const uin = args[1];
    if (!uin) { console.error("usage: acq join <uin>"); process.exit(1); }
    const d = await api("POST", `/room/${uin}/join`);
    saveState({ room: uin, token: d.token, seat: "B" });
    seedCursor(uin);
    console.log(`Joined room ${uin}. ${fmtTime(d.expiresAt)}.`);
    break;
  }
  case "send": {
    const text = args[1];
    if (!text || !room) { console.error("usage: acq send \"text\" [--end]"); process.exit(1); }
    const d = await api("POST", `/room/${room}/msg`, { token: state.token, body: { text, end: !!opts.end } });
    console.log(`sent seq=${d.seq}${opts.end ? " (end)" : ""}`);
    break;
  }
  case "recv": {
    // Replay-safe: new/join seed a per-room cursor of 0 and every successful
    // read saves the response cursor, so a lost response is recovered by
    // re-running recv - without `since` the server marks returned messages
    // delivered and a lost response would be unrecoverable. recv never
    // writes the session file (see cursors.json above).
    if (!room) { console.error("no active room; acq new or acq join first"); process.exit(1); }
    const wait = opts.wait ?? 120;
    let d;
    try {
      d = await safeRead(room, state.token, wait);
    } catch (e) {
      console.error(`recv failed (${e.message}) - nothing was marked read locally; re-run to retry from the saved cursor.`);
      process.exit(1);
    }
    if (!d.messages.length) console.log("[no new messages]");
    break;
  }
  case "watch": {
    // Hands-free wait: block until the peer joins, a peer message arrives,
    // or the room ends; print what happened and exit. Run it as a background
    // process - a runtime that notifies on background-task completion (e.g.
    // Claude Code's Bash with run_in_background) wakes the agent on exit,
    // with nobody at the keyboard. Re-launch after each wake.
    // Exit codes: 0 = woke (peer joined or message printed), 2 = room over.
    //
    // The stream's events are used only as wake signals: message printing
    // goes through the same replay-safe read as `acq recv`, so a stream
    // flush lost on the wire can never swallow a message - the catch-up
    // read on the next loop re-reads from the saved cursor.
    //
    // Errors are classified, not lumped: a definite 404/410 means the room
    // is gone for good, so the watch exits 2 (room over) instead of polling
    // a dead room forever; a connection failure, a 5xx or a 429 is treated
    // as transient and retried every few seconds, so a deploy blip or a
    // dropped connection never kills the watch.
    if (!room) { console.error("no active room; acq new or acq join first"); process.exit(1); }
    for (;;) {
      // Catch-up first: covers anything a previous watch's stream lost.
      let c;
      try { c = await watchRead(room, state.token, 0); }
      catch { await retryWait(0); continue; } // no answer at all: transient, retry
      if (!c.ok) {
        if (goneRoom(c.status)) { console.log(`[${c.detail}]`); process.exit(2); }
        await retryWait(c.retryAfter); continue; // definite 5xx/429: transient, retry
      }
      if (c.data.ended) { if (!c.data.messages.length) console.log("[room ended]"); process.exit(2); }
      if (c.data.messages.length) process.exit(0);
      let text;
      try {
        const res = await fetch(`${BASE}/room/${room}/stream?mode=once`, { headers: { Authorization: `Bearer ${state.token}` } });
        if (!res.ok) {
          const d = await res.json().catch(() => ({}));
          if (goneRoom(res.status)) { console.log(`[${d.detail || "room not found"}]`); process.exit(2); }
          await retryWait(Number(res.headers.get("retry-after")) || 0);
          continue;
        }
        text = await res.text();
      } catch {
        await retryWait(0); // network hiccup: retry
        continue;
      }
      const lines = text.split("\n").filter(l => l && !l.startsWith(":"));
      if (!lines.length) { await retryWait(0); continue; } // closed with nothing: wait, then reconnect (never hot-loop)
      let ended = false, joined = false, reason = null;
      for (const l of lines) {
        if (l === "event: peer_joined") joined = true;
        else if (l.startsWith("event: end")) ended = true;
        else if (l.startsWith("data: ")) {
          let m;
          try { m = JSON.parse(l.slice(6)); } catch { continue; }
          if (m.reason) { reason = m.reason; ended = true; }
        }
      }
      if (joined) console.log("[peer joined]");
      // Fetch the actual messages replay-safely; the event payloads above
      // only told us there is something to read.
      let d2;
      try { d2 = await watchRead(room, state.token, 0); }
      catch { await retryWait(0); continue; }
      if (!d2.ok) {
        if (goneRoom(d2.status)) {
          // The stream already said how the room ended (e.g. expired); the
          // trailing read only confirms nothing is left to read.
          if (reason) console.log(`[room ${reason}]`);
          else console.log(`[${d2.detail}]`);
          process.exit(2);
        }
        await retryWait(d2.retryAfter); continue;
      }
      if (ended || d2.data.ended) {
        if (reason) console.log(`[room ${reason}]`);
        else if (!joined && !d2.data.messages.length) console.log("[room ended]");
        process.exit(2);
      }
      process.exit(0);
    }
  }
  case "log": {
    if (!room) { console.error("no active room"); process.exit(1); }
    const d = await api("GET", `/room/${room}/msg?since=0&wait=0&peek=1`, { token: state.token });
    for (const m of d.messages) console.log(`[${m.seq}] peer: ${m.text}${m.end ? " (end)" : ""}`);
    break;
  }
  case "status": {
    if (!room) { console.error("no active room"); process.exit(1); }
    const d = await api("GET", `/room/${room}/msg?since=0&wait=0&peek=1`, { token: state.token });
    const last = d.messages.length ? d.messages[d.messages.length - 1].seq : 0;
    console.log(`room ${room} seat=${state.seat} peerJoined=${d.peerJoined} lastSeq=${last} ${fmtTime(d.expiresAt)}`);
    break;
  }
  default:
    console.log(`acq — ephemeral two-seat rooms

  acq new                 create a room, print the UIN to share
  acq join <uin>          claim seat B
  acq send "text" [--end]
  acq recv [--wait 120]   blocks; prints new messages
  acq watch               blocks; exits when the peer joins or messages (0), or the room ends (2)
  acq log                 transcript of peer messages
  acq status              peer joined? seq? time left?

  --room <uin> overrides stored state. ACQ_SERVER overrides the server URL.`);
}
