// Instinct, acting on behalf of Tal Chaim Damri.
// Offline participant-facing regression. No external requests or real rooms.
import test from 'node:test';
import assert from 'node:assert/strict';
import {createServer} from 'node:http';
import {spawn} from 'node:child_process';
import {mkdtemp,rm,access} from 'node:fs/promises';
import {tmpdir} from 'node:os';
import {join} from 'node:path';
import {fileURLToPath} from 'node:url';
const cli=fileURLToPath(new URL(process.env.ACQ_CANDIDATE || './acq.js',import.meta.url));

for (const firstCommitted of [true,false]) {
 test(`lost join reply, seat held by ${firstCommitted?'original participant':'different participant'}: never infer identity`,{timeout:8000},async()=>{
  const home=await mkdtemp(join(tmpdir(),'acq-identity-'));
  let posts=0,holder=null;
  const sockets=new Set();
  const server=createServer(async(req,res)=>{
   assert.equal(req.method,'POST');assert.equal(req.url,'/room/SYNTHETIC/join');
   for await (const _ of req) {};
   posts++;
   if(posts===1){
    // Both histories are possible after a connection closes without a response.
    // In one the initial request committed. In the other a different holder
    // claimed the free seat before this participant tried again.
    holder=firstCommitted?'original':'different';
    req.socket.destroy();return;
   }
   res.writeHead(409,{'content-type':'application/json'});
   res.end(JSON.stringify({detail:'room full'}));
  });
  server.on('connection',s=>{sockets.add(s);s.on('close',()=>sockets.delete(s));});
  await new Promise(r=>server.listen(0,'127.0.0.1',r));
  async function run(){
   const child=spawn(process.execPath,[cli,'join','SYNTHETIC'],{env:{PATH:process.env.PATH,HOME:home,ACQ_SERVER:`http://127.0.0.1:${server.address().port}`},stdio:['ignore','pipe','pipe']});
   let out='',err='';child.stdout.on('data',c=>out+=c);child.stderr.on('data',c=>err+=c);
   const timer=setTimeout(()=>child.kill('SIGKILL'),2500);
   try{return await new Promise((resolve,reject)=>{child.once('error',reject);child.once('close',(code,signal)=>resolve({code,signal,out,err}));});}
   finally{clearTimeout(timer);}
  }
  try{
   const first=await run();assert.equal(first.code,1);assert.equal(first.signal,null);assert.equal(posts,1,'must not silently retry');assert.equal(first.out,'');
   assert.match(first.err,/join outcome unknown/);
   assert.doesNotMatch(first.err,/your earlier join committed/,'a later 409 is not proof of seat identity');
   assert.match(first.err,/another participant/);
   assert.match(first.err,/creator/);
   const second=await run();assert.equal(second.code,1);assert.equal(second.signal,null);assert.match(second.err,/409: room full/);assert.equal(posts,2);assert.equal(holder,firstCommitted?'original':'different');
   await assert.rejects(access(join(home,'.config','acq','session.json')),'neither response supplied a usable credential');
  }finally{for(const s of sockets)s.destroy();await new Promise(r=>server.close(r));await rm(home,{recursive:true,force:true});}
 });
}
