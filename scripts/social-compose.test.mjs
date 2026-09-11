import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import vm from "node:vm";
import {webcrypto,createHash} from "node:crypto";

const source=fs.readFileSync("internal/webui/components.js","utf8");
const code=source.slice(source.indexOf("  // Social follows"),source.indexOf("  // Social end."));
const author="a".repeat(64),parentAuthor="b".repeat(64),root="c".repeat(64),parent="d".repeat(64),hint="wss://relay.test";
function setup(attrs={},values={},options={}) {
  const attributes={actor:author,relay:hint,...attrs},store=options.store||new Map(),calls=[],signed=[];
  let now=1789000000,fail=options.fail||0;
  class FormElement {getAttribute(n){return attributes[n]??null;}report(message){this.message=message;}}
  class HTMLElement {}
  const fields=Object.fromEntries(Object.entries(values).map(([name,value])=>[name, typeof value === "object" ? value : {value}]));
  const form={elements:fields,reset(){for(const field of Object.values(fields))if("value"in field)field.value="";}};
  const tiny={root:"",localPath:path=>path,sha256hex:async data=>createHash("sha256").update(data).digest("hex"),
    signing:{signEvent:async unsigned=>{signed.push(unsigned);return {...unsigned,id:"e".repeat(64),pubkey:author};}},
    signedFetch:async(path,method,body)=>{calls.push({path,method,body});if(path === "/social/preview") return {ok:true,json:async()=>({html:"<p>Preview</p>"})};if(path.startsWith("/upload")){return {ok:true,json:async()=>({sha256:await tiny.sha256hex(body),size:body.length,url:"https://relay.test/file.jpg"})};}if(fail-->0)return {ok:false,json:async()=>({error:"interrupted"})};return {ok:true,json:async()=>({accepted:true})};},
    navigate:async href=>{tiny.href=href;}
  };
  const ctx={FormElement,HTMLElement,tiny,URL,Uint8Array,WeakMap,crypto:webcrypto,location:{origin:"https://relay.test",pathname:"/social",href:"https://relay.test/social"},localStorage:{getItem:k=>store.get(k)||null,setItem:(k,v)=>store.set(k,v),removeItem:k=>store.delete(k)},isHex64:v=>typeof v==="string"&&/^[0-9a-f]{64}$/.test(v),unixNow:()=>now,el:()=>({setAttribute(){}}),fetch:options.fetch};
  const classes=vm.runInNewContext(code+"\n({SocialCompose,SocialReaction,socialDraftKey})",ctx);
  const component=new classes.SocialCompose();component.form=form;
  return {component,form,calls,signed,tiny,store,attrs:attributes,classes,advance(){now+=90;},tags(){return JSON.parse(JSON.stringify(signed.at(-1).tags));}};
}

test("notes keep plaintext and topics normalized",async()=>{
  const s=setup({mode:"note"},{content:"**literal**\nTwo lines",tags:"#Nostr, nostr, NEWS"});
  await s.component.submit(s.form);
  assert.equal(s.signed[0].kind,1);assert.equal(s.signed[0].content,"**literal**\nTwo lines");
  assert.deepEqual(s.tags(),[["t","nostr"],["t","news"]]);
});

test("new articles with identical titles have distinct persistent addresses",async()=>{
  const s=setup({mode:"article"},{title:"Same title",content:"# Markdown",summary:"Subtitle",cover:"https://cdn.test/a.png"});
  const other=setup({mode:"article"},{title:"Same title",content:"Another story"});
  await s.component.submit(s.form);await other.component.submit(other.form);
  const tags=s.tags();assert.notEqual(tags.find(t=>t[0]==="d")[1],other.tags().find(t=>t[0]==="d")[1]);
  assert.equal(s.signed[0].kind,30023);assert.ok(tags.some(t=>t[0]==="title"&&t[1]==="Same title"));assert.ok(tags.some(t=>t[0]==="published_at"));
});

test("NIP-10 top-level reply uses one root marker and inherited p tags",async()=>{
  const s=setup({mode:"comment",target:root,"target-kind":"1","target-pubkey":author,"target-tags":JSON.stringify([["p",parentAuthor]])},{content:"Reply"});
  await s.component.submit(s.form);
  assert.deepEqual(s.tags(),[["e",root,hint,"root",author],["p",author,hint],["p",parentAuthor,hint]]);
});

test("NIP-10 nested replies distinguish root and direct parent",async()=>{
  const s=setup({mode:"comment",target:parent,"target-kind":"1","target-pubkey":parentAuthor,root,"root-pubkey":author},{content:"Nested"});
  await s.component.submit(s.form);assert.deepEqual(s.tags(),[["e",root,hint,"root",author],["e",parent,hint,"reply",parentAuthor],["p",author,hint],["p",parentAuthor,hint]]);
});

