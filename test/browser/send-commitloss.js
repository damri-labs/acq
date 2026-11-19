// Browser guest send whose response is lost AFTER commit (commitloss flag):
// Chrome transparently retries the dropped POST once, re-sending the same
// body. With the per-draft cid + server resubmission handling, one click
// must commit the message EXACTLY ONCE; a resubmission carrying the same
// cid (the manual retry of an unconfirmed draft) must also not duplicate.
// Run: server on UPSTREAM_PORT, test/browser/proxy.js on PROXY_PORT with
// FLAGS_DIR, then: node test/browser/send-commitloss.js
const puppeteer = require('puppeteer-core');
const fs = require('fs');
const FLAGS = process.env.FLAGS_DIR || '/tmp/acq-ui-flags';
const UP = 'http://127.0.0.1:' + (process.env.UPSTREAM_PORT || '8099');
const PX = 'http://127.0.0.1:' + (process.env.PROXY_PORT || '8100');
const results = [];
let failures = 0;
const ok = (name, cond, extra) => { if (!cond) failures++; results.push((cond?'PASS':'FAIL')+' '+name+(extra?' :: '+extra:'')); };

(async () => {
  const room = await (await fetch(UP + '/room', { method: 'POST' })).json();
  const guest = await (await fetch(UP + '/room/' + room.roomId + '/join', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: '{"as":"human"}' })).json();
  const browser = await puppeteer.launch({ executablePath: process.env.CHROME_PATH || '/usr/bin/google-chrome', args: ['--no-sandbox','--disable-dev-shm-usage'] });
  const page = await browser.newPage();
  await page.setViewport({ width: 1100, height: 900 });
  await page.goto(PX + '/', { waitUntil: 'domcontentloaded' });
  await page.evaluate((cred) => localStorage.setItem('acq_credentials', JSON.stringify([cred])), guest.credential);
  await page.goto(PX + '/room/' + room.roomId, { waitUntil: 'networkidle0' });
  await page.waitForSelector('#draft', { visible: true });

  // One click; the proxy commits upstream then destroys the response.
  fs.writeFileSync(FLAGS + '/commitloss', '1');
  await page.type('#draft', 'commit-loss: this text must appear exactly once');
  await page.click('#post');
  await new Promise(r => setTimeout(r, 2500));
  const s1 = await page.evaluate(() => ({
    err: document.getElementById('send-err').textContent,
    draft: document.getElementById('draft').value,
    n: [...document.querySelectorAll('.msg')].filter(m => m.textContent.includes('exactly once')).length,
  }));
  ok('commitloss: message renders exactly once after one click', s1.n === 1, JSON.stringify(s1));
  ok('commitloss: draft cleared (resubmission outcome is a success)', s1.draft === '', JSON.stringify(s1));
  ok('commitloss: no error shown for the deduped retry', s1.err === '', JSON.stringify(s1));

  // Manual-retry analog: two page-side sends carrying the same cid must
  // produce one commit; the second is marked duplicate in its response.
  const s2 = await page.evaluate(async (uin, tok) => {
    const body = JSON.stringify({ text: 'manual retry, same cid', end: false, cid: 'fixed-manual-retry-cid' });
    const h = { 'Content-Type': 'application/json', 'Authorization': 'Bearer ' + tok };
    const r1 = await fetch('/room/' + uin + '/msg', { method: 'POST', headers: h, body });
    const r2 = await fetch('/room/' + uin + '/msg', { method: 'POST', headers: h, body });
    return { s1: r1.status, s2: r2.status, d2: (await r2.json()).duplicate === true };
  }, room.roomId, guest.token).catch(e => ({ err: String(e) }));
  ok('manual retry with same cid: both accepted, second marked duplicate', s2.s1 === 200 && s2.s2 === 200 && s2.d2 === true, JSON.stringify(s2));

  // Ground truth from the server transcript: exactly 2 commits total.
  const truth = await (await fetch(UP + '/room/' + room.roomId + '/msg?self=1&peek=1&since=0', { headers: { Authorization: 'Bearer ' + room.token } })).json();
  const texts = (truth.messages || []).map(m => m.text);
  ok('server transcript: one commit per logical send (2 total)', texts.length === 2 && texts[0].includes('exactly once') && texts[1].includes('manual retry'), JSON.stringify(texts));
  await browser.close();
  console.log(results.join('\n'));
  if (failures) { console.error(failures + ' assertion(s) FAILED'); process.exitCode = 1; }
})().catch(e => { console.error(e); console.log(results.join('\n')); process.exit(1); });
