import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';
const source=fs.readFileSync('internal/webui/grant-request.js','utf8');
const id='a'.repeat(64),base='b'.repeat(64),agent='c'.repeat(64),operator='d'.repeat(64);
const review=()=>({request_id:id,base,agent,operator,before:{name:"Agent",scope:{kinds:[9]}},after:{scope:{kinds:[9,30617]}},unsigned:{kind:30392,pubkey:operator,created_at:100,tags:[['grant-request',id],['grant-base',base],['e',id]],content:''}});
function setup() {
 const published=[],buttons=[{hasAttribute:n=>n==='data-grant-approve'},{hasAttribute:()=>false}]; let Ctor;
 class Element {constructor(){this.isConnected=true;this.attrs={'data-review':JSON.stringify(review())};}getAttribute(n){return this.attrs[n]??null;}hasAttribute(n){return n in this.attrs;}querySelectorAll(){return buttons;}addEventListener(){} }
 const signer={getPublicKey:async()=>operator},tiny={signer:()=>signer,signing:{publish:async e=>published.push(e)},navigate:async()=>{}};
 vm.runInNewContext(source,{tiny,HTMLElement:Element,customElements:{define:(_,c)=>Ctor=c},AbortController,JSON,Date,Error,location:{href:'https://relay.test/approvals'}});
 const host=new Ctor();host.report=m=>host.message=m;host.connectedCallback();
 const fresh=review();fresh.unsigned.created_at=200;fresh.before={scope:{kinds:[9]},name:"Agent"};
 const item={id,type:'grant',state:'open',asker:agent,asked:[operator],grant:fresh};host.latest=async()=>({item});
 return {host,tiny,signer,buttons,published,item};
}
test('grant approval signs exactly one fresh replacement, not a reaction',async()=>{
 const {host,published}=setup();await host.decide(null,true);
 assert.equal(published.length,1);assert.equal(published[0].kind,30392);assert.equal(published[0].created_at,200);assert.deepEqual(Object.keys(published[0]),['kind','created_at','tags','content']);
 assert.equal(published[0].tags.find(t=>t[0]==='grant-base')[1],base);
});
test('denial signs one reaction to the agent even when grant is paused',async()=>{
 const {host,published,item}=setup();delete item.grant;item.grant_error='paused';await host.decide(null,false);
 assert.equal(published.length,1);assert.equal(published[0].kind,7);assert.equal(published[0].content,'-');assert.equal(published[0].tags.find(t=>t[0]==='p')[1],agent);
});
for(const scenario of ['base','scope','closed','actor','identity','disconnect']) test('rejects changed '+scenario+' before signing',async()=>{
 const {host,published,item,tiny}=setup();
 host.latest=async()=>{if(scenario==='base')item.grant.base='e'.repeat(64);if(scenario==='scope')item.grant.after.scope.kinds.push(7);if(scenario==='closed')item.state='answered';if(scenario==='actor')item.asked=['f'.repeat(64)];if(scenario==='identity')tiny.signer=()=>({getPublicKey:async()=>operator});if(scenario==='disconnect'){host.isConnected=false;host.disconnectedCallback();}return {item};};
 await host.decide(null,true);assert.equal(published.length,0);
});
test('double tap cannot publish an approval and denial together',async()=>{
 const {host,published,buttons,item}=setup();let resolve;host.latest=()=>new Promise(done=>resolve=done);
 const pending=host.decide(null,true);await new Promise(done=>setImmediate(done));assert.ok(buttons.every(b=>b.disabled));
 await host.decide(null,false);resolve({item});await pending;assert.equal(published.length,1);assert.equal(published[0].kind,30392);
});
