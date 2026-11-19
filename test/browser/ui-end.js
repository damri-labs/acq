// ui-end.js - deliberate in-UI end action for browser seat sessions.
//
// Scenario: two browser participants (seat A via saved credential, seat B
// via "Join as human guest") both get an "End room" control. Seat B uses
// it: two-step inline confirm, then one end:true post on the seat send
// path. Both pages must converge: pill "ended", composer disabled, the
// empty-text end message rendered as "(ended the room)" with the [end]
// marker, and no End control left visible.
//
// Controls: Cancel must restore the button without ending; the owner (H)
// voice path keeps no end control (ending stays a seat action).
// Race coverage (deterministic, via request interception):
//  R1 peer-end-during-lost-request: the page's end POST is aborted
//     (never commits) while the PEER ends the room server-side; the UI
//     must say only "not confirmed - refreshing" and then converge to
//     ended through its single reconciliation poll - that poll is not an
//     end retry. (R1 does NOT establish commit-then-loss; C1 does.)
//  R2 failed reconciliation: the end POST and the immediate reconciliation
//     poll are both aborted; state must remain unknown (no open/ended
//     claim), controls re-enable, and no second end POST ever leaves the
//     page (no auto-retry, no duplicate writes).
//  C1 commit-then-lost-response (via the 8100 proxy's one-shot commitloss
//     flag): the end POST commits upstream, the response is swallowed and
//     the socket destroyed; the end must commit exactly once on the wire
//     and the page must converge to the truthful ENDED state - never the
//     "room gone" view - whether Chrome surfaces a network error or
//     transparently retries into the server's 410.
//  C2 delayed-send race (via the proxy's delaypost flag): a peer end
//     commits while this page's send is held at the proxy; the send loses
//     with a 410 and the page must converge to ENDED with the transcript
//     intact, not showGone.
//  C1/C2 need the proxy (PROXY_URL, default http://127.0.0.1:8100) with
//  FLAGS_DIR shared between this script and the proxy.
// Run against the README server env (PORT=8099 ...).
const puppeteer = require('puppeteer-core');
const fs = require('fs');
const BASE = process.env.BASE_URL || 'http://127.0.0.1:8099';
const PROXY = process.env.PROXY_URL || 'http://127.0.0.1:8100';
const FLAGS = process.env.FLAGS_DIR || '/tmp/acq-ui-flags';
const CHROME = process.env.CHROME_PATH || 'google-chrome';
const flag = (n) => fs.writeFileSync(FLAGS + '/' + n, '1');

const results = [];
let failures = 0;
const ok = (name, cond, extra) => { if (!cond) failures++; results.push((cond?'PASS':'FAIL')+' '+name+(extra?' | '+extra:'')); };

