// Instinct, acting on behalf of Tal Chaim Damri.
// Synthetic loopback deadline regression. Accelerate constants only in a private
// copy, assert their production values, and bound every spawned process.
import test from 'node:test';
import assert from 'node:assert/strict';
import {createServer} from 'node:http';
import {spawn} from 'node:child_process';
import {readFile,writeFile,mkdtemp,mkdir,rm} from 'node:fs/promises';
import {tmpdir} from 'node:os';
import {join} from 'node:path';

const original=await readFile(new URL('./acq.js',import.meta.url),'utf8');
assert.equal(original.split('const REQUEST_BUDGET_MS = 30000;').length,2);
assert.equal(original.split('const STREAM_IDLE_MS = 60000;').length,2);
const accelerated=original.replace('const REQUEST_BUDGET_MS = 30000;','const REQUEST_BUDGET_MS = 150;').replace('const STREAM_IDLE_MS = 60000;','const STREAM_IDLE_MS = 300;');

async function fixture(run){
 const dir=await mkdtemp(join(tmpdir(),'acq-deadline-'));
 const sockets=new Set(),timers=new Set(),children=new Set();
 const state={posts:0,gets:0,streams:0,cid:null};
 let handler;
 const server=createServer((req,res)=>handler(req,res));
 server.on('connection',s=>{sockets.add(s);s.on('close',()=>sockets.delete(s));});
 const later=(f,ms)=>{const t=setTimeout(f,ms);timers.add(t);return t;};
 const json=(res,data)=>{res.writeHead(200,{'content-type':'application/json'});res.end(JSON.stringify(data));};
 const response=messages=>({messages,cursor:messages.length,peerJoined:true,ended:false,expiresAt:0});
 const stall=res=>{res.writeHead(200,{'content-type':'application/json'});res.write('{');const tick=()=>{if(!res.destroyed){res.write(' ');later(tick,50);}};later(tick,50);};
 try{
  await writeFile(join(dir,'cli.mjs'),accelerated);
  await mkdir(join(dir,'.config','acq'),{recursive:true});
  await writeFile(join(dir,'.config','acq','session.json'),JSON.stringify({room:'SYNTHETIC',token:'synthetic-token',seat:'A'}));
  await writeFile(join(dir,'.config','acq','cursors.json'),JSON.stringify({SYNTHETIC:0}));
  await new Promise(r=>server.listen(0,'127.0.0.1',r));
  async function cli(args){
   const child=spawn(process.execPath,[join(dir,'cli.mjs'),...args],{env:{PATH:process.env.PATH,HOME:dir,ACQ_SERVER:`http://127.0.0.1:${server.address().port}`},stdio:['ignore','pipe','pipe']});children.add(child);
   let out='',err='';child.stdout.on('data',c=>out+=c);child.stderr.on('data',c=>err+=c);
   const start=Date.now();
   const bound=setTimeout(()=>child.kill('SIGKILL'),9000);
   try{return await new Promise((resolve,reject)=>{child.once('error',reject);child.once('close',(code,signal)=>resolve({code,signal,out,err,elapsed:Date.now()-start}));});}
   finally{clearTimeout(bound);children.delete(child);}
  }
  await run({state,setHandler:f=>handler=f,json,response,stall,later,cli,dir});
 }finally{
  for(const t of timers)clearTimeout(t);
  for(const c of children)c.kill('SIGKILL');
  await Promise.all([...children].map(c=>new Promise(r=>c.once('close',r))));
  for(const s of sockets)s.destroy();
  await new Promise(r=>server.close(r));await rm(dir,{recursive:true,force:true});
 }
}

test('stalled committed send body reaches cid settlement without resend',{timeout:10000},async()=>fixture(async({state,setHandler,json,response,stall,cli})=>{
 setHandler(async(req,res)=>{
  if(req.method==='POST'){state.posts++;let raw='';for await(const c of req)raw+=c;state.cid=JSON.parse(raw).cid;stall(res);}
  else{state.gets++;json(res,response([{seq:1,from:'A',cid:state.cid,text:'synthetic'}]));}
 });
 const r=await cli(['send','synthetic']);assert.equal(r.code,0);assert.match(r.out,/verified after an ambiguous failure/);assert.equal(state.posts,1);assert.equal(state.gets,1);assert.ok(r.elapsed<2500);
}));

