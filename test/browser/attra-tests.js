const puppeteer = require('puppeteer-core');
const fs = require('fs');
const FLAGS = process.env.FLAGS_DIR || '/tmp/acq-ui-flags';
fs.mkdirSync(FLAGS, { recursive: true });
const env = Object.fromEntries(fs.readFileSync(process.env.ENV_FILE || 'env.txt','utf8').trim().split('\n').map(l=>l.split(/=(.*)/).slice(0,2)));
const base = 'http://127.0.0.1:8100';
const realBase = 'http://127.0.0.1:8099';
const results = [];
let failures = 0;
const ok = (name, cond, extra) => { if (!cond) failures++; results.push((cond?'PASS':'FAIL')+' '+name+(extra?' :: '+extra:'')); };

(async () => {
  const browser = await puppeteer.launch({ executablePath: process.env.CHROME_PATH || 'google-chrome', args: ['--no-sandbox','--disable-dev-shm-usage'] });
  const page = await browser.newPage();
  await page.setViewport({ width: 1100, height: 900 });
  // seed seat-A credential for the test room
  await page.goto(base + '/', { waitUntil: 'domcontentloaded' });
  await page.evaluate((cred) => localStorage.setItem('acq_credentials', JSON.stringify([cred])), env.CRED2);

  // --- Finding 3: no duplicate appends under poll storms ---
  await page.goto(base + '/room/' + env.U2, { waitUntil: 'networkidle0' });
  await new Promise(r => setTimeout(r, 1200));
  const countBefore = await page.evaluate(() => document.querySelectorAll('.msg').length);
  await page.evaluate(() => { for (let i = 0; i < 6; i++) window.__acq.pollNow(); });
  await new Promise(r => setTimeout(r, 2500));
  const after = await page.evaluate(() => ({ n: document.querySelectorAll('.msg').length, st: window.__acq.state() }));
  ok('dedup/single-flight: 3 seeded msgs render once', countBefore === 3 && after.n === 3, 'before='+countBefore+' after='+after.n+' lastSeq='+after.st.lastSeq);

  // --- Finding 2: transient 502 recovers ---
  fs.writeFileSync(FLAGS + '/failget', '1');
  await page.evaluate(() => window.__acq.pollNow());
  await new Promise(r => setTimeout(r, 700));
  const recon = await page.evaluate(() => document.getElementById('pill').textContent);
  // now post a new message from seat B and let polling recover naturally
  await fetch(realBase + '/room/' + env.U2 + '/msg', { method: 'POST', headers: { Authorization: 'Bearer ' + env.T2B, 'Content-Type': 'application/json' }, body: JSON.stringify({ text: 'msg four after 502', end: false }) });
  await new Promise(r => setTimeout(r, 7000));
  const recov = await page.evaluate(() => ({ pill: document.getElementById('pill').textContent, n: document.querySelectorAll('.msg').length, last: document.querySelector('.msg:last-child .text') ? document.querySelector('.msg:last-child .text').textContent : '' }));
  ok('transient 502: reconnecting shown', recon === 'reconnecting...', 'pill was: '+recon);
  ok('transient 502: poll recovered, new msg rendered', recov.n === 4 && recov.last === 'msg four after 502', JSON.stringify(recov));

  // --- Finding 1a: failed send keeps draft + error shown ---
  fs.writeFileSync(FLAGS + '/failpost', '1');
  await page.click('#draft');
  await page.type('#draft', 'draft that must survive');
  await page.click('#post');
  await new Promise(r => setTimeout(r, 800));
  const d1 = await page.evaluate(() => ({ v: document.getElementById('draft').value, e: document.getElementById('send-err').textContent }));
  ok('failed send: draft kept', d1.v === 'draft that must survive', JSON.stringify(d1));
  ok('failed send: error shown', d1.e.length > 0, d1.e);
  // recovery: send again, should succeed this time
  await page.click('#post');
  await new Promise(r => setTimeout(r, 1000));
  const d2 = await page.evaluate(() => ({ v: document.getElementById('draft').value, n: document.querySelectorAll('.msg').length }));
  ok('retry after failure sends', d2.v === '' && d2.n === 5, JSON.stringify(d2));

  // --- Finding 1b: ended room disables composer ---
  await fetch(realBase + '/room/' + env.U2 + '/msg', { method: 'POST', headers: { Authorization: 'Bearer ' + env.T2B, 'Content-Type': 'application/json' }, body: JSON.stringify({ text: 'closing this room', end: true }) });
  await new Promise(r => setTimeout(r, 4000));
  const endState = await page.evaluate(() => ({ pill: document.getElementById('pill').textContent, dis: document.getElementById('draft').disabled && document.getElementById('post').disabled, ph: document.getElementById('draft').placeholder }));
  ok('ended: pill ENDED', endState.pill === 'ended', endState.pill);
  ok('ended: composer disabled', endState.dis, endState.ph);
  await page.screenshot({ path: 'ended-state.png' });

  await browser.close();
  console.log(results.join('\n'));
  if (failures > 0) { console.error(failures + ' assertion(s) FAILED'); process.exitCode = 1; }
})().catch(e => { console.error(e); console.log(results.join('\n')); process.exit(1); });