test("NIP-22 article comment has address scope and address plus id parent",async()=>{
  const address=`30023:${author}:essay:part-one`;
  const s=setup({mode:"comment",target:root,"target-kind":"30023","target-pubkey":author,address},{content:"Article comment"});
  await s.component.submit(s.form);assert.equal(s.signed[0].kind,1111);
  assert.deepEqual(s.tags(),[["A",address,hint],["K","30023"],["P",author,hint],["a",address,hint],["e",root,hint,author],["k","30023"],["p",author,hint]]);
});

test("NIP-22 nested comment keeps root scope but direct parent's author and kind",async()=>{
  const address=`30023:${author}:essay`;
  const s=setup({mode:"comment",target:parent,"target-kind":"1111","target-pubkey":parentAuthor,root,"root-pubkey":author,address},{content:"Nested article comment"});
  await s.component.submit(s.form);assert.equal(s.signed[0].kind,1111);
  assert.deepEqual(s.tags(),[["A",address,hint],["K","30023"],["P",author,hint],["e",parent,hint,parentAuthor],["k","1111"],["p",parentAuthor,hint]]);
});

test("relay failure retains signed event across a later retry",async()=>{
  const s=setup({mode:"note"},{content:"Retry"},{fail:1});
  await assert.rejects(s.component.submit(s.form),/interrupted/);s.advance();await s.component.submit(s.form);
  assert.equal(s.signed.length,1);assert.equal(s.calls.length,2);assert.equal(s.calls[0].body,s.calls[1].body);
});

test("editing a failed post requires a fresh signature",async()=>{
  const s=setup({mode:"note"},{content:"Before"},{fail:1});
  await assert.rejects(s.component.submit(s.form));s.form.elements.content.value="After";await s.component.submit(s.form);assert.equal(s.signed.length,2);
});

test("duplicate submits publish only once",async()=>{
  const s=setup({mode:"note"},{content:"One"});await Promise.all([s.component.submit(s.form),s.component.submit(s.form)]);assert.equal(s.signed.length,1);assert.equal(s.calls.length,1);
});

test("drafts remain scoped to account and article and clear when erased",()=>{
  const s=setup({mode:"article"},{title:"Story",content:"private draft"});s.component.saveDraft();
  const other=setup({mode:"article",actor:parentAuthor},{title:"",content:""},{store:s.store});other.component.restoreDraft();assert.equal(other.form.elements.content.value,"");
  const same=setup({mode:"article"},{title:"",content:""},{store:s.store});same.component.restoreDraft();assert.equal(same.form.elements.content.value,"private draft");
  s.form.elements.title.value="";s.form.elements.content.value="";s.component.saveDraft();assert.equal(s.store.size,0);
});

test("media is uploaded once on retry and notes embed raw URLs with imeta",async()=>{
  const file={name:"photo.jpg",type:"image/jpeg",size:3,arrayBuffer:async()=>new Uint8Array([1,2,3]).buffer};
  const s=setup({mode:"note"},{content:"Photo",alt:"A mountain", "media-file":{files:[file]}},{fail:1});
  await assert.rejects(s.component.submit(s.form),/interrupted/);await s.component.submit(s.form);
  assert.equal(s.calls.filter(c=>c.path.startsWith("/upload")).length,1);
  assert.equal(s.signed[0].content,"Photo\n\nhttps://relay.test/file.jpg");
  const imeta=s.tags().find(t=>t[0]==="imeta");assert.ok(imeta.includes("m image/jpeg"));assert.ok(imeta.includes("alt A mountain"));assert.ok(imeta.includes("size 3"));
});

test("unsafe media and cover URLs are rejected before publishing",async()=>{
  for(const values of [{content:"note",media:"javascript:alert(1)"},{content:"note",media:"https://secret@example.com/a"}]){const s=setup({mode:"note"},values);await assert.rejects(s.component.submit(s.form));assert.equal(s.signed.length,0);}
  const s=setup({mode:"article"},{title:"Title",content:"Body",cover:"data:image/svg+xml,bad"});await assert.rejects(s.component.submit(s.form));assert.equal(s.signed.length,0);
});


test("draft previews use authenticated requests without publishing", async()=>{
  const s=setup({mode:"article"},{content:"**Preview**"});
  const output={hidden:true,innerHTML:""};s.component.querySelector=()=>output;
  await s.component.preview();
  assert.equal(s.calls[0].path,"/social/preview");assert.equal(s.calls[0].method,"POST");
  assert.deepEqual(JSON.parse(s.calls[0].body),{content:"**Preview**",kind:30023});
  assert.equal(s.signed.length,0);assert.equal(output.hidden,false);assert.equal(output.innerHTML,"<p>Preview</p>");
});

test("legacy replies omit an unknown optional root author rather than misattribute it", async()=>{
  const s=setup({mode:"comment",target:parent,"target-kind":"1","target-pubkey":parentAuthor,root,"root-pubkey":""},{content:"Legacy reply"});
  await s.component.submit(s.form);
  assert.deepEqual(s.tags(),[["e",root,hint,"root"],["e",parent,hint,"reply",parentAuthor],["p",parentAuthor,hint]]);
});
