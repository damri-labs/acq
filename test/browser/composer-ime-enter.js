// Composer IME Enter suite (room page, #draft): a composition-confirming
// Enter (isComposing=true, the event an IME sends when confirming a
// candidate) is text input, not send intent - it must never post the
// draft. An ordinary Enter (trusted keypress, or synthetic with
// isComposing=false) must post. Synthetic dispatch only: this suite makes
// no claim about real IME behavior beyond the DOM contract it dispatches.
// Same guard as the landing topic field (topic-limit.js convention).
//
// Run: BASE_URL=http://127.0.0.1:8099 node composer-ime-enter.js
// against a locally built server (no proxy needed). Use a free loopback
// port and an isolated throwaway ROOMS_DIR per run: the suite creates a
// real room and sends real (ordinary-Enter) messages on that server by
// design. BASE_URL is enforced loopback-only before any browser launch,
// so this suite can never be pointed at a deployed service.
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

  const create = await (await safeFetch(BASE + '/room', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: '{"topic":"ime-enter suite"}' })).json();
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
  const draftValue = () => page.evaluate(() => document.getElementById('draft').value);
  const syntheticEnter = (composing) => page.evaluate((c) => {
    document.getElementById('draft').dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', isComposing: c, bubbles: true }));
  }, composing);

  // -- synthetic composition-confirm Enter: must not post, draft kept --
  await setDraft('hello composition');
  await syntheticEnter(true);
  await sleep(400);
  ok('ime: composition-confirm Enter posts nothing, no POST /msg', msgPosts === 0, 'posts=' + msgPosts);
  ok('ime: composition-confirm Enter keeps the draft', (await draftValue()) === 'hello composition');

  // -- synthetic ordinary Enter (isComposing=false): posts --
  await syntheticEnter(false);
  await page.waitForFunction(() => document.getElementById('draft').value === '', { timeout: 6000 }).catch(() => {});
  ok('ordinary synthetic Enter posts, draft cleared on success', msgPosts === 1 && (await draftValue()) === '', 'posts=' + msgPosts);

  // -- trusted ordinary Enter (real keypress): posts --
  await setDraft('hello trusted');
  await page.click('#draft');
  await page.keyboard.press('Enter');
  await page.waitForFunction(() => document.getElementById('draft').value === '', { timeout: 6000 }).catch(() => {});
  ok('trusted Enter posts, draft cleared on success', msgPosts === 2 && (await draftValue()) === '', 'posts=' + msgPosts);

  // -- server-side truth: exactly the two ordinary-Enter messages stored --
  const tr = await (await safeFetch(BASE + '/room/' + encodeURIComponent(uin) + '/msg?since=0&wait=0&self=1&peek=1', { headers: { Authorization: 'Bearer ' + token } })).json();
  const msgs = tr.msgs || tr.messages || [];
  ok('transcript: exactly the 2 ordinary-Enter messages stored, in order',
     msgs.length === 2 && msgs[0].text === 'hello composition' && msgs[1].text === 'hello trusted',
     'texts=' + msgs.map(m => m.text).join(' | '));

  ok('scope: no non-loopback request finished or blocked', nonLoopbackFinished === 0 && nonLoopbackBlocked === 0, 'finished=' + nonLoopbackFinished + ' blocked=' + nonLoopbackBlocked);
  } finally {
    await browser.close();
  }

  console.log(results.join('\n'));
  console.log(failures === 0 ? 'ALL PASS' : failures + ' FAILURES');
  process.exit(failures === 0 ? 0 : 1);
})().catch(e => { console.error(e); process.exit(2); });
