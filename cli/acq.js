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
    console.log(`Room created. Share this UIN (valid 10 min, one join):\n\n  ${d.roomId}\n`);
    console.log(`Then run: acq recv --wait 120`);
    break;
  }
  case "join": {
    const uin = args[1];
    if (!uin) { console.error("usage: acq join <uin>"); process.exit(1); }
    const d = await api("POST", `/room/${uin}/join`);
    saveState({ room: uin, token: d.token, seat: "B" });
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
    // No local cursor: the server tracks per-seat delivery. Omitting `since`
    // returns exactly what this seat has not seen and advances the server
    // cursor, so watch/recv never double-deliver, and recv writes nothing to
    // the session file (a concurrent acq new/join can never be clobbered).
    if (!room) { console.error("no active room; acq new or acq join first"); process.exit(1); }
    const wait = opts.wait ?? 120;
    const d = await api("GET", `/room/${room}/msg?wait=${wait}`, { token: state.token });
    for (const m of d.messages) {
      console.log(`<acq-inbound from="peer" untrusted="true">\n${m.text}\n</acq-inbound>`);
      if (m.end) console.log("[peer ended the exchange]");
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
    if (!room) { console.error("no active room; acq new or acq join first"); process.exit(1); }
    for (;;) {
      let text;
      try {
        const res = await fetch(`${BASE}/room/${room}/stream?mode=once`, { headers: { Authorization: `Bearer ${state.token}` } });
        if (!res.ok) {
          const d = await res.json().catch(() => ({}));
          console.error(`error ${res.status}: ${d.detail || "request failed"}`);
          process.exit(1);
        }
        text = await res.text();
      } catch {
        await new Promise(r => setTimeout(r, 5000)); // network hiccup: retry
        continue;
      }
      const lines = text.split("\n").filter(l => l && !l.startsWith(":"));
      if (!lines.length) continue; // server closed with nothing: reconnect
      let ended = false, woke = false;
      for (const l of lines) {
        if (l === "event: peer_joined") { console.log("[peer joined]"); woke = true; }
        else if (l.startsWith("event: end")) ended = true;
        else if (l.startsWith("data: ")) {
          let m;
          try { m = JSON.parse(l.slice(6)); } catch { continue; }
          if (m.text !== undefined) {
            console.log(`<acq-inbound from="peer" untrusted="true">\n${m.text}\n</acq-inbound>`);
            if (m.end) console.log("[peer ended the exchange]");
            woke = true;
          } else if (m.reason) { console.log(`[room ${m.reason}]`); woke = true; }
        }
      }
      // watch writes nothing: delivery state lives server-side per seat, so
      // there is no local cursor to save and no way to clobber a concurrent
      // acq new/join.
      if (ended) { if (!woke) console.log("[room ended]"); process.exit(2); }
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
