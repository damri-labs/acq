// proxy.js - failure-injecting reverse proxy between Chrome and the server.
// Sits on PROXY_PORT (default 8100) and forwards to UPSTREAM_PORT (default
// 8099). Flag files in FLAGS_DIR (default /tmp/acq-ui-flags) inject failures;
// one-shot unless noted:
//   failget        - next GET /msg returns 502
//   failpost       - next POST /msg returns 500
//   hangup         - next POST /msg gets its socket destroyed
//   blackhole      - ALL /msg requests destroyed until the file is removed
//   commitloss     - next POST /msg commits upstream, response swallowed, socket destroyed
//   delaypost      - next POST /msg held 2.5s
//   failjoin       - next POST /room/:id/join returns 500
//   bogusjoin      - next POST /room/:id/join gets a synthetic 200 with the file's body, never forwarded
//   delayjoin      - next POST /room/:id/join held N ms (file content, default 6000)
//   blackholejoin  - next POST /room/:id/join accepted, never answered, never forwarded
//   commitlossjoin - next POST /room/:id/join commits upstream, response swallowed, socket destroyed
//   holdbodyjoin   - next join gets 200 headers now, body after N ms (default 17000)
//   holdstatus     - next GET /room/:id accepted, never answered
//   failstatus     - next GET /room/:id returns a synthetic 410
//   delaystatus    - next GET /room/:id held N ms (file content, default 4000)
//   holdbodystatus - next status GET gets 200 headers now, body after N ms (default 17000)
//   blackholeall   - ALL requests destroyed until the file is removed
// Chrome's network stack transparently retries an in-flight POST once when
// the connection dies without a response, so a true client-side send failure
// needs 'blackhole' (or 'blackholeall'), not a one-shot hangup.
const http = require('http');
const fs = require('fs');
const FLAGS = process.env.FLAGS_DIR || '/tmp/acq-ui-flags';
const UPSTREAM = parseInt(process.env.UPSTREAM_PORT || '8099', 10);
const PORT = parseInt(process.env.PROXY_PORT || '8100', 10);
fs.mkdirSync(FLAGS, { recursive: true });
http.createServer((req, res) => {
  const isMsg = /^\/room\/[^/]+\/msg(\?|$)/.test(req.url);
  const isJoin = req.method === 'POST' && /^\/room\/[^/]+\/join(\?|$)/.test(req.url);
  const isStatus = req.method === 'GET' && /^\/room\/[^/]+(\?|$)/.test(req.url) && !isMsg;

  if (isStatus && fs.existsSync(FLAGS + '/holdstatus')) {
    fs.unlinkSync(FLAGS + '/holdstatus');
    console.log('HOLD-STATUS (accepted, never answered)');
    req.resume();
    return;
  }
  if (fs.existsSync(FLAGS + '/blackholeall')) { console.log('BLACKHOLE-ALL', req.method, req.url.slice(0, 50)); req.socket.destroy(); return; }
  if (isMsg && fs.existsSync(FLAGS + '/blackhole')) { req.socket.destroy(); return; }
  if (isJoin && fs.existsSync(FLAGS + '/blackholejoin')) {
    fs.unlinkSync(FLAGS + '/blackholejoin');
    console.log('BLACKHOLE-JOIN (accepted, never answered, never forwarded)');
    req.resume(); // drain the request body, then never respond
    return;
  }
  const swallowJoin = isJoin && fs.existsSync(FLAGS + '/commitlossjoin');
  if (swallowJoin) fs.unlinkSync(FLAGS + '/commitlossjoin');
  const swallowMsg = isMsg && req.method === 'POST' && fs.existsSync(FLAGS + '/commitloss');
  if (swallowMsg) fs.unlinkSync(FLAGS + '/commitloss');
  const swallow = swallowJoin || swallowMsg;
  if (isJoin && fs.existsSync(FLAGS + '/bogusjoin')) {
    const body = fs.readFileSync(FLAGS + '/bogusjoin', 'utf8') || '{}';
    fs.unlinkSync(FLAGS + '/bogusjoin');
    console.log('BOGUS-JOIN synthetic 200, never forwarded');
    res.writeHead(200, { 'Content-Type': 'application/json' });
    res.end(body);
    return;
  }
  if (isJoin && fs.existsSync(FLAGS + '/failjoin')) {
    fs.unlinkSync(FLAGS + '/failjoin');
    console.log('FAIL-JOIN synthetic 500');
    res.writeHead(500, { 'Content-Type': 'application/json' });
    res.end('{}');
    return;
  }
  if (isMsg && req.method === 'GET' && fs.existsSync(FLAGS + '/failget')) { fs.unlinkSync(FLAGS + '/failget'); res.writeHead(502); res.end('bad gateway'); return; }
  if (isMsg && req.method === 'POST' && fs.existsSync(FLAGS + '/failpost')) { fs.unlinkSync(FLAGS + '/failpost'); res.writeHead(500); res.end('{}'); return; }
  if (isMsg && req.method === 'POST' && fs.existsSync(FLAGS + '/hangup')) { fs.unlinkSync(FLAGS + '/hangup'); console.log('HANGUP consumed for', req.url.slice(0, 50)); req.socket.destroy(); return; }
  const hbj = isJoin && fs.existsSync(FLAGS + '/holdbodyjoin');
  if (hbj) { var hbjMs = parseInt(fs.readFileSync(FLAGS + '/holdbodyjoin', 'utf8'), 10) || 17000; fs.unlinkSync(FLAGS + '/holdbodyjoin'); }
  const hbs = isStatus && fs.existsSync(FLAGS + '/holdbodystatus');
  if (hbs) { var hbsMs = parseInt(fs.readFileSync(FLAGS + '/holdbodystatus', 'utf8'), 10) || 17000; fs.unlinkSync(FLAGS + '/holdbodystatus'); }
  const go = () => {
    if (hbj || hbs) {
      // Headers now, body after a hold: reproduces 200-headers/stalled-body.
      const ms = hbj ? hbjMs : hbsMs;
      console.log('HOLDBODY ' + ms + 'ms', req.method, req.url.slice(0, 60));
      const p = http.request({ hostname: '127.0.0.1', port: UPSTREAM, path: req.url, method: req.method, headers: req.headers }, (pr) => {
        const chunks = [];
        pr.on('data', (c) => chunks.push(c));
        pr.on('end', () => {
          res.writeHead(pr.statusCode, pr.headers);
          res.flushHeaders();
          setTimeout(() => res.end(Buffer.concat(chunks)), ms);
        });
      });
      p.on('error', () => { try { res.writeHead(502); res.end(); } catch (e) {} });
      req.pipe(p);
      return;
    }
    if (isStatus && fs.existsSync(FLAGS + '/failstatus')) {
      fs.unlinkSync(FLAGS + '/failstatus');
      console.log('FAIL-STATUS synthetic 410');
      res.writeHead(410, { 'Content-Type': 'application/json' });
      res.end('{"detail":"room expired"}');
      return;
    }
    const p = http.request({ hostname: '127.0.0.1', port: UPSTREAM, path: req.url, method: req.method, headers: req.headers }, (pr) => {
      console.log('UPSTREAM', req.method, req.url.slice(0, 60), '->', pr.statusCode, swallow ? '(swallowed)' : '');
      if (swallow) { console.log('COMMITLOSS: upstream answered', pr.statusCode, '- response swallowed'); pr.resume(); pr.on('end', () => req.socket.destroy()); return; }
      res.writeHead(pr.statusCode, pr.headers); pr.pipe(res);
    });
    p.on('error', (e) => { console.log('UPSTREAM ERR', e.message); try { res.writeHead(502); res.end(); } catch (x) {} });
    req.pipe(p);
  };
  if (isStatus && fs.existsSync(FLAGS + '/delaystatus')) {
    const ms = parseInt(fs.readFileSync(FLAGS + '/delaystatus', 'utf8'), 10) || 4000;
    fs.unlinkSync(FLAGS + '/delaystatus');
    console.log('DELAYING STATUS ' + ms + 'ms', req.url.slice(0, 60));
    setTimeout(go, ms);
  } else if (isJoin && fs.existsSync(FLAGS + '/delayjoin')) {
    const ms = parseInt(fs.readFileSync(FLAGS + '/delayjoin', 'utf8'), 10) || 6000;
    fs.unlinkSync(FLAGS + '/delayjoin');
    console.log('DELAYING JOIN ' + ms + 'ms', req.url.slice(0, 60));
    setTimeout(go, ms);
  } else if (isMsg && req.method === 'POST' && fs.existsSync(FLAGS + '/delaypost')) {
    fs.unlinkSync(FLAGS + '/delaypost');
    console.log('DELAYING POST', req.url.slice(0, 60));
    setTimeout(go, 2500);
  } else go();
}).listen(PORT, () => console.log('proxy on ' + PORT + ' -> ' + UPSTREAM));
