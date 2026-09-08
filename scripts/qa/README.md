# UI checks

Use a disposable local relay with the public fixture identity. This identity is for testing only.

```sh
go build -o /tmp/tiny-ux ./cmd/tiny
/tmp/tiny-ux tenant create --data-dir /tmp/tinyrelay-ux --name main \
  --owner 5ac640e5df8f7945381c31f435288ba3f587fbd68efb27abaf941792f9c36369
/tmp/tiny-ux serve --data-dir /tmp/tinyrelay-ux --default-tenant main \
  --listen 127.0.0.1:18447 --allow-private-relays
```

In another terminal, seed notes, articles, files and a Git repository, then check valid and malformed URLs:

```sh
node scripts/qa/ux-fixture.mjs --data /tmp/tinyrelay-ux
node scripts/qa/ux-http-fuzz.mjs
go test ./internal/webui -run '^$' -fuzz FuzzInjectBasePreservesText -fuzztime=10s
```

The HTTP report and browser screenshots are saved under `output/playwright/ux/`.

For browser checks, open the fixture relay with Playwright CLI. The edge checks use a separate guest session. They cover failed requests, competing navigation, sign-in errors, new tabs and 60 seeded navigation actions.

```sh
npx --package @playwright/cli playwright-cli -s=tiny-ux open http://127.0.0.1:18447
npx --package @playwright/cli playwright-cli -s=tiny-ux run-code \
  --filename scripts/qa/ux-browser-edges.js
```

The full flow checks require signing in as the fixture owner. Start the local test signer, open `/signin` in that browser session and connect using the printed Bunker URL:

```sh
BUNKER_USER_SECRET=fc1d06a0fd5e622dcf448d0b3c2fccc891a5ecf74546a7675460d92441fecfdd \
  node scripts/qa/nip46-bunker.mjs
```

Then run the viewer, filter, history, signed form, mobile and no-JavaScript checks:

```sh
npx --package @playwright/cli playwright-cli -s=tiny-ux run-code \
  --filename scripts/qa/ux-browser-flow.js
npx --package @playwright/cli playwright-cli -s=tiny-ux run-code \
  --filename scripts/qa/ux-browser-menu.js
```

The menu checks cover public pages, hosted viewers and management pages with JavaScript enabled and disabled. They verify that mobile navigation starts closed, opens with a click or keyboard input, and leaves content in place. Desktop navigation remains visible.

Each browser check returns its results and failure count. A real phone signer is still needed to verify the operating system handoff and approval screens.
