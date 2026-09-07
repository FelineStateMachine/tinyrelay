# Personal relay deployment

The personal relay runs on a Linux Docker host and is published through Tailscale Funnel at [https://tiny.tailbe516a.ts.net](https://tiny.tailbe516a.ts.net). The relay, Caddy and Tailscale containers share one network namespace, so only that node's port 443 is public; the host itself stays private.

## Public access

Funnel forwards raw TLS on port 443 to Caddy inside the relay's Tailscale node. Caddy keeps terminating TLS with the tailnet certificate, so nothing else changes. The setting persists in the Tailscale state directory across container restarts.

```sh
docker exec tiny-tailscale tailscale funnel --bg --tcp=443 tcp://127.0.0.1:443
docker exec tiny-tailscale tailscale funnel status
docker exec tiny-tailscale tailscale funnel --tcp=443 off
```

Public DNS for the relay name resolves to Tailscale's ingress only while Funnel is on; tailnet devices keep using the direct path. Writes stay restricted to members, so public visitors can read and sign in but cannot publish without an invitation. The relay sees the loopback address for every connection behind Caddy, so IP based blocks apply to the proxy rather than to individual clients.

## Storage

The relay data lives on a dedicated 2 TB WD drive mounted at `/srv/tiny`. The drive uses UUID `98f49cba-bd8c-432d-a91a-41db66920e90` and is formatted as ext4.

Persisted directories include:

- `/srv/tiny/data` for relay and tenant data
- `/srv/tiny/tailscale` for Tailscale state
- `/srv/tiny/certs` for certificates
- `/srv/tiny/logs` for service logs

The service checks the drive's UUID before starting and stops if the mount disappears.

## Service operations

The service is `tiny.service`, with certificate renewal scheduled by `tiny-certificates.timer`.

In the Tailscale admin console, open **Machines > tiny > Endpoints** to find the HTTP and HTTPS listeners. The node's key expiry is disabled.

```sh
ssh <host> sudo systemctl status tiny
ssh <host> sudo systemctl restart tiny
ssh <host> sudo systemctl status tiny-certificates.timer
ssh <host> docker logs tiny
```

To deploy an update, build the images from a checkout on the host, then restart the service:

```sh
docker build --target runtime -t tiny:dogfood .
docker build -f deploy/Tailscale.Dockerfile -t tiny-tailscale:dogfood deploy
sudo systemctl restart tiny
```

The owner is `npub1cq2s86qrtadf36eqq3epm38wmhdynrrcwxf84eldk3f94dkv7ttsrulrz5`. Sign in with that identity to manage the relay.

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
