// Run with playwright-cli run-code --filename after opening the disposable UX relay.
async page => {
  const base = await page.evaluate(() => location.origin);
  const results = [], errors = [];
  const onError = error => errors.push(error.message);
  page.on('pageerror', onError);
  const check = (ok, message) => { if (!ok) throw Error(message); };
  const run = async (name, action) => {
    try { const detail = await action(); results.push({name, ok:true, detail}); }
    catch (error) { results.push({name, ok:false, error:error.message}); }
  };
  const clickNav = async name => {
    const link = page.locator('#rail').getByRole('link', {name, exact:true});
    const href = await link.getAttribute('href');
    await link.click();
    await page.waitForURL(href.startsWith('/') ? base+href : href);
    await page.waitForFunction(() => document.querySelector('#content')?.getAttribute('aria-busy') !== 'true');
  };
  const forms = () => page.evaluate(() => [...document.querySelectorAll('rpc-form,signed-form,publish-list')].map(e => ({tag:e.localName,method:e.getAttribute('method'),form:!!e.form?.isConnected,output:!!e.output?.isConnected,nested:!!e.querySelector('form form')})));
  await page.setViewportSize({width:1440,height:1000});
  await page.goto(base + '/repos');
  await page.evaluate(() => { window.__uxMarker = 'preserved'; });
  await run('repository navigation preserves document and ref filters', async () => {
    await page.getByRole('link', {name:'ux-edge-repo',exact:true}).click();
    await page.waitForURL('**/repo?**');
    await clickNav('/code');
    await page.locator('#ref-select').selectOption('refs/heads/feature/café');
    await page.locator('#panel form').getByRole('button', {name:'go',exact:true}).click();
    await page.waitForFunction(() => new URL(location.href).searchParams.get('ref') === 'refs/heads/feature/café');
    check(await page.evaluate(() => window.__uxMarker) === 'preserved', 'navigation reloaded the document');
    const links = await page.locator('#tree a').evaluateAll(nodes => nodes.map(n => ({name:n.textContent,href:n.href})));
    check(links.length > 0, 'tree links missing');
    await clickNav('/history');
    await clickNav('/activity');
    await clickNav('/code');
    await page.screenshot({path:'output/playwright/ux/repo-desktop.png'});
    return {links:links.length,url:page.url()};
  });
  await run('search GET submission and history', async () => {
    await clickNav('all repositories');
    await clickNav('/search');
    await page.locator('#search input[name=q]').fill('fixture');
    await page.locator('#search').getByRole('button', {name:'Search',exact:true}).click();
    await page.waitForFunction(() => new URL(location.href).searchParams.get('q') === 'fixture');
    const searched = page.url();
    await clickNav('/articles');
    await page.goBack();
    await page.waitForFunction(() => document.querySelector('#search input[name=q]')?.value === 'fixture');
    check(page.url() === searched, 'back URL incorrect');
    await page.goForward();
    await page.waitForFunction(() => document.querySelector('#content h1')?.textContent === 'Articles');
    check(await page.evaluate(() => window.__uxMarker) === 'preserved', 'filter or history reloaded document');
  });
  await run('repeated signed forms survive swaps', async () => {
    await clickNav('/manage');
    for (const name of ['/connect','/identity','/connect','/people','/connect']) {
      await clickNav(name);
      const current = await forms();
      check(current.length > 0 && current.every(x => x.form && x.output && !x.nested), 'detached or nested forms: '+JSON.stringify(current));
    }
    await page.waitForFunction(() => !!window.nostr?.signEvent);
    await page.waitForFunction(() => document.querySelector('connect-list #rows tbody tr[data-index]') !== null);
    check(await page.locator('connect-list select[name=template] option').count() > 1, 'connect list offers no catalog cards');
    await clickNav('/people');
    const list = page.locator('rpc-form[method=listmembers]');
    let submissions = 0;
    const count = request => { if (request.method()==='POST' && request.url().endsWith('/manage/rpc')) submissions++; };
    page.on('request', count);
    try {
      await list.evaluate(node => { node.form.requestSubmit(); node.form.requestSubmit(); });
      await page.waitForFunction(() => document.querySelector('rpc-form[method=listmembers] output')?.textContent === 'Done.');
      check(submissions===1, 'duplicate form submissions sent '+submissions+' signed requests');
    } finally { page.off('request',count); }
    await clickNav('/connect');
    await page.waitForFunction(() => document.querySelector('connect-list #rows tbody tr[data-index]') !== null);
    const result = await forms();
    await page.screenshot({path:'output/playwright/ux/connect-desktop.png'});
    return result;
  });
  await run('job status stream closes when leaving the page', async () => {
    await clickNav('/sync');
    await page.locator('#watch-jobs').click();
    await page.waitForFunction(() => {
      const requests = document.querySelector('#watch-jobs').__fixi?.requests;
      const config = requests && [...requests].find(cfg => cfg.sse);
      if (config) { window.__uxStream = config.sse; return true; }
      return false;
    });
    await clickNav('/connect');
    check(await page.evaluate(() => window.__uxStream.closed), 'job stream remained open after navigation');
  });
  await run('file viewers preserve one content section and media bounds', async () => {
    await page.goto(base+'/files');
    const links = await page.locator('#rows a').evaluateAll(nodes => nodes.map(n => n.href));
    check(links.length >= 4, 'fixture files missing');
    const visited=[];
    for (const href of links.slice(0,4)) {
      await page.goto(href);
      const info = await page.evaluate(() => ({sections:document.querySelectorAll('#content>section').length,children:[...document.querySelector('#content').children].map(e=>e.localName),overflow:document.documentElement.scrollWidth>innerWidth,raw:document.querySelector('#facts a')?.getAttribute('fx-action'),forms:[...document.querySelectorAll('rpc-form,signed-form')].every(e=>e.form?.isConnected)}));
      check(info.sections===1 && info.children.length===1, 'file viewer has content outside section: '+JSON.stringify(info));
      check(!info.raw, 'download intercepted by fixi');
      check(info.forms, 'file forms disconnected');
      visited.push(info);
    }
    await page.setViewportSize({width:360,height:800});
    const mobile = await page.evaluate(() => ({width:innerWidth,scroll:document.documentElement.scrollWidth,content:document.querySelector('#content').scrollWidth}));
    check(mobile.scroll<=mobile.width && mobile.content<=mobile.width, 'mobile file page overflows: '+JSON.stringify(mobile));
    await page.screenshot({path:'output/playwright/ux/file-mobile.png'});
    return {visited,mobile};
  });
  await run('mobile menu and theme survive navigation', async () => {
    await page.goto(base+'/');
    await page.setViewportSize({width:360,height:800});
    await page.locator('#menu').click();
    check(await page.locator('#nav-menu').evaluate(e=>e.open),'menu did not open');
    await clickNav('/articles');
    check(!await page.locator('#nav-menu').evaluate(e=>e.open),'navigation did not close menu');
    await page.locator('#menu').click();
    check(await page.locator('#nav-menu').evaluate(e=>e.open),'morphed menu did not open');
    await page.keyboard.press('Escape');
    check(!await page.locator('#nav-menu').evaluate(e=>e.open),'Escape did not close menu');
    const before = await page.locator('html').getAttribute('data-theme');
    await page.locator('#theme').click();
    check(await page.locator('html').getAttribute('data-theme') !== before,'morphed theme button inactive');
    await page.setViewportSize({width:1440,height:1000});
    check(await page.locator('#rail').getAttribute('aria-hidden')!=='true','desktop rail inaccessible');
  });
  await run('no JavaScript mobile content and navigation', async () => {
    const context=await page.context().browser().newContext({javaScriptEnabled:false,viewport:{width:360,height:800}});
    try {
      const noJS=await context.newPage();
      await noJS.goto(base+'/repos');
      check(await noJS.locator('#menu').isVisible(),'no-JS menu missing');
      check(!await noJS.locator('#rail').isVisible(),'no-JS navigation starts expanded');
      await noJS.locator('#menu').click();
      const deadline=Date.now()+2000;
      while ((await noJS.locator('#rail').boundingBox()).x<0 && Date.now()<deadline) await noJS.waitForTimeout(25);
      check((await noJS.locator('#rail').boundingBox()).x===0,'no-JS menu did not finish opening');
      check(await noJS.locator('#rail').isVisible(),'no-JS menu did not open');
      await noJS.locator('#rail').getByRole('link',{name:'/articles',exact:true}).click();
      check(noJS.url()===base+'/articles','no-JS rail link inaccessible');
      check(await noJS.locator('#content h1').first().textContent()==='Articles','no-JS articles missing');
      check(!await noJS.locator('#rail').isVisible(),'no-JS navigation did not reset closed');
      check((await noJS.locator('#content').boundingBox()).y<80,'no-JS navigation pushes content down');
      await noJS.screenshot({path:'output/playwright/ux/nojs-mobile.png'});
    } finally {await context.close();}
  });
  page.off('pageerror',onError);
  results.push({name:'uncaught browser errors',ok:errors.length===0,errors});
  return {passed:results.filter(x=>x.ok).length,failed:results.filter(x=>!x.ok).length,results};
}
