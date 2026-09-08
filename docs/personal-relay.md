# Personal relay deployment

The personal relay runs on a Linux Docker host. Tailnet devices reach it at [https://tiny.tailbe516a.ts.net](https://tiny.tailbe516a.ts.net); everyone else reaches it through the Fly edge at `https://012.run`. The relay, Caddy and Tailscale containers share one network namespace, so nothing on the host itself listens publicly.

## Public domain through a Fly edge

The public address is `https://012.run`, with sites on `*.012.run`. A small Fly machine, `deploy/edge`, terminates TLS at Fly's edge, joins the tailnet in userspace mode, and forwards plain HTTP over the tailnet to a listener on the relay host that only the tailnet can reach. Fly's proxy has no request body cap and passes WebSockets.

```sh
cd deploy/edge
fly deploy
fly logs --no-tail          # first boot prints a Tailscale login link; open it to approve the machine
fly certs check 012.run
fly certs check "*.012.run"
```

DNS at the registrar, all DNS-only records: A and AAAA for `012.run` and `*.012.run` pointing at the app's addresses from `fly ips list`, and a `_acme-challenge.012.run` CNAME from `fly certs setup "*.012.run"` for the wildcard certificate. The relay host's Caddyfile serves `012.run:8080` and `*.012.run:8080` over plain HTTP for the edge; see `deploy/edge/slate.Caddyfile.snippet`. After the certificates issue, set `--public-url https://012.run` on the relay so signed requests, NIP-11 and site hosts use the public name. The ts.net name stays tailnet only.

## Repository hosting

The relay hosts Git repositories over GRASP. Use [ngit](https://ngit.dev) so the owner signs the announcement, the ref state and each push with a remote signer. `ngit account login` shows a QR code to scan with Amber; the key stays on the phone.

```sh
ngit account login
ngit init --name tiny --identifier tinyrelay --grasp-server tiny.tailbe516a.ts.net \
  --description "self-hosted, multitenant Nostr relay"
git remote add tiny nostr://<owner npub>/tiny.tailbe516a.ts.net/tinyrelay
git push tiny main --tags
```

Later pushes are plain `git push tiny`. Anyone can clone the repository with `git clone nostr://<owner npub>/tiny.tailbe516a.ts.net/tinyrelay`, or with plain Git from `https://tiny.tailbe516a.ts.net/<owner npub>/tinyrelay.git`.

## Web operations

Sign in at the relay URL with a Nostr signer before using owner controls. The Manage pages provide relay configuration, jobs, connections, backups, and file operations. Use Manage data or the browser tools page to start and inspect backups. The [WebMCP guide](webmcp.md) describes native browser-agent controls and their existing signer authorization path.

Download backups from **Manage > Data** and keep a copy on another device.
