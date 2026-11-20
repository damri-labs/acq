// Unsent-draft recovery suite (room page, #draft) - v2 candidate.
// Seat-credential sessions only (owner path excluded by construction).
// The sessionStorage copy (key: acq_draft_<UIN>_<seat>_<token8>, value
// JSON {t, c}) must: (1) save on input / remove on empty, (2) restore
// after refresh, (3) clear on confirmed send and stay cleared, (4) not
// leak across rooms, (5) not leak across seats, (6) not appear in a fresh
// tab, (7) TRUTHFULLY inherit into an opener-created tab (documented
// sessionStorage copy behavior), (8) preserve the bound cid across a
// refresh between an unconfirmed send and its retry, so the retry is an
// idempotent resubmission (exactly one committed message, same cid).
// Runs against LOCAL servers only (direct :8099 + failure proxy :8100).
// Exit 0 = all pass, 1 = assertion failures, 2 = setup error.
const puppeteer = require('puppeteer-core');
const fs = require('fs');
const BASE = process.env.BASE_URL || 'http://127.0.0.1:8099';
const PBASE = process.env.PROXY_URL || 'http://127.0.0.1:8100';
const FLAGS = process.env.FLAGS_DIR || '/tmp/acq-ui-flags';
fs.mkdirSync(FLAGS, { recursive: true });
let fails = 0;
const ok = (name, cond, detail) => { console.log((cond ? 'PASS ' : 'FAIL ') + name + (detail ? ' :: ' + detail : '')); if (!cond) fails++; };
const safeFetch = async (url, opts) => {
  const r = await fetch(url, { ...(opts || {}), redirect: 'manual' });
  if (r.status >= 300 && r.status < 400) throw new Error('REFUSING redirect from ' + url);
  return r;
};
const isLoopbackHttp = (raw) => {
  try {
    const u = new URL(raw);
    return (u.protocol === 'http:' || u.protocol === 'https:') &&
           (u.hostname === 'localhost' || u.hostname === '127.0.0.1' || u.hostname === '[::1]');
  } catch (e) { return false; }
};
for (const u of [BASE, PBASE]) if (!isLoopbackHttp(u)) { console.error('non-loopback base: ' + u); process.exit(2); }
const b64url = (s) => Buffer.from(s, 'utf8').toString('base64').replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
const unpack = (blob) => JSON.parse(Buffer.from(blob.slice(5), 'base64url').toString('utf8'));
(async () => {
  const mk = async (topic) => {
    const r = await (await safeFetch(BASE + '/room', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ topic }) })).json();
    if (!r.roomId || !r.credential) throw new Error('room create failed: ' + JSON.stringify(r));
    return r;
  };
  const room1 = await mk('draft-restore room1');
  const room2 = await mk('draft-restore room2');
  const joinB = await (await safeFetch(BASE + '/room/' + room1.roomId + '/join', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: '{"as":"human"}' })).json();
  if (!joinB.token) throw new Error('seat B join failed');
  const credB = 'acq1_' + b64url(JSON.stringify({ uin: room1.roomId, token: joinB.token, seat: 'B' }));
  const keyOf = (uin, credBlob) => {
    const c = unpack(credBlob);
    let h = 5381; for (let i = 0; i < c.token.length; i++) { h = ((h << 5) + h + c.token.charCodeAt(i)) >>> 0; }
    return 'acq_draft_' + uin + '_' + c.seat + '_' + h.toString(16);
  };
  const browser = await puppeteer.launch({ executablePath: process.env.CHROME_PATH || 'google-chrome', args: ['--no-sandbox', '--disable-dev-shm-usage'] });
  try {
    const page = await browser.newPage();
    await page.setRequestInterception(true);
    page.on('request', r => { if (isLoopbackHttp(r.url())) r.continue(); else r.abort(); });
    const seed = (creds) => page.evaluateOnNewDocument((cs) => { localStorage.setItem('acq_credentials', JSON.stringify(cs)); }, creds);
    const openRoom = async (base, uin) => {
      await page.goto(base + '/room/' + encodeURIComponent(uin), { waitUntil: 'domcontentloaded' });
      await page.waitForFunction(() => !document.getElementById('composer').hidden, { timeout: 10000, polling: 200 });
    };
    const draftVal = () => page.$eval('#draft', el => el.value);
    const storeRaw = (k) => page.evaluate((kk) => sessionStorage.getItem(kk), k);
    const storeObj = async (k) => { const v = await storeRaw(k); return v ? JSON.parse(v) : null; };
    const K1 = keyOf(room1.roomId, room1.credential), K2 = keyOf(room2.roomId, room2.credential), KB = keyOf(room1.roomId, credB);

    await seed([room1.credential]);
    await openRoom(BASE, room1.roomId);

    // 1: typing saves (with cid field present); emptying removes
    await page.type('#draft', 'draft one');
    const s1 = await storeObj(K1);
    ok('save-on-input', s1 && s1.t === 'draft one' && ('c' in s1), JSON.stringify(s1));
    await page.$eval('#draft', el => { el.value = ''; el.dispatchEvent(new Event('input', { bubbles: true })); });
    ok('empty-removes-key', (await storeRaw(K1)) === null);

    // 2: refresh recovery
    await page.type('#draft', 'draft one');
    await page.reload({ waitUntil: 'domcontentloaded' });
    await page.waitForFunction(() => !document.getElementById('composer').hidden, { timeout: 10000, polling: 200 });
    ok('refresh-restores', (await draftVal()) === 'draft one');
    await page.screenshot({ path: '/tmp/acq-draft-test/restored-composer.png' });

    // 3: confirmed send clears box AND stored copy, surviving refresh
    await page.$eval('#draft', el => { el.value = 'send me'; el.dispatchEvent(new Event('input', { bubbles: true })); });
    await page.click('#post');
    await page.waitForFunction(() => document.getElementById('draft').value === '', { timeout: 10000, polling: 200 });
    ok('send-clears-stored', (await storeRaw(K1)) === null);
    await page.reload({ waitUntil: 'domcontentloaded' });
    await page.waitForFunction(() => !document.getElementById('composer').hidden, { timeout: 10000, polling: 200 });
    ok('post-send-refresh-empty', (await draftVal()) === '');

    // 4: cross-room isolation in the same tab
    await seed([room1.credential, room2.credential]);
    await openRoom(BASE, room2.roomId);
    await page.type('#draft', 'room two draft');
    ok('room2-saved-under-own-key', ((await storeObj(K2)) || {}).t === 'room two draft');
    await openRoom(BASE, room1.roomId);
    ok('room1-sees-no-room2-draft', (await draftVal()) === '');
    await openRoom(BASE, room2.roomId);
    ok('room2-draft-survives-navigation', (await draftVal()) === 'room two draft');

    // 5: cross-seat isolation, same room, same tab
    await seed([credB]);
    await openRoom(BASE, room1.roomId);
    ok('seatB-sees-no-seatA-draft', (await draftVal()) === '');

    // 6: a fresh tab gets nothing
    const page2 = await browser.newPage();
    await page2.setRequestInterception(true);
    page2.on('request', r => { if (isLoopbackHttp(r.url())) r.continue(); else r.abort(); });
    await page2.evaluateOnNewDocument((cs) => { localStorage.setItem('acq_credentials', JSON.stringify(cs)); }, [room2.credential]);
    await page2.goto(BASE + '/room/' + encodeURIComponent(room2.roomId), { waitUntil: 'domcontentloaded' });
    await page2.waitForFunction(() => !document.getElementById('composer').hidden, { timeout: 10000, polling: 200 });
    ok('fresh-tab-no-draft', (await page2.$eval('#draft', el => el.value)) === '');
    await page2.close();

    // 7: opener-created tab INHERITS the copy (truthful sessionStorage behavior)
    await page.evaluate((cs) => localStorage.setItem('acq_credentials', JSON.stringify(cs)), [room1.credential, room2.credential]);
    const beforeTargets = new Set(await browser.targets());
    const newTarget = browser.waitForTarget(t => !beforeTargets.has(t) && t.url().includes(encodeURIComponent(room2.roomId)), { timeout: 8000 });
    await page.evaluate((u) => window.open(u, '_blank'), BASE + '/room/' + encodeURIComponent(room2.roomId));
    const page3 = await (await newTarget).page();
    await page3.waitForFunction(() => !document.getElementById('composer').hidden, { timeout: 10000, polling: 200 });
    ok('opener-tab-inherits-copy', (await page3.$eval('#draft', el => el.value)) === 'room two draft');
    await page3.close();

    // 8: cid binding across refresh between unconfirmed send and retry.
    // Proxy blackhole: server commits the POST, every response is destroyed
    // (Chrome's transparent in-flight retry also dies), the client keeps
    // the text; refresh; retry must resubmit the SAME cid and commit no
    // second message.
    await seed([room1.credential]);
    await openRoom(PBASE, room1.roomId);
    const sentCids = [];
    page.on('request', r => {
      if (r.method() === 'POST' && r.url().includes('/msg')) {
        try { sentCids.push(JSON.parse(r.postData() || '{}').cid || 'NO-CID'); } catch (e) { sentCids.push('PARSE-ERR'); }
      }
    });
    await page.type('#draft', 'cid binding probe');
    fs.writeFileSync(FLAGS + '/blackhole', '1');
    await page.click('#post');
    await page.waitForFunction(() => document.getElementById('send-err').textContent.length > 3, { timeout: 20000, polling: 300 });
    ok('blackholed-send-keeps-text', (await draftVal()) === 'cid binding probe');
    fs.unlinkSync(FLAGS + '/blackhole');
    await page.reload({ waitUntil: 'domcontentloaded' });
    await page.waitForFunction(() => !document.getElementById('composer').hidden, { timeout: 10000, polling: 200 });
    ok('unconfirmed-draft-restored', (await draftVal()) === 'cid binding probe');
    await page.click('#post');
    await page.waitForFunction(() => document.getElementById('draft').value === '', { timeout: 10000, polling: 200 });
    const distinct = [...new Set(sentCids)];
    ok('retry-reuses-same-cid', sentCids.length >= 2 && distinct.length === 1 && distinct[0] !== 'NO-CID' && distinct[0] !== 'PARSE-ERR', JSON.stringify(sentCids));
    const tr = await (await safeFetch(BASE + '/room/' + room1.roomId + '/msg?since=0', { headers: { Authorization: 'Bearer ' + joinB.token } })).json();
    const copies = tr.messages.filter(m => m.text === 'cid binding probe').length;
    ok('exactly-one-committed', copies === 1, 'copies=' + copies);

    // 9: a restored over-cap draft re-engages the cap UI immediately
    // (restore runs the same draftOverCap measurement as typing)
    await seed([room2.credential]);
    await openRoom(BASE, room2.roomId);
    await page.$eval('#draft', el => { el.value = 'x'.repeat(65537); el.dispatchEvent(new Event('input', { bubbles: true })); });
    await page.reload({ waitUntil: 'domcontentloaded' });
    await page.waitForFunction(() => !document.getElementById('composer').hidden, { timeout: 10000, polling: 200 });
    const over = await page.evaluate(() => ({ len: document.getElementById('draft').value.length, disabled: document.getElementById('post').disabled, note: document.getElementById('draft-note').hidden }));
    ok('restored-overcap-held', over.len === 65537 && over.disabled === true && over.note === false, JSON.stringify(over));
  } finally {
    try { fs.unlinkSync(FLAGS + '/blackhole'); } catch (e) {}
    await browser.close();
  }
  console.log(fails === 0 ? 'ALL PASS' : fails + ' FAILURES');
  process.exit(fails === 0 ? 0 : 1);
})().catch(e => { console.error('SETUP ERROR: ' + (e && e.message)); process.exit(2); });
