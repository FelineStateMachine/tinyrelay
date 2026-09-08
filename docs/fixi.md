# Vendored Fixi integration

The web UI vendors the all-in-one Fixi bundle at `internal/webui/fixi.js`. It is copied from [the-fixi-project](https://github.com/bigskysoftware/the-fixi-project) commit `71307c9694dff3cc7f2aef087cb57b44db4bdd17` (retrieved 2026-09-07). The upstream project documents the bundle and APIs at [fixiproject.org/llms.txt](https://fixiproject.org/llms.txt). Upstream declares BSD-0 (Zero-Clause BSD) for all five libraries; see its [README license section](https://github.com/bigskysoftware/the-fixi-project#license).

The vendored file's SHA-256 is:

```text
b70458c212d0409f8a6319c286351865d92587292ad4f9bc0dfd1c46ffcad855
```

`/fixi.js` is served locally with no CDN dependency. The page loads it before the existing signer bundle. The Sync page uses Fixi's `fx-action` request for the job status stream, which renders server-escaped table HTML; `/connect.json` remains a JSON API for the configured connections. Mutation forms remain handled by the existing signer bridge because every request body must be serialized once, hashed exactly, signed as NIP-98, and submitted once. Fixi is not allowed to issue those signed mutations or retry them.

The signer bridge handles NIP-07, NIP-46, Nostr Connect, Amber links, session login and management actions. Encrypted uploads, NIP-34 publishing and NIP-17 file messages sign through the same bridge and never retry a signed request on their own. Fixi updates page links and GET forms within the current tenant without a full reload. Filters, repository refs, browser history and page titles remain available. Downloads, external links and modified clicks retain normal browser behavior. A failed request displays an error in the footer and leaves the current page usable. Navigation and content also work without JavaScript, including on phones.

Signed actions require JavaScript and submit once while their controls are disabled. Connect previews update in place. Job status uses an authenticated stream that sends updates when the results change.

On phones, navigation starts closed and opens from the menu control, with or without JavaScript. Opening the menu leaves content in place. Desktop pages retain the sidebar.

The layout remains a semantic table: a rail, a content column and a panel on desktop, stacked with a slide-in menu on phones. Fixi does not add branding or layout CSS.
