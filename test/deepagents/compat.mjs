// Instinct, acting on behalf of Tal Chaim Damri. Synthetic local compatibility only.
import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import {MCPAdapter} from '@langchain/mcp-adapters';
import {createDeepAgent} from 'deepagents';
import {BaseChatModel} from '@langchain/core/language_models/chat_models';
import {AIMessage} from '@langchain/core/messages';
const base=readFileSync(process.env.ACQ_LOCAL_READY,'utf8').trim();assert.match(base,/^http:\/\/127\.0\.0\.1:\d+$/);
// Reject accidental outbound HTTP by package or harness, even with no credentials.
const originalFetch=globalThis.fetch;let requests=0;
globalThis.fetch=async(u,o)=>{const target=typeof u==='string'?u:u instanceof URL?u.href:u.url;assert.ok(target.startsWith(base+'/'),`nonlocal fetch refused: ${new URL(target).origin}`);requests++;return originalFetch(u,o);};
const a=new MCPAdapter({servers:{acq:{transport:'http',url:base+'/mcp'}}});
const b=new MCPAdapter({servers:{acq:{transport:'http',url:base+'/mcp'}}});
try{
 const ta=await a.listTools(),tb=await b.listTools();assert.equal(ta.length,9);assert.equal(tb.length,9);console.log('PASS actual adapter discovery: nine tools in each independent client');
 const tool=(ts,n)=>{const t=ts.find(t=>t.name===n||t.name.endsWith(n));assert.ok(t,n);return t};
 const parse=r=>{if(typeof r==='string')return JSON.parse(r);let c=r.content??r;if(Array.isArray(c))return JSON.parse(c.filter(x=>x.type==='text').map(x=>x.text).join(''));throw new Error('unexpected tool response '+JSON.stringify(r));};
 const room=parse(await tool(ta,'acq_create_room').invoke({}));
 const uin=room.roomId;assert.ok(uin);const owner=room.token;assert.ok(owner);
 const guest=parse(await tool(tb,'acq_join_room').invoke({uin}));assert.ok(guest.token);assert.notEqual(owner,guest.token);
 await tool(ta,'acq_send').invoke({uin,token:owner,text:'synthetic independent owner',cid:'local-owner-1'});
 const recv=parse(await tool(tb,'acq_recv').invoke({uin,token:guest.token,wait:0,since:0,peek:true}));assert.ok(recv.messages.some(m=>m.text==='synthetic independent owner'));console.log('PASS real ACQ create/join/send/receive through separate official adapter clients');
 await tool(tb,'acq_send').invoke({uin,token:guest.token,text:'synthetic independent guest',cid:'local-guest-1'});
 // Deterministic local chat stub is not an adapter or server substitute. Actual DeepAgents
 // runtime dispatches one discovered remote tool; no provider or paid model is involved.
 const readTool=tool(ta,'acq_recv');
 class LocalScriptedChat extends BaseChatModel{
  n=0;constructor(){super({});} _llmType(){return 'local-scripted-no-provider';}bindTools(){return this;}
  async _generate(){const message=this.n++===0?new AIMessage({content:'',tool_calls:[{id:'synthetic-read',name:readTool.name,args:{uin,token:owner,wait:0,since:0,peek:true}}]}):new AIMessage('synthetic test complete');return {generations:[{text:typeof message.content==='string'?message.content:'',message}]};}
 }
 const agent=await createDeepAgent({model:new LocalScriptedChat(),tools:[readTool]});
 const result=await agent.invoke({messages:[{role:'user',content:'Run the one scripted synthetic read.'}]},{recursionLimit:8});
 const outputs=result.messages.filter(m=>m.getType?.()==='tool');assert.ok(outputs.length);assert.ok(JSON.stringify(outputs).includes('synthetic independent guest'));console.log('PASS actual DeepAgents graph dispatches discovered adapter tool and receives peer text with deterministic local chat stub');
 await tool(tb,'acq_send').invoke({uin,token:guest.token,text:'synthetic done',cid:'local-end-1',end:true});
 const final=parse(await tool(ta,'acq_recv').invoke({uin,token:owner,wait:0,since:0,peek:true}));assert.equal(final.ended,true);console.log('PASS local room ended and transcript readable; HTTP requests',requests);
}finally{await Promise.allSettled([a.close(),b.close()]);}
