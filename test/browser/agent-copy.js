const puppeteer = require('puppeteer-core');
const fs = require('fs');
const env = Object.fromEntries(fs.readFileSync(process.env.SEAT_ENV || 'env-agentcopy.txt','utf8').trim().split('\n').map(l=>l.split(/=(.*)/).slice(0,2)));
const ownU = JSON.parse(fs.readFileSync(process.env.OWN_ROOM || 'ownroom.json','utf8'));
const ROOM4_FILE = process.env.ROOM4_FILE || 'room4.json';
const cookie = fs.readFileSync(process.env.COOKIE_FILE || 'cookie.txt','utf8').trim();
const base = process.env.BASE_URL || 'http://127.0.0.1:8099';
const credAObj = JSON.parse(Buffer.from(env.CREDA.slice(5).replace(/-/g,'+').replace(/_/g,'/'), 'base64').toString());
const results = [];
let failures = 0;
const ok = (name, cond, extra) => { if (!cond) failures++; results.push((cond?'PASS':'FAIL')+' '+name+(extra?' :: '+extra:'')); };
const sleep = ms => new Promise(r => setTimeout(r, ms));

(async () => {
  const browser = await puppeteer.launch({ executablePath: process.env.CHROME_PATH || 'google-chrome', args: ['--no-sandbox','--disable-dev-shm-usage'] });

  async function newPage() {
    const page = await browser.newPage();
    await page.setViewport({ width: 1100, height: 900 });
    await page.evaluateOnNewDocument(() => {
      window.__copied = null;
      Object.defineProperty(navigator.clipboard, 'writeText', { value: t => { window.__copied = t; return Promise.resolve(); } });
    });
    return page;
  }

  // Case 1: pre-upgrade room, old credential in localStorage (seat A)
  let page = await newPage();
  await page.goto(base + '/', { waitUntil: 'domcontentloaded' });
  await page.evaluate(cred => localStorage.setItem('acq_credentials', JSON.stringify([cred])), env.CREDA);
  await page.goto(base + '/room/' + env.U, { waitUntil: 'networkidle0' });
  await sleep(1200);
  let st = await page.evaluate(() => ({
    agentBtn: !document.getElementById('copy-agent').hidden,
    hint: !document.getElementById('metahint').hidden,
    linkBtn: !document.getElementById('copy-link').hidden,
    who: document.getElementById('who').textContent,
    msgs: Array.from(document.querySelectorAll('.msg .text')).map(x => x.textContent),
  }));
  ok('pre-upgrade room: transcript renders after upgrade', st.msgs.includes('before upgrade from A') && st.msgs.includes('after upgrade from A'), st.msgs.join('|'));
  ok('seat A: agent button + hint visible', st.agentBtn && st.hint, st.who);
  await page.click('#copy-agent');
  await sleep(300);
  let copied = await page.evaluate(() => window.__copied);
  ok('seat A copy: explains existing seat without join', copied && copied.includes('seat A') && copied.includes('Do NOT run the join command'), (copied||'').split('\n')[1]);
  ok('seat A copy: no join command present', copied && !copied.includes('/join'), '');
  ok('seat A copy: carries room ID + seat token', copied && copied.includes(env.U) && copied.includes(env.TA), '');
  ok('seat A copy: EXCLUDES recovery blob and pair code', copied && !copied.includes(env.CREDA) && !copied.includes('acq1_') && !copied.includes(credAObj.pairCode), '');

  // Copy invite link must carry no secret material at all
  await page.click('#copy-link');
  await sleep(300);
  const inviteCopied = await page.evaluate(() => window.__copied);
  ok('invite link: no token, recovery code, or credential blob', inviteCopied && inviteCopied.includes(env.U) && !inviteCopied.includes(env.TA) && !inviteCopied.includes('acq1_') && !inviteCopied.includes(credAObj.pairCode) && !inviteCopied.includes(env.TB), inviteCopied || 'null');
  await page.screenshot({ path: (process.env.OUT_DIR || '.') + '/room-seatA.png', fullPage: true });

  // seat A composer still works post-upgrade from the page
  await page.type('#draft', 'from the page after upgrade');
  await page.click('#post');
  await sleep(1200);
  st = await page.evaluate(() => Array.from(document.querySelectorAll('.msg .text')).map(x => x.textContent));
  ok('seat A: send from page works post-upgrade', st.includes('from the page after upgrade'), '');

  // Case 2: invite visitor (no credential, not owner) - button hidden.
  // Isolated context: the main context's localStorage holds the seat-A credential.
  const ctx2 = browser.createBrowserContext ? await browser.createBrowserContext() : await browser.createIncognitoBrowserContext();
  page = await ctx2.newPage();
  await page.setViewport({ width: 1100, height: 900 });
  await page.evaluateOnNewDocument(() => {
    window.__copied = null;
    Object.defineProperty(navigator.clipboard, 'writeText', { value: t => { window.__copied = t; return Promise.resolve(); } });
  });
  await page.goto(base + '/room/' + env.U, { waitUntil: 'networkidle0' });
  await sleep(1000);
  st = await page.evaluate(() => ({
    agentBtn: !document.getElementById('copy-agent').hidden,
    hint: !document.getElementById('metahint').hidden,
    invite: !document.getElementById('invite').hidden,
    joinCmd: document.getElementById('join-cmd').textContent,
  }));
  ok('invite visitor: agent button + hint hidden', !st.agentBtn && !st.hint, '');
  ok('invite visitor: join flow intact', st.invite && st.joinCmd.includes('/join'), st.joinCmd);
  await page.screenshot({ path: (process.env.OUT_DIR || '.') + '/room-invite.png', fullPage: true });
  await page.close();

  // Case 3: owner via session cookie (no localStorage credential)
  page = await newPage();
  await page.setCookie({ name: 'acq_session', value: cookie, domain: '127.0.0.1', path: '/' });
  await page.goto(base + '/room/' + ownU.roomId, { waitUntil: 'networkidle0' });
  await sleep(1200);
  st = await page.evaluate(() => ({
    agentBtn: !document.getElementById('copy-agent').hidden,
    ownerHint: !document.getElementById('ownerhint').hidden,
    hintText: document.getElementById('ownerhint').textContent,
    who: document.getElementById('who').textContent,
  }));
  ok('owner without saved credential: button hidden', !st.agentBtn, st.who);
  ok('owner without saved credential: guidance shown', st.ownerHint && st.hintText.includes('browser that created it'), st.hintText.slice(0, 60));
  await page.screenshot({ path: (process.env.OUT_DIR || '.') + '/room-owner.png', fullPage: true });
  await page.close();

  // Case 4: seat-B credential holder - no agent button, no owner hint
  page = await newPage();
  await page.goto(base + '/', { waitUntil: 'domcontentloaded' });
  const room4 = JSON.parse(fs.readFileSync(ROOM4_FILE,'utf8'));
  await page.evaluate(cred => localStorage.setItem('acq_credentials', JSON.stringify([cred])), room4.credentialB);
  await page.goto(base + '/room/' + room4.roomId, { waitUntil: 'networkidle0' });
  await sleep(1000);
  st = await page.evaluate(() => ({
    agentBtn: !document.getElementById('copy-agent').hidden,
    ownerHint: !document.getElementById('ownerhint').hidden,
    who: document.getElementById('who').textContent,
  }));
  ok('seat B: agent button + owner hint hidden', !st.agentBtn && !st.ownerHint, st.who);
  await page.close();

  await browser.close();
  console.log(results.join('\n'));
  if (failures > 0) { console.error(failures + ' assertion(s) FAILED'); process.exitCode = 1; }
})().catch(e => { console.error(e); console.log(results.join('\n')); process.exit(1); });
