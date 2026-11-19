// Message byte-limit suite (room page, #draft): a non-ASCII message must
// never pass the client gate while over the server's 65536-BYTE cap.
// maxlength counts UTF-16 code units, so Hebrew (2 bytes/char) and emoji
// (4 bytes/char) drafts the browser accepts can be ones the server must
// reject with 400 {"detail":"bad message"}. While the draft exceeds the
// cap, the page must keep the user's text untouched, show its byte size,
// and hold the send (click AND Enter). At or under the cap the send must
// go through and the stored message must equal the entered text.
//
// Run: BASE_URL=http://127.0.0.1:8099 node message-limit.js
// against a locally built server (no proxy needed). Use a free loopback
// port and an isolated throwaway ROOMS_DIR per run: the suite creates a
// real room and sends real (boundary, under-cap) messages on that server
// by design. BASE_URL is enforced loopback-only before any browser
// launch, so this suite can never be pointed at a deployed service.
const puppeteer = require('puppeteer-core');
const BASE = process.env.BASE_URL || 'http://127.0.0.1:8099';
const results = [];
let failures = 0;
const ok = (name, cond, extra) => { if (!cond) failures++; results.push((cond ? 'PASS' : 'FAIL') + ' ' + name + (extra ? ' :: ' + extra : '')); };
const sleep = ms => new Promise(r => setTimeout(r, ms));

// Node-side API calls (room setup, transcript readback) run OUTSIDE the
// browser's request interception, so they fail closed on their own:
// redirects are never followed - a local server answered by a redirect is
// a harness bug or a wrong port, not something to chase off-loopback.
const safeFetch = async (url, opts) => {
  const r = await fetch(url, { ...(opts || {}), redirect: 'manual' });
  if (r.status >= 300 && r.status < 400) throw new Error('REFUSING to follow redirect from ' + url + ' (status ' + r.status + '): node-side calls stay on the loopback base');
  return r;
};

const CAP = 65536;
const HEB_OVER = 'ש'.repeat(40000);          // 80000 bytes, 40000 units - under maxlength, over cap
const HEB_FIT  = 'ש'.repeat(CAP / 2);        // exactly 65536 bytes
const ASCII_FIT = 'a'.repeat(CAP);           // exactly 65536 bytes
const ASCII_OVER = 'a'.repeat(CAP + 1);      // 65537 bytes (evaluate-set bypasses native maxlength)
const EMOJI_OVER = '😀'.repeat(16385);       // 65540 bytes, 32770 units - under maxlength, over cap
// Envelope cases: the server caps the whole JSON request body at 69632 bytes
// (maxMsgSize+4096). With a 36-char uuid cid the body overhead is 68 bytes, so
// a text of q quote characters (each escapes to 2 bytes) has envelope 2q+68:
// 34782 quotes = exactly 69632 (must send), 34783 = 69634 (must be held).
const QUOTE_ENV_FIT = '"'.repeat(34782);   // envelope exactly 69632 - accepted
const QUOTE_ENV_OVER = '"'.repeat(34783);  // envelope 69634 - held
const BACKSLASH_OVER = '\\'.repeat(40000); // envelope 80068 - held
const CTRL_FIT = '\u0001'.repeat(10000);   // envelope 60068 (6 bytes each) - accepted, stored verbatim
const utf8 = s => new TextEncoder().encode(s).length;

