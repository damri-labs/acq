// P10: join 200 headers arrive fast but the body is held - the join watchdog
//      must stay armed until the body settles.
// P11: same for the status check.
// P12: a newly started join invalidates a still-pending status check's right
//      to overwrite the join's pending UI.
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
  checkHidden: document.getElementById('join-check') ? document.getElementById('join-check').hidden : null,
  err: (document.getElementById('join-human-err') || {}).textContent || '',
  pill: (document.getElementById('pill') || {}).textContent || '',
  inviteHidden: document.getElementById('invite') ? document.getElementById('invite').hidden : null,
  checkDisabled: document.getElementById('join-check') ? document.getElementById('join-check').disabled : null,
}));
const ONLY = (process.env.ONLY || '').split(',').filter(Boolean);
const want = (n) => !ONLY.length || ONLY.includes(n);

(async () => {
  const browser = await puppeteer.launch({ executablePath: CHROME, headless: 'new', args: ['--no-sandbox'] });

  if (want('P10')) {
    const room = await fetch(BASE + '/room', { method: 'POST' }).then(r => r.json());
    const ctx = await browser.createBrowserContext();
    const pg = await ctx.newPage();
    await pg.goto(PROXY + '/room/' + room.roomId, { waitUntil: 'networkidle0' });
    fs.writeFileSync(FLAGS + '/holdbodyjoin', '18000'); // 200 headers now, body at ~18s
    await pg.click('#join-human');
    await sleep(2000);
    const early = await snap(pg);
    ok('P10 pending shown while body held', /Joining/.test(early.err), early.err);
    await sleep(14000); // 16s: headers long since arrived; watchdog must still fire
    const mid = await snap(pg);
    ok('P10 watchdog survives early headers (body unsettled)', /Still waiting/.test(mid.err) && mid.checkHidden === false, mid.err);
    await sleep(5000); // ~21s: body arrived -> seated
    const late = await snap(pg);
    ok('P10 body settlement seats the room', late.inviteHidden === true && /paired/.test(late.pill), late.pill);
    await ctx.close();
  }

  if (want('P11')) {
    const room = await fetch(BASE + '/room', { method: 'POST' }).then(r => r.json());
    const ctx = await browser.createBrowserContext();
    const pg = await ctx.newPage();
    await pg.goto(PROXY + '/room/' + room.roomId, { waitUntil: 'networkidle0' });
    fs.writeFileSync(FLAGS + '/blackholejoin', '1'); // join never commits
    await pg.click('#join-human');
    await sleep(16000); // join watchdog
    fs.writeFileSync(FLAGS + '/holdbodystatus', '17000'); // check: headers now, body at +17s
    await pg.click('#join-check');
    await sleep(1000);
    const early = await snap(pg);
    ok('P11 checking shown while body held', /Checking/.test(early.err), early.err);
    await sleep(15500); // 16.5s: check watchdog must fire despite early headers
    const mid = await snap(pg);
    ok('P11 check watchdog survives early headers', /Still checking/.test(mid.err), mid.err);
    await sleep(4000); // ~20.5s: body arrived -> snapshot
    const late = await snap(pg);
    ok('P11 body settlement delivers the snapshot', /Still open as of this check/.test(late.err), late.err);
    await ctx.close();
  }

  if (want('P12')) {
    const room = await fetch(BASE + '/room', { method: 'POST' }).then(r => r.json());
    const ctx = await browser.createBrowserContext();
    const pg = await ctx.newPage();
    await pg.goto(PROXY + '/room/' + room.roomId, { waitUntil: 'networkidle0' });
    fs.writeFileSync(FLAGS + '/delayjoin', '20000');
    await pg.click('#join-human'); // join A, resolves ~20s
    await sleep(16000); // watchdog at 15s
    fs.writeFileSync(FLAGS + '/delaystatus', '12000');
    await pg.click('#join-check'); // check C1, resolves ~28s
    await sleep(2000); // ~18s
    await fetch(BASE + '/room/' + room.roomId + '/join', { method: 'POST' }); // external claim
    await sleep(3000); // ~21s: A settled 409, button re-enabled
    const afterA = await snap(pg);
    ok('P12 A met the honest 409 and re-enabled', /cannot be reclaimed|lost reply/.test(afterA.err) && afterA.btnDisabled === false, afterA.err);
    fs.writeFileSync(FLAGS + '/delayjoin', '25000');
    await pg.click('#join-human'); // join B, pending until ~46s
    await sleep(9000); // ~30s: C1 resolved at ~28s while B pending
    const duringB = await snap(pg);
    ok('P12 stale check cannot overwrite the new pending join', /Joining/.test(duringB.err) && duringB.btnDisabled === true, duringB.err);
    ok('P12 stale check cannot re-enable the join button', duringB.btnDisabled === true);
    await ctx.close(); // B abandoned; nothing more to prove
  }


  if (want('P13')) {
    const room = await fetch(BASE + '/room', { method: 'POST' }).then(r => r.json());
    const ctx = await browser.createBrowserContext();
    const pg = await ctx.newPage();
    await pg.goto(PROXY + '/room/' + room.roomId, { waitUntil: 'networkidle0' });
    fs.writeFileSync(FLAGS + '/blackholejoin', '');
    await pg.click('#join-human'); // never answered -> 15s watchdog reveals the check button
    await sleep(16000);
    const exposed = await snap(pg);
    ok('P13 watchdog exposes manual check', exposed.checkHidden === false && /Still waiting/.test(exposed.err), exposed.err);
    fs.writeFileSync(FLAGS + '/holdbodystatus', '17000'); // check C1: headers now, body ~17s
    await pg.click('#join-check');
    await sleep(16000); // ~16s: C1 watchdog re-enables the button while C1's body is still held
    const c1w = await snap(pg);
    ok('P13 C1 watchdog re-enables for a manual recheck', c1w.checkDisabled === false && /Still checking/.test(c1w.err), c1w.err);
    fs.writeFileSync(FLAGS + '/holdbodystatus', '4000'); // check C2: body ~4s
    await pg.click('#join-check'); // C2 starts, button disabled again
    await sleep(1500); // ~17.5s: C1's held body has now completed (17s) while C2 is outstanding
    const during = await snap(pg);
    ok('P13 stale check body cannot enable a newer outstanding check', during.checkDisabled === true, 'checkDisabled=' + during.checkDisabled);
    ok('P13 stale check body cannot overwrite the pending state', /Checking/.test(during.err), during.err);
    await sleep(4000); // ~21.5s: C2 settles -> its snapshot owns the UI
    const late = await snap(pg);
    ok('P13 newest check owns the final state', late.checkDisabled === false && /Still open as of this check/.test(late.err), late.err);
    await ctx.close();
  }

  console.log(results.join('\n'));
  console.log(failures ? 'FAILURES: ' + failures : 'ALL PASS');
  await browser.close();
  process.exit(failures ? 1 : 0);
})().catch(e => { console.error('DRIVER ERROR', e); process.exit(2); });
