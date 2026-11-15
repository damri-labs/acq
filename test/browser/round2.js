const puppeteer = require('puppeteer-core');
const fs = require('fs');
const FLAGS = process.env.FLAGS_DIR || '/tmp/acq-ui-flags';
fs.mkdirSync(FLAGS, { recursive: true });
const env = Object.fromEntries(fs.readFileSync(process.env.ENV_FILE || 'env3.txt','utf8').trim().split('\n').map(l=>l.split(/=(.*)/).slice(0,2)));
const base = 'http://127.0.0.1:8100';
const results = [];
let failures = 0;
const ok = (name, cond, extra) => { if (!cond) failures++; results.push((cond?'PASS':'FAIL')+' '+name+(extra?' :: '+extra:'')); };
const sleep = ms => new Promise(r => setTimeout(r, ms));

(async () => {
  const browser = await puppeteer.launch({ executablePath: process.env.CHROME_PATH || 'google-chrome', args: ['--no-sandbox','--disable-dev-shm-usage'] });
  const page = await browser.newPage();
  await page.setViewport({ width: 1100, height: 900 });
  await page.goto(base + '/', { waitUntil: 'domcontentloaded' });
  await page.evaluate((cred) => localStorage.setItem('acq_credentials', JSON.stringify([cred])), env.CRED3);
  await page.goto(base + '/room/' + env.U3, { waitUntil: 'networkidle0' });
  await sleep(800);

  // Test 1: delayed POST + typing attempt during pending
  fs.writeFileSync(FLAGS + '/delaypost', '1');
  await page.click('#draft');
  await page.type('#draft', 'first message');
  await page.click('#post');
  await sleep(600); // mid-flight (proxy holds the POST for 2.5s)
  const mid = await page.evaluate(() => ({ dd: document.getElementById('draft').disabled, bd: document.getElementById('post').disabled, v: document.getElementById('draft').value }));
  ok('pending: draft+button disabled', mid.dd && mid.bd, JSON.stringify(mid));
  // typing attempt during pending must be a no-op (field disabled)
  await page.keyboard.type('SNEAKED-TEXT');
  await sleep(300);
  const mid2 = await page.evaluate(() => document.getElementById('draft').value);
  ok('pending: typing blocked, text unchanged', mid2 === 'first message', mid2);
  await sleep(2600); // let the delayed POST land
  const after = await page.evaluate(() => ({ v: document.getElementById('draft').value, dd: document.getElementById('draft').disabled, n: document.querySelectorAll('.msg').length, last: document.querySelector('.msg:last-child .text') ? document.querySelector('.msg:last-child .text').textContent : '' }));
  ok('success: cleared, re-enabled, message rendered', after.v === '' && !after.dd && after.last === 'first message', JSON.stringify(after));

  // Test 2: true network failure (blackholed: first attempt + Chrome's
  // transparent retry both die) -> honest copy + text kept + re-enabled
  fs.writeFileSync(FLAGS + '/blackhole', '1');
  await page.click('#draft');
  await page.type('#draft', 'second message');
  await page.click('#post');
  await sleep(3000);
  const fail = await page.evaluate(() => ({ v: document.getElementById('draft').value, e: document.getElementById('send-err').textContent, dd: document.getElementById('draft').disabled, bd: document.getElementById('post').disabled }));
  ok('network failure: text kept', fail.v === 'second message', JSON.stringify(fail));
  ok('network failure: honest copy', fail.e === 'Delivery not confirmed - your text is kept.', fail.e);
  ok('network failure: controls re-enabled', !fail.dd && !fail.bd, '');
  // no auto-retry: after connectivity returns, 'second message' must not appear
  fs.unlinkSync(FLAGS + '/blackhole');
  await sleep(6000);
  const final = await page.evaluate(() => ({ n: document.querySelectorAll('.msg').length, texts: Array.from(document.querySelectorAll('.msg .text')).map(x => x.textContent) }));
  ok('no auto-retry: second message not sent', !final.texts.includes('second message'), final.texts.join('|'));

  await browser.close();
  console.log(results.join('\n'));
  if (failures > 0) { console.error(failures + ' assertion(s) FAILED'); process.exitCode = 1; }
})().catch(e => { console.error(e); console.log(results.join('\n')); process.exit(1); });
