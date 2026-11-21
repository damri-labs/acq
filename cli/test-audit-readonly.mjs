// Instinct, acting on behalf of Tal Chaim Damri.
// User audit commands must not consume peer delivery, even without a local cursor.
import {createServer} from 'node:http';
import {spawn} from 'node:child_process';
import {mkdtempSync,mkdirSync,writeFileSync,readFileSync,existsSync,rmSync} from 'node:fs';
import {join} from 'node:path';
import {tmpdir} from 'node:os';
import assert from 'node:assert/strict';
const cli=process.env.ACQ_TEST_CLI || new URL('./acq.js',import.meta.url).pathname;
let failures=0;
for(const seeded of [false,true]) for(const ended of [false,true]) {
 const home=mkdtempSync(join(tmpdir(),'acq-audit-'));const dir=join(home,'.config','acq');mkdirSync(dir,{recursive:true});
 const session=JSON.stringify({room:'FIXTURE',seat:'A',token:'synthetic'});writeFileSync(join(dir,'session.json'),session);
 const curs=join(dir,'cursors.json');if(seeded)writeFileSync(curs,JSON.stringify({FIXTURE:0,OTHER:12}));const before=seeded?readFileSync(curs,'utf8'):null;
 let delivered=0;const queries=[];let writes=0;
 const messages=[{seq:1,from:'A',text:'own synthetic message',cid:'own-id'},{seq:2,from:'B',text:'pending synthetic peer',end:ended,cid:'peer-id'}];
 const srv=createServer((req,res)=>{
  if(req.method!=='GET'){writes++;res.writeHead(405);res.end();return;}
  const u=new URL(req.url,'http://localhost');queries.push(u);
  const since=u.searchParams.has('since')?Number(u.searchParams.get('since')):delivered;
  const out=messages.filter(m=>m.seq>since&&(u.searchParams.get('self')==='1'||m.from!=='A'));
  if(u.searchParams.get('peek')!=='1')delivered=2;
  res.writeHead(200,{'content-type':'application/json'});res.end(JSON.stringify({messages:out,cursor:2,peerJoined:true,ended,expiresAt:0}));
 });await new Promise(r=>srv.listen(0,'127.0.0.1',r));
 const run=args=>new Promise(resolve=>{const p=spawn(process.execPath,[cli,...args],{env:{...process.env,HOME:home,ACQ_SERVER:`http://127.0.0.1:${srv.address().port}`}});let out='',err='';p.stdout.on('data',d=>out+=d);p.stderr.on('data',d=>err+=d);p.on('close',code=>resolve({code,out,err}));});
 try {
  for(const args of [['log'],['log','--self'],['status']]){
   const r=await run(args);assert.equal(r.code,0,r.err);assert.equal(delivered,0,'audit consumed server-side delivery');
   assert.equal(readFileSync(join(dir,'session.json'),'utf8'),session,'audit changed session');
   if(seeded)assert.equal(readFileSync(curs,'utf8'),before,'audit changed cursor');else assert.equal(existsSync(curs),false,'audit seeded legacy cursor');
   const q=queries.at(-1);assert.equal(q.searchParams.get('peek'),'1');assert.equal(q.searchParams.get('since'),'0');assert.equal(q.searchParams.get('wait'),'0');
   if(args[0]==='log'){assert.match(r.out,/pending synthetic peer/);assert.equal(r.out.includes('own synthetic message'),args.includes('--self'));if(ended)assert.match(r.out,/\(end\)/);}
   else assert.match(r.out,/peerJoined=true lastSeq=2/);
  }
  const r=await run(['recv','--wait','0']);assert.equal(r.code,0,r.err);assert.match(r.out,/pending synthetic peer/);assert.equal(JSON.parse(readFileSync(curs,'utf8')).FIXTURE,2);if(seeded)assert.equal(JSON.parse(readFileSync(curs,'utf8')).OTHER,12);
  const q=queries.at(-1);assert.equal(q.searchParams.has('since'),seeded);assert.equal(writes,0);assert.equal(queries.length,4);
  console.log(`PASS seeded=${seeded} ended=${ended}: three audit commands preserve pending peer, subsequent recv delivers`);
 }catch(e){failures++;console.error(`FAIL seeded=${seeded} ended=${ended}: ${e.message}`);}
 finally{srv.closeAllConnections();await new Promise(r=>srv.close(r));rmSync(home,{recursive:true,force:true});}
}
if(failures)process.exitCode=1;
