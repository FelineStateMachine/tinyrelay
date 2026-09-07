# Vendored Fixi integration

The web UI vendors the all-in-one Fixi bundle at `internal/webui/fixi.js`. It is copied from [the-fixi-project](https://github.com/bigskysoftware/the-fixi-project) commit `71307c9694dff3cc7f2aef087cb57b44db4bdd17` (retrieved 2026-09-07). The upstream project documents the bundle and APIs at [fixiproject.org/llms.txt](https://fixiproject.org/llms.txt). Upstream declares BSD-0 (Zero-Clause BSD) for all five libraries; see its [README license section](https://github.com/bigskysoftware/the-fixi-project#license).

The vendored file's SHA-256 is:

```text
b70458c212d0409f8a6319c286351865d92587292ad4f9bc0dfd1c46ffcad855
```

`/fixi.js` is served locally with no CDN dependency. The page loads it before the existing signer bundle. The Connect page uses Fixi's `fx-action` fragment request for a read-only connection preview; `/connect/fragment` renders server-escaped table HTML, while `/connect.json` remains a JSON API. Mutation forms remain handled by the existing signer bridge because every request body must be serialized once, hashed exactly, signed as NIP-98, and submitted once. Fixi is not allowed to issue those signed mutations or retry them.

The existing signer bridge still owns NIP-07, NIP-46, Nostr Connect, Amber links, session login, and management RPC. Fixi's fragment swaps are optional progressive enhancement: links and read-only navigation continue to work if JavaScript is unavailable; signed management actions require the signer bridge JavaScript. Paxi's focus-preserving `morph` swap is used by the read-only connection fragment. Ssexi consumes the authenticated `/manage/jobs/status` stream; the handler resolves the actor before opening it, passes that actor to every `listjobs` query, emits only changed escaped HTML table fragments into the existing job table, and closes on request cancellation. The `rpc-form` element disables its buttons while a signed call is in flight and re-enables them after completion. The current backend contract remains `Query(ctx, method, params, actor)`.

The layout remains a semantic table with fixed 960px desktop and 360px mobile variants. Fixi does not add branding or layout CSS.