test('stalled send and all verification bodies end as unknown, never resend',{timeout:10000},async()=>fixture(async({state,setHandler,stall,cli})=>{
 setHandler(async(req,res)=>{if(req.method==='POST'){state.posts++;for await(const _ of req){};}else state.gets++;stall(res);});
 const r=await cli(['send','synthetic']);assert.equal(r.code,1);assert.match(r.err,/send outcome unknown/);assert.doesNotMatch(r.out,/sent seq=/);assert.equal(state.posts,1);assert.equal(state.gets,3);assert.ok(r.elapsed<7500);
}));

test('missing response headers are bounded for read-only command',{timeout:10000},async()=>fixture(async({setHandler,cli})=>{
 setHandler(()=>{});const r=await cli(['status']);assert.equal(r.code,1);assert.ok(r.elapsed<2000);
}));

test('stalled create body remains unconfirmed, no replacement saved',{timeout:10000},async()=>fixture(async({state,setHandler,stall,cli,dir})=>{
 const path=join(dir,'.config','acq','session.json'),before=await readFile(path,'utf8');
 setHandler(async(req,res)=>{state.posts++;for await(const _ of req){}stall(res);});
 const r=await cli(['new','--replace']);assert.equal(r.code,1);assert.match(r.err,/unconfirmed|unknown|may have/i);assert.equal(state.posts,1);assert.equal(await readFile(path,'utf8'),before);
}));

test('stalled join body remains unconfirmed and does not save a seat',{timeout:10000},async()=>fixture(async({state,setHandler,stall,cli,dir})=>{
 const path=join(dir,'.config','acq','session.json'),before=await readFile(path,'utf8');
 setHandler(async(req,res)=>{state.posts++;for await(const _ of req){}stall(res);});
 const r=await cli(['join','OTHER','--replace']);assert.equal(r.code,1);assert.match(r.err,/unconfirmed|unknown|may have/i);assert.equal(state.posts,1);assert.equal(await readFile(path,'utf8'),before);
}));

test('stalled receive body preserves replay cursor',{timeout:10000},async()=>fixture(async({setHandler,stall,cli,dir})=>{
 const path=join(dir,'.config','acq','cursors.json'),before=await readFile(path,'utf8');
 setHandler((req,res)=>stall(res));const r=await cli(['recv','--wait','0']);assert.equal(r.code,1);assert.match(r.err,/nothing was marked read locally/);assert.equal(await readFile(path,'utf8'),before);assert.equal(r.out,'');
}));

test('recv wait120 is not cut off at ordinary request budget',{timeout:10000},async()=>fixture(async({setHandler,json,response,later,cli,dir})=>{
 setHandler((req,res)=>{assert.equal(new URL(req.url,'http://local').searchParams.get('wait'),'120');later(()=>json(res,response([{seq:1,from:'B',text:'delayed synthetic'}])),400);});
 const r=await cli(['recv','--wait','120']);assert.equal(r.code,0);assert.match(r.out,/delayed synthetic/);assert.equal(JSON.parse(await readFile(join(dir,'.config','acq','cursors.json'),'utf8')).SYNTHETIC,1);
}));

test('watch heartbeats renew idle budget and preserve intentional parked wait',{timeout:10000},async()=>fixture(async({state,setHandler,json,response,later,cli})=>{
 setHandler((req,res)=>{
  if(req.url.includes('/stream')){state.streams++;res.writeHead(200,{'content-type':'text/event-stream'});res.flushHeaders();for(const ms of [75,150,225,300,375,450])later(()=>res.write(': ping\n\n'),ms);later(()=>res.end('event: peer_joined\ndata: {}\n\n'),500);}
  else{state.gets++;json(res,response([]));}
 });
 const r=await cli(['watch']);assert.equal(r.code,0);assert.match(r.out,/peer joined/);assert.equal(state.streams,1);assert.ok(r.elapsed>=500);
}));

test('watch silent stream reconnects with retry floor, catch-up stays replay-safe',{timeout:10000},async()=>fixture(async({state,setHandler,json,response,cli})=>{
 setHandler((req,res)=>{
  if(req.url.includes('/stream')){state.streams++;res.writeHead(200,{'content-type':'text/event-stream'});res.flushHeaders();}
  else{state.gets++;assert.equal(new URL(req.url,'http://local').searchParams.get('since'),'0');json(res,response(state.gets>1?[{seq:1,from:'B',text:'after silence'}]:[]));}
 });
 const r=await cli(['watch']);assert.equal(r.code,0);assert.match(r.out,/after silence/);assert.equal(state.streams,1);assert.equal(state.gets,2);assert.ok(r.elapsed>=5000);assert.ok(r.elapsed<8500);
}));
