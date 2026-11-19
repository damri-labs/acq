// Message client-correlation-id suite (room page, #draft): the cid the
// composer attaches to a message must be stable across send retries of the
// same draft (idempotent resubmission), fresh after any edit (a changed
// draft is a new message), and measured exactly - uuid or fallback form -
// by the request-envelope gate in draftOverCap().
// Runs against a LOCAL server only: every non-loopback request is aborted.
// Exit 0 = all pass, 1 = assertion failures, 2 = setup error.
const puppeteer = require('puppeteer-core');
const BASE = process.env.BASE_URL || 'http://127.0.0.1:8099';
let fails = 0;
const ok = (name, cond, detail) => { console.log((cond ? 'PASS ' : 'FAIL ') + name + (detail ? ' :: ' + detail : '')); if (!cond) fails++; };
const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;
const safeFetch = async (url, opts) => {
  const r = await fetch(url, { ...(opts || {}), redirect: 'manual' });
  if (r.status >= 300 && r.status < 400) throw new Error('REFUSING to follow redirect from ' + url + ' (status ' + r.status + '): node-side calls stay on the loopback base');
  return r;
};
const isLoopbackHttp = (raw) => {
  try {
    const u = new URL(raw);
    return (u.protocol === 'http:' || u.protocol === 'https:') &&
           (u.hostname === 'localhost' || u.hostname === '127.0.0.1' || u.hostname === '[::1]');
  } catch (e) { return false; }
};
const isHttpNonLoopback = (raw) => {
  try {
    const u = new URL(raw);
    return (u.protocol === 'http:' || u.protocol === 'https:') &&
           !(u.hostname === 'localhost' || u.hostname === '127.0.0.1' || u.hostname === '[::1]');
  } catch (e) { return false; }
};
if (!isLoopbackHttp(BASE)) { console.error('BASE_URL must be loopback http(s), got: ' + BASE); process.exit(2); }
(async () => {
  const create = await (await safeFetch(BASE + '/room', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: '{"topic":"message-cid suite"}' })).json();
  const uin = create.roomId, credential = create.credential;
  if (!uin || !credential) { console.error('room create failed: ' + JSON.stringify(create)); process.exit(2); }
  let nonLoopbackFinished = 0;
  let nonLoopbackBlocked = 0;
  const browser = await puppeteer.launch({ executablePath: process.env.CHROME_PATH || 'google-chrome', args: ['--no-sandbox', '--disable-dev-shm-usage'] });
  try {
  // --- uuid path: retry reuses cid and body, edit resets it ---
  const posts = [];
  const page = await browser.newPage();
  let abortNext = true;
  await page.setRequestInterception(true);
  page.on('request', r => {
    if (isLoopbackHttp(r.url())) {
      if (r.method() === 'POST' && r.url().endsWith('/msg')) {
        posts.push(r.postData() || '');
        if (abortNext) { abortNext = false; r.abort(); return; }  // simulated network failure on the first attempt
      }
      r.continue();
    } else { r.abort(); }
  });
  page.on('requestfailed', r => { if (isHttpNonLoopback(r.url())) nonLoopbackBlocked++; });
  page.on('requestfinished', r => { if (isHttpNonLoopback(r.url())) nonLoopbackFinished++; });
  await page.evaluateOnNewDocument((cred) => { localStorage.setItem('acq_credentials', JSON.stringify([cred])); }, credential);
  await page.goto(BASE + '/room/' + encodeURIComponent(uin), { waitUntil: 'domcontentloaded' });
  await page.waitForFunction(() => { const c = document.getElementById('composer'); return c && !c.hidden; }, { timeout: 8000 });
  const setDraft = (t) => page.evaluate((v) => { const d = document.getElementById('draft'); d.value = v; d.dispatchEvent(new Event('input', { bubbles: true })); }, t);
  await setDraft('hello lifecycle');
  await page.click('#post');
  await new Promise(r => setTimeout(r, 600));
  await page.click('#post');            // retry after the aborted attempt
  await new Promise(r => setTimeout(r, 800));
  const b0 = JSON.parse(posts[0] || '{}'), b1 = JSON.parse(posts[1] || '{}');
  ok('retry after failed send reuses the same cid (idempotency)', posts.length >= 2 && b0.cid && b0.cid === b1.cid && UUID_RE.test(b0.cid));
  ok('retry body byte-identical to aborted attempt', posts.length >= 2 && posts[0] === posts[1], 'len=' + (posts[0] || '').length);
  await setDraft('hello lifecycle edited');
  await page.click('#post');
  await new Promise(r => setTimeout(r, 800));
  const b2 = JSON.parse(posts[2] || '{}');
  ok('editing the draft issues a fresh cid', b2.cid && b2.cid !== b1.cid && b2.text === 'hello lifecycle edited');
  await page.close();

  // --- fallback cid path (crypto.randomUUID unavailable): measured exactly ---
  const page2 = await browser.newPage();
  const posts2 = [];
  await page2.setRequestInterception(true);
  page2.on('request', r => {
    if (isLoopbackHttp(r.url())) {
      if (r.method() === 'POST' && r.url().endsWith('/msg')) posts2.push(r.postData() || '');
      r.continue();
    } else { r.abort(); }
  });
  page2.on('requestfailed', r => { if (isHttpNonLoopback(r.url())) nonLoopbackBlocked++; });
  page2.on('requestfinished', r => { if (isHttpNonLoopback(r.url())) nonLoopbackFinished++; });
  await page2.evaluateOnNewDocument((cred) => {
    localStorage.setItem('acq_credentials', JSON.stringify([cred]));
    try { Object.defineProperty(window.crypto, 'randomUUID', { value: undefined, configurable: true }); } catch (e) {}
  }, credential);
  await page2.goto(BASE + '/room/' + encodeURIComponent(uin), { waitUntil: 'domcontentloaded' });
  await page2.waitForFunction(() => { const c = document.getElementById('composer'); return c && !c.hidden; }, { timeout: 8000 });
  const uuidGone = await page2.evaluate(() => !crypto.randomUUID);
  const setDraft2 = (t) => page2.evaluate((v) => { const d = document.getElementById('draft'); d.value = v; d.dispatchEvent(new Event('input', { bubbles: true })); }, t);
  await setDraft2('fallback probe');
  await page2.click('#post');
  await new Promise(r => setTimeout(r, 800));
  const f0 = JSON.parse(posts2[0] || '{}');
  ok('fallback path actually exercised (non-uuid cid captured)', uuidGone && f0.cid && !UUID_RE.test(f0.cid), 'cid=' + (f0.cid || '?'));
  const before = posts2.length;
  await setDraft2('\\'.repeat(40000));   // escaped envelope over 69632 for any cid length
  await new Promise(r => setTimeout(r, 400));
  const st = await page2.evaluate(() => { const n = document.getElementById('draft-note'); return { shown: !n.hidden, note: n.textContent, dis: document.getElementById('post').disabled }; });
  ok('fallback: 40000 backslashes held with exact measured size', st.shown && st.dis && /request is 800\d\d bytes/.test(st.note), st.note.slice(0, 110));
  await page2.click('#post').catch(() => {});
  await new Promise(r => setTimeout(r, 300));
  ok('fallback: held draft posts nothing', posts2.length === before, 'posts=' + posts2.length);
  await setDraft2('\\'.repeat(34000));   // escaped envelope fits for any plausible fallback cid
  await page2.click('#post');
  await new Promise(r => setTimeout(r, 800));
  const fLast = JSON.parse(posts2[posts2.length - 1] || '{}');
  ok('fallback: 34000 backslashes accepted, stored verbatim', posts2.length === before + 1 && fLast.text === '\\'.repeat(34000), 'bodylen=' + (posts2[posts2.length - 1] || '').length);
  ok('fallback: accepted send used a fallback-form cid, measured exactly', fLast.cid && !UUID_RE.test(fLast.cid), 'cidlen=' + (fLast.cid || '').length);
  await page2.close();

  ok('scope: no non-loopback request finished or blocked', nonLoopbackFinished === 0 && nonLoopbackBlocked === 0, 'finished=' + nonLoopbackFinished + ' blocked=' + nonLoopbackBlocked);
  } finally {
    await browser.close();
  }
  console.log(fails ? fails + ' FAILURES' : 'ALL PASS');
  process.exit(fails ? 1 : 0);
})().catch(e => { console.error('ERR', e.message); process.exit(2); });
