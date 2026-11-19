// Tests for the proposed guest-join pending/recovery UI (room.html patch).
// P1: delayed join shows a visible pending state, then joins.
// P2: blackholed join -> watchdog offers a manual check -> check reads an
//     unpaired snapshot with truthful in-flight wording -> deliberate retry joins.
// P3: commit-then-lost join -> honest paired wording incl. lost-reply
//     possibility; no duplicate claim; button stays disabled.
// P4: status check itself fails -> state stays unknown, no re-enable; a later
//     successful check recovers.
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
const ONLY = (process.env.ONLY || '').split(',').filter(Boolean);
const want = (n) => !ONLY.length || ONLY.includes(n);
const snap = (pg) => pg.evaluate(() => ({
  btnDisabled: document.getElementById('join-human') ? document.getElementById('join-human').disabled : null,
  checkHidden: document.getElementById('join-check') ? document.getElementById('join-check').hidden : null,
  err: (document.getElementById('join-human-err') || {}).textContent || '',
  pill: (document.getElementById('pill') || {}).textContent || '',
  inviteHidden: document.getElementById('invite') ? document.getElementById('invite').hidden : null,
}));
const status = (uin) => fetch(BASE + '/room/' + uin, { headers: { Accept: 'application/json' } }).then(r => r.json());