(async () => {
  const isLoopbackHttp = (raw) => {
    try {
      const u = new URL(raw);
      return (u.protocol === 'http:' || u.protocol === 'https:') &&
             (u.hostname === 'localhost' || u.hostname === '127.0.0.1' || u.hostname === '[::1]');
    } catch (e) { return false; }
  };
  if (!isLoopbackHttp(BASE)) {
    console.error('REFUSING to run: BASE_URL must be a loopback origin (http://127.0.0.1:PORT, localhost or [::1]); got ' + BASE);
    process.exit(2);
  }

  // Room setup over the local API: one room, seat A credential seeded into
  // localStorage before any page script runs (the page's own storage path).
  const create = await (await safeFetch(BASE + '/room', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: '{"topic":"msg-limit suite"}' })).json();
  const uin = create.roomId, token = create.token, credential = create.credential;
  if (!uin || !token || !credential) { console.error('room create failed: ' + JSON.stringify(create)); process.exit(2); }

  const browser = await puppeteer.launch({ executablePath: process.env.CHROME_PATH || 'google-chrome', args: ['--no-sandbox', '--disable-dev-shm-usage'] });
  try {
  const page = await browser.newPage();
  await page.setViewport({ width: 1100, height: 900 });

  // Single request listener: loopback requests continue (and message posts
  // are counted here), anything else is aborted before it is sent.
  let msgPosts = 0;
  let nonLoopbackFinished = 0;
  let nonLoopbackBlocked = 0;
  await page.setRequestInterception(true);
  page.on('request', r => {
    if (isLoopbackHttp(r.url())) {
      if (r.method() === 'POST' && new URL(r.url()).pathname.endsWith('/msg')) msgPosts++;
      r.continue();
    } else {
      r.abort();
    }
  });
  const isHttpNonLoopback = (raw) => {
    try {
      const u = new URL(raw);
      return (u.protocol === 'http:' || u.protocol === 'https:') &&
             !(u.hostname === 'localhost' || u.hostname === '127.0.0.1' || u.hostname === '[::1]');
    } catch (e) { return false; }
  };
  page.on('requestfailed', r => { if (isHttpNonLoopback(r.url())) nonLoopbackBlocked++; });
  page.on('requestfinished', r => { if (isHttpNonLoopback(r.url())) nonLoopbackFinished++; });

  await page.evaluateOnNewDocument((cred) => { localStorage.setItem('acq_credentials', JSON.stringify([cred])); }, credential);
  await page.goto(BASE + '/room/' + encodeURIComponent(uin), { waitUntil: 'domcontentloaded' });
  await page.waitForFunction(() => { const c = document.getElementById('composer'); return c && !c.hidden; }, { timeout: 8000 });

  const setDraft = (t) => page.evaluate((v) => {
    const d = document.getElementById('draft');
    d.value = v;
    d.dispatchEvent(new Event('input', { bubbles: true }));
  }, t);
  const draftState = () => page.evaluate(() => {
    const d = document.getElementById('draft'), n = document.getElementById('draft-note'), b = document.getElementById('post');
    return { chars: d.value.length, noteShown: !!(n && !n.hidden), note: n ? n.textContent : '', postDisabled: b.disabled, err: document.getElementById('send-err').textContent };
  });

  // -- over cap (Hebrew, 80000 bytes): text kept, byte note shown, send held --
  await setDraft(HEB_OVER);
  let st = await draftState();
  ok('overcap hebrew: user text preserved, not rewritten', st.chars === 40000, 'chars=' + st.chars);
  ok('overcap hebrew: note visible with exact byte size', st.noteShown && st.note.includes(utf8(HEB_OVER) + ' bytes') && st.note.includes('65536'), st.note.slice(0, 70));
  ok('overcap hebrew: send button held', st.postDisabled === true);

  // -- over cap: neither Enter nor click may post; draft untouched --
  let postsBefore = msgPosts;
  await page.click('#draft');
  await page.keyboard.press('Enter');
  await sleep(400);
  await page.click('#post').catch(() => {});
  await sleep(400);
  st = await draftState();
  ok('overcap hebrew: Enter+click post nothing, no POST /msg', msgPosts === postsBefore, 'posts=' + msgPosts);
  ok('overcap hebrew: draft still intact after attempts', st.chars === 40000, 'chars=' + st.chars);

  // -- ASCII one byte over the cap (native maxlength bypassed by set): held --
  await setDraft(ASCII_OVER);
  st = await draftState();
  ok('overcap ascii: note visible, send held', st.noteShown && st.postDisabled === true, 'note=' + st.note.slice(0, 50));

  // -- emoji over the cap (32770 units < maxlength): held --
  await setDraft(EMOJI_OVER);
  st = await draftState();
  ok('overcap emoji: note visible with exact byte size, send held', st.noteShown && st.note.includes(utf8(EMOJI_OVER) + ' bytes') && st.postDisabled === true, st.note.slice(0, 70));

  // -- escape-heavy text just OVER the request envelope: held, exact reason --
  await setDraft(QUOTE_ENV_OVER);
  st = await draftState();
  ok('envelope 69634 (34783 quotes): text fits message cap yet send held', st.noteShown && st.postDisabled === true, st.note.slice(0, 80));
  ok('envelope over: note shows exact escaped request size and limit', st.note.includes('69634') && st.note.includes('69632-byte request limit'), st.note.slice(0, 120));
  postsBefore = msgPosts;
  await page.click('#post').catch(() => {});
  await sleep(300);
  ok('envelope over: click posts nothing, draft intact', msgPosts === postsBefore && (await draftState()).chars === 34783, 'posts=' + msgPosts);

  // -- escape-heavy text at EXACTLY the request envelope cap: sends, stored verbatim --
  await setDraft(QUOTE_ENV_FIT);
  st = await draftState();
  ok('envelope exactly 69632 (34782 quotes): note cleared, send enabled', st.noteShown === false && st.postDisabled === false);
  await page.click('#post');
  await page.waitForFunction(() => document.getElementById('draft').value === '', { timeout: 6000 }).catch(() => {});
  st = await draftState();
  ok('envelope exactly 69632: send accepted by server, draft cleared', st.chars === 0, 'chars=' + st.chars);

  // -- backslash-heavy text over the envelope: held --
  await setDraft(BACKSLASH_OVER);
  st = await draftState();
  ok('envelope 80068 (40000 backslashes): held with exact size', st.noteShown && st.postDisabled === true && st.note.includes('80068'), st.note.slice(0, 80));

  // -- control-character text inside both caps: sends, stored verbatim --
  await setDraft(CTRL_FIT);
  st = await draftState();
  ok('envelope 60068 (10000 control chars): note cleared, send enabled', st.noteShown === false && st.postDisabled === false);
  await page.click('#post');
  await page.waitForFunction(() => document.getElementById('draft').value === '', { timeout: 6000 }).catch(() => {});
  st = await draftState();
  ok('envelope 60068 control chars: send accepted, draft cleared', st.chars === 0, 'chars=' + st.chars);

  // -- back under the cap: note clears, button re-enables --
  await setDraft(HEB_FIT);
  st = await draftState();
  ok('boundary hebrew 65536B: note cleared, send enabled', st.noteShown === false && st.postDisabled === false);

  // -- boundary send goes through; draft clears only on success --
  postsBefore = msgPosts;
  await page.click('#post');
  await page.waitForFunction(() => document.getElementById('draft').value === '', { timeout: 6000 }).catch(() => {});
  st = await draftState();
  ok('boundary hebrew 65536B: send accepted, draft cleared', msgPosts === postsBefore + 1 && st.chars === 0, 'posts=' + msgPosts + ' chars=' + st.chars);

  // -- ASCII exactly at the cap: accepted too --
  await setDraft(ASCII_FIT);
  st = await draftState();
  ok('boundary ascii 65536B: note cleared, send enabled', st.noteShown === false && st.postDisabled === false);
  await page.click('#post');
  await page.waitForFunction(() => document.getElementById('draft').value === '', { timeout: 6000 }).catch(() => {});
  st = await draftState();
  ok('boundary ascii 65536B: send accepted, draft cleared', st.chars === 0, 'chars=' + st.chars);

  // -- server-side truth: exactly the four accepted messages, byte-for-byte --
  const tr = await (await safeFetch(BASE + '/room/' + encodeURIComponent(uin) + '/msg?since=0&wait=0&self=1&peek=1', { headers: { Authorization: 'Bearer ' + token } })).json();
  const msgs = tr.msgs || tr.messages || [];
  ok('transcript: exactly the 4 accepted messages stored', msgs.length === 4, 'msgs=' + msgs.length);
  ok('transcript: stored text equals entered text byte-for-byte',
     msgs.length === 4 && msgs[0].text === QUOTE_ENV_FIT && msgs[1].text === CTRL_FIT && msgs[2].text === HEB_FIT && msgs[3].text === ASCII_FIT,
     'lens=' + msgs.map(m => (m.text || '').length).join(','));

  ok('scope: no non-loopback request finished or blocked', nonLoopbackFinished === 0 && nonLoopbackBlocked === 0, 'finished=' + nonLoopbackFinished + ' blocked=' + nonLoopbackBlocked);
  } finally {
    await browser.close();
  }

  console.log(results.join('\n'));
  console.log(failures === 0 ? 'ALL PASS' : failures + ' FAILURES');
  process.exit(failures === 0 ? 0 : 1);
})().catch(e => { console.error(e); process.exit(2); });