(async()=>{
  const c = await fetch(BASE + '/room', { method:'POST' }).then(r=>r.json());
  ok('create room', !!c.roomId, c.roomId);
  if (!c.roomId) { console.log(results.join('\n')); process.exit(1); }
  const uin = c.roomId, tokA = c.token, credA = c.credential;

  const browser = await puppeteer.launch({ headless:'new', executablePath: CHROME, args:['--no-sandbox','--disable-dev-shm-usage'] });

  // --- page A: creator browser with saved seat-A credential -------------
  const pa = await browser.newPage();
  await pa.goto(BASE + '/', { waitUntil:'networkidle0' });
  await pa.evaluate((blob)=>{ localStorage.setItem('acq_credentials', JSON.stringify([blob])); }, credA);
  await pa.goto(BASE + '/room/' + uin, { waitUntil:'networkidle0' });
  await new Promise(r=>setTimeout(r,1200));
  const a1 = await pa.evaluate(()=>({
    endVisible: !document.getElementById('end').hidden,
    confirmHidden: document.getElementById('end-confirm').hidden,
  }));
  ok('seat A sees End room control', a1.endVisible && a1.confirmHidden);

  // --- page B (separate profile): join as human guest -------------------
  const ctxB = await browser.createBrowserContext();
  const pb = await ctxB.newPage();
  await pb.goto(BASE + '/room/' + uin, { waitUntil:'networkidle0' });
  await new Promise(r=>setTimeout(r,800));
  await pb.click('#join-human');
  await new Promise(r=>setTimeout(r,1500));
  const b1 = await pb.evaluate(()=>({
    endVisible: !document.getElementById('end').hidden,
    who: (document.getElementById('who')||{}).textContent || '',
  }));
  ok('seat B guest sees End room control', b1.endVisible, b1.who);

  // --- cancel path: confirm strip opens, Cancel restores the button -----
  await pb.click('#end');
  await new Promise(r=>setTimeout(r,300));
  const b2 = await pb.evaluate(()=>({
    confirmShown: !document.getElementById('end-confirm').hidden,
    btnHidden: document.getElementById('end').hidden,
    text: (document.querySelector('#end-confirm span')||{}).textContent || '',
  }));
  ok('click opens inline confirm with consequence text', b2.confirmShown && b2.btnHidden && /cannot be undone/.test(b2.text));
  await pb.click('#end-no');
  await new Promise(r=>setTimeout(r,300));
  const b3 = await pb.evaluate(()=>({
    confirmHidden: document.getElementById('end-confirm').hidden,
    btnShown: !document.getElementById('end').hidden,
  }));
  ok('Cancel restores End room control, room still open', b3.confirmHidden && b3.btnShown);

  // --- seat A speaks, so the transcript has one normal message ----------
  await pa.type('#draft', 'hello from A');
  await pa.click('#post');
  await new Promise(r=>setTimeout(r,1200));

  // --- seat B confirms the end ------------------------------------------
  await pb.click('#end');
  await new Promise(r=>setTimeout(r,300));
  await pb.screenshot({ path:'/downloads/ui-end-confirm.png' });
  await pb.click('#end-yes');
  await new Promise(r=>setTimeout(r,1800));

  const b4 = await pb.evaluate(()=>({
    pill: (document.getElementById('pill')||{}).textContent || '',
    draftDisabled: document.getElementById('draft').disabled,
    endHidden: document.getElementById('end').hidden,
    confirmHidden: document.getElementById('end-confirm').hidden,
    log: (document.getElementById('log')||{}).textContent || '',
  }));
  ok('seat B page shows ended pill', /ended/.test(b4.pill), b4.pill);
  ok('seat B composer disabled after end', b4.draftDisabled);
  ok('seat B end controls gone after end', b4.endHidden && b4.confirmHidden);
  ok('transcript renders truthful end line', /\(ended the room\)/.test(b4.log) && /\[end\]/.test(b4.log));

  await new Promise(r=>setTimeout(r,3000));
  const a2 = await pa.evaluate(()=>({
    pill: (document.getElementById('pill')||{}).textContent || '',
    draftDisabled: document.getElementById('draft').disabled,
    endHidden: document.getElementById('end').hidden,
    log: (document.getElementById('log')||{}).textContent || '',
  }));
  ok('seat A page converges to ended pill', /ended/.test(a2.pill), a2.pill);
  ok('seat A composer disabled after peer end', a2.draftDisabled);
  ok('seat A end control gone after end', a2.endHidden);
  ok('seat A sees prior message and end line', /hello from A/.test(a2.log) && /\(ended the room\)/.test(a2.log));
  await pa.screenshot({ path:'/downloads/ui-end-ended-a.png' });

  // --- wire check: room state says ended, writes rejected ---------------
  const wire = await fetch(BASE + '/room/' + uin + '/msg?wait=0&view=1', { headers:{ Authorization:'Bearer ' + tokA } }).then(r=>r.json());
  ok('wire: room ended flag set', wire.ended === true);
  const endMsg = (wire.messages||[]).find(m=>m.end);
  ok('wire: end message from seat B with empty text', !!endMsg && endMsg.from==='B' && endMsg.text==='');

  // ================= R1: committed end, lost response =================
  const r1 = await fetch(BASE + '/room', { method:'POST' }).then(r=>r.json());
  ok('R1 create room', !!r1.roomId, r1.roomId);
  const ctxR1 = await browser.createBrowserContext();
  const pr1 = await ctxR1.newPage();
  // join as human guest so this page holds seat B
  await pr1.goto(BASE + '/room/' + r1.roomId, { waitUntil:'networkidle0' });
  await new Promise(r=>setTimeout(r,800));
  await pr1.click('#join-human');
  await new Promise(r=>setTimeout(r,1500));
  let r1EndPosts = 0;
  await pr1.setRequestInterception(true);
  pr1.on('request', (rq)=>{
    const u = rq.url();
    if (rq.method() === 'POST' && u.includes('/msg')) { r1EndPosts++; rq.abort(); }
    else rq.continue();
  });
  // peer (seat A, via API) ends the room exactly as the page's end is lost
  await pr1.click('#end');
  await new Promise(r=>setTimeout(r,300));
  await pr1.click('#end-yes');
  await fetch(BASE + '/room/' + r1.roomId + '/msg', { method:'POST', headers:{ Authorization:'Bearer ' + r1.token, 'Content-Type':'application/json' }, body: JSON.stringify({ text:'', end:true }) });
  await new Promise(r=>setTimeout(r,600));
  const r1a = await pr1.evaluate(()=>({
    err: (document.getElementById('send-err')||{}).textContent || '',
  }));
  ok('R1 lost response: wording says not-confirmed + refreshing, no state claim', /not confirmed/i.test(r1a.err) && /refresh/i.test(r1a.err) && !/still open/i.test(r1a.err), r1a.err);
  await new Promise(r=>setTimeout(r,4000));
  const r1b = await pr1.evaluate(()=>({
    pill: (document.getElementById('pill')||{}).textContent || '',
    draftDisabled: document.getElementById('draft').disabled,
  }));
  ok('R1 single reconciliation poll converges to ended', /ended/.test(r1b.pill) && r1b.draftDisabled, r1b.pill);
  ok('R1 exactly one end POST left the page (poll is not a retry)', r1EndPosts === 1, 'count='+r1EndPosts);
  await ctxR1.close();

  // ============ R2: end lost AND reconciliation poll fails =============
  const r2 = await fetch(BASE + '/room', { method:'POST' }).then(r=>r.json());
  ok('R2 create room', !!r2.roomId, r2.roomId);
  const ctxR2 = await browser.createBrowserContext();
  const pr2 = await ctxR2.newPage();
  await pr2.goto(BASE + '/room/' + r2.roomId, { waitUntil:'networkidle0' });
  await new Promise(r=>setTimeout(r,800));
  await pr2.click('#join-human');
  await new Promise(r=>setTimeout(r,1500));
  let r2EndPosts = 0, r2PollsAborted = 0;
  await pr2.setRequestInterception(true);
  pr2.on('request', (rq)=>{
    const u = rq.url();
    if (rq.method() === 'POST' && u.includes('/msg')) { r2EndPosts++; rq.abort(); return; }
    if (rq.method() === 'GET' && u.includes('/msg') && r2PollsAborted < 2) { r2PollsAborted++; rq.abort(); return; }
    rq.continue();
  });
  await pr2.click('#end');
  await new Promise(r=>setTimeout(r,300));
  await pr2.click('#end-yes');
  await new Promise(r=>setTimeout(r,900));
  const r2a = await pr2.evaluate(()=>({
    err: (document.getElementById('send-err')||{}).textContent || '',
    pill: (document.getElementById('pill')||{}).textContent || '',
    draftDisabled: document.getElementById('draft').disabled,
    endYesDisabled: document.getElementById('end-yes').disabled,
  }));
  ok('R2 unknown stays unknown: not-confirmed wording, no open/ended claim', /not confirmed/i.test(r2a.err) && !/still open/i.test(r2a.err) && !/ended the room/i.test(r2a.err), r2a.err);
  ok('R2 pill does not claim ended after failed refresh', !/^ended$/.test(r2a.pill), r2a.pill);
  ok('R2 controls re-enabled for a deliberate operator decision', !r2a.draftDisabled && !r2a.endYesDisabled);
  await new Promise(r=>setTimeout(r,11000));
  ok('R2 no duplicate end POST after failed refresh (no auto-retry)', r2EndPosts === 1, 'count='+r2EndPosts);
  const r2b = await pr2.evaluate(()=>({ pill: (document.getElementById('pill')||{}).textContent || '' }));
  ok('R2 room actually still open, later poll shows paired', /paired/.test(r2b.pill), r2b.pill);
  await ctxR2.close();

  // ======== C1: commit-then-lost-response (proxy commitloss) ========
  {
    const c1 = await fetch(BASE + '/room', { method:'POST' }).then(r=>r.json());
    ok('C1 create room', !!c1.roomId, c1.roomId);
    const ctxC = await browser.createBrowserContext();
    const pc = await ctxC.newPage();
    await pc.goto(PROXY + '/room/' + c1.roomId, { waitUntil:'networkidle0' });
    await new Promise(r=>setTimeout(r,900));
    await pc.click('#join-human');
    await new Promise(r=>setTimeout(r,1500));
    flag('commitloss'); // exactly one POST commits, its response is swallowed
    await pc.click('#end');
    await new Promise(r=>setTimeout(r,300));
    await pc.click('#end-yes');
    await new Promise(r=>setTimeout(r,3500));
    const wire = await fetch(BASE + '/room/' + c1.roomId + '/msg?since=0&self=1&wait=0&view=1', { headers:{ Authorization:'Bearer ' + c1.token } }).then(r=>r.json());
    const ends = (wire.messages||[]).filter(m=>m.end);
    ok('C1 end committed server-side exactly once (no duplicate write)', wire.ended === true && ends.length === 1, JSON.stringify((wire.messages||[]).map(m=>[m.seq,m.from,m.end])));
    const sc = await pc.evaluate(()=>({
      pill: (document.getElementById('pill')||{}).textContent || '',
      empty: (document.getElementById('empty')||{}).textContent || '',
      draftDisabled: document.getElementById('draft').disabled,
    }));
    ok('C1 page converges to truthful ended (not gone)', /ended/.test(sc.pill) && !/gone/.test(sc.empty), 'pill='+sc.pill+' empty='+sc.empty);
    ok('C1 composer disabled after convergence', sc.draftDisabled);
    await ctxC.close();
  }

  // ======== C2: peer end commits while this page's send is delayed ========
  {
    const c2 = await fetch(BASE + '/room', { method:'POST' }).then(r=>r.json());
    ok('C2 create room', !!c2.roomId, c2.roomId);
    const ctxD = await browser.createBrowserContext();
    const pd = await ctxD.newPage();
    await pd.goto(PROXY + '/room/' + c2.roomId, { waitUntil:'networkidle0' });
    await new Promise(r=>setTimeout(r,900));
    await pd.click('#join-human');
    await new Promise(r=>setTimeout(r,1500));
    flag('delaypost'); // this page's next POST /msg held 2.5s at the proxy
    await pd.type('#draft', 'B speaking during A end');
    await pd.click('#post');
    await new Promise(r=>setTimeout(r,300)); // send in flight, held
    await fetch(BASE + '/room/' + c2.roomId + '/msg', { method:'POST', headers:{ Authorization:'Bearer ' + c2.token, 'Content-Type':'application/json' }, body: JSON.stringify({ text:'', end:true }) });
    await new Promise(r=>setTimeout(r,5000));
    const wire = await fetch(BASE + '/room/' + c2.roomId + '/msg?since=0&self=1&wait=0&view=1', { headers:{ Authorization:'Bearer ' + c2.token } }).then(r=>r.json());
    const ends = (wire.messages||[]).filter(m=>m.end);
    ok('C2 room ended exactly once by the peer', wire.ended === true && ends.length === 1 && ends[0].from === 'A');
    ok('C2 delayed send rejected (not appended)', !(wire.messages||[]).some(m=>m.text === 'B speaking during A end'));
    const sd = await pd.evaluate(()=>({
      pill: (document.getElementById('pill')||{}).textContent || '',
      empty: (document.getElementById('empty')||{}).textContent || '',
      draftDisabled: document.getElementById('draft').disabled,
      draftValue: document.getElementById('draft').value,
    }));
    ok('C2 send-race loser sees truthful ended (not gone)', /ended/.test(sd.pill) && !/gone/.test(sd.empty), 'pill='+sd.pill+' empty='+sd.empty);
    ok('C2 composer disabled after convergence', sd.draftDisabled);
    await ctxD.close();
  }

  console.log(results.join('\n'));
  console.log(failures ? 'FAILURES: '+failures : 'ALL PASS');
  await browser.close();
  process.exit(failures ? 1 : 0);
})().catch(e=>{ console.error('FATAL', e.message); console.log(results.join('\n')); process.exit(1); });
