// Run after signing in to the disposable UX relay as its fixture owner.
async page => {
  const base = await page.evaluate(() => location.origin);
  const storageState = await page.context().storageState();
  const results = [], errors = [];
  const check = (ok, message) => { if (!ok) throw Error(message); };
  const run = async (name, action) => {
    try { results.push({name, ok:true, detail:await action()}); }
    catch (error) { results.push({name, ok:false, error:error.message}); }
  };
  await page.goto(base+'/manage/connect');
  check(await page.locator('#session-logout').count() === 1, 'Sign in as the fixture owner before running menu checks.');
  const routes = ['/', '/search', '/repos', '/files', '/sites', '/articles', '/inbox', '/outbox', '/signin', '/terms', '/tools', '/private', '/chat', '/media', '/marmot', '/grasp'];
  routes.push(...await page.locator('#rail nav a').evaluateAll(nodes => nodes.map(e=>e.getAttribute('href'))));
  await page.goto(base+'/repos');
  const repo = await page.getByRole('link', {name:'ux-edge-repo', exact:true}).getAttribute('href');
  await page.goto(base+repo);
  routes.push(...await page.locator('#rail nav a').evaluateAll(nodes => nodes.map(e=>e.getAttribute('href'))));
  await page.goto(base+'/files');
  routes.push(...await page.locator('#rows a').evaluateAll(nodes => nodes.map(e=>e.getAttribute('href'))));
  await page.goto(base+'/articles');
  routes.push(await page.locator('#content a[href^="/e/"]').first().getAttribute('href'));
  await page.goto(base+'/');
  routes.push(await page.locator('#content a[href^="/e/"]').last().getAttribute('href'));
  routes.push('/a/ux-long-article', '/r/main/articles', '/r/main/manage/connect');
  for (const javaScriptEnabled of [true, false]) {
    const context = await page.context().browser().newContext({javaScriptEnabled, storageState, viewport:{width:360,height:800}});
    try {
      const probe = await context.newPage();
      probe.on('pageerror', error=>errors.push(error.message));
      const waitForMenu = async () => {
        // Poll outside the page because disabled JavaScript also disables browser timers.
        const deadline = Date.now()+2000;
        while ((await probe.locator('#rail').boundingBox()).x<0 && Date.now()<deadline) await probe.waitForTimeout(25);
        check((await probe.locator('#rail').boundingBox()).x===0,'menu did not finish opening');
      };
      for (const path of [...new Set(routes)]) await run(`${javaScriptEnabled?'JS':'no JS'} ${path}`, async () => {
        await probe.setViewportSize({width:360,height:800});
        const response = await probe.goto(base+path);
        check(response.ok(), 'HTTP '+response.status());
        check(await probe.locator('#menu').isVisible(), 'menu control missing');
        check(!await probe.locator('#rail').isVisible(), 'navigation starts expanded');
        check(await probe.locator('#rail nav').count()===1, 'navigation duplicated');
        const before = await probe.locator('#content').boundingBox();
        check(before.y<80, 'navigation pushes content down');
        await probe.locator('#menu').click();
        await waitForMenu();
        check(await probe.locator('#rail').isVisible(), 'menu did not open');
        check(await probe.locator('#rail nav a').first().isVisible(), 'menu links hidden');
        check((await probe.locator('#content').boundingBox()).y===before.y, 'opening menu moves content');
        await probe.locator('#menu').click();
        check(!await probe.locator('#rail').isVisible(), 'menu did not close');
        await probe.locator('#menu').focus();
        await probe.keyboard.press('Enter');
        await waitForMenu();
        check(await probe.locator('#rail').isVisible(), 'Enter did not open menu');
        await probe.keyboard.press('Space');
        check(!await probe.locator('#rail').isVisible(), 'Space did not close menu');
        await probe.setViewportSize({width:1440,height:1000});
        check(await probe.locator('#rail').isVisible(), 'desktop sidebar missing');
        check(!await probe.locator('#menu').isVisible(), 'mobile menu appears on desktop');
        return {title:await probe.title(),contentTop:before.y};
      });
      await run(`${javaScriptEnabled?'JS':'no JS'} navigation resets the menu`, async () => {
        await probe.setViewportSize({width:360,height:800});
        await probe.goto(base+'/repos');
        if(javaScriptEnabled) await probe.evaluate(()=>{window.__menuDocument='preserved';});
        for(const label of ['/articles','/files','/repos']) {
          await probe.locator('#menu').click();
          await waitForMenu();
          await probe.locator('#rail').getByRole('link',{name:label,exact:true}).click();
          await probe.waitForURL(base+label);
          await probe.waitForFunction(()=>document.querySelector('#content').getAttribute('aria-busy')!=='true');
          check(!await probe.locator('#rail').isVisible(),'navigation left menu open');
        }
        if(javaScriptEnabled) {
          check(await probe.evaluate(()=>window.__menuDocument)==='preserved','navigation reloaded document');
          await probe.locator('#menu').click();
          await probe.keyboard.press('Escape');
          check(!await probe.locator('#rail').isVisible(),'Escape did not close menu');
          check(await probe.locator('#menu').evaluate(e=>e===document.activeElement),'Escape did not restore menu focus');
        }
      });
      if(!javaScriptEnabled) {
        await context.clearCookies();
        await probe.goto(base+'/articles');
        await probe.screenshot({path:'output/playwright/ux/nojs-mobile.png'});
        await probe.locator('#menu').click();
        await waitForMenu();
        await probe.screenshot({path:'output/playwright/ux/nojs-mobile-menu-open.png'});
      }
    } finally {await context.close();}
  }
  results.push({name:'uncaught browser errors',ok:errors.length===0,errors});
  return {passed:results.filter(x=>x.ok).length,failed:results.filter(x=>!x.ok).length,results};
}
