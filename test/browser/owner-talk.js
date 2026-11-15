// owner-talk.js - human-voice suite (20 assertions): the signed-in room owner
// posts into their own room from the web UI as "H". Covers composer
// visibility for the owner, H rendering, server-side storage, delivery to a
// joined seat, that anonymous invite visitors see no composer, and that a
// transient owner-probe failure never falls back to posting as seat A.
// Seeding: none needed - the suite mints its own session cookie (key
// sha256("acq-session-v1\0" + ACQ_SESSION_SECRET, default test-secret) and
// creates its own owner room. Run against a server started with the README
// env (PORT=8099 WORKOS_CLIENT_ID=x WORKOS_API_KEY=y ACQ_SESSION_SECRET=test-secret).
const puppeteer = require('puppeteer-core');
const crypto = require('crypto');
const fs = require('fs');
const FLAGS = process.env.FLAGS_DIR || '/tmp/acq-ui-flags';
const BASE = process.env.BASE_URL || 'http://127.0.0.1:8099';
const SECRET = process.env.ACQ_SESSION_SECRET || 'test-secret';
const CHROME = process.env.CHROME_PATH || 'google-chrome';

function mintCookie(){
  const key = crypto.createHash('sha256').update('acq-session-v1\x00' + SECRET).digest();
  const u = { sub:'user_01ABC', email:'tal@example.com', exp: Math.floor(Date.now()/1000)+3600 };
  const p = Buffer.from(JSON.stringify(u)).toString('base64url');
  const sig = crypto.createHmac('sha256', key).update(p).digest('base64url');
  return p + '.' + sig;
}
const results = [];
let failures = 0;
const ok = (name, cond, extra) => { if (!cond) failures++; results.push((cond?'PASS':'FAIL')+' '+name+(extra?' | '+extra:'')); };

