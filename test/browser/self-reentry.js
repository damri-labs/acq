// self-reentry.js - credential-isolation diagnosis for the same-browser
// second-participant path (01:50 control-unit reproduction).
//
// Scenario: a first-time evaluator creates a room in their browser (seat A
// credential lands in localStorage via the Saved-rooms shape), then opens
// the invite link in a SECOND TAB of the same browser profile to try the
// peer side. Hypothesis: the room page silently re-enters seat A (stored
// credential wins, invite card never shown), so the person talks to
// themselves and can never reach "Join as human guest" from this profile.
//
// Controls: reload on tab 1 must still resume seat A (deliberate
// re-entry), and a separate browser profile (incognito context = separate
// localStorage) must still get the invite card and join as seat B.
// Wire From is asserted via the seat-authenticated transcript, not the UI.
//
// Seeding: creates its own guest room via the API and injects the packed
// credential into the page's localStorage exactly in the Saved-rooms
// shape. Run against the README server env (PORT=8099 ...).
const puppeteer = require('puppeteer-core');
const BASE = process.env.BASE_URL || 'http://127.0.0.1:8099';
const CHROME = process.env.CHROME_PATH || 'google-chrome';

const results = [];
let failures = 0;
const ok = (name, cond, extra) => { if (!cond) failures++; results.push((cond?'PASS':'FAIL')+' '+name+(extra?' | '+extra:'')); };
const note = (s) => results.push('INFO '+s);

(async()=>{
  const c = await fetch(BASE + '/room', { method:'POST' }).then(r=>r.json());
  ok('create room', !!c.roomId, c.roomId);
  if (!c.roomId) { console.log(results.join('\n')); process.exit(1); }
  const uin = c.roomId, tokA = c.token, credA = c.credential;
  ok('create response carries packed credential', typeof credA === 'string' && credA.startsWith('acq1_'));

  const browser = await puppeteer.launch({ headless:'new', executablePath: CHROME, args:['--no-sandbox','--disable-dev-shm-usage'] });
  const ctx = browser.defaultBrowserContext();

  // --- tab 1: creator browser (credential already saved) -----------------
  const t1 = await browser.newPage();
  await t1.goto(BASE + '/', { waitUntil:'networkidle0' });
  await t1.evaluate((blob)=>{ localStorage.setItem('acq_credentials', JSON.stringify([blob])); }, credA);
  await t1.goto(BASE + '/room/' + uin, { waitUntil:'networkidle0' });
  await new Promise(r=>setTimeout(r,1200));
  const s1 = await t1.evaluate(()=>({
    composer: !document.getElementById('composer').hidden,
    invite: !document.getElementById('invite').hidden,
    who: (document.getElementById('who')||{}).textContent || '',
  }));
  ok('tab1 enters seat A (deliberate re-entry)', s1.composer && !s1.invite && /seat A/.test(s1.who), s1.who);

  // --- tab 1 reload: deliberate resume must survive -----------------------
  await t1.reload({ waitUntil:'networkidle0' });
  await new Promise(r=>setTimeout(r,1200));
  const s1r = await t1.evaluate(()=>({
    composer: !document.getElementById('composer').hidden,
    who: (document.getElementById('who')||{}).textContent || '',
  }));
  ok('tab1 reload resumes seat A (deliberate)', s1r.composer && /seat A/.test(s1r.who), s1r.who);

  // --- tab 2, SAME profile: the evaluator's peer-side attempt -------------
  const t2 = await browser.newPage();
  await t2.goto(BASE + '/room/' + uin, { waitUntil:'networkidle0' });
  await new Promise(r=>setTimeout(r,1200));
  const s2 = await t2.evaluate(()=>({
    composer: !document.getElementById('composer').hidden,
    invite: !document.getElementById('invite').hidden,
    joinHuman: !!(document.getElementById('join-human') && !document.getElementById('join-human').closest('[hidden]')),
    who: (document.getElementById('who')||{}).textContent || '',
    pill: (document.getElementById('pill')||{}).textContent || '',
  }));
  note('tab2 state: ' + JSON.stringify(s2));
  ok('tab2 (same profile) re-enters seat A (deliberate re-entry preserved)',
     s2.composer && !s2.invite && /seat A/.test(s2.who),
     'composer=' + s2.composer + ' invite=' + s2.invite + ' who=' + JSON.stringify(s2.who));
  const n2 = await t2.evaluate(()=>{
    const el = document.getElementById('selfpeer');
    return el ? { shown: !el.hidden, text: el.textContent } : { shown: false, text: '(element missing)' };
  });
  ok('FIX: tab2 shows the same-browser note while unpaired', n2.shown, n2.text.slice(0,90));
  ok('FIX: note names seat A and an accurate remedy',
     /seat A/.test(n2.text) && /another browser|private window|agent running the join command/.test(n2.text), n2.text.slice(0,140));

  // tab 2 posts: the wire From must prove it spoke AS SEAT A (talking to self)
  await t2.type('#draft', 'is anyone there?');
  await t2.click('#post');
  await new Promise(r=>setTimeout(r,1200));
  const wire = await fetch(BASE + '/room/' + uin + '/msg?since=0&self=1&wait=0', { headers:{ Authorization:'Bearer ' + tokA } }).then(r=>r.json());
  const froms = (wire.messages||[]).map(m=>m.from);
  ok('DEFECT: tab2 message lands on the wire as from=A (self-talk)', froms.length === 1 && froms[0] === 'A', JSON.stringify(froms));

  // --- control: separate profile (incognito = separate localStorage) ------
  const inc = await browser.createBrowserContext();
  const p3 = await inc.newPage();
  await p3.goto(BASE + '/room/' + uin, { waitUntil:'networkidle0' });
  await new Promise(r=>setTimeout(r,1200));
  const s3 = await p3.evaluate(()=>({
    invite: !document.getElementById('invite').hidden,
    composer: !document.getElementById('composer').hidden,
  }));
  ok('control: separate profile gets the invite card, not a silent seat', s3.invite && !s3.composer);
  await p3.click('#join-human');
  await new Promise(r=>setTimeout(r,1500));
  await p3.type('#draft', 'peer here');
  await p3.click('#post');
  await new Promise(r=>setTimeout(r,1200));
  const wire2 = await fetch(BASE + '/room/' + uin + '/msg?since=0&self=1&wait=0', { headers:{ Authorization:'Bearer ' + tokA } }).then(r=>r.json());
  const froms2 = (wire2.messages||[]).map(m=>m.from);
  ok('control: separate profile posts as from=B', froms2.join(',') === 'A,B', JSON.stringify(froms2));

  // pairing makes the note irrelevant: tab1's next poll must hide it
  await new Promise(r=>setTimeout(r,3500));
  const n1 = await t1.evaluate(()=>{
    const el = document.getElementById('selfpeer');
    return el ? !el.hidden : false;
  });
  ok('FIX: note hidden once the room is paired', !n1);

  await browser.close();
  console.log(results.join('\n'));
  console.log(failures ? ('FAILURES: ' + failures) : 'ALL PASS');
  process.exit(failures ? 1 : 0);
})().catch(e=>{ console.error('HARNESS ERROR', e); process.exit(2); });