(async () => {
  const browser = await puppeteer.launch({ executablePath: CHROME, headless: 'new', args: ['--no-sandbox'] });

  // P1: delayed join (6s)
  if (want('P1')) {
    const room = await fetch(BASE + '/room', { method: 'POST' }).then(r => r.json());
    const ctx = await browser.createBrowserContext();
    const pg = await ctx.newPage();
    await pg.goto(PROXY + '/room/' + room.roomId, { waitUntil: 'networkidle0' });
    fs.writeFileSync(FLAGS + '/delayjoin', '1');
    await pg.click('#join-human');
    await sleep(2000);
    const mid = await snap(pg);
    ok('P1 pending state visible during delay', mid.btnDisabled === true && /Joining/.test(mid.err), mid.err);
    await sleep(6000);
    const after = await snap(pg);
    ok('P1 delayed join completes into the room', after.inviteHidden === true && /paired/.test(after.pill), after.pill);
    await ctx.close();
  }

  // P2: blackholed join -> watchdog -> check proves uncommitted -> retry joins
  if (want('P2')) {
    const room = await fetch(BASE + '/room', { method: 'POST' }).then(r => r.json());
    const ctx = await browser.createBrowserContext();
    const pg = await ctx.newPage();
    await pg.goto(PROXY + '/room/' + room.roomId, { waitUntil: 'networkidle0' });
    fs.writeFileSync(FLAGS + '/blackholejoin', '1');
    await pg.click('#join-human');
    await sleep(16000); // past the 15s watchdog
    const mid = await snap(pg);
    ok('P2 watchdog explains the stall and offers a check', /Still waiting/.test(mid.err) && mid.checkHidden === false, mid.err);
    ok('P2 join button stays disabled while unknown', mid.btnDisabled === true);
    await pg.click('#join-check');
    await sleep(1500);
    const checked = await snap(pg);
    ok('P2 check is snapshot-honest, no false noncommit certainty', /Still open as of this check/.test(checked.err) && /may still be in flight/.test(checked.err) && !/did not reach/.test(checked.err), checked.err);
    ok('P2 button re-enabled for a deliberate retry', checked.btnDisabled === false);
    await pg.click('#join-human');
    await sleep(2500);
    const joined = await snap(pg);
    const st = await status(room.roomId);
    ok('P2 manual retry joins the room', joined.inviteHidden === true && /paired/.test(joined.pill) && st.paired === true, joined.pill);
    await ctx.close();
  }

  // P3: commit-then-lost join response
  if (want('P3')) {
    const room = await fetch(BASE + '/room', { method: 'POST' }).then(r => r.json());
    const ctx = await browser.createBrowserContext();
    const pg = await ctx.newPage();
    await pg.goto(PROXY + '/room/' + room.roomId, { waitUntil: 'networkidle0' });
    fs.writeFileSync(FLAGS + '/commitlossjoin', '1');
    await pg.click('#join-human');
    await sleep(8000);
    const after = await snap(pg);
    const st = await status(room.roomId);
    ok('P3 room paired exactly once (no duplicate claim)', st.paired === true);
    // Chrome retries the lost POST into the server's 409; the wording must
    // carry the lost-own-reply possibility even on the first visible click.
    ok('P3 wording keeps lost-own-reply possibility', /cannot be reclaimed|lost reply/.test(after.err), after.err);
    ok('P3 no false "room full" alone (stranger implication only)', !/^This room is full - both seats are taken\.$/.test(after.err), after.err);
    // A manual re-click is allowed (no auto-retry anywhere); it must meet the
    // same honest 409 and still claim nothing.
    await pg.click('#join-human');
    await sleep(2000);
    const again = await snap(pg);
    const st2 = await status(room.roomId);
    ok('P3 deliberate re-click meets the same honest 409, no duplicate claim', /cannot be reclaimed|lost reply/.test(again.err) && st2.paired === true, again.err);
    await ctx.close();
  }

  // P4: the check itself fails -> unknown stays unknown
  if (want('P4')) {
    const room = await fetch(BASE + '/room', { method: 'POST' }).then(r => r.json());
    const ctx = await browser.createBrowserContext();
    const pg = await ctx.newPage();
    await pg.goto(PROXY + '/room/' + room.roomId, { waitUntil: 'networkidle0' });
    fs.writeFileSync(FLAGS + '/blackholejoin', '1');
    await pg.click('#join-human');
    await sleep(16000); // watchdog fires
    fs.writeFileSync(FLAGS + '/blackholeall', '1'); // now the check fails too
    await pg.click('#join-check');
    await sleep(2500);
    const failed = await snap(pg);
    ok('P4 failed check keeps state unknown', /Still unknown/.test(failed.err), failed.err);
    ok('P4 failed check does not re-enable join', failed.btnDisabled === true);
    ok('P4 check action remains available', failed.checkHidden === false);
    fs.unlinkSync(FLAGS + '/blackholeall');
    await pg.click('#join-check');
    await sleep(1500);
    const recovered = await snap(pg);
    ok('P4 later successful check reads still-open snapshot', /Still open as of this check/.test(recovered.err), recovered.err);
    ok('P4 retry enabled after still-open read', recovered.btnDisabled === false);
    const st = await status(room.roomId);
    ok('P4 wire still unpaired (nothing committed)', st.paired === false);
    await ctx.close();
  }

  // P5: delayed original commits AFTER the check read; the deliberate retry
  // wins the seat first, and the stale original's late 409 must be ignored.
  if (want('P5')) {
    const room = await fetch(BASE + '/room', { method: 'POST' }).then(r => r.json());
    const ctx = await browser.createBrowserContext();
    const pg = await ctx.newPage();
    await pg.goto(PROXY + '/room/' + room.roomId, { waitUntil: 'networkidle0' });
    fs.writeFileSync(FLAGS + '/delayjoin', '20000'); // original held 20s
    await pg.click('#join-human');
    await sleep(16000); // watchdog
    await pg.click('#join-check'); // snapshot: unpaired (original still held)
    await sleep(1200);
    const checked = await snap(pg);
    ok('P5 still-open snapshot while original in flight', /Still open as of this check/.test(checked.err) && checked.btnDisabled === false, checked.err);
    await pg.click('#join-human'); // deliberate retry, not delayed: commits first
    await sleep(2500);
    const joined = await snap(pg);
    ok('P5 retry seats the browser', joined.inviteHidden === true && /paired/.test(joined.pill), joined.pill);
    await sleep(6000); // original forwards now and must lose with a stale 409
    const late = await snap(pg);
    const st = await status(room.roomId);
    ok('P5 stale late 409 does not clobber the joined room', late.inviteHidden === true && /paired/.test(late.pill), late.pill);
    ok('P5 exactly one claim on the wire', st.paired === true);
    await ctx.close();
  }

  // P6: delayed original commits AFTER the check read; with NO retry, its
  // late success still seats the browser (the reply carries the credential).
  if (want('P6')) {
    const room = await fetch(BASE + '/room', { method: 'POST' }).then(r => r.json());
    const ctx = await browser.createBrowserContext();
    const pg = await ctx.newPage();
    await pg.goto(PROXY + '/room/' + room.roomId, { waitUntil: 'networkidle0' });
    fs.writeFileSync(FLAGS + '/delayjoin', '19000'); // original held 19s
    await pg.click('#join-human');
    await sleep(16000); // watchdog
    await pg.click('#join-check'); // snapshot while original in flight
    await sleep(1200);
    const checked = await snap(pg);
    ok('P6 still-open snapshot while original in flight', /Still open as of this check/.test(checked.err), checked.err);
    await sleep(6000); // original forwards and resolves 200 at ~19s
    const late = await snap(pg);
    const st = await status(room.roomId);
    ok('P6 late original success still seats the browser', late.inviteHidden === true && /paired/.test(late.pill), late.pill);
    ok('P6 exactly one claim on the wire', st.paired === true);
    await ctx.close();
  }

  console.log(results.join('\n'));
  console.log(failures ? 'FAILURES: ' + failures : 'ALL PASS');
  await browser.close();
  process.exit(failures ? 1 : 0);
})().catch(e => { console.error('DRIVER ERROR', e); process.exit(2); });
