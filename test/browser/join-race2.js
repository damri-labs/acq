// P7-P9: stale-check guards, out-of-order retries, timer locality, check hang.
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
  checkDisabled: document.getElementById('join-check') ? document.getElementById('join-check').disabled : null,
  err: (document.getElementById('join-human-err') || {}).textContent || '',
  pill: (document.getElementById('pill') || {}).textContent || '',
  inviteHidden: document.getElementById('invite') ? document.getElementById('invite').hidden : null,
  gone: /expired or deleted/.test(document.body.innerText),
}));
const status = (uin) => fetch(BASE + '/room/' + uin, { headers: { Accept: 'application/json' } }).then(r => r.json());
const ONLY = (process.env.ONLY || '').split(',').filter(Boolean);
const want = (n) => !ONLY.length || ONLY.includes(n);

(async () => {
  const browser = await puppeteer.launch({ executablePath: CHROME, headless: 'new', args: ['--no-sandbox'] });

  // P7a: join succeeds WHILE the check is outstanding; the late check (200
  // paired=true) must not clobber the seated room.
  if (want('P7a')) {
    const room = await fetch(BASE + '/room', { method: 'POST' }).then(r => r.json());
    const ctx = await browser.createBrowserContext();
    const pg = await ctx.newPage();
    await pg.goto(PROXY + '/room/' + room.roomId, { waitUntil: 'networkidle0' });
    fs.writeFileSync(FLAGS + '/delayjoin', '17000');
    await pg.click('#join-human');
    await sleep(16000); // join watchdog fires
    fs.writeFileSync(FLAGS + '/delaystatus', '4000');
    await pg.click('#join-check'); // check resolves ~19.5s, join resolves ~17s
    await sleep(6000);
    const s = await snap(pg);
    const st = await status(room.roomId);
    ok('P7a late check (paired=true) does not clobber seated room', s.inviteHidden === true && /paired/.test(s.pill) && !s.gone, s.pill);
    ok('P7a exactly one claim', st.paired === true);
    await ctx.close();
  }

  // P7b: late check answers a synthetic 410 after the join seated us; must
  // NOT trigger showGone.
  if (want('P7b')) {
    const room = await fetch(BASE + '/room', { method: 'POST' }).then(r => r.json());
    const ctx = await browser.createBrowserContext();
    const pg = await ctx.newPage();
    await pg.goto(PROXY + '/room/' + room.roomId, { waitUntil: 'networkidle0' });
    fs.writeFileSync(FLAGS + '/delayjoin', '17000');
    await pg.click('#join-human');
    await sleep(16000);
    fs.writeFileSync(FLAGS + '/delaystatus', '4000');
    fs.writeFileSync(FLAGS + '/failstatus', '1'); // the delayed check GET gets a synthetic 410
    await pg.click('#join-check');
    await sleep(6000);
    const s = await snap(pg);
    ok('P7b late 410 check does not showGone a seated room', s.inviteHidden === true && /paired/.test(s.pill) && !s.gone, 'pill=' + s.pill + ' gone=' + s.gone);
    await ctx.close();
  }

  // P7c: the check hangs past its own watchdog after the join seated us; the
  // check watchdog is guarded and writes nothing.
  if (want('P7c')) {
    const room = await fetch(BASE + '/room', { method: 'POST' }).then(r => r.json());
    const ctx = await browser.createBrowserContext();
    const pg = await ctx.newPage();
    await pg.goto(PROXY + '/room/' + room.roomId, { waitUntil: 'networkidle0' });
    fs.writeFileSync(FLAGS + '/delayjoin', '17000');
    await pg.click('#join-human');
    await sleep(16000);
    fs.writeFileSync(FLAGS + '/holdstatus', '1'); // the check never answers
    await pg.click('#join-check');
    await sleep(17500); // join seats at 17s; check watchdog would fire at ~31s
    const s = await snap(pg);
    ok('P7c hung check after seating writes nothing', s.inviteHidden === true && /paired/.test(s.pill) && !/Still checking|Still unknown/.test(s.err), 'err=' + JSON.stringify(s.err));
    await ctx.close();
  }

  // P8: out-of-order settlement + timer locality. A (20s) and B (21s, started
  // ~17s) both pending; an EXTERNAL join pairs the room at ~19s. A settles
  // 409 at 20s (stale: must be fully ignored, and must NOT cancel B's
  // watchdog); B's watchdog must still fire at ~32s; B settles 409 at ~38s
  // (current: the full message appears).
  if (want('P8')) {
    const room = await fetch(BASE + '/room', { method: 'POST' }).then(r => r.json());
    const ctx = await browser.createBrowserContext();
    const pg = await ctx.newPage();
    await pg.goto(PROXY + '/room/' + room.roomId, { waitUntil: 'networkidle0' });
    fs.writeFileSync(FLAGS + '/delayjoin', '20000');
    await pg.click('#join-human');
    await sleep(16000); // A's watchdog
    await pg.click('#join-check'); // immediate snapshot, still open
    await sleep(1200);
    fs.writeFileSync(FLAGS + '/delayjoin', '21000');
    await pg.click('#join-human'); // B starts ~17.2s, resolves ~38.2s
    await sleep(2000); // ~19.2s
    await fetch(BASE + '/room/' + room.roomId + '/join', { method: 'POST' }); // external claim ~19.5s
    await sleep(14000); // ~33.2s: A settled (20s, stale 409 ignored); B watchdog fired at ~32.2s
    const mid = await snap(pg);
    ok('P8 newer attempt watchdog survives older settle', /Still waiting/.test(mid.err), mid.err);
    await sleep(7000); // ~40.2s: B settles 409
    const end = await snap(pg);
    ok('P8 newest attempt gets the honest 409', /cannot be reclaimed|lost reply/.test(end.err), end.err);
    const st = await status(room.roomId);
    ok('P8 exactly one (external) claim', st.paired === true);
    await ctx.close();
  }

  // P8b: retry settles BEFORE the original (out-of-order success): B seats
  // the browser at ~19s, the original A settles 409 at ~20s and is ignored.
  if (want('P8b')) {
    const room = await fetch(BASE + '/room', { method: 'POST' }).then(r => r.json());
    const ctx = await browser.createBrowserContext();
    const pg = await ctx.newPage();
    await pg.goto(PROXY + '/room/' + room.roomId, { waitUntil: 'networkidle0' });
    fs.writeFileSync(FLAGS + '/delayjoin', '20000');
    await pg.click('#join-human');
    await sleep(16000);
    await pg.click('#join-check');
    await sleep(1200);
    fs.writeFileSync(FLAGS + '/delayjoin', '3000');
    await pg.click('#join-human'); // B resolves ~20.2s... A resolves 20s
    await sleep(8000);
    const s = await snap(pg);
    const st = await status(room.roomId);
    ok('P8b seated exactly once, late settle ignored', s.inviteHidden === true && /paired/.test(s.pill) && st.paired === true, s.pill);
    await ctx.close();
  }

  // P9: the check itself hangs; its watchdog explains and offers a manual
  // recheck without any automatic mutation; the recheck works.
  if (want('P9')) {
    const room = await fetch(BASE + '/room', { method: 'POST' }).then(r => r.json());
    const ctx = await browser.createBrowserContext();
    const pg = await ctx.newPage();
    await pg.goto(PROXY + '/room/' + room.roomId, { waitUntil: 'networkidle0' });
    fs.writeFileSync(FLAGS + '/blackholejoin', '1'); // join never commits
    await pg.click('#join-human');
    await sleep(16000); // join watchdog
    fs.writeFileSync(FLAGS + '/holdstatus', '1'); // first check never answers
    await pg.click('#join-check');
    await sleep(1000);
    const checking = await snap(pg);
    ok('P9 checking state visible, one check at a time', /Checking/.test(checking.err) && checking.checkDisabled === true, checking.err);
    await sleep(15500); // check watchdog fires
    const hung = await snap(pg);
    ok('P9 hung check gets honest state + recheck offer', /Still checking/.test(hung.err) && hung.checkDisabled === false, hung.err);
    ok('P9 join button untouched by hung check', hung.btnDisabled === true);
    await pg.click('#join-check'); // recheck, goes through
    await sleep(1500);
    const recheck = await snap(pg);
    ok('P9 recheck reads the still-open snapshot', /Still open as of this check/.test(recheck.err), recheck.err);
    const st = await status(room.roomId);
    ok('P9 wire still unpaired (no automatic mutation)', st.paired === false);
    await ctx.close();
  }

  console.log(results.join('\n'));
  console.log(failures ? 'FAILURES: ' + failures : 'ALL PASS');
  await browser.close();
  process.exit(failures ? 1 : 0);
})().catch(e => { console.error('DRIVER ERROR', e); process.exit(2); });
