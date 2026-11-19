// Topic byte-limit suite (landing page, #open-topic): a non-ASCII topic
// must never be shortened silently. While the trimmed topic exceeds the
// server's 200-byte cap, the page must keep the user's text untouched,
// show the byte size and the exact prefix a room would store, and hold
// the submit (click AND Enter). A composition-confirming Enter (IME)
// must never submit; an ordinary Enter under the cap must.
//
// Run: BASE_URL=http://127.0.0.1:8099 node topic-limit.js
// against a locally built server (no proxy needed). Use a free loopback
// port and an isolated throwaway ROOMS_DIR per run: the suite creates
// real rooms on that server by design (the two under-cap cases). BASE_URL is
// enforced loopback-only before any browser launch, so this suite can
// never be pointed at a deployed service.
const puppeteer = require('puppeteer-core');
const BASE = process.env.BASE_URL || 'http://127.0.0.1:8099';
const results = [];
let failures = 0;
const ok = (name, cond, extra) => { if (!cond) failures++; results.push((cond ? 'PASS' : 'FAIL') + ' ' + name + (extra ? ' :: ' + extra : '')); };
const sleep = ms => new Promise(r => setTimeout(r, ms));

// 150+ invented Hebrew characters (2 UTF-8 bytes each) -> over 200 bytes.
const HEBREW_OVER = 'סיכום דיון צוות המוצר על שיפור זרימת ההצטרפות למשתמשים חדשים, כולל שינויי מסך הפתיחה, קיצור טופס הרישום, והוספת הדרכה קצרה בשלושה צעדים שתוצג בפעם הראשונה שהמשתמש פותח את היישום לאחר העדכון';
const HEBREW_UNDER = 'סיכום דיון צוות';
const utf8 = s => new TextEncoder().encode(s).length;

