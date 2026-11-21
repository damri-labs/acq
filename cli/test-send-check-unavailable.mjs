// Instinct acting on behalf of Tal Chaim Damri.
// A failed read cannot settle an earlier send. Test only local fixture servers.
import { createServer } from 'node:http';
import { spawn } from 'node:child_process';
import { mkdtempSync, mkdirSync, writeFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import assert from 'node:assert/strict';

const cli = process.env.ACQ_TEST_CLI || new URL('./acq.js', import.meta.url).pathname;
let failures = 0;
for (const mode of ['transient503', 'all503', 'malformed200', 'falseThen503']) {
  const home = mkdtempSync(join(tmpdir(), 'acq-check-'));
  const dir = join(home, '.config', 'acq'); mkdirSync(dir, { recursive: true });
  writeFileSync(join(dir, 'session.json'), JSON.stringify({ room: 'TESTROOM', token: 'fixture', seat: 'A' }));
  let reads = 0, posts = 0, cid, message;
  const srv = createServer((req, res) => {
    if (req.method === 'POST') {
      posts++;
      let raw = ''; req.on('data', d => raw += d); req.on('end', () => {
        const b = JSON.parse(raw); cid = b.cid;
        message = { seq: 1, cid, text: b.text, from: 'A' };
        if (mode === 'falseThen503') {
          res.writeHead(500, { 'content-type': 'application/json' });
          res.end(JSON.stringify({ stored: false, detail: 'fixture precommit rejection' }));
        } else req.socket.destroy();
      }); return;
    }
    reads++;
    if (mode === 'transient503' && reads > 1) {
      res.writeHead(200, { 'content-type': 'application/json' });
      res.end(JSON.stringify({ messages: [message] }));
    } else if (mode === 'malformed200') {
      res.writeHead(200, { 'content-type': 'application/json' }); res.end('{}');
    } else {
      res.writeHead(503, { 'content-type': 'application/json' });
      res.end(JSON.stringify({ detail: 'fixture check temporarily unavailable' }));
    }
  });
  await new Promise(r => srv.listen(0, '127.0.0.1', r));
  const child = spawn(process.execPath, [cli, 'send', 'fixture message'], {
    env: { ...process.env, HOME: home, ACQ_SERVER: `http://127.0.0.1:${srv.address().port}` },
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  let out = '', err = ''; child.stdout.on('data', d => out += d); child.stderr.on('data', d => err += d);
  const code = await new Promise(r => child.on('close', r));
  srv.closeAllConnections(); await new Promise(r => srv.close(r)); rmSync(home, { recursive: true, force: true });
  try {
    assert.equal(posts, 1, 'never resend');
    if (mode === 'transient503') {
      assert.equal(code, 0); assert.match(out, /sent seq=1 \(verified after an ambiguous failure\)/);
      assert.equal(reads, 2);
    } else {
      assert.equal(code, 1); assert.equal(reads, 3);
      assert.ok(err.includes(cid), 'retain request id for checking');
      assert.match(err, /unknown|unconfirmed/);
      assert.doesNotMatch(err, /message was not stored|checks are negative|TypeError/);
    }
    console.log(`PASS ${mode}`);
  } catch (e) { failures++; console.error(`FAIL ${mode}: ${e.message}; reads=${reads}; code=${code}; stderr=${err}`); }
}
if (failures) process.exitCode = 1;
