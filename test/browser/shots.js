const puppeteer = require('puppeteer-core');
const fs = require('fs');
const BASE = process.env.BASE_URL || 'http://127.0.0.1:8099';
const OUT = process.env.OUT_DIR || '.';
const cookie = fs.readFileSync(process.env.COOKIE_FILE || 'cookie.txt','utf8').trim();
(async () => {
  const browser = await puppeteer.launch({ executablePath: process.env.CHROME_PATH || 'google-chrome', args: ['--no-sandbox','--disable-dev-shm-usage'] });
  // guest
  let page = await browser.newPage();
  await page.setViewport({ width: 1100, height: 1000 });
  await page.goto(BASE + '/', { waitUntil: 'networkidle0' });
  await new Promise(r => setTimeout(r, 600));
  await page.screenshot({ path: OUT + '/landing-guest-full.png', fullPage: true });
  await page.close();
  // signed-in
  page = await browser.newPage();
  await page.setViewport({ width: 1100, height: 1000 });
  await page.setCookie({ name: 'acq_session', value: cookie, domain: '127.0.0.1', path: '/' });
  await page.goto(BASE + '/', { waitUntil: 'networkidle0' });
  await new Promise(r => setTimeout(r, 900));
  const info = await page.evaluate(() => ({ acct: !document.getElementById('account').hidden, rooms: document.querySelectorAll('#acct-rooms li').length }));
  console.log('signed-in state:', JSON.stringify(info));
  await page.screenshot({ path: OUT + '/landing-signedin-full.png', fullPage: true });
  await page.close();
  await browser.close();
  console.log('shots done');
})().catch(e => { console.error(e); process.exit(1); });
