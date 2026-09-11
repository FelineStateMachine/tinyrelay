#!/usr/bin/env node
// Disposable local corpus for Buzz-style replies, task cards and decisions.
import {finalizeEvent,getPublicKey} from 'nostr-tools/pure';
import {createHash} from 'node:crypto';
import {mkdir,writeFile} from 'node:fs/promises';
const base=process.env.TINY_QA_URL||'http://127.0.0.1:18468';
if(!['127.0.0.1','localhost'].includes(new URL(base).hostname)) throw Error('This fixture only runs locally.');
export const keys=Object.fromEntries(['owner','alice','agent'].map((name,i)=>[name,Uint8Array.from(Buffer.from((i+1).toString(16).padStart(64,'0'),'hex'))]));
const pub=Object.fromEntries(Object.entries(keys).map(([name,key])=>[name,getPublicKey(key)]));
export async function signed(path,method,value,who='owner') {
 const body=JSON.stringify(value),auth=finalizeEvent({kind:27235,created_at:Math.floor(Date.now()/1000),tags:[['u',base+path],['method',method],['payload',createHash('sha256').update(body).digest('hex')]],content:''},keys[who]);
 const response=await fetch(base+path,{method,headers:{'content-type':'application/json',authorization:'Nostr '+Buffer.from(JSON.stringify(auth)).toString('base64')},body});
 const result=await response.json();if(!response.ok||result.error||result.accepted===false&&!String(result.message).startsWith('duplicate:'))throw Error(JSON.stringify(result));return result;
}
export async function publish(who,kind,content,tags=[]) {const e=finalizeEvent({kind,content,tags,created_at:Math.floor(Date.now()/1000)},keys[who]);await signed('/events','POST',e,who);return e;}
if(process.argv[1]?.endsWith('buzz-chat-fixture.mjs')) {
 await mkdir('output/playwright/buzz',{recursive:true});
 for(const name of ['owner','alice']){await signed('/manage/rpc','POST',{method:'setmember',params:[pub[name],{role:'member'}]});await publish(name,0,JSON.stringify({name:name==='owner'?'Morgan':'Alice'}));}
 await publish('owner',30392,'',[['d',pub.agent],['p',pub.agent],['name','Build agent'],['room','work'],['k','0'],['k','9'],['k','11'],['k','12'],['k','7'],['k','1111'],['k','20001'],['k','20002'],['jobs','both'],['expiration',String(Math.floor(Date.now()/1000)+86400)]]);
 await publish('agent',0,JSON.stringify({name:'Build agent'}));
 await publish('owner',9007,'',[['h','work'],['name','Workshop'],['about','Agent tasks, decisions and chat'],['visibility','open']]);
 await publish('owner',9000,'',[['h','work'],['p',pub.agent,'member']]);
 const root=await publish('alice',9,'Could you prepare a preview?',[['h','work']]);
 const reply=await publish('owner',9,'Keep the existing color palette.',[['h','work'],['e',root.id,'','reply',pub.alice]]);
 await publish('alice',9,'And check the layout on mobile.',[['h','work'],['e',reply.id,'','reply',pub.owner]]);
 const task=await publish('owner',5000,'Prepare the site preview',[['h','work'],['e',root.id],['p',pub.agent],['subject','Prepare the site preview']]);
 await publish('agent',7000,'Desktop checks passed. Checking mobile navigation next.',[['h','work'],['e',task.id],['p',pub.owner],['status','processing','2 of 3 checks complete']]);
 const approval=await publish('agent',9,'Publish the preview with the existing palette and updated mobile navigation?',[['h','work'],['e',root.id,'','root'],['request','approve'],['p',pub.owner],['subject','Publish preview']]);
 const question=await publish('agent',9,'Which title should the preview use?',[['h','work'],['request','question'],['p',pub.owner],['subject','Preview title']]);
 await publish('owner',9,'The task card holds progress updates. Replies stay in their thread.',[['h','work']]);
 const fixture={base,pub,root:root.id,reply:reply.id,task:task.id,approval:approval.id,question:question.id};
 await writeFile('output/playwright/buzz/fixture.json',JSON.stringify(fixture,null,2));console.log(JSON.stringify(fixture));
}
