// Run with Playwright CLI on a disposable room page containing a thread link.
// Uses real link clicks, the bundled Fixi and morph, and browser history.
async page => {
  const roomURL = page.url().split("#")[0];
  if (!/\/rooms\/[a-z0-9_-]+$/.test(roomURL)) throw Error("Open a room with a thread before running this check.");
  const results = [], requests = [];
  const record = request => requests.push(request.url().split("?")[0]);
  page.on("request", record);
  const shell = async expected => {
    const state = await page.evaluate(() => ({
      headers: document.querySelectorAll("#topbar").length,
      contents: document.querySelectorAll("#content").length,
      sections: [...document.querySelectorAll("#content>section")].map(node => node.id),
      composers: document.querySelectorAll("room-compose").length
    }));
    if (state.headers !== 1 || state.contents !== 1 || state.sections.length !== 1 || state.sections[0] !== expected || state.composers > 1) throw Error(JSON.stringify(state));
    return state;
  };
  try {
    for (const width of [320, 390, 600]) {
      await page.setViewportSize({width, height:844});
      await page.goto(roomURL);
      await page.evaluate(() => { window.__threadNavigation = true; });
      await page.locator('#messages a[href*="/thread/"]').first().click();
      await page.waitForURL("**/thread/**");
      await shell("thread");
      if (!await page.evaluate(() => window.__threadNavigation)) throw Error("Thread navigation unexpectedly reloaded the page.");
      const back = page.locator("#mobile-back");
      const destination = await back.evaluate(node => node.href);
      if (!destination.includes("#msg-")) throw Error("Back link lost its message target.");
      await back.click();
      await page.waitForURL(destination);
      const returned = await shell("room");
      await page.locator("#mobile-back").click();
      await page.waitForURL("**/chat");
      const chat = await shell("chat");
      if (chat.composers !== 0) throw Error("Chat list kept the thread composer.");
      await page.goBack();
      await page.waitForFunction(() => document.querySelector("#content>section")?.id === "room");
      await shell("room");
      await page.goForward();
      await page.waitForFunction(() => document.querySelector("#content>section")?.id === "chat");
      await shell("chat");
      results.push({width, returned, history:true});
    }
    if (requests.some(path => /\/(?:null|undefined)$/.test(path))) throw Error("A stale handler requested an invalid navigation URL.");
    return results;
  } finally { page.off("request", record); }
}
