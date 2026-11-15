const http = require('http');
const fs = require('fs');
const FLAGS = process.env.FLAGS_DIR || '/tmp/acq-ui-flags';
fs.mkdirSync(FLAGS, { recursive: true });
http.createServer((req, res) => {
  const isMsg = req.url.includes('/msg');
  if (isMsg && fs.existsSync(FLAGS + '/blackhole')) { req.socket.destroy(); return; }
  if (isMsg && req.method === 'GET' && fs.existsSync(FLAGS + '/failget')) { fs.unlinkSync(FLAGS + '/failget'); res.writeHead(502); res.end('bad gateway'); return; }
  if (isMsg && req.method === 'POST' && fs.existsSync(FLAGS + '/failpost')) { fs.unlinkSync(FLAGS + '/failpost'); res.writeHead(500); res.end('{}'); return; }
  if (isMsg && req.method === 'POST' && fs.existsSync(FLAGS + '/hangup')) { fs.unlinkSync(FLAGS + '/hangup'); console.log('HANGUP consumed for', req.url.slice(0,50)); req.socket.destroy(); return; }
  console.log(new Date().toISOString().slice(17,23), req.method, req.url.slice(0,60), 'flags:', fs.existsSync(FLAGS + '/hangup')?'H':'-', fs.existsSync(FLAGS + '/delaypost')?'D':'-');
  const go = () => {
    const p = http.request({ hostname: '127.0.0.1', port: 8099, path: req.url, method: req.method, headers: req.headers }, (pr) => { res.writeHead(pr.statusCode, pr.headers); pr.pipe(res); });
    p.on('error', () => { try { res.writeHead(502); res.end(); } catch(e){} });
    req.pipe(p);
  };
  if (isMsg && req.method === 'POST' && fs.existsSync(FLAGS + '/delaypost')) { fs.unlinkSync(FLAGS + '/delaypost'); setTimeout(go, 2500); return; }
  go();
}).listen(8100, () => console.log('proxy v2 on 8100'));
