// guest-join.js - human-guest suite (26 assertions): an invited human with
// no account claims seat B from the invite link itself. Covers the invite
// panel copy and button, the in-browser join (credential stored in
// localStorage in the Saved-rooms shape, never in the URL), the seat-B UI
// with composer, the "B (human, self-declared)" label in the UI, the
// server-recorded seatBKind on every read path, wire From staying "B",
// seat semantics (a second join is 409, a fresh visitor still sees the
// agent invite card), and reload re-entry through the saved credential.
// Seeding: none - the suite creates its own guest room. Run against a
// server started with the README env (PORT=8099 ...).
const puppeteer = require('puppeteer-core');
const BASE = process.env.BASE_URL || 'http://127.0.0.1:8099';
const CHROME = process.env.CHROME_PATH || 'google-chrome';
const OUT = process.env.OUT_DIR || '.';

const results = [];
let failures = 0;
const ok = (name, cond, extra) => { if (!cond) failures++; results.push((cond?'PASS':'FAIL')+' '+name+(extra?' | '+extra:'')); };

function unpack(blob){
  if (!blob || !blob.startsWith('acq1_')) return null;
  try {
    const b64 = blob.slice(5).replace(/-/g,'+').replace(/_/g,'/');
    const c = JSON.parse(Buffer.from(b64, 'base64').toString('utf8'));
    return (c.uin && c.token) ? c : null;
  } catch(e){ return null; }
}

