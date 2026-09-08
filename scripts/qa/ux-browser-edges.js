// Run with playwright-cli run-code --filename after opening the disposable UX relay.
async page => {
  const base = await page.evaluate(()=>location.origin);
  const context = await page.context().browser().newContext({serviceWorkers:'block',viewport:{width:1280,height:900}});
  const probe=await context.newPage(), results=[], errors=[];
  probe.on('pageerror',error=>errors.push(error.message));
  const check=(ok,message)=>{if(!ok)throw Error(message);};
  const run=async(name,action)=>{try{results.push({name,ok:true,detail:await action()});}catch(error){results.push({name,ok:false,error:error.message});}};
  try {
    await probe.goto(base+'/');
    await probe.evaluate(()=>window.__uxMarker='same');
    await run('network and HTTP failures leave the current page usable',async()=>{
      await probe.route('**/articles',route=>route.fulfill({status:503,body:'Temporary failure'}));
      await probe.locator('#rail').getByRole('link',{name:'/articles',exact:true}).click();
      await probe.waitForFunction(()=>document.querySelector('#navigation-status')?.textContent.includes('Unable'));
      check(probe.url()===base+'/','HTTP error changed the URL');
      await probe.unroute('**/articles');
      await probe.route('**/articles',route=>route.abort());
      await probe.locator('#rail').getByRole('link',{name:'/articles',exact:true}).click();
      await probe.waitForFunction(()=>document.querySelector('#navigation-status')?.textContent.includes('Unable'));
      check(await probe.locator('#content').getAttribute('aria-busy')!=='true','network error leaves busy state');
      await probe.unroute('**/articles');
    });
    await run('slow earlier navigation cannot overwrite the latest click',async()=>{
      let finish;
      const gate=new Promise(resolve=>{finish=resolve;});
      await probe.route('**/articles',async route=>{await gate;await route.continue().catch(()=>{});});
      await probe.locator('#rail').getByRole('link',{name:'/articles',exact:true}).click();
      await probe.locator('#rail').getByRole('link',{name:'/repos',exact:true}).click();
      await probe.waitForURL(base+'/repos');
      finish();
      await probe.unroute('**/articles');
      check(await probe.locator('#content h1').textContent()==='Repositories','stale response replaced content');
    });
    await run('sign-in errors use current status after a shell swap',async()=>{
      await probe.locator('#who').getByRole('link',{name:'Sign in',exact:true}).click();
      await probe.waitForURL(base+'/signin');
      await probe.locator('#session-login').click();
      await probe.waitForFunction(()=>document.querySelector('#signer-status')?.textContent.includes('Connect a Nostr signer first'));
      await probe.locator('#bunker-url').fill('https://example.invalid');
      await probe.locator('#bunker button').click();
      await probe.waitForFunction(()=>document.querySelector('#signer-status')?.textContent.includes('valid bunker URL'));
      check(await probe.evaluate(()=>window.__uxMarker)==='same','sign-in navigation reloaded');
    });
    await run('modified clicks retain native new-tab behavior',async()=>{
      const newTab=context.waitForEvent('page');
      await probe.locator('#rail').getByRole('link',{name:'/repos',exact:true}).click({modifiers:['ControlOrMeta']});
      const opened=await newTab;
      await opened.waitForLoadState();
      check(probe.url()===base+'/signin','modified click navigated the current page');
      check(opened.url()===base+'/repos','modified click opened wrong URL');
      await opened.close();
    });
    await run('seeded sequence of 60 read-only navigation actions',async()=>{
      let seed=0x74696e79;
      const routes=['/home','/search','/repos','/files','/sites','/articles','/inbox','/outbox'];
      for(let i=0;i<60;i++){
        seed=(Math.imul(seed,1664525)+1013904223)>>>0;
        const name=routes[(seed>>>16)%routes.length], path=name==='/home'?'/':name;
        await probe.locator('#rail').getByRole('link',{name,exact:true}).click();
        await probe.waitForURL(base+path);
        await probe.waitForFunction(()=>document.querySelector('#content')?.getAttribute('aria-busy')!=='true');
        const state=await probe.evaluate(()=>({marker:window.__uxMarker,ids:[...document.querySelectorAll('[id]')].map(e=>e.id),classes:document.querySelectorAll('[class]').length,sections:document.querySelectorAll('#content>section').length}));
        check(state.marker==='same','document reloaded at '+i);
        check(state.classes===0,'class attributes introduced at '+i);
        check(new Set(state.ids).size===state.ids.length,'duplicate DOM IDs at '+i);
        check(state.sections===1,'content section missing at '+i);
      }
      return {seed:'0x74696e79',actions:60};
    });
  } finally { await context.close(); }
  results.push({name:'uncaught browser errors',ok:errors.length===0,errors});
  return {passed:results.filter(r=>r.ok).length,failed:results.filter(r=>!r.ok).length,results};
}
