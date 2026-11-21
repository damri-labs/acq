// Instinct, acting on behalf of Tal Chaim Damri.
// Local malformed 2xx acknowledgement contract tests. Never a hosted room.
import {createServer} from 'node:http';
import {spawn} from 'node:child_process';
import {mkdtempSync,mkdirSync,writeFileSync,rmSync} from 'node:fs';
import {tmpdir} from 'node:os';
import {join} from 'node:path';
import assert from 'node:assert/strict';
const cli=process.env.ACQ_TEST_CLI || new URL('./acq.js',import.meta.url).pathname;
let failures=0;
for (const kind of ['zero','fraction','unsafe','foreigncid','missingcid','committedforeign','valid']) {
 const home=mkdtempSync(join(tmpdir(),'acq-ack-'));const dir=join(home,'.config','acq');mkdirSync(dir,{recursive:true});writeFileSync(join(dir,'session.json'),JSON.stringify({room:'FIXTURE',seat:'A',token:'fixture'}));
 let posts=0,reads=0,cid;
 const srv=createServer((req,res)=>{
  if(req.method==='POST') {posts++;let raw='';req.on('data',d=>raw+=d);req.on('end',()=>{
   cid=JSON.parse(raw).cid;const d={seq:1,cid};
   if(kind==='zero')d.seq=0;if(kind==='fraction')d.seq=1.5;if(kind==='unsafe')d.seq=2**53;
   if(kind==='foreigncid'||kind==='committedforeign')d.cid='another-request';if(kind==='missingcid')delete d.cid;
   res.writeHead(200,{'content-type':'application/json'});res.end(JSON.stringify(d));
  });return;}
  reads++;res.writeHead(200,{'content-type':'application/json'});res.end(JSON.stringify({messages:kind==='committedforeign'?[{seq:1,cid,text:'fixture'}]:[]}));
 });
 await new Promise(r=>srv.listen(0,'127.0.0.1',r));
 const p=spawn(process.execPath,[cli,'send','fixture'],{env:{...process.env,HOME:home,ACQ_SERVER:`http://127.0.0.1:${srv.address().port}`}});let out='',err='';p.stdout.on('data',d=>out+=d);p.stderr.on('data',d=>err+=d);const code=await new Promise(r=>p.on('close',r));srv.closeAllConnections();await new Promise(r=>srv.close(r));rmSync(home,{recursive:true,force:true});
 try {assert.equal(posts,1);if(kind==='valid'||kind==='committedforeign'){assert.equal(code,0);assert.equal(reads,kind==='valid'?0:1);assert.match(out,/sent seq=1/);if(kind==='committedforeign')assert.match(out,/verified after an ambiguous failure/);}else{assert.equal(code,1);assert.equal(reads,3);assert.ok(err.includes(cid));assert.match(err,/unconfirmed|unknown/);assert.doesNotMatch(out,/sent seq=/);}console.log(`PASS ${kind}`);}catch(e){failures++;console.error(`FAIL ${kind}: ${e.message}; code=${code} reads=${reads} out=${out} err=${err}`);}
}
if(failures)process.exitCode=1;