(async()=>{
  const c = await fetch(BASE + '/room', { method:'POST', headers:{'Content-Type':'application/json'}, body: JSON.stringify({ topic:'guest suite' }) }).then(r=>r.json());
  ok('create room', !!c.roomId, c.roomId);
  if (!c.roomId) { console.log(results.join('\n')); process.exit(1); }
  const uin = c.roomId, tokA = c.token;

  const browser = await puppeteer.launch({ headless:'new', executablePath: CHROME, args:['--no-sandbox','--disable-dev-shm-usage'] });
  const page = await browser.newPage();
  await page.setViewport({ width:1200, height:900 });
  await page.goto(BASE + '/room/' + uin, { waitUntil:'networkidle0' });
  await new Promise(r=>setTimeout(r,1200));

  // 1-2. Invite panel: agent command AND the human option, copy no longer
  // assumes two AI agents.
  const invite = await page.evaluate(()=>({
    visible: !document.getElementById('invite').hidden,
    btn: !document.getElementById('join-human').hidden,
    text: document.getElementById('invite').textContent,
    composer: !document.getElementById('composer').hidden,
  }));
  ok('invite panel visible with guest button', invite.visible && invite.btn);
  ok('copy covers person+agent', /person and an agent/.test(invite.text) && !/two AI agents/.test(invite.text), invite.text.slice(0,120));
  ok('no composer for invite visitor', !invite.composer);
  await page.screenshot({ path: OUT + '/room-invite-guest.png', fullPage: true });

  // 3-5. Click: the browser itself joins, credential lands in localStorage
  // in the Saved-rooms shape (never the URL), seat-B UI opens.
  await page.click('#join-human');
  await new Promise(r=>setTimeout(r,1500));
  const after = await page.evaluate(()=>{
    const items = JSON.parse(localStorage.getItem('acq_credentials') || '[]');
    return {
      composer: !document.getElementById('composer').hidden,
      inviteHidden: document.getElementById('invite').hidden,
      who: document.getElementById('who').textContent,
      creds: items,
      url: location.href,
    };
  });
  ok('guest enters room UI with composer', after.composer && after.inviteHidden);
  const stored = after.creds.map(unpack).find(x => x && x.uin.toUpperCase() === uin.toUpperCase());
  ok('credential stored in localStorage (seat B, this room)', !!stored && stored.seat === 'B', JSON.stringify(after.creds).slice(0,80));
  ok('no token in URL', !/acq1_|token/i.test(after.url), after.url);
  ok('who label marks the human guest', /human guest/.test(after.who), after.who);

  // 6. Server recorded the self-declared kind on every read path.
  const tr = await fetch(BASE + '/room/' + uin + '/msg?view=1&wait=0', { headers:{ Authorization:'Bearer '+stored.token } }).then(r=>r.json());
  ok('transcript carries seatBKind human', tr.seatBKind === 'human', JSON.stringify(tr.seatBKind));
  const ra = await fetch(BASE + '/room/' + uin + '/msg?wait=0', { headers:{ Authorization:'Bearer '+tokA } }).then(r=>r.json());
  ok('seat A recv carries seatBKind human', ra.seatBKind === 'human');

  // 7-8. The guest speaks: UI label is the self-declared human label, the
  // wire From stays "B".
  await page.type('#draft', 'hello, I am a person at the keyboard');
  await page.click('#post');
  await new Promise(r=>setTimeout(r,2000));
  const rendered = await page.evaluate(()=>[...document.querySelectorAll('.msg')].map(e=>e.getAttribute('data-seat')));
  ok('UI renders B (human, self-declared)', rendered.some(s=>s==='B (human, self-declared)'), JSON.stringify(rendered));
  const ra2 = await fetch(BASE + '/room/' + uin + '/msg?wait=0', { headers:{ Authorization:'Bearer '+tokA } }).then(r=>r.json());
  const wire = (ra2.messages||[]).map(m=>m.from + ':' + m.text);
  ok('wire From stays B', (ra2.messages||[]).some(m=>m.from==='B' && /person at the keyboard/.test(m.text)), JSON.stringify(wire));
  await page.screenshot({ path: OUT + '/room-guest-human.png', fullPage: true });

  // 9. Seat semantics: a second join - agent or human - is a 409.
  const again = await fetch(BASE + '/room/' + uin + '/join', { method:'POST' });
  ok('second join is 409', again.status === 409, String(again.status));

  // 10. A fresh visitor (no credential) still gets the agent invite card -
  // the guest claim closed no doors and opened none.
  const ctx2 = await browser.createBrowserContext();
  const page2 = await ctx2.newPage();
  await page2.goto(BASE + '/room/' + uin, { waitUntil:'networkidle0' });
  await new Promise(r=>setTimeout(r,1200));
  const fresh = await page2.evaluate(()=>({
    invite: !document.getElementById('invite').hidden,
    cmd: document.getElementById('join-cmd').textContent,
    pill: document.getElementById('pill').textContent,
    composer: !document.getElementById('composer').hidden,
  }));
  ok('fresh visitor sees agent invite card, paired status', fresh.invite && /curl -s -X POST/.test(fresh.cmd) && /paired/.test(fresh.pill) && !fresh.composer, fresh.pill);

  // Room 2: storage blocked at claim time. The join already happened
  // server-side, so the seat must stay usable from memory, the persistence
  // failure must be loud, and explicit recovery actions must work - never a
  // silently lost seat (Atra round-4 P2).
  const c2 = await fetch(BASE + '/room', { method:'POST', headers:{'Content-Type':'application/json'}, body: JSON.stringify({ topic:'guest suite blocked storage' }) }).then(r=>r.json());
  ok('create room 2', !!c2.roomId, c2.roomId);
  const ctx3 = await browser.createBrowserContext();
  const pg = await ctx3.newPage();
  await pg.setViewport({ width:1200, height:900 });
  await pg.evaluateOnNewDocument(() => {
    window.__copied = null;
    Object.defineProperty(navigator.clipboard, 'writeText', { value: t => { window.__copied = t; return Promise.resolve(); } });
    if (!Storage.prototype.__origSet) Storage.prototype.__origSet = Storage.prototype.setItem;
  });
  await pg.goto(BASE + '/room/' + c2.roomId, { waitUntil:'networkidle0' });
  await new Promise(r=>setTimeout(r,1000));
  await pg.evaluate(() => { Storage.prototype.setItem = function(){ throw new Error('blocked'); }; });
  await pg.click('#join-human');
  await new Promise(r=>setTimeout(r,1500));
  const blocked = await pg.evaluate(() => ({
    composer: !document.getElementById('composer').hidden,
    inviteHidden: document.getElementById('invite').hidden,
    warn: !document.getElementById('storefail').hidden,
    stored: localStorage.getItem('acq_credentials'),
  }));
  ok('blocked storage: session still enters from memory', blocked.composer && blocked.inviteHidden);
  ok('blocked storage: loud persistence warning', blocked.warn);
  ok('blocked storage: nothing silently stored', blocked.stored === null, String(blocked.stored));

  // The seat still works in-session.
  await pg.type('#draft', 'still here despite blocked storage');
  await pg.click('#post');
  await new Promise(r=>setTimeout(r,1800));
  const sentBlocked = await pg.evaluate(() => [...document.querySelectorAll('.msg')].map(e=>e.getAttribute('data-seat')));
  ok('blocked storage: in-session send works', sentBlocked.some(s=>s && s.indexOf('B') === 0), JSON.stringify(sentBlocked));
  await pg.screenshot({ path: OUT + '/room-guest-storefail.png', fullPage: true });

  // Explicit recovery: copy the credential (never in the URL).
  await pg.click('#copy-recovery');
  await new Promise(r=>setTimeout(r,600));
  const copied = await pg.evaluate(() => window.__copied);
  const copiedCred = unpack(copied);
  ok('recovery copy carries the packed credential', !!copiedCred && copiedCred.uin.toUpperCase() === c2.roomId.toUpperCase() && copiedCred.seat === 'B', (copied||'').slice(0,30));
  ok('recovery never touches the URL', !/acq1_/.test(pg.url()), pg.url());

  // Storage heals; retry-save persists and clears the warning.
  await pg.evaluate(() => { Storage.prototype.setItem = Storage.prototype.__origSet; });
  await pg.click('#save-retry');
  await new Promise(r=>setTimeout(r,600));
  const healed = await pg.evaluate(() => ({
    warn: !document.getElementById('storefail').hidden,
    items: JSON.parse(localStorage.getItem('acq_credentials') || '[]'),
  }));
  ok('retry save persists and clears warning', !healed.warn && healed.items.map(unpack).some(x => x && x.uin.toUpperCase() === c2.roomId.toUpperCase()), String(healed.items.length));

  // Reload after successful storage re-enters the seat UI with the label.
  await pg.goto(BASE + '/room/' + c2.roomId, { waitUntil:'networkidle0' });
  await new Promise(r=>setTimeout(r,1500));
  const back = await pg.evaluate(() => ({
    invite: !document.getElementById('invite').hidden,
    composer: !document.getElementById('composer').hidden,
    warn: !document.getElementById('storefail').hidden,
    seats: [...document.querySelectorAll('.msg')].map(e=>e.getAttribute('data-seat')),
  }));
  ok('reload after healed storage re-enters seat UI', !back.invite && back.composer && !back.warn);
  ok('human label intact after healed reload', back.seats.some(s=>s==='B (human, self-declared)'), JSON.stringify(back.seats));

  // 11-12. Reload in the guest browser: the saved credential re-enters the
  // seat UI directly (no invite card), the human label still renders.
  await page.goto(BASE + '/room/' + uin, { waitUntil:'networkidle0' });
  await new Promise(r=>setTimeout(r,1500));
  const reload = await page.evaluate(()=>({
    invite: !document.getElementById('invite').hidden,
    composer: !document.getElementById('composer').hidden,
    who: document.getElementById('who').textContent,
    seats: [...document.querySelectorAll('.msg')].map(e=>e.getAttribute('data-seat')),
  }));
  ok('reload re-enters seat UI via saved credential', !reload.invite && reload.composer);
  ok('human label survives reload', reload.seats.some(s=>s==='B (human, self-declared)') && /human guest/.test(reload.who), JSON.stringify(reload.seats));

  await browser.close();
  console.log(results.join('\n'));
  process.exit(failures ? 1 : 0);
})().catch(e=>{ console.error('HARNESS ERROR', e); console.log(results.join('\n')); process.exit(1); });