(async () => {
  // Hard scope guard, fail closed at two layers:
  // 1. refuse a non-loopback BASE_URL BEFORE the browser launches;
  // 2. intercept every http(s) request the page makes (including redirect
  //    hops, subresources and form posts) and abort any non-loopback
  //    destination, so no page activity can leave the local server.
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
  const browser = await puppeteer.launch({ executablePath: process.env.CHROME_PATH || 'google-chrome', args: ['--no-sandbox', '--disable-dev-shm-usage'] });
  try {
  const page = await browser.newPage();
  await page.setViewport({ width: 1100, height: 900 });

  // Single request listener: loopback requests continue (and room creates
  // are counted here), anything else is aborted before it is sent.
  let roomCreates = 0;
  let nonLoopbackFinished = 0;
  let nonLoopbackBlocked = 0;
  await page.setRequestInterception(true);
  page.on('request', r => {
    if (isLoopbackHttp(r.url())) {
      if (r.method() === 'POST' && new URL(r.url()).pathname === '/room') roomCreates++;
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

  // -- over cap: paste-like entry keeps the user's text, shows the note --
  await page.goto(BASE + '/', { waitUntil: 'domcontentloaded' });
  await page.click('#open-topic');
  await page.type('#open-topic', HEBREW_OVER, { delay: 0 });
  const over = await page.evaluate(() => {
    const i = document.getElementById('open-topic');
    const n = document.getElementById('open-topic-note');
    return { value: i.value, shown: n && !n.hidden, note: n ? n.textContent : '' };
  });
  ok('overcap: user text preserved, not rewritten', over.value === HEBREW_OVER, 'chars=' + over.value.length);
  ok('overcap: note visible with exact byte size', over.shown && over.note.includes(utf8(HEBREW_OVER) + ' bytes'), over.note.slice(0, 60));

  // preview invariant, checked against the property (not a reimplemented
  // algorithm): the previewed prefix must be a prefix of the input, fit
  // the cap, and be valid text (no U+FFFD from a mid-rune cut).
  const preview = (over.note.match(/stored as: "([^"]*)"/) || [])[1] || '';
  ok('overcap: preview is a real prefix of the input', preview.length > 0 && HEBREW_OVER.startsWith(preview), 'preview chars=' + preview.length);
  ok('overcap: preview fits the 200-byte cap, no U+FFFD', utf8(preview) <= 200 && !preview.includes('�'), 'bytes=' + utf8(preview));

  // -- over cap: neither click nor Enter may create a room --
  await page.click('#open-go');
  await sleep(400);
  await page.keyboard.press('Enter');
  await sleep(400);
  ok('overcap: click+Enter create nothing, no POST /room', roomCreates === 0 && page.url() === BASE + '/', 'url=' + page.url());

  // -- IME: a composition-confirming Enter must never submit --
  // (reload first: unfixed code navigated away in the block above)
  await page.goto(BASE + '/', { waitUntil: 'domcontentloaded' });
  await page.evaluate(() => {
    const i = document.getElementById('open-topic');
    i.value = '';
    i.dispatchEvent(new Event('input', { bubbles: true }));
  });
  await page.type('#open-topic', HEBREW_UNDER, { delay: 0 });
  await page.evaluate(() => {
    document.getElementById('open-topic').dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', isComposing: true, bubbles: true }));
  });
  await sleep(400);
  ok('ime: composition-confirm Enter creates nothing', roomCreates === 0 && page.url() === BASE + '/', 'url=' + page.url());

  // -- under cap: note cleared, ordinary Enter creates exactly one room
  //    and the stored topic equals the entered text byte-for-byte --
  const cleared = await page.evaluate(() => { const n = document.getElementById('open-topic-note'); return n ? n.hidden : null; });
  await Promise.all([
    page.waitForNavigation({ waitUntil: 'networkidle0', timeout: 8000 }),
    page.keyboard.press('Enter'),
  ]).catch(() => {});
  const stored = await page.evaluate(() => {
    const t = document.getElementById('topic');
    return t ? t.textContent.replace(/ - declared by the creator, not verified$/, '') : null;
  });
  ok('undercap: note cleared once the topic fits', cleared === true, '');
  ok('undercap: ordinary Enter creates exactly one room', roomCreates === 1 && page.url().includes('/room/'), 'creates=' + roomCreates);
  ok('undercap: stored topic equals entered text', stored === HEBREW_UNDER, JSON.stringify(stored));

  // -- whitespace-padded under cap: the page counts the TRIMMED topic, so
  //    padding that pushes the raw value over 200 bytes must not warn, and
  //    the room must store the trimmed text byte-for-byte --
  await page.goto(BASE + '/', { waitUntil: 'domcontentloaded' });
  const padded = ' '.repeat(150) + HEBREW_UNDER + ' '.repeat(150); // raw 328 bytes, trimmed 28
  await page.click('#open-topic');
  await page.type('#open-topic', padded, { delay: 0 });
  const paddedNote = await page.evaluate(() => { const n = document.getElementById('open-topic-note'); return n ? n.hidden : null; });
  ok('padded undercap: note stays hidden (trimmed value fits)', paddedNote === true, 'raw bytes=' + utf8(padded));
  await Promise.all([
    page.waitForNavigation({ waitUntil: 'networkidle0', timeout: 8000 }),
    page.keyboard.press('Enter'),
  ]).catch(() => {});
  const storedPadded = await page.evaluate(() => {
    const t = document.getElementById('topic');
    return t ? t.textContent.replace(/ - declared by the creator, not verified$/, '') : null;
  });
  ok('padded undercap: stored topic is the trimmed text', storedPadded === HEBREW_UNDER, JSON.stringify(storedPadded));

  // -- guard self-test: a non-loopback navigation must be refused by the
  //    interception layer; the destination request is aborted, never sent --
  await page.goto('https://acq.damri.org/', { waitUntil: 'domcontentloaded', timeout: 8000 }).catch(() => {});
  ok('guard: non-loopback navigation aborted, destination never completed', nonLoopbackBlocked >= 1 && nonLoopbackFinished === 0,
     'blocked=' + nonLoopbackBlocked + ' finished=' + nonLoopbackFinished);

  } finally {
    await browser.close();
  }
  console.log(results.join('\n'));
  if (failures > 0) { console.error(failures + ' assertion(s) FAILED'); process.exitCode = 1; }
})().catch(e => { console.error(e); console.log(results.join('\n')); process.exit(1); });
