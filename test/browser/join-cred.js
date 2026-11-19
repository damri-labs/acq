// P14: join credential-validation boundary. A 200 whose body is missing or
// carries an unusable credential must NOT latch the join: an outstanding
// attempt's valid late reply can still seat the browser, and the UI must
// say the seat may-or-may-not be claimed instead of asserting a claim.
// Needs the proxy (PROXY_URL) with FLAGS_DIR shared.
const puppeteer = require('puppeteer-core');
const fs = require('fs');
const BASE = process.env.BASE_URL || 'http://127.0.0.1:8099';
const PROXY = process.env.PROXY_URL || 'http://127.0.0.1:8100';
const FLAGS = process.env.FLAGS_DIR || '/tmp/acq-ui-flags';
const CHROME = process.env.CHROME_PATH || '/usr/bin/google-chrome';
const results = [];
let failures = 0;
const ok = (name, cond, extra) => { if (!cond) failures++; results.push((cond ? 'PASS ' : 'FAIL ') + name + (extra ? ' | ' + extra : '')); };
const sleep = (ms) => new Promise(r => setTimeout(r, ms));
const snap = (pg) => pg.evaluate(() => ({
  btnDisabled: document.getElementById('join-human') ? document.getElementById('join-human').disabled : null,
  err: (document.getElementById('join-human-err') || {}).textContent || '',
  pill: (document.getElementById('pill') || {}).textContent || '',
  inviteHidden: document.getElementById('invite') ? document.getElementById('invite').hidden : null,
}));
const status = (uin) => fetch(BASE + '/room/' + uin, { headers: { Accept: 'application/json' } }).then(r => r.json());

(async () => {
  const browser = await puppeteer.launch({ executablePath: CHROME, headless: 'new', args: ['--no-sandbox'] });

  // P14a: 200 with MISSING credential and no other attempt - no latch, no
  // false claim copy; a later real join still seats this browser.
  if (true) {
    const room = await fetch(BASE + '/room', { method: 'POST' }).then(r => r.json());
    const ctx = await browser.createBrowserContext();
    const pg = await ctx.newPage();
    await pg.goto(PROXY + '/room/' + room.roomId, { waitUntil: 'networkidle0' });
    fs.writeFileSync(FLAGS + '/bogusjoin', '{}'); // synthetic 200, no credential, never forwarded
    await pg.click('#join-human');
    await sleep(1500);
    const bad = await snap(pg);
    ok('P14a missing credential is not a claim', /not a usable seat credential/.test(bad.err), bad.err);
    ok('P14a page not seated', bad.pill !== 'paired', 'pill=' + bad.pill);
    ok('P14a button usable again', bad.btnDisabled === false);
    let st = await status(room.roomId);
    ok('P14a wire still unpaired (bogus 200 never reached the server)', st.paired === false, JSON.stringify(st.paired));
    await pg.evaluate(() => document.getElementById('join-human').click()); // real attempt, no flag left
    await sleep(2500);
    const good = await snap(pg);
    ok('P14a no latch: a later valid join still seats', good.pill === 'paired', 'pill=' + good.pill);
    st = await status(room.roomId);
    ok('P14a wire paired exactly once', st.paired === true);
    await ctx.close();
  }

  // P14b: malformed-credential variant (acq1_ blob that does not decode),
  // sequential: the latch must not suppress the next valid claim either.
  if (true) {
    const room = await fetch(BASE + '/room', { method: 'POST' }).then(r => r.json());
    const ctx = await browser.createBrowserContext();
    const pg = await ctx.newPage();
    await pg.goto(PROXY + '/room/' + room.roomId, { waitUntil: 'networkidle0' });
    fs.writeFileSync(FLAGS + '/bogusjoin', '{"credential":"acq1_bogus","seatKind":"human"}');
    await pg.click('#join-human');
    await sleep(1500);
    const mid = await snap(pg);
    ok('P14b malformed response named honestly', /not a usable seat credential/.test(mid.err), mid.err);
    ok('P14b malformed response did not seat', mid.pill !== 'paired', 'pill=' + mid.pill);
    await pg.evaluate(() => document.getElementById('join-human').click());
    await sleep(2500);
    const late = await snap(pg);
    ok('P14b no latch: later valid join still seats', late.pill === 'paired', 'pill=' + late.pill + ' err=' + late.err);
    const st = await status(room.roomId);
    ok('P14b wire paired exactly once', st.paired === true);
    await ctx.close();
  }

  console.log(results.join('\n'));
  console.log(failures ? 'FAILURES: ' + failures : 'ALL PASS');
  await browser.close();
  process.exit(failures ? 1 : 0);
})().catch(e => { console.error('DRIVER ERROR', e); process.exit(2); });
