// Run with playwright-cli run-code after signing in as the fixture owner:
//   ... run-code --filename scripts/qa/room-attachments-browser.js
// The fixture is created by room-attachments-fixture.mjs.
async page => {
  const results = [], errors = [];
  const check = (condition, message) => { if (!condition) throw Error(message); };
  const run = async (name, action) => { try { results.push({name, ok: true, detail: await action()}); } catch (error) { results.push({name, ok: false, error: error.message}); } };
  page.on("pageerror", error => errors.push(error.message));
  const base = await page.evaluate(() => location.origin);
  const room = "/rooms/attachments-qa";
  await page.setViewportSize({width: 1440, height: 900});

  await run("SSR room shows image, generic file and agent message", async () => {
    await page.goto(base + room);
    await page.waitForSelector("#messages room-message");
    await page.waitForFunction(() => typeof window.tiny?.signer === "function" && typeof window.tiny.signer()?.signEvent === "function");
    const info = await page.evaluate(() => ({
      messages: document.querySelectorAll("#messages room-message").length,
      images: document.querySelectorAll("#messages img").length,
      links: [...document.querySelectorAll("#messages a")].map(a => a.textContent.trim()),
      videos: document.querySelectorAll("#messages video").length,
      agent: [...document.querySelectorAll("#messages room-message")].filter(node => node.textContent.includes("Agent published file")).length,
      live: !!document.querySelector("room-live")
    }));
    check(info.images >= 1, "initial image was not rendered inline");
    check(info.links.some(label => label.includes("fixture notes.txt")), "generic file link missing");
    check(info.agent >= 1, "agent message missing from room timeline");
    check(info.live, "room live stream missing");
    const liveText = "Live agent attachment";
    await page.evaluate(async text => {
      const url = document.querySelector("#messages img")?.src;
      const unsigned = {kind: 9, created_at: Math.floor(Date.now() / 1000), tags: [["h", "attachments-qa"], ["imeta", "url " + url, "m image/png"]], content: text + "\n\n![image](" + url + ")"};
      const signer = window.tiny.signer();
      if (!signer?.signEvent) throw Error("connected signer unavailable for live check");
      const signed = await signer.signEvent(unsigned);
      const response = await window.tiny.signedFetch("/events", "POST", JSON.stringify(signed), {contentType: "application/json"});
      if (!response.ok) throw Error("live publish failed");
    }, liveText);
    await page.waitForFunction(text => document.querySelector("#messages")?.textContent.includes(text), liveText);
    await page.screenshot({path: "output/playwright/room-attachments/room-desktop.png"});
    return {...info, liveAppend: true};
  });

  await run("file picker previews and publishes an image", async () => {
    const picker = page.locator('room-compose input[name="attachments"]');
    check(await picker.count() === 1, "attachment picker missing");
    const attach = page.locator('room-compose [data-attach]');
    check(await attach.count() === 1, "compact attach button missing");
    check(!await picker.isVisible(), "native file picker should be hidden behind the attach button");
    // Check the native chooser separately with CLI click/upload commands;
    // its modal state is managed outside run-code by playwright-cli.
    await picker.setInputFiles("output/playwright/room-attachments/browser-upload.png");
    await page.waitForSelector("room-files li");
    check((await page.locator("room-files").textContent()).includes("browser-upload.png"), "pending file preview missing");
    check(await page.locator("room-files img").count() === 1, "image attachment thumbnail missing");
    await page.waitForFunction(() => {
      const img = document.querySelector("room-files img");
      return img?.complete && img.naturalWidth > 0;
    });
    check(await page.locator("room-files img").first().evaluate(img => img.complete && img.naturalWidth > 0), "image attachment thumbnail did not load");
    const remove = page.getByRole("button", {name: "Remove browser-upload.png", exact: true});
    check(await remove.count() === 1, "attachment remove button missing");
    await remove.click();
    check(await page.locator("room-files li").count() === 0, "attachment remove did not clear preview");
    await picker.setInputFiles("output/playwright/room-attachments/browser-upload.png");
    await page.waitForSelector("room-files li");
    await page.locator('room-compose textarea[name="content"]').fill("Browser upload");
    const request = page.waitForRequest(req => req.method() === "PUT" && req.url().includes("/rooms/attachments-qa/attachments"));
    await page.locator("room-compose").getByRole("button", {name: "Send", exact: true}).click();
    await request;
    await page.waitForFunction(() => !document.querySelector("room-files li"));
    check(await page.locator("#messages").textContent().then(text => text.includes("Browser upload")), "published upload missing from timeline");
    return {uploaded: true};
  });

  await run("attachment-only message is accepted", async () => {
    const picker = page.locator('room-compose input[name="attachments"]');
    await picker.setInputFiles("output/playwright/room-attachments/only.txt");
    await page.waitForSelector("room-files li");
    check(await page.locator('room-compose textarea[name="content"]').getAttribute("required") === null, "content remained required with an attachment");
    await page.locator("room-compose").getByRole("button", {name: "Send", exact: true}).click();
    await page.waitForFunction(() => !document.querySelector("room-files li"));
    return {uploaded: true};
  });

  await run("reload and narrow viewport preserve media layout", async () => {
    await page.reload();
    await page.waitForSelector("#messages room-message");
    await page.setViewportSize({width: 360, height: 800});
    const metrics = await page.evaluate(() => ({width: innerWidth, scrollWidth: document.documentElement.scrollWidth, content: document.querySelector("#content")?.scrollWidth, image: document.querySelector("#messages img")?.complete}));
    check(metrics.scrollWidth <= metrics.width && metrics.content <= metrics.width, "room overflows at narrow viewport");
    check(metrics.image, "image did not load after reload");
    await page.screenshot({path: "output/playwright/room-attachments/room-mobile.png"});
    return metrics;
  });

  await run("composer stays compact and touch friendly", async () => {
    await page.setViewportSize({width: 360, height: 420});
    const compose = page.locator("room-compose");
    const input = compose.locator('input[name="attachments"]');
    check(await input.count() === 1, "attachment input missing");
    const metrics = await page.evaluate(() => {
      const node = document.querySelector("room-compose");
      const attach = node?.querySelector('button[aria-label*="Attach" i], label[for], label');
      const send = node?.querySelector('button[type="submit"], button:not([type])');
      const box = element => element?.getBoundingClientRect();
      const attachBox = box(attach), sendBox = box(send);
      return {
        composeHeight: box(node)?.height || 0,
        viewportHeight: innerHeight,
        scrollWidth: document.documentElement.scrollWidth,
        viewportWidth: innerWidth,
        attachWidth: attachBox?.width || 0,
        attachHeight: attachBox?.height || 0,
        sendWidth: sendBox?.width || 0,
        sendHeight: sendBox?.height || 0,
        placeholder: node?.querySelector("textarea")?.getAttribute("placeholder") || ""
      };
    });
    check(metrics.scrollWidth <= metrics.viewportWidth, "composer overflows horizontally");
    check(metrics.composeHeight <= 90, "empty composer occupies too much of the viewport");
    check(metrics.attachWidth >= 40 && metrics.attachHeight >= 40, "attach control is too small to tap");
    check(metrics.sendWidth >= 40 && metrics.sendHeight >= 40, "send control is too small to tap");
    check(!/[0-9a-f]{16,}/i.test(metrics.placeholder), "composer exposes an internal identifier in its placeholder");
    await page.screenshot({path: "output/playwright/room-attachments/room-mobile-composer.png"});
    await page.setViewportSize({width: 1440, height: 900});
    const desktop = await page.evaluate(() => ({
      width: innerWidth,
      scrollWidth: document.documentElement.scrollWidth,
      composeHeight: document.querySelector("room-compose")?.getBoundingClientRect().height || 0
    }));
    check(desktop.scrollWidth <= desktop.width, "desktop composer overflows horizontally");
    await page.screenshot({path: "output/playwright/room-attachments/room-desktop-composer.png"});
    return {mobile: metrics, desktop};
  });

  await run("JavaScript-disabled SSR still renders media", async () => {
    const context = await page.context().browser().newContext({javaScriptEnabled: false, viewport: {width: 360, height: 800}, storageState: await page.context().storageState()});
    try {
      const noJS = await context.newPage();
      await noJS.goto(base + room);
      const info = await noJS.evaluate(() => ({images: document.querySelectorAll("#messages img").length, links: document.querySelectorAll("#messages a").length, compose: !!document.querySelector("room-compose")}));
      check(info.images >= 1 && info.links >= 1, "no-JS room omitted attachment markup");
      check(info.compose, "no-JS room omitted the attachment composer markup");
      await noJS.screenshot({path: "output/playwright/room-attachments/room-nojs.png"});
      return info;
    } finally { await context.close(); }
  });

  await run("guest cannot fetch private media", async () => {
    const context = await page.context().browser().newContext();
    try {
      const guest = await context.newPage();
      const mediaURL = await page.locator("#messages img").first().getAttribute("src");
      check(mediaURL, "could not locate fixture media URL");
      const media = await context.request.get(mediaURL);
      check([401, 403].includes(media.status()), "guest private media returned " + media.status());
      return {status: media.status()};
    } finally { await context.close(); }
  });
  results.push({name: "uncaught browser errors", ok: errors.length === 0, errors});
  return {passed: results.filter(item => item.ok).length, failed: results.filter(item => !item.ok).length, results};
}
