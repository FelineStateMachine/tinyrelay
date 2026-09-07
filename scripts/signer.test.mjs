import assert from "node:assert/strict";
import {webcrypto} from "node:crypto";
import {readFile} from "node:fs/promises";
import test from "node:test";
import vm from "node:vm";

const source=await readFile(new URL("../internal/webui/webui.go",import.meta.url),"utf8");
const bridge=source.split('const signerBridgeJS = `')[1].split('`')[0];

async function page({signedIn=false}={}) {
  const elements=new Map();
  for(const id of ["session-login","nostrconnect","nostrconnect-open","signer-qr","signer-status",...(signedIn?["session-logout"]:[])]) {
    elements.set(id,{hidden:true,addEventListener(event,handler){this[event]=handler;}});
  }
  const saved=new Map();
  const requests=[];
  const signer={bp:{pubkey:"a".repeat(64),relays:["wss://tiny.example/r/work"]},getPublicKey:async()=>"a".repeat(64),signEvent:async event=>({...event,pubkey:"a".repeat(64),sig:"test"})};
  const sandbox={document:{getElementById:id=>elements.get(id)},crypto:webcrypto,TextEncoder,URL,Uint8Array,ArrayBuffer,
    btoa:value=>Buffer.from(value).toString("base64"),
    location:{pathname:"/r/work/signin",href:"https://tiny.example/r/work/signin",origin:"https://tiny.example",assign(url){sandbox.opened=url},reload(){sandbox.reloaded=true}},
    navigator:{clipboard:{writeText:async()=>{}}},
    sessionStorage:{getItem:key=>saved.get(key),setItem:(key,value)=>saved.set(key,value),removeItem:key=>saved.delete(key)},
    NostrSigner:{generateSecretKey:()=>new Uint8Array(32),getPublicKey:()=>"b".repeat(64),createNostrConnectURI:()=>"nostrconnect://test",bytesToHex:()=>"test-key",BunkerSigner:{fromURI:async()=>signer}},
    fetch:async(url,options)=>{requests.push({url,options});return new Response("{}")}
  };
  sandbox.window=sandbox;
  vm.runInNewContext(bridge,sandbox);
  return {elements,saved,requests,sandbox};
}

test("approving a phone signer signs in and opens the tenant homepage",async()=>{
  const {elements,saved,requests,sandbox}=await page();
  await elements.get("nostrconnect").click({preventDefault(){}});
  assert.equal(requests.length,1);
  assert.equal(requests[0].url,"/r/work/session");
  assert.ok(requests[0].options.headers.authorization.startsWith("Nostr "));
  assert.equal(sandbox.opened,"/r/work/");
  assert.equal(saved.get("tiny.bunker/r/work.identity"),"a".repeat(64));
});

test("sign out revokes the session without requiring a connected phone",async()=>{
  const {elements,requests,sandbox}=await page({signedIn:true});
  await elements.get("session-logout").click();
  assert.equal(requests[0].url,"/r/work/session/logout");
  assert.equal(requests[0].options.credentials,"same-origin");
  assert.equal(sandbox.reloaded,true);
});