(async()=>{
  const cookie = mintCookie();
  const res = await fetch(BASE + '/room', { method:'POST', headers:{ Cookie: 'acq_session=' + cookie } });
  const c = await res.json();
  ok('create owner room', res.status===201 && !!c.roomId, c.roomId);
  if (!c.roomId) { console.log(results.join('\n')); console.error('fixture creation failed - bailing before the browser polls a bad UIN'); process.exit(1); }
  const uin = c.roomId;

  const browser = await puppeteer.launch({ headless:'new', executablePath: CHROME, args:['--no-sandbox','--disable-dev-shm-usage'] });
  const page = await browser.newPage();
  await page.setViewport({ width:1200, height:900 });
  await page.setCookie({ name:'acq_session', value:cookie, domain:'127.0.0.1', path:'/' });
  await page.goto(BASE + '/room/' + uin, { waitUntil:'networkidle0' });
  await new Promise(r=>setTimeout(r,1500));

  const composerVisible = await page.evaluate(()=>{ const el=document.getElementById('composer'); return el && !el.hidden; });
  ok('owner sees composer', composerVisible);
  const who = await page.evaluate(()=>document.getElementById('who').textContent);
  ok('who label says H', /post as H/.test(who), who);

  await page.type('#draft', 'hello from the human');
  await page.click('#post');
  await new Promise(r=>setTimeout(r,2000));
  const rendered = await page.evaluate(()=>[...document.querySelectorAll('.msg')].map(e=>({seat:e.getAttribute('data-seat'), text:e.textContent})));
  ok('message renders in UI', rendered.some(m=>m.seat==='H' && m.text.includes('hello from the human')), JSON.stringify(rendered));
  await page.screenshot({ path:(process.env.OUT_DIR || '.') + '/room-owner-talk.png', fullPage:true });

  const owner = await fetch(BASE + '/room/' + uin + '/msg?since=0&wait=0', { headers:{ Cookie:'acq_session='+cookie } }).then(r=>r.json());
  ok('server stores From H', (owner.messages||[]).some(m=>m.from==='H'), JSON.stringify(owner.messages));

  const join = await fetch(BASE + '/room/' + uin + '/join', { method:'POST' }).then(r=>r.json());
  ok('seat B joins', !!join.token);
  const recvB = await fetch(BASE + '/room/' + uin + '/msg?wait=0', { headers:{ Authorization:'Bearer '+join.token } }).then(r=>r.json());
  ok('seat B receives H', (recvB.messages||[]).some(m=>m.from==='H'), JSON.stringify((recvB.messages||[]).map(m=>m.from)));

  // Creating-browser case: owner cookie AND the saved seat-A credential in
  // localStorage. The human must still speak as H (never under the agent's
  // seat), the message must reach BOTH seats' recv queues, and Copy agent
  // connection stays available from the saved credential.
  const ctx3 = await browser.createBrowserContext();
  const page3 = await ctx3.newPage();
  await page3.goto(BASE + '/', { waitUntil:'domcontentloaded' });
  const joinA = await fetch(BASE + '/room', { method:'POST', headers:{ Cookie:'acq_session='+cookie } }).then(r=>r.json());
  const uin3 = joinA.roomId, credA3 = joinA.credential, tokA3 = joinA.token;
  await page3.evaluate(cr => localStorage.setItem('acq_credentials', JSON.stringify([cr])), credA3);
  await page3.setCookie({ name:'acq_session', value:cookie, domain:'127.0.0.1', path:'/' });
  await page3.goto(BASE + '/room/' + uin3, { waitUntil:'networkidle0' });
  await new Promise(r=>setTimeout(r,1500));
  const st3 = await page3.evaluate(()=>({
    who: document.getElementById('who').textContent,
    agentBtn: !document.getElementById('copy-agent').hidden,
    composer: !document.getElementById('composer').hidden,
  }));
  ok('owner+savedA: owner (H) view, not seat A', /post as H/.test(st3.who), st3.who);
  ok('owner+savedA: Copy agent connection retained', st3.agentBtn);
  ok('owner+savedA: composer shown', st3.composer);
  await page3.type('#draft', 'human on the creating browser');
  await page3.click('#post');
  await new Promise(r=>setTimeout(r,2000));
  const srv3 = await fetch(BASE + '/room/' + uin3 + '/msg?since=0&wait=0', { headers:{ Cookie:'acq_session='+cookie } }).then(r=>r.json());
  ok('owner+savedA: stored From H (not A)', (srv3.messages||[]).length===1 && srv3.messages[0].from==='H', JSON.stringify(srv3.messages));
  const recvA3 = await fetch(BASE + '/room/' + uin3 + '/msg?wait=0', { headers:{ Authorization:'Bearer '+tokA3 } }).then(r=>r.json());
  ok('owner+savedA: seat A receives H', (recvA3.messages||[]).some(m=>m.from==='H'), JSON.stringify((recvA3.messages||[]).map(m=>m.from)));
  const joinB3 = await fetch(BASE + '/room/' + uin3 + '/join', { method:'POST' }).then(r=>r.json());
  const recvB3 = await fetch(BASE + '/room/' + uin3 + '/msg?wait=0', { headers:{ Authorization:'Bearer '+joinB3.token } }).then(r=>r.json());
  ok('owner+savedA: seat B receives H', (recvB3.messages||[]).some(m=>m.from==='H'), JSON.stringify((recvB3.messages||[]).map(m=>m.from)));
  await page3.screenshot({ path:(process.env.OUT_DIR || '.') + '/room-owner-savedA.png', fullPage:true });

  // Failed owner probe: the creating browser (owner cookie + saved seat-A
  // credential) hits a transient 502 on the probe. Identity must stay
  // unresolved - no silent fall back to posting as seat A, no bearer POST -
  // until a retry gets a definitive answer, then the owner recovers to H.
  const ctx4 = await browser.createBrowserContext();
  const page4 = await ctx4.newPage();
  await page4.goto(BASE + '/', { waitUntil:'domcontentloaded' });
  const joinA4 = await fetch(BASE + '/room', { method:'POST', headers:{ Cookie:'acq_session='+cookie } }).then(r=>r.json());
  const uin4 = joinA4.roomId, credA4 = joinA4.credential, tokA4 = joinA4.token;
  await page4.evaluate(cr => localStorage.setItem('acq_credentials', JSON.stringify([cr])), credA4);
  await page4.setCookie({ name:'acq_session', value:cookie, domain:'127.0.0.1', path:'/' });
  fs.writeFileSync(FLAGS + '/failget', '1'); // one-shot 502 for the next GET /msg: the probe
  await page4.goto(BASE + '/room/' + uin4, { waitUntil:'networkidle0' });
  await new Promise(r=>setTimeout(r,1500));
  const st4 = await page4.evaluate(()=>({
    idfail: !document.getElementById('idfail').hidden,
    composer: !document.getElementById('composer').hidden,
    who: document.getElementById('who').textContent,
    agentBtn: !document.getElementById('copy-agent').hidden,
  }));
  ok('failed probe: identity-check-failed UI shown', st4.idfail);
  ok('failed probe: no seat-A view, composer locked', !st4.composer && st4.who==='' && !st4.agentBtn, JSON.stringify(st4));
  const srv4a = await fetch(BASE + '/room/' + uin4 + '/msg?since=0&wait=0', { headers:{ Cookie:'acq_session='+cookie } }).then(r=>r.json());
  ok('failed probe: no bearer POST reached the room', (srv4a.messages||[]).length===0, JSON.stringify(srv4a.messages));
  await page4.screenshot({ path:(process.env.OUT_DIR || '.') + '/room-owner-idfail.png', fullPage:true });
  await page4.click('#idretry');
  await new Promise(r=>setTimeout(r,2000));
  const st4b = await page4.evaluate(()=>({
    idfail: !document.getElementById('idfail').hidden,
    composer: !document.getElementById('composer').hidden,
    who: document.getElementById('who').textContent,
  }));
  ok('retry: recovers to owner H view', !st4b.idfail && st4b.composer && /post as H/.test(st4b.who), st4b.who);
  await page4.type('#draft', 'human after a hiccup');
  await page4.click('#post');
  await new Promise(r=>setTimeout(r,2000));
  const srv4b = await fetch(BASE + '/room/' + uin4 + '/msg?since=0&wait=0', { headers:{ Cookie:'acq_session='+cookie } }).then(r=>r.json());
  ok('retry: stored From H (not A)', (srv4b.messages||[]).length===1 && srv4b.messages[0].from==='H', JSON.stringify(srv4b.messages));
  const recvA4 = await fetch(BASE + '/room/' + uin4 + '/msg?wait=0', { headers:{ Authorization:'Bearer '+tokA4 } }).then(r=>r.json());
  ok('retry: seat A receives H', (recvA4.messages||[]).some(m=>m.from==='H'), JSON.stringify((recvA4.messages||[]).map(m=>m.from)));
  await page4.screenshot({ path:(process.env.OUT_DIR || '.') + '/room-owner-probefail.png', fullPage:true });

  // Anonymous invite visitor: fresh browser context (no shared cookie jar).
  const ctx2 = await browser.createBrowserContext();
  const page2 = await ctx2.newPage();
  await page2.goto(BASE + '/room/' + uin, { waitUntil:'networkidle0' });
  await new Promise(r=>setTimeout(r,1500));
  const anonComposer = await page2.evaluate(()=>{ const el=document.getElementById('composer'); return el && !el.hidden; });
  const anonInvite = await page2.evaluate(()=>{ const el=document.getElementById('invite'); return el && !el.hidden; });
  ok('anon sees invite not composer', !anonComposer && anonInvite);

  await browser.close();
  console.log(results.join('\n'));
  if (failures > 0) { console.error(failures + ' assertion(s) FAILED'); process.exitCode = 1; }
})().catch(e=>{ console.error(e); console.log(results.join('\n')); process.exit(1); });
